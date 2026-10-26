package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// hookArgs lists one invocation of every client and git hook. The message
// file of prepare-commit-msg is filled in by the tests.
var hookArgs = [][]string{
	{"claude-code", "session-start"},
	{"claude-code", "user-prompt-submit"},
	{"claude-code", "post-tool-use"},
	{"git", "prepare-commit-msg", "", "message"},
	{"git", "post-commit"},
	{"git", "pre-push", "origin", "https://example.com/repo.git"},
}

// withMessageFile returns args with a commit message file in place of "".
func withMessageFile(t *testing.T, args []string) (out []string, file string) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(file, []byte("Add strict mode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = append([]string(nil), args...)
	for i, a := range out {
		if a == "" {
			out[i] = file
		}
	}
	return out, file
}

func assertMessage(t *testing.T, file, want string) {
	t.Helper()
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("message file = %q, want %q", got, want)
	}
}

func TestUsage(t *testing.T) {
	tests := []struct {
		args []string
		code int
	}{
		{nil, 0},
		{[]string{"bogus"}, 0},
		{[]string{"claude-code"}, 0},
		{[]string{"claude-code", "pre-tool-use"}, 0},
		{[]string{"claude-code", "SessionStart"}, 0},
		{[]string{"git"}, 0},
		{[]string{"git", "pre-commit"}, 0},
		{[]string{"git", "prepare-commit-msg"}, 0},
		{[]string{"git", "prepare-commit-msg", ""}, 0},
		{[]string{"sequence-editor"}, 2},
		{[]string{"sequence-editor", "a", "b"}, 2},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			r, dirs, stdout, stderr := recordingRunner("")
			if code := r.run(context.Background(), tt.args); code != tt.code {
				t.Fatalf("exit %d, want %d", code, tt.code)
			}
			if !strings.HasPrefix(stderr.String(), "usage: gitlet hook ") || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("stderr %q, want one usage line", stderr)
			}
			if stdout.Len() > 0 || len(*dirs) > 0 {
				t.Fatalf("stdout %q, opened %q", stdout, *dirs)
			}
		})
	}
}

func TestNotRepoIsSilent(t *testing.T) {
	for _, args := range hookArgs {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			args, file := withMessageFile(t, args)
			r, dirs, stdout, stderr := recordingRunner(`{"cwd":"/not/a/repo","tool_name":"Bash"}`)
			if code := r.run(context.Background(), args); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(*dirs) != 1 {
				t.Fatalf("opened %q, want one lookup", *dirs)
			}
			if stdout.Len()+stderr.Len() > 0 {
				t.Fatalf("stdout %q, stderr %q", stdout, stderr)
			}
			assertMessage(t, file, "Add strict mode\n")
		})
	}
}

func TestMissingLedgerIsSilent(t *testing.T) {
	for _, args := range hookArgs {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			args, file := withMessageFile(t, args)
			root := t.TempDir()
			common := filepath.Join(root, ".git")
			if err := os.Mkdir(common, 0o755); err != nil {
				t.Fatal(err)
			}
			r, _, stdout, stderr := recordingRunner(`{"cwd":"` + root + `"}`)
			r.openRepo = func(context.Context, string) (*gitx.Repo, error) {
				// No git command runs before the ledger check, so the
				// repository needs no more than its paths.
				return &gitx.Repo{Root: root, GitDir: common, CommonDir: common}, nil
			}
			if code := r.run(context.Background(), args); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if stdout.Len()+stderr.Len() > 0 {
				t.Fatalf("stdout %q, stderr %q", stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(common, "gitlet")); !os.IsNotExist(err) {
				t.Fatalf("a hook created the ledger directory: %v", err)
			}
			assertMessage(t, file, "Add strict mode\n")
		})
	}
}

func TestPrepareCommitMsgSkipsSources(t *testing.T) {
	for _, source := range []string{"merge", "squash", "template"} {
		t.Run(source, func(t *testing.T) {
			args, file := withMessageFile(t, []string{"git", "prepare-commit-msg", "", source})
			r, dirs, stdout, stderr := recordingRunner("")
			if code := r.run(context.Background(), args); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if len(*dirs) > 0 || stdout.Len()+stderr.Len() > 0 {
				t.Fatalf("opened %q, stdout %q, stderr %q", *dirs, stdout, stderr)
			}
			assertMessage(t, file, "Add strict mode\n")
		})
	}
}

