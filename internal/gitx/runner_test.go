package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("one", map[string]string{"sub/dir/a.txt": "a\n"})

	r, err := Open(bg, filepath.Join(f.dir, "sub", "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != f.dir || r.GitDir != filepath.Join(f.dir, ".git") || r.CommonDir != r.GitDir {
		t.Fatalf("Open from a subdirectory = %+v", r)
	}
	if !filepath.IsAbs(r.Root) || !filepath.IsAbs(r.GitDir) || !filepath.IsAbs(r.CommonDir) {
		t.Fatalf("Open returned relative paths: %+v", r)
	}
	// A relative directory gives absolute paths too.
	rel, err := filepath.Rel(mustGetwd(t), filepath.Join(f.dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if r, err := Open(bg, rel); err != nil || r.Root != f.dir || !filepath.IsAbs(r.GitDir) {
		t.Fatalf("Open(%s) = %+v, %v", rel, r, err)
	}
	// Through a symlink, the paths are the real ones.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(f.dir, link); err != nil {
		t.Fatal(err)
	}
	if r, err := Open(bg, filepath.Join(link, "sub")); err != nil || r.Root != f.dir || r.GitDir != filepath.Join(f.dir, ".git") || r.CommonDir != r.GitDir {
		t.Fatalf("Open through a symlink = %+v, %v", r, err)
	}
	// A symlink into a subdirectory: git prints the common dir relative to
	// where it really runs ("../../.git"), not to the link.
	deep := filepath.Join(t.TempDir(), "deep")
	if err := os.Symlink(filepath.Join(f.dir, "sub", "dir"), deep); err != nil {
		t.Fatal(err)
	}
	if r, err = Open(bg, deep); err != nil {
		t.Fatal(err)
	}
	if r.Root != f.dir || r.GitDir != filepath.Join(f.dir, ".git") || r.CommonDir != r.GitDir {
		t.Fatalf("Open through a symlink into a subdirectory: Root %s, GitDir %s, CommonDir %s", r.Root, r.GitDir, r.CommonDir)
	}

	wt := filepath.Join(t.TempDir(), "wt")
	f.git("worktree", "add", "-q", "-b", "wt", wt)
	w, err := Open(bg, wt)
	if err != nil {
		t.Fatal(err)
	}
	wantGitDir := filepath.Join(f.dir, ".git", "worktrees", "wt")
	if !filepath.IsAbs(w.Root) || filepath.Base(w.Root) != "wt" || w.GitDir != wantGitDir || w.CommonDir != filepath.Join(f.dir, ".git") {
		t.Fatalf("Open in a linked worktree = %+v", w)
	}

	for name, dir := range map[string]string{
		"plain directory": t.TempDir(),
		"inside .git":     filepath.Join(f.dir, ".git"),
		"bare repository": newBare(t),
	} {
		if _, err := Open(bg, dir); !errors.Is(err, ErrNotRepo) {
			t.Errorf("Open(%s) = %v, want ErrNotRepo", name, err)
		}
	}
	if _, err := Open(bg, filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrNotRepo) {
		t.Errorf("Open(missing dir) = %v, want a plain error", err)
	}
}

// TestOpenOldGit pretends git is older than gitlet supports by seeding the
// per-process version cache. It must not run in parallel with other tests.
func TestOpenOldGit(t *testing.T) {
	f := newFixture(t)
	major, minor, err := f.repo.Version(bg)
	if err != nil {
		t.Fatal(err)
	}
	pretend := func(major, minor int) {
		gitVersion.Lock()
		gitVersion.known, gitVersion.major, gitVersion.minor = true, major, minor
		gitVersion.Unlock()
	}
	t.Cleanup(func() { pretend(major, minor) })

	pretend(OldestMajor, OldestMinor-1)
	_, err = Open(bg, f.dir)
	want := fmt.Sprintf("found %d.%d, need %d.%d or later", OldestMajor, OldestMinor-1, OldestMajor, OldestMinor)
	if !errors.Is(err, ErrGitTooOld) || errors.Is(err, ErrNotRepo) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Open with git %d.%d = %v, want ErrGitTooOld saying %q", OldestMajor, OldestMinor-1, err, want)
	}
	pretend(OldestMajor, OldestMinor)
	if _, err := Open(bg, f.dir); err != nil {
		t.Fatalf("Open with git %d.%d = %v", OldestMajor, OldestMinor, err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// envOf returns the environment git commands of r see, through a shell alias.
func envOf(t *testing.T, r *Repo) []string {
	t.Helper()
	out, err := r.Run(bg, "-c", "alias.envdump=!env", "envdump")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(out, "\n")
}

func TestEnvironment(t *testing.T) {
	f := newFixture(t)
	g := newFixture(t)
	g.git("config", "core.sshCommand", "ssh -i /some/key")
	// As inside a git hook: these point elsewhere and must not leak.
	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("GIT_INDEX_FILE", "/nonexistent/index")
	r, err := Open(bg, f.dir)
	if err != nil {
		t.Fatal(err)
	}

	env := envOf(t, r)
	for _, want := range []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %s", want)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_OPTIONAL_LOCKS=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			t.Errorf("foreground environment has %s", kv)
		}
	}
	if !slices.Contains(envOf(t, r.Background()), "GIT_OPTIONAL_LOCKS=0") {
		t.Error("Background environment lacks GIT_OPTIONAL_LOCKS=0")
	}
	if r.background {
		t.Error("Background changed the original Repo")
	}

	// A user who chose an ssh command keeps it.
	rs, err := Open(bg, g.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range envOf(t, rs) {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			t.Errorf("core.sshCommand set, but environment has %s", kv)
		}
	}
	t.Setenv("GIT_SSH", "/usr/bin/plink")
	if slices.Contains(baseEnv(false), "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Error("GIT_SSH set, but BatchMode ssh was forced")
	}
}

func TestRunStdin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Without input, stdin is empty: cat returns at once.
	out, err := f.repo.Run(bg, "-c", "alias.in=!cat", "in")
	if err != nil || out != "" {
		t.Fatalf("Run(cat) = %q, %v", out, err)
	}
	out, err = f.repo.RunInput(bg, []byte("hello\n\n"), "-c", "alias.in=!cat", "in")
	if err != nil || out != "hello\n" {
		t.Fatalf("RunInput(cat) = %q, %v; want one trailing newline removed", out, err)
	}
	out, err = f.repo.RunEnv(bg, []string{"GITX_PROBE=42"}, "-c", "alias.probe=!echo $GITX_PROBE", "probe")
	if err != nil || out != "42" {
		t.Fatalf("RunEnv = %q, %v", out, err)
	}
}

