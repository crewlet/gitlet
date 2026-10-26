package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/engine"
	"github.com/crewlet/gitlet/internal/install"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// fakeEngine is a backend that records what the server asks of it.
type fakeEngine struct {
	opt engine.Options

	mu        sync.Mutex
	calls     []fakeCall
	ticks     int
	shutdowns int
	reply     func(verb string, in any) (api.Output, error)
	tickErr   error
}

type fakeCall struct {
	verb string
	in   any
}

func (f *fakeEngine) Call(_ context.Context, verb string, in any) (api.Output, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{verb, in})
	reply := f.reply
	f.mu.Unlock()
	if reply != nil {
		return reply(verb, in)
	}
	return &api.Reply[struct{}]{Result: api.Result{Outcome: api.OK, Summary: verb + " done"}}, nil
}

func (f *fakeEngine) Tick(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ticks++
	return f.tickErr
}

func (f *fakeEngine) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	return nil
}

func (f *fakeEngine) ReadOnly() bool { return false }

func (f *fakeEngine) Agent() model.Agent {
	return model.Agent{ID: model.AgentID(f.opt.Client + "-host-abc123"), Client: f.opt.Client}
}

func (f *fakeEngine) snapshot() (calls []fakeCall, ticks, shutdowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), f.ticks, f.shutdowns
}

// harness is a server with fake engines, connected to an in-memory client.
type harness struct {
	s  *Server
	cs *mcp.ClientSession

	mu      sync.Mutex
	opened  []*fakeEngine
	prepare func(*fakeEngine) // configures each engine as it opens
}

type harnessOptions struct {
	opt     Options
	client  string // clientInfo name; default "claude-code"
	legacy  bool   // use the initialize handshake instead of server/discover
	roots   map[string]string
	prepare func(*fakeEngine)
}

func newHarness(t *testing.T, ho harnessOptions) *harness {
	t.Helper()
	if ho.opt.Logger == nil {
		ho.opt.Logger = slog.New(slog.DiscardHandler)
	}
	if ho.opt.TickInterval == 0 {
		ho.opt.TickInterval = -1
	}
	if ho.opt.Dir == "" {
		ho.opt.Dir = "/repo"
	}
	if ho.client == "" {
		ho.client = "claude-code"
	}
	h := &harness{prepare: ho.prepare}
	h.s = New(ho.opt)
	h.s.openEngine = func(_ context.Context, opt engine.Options) (backend, error) {
		f := &fakeEngine{opt: opt}
		if h.prepare != nil {
			h.prepare(f)
		}
		h.mu.Lock()
		h.opened = append(h.opened, f)
		h.mu.Unlock()
		return f, nil
	}
	h.s.findRoot = func(_ context.Context, dir string) (string, error) {
		if root, ok := ho.roots[dir]; ok {
			return root, nil
		}
		if ho.roots == nil {
			return dir, nil
		}
		return "", fmt.Errorf("%s: %w", dir, errNotRepoForTest)
	}

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := h.s.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	var copt *mcp.ClientSessionOptions
	if ho.legacy {
		copt = &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: ho.client, Version: "1.0"}, nil)
	h.cs, err = client.Connect(ctx, ct, copt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.cs.Close()
		ss.Wait()
		h.s.Close()
	})
	return h
}

var errNotRepoForTest = errors.New("not inside a git work tree")

func (h *harness) engines() []*fakeEngine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.opened)
}

func (h *harness) call(t *testing.T, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestToolsMatchVerbTable(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != len(api.Verbs) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(api.Verbs))
	}
	for i, v := range api.Verbs {
		tool := res.Tools[i]
		if tool.Name != v.Name {
			t.Errorf("tool %d is %q, want %q (table order)", i, tool.Name, v.Name)
			continue
		}
		if tool.Description != v.Summary {
			t.Errorf("%s: description %q, want the verb summary", v.Name, tool.Description)
		}
		if tool.Title == "" || tool.Title == "gitlet "+v.Name {
			t.Errorf("%s: no title of its own", v.Name)
		}

		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", v.Name)
			continue
		}
		if a.ReadOnlyHint != v.ReadOnly || a.IdempotentHint != v.Idempotent {
			t.Errorf("%s: readOnly=%v idempotent=%v, want %v %v", v.Name, a.ReadOnlyHint, a.IdempotentHint, v.ReadOnly, v.Idempotent)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != v.Destructive {
			t.Errorf("%s: destructiveHint %v, want %v", v.Name, a.DestructiveHint, v.Destructive)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint != v.OpenWorld {
			t.Errorf("%s: openWorldHint %v, want %v", v.Name, a.OpenWorldHint, v.OpenWorld)
		}
		if a.Title != tool.Title {
			t.Errorf("%s: annotation title %q, tool title %q", v.Name, a.Title, tool.Title)
		}

		checkSchema(t, v, tool.InputSchema)
	}
}

