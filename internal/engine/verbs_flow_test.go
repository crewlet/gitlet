package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// The tests in this file cover discuss, accept, dismiss, update, submit and
// abandon, on real git like integration_test.go: a bare remote and two
// clones, one per agent.

// ---- helpers ----

// flowStart states a task and returns its item and branch.
func flowStart(t *testing.T, ctx context.Context, e *Engine, intent string) (model.ItemID, string) {
	t.Helper()
	r, err := e.Start(ctx, api.StartInput{Intent: intent})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start %q: %s %q %v", intent, r.Outcome, r.Summary, r.Details)
	}
	return r.Data.Item.ID, r.Data.Branch
}

// flowCommit writes files (name, content pairs) in dir, commits everything
// and returns the commit.
func flowCommit(t *testing.T, dir, msg string, files ...string) string {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		writeFile(t, dir, files[i], files[i+1])
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", msg)
	return runGit(t, dir, "rev-parse", "HEAD")
}

// flowMoveMain commits files on main through a third clone, c, and pushes
// it: someone else's work landing.
func flowMoveMain(t *testing.T, f *fixture, msg string, files ...string) string {
	t.Helper()
	c := filepath.Join(f.root, "c")
	if _, err := os.Stat(c); err != nil {
		runGit(t, f.root, "clone", "-q", f.remote, c)
	} else {
		runGit(t, c, "pull", "-q", "--ff-only", "origin", "main")
	}
	oid := flowCommit(t, c, msg, files...)
	runGit(t, c, "push", "-q", "origin", "main")
	return oid
}

// flowWant fails the test unless r has outcome want; a refusal must offer
// a way out.
func flowWant(t *testing.T, what string, r api.Result, want api.Outcome) {
	t.Helper()
	if r.Outcome != want {
		t.Fatalf("%s: want %s, got:\n%s", what, want, render.Text(&r, render.CLI))
	}
	if r.Outcome == api.Refused && len(r.Options) == 0 {
		t.Fatalf("%s: a refusal without options: %q", what, r.Summary)
	}
}

// flowHasTool reports whether an option calls tool with the given
// arguments (name, value pairs).
func flowHasTool(opts []model.Option, tool string, kv ...string) bool {
	return slices.ContainsFunc(opts, func(o model.Option) bool {
		if o.Tool != tool {
			return false
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if v, _ := o.Args[kv[i]].(string); v != kv[i+1] {
				return false
			}
		}
		return true
	})
}

// flowHasCommand reports whether an option runs a command containing sub.
func flowHasCommand(opts []model.Option, sub string) bool {
	return slices.ContainsFunc(opts, func(o model.Option) bool { return strings.Contains(o.Command, sub) })
}

// flowNote returns the first note of kind, or nil.
func flowNote(notes []model.Note, kind model.NoteKind) *model.Note {
	for i := range notes {
		if notes[i].Kind == kind {
			return &notes[i]
		}
	}
	return nil
}

// flowLocked runs fn under the engine's verb lock, as verbs run.
func flowLocked(e *Engine, fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn()
}

// flowClaim makes e claim path:qualified as it is at rev for item. With
// contract, the claim carries rev's signature as its published contract
// and rev as its stub commit.
func flowClaim(t *testing.T, ctx context.Context, e *Engine, item model.ItemID, rev, path, qualified string, contract bool, pr string) bus.ClaimView {
	t.Helper()
	var cv bus.ClaimView
	flowLocked(e, func() {
		sym, ok := e.symbolAt(ctx, rev, path, qualified)
		if !ok {
			t.Fatalf("no %s:%s at %s", path, qualified, rev)
		}
		c := e.newClaim(sym, item, false, nil)
		parent := ""
		if contract {
			c.Contract = &model.Contract{Signature: sym.Signature, SigHash: sym.SigHash, StubCommit: rev, PR: pr, PublishedAt: e.now().UTC()}
			parent = rev
		}
		won, cur, err := e.createClaim(ctx, c, parent)
		must(t, err)
		if !won {
			t.Fatalf("claim on %s:%s lost to %s", path, qualified, cur.Claim.Owner)
		}
		cv = cur
	})
	return cv
}

// flowAddNeed records a need in e's outbox, as need and use do.
func flowAddNeed(t *testing.T, ctx context.Context, e *Engine, n model.Need) {
	t.Helper()
	flowLocked(e, func() {
		_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
			ob.Needs = append(ob.Needs, n)
			return nil
		})
		must(t, err)
	})
}

