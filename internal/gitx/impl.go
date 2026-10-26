package gitx

// This file holds stubs behind the exported API in gitx.go.
// TODO(implement): every function below (docs/SPEC.md, section 14.1).

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

var errNotImplemented = errors.New("gitx: not implemented")

func joinArgs(a []string) string { return strings.Join(a, " ") }
func itoa(i int) string          { return strconv.Itoa(i) }

func isNetwork(err error) bool    { return false }
func isPermission(err error) bool { return false }
func isDenied(path string) bool   { return false }

func open(ctx context.Context, dir string) (*Repo, error) { return nil, errNotImplemented }
func (r *Repo) run(ctx context.Context, stdin []byte, env []string, args ...string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) version(ctx context.Context) (int, int, error) { return 0, 0, errNotImplemented }
func (r *Repo) revParse(ctx context.Context, rev string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) currentBranch(ctx context.Context) (string, error) { return "", errNotImplemented }
func (r *Repo) branchExists(ctx context.Context, branch string) (bool, error) {
	return false, errNotImplemented
}
func (r *Repo) remotes(ctx context.Context) ([]string, error) { return nil, errNotImplemented }
func (r *Repo) remoteURL(ctx context.Context, remote string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) defaultBranch(ctx context.Context, remote string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) mergeBase(ctx context.Context, a, b string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	return false, errNotImplemented
}
func (r *Repo) readFile(ctx context.Context, rev, path string) ([]byte, error) {
	return nil, errNotImplemented
}

type readerImpl struct{}

func (r *Repo) openReader(ctx context.Context) (*Reader, error)  { return nil, errNotImplemented }
func (rd *readerImpl) readFile(rev, path string) ([]byte, error) { return nil, errNotImplemented }
func (rd *readerImpl) readObject(spec string, max int64) ([]byte, int64, bool, error) {
	return nil, 0, false, errNotImplemented
}
func (rd *readerImpl) close() error { return nil }

func (r *Repo) diffNameStatus(ctx context.Context, from, to string) ([]DiffEntry, error) {
	return nil, errNotImplemented
}
func (r *Repo) diffNames(ctx context.Context, from, to string) ([]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) status(ctx context.Context) ([]StatusEntry, error)       { return nil, errNotImplemented }
func (r *Repo) isClean(ctx context.Context) (bool, error)               { return false, errNotImplemented }
func (r *Repo) worktreeFingerprint(ctx context.Context) (string, error) { return "", errNotImplemented }
func (r *Repo) snapshot(ctx context.Context, opt SnapshotOptions) (SnapshotResult, error) {
	return SnapshotResult{}, errNotImplemented
}
func (r *Repo) treeOf(ctx context.Context, commit string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) hashBlob(ctx context.Context, data []byte) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) makeTree(ctx context.Context, entries []TreeEntry) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) commitTree(ctx context.Context, tree string, parents []string, message string, id Identity) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) commitFile(ctx context.Context, base, path string, content []byte, message string, id Identity) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) updateRef(ctx context.Context, ref, newOID, oldOID string) error {
	return errNotImplemented
}
func (r *Repo) deleteRef(ctx context.Context, ref, oldOID string) error { return errNotImplemented }
func (r *Repo) listRefs(ctx context.Context, prefix string) (map[string]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) listRefsByDate(ctx context.Context, prefix string, max int) ([]RefInfo, error) {
	return nil, errNotImplemented
}
func (r *Repo) lsRemote(ctx context.Context, remote string, patterns ...string) (map[string]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) fetch(ctx context.Context, opt FetchOptions) error { return errNotImplemented }
func (r *Repo) push(ctx context.Context, opt PushOptions) (PushReport, error) {
	return PushReport{}, errNotImplemented
}
func (r *Repo) mergeTree(ctx context.Context, ours, theirs string) (MergeResult, error) {
	return MergeResult{}, errNotImplemented
}
func (r *Repo) switchBranch(ctx context.Context, branch string) error { return errNotImplemented }
func (r *Repo) createBranch(ctx context.Context, branch, startPoint string) error {
	return errNotImplemented
}
func (r *Repo) operation(ctx context.Context) (string, error) { return "", errNotImplemented }
func (r *Repo) rebase(ctx context.Context, opt RebaseOptions) (ApplyResult, error) {
	return ApplyResult{}, errNotImplemented
}
func (r *Repo) cherryPick(ctx context.Context, commit string) (ApplyResult, error) {
	return ApplyResult{}, errNotImplemented
}
func (r *Repo) revert(ctx context.Context, commit string) (ApplyResult, error) {
	return ApplyResult{}, errNotImplemented
}
func (r *Repo) blameLine(ctx context.Context, path string, line int) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) showCommit(ctx context.Context, rev string) (CommitInfo, error) {
	return CommitInfo{}, errNotImplemented
}
func (r *Repo) log(ctx context.Context, rev string, since time.Time, max int) ([]CommitInfo, error) {
	return nil, errNotImplemented
}
func (r *Repo) logGrep(ctx context.Context, revRange, text string) ([]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) revList(ctx context.Context, include, exclude string) ([]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) grep(ctx context.Context, rev, word string, max int) ([]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) worktrees(ctx context.Context) ([]Worktree, error) { return nil, errNotImplemented }
func (r *Repo) configGet(ctx context.Context, key string) (string, error) {
	return "", errNotImplemented
}
func (r *Repo) configGetAll(ctx context.Context, key string) ([]string, error) {
	return nil, errNotImplemented
}
func (r *Repo) hooksDir(ctx context.Context) (string, error) { return "", errNotImplemented }
