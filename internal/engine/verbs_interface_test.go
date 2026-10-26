package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
)

// The tests in this file run claim, publish, need, use and release on real
// git: a bare remote and two clones, a and b, one agent each.

const (
	ifcLoadOld   = "func Load(path string) (*Config, error) {\n\treturn &Config{Path: path}, nil\n}"
	ifcLoad      = "config.go:Load"
	ifcValidate  = "func (c *Config) Validate() error {\n\treturn nil\n}"
	ifcLoadStrFn = "func Load(path string, strict bool) (*Config, error) {\n\treturn &Config{Path: path, Strict: strict}, nil\n}"
	ifcLoadEnvFn = "func Load(path string, strict bool, env string) (*Config, error) {\n\treturn &Config{Path: path + env, Strict: strict}, nil\n}"
	ifcLoadCtxFn = "func Load(ctx context.Context, path string) (*Config, error) {\n\treturn &Config{Path: path}, nil\n}"
)

// ifcConfig returns configGo with Load replaced by fn.
func ifcConfig(fn string) string { return strings.Replace(configGo, ifcLoadOld, fn, 1) }

// ifcPair is a fixture with one engine per clone, sharing a clock.
type ifcPair struct {
	f    *fixture
	clk  *clock
	a, b *Engine
}

func newIfcPair(t *testing.T, cfg func() *Options) *ifcPair {
	t.Helper()
	requireDeps(t)
	p := &ifcPair{f: newFixture(t), clk: newClock()}
	open := func(dir string) *Engine {
		opt := Options{Dir: dir, Now: p.clk.Now}
		if cfg != nil {
			o := cfg()
			opt.Config = o.Config
		}
		return openEngine(t, opt)
	}
	p.a, p.b = open(p.f.a), open(p.f.b)
	return p
}

// fresh moves the clock past the verb refresh interval, so the next verb
// fetches what the other agent pushed.
func (p *ifcPair) fresh() { p.clk.Advance(verbRefreshAfter + time.Second) }

// ifcStart starts a task and fails the test unless it worked.
func ifcStart(t *testing.T, e *Engine, intent string) model.WorkItem {
	t.Helper()
	r, err := e.Start(ctxT(t), api.StartInput{Intent: intent})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("start %q: %s %q %v", intent, r.Outcome, r.Summary, r.Details)
	}
	return r.Data.Item
}

// ifcRemoteClaim reads a claim from the bare remote.
func ifcRemoteClaim(t *testing.T, f *fixture, key string) (model.Claim, string, bool) {
	t.Helper()
	oid := runGit(t, f.remote, "for-each-ref", "--format=%(objectname)", bus.ClaimRef(key))
	if oid == "" {
		return model.Claim{}, "", false
	}
	var c model.Claim
	if err := json.Unmarshal([]byte(runGit(t, f.remote, "show", oid+":claim.json")), &c); err != nil {
		t.Fatal(err)
	}
	return c, oid, true
}

// ifcRemoteOutbox reads an agent's outbox from the bare remote.
func ifcRemoteOutbox(t *testing.T, f *fixture, agent model.AgentID) model.Outbox {
	t.Helper()
	var ob model.Outbox
	if runGit(t, f.remote, "for-each-ref", "--format=%(objectname)", bus.OutboxRef(agent)) == "" {
		return ob
	}
	out := runGit(t, f.remote, "show", bus.OutboxRef(agent)+":outbox.json")
	if err := json.Unmarshal([]byte(out), &ob); err != nil {
		t.Fatal(err)
	}
	return ob
}

// ifcRefused checks a refusal: it says so, and every option is an exact
// call (a verb of api.Verbs with its own argument names and real values)
// or a shell command.
func ifcRefused(t *testing.T, what string, r api.Result) {
	t.Helper()
	if r.Outcome != api.Refused {
		t.Fatalf("%s: %s %q %v, want a refusal", what, r.Outcome, r.Summary, r.Details)
	}
	if len(r.Options) == 0 {
		t.Fatalf("%s: refusal %q offers no option", what, r.Summary)
	}
	for _, o := range r.Options {
		if o.Command != "" {
			continue
		}
		v, ok := api.Lookup(o.Tool)
		if !ok {
			t.Errorf("%s: option %+v names no verb", what, o)
			continue
		}
		for name, val := range o.Args {
			known := false
			for _, a := range v.Args {
				known = known || a.Name == name
			}
			if s := fmt.Sprint(val); !known || s == "" || strings.Contains(s, "<") {
				t.Errorf("%s: option %+v has argument %s=%q", what, o, name, s)
			}
		}
	}
}

