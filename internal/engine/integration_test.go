package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// The tests in this file run the engine on real git: a bare remote and two
// clones of it, one per agent.

// requireDeps skips the test when git is not installed.
func requireDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
}

// must fails the test on err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- fixture ----

// clock is an adjustable time source for Options.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const configGo = `package config

// Config is the loaded configuration.
type Config struct {
	Path   string
	Strict bool
}

// Load reads the configuration at path.
func Load(path string) (*Config, error) {
	return &Config{Path: path}, nil
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	return nil
}
`

const mainGo = `package config

// Default loads the default configuration.
func Default() (*Config, error) {
	return Load("gitlet.toml")
}
`

// fixture is a bare remote with two clones, a and b.
type fixture struct {
	root, remote, a, b string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		root:   root,
		remote: filepath.Join(root, "remote.git"),
		a:      filepath.Join(root, "a"),
		b:      filepath.Join(root, "b"),
	}
	seed := filepath.Join(root, "seed")
	runGit(t, root, "init", "-q", "--bare", f.remote)
	runGit(t, root, "init", "-q", seed)
	writeFile(t, seed, "config.go", configGo)
	writeFile(t, seed, "default.go", mainGo)
	writeFile(t, seed, "README.md", "# demo\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-q", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", f.remote)
	runGit(t, seed, "push", "-q", "origin", "main")
	runGit(t, f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, root, "clone", "-q", f.remote, f.a)
	runGit(t, root, "clone", "-q", f.remote, f.b)
	return f
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// openEngine opens an engine on dir and closes it with the test.
func openEngine(t *testing.T, opt Options) *Engine {
	t.Helper()
	if opt.Client == "" {
		opt.Client = "test"
	}
	e, err := Open(context.Background(), opt)
	must(t, err)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// ---- tests ----

func TestOpenReusesAgentID(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	e1 := openEngine(t, Options{Dir: filepath.Join(f.a)})
	id := e1.Agent().ID
	if !model.ValidAgentID(id) || !strings.HasPrefix(string(id), "test-") {
		t.Fatalf("agent id %q", id)
	}
	if e1.Root() != f.a {
		t.Errorf("Root() = %q, want %q", e1.Root(), f.a)
	}
	if e1.ReadOnly() {
		t.Error("a lone engine is read-only")
	}
	if e1.Config().Remote != "origin" || e1.Config().Main != "main" {
		t.Errorf("config: remote %q main %q", e1.Config().Remote, e1.Config().Main)
	}
	must(t, e1.Close())
	if _, err := e1.Status(ctx, api.StatusInput{}); !errors.Is(err, errClosed) {
		t.Errorf("verb after Close: %v", err)
	}

	// A subdirectory of the same clone gets the same agent.
	if err := os.MkdirAll(filepath.Join(f.a, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	e2 := openEngine(t, Options{Dir: filepath.Join(f.a, "sub")})
	if e2.Agent().ID != id {
		t.Errorf("reopened agent %q, want %q", e2.Agent().ID, id)
	}
	// The other clone is another agent.
	eb := openEngine(t, Options{Dir: f.b})
	if eb.Agent().ID == id {
		t.Error("two clones share an agent id")
	}
	// Options.Agent overrides the stored id.
	e3 := openEngine(t, Options{Dir: f.a, Agent: "override-agent-1"})
	if e3.Agent().ID != "override-agent-1" {
		t.Errorf("override: %q", e3.Agent().ID)
	}
	if _, err := Open(ctx, Options{Dir: f.a, Agent: "Not Valid!"}); err == nil {
		t.Error("an invalid agent id was accepted")
	}
}

func TestSessionLockMakesSecondEngineReadOnly(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	first := openEngine(t, Options{Dir: f.a, Session: true})
	second := openEngine(t, Options{Dir: f.a, Session: true})
	if first.ReadOnly() {
		t.Fatal("the session holder is read-only")
	}
	if !second.ReadOnly() {
		t.Fatal("a second session in the same worktree is not read-only")
	}
	if second.Agent().ID != first.Agent().ID {
		t.Errorf("same worktree, different agents: %s %s", first.Agent().ID, second.Agent().ID)
	}

	r, err := second.Status(ctx, api.StatusInput{})
	must(t, err)
	if !r.Data.ReadOnly {
		t.Error("status does not say read-only")
	}
	var sw *model.Note
	for i, n := range r.Notes {
		if n.Kind == model.NoteSameWorktree {
			sw = &r.Notes[i]
		}
	}
	if sw == nil {
		t.Fatalf("no same_worktree note in %+v", r.Notes)
	}
	cmd := ""
	for _, o := range sw.Options {
		if o.Command != "" {
			cmd = o.Command
		}
	}
	if !strings.Contains(cmd, "git worktree add") {
		t.Errorf("same_worktree note offers %+v", sw.Options)
	}

	s, err := second.Start(ctx, api.StartInput{Intent: "Do something"})
	must(t, err)
	if s.Outcome != api.Refused || len(s.Options) == 0 {
		t.Errorf("start in a read-only session: %s %q", s.Outcome, s.Summary)
	}

	// Once the holder is gone, a new session gets the lock.
	must(t, first.Close())
	third := openEngine(t, Options{Dir: f.a, Session: true})
	if third.ReadOnly() {
		t.Error("the session lock was not released by Close")
	}
}

func TestStartOnMainCreatesBranchVisibleToOthers(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	ea := openEngine(t, Options{Dir: f.a})
	eb := openEngine(t, Options{Dir: f.b})

	// Agent a changes Load before stating its task.
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path, Strict: true}, nil", 1))

	r, err := ea.Start(ctx, api.StartInput{Intent: "Add strict mode to config loading", Paths: []string{"config.go"}})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	if !r.Data.NewBranch || !strings.HasPrefix(r.Data.Branch, "gitlet/add-strict-mode") {
		t.Errorf("branch %q (new %v)", r.Data.Branch, r.Data.NewBranch)
	}
	if got := runGit(t, f.a, "rev-parse", "--abbrev-ref", "HEAD"); got != r.Data.Branch {
		t.Errorf("worktree is on %q, want %q", got, r.Data.Branch)
	}
	if !model.ValidItemID(r.Data.Item.ID) || r.Data.Item.Status != model.ItemActive || r.Data.Item.Provisional {
		t.Errorf("item %+v", r.Data.Item)
	}
	if text := render.Text(&r.Result, render.CLI); !strings.Contains(text, "ok") {
		t.Errorf("rendered start:\n%s", text)
	}

	// A second start while the item is active is refused with a way out.
	again, err := ea.Start(ctx, api.StartInput{Intent: "Something else"})
	must(t, err)
	if again.Outcome != api.Refused || len(again.Options) == 0 {
		t.Errorf("second start: %s %q", again.Outcome, again.Summary)
	}

	// The remote has a's state, and b sees it.
	refs := runGit(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/gitlet/")
	if !strings.Contains(refs, string(ea.Agent().ID)) {
		t.Fatalf("remote refs:\n%s", refs)
	}
	st, err := eb.Status(ctx, api.StatusInput{})
	must(t, err)
	var peer *api.PeerInfo
	for i, p := range st.Data.Others {
		if p.Agent.ID == ea.Agent().ID {
			peer = &st.Data.Others[i]
		}
	}
	if peer == nil {
		t.Fatalf("b does not see a: %+v", st.Data.Others)
	}
	if len(peer.Items) != 1 || peer.Items[0] != "Add strict mode to config loading" {
		t.Errorf("b sees a's items as %v", peer.Items)
	}

	// b asks who touches Load and config.go.
	who, err := eb.Who(ctx, api.WhoInput{Target: "config.go:Load"})
	must(t, err)
	if who.Outcome != api.OK || !hasEntry(who.Data.Entries, ea.Agent().ID, "changes") {
		t.Errorf("who config.go:Load: %s %q %+v", who.Outcome, who.Summary, who.Data.Entries)
	}
	who, err = eb.Who(ctx, api.WhoInput{Target: "config.go"})
	must(t, err)
	if !hasEntry(who.Data.Entries, ea.Agent().ID, "changes") {
		t.Errorf("who config.go: %+v", who.Data.Entries)
	}
}

func hasEntry(entries []api.WhoEntry, agent model.AgentID, role string) bool {
	for _, e := range entries {
		if e.Agent == agent && e.Role == role {
			return true
		}
	}
	return false
}

func TestTickOpensProvisionalItem(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	runGit(t, f.a, "checkout", "-q", "-b", "feature/fix-login")
	ea := openEngine(t, Options{Dir: f.a})
	must(t, ea.Tick(ctx)) // first look: nothing changed yet
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if len(st.Data.Items) != 0 {
		t.Fatalf("items before any change: %+v", st.Data.Items)
	}

	writeFile(t, f.a, "default.go", strings.Replace(mainGo, "gitlet.toml", "login.toml", 1))
	must(t, ea.Tick(ctx))
	st, err = ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if len(st.Data.Items) != 1 {
		t.Fatalf("items after a change: %+v", st.Data.Items)
	}
	it := st.Data.Items[0].Item
	if !it.Provisional || it.Intent != "fix login" || it.Branch != "feature/fix-login" || it.Status != model.ItemActive {
		t.Errorf("provisional item %+v", it)
	}

	// start adopts it.
	r, err := ea.Start(ctx, api.StartInput{Intent: "Fix the login defaults"})
	must(t, err)
	if r.Outcome != api.OK || !r.Data.Adopted || r.Data.Item.ID != it.ID || r.Data.Item.Provisional || r.Data.NewBranch {
		t.Errorf("start after a provisional item: %s %q %+v", r.Outcome, r.Summary, r.Data)
	}
}

func TestStatusWhoWhy(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	ea := openEngine(t, Options{Dir: f.a})
	eb := openEngine(t, Options{Dir: f.b})

	r, err := ea.Start(ctx, api.StartInput{Intent: "Validate strict configs"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %s %q", r.Outcome, r.Summary)
	}
	item := r.Data.Item.ID
	changed := strings.Replace(configGo, "\treturn nil\n}", "\tif c.Strict && c.Path == \"\" {\n\t\treturn errStrict\n\t}\n\treturn nil\n}", 1)
	changed = strings.Replace(changed, "Validate() error {", "Validate() (err error) {", 1)
	writeFile(t, f.a, "config.go", changed)
	runGit(t, f.a, "commit", "-q", "-am", "config: validate strict configs\n\nGitlet-Item: "+string(item)+"\nGitlet-Agent: "+string(ea.Agent().ID))

	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if st.Outcome != api.OK || len(st.Data.Items) != 1 || st.Data.Items[0].Item.ID != item {
		t.Fatalf("status: %s %q %+v", st.Outcome, st.Summary, st.Data.Items)
	}
	if !st.AllNotes || st.Data.Main == "" || st.Data.Agent.ID != ea.Agent().ID {
		t.Errorf("status envelope: all %v main %q agent %q", st.AllNotes, st.Data.Main, st.Data.Agent.ID)
	}
	if st.StateAge == "" {
		t.Error("status reports no state age")
	}
	if text := render.Text(&st.Result, render.MCP); !strings.Contains(text, "Validate strict configs") {
		t.Errorf("rendered status lacks the intent:\n%s", text)
	}

	// why on a changed line explains it with the item.
	line := 0
	for i, l := range strings.Split(runGit(t, f.a, "show", "HEAD:config.go"), "\n") {
		if strings.Contains(l, "errStrict") {
			line = i + 1
			break
		}
	}
	why, err := ea.Why(ctx, api.WhyInput{Target: fmt.Sprintf("config.go:%d", line)})
	must(t, err)
	if why.Outcome != api.OK || why.Data.Item != item || why.Data.Agent != ea.Agent().ID || why.Data.Intent != "Validate strict configs" {
		t.Errorf("why config.go:%d: %s %q %+v", line, why.Outcome, why.Summary, why.Data)
	}
	if why.Data.Subject != "config: validate strict configs" || why.Data.Commit == "" {
		t.Errorf("why commit: %+v", why.Data)
	}
	// why on a symbol blames its first line.
	why, err = ea.Why(ctx, api.WhyInput{Target: "Config.Validate"})
	must(t, err)
	if why.Outcome != api.OK || why.Data.Item != item {
		t.Errorf("why Config.Validate: %s %q %+v", why.Outcome, why.Summary, why.Data)
	}
	// An unknown symbol is refused with candidates or a way out.
	why, err = ea.Why(ctx, api.WhyInput{Target: "Nope.Missing"})
	must(t, err)
	if why.Outcome != api.Refused || len(why.Options) == 0 {
		t.Errorf("why on an unknown symbol: %s %q", why.Outcome, why.Summary)
	}

	// b sees a's item on Validate once a's snapshot is published.
	who, err := eb.Who(ctx, api.WhoInput{Target: "config.go:Config.Validate"})
	must(t, err)
	if !hasEntry(who.Data.Entries, ea.Agent().ID, "changes") {
		t.Errorf("who from b: %s %q %+v", who.Outcome, who.Summary, who.Data.Entries)
	}

	// Call decodes JSON and refuses unknown fields as a usage problem.
	out, err := eb.Call(ctx, "who", []byte(`{"target":"config.go"}`))
	must(t, err)
	if out.Envelope().Outcome != api.OK {
		t.Errorf("Call who: %+v", out.Envelope())
	}
	out, err = eb.Call(ctx, "who", []byte(`{"target":"config.go","bogus":1}`))
	must(t, err)
	if env := out.Envelope(); env.Outcome != api.Refused || len(env.Options) == 0 {
		t.Errorf("Call with an unknown field: %+v", env)
	}
	out, err = eb.Call(ctx, "frobnicate", nil)
	must(t, err)
	if out.Envelope().Outcome != api.Refused {
		t.Errorf("Call of an unknown verb: %+v", out.Envelope())
	}
	out, err = eb.Call(ctx, "status", api.StatusInput{Where: api.Where{Worktree: f.a}})
	must(t, err)
	if out.Envelope().Outcome != api.Refused {
		t.Errorf("Call for another worktree: %+v", out.Envelope())
	}
}

func TestVerbRefreshRule(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	if _, err := ea.Status(ctx, api.StatusInput{}); err != nil {
		must(t, err)
	}
	if n := ea.fetches.Load(); n != 1 {
		t.Fatalf("first verb fetched %d times, want 1", n)
	}
	clk.Advance(3 * time.Second)
	if _, err := ea.Who(ctx, api.WhoInput{Target: "config.go"}); err != nil {
		must(t, err)
	}
	if n := ea.fetches.Load(); n != 1 {
		t.Errorf("a verb 3s after a fetch fetched again (%d)", n)
	}
	clk.Advance(3 * time.Second)
	if _, err := ea.Who(ctx, api.WhoInput{Target: "config.go"}); err != nil {
		must(t, err)
	}
	if n := ea.fetches.Load(); n != 2 {
		t.Errorf("a verb 6s after a fetch did not fetch (%d)", n)
	}
}

func TestVerbRefreshBudget(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	// A git daemon that accepts connections and never answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	var conns sync.WaitGroup
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				<-done
				c.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		close(done)
		ln.Close()
		conns.Wait()
	})
	runGit(t, f.a, "remote", "set-url", "origin", "git://"+ln.Addr().String()+"/remote.git")

	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	status := func(wantFetches int64, maxTime time.Duration) *api.Reply[api.StatusData] {
		t.Helper()
		begin := time.Now()
		r, err := ea.Status(ctx, api.StatusInput{})
		elapsed := time.Since(begin)
		must(t, err)
		if r.Outcome != api.OK {
			t.Errorf("status: %s %q", r.Outcome, r.Summary)
		}
		if n := ea.fetches.Load(); n != wantFetches {
			t.Errorf("fetched %d times, want %d", n, wantFetches)
		}
		if elapsed > maxTime {
			t.Errorf("status took %v, want at most %v", elapsed, maxTime)
		}
		return r
	}
	slowWarning := func(r *api.Reply[api.StatusData]) bool {
		for _, w := range r.Warnings {
			if strings.Contains(w, "did not answer") {
				return true
			}
		}
		return false
	}

	// The first verb spends its budget on the hanging remote.
	r := status(1, verbRefreshBudget+3*time.Second)
	if r.OfflineSince != nil {
		t.Errorf("a slow remote is not offline: %v", r.OfflineSince)
	}
	if !slowWarning(r) {
		t.Errorf("no warning about the slow remote: %q", r.Warnings)
	}
	// The next verbs use the cached state until the backoff has passed.
	r = status(1, time.Second)
	if !slowWarning(r) {
		t.Errorf("no warning about the slow remote: %q", r.Warnings)
	}
	clk.Advance(time.Minute)
	status(2, verbRefreshBudget+3*time.Second)

	// Once a tick reaches the remote again, verbs refresh as usual.
	runGit(t, f.a, "remote", "set-url", "origin", f.remote)
	must(t, ea.Tick(ctx))
	if n := ea.fetches.Load(); n != 3 {
		t.Fatalf("tick fetched %d times in all, want 3", n)
	}
	clk.Advance(6 * time.Second)
	r = status(4, 5*time.Second)
	if slowWarning(r) || r.OfflineSince != nil {
		t.Errorf("after a good fetch: offline %v, warnings %q", r.OfflineSince, r.Warnings)
	}
}

// testConfig is a configuration with a short lease and poll interval.
func testConfig() *config.Config {
	c := config.Defaults()
	c.Remote, c.Main = "origin", "main"
	c.Lease = time.Minute
	c.PollInterval = 10 * time.Second
	return &c
}

func hasNoteKind(notes []model.Note, kinds ...model.NoteKind) bool {
	for _, n := range notes {
		for _, k := range kinds {
			if n.Kind == k {
				return true
			}
		}
	}
	return false
}

func noteKinds(notes []model.Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, string(n.Kind)+": "+n.Title)
	}
	return out
}

func TestTwoAgentsOnTheSameCodeGetNotes(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})

	writeFile(t, f.a, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path, Strict: true}, nil", 1))
	writeFile(t, f.b, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path + \".toml\"}, nil", 1))
	ra, err := ea.Start(ctx, api.StartInput{Intent: "Add strict mode to config loading"})
	must(t, err)
	rb, err := eb.Start(ctx, api.StartInput{Intent: "Append the extension to config paths"})
	must(t, err)
	if ra.Outcome != api.OK || rb.Outcome != api.OK {
		t.Fatalf("start: %q / %q", ra.Summary, rb.Summary)
	}
	// b started after a and had already edited config.go, so without
	// naming paths it sees a changing it.
	if !hasEntry(rb.Data.Touching, ea.Agent().ID, "changes") {
		t.Errorf("b's start shows nobody touching its paths: %+v", rb.Data.Touching)
	}

	clk.Advance(30 * time.Second)
	must(t, ea.Tick(ctx))
	must(t, eb.Tick(ctx))
	clk.Advance(6 * time.Second)
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	// Both edit Load's body, and merge-tree predicts the conflict.
	conflict := false
	for _, n := range st.Notes {
		conflict = conflict || n.Kind == model.NotePredictedConflict ||
			n.Kind == model.NoteBodyOverlap && strings.Contains(n.Title, "conflict")
	}
	if !hasNoteKind(st.Notes, model.NoteBodyOverlap) || !conflict {
		t.Errorf("a's notes: %v", noteKinds(st.Notes))
	}
	if st.Open == 0 || st.Open != len(st.Notes) {
		t.Errorf("open %d with %d notes listed", st.Open, len(st.Notes))
	}
	if len(st.Data.Items) != 1 || len(st.Data.Items[0].Overlaps) == 0 {
		t.Errorf("a's item shows no overlap: %+v", st.Data.Items)
	}
	// A later result does not repeat the notes status showed.
	who, err := ea.Who(ctx, api.WhoInput{Target: "config.go:Load"})
	must(t, err)
	if len(who.Notes) != 0 {
		t.Errorf("notes repeated after status: %v", noteKinds(who.Notes))
	}
	// who lists the others, not the caller.
	if !hasEntry(who.Data.Entries, eb.Agent().ID, "changes") || len(distinctAgents(who.Data.Entries)) != 1 {
		t.Errorf("who config.go:Load: %+v", who.Data.Entries)
	}
}

func TestBreakingChangeIsClaimedAutomatically(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})

	// a changes Load's signature; b writes a new caller of Load.
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {\n\treturn &Config{Path: path}, nil",
		"func Load(path string, strict bool) (*Config, error) {\n\treturn &Config{Path: path, Strict: strict}, nil", 1))
	writeFile(t, f.b, "default.go", mainGo+"\n// Strict loads the strict configuration.\nfunc Strict() (*Config, error) {\n\treturn Load(\"strict.toml\")\n}\n")
	ra, err := ea.Start(ctx, api.StartInput{Intent: "Add a strict flag to Load"})
	must(t, err)
	rb, err := eb.Start(ctx, api.StartInput{Intent: "Add a strict default"})
	must(t, err)
	if ra.Outcome != api.OK || rb.Outcome != api.OK {
		t.Fatalf("start: %q / %q", ra.Summary, rb.Summary)
	}

	clk.Advance(time.Minute)
	must(t, ea.Tick(ctx))
	clk.Advance(6 * time.Second)
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	var claim *api.ClaimInfo
	for i, c := range st.Data.Claims {
		if c.Symbol == "config.go:Load" {
			claim = &st.Data.Claims[i]
		}
	}
	if claim == nil || !claim.Mine || !claim.Auto || claim.Owner != ea.Agent().ID {
		t.Fatalf("a's claims: %+v (notes %v)", st.Data.Claims, noteKinds(st.Notes))
	}
	claimRef := func() string {
		return runGit(t, f.remote, "for-each-ref", "--format=%(objectname)", bus.ClaimsPrefix)
	}
	before := claimRef()
	if before == "" {
		t.Error("no claim on the remote")
	}

	// b sees a owning Load.
	clk.Advance(6 * time.Second)
	who, err := eb.Who(ctx, api.WhoInput{Target: "config.go:Load"})
	must(t, err)
	if !hasEntry(who.Data.Entries, ea.Agent().ID, "owns") {
		t.Errorf("who from b: %+v", who.Data.Entries)
	}

	// The claim is renewed while a is active, in a later tick.
	clk.Advance(e2eLease(ea) / 2)
	must(t, ea.Tick(ctx))
	if after := claimRef(); after == before {
		t.Error("the claim was not renewed")
	}
	clk.Advance(6 * time.Second)
	st, err = ea.Status(ctx, api.StatusInput{})
	must(t, err)
	for _, c := range st.Data.Claims {
		if c.Symbol == "config.go:Load" && (c.Lapsed || !c.Mine) {
			t.Errorf("claim after renewal: %+v", c)
		}
	}
}

func e2eLease(e *Engine) time.Duration { return e.Config().Lease }

func TestHolderMergesNeedRequirements(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {\n\treturn &Config{Path: path}, nil",
		"func Load(path string, strict bool) (*Config, error) {\n\treturn &Config{Path: path, Strict: strict}, nil", 1))
	writeFile(t, f.b, "default.go", mainGo+"\n// Strict loads the strict configuration.\nfunc Strict() (*Config, error) {\n\treturn Load(\"strict.toml\")\n}\n")
	ra, err := ea.Start(ctx, api.StartInput{Intent: "Add a strict flag to Load"})
	must(t, err)
	rb, err := eb.Start(ctx, api.StartInput{Intent: "Add a strict default"})
	must(t, err)
	if ra.Outcome != api.OK || rb.Outcome != api.OK {
		t.Fatalf("start: %q / %q", ra.Summary, rb.Summary)
	}

	// a claims Load automatically.
	clk.Advance(time.Minute)
	must(t, ea.Tick(ctx))
	claimOID := func() string {
		return runGit(t, f.remote, "for-each-ref", "--format=%(objectname)", bus.ClaimsPrefix)
	}
	created := claimOID()
	ea.mu.Lock()
	var key string
	for _, cv := range ea.myClaims() {
		if cv.Claim.Symbol.Ref() == "config.go:Load" {
			key = cv.Claim.Key
		}
	}
	ea.mu.Unlock()
	if key == "" || created == "" {
		t.Fatalf("a holds no claim on Load (remote %q)", created)
	}

	// b states what it needs from Load, as the need verb will.
	const text = "keep accepting a plain path"
	eb.mu.Lock()
	_, err = eb.mutateOutbox(ctx, func(ob *model.Outbox) error {
		ob.Needs = append(ob.Needs, model.Need{
			ID: model.NewMessageID(), Key: key, Name: "config.go:Load", Item: rb.Data.Item.ID,
			Requirement: text, CreatedAt: clk.Now().UTC(),
		})
		return nil
	})
	eb.mu.Unlock()
	must(t, err)

	// a's next poll sees the need and writes the requirement into its claim,
	// long before the claim's renewal is due.
	clk.Advance(30 * time.Second)
	must(t, ea.Tick(ctx))
	merged := claimOID()
	if merged == created {
		t.Fatal("the claim was not rewritten with b's requirement")
	}
	ea.mu.Lock()
	cv, _ := ea.claimFor(key)
	ea.mu.Unlock()
	want := []model.Requirement{{From: eb.Agent().ID, Text: text}}
	if !reflect.DeepEqual(cv.Claim.Requirements, want) {
		t.Errorf("claim requirements = %+v, want %+v", cv.Claim.Requirements, want)
	}

	// Nothing changed since: no rewrite.
	clk.Advance(30 * time.Second)
	must(t, ea.Tick(ctx))
	if claimOID() != merged {
		t.Error("the claim was rewritten although nothing changed")
	}

	// b sees its requirement on a's claim.
	clk.Advance(6 * time.Second)
	st, err := eb.Status(ctx, api.StatusInput{})
	must(t, err)
	found := false
	for _, n := range st.Data.Needs {
		if n.Need.Key == key && n.Owner == ea.Agent().ID {
			found = true
		}
	}
	who, err := eb.Who(ctx, api.WhoInput{Target: "config.go:Load"})
	must(t, err)
	if !found || !hasEntry(who.Data.Entries, ea.Agent().ID, "owns") {
		t.Errorf("b's view: needs %+v, who %+v", st.Data.Needs, who.Data.Entries)
	}
}

func TestSignatureClashFallsBackToTheHolder(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	eb := openEngine(t, Options{Dir: f.b, Now: clk.Now})
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {", "func Load(path string, strict bool) (*Config, error) {", 1))
	writeFile(t, f.b, "config.go", strings.Replace(configGo, "func Load(path string) (*Config, error) {", "func Load(ctx context.Context, path string) (*Config, error) {", 1))
	ra, err := ea.Start(ctx, api.StartInput{Intent: "Add a strict flag to Load"})
	must(t, err)
	rb, err := eb.Start(ctx, api.StartInput{Intent: "Pass a context to Load"})
	must(t, err)
	if ra.Outcome != api.OK || rb.Outcome != api.OK {
		t.Fatalf("start: %q / %q", ra.Summary, rb.Summary)
	}

	// a ticks first and claims Load; b then sees the clash with a holder.
	clk.Advance(time.Minute)
	must(t, ea.Tick(ctx))
	clk.Advance(time.Second)
	must(t, eb.Tick(ctx))
	clk.Advance(6 * time.Second)
	st, err := eb.Status(ctx, api.StatusInput{})
	must(t, err)
	t.Logf("b's notes: %v", noteKinds(st.Notes))
	var clash *model.Note
	for i, n := range st.Notes {
		if n.Kind == model.NoteSignatureClash {
			clash = &st.Notes[i]
		}
	}
	if clash == nil || clash.Thread == "" {
		t.Fatalf("b has no signature clash note: %v", noteKinds(st.Notes))
	}
	eb.mu.Lock()
	seen := eb.clashSeen(ctx)
	eb.mu.Unlock()
	if _, ok := seen[clash.Thread]; !ok {
		t.Errorf("clash %s not recorded: %v", clash.Thread, seen)
	}

	// Nobody discusses: after the thread timeout the holder keeps it, and
	// b records that it needs Load.
	for range 12 {
		clk.Advance(time.Minute)
		must(t, ea.Tick(ctx))
		must(t, eb.Tick(ctx))
	}
	clk.Advance(6 * time.Second)
	st, err = eb.Status(ctx, api.StatusInput{})
	must(t, err)
	found := false
	for _, n := range st.Data.Needs {
		if n.Need.Name == "config.go:Load" {
			found = true
			if n.Owner != ea.Agent().ID {
				t.Errorf("b's need on Load names owner %q", n.Owner)
			}
		}
	}
	if !found {
		t.Errorf("b recorded no need after the fallback: needs %+v, threads %+v, notes %v", st.Data.Needs, st.Data.Threads, noteKinds(st.Notes))
	}
}

func TestIdlePeerLapses(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()

	ea := openEngine(t, Options{Dir: f.a, Config: testConfig()})
	eb := openEngine(t, Options{Dir: f.b, Config: testConfig(), Now: clk.Now})
	r, err := ea.Start(ctx, api.StartInput{Intent: "Rework config loading"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %q", r.Summary)
	}

	peerOf := func(e *Engine) (api.PeerInfo, bool) {
		t.Helper()
		st, err := e.Status(ctx, api.StatusInput{})
		must(t, err)
		for _, p := range st.Data.Others {
			if p.Agent.ID == ea.Agent().ID {
				return p, true
			}
		}
		return api.PeerInfo{}, false
	}
	if p, ok := peerOf(eb); !ok || p.Lapsed {
		t.Fatalf("b's first look at a: %+v (seen %v)", p, ok)
	}
	// b watches for longer than the lease while a stays silent.
	for range 8 {
		clk.Advance(13 * time.Second)
		must(t, eb.Tick(ctx))
	}
	if p, _ := peerOf(eb); !p.Lapsed || p.IdleSince == nil {
		t.Errorf("a has not lapsed after %v of silence: %+v", 8*13*time.Second, p)
	}

	// After a long gap (b slept), time nobody watched does not count.
	clk.Advance(time.Hour)
	if p, _ := peerOf(eb); p.Lapsed {
		t.Errorf("a lapsed on time nobody watched: %+v", p)
	}
}

func TestLapsesSurviveALongVerbAndABlip(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	cfg := testConfig()
	cfg.Lease = 3 * time.Minute
	ea := openEngine(t, Options{Dir: f.a, Config: cfg})
	eb := openEngine(t, Options{Dir: f.b, Config: cfg, Now: clk.Now})
	r, err := ea.Start(ctx, api.StartInput{Intent: "Rework config loading"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %q", r.Summary)
	}
	// idle returns since when b judges a lapsed, or nil.
	idle := func() *time.Time {
		t.Helper()
		var since *time.Time
		flowLocked(eb, func() {
			if p := eb.peers(ctx, eb.now())[ea.Agent().ID]; p.Lapsed {
				since = p.IdleSince
			}
		})
		return since
	}
	must(t, eb.Tick(ctx))
	for range 16 {
		clk.Advance(13 * time.Second)
		must(t, eb.Tick(ctx))
	}
	first := idle()
	if first == nil {
		t.Fatalf("a has not lapsed after %v of silence", 16*13*time.Second)
	}

	// A tick fetches, then waits 45s for a verb that holds the engine: the
	// next tick comes right after, so that is no gap between ticks.
	before, _ := eb.poll.snapshot()
	clk.Advance(13 * time.Second)
	eb.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- eb.Tick(ctx) }()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if last, _ := eb.poll.snapshot(); last.After(before) {
			break
		}
		if time.Now().After(deadline) {
			eb.mu.Unlock()
			t.Fatal("the tick did not fetch")
		}
	}
	clk.Advance(45 * time.Second)
	eb.mu.Unlock()
	must(t, <-done)
	clk.Advance(5 * time.Second)
	must(t, eb.Tick(ctx))
	if since := idle(); since == nil || !since.Equal(*first) {
		t.Errorf("after a long verb, a is idle since %v, want %v", since, first)
	}

	// The remote is away for one poll: too short to hold back a renewal.
	away := f.remote + ".away"
	must(t, os.Rename(f.remote, away))
	clk.Advance(13 * time.Second)
	must(t, eb.Tick(ctx))
	must(t, os.Rename(away, f.remote))
	for range 2 {
		clk.Advance(13 * time.Second)
		must(t, eb.Tick(ctx))
	}
	if since := idle(); since == nil || !since.Equal(*first) {
		t.Errorf("after a blip, a is idle since %v, want %v", since, first)
	}

	// After b slept, time nobody watched does not count.
	clk.Advance(time.Hour)
	must(t, eb.Tick(ctx))
	if since := idle(); since != nil {
		t.Errorf("a is judged on time b slept through: idle since %v", since)
	}
}

func TestReapJudgesAStateItCannotRead(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)
	clk := newClock()
	cfg := testConfig()
	cfg.ReapAfter = 2 * time.Minute
	eb := openEngine(t, Options{Dir: f.b, Config: cfg, Now: clk.Now})

	// A newer gitlet's agent: a state.json in a format b skips, which its
	// heartbeat keeps moving, and an outbox b reads, which stays as it is.
	const peer = model.AgentID("newer-host-abc123")
	docs := filepath.Join(f.root, "docs")
	runGit(t, f.root, "init", "-q", docs)
	push := func(ref, file, content string) {
		runGit(t, docs, "read-tree", "--empty")
		writeFile(t, docs, file, content)
		runGit(t, docs, "add", file)
		commit := runGit(t, docs, "commit-tree", runGit(t, docs, "write-tree"), "-m", "gitlet: "+strings.TrimSuffix(file, ".json"))
		runGit(t, docs, "push", "-q", "-f", f.remote, commit+":"+ref)
	}
	heartbeat := func() {
		push(bus.StateRef(peer), "state.json", fmt.Sprintf(`{"format":2,"agent":{"id":%q},"heartbeat":%q}`, peer, clk.Now().UTC().Format(time.RFC3339Nano)))
	}
	heartbeat()
	push(bus.OutboxRef(peer), "outbox.json", fmt.Sprintf(`{"format":1,"agent":%q}`, peer))
	reap := func() string {
		flowLocked(eb, func() {
			eb.lastReap = time.Time{} // reaping runs at most hourly
			must(t, eb.reap(ctx))
		})
		return runGit(t, f.remote, "for-each-ref", "--format=%(refname)", bus.AgentsPrefix+string(peer)+"/")
	}

	// b watches for longer than the lease and reapAfter together.
	for i := range 30 {
		clk.Advance(13 * time.Second)
		if i%3 == 0 {
			heartbeat()
		}
		must(t, eb.Tick(ctx))
	}
	flowLocked(eb, func() {
		if _, read := eb.shared.Agents[peer]; read {
			t.Fatal("b read the newer state")
		}
	})
	if refs := reap(); refs == "" {
		t.Fatal("b reaped a live agent whose state it cannot read")
	}

	// Once the state stops moving for as long, the agent is gone.
	for range 30 {
		clk.Advance(13 * time.Second)
		must(t, eb.Tick(ctx))
	}
	if refs := reap(); refs != "" {
		t.Errorf("refs of a gone agent left on the remote:\n%s", refs)
	}
}

func TestSubmittedItemLands(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	clk := newClock()
	ea := openEngine(t, Options{Dir: f.a, Now: clk.Now})
	r, err := ea.Start(ctx, api.StartInput{Intent: "Make Default use gitlet.json"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %q", r.Summary)
	}
	item, branch := r.Data.Item.ID, r.Data.Branch
	writeFile(t, f.a, "default.go", strings.Replace(mainGo, "gitlet.toml", "gitlet.json", 1))
	runGit(t, f.a, "commit", "-q", "-am", "default: use gitlet.json")
	runGit(t, f.a, "push", "-q", "origin", branch)
	head := runGit(t, f.a, "rev-parse", "HEAD")

	// Mark the item submitted, as submit will.
	ea.mu.Lock()
	_, err = ea.mutateState(ctx, func(st *model.AgentState) error {
		i := itemIndex(st.Items, item)
		st.Items[i].Status, st.Items[i].Head = model.ItemSubmitted, head
		return nil
	})
	ea.mu.Unlock()
	must(t, err)

	// Someone squash-merges the branch on the remote.
	runGit(t, f.b, "fetch", "-q", "origin", branch)
	runGit(t, f.b, "merge", "-q", "--squash", "FETCH_HEAD")
	runGit(t, f.b, "commit", "-q", "-m", "Make Default use gitlet.json (#1)")
	runGit(t, f.b, "push", "-q", "origin", "main")

	clk.Advance(time.Minute) // the next poll is due
	must(t, ea.Tick(ctx))
	ea.mu.Lock()
	_, it, _ := ea.findItem(item)
	ea.mu.Unlock()
	if it.Status != model.ItemLanded || it.ClosedAt == nil {
		t.Fatalf("item after the merge: %s (closed %v)", it.Status, it.ClosedAt)
	}

	// With nothing open, Shutdown removes the agent's refs from the remote.
	must(t, ea.Shutdown(ctx))
	if refs := runGit(t, f.remote, "for-each-ref", "--format=%(refname)", bus.AgentsPrefix+string(ea.Agent().ID)+"/"); refs != "" {
		t.Errorf("refs left after Shutdown:\n%s", refs)
	}
	if _, err := ea.Status(ctx, api.StatusInput{}); !errors.Is(err, errClosed) {
		t.Errorf("verb after Shutdown: %v", err)
	}
}

func TestShutdownKeepsOpenWork(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	ea := openEngine(t, Options{Dir: f.a})
	r, err := ea.Start(ctx, api.StartInput{Intent: "Keep me around"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start: %q", r.Summary)
	}
	must(t, ea.Shutdown(ctx))
	if refs := runGit(t, f.remote, "for-each-ref", "--format=%(refname)", bus.StateRef(ea.Agent().ID)); refs == "" {
		t.Error("Shutdown deleted the state of an agent with an open item")
	}
}

func TestOfflineEngineStaysLocal(t *testing.T) {
	requireDeps(t)
	f := newFixture(t)
	ctx := ctxT(t)

	ea := openEngine(t, Options{Dir: f.a, Offline: true})
	r, err := ea.Start(ctx, api.StartInput{Intent: "Work on a plane"})
	must(t, err)
	if r.Outcome != api.OK || !r.Data.NewBranch {
		t.Fatalf("offline start: %s %q", r.Outcome, r.Summary)
	}
	must(t, ea.Tick(ctx))
	st, err := ea.Status(ctx, api.StatusInput{})
	must(t, err)
	if len(st.Data.Items) != 1 || st.Data.Items[0].Item.Intent != "Work on a plane" {
		t.Errorf("offline status: %+v", st.Data.Items)
	}
	if refs := runGit(t, f.remote, "for-each-ref", "--format=%(refname)", bus.Prefix); refs != "" {
		t.Errorf("an offline engine wrote to the remote:\n%s", refs)
	}
	if refs := runGit(t, f.a, "for-each-ref", "--format=%(refname)", bus.StateRef(ea.Agent().ID)); refs == "" {
		t.Error("an offline engine kept no local state")
	}
}
