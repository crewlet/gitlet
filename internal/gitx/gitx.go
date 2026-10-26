// Package gitx is gitlet's only way to talk to git. It runs the git binary
// (2.29 or later, 2.38 for merge checks) with a context, a fixed
// environment and argument vectors, never through a shell, and turns its
// output into typed values. docs/SPEC.md, section 14.1 lists the
// environment, flags and timeouts every call uses.
package gitx

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ZeroOID is git's all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

// IsZeroOID reports whether oid is the all-zero id of either hash, as git
// blame shows for lines not committed yet.
func IsZeroOID(oid string) bool { return oid != "" && strings.Trim(oid, "0") == "" }

// InternalEnv is set to 1 in the environment of the rebases, cherry-picks
// and reverts gitx runs, so gitlet's own commit hooks leave the commits they
// replay alone.
const InternalEnv = "GITLET_INTERNAL"

// MinMajor and MinMinor are the git version merge checks need.
const (
	MinMajor = 2
	MinMinor = 38
)

// OldestMajor and OldestMinor are the oldest git gitlet runs on at all:
// metadata fetches pass --no-write-fetch-head and --no-auto-maintenance,
// which git 2.29 introduced. Open refuses older versions with ErrGitTooOld.
const (
	OldestMajor = 2
	OldestMinor = 29
)

// ErrNotRepo is returned by Open outside a git work tree.
var ErrNotRepo = errors.New("not inside a git work tree")

// ErrGitTooOld is returned by Open when git is older than
// OldestMajor.OldestMinor.
var ErrGitTooOld = errors.New("git is too old for gitlet")

// ErrNotFound is returned when a ref, revision or path does not exist.
var ErrNotFound = errors.New("not found")

// ErrBusy is returned when another rebase, merge, cherry-pick or revert is in
// progress.
var ErrBusy = errors.New("another git operation is in progress")

// ErrTimeout marks a git command stopped by gitx's own timeout (15 s for a
// fetch, 60 s for a push, and so on) rather than by the caller's context.
// Such an error also matches context.DeadlineExceeded; one stopped by the
// caller's deadline or cancellation matches only the context's error.
var ErrTimeout = errors.New("git took longer than gitx allows")

// Error is a failed git invocation.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	return "git " + joinArgs(e.Args) + ": exit " + itoa(e.ExitCode) + ": " + e.Stderr
}

// IsNetwork reports whether err looks like a transport failure (could not
// resolve host, connection refused or reset, timeout talking to the remote).
func IsNetwork(err error) bool { return isNetwork(err) }

// IsPermission reports whether err looks like the remote refusing access
// (permission denied, 403, authentication failed, read-only).
func IsPermission(err error) bool { return isPermission(err) }

// Identity is an author and committer. A zero Identity means "whatever git
// itself would use" (GIT_AUTHOR_*/GIT_COMMITTER_* env vars, then user.name
// and user.email), falling back to FallbackIdentity only when git has none.
type Identity struct {
	Name  string
	Email string
	When  time.Time // zero means now
}

// FallbackIdentity is used for gitlet's own commits when git has no identity.
var FallbackIdentity = Identity{Name: "gitlet", Email: "gitlet@localhost"}

// Repo is one work tree of a repository.
type Repo struct {
	Root      string // top level of the work tree
	GitDir    string // this work tree's git dir
	CommonDir string // shared git dir (same as GitDir outside linked worktrees)

	background bool     // set GIT_OPTIONAL_LOCKS=0
	env        []string // computed once by Open: LC_ALL=C, GIT_TERMINAL_PROMPT=0, ...
}

// Open finds the repository containing dir.
func Open(ctx context.Context, dir string) (*Repo, error) { return open(ctx, dir) }

// Background returns a copy of r whose commands never take optional locks
// (GIT_OPTIONAL_LOCKS=0), for work the agent did not ask for.
func (r *Repo) Background() *Repo {
	c := *r
	c.background = true
	return &c
}

// Run runs git with args in the work tree and returns stdout with the
// trailing newline removed. Local commands time out after 30 s unless ctx
// has an earlier deadline.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, nil, nil, args...)
}

// RunInput is Run with stdin.
func (r *Repo) RunInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return r.run(ctx, stdin, nil, args...)
}

// RunEnv is Run with extra environment variables ("KEY=value").
func (r *Repo) RunEnv(ctx context.Context, env []string, args ...string) (string, error) {
	return r.run(ctx, nil, env, args...)
}

