package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/install"
	"github.com/crewlet/gitlet/internal/mcpserver"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// fakeEngine records the calls a verb command makes.
type fakeEngine struct {
	opt    engine.Options
	verb   string
	input  any
	closed bool
}

// testApp runs the CLI in-process with fake dependencies.
type testApp struct {
	app            *app
	stdin          io.Reader
	stdout, stderr bytes.Buffer

	engines  []*fakeEngine
	openErr  error
	reply    func(verb string, in any) (api.Output, error)
	installs []install.Options
	changes  []install.Change
	hooks    [][]string
	hookFunc func(args []string, stdin io.Reader, stdout, stderr io.Writer) int
	repoErr  error
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	ta := &testApp{stdin: strings.NewReader("")}
	ta.reply = func(verb string, in any) (api.Output, error) {
		return &api.Reply[struct{}]{Result: api.Result{Outcome: api.OK, Summary: verb + " done"}}, nil
	}
	ta.app = &app{deps: deps{
		openEngine: func(_ context.Context, opt engine.Options) (verbEngine, error) {
			if ta.openErr != nil {
				return nil, ta.openErr
			}
			e := &fakeEngine{opt: opt}
			ta.engines = append(ta.engines, e)
			return &fakeHandle{e: e, ta: ta}, nil
		},
		openRepo: func(_ context.Context, dir string) (*gitx.Repo, error) {
			if ta.repoErr != nil {
				return nil, ta.repoErr
			}
			return &gitx.Repo{Root: "/repo", GitDir: "/repo/.git", CommonDir: "/repo/.git"}, nil
		},
		install: func(_ context.Context, opt install.Options) ([]install.Change, error) {
			ta.installs = append(ta.installs, opt)
			return ta.changes, nil
		},
		hook: func(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
			ta.hooks = append(ta.hooks, args)
			if ta.hookFunc != nil {
				return ta.hookFunc(args, stdin, stdout, stderr)
			}
			return 0
		},
		serve: func(context.Context, mcpserver.Options, io.Reader, io.Writer) error { return nil },
		ghStatus: func(context.Context) (bool, bool) {
			return true, false
		},
	}}
	return ta
}

type fakeHandle struct {
	e  *fakeEngine
	ta *testApp
}

func (h *fakeHandle) Call(_ context.Context, verb string, in any) (api.Output, error) {
	h.e.verb, h.e.input = verb, in
	return h.ta.reply(verb, in)
}

func (h *fakeHandle) Close() error {
	h.e.closed = true
	return nil
}

func (ta *testApp) run(args ...string) int {
	ta.stdout.Reset()
	ta.stderr.Reset()
	ta.app.stdin, ta.app.stdout, ta.app.stderr = ta.stdin, &ta.stdout, &ta.stderr
	return ta.app.main(context.Background(), args)
}

func (ta *testApp) lastEngine(t *testing.T) *fakeEngine {
	t.Helper()
	if len(ta.engines) == 0 {
		t.Fatal("no engine was opened")
	}
	return ta.engines[len(ta.engines)-1]
}

func findCommand(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("no command %q", name)
	return nil
}

func TestVerbCommandsMatchTable(t *testing.T) {
	root := newTestApp(t).app.rootCommand()
	for _, v := range api.Verbs {
		cmd := findCommand(t, root, v.Name)
		if cmd.Annotations[annVerb] != v.Name {
			t.Errorf("%s: not marked as a verb command", v.Name)
		}
		if cmd.Short != v.Summary {
			t.Errorf("%s: short help %q, want the summary", v.Name, cmd.Short)
		}
		wantUse := []string{v.Name}
		wantFlags := map[string]string{"worktree": "string", "json": "bool"}
		for _, a := range v.Args {
			if a.Required {
				wantUse = append(wantUse, "<"+a.Name+">")
				if !strings.Contains(cmd.Long, "<"+a.Name+">") || !strings.Contains(cmd.Long, a.Help) {
					t.Errorf("%s: long help does not explain <%s>: %q", v.Name, a.Name, cmd.Long)
				}
				continue
			}
			wantFlags[a.Name] = map[string]string{
				api.KindString: "string", api.KindStrings: "stringSlice", api.KindBool: "bool", api.KindObject: "string",
			}[a.Kind]
		}
		if got := strings.Join(wantUse, " "); cmd.Use != got {
			t.Errorf("%s: use %q, want %q", v.Name, cmd.Use, got)
		}
		gotFlags := map[string]string{}
		cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) { gotFlags[f.Name] = f.Value.Type() })
		delete(gotFlags, "help")
		if !reflect.DeepEqual(gotFlags, wantFlags) {
			t.Errorf("%s: flags %v, want %v", v.Name, gotFlags, wantFlags)
		}
		if !strings.Contains(cmd.Example, example(v)) {
			t.Errorf("%s: help has no example", v.Name)
		}
	}
}