func TestRunError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.repo.Run(bg, "rev-parse", "--verify", "nope")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if e.ExitCode != 128 || !slices.Equal(e.Args, []string{"rev-parse", "--verify", "nope"}) || e.Stderr != "fatal: Needed a single revision" {
		t.Fatalf("Error = %+v", e)
	}
	if got := e.Error(); got != "git rev-parse --verify nope: exit 128: fatal: Needed a single revision" {
		t.Fatalf("Error() = %q", got)
	}

	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := f.repo.Run(ctx, "status"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
}

func TestRunTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.repo.Run(ctx, "-c", "alias.slow=!sleep 20", "slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.ExitCode != -1 {
		t.Fatalf("err = %v, want *Error with exit -1", err)
	}
	if d := time.Since(start); d > 3*time.Second { // SIGINT reaches the alias's sleep too
		t.Fatalf("stopping took %v", d)
	}
}

func TestClassification(t *testing.T) {
	t.Parallel()
	gitErr := func(sub, stderr string) error {
		return &Error{Args: []string{"-c", "x=y", sub, "origin"}, ExitCode: 128, Stderr: stderr}
	}
	tests := []struct {
		name             string
		err              error
		network, permits bool
	}{
		{"ssh unknown host", gitErr("fetch", "ssh: Could not resolve hostname nohost: Name or service not known\r\nfatal: Could not read from remote repository."), true, false},
		{"https unknown host", gitErr("fetch", "fatal: unable to access 'https://nohost/r.git/': Could not resolve host: nohost"), true, false},
		{"refused", gitErr("push", "fatal: unable to access 'http://127.0.0.1:1/r.git/': Failed to connect to 127.0.0.1 port 1: Connection refused"), true, false},
		{"connection timed out", gitErr("fetch", "ssh: connect to host example.com port 22: Connection timed out"), true, false},
		{"early eof", gitErr("fetch", "fatal: early EOF\nfatal: index-pack failed"), true, false},
		{"rpc failed", gitErr("push", "error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502"), true, false},
		{"hung up", gitErr("push", "fatal: the remote end hung up unexpectedly"), true, false},
		{"ssh key refused", gitErr("push", "git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository."), false, true},
		{"github 403", gitErr("push", "remote: Permission to o/r.git denied to u.\nfatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403"), false, true},
		{"auth failed", gitErr("fetch", "fatal: Authentication failed for 'https://example.com/r.git/'"), false, true},
		{"no prompt", gitErr("fetch", "fatal: could not read Username for 'https://github.com': terminal prompts disabled"), false, true},
		{"protected branch", gitErr("push", "remote: GitLab: You are not allowed to push code to protected branches on this project."), false, true},
		{"hook declined", gitErr("push", "! [remote rejected] main -> main (pre-receive hook declined)"), false, true},
		{"read-only", gitErr("push", "remote: ERROR: This repository is read-only."), false, true},
		{"fetch timed out", fmt.Errorf("gitx: %w: %w: %w", gitErr("fetch", ""), ErrTimeout, context.DeadlineExceeded), true, false},
		{"push timed out", fmt.Errorf("gitx: %w: %w: %w", gitErr("push", ""), ErrTimeout, context.DeadlineExceeded), true, false},
		{"push past the caller's deadline", fmt.Errorf("gitx: %w: %w", gitErr("push", ""), context.DeadlineExceeded), false, false},
		{"local read timed out", fmt.Errorf("gitx: %w: %w: %w", gitErr("status", ""), ErrTimeout, context.DeadlineExceeded), false, false},
		{"fetch cancelled", fmt.Errorf("gitx: %w: %w", gitErr("fetch", ""), context.Canceled), false, false},
		{"wrapped by a caller", fmt.Errorf("bus: write state: %w", gitErr("push", "fatal: unable to access 'https://h/r.git/': Could not resolve host: h")), true, false},
		{"wrapped permission", fmt.Errorf("bus: %w", fmt.Errorf("gitx: %w", gitErr("push", "remote: Permission to o/r.git denied to u."))), false, true},
		{"other failure", gitErr("fetch", "fatal: invalid refspec 'x'"), false, false},
		{"missing remote ref", gitErr("fetch", "fatal: couldn't find remote ref refs/gitlet/wip/a/i"), false, false},
		{"not a git error", errors.New("connection refused"), false, false},
		{"nil", nil, false, false},
	}
	for _, tt := range tests {
		if got := IsNetwork(tt.err); got != tt.network {
			t.Errorf("%s: IsNetwork = %v, want %v", tt.name, got, tt.network)
		}
		if got := IsPermission(tt.err); got != tt.permits {
			t.Errorf("%s: IsPermission = %v, want %v", tt.name, got, tt.permits)
		}
	}
}