// Version returns git's version as major, minor.
func (r *Repo) Version(ctx context.Context) (major, minor int, err error) { return r.version(ctx) }

// AtLeast reports whether git is at least major.minor.
func (r *Repo) AtLeast(ctx context.Context, major, minor int) (bool, error) {
	ma, mi, err := r.version(ctx)
	if err != nil {
		return false, err
	}
	return ma > major || (ma == major && mi >= minor), nil
}

// RevParse resolves rev to a full object id (rev^{commit} is not implied).
// Missing revisions return ErrNotFound.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) { return r.revParse(ctx, rev) }

// CurrentBranch returns the short name of the checked-out branch, or "" when detached.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) { return r.currentBranch(ctx) }

// BranchExists reports whether refs/heads/<branch> exists.
func (r *Repo) BranchExists(ctx context.Context, branch string) (bool, error) {
	return r.branchExists(ctx, branch)
}

// Remotes lists configured remote names.
func (r *Repo) Remotes(ctx context.Context) ([]string, error) { return r.remotes(ctx) }

// RemoteURL returns the fetch URL of a remote.
func (r *Repo) RemoteURL(ctx context.Context, remote string) (string, error) {
	return r.remoteURL(ctx, remote)
}

// DefaultBranch returns the remote's default branch without touching the
// network: the target of refs/remotes/<remote>/HEAD, else "main" or "master"
// when either exists locally or as a remote-tracking branch, else "".
func (r *Repo) DefaultBranch(ctx context.Context, remote string) (string, error) {
	return r.defaultBranch(ctx, remote)
}

// MergeBase returns the best common ancestor of a and b (ErrNotFound when none).
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	return r.mergeBase(ctx, a, b)
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	return r.isAncestor(ctx, a, b)
}

// ReadFile returns the content of path at rev. Missing paths return ErrNotFound.
// For many reads, use OpenReader.
func (r *Repo) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	return r.readFile(ctx, rev, path)
}

// Reader reads many objects through one long-running git cat-file --batch
// process. It is not safe for concurrent use; Close it when done.
type Reader struct{ readerImpl }

// OpenReader starts a cat-file --batch process for the repository.
func (r *Repo) OpenReader(ctx context.Context) (*Reader, error) { return r.openReader(ctx) }

// ReadFile returns the content of path at rev ("rev:path"), or ErrNotFound.
func (rd *Reader) ReadFile(rev, path string) ([]byte, error) { return rd.readFile(rev, path) }

// ReadObject returns the content of an object by id or revision expression,
// with its size, or ErrNotFound. Objects larger than max bytes (max > 0) are
// not returned; ok is false and size is still set.
func (rd *Reader) ReadObject(spec string, max int64) (data []byte, size int64, ok bool, err error) {
	return rd.readObject(spec, max)
}

// Close stops the cat-file process.
func (rd *Reader) Close() error { return rd.close() }

// DiffEntry is one line of git diff --name-status.
type DiffEntry struct {
	Status  string // A, M, D, R, C, T (score stripped)
	Path    string
	OldPath string // for R and C
}

// DiffNameStatus lists files changed between two commits, with rename
// detection (-M), as NUL-separated output so any path is safe.
func (r *Repo) DiffNameStatus(ctx context.Context, from, to string) ([]DiffEntry, error) {
	return r.diffNameStatus(ctx, from, to)
}

// DiffNames lists the paths changed between two commits (both sides of renames).
func (r *Repo) DiffNames(ctx context.Context, from, to string) ([]string, error) {
	return r.diffNames(ctx, from, to)
}

// StatusEntry is one entry of git status --porcelain=v2.
type StatusEntry struct {
	XY        string // two-letter status, "??" for untracked
	Path      string
	OrigPath  string
	Untracked bool
}

// Status lists changed and untracked files (ignored files excluded).
func (r *Repo) Status(ctx context.Context) ([]StatusEntry, error) { return r.status(ctx) }

// IsClean reports whether the work tree has no changes (untracked included).
func (r *Repo) IsClean(ctx context.Context) (bool, error) { return r.isClean(ctx) }

// WorktreeFingerprint returns a cheap hash of the work tree's changed state:
// HEAD, status entries, and the size and modification time of each changed
// or untracked file. Used to decide whether a new snapshot is needed.
func (r *Repo) WorktreeFingerprint(ctx context.Context) (string, error) {
	return r.worktreeFingerprint(ctx)
}

