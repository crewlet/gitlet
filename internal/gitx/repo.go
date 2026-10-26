package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func open(ctx context.Context, dir string) (*Repo, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("gitx: open %s: %w", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("gitx: open: %w", err)
	}
	if !fi.IsDir() {
		abs = filepath.Dir(abs)
	}

	probe := &Repo{Root: abs, env: baseEnv(true)}
	if err := probe.checkOldest(ctx); err != nil {
		return nil, err
	}
	// --show-toplevel and --absolute-git-dir print absolute paths;
	// --git-common-dir may be relative to the directory git ran in.
	out, err := probe.read(ctx, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		if s := stderrOf(err); strings.Contains(s, "not a git repository") || strings.Contains(s, "must be run in a work tree") {
			return nil, fmt.Errorf("gitx: %s: %w", abs, ErrNotRepo)
		}
		return nil, err
	}
	paths := strings.Split(out, "\n")
	if len(paths) != 3 || paths[0] == "" {
		return nil, fmt.Errorf("gitx: %s: %w", abs, ErrNotRepo)
	}
	// git runs in the directory abs resolves to, and ".." in its answer
	// climbs from there, not from a symlink abs may pass through.
	ran := abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		ran = real
	}
	r := &Repo{
		Root:      filepath.Clean(filepath.FromSlash(paths[0])),
		GitDir:    filepath.Clean(filepath.FromSlash(paths[1])),
		CommonDir: canonicalDir(ran, paths[2]),
	}

	sshConfigured := os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != ""
	if !sshConfigured {
		probe.Root = r.Root
		v, err := probe.configGet(ctx, "core.sshCommand")
		if err != nil {
			return nil, err
		}
		sshConfigured = v != ""
	}
	r.env = baseEnv(sshConfigured)
	return r, nil
}