func TestOverview(t *testing.T) {
	ta := newTestApp(t)
	for _, args := range [][]string{nil, {"--help"}, {"help"}} {
		if code := ta.run(args...); code != exitOK {
			t.Fatalf("%v: exit %d, stderr %q", args, code, ta.stderr.String())
		}
		out := ta.stdout.String()
		last := -1
		for _, g := range groups {
			i := strings.Index(out, "\n"+g.title+":\n")
			if i < last {
				t.Errorf("%v: group %q missing or out of order", args, g.title)
			}
			last = i
		}
		for _, name := range append(verbNames(), "init", "mcp", "doctor", "hook", "version") {
			if n := strings.Count(out, "\n  "+name+" "); n != 1 {
				t.Errorf("%v: %s listed %d times", args, name, n)
			}
		}
	}
	for _, v := range api.Verbs {
		if !inGroups(v.Name) {
			t.Errorf("verb %s has no group in the overview", v.Name)
		}
	}
}

func verbNames() []string {
	var names []string
	for _, v := range api.Verbs {
		names = append(names, v.Name)
	}
	return names
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"need"}, []string{"gitlet need: missing <symbol>", "usage: gitlet need <symbol> [--requirement <requirement>]", "example: gitlet need"}},
		{[]string{"discuss"}, []string{"missing <about> and <message>"}},
		{[]string{"discuss", "n-3f2a"}, []string{"missing <message>"}},
		{[]string{"start", "Add", "strict", "mode"}, []string{"expected 1 argument (<intent>), got 3; quote arguments that contain spaces"}},
		{[]string{"status", "now"}, []string{`gitlet status: takes no arguments, got "now"`}},
		{[]string{"need", "f", "--requirment", "x"}, []string{"unknown flag: --requirment", "usage: gitlet need"}},
		{[]string{"stat"}, []string{`unknown command "stat"`, "did you mean status?"}},
		{[]string{"dismiss", "n-3f2a", "maybe"}, []string{`<as> must be not_needed or false_alarm, got "maybe"`}},
		{[]string{"who", " "}, []string{"<target> must not be empty"}},
		{[]string{"discuss", "n-3f2a", "-1 for this"}, []string{"unknown shorthand flag", "put -- before"}},
		{[]string{"discuss", "n-3f2a", "hi", "--proposal", "me"}, []string{"--proposal must be a JSON object"}},
		{[]string{"discuss", "n-3f2a", "hi", "--proposal", `{"owner":"me"}`}, []string{"--proposal needs summary"}},
		{[]string{"discuss", "n-3f2a", "hi", "--proposal", `{"owner":"me","summary":"x","color":"red"}`}, []string{`--proposal has unknown field "color"`}},
		{[]string{"discuss", "n-3f2a", "hi", "--proposal", `{"owner":1,"summary":"x"}`}, []string{"--proposal: owner must be a JSON string"}},
		{[]string{"submit", "--draft=maybe"}, []string{"invalid argument"}},
		{[]string{"init", "--client", "vscode"}, []string{`unknown client "vscode"; use claude-code, codex or cursor`}},
		{[]string{"version", "now"}, []string{"takes no arguments"}},
		{[]string{"doctor", "--fix"}, []string{"unknown flag: --fix", "usage: gitlet doctor"}},
	}
	for _, c := range cases {
		ta := newTestApp(t)
		if code := ta.run(c.args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d; stderr %q", c.args, code, exitUsage, ta.stderr.String())
		}
		for _, w := range c.want {
			if !strings.Contains(ta.stderr.String(), w) {
				t.Errorf("%q: stderr does not mention %q:\n%s", c.args, w, ta.stderr.String())
			}
		}
		if ta.stdout.Len() > 0 {
			t.Errorf("%q: usage error wrote to stdout: %q", c.args, ta.stdout.String())
		}
		if len(ta.engines) > 0 || len(ta.installs) > 0 {
			t.Errorf("%q: a usage error still ran the command", c.args)
		}
	}
}

