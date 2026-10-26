package gitx

// This file holds the command runner every other file goes through, and the
// classification of git's failures (docs/SPEC.md, section 14.1).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Timeouts (docs/SPEC.md, section 14.1). A caller's earlier deadline wins.
const (
	timeoutRead     = 5 * time.Second   // local reads
	timeoutLocal    = 30 * time.Second  // Run and local writes (objects, refs, temporary indexes)
	timeoutFetch    = 15 * time.Second  // fetch and ls-remote
	timeoutPush     = 60 * time.Second  // push
	timeoutWorktree = 120 * time.Second // rebase, cherry-pick, revert, switch
	timeoutCleanup  = 30 * time.Second  // aborting a work-tree command that failed
	waitDelay       = 5 * time.Second   // from SIGINT to SIGKILL, and for pipes after exit
	maxStderr       = 1 << 20
)

func joinArgs(a []string) string { return strings.Join(a, " ") }
func itoa(i int) string          { return strconv.Itoa(i) }

// strippedEnv lists inherited variables that would point git at another
// repository, index or pathspec mode than the one Open found. Git sets some
// of them for hooks, and gitlet's git hooks open repositories of their own.
var strippedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE",
	"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
}

// baseEnv is the environment every git call runs with. sshConfigured says
// whether the user chose an ssh command through core.sshCommand; gitlet then
// leaves ssh alone, as it does when GIT_SSH_COMMAND or GIT_SSH is set.
func baseEnv(sshConfigured bool) []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+4)
	for _, kv := range inherited {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(strippedEnv, k) {
			env = append(env, kv)
		}
	}
	env = append(env, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	if !sshConfigured && os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// environ returns the environment for one call. A Repo built without Open
// still gets the fixed variables, but its ssh settings are left alone
// because core.sshCommand was never checked.
func (r *Repo) environ(extra []string) []string {
	base := r.env
	if base == nil {
		base = baseEnv(true)
	}
	env := make([]string, 0, len(base)+len(extra)+1)
	env = append(env, base...)
	if r.background {
		env = append(env, "GIT_OPTIONAL_LOCKS=0")
	}
	return append(env, extra...)
}

// cmdOpts controls one git invocation.
type cmdOpts struct {
	stdin   []byte        // nil reads from the null device
	stdout  io.Writer     // receives stdout as it comes; nil returns it whole
	env     []string      // extra KEY=value pairs, overriding the base environment
	timeout time.Duration // 0 means timeoutLocal
	// detached runs the command even after ctx is cancelled, bounded only by
	// timeout, for commands that change the work tree and must not be
	// interrupted halfway.
	detached bool
}

func (r *Repo) run(ctx context.Context, stdin []byte, env []string, args ...string) (string, error) {
	out, err := r.exec(ctx, cmdOpts{stdin: stdin, env: env}, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// read runs a local read and returns stdout without its trailing newline.
func (r *Repo) read(ctx context.Context, args ...string) (string, error) {
	out, err := r.exec(ctx, cmdOpts{timeout: timeoutRead}, args...)
	return strings.TrimSuffix(string(out), "\n"), err
}

// exec runs git and returns stdout, which is also returned on failure.
func (r *Repo) exec(ctx context.Context, o cmdOpts, args ...string) ([]byte, error) {
	stdout, _, err := r.execFull(ctx, o, args...)
	return stdout, err
}

// execFull runs git with args in the work tree. A non-zero exit returns an
// *Error. A command stopped by the caller's ctx returns an error that wraps
// both the *Error and ctx's error; one stopped by its timeout wraps the
// *Error, ErrTimeout and context.DeadlineExceeded.
func (r *Repo) execFull(ctx context.Context, o cmdOpts, args ...string) (stdout, stderr []byte, err error) {
	if o.detached {
		ctx = context.WithoutCancel(ctx)
	} else if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("gitx: git %s: %w", subcommand(args), err)
	}
	timeout := o.timeout
	if timeout <= 0 {
		timeout = timeoutLocal
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var out bytes.Buffer
	errBuf := &limitedBuffer{max: maxStderr}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Root
	cmd.Env = r.environ(o.env)
	if o.stdin != nil {
		cmd.Stdin = bytes.NewReader(o.stdin)
	}
	cmd.Stdout = &out
	if o.stdout != nil {
		cmd.Stdout = o.stdout
	}
	cmd.Stderr = errBuf
	stopped := supervise(cmd)

	runErr := cmd.Run()
	stopped()
	if runErr == nil {
		return out.Bytes(), errBuf.Bytes(), nil
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) && ctx.Err() == nil {
		return out.Bytes(), errBuf.Bytes(), fmt.Errorf("gitx: git %s: %w", subcommand(args), runErr)
	}
	e := &Error{Args: args, ExitCode: -1, Stderr: strings.TrimSpace(errBuf.String())}
	if exitErr != nil {
		e.ExitCode = exitErr.ExitCode()
	}
	if cerr := ctx.Err(); cerr != nil {
		// Stopped by SIGINT/SIGKILL: the exit code says nothing about git's view.
		e.ExitCode = -1
		if perr := parent.Err(); perr != nil {
			// The caller's deadline or cancellation: its budget ran out,
			// which says nothing about the remote.
			return out.Bytes(), errBuf.Bytes(), fmt.Errorf("gitx: %w: %w", e, perr)
		}
		return out.Bytes(), errBuf.Bytes(), fmt.Errorf("gitx: %w: %w after %s: %w", e, ErrTimeout, timeout, cerr)
	}
	return out.Bytes(), errBuf.Bytes(), e
}

// supervise sets how cmd stops when its context ends: SIGINT to git and
// everything it started, so git removes its lock files, then SIGKILL to git
// after waitDelay. The returned function must be called after cmd exits; it
// kills what is left of a stopped command's process group.
func supervise(cmd *exec.Cmd) (exited func()) {
	setProcessGroup(cmd)
	var cancelled atomic.Bool
	cmd.Cancel = func() error {
		cancelled.Store(true)
		return interruptGroup(cmd)
	}
	cmd.WaitDelay = waitDelay
	return func() {
		if cancelled.Load() {
			killGroup(cmd)
		}
	}
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
// It may be read while a process is still writing to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// subcommand returns git's subcommand in args, skipping global options.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "-C":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

// exitCode returns the exit code of a failed git command, or -1 when err is
// not an *Error or git was stopped.
func exitCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.ExitCode
	}
	return -1
}

// stderrOf returns the lower-cased stderr of a failed git command.
func stderrOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return strings.ToLower(e.Stderr)
	}
	return ""
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var permissionPatterns = []string{
	"permission denied",
	"permission to ", // GitHub: "Permission to o/r.git denied to u."
	"access denied",
	"returned error: 401",
	"returned error: 403",
	"http 403",
	"403 forbidden",
	"authentication failed",
	"could not read username",
	"could not read password",
	"invalid username or password",
	"terminal prompts disabled",
	"protected branch",
	"pre-receive hook declined",
	"not allowed to push",
	"you are not allowed",
	"you need the git", // Azure DevOps: "You need the Git 'GenericContribute' permission"
	"write access to repository not granted",
	"is read-only", // "This repository is read-only", "archived so it is read-only"
	"read-only mode",
}

var networkPatterns = []string{
	"could not resolve host", // also "could not resolve hostname"
	"temporary failure in name resolution",
	"name or service not known",
	"nodename nor servname",
	"connection refused",
	"connection reset",
	"connection closed",
	"timed out",
	"network is unreachable",
	"no route to host",
	"failed to connect",
	"couldn't connect",
	"unable to access",
	"early eof",
	"rpc failed",
	"the remote end hung up unexpectedly",
	"could not read from remote repository",
	"broken pipe",
}

// isPermission reports whether a failed git command was refused by the remote.
func isPermission(err error) bool {
	return containsAny(stderrOf(err), permissionPatterns)
}

// isNetwork reports whether a failed git command could not talk to the
// remote. Permission failures, which often share git's generic "could not
// read from remote repository" line, are not network failures, and neither
// is a command the caller's own deadline cut short: only gitx's timeout,
// which gives the remote ample time, says the remote is not answering.
func isNetwork(err error) bool {
	var e *Error
	if !errors.As(err, &e) || isPermission(err) {
		return false
	}
	if errors.Is(err, ErrTimeout) {
		switch subcommand(e.Args) {
		case "fetch", "push", "ls-remote", "pull", "clone":
			return true
		}
	}
	return containsAny(strings.ToLower(e.Stderr), networkPatterns)
}

var notFoundPatterns = []string{
	"unknown revision",
	"bad revision",
	"bad object",
	"not a valid object name",
	"invalid object name",
	"not a valid commit name",
	"does not exist in",
	"exists on disk, but not in",
	"no such path",
	"no such ref",
	"needed a single revision",
	"unable to resolve revision",
	"not something we can merge",
	"does not have any commits yet",
}

// isNotFound reports whether a failed git command named a missing revision,
// ref or path.
func isNotFound(err error) bool {
	return containsAny(stderrOf(err), notFoundPatterns)
}

func notFound(what string) error { return fmt.Errorf("gitx: %s: %w", what, ErrNotFound) }

// mapNotFound turns a git failure about a missing object into ErrNotFound.
// The *Error stays in the chain for callers that want git's own words.
func mapNotFound(err error, what string) error {
	if err != nil && isNotFound(err) {
		return fmt.Errorf("gitx: %s: %w (%w)", what, ErrNotFound, err)
	}
	return err
}

// checkRev rejects revisions git would read as options or that cannot be
// passed safely.
func checkRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n") {
		return fmt.Errorf("gitx: invalid revision %q", rev)
	}
	return nil
}

// checkName rejects branch and ref names git would read as options.
func checkName(kind, name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\n\r\t ") {
		return fmt.Errorf("gitx: invalid %s %q", kind, name)
	}
	return nil
}

// checkRemote accepts a remote name, URL or path, which may hold spaces.
func checkRemote(remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\n\r") {
		return fmt.Errorf("gitx: invalid remote %q", remote)
	}
	return nil
}

// splitNUL splits NUL-terminated output, dropping the final empty field.
func splitNUL(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// splitWriter hands each sep-terminated field written to it to fn, for
// output too long to hold whole (cmdOpts.stdout). An unterminated last
// field is dropped.
type splitWriter struct {
	sep     byte
	fn      func(field string)
	partial []byte
}

func (w *splitWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, w.sep)
		if i < 0 {
			w.partial = append(w.partial, p...)
			return n, nil
		}
		w.partial = append(w.partial, p[:i]...)
		w.fn(string(w.partial))
		w.partial, p = w.partial[:0], p[i+1:]
	}
}

// lines splits newline-separated output, dropping empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSuffix(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}
