package hook

// Integration tests run the hooks against a real ledger and the real
// renderer, and those that read git state against a real repository too.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// requireGit skips the test when git is not installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

const (
	testAgent  model.AgentID = "claude-code-test-3fa9c1"
	testItem   model.ItemID  = "8c1f0e2a9b3d"
	testBranch               = "gitlet/add-strict-mode-8c1f"
	testIntent               = "Add strict mode to config loading"
)

func testNote(kind model.NoteKind, sev model.Severity, subject, title string) model.Note {
	id := model.NoteID(kind, subject, testItem)
	return model.Note{
		ID:          id,
		Version:     model.NoteVersion(string(sev), subject),
		Kind:        kind,
		Severity:    sev,
		Item:        testItem,
		Subject:     subject,
		SubjectName: subject,
		Parties:     []model.Party{{Agent: "codex-ci-1a2b3c", Item: "1a2b3c4d5e6f", Intent: "Make config parsing strict"}},
		Title:       title,
		Options: []model.Option{
			{Label: "wait for their published signature", Tool: "need", Args: map[string]any{"symbol": subject, "note": id}},
			{Label: "not needed", Tool: "dismiss", Args: map[string]any{"note": id, "as": "not_needed"}},
		},
		CreatedAt: time.Now(),
	}
}

// actNote and infoNote are the notes every fixture starts with.
var (
	actNote  = testNote(model.NoteCalleeChanged, model.SeverityAct, "src/config.ts:parseConfig", "codex-ci-1a2b3c is changing src/config.ts:parseConfig, which your code calls")
	infoNote = testNote(model.NoteFileOverlap, model.SeverityInfo, "docs/README.md", "codex-ci-1a2b3c also changes docs/README.md")
)

// manyNotes returns n new notes of severity sev.
func manyNotes(n int, sev model.Severity) []model.Note {
	var notes []model.Note
	for i := range n {
		subject := fmt.Sprintf("src/m%d.ts:f%d", i, i)
		notes = append(notes, testNote(model.NoteCallersAffected, sev, subject, "codex-ci-1a2b3c calls "+subject))
	}
	return notes
}