// ifcOption returns the first option calling tool.
func ifcOption(r api.Result, tool string) (model.Option, bool) {
	for _, o := range r.Options {
		if o.Tool == tool {
			return o, true
		}
	}
	return model.Option{}, false
}

func ifcKey(path, qualified string) string { return model.SymbolKey("go", path, qualified) }

// ---- claim ----

func TestIfcClaimCreateIdempotentAndHeld(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	writeFile(t, p.f.b, "config.go", ifcConfig(ifcLoadCtxFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	ifcStart(t, p.b, "Pass a context to Load")

	r, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("claim: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	c := r.Data.Claim
	if c.Symbol != ifcLoad || c.Owner != p.a.Agent().ID || !c.Mine || c.Auto || c.Epoch != 0 || c.Lapsed {
		t.Errorf("claim data %+v", c)
	}
	remote, _, ok := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load"))
	if !ok || remote.Owner != p.a.Agent().ID || remote.Auto || remote.Symbol.Qualified != "Load" {
		t.Fatalf("remote claim %+v (exists %v)", remote, ok)
	}

	// Claiming again changes nothing.
	again, err := p.a.Claim(ctx, api.ClaimInput{Symbol: "Load"})
	must(t, err)
	if again.Outcome != api.NothingToDo || !again.Data.Claim.Mine {
		t.Errorf("claim again: %s %q %+v", again.Outcome, again.Summary, again.Data.Claim)
	}

	// b changes Load's signature too: its claim is refused, naming a, with
	// need and a discussion in the signature thread.
	p.fresh()
	held, err := p.b.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "claim held by a", held.Result)
	if !strings.Contains(held.Summary, string(p.a.Agent().ID)) {
		t.Errorf("refusal does not name the holder: %q", held.Summary)
	}
	if o, ok := ifcOption(held.Result, "need"); !ok || o.Args["symbol"] != ifcLoad {
		t.Errorf("no need option in %+v", held.Options)
	}
	o, ok := ifcOption(held.Result, "discuss")
	if !ok || o.Args["about"] != model.SigThreadID(ifcKey("config.go", "Load"), 0) {
		t.Errorf("discuss option %+v, want the signature thread", held.Options)
	}
}

func TestIfcClaimNeedsATask(t *testing.T) {
	p := newIfcPair(t, nil)
	r, err := p.a.Claim(ctxT(t), api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "claim without a task", r.Result)
	if _, ok := ifcOption(r.Result, "start"); !ok {
		t.Errorf("no start option: %+v", r.Options)
	}
	// An unknown symbol is refused with a way out.
	ifcStart(t, p.a, "Rework the configuration")
	r, err = p.a.Claim(ctxT(t), api.ClaimInput{Symbol: "config.go:Nope"})
	must(t, err)
	ifcRefused(t, "claim of an unknown symbol", r.Result)
}

func TestIfcClaimMakesAutomaticClaimDeliberate(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	writeFile(t, p.f.b, "default.go", mainGo+"\n// Strict loads the strict configuration.\nfunc Strict() (*Config, error) {\n\treturn Load(\"strict.toml\")\n}\n")
	ifcStart(t, p.a, "Add a strict flag to Load")
	ifcStart(t, p.b, "Add a strict default")
	p.clk.Advance(time.Minute)
	must(t, p.a.Tick(ctx))
	remote, _, ok := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load"))
	if !ok || !remote.Auto {
		t.Fatalf("no automatic claim after the tick: %+v", remote)
	}

	r, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK || r.Data.Claim.Auto || !r.Data.Claim.Mine {
		t.Fatalf("claim of an automatic claim: %s %q %+v", r.Outcome, r.Summary, r.Data.Claim)
	}
	if remote, _, _ = ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load")); remote.Auto || remote.Owner != p.a.Agent().ID {
		t.Errorf("remote claim after a manual claim: %+v", remote)
	}
	if again, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad}); err != nil || again.Outcome != api.NothingToDo {
		t.Errorf("claim once deliberate: %v %+v", err, again)
	}
}