func TestArgumentMapping(t *testing.T) {
	wd, _ := os.Getwd()
	other := t.TempDir()
	cases := []struct {
		args []string
		verb string
		want any
		dir  string
	}{
		{[]string{"need", "src/config.ts:parseConfig", "--requirement", "keep returning Config", "--note", "n-3f2a"},
			"need", &api.NeedInput{Symbol: "src/config.ts:parseConfig", Requirement: "keep returning Config", Note: "n-3f2a"}, wd},
		{[]string{"start", "Add strict mode", "--paths", "a.go,b.go", "--paths", "c.go", "--branch", "feat"},
			"start", &api.StartInput{Intent: "Add strict mode", Branch: "feat", Paths: []string{"a.go", "b.go", "c.go"}}, wd},
		{[]string{"discuss", "t-8c1f", "I can own it", "--proposal", `{"owner":"me","signature":"f(x int)","summary":"I own f"}`},
			"discuss", &api.DiscussInput{About: "t-8c1f", Message: "I can own it",
				Proposal: &api.ProposalInput{Owner: "me", Signature: "f(x int)", Summary: "I own f"}}, wd},
		{[]string{"discuss", "--", "n-3f2a", "-1 for this"},
			"discuss", &api.DiscussInput{About: "n-3f2a", Message: "-1 for this"}, wd},
		{[]string{"submit", "--draft", "--title", "T"}, "submit", &api.SubmitInput{Title: "T", Draft: true}, wd},
		{[]string{"submit", "--draft=false"}, "submit", &api.SubmitInput{}, wd},
		{[]string{"dismiss", "n-3f2a", "false_alarm", "--reason", "gitlet is wrong"},
			"dismiss", &api.DismissInput{Note: "n-3f2a", As: "false_alarm", Reason: "gitlet is wrong"}, wd},
		{[]string{"who", "f", "--worktree", other},
			"who", &api.WhoInput{Target: "f", Where: api.Where{Worktree: other}}, other},
		{[]string{"status"}, "status", &api.StatusInput{}, wd},
		{[]string{"update", "--note=-n-odd"}, "update", &api.UpdateInput{Note: "-n-odd"}, wd},
	}
	for _, c := range cases {
		ta := newTestApp(t)
		if code := ta.run(c.args...); code != exitOK {
			t.Errorf("%q: exit %d; stderr %q", c.args, code, ta.stderr.String())
			continue
		}
		e := ta.lastEngine(t)
		if e.verb != c.verb || !reflect.DeepEqual(e.input, c.want) {
			t.Errorf("%q: called %s with %#v, want %s %#v", c.args, e.verb, e.input, c.verb, c.want)
		}
		if e.opt.Dir != c.dir || e.opt.Client != "cli" || e.opt.Session {
			t.Errorf("%q: engine options %+v", c.args, e.opt)
		}
		if !e.closed {
			t.Errorf("%q: engine not closed", c.args)
		}
	}
}

// shellWords splits a command line the way a POSIX shell would, for the
// quoting the examples and render use (single and double quotes,
// backslash escapes outside quotes).
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		t.Fatalf("unterminated quote in %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func TestExamplesRun(t *testing.T) {
	for _, v := range api.Verbs {
		for _, ex := range append([]string{example(v)}, extraExamples[v.Name]...) {
			words := shellWords(t, ex)
			if words[0] != "gitlet" || words[1] != v.Name {
				t.Errorf("%s: example %q does not call it", v.Name, ex)
				continue
			}
			ta := newTestApp(t)
			if code := ta.run(words[1:]...); code != exitOK {
				t.Errorf("%s: example %q exits %d: %s", v.Name, ex, code, ta.stderr.String())
				continue
			}
			if e := ta.lastEngine(t); e.verb != v.Name {
				t.Errorf("%s: example %q called %s", v.Name, ex, e.verb)
			}
		}
	}
}