// flowPath leaves only git, and a gh running ghScript when it is not empty,
// on PATH for the rest of the test.
func flowPath(t *testing.T, ghScript string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	if ghScript != "" {
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
}

const flowPRURL = "https://github.com/acme/widget/pull/7"

// flowFakeGh puts a gh on PATH that logs its arguments, one per line, to
// the returned file. pr create prints flowPRURL; pr view finds it open once
// it was created. Only shell builtins: PATH has nothing else.
func flowFakeGh(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log, mark := filepath.Join(dir, "gh.log"), filepath.Join(dir, "created")
	flowPath(t, `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >> '`+log+`'
case "$1 $2" in
"pr view")
	if [ -f '`+mark+`' ]; then
		printf '{"url":"%s","state":"OPEN"}\n' '`+flowPRURL+`'
		exit 0
	fi
	echo 'no pull requests found for branch' >&2
	exit 1 ;;
"pr create")
	: > '`+mark+`'
	printf '%s\n' '`+flowPRURL+`'
	exit 0 ;;
esac
echo "unexpected gh $*" >&2
exit 1
`)
	return log
}

// flowForgeRemote makes dir's origin git@github.com:acme/widget.git, which
// a stand-in ssh serves from the fixture's bare remote.
func flowForgeRemote(t *testing.T, f *fixture, dir string) {
	t.Helper()
	link := filepath.Join(f.root, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.remote, link); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	ssh := filepath.Join(f.root, "fake-ssh")
	// ssh.variant=simple: $1 is the host, $2 the git command to run.
	script := "#!/bin/sh\ncd '" + f.root + "' || exit 1\neval \"exec git ${2#git-}\"\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "config", "core.sshCommand", ssh)
	runGit(t, dir, "config", "ssh.variant", "simple")
	runGit(t, dir, "remote", "set-url", "origin", "git@github.com:acme/widget.git")
}

// flowGitlet builds the gitlet binary, for update's sequence editor.
func flowGitlet(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not found: building gitlet for the sequence editor needs it")
	}
	exe := filepath.Join(t.TempDir(), "gitlet")
	cmd := exec.Command(gobin, "build", "-buildvcs=false", "-o", exe, "github.com/crewlet/gitlet/cmd/gitlet")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

// flowPair is two agents whose tasks change Load's body differently, and
// a's note about it, which offers a thread.
type flowPair struct {
	a, b   *Engine
	ia, ib model.ItemID
	branch string // a's
	note   model.Note
}

func flowOverlap(t *testing.T, ctx context.Context, f *fixture, clk *clock) flowPair {
	t.Helper()
	p := flowPair{a: openEngine(t, Options{Dir: f.a, Now: clk.Now}), b: openEngine(t, Options{Dir: f.b, Now: clk.Now})}
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path, Strict: true}, nil", 1))
	writeFile(t, f.b, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path + \".toml\"}, nil", 1))
	p.ia, p.branch = flowStart(t, ctx, p.a, "Add strict mode to config loading")
	p.ib, _ = flowStart(t, ctx, p.b, "Append the extension to config paths")
	clk.Advance(30 * time.Second)
	must(t, p.a.Tick(ctx))
	must(t, p.b.Tick(ctx))
	clk.Advance(6 * time.Second)
	st, err := p.a.Status(ctx, api.StatusInput{})
	must(t, err)
	for _, n := range st.Notes {
		if n.Thread != "" && n.Kind != model.NoteMessage && n.Kind != model.NoteSignatureClash {
			p.note = n
			return p
		}
	}
	t.Fatalf("a has no note that offers a thread: %v", noteKinds(st.Notes))
	return p
}

// strictStub is config.go with the stub of Load's new signature; strictLoad
// is its implementation, as it lands on main.
var (
	strictStub = strings.Replace(configGo, "func Load(path string) (*Config, error) {\n\treturn &Config{Path: path}, nil",
		"func Load(path string, strict bool) (*Config, error) {\n\treturn &Config{Path: path}, nil // stub: strict comes later", 1)
	strictLoad = strings.Replace(configGo, "func Load(path string) (*Config, error) {\n\treturn &Config{Path: path}, nil",
		"func Load(path string, strict bool) (*Config, error) {\n\treturn &Config{Path: path, Strict: strict}, nil", 1)
)

// ---- pure helpers ----