func TestIfcClaimTakesOverALapsedClaim(t *testing.T) {
	p := newIfcPair(t, func() *Options { return &Options{Config: testConfig()} })
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	r, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("a's claim: %s %q", r.Outcome, r.Summary)
	}
	_, seen, _ := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load"))
	p.fresh()
	ifcStart(t, p.b, "Rework config loading")

	// While a's lease runs, b is refused.
	early, err := p.b.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "claim before the lapse", early.Result)

	// a goes silent; b watches for longer than the lease.
	for range 8 {
		p.clk.Advance(13 * time.Second)
		must(t, p.b.Tick(ctx))
	}
	took, err := p.b.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	if took.Outcome != api.OK {
		t.Fatalf("takeover: %s %q %v", took.Outcome, took.Summary, took.Details)
	}
	c := took.Data.Claim
	if c.Owner != p.b.Agent().ID || !c.Mine || c.Epoch != 1 || c.Lapsed {
		t.Errorf("claim after the takeover: %+v", c)
	}
	if !strings.Contains(took.Summary, string(p.a.Agent().ID)) {
		t.Errorf("takeover summary does not name the old owner: %q", took.Summary)
	}
	remote, oid, _ := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load"))
	if remote.Owner != p.b.Agent().ID || remote.Epoch != 1 || oid == seen {
		t.Errorf("remote claim after the takeover: %+v at %s", remote, oid)
	}

	// a learns it lost: its publish is refused, naming b.
	p.fresh()
	pub, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "publish after losing the claim", pub.Result)
	if !strings.Contains(pub.Summary, string(p.b.Agent().ID)) {
		t.Errorf("refusal does not name the new owner: %q", pub.Summary)
	}
}

func TestIfcClaimRaceHasOneWinner(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	ifcStart(t, p.a, "Rework config loading")
	ifcStart(t, p.b, "Rework config loading differently")

	var wg sync.WaitGroup
	results := make([]*api.Reply[api.ClaimData], 2)
	errs := make([]error, 2)
	for i, e := range []*Engine{p.a, p.b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
		}()
	}
	wg.Wait()
	must(t, errs[0])
	must(t, errs[1])
	var winner model.AgentID
	for i, e := range []*Engine{p.a, p.b} {
		switch results[i].Outcome {
		case api.OK:
			if winner != "" {
				t.Fatalf("two winners: %q and %q", results[0].Summary, results[1].Summary)
			}
			winner = e.Agent().ID
		case api.Refused:
			ifcRefused(t, "the race's loser", results[i].Result)
		default:
			t.Errorf("claim race: %s %q", results[i].Outcome, results[i].Summary)
		}
	}
	if remote, _, _ := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load")); winner == "" || remote.Owner != winner {
		t.Errorf("winner %q, remote claim %+v", winner, remote)
	}
}

func TestIfcClaimLostRaceNamesTheWinner(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	ifcStart(t, p.a, "Rework config loading")
	ifcStart(t, p.b, "Rework config loading differently")
	// b looked last before a claimed, and does not refresh within 5 s, so
	// it believes the symbol is free and loses the create-only push.
	if r, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad}); err != nil || r.Outcome != api.OK {
		t.Fatalf("a's claim: %v %+v", err, r)
	}
	lost, err := p.b.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "lost race", lost.Result)
	if !strings.Contains(lost.Summary, string(p.a.Agent().ID)) {
		t.Errorf("refusal does not name the winner: %q", lost.Summary)
	}
	if _, ok := ifcOption(lost.Result, "need"); !ok {
		t.Errorf("no need option: %+v", lost.Options)
	}
}

func TestIfcClaimAndPublishNeedTheRemote(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	runGit(t, p.f.a, "remote", "set-url", "origin", filepath.Join(p.f.root, "gone.git"))
	p.fresh()

	r, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "claim offline", r.Result)
	if !strings.Contains(r.Summary, "remote unreachable; claims need the remote") {
		t.Errorf("claim offline: %q", r.Summary)
	}
	if o, ok := ifcOption(r.Result, "claim"); !ok || o.Args["symbol"] != ifcLoad {
		t.Errorf("no retry option with the symbol: %+v", r.Options)
	}
	pub, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "publish offline", pub.Result)
	if !strings.Contains(pub.Summary, "remote unreachable; claims need the remote") {
		t.Errorf("publish offline: %q", pub.Summary)
	}
	// No local claim stands in for the remote's.
	if refs := runGit(t, p.f.a, "for-each-ref", "--format=%(refname)", "refs/gitlet/claims/", "refs/gitlet-remote/"); strings.Contains(refs, "claims/") {
		t.Errorf("claim refs written offline:\n%s", refs)
	}
	// A need works offline: it waits in the outbox.
	n, err := p.a.Need(ctx, api.NeedInput{Symbol: "config.go:Config.Validate"})
	must(t, err)
	if n.Outcome != api.OK {
		t.Errorf("need offline: %s %q", n.Outcome, n.Summary)
	}
}

// ---- publish ----

