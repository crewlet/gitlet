package install

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const mcpJSON = `{
  "mcpServers": {
    "gitlet": {
      "type": "stdio",
      "command": "gitlet",
      "args": [
        "mcp"
      ]
    }
  }
}
`

const settingsJSON = `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code session-start || true"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code user-prompt-submit || true"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code post-tool-use || true"
          }
        ]
      }
    ]
  }
}
`

const codexTOML = `[mcp_servers.gitlet]
command = "gitlet"
args = ["mcp"]
`

const cursorJSON = `{
  "mcpServers": {
    "gitlet": {
      "command": "gitlet",
      "args": [
        "mcp"
      ]
    }
  }
}
`

func TestInstallFresh(t *testing.T) {
	tests := []struct {
		client string
		files  map[string]string // relative path -> content
		order  []string
	}{
		{ClaudeCode, map[string]string{
			".mcp.json":                      mcpJSON,
			".claude/skills/gitlet/SKILL.md": Skill(),
			".claude/settings.json":          settingsJSON,
		}, []string{".mcp.json", ".claude/skills/gitlet/SKILL.md", ".claude/settings.json"}},
		{Codex, map[string]string{
			".codex/config.toml": codexTOML,
			"AGENTS.md":          agentsSection() + "\n",
		}, []string{".codex/config.toml", "AGENTS.md"}},
		{Cursor, map[string]string{
			".cursor/mcp.json":         cursorJSON,
			".cursor/rules/gitlet.mdc": cursorRule(),
		}, []string{".cursor/mcp.json", ".cursor/rules/gitlet.mdc"}},
	}
	for _, tt := range tests {
		t.Run(tt.client, func(t *testing.T) {
			root := t.TempDir()
			changes, err := Install(context.Background(), Options{Root: root, Clients: []string{tt.client}})
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, rel := range tt.order {
				want = append(want, "create "+rel)
			}
			if got := summarize(root, changes); !reflect.DeepEqual(got, want) {
				t.Fatalf("changes = %q, want %q", got, want)
			}
			for rel, content := range tt.files {
				if got := read(t, filepath.Join(root, rel)); got != content {
					t.Errorf("%s =\n%s\nwant:\n%s", rel, got, content)
				}
				if m := mode(t, filepath.Join(root, rel)); m != 0o644 {
					t.Errorf("%s mode = %v, want 0644", rel, m)
				}
			}
			if n := countFiles(t, root); n != len(tt.files) {
				t.Errorf("wrote %d files, want %d (no stray temporary files)", n, len(tt.files))
			}
		})
	}
}

func TestInstallBinary(t *testing.T) {
	root := t.TempDir()
	hooks := filepath.Join(root, "hooks")
	bin := "/opt/my tools/gitlet"
	if _, err := Install(context.Background(), Options{Root: root, Clients: []string{ClaudeCode, Codex}, GitHooks: true, HooksDir: hooks, Binary: bin}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ rel, want string }{
		{".mcp.json", `"command": "/opt/my tools/gitlet",`},
		{".claude/settings.json", `"command": "command -v '/opt/my tools/gitlet' >/dev/null 2>&1 && '/opt/my tools/gitlet' hook claude-code post-tool-use || true"`},
		{".codex/config.toml", `command = "/opt/my tools/gitlet"`},
		{"hooks/pre-push", `'/opt/my tools/gitlet' hook git pre-push "$@" || true`},
	} {
		if got := read(t, filepath.Join(root, c.rel)); !strings.Contains(got, c.want) {
			t.Errorf("%s lacks %q:\n%s", c.rel, c.want, got)
		}
	}
}

