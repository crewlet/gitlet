package gitx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestRevisions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c1 := f.commitFiles("one", map[string]string{"a.txt": "a\n"})
	c2 := f.commitFiles("two", map[string]string{"b.txt": "b\n"})
	f.git("switch", "-q", "--orphan", "lonely")
	lonely := f.commitFiles("lonely", map[string]string{"z.txt": "z\n"})
	f.git("switch", "-q", "main")

	if got, err := f.repo.RevParse(bg, "HEAD~1"); err != nil || got != c1 {
		t.Errorf("RevParse(HEAD~1) = %q, %v", got, err)
	}
	if got, err := f.repo.RevParse(bg, c2[:8]); err != nil || got != c2 {
		t.Errorf("RevParse(short) = %q, %v", got, err)
	}
	for _, rev := range []string{"nope", "HEAD~5", "HEAD:missing.txt"} {
		if _, err := f.repo.RevParse(bg, rev); !errors.Is(err, ErrNotFound) {
			t.Errorf("RevParse(%q) = %v, want ErrNotFound", rev, err)
		}
	}
	for _, rev := range []string{"", "--all", "a\nb"} {
		if _, err := f.repo.RevParse(bg, rev); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("RevParse(%q) = %v, want an invalid revision error", rev, err)
		}
	}
	tree := f.git("rev-parse", "HEAD^{tree}")
	if got, err := f.repo.TreeOf(bg, "HEAD"); err != nil || got != tree {
		t.Errorf("TreeOf = %q, %v", got, err)
	}

	if b, err := f.repo.CurrentBranch(bg); err != nil || b != "main" {
		t.Errorf("CurrentBranch = %q, %v", b, err)
	}
	for branch, want := range map[string]bool{"main": true, "lonely": true, "nope": false} {
		if got, err := f.repo.BranchExists(bg, branch); err != nil || got != want {
			t.Errorf("BranchExists(%s) = %v, %v", branch, got, err)
		}
	}

	if got, err := f.repo.MergeBase(bg, "main", "HEAD~1"); err != nil || got != c1 {
		t.Errorf("MergeBase = %q, %v", got, err)
	}
	if _, err := f.repo.MergeBase(bg, "main", lonely); !errors.Is(err, ErrNotFound) {
		t.Errorf("MergeBase(unrelated) = %v, want ErrNotFound", err)
	}
	if _, err := f.repo.MergeBase(bg, "main", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MergeBase(missing) = %v, want ErrNotFound", err)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{{c1, c2, true}, {c2, c2, true}, {c2, c1, false}, {lonely, c2, false}} {
		if got, err := f.repo.IsAncestor(bg, c.a, c.b); err != nil || got != c.want {
			t.Errorf("IsAncestor(%.7s, %.7s) = %v, %v", c.a, c.b, got, err)
		}
	}
	if _, err := f.repo.IsAncestor(bg, "nope", c2); !errors.Is(err, ErrNotFound) {
		t.Errorf("IsAncestor(missing) = %v, want ErrNotFound", err)
	}

	f.git("switch", "-q", "--detach", c1)
	if b, err := f.repo.CurrentBranch(bg); err != nil || b != "" {
		t.Errorf("CurrentBranch(detached) = %q, %v", b, err)
	}
}

func TestReadFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("one", map[string]string{"dir/my file.txt": "hello\n", "empty": ""})

	tests := []struct {
		rev, path, want string
		err             error
	}{
		{"HEAD", "dir/my file.txt", "hello\n", nil},
		{"HEAD", "empty", "", nil},
		{"HEAD", "missing.txt", "", ErrNotFound},
		{"nope", "dir/my file.txt", "", ErrNotFound},
	}
	for _, tt := range tests {
		got, err := f.repo.ReadFile(bg, tt.rev, tt.path)
		if !errors.Is(err, tt.err) || string(got) != tt.want {
			t.Errorf("ReadFile(%s, %s) = %q, %v", tt.rev, tt.path, got, err)
		}
	}
	if _, err := f.repo.ReadFile(bg, "HEAD", "dir"); err == nil {
		t.Error("ReadFile(directory) succeeded")
	}
	// ErrNotFound keeps git's error in the chain.
	_, err := f.repo.ReadFile(bg, "HEAD", "missing.txt")
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Stderr, "does not exist") {
		t.Errorf("ReadFile(missing) = %v, want the *Error kept", err)
	}
}