// checkSchema compares a tool's input schema, as a client sees it, with the
// verb table.
func checkSchema(t *testing.T, v api.Verb, raw any) {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var sch struct {
		Type                 any                       `json:"type"`
		AdditionalProperties *bool                     `json:"additionalProperties"`
		Required             []string                  `json:"required"`
		Properties           map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &sch); err != nil {
		t.Fatalf("%s: %v", v.Name, err)
	}
	if sch.Type != "object" {
		t.Errorf("%s: schema type %v", v.Name, sch.Type)
	}
	if sch.AdditionalProperties == nil || *sch.AdditionalProperties {
		t.Errorf("%s: schema allows unknown arguments", v.Name)
	}
	var wantRequired []string
	want := map[string]bool{"worktree": true}
	for _, a := range v.Args {
		want[a.Name] = true
		if a.Required {
			wantRequired = append(wantRequired, a.Name)
		}
	}
	if !slices.Equal(sch.Required, wantRequired) {
		t.Errorf("%s: required %v, want %v", v.Name, sch.Required, wantRequired)
	}
	for name := range want {
		if _, ok := sch.Properties[name]; !ok {
			t.Errorf("%s: no property %q", v.Name, name)
		}
	}
	for name, p := range sch.Properties {
		if !want[name] {
			t.Errorf("%s: property %q is not in the verb table", v.Name, name)
		}
		if d, _ := p["description"].(string); d == "" {
			t.Errorf("%s: property %q has no description", v.Name, name)
		}
		if _, isList := p["type"].([]any); isList {
			t.Errorf("%s: property %q has a type list %v", v.Name, name, p["type"])
		}
	}
	whereTag, _ := reflect.TypeFor[api.Where]().Field(0).Tag.Lookup("jsonschema")
	if d := sch.Properties["worktree"]["description"]; d != whereTag {
		t.Errorf("%s: worktree description %q, want %q", v.Name, d, whereTag)
	}
	for _, a := range v.Args {
		if len(a.Enum) == 0 {
			continue
		}
		got, _ := json.Marshal(sch.Properties[a.Name]["enum"])
		wantEnum, _ := json.Marshal(a.Enum)
		if !bytes.Equal(got, wantEnum) {
			t.Errorf("%s: %s enum %s, want %s", v.Name, a.Name, got, wantEnum)
		}
	}
}

func TestSchemaFieldsMatchVerbArgs(t *testing.T) {
	for _, v := range api.Verbs {
		sch, err := inputSchema(v)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		var names []string
		for _, a := range v.Args {
			names = append(names, a.Name)
			p := sch.Properties[a.Name]
			if p == nil {
				t.Errorf("%s: arg %q has no field in %s", v.Name, a.Name, v.Input)
				continue
			}
			wantType := map[string]string{api.KindString: "string", api.KindStrings: "array", api.KindBool: "boolean", api.KindObject: "object"}[a.Kind]
			if p.Type != wantType {
				t.Errorf("%s: %s has type %q, want %q for kind %s", v.Name, a.Name, p.Type, wantType, a.Kind)
			}
		}
		if got := sch.PropertyOrder; !slices.Equal(got, append(names, "worktree")) {
			t.Errorf("%s: properties %v, want the table's args then worktree: %v", v.Name, got, append(names, "worktree"))
		}
	}
}

func TestInitialize(t *testing.T) {
	h := newHarness(t, harnessOptions{opt: Options{Version: "1.2.3"}, legacy: true})
	init := h.cs.InitializeResult()
	if init.Instructions != Instructions {
		t.Errorf("instructions %q", init.Instructions)
	}
	if init.ServerInfo == nil || init.ServerInfo.Name != "gitlet" || init.ServerInfo.Version != "1.2.3" {
		t.Errorf("server info %+v", init.ServerInfo)
	}
	if init.Capabilities == nil || init.Capabilities.Tools == nil {
		t.Errorf("no tools capability: %+v", init.Capabilities)
	}
}