func TestFlowParseForge(t *testing.T) {
	cases := []struct {
		url, kind, compare string
	}{
		{"https://github.com/acme/widget.git", forgeGitHub, "https://github.com/acme/widget/compare/main...gitlet/fix-it?expand=1"},
		{"https://x-access-token:secret@github.com/acme/widget", forgeGitHub, "https://github.com/acme/widget/compare/main...gitlet/fix-it?expand=1"},
		{"git@github.com:acme/widget.git", forgeGitHub, "https://github.com/acme/widget/compare/main...gitlet/fix-it?expand=1"},
		{"ssh://git@ssh.github.com:443/acme/widget.git", forgeGitHub, "https://github.com/acme/widget/compare/main...gitlet/fix-it?expand=1"},
		{"https://github.example.com/team/widget.git", forgeGitHub, "https://github.example.com/team/widget/compare/main...gitlet/fix-it?expand=1"},
		{"git@gitlab.com:group/sub/widget.git", forgeGitLab,
			"https://gitlab.com/group/sub/widget/-/merge_requests/new?merge_request%5Bsource_branch%5D=gitlet%2Ffix-it&merge_request%5Btarget_branch%5D=main"},
		{"ssh://git@gitlab.example.com:2222/group/widget.git", forgeGitLab,
			"https://gitlab.example.com/group/widget/-/merge_requests/new?merge_request%5Bsource_branch%5D=gitlet%2Ffix-it&merge_request%5Btarget_branch%5D=main"},
		{"http://gitlab.example.com:8080/group/widget", forgeGitLab,
			"http://gitlab.example.com:8080/group/widget/-/merge_requests/new?merge_request%5Bsource_branch%5D=gitlet%2Ffix-it&merge_request%5Btarget_branch%5D=main"},
		{"https://me@bitbucket.org/team/widget.git", forgeBitbucket, "https://bitbucket.org/team/widget/pull-requests/new?dest=main&source=gitlet%2Ffix-it"},
		{"git@bitbucket.org:team/widget.git", forgeBitbucket, "https://bitbucket.org/team/widget/pull-requests/new?dest=main&source=gitlet%2Ffix-it"},
		{"https://git.example.com/team/widget.git", "", ""},
		{"/srv/git/widget.git", "", ""},
		{"../widget.git", "", ""},
		{"file:///srv/git/github.com/widget.git", "", ""},
		{"C:/repos/widget", "", ""},
		{"https://github.com/widget", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		f := parseForge(c.url)
		if f.kind != c.kind {
			t.Errorf("parseForge(%q).kind = %q, want %q", c.url, f.kind, c.kind)
		}
		if got := f.compareURL("main", "gitlet/fix-it"); got != c.compare {
			t.Errorf("compare URL for %q:\n got %s\nwant %s", c.url, got, c.compare)
		}
	}
	// Branch names are escaped segment by segment.
	if got := parseForge("git@github.com:acme/widget.git").compareURL("main", "gitlet/fix #1"); got != "https://github.com/acme/widget/compare/main...gitlet/fix%20%231?expand=1" {
		t.Errorf("escaped compare URL: %s", got)
	}
}

func TestFlowLastURL(t *testing.T) {
	out := "\nCreating pull request for gitlet/x into main in acme/widget\n\nhttps://github.com/acme/widget/pull/9\n"
	if got := lastURL(out); got != "https://github.com/acme/widget/pull/9" {
		t.Errorf("lastURL = %q", got)
	}
	if got := lastURL("nothing here\n"); got != "" {
		t.Errorf("lastURL without a URL = %q", got)
	}
}

func TestFlowDropEditor(t *testing.T) {
	e := &Engine{}
	e.opt.Exe = "/opt/my tools/gitlet"
	got, err := e.dropEditor([]string{"3f2a9c1", "77aa001"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `GITLET_DROP='3f2a9c1 77aa001' '/opt/my tools/gitlet' hook sequence-editor`; got != want {
		t.Errorf("dropEditor = %s, want %s", got, want)
	}
}

func TestFlowProposalOf(t *testing.T) {
	e := &Engine{}
	e.me.ID = "test-aaaa1111"
	two := model.ThreadState{ID: "t-0123456789ab", SubjectName: "config.go:Load", Participants: []model.Participant{
		{Agent: "test-aaaa1111", Item: "i1"}, {Agent: "test-bbbb2222", Item: "i2"},
	}}
	three := two
	three.Participants = append(slices.Clone(two.Participants), model.Participant{Agent: "test-cccc3333", Item: "i3"})

	for _, c := range []struct {
		t     model.ThreadState
		owner string
		want  model.AgentID // "" when refused
	}{
		{two, "me", "test-aaaa1111"},
		{two, "them", "test-bbbb2222"},
		{two, "Test-BBBB2222", "test-bbbb2222"},
		{two, "test-dddd4444", ""},
		{two, "", ""},
		{three, "them", ""},
		{three, "test-cccc3333", "test-cccc3333"},
	} {
		p, refusal := e.proposalOf(c.t, api.ProposalInput{Owner: c.owner, Summary: "one owner"})
		switch {
		case c.want == "" && refusal == nil:
			t.Errorf("owner %q among %d: accepted as %q", c.owner, len(c.t.Participants), p.Owner)
		case c.want == "" && !flowHasTool(refusal.Options, "discuss", "about", c.t.ID):
			t.Errorf("owner %q: refusal options %+v", c.owner, refusal.Options)
		case c.want != "" && refusal != nil:
			t.Errorf("owner %q: refused: %s", c.owner, refusal.Summary)
		case c.want != "" && (p.Owner != c.want || !strings.HasPrefix(p.ID, "p-")):
			t.Errorf("owner %q: proposal %+v, want owner %s", c.owner, p, c.want)
		}
	}
	if _, refusal := e.proposalOf(two, api.ProposalInput{Owner: "me"}); refusal == nil {
		t.Error("a proposal without a summary was accepted")
	}
}

// ---- discuss, accept, dismiss ----

func TestFlowDiscussAndAccept(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	p := flowOverlap(t, ctx, f, clk)

	// a opens the note's thread.
	r, err := p.a.Discuss(ctx, api.DiscussInput{About: p.note.ID, Message: "I add a Strict flag to Load; what do you need from it?"})
	must(t, err)
	flowWant(t, "discuss on a note", r.Result, api.OK)
	th, msg := r.Data.Thread, r.Data.Message
	if th.ID != p.note.Thread || len(th.Messages) != 1 || msg.From != p.a.Agent().ID || msg.Thread != th.ID || msg.Item != p.ia {
		t.Fatalf("thread %+v, message %+v", th, msg)
	}
	if want := []model.ItemID{p.ia, p.ib}; !slices.Equal(msg.Items, slices.Sorted(slices.Values(want))) {
		t.Errorf("message items %v, want both items", msg.Items)
	}

	// b gets a message note and answers in the thread, proposing that a
	// owns Load.
	clk.Advance(6 * time.Second)
	st, err := p.b.Status(ctx, api.StatusInput{})
	must(t, err)
	mn := flowNote(st.Notes, model.NoteMessage)
	if mn == nil || mn.Thread != th.ID {
		t.Fatalf("b's notes after a's message: %v", noteKinds(st.Notes))
	}
	rb, err := p.b.Discuss(ctx, api.DiscussInput{About: mn.ID, Message: "Go ahead, I rebase on yours.",
		Proposal: &api.ProposalInput{Owner: "them", Summary: "a changes Load; b appends the extension in Default"}})
	must(t, err)
	flowWant(t, "b's proposal", rb.Result, api.OK)
	prop := rb.Data.Message.Proposal
	if prop == nil || prop.Owner != p.a.Agent().ID || len(rb.Data.Thread.Messages) != 2 || !slices.Contains(rb.Data.Message.Seen, msg.ID) {
		t.Fatalf("b's message %+v in %+v", rb.Data.Message, rb.Data.Thread)
	}

	// The proposer does not accept its own proposal.
	own, err := p.b.Accept(ctx, api.AcceptInput{Proposal: prop.ID})
	must(t, err)
	flowWant(t, "accepting one's own proposal", own.Result, api.NothingToDo)

	// a accepts by prefix: everyone agreed.
	clk.Advance(6 * time.Second)
	ra, err := p.a.Accept(ctx, api.AcceptInput{Proposal: prop.ID[:8]})
	must(t, err)
	flowWant(t, "a's accept", ra.Result, api.OK)
	if !ra.Data.Agreed || !ra.Data.Thread.Settled || ra.Data.Thread.Owner != p.a.Agent().ID {
		t.Fatalf("after a's accept: %+v", ra.Data)
	}
	again, err := p.a.Accept(ctx, api.AcceptInput{Proposal: prop.ID})
	must(t, err)
	flowWant(t, "accepting again", again.Result, api.NothingToDo)

	// b sees the agreement; the thread takes no more messages.
	clk.Advance(6 * time.Second)
	st, err = p.b.Status(ctx, api.StatusInput{})
	must(t, err)
	i := slices.IndexFunc(st.Data.Threads, func(x model.ThreadState) bool { return x.ID == th.ID })
	if i < 0 || !st.Data.Threads[i].Settled || st.Data.Threads[i].Agreement == nil || st.Data.Threads[i].Owner != p.a.Agent().ID {
		t.Fatalf("b's threads: %+v", st.Data.Threads)
	}
	late, err := p.b.Discuss(ctx, api.DiscussInput{About: th.ID, Message: "One more thing."})
	must(t, err)
	flowWant(t, "discuss in a settled thread", late.Result, api.Refused)
	if !strings.Contains(late.Summary, "settled") {
		t.Errorf("settled refusal: %q", late.Summary)
	}
}

func TestFlowDiscussRefusals(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	p := flowOverlap(t, ctx, f, clk)

	for _, c := range []struct {
		name string
		in   api.DiscussInput
	}{
		{"empty message", api.DiscussInput{About: p.note.ID, Message: "  "}},
		{"long message", api.DiscussInput{About: p.note.ID, Message: strings.Repeat("x", maxMessageLen+1)}},
		{"unknown note", api.DiscussInput{About: "n-0000ffff0000", Message: "hello"}},
		{"unknown thread", api.DiscussInput{About: "t-0000ffff0000", Message: "hello"}},
		{"owner outside the thread", api.DiscussInput{About: p.note.ID, Message: "hello", Proposal: &api.ProposalInput{Owner: "test-ffff0000", Summary: "x"}}},
		{"no owner", api.DiscussInput{About: p.note.ID, Message: "hello", Proposal: &api.ProposalInput{Summary: "x"}}},
	} {
		r, err := p.a.Discuss(ctx, c.in)
		must(t, err)
		flowWant(t, c.name, r.Result, api.Refused)
	}
	// Nothing was written.
	flowLocked(p.a, func() {
		if n := len(p.a.myOutbox().Messages); n != 0 {
			t.Errorf("refusals wrote %d messages", n)
		}
	})

	// Proposals name me, them or a participant.
	for _, c := range []struct {
		owner string
		want  model.AgentID
	}{{"me", p.a.Agent().ID}, {string(p.b.Agent().ID), p.b.Agent().ID}, {"them", p.b.Agent().ID}} {
		r, err := p.a.Discuss(ctx, api.DiscussInput{About: p.note.ID, Message: "How about this?",
			Proposal: &api.ProposalInput{Owner: c.owner, Summary: "one of us changes Load"}})
		must(t, err)
		flowWant(t, "proposal with owner "+c.owner, r.Result, api.OK)
		if pr := r.Data.Message.Proposal; pr == nil || pr.Owner != c.want {
			t.Errorf("owner %q: proposal %+v", c.owner, pr)
		}
	}
}

func TestFlowSignatureThreadQuota(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {", "func Load(path string, strict bool) (*Config, error) {", 1))
	writeFile(t, f.b, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {", "func Load(ctx context.Context, path string) (*Config, error) {", 1))
	flowStart(t, ctx, ea, "Add a strict flag to Load")
	flowStart(t, ctx, eb, "Pass a context to Load")
	clk.Advance(time.Minute)
	must(t, ea.Tick(ctx))
	clk.Advance(time.Second)
	must(t, eb.Tick(ctx))
	clk.Advance(6 * time.Second)
	st, err := eb.Status(ctx, api.StatusInput{})
	must(t, err)
	clash := flowNote(st.Notes, model.NoteSignatureClash)
	if clash == nil || clash.Thread == "" {
		t.Fatalf("b has no signature clash note: %v", noteKinds(st.Notes))
	}

	// b proposes that a, the holder, keeps Load, then uses up its messages.
	limit := eb.Config().ThreadMessages
	r, err := eb.Discuss(ctx, api.DiscussInput{About: clash.ID, Message: "Keep your signature; I adapt.",
		Proposal: &api.ProposalInput{Owner: "them", Summary: "a keeps Load(path, strict)", Signature: "func Load(path string, strict bool) (*Config, error)"}})
	must(t, err)
	flowWant(t, "first message", r.Result, api.OK)
	prop := r.Data.Message.Proposal
	if r.Data.Thread.Kind != model.ThreadSignature || prop == nil || prop.Owner != ea.Agent().ID {
		t.Fatalf("thread %+v, proposal %+v", r.Data.Thread, prop)
	}
	for i := 1; i < limit; i++ {
		r, err = eb.Discuss(ctx, api.DiscussInput{About: clash.Thread, Message: "Still here."})
		must(t, err)
		flowWant(t, "message within the quota", r.Result, api.OK)
	}
	r, err = eb.Discuss(ctx, api.DiscussInput{About: clash.Thread, Message: "And another thing."})
	must(t, err)
	flowWant(t, "message beyond the quota", r.Result, api.Refused)
	if !strings.Contains(r.Summary, "used your") {
		t.Errorf("quota refusal: %q", r.Summary)
	}

	// a accepts: the thread settles on a, and b may no longer write.
	clk.Advance(6 * time.Second)
	ra, err := ea.Accept(ctx, api.AcceptInput{Proposal: prop.ID})
	must(t, err)
	flowWant(t, "a's accept", ra.Result, api.OK)
	if !ra.Data.Agreed || ra.Data.Thread.Owner != ea.Agent().ID {
		t.Fatalf("after a's accept: %+v", ra.Data)
	}
	clk.Advance(6 * time.Second)
	r, err = eb.Discuss(ctx, api.DiscussInput{About: clash.Thread, Message: "Thanks."})
	must(t, err)
	flowWant(t, "discuss after the agreement", r.Result, api.Refused)
	if !strings.Contains(r.Summary, "settled") || !flowHasTool(r.Options, "status") {
		t.Errorf("settled refusal: %q %+v", r.Summary, r.Options)
	}
}

func TestFlowDismiss(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	p := flowOverlap(t, ctx, f, clk)
	id := p.note.ID

	r, err := p.a.Dismiss(ctx, api.DismissInput{Note: id, As: "whatever"})
	must(t, err)
	flowWant(t, "dismiss with an invalid as", r.Result, api.Refused)
	if !flowHasTool(r.Options, "dismiss", "note", id, "as", "not_needed") || !flowHasTool(r.Options, "dismiss", "note", id, "as", "false_alarm") {
		t.Errorf("invalid as: options %+v", r.Options)
	}

	r, err = p.a.Dismiss(ctx, api.DismissInput{Note: id, As: "not_needed", Reason: "b only renames the path"})
	must(t, err)
	flowWant(t, "dismiss as not_needed", r.Result, api.OK)
	if r.Data.Note != id || r.Data.As != "not_needed" {
		t.Errorf("data %+v", r.Data)
	}
	r, err = p.a.Dismiss(ctx, api.DismissInput{Note: id, As: "not_needed"})
	must(t, err)
	flowWant(t, "dismiss again", r.Result, api.NothingToDo)

	// It may be dismissed again the other way.
	r, err = p.a.Dismiss(ctx, api.DismissInput{Note: id[:6], As: "false_alarm"})
	must(t, err)
	flowWant(t, "dismiss as false_alarm", r.Result, api.OK)
	flowLocked(p.a, func() {
		sn, err := p.a.notes.Note(ctx, p.a.Agent().ID, id)
		must(t, err)
		if sn.DismissedAs != "false_alarm" {
			t.Errorf("ledger: %+v", sn)
		}
	})

	// The dismissed note is no longer open.
	clk.Advance(6 * time.Second)
	st, err := p.a.Status(ctx, api.StatusInput{})
	must(t, err)
	if slices.ContainsFunc(st.Notes, func(n model.Note) bool { return n.ID == id }) {
		t.Errorf("dismissed note still open: %v", noteKinds(st.Notes))
	}

	r, err = p.a.Dismiss(ctx, api.DismissInput{Note: "n-zz", As: "not_needed"})
	must(t, err)
	flowWant(t, "dismiss an invalid id", r.Result, api.Refused)
}

// ---- update ----

func TestFlowUpdateRebasesOntoMain(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})

	_, branch := flowStart(t, ctx, ea, "Use gitlet.json by default")
	r, err := ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update a fresh branch", r.Result, api.NothingToDo)
	if r.Data.Main != runGit(t, f.remote, "rev-parse", "main") {
		t.Errorf("main %q", r.Data.Main)
	}

	flowCommit(t, f.a, "default: use gitlet.json", "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	main := flowMoveMain(t, f, "readme: describe the demo", "README.md", "# demo\n\nA demo.\n")
	clk.Advance(6 * time.Second)
	r, err = ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update after main moved", r.Result, api.OK)
	head := runGit(t, f.a, "rev-parse", "HEAD")
	if !r.Data.Rebased || r.Data.Main != main || r.Data.Head != head {
		t.Errorf("data %+v, HEAD %s, main %s", r.Data, head, main)
	}
	if parent := runGit(t, f.a, "rev-parse", "HEAD^"); parent != main {
		t.Errorf("HEAD^ = %s, want main %s", parent, main)
	}
	if got := runGit(t, f.a, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("on %q after update, want %q", got, branch)
	}
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if len(st.Data.Items) != 1 || st.Data.Items[0].Item.Base != main {
		t.Errorf("item after update: %+v", st.Data.Items)
	}

	r, err = ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update again", r.Result, api.NothingToDo)
}

func TestFlowUpdateRefusals(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})

	flowStart(t, ctx, ea, "Add strict mode to config loading")
	before := flowCommit(t, f.a, "config: strict", "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path, Strict: true}, nil", 1))
	flowMoveMain(t, f, "config: extension", "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path + \".toml\"}, nil", 1))

	// merge-tree predicts the conflict: nothing changes.
	clk.Advance(6 * time.Second)
	r, err := ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update into a conflict", r.Result, api.Refused)
	if !slices.Contains(r.Data.Conflicts, "config.go") || !flowHasCommand(r.Options, "git rebase --autostash origin/main") {
		t.Errorf("conflict refusal: %+v, options %+v", r.Data, r.Options)
	}
	if head := runGit(t, f.a, "rev-parse", "HEAD"); head != before {
		t.Errorf("HEAD moved to %s", head)
	}
	flowLocked(ea, func() {
		if op, err := ea.repo.Operation(ctx); err != nil || op != "" {
			t.Errorf("operation after the refusal: %q %v", op, err)
		}
	})

	// A merge in progress: update says how to finish or abort it.
	runGit(t, f.a, "fetch", "-q", "origin")
	if out, err := exec.Command("git", "-C", f.a, "merge", "-q", "origin/main").CombinedOutput(); err == nil {
		t.Fatalf("the merge did not conflict:\n%s", out)
	}
	r, err = ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update during a merge", r.Result, api.Refused)
	if !strings.Contains(r.Summary, "merge is in progress") || !flowHasCommand(r.Options, "git merge --abort") || !flowHasCommand(r.Options, "git merge --continue") {
		t.Errorf("merge refusal: %q %+v", r.Summary, r.Options)
	}
	s, err := ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit during a merge", s.Result, api.Refused)
	runGit(t, f.a, "merge", "--abort")

	// A detached HEAD is not a branch to rebase.
	runGit(t, f.a, "checkout", "-q", "--detach")
	r, err = ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update on a detached HEAD", r.Result, api.Refused)

	// A second session in the worktree does not rebase the first one's
	// branch.
	openEngine(t, Options{Dir: f.a, Session: true, Now: clk.Now})
	second := openEngine(t, Options{Dir: f.a, Session: true, Now: clk.Now})
	r, err = second.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update in a second session", r.Result, api.Refused)
	if !strings.Contains(r.Summary, "read-only") {
		t.Errorf("summary %q", r.Summary)
	}
}

func TestFlowUpdateAutostash(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})

	flowStart(t, ctx, ea, "Use gitlet.json by default")
	flowCommit(t, f.a, "default: use gitlet.json", "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	local := "# demo\n\nlocal notes\n"
	writeFile(t, f.a, "README.md", local)

	// Uncommitted changes ride along.
	flowMoveMain(t, f, "config: version", "config.go", configGo+"\n// Version is the configuration format.\nconst Version = 1\n")
	clk.Advance(6 * time.Second)
	r, err := ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update with uncommitted changes", r.Result, api.OK)
	if got, _ := os.ReadFile(filepath.Join(f.a, "README.md")); string(got) != local {
		t.Errorf("README.md after update: %q", got)
	}
	if len(r.Warnings) != 0 || runGit(t, f.a, "stash", "list") != "" {
		t.Errorf("warnings %v, stash %q", r.Warnings, runGit(t, f.a, "stash", "list"))
	}

	// Uncommitted changes that conflict with main stay in the stash, and
	// update says so.
	flowMoveMain(t, f, "readme: theirs", "README.md", "# demo\n\ntheirs\n")
	clk.Advance(6 * time.Second)
	r, err = ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update with conflicting uncommitted changes", r.Result, api.OK)
	if !slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, "stash") }) {
		t.Errorf("no autostash warning: %v", r.Warnings)
	}
	if runGit(t, f.a, "stash", "list") == "" {
		t.Error("the autostash is not in the stash")
	}
}