func TestReader(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	big := strings.Repeat("0123456789", 1000)
	f.commitFiles("one", map[string]string{"a b.txt": "hello\n", "big.txt": big, "dir/x.go": "package x\n"})
	bigOID := f.git("rev-parse", "HEAD:big.txt")

	rd, err := f.repo.OpenReader(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	if got, err := rd.ReadFile("HEAD", "a b.txt"); err != nil || string(got) != "hello\n" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	for _, c := range [][2]string{{"HEAD", "missing"}, {"nope", "a b.txt"}} {
		if _, err := rd.ReadFile(c[0], c[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadFile(%s, %s) = %v, want ErrNotFound", c[0], c[1], err)
		}
	}
	if _, err := rd.ReadFile("HEAD", "dir"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile(tree) = %v, want a not-a-file error", err)
	}

	// Over the limit: size only, and the stream stays in step.
	data, size, ok, err := rd.ReadObject(bigOID, 100)
	if err != nil || ok || data != nil || size != int64(len(big)) {
		t.Fatalf("ReadObject(max 100) = %d bytes, %d, %v, %v", len(data), size, ok, err)
	}
	data, size, ok, err = rd.ReadObject("HEAD:dir/x.go", 100)
	if err != nil || !ok || string(data) != "package x\n" || size != 10 {
		t.Fatalf("ReadObject after a skipped object = %q, %d, %v, %v", data, size, ok, err)
	}
	data, _, ok, err = rd.ReadObject(bigOID, 0)
	if err != nil || !ok || string(data) != big {
		t.Fatalf("ReadObject(no limit) = %d bytes, %v, %v", len(data), ok, err)
	}
	if _, _, _, err := rd.ReadObject("HEAD:nope", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadObject(missing) = %v", err)
	}
	if _, _, _, err := rd.ReadObject("HEAD\nHEAD", 0); err == nil {
		t.Fatal("ReadObject accepted a newline")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := []string{"a b.txt", "dir/x.go"}[i%2]
			want := map[string]string{"a b.txt": "hello\n", "dir/x.go": "package x\n"}[path]
			if got, err := rd.ReadFile("HEAD", path); err != nil || string(got) != want {
				errs <- fmt.Errorf("concurrent ReadFile(%s) = %q, %v", path, got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := rd.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := rd.ReadFile("HEAD", "a b.txt"); err == nil {
		t.Fatal("ReadFile after Close succeeded")
	}
	var zero Reader
	if _, err := zero.ReadFile("HEAD", "a"); err == nil {
		t.Fatal("zero Reader read")
	}
}

func TestConfig(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.git("config", "gitlet.lease", "5m")
	f.git("config", "--add", "gitlet.snapshotInclude", "*.sql")
	f.git("config", "--add", "gitlet.snapshotInclude", "docs/*.md")
	f.git("config", "branch.my topic.remote", "origin")

	for key, want := range map[string]string{
		"gitlet.lease":           "5m",
		"gitlet.missing":         "",
		"gitlet.snapshotInclude": "docs/*.md", // last value wins
		"branch.my topic.remote": "origin",
	} {
		if got, err := f.repo.ConfigGet(bg, key); err != nil || got != want {
			t.Errorf("ConfigGet(%s) = %q, %v", key, got, err)
		}
	}
	if got, err := f.repo.ConfigGetAll(bg, "gitlet.snapshotInclude"); err != nil || !slices.Equal(got, []string{"*.sql", "docs/*.md"}) {
		t.Errorf("ConfigGetAll = %q, %v", got, err)
	}
	if got, err := f.repo.ConfigGetAll(bg, "gitlet.missing"); err != nil || got != nil {
		t.Errorf("ConfigGetAll(missing) = %q, %v", got, err)
	}
	for _, key := range []string{"nosection", "", "--list"} {
		if _, err := f.repo.ConfigGet(bg, key); err == nil {
			t.Errorf("ConfigGet(%q) succeeded", key)
		}
	}
}

func TestHooksDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got, err := f.repo.HooksDir(bg); err != nil || got != filepath.Join(f.repo.CommonDir, "hooks") {
		t.Errorf("HooksDir = %q, %v", got, err)
	}
	f.git("config", "core.hooksPath", ".githooks")
	if got, err := f.repo.HooksDir(bg); err != nil || got != filepath.Join(f.dir, ".githooks") {
		t.Errorf("HooksDir(relative hooksPath) = %q, %v", got, err)
	}
	abs := filepath.Join(t.TempDir(), "hooks")
	f.git("config", "core.hooksPath", abs)
	if got, err := f.repo.HooksDir(bg); err != nil || got != abs {
		t.Errorf("HooksDir(absolute hooksPath) = %q, %v", got, err)
	}

	// A linked worktree shares the common hooks.
	f.git("config", "--unset", "core.hooksPath")
	f.commit("one")
	wt := filepath.Join(t.TempDir(), "wt")
	f.git("worktree", "add", "-q", "-b", "wt", wt)
	w := openFixture(t, wt)
	if got, err := w.repo.HooksDir(bg); err != nil || got != filepath.Join(f.repo.CommonDir, "hooks") {
		t.Errorf("HooksDir(worktree) = %q, %v", got, err)
	}
}

func TestWorktrees(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	head := f.commit("one")
	tmp := t.TempDir()
	f.git("worktree", "add", "-q", "-b", "feature", filepath.Join(tmp, "feature"))
	f.git("worktree", "add", "-q", "--detach", filepath.Join(tmp, "detached"))

	got, err := f.repo.Worktrees(bg)
	if err != nil {
		t.Fatal(err)
	}
	// git lists the main work tree first, then linked ones by name.
	want := []Worktree{
		{Path: f.dir, HEAD: head, Branch: "main"},
		{Path: filepath.Join(tmp, "detached"), HEAD: head},
		{Path: filepath.Join(tmp, "feature"), HEAD: head, Branch: "feature"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Worktrees =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRemotesAndDefaultBranch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if b, err := f.repo.DefaultBranch(bg, "origin"); err != nil || b != "" {
		t.Errorf("DefaultBranch(empty repo) = %q, %v", b, err)
	}
	head := f.commit("one")
	if b, err := f.repo.DefaultBranch(bg, ""); err != nil || b != "main" {
		t.Errorf("DefaultBranch(no remote) = %q, %v", b, err)
	}

	f.git("remote", "add", "origin", "https://example.com/r.git")
	f.git("remote", "add", "fork", "git@example.com:me/r.git")
	if got, err := f.repo.Remotes(bg); err != nil || !slices.Equal(got, []string{"fork", "origin"}) {
		t.Errorf("Remotes = %q, %v", got, err)
	}
	if got, err := f.repo.RemoteURL(bg, "fork"); err != nil || got != "git@example.com:me/r.git" {
		t.Errorf("RemoteURL = %q, %v", got, err)
	}
	if _, err := f.repo.RemoteURL(bg, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoteURL(missing) = %v, want ErrNotFound", err)
	}

	// refs/remotes/origin/HEAD decides, without the network.
	f.git("update-ref", "refs/remotes/origin/trunk", head)
	f.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	if b, err := f.repo.DefaultBranch(bg, "origin"); err != nil || b != "trunk" {
		t.Errorf("DefaultBranch(origin/HEAD) = %q, %v", b, err)
	}

	g := newFixture(t)
	g.git("switch", "-q", "-c", "master")
	g.commit("one")
	if b, err := g.repo.DefaultBranch(bg, "origin"); err != nil || b != "master" {
		t.Errorf("DefaultBranch(master only) = %q, %v", b, err)
	}
	g.git("update-ref", "refs/remotes/origin/main", g.git("rev-parse", "HEAD"))
	if b, err := g.repo.DefaultBranch(bg, "origin"); err != nil || b != "main" {
		t.Errorf("DefaultBranch(origin/main) = %q, %v", b, err)
	}
}

func TestOperation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("base", map[string]string{"a.txt": "base\n"})
	f.git("switch", "-q", "-c", "side")
	side := f.commitFiles("side", map[string]string{"a.txt": "side\n"})
	f.git("switch", "-q", "main")
	f.commitFiles("main", map[string]string{"a.txt": "main\n"})

	check := func(want string) {
		t.Helper()
		if got, err := f.repo.Operation(bg); err != nil || got != want {
			t.Fatalf("Operation = %q, %v; want %q", got, err, want)
		}
	}
	check("")
	gitFails(t, f.dir, "merge", "side")
	check("merge")
	f.git("merge", "--abort")
	gitFails(t, f.dir, "cherry-pick", side)
	check("cherry-pick")
	f.git("cherry-pick", "--abort")
	gitFails(t, f.dir, "rebase", "side")
	check("rebase")
	f.git("rebase", "--abort")
	f.git("bisect", "start")
	check("bisect")
	f.git("bisect", "reset")
	check("")

	// Linked worktrees keep their state in their own git dir.
	wt := filepath.Join(t.TempDir(), "wt")
	f.git("worktree", "add", "-q", "--detach", wt, "main")
	w := openFixture(t, wt)
	gitFails(t, wt, "merge", "side")
	if got, err := w.repo.Operation(bg); err != nil || got != "merge" {
		t.Fatalf("worktree Operation = %q, %v", got, err)
	}
	check("")
	gitIn(t, wt, nil, "merge", "--abort")
	gitFails(t, wt, "cherry-pick", side)
	if got, err := w.repo.Operation(bg); err != nil || got != "cherry-pick" {
		t.Fatalf("worktree Operation = %q, %v", got, err)
	}
	gitIn(t, wt, nil, "cherry-pick", "--abort")
	w.commitFiles("wt", map[string]string{"a.txt": "wt\n"})
	gitFails(t, wt, "revert", "main")
	if got, err := w.repo.Operation(bg); err != nil || got != "revert" {
		t.Fatalf("worktree Operation = %q, %v", got, err)
	}
	gitIn(t, wt, nil, "revert", "--abort")
	gitFails(t, wt, "merge", "side")
	if _, err := os.Stat(filepath.Join(w.repo.GitDir, "MERGE_HEAD")); err != nil {
		t.Fatal(err)
	}
}
