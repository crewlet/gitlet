package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    event
		wantErr bool
	}{
		{
			name: "post tool use with unknown fields",
			in: `{"session_id":"abc123","transcript_path":"/tmp/t.jsonl","cwd":"/work/repo","permission_mode":"default",
				"hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"/work/repo/a.go","old_string":"x"},
				"tool_response":{"success":true,"lines":[1,2,3]},"future_field":null}`,
			want: event{SessionID: "abc123", Cwd: "/work/repo", HookEventName: "PostToolUse", ToolName: "Edit"},
		},
		{
			name: "session start",
			in:   `{"session_id":"s","cwd":"/w","hook_event_name":"SessionStart","source":"startup"}`,
			want: event{SessionID: "s", Cwd: "/w", HookEventName: "SessionStart"},
		},
		{
			name: "user prompt",
			in:   "{\"cwd\":\"/w\",\"hook_event_name\":\"UserPromptSubmit\",\"prompt\":\"fix <the> bug\"}\n",
			want: event{Cwd: "/w", HookEventName: "UserPromptSubmit"},
		},
		{name: "empty", in: ""},
		{name: "whitespace", in: " \n\t"},
		{name: "null", in: "null"},
		{name: "empty object", in: "{}"},
		{name: "malformed", in: `{"cwd":`, wantErr: true},
		{name: "not an object", in: `["cwd"]`, wantErr: true},
		{name: "wrong field type", in: `{"cwd":42}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvent([]byte(tt.in))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseEvent() = %+v, %v; want %+v, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestEncodeOutput(t *testing.T) {
	for sub, name := range clientEvents {
		t.Run(sub, func(t *testing.T) {
			text := "new gitlet notes:\n[act] n-3f2a9c1d0e4b codex-ci-1a2b3c changes <parseConfig> & \"more\""
			got, err := encodeOutput(name, text)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"hookSpecificOutput":{"hookEventName":"` + name + `","additionalContext":` +
				`"new gitlet notes:\n[act] n-3f2a9c1d0e4b codex-ci-1a2b3c changes <parseConfig> & \"more\""}}` + "\n"
			if string(got) != want {
				t.Fatalf("encodeOutput() =\n%s\nwant\n%s", got, want)
			}
			var back map[string]map[string]string
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatal(err)
			}
			if o := back["hookSpecificOutput"]; o["hookEventName"] != name || o["additionalContext"] != text || len(o) != 2 || len(back) != 1 {
				t.Fatalf("round trip = %v", back)
			}
		})
	}
	if clientEvents["session-start"] != "SessionStart" || clientEvents["user-prompt-submit"] != "UserPromptSubmit" ||
		clientEvents["post-tool-use"] != "PostToolUse" || len(clientEvents) != 3 {
		t.Fatalf("clientEvents = %v", clientEvents)
	}
}

// recordingRunner returns a runner whose repository lookups are recorded
// and answered with gitx.ErrNotRepo.
func recordingRunner(stdin string) (r *runner, dirs *[]string, stdout, stderr *bytes.Buffer) {
	stdout, stderr = new(bytes.Buffer), new(bytes.Buffer)
	r = newRunner(strings.NewReader(stdin), stdout, stderr)
	dirs = new([]string)
	r.openRepo = func(_ context.Context, dir string) (*gitx.Repo, error) {
		*dirs = append(*dirs, dir)
		return nil, gitx.ErrNotRepo
	}
	r.getwd = func() (string, error) { return "/hook/cwd", nil }
	r.getenv = func(string) string { return "" }
	return r, dirs, stdout, stderr
}