// TestInstallKeepsBinary: a gitlet binary someone set by hand, for a client
// whose PATH lacks gitlet, survives init and doctor's dry run; only an
// explicit Options.Binary replaces it.
func TestInstallKeepsBinary(t *testing.T) {
	root := t.TempDir()
	hooks := filepath.Join(root, "hooks")
	files := map[string]string{
		".mcp.json":          `{"mcpServers": {"gitlet": {"type": "stdio", "command": "/home/me/go/bin/gitlet", "args": ["mcp"]}}}` + "\n",
		".cursor/mcp.json":   `{"mcpServers": {"gitlet": {"command": "C:\\tools\\gitlet.exe", "args": ["mcp"]}}}` + "\n",
		".codex/config.toml": "[mcp_servers.gitlet]\ncommand = '/home/me/go/bin/gitlet'\nargs = [\"mcp\"]\n",
		".claude/settings.json": `{"hooks": {
  "SessionStart": [{"hooks": [{"type": "command", "command": "\"$HOME/go/bin/gitlet\" hook claude-code session-start"}]}],
  "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "command -v '/opt/my tools/gitlet' >/dev/null 2>&1 && '/opt/my tools/gitlet' hook claude-code user-prompt-submit || true"}]}],
  "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/usr/local/bin/gitlet-wrapper hook claude-code post-tool-use"}]}]
}}` + "\n",
		"hooks/pre-push": hookScript("~/bin/gitlet", "pre-push"),
	}
	for rel, content := range files {
		write(t, filepath.Join(root, rel), content, 0o755)
	}
	opt := Options{Root: root, Clients: []string{ClaudeCode, Codex, Cursor}, GitHooks: true, HooksDir: hooks}
	if _, err := Install(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		if rel == ".claude/settings.json" {
			continue
		}
		if got := read(t, filepath.Join(root, rel)); got != content {
			t.Errorf("%s =\n%s\nwant it unchanged:\n%s", rel, got, content)
		}
	}
	settings := read(t, filepath.Join(root, ".claude", "settings.json"))
	for _, want := range []string{
		`"command": "command -v \"$HOME/go/bin/gitlet\" >/dev/null 2>&1 && \"$HOME/go/bin/gitlet\" hook claude-code session-start || true"`,
		`"command": "command -v '/opt/my tools/gitlet' >/dev/null 2>&1 && '/opt/my tools/gitlet' hook claude-code user-prompt-submit || true"`,
		`"command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code post-tool-use || true"`,
	} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %s:\n%s", want, settings)
		}
	}
	// doctor's dry run finds nothing to do.
	opt.DryRun = true
	changes, err := Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Action != actUnchanged {
			t.Errorf("rerun: %s %s, want unchanged", c.Action, rel(root, c.Path))
		}
	}

	// An explicit binary wins.
	opt.DryRun, opt.Binary = false, "gitlet"
	if _, err := Install(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ rel, unwanted string }{
		{".mcp.json", "/home/me"}, {".cursor/mcp.json", "tools"}, {".codex/config.toml", "/home/me"},
		{".claude/settings.json", "$HOME"}, {".claude/settings.json", "my tools"}, {"hooks/pre-push", "~/bin"},
	} {
		if got := read(t, filepath.Join(root, c.rel)); strings.Contains(got, c.unwanted) {
			t.Errorf("%s still has %q:\n%s", c.rel, c.unwanted, got)
		}
	}
}