func TestBadArguments(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	cases := []struct {
		tool string
		args any
		want []string
	}{
		{"start", map[string]any{"intnt": "x"}, []string{`unknown argument "intnt"`, "intent, branch, paths, worktree"}},
		{"start", map[string]any{"intent": "x", "a": 1, "b": 2}, []string{`unknown arguments "a", "b"`}},
		{"start", map[string]any{}, []string{"missing required argument for start: intent"}},
		{"start", map[string]any{"intent": "  "}, []string{"missing required argument for start: intent"}},
		{"discuss", map[string]any{}, []string{"missing required arguments", "about", "message"}},
		{"who", map[string]any{"target": 5}, []string{`invalid arguments for who: target: type: 5 has type "integer", want "string"`}},
		{"start", map[string]any{"intent": "x", "paths": "a.go"}, []string{"invalid arguments for start"}},
		{"dismiss", map[string]any{"note": "n-1234", "as": "maybe"}, []string{"invalid arguments for dismiss"}},
		{"discuss", map[string]any{"about": "n-1234", "message": "hi", "proposal": map[string]any{"owner": "me", "color": "red"}},
			[]string{`unknown argument "proposal.color"`, "valid fields of proposal: owner, signature, summary"}},
		{"discuss", map[string]any{"about": "n-1234", "message": "hi", "proposal": map[string]any{"owner": "me"}}, []string{`invalid arguments for discuss: proposal: required: missing properties: ["summary"]`}},
		{"status", map[string]any{"worktree": "relative/dir"}, []string{"worktree must be an absolute path"}},
	}
	for _, c := range cases {
		res := h.call(t, c.tool, c.args)
		got := text(res)
		t.Logf("%s %v: %s", c.tool, c.args, got)
		if !res.IsError {
			t.Errorf("%s %v: not an error: %s", c.tool, c.args, got)
		}
		if !strings.HasPrefix(got, "error: ") {
			t.Errorf("%s %v: text %q does not start with error:", c.tool, c.args, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %v: %q does not mention %q", c.tool, c.args, got, w)
			}
		}
	}
	if n := len(h.engines()); n != 0 {
		t.Errorf("bad arguments opened %d engines", n)
	}
}