// withLedger runs fn on the ledger in common, opened as the engine would.
func withLedger(t *testing.T, common string, fn func(ctx context.Context, l *ledger.Ledger) error) {
	t.Helper()
	ctx := context.Background()
	l, err := ledger.Open(ctx, filepath.Join(common, "gitlet", "ledger.db"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := fn(ctx, l); err != nil {
		t.Fatal(err)
	}
}

// seedLedger registers the test agent for root and syncs notes for it.
func seedLedger(t *testing.T, common, root string, notes ...model.Note) {
	t.Helper()
	withLedger(t, common, func(ctx context.Context, l *ledger.Ledger) error {
		if err := l.SetAgentFor(ctx, root, testAgent); err != nil {
			return err
		}
		return l.SyncNotes(ctx, testAgent, notes, time.Now())
	})
}

// syncNotes replaces the notes the rules produce for the test agent.
func syncNotes(t *testing.T, common string, notes ...model.Note) {
	t.Helper()
	withLedger(t, common, func(ctx context.Context, l *ledger.Ledger) error {
		return l.SyncNotes(ctx, testAgent, notes, time.Now())
	})
}

// statuses returns the test agent's new and open notes by id.
func statuses(t *testing.T, common string) map[string]ledger.NoteStatus {
	t.Helper()
	got := map[string]ledger.NoteStatus{}
	withLedger(t, common, func(ctx context.Context, l *ledger.Ledger) error {
		notes, err := l.OpenNotes(ctx, testAgent)
		for _, n := range notes {
			got[n.Note.ID] = n.Status
		}
		return err
	})
	return got
}

// decodeOutput decodes a Claude Code hook answer, checking its shape; ""
// decodes to the zero answer.
func decodeOutput(t *testing.T, stdout string) specificOutput {
	t.Helper()
	if stdout == "" {
		return specificOutput{}
	}
	var o output
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("output %q: %v", stdout, err)
	}
	if dec.More() || !strings.HasSuffix(stdout, "}\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("output is not one line of JSON: %q", stdout)
	}
	return o.HookSpecificOutput
}

// ---- note delivery: the ledger and render, no git state ----

// pathsFixture is a worktree known only by its paths. Hooks that deliver
// notes run no git command, so it needs no repository.
type pathsFixture struct{ root, common string }

func newPathsFixture(t *testing.T, notes ...model.Note) *pathsFixture {
	t.Helper()
	root := t.TempDir()
	f := &pathsFixture{root: root, common: filepath.Join(root, ".git")}
	if err := os.Mkdir(f.common, 0o755); err != nil {
		t.Fatal(err)
	}
	seedLedger(t, f.common, root, notes...)
	return f
}

// run runs a hook in the fixture, checks it exits 0, and returns its output.
func (f *pathsFixture) run(t *testing.T, stdin string, env map[string]string, args ...string) (stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	r := newRunner(strings.NewReader(stdin), &o, &e)
	r.openRepo = func(context.Context, string) (*gitx.Repo, error) {
		return &gitx.Repo{Root: f.root, GitDir: f.common, CommonDir: f.common}, nil
	}
	r.getwd = func() (string, error) { return f.root, nil }
	r.getenv = func(k string) string { return env[k] }
	if code := r.run(context.Background(), args); code != 0 {
		t.Fatalf("exit %d", code)
	}
	return o.String(), e.String()
}

// client runs a Claude Code hook and returns its decoded answer.
func (f *pathsFixture) client(t *testing.T, sub, tool string) specificOutput {
	t.Helper()
	stdout, stderr := f.run(t, `{"cwd":"`+f.root+`","tool_name":"`+tool+`"}`, nil, "claude-code", sub)
	if stderr != "" {
		t.Fatalf("stderr %q", stderr)
	}
	return decodeOutput(t, stdout)
}

func TestLedgerClientNotes(t *testing.T) {
	f := newPathsFixture(t, actNote, infoNote)

	// gitlet's own tools already show notes.
	if out := f.client(t, "post-tool-use", "mcp__gitlet__status"); out != (specificOutput{}) {
		t.Fatalf("post-tool-use for a gitlet tool = %+v", out)
	}

	out := f.client(t, "user-prompt-submit", "")
	if out.HookEventName != "UserPromptSubmit" || !strings.HasPrefix(out.AdditionalContext, notesHeading+"\n") ||
		!strings.Contains(out.AdditionalContext, actNote.Title) || !strings.Contains(out.AdditionalContext, infoNote.Title) ||
		!strings.Contains(out.AdditionalContext, `note="`+actNote.ID+`"`) {
		t.Fatalf("user-prompt-submit = %+v", out)
	}
	if st := statuses(t, f.common); st[actNote.ID] != ledger.NoteOpen || st[infoNote.ID] != ledger.NoteOpen {
		t.Fatalf("statuses after showing = %v", st)
	}
	if out := f.client(t, "user-prompt-submit", ""); out != (specificOutput{}) {
		t.Fatalf("notes shown twice: %+v", out)
	}

	// A backlog: the first notes in full, the rest stay new. The info
	// note's resolution waits until the backlog is gone.
	backlog := manyNotes(5, model.SeverityWarn)
	syncNotes(t, f.common, append([]model.Note{actNote}, backlog...)...)
	out = f.client(t, "post-tool-use", "Edit")
	if out.HookEventName != "PostToolUse" || !strings.Contains(out.AdditionalContext, "2 more new notes, run status.") ||
		strings.Contains(out.AdditionalContext, "resolved") {
		t.Fatalf("post-tool-use with a backlog = %+v", out)
	}
	st := statuses(t, f.common)
	shown := 0
	for _, n := range backlog {
		if strings.Contains(out.AdditionalContext, n.Title) != (st[n.ID] == ledger.NoteOpen) {
			t.Fatalf("note %q: shown %v, status %s", n.Title, strings.Contains(out.AdditionalContext, n.Title), st[n.ID])
		}
		if st[n.ID] == ledger.NoteOpen {
			shown++
		}
	}
	if shown != render.MaxFullNotes {
		t.Fatalf("%d notes marked shown, want %d", shown, render.MaxFullNotes)
	}

	out = f.client(t, "user-prompt-submit", "")
	if strings.Contains(out.AdditionalContext, "more new notes") || !strings.Contains(out.AdditionalContext, "resolved gitlet note: "+infoNote.Title) {
		t.Fatalf("user-prompt-submit after the backlog = %+v", out)
	}
	for _, n := range backlog {
		if s := statuses(t, f.common)[n.ID]; s != ledger.NoteOpen {
			t.Fatalf("note %q is %s after the second hook", n.Title, s)
		}
	}
	if out := f.client(t, "post-tool-use", "Bash"); out != (specificOutput{}) {
		t.Fatalf("post-tool-use with nothing new = %+v", out)
	}
}

func TestLedgerAgentOverride(t *testing.T) {
	f := newPathsFixture(t, actNote)
	ev := `{"cwd":"` + f.root + `"}`

	stdout, stderr := f.run(t, ev, map[string]string{"GITLET_AGENT": "codex-other-000000"}, "claude-code", "user-prompt-submit")
	if stdout != "" || stderr != "" {
		t.Fatalf("another agent: stdout %q, stderr %q", stdout, stderr)
	}
	stdout, stderr = f.run(t, ev, map[string]string{"GITLET_AGENT": "Not An Agent"}, "claude-code", "user-prompt-submit")
	if stdout != "" || !strings.Contains(stderr, "GITLET_AGENT") {
		t.Fatalf("invalid GITLET_AGENT: stdout %q, stderr %q", stdout, stderr)
	}
	stdout, _ = f.run(t, ev, map[string]string{"GITLET_AGENT": string(testAgent)}, "claude-code", "user-prompt-submit")
	if !strings.Contains(decodeOutput(t, stdout).AdditionalContext, actNote.Title) {
		t.Fatalf("GITLET_AGENT naming the agent: stdout %q", stdout)
	}
}

func TestLedgerPostCommit(t *testing.T) {
	f := newPathsFixture(t, actNote, infoNote)

	stdout, stderr := f.run(t, "", nil, "git", "post-commit")
	if stdout != "" || !strings.HasPrefix(stderr, notesHeading+"\n") || !strings.Contains(stderr, actNote.Title) ||
		strings.Contains(stderr, infoNote.Title) || !strings.Contains(stderr, "gitlet need src/config.ts:parseConfig") {
		t.Fatalf("post-commit: stdout %q, stderr %q", stdout, stderr)
	}
	if st := statuses(t, f.common); st[actNote.ID] != ledger.NoteOpen || st[infoNote.ID] != ledger.NoteNew {
		t.Fatalf("statuses = %v", st)
	}
	if _, stderr := f.run(t, "", nil, "git", "post-commit"); stderr != "" {
		t.Fatalf("second post-commit printed %q", stderr)
	}

	// Four new act notes: three in full, the fourth on the next commit.
	acts := manyNotes(4, model.SeverityAct)
	syncNotes(t, f.common, append([]model.Note{actNote, infoNote}, acts...)...)
	if _, stderr := f.run(t, "", nil, "git", "post-commit"); !strings.Contains(stderr, "1 more new note, run status.") {
		t.Fatalf("post-commit with four act notes printed %q", stderr)
	}
	_, stderr = f.run(t, "", nil, "git", "post-commit")
	if strings.Count(stderr, "codex-ci-1a2b3c calls ") != 1 || strings.Contains(stderr, "more new") {
		t.Fatalf("post-commit after three were shown printed %q", stderr)
	}
	if st := statuses(t, f.common); st[infoNote.ID] != ledger.NoteNew {
		t.Fatalf("info note is %s, want new", st[infoNote.ID])
	}
}

// ---- hooks that read git state: a real repository ----

// fixture is a worktree whose agent has an active item, with the state ref
// and the ledger an engine would leave behind.
type fixture struct {
	root string
	repo *gitx.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGit(t)
	dir := newGitRepo(t)
	gitIn(t, dir, nil, "switch", "-q", "-c", testBranch)
	repo, err := gitx.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: repo.Root, repo: repo}
	f.writeState(t, model.ItemActive)
	seedLedger(t, repo.CommonDir, f.root, actNote, infoNote)
	return f
}

