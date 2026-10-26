package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/gitlet/internal/api"
)

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
	src := "package config\n\n// Config is the loaded configuration.\ntype Config struct{ Strict bool }\n\n" +
		"// ParseConfig parses s.\nfunc ParseConfig(s string) (Config, error) {\n\treturn Config{}, nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "config.go"},
		{"-c", "user.name=gitlet test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "Add config parsing"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// TestIntegrationEveryTool calls every tool against a real engine on a
// temporary repository without a remote. Every call must produce a result
// envelope (refusals included) whose text starts with its outcome.
func TestIntegrationEveryTool(t *testing.T) {
	dir := tempRepo(t)
	s := New(Options{Dir: dir, Version: "test", TickInterval: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cs.Close()
		ss.Wait()
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	calls := []struct {
		tool string
		args map[string]any
	}{
		{"status", nil},
		{"start", map[string]any{"intent": "Add strict mode to config loading", "paths": []string{"config.go"}}},
		{"who", map[string]any{"target": "config.go"}},
		{"why", map[string]any{"target": "config.go:7"}},
		{"claim", map[string]any{"symbol": "config.go:ParseConfig"}},
		{"publish", map[string]any{"symbol": "config.go:ParseConfig"}},
		{"need", map[string]any{"symbol": "config.go:ParseConfig", "requirement": "keep returning Config"}},
		{"use", map[string]any{"symbol": "config.go:ParseConfig"}},
		{"release", map[string]any{"symbol": "config.go:ParseConfig"}},
		{"discuss", map[string]any{"about": "n-000000000000", "message": "Do you need the strict flag?"}},
		{"accept", map[string]any{"proposal": "p-000000000000"}},
		{"dismiss", map[string]any{"note": "n-000000000000", "as": "not_needed"}},
		{"update", nil},
		{"submit", map[string]any{"draft": true}},
		{"abandon", map[string]any{"reason": "integration test done"}},
		{"status", map[string]any{"worktree": dir}},
	}
	called := map[string]bool{}
	for i, c := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Fatalf("%s: protocol error: %v", c.tool, err)
		}
		if i == 0 {
		}
		called[c.tool] = true
		var r struct {
			api.Result
			Data json.RawMessage `json:"data"`
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, &r); err != nil || res.StructuredContent == nil {
			t.Errorf("%s: no result envelope (isError %v): %s", c.tool, res.IsError, text(res))
			continue
		}
		switch r.Outcome {
		case api.OK, api.NothingToDo, api.Refused:
		default:
			t.Errorf("%s: outcome %q", c.tool, r.Outcome)
		}
		if res.IsError != (r.Outcome == api.Refused) {
			t.Errorf("%s: isError %v for outcome %s", c.tool, res.IsError, r.Outcome)
		}
		if r.Outcome == api.Refused && len(r.Options) == 0 {
			t.Errorf("%s: refusal without options: %s", c.tool, text(res))
		}
		if got := text(res); !strings.HasPrefix(got, string(r.Outcome)+": ") {
			t.Errorf("%s: text does not start with the outcome:\n%s", c.tool, got)
		}
		if c.tool == "status" && i == 0 {
			var data api.StatusData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				t.Errorf("status data: %v", err)
			} else if data.Agent.Client != "claude-code" || !strings.HasPrefix(string(data.Agent.ID), "claude-code-") {
				t.Errorf("agent %+v does not carry the client name", data.Agent)
			}
		}
	}
	for _, v := range api.Verbs {
		if !called[v.Name] {
			t.Errorf("verb %s was not called", v.Name)
		}
	}
}