func TestNeverFails(t *testing.T) {
	// ledgerRepo returns a repository whose ledger file exists but is empty.
	ledgerRepo := func(t *testing.T) *gitx.Repo {
		root := t.TempDir()
		common := filepath.Join(root, ".git")
		if err := os.MkdirAll(filepath.Join(common, "gitlet"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(common, "gitlet", "ledger.db"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return &gitx.Repo{Root: root, GitDir: common, CommonDir: common}
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, r *runner)
	}{
		{"panic while opening the repository", func(t *testing.T, r *runner) {
			r.openRepo = func(context.Context, string) (*gitx.Repo, error) { panic("boom") }
		}},
		{"repository error", func(t *testing.T, r *runner) {
			r.openRepo = func(context.Context, string) (*gitx.Repo, error) {
				return nil, &gitx.Error{Args: []string{"rev-parse"}, ExitCode: 128, Stderr: "fatal: broken"}
			}
		}},
		{"working directory gone", func(t *testing.T, r *runner) {
			r.getwd = func() (string, error) { return "", errors.New("getwd: no such file or directory") }
		}},
		{"empty ledger file, real dependencies", func(t *testing.T, r *runner) {
			// With the ledger still a stub, opening it panics; once it
			// lands, an empty file is an empty ledger with no agent.
			repo := ledgerRepo(t)
			r.openRepo = func(context.Context, string) (*gitx.Repo, error) { return repo, nil }
		}},
		{"invalid GITLET_AGENT", func(t *testing.T, r *runner) {
			repo := ledgerRepo(t)
			r.openRepo = func(context.Context, string) (*gitx.Repo, error) { return repo, nil }
			r.getenv = func(k string) string {
				if k == "GITLET_AGENT" {
					return "Not An Agent!"
				}
				return ""
			}
		}},
	}
	for _, tt := range tests {
		for _, args := range hookArgs {
			if args[1] == "session-start" && strings.HasPrefix(tt.name, "empty ledger") {
				continue // prints the status of a worktree with no task once the ledger works
			}
			t.Run(tt.name+"/"+strings.Join(args[:2], " "), func(t *testing.T) {
				args, _ := withMessageFile(t, args)
				r, _, stdout, _ := recordingRunner(`{"tool_name":"Bash"}`)
				tt.setup(t, r)
				if code := r.run(context.Background(), args); code != 0 {
					t.Fatalf("exit %d", code)
				}
				if stdout.Len() > 0 {
					t.Fatalf("stdout %q", stdout)
				}
			})
		}
	}
}

func TestPanicIsReported(t *testing.T) {
	r, _, stdout, stderr := recordingRunner("")
	r.openRepo = func(context.Context, string) (*gitx.Repo, error) { panic("boom") }
	if code := r.run(context.Background(), []string{"git", "post-commit"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout.Len() > 0 || stderr.String() != "gitlet hook: internal error: boom\n" {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
}

func TestRunNilStreams(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"claude-code", "user-prompt-submit"}} {
		if code := Run(context.Background(), args, nil, nil, nil); code != 0 {
			t.Fatalf("Run(%q) = %d", args, code)
		}
	}
}

func TestClientHookRespectsContext(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		r, dirs, stdout, stderr := recordingRunner(`{"cwd":"/work/repo"}`)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if code := r.run(ctx, []string{"claude-code", "user-prompt-submit"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if len(*dirs) > 0 || stdout.Len()+stderr.Len() > 0 {
			t.Fatalf("opened %q, stdout %q, stderr %q", *dirs, stdout, stderr)
		}
	})
	t.Run("stdin never closed", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() })
		r, dirs, stdout, stderr := recordingRunner("")
		r.stdin = pr
		r.clientBudget = 50 * time.Millisecond
		start := time.Now()
		if code := r.run(context.Background(), []string{"claude-code", "post-tool-use"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("hook took %v with a 50ms budget", d)
		}
		if len(*dirs) > 0 || stdout.Len() > 0 || !strings.Contains(stderr.String(), "out of time") {
			t.Fatalf("opened %q, stdout %q, stderr %q", *dirs, stdout, stderr)
		}
	})
}

func TestPickItem(t *testing.T) {
	item := func(id, branch string, status model.ItemStatus) model.WorkItem {
		return model.WorkItem{ID: model.ItemID(id), Branch: branch, Status: status}
	}
	tests := []struct {
		name   string
		items  []model.WorkItem
		branch string
		want   model.ItemID // pickItem
		sole   model.ItemID // soleActive
	}{
		{"none", nil, "main", "", ""},
		{"on its branch", []model.WorkItem{item("aaaaaaaaaaaa", "gitlet/a", model.ItemActive)}, "gitlet/a", "aaaaaaaaaaaa", "aaaaaaaaaaaa"},
		{"only active item on another branch", []model.WorkItem{item("aaaaaaaaaaaa", "gitlet/a", model.ItemActive)}, "main", "", "aaaaaaaaaaaa"},
		{"only active item, detached", []model.WorkItem{item("aaaaaaaaaaaa", "gitlet/a", model.ItemActive)}, "", "", "aaaaaaaaaaaa"},
		{"only active item without a branch", []model.WorkItem{item("aaaaaaaaaaaa", "", model.ItemActive)}, "main", "aaaaaaaaaaaa", "aaaaaaaaaaaa"},
		{"only active item without a branch, detached", []model.WorkItem{item("aaaaaaaaaaaa", "", model.ItemActive)}, "", "aaaaaaaaaaaa", "aaaaaaaaaaaa"},
		{"branch match wins", []model.WorkItem{
			item("aaaaaaaaaaaa", "", model.ItemActive),
			item("bbbbbbbbbbbb", "gitlet/b", model.ItemActive),
		}, "gitlet/b", "bbbbbbbbbbbb", ""},
		{"several active, no match", []model.WorkItem{
			item("aaaaaaaaaaaa", "", model.ItemActive),
			item("bbbbbbbbbbbb", "gitlet/b", model.ItemActive),
		}, "main", "", ""},
		{"closed items ignored", []model.WorkItem{
			item("aaaaaaaaaaaa", "main", model.ItemSubmitted),
			item("bbbbbbbbbbbb", "main", model.ItemLanded),
			item("cccccccccccc", "main", model.ItemAbandoned),
			item("dddddddddddd", "", model.ItemActive),
		}, "main", "dddddddddddd", "dddddddddddd"},
		{"malformed id ignored", []model.WorkItem{
			item("not-an-id\nGitlet-Item: x", "main", model.ItemActive),
			item("eeeeeeeeeeee", "gitlet/e", model.ItemActive),
		}, "main", "", "eeeeeeeeeeee"},
	}
	id := func(it *model.WorkItem) model.ItemID {
		if it == nil {
			return ""
		}
		return it.ID
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := id(pickItem(tt.items, tt.branch)); got != tt.want {
				t.Fatalf("pickItem() = %q, want %q", got, tt.want)
			}
			if got := id(soleActive(tt.items)); got != tt.sole {
				t.Fatalf("soleActive() = %q, want %q", got, tt.sole)
			}
		})
	}
}