// writeState writes the agent's local state ref with the test item, the way
// the bus does: a parentless commit holding state.json.
func (f *fixture) writeState(t *testing.T, status model.ItemStatus) {
	t.Helper()
	now := time.Now().UTC()
	st := model.AgentState{
		Format:       model.Format,
		Agent:        model.Agent{ID: testAgent, Client: "claude-code", Host: "test"},
		Heartbeat:    now,
		LeaseSeconds: 600,
		Items: []model.WorkItem{{
			ID: testItem, Intent: testIntent, Branch: testBranch, Status: status, OpenedAt: now, UpdatedAt: now,
		}},
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	blob, errOut, err := gitRun(f.root, nil, data, "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatalf("hash-object: %v\n%s", err, errOut)
	}
	tree, errOut, err := gitRun(f.root, nil, []byte("100644 blob "+blob+"\tstate.json\n"), "mktree")
	if err != nil {
		t.Fatalf("mktree: %v\n%s", err, errOut)
	}
	commit := gitIn(t, f.root, nil, "commit-tree", tree, "-m", "gitlet: state")
	gitIn(t, f.root, nil, "update-ref", stateRef(testAgent), commit)
}

// client runs a Claude Code hook through Run and returns its decoded answer.
func client(t *testing.T, sub string, ev map[string]any) specificOutput {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"claude-code", sub}, bytes.NewReader(data), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr %q", stderr.String())
	}
	return decodeOutput(t, stdout.String())
}