func TestFlowUpdateDropsLandedStub(t *testing.T) {
	requireDeps(t)
	exe := flowGitlet(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now, Exe: exe})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})

	// b publishes Load's new signature with a stub commit.
	ib, _ := flowStart(t, ctx, eb, "Add a strict flag to Load")
	stub := flowCommit(t, f.b, "config: Load takes strict (stub)", "config.go", strictStub)
	cv := flowClaim(t, ctx, eb, ib, stub, "config.go", "Load", true, "")

	// a picks the stub, as use does, and builds on it.
	clk.Advance(6 * time.Second)
	ia, _ := flowStart(t, ctx, ea, "Load strict configs by default")
	runGit(t, f.a, "cherry-pick", "-x", stub)
	picked := runGit(t, f.a, "rev-parse", "HEAD")
	flowAddNeed(t, ctx, ea, model.Need{ID: model.NewMessageID(), Key: cv.Claim.Key, Name: "config.go:Load", Item: ia,
		PinnedSigHash: cv.Claim.Contract.SigHash, PinnedStub: stub, PinnedCommit: picked, CreatedAt: clk.Now().UTC()})
	flowCommit(t, f.a, "default: strict", "default.go", strings.Replace(mainGo, `Load("gitlet.toml")`, `Load("gitlet.toml", true)`, 1))

	// The implementation lands on main, in a different form than the stub.
	main := flowMoveMain(t, f, "config: strict Load", "config.go", strictLoad)

	clk.Advance(6 * time.Second)
	r, err := ea.Update(ctx, api.UpdateInput{})
	must(t, err)
	flowWant(t, "update over a landed stub", r.Result, api.OK)
	if !r.Data.Rebased || !slices.Equal(r.Data.Dropped, []string{picked}) {
		t.Fatalf("data %+v", r.Data)
	}
	if parent := runGit(t, f.a, "rev-parse", "HEAD^"); parent != main {
		t.Errorf("HEAD^ = %s, want main %s: the stub was not dropped", parent, main)
	}
	if got := runGit(t, f.a, "show", "HEAD:config.go"); got != strings.TrimSpace(strictLoad) {
		t.Errorf("config.go after update:\n%s", got)
	}
	if runGit(t, f.a, "status", "--porcelain") != "" {
		t.Error("the work tree is not clean")
	}
	// The need is done: its contract is on main.
	flowLocked(ea, func() {
		if needs := ea.myOutbox().Needs; len(needs) != 0 {
			t.Errorf("needs after the contract landed: %+v", needs)
		}
	})
}

