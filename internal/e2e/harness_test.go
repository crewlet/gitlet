package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/config"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/mcpserver"
	"github.com/crewlet/gitlet/internal/model"
)

// ---- the repository ----

// The project both agents work on: a TypeScript config parser and an app
// that calls it.
const (
	configTS = `// Config is the parsed application configuration.
export interface Config {
  name: string;
  strict: boolean;
}

// parseConfig reads the application name from text.
export function parseConfig(text: string): Config {
  const name = text.trim();
  return { name, strict: false };
}
`
	appTS = `import { parseConfig } from "./config";

// loadApp returns the configured application name.
export function loadApp(text: string): string {
  const config = parseConfig(text);
  return config.name;
}

// appTitle formats a title for the application.
export function appTitle(name: string): string {
  return "App: " + name;
}
`
	parseConfigOld = "export function parseConfig(text: string): Config {\n  const name = text.trim();\n  return { name, strict: false };\n}"
)

// Symbol names as agents write them (docs/SPEC.md, section 4.1).
const parseConfigRef = "src/config.ts:parseConfig"

// symbolKey is the symbol key of docs/SPEC.md, section 5: the first 16 hex
// characters of sha256(lang NUL path NUL qualified). It names the claim ref.
func symbolKey(lang, path, qualified string) string {
	h := sha256.Sum256([]byte(lang + "\x00" + path + "\x00" + qualified))
	return hex.EncodeToString(h[:])[:16]
}

var parseConfigKey = symbolKey("typescript", "src/config.ts", "parseConfig")

// withParseConfig returns configTS with parseConfig replaced by fn.
func withParseConfig(fn string) string {
	if !strings.Contains(configTS, parseConfigOld) {
		panic("configTS lost parseConfig")
	}
	return strings.Replace(configTS, parseConfigOld, fn, 1)
}

// fixture is a bare remote with two clones, a and b: two machines.
type fixture struct {
	root, remote, a, b string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
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
	runGit(t, root, "init", "-q", "--bare", "-b", "main", f.remote)
	runGit(t, root, "init", "-q", "-b", "main", seed)
	identity(t, seed, "Seed", "seed@example.invalid")
	writeFile(t, seed, "src/config.ts", configTS)
	writeFile(t, seed, "src/app.ts", appTS)
	writeFile(t, seed, "README.md", "# demo\n")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-q", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", f.remote)
	runGit(t, seed, "push", "-q", "origin", "main")
	runGit(t, root, "clone", "-q", f.remote, f.a)
	runGit(t, root, "clone", "-q", f.remote, f.b)
	identity(t, f.a, "Agent A", "a@example.invalid")
	identity(t, f.b, "Agent B", "b@example.invalid")
	return f
}