func TestIntegrationClientHooks(t *testing.T) {
	f := newFixture(t)
	sub := filepath.Join(f.root, "src")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Session start: a short status, and the notes stay new.
	out := client(t, "session-start", map[string]any{"session_id": "s1", "cwd": f.root, "hook_event_name": "SessionStart", "source": "startup"})
	want := "gitlet: 2 open notes (1 act).\nYour task: \"" + testIntent + "\" on branch " + testBranch + "."
	if out.HookEventName != "SessionStart" || !strings.HasPrefix(out.AdditionalContext, want) {
		t.Fatalf("session-start = %+v", out)
	}

	// The first prompt gets them, from a subdirectory of the worktree too.
	out = client(t, "user-prompt-submit", map[string]any{"cwd": sub, "hook_event_name": "UserPromptSubmit", "prompt": "go on"})
	if out.HookEventName != "UserPromptSubmit" || !strings.Contains(out.AdditionalContext, actNote.Title) ||
		!strings.Contains(out.AdditionalContext, infoNote.Title) {
		t.Fatalf("user-prompt-submit = %+v", out)
	}
	if out := client(t, "post-tool-use", map[string]any{"cwd": f.root, "tool_name": "Bash"}); out != (specificOutput{}) {
		t.Fatalf("notes shown twice: %+v", out)
	}

	// On another branch, session start names the task and its branch.
	gitIn(t, f.root, nil, "switch", "-q", "main")
	out = client(t, "session-start", map[string]any{"cwd": f.root})
	if !strings.HasSuffix(out.AdditionalContext, "\nYour task \""+testIntent+"\" is on branch "+testBranch+", not the one checked out: switch back to it, or call status to see it and its notes.") {
		t.Fatalf("session-start on another branch = %+v", out)
	}

	// Without an active item, session start asks for start.
	f.writeState(t, model.ItemSubmitted)
	out = client(t, "session-start", map[string]any{"cwd": f.root})
	if out.AdditionalContext != "gitlet: 2 open notes (1 act).\nNo task started in this worktree: call status to see the notes, and start with a one-sentence intent before you edit code." {
		t.Fatalf("session-start without an item = %+v", out)
	}

	// An agent with no state ref yet has no task, and that is no error.
	t.Setenv("GITLET_AGENT", "codex-other-000000")
	out = client(t, "session-start", map[string]any{"cwd": f.root})
	if out.AdditionalContext != "gitlet: no open notes.\nNo task started in this worktree: call start with a one-sentence intent before you edit code." {
		t.Fatalf("session-start for an agent without state = %+v", out)
	}
}

// commitMessage returns the message of a commit.
func commitMessage(t *testing.T, dir, rev string) string {
	t.Helper()
	return gitIn(t, dir, nil, "log", "-1", "--format=%B", rev)
}