func TestInstallDefaultClients(t *testing.T) {
	tests := []struct {
		name  string
		setup []string // paths to create; a trailing slash makes a directory
		want  []string
	}{
		{"nothing detected", nil, []string{".mcp.json", ".claude/skills/gitlet/SKILL.md", ".claude/settings.json"}},
		{"codex and cursor detected", []string{"AGENTS.md", ".cursor/"}, []string{".codex/config.toml", "AGENTS.md", ".cursor/mcp.json", ".cursor/rules/gitlet.mdc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			setup(t, root, tt.setup)
			changes, err := Install(context.Background(), Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range changes {
				got = append(got, rel(root, c.Path))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("files = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstallMerge(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".mcp.json"), `{
  "mcpServers": {
    "other": {"command": "other-server", "args": ["--port", "1"]},
    "gitlet": {"command": "old-gitlet", "env": {"GITLET_AGENT": "me"}}
  },
  "zzz": 1.50,
  "aaa": [true, null, "x<y"]
}`, 0o644)
	write(t, filepath.Join(root, ".claude", "settings.json"), `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PostToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo done"}]}
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/old/path/gitlet hook claude-code session-start", "timeout": 5}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "say bye"}]}]
  },
  "model": "opus"
}`, 0o600)

	changes, err := Install(context.Background(), Options{Root: root, Clients: []string{ClaudeCode}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"update .mcp.json", "create .claude/skills/gitlet/SKILL.md", "update .claude/settings.json"}
	if got := summarize(root, changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	if got, want := read(t, filepath.Join(root, ".mcp.json")), `{
  "mcpServers": {
    "other": {
      "command": "other-server",
      "args": [
        "--port",
        "1"
      ]
    },
    "gitlet": {
      "command": "gitlet",
      "env": {
        "GITLET_AGENT": "me"
      },
      "type": "stdio",
      "args": [
        "mcp"
      ]
    }
  },
  "zzz": 1.50,
  "aaa": [
    true,
    null,
    "x<y"
  ]
}
`; got != want {
		t.Errorf(".mcp.json =\n%s\nwant:\n%s", got, want)
	}
	settings := filepath.Join(root, ".claude", "settings.json")
	if got, want := read(t, settings), `{
  "permissions": {
    "allow": [
      "Bash(go test:*)"
    ]
  },
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "echo done"
          }
        ]
      },
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code post-tool-use || true"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "command -v /old/path/gitlet >/dev/null 2>&1 && /old/path/gitlet hook claude-code session-start || true",
            "timeout": 5
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say bye"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "command -v gitlet >/dev/null 2>&1 && gitlet hook claude-code user-prompt-submit || true"
          }
        ]
      }
    ]
  },
  "model": "opus"
}
`; got != want {
		t.Errorf("settings.json =\n%s\nwant:\n%s", got, want)
	}
	if m := mode(t, settings); m != 0o600 {
		t.Errorf("settings.json mode = %v, want 0600 kept", m)
	}

	// A rerun finds everything in place.
	changes, err = Install(context.Background(), Options{Root: root, Clients: []string{ClaudeCode}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Action != actUnchanged {
			t.Errorf("rerun: %s %s, want unchanged", c.Action, rel(root, c.Path))
		}
	}
}

func TestInstallIdempotent(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "AGENTS.md"), "# Project\n\nBe nice.\n", 0o644)
	write(t, filepath.Join(root, ".codex", "config.toml"), "model = \"o3\"\n", 0o644)
	opt := Options{Root: root, Clients: []string{Cursor, ClaudeCode, Codex, Codex}, GitHooks: true, HooksDir: filepath.Join(root, ".git", "hooks")}
	first, err := Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 10 {
		t.Fatalf("first run changed %d files, want 10: %q", len(first), summarize(root, first))
	}
	before := snapshot(t, root)
	second, err := Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("second run reports %d files, want %d", len(second), len(first))
	}
	for i, c := range second {
		if c.Action != actUnchanged || c.Path != first[i].Path {
			t.Errorf("second run: %s %s, want unchanged %s", c.Action, rel(root, c.Path), rel(root, first[i].Path))
		}
	}
	if after := snapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Errorf("second run changed files")
	}
}

func TestInstallDryRun(t *testing.T) {
	root := t.TempDir()
	original := `{"mcpServers": {"other": {"command": "x"}}}`
	write(t, filepath.Join(root, ".mcp.json"), original, 0o644)
	opt := Options{Root: root, Clients: []string{ClaudeCode, Codex, Cursor}, GitHooks: true, HooksDir: filepath.Join(root, "hooks"), DryRun: true}
	changes, err := Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"update .mcp.json", "create .claude/skills/gitlet/SKILL.md", "create .claude/settings.json",
		"create .codex/config.toml", "create AGENTS.md",
		"create .cursor/mcp.json", "create .cursor/rules/gitlet.mdc",
		"create hooks/prepare-commit-msg", "create hooks/post-commit", "create hooks/pre-push",
	}
	if got := summarize(root, changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	if got := read(t, filepath.Join(root, ".mcp.json")); got != original {
		t.Errorf("dry run changed .mcp.json:\n%s", got)
	}
	if n := countFiles(t, root); n != 1 {
		t.Errorf("dry run left %d files, want only .mcp.json", n)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Errorf("dry run created directories: %v", entries)
	}
}

func TestGitHooks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".git", "hooks")
	foreign := "#!/bin/sh\nexec ./scripts/check-push\n"
	write(t, filepath.Join(dir, "pre-push"), foreign, 0o755)
	byHand := "#!/bin/sh\nmake lint\ngitlet hook git post-commit || true\n"
	write(t, filepath.Join(dir, "post-commit"), byHand, 0o755)
	write(t, filepath.Join(dir, "prepare-commit-msg"), "#!/bin/sh\n"+hookMarker+" an older version\n/old/gitlet hook git prepare-commit-msg \"$@\"\n", 0o755)

	opt := Options{Root: root, Clients: []string{Cursor}, GitHooks: true, HooksDir: ".git/hooks"}
	changes, err := Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	got := summarize(root, changes)[2:]
	want := []string{"update .git/hooks/prepare-commit-msg", "unchanged .git/hooks/post-commit", "skip .git/hooks/pre-push"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hook changes = %q, want %q", got, want)
	}
	if s, want := changes[4].Summary, `command -v gitlet >/dev/null 2>&1 && gitlet hook git pre-push "$@" || true`; s != want {
		t.Errorf("skip summary = %q, want %q", s, want)
	}
	if read(t, filepath.Join(dir, "pre-push")) != foreign || read(t, filepath.Join(dir, "post-commit")) != byHand {
		t.Error("a hook gitlet does not own was changed")
	}
	if got, want := read(t, filepath.Join(dir, "prepare-commit-msg")), hookScript("/old/gitlet", "prepare-commit-msg"); got != want {
		t.Errorf("prepare-commit-msg =\n%s\nwant:\n%s", got, want)
	}

	// gitlet's own hook that lost its executable bit is fixed.
	hook := filepath.Join(dir, "prepare-commit-msg")
	if err := os.Chmod(hook, 0o644); err != nil {
		t.Fatal(err)
	}
	changes, err = Install(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if c := changes[2]; c.Action != actUpdate {
		t.Errorf("non-executable hook: %s, want update", c.Action)
	}
	if m := mode(t, hook); m != 0o755 {
		t.Errorf("hook mode = %v, want 0755", m)
	}
}

func TestGitHooksFresh(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "custom-hooks")
	changes, err := Install(context.Background(), Options{Root: root, Clients: []string{Codex}, GitHooks: true, HooksDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range gitHooks {
		path := filepath.Join(dir, name)
		got := read(t, path)
		if got != hookScript("gitlet", name) {
			t.Errorf("%s =\n%s", name, got)
		}
		if !isGitletHook([]byte(got)) || !callsGitHook([]byte(got), name) {
			t.Errorf("%s is not recognized as gitlet's own hook", name)
		}
		if m := mode(t, path); m != 0o755 {
			t.Errorf("%s mode = %v, want 0755", name, m)
		}
	}
	if len(changes) != 5 {
		t.Errorf("changes = %q", summarize(root, changes))
	}
}

// TestHookScriptNeverFails runs the installed hooks with sh: a missing
// binary and a failing binary both exit 0, and arguments reach gitlet.
func TestHookScriptNeverFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	failing := filepath.Join(dir, "fake gitlet")
	write(t, failing, "#!/bin/sh\necho \"args: $*\" >&2\nexit 3\n", 0o755)
	tests := []struct {
		name, bin, stderr string
	}{
		{"missing binary", filepath.Join(dir, "missing"), ""},
		{"failing binary", failing, "args: hook git pre-push origin https://example.com/r.git\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := filepath.Join(t.TempDir(), "pre-push")
			write(t, hook, hookScript(shellQuote(tt.bin), "pre-push"), 0o755)
			cmd := exec.Command(sh, hook, "origin", "https://example.com/r.git")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("hook failed: %v (%s)", err, stderr.String())
			}
			if stderr.String() != tt.stderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.stderr)
			}
		})
	}
}

// TestClaudeHooksNeverFail runs the Claude Code hook commands init writes
// with sh, as Claude Code does: without gitlet on PATH, and with a failing
// binary, each exits 0, and arguments reach gitlet. The settings file is
// shared with teammates who may not have gitlet installed.
func TestClaudeHooksNeverFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	failing := filepath.Join(dir, "fake gitlet")
	write(t, failing, "#!/bin/sh\necho \"args: $*\" >&2\nexit 3\n", 0o755)
	tests := []struct {
		name, bin, stderr string
	}{
		{"not on PATH", "", ""},
		{"missing binary", filepath.Join(dir, "missing"), ""},
		{"failing binary", failing, "args: hook claude-code "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := Install(context.Background(), Options{Root: root, Clients: []string{ClaudeCode}, Binary: tt.bin}); err != nil {
				t.Fatal(err)
			}
			v, err := decodeJSON([]byte(read(t, filepath.Join(root, ".claude", "settings.json"))))
			if err != nil {
				t.Fatal(err)
			}
			hooks := v.(*object).vals["hooks"].(*object)
			for _, h := range claudeHooks {
				group := hooks.vals[h.event].([]any)[0].(*object)
				command := group.vals["hooks"].([]any)[0].(*object).vals["command"].(string)
				cmd := exec.Command(sh, "-c", command)
				cmd.Env = []string{"PATH=" + t.TempDir()}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("%s: %s: %v (%s)", h.event, command, err, stderr.String())
				}
				if want := tt.stderr; want != "" && stderr.String() != want+h.arg+"\n" || want == "" && stdout.Len()+stderr.Len() > 0 {
					t.Errorf("%s: stdout %q, stderr %q", h.event, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestInstallRefusesBadFiles(t *testing.T) {
	tests := []struct {
		name, rel, content, want string
	}{
		{"invalid JSON", ".mcp.json", `{"mcpServers": {`, "is not valid JSON"},
		{"trailing data", ".mcp.json", `{} {}`, "is not valid JSON"},
		{"not an object", ".mcp.json", `[1, 2]`, "does not hold a JSON object"},
		{"servers not an object", ".mcp.json", `{"mcpServers": []}`, "mcpServers is not a JSON object"},
		{"gitlet entry not an object", ".mcp.json", `{"mcpServers": {"gitlet": "x"}}`, "mcpServers.gitlet is not a JSON object"},
		{"hooks not an object", ".claude/settings.json", `{"hooks": []}`, "hooks is not a JSON object"},
		{"event not an array", ".claude/settings.json", `{"hooks": {"SessionStart": {}}}`, "hooks.SessionStart is not a JSON array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tt.rel)
			write(t, path, tt.content, 0o644)
			_, err := Install(context.Background(), Options{Root: root, Clients: []string{ClaudeCode}})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) || !strings.HasPrefix(err.Error(), "install: ") {
				t.Fatalf("err = %v, want one naming %s with %q", err, path, tt.want)
			}
			if read(t, path) != tt.content {
				t.Error("the file was overwritten")
			}
			if n := countFiles(t, root); n != 1 {
				t.Errorf("%d files after a refused install, want 1: nothing may be written", n)
			}
		})
	}
}

func TestInstallErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	write(t, file, "x", 0o644)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		opt  Options
		want string
	}{
		{"no root", context.Background(), Options{}, "no root"},
		{"root is a file", context.Background(), Options{Root: file}, "not a directory"},
		{"missing root", context.Background(), Options{Root: filepath.Join(root, "missing")}, "no such file"},
		{"unknown client", context.Background(), Options{Root: root, Clients: []string{"vim"}}, `unknown client "vim"`},
		{"hooks without dir", context.Background(), Options{Root: root, GitHooks: true}, "hooks directory"},
		{"canceled", canceled, Options{Root: root}, "context canceled"},
		{"directory in the way", context.Background(), Options{Root: root, Clients: []string{Cursor}}, "not a regular file"},
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor", "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Install(tt.ctx, tt.opt)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "install: ") {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAgentsSection(t *testing.T) {
	section := agentsSection()
	tests := []struct {
		name, old, want, err string
	}{
		{"empty", "", section + "\n", ""},
		{"appended after a blank line", "# Project\n\nBe nice.\n", "# Project\n\nBe nice.\n\n" + section + "\n", ""},
		{"no final newline", "# Project\nBe nice.", "# Project\nBe nice.\n\n" + section + "\n", ""},
		{"blank line already there", "# Project\n\n", "# Project\n\n" + section + "\n", ""},
		{"replaced between markers",
			"# Project\n\n<!-- gitlet:start -->\nold guidance\n<!-- gitlet:end -->\n\n## Other\nkeep me\n",
			"# Project\n\n" + section + "\n\n## Other\nkeep me\n", ""},
		{"crlf", "# Project\r\n", "# Project\r\n\r\n" + strings.ReplaceAll(section, "\n", "\r\n") + "\r\n", ""},
		{"start without end", "<!-- gitlet:start -->\nold\n", "", "without " + markerEnd},
		{"end without start", "old\n<!-- gitlet:end -->\n", "", "without " + markerStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceSection("AGENTS.md", []byte(tt.old), section)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.Contains(err.Error(), "AGENTS.md") {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
			again, err := replaceSection("AGENTS.md", got, section)
			if err != nil || !bytes.Equal(again, got) {
				t.Errorf("second pass changed the file: %v\n%q", err, again)
			}
		})
	}
}

func TestSymlinkedAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "CLAUDE.md"), "# Notes\n", 0o644)
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), Options{Root: root, Clients: []string{Codex}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "AGENTS.md")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("AGENTS.md is no longer a symlink: %v", err)
	}
	if got := read(t, filepath.Join(root, "CLAUDE.md")); got != "# Notes\n\n"+agentsSection()+"\n" {
		t.Errorf("CLAUDE.md =\n%s", got)
	}
}

func TestCodexConfig(t *testing.T) {
	const table = "[mcp_servers.gitlet]\ncommand = \"gitlet\"\nargs = [\"mcp\"]\n"
	tests := []struct {
		name, old, bin, want, skip string
	}{
		{"empty", "", "gitlet", table, ""},
		{"appended after other settings", "model = \"o3\"", "gitlet", "model = \"o3\"\n\n" + table, ""},
		{"other servers kept", "[mcp_servers.other]\ncommand = \"x\"\n\n", "gitlet",
			"[mcp_servers.other]\ncommand = \"x\"\n\n" + table, ""},
		{"replaced in place", `model = "o3"

[mcp_servers.gitlet]
command = "/old/gitlet"   # old
args = [
  "mcp",
  "--verbose", # [not.a.header]
]
startup_timeout_sec = 20

[mcp_servers.gitlet.env]
GITLET_AGENT = "me"
`, "gitlet", `model = "o3"

[mcp_servers.gitlet]
command = "gitlet"
args = ["mcp"]
startup_timeout_sec = 20

[mcp_servers.gitlet.env]
GITLET_AGENT = "me"
`, ""},
		{"already right, comment kept", "[mcp_servers.gitlet]\ncommand = \"gitlet\" # mine\nargs = [\"mcp\"]\n", "gitlet",
			"[mcp_servers.gitlet]\ncommand = \"gitlet\" # mine\nargs = [\"mcp\"]\n", ""},
		{"quoted header, missing key added", "[ mcp_servers . \"gitlet\" ]\n  command = \"gitlet\"\n\n[other]\nx = 1\n", "gitlet",
			"[ mcp_servers . \"gitlet\" ]\n  command = \"gitlet\"\nargs = [\"mcp\"]\n\n[other]\nx = 1\n", ""},
		{"header inside a multi-line string ignored", "notes = \"\"\"\n[mcp_servers.gitlet]\ncommand = \"fake\"\n\"\"\"\n", "gitlet",
			"notes = \"\"\"\n[mcp_servers.gitlet]\ncommand = \"fake\"\n\"\"\"\n\n" + table, ""},
		{"header inside a multi-line array ignored", "list = [\n[\"mcp_servers.gitlet\"],\n]\n", "gitlet",
			"list = [\n[\"mcp_servers.gitlet\"],\n]\n\n" + table, ""},
		{"crlf", "model = \"o3\"\r\n", "gitlet", "model = \"o3\"\r\n\r\n" + strings.ReplaceAll(table, "\n", "\r\n"), ""},
		{"escaped binary", "", `C:\tools\git"let`, "[mcp_servers.gitlet]\ncommand = \"C:\\\\tools\\\\git\\\"let\"\nargs = [\"mcp\"]\n", ""},
		{"inline table in [mcp_servers]", "[mcp_servers]\ngitlet = { command = \"gitlet\" }\n", "gitlet", "", "inline table"},
		{"dotted keys at the root", "mcp_servers.gitlet.command = \"gitlet\"\n", "gitlet", "", "dotted keys"},
		{"inline mcp_servers at the root", "mcp_servers = { other = { command = \"x\" } }\n", "gitlet", "", "inline table"},
		{"array of tables", "[[mcp_servers.gitlet]]\ncommand = \"x\"\n", "gitlet", "", "array of tables"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skip := editCodexConfig([]byte(tt.old), binary{name: tt.bin})
			if tt.skip != "" {
				if !strings.Contains(skip, tt.skip) {
					t.Fatalf("skip = %q, want %q", skip, tt.skip)
				}
				return
			}
			if skip != "" {
				t.Fatalf("unexpected skip: %s", skip)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if again, _ := editCodexConfig(got, binary{name: tt.bin}); !bytes.Equal(again, got) {
				t.Errorf("second pass changed the file:\n%s", again)
			}
		})
	}
}