// TestRenderedOptionsRun checks the contract with render: an option written
// as a command runs as written and reaches the engine with the option's
// arguments.
func TestRenderedOptionsRun(t *testing.T) {
	options := []model.Option{
		{Tool: "need", Args: map[string]any{"symbol": "src/config.ts:parseConfig", "note": "n-3f2a9c1d0e4b"}},
		{Tool: "dismiss", Args: map[string]any{"note": "n-3f2a9c1d0e4b", "as": "not_needed", "reason": "it's only a comment"}},
		{Tool: "discuss", Args: map[string]any{"about": "t-9b2c", "message": "I can own it",
			"proposal": map[string]any{"owner": "me", "signature": "f(x int) error", "summary": "I own f"}}},
		{Tool: "discuss", Args: map[string]any{"about": "n-3f2a", "message": "-1, I need the old form"}},
		{Tool: "start", Args: map[string]any{"intent": "Add strict mode", "paths": []string{"a.go", "dir with space/b.go"}}},
		{Tool: "submit", Args: map[string]any{"draft": false, "title": "--odd title", "worktree": t.TempDir()}},
		{Tool: "update", Args: map[string]any{"note": "n-3f2a9c1d0e4b"}},
	}
	for _, o := range options {
		line := render.Option(o, render.CLI)
		words := shellWords(t, line)
		if len(words) < 2 || words[0] != "gitlet" || words[1] != o.Tool {
			t.Errorf("%s: rendered %q", o.Tool, line)
			continue
		}
		ta := newTestApp(t)
		if code := ta.run(words[1:]...); code != exitOK {
			t.Errorf("%q exits %d: %s", line, code, ta.stderr.String())
			continue
		}
		v, _ := api.Lookup(o.Tool)
		want := v.NewInput()
		data, _ := json.Marshal(o.Args)
		if err := json.Unmarshal(data, want); err != nil {
			t.Fatal(err)
		}
		if e := ta.lastEngine(t); e.verb != o.Tool || !reflect.DeepEqual(e.input, want) {
			t.Errorf("%q called %s with %+v, want %+v", line, e.verb, e.input, want)
		}
	}
}

func whoReply(outcome api.Outcome) *api.Reply[api.WhoData] {
	return &api.Reply[api.WhoData]{
		Result: api.Result{Outcome: outcome, Summary: "one agent changes src/a.ts", Details: []string{"a fact"},
			Options: []model.Option{{Label: "ask them", Tool: "discuss", Args: map[string]any{"about": "n-3f2a"}}}, Open: 2, OpenAct: 1},
		Data: api.WhoData{Target: "src/a.ts", Entries: []api.WhoEntry{{Agent: "codex-ci-1a2b3c", Role: "changes"}}},
	}
}

func TestOutcomesAndExitCodes(t *testing.T) {
	for _, c := range []struct {
		outcome api.Outcome
		code    int
	}{{api.OK, exitOK}, {api.NothingToDo, exitOK}, {api.Refused, exitRefused}} {
		ta := newTestApp(t)
		r := whoReply(c.outcome)
		ta.reply = func(string, any) (api.Output, error) { return r, nil }
		if code := ta.run("who", "src/a.ts"); code != c.code {
			t.Errorf("%s: exit %d, want %d", c.outcome, code, c.code)
		}
		want := render.Text(&r.Result, render.CLI)
		if want != "" && !strings.HasSuffix(want, "\n") {
			want += "\n"
		}
		if got := ta.stdout.String(); got != want {
			t.Errorf("%s: stdout %q, want render.Text %q", c.outcome, got, want)
		}
		if ta.stderr.Len() > 0 {
			t.Errorf("%s: stderr %q", c.outcome, ta.stderr.String())
		}
	}

	errorCases := []struct {
		name    string
		openErr error
		reply   func(string, any) (api.Output, error)
		want    string
	}{
		{"call fails", nil, func(string, any) (api.Output, error) { return nil, errors.New("ledger: database is locked") },
			"gitlet: who: ledger: database is locked\n"},
		{"open fails", errors.New("engine: not a git repository"), nil, "gitlet: who: engine: not a git repository\n"},
		{"no result", nil, func(string, any) (api.Output, error) { return (*api.Reply[api.WhoData])(nil), nil },
			"gitlet: who: the engine returned no result\n"},
	}
	for _, c := range errorCases {
		ta := newTestApp(t)
		ta.openErr = c.openErr
		if c.reply != nil {
			ta.reply = c.reply
		}
		if code := ta.run("who", "src/a.ts"); code != exitError {
			t.Errorf("%s: exit %d, want %d", c.name, code, exitError)
		}
		if got := ta.stderr.String(); got != c.want {
			t.Errorf("%s: stderr %q, want %q", c.name, got, c.want)
		}
		if ta.stdout.Len() > 0 {
			t.Errorf("%s: stdout %q", c.name, ta.stdout.String())
		}
	}
}