// UntrackedPolicy says which untracked files a snapshot includes.
type UntrackedPolicy string

const (
	UntrackedSource UntrackedPolicy = "source" // files IsSource accepts, plus Include globs
	UntrackedAll    UntrackedPolicy = "all"
	UntrackedNone   UntrackedPolicy = "none"
)

// SnapshotOptions controls Snapshot.
type SnapshotOptions struct {
	Parent      string          // commit the snapshot builds on, usually HEAD
	Message     string          // commit message, e.g. "gitlet: snapshot"
	Untracked   UntrackedPolicy // default UntrackedSource
	Include     []string        // extra globs for untracked files (path.Match on the slash path and on the base name)
	IsSource    func(path string) bool
	MaxFileSize int64 // files larger than this are skipped, tracked or not; 0 means no limit
}

// SnapshotResult describes a snapshot commit.
type SnapshotResult struct {
	Commit  string
	Tree    string
	Skipped []string // paths left out because they are denied or too large
}

// Snapshot records the work tree as a commit without touching the real
// index, HEAD or any branch (docs/SPEC.md, section 9.1). It copies the real
// index to a temporary index file, stages tracked changes and the untracked
// files the policy allows into it, writes the tree and commits it with
// Parent as parent. Files on the deny list (IsDenied) and files larger than
// MaxFileSize keep their Parent version (or are left out when new).
func (r *Repo) Snapshot(ctx context.Context, opt SnapshotOptions) (SnapshotResult, error) {
	return r.snapshot(ctx, opt)
}

// IsDenied reports whether a path is on the built-in snapshot deny list:
// .env*, *.pem, *.key, *.p12, *.pfx, id_rsa*, id_ed25519*, id_ecdsa*,
// *.keystore, credentials*, secrets.* (matched on the base name).
func IsDenied(path string) bool { return isDenied(path) }

// PushExcluded lists the files a push of commit would publish although a
// snapshot never carries them (docs/SPEC.md, section 9.1): denied files
// and files larger than maxSize (0 means no limit) among the blobs commit
// reaches and the refs matching published do not. published holds
// for-each-ref patterns ("refs/remotes/origin/") for the refs the remote is
// known to have, so these are the blobs the push would send. Each is
// checked at every path a commit the push would send holds it at, so a
// denied file renamed or copied under an allowed name is still found. A
// snapshot leaves such files out of what it changes, so the ones found come
// from commits that are not pushed yet. Paths are sorted and listed once.
func (r *Repo) PushExcluded(ctx context.Context, commit string, published []string, maxSize int64) ([]string, error) {
	return r.pushExcluded(ctx, commit, published, maxSize)
}

// TreeOf returns the tree id of a commit.
func (r *Repo) TreeOf(ctx context.Context, commit string) (string, error) {
	return r.treeOf(ctx, commit)
}

// HashBlob writes data as a blob and returns its id.
func (r *Repo) HashBlob(ctx context.Context, data []byte) (string, error) {
	return r.hashBlob(ctx, data)
}

// TreeEntry is one entry for MakeTree. Mode "100644" for files.
type TreeEntry struct {
	Mode string
	Type string // "blob" or "tree"
	OID  string
	Name string
}

// MakeTree writes a tree object from entries.
func (r *Repo) MakeTree(ctx context.Context, entries []TreeEntry) (string, error) {
	return r.makeTree(ctx, entries)
}

// CommitTree creates a commit object for tree with the given parents, never
// signed (-c commit.gpgSign=false). A zero id uses git's own identity, with
// FallbackIdentity when git has none.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string, id Identity) (string, error) {
	return r.commitTree(ctx, tree, parents, message, id)
}

// CommitFile creates a commit on top of base whose tree is base's tree with
// path set to content (added when missing). It uses a temporary index and
// never touches the work tree. Used to build interface stubs.
func (r *Repo) CommitFile(ctx context.Context, base, path string, content []byte, message string, id Identity) (string, error) {
	return r.commitFile(ctx, base, path, content, message, id)
}

// UpdateRef sets ref to newOID if its current value is oldOID. oldOID ""
// means any current value; ZeroOID means the ref must not exist, in SHA-256
// repositories too. A failed check returns an *Error whose Stderr mentions
// the mismatch.
func (r *Repo) UpdateRef(ctx context.Context, ref, newOID, oldOID string) error {
	return r.updateRef(ctx, ref, newOID, oldOID)
}

