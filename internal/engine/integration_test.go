package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/footprint"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
	"github.com/crewlet/gitlet/internal/rules"
)

// The tests in this file run the engine on real git: a bare remote and two
// clones of it, one per agent. They skip while a dependency of the engine
// is still a stub.

// ---- dependency guard ----

var (
	depsOnce sync.Once
	depsErr  error
)

// requireDeps skips the test while any engine dependency is not
// implemented yet.
func requireDeps(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	depsOnce.Do(func() { depsErr = probeDeps() })
	if depsErr != nil {
		t.Skipf("engine dependencies not implemented yet: %v", depsErr)
	}
}

func notImplemented(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not implemented")
}

// guard runs f, turning a stub's "not implemented" panic into an error.
func guard(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if !strings.Contains(fmt.Sprint(r), "not implemented") {
				panic(r)
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	return f()
}

type emptySource struct{}

func (emptySource) DiffNameStatus(context.Context, string, string) ([]gitx.DiffEntry, error) {
	return nil, nil
}

func (emptySource) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("no files")
}

func probeDeps() error {
	ctx := context.Background()
	if err := guard(func() error {
		_, err := footprint.Compute(ctx, emptySource{}, "a", "b", footprint.Options{})
		return err
	}); notImplemented(err) {
		return err
	}
	if score, _ := rules.Similarity("add strict mode", "add strict mode", nil); score == 0 {
		return errors.New("rules: not implemented")
	}
	if render.Text(&api.Result{Outcome: api.OK, Summary: "probe"}, render.CLI) == "" {
		return errors.New("render: not implemented")
	}
	dir, err := os.MkdirTemp("", "gitlet-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "probe"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}
	return guard(func() error {
		e, err := Open(ctx, Options{Dir: dir, Client: "test", Offline: true})
		if err != nil {
			return err
		}
		defer e.Close()
		_, err = e.Status(ctx, api.StatusInput{})
		return err
	})
}

// must fails the test on err, or skips it when err comes from a stub.
func must(t *testing.T, err error) {
	t.Helper()
	if notImplemented(err) {
		t.Skipf("not implemented yet: %v", err)
	}
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
	var e *Engine
	must(t, guard(func() error {
		var err error
		e, err = Open(context.Background(), opt)
		return err
	}))
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
	writeFile(t, f.a, "config.go", strings.Replace(configGo, "\treturn nil\n}", "\tif c.Strict && c.Path == \"\" {\n\t\treturn errStrict\n\t}\n\treturn nil\n}", 1))
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
	// why on a symbol explains its last change.
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

	ea := openEngine(t, Options{Dir: f.a})
	begin := time.Now()
	r, err := ea.Status(ctx, api.StatusInput{})
	elapsed := time.Since(begin)
	must(t, err)
	if r.Outcome != api.OK {
		t.Errorf("status with a hanging remote: %s %q", r.Outcome, r.Summary)
	}
	if n := ea.fetches.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
	if elapsed > verbRefreshBudget+3*time.Second {
		t.Errorf("status took %v with a hanging remote; the refresh budget is %v", elapsed, verbRefreshBudget)
	}
	if r.OfflineSince != nil {
		t.Errorf("a slow remote is not offline: %v", r.OfflineSince)
	}
}