func TestIfcPublishBuildsTheStubOnMain(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	// a changes Load's signature and Validate's body.
	work := strings.Replace(ifcConfig(ifcLoadStrFn), ifcValidate, "func (c *Config) Validate() error {\n\tif c.Path == \"\" {\n\t\treturn nil\n\t}\n\treturn nil\n}", 1)
	writeFile(t, p.f.a, "config.go", work)
	ifcStart(t, p.a, "Add a strict flag to Load")
	// Meanwhile main moves on.
	writeFile(t, p.f.b, "README.md", "# demo\n\nMore words.\n")
	runGit(t, p.f.b, "commit", "-q", "-am", "readme: more words")
	runGit(t, p.f.b, "push", "-q", "origin", "main")
	main := runGit(t, p.f.b, "rev-parse", "HEAD")
	p.fresh()

	r, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("publish: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	ct := r.Data.Contract
	stub := ct.StubCommit
	if r.Data.Symbol != ifcLoad || stub == "" || !strings.Contains(ct.Signature, "bool") || !ct.Breaking || ct.PublishedAt.IsZero() {
		t.Errorf("publish data %+v", r.Data)
	}
	if !strings.Contains(r.Summary, "Claimed") {
		t.Errorf("publish without a claim does not say it claimed: %q", r.Summary)
	}
	text := strings.Join(r.Details, "\n")
	if !strings.Contains(text, "must build on its own") || !strings.Contains(text, stub) {
		t.Errorf("details do not name the stub to build:\n%s", text)
	}

	// The stub is main plus Load from a's work, and nothing else.
	if parent := runGit(t, p.f.a, "rev-parse", stub+"^"); parent != main {
		t.Errorf("stub parent %s, want main %s", parent, main)
	}
	if files := runGit(t, p.f.a, "diff-tree", "--no-commit-id", "--name-only", "-r", stub); files != "config.go" {
		t.Errorf("stub changes %q", files)
	}
	if got := runGit(t, p.f.a, "show", stub+":config.go"); got != strings.TrimSpace(ifcConfig(ifcLoadStrFn)) {
		t.Errorf("stub config.go:\n%s", got)
	}
	if got := runGit(t, p.f.a, "show", stub+":README.md"); !strings.Contains(got, "More words") {
		t.Errorf("stub is not on top of main: README %q", got)
	}
	key := ifcKey("config.go", "Load")
	msg := runGit(t, p.f.a, "log", "-1", "--format=%B", stub)
	if !strings.HasPrefix(msg, "gitlet stub: config.go:Load\n") || !strings.Contains(msg, "Gitlet-Stub: "+key+"@"+ct.SigHash) {
		t.Errorf("stub message:\n%s", msg)
	}

	// The contract is on the claim, whose commit has the stub as parent.
	remote, oid, ok := ifcRemoteClaim(t, p.f, key)
	if !ok || remote.Owner != p.a.Agent().ID || remote.Auto || remote.Contract == nil || remote.Contract.StubCommit != stub || remote.Contract.SigHash != ct.SigHash {
		t.Fatalf("remote claim %+v", remote)
	}
	if parent := runGit(t, p.f.remote, "rev-parse", oid+"^"); parent != stub {
		t.Errorf("claim commit parent %s, want the stub %s", parent, stub)
	}

	// b sees the contract.
	ifcStart(t, p.b, "Use the strict flag")
	n, err := p.b.Need(ctx, api.NeedInput{Symbol: ifcLoad})
	must(t, err)
	if n.Outcome != api.OK || n.Data.Owner != p.a.Agent().ID || n.Data.Contract == nil || n.Data.Contract.SigHash != ct.SigHash {
		t.Errorf("b's need: %s %q %+v", n.Outcome, n.Summary, n.Data)
	}

	// Publishing the same signature again changes nothing.
	again, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	if again.Outcome != api.NothingToDo || again.Data.Contract.StubCommit != stub {
		t.Errorf("publish again: %s %q %+v", again.Outcome, again.Summary, again.Data.Contract)
	}
}

func TestIfcPublishAppendsNewSymbolsAndFiles(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	fn := "func LoadStrict(path string) (*Config, error) {\n\treturn Load(path)\n}"
	writeFile(t, p.f.a, "config.go", configGo+"\n// LoadStrict loads in strict mode.\n"+fn+"\n")
	strict := "package config\n\n// StrictDefault loads the strict default.\nfunc StrictDefault() (*Config, error) {\n\treturn LoadStrict(\"gitlet.toml\")\n}\n"
	writeFile(t, p.f.a, "strict.go", strict)
	ifcStart(t, p.a, "Add strict loading")

	// A symbol main lacks is appended after a blank line.
	r, err := p.a.Publish(ctx, api.PublishInput{Symbol: "config.go:LoadStrict"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("publish a new symbol: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	want := strings.TrimRight(configGo, "\n") + "\n\n" + fn + "\n"
	if got := runGit(t, p.f.a, "show", r.Data.Contract.StubCommit+":config.go"); got != strings.TrimSpace(want) {
		t.Errorf("stub config.go:\n%s\nwant:\n%s", got, want)
	}
	if len(r.Warnings) > 0 && strings.Contains(strings.Join(r.Warnings, "\n"), "whole file") {
		t.Errorf("unexpected warnings: %v", r.Warnings)
	}

	// A file main lacks comes whole, with a warning.
	r, err = p.a.Publish(ctx, api.PublishInput{Symbol: "strict.go:StrictDefault"})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("publish in a new file: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	if got := runGit(t, p.f.a, "show", r.Data.Contract.StubCommit+":strict.go"); got != strings.TrimSpace(strict) {
		t.Errorf("stub strict.go:\n%s", got)
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "whole file") {
		t.Errorf("no warning about the whole file: %v", r.Warnings)
	}
}

func TestIfcPublishAnExplicitCommit(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	work := ifcConfig(ifcLoadStrFn)
	writeFile(t, p.f.a, "config.go", work)
	ifcStart(t, p.a, "Add a strict flag to Load")
	main := runGit(t, p.f.a, "rev-parse", "origin/main")
	commitFile := func(base, path, content string) string {
		t.Helper()
		oid, err := p.a.repo.CommitFile(ctx, base, path, []byte(content), "hand-made stub", gitx.Identity{})
		must(t, err)
		return oid
	}
	publish := func(commit string) *api.Reply[api.PublishData] {
		t.Helper()
		r, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad, Commit: commit})
		must(t, err)
		return r
	}
	builds := func(r *api.Reply[api.PublishData]) bool {
		o, ok := ifcOption(r.Result, "publish")
		_, hasCommit := o.Args["commit"]
		return ok && !hasCommit
	}

	// Not a commit.
	r := publish("no-such-commit")
	ifcRefused(t, "publish of a missing commit", r.Result)
	if !builds(r) {
		t.Errorf("no option to let gitlet build the stub: %+v", r.Options)
	}
	// A commit whose parent is not on main.
	local := commitFile(main, "README.md", "# local\n")
	r = publish(commitFile(local, "config.go", work))
	ifcRefused(t, "publish of a commit off main", r.Result)
	if !strings.Contains(r.Summary, "not on main") {
		t.Errorf("off main: %q", r.Summary)
	}
	// Two parents.
	tree := runGit(t, p.f.a, "rev-parse", commitFile(main, "config.go", work)+"^{tree}")
	merge := runGit(t, p.f.a, "commit-tree", tree, "-p", main, "-p", local, "-m", "merge")
	r = publish(merge)
	ifcRefused(t, "publish of a merge", r.Result)
	if !strings.Contains(r.Summary, "2 parents") {
		t.Errorf("merge: %q", r.Summary)
	}
	// A commit that does not define the symbol.
	r = publish(commitFile(main, "config.go", ifcConfig("")))
	ifcRefused(t, "publish of a commit without the symbol", r.Result)
	if !strings.Contains(r.Summary, "does not define") {
		t.Errorf("without the symbol: %q", r.Summary)
	}
	// A commit with another signature.
	r = publish(commitFile(main, "config.go", ifcConfig(ifcLoadEnvFn)))
	ifcRefused(t, "publish of a mismatching stub", r.Result)
	if text := strings.Join(r.Details, "\n"); !strings.Contains(text, "in the stub: ") || !strings.Contains(text, "yours now: ") {
		t.Errorf("mismatch details: %v", r.Details)
	}
	if !builds(r) {
		t.Errorf("no option to let gitlet build the stub: %+v", r.Options)
	}
	if _, _, ok := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load")); ok {
		t.Error("a refused publish left a claim")
	}

	// A good stub is published as it is.
	good := commitFile(main, "config.go", work)
	r = publish(good)
	if r.Outcome != api.OK || r.Data.Contract.StubCommit != good {
		t.Fatalf("publish of a good commit: %s %q %+v", r.Outcome, r.Summary, r.Data)
	}
	if remote, _, _ := ifcRemoteClaim(t, p.f, ifcKey("config.go", "Load")); remote.Contract == nil || remote.Contract.StubCommit != good {
		t.Errorf("remote claim %+v", remote)
	}
	if again := publish(good); again.Outcome != api.NothingToDo {
		t.Errorf("publish of the same commit again: %s %q", again.Outcome, again.Summary)
	}
}

func TestIfcPublishRefusals(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	ifcStart(t, p.b, "Own Load")

	// Nothing changed: main already has the signature.
	r, err := p.a.Publish(ctx, api.PublishInput{Symbol: "config.go:Config.Validate"})
	must(t, err)
	ifcRefused(t, "publish of an unchanged signature", r.Result)

	// Held by someone else.
	if c, err := p.b.Claim(ctx, api.ClaimInput{Symbol: ifcLoad}); err != nil || c.Outcome != api.OK {
		t.Fatalf("b's claim: %v %+v", err, c)
	}
	p.fresh()
	r, err = p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "publish of b's claim", r.Result)
	if !strings.Contains(r.Summary, string(p.b.Agent().ID)) {
		t.Errorf("refusal does not name the holder: %q", r.Summary)
	}
	if o, ok := ifcOption(r.Result, "need"); !ok || o.Args["symbol"] != ifcLoad {
		t.Errorf("no need option: %+v", r.Options)
	}
}