// DeleteRef deletes ref if its current value is oldOID ("" means any).
func (r *Repo) DeleteRef(ctx context.Context, ref, oldOID string) error {
	return r.deleteRef(ctx, ref, oldOID)
}

// ListRefs returns ref name to object id for refs under prefix (for example
// "refs/gitlet/"). A missing namespace returns an empty map.
func (r *Repo) ListRefs(ctx context.Context, prefix string) (map[string]string, error) {
	return r.listRefs(ctx, prefix)
}

// RefInfo is a ref with its commit time.
type RefInfo struct {
	Name          string
	OID           string
	CommitterDate time.Time
}

// ListRefsByDate returns refs under prefix that point at commits, newest
// committer date first, at most max (0 means all).
func (r *Repo) ListRefsByDate(ctx context.Context, prefix string, max int) ([]RefInfo, error) {
	return r.listRefsByDate(ctx, prefix, max)
}

// LsRemote returns ref name to object id on a remote for the given patterns.
func (r *Repo) LsRemote(ctx context.Context, remote string, patterns ...string) (map[string]string, error) {
	return r.lsRemote(ctx, remote, patterns...)
}

// FetchOptions controls Fetch.
type FetchOptions struct {
	Remote   string
	Refspecs []string
	Prune    bool
	// Metadata adds --no-tags --no-recurse-submodules --no-write-fetch-head
	// --no-show-forced-updates --no-auto-maintenance -c fetch.pruneTags=false.
	Metadata bool
	Timeout  time.Duration // default 15 s
}

// Fetch fetches refspecs from a remote.
func (r *Repo) Fetch(ctx context.Context, opt FetchOptions) error { return r.fetch(ctx, opt) }

// Expect says what a push expects the remote ref to be.
type Expect int

const (
	// ExpectFastForward is a plain push: accepted when it fast-forwards.
	ExpectFastForward Expect = iota
	// ExpectAbsent requires that the remote ref does not exist (create only).
	ExpectAbsent
	// ExpectOID requires the remote ref to equal PushUpdate.OldOID.
	ExpectOID
	// Force overwrites unconditionally. gitlet never uses it for shared refs.
	Force
)

// PushUpdate is one ref update in a push. An empty Src deletes Dst.
type PushUpdate struct {
	Src    string
	Dst    string
	Expect Expect
	OldOID string // for ExpectOID
}

// RejectKind classifies a rejected ref.
type RejectKind string

const (
	RejectNone   RejectKind = ""
	RejectStale  RejectKind = "stale"  // lease failed: the remote ref is not what we expected
	RejectNonFF  RejectKind = "non-ff" // not a fast-forward, or fetch first
	RejectRemote RejectKind = "remote" // remote rejected, pre-receive hook declined, protected ref
	RejectOther  RejectKind = "other"
)

// PushResult is the outcome for one ref.
type PushResult struct {
	Dst      string
	OK       bool
	UpToDate bool
	Kind     RejectKind
	Reason   string // git's reason text, e.g. "stale info", "non-fast-forward"
}

// PushOptions controls Push.
type PushOptions struct {
	Remote  string
	Updates []PushUpdate
	// Metadata adds --no-verify --no-recurse-submodules
	// -c push.negotiate=false -c push.gpgSign=false.
	Metadata bool
	Timeout  time.Duration // default 60 s
}

// PushReport is the outcome of one push invocation.
type PushReport struct {
	Results []PushResult
	Remote  []string // "remote:" lines from stderr, prefix stripped
}

// Push pushes updates in one invocation with --porcelain (never --atomic)
// and returns a result per ref. Rejected refs are not errors; transport and
// permission failures are (see IsNetwork and IsPermission).
func (r *Repo) Push(ctx context.Context, opt PushOptions) (PushReport, error) {
	return r.push(ctx, opt)
}

// MergeResult is the outcome of a merge-tree check.
type MergeResult struct {
	Clean     bool
	Tree      string
	Conflicts []string // conflicted paths, sorted
}

// MergeTree performs a three-way merge of two commits in memory
// (git merge-tree --write-tree) without touching the work tree.
func (r *Repo) MergeTree(ctx context.Context, ours, theirs string) (MergeResult, error) {
	return r.mergeTree(ctx, ours, theirs)
}