func TestArgumentMapping(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.call(t, "need", map[string]any{"symbol": "src/config.ts:parseConfig", "requirement": "keep returning Config", "note": "n-3f2a"})
	h.call(t, "start", map[string]any{"intent": "Add strict mode", "paths": []string{"a.go", "b.go"}, "branch": nil})
	h.call(t, "discuss", map[string]any{"about": "t-8c1f", "message": "I can own it",
		"proposal": map[string]any{"owner": "me", "signature": "f(x int)", "summary": "I own f"}})
	h.call(t, "submit", map[string]any{"draft": true})
	h.call(t, "status", nil)

	engines := h.engines()
	if len(engines) != 1 {
		t.Fatalf("opened %d engines, want 1", len(engines))
	}
	calls, _, _ := engines[0].snapshot()
	want := []fakeCall{
		{"need", &api.NeedInput{Symbol: "src/config.ts:parseConfig", Requirement: "keep returning Config", Note: "n-3f2a"}},
		{"start", &api.StartInput{Intent: "Add strict mode", Paths: []string{"a.go", "b.go"}}},
		{"discuss", &api.DiscussInput{About: "t-8c1f", Message: "I can own it",
			Proposal: &api.ProposalInput{Owner: "me", Signature: "f(x int)", Summary: "I own f"}}},
		{"submit", &api.SubmitInput{Draft: true}},
		{"status", &api.StatusInput{}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls:\n%#v\nwant:\n%#v", calls, want)
	}
}

func TestResults(t *testing.T) {
	reply := func(outcome api.Outcome) *api.Reply[api.WhoData] {
		return &api.Reply[api.WhoData]{
			Result: api.Result{Outcome: outcome, Summary: "who: one agent", Details: []string{"a fact"},
				Options: []model.Option{{Label: "ask them", Tool: "discuss", Args: map[string]any{"about": "n-3f2a"}}}, Open: 2, OpenAct: 1},
			Data: api.WhoData{Target: "src/a.ts", Entries: []api.WhoEntry{{Agent: "codex-ci-1a2b3c", Role: "changes"}}},
		}
	}
	var mu sync.Mutex
	next := func(string, any) (api.Output, error) { return nil, nil }
	h := newHarness(t, harnessOptions{prepare: func(f *fakeEngine) {
		f.reply = func(verb string, in any) (api.Output, error) {
			mu.Lock()
			defer mu.Unlock()
			return next(verb, in)
		}
	}})
	set := func(f func(string, any) (api.Output, error)) {
		mu.Lock()
		next = f
		mu.Unlock()
	}

	for _, outcome := range []api.Outcome{api.OK, api.NothingToDo, api.Refused} {
		r := reply(outcome)
		set(func(string, any) (api.Output, error) { return r, nil })
		res := h.call(t, "who", map[string]any{"target": "src/a.ts"})
		if res.IsError != (outcome == api.Refused) {
			t.Errorf("%s: isError %v", outcome, res.IsError)
		}
		if got, want := text(res), render.Text(&r.Result, render.MCP); got != want {
			t.Errorf("%s: text %q, want render.Text %q", outcome, got, want)
		}
		got, _ := json.Marshal(res.StructuredContent)
		want, _ := json.Marshal(r)
		var shape, wantShape map[string]any
		_ = json.Unmarshal(got, &shape)
		_ = json.Unmarshal(want, &wantShape)
		if !reflect.DeepEqual(shape, wantShape) {
			t.Errorf("%s: structured content\n%s\nwant\n%s", outcome, got, want)
		}
		for _, key := range []string{"outcome", "summary", "open", "openAct", "data"} {
			if _, ok := shape[key]; !ok {
				t.Errorf("%s: structured content has no %q", outcome, key)
			}
		}
	}

	set(func(string, any) (api.Output, error) { return nil, errors.New("ledger: database is locked") })
	res := h.call(t, "who", map[string]any{"target": "src/a.ts"})
	if got := text(res); !res.IsError || !strings.Contains(got, "gitlet failed to run who: ledger: database is locked") || !strings.Contains(got, "gitlet doctor") {
		t.Errorf("engine error: isError %v, %q", res.IsError, got)
	}

	set(func(string, any) (api.Output, error) { return (*api.Reply[api.WhoData])(nil), nil })
	if res := h.call(t, "who", map[string]any{"target": "x"}); !res.IsError || !strings.Contains(text(res), "no result") {
		t.Errorf("nil result: isError %v, %q", res.IsError, text(res))
	}

	set(func(string, any) (api.Output, error) { panic("boom") })
	if res := h.call(t, "who", map[string]any{"target": "x"}); !res.IsError || !strings.Contains(text(res), "internal error in who") {
		t.Errorf("panic: isError %v, %q", res.IsError, text(res))
	}
	// The server still works after a panic.
	set(func(string, any) (api.Output, error) { return reply(api.OK), nil })
	if res := h.call(t, "who", map[string]any{"target": "x"}); res.IsError {
		t.Errorf("after panic: %q", text(res))
	}
}

func TestEnginePerWorktree(t *testing.T) {
	h := newHarness(t, harnessOptions{
		opt:    Options{Dir: "/repo/a/sub", Agent: "override-agent-1"},
		client: "Claude-Code",
		roots: map[string]string{
			"/repo/a/sub": "/repo/a",
			"/repo/a":     "/repo/a",
			"/repo/a/x":   "/repo/a",
			"/repo/b":     "/repo/b",
		},
	})
	h.call(t, "status", nil)
	h.call(t, "status", map[string]any{"worktree": "/repo/b"})
	h.call(t, "status", map[string]any{"worktree": "/repo/a/x"})
	h.call(t, "who", map[string]any{"target": "f", "worktree": "/repo/b/"})

	engines := h.engines()
	if len(engines) != 2 {
		t.Fatalf("opened %d engines, want one per worktree root", len(engines))
	}
	a, b := engines[0], engines[1]
	if a.opt.Dir != "/repo/a" || a.opt.Agent != "override-agent-1" || a.opt.Client != "claude-code" || !a.opt.Session {
		t.Errorf("default engine options %+v", a.opt)
	}
	if b.opt.Dir != "/repo/b" || b.opt.Agent != "" || b.opt.Client != "claude-code" || !b.opt.Session {
		t.Errorf("second engine options %+v", b.opt)
	}
	ac, _, _ := a.snapshot()
	bc, _, _ := b.snapshot()
	if len(ac) != 2 || len(bc) != 2 {
		t.Errorf("calls: /repo/a %d, /repo/b %d; want 2 each", len(ac), len(bc))
	}
	if in := bc[1].in.(*api.WhoInput); in.Worktree != "/repo/b/" {
		t.Errorf("worktree argument not passed through: %+v", in)
	}

	res := h.call(t, "status", map[string]any{"worktree": "/elsewhere"})
	if !res.IsError || !strings.Contains(text(res), "/elsewhere") {
		t.Errorf("unknown worktree: isError %v, %q", res.IsError, text(res))
	}
}

func TestClientName(t *testing.T) {
	cases := map[string]string{
		"":                 "mcp",
		"claude-code":      "claude-code",
		"Claude-Code":      "claude-code",
		"codex-mcp-client": "codex",
		"cursor-vscode":    "cursor",
		"Zed":              "zed",
	}
	for name, want := range cases {
		if got := clientName(&mcp.Implementation{Name: name}); got != want {
			t.Errorf("clientName(%q) = %q, want %q", name, got, want)
		}
	}
	if got := clientName(nil); got != "mcp" {
		t.Errorf("clientName(nil) = %q", got)
	}
	if want := []string{install.ClaudeCode, install.Codex, install.Cursor}; !slices.Equal(knownClients, want) {
		t.Errorf("knownClients %v, want install's client names %v", knownClients, want)
	}
}

// eventually polls cond until it holds or a few seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInitializedOpensDefaultEngine(t *testing.T) {
	h := newHarness(t, harnessOptions{legacy: true, client: "codex-mcp-client"})
	eventually(t, "the default engine to open", func() bool { return len(h.engines()) == 1 })
	if e := h.engines()[0]; e.opt.Client != "codex" || e.opt.Dir != "/repo" {
		t.Errorf("engine options %+v", e.opt)
	}
	h.call(t, "status", nil)
	if n := len(h.engines()); n != 1 {
		t.Errorf("the first call opened another engine: %d", n)
	}
}