// identity gives a repository its own committer, so commits never depend
// on the developer's global configuration.
func identity(t *testing.T, dir, name, email string) {
	t.Helper()
	runGit(t, dir, "config", "user.name", name)
	runGit(t, dir, "config", "user.email", email)
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

// commit writes files (name, content pairs) in dir, commits everything and
// returns the commit.
func commit(t *testing.T, dir, msg string, files ...string) string {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		writeFile(t, dir, files[i], files[i+1])
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", msg)
	return runGit(t, dir, "rev-parse", "HEAD")
}

// ---- refs on the remote (docs/SPEC.md, section 6) ----

// remoteRef returns the oid of ref on the bare remote, or "".
func (f *fixture) remoteRef(t *testing.T, ref string) string {
	t.Helper()
	return runGit(t, f.remote, "for-each-ref", "--format=%(objectname)", ref)
}

// remoteRefs lists the refs under prefix on the bare remote.
func (f *fixture) remoteRefs(t *testing.T, prefix string) []string {
	t.Helper()
	out := runGit(t, f.remote, "for-each-ref", "--format=%(refname)", prefix)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// remoteDoc decodes the JSON document file of ref on the remote into v and
// reports whether the ref exists.
func (f *fixture) remoteDoc(t *testing.T, ref, file string, v any) bool {
	t.Helper()
	if f.remoteRef(t, ref) == "" {
		return false
	}
	if err := json.Unmarshal([]byte(runGit(t, f.remote, "show", ref+":"+file)), v); err != nil {
		t.Fatalf("%s:%s: %v", ref, file, err)
	}
	return true
}

func claimRef(key string) string           { return "refs/gitlet/claims/" + key }
func stateRef(agent model.AgentID) string  { return "refs/gitlet/agents/" + string(agent) + "/state" }
func outboxRef(agent model.AgentID) string { return "refs/gitlet/agents/" + string(agent) + "/outbox" }
func wipRef(agent model.AgentID, item model.ItemID) string {
	return "refs/gitlet/wip/" + string(agent) + "/" + string(item)
}

// claim reads a claim from the remote.
func (f *fixture) claim(t *testing.T, key string) (model.Claim, bool) {
	t.Helper()
	var c model.Claim
	ok := f.remoteDoc(t, claimRef(key), "claim.json", &c)
	return c, ok
}

// outbox reads an agent's outbox from the remote.
func (f *fixture) outbox(t *testing.T, agent model.AgentID) model.Outbox {
	t.Helper()
	var ob model.Outbox
	f.remoteDoc(t, outboxRef(agent), "outbox.json", &ob)
	return ob
}

// item reads one of an agent's items from its state on the remote.
func (f *fixture) item(t *testing.T, agent model.AgentID, id model.ItemID) model.WorkItem {
	t.Helper()
	var st model.AgentState
	if !f.remoteDoc(t, stateRef(agent), "state.json", &st) {
		t.Fatalf("no state for %s on the remote", agent)
	}
	for _, it := range st.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("%s's state on the remote has no item %s: %+v", agent, id, st.Items)
	return model.WorkItem{}
}

// ---- time ----

// clock is the adjustable time both machines share, so tests move time
// instead of sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now().UTC().Truncate(time.Second)} }

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

const (
	// fresh is more than the verb refresh interval (5 s): the next verb
	// fetches what the other machine pushed (docs/SPEC.md, section 6.2).
	fresh = 6 * time.Second
	// pollDue is more than a jittered poll interval (20 s ± 20%): the next
	// Tick polls the remote.
	pollDue = 25 * time.Second
)

// ---- machines ----

// machine is one agent on its own clone, served by its own MCP server and
// driven through an MCP client session, the way an agent runs gitlet.
type machine struct {
	t    *testing.T
	name string
	dir  string
	f    *fixture
	clk  *clock
	cs   *mcp.ClientSession

	mu  sync.Mutex
	eng *engine.Engine
}

// machineOptions configures one machine.
type machineOptions struct {
	client string        // the MCP client's name
	agent  model.AgentID // fixed agent id, so failures are readable
	config func(*config.Config)
}

// newMachine starts an MCP server on dir and connects a client to it. The
// server never ticks by itself (TickInterval < 0): the test moves the shared
// clock and ticks with machine.tick.
func newMachine(t *testing.T, f *fixture, clk *clock, name, dir string, opt machineOptions) *machine {
	t.Helper()
	m := &machine{t: t, name: name, dir: dir, f: f, clk: clk}
	srv := mcpserver.New(mcpserver.Options{
		Dir:          dir,
		Version:      "e2e",
		Agent:        opt.agent,
		TickInterval: -1,
		Logger:       slog.New(slog.DiscardHandler),
		OpenEngine: func(ctx context.Context, eo engine.Options) (*engine.Engine, error) {
			eo.Now = clk.Now
			if opt.config != nil {
				cfg := config.Defaults()
				cfg.Remote, cfg.Main = "origin", "main"
				opt.config(&cfg)
				eo.Config = &cfg
			}
			// The sequence editor of update is this test binary, which
			// TestMain runs as gitlet.
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			eo.Exe = exe
			e, err := engine.Open(ctx, eo)
			if err == nil {
				m.mu.Lock()
				m.eng = e
				m.mu.Unlock()
			}
			return e, err
		},
	})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: opt.client, Version: "e2e"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.cs = cs
	t.Cleanup(func() {
		cs.Close()
		ss.Wait()
		if err := srv.Close(); err != nil {
			t.Errorf("%s: close the server: %v", name, err)
		}
	})
	return m
}

// engine returns the engine the server opened. It exists once the first
// tool call returned.
func (m *machine) engine() *engine.Engine {
	m.t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.eng == nil {
		m.t.Fatalf("%s: the server has not opened its engine yet", m.name)
	}
	return m.eng
}

// agent returns the machine's agent id.
func (m *machine) agent() model.AgentID { return m.engine().Agent().ID }

// tick runs the engine's background work once, after moving the shared
// clock past the poll interval so that it polls the remote. MCP has no way
// to ask for a tick: a real server ticks every few seconds on its own
// (mcpserver.Options.TickInterval). Engine.Tick is that tick, called
// directly so the test decides when it happens.
func (m *machine) tick() {
	m.t.Helper()
	m.clk.Advance(pollDue)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.engine().Tick(ctx); err != nil {
		m.t.Fatalf("%s: tick: %v", m.name, err)
	}
}

// reply is a tool result as the agent's client sees it: the structured
// content decoded into the verb's reply type, the text block, and isError.
type reply[D any] struct {
	api.Reply[D]
	Text    string
	IsError bool
}

// call calls a tool by name with JSON arguments and decodes its result. It
// checks what every result must satisfy (docs/SPEC.md, section 4.2): an
// envelope with a known outcome, isError exactly for refusals, a refusal
// that offers a way out, and a text block whose first line is the outcome
// and the summary.
func call[D any](m *machine, tool string, args map[string]any) reply[D] {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := m.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		m.t.Fatalf("%s: %s: protocol error: %v", m.name, tool, err)
	}
	var r reply[D]
	r.IsError = res.IsError
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	r.Text = strings.Join(parts, "\n")
	if res.StructuredContent == nil {
		m.t.Fatalf("%s: %s %v: no structured content (isError %v):\n%s", m.name, tool, args, res.IsError, r.Text)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		m.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &r.Reply); err != nil {
		m.t.Fatalf("%s: %s: decode %s: %v", m.name, tool, raw, err)
	}
	switch r.Outcome {
	case api.OK, api.NothingToDo, api.Refused:
	default:
		m.t.Fatalf("%s: %s: outcome %q:\n%s", m.name, tool, r.Outcome, r.Text)
	}
	if r.IsError != (r.Outcome == api.Refused) {
		m.t.Errorf("%s: %s: isError %v for outcome %s", m.name, tool, r.IsError, r.Outcome)
	}
	if r.Outcome == api.Refused && len(r.Options) == 0 {
		m.t.Errorf("%s: %s: a refusal without options:\n%s", m.name, tool, r.Text)
	}
	if first, _, _ := strings.Cut(r.Text, "\n"); first != string(r.Outcome)+": "+r.Summary {
		m.t.Errorf("%s: %s: first line %q, want the outcome and the summary %q", m.name, tool, first, r.Summary)
	}
	return r
}

// must fails the test unless r has outcome want.
func must[D any](m *machine, what string, r reply[D], want api.Outcome) reply[D] {
	m.t.Helper()
	if r.Outcome != want {
		m.t.Fatalf("%s: %s: want %s, got:\n%s", m.name, what, want, r.Text)
	}
	return r
}

// status calls status after the clock moved past the verb refresh interval,
// so it sees what the other machine pushed. The text goes to the test log,
// which shows what each agent saw when a later step fails.
func (m *machine) status() reply[api.StatusData] {
	m.t.Helper()
	m.clk.Advance(fresh)
	r := must(m, "status", call[api.StatusData](m, "status", nil), api.OK)
	m.t.Logf("%s's status:\n%s", m.name, r.Text)
	return r
}

// take calls an option exactly as a note or refusal offers it, with extra
// arguments (name, value pairs) for the free text the agent writes itself.
// It fails the test when the option leaves out an argument the verb
// requires, which agents would have to guess.
func take[D any](m *machine, o model.Option, extra ...any) reply[D] {
	m.t.Helper()
	if o.Tool == "" {
		m.t.Fatalf("%s: option %+v is a command, not a tool", m.name, o)
	}
	v, ok := api.Lookup(o.Tool)
	if !ok {
		m.t.Fatalf("%s: option %+v names no verb", m.name, o)
	}
	args := map[string]any{}
	for k, val := range o.Args {
		args[k] = val
	}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i].(string)] = extra[i+1]
	}
	for _, a := range v.Args {
		if s, isStr := args[a.Name].(string); a.Required && (args[a.Name] == nil || isStr && s == "") {
			m.t.Fatalf("%s: option %+v leaves out the required argument %s", m.name, o, a.Name)
		}
	}
	return call[D](m, o.Tool, args)
}

