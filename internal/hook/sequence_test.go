package hook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteTodo(t *testing.T) {
	const full = "3f2a9c1d0e4b5a6978877665544332211aabbccd"
	tests := []struct {
		name  string
		todo  string
		drop  []string
		want  string
		count int
	}{
		{
			name:  "full id drops abbreviated pick",
			todo:  "pick 3f2a9c1 stub: parseConfig\npick 1234abc keep me\n",
			drop:  []string{full},
			want:  "drop 3f2a9c1 stub: parseConfig\npick 1234abc keep me\n",
			count: 1,
		},
		{
			name:  "short prefix drops longer pick",
			todo:  "pick 3f2a9c1d0e4b stub\n",
			drop:  []string{"3f2a"},
			want:  "drop 3f2a9c1d0e4b stub\n",
			count: 1,
		},
		{
			name:  "case-insensitive",
			todo:  "pick 3F2A9C1 stub\n",
			drop:  []string{"3f2a9c1d"},
			want:  "drop 3F2A9C1 stub\n",
			count: 1,
		},
		{
			name:  "no match keeps everything",
			todo:  "pick 3f2a9c1 stub\npick 1234abc other\n",
			drop:  []string{"deadbeef"},
			want:  "pick 3f2a9c1 stub\npick 1234abc other\n",
			count: 0,
		},
		{
			name:  "several drops",
			todo:  "pick aaaa111 one\npick bbbb222 two\npick cccc333 three\n",
			drop:  []string{"aaaa1112", "cccc333"},
			want:  "drop aaaa111 one\npick bbbb222 two\ndrop cccc333 three\n",
			count: 2,
		},
		{
			name: "comments and blank lines kept",
			todo: "pick 3f2a9c1 stub\n\n# Rebase 0dc5527..5b4c160 onto 0dc5527 (1 command)\n" +
				"#\n# p, pick 3f2a9c1 = use commit\n# d, drop <commit> = remove commit\n",
			drop: []string{"3f2a9c1"},
			want: "drop 3f2a9c1 stub\n\n# Rebase 0dc5527..5b4c160 onto 0dc5527 (1 command)\n" +
				"#\n# p, pick 3f2a9c1 = use commit\n# d, drop <commit> = remove commit\n",
			count: 1,
		},
		{
			name: "other commands untouched",
			todo: "label onto\nreset onto\nexec make test 3f2a9c1\nfixup 3f2a9c1 fix\nreword 3f2a9c1 r\n" +
				"merge -C 3f2a9c1 topic\nupdate-ref refs/heads/x\nbreak\npick 3f2a9c1 stub\n",
			drop: []string{"3f2a9c1"},
			want: "label onto\nreset onto\nexec make test 3f2a9c1\nfixup 3f2a9c1 fix\nreword 3f2a9c1 r\n" +
				"merge -C 3f2a9c1 topic\nupdate-ref refs/heads/x\nbreak\ndrop 3f2a9c1 stub\n",
			count: 1,
		},
		{
			name:  "abbreviated command",
			todo:  "p 3f2a9c1 stub\np 1234abc other\n",
			drop:  []string{"3f2a9c1"},
			want:  "d 3f2a9c1 stub\np 1234abc other\n",
			count: 1,
		},
		{
			name:  "indentation, tabs and CRLF kept",
			todo:  "  pick\t3f2a9c1   stub\r\npick 1234abc other\r\n",
			drop:  []string{"3f2a9c1"},
			want:  "  drop\t3f2a9c1   stub\r\npick 1234abc other\r\n",
			count: 1,
		},
		{
			name:  "last line without newline and without subject",
			todo:  "pick 1234abc other\npick 3f2a9c1",
			drop:  []string{"3f2a9c1"},
			want:  "pick 1234abc other\ndrop 3f2a9c1",
			count: 1,
		},
		{
			name:  "already dropped",
			todo:  "drop 3f2a9c1 stub\n",
			drop:  []string{"3f2a9c1"},
			want:  "drop 3f2a9c1 stub\n",
			count: 0,
		},
		{
			name:  "malformed pick lines untouched",
			todo:  "pick\npick zzzz123 not hex\npick 3f2 too short\npicky 3f2a9c1 not a command\n",
			drop:  []string{"3f2a9c1", "3f2a"},
			want:  "pick\npick zzzz123 not hex\npick 3f2 too short\npicky 3f2a9c1 not a command\n",
			count: 0,
		},
		{
			name:  "empty todo",
			todo:  "",
			drop:  []string{"3f2a9c1"},
			want:  "",
			count: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := rewriteTodo(tt.todo, tt.drop)
			if got != tt.want || n != tt.count {
				t.Fatalf("rewriteTodo() = %q, %d\nwant %q, %d", got, n, tt.want, tt.count)
			}
		})
	}
}