func TestClientHookSkipsGitletTools(t *testing.T) {
	tests := []struct {
		sub, tool string
		opens     bool
	}{
		{"post-tool-use", "mcp__gitlet__status", false},
		{"post-tool-use", "mcp__gitlet__start", false},
		{"post-tool-use", "Bash", true},
		{"post-tool-use", "mcp__other__gitlet", true},
		{"post-tool-use", "", true},
		{"user-prompt-submit", "mcp__gitlet__status", true}, // only post-tool-use looks at the tool
	}
	for _, tt := range tests {
		t.Run(tt.sub+"/"+tt.tool, func(t *testing.T) {
			r, dirs, stdout, stderr := recordingRunner(`{"cwd":"/work/repo","tool_name":"` + tt.tool + `"}`)
			if code := r.run(context.Background(), []string{"claude-code", tt.sub}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if opened := len(*dirs) > 0; opened != tt.opens {
				t.Fatalf("opened repository: %v, want %v", opened, tt.opens)
			}
			if tt.opens && (*dirs)[0] != "/work/repo" {
				t.Fatalf("opened %q, want the event's cwd", (*dirs)[0])
			}
			if stdout.Len()+stderr.Len() > 0 {
				t.Fatalf("stdout %q, stderr %q", stdout, stderr)
			}
		})
	}
}

func TestClientHookFallsBackToWorkingDir(t *testing.T) {
	tests := []struct {
		name, stdin string
		quiet       bool
	}{
		{"empty input", "", true},
		{"no cwd", `{"session_id":"s"}`, true},
		{"malformed input", `{"cwd":`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, dirs, stdout, stderr := recordingRunner(tt.stdin)
			if code := r.run(context.Background(), []string{"claude-code", "user-prompt-submit"}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(*dirs) != 1 || (*dirs)[0] != "/hook/cwd" {
				t.Fatalf("opened %q, want the working directory", *dirs)
			}
			if stdout.Len() > 0 || (stderr.Len() == 0) != tt.quiet {
				t.Fatalf("stdout %q, stderr %q", stdout, stderr)
			}
		})
	}
}

func TestNotesText(t *testing.T) {
	resolved := func(titles ...string) []model.Note {
		var ns []model.Note
		for _, title := range titles {
			ns = append(ns, model.Note{ID: "n-000000000000", Title: title})
		}
		return ns
	}
	tests := []struct {
		name     string
		resolved []model.Note
		want     string
	}{
		{"nothing", nil, ""},
		{"one", resolved("codex-ci-1a2b3c no longer changes parseConfig"), "resolved gitlet note: codex-ci-1a2b3c no longer changes parseConfig"},
		{"title on one line", resolved("two\nlines"), "resolved gitlet note: two lines"},
		{"capped", resolved("a", "b", "c", "d", "e"), "resolved gitlet note: a\nresolved gitlet note: b\nresolved gitlet note: c\nand 2 more resolved gitlet notes."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notesText(nil, tt.resolved, render.MCP); got != tt.want {
				t.Fatalf("notesText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNotesTextUsesRender(t *testing.T) {
	n := model.Note{ID: "n-3f2a9c1d0e4b", Kind: model.NoteCalleeChanged, Severity: model.SeverityAct, Title: "codex-ci-1a2b3c is changing src/config.ts:parseConfig"}
	want := strings.TrimRight(render.Notes(notesHeading, []model.Note{n}, render.MCP), "\n")
	got := notesText([]model.Note{n}, nil, render.MCP)
	if got != want {
		t.Fatalf("notesText() = %q, want render.Notes output %q", got, want)
	}
}

func TestSessionStatus(t *testing.T) {
	item := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: "Add strict mode to config loading", Branch: "gitlet/add-strict-mode-8c1f", Status: model.ItemActive}
	provisional := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: "fix parser", Branch: "fix-parser", Status: model.ItemActive, Provisional: true}
	detached := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: "untitled", Status: model.ItemActive, Provisional: true}
	noBranch := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: "Fix the parser", Status: model.ItemActive}
	long := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: strings.Repeat("word ", 40), Branch: "b", Status: model.ItemActive}
	tests := []struct {
		name      string
		open, act int
		item      *model.WorkItem
		branch    string
		known     bool
		want      string
	}{
		{"no notes, no task", 0, 0, nil, "main", true,
			"gitlet: no open notes.\nNo task started in this worktree: call start with a one-sentence intent before you edit code."},
		{"notes, no task", 2, 1, nil, "main", true,
			"gitlet: 2 open notes (1 act).\nNo task started in this worktree: call status to see the notes, and start with a one-sentence intent before you edit code."},
		{"one note, task", 1, 0, item, item.Branch, true,
			"gitlet: 1 open note (0 act).\nYour task: \"Add strict mode to config loading\" on branch gitlet/add-strict-mode-8c1f. Call status to see it, what overlaps it and every open note."},
		{"task on another branch", 0, 0, item, "main", true,
			"gitlet: no open notes.\nYour task \"Add strict mode to config loading\" is on branch gitlet/add-strict-mode-8c1f, not the one checked out: switch back to it, or call status to see it and its notes."},
		{"task, detached HEAD", 0, 0, item, "", true,
			"gitlet: no open notes.\nYour task \"Add strict mode to config loading\" is on branch gitlet/add-strict-mode-8c1f, not the one checked out: switch back to it, or call status to see it and its notes."},
		{"task without a branch", 0, 0, noBranch, "", true,
			"gitlet: no open notes.\nYour task: \"Fix the parser\". Call status to see it, what overlaps it and every open note."},
		{"provisional task", 0, 0, provisional, "fix-parser", true,
			"gitlet: no open notes.\nYour work on branch fix-parser has no stated task: call start with a one-sentence intent."},
		{"provisional task without a branch", 0, 0, detached, "", true,
			"gitlet: no open notes.\nYour work has no stated task: call start with a one-sentence intent."},
		{"state unreadable", 3, 3, nil, "", false,
			"gitlet: 3 open notes (3 act).\nCall status to see your task and notes, or start with a one-sentence intent before you edit code."},
		{"long intent clipped", 0, 0, long, "b", true,
			"gitlet: no open notes.\nYour task: \"" + strings.Repeat("word ", 40)[:97] + "...\" on branch b. Call status to see it, what overlaps it and every open note."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionStatus(tt.open, tt.act, tt.item, tt.branch, tt.known)
			if got != tt.want {
				t.Fatalf("sessionStatus() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