func TestIntegrationPrepareCommitMsg(t *testing.T) {
	f := newFixture(t)
	installHook(t, f.root, "prepare-commit-msg")
	trailers := "Gitlet-Item: " + string(testItem) + "\nGitlet-Agent: " + string(testAgent)

	commitFile(t, f.root, "a.txt", "a\n", "Add strict mode")
	if got := commitMessage(t, f.root, "HEAD"); got != "Add strict mode\n\n"+trailers {
		t.Fatalf("message = %q", got)
	}

	// Amending keeps one copy.
	gitIn(t, f.root, nil, "commit", "-q", "--amend", "--no-edit")
	if got := commitMessage(t, f.root, "HEAD"); strings.Count(got, "Gitlet-Item:") != 1 || strings.Count(got, "Gitlet-Agent:") != 1 {
		t.Fatalf("amended message = %q", got)
	}

	// Trailers join the sign-off block.
	if err := os.WriteFile(filepath.Join(f.root, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.root, nil, "add", "b.txt")
	gitIn(t, f.root, nil, "commit", "-q", "-s", "-m", "Add b")
	if got := commitMessage(t, f.root, "HEAD"); !strings.HasPrefix(got, "Add b\n\nSigned-off-by: ") || !strings.HasSuffix(got, ">\n"+trailers) {
		t.Fatalf("signed-off message = %q", got)
	}

	// An editor commit with no text yet is left alone, so it still aborts.
	if err := os.WriteFile(filepath.Join(f.root, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.root, nil, "add", "c.txt")
	if _, stderr, err := gitRun(f.root, []string{"GIT_EDITOR=true"}, nil, "commit", "-v"); err == nil || !strings.Contains(stderr, "empty") {
		t.Fatalf("empty editor commit did not abort: %v\n%s", err, stderr)
	}
	gitIn(t, f.root, nil, "reset", "-q", "--hard")

	// Merges and cherry-picks keep the messages they have.
	gitIn(t, f.root, nil, "switch", "-q", "-c", "other", "main")
	commitFile(t, f.root, "other.txt", "o\n", "Other work")
	other := gitIn(t, f.root, nil, "rev-parse", "HEAD")
	commitFile(t, f.root, "pick.txt", "p\n", "Picked work")
	picked := gitIn(t, f.root, nil, "rev-parse", "HEAD")
	if got := commitMessage(t, f.root, "HEAD"); got != "Picked work" {
		t.Fatalf("commit on a branch without an item = %q", got)
	}
	gitIn(t, f.root, nil, "switch", "-q", testBranch)
	gitIn(t, f.root, nil, "cherry-pick", picked)
	if got := commitMessage(t, f.root, "HEAD"); got != "Picked work" {
		t.Fatalf("cherry-picked message = %q", got)
	}
	gitIn(t, f.root, nil, "merge", "-q", "--no-ff", "--no-edit", other)
	if got := commitMessage(t, f.root, "HEAD"); strings.Contains(got, "Gitlet-") {
		t.Fatalf("merge message = %q", got)
	}

	// Without an active item nothing is added.
	f.writeState(t, model.ItemSubmitted)
	commitFile(t, f.root, "d.txt", "d\n", "After submit")
	if got := commitMessage(t, f.root, "HEAD"); got != "After submit" {
		t.Fatalf("message without an active item = %q", got)
	}
}

func TestIntegrationPostCommit(t *testing.T) {
	f := newFixture(t)
	installHook(t, f.root, "post-commit")
	commit := func(name string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, f.root, nil, "add", name)
		_, stderr, err := gitRun(f.root, nil, nil, "commit", "-q", "-m", "Add "+name)
		if err != nil {
			t.Fatalf("commit: %v\n%s", err, stderr)
		}
		return stderr
	}
	if stderr := commit("a.txt"); !strings.Contains(stderr, actNote.Title) || strings.Contains(stderr, infoNote.Title) {
		t.Fatalf("post-commit printed %q", stderr)
	}
	if stderr := commit("b.txt"); stderr != "" {
		t.Fatalf("second post-commit printed %q", stderr)
	}

	// gitlet's use and update replay commits with git's hooks running:
	// their output is swallowed, so a new act note must stay new for the
	// verb's result instead of being marked shown there.
	gitIn(t, f.root, nil, "switch", "-q", "-c", "stub", "main")
	commit("stub.txt")
	stub := gitIn(t, f.root, nil, "rev-parse", "HEAD")
	gitIn(t, f.root, nil, "switch", "-q", testBranch)
	fresh := manyNotes(1, model.SeverityAct)[0]
	syncNotes(t, f.repo.CommonDir, actNote, infoNote, fresh)
	ctx := context.Background()
	for _, step := range []struct {
		name string
		run  func() (gitx.ApplyResult, error)
	}{
		{"cherry-pick", func() (gitx.ApplyResult, error) { return f.repo.CherryPick(ctx, stub) }},
		{"revert", func() (gitx.ApplyResult, error) { return f.repo.Revert(ctx, "HEAD") }},
		{"rebase", func() (gitx.ApplyResult, error) { return f.repo.Rebase(ctx, gitx.RebaseOptions{Upstream: "stub"}) }},
	} {
		if res, err := step.run(); err != nil || !res.OK {
			t.Fatalf("%s: %+v, %v", step.name, res, err)
		}
		if st := statuses(t, f.repo.CommonDir)[fresh.ID]; st != ledger.NoteNew {
			t.Fatalf("after gitlet's %s the new act note is %s, want new", step.name, st)
		}
	}
	if stderr := commit("c.txt"); !strings.Contains(stderr, fresh.Title) {
		t.Fatalf("post-commit after the replays printed %q", stderr)
	}
}

func TestIntegrationPrePush(t *testing.T) {
	f := newFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, f.root, nil, "init", "-q", "--bare", remote)
	gitIn(t, f.root, nil, "remote", "add", "origin", remote)

	// origin/main, as last fetched, changes README one way; the item
	// branch changes it another.
	gitIn(t, f.root, nil, "switch", "-q", "-c", "tmp", "main")
	commitFile(t, f.root, "README", "remote\n", "Remote change")
	gitIn(t, f.root, nil, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, f.root, nil, "switch", "-q", testBranch)
	gitIn(t, f.root, nil, "branch", "-q", "-D", "tmp")
	commitFile(t, f.root, "README", "local\n", "Local change")
	if err := os.WriteFile(filepath.Join(f.root, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(f.root)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"git", "pre-push", "origin", remote}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"gitlet: work item " + string(testItem),
		"- 1 uncommitted change is not in this push",
		"- merging into origin/main as last fetched conflicts in README",
		"- 1 open act note",
		"gitlet submit",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("pre-push output lacks %q:\n%s", want, stderr.String())
		}
	}
	if stdout.Len() > 0 {
		t.Fatalf("stdout %q", stdout.String())
	}

	// The checks never block the push.
	installHook(t, f.root, "pre-push")
	_, errOut, err := gitRun(f.root, nil, nil, "push", "-q", "origin", testBranch)
	if err != nil || !strings.Contains(errOut, "gitlet submit") {
		t.Fatalf("push: %v\n%s", err, errOut)
	}

	// Off the item's branch, or without an active item, there is nothing to say.
	for _, step := range []struct {
		name  string
		setup func()
	}{
		{"on another branch", func() { gitIn(t, f.root, nil, "switch", "-q", "main") }},
		{"without an active item", func() {
			gitIn(t, f.root, nil, "switch", "-q", testBranch)
			f.writeState(t, model.ItemSubmitted)
		}},
	} {
		step.setup()
		stderr.Reset()
		Run(context.Background(), []string{"git", "pre-push", "origin", remote}, strings.NewReader(""), &stdout, &stderr)
		if stdout.Len()+stderr.Len() > 0 {
			t.Fatalf("pre-push %s: stdout %q, stderr %q", step.name, stdout.String(), stderr.String())
		}
	}
}

func TestIntegrationSilence(t *testing.T) {
	requireGit(t)
	notRepo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notRepo))
	noLedger := newGitRepo(t)
	ctx := context.Background()

	for _, dir := range []string{notRepo, noLedger} {
		for _, args := range hookArgs {
			t.Run(filepath.Base(dir)+"/"+strings.Join(args[:2], " "), func(t *testing.T) {
				args, file := withMessageFile(t, args)
				t.Chdir(dir)
				var stdout, stderr bytes.Buffer
				ev := `{"cwd":"` + dir + `","tool_name":"Bash"}`
				if code := Run(ctx, args, strings.NewReader(ev), &stdout, &stderr); code != 0 {
					t.Fatalf("exit %d", code)
				}
				if stdout.Len()+stderr.Len() > 0 {
					t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
				}
				assertMessage(t, file, "Add strict mode\n")
			})
		}
	}
	if _, err := os.Stat(filepath.Join(noLedger, ".git", "gitlet")); !os.IsNotExist(err) {
		t.Fatalf("a hook created the ledger: %v", err)
	}
}