func TestTicksAndClose(t *testing.T) {
	var logs syncBuffer
	h := newHarness(t, harnessOptions{
		opt:     Options{TickInterval: 2 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))},
		prepare: func(f *fakeEngine) { f.tickErr = errors.New("remote unreachable") },
	})
	h.call(t, "status", nil)
	e := h.engines()[0]
	eventually(t, "ticks", func() bool { _, ticks, _ := e.snapshot(); return ticks >= 3 })

	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	_, ticks, shutdowns := e.snapshot()
	if shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", shutdowns)
	}
	time.Sleep(10 * time.Millisecond)
	if _, after, _ := e.snapshot(); after != ticks {
		t.Errorf("ticks went on after Close: %d -> %d", ticks, after)
	}
	if err := h.s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, _, shutdowns := e.snapshot(); shutdowns != 1 {
		t.Errorf("second Close shut down again: %d", shutdowns)
	}
	if n := strings.Count(logs.String(), "background work failed\""); n != 1 {
		t.Errorf("a repeated tick error was logged %d times at warn level:\n%s", n, logs.String())
	}
	if res := h.call(t, "status", nil); !res.IsError || !strings.Contains(text(res), "shutting down") {
		t.Errorf("call after Close: isError %v, %q", res.IsError, text(res))
	}
}

func TestRunEndsWhenClientLeaves(t *testing.T) {
	for _, how := range []string{"disconnect", "cancel"} {
		t.Run(how, func(t *testing.T) {
			s := New(Options{Dir: "/repo", TickInterval: -1, Logger: slog.New(slog.DiscardHandler)})
			var f *fakeEngine
			s.findRoot = func(_ context.Context, dir string) (string, error) { return dir, nil }
			s.openEngine = func(_ context.Context, opt engine.Options) (backend, error) {
				f = &fakeEngine{opt: opt}
				return f, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st, ct := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx, st) }()

			cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"}); err != nil {
				t.Fatal(err)
			}
			if how == "cancel" {
				cancel()
			} else {
				cs.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			if _, _, shutdowns := f.snapshot(); shutdowns != 1 {
				t.Errorf("shutdowns = %d, want 1", shutdowns)
			}
			cs.Close()
		})
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

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