// canonicalDir makes a directory git printed absolute, relative to dir, the
// directory git ran in, whose symlinks must already be resolved because a
// relative answer is joined as text. The result has its symlinks resolved
// as in git's own absolute paths, so the common dir of the main work tree
// equals its git dir.
func canonicalDir(dir, p string) string {
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// checkOldest refuses git older than OldestMajor.OldestMinor before any
// newer option can fail in a way that hides the cause. A version gitx
// cannot read is let through.
func (r *Repo) checkOldest(ctx context.Context) error {
	major, minor, err := r.version(ctx)
	if err != nil {
		if errors.Is(err, errBadVersion) {
			return nil
		}
		return err
	}
	if major < OldestMajor || major == OldestMajor && minor < OldestMinor {
		return fmt.Errorf("gitx: %w: found %d.%d, need %d.%d or later (%d.%d for conflict prediction and merge checks)",
			ErrGitTooOld, major, minor, OldestMajor, OldestMinor, MinMajor, MinMinor)
	}
	return nil
}

// The git binary does not change while gitlet runs, so its version is
// looked up once per process.
var gitVersion struct {
	sync.Mutex
	known        bool
	major, minor int
}

func (r *Repo) version(ctx context.Context) (int, int, error) {
	gitVersion.Lock()
	defer gitVersion.Unlock()
	if gitVersion.known {
		return gitVersion.major, gitVersion.minor, nil
	}
	out, err := r.read(ctx, "version")
	if err != nil {
		return 0, 0, err
	}
	major, minor, err := parseVersion(out)
	if err != nil {
		return 0, 0, err
	}
	gitVersion.known, gitVersion.major, gitVersion.minor = true, major, minor
	return major, minor, nil
}

var errBadVersion = errors.New("unrecognized git version")

// parseVersion reads "git version 2.43.0", "git version 2.39.3 (Apple
// Git-146)", "git version 2.45.1.windows.1" and the like.
func parseVersion(s string) (major, minor int, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "git version ")
	fields := strings.Fields(rest)
	if !ok || len(fields) == 0 {
		return 0, 0, fmt.Errorf("gitx: %w %q", errBadVersion, s)
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("gitx: %w %q", errBadVersion, s)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(leadingDigits(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("gitx: %w %q", errBadVersion, s)
	}
	return major, minor, nil
}

func leadingDigits(s string) string {
	for i, c := range s {
		if c < '0' || c > '9' {
			return s[:i]
		}
	}
	return s
}

func (r *Repo) revParse(ctx context.Context, rev string) (string, error) {
	if err := checkRev(rev); err != nil {
		return "", err
	}
	out, err := r.read(ctx, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		if exitCode(err) == 1 || isNotFound(err) {
			return "", notFound(rev)
		}
		return "", err
	}
	return out, nil
}

func (r *Repo) treeOf(ctx context.Context, commit string) (string, error) {
	if err := checkRev(commit); err != nil {
		return "", err
	}
	return r.revParse(ctx, commit+"^{tree}")
}

func (r *Repo) currentBranch(ctx context.Context) (string, error) {
	out, err := r.read(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if exitCode(err) == 1 {
			return "", nil // detached
		}
		return "", err
	}
	return strings.TrimPrefix(out, "refs/heads/"), nil
}

func (r *Repo) branchExists(ctx context.Context, branch string) (bool, error) {
	if err := checkName("branch", branch); err != nil {
		return false, err
	}
	_, err := r.read(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	}
	return false, err
}

func (r *Repo) remotes(ctx context.Context) ([]string, error) {
	out, err := r.read(ctx, "remote")
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

func (r *Repo) remoteURL(ctx context.Context, remote string) (string, error) {
	if err := checkName("remote", remote); err != nil {
		return "", err
	}
	out, err := r.read(ctx, "remote", "get-url", remote)
	if err != nil {
		if strings.Contains(stderrOf(err), "no such remote") {
			return "", notFound("remote " + remote)
		}
		return "", err
	}
	return out, nil
}

func (r *Repo) defaultBranch(ctx context.Context, remote string) (string, error) {
	var patterns []string
	if remote != "" {
		if err := checkName("remote", remote); err != nil {
			return "", err
		}
		prefix := "refs/remotes/" + remote + "/"
		out, err := r.read(ctx, "symbolic-ref", "--quiet", prefix+"HEAD")
		if err == nil {
			if b, ok := strings.CutPrefix(out, prefix); ok && b != "" {
				return b, nil
			}
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return "", err
		}
		patterns = append(patterns, prefix+"main", prefix+"master")
	}
	patterns = append(patterns, "refs/heads/main", "refs/heads/master")
	out, err := r.read(ctx, append([]string{"for-each-ref", "--format=%(refname)"}, patterns...)...)
	if err != nil {
		return "", err
	}
	have := map[string]bool{}
	for _, ref := range lines(out) {
		have[ref] = true
	}
	for _, b := range []string{"main", "master"} {
		if have["refs/heads/"+b] || (remote != "" && have["refs/remotes/"+remote+"/"+b]) {
			return b, nil
		}
	}
	return "", nil
}

func (r *Repo) mergeBase(ctx context.Context, a, b string) (string, error) {
	if err := errors.Join(checkRev(a), checkRev(b)); err != nil {
		return "", err
	}
	out, err := r.read(ctx, "merge-base", a, b)
	if err != nil {
		if exitCode(err) == 1 || isNotFound(err) {
			return "", notFound("merge base of " + a + " and " + b)
		}
		return "", err
	}
	return out, nil
}

func (r *Repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	if err := errors.Join(checkRev(a), checkRev(b)); err != nil {
		return false, err
	}
	_, err := r.read(ctx, "merge-base", "--is-ancestor", a, b)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	}
	return false, mapNotFound(err, a+" or "+b)
}

func (r *Repo) readFile(ctx context.Context, rev, path string) ([]byte, error) {
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	if path == "" || strings.ContainsAny(path, "\x00\n") {
		return nil, fmt.Errorf("gitx: invalid path %q", path)
	}
	spec := rev + ":" + path
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "cat-file", "blob", spec)
	if err != nil {
		return nil, mapNotFound(err, spec)
	}
	return out, nil
}

// checkConfigKey rejects keys git would read as options. Subsection names
// may contain spaces.
func checkConfigKey(key string) error {
	if key == "" || strings.HasPrefix(key, "-") || strings.ContainsAny(key, "\x00\n") {
		return fmt.Errorf("gitx: invalid config key %q", key)
	}
	return nil
}

func (r *Repo) configGet(ctx context.Context, key string) (string, error) {
	if err := checkConfigKey(key); err != nil {
		return "", err
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "config", "-z", "--get", key)
	if err != nil {
		if exitCode(err) == 1 && stderrOf(err) == "" {
			return "", nil // unset
		}
		return "", err
	}
	return strings.TrimSuffix(string(out), "\x00"), nil
}

func (r *Repo) configGetAll(ctx context.Context, key string) ([]string, error) {
	if err := checkConfigKey(key); err != nil {
		return nil, err
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, "config", "-z", "--get-all", key)
	if err != nil {
		if exitCode(err) == 1 && stderrOf(err) == "" {
			return nil, nil // unset
		}
		return nil, err
	}
	return splitNUL(out), nil
}

func (r *Repo) hooksDir(ctx context.Context) (string, error) {
	out, err := r.read(ctx, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	return r.abs(out), nil
}

// abs makes a path printed by git absolute, relative to the work tree.
func (r *Repo) abs(p string) string {
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Root, p)
	}
	return filepath.Clean(p)
}