// Switch checks out an existing branch. It refuses to discard changes.
func (r *Repo) Switch(ctx context.Context, branch string) error { return r.switchBranch(ctx, branch) }

// CreateBranch creates branch at startPoint ("" means HEAD) and switches to
// it, carrying uncommitted changes along (git switch -c).
func (r *Repo) CreateBranch(ctx context.Context, branch, startPoint string) error {
	return r.createBranch(ctx, branch, startPoint)
}

// Operation returns the git operation in progress: "rebase", "merge",
// "cherry-pick", "revert", "bisect", or "".
func (r *Repo) Operation(ctx context.Context) (string, error) { return r.operation(ctx) }

// RebaseOptions controls Rebase.
type RebaseOptions struct {
	Upstream  string
	Autostash bool
	// SequenceEditor, when set, makes the rebase interactive with this
	// command as GIT_SEQUENCE_EDITOR (it receives the todo file path).
	SequenceEditor string
}

// ApplyResult is the outcome of a rebase, cherry-pick or revert.
type ApplyResult struct {
	OK        bool
	Head      string   // new HEAD when OK
	Conflicts []string // when !OK; the operation has been aborted and the branch restored
}

// Rebase rebases the current branch. On conflict it aborts and reports the
// conflicted paths. It refuses with ErrBusy when an operation is in progress.
// It runs detached from ctx's cancellation, with its own timeout (120 s).
func (r *Repo) Rebase(ctx context.Context, opt RebaseOptions) (ApplyResult, error) {
	return r.rebase(ctx, opt)
}

// CherryPick applies commit onto HEAD with -x. On conflict it aborts.
func (r *Repo) CherryPick(ctx context.Context, commit string) (ApplyResult, error) {
	return r.cherryPick(ctx, commit)
}

// Revert reverts commit on HEAD (--no-edit). On conflict it aborts.
func (r *Repo) Revert(ctx context.Context, commit string) (ApplyResult, error) {
	return r.revert(ctx, commit)
}

// BlameLine returns the commit that last changed line (1-based) of path at HEAD.
func (r *Repo) BlameLine(ctx context.Context, path string, line int) (string, error) {
	return r.blameLine(ctx, path, line)
}

// CommitInfo is a parsed commit.
type CommitInfo struct {
	OID         string
	Subject     string
	Message     string // full message
	Author      string
	AuthorEmail string
	When        time.Time
}

// ShowCommit returns commit metadata.
func (r *Repo) ShowCommit(ctx context.Context, rev string) (CommitInfo, error) {
	return r.showCommit(ctx, rev)
}

// Log returns commits reachable from rev and newer than since (zero means
// no limit), newest first, at most max (0 means 100).
func (r *Repo) Log(ctx context.Context, rev string, since time.Time, max int) ([]CommitInfo, error) {
	return r.log(ctx, rev, since, max)
}

// LogGrep returns commits in revRange (for example "base..main") whose
// message contains text (fixed string), newest first.
func (r *Repo) LogGrep(ctx context.Context, revRange, text string) ([]string, error) {
	return r.logGrep(ctx, revRange, text)
}

// RevList returns commits reachable from include and not from exclude
// ("" for none), newest first.
func (r *Repo) RevList(ctx context.Context, include, exclude string) ([]string, error) {
	return r.revList(ctx, include, exclude)
}

// Grep returns paths at rev containing word as a whole word (fixed string,
// binary files skipped), at most max (0 means 50).
func (r *Repo) Grep(ctx context.Context, rev, word string, max int) ([]string, error) {
	return r.grep(ctx, rev, word, max)
}

// Worktree is one entry of git worktree list.
type Worktree struct {
	Path   string
	HEAD   string
	Branch string // short name, "" when detached
}

// Worktrees lists the repository's work trees.
func (r *Repo) Worktrees(ctx context.Context) ([]Worktree, error) { return r.worktrees(ctx) }

// ConfigGet returns a git config value, or "" when unset.
func (r *Repo) ConfigGet(ctx context.Context, key string) (string, error) {
	return r.configGet(ctx, key)
}

// ConfigGetAll returns every value of a multi-valued key.
func (r *Repo) ConfigGetAll(ctx context.Context, key string) ([]string, error) {
	return r.configGetAll(ctx, key)
}

// HooksDir returns the directory git runs hooks from (core.hooksPath, made
// absolute, or <common dir>/hooks).
func (r *Repo) HooksDir(ctx context.Context) (string, error) { return r.hooksDir(ctx) }