func TestClassificationLive(t *testing.T) {
	for _, k := range []string{"http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	f := newFixture(t)
	for name, url := range map[string]string{
		"missing repository": filepath.Join(t.TempDir(), "missing.git"),
		"refused":            "http://127.0.0.1:1/r.git",
	} {
		err := f.repo.Fetch(bg, FetchOptions{Remote: url, Refspecs: []string{"refs/heads/*:refs/x/*"}, Metadata: true})
		if err == nil || !IsNetwork(err) || IsPermission(err) {
			t.Errorf("%s: fetch err = %v, want a network error", name, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"git version 2.43.0", 2, 43, true},
		{"git version 2.39.3 (Apple Git-146)\n", 2, 39, true},
		{"git version 2.45.1.windows.1", 2, 45, true},
		{"git version 2.44.0.rc1", 2, 44, true},
		{"git version 3.0", 3, 0, true},
		{"git version 2.38rc0", 2, 38, true},
		{"git version 2", 0, 0, false},
		{"git version x.y", 0, 0, false},
		{"hub version 2.14.2", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		major, minor, err := parseVersion(tt.in)
		if (err == nil) != tt.ok || major != tt.major || minor != tt.minor {
			t.Errorf("parseVersion(%q) = %d, %d, %v", tt.in, major, minor, err)
		}
	}

	f := newFixture(t)
	major, minor, err := f.repo.Version(bg)
	if err != nil || major < 2 {
		t.Fatalf("Version = %d.%d, %v", major, minor, err)
	}
	for _, c := range []struct {
		major, minor int
		want         bool
	}{{major, minor, true}, {major, minor + 1, false}, {major - 1, 99, true}, {major + 1, 0, false}} {
		if got, err := f.repo.AtLeast(bg, c.major, c.minor); err != nil || got != c.want {
			t.Errorf("AtLeast(%d, %d) = %v, %v", c.major, c.minor, got, err)
		}
	}
}

func TestSubcommand(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"push":  {"-c", "push.negotiate=false", "-c", "push.gpgSign=false", "push", "--porcelain"},
		"fetch": {"fetch", "origin"},
		"add":   {"-c", "core.splitIndex=false", "--literal-pathspecs", "add", "-u"},
		"":      {"-c", "x=y"},
	}
	for want, args := range tests {
		if got := subcommand(args); got != want {
			t.Errorf("subcommand(%q) = %q, want %q", args, got, want)
		}
	}
}

func TestSplitWriter(t *testing.T) {
	t.Parallel()
	var got []string
	w := &splitWriter{sep: 0, fn: func(field string) { got = append(got, field) }}
	for _, chunk := range []string{"a", "b\x00\x00c", "d\x00e"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if want := []string{"ab", "", "cd"}; !slices.Equal(got, want) {
		t.Fatalf("fields = %q, want %q (a field split across writes kept whole, the unterminated last one dropped)", got, want)
	}
}

func TestRepoWithoutOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commitFiles("one", map[string]string{"a.txt": "a\n"})
	r := &Repo{Root: f.dir}
	if _, err := r.RevParse(bg, "HEAD"); err != nil {
		t.Fatalf("zero Repo: %v", err)
	}
	if op, err := r.Operation(bg); err != nil || op != "" {
		t.Fatalf("zero Repo Operation = %q, %v", op, err)
	}
}