// ---- submit ----

func TestFlowSubmitRefusesUnreadyWork(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})

	r, err := ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit without a task", r.Result, api.Refused)

	_, branch := flowStart(t, ctx, ea, "Add strict mode to config loading")
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path, Strict: true}, nil", 1))
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit with uncommitted changes", r.Result, api.Refused)
	if len(r.Data.Checks) != 1 || r.Data.Checks[0].Name != "committed" || r.Data.Checks[0].OK || !flowHasCommand(r.Options, "git status") {
		t.Errorf("checks %+v, options %+v", r.Data.Checks, r.Options)
	}
	flowCommit(t, f.a, "config: strict")
	writeFile(t, f.a, "scratch.txt", "untracked\n")
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit with an untracked file", r.Result, api.Refused)
	must(t, os.Remove(filepath.Join(f.a, "scratch.txt")))

	// main changed the same line: update first.
	flowMoveMain(t, f, "config: extension", "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path + \".toml\"}, nil", 1))
	clk.Advance(6 * time.Second)
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit into a conflict", r.Result, api.Refused)
	if !slices.Contains(r.Data.Conflicts, "config.go") || !flowHasTool(r.Options, "update") {
		t.Errorf("conflict refusal: %+v %+v", r.Data, r.Options)
	}
	if len(r.Data.Checks) != 2 || !r.Data.Checks[0].OK || r.Data.Checks[1].OK {
		t.Errorf("checks %+v", r.Data.Checks)
	}
	if out := runGit(t, f.remote, "for-each-ref", "refs/heads/"+branch); out != "" {
		t.Errorf("a refused submit pushed: %s", out)
	}
}

