package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain makes every git command hermetic: a private global config
// (identity, main as the default branch, no signing) and no system config,
// so the host's settings never leak into the tests.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "gitx-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	err = os.WriteFile(cfg, []byte(`[user]
	name = Gitx Test
	email = gitx@example.com
[init]
	defaultBranch = main
[commit]
	gpgsign = false
[tag]
	gpgsign = false
[advice]
	detachedHead = false
`), 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE", "EMAIL",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_SSH_COMMAND", "GIT_SSH",
	} {
		os.Unsetenv(k)
	}
	return m.Run()
}

var bg = context.Background()

// fixture is a work tree with an opened Repo.
type fixture struct {
	t    *testing.T
	dir  string
	repo *Repo
}

// realTempDir is t.TempDir with symlinks resolved, as git reports paths
// (on macOS the temporary directory lives under the /var symlink).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// newFixture creates an empty repository on main.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, nil, "init", "-q")
	return openFixture(t, dir)
}

func openFixture(t *testing.T, dir string) *fixture {
	t.Helper()
	r, err := Open(bg, dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return &fixture{t: t, dir: r.Root, repo: r}
}

// gitIn runs git in dir with extra env and returns trimmed stdout.
func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitRaw(t, dir, env, args...))
}

// gitRaw runs git in dir with extra env and returns stdout as is.
func gitRaw(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// gitFails runs git in dir and fails the test if it succeeds.
func gitFails(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git %s succeeded unexpectedly:\n%s", strings.Join(args, " "), out)
	}
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	return gitIn(f.t, f.dir, nil, args...)
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(path)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// commit stages everything and commits it, returning the new HEAD.
func (f *fixture) commit(msg string) string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

// commitFiles writes files and commits them.
func (f *fixture) commitFiles(msg string, files map[string]string) string {
	f.t.Helper()
	for p, c := range files {
		f.write(p, c)
	}
	return f.commit(msg)
}

// newBare creates a bare repository to act as the remote.
func newBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, filepath.Dir(dir), nil, "init", "-q", "--bare", dir)
	return dir
}

// treeFiles maps each path in a tree to its content.
func (f *fixture) treeFiles(tree string) map[string]string {
	f.t.Helper()
	files := map[string]string{}
	out := gitRaw(f.t, f.dir, nil, "ls-tree", "-r", "-z", "--name-only", tree)
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			files[p] = gitRaw(f.t, f.dir, nil, "cat-file", "blob", tree+":"+p)
		}
	}
	return files
}

// fileSHA hashes a file, to prove it did not change.
func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