func (r *Repo) worktrees(ctx context.Context) ([]Worktree, error) {
	// -z (git 2.36) keeps every path intact; older git ends each field with
	// a newline instead.
	args, sep := []string{"worktree", "list", "--porcelain", "-z"}, "\x00"
	if ok, err := r.AtLeast(ctx, 2, 36); err == nil && !ok {
		args, sep = args[:3], "\n"
	}
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, args...)
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(out), sep), nil
}

// parseWorktrees reads git worktree list --porcelain output whose fields
// end with sep. Bare repositories are left out.
func parseWorktrees(out, sep string) []Worktree {
	var (
		list []Worktree
		cur  *Worktree
		bare bool
	)
	flush := func() {
		if cur != nil && !bare {
			list = append(list, *cur)
		}
		cur, bare = nil, false
	}
	for _, field := range strings.Split(out, sep) {
		key, val, _ := strings.Cut(field, " ")
		switch key {
		case "":
			flush() // an empty field ends a record
		case "worktree":
			flush()
			cur = &Worktree{Path: filepath.Clean(filepath.FromSlash(val))}
		case "HEAD":
			if cur != nil {
				cur.HEAD = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "bare":
			bare = true
		}
	}
	flush()
	return list
}

// gitDir returns the work tree's git dir, asking git when the Repo was not
// built by Open.
func (r *Repo) gitDir(ctx context.Context) (string, error) {
	if r.GitDir != "" {
		return r.GitDir, nil
	}
	out, err := r.read(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.FromSlash(out)), nil
}

func (r *Repo) operation(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("gitx: operation: %w", err)
	}
	dir, err := r.gitDir(ctx)
	if err != nil {
		return "", err
	}
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case exists("rebase-merge"), exists("rebase-apply"): // rebase-apply also covers git am
		return "rebase", nil
	case exists("MERGE_HEAD"):
		return "merge", nil
	case exists("CHERRY_PICK_HEAD"):
		return "cherry-pick", nil
	case exists("REVERT_HEAD"):
		return "revert", nil
	case exists("sequencer"):
		// A multi-commit pick or revert between steps.
		todo, _ := os.ReadFile(filepath.Join(dir, "sequencer", "todo"))
		if first, _, _ := strings.Cut(strings.TrimSpace(string(todo)), " "); first == "revert" {
			return "revert", nil
		}
		return "cherry-pick", nil
	case exists("BISECT_LOG"):
		return "bisect", nil
	}
	return "", nil
}