func TestParseDrop(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"   ", nil, false},
		{"3F2A9C1 abcd\t1234567890abcdef1234567890abcdef12345678\n", []string{"3f2a9c1", "abcd", "1234567890abcdef1234567890abcdef12345678"}, false},
		{"abc", nil, true},
		{"3f2a9c1 main", nil, true},
		{"HEAD~1", nil, true},
		{strings.Repeat("a", 65), nil, true},
	}
	for _, tt := range tests {
		got, err := parseDrop(tt.in)
		if (err != nil) != tt.wantErr || strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("parseDrop(%q) = %q, %v; want %q, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestSequenceEditor(t *testing.T) {
	const todo = "pick 3f2a9c1 stub: parseConfig\npick 1234abc keep me\n\n# Commands:\n# p, pick <commit> = use commit\n"
	tests := []struct {
		name     string
		args     []string // nil: the todo file
		drop     string
		wantCode int
		wantTodo string
	}{
		{"drops listed commit", nil, "3f2a9c1d0e4b", 0, strings.Replace(todo, "pick 3f2a9c1", "drop 3f2a9c1", 1)},
		{"nothing to drop", nil, "", 0, todo},
		{"no match", nil, "deadbeef", 0, todo},
		{"invalid GITLET_DROP fails", nil, "not-an-oid", 1, todo},
		{"missing file fails", []string{"/nonexistent/git-rebase-todo"}, "3f2a9c1", 1, todo},
		{"no argument is misuse", []string{}, "3f2a9c1", 2, todo},
		{"two arguments are misuse", []string{"a", "b"}, "3f2a9c1", 2, todo},
		{"empty argument is misuse", []string{""}, "3f2a9c1", 2, todo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "git-rebase-todo")
			if err := os.WriteFile(path, []byte(todo), 0o600); err != nil {
				t.Fatal(err)
			}
			args := tt.args
			if args == nil {
				args = []string{path}
			}
			var stdout, stderr bytes.Buffer
			r := newRunner(nil, &stdout, &stderr)
			r.getenv = func(k string) string {
				if k == "GITLET_DROP" {
					return tt.drop
				}
				return ""
			}
			code := r.run(context.Background(), append([]string{"sequence-editor"}, args...))
			if code != tt.wantCode {
				t.Fatalf("exit %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if (code != 0) != (stderr.Len() > 0) || stdout.Len() > 0 {
				t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.wantTodo {
				t.Fatalf("todo = %q, want %q", got, tt.wantTodo)
			}
			fi, err := os.Stat(path)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, %v; want 0600", fi.Mode(), err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Fatalf("temporary files left behind: %v", entries)
			}
		})
	}
}

func TestSequenceEditorCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "git-rebase-todo")
	if err := os.WriteFile(path, []byte("pick 3f2a9c1 stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITLET_DROP", "3f2a9c1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := Run(ctx, []string{"sequence-editor", path}, nil, nil, nil); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// TestSequenceEditorRebase runs the sequence editor the way update does,
// under a real interactive rebase.
func TestSequenceEditorRebase(t *testing.T) {
	dir := newGitRepo(t)
	base := gitIn(t, dir, nil, "rev-parse", "HEAD")
	commitFile(t, dir, "a.txt", "a\n", "keep a")
	commitFile(t, dir, "stub.txt", "stub\n", "gitlet stub: parseConfig")
	stub := gitIn(t, dir, nil, "rev-parse", "HEAD")
	commitFile(t, dir, "b.txt", "b\n", "keep b")
	editor := []string{"GIT_SEQUENCE_EDITOR=" + helperCommand(t, "sequence-editor"), "GIT_EDITOR=true"}

	// A broken GITLET_DROP fails the editor, and git aborts the rebase.
	head := gitIn(t, dir, nil, "rev-parse", "HEAD")
	if _, stderr, err := gitRun(dir, append(editor, "GITLET_DROP=HEAD"), nil, "rebase", "-i", base); err == nil {
		t.Fatalf("rebase with a broken GITLET_DROP succeeded\n%s", stderr)
	}
	if got := gitIn(t, dir, nil, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved from %s to %s", head, got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Fatalf("rebase left in progress: %v", err)
	}

	gitIn(t, dir, append(editor, "GITLET_DROP="+stub), "rebase", "-q", "-i", base)
	if got := gitIn(t, dir, nil, "log", "--format=%s", base+"..HEAD"); got != "keep b\nkeep a" {
		t.Fatalf("commits after rebase:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "stub.txt")); !os.IsNotExist(err) {
		t.Fatalf("stub.txt still present: %v", err)
	}
}