func TestJSONOutput(t *testing.T) {
	ta := newTestApp(t)
	r := whoReply(api.Refused)
	ta.reply = func(string, any) (api.Output, error) { return r, nil }
	if code := ta.run("who", "src/a.ts", "--json"); code != exitRefused {
		t.Errorf("exit %d, want %d", code, exitRefused)
	}
	out := ta.stdout.String()
	want, _ := json.MarshalIndent(r, "", "  ")
	if out != string(want)+"\n" {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(out), &shape); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"outcome", "summary", "details", "options", "open", "openAct", "data"} {
		if _, ok := shape[key]; !ok {
			t.Errorf("no %q in %s", key, out)
		}
	}
	if data, _ := shape["data"].(map[string]any); data["target"] != "src/a.ts" {
		t.Errorf("data %v", shape["data"])
	}
}

func TestAgentOverride(t *testing.T) {
	ta := newTestApp(t)
	t.Setenv("GITLET_AGENT", "claude-code-laptop-3fa9c1")
	if code := ta.run("status"); code != exitOK {
		t.Fatalf("exit %d: %s", code, ta.stderr.String())
	}
	if got := ta.lastEngine(t).opt.Agent; got != "claude-code-laptop-3fa9c1" {
		t.Errorf("agent %q", got)
	}
	t.Setenv("GITLET_AGENT", "Not Valid!")
	if code := ta.run("status"); code != exitError || !strings.Contains(ta.stderr.String(), "GITLET_AGENT") {
		t.Errorf("invalid GITLET_AGENT: exit %d, stderr %q", code, ta.stderr.String())
	}
}

func TestVersion(t *testing.T) {
	defer func(v, c, d string) { Version, Commit, Date = v, c, d }(Version, Commit, Date)
	Version, Commit, Date = "0.2.0", "1d0f9af", "2026-10-26T03:00:00Z"
	for _, args := range [][]string{{"version"}, {"--version"}} {
		ta := newTestApp(t)
		if code := ta.run(args...); code != exitOK {
			t.Errorf("%v: exit %d", args, code)
		}
		if got, want := ta.stdout.String(), "gitlet 0.2.0 (commit 1d0f9af, built 2026-10-26T03:00:00Z)\n"; got != want {
			t.Errorf("%v: %q, want %q", args, got, want)
		}
	}
	Commit, Date = "", ""
	ta := newTestApp(t)
	ta.run("version")
	if got := ta.stdout.String(); got != "gitlet 0.2.0\n" {
		t.Errorf("without build info: %q", got)
	}
}

