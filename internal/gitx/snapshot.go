package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sourceExts are the extensions of the languages gitlet parses
// (docs/SPEC.md, section 9.2): the default IsSource.
var sourceExts = map[string]bool{
	".go": true,
	".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".pyi": true,
	".rs": true,
}

func defaultIsSource(p string) bool { return sourceExts[strings.ToLower(path.Ext(p))] }

// isDenied matches the base name case-insensitively, so SECRETS.json and
// Id_RSA are caught on case-insensitive file systems too.
func isDenied(p string) bool {
	base := strings.ToLower(p[strings.LastIndexAny(p, `/\`)+1:])
	if base == "" {
		return false
	}
	for _, prefix := range []string{".env", "id_rsa", "id_ed25519", "id_ecdsa", "credentials", "secrets."} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".keystore"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// snapLocks serializes a process's snapshots of one work tree.
var snapLocks sync.Map // scratch dir -> *sync.Mutex

// staleSnapAge is the age after which a temporary index left behind by a
// crashed process is removed.
const staleSnapAge = time.Hour

func (r *Repo) snapshot(ctx context.Context, opt SnapshotOptions) (SnapshotResult, error) {
	parent, err := r.snapshotParent(ctx, opt.Parent)
	if err != nil {
		return SnapshotResult{}, err
	}
	dir, err := r.scratchDir(ctx)
	if err != nil {
		return SnapshotResult{}, err
	}
	mu, _ := snapLocks.LoadOrStore(dir, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	removeStaleSnaps(dir, time.Now())
	// The name says when the snapshot began: the copy of the index keeps
	// the real index's older time (prepareIndex), so another process's
	// sweep cannot go by the file's time.
	tmp := filepath.Join(dir, fmt.Sprintf("snap-%d-%d.index", os.Getpid(), time.Now().UnixNano()))
	defer os.Remove(tmp + ".lock")
	defer os.Remove(tmp)

	s := &snapper{r: r, ctx: ctx, opt: opt, index: tmp, parent: parent}
	if err := s.prepareIndex(); err != nil {
		return SnapshotResult{}, err
	}
	if err := s.stageTracked(); err != nil {
		return SnapshotResult{}, err
	}
	if err := s.stageUntracked(); err != nil {
		return SnapshotResult{}, err
	}
	if err := s.dropExcluded(); err != nil {
		return SnapshotResult{}, err
	}
	tree, err := s.git(nil, "write-tree")
	if err != nil {
		return SnapshotResult{}, err
	}
	tree = strings.TrimSpace(tree)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	msg := opt.Message
	if msg == "" {
		msg = "gitlet: snapshot"
	}
	commit, err := r.commitTree(ctx, tree, parents, msg, Identity{})
	if err != nil {
		return SnapshotResult{}, err
	}
	slices.Sort(s.skipped)
	return SnapshotResult{Commit: commit, Tree: tree, Skipped: slices.Compact(s.skipped)}, nil
}

// snapshotParent resolves the snapshot's parent: rev, or HEAD when rev is
// empty. An unborn HEAD gives "".
func (r *Repo) snapshotParent(ctx context.Context, rev string) (string, error) {
	if rev != "" {
		if err := checkRev(rev); err != nil {
			return "", err
		}
		return r.revParse(ctx, rev+"^{commit}")
	}
	oid, err := r.revParse(ctx, "HEAD^{commit}")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return oid, err
}

// removeStaleSnaps removes temporary indexes (and their locks) that crashed
// processes left: those begun more than staleSnapAge before now, by the time
// in their name, or by their modification time for names without one.
func removeStaleSnaps(dir string, now time.Time) {
	old, _ := filepath.Glob(filepath.Join(dir, "snap-*.index*"))
	for _, p := range old {
		began, ok := snapBegan(filepath.Base(p))
		if !ok {
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			began = fi.ModTime()
		}
		if now.Sub(began) > staleSnapAge {
			os.Remove(p)
		}
	}
}

// snapBegan reads the start time from a temporary index name,
// snap-<pid>-<unix nanoseconds>.index[.lock].
func snapBegan(name string) (time.Time, bool) {
	name, _, _ = strings.Cut(strings.TrimPrefix(name, "snap-"), ".")
	_, nanos, ok := strings.Cut(name, "-")
	n, err := strconv.ParseInt(nanos, 10, 64)
	if !ok || err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// snapper builds one snapshot in a temporary index.
type snapper struct {
	r       *Repo
	ctx     context.Context
	opt     SnapshotOptions
	index   string // temporary index file
	parent  string // "" for an unborn HEAD
	skipped []string
}

// git runs a command against the temporary index. Pathspecs are literal, so
// a file named "[ab].go" never stands for a.go.
func (s *snapper) git(stdin []byte, args ...string) (string, error) {
	args = append([]string{"-c", "core.splitIndex=false", "--literal-pathspecs"}, args...)
	out, err := s.r.exec(s.ctx, cmdOpts{stdin: stdin, env: []string{"GIT_INDEX_FILE=" + s.index}}, args...)
	return string(out), err
}

// prepareIndex copies the real index, which leaves git only the changed
// files to hash. The copy keeps the real index's modification time: git
// trusts an entry's cached stat data only when the file was last changed
// before the index was written, and a copy dated now would make a file
// rewritten in the same second as the real index, at the same size, look
// unchanged ("racy git"). An unborn HEAD starts from an empty index; a
// missing real index starts from the parent's tree.
func (s *snapper) prepareIndex() error {
	os.Remove(s.index)
	if s.parent == "" {
		return nil // git treats a missing index file as empty
	}
	real, err := s.r.read(s.ctx, "rev-parse", "--git-path", "index")
	if err != nil {
		return err
	}
	switch err := copyFile(s.r.abs(real), s.index); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		_, err := s.git(nil, "read-tree", s.parent)
		return err
	default:
		return fmt.Errorf("gitx: snapshot: %w", err)
	}
}

// copyFile copies src to dst, keeping src's modification time.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// excluded reports whether a work-tree file must stay out of the snapshot,
// recording it as skipped.
func (s *snapper) excluded(p string) bool {
	if isDenied(p) {
		s.skipped = append(s.skipped, p)
		return true
	}
	if s.opt.MaxFileSize > 0 {
		fi, err := os.Lstat(filepath.Join(s.r.Root, filepath.FromSlash(p)))
		if err == nil && fi.Mode().IsRegular() && fi.Size() > s.opt.MaxFileSize {
			s.skipped = append(s.skipped, p)
			return true
		}
	}
	return false
}

// stageTracked stages changes to tracked files, except denied and oversized
// ones, which are never hashed.
func (s *snapper) stageTracked() error {
	out, err := s.git(nil, "diff-files", "--name-only", "-z")
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range splitNUL([]byte(out)) {
		if !s.excluded(p) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths) // unmerged paths are listed once per stage
	if len(paths) == 0 {
		return nil // add -u without pathspecs would stage everything
	}
	_, err = s.git(nulJoin(paths), "add", "-u", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// stageUntracked adds the untracked, non-ignored files the policy allows.
func (s *snapper) stageUntracked() error {
	if s.opt.Untracked == UntrackedNone {
		return nil
	}
	out, err := s.git(nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range splitNUL([]byte(out)) {
		// A trailing slash marks a nested repository.
		if strings.HasSuffix(p, "/") || !s.allowUntracked(p) || s.excluded(p) {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil
	}
	_, err = s.git(nulJoin(paths), "add", "--pathspec-from-file=-", "--pathspec-file-nul")
	if err != nil && strings.Contains(stderrOf(err), "did not match any files") {
		// A file went away since it was listed: retry with those still there.
		var still []string
		for _, p := range paths {
			if _, err := os.Lstat(filepath.Join(s.r.Root, filepath.FromSlash(p))); err == nil {
				still = append(still, p)
			}
		}
		if len(still) == 0 {
			return nil
		}
		_, err = s.git(nulJoin(still), "add", "--pathspec-from-file=-", "--pathspec-file-nul")
	}
	return err
}

func (s *snapper) allowUntracked(p string) bool {
	switch s.opt.Untracked {
	case UntrackedAll:
		return true
	case UntrackedNone:
		return false
	}
	isSource := s.opt.IsSource
	if isSource == nil {
		isSource = defaultIsSource
	}
	if isSource(p) {
		return true
	}
	base := path.Base(p)
	for _, g := range s.opt.Include {
		if ok, _ := path.Match(g, p); ok {
			return true
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// dropExcluded checks every entry that differs from the parent, including
// entries the user staged in the real index, and gives denied and oversized
// files their parent version back (or removes them when they are new).
func (s *snapper) dropExcluded() error {
	base := s.parent
	if base == "" {
		empty, err := s.r.emptyTree(s.ctx)
		if err != nil {
			return err
		}
		base = empty
	}
	out, err := s.git(nil, "diff-index", "--cached", "-z", "--raw", "--no-renames", base)
	if err != nil {
		return err
	}
	var changes []indexChange
	fields := splitNUL([]byte(out))
	for i := 0; i+1 < len(fields); i += 2 {
		// :<old mode> <new mode> <old oid> <new oid> <status> NUL <path>
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return fmt.Errorf("gitx: snapshot: malformed diff-index record %q", fields[i])
		}
		c := indexChange{oldMode: meta[0], newMode: meta[1], oldOID: meta[2], newOID: meta[3], status: meta[4], path: fields[i+1]}
		if c.status != "D" {
			changes = append(changes, c)
		}
	}

	sizes, err := s.blobSizes(changes)
	if err != nil {
		return err
	}
	var info strings.Builder
	for _, c := range changes {
		size, sized := sizes[c.newOID]
		tooBig := s.opt.MaxFileSize > 0 && sized && size > s.opt.MaxFileSize
		// Unmerged entries left now belong to excluded files: add -u
		// resolved every other one.
		if !isDenied(c.path) && !tooBig && c.status != "U" {
			continue
		}
		s.skipped = append(s.skipped, c.path)
		// Remove every stage. The null id must have the repository's hash
		// length, which the old id in the record has.
		fmt.Fprintf(&info, "0 %s\t%s\x00", strings.Repeat("0", len(c.oldOID)), c.path)
		if c.oldMode != "000000" {
			fmt.Fprintf(&info, "%s %s\t%s\x00", c.oldMode, c.oldOID, c.path)
		}
	}
	if info.Len() == 0 {
		return nil
	}
	_, err = s.git([]byte(info.String()), "update-index", "-z", "--index-info")
	return err
}

// indexChange is one record of git diff-index --raw.
type indexChange struct{ oldMode, newMode, oldOID, newOID, status, path string }

// blobSizes returns the sizes of the new blobs among changes, by object id.
// Gitlinks are skipped: their commits live in another repository.
func (s *snapper) blobSizes(changes []indexChange) (map[string]int64, error) {
	sizes := map[string]int64{}
	if s.opt.MaxFileSize <= 0 {
		return sizes, nil
	}
	var in strings.Builder
	for _, c := range changes {
		switch c.newMode {
		case "100644", "100755", "120000":
			in.WriteString(c.newOID + "\n")
		}
	}
	if in.Len() == 0 {
		return sizes, nil
	}
	out, err := s.r.exec(s.ctx, cmdOpts{stdin: []byte(in.String())}, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	for _, l := range lines(string(out)) {
		f := strings.Fields(l) // <oid> <type> <size>, or <oid> missing
		if len(f) == 3 {
			if n, err := strconv.ParseInt(f[2], 10, 64); err == nil {
				sizes[f[0]] = n
			}
		}
	}
	return sizes, nil
}

// isOID reports whether s is a full object id, SHA-1 or SHA-256.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

func (r *Repo) pushExcluded(ctx context.Context, commit string, published []string, maxSize int64) ([]string, error) {
	if err := checkRev(commit); err != nil {
		return nil, err
	}
	revs := []string{commit}
	var patterns, oids []string
	for _, p := range published {
		switch {
		case isOID(p):
			oids = append(oids, p)
		case strings.HasPrefix(p, "refs/") && !strings.ContainsAny(p, "\x00\n"):
			patterns = append(patterns, p)
		default:
			return nil, fmt.Errorf("gitx: invalid ref pattern %q", p)
		}
	}
	if len(patterns) > 0 {
		out, err := r.read(ctx, append([]string{"for-each-ref", "--format=%(objectname)"}, patterns...)...)
		if err != nil {
			return nil, err
		}
		for _, oid := range lines(out) {
			revs = append(revs, "^"+oid)
		}
	}
	// Nothing here fetches what a partial clone lacks: an object the clone
	// does not have is not one a push from it sends.
	noFetch := []string{"GIT_NO_LAZY_FETCH=1"}
	if len(oids) > 0 {
		// The remote's tips this clone does not have cannot be walked, and
		// nothing local builds on them. --missing also keeps a partial clone
		// from fetching each of them, which GIT_NO_LAZY_FETCH does only on
		// newer git.
		out, err := r.exec(ctx, cmdOpts{stdin: []byte(strings.Join(oids, "\n") + "\n"), env: noFetch},
			"rev-list", "--no-walk", "--missing=allow-any", "--ignore-missing", "--stdin")
		if err != nil {
			return nil, err
		}
		for _, oid := range lines(string(out)) {
			revs = append(revs, "^"+oid)
		}
	}
	// The tips go through stdin: a clone can track many refs. Commits are
	// listed as "<oid>", trees and blobs as "<oid> <path>", with only the
	// first path the walk met the object at.
	var commits, objects []string
	list := &splitWriter{sep: '\n', fn: func(l string) {
		if oid, _, ok := strings.Cut(l, " "); ok {
			objects = append(objects, oid)
		} else if l != "" {
			commits = append(commits, l)
		}
	}}
	_, err := r.exec(ctx, cmdOpts{stdin: []byte(strings.Join(revs, "\n") + "\n"), stdout: list, env: noFetch},
		"rev-list", "--objects", "--missing=allow-any", "--stdin")
	if err != nil {
		return nil, mapNotFound(err, commit)
	}
	if len(commits) == 0 || len(objects) == 0 {
		return nil, nil
	}
	out, err := r.exec(ctx, cmdOpts{stdin: []byte(strings.Join(objects, "\n") + "\n"), env: noFetch}, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	blobs := map[string]int64{} // the blobs the push would send, by oid, with their sizes
	for _, l := range lines(string(out)) {
		if f := strings.Fields(l); len(f) == 3 && f[1] == "blob" { // <oid> <type> <size>
			blobs[f[0]], _ = strconv.ParseInt(f[2], 10, 64)
		}
	}
	if len(blobs) == 0 {
		return nil, nil
	}
	// A blob can sit at several paths (renamed in a later commit, copied
	// under another name), so each new blob is checked at every path the
	// commits hold it at. Following the parents that hold it at a path
	// ends at a commit, never one the remote has, whose parents do not,
	// so diffing every commit against each parent (-m) and a root against
	// nothing (--root) meets every such path. diff-tree is plumbing, so
	// the user's diff settings do not change its output, which is read as
	// it comes: a long history can change many paths.
	found := map[string]bool{}
	var meta string
	diff := &splitWriter{sep: 0, fn: func(field string) {
		if meta == "" {
			meta = field // :<old mode> <new mode> <old oid> <new oid> <status>
			return
		}
		f := strings.Fields(meta)
		meta = ""
		if len(f) != 5 {
			return
		}
		if size, ok := blobs[f[3]]; ok && (isDenied(field) || maxSize > 0 && size > maxSize) {
			found[field] = true
		}
	}}
	_, err = r.exec(ctx, cmdOpts{stdin: []byte(strings.Join(commits, "\n") + "\n"), stdout: diff, env: noFetch},
		"diff-tree", "--stdin", "-r", "-m", "--root", "--no-renames", "--no-commit-id", "-z", "--diff-filter=AMT")
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(found)), nil
}

func nulJoin(paths []string) []byte {
	return []byte(strings.Join(paths, "\x00") + "\x00")
}