func TestPrePushReport(t *testing.T) {
	item := &model.WorkItem{ID: "8c1f0e2a9b3d", Intent: "Add strict mode to config loading"}
	got := prePushReport(item, []string{
		"2 uncommitted changes are not in this push; submit refuses until they are committed.",
		"1 open act note: run gitlet status and act on them.",
	})
	want := "gitlet: work item 8c1f0e2a9b3d \"Add strict mode to config loading\" is active on this branch.\n" +
		"- 2 uncommitted changes are not in this push; submit refuses until they are committed.\n" +
		"- 1 open act note: run gitlet status and act on them.\n" +
		"gitlet submit runs every check against fresh main, pushes the branch and opens the pull request; use it instead of git push.\n"
	if got != want {
		t.Fatalf("prePushReport() =\n%s\nwant\n%s", got, want)
	}
	if got := listPaths([]string{"a", "b", "c", "d", "e", "f", "g"}, 5); got != "a, b, c, d, e and 2 more" {
		t.Fatalf("listPaths() = %q", got)
	}
	if got := plural(1, "note", "notes") + ", " + plural(3, "note", "notes"); got != "1 note, 3 notes" {
		t.Fatalf("plural() = %q", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, []byte("new")) {
		t.Fatalf("content = %q", got)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing", "file"), []byte("x")); err == nil {
		t.Fatal("writing into a missing directory succeeded")
	}
}