func TestHookPassthrough(t *testing.T) {
	ta := newTestApp(t)
	ta.stdin = strings.NewReader(`{"cwd":"/repo"}`)
	var gotStdin string
	ta.hookFunc = func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		b, _ := io.ReadAll(stdin)
		gotStdin = string(b)
		fmt.Fprint(stdout, `{"hookSpecificOutput":{}}`)
		return 0
	}
	args := []string{"claude-code", "post-tool-use", "--weird", "-x", "--help"}
	if code := ta.run(append([]string{"hook"}, args...)...); code != exitOK {
		t.Errorf("exit %d", code)
	}
	if len(ta.hooks) != 1 || !slices.Equal(ta.hooks[0], args) {
		t.Errorf("hook got %q, want %q", ta.hooks, args)
	}
	if gotStdin != `{"cwd":"/repo"}` {
		t.Errorf("hook stdin %q", gotStdin)
	}
	if ta.stdout.String() != `{"hookSpecificOutput":{}}` || ta.stderr.Len() > 0 {
		t.Errorf("extra output: stdout %q, stderr %q", ta.stdout.String(), ta.stderr.String())
	}

	ta.hookFunc = func([]string, io.Reader, io.Writer, io.Writer) int { return 5 }
	if code := ta.run("hook", "sequence-editor", "todo"); code != 5 {
		t.Errorf("exit %d, want the hook's 5", code)
	}
	if ta.stdout.Len()+ta.stderr.Len() > 0 {
		t.Errorf("extra output: stdout %q, stderr %q", ta.stdout.String(), ta.stderr.String())
	}
}

func TestInitDryRun(t *testing.T) {
	ta := newTestApp(t)
	ta.changes = []install.Change{
		{Path: "/repo/.mcp.json", Action: "create"},
		{Path: "/repo/.claude/settings.json", Action: "update"},
		{Path: "/repo/AGENTS.md", Action: "unchanged"},
		{Path: "/repo/.git/hooks/pre-push", Action: "skip", Summary: "add: gitlet hook git pre-push \"$@\""},
	}
	if code := ta.run("init", "--dry-run", "--client", "claude-code,codex"); code != exitOK {
		t.Fatalf("exit %d: %s", code, ta.stderr.String())
	}
	want := install.Options{Root: "/repo", Clients: []string{"claude-code", "codex"}, DryRun: true}
	if len(ta.installs) != 1 || !reflect.DeepEqual(ta.installs[0], want) {
		t.Errorf("install options %+v, want %+v", ta.installs, want)
	}
	wantOut := `create     .mcp.json
update     .claude/settings.json
unchanged  AGENTS.md
skip       .git/hooks/pre-push: add: gitlet hook git pre-push "$@"
Dry run: nothing was written.
`
	if got := ta.stdout.String(); got != wantOut {
		t.Errorf("output:\n%s\nwant:\n%s", got, wantOut)
	}

	ta.repoErr = fmt.Errorf("open: %w", gitx.ErrNotRepo)
	if code := ta.run("init"); code != exitError || !strings.Contains(ta.stderr.String(), "is not inside a git work tree") {
		t.Errorf("outside a repository: exit %d, stderr %q", code, ta.stderr.String())
	}
}