// TestCodexConfigKeepsBinary: without an explicit binary, a command that
// names a gitlet binary stays as written; any other command is replaced.
func TestCodexConfigKeepsBinary(t *testing.T) {
	bin := binary{name: "gitlet", keep: true}
	for _, tt := range []struct{ name, command, want string }{
		{"basic string", `"/home/me/go/bin/gitlet"   # mine`, `"/home/me/go/bin/gitlet"   # mine`},
		{"literal string", `'C:\tools\gitlet.exe'`, `'C:\tools\gitlet.exe'`},
		{"escapes", `"C:\\tools\\gitlet.exe"`, `"C:\\tools\\gitlet.exe"`},
		{"not gitlet", `"/usr/bin/gitlet-wrapper"`, `"gitlet"`},
		{"not a string", `["gitlet"]`, `"gitlet"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := "[mcp_servers.gitlet]\ncommand = " + tt.command + "\nargs = [\"mcp\"]\n"
			got, skip := editCodexConfig([]byte(old), bin)
			if want := "[mcp_servers.gitlet]\ncommand = " + tt.want + "\nargs = [\"mcp\"]\n"; skip != "" || string(got) != want {
				t.Errorf("got (skip %q):\n%s\nwant:\n%s", skip, got, want)
			}
		})
	}
}

func TestCodexConfigSkip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".codex", "config.toml")
	old := "[mcp_servers]\ngitlet = { command = \"other\" }\n"
	write(t, path, old, 0o644)
	changes, err := Install(context.Background(), Options{Root: root, Clients: []string{Codex}})
	if err != nil {
		t.Fatal(err)
	}
	if c := changes[0]; c.Action != actSkip || c.Summary != `mcp_servers.gitlet = { command = "gitlet", args = ["mcp"] }` {
		t.Errorf("change = %+v", c)
	}
	if read(t, path) != old {
		t.Error("config.toml was changed")
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
		want  []string
	}{
		{"nothing", nil, nil},
		{".claude dir", []string{".claude/"}, []string{ClaudeCode}},
		{".claude file is not a client dir", []string{".claude"}, nil},
		{".mcp.json", []string{".mcp.json"}, []string{ClaudeCode}},
		{".codex dir", []string{".codex/"}, []string{Codex}},
		{"AGENTS.md", []string{"AGENTS.md"}, []string{Codex}},
		{".cursor dir", []string{".cursor/"}, []string{Cursor}},
		{"all, sorted", []string{".cursor/", "AGENTS.md", ".mcp.json", ".claude/"}, []string{ClaudeCode, Codex, Cursor}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			setup(t, root, tt.setup)
			if got := Detect(root); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- helpers ----

func write(t *testing.T, path, content string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // umask
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func setup(t *testing.T, root string, paths []string) {
	t.Helper()
	for _, p := range paths {
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(t, filepath.Join(root, p), "{}", 0o644)
	}
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// summarize lists changes as "action path", paths relative to root.
func summarize(root string, changes []Change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Action+" "+rel(root, c.Path))
	}
	return out
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	return len(snapshot(t, root))
}

// snapshot maps every file under root to its mode and content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		files[rel(root, path)] = info.Mode().String() + "\n" + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