func TestFlowSubmitChecksContracts(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})

	// b published Load's new signature, with an interface pull request.
	ib, _ := flowStart(t, ctx, eb, "Add a strict flag to Load")
	stub := flowCommit(t, f.b, "config: Load takes strict (stub)", "config.go", strictStub)
	const interfacePR = "https://github.com/acme/widget/pull/3"
	cv := flowClaim(t, ctx, eb, ib, stub, "config.go", "Load", true, interfacePR)

	// a builds on it before it landed.
	clk.Advance(6 * time.Second)
	ia, _ := flowStart(t, ctx, ea, "Load strict configs by default")
	runGit(t, f.a, "cherry-pick", "-x", stub)
	need := model.Need{ID: model.NewMessageID(), Key: cv.Claim.Key, Name: "config.go:Load", Item: ia,
		PinnedSigHash: cv.Claim.Contract.SigHash, PinnedStub: stub, PinnedCommit: runGit(t, f.a, "rev-parse", "HEAD"), CreatedAt: clk.Now().UTC()}
	flowAddNeed(t, ctx, ea, need)
	flowCommit(t, f.a, "default: strict", "default.go", strings.Replace(mainGo, `Load("gitlet.toml")`, `Load("gitlet.toml", true)`, 1))

	r, err := ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit before the contract landed", r.Result, api.Refused)
	if !strings.Contains(r.Summary, string(eb.Agent().ID)) || !strings.Contains(r.Summary, interfacePR) {
		t.Errorf("summary %q", r.Summary)
	}
	if !flowHasTool(r.Options, "release", "symbol", "config.go:Load") ||
		!(flowHasTool(r.Options, "discuss") || flowHasTool(r.Options, "who", "target", "config.go:Load")) {
		t.Errorf("options %+v", r.Options)
	}

	// a published Default's signature, but its code says otherwise.
	flowLocked(ea, func() {
		_, err := ea.mutateOutbox(ctx, func(ob *model.Outbox) error {
			ob.Needs = nil
			return nil
		})
		must(t, err)
	})
	base := runGit(t, f.a, "rev-parse", "HEAD")
	published := flowCommit(t, f.a, "default: takes a name (stub)", "default.go",
		"package config\n\n// Default loads the named default configuration.\nfunc Default(name string) (*Config, error) {\n\treturn Load(name, true)\n}\n")
	flowClaim(t, ctx, ea, ia, published, "default.go", "Default", true, "")
	runGit(t, f.a, "reset", "-q", "--hard", base)
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit with a contract the code breaks", r.Result, api.Refused)
	if !flowHasTool(r.Options, "publish", "symbol", "default.go:Default") {
		t.Errorf("options %+v", r.Options)
	}
	if i := slices.IndexFunc(r.Data.Checks, func(c api.Check) bool { return c.Name == "contracts owned" }); i < 0 || r.Data.Checks[i].OK {
		t.Errorf("checks %+v", r.Data.Checks)
	}
}