// ---- need ----

func TestIfcNeedRecordsAndUpdates(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	if c, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad}); err != nil || c.Outcome != api.OK {
		t.Fatalf("a's claim: %v %+v", err, c)
	}
	p.fresh()
	item := ifcStart(t, p.b, "Load strict configs")

	r, err := p.b.Need(ctx, api.NeedInput{Symbol: ifcLoad, Requirement: "keep returning *Config"})
	must(t, err)
	n := r.Data.Need
	if r.Outcome != api.OK || r.Data.Owner != p.a.Agent().ID || r.Data.Contract != nil {
		t.Fatalf("need: %s %q %+v", r.Outcome, r.Summary, r.Data)
	}
	if n.Key != ifcKey("config.go", "Load") || n.Name != ifcLoad || n.Item != item.ID || n.Requirement != "keep returning *Config" || n.ID == "" || n.CreatedAt.IsZero() {
		t.Errorf("need %+v", n)
	}
	for _, in := range []api.NeedInput{
		{Symbol: ifcLoad, Requirement: "keep returning *Config"},
		{Symbol: "Load"},
	} {
		again, err := p.b.Need(ctx, in)
		must(t, err)
		if again.Outcome != api.NothingToDo || again.Data.Need.ID != n.ID {
			t.Errorf("need %+v again: %s %q", in, again.Outcome, again.Summary)
		}
	}
	upd, err := p.b.Need(ctx, api.NeedInput{Symbol: ifcLoad, Requirement: "accept a strict flag"})
	must(t, err)
	if upd.Outcome != api.OK || upd.Data.Need.ID != n.ID || upd.Data.Need.Requirement != "accept a strict flag" {
		t.Errorf("need update: %s %q %+v", upd.Outcome, upd.Summary, upd.Data.Need)
	}
	ob := ifcRemoteOutbox(t, p.f, p.b.Agent().ID)
	if len(ob.Needs) != 1 || ob.Needs[0].Requirement != "accept a strict flag" {
		t.Errorf("remote outbox needs %+v", ob.Needs)
	}
	// b's need never touches a's claim; a's own engine carries it over.
	if remote, _, _ := ifcRemoteClaim(t, p.f, n.Key); remote.Owner != p.a.Agent().ID || len(remote.Requirements) != 0 {
		t.Errorf("b wrote a's claim: %+v", remote)
	}

	// The holder cannot need its own interface.
	own, err := p.a.Need(ctx, api.NeedInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "need of an own claim", own.Result)
	if _, ok := ifcOption(own.Result, "publish"); !ok {
		t.Errorf("no publish option: %+v", own.Options)
	}

	// Without a task there is nothing to need it for.
	q := newIfcPair(t, nil)
	none, err := q.a.Need(ctx, api.NeedInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "need without a task", none.Result)
}