func TestDoctorOutsideRepository(t *testing.T) {
	ta := newTestApp(t)
	ta.repoErr = fmt.Errorf("open: %w", gitx.ErrNotRepo)
	if code := ta.run("doctor"); code != exitError {
		t.Errorf("exit %d, want %d", code, exitError)
	}
	out := ta.stdout.String()
	for _, w := range []string{"fail  repository", "is not inside a git work tree", "warn  gh", "not signed in", "1 check failed."} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor output does not mention %q:\n%s", w, out)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// TestMCPCommand runs gitlet mcp in-process on pipes and lists the tools
// over raw JSON-RPC.
func TestMCPCommand(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		code := Main([]string{"mcp", "--worktree", t.TempDir()}, inR, outW, &stderr)
		outW.Close()
		done <- code
	}()

	send := func(msg string) {
		if _, err := io.WriteString(inW, msg+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	responses := bufio.NewScanner(outR)
	responses.Buffer(nil, 1<<20)
	read := func(id int) map[string]any {
		for responses.Scan() {
			var m map[string]any
			if err := json.Unmarshal(responses.Bytes(), &m); err != nil {
				t.Fatalf("bad JSON-RPC line %q: %v", responses.Text(), err)
			}
			if m["id"] == float64(id) {
				return m
			}
		}
		t.Fatalf("no response %d: %v", id, responses.Err())
		return nil
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"1"}}}`)
	init := read(1)
	if res, _ := init["result"].(map[string]any); res["instructions"] != mcpserver.Instructions {
		t.Errorf("initialize result %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := read(2)
	res, _ := list["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, verbNames()) {
		t.Errorf("tools %v, want %v", names, verbNames())
	}

	inW.Close()
	go io.Copy(io.Discard, outR)
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("gitlet mcp exited %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gitlet mcp did not exit after stdin closed")
	}
}

// TestToolsAndCommandsCoverSameVerbs checks that the MCP server and the CLI
// expose exactly the verbs of the table.
func TestToolsAndCommandsCoverSameVerbs(t *testing.T) {
	s := mcpserver.New(mcpserver.Options{Dir: t.TempDir(), TickInterval: -1, Logger: slog.New(slog.DiscardHandler)})
	defer s.Close()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cs.Close(); ss.Wait() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, tool := range res.Tools {
		tools = append(tools, tool.Name)
	}

	var commands []string
	for _, c := range newTestApp(t).app.rootCommand().Commands() {
		if c.Annotations[annVerb] != "" {
			commands = append(commands, c.Name())
		}
	}
	slices.Sort(commands)
	sortedVerbs := slices.Sorted(slices.Values(verbNames()))

	if !slices.Equal(tools, verbNames()) {
		t.Errorf("MCP tools %v, want the verb table %v", tools, verbNames())
	}
	if !slices.Equal(commands, sortedVerbs) {
		t.Errorf("CLI verb commands %v, want %v", commands, sortedVerbs)
	}
}

// ---- integration (real engine) ----

// tempRepo creates a git repository with one commit holding a Go file.
func tempRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "package config\n\n// ParseConfig parses s.\nfunc ParseConfig(s string) (bool, error) {\n\treturn false, nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "add", "config.go")
	runGit(t, dir, "-c", "user.name=gitlet test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "Add config parsing")
	return dir
}

func TestIntegrationVerbs(t *testing.T) {
	dir := tempRepo(t)
	t.Setenv("GITLET_AGENT", "")
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := Main(append(args, "--worktree", dir), strings.NewReader(""), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, out, errOut := run("status")
	if code != exitOK || !strings.HasPrefix(out, "ok: ") {
		t.Fatalf("status: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = run("start", "Add strict mode to config loading", "--paths", "config.go"); code != exitOK {
		t.Errorf("start: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = run("status", "--json")
	var reply struct {
		api.Result
		Data api.StatusData `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil || code != exitOK {
		t.Fatalf("status --json: exit %d, %v\n%s%s", code, err, out, errOut)
	}
	if reply.Data.Agent.Client != "cli" || len(reply.Data.Items) == 0 {
		t.Errorf("status data %+v", reply.Data)
	}
	if code, out, errOut = run("dismiss", "n-000000000000", "not_needed"); code != exitRefused || !strings.HasPrefix(out, "refused: ") {
		t.Errorf("dismiss of an unknown note: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = run("abandon", "--reason", "integration test done"); code == exitError || code == exitUsage {
		t.Errorf("abandon: exit %d\n%s%s", code, out, errOut)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestIntegrationDoctorSeveralRemotes(t *testing.T) {
	dir := tempRepo(t)
	runGit(t, dir, "remote", "add", "upstream", filepath.Join(t.TempDir(), "upstream.git"))
	runGit(t, dir, "remote", "add", "public", filepath.Join(t.TempDir(), "public.git"))
	ta := newTestApp(t)
	ta.app.deps.openRepo = gitx.Open
	ta.run("doctor", "--worktree", dir)
	out := ta.stdout.String()
	for _, w := range []string{"warn  remote", "the remotes public, upstream exist, none is origin", "git config gitlet.remote <name>", "skip  ref access"} {
		if !strings.Contains(out, w) {
			t.Errorf("doctor output does not mention %q:\n%s", w, out)
		}
	}
}

func TestIntegrationInitDryRun(t *testing.T) {
	dir := tempRepo(t)
	var stdout, stderr bytes.Buffer
	code := Main([]string{"init", "--dry-run", "--client", "claude-code", "--worktree", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), ".mcp.json") || !strings.HasSuffix(stdout.String(), "Dry run: nothing was written.\n") {
		t.Errorf("output:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry run wrote .mcp.json: %v", err)
	}
}