func TestFlowSubmitPushesAndOpensPullRequest(t *testing.T) {
	requireDeps(t)
	ghLog := flowFakeGh(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})

	item, branch := flowStart(t, ctx, ea, "Use gitlet.json by default")
	head := flowCommit(t, f.a, "default: use gitlet.json", "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	r, err := ea.Submit(ctx, api.SubmitInput{Body: "Closes #12."})
	must(t, err)
	flowWant(t, "submit", r.Result, api.OK)
	if !r.Data.Pushed || r.Data.PR != flowPRURL || r.Data.Branch != branch || r.Data.CompareURL != "" {
		t.Errorf("data %+v", r.Data)
	}
	var names []string
	for _, c := range r.Data.Checks {
		names = append(names, c.Name)
	}
	if want := []string{"committed", "merge", "contracts used", "contracts owned", "main's changes", "act notes", "push", "pull request"}; !slices.Equal(names, want) {
		t.Errorf("checks %v, want %v", names, want)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != head {
		t.Errorf("remote %s at %s, want %s", branch, got, head)
	}
	logged, err := os.ReadFile(ghLog)
	must(t, err)
	call := string(logged)
	for _, want := range []string{
		"pr\nview\n" + branch + "\n--json\nurl,state\n",
		"pr\ncreate\n--base\nmain\n--head\n" + branch + "\n--title\nUse gitlet.json by default\n--body\nCloses #12.\n",
		"\nTask: Use gitlet.json by default\n",
		"\nGitlet-Item: " + string(item) + "\nGitlet-Agent: " + string(ea.Agent().ID) + "\n",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("gh calls lack %q:\n%s", want, call)
		}
	}
	if strings.Contains(call, "--draft") {
		t.Error("a draft was asked for")
	}
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if len(st.Data.Items) != 1 || st.Data.Items[0].Item.Status != model.ItemSubmitted || st.Data.Items[0].Item.PR != flowPRURL {
		t.Fatalf("item after submit: %+v", st.Data.Items)
	}

	// A review fix: a fast-forward, and the pull request is already open.
	head = flowCommit(t, f.a, "default: document gitlet.json", "README.md", "# demo\n\nReads gitlet.json.\n")
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit a review fix", r.Result, api.OK)
	if r.Data.PR != flowPRURL || !strings.Contains(r.Summary, "its pull request is") {
		t.Errorf("second submit: %q %+v", r.Summary, r.Data)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != head {
		t.Errorf("remote %s at %s after the fix, want %s", branch, got, head)
	}

	// Rewritten history goes out with a lease on what this clone pushed.
	runGit(t, f.a, "commit", "-q", "--amend", "-m", "readme: document gitlet.json")
	head = runGit(t, f.a, "rev-parse", "HEAD")
	clk.Advance(6 * time.Second)
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit rewritten history", r.Result, api.OK)
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != head {
		t.Errorf("remote %s at %s after the amend, want %s", branch, got, head)
	}

	// Someone else pushed to the branch: submit does not overwrite it.
	runGit(t, f.b, "fetch", "-q", "origin", branch)
	runGit(t, f.b, "checkout", "-q", "-b", "fix", "FETCH_HEAD")
	theirs := flowCommit(t, f.b, "readme: typo", "README.md", "# demo\n\nReads gitlet.json, always.\n")
	runGit(t, f.b, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	runGit(t, f.a, "commit", "-q", "--amend", "-m", "readme: document the gitlet.json default")
	clk.Advance(6 * time.Second)
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit over someone else's push", r.Result, api.Refused)
	if !strings.Contains(r.Summary, "someone else pushed") {
		t.Errorf("summary %q", r.Summary)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != theirs {
		t.Errorf("remote %s at %s, want theirs %s", branch, got, theirs)
	}
}

func TestFlowSubmitCompareURLWithoutGh(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	flowForgeRemote(t, f, f.a)
	flowPath(t, "")
	ctx := ctxT(t)
	ea := openEngine(t, Options{Dir: f.a})

	_, branch := flowStart(t, ctx, ea, "Use gitlet.json by default")
	head := flowCommit(t, f.a, "default: use gitlet.json", "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	r, err := ea.Submit(ctx, api.SubmitInput{Draft: true})
	must(t, err)
	flowWant(t, "submit without gh", r.Result, api.OK)
	want := "https://github.com/acme/widget/compare/main..." + branch + "?expand=1"
	if !r.Data.Pushed || r.Data.PR != "" || r.Data.CompareURL != want || !strings.Contains(r.Summary, want) {
		t.Errorf("submit without gh: %q %+v", r.Summary, r.Data)
	}
	if last := r.Data.Checks[len(r.Data.Checks)-1]; last.Name != "pull request" || last.OK {
		t.Errorf("last check %+v", last)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != head {
		t.Errorf("remote %s at %s, want %s", branch, got, head)
	}

	// A gh that fails on a GitHub remote is a warning, and the compare URL
	// still comes back.
	flowPath(t, "#!/bin/sh\necho 'To get started with GitHub CLI, please run:  gh auth login' >&2\nexit 4\n")
	flowCommit(t, f.a, "readme: gitlet.json", "README.md", "# demo\n\nReads gitlet.json.\n")
	r, err = ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit with a failing gh", r.Result, api.OK)
	if r.Data.CompareURL != want || !slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, "gh auth login") }) {
		t.Errorf("submit with a failing gh: %+v, warnings %v", r.Data, r.Warnings)
	}
}

func TestFlowSubmitWithoutRemote(t *testing.T) {
	requireDeps(t)
	ctx := ctxT(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	runGit(t, dir, "init", "-q")
	flowCommit(t, dir, "initial", "config.go", configGo, "default.go", mainGo)
	ea := openEngine(t, Options{Dir: dir})

	item, _ := flowStart(t, ctx, ea, "Use gitlet.json by default")
	flowCommit(t, dir, "default: use gitlet.json", "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	r, err := ea.Submit(ctx, api.SubmitInput{})
	must(t, err)
	flowWant(t, "submit without a remote", r.Result, api.OK)
	if r.Data.Pushed || r.Data.PR != "" || !strings.Contains(r.Summary, "no remote") {
		t.Errorf("%q %+v", r.Summary, r.Data)
	}
	flowLocked(ea, func() {
		if _, it, _ := ea.findItem(item); it.Status != model.ItemSubmitted {
			t.Errorf("item %+v", it)
		}
	})
}

func TestFlowSubmitInterface(t *testing.T) {
	requireDeps(t)
	ghLog := flowFakeGh(t)
	f := newFixture(t)
	ctx := ctxT(t)
	ea := openEngine(t, Options{Dir: f.a})

	item, _ := flowStart(t, ctx, ea, "Add a strict flag to Load")
	stub := flowCommit(t, f.a, "config: Load takes strict (stub)", "config.go", strictStub)

	r, err := ea.Submit(ctx, api.SubmitInput{Symbol: "config.go:Load"})
	must(t, err)
	flowWant(t, "interface submit before publishing", r.Result, api.Refused)
	if !flowHasTool(r.Options, "publish", "symbol", "config.go:Load") {
		t.Errorf("options %+v", r.Options)
	}

	cv := flowClaim(t, ctx, ea, item, stub, "config.go", "Load", true, "")
	r, err = ea.Submit(ctx, api.SubmitInput{Symbol: "config.go:Load"})
	must(t, err)
	flowWant(t, "interface submit", r.Result, api.OK)
	branch := "gitlet/interface-config-go-load"
	if r.Data.Branch != branch || !r.Data.Pushed || r.Data.PR != flowPRURL {
		t.Errorf("data %+v", r.Data)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+branch); got != stub {
		t.Errorf("remote %s at %s, want the stub %s", branch, got, stub)
	}
	logged, err := os.ReadFile(ghLog)
	must(t, err)
	if call := string(logged); !strings.Contains(call, "--title\ninterface: config.go:Load\n") || strings.Contains(call, "Gitlet-Item:") {
		t.Errorf("gh calls:\n%s", call)
	}
	flowLocked(ea, func() {
		if now, ok := ea.claimFor(cv.Claim.Key); !ok || now.Claim.Contract == nil || now.Claim.Contract.PR != flowPRURL {
			t.Errorf("claim after the interface pull request: %+v", now.Claim)
		}
	})

	r, err = ea.Submit(ctx, api.SubmitInput{Symbol: "config.go:Load"})
	must(t, err)
	flowWant(t, "interface submit again", r.Result, api.NothingToDo)
}

// ---- abandon ----

func TestFlowAbandon(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	p := flowOverlap(t, ctx, f, clk)
	ea := p.a

	r, err := ea.Discuss(ctx, api.DiscussInput{About: p.note.ID, Message: "Who changes Load?"})
	must(t, err)
	flowWant(t, "discuss", r.Result, api.OK)
	thread := r.Data.Thread.ID
	head := flowCommit(t, f.a, "config: strict")
	flowClaim(t, ctx, ea, p.ia, head, "default.go", "Default", false, "")
	flowAddNeed(t, ctx, ea, model.Need{ID: model.NewMessageID(), Key: model.SymbolKey("go", "config.go", "Config.Validate"),
		Name: "config.go:Config.Validate", Item: p.ia, CreatedAt: clk.Now().UTC()})
	if refs := runGit(t, f.remote, "for-each-ref", bus.ClaimsPrefix); refs == "" {
		t.Fatal("no claim on the remote")
	}

	ab, err := ea.Abandon(ctx, api.AbandonInput{Reason: "b covers it"})
	must(t, err)
	flowWant(t, "abandon", ab.Result, api.OK)
	if ab.Data.Item != p.ia || !slices.Contains(ab.Data.Released, "claim default.go:Default") || !slices.Contains(ab.Data.Released, "need config.go:Config.Validate") {
		t.Errorf("data %+v", ab.Data)
	}
	// The branch and its commits stay; the claim and the snapshot go.
	if got := runGit(t, f.a, "rev-parse", "refs/heads/"+p.branch); got != head {
		t.Errorf("branch %s at %s, want %s", p.branch, got, head)
	}
	if refs := runGit(t, f.remote, "for-each-ref", bus.ClaimsPrefix, bus.WipRef(ea.Agent().ID, p.ia)); refs != "" {
		t.Errorf("refs left on the remote:\n%s", refs)
	}
	flowLocked(ea, func() {
		if _, it, _ := ea.findItem(p.ia); it.Status != model.ItemAbandoned || it.ClosedAt == nil {
			t.Errorf("item %+v", it)
		}
		told := slices.ContainsFunc(ea.myOutbox().Messages, func(m model.Message) bool {
			return m.Thread == thread && strings.HasPrefix(m.Text, "abandoned:") && strings.Contains(m.Text, "b covers it")
		})
		if !told || len(ea.myOutbox().Needs) != 0 {
			t.Errorf("outbox %+v", ea.myOutbox())
		}
	})

	again, err := ea.Abandon(ctx, api.AbandonInput{})
	must(t, err)
	flowWant(t, "abandon again", again.Result, api.NothingToDo)
}