// ---- use ----

func TestIfcUsePicksAndPinsTheStub(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	p.fresh()
	ifcStart(t, p.b, "Load strict configs")

	// Nothing is published yet.
	early, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "use before publish", early.Result)
	if _, ok := ifcOption(early.Result, "need"); !ok {
		t.Errorf("no need option: %+v", early.Options)
	}

	pub, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	if pub.Outcome != api.OK {
		t.Fatalf("publish: %s %q %v", pub.Outcome, pub.Summary, pub.Details)
	}
	stub := pub.Data.Contract.StubCommit
	p.fresh()

	// Uncommitted changes in the stub's files are refused.
	writeFile(t, p.f.b, "config.go", configGo+"\n// a note\n")
	dirty, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "use with uncommitted changes", dirty.Result)
	if len(dirty.Options) == 0 || !strings.Contains(dirty.Options[0].Command, "git add -- config.go") {
		t.Errorf("no commit instruction: %+v", dirty.Options)
	}
	runGit(t, p.f.b, "checkout", "--", "config.go")

	r, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("use: %s %q %v", r.Outcome, r.Summary, r.Details)
	}
	head := runGit(t, p.f.b, "rev-parse", "HEAD")
	if r.Data.Symbol != ifcLoad || r.Data.Stub != stub || r.Data.Commit != head || r.Data.SigHash != pub.Data.Contract.SigHash || r.Data.Reverted != "" {
		t.Errorf("use data %+v (HEAD %s)", r.Data, head)
	}
	if msg := runGit(t, p.f.b, "log", "-1", "--format=%B"); !strings.Contains(msg, "gitlet stub: config.go:Load") || !strings.Contains(msg, "cherry picked from commit "+stub) {
		t.Errorf("picked commit message:\n%s", msg)
	}
	if got := runGit(t, p.f.b, "show", "HEAD:config.go"); !strings.Contains(got, ifcLoadStrFn) {
		t.Errorf("b's config.go after use:\n%s", got)
	}
	ob := ifcRemoteOutbox(t, p.f, p.b.Agent().ID)
	if len(ob.Needs) != 1 || ob.Needs[0].PinnedSigHash != r.Data.SigHash || ob.Needs[0].PinnedStub != stub || ob.Needs[0].PinnedCommit != head || ob.Needs[0].Name != ifcLoad {
		t.Errorf("b's needs after use: %+v", ob.Needs)
	}
	again, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	if again.Outcome != api.NothingToDo || again.Data.Commit != head {
		t.Errorf("use again: %s %q %+v", again.Outcome, again.Summary, again.Data)
	}

	// a changes the signature again and republishes: b's use reverts the
	// old stub and picks the new one.
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadEnvFn))
	p.fresh()
	pub2, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad})
	must(t, err)
	if pub2.Outcome != api.OK || pub2.Data.Contract.SigHash == pub.Data.Contract.SigHash {
		t.Fatalf("republish: %s %q %+v", pub2.Outcome, pub2.Summary, pub2.Data.Contract)
	}
	p.fresh()
	drift, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	if drift.Outcome != api.OK || drift.Data.Reverted != head || drift.Data.Stub != pub2.Data.Contract.StubCommit {
		t.Fatalf("use after a drift: %s %q %v %+v", drift.Outcome, drift.Summary, drift.Details, drift.Data)
	}
	subjects := strings.Split(runGit(t, p.f.b, "log", "-3", "--format=%s"), "\n")
	if len(subjects) != 3 || subjects[0] != "gitlet stub: config.go:Load" || !strings.HasPrefix(subjects[1], "Revert ") || subjects[2] != "gitlet stub: config.go:Load" {
		t.Errorf("b's history after the drift: %q", subjects)
	}
	if got := runGit(t, p.f.b, "show", "HEAD:config.go"); !strings.Contains(got, ifcLoadEnvFn) || strings.Contains(got, ifcLoadStrFn) {
		t.Errorf("b's config.go after the drift:\n%s", got)
	}
	if ob := ifcRemoteOutbox(t, p.f, p.b.Agent().ID); len(ob.Needs) != 1 || ob.Needs[0].PinnedSigHash != pub2.Data.Contract.SigHash || ob.Needs[0].PinnedCommit != drift.Data.Commit {
		t.Errorf("b's needs after the drift: %+v", ob.Needs)
	}
}