// ---- notes and options ----

// note returns the only note of kind, failing the test when there is none
// or more than one.
func note(t *testing.T, who string, notes []model.Note, kind model.NoteKind) model.Note {
	t.Helper()
	var found []model.Note
	for _, n := range notes {
		if n.Kind == kind {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d %s notes, want 1; notes: %v", who, len(found), kind, kinds(notes))
	}
	return found[0]
}

// noNote fails the test when notes has a note of kind.
func noNote(t *testing.T, who string, notes []model.Note, kind model.NoteKind) {
	t.Helper()
	for _, n := range notes {
		if n.Kind == kind {
			t.Fatalf("%s: unexpected %s note %q; notes: %v", who, kind, n.Title, kinds(notes))
		}
	}
}

func kinds(notes []model.Note) []string {
	var out []string
	for _, n := range notes {
		out = append(out, fmt.Sprintf("%s/%s %q", n.Kind, n.Severity, n.Title))
	}
	return out
}

// tools lists the tools a note's options call, in order.
func tools(n model.Note) []string {
	var out []string
	for _, o := range n.Options {
		out = append(out, o.Tool)
	}
	return out
}

// option returns the option of n that calls tool.
func option(t *testing.T, n model.Note, tool string) model.Option {
	t.Helper()
	return optionIn(t, fmt.Sprintf("note %s %q", n.Kind, n.Title), n.Options, tool)
}

// optionIn returns the option of opts, which what offers, that calls tool.
func optionIn(t *testing.T, what string, opts []model.Option, tool string) model.Option {
	t.Helper()
	i := slices.IndexFunc(opts, func(o model.Option) bool { return o.Tool == tool })
	if i < 0 {
		t.Fatalf("%s offers no %s: %+v", what, tool, opts)
	}
	return opts[i]
}

// wantArgs fails the test unless option o calls its tool with exactly args.
func wantArgs(t *testing.T, o model.Option, args map[string]any) {
	t.Helper()
	if len(o.Args) != len(args) {
		t.Fatalf("option %s %v, want arguments %v", o.Tool, o.Args, args)
	}
	for k, v := range args {
		if o.Args[k] != v {
			t.Fatalf("option %s %v, want arguments %v", o.Tool, o.Args, args)
		}
	}
}

// wantText fails the test unless the rendered text has every line in lines,
// each as a substring of one line (options carry an aligned label after the
// call).
func wantText(t *testing.T, who, text string, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if !strings.Contains(text, want) {
			t.Fatalf("%s: the text lacks %q:\n%s", who, want, text)
		}
	}
}