func TestIfcUseConflictIsReportedByFile(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	if pub, err := p.a.Publish(ctx, api.PublishInput{Symbol: ifcLoad}); err != nil || pub.Outcome != api.OK {
		t.Fatalf("publish: %v %+v", err, pub)
	}
	p.fresh()
	ifcStart(t, p.b, "Append the extension to config paths")
	writeFile(t, p.f.b, "config.go", strings.Replace(configGo, "return &Config{Path: path}, nil", "return &Config{Path: path + \".toml\"}, nil", 1))
	runGit(t, p.f.b, "commit", "-q", "-am", "config: append the extension")
	before := runGit(t, p.f.b, "rev-parse", "HEAD")

	r, err := p.b.Use(ctx, api.UseInput{Symbol: ifcLoad})
	must(t, err)
	ifcRefused(t, "conflicting use", r.Result)
	if !strings.Contains(r.Summary, "config.go") || !strings.Contains(strings.Join(r.Details, "\n"), "conflict: config.go") {
		t.Errorf("conflict not reported by file: %q %v", r.Summary, r.Details)
	}
	if after := runGit(t, p.f.b, "rev-parse", "HEAD"); after != before {
		t.Errorf("HEAD moved from %s to %s", before, after)
	}
	if st := runGit(t, p.f.b, "status", "--porcelain"); st != "" {
		t.Errorf("work tree after the aborted cherry-pick:\n%s", st)
	}
	if ob := ifcRemoteOutbox(t, p.f, p.b.Agent().ID); len(ob.Needs) != 0 {
		t.Errorf("a refused use pinned: %+v", ob.Needs)
	}
}

// ---- release ----

func TestIfcReleaseClaimAndNeed(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	ifcStart(t, p.a, "Add a strict flag to Load")
	if c, err := p.a.Claim(ctx, api.ClaimInput{Symbol: ifcLoad}); err != nil || c.Outcome != api.OK {
		t.Fatalf("claim: %v %+v", err, c)
	}
	p.fresh()
	ifcStart(t, p.b, "Load strict configs")
	if n, err := p.b.Need(ctx, api.NeedInput{Symbol: ifcLoad}); err != nil || n.Outcome != api.OK {
		t.Fatalf("need: %v %+v", err, n)
	}
	key := ifcKey("config.go", "Load")

	r, err := p.a.Release(ctx, api.ReleaseInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK || len(r.Data.Released) != 1 || r.Data.Released[0] != "claim" || r.Data.Symbol != ifcLoad {
		t.Fatalf("release a's claim: %s %q %+v", r.Outcome, r.Summary, r.Data)
	}
	if _, _, ok := ifcRemoteClaim(t, p.f, key); ok {
		t.Error("the claim is still on the remote")
	}
	again, err := p.a.Release(ctx, api.ReleaseInput{Symbol: ifcLoad})
	must(t, err)
	if again.Outcome != api.NothingToDo || len(again.Data.Released) != 0 {
		t.Errorf("release again: %s %q %+v", again.Outcome, again.Summary, again.Data)
	}

	p.fresh()
	r, err = p.b.Release(ctx, api.ReleaseInput{Symbol: ifcLoad})
	must(t, err)
	if r.Outcome != api.OK || len(r.Data.Released) != 1 || r.Data.Released[0] != "need" {
		t.Fatalf("release b's need: %s %q %+v", r.Outcome, r.Summary, r.Data)
	}
	if ob := ifcRemoteOutbox(t, p.f, p.b.Agent().ID); len(ob.Needs) != 0 {
		t.Errorf("b's needs after release: %+v", ob.Needs)
	}
}

// ---- note argument ----

func TestIfcNoteArgumentMarksTheNoteActed(t *testing.T) {
	p := newIfcPair(t, nil)
	ctx := ctxT(t)
	writeFile(t, p.f.a, "config.go", ifcConfig(ifcLoadStrFn))
	writeFile(t, p.f.b, "default.go", mainGo+"\n// Strict loads the strict configuration.\nfunc Strict() (*Config, error) {\n\treturn Load(\"strict.toml\")\n}\n")
	ifcStart(t, p.a, "Add a strict flag to Load")
	p.fresh()
	ifcStart(t, p.b, "Add a strict default")
	st, err := p.b.Status(ctx, api.StatusInput{})
	must(t, err)
	var note *model.Note
	for i, n := range st.Notes {
		if n.Kind == model.NoteCalleeChanged && n.Subject == ifcKey("config.go", "Load") {
			note = &st.Notes[i]
		}
	}
	if note == nil {
		t.Fatalf("b has no callee_changed note on Load: %v", noteKinds(st.Notes))
	}

	r, err := p.b.Need(ctx, api.NeedInput{Symbol: ifcLoad, Note: note.ID[:6]})
	must(t, err)
	if r.Outcome != api.OK {
		t.Fatalf("need with a note: %s %q", r.Outcome, r.Summary)
	}
	p.b.mu.Lock()
	sn, err := p.b.led.Note(ctx, p.b.Agent().ID, note.ID)
	p.b.mu.Unlock()
	must(t, err)
	if sn.Status != ledger.NoteResolved || sn.Resolution != ledger.ResolvedByAgent {
		t.Errorf("note after need: %s (%s)", sn.Status, sn.Resolution)
	}
}