// ---- a fake gh ----

const prURL = "https://github.com/acme/widget/pull/"

// fakeGh leaves only git and a fake gh on PATH for the rest of the test and
// returns the file gh logs its arguments to, one per line. pr create prints
// a new pull request URL per branch; pr view finds a created one open, and
// never reports it merged, so landing must be judged by content. Only shell
// builtins: PATH has nothing else.
func fakeGh(t *testing.T) (log string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "gh.log")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >> '` + log + `'
printf '%s\n' '--' >> '` + log + `'
case "$1 $2" in
"pr view")
	for f in '` + dir + `'/pr-*; do
		[ -f "$f" ] || continue
		read -r branch url < "$f"
		if [ "$3" = "$branch" ] || [ "$3" = "$url" ]; then
			printf '{"url":"%s","state":"OPEN"}\n' "$url"
			exit 0
		fi
	done
	echo 'no pull requests found for branch' >&2
	exit 1 ;;
"pr create")
	head=
	while [ $# -gt 0 ]; do
		[ "$1" = "--head" ] && head=$2
		shift
	done
	n=7
	for f in '` + dir + `'/pr-*; do
		[ -f "$f" ] && n=$((n + 1))
	done
	printf '%s %s%s\n' "$head" '` + prURL + `' "$n" > '` + dir + `'/pr-$n
	printf '%s%s\n' '` + prURL + `' "$n"
	exit 0 ;;
esac
echo "unexpected gh $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return log
}
