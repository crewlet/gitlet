package bus

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
)

// Failures the fake remote produces. The test buses classify them; gitx's
// own classifiers recognise the same stderr from real git.
var (
	errFakeNetwork = &gitx.Error{Args: []string{"push"}, ExitCode: 128,
		Stderr: "fatal: unable to access 'https://git.example/repo.git/': Could not resolve host: git.example"}
	errFakeDenied = &gitx.Error{Args: []string{"push"}, ExitCode: 128,
		Stderr: "remote: Permission to team/repo.git denied to bot.\nfatal: unable to access 'https://git.example/repo.git/': The requested URL returned error: 403"}
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeObject struct {
	kind    string // blob, tree or commit
	data    []byte
	entries []gitx.TreeEntry
	tree    string
	parents []string
	message string
	when    time.Time
}

// fakeStore is the object database shared by every fake clone, as if each
// fetch brought every object.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string]*fakeObject
	seq     int
	clock   *fakeClock
}

func (s *fakeStore) put(o *fakeObject) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha1.New()
	fmt.Fprintf(h, "%s\x00", o.kind)
	h.Write(o.data)
	for _, e := range o.entries {
		fmt.Fprintf(h, "%s %s %s %s\x00", e.Mode, e.Type, e.OID, e.Name)
	}
	if o.kind == "commit" {
		// Every commit is distinct, like commits made at different times.
		s.seq++
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d", o.tree, strings.Join(o.parents, ","), o.message, s.seq)
	}
	oid := hex.EncodeToString(h.Sum(nil))
	s.objects[oid] = o
	return oid
}

func (s *fakeStore) get(oid string) *fakeObject {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[oid]
}

// isAncestor reports whether a is b or one of its ancestors.
func (s *fakeStore) isAncestor(a, b string) bool {
	todo := []string{b}
	for len(todo) > 0 {
		oid := todo[0]
		todo = todo[1:]
		if oid == a {
			return true
		}
		if o := s.get(oid); o != nil {
			todo = append(todo, o.parents...)
		}
	}
	return false
}

// commit writes a commit holding one file, for tests that build refs by hand.
func (s *fakeStore) commit(file string, data []byte, message string, parents ...string) string {
	blob := s.put(&fakeObject{kind: "blob", data: data})
	tree := s.put(&fakeObject{kind: "tree", entries: []gitx.TreeEntry{{Mode: "100644", Type: "blob", OID: blob, Name: file}}})
	return s.put(&fakeObject{kind: "commit", tree: tree, parents: parents, message: message, when: s.clock.Now()})
}

// fakeRemote is the shared remote: a ref store that honours leases the way
// git push --force-with-lease does.
type fakeRemote struct {
	mu    sync.Mutex
	store *fakeStore
	refs  map[string]string
	down  bool // transport failure
	// denied refuses every push for lack of permission.
	denied bool
	// reject refuses updates of matching refs like a pre-receive hook. It
	// gets the ref and its current value ("" when absent) and runs under the
	// remote's lock.
	reject func(dst, cur string) (reason string, remoteLines []string, ok bool)
	// beforePush runs before each push is applied, outside the lock, to
	// stage races.
	beforePush func(opt gitx.PushOptions)
	// listHangs makes LsRemoteHeads wait for its context, like a remote
	// slower than the caller's budget.
	listHangs bool
}

func (r *fakeRemote) get(ref string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs[ref]
}

func (r *fakeRemote) set(ref, oid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if oid == "" {
		delete(r.refs, ref)
	} else {
		r.refs[ref] = oid
	}
}

func (r *fakeRemote) setDown(down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = down
}

func (r *fakeRemote) setDenied(denied bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.denied = denied
}

func (r *fakeRemote) setReject(fn func(dst, cur string) (string, []string, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reject = fn
}

func (r *fakeRemote) setBeforePush(fn func(opt gitx.PushOptions)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforePush = fn
}

func (r *fakeRemote) list(prefix string) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for name, oid := range r.refs {
		if strings.HasPrefix(name, prefix) {
			out[name] = oid
		}
	}
	return out
}

// fakeRepo is one clone: local refs over the shared store, and an optional
// remote. It records every fetch and push.
type fakeRepo struct {
	store  *fakeStore
	remote *fakeRemote // nil: the repository has no remote

	mu      sync.Mutex
	refs    map[string]string
	fetches []gitx.FetchOptions
	pushes  []gitx.PushOptions
	readers int
	// bigReads counts objects over MaxReadDoc read into memory.
	bigReads int
}

var _ Repo = (*fakeRepo)(nil)

func notFound(what string) error { return fmt.Errorf("%s: %w", what, gitx.ErrNotFound) }

func (r *fakeRepo) HashBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return r.store.put(&fakeObject{kind: "blob", data: slices.Clone(data)}), nil
}

func (r *fakeRepo) MakeTree(ctx context.Context, entries []gitx.TreeEntry) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return r.store.put(&fakeObject{kind: "tree", entries: slices.Clone(entries)}), nil
}

func (r *fakeRepo) CommitTree(ctx context.Context, tree string, parents []string, message string, _ gitx.Identity) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if o := r.store.get(tree); o == nil || o.kind != "tree" {
		return "", notFound("tree " + tree)
	}
	return r.store.put(&fakeObject{kind: "commit", tree: tree, parents: slices.Clone(parents), message: message, when: r.store.clock.Now()}), nil
}

func (r *fakeRepo) OpenReader(context.Context) (*gitx.Reader, error) {
	return nil, errors.New("fake repository: use openObjectReader")
}

func (r *fakeRepo) openObjectReader(ctx context.Context) (objectReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.readers++
	r.mu.Unlock()
	return fakeReader{r}, nil
}

type fakeReader struct{ r *fakeRepo }

func (fr fakeReader) ReadObject(spec string, max int64) ([]byte, int64, bool, error) {
	rev, path, _ := strings.Cut(spec, ":")
	data, err := fr.r.readFile(rev, path)
	if err != nil {
		return nil, 0, false, err
	}
	size := int64(len(data))
	if max > 0 && size > max {
		return nil, size, false, nil
	}
	if size > MaxReadDoc {
		fr.r.mu.Lock()
		fr.r.bigReads++
		fr.r.mu.Unlock()
	}
	return data, size, true, nil
}

func (fakeReader) Close() error { return nil }

func (r *fakeRepo) readFile(rev, path string) ([]byte, error) {
	c := r.store.get(r.resolve(rev))
	if c == nil || c.kind != "commit" {
		return nil, notFound(rev)
	}
	for _, e := range r.store.get(c.tree).entries {
		if e.Name == path {
			return slices.Clone(r.store.get(e.OID).data), nil
		}
	}
	return nil, notFound(rev + ":" + path)
}

// resolve turns a ref name or object id into an oid ("" when unknown).
func (r *fakeRepo) resolve(rev string) string {
	r.mu.Lock()
	oid, ok := r.refs[rev]
	r.mu.Unlock()
	if ok {
		return oid
	}
	if r.store.get(rev) != nil {
		return rev
	}
	return ""
}

func (r *fakeRepo) RevParse(ctx context.Context, rev string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if oid := r.resolve(rev); oid != "" {
		return oid, nil
	}
	return "", notFound(rev)
}

func lockErr(ref, cur, want string) error {
	return &gitx.Error{Args: []string{"update-ref", ref}, ExitCode: 128,
		Stderr: fmt.Sprintf("fatal: cannot lock ref '%s': is at %q but expected %q", ref, cur, want)}
}

func (r *fakeRepo) UpdateRef(ctx context.Context, ref, newOID, oldOID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.refs[ref]
	switch {
	case oldOID == "":
	case oldOID == gitx.ZeroOID:
		if ok {
			return lockErr(ref, cur, oldOID)
		}
	case !ok || cur != oldOID:
		return lockErr(ref, cur, oldOID)
	}
	r.refs[ref] = newOID
	return nil
}

func (r *fakeRepo) DeleteRef(ctx context.Context, ref, oldOID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.refs[ref]
	if oldOID != "" && (!ok || cur != oldOID) {
		return lockErr(ref, cur, oldOID)
	}
	delete(r.refs, ref)
	return nil
}

func (r *fakeRepo) ListRefs(ctx context.Context, prefix string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for name, oid := range r.refs {
		if strings.HasPrefix(name, prefix) {
			out[name] = oid
		}
	}
	return out, nil
}

func (r *fakeRepo) ListRefsByDate(ctx context.Context, prefix string, max int) ([]gitx.RefInfo, error) {
	refs, err := r.ListRefs(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []gitx.RefInfo
	for name, oid := range refs {
		if o := r.store.get(oid); o != nil && o.kind == "commit" {
			out = append(out, gitx.RefInfo{Name: name, OID: oid, CommitterDate: o.when})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CommitterDate.Equal(out[j].CommitterDate) {
			return out[i].CommitterDate.After(out[j].CommitterDate)
		}
		return out[i].Name < out[j].Name
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (r *fakeRepo) Fetch(ctx context.Context, opt gitx.FetchOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	opt.Refspecs = slices.Clone(opt.Refspecs)
	r.fetches = append(r.fetches, opt)
	r.mu.Unlock()
	if r.remote == nil {
		return &gitx.Error{Args: []string{"fetch", opt.Remote}, ExitCode: 128, Stderr: "fatal: '" + opt.Remote + "' does not appear to be a git repository"}
	}
	r.remote.mu.Lock()
	if r.remote.down {
		r.remote.mu.Unlock()
		return errFakeNetwork
	}
	remote := make(map[string]string, len(r.remote.refs))
	for k, v := range r.remote.refs {
		remote[k] = v
	}
	r.remote.mu.Unlock()

	updates := map[string]string{}
	var globs []string // destination prefixes of glob refspecs, for --prune
	for _, spec := range opt.Refspecs {
		src, dst, _ := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if sp, ok := strings.CutSuffix(src, "*"); ok {
			dp := strings.TrimSuffix(dst, "*")
			for name, oid := range remote {
				if rest, ok := strings.CutPrefix(name, sp); ok {
					updates[dp+rest] = oid
				}
			}
			globs = append(globs, dp)
			continue
		}
		oid, ok := remote[src]
		if !ok {
			return &gitx.Error{Args: []string{"fetch", opt.Remote}, ExitCode: 128, Stderr: "fatal: couldn't find remote ref " + src}
		}
		updates[dst] = oid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for dst, oid := range updates {
		r.refs[dst] = oid
	}
	if opt.Prune {
		for name := range r.refs {
			for _, dp := range globs {
				if _, ok := updates[name]; !ok && strings.HasPrefix(name, dp) {
					delete(r.refs, name)
				}
			}
		}
	}
	return nil
}

func (r *fakeRepo) Push(ctx context.Context, opt gitx.PushOptions) (gitx.PushReport, error) {
	if err := ctx.Err(); err != nil {
		return gitx.PushReport{}, err
	}
	r.mu.Lock()
	opt.Updates = slices.Clone(opt.Updates)
	r.pushes = append(r.pushes, opt)
	r.mu.Unlock()
	if r.remote == nil {
		return gitx.PushReport{}, &gitx.Error{Args: []string{"push", opt.Remote}, ExitCode: 128, Stderr: "fatal: '" + opt.Remote + "' does not appear to be a git repository"}
	}
	r.remote.mu.Lock()
	hook := r.remote.beforePush
	r.remote.mu.Unlock()
	if hook != nil {
		hook(opt)
	}

	rem := r.remote
	rem.mu.Lock()
	defer rem.mu.Unlock()
	switch {
	case rem.down:
		return gitx.PushReport{}, errFakeNetwork
	case rem.denied:
		return gitx.PushReport{}, errFakeDenied
	}
	var rep gitx.PushReport
	for _, u := range opt.Updates {
		var src string
		if u.Src != "" {
			if src = r.resolve(u.Src); src == "" {
				return gitx.PushReport{}, &gitx.Error{Args: []string{"push"}, ExitCode: 1, Stderr: "error: src refspec " + u.Src + " does not match any"}
			}
		}
		cur, exists := rem.refs[u.Dst]
		res := gitx.PushResult{Dst: u.Dst}
		stale := false
		switch u.Expect {
		case gitx.ExpectAbsent:
			stale = exists
		case gitx.ExpectOID:
			stale = !exists || cur != u.OldOID
		}
		switch {
		case src != "" && exists && cur == src:
			res.OK, res.UpToDate = true, true
		case stale:
			res.Kind, res.Reason = gitx.RejectStale, "stale info"
		case u.Expect == gitx.ExpectFastForward && src != "" && exists && !r.store.isAncestor(cur, src):
			res.Kind, res.Reason = gitx.RejectNonFF, "non-fast-forward"
		default:
			if rem.reject != nil {
				if reason, lines, ok := rem.reject(u.Dst, cur); ok {
					res.Kind, res.Reason = gitx.RejectRemote, reason
					rep.Remote = append(rep.Remote, lines...)
					break
				}
			}
			res.OK = true
			if src == "" {
				delete(rem.refs, u.Dst)
			} else {
				rem.refs[u.Dst] = src
			}
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

// LsRemoteHeads lists the fake remote's branches.
func (r *fakeRepo) LsRemoteHeads(ctx context.Context, remote string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.remote == nil {
		return nil, &gitx.Error{Args: []string{"ls-remote", remote}, ExitCode: 128, Stderr: "fatal: '" + remote + "' does not appear to be a git repository"}
	}
	r.remote.mu.Lock()
	hangs := r.remote.listHangs
	r.remote.mu.Unlock()
	if hangs {
		<-ctx.Done()
		return nil, &gitx.Error{Args: []string{"ls-remote", remote}, ExitCode: -1, Stderr: ctx.Err().Error()}
	}
	r.remote.mu.Lock()
	defer r.remote.mu.Unlock()
	if r.remote.down {
		return nil, errFakeNetwork
	}
	out := map[string]string{}
	for name, oid := range r.remote.refs {
		if strings.HasPrefix(name, "refs/heads/") {
			out[name] = oid
		}
	}
	return out, nil
}

// PushExcluded walks the fake store the way the real one checks a push:
// every path at which something commit reaches, and no ref matching
// published reaches, holds a blob no such ref reaches.
func (r *fakeRepo) PushExcluded(ctx context.Context, commit string, published []string, maxSize int64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.store.get(commit) == nil {
		return nil, notFound(commit)
	}
	r.mu.Lock()
	var tips []string
	for name, oid := range r.refs {
		for _, p := range published {
			if name == p || strings.HasSuffix(p, "/") && strings.HasPrefix(name, p) {
				tips = append(tips, oid)
			}
		}
	}
	r.mu.Unlock()
	for _, p := range published {
		if !strings.HasPrefix(p, "refs/") {
			tips = append(tips, p) // a commit id; reach skips one the store lacks
		}
	}
	have := map[string]bool{} // what the tips reach
	var reach func(oid string)
	reach = func(oid string) {
		o := r.store.get(oid)
		if have[oid] || o == nil {
			return
		}
		have[oid] = true
		switch o.kind {
		case "commit":
			reach(o.tree)
			for _, parent := range o.parents {
				reach(parent)
			}
		case "tree":
			for _, e := range o.entries {
				reach(e.OID)
			}
		}
	}
	for _, tip := range tips {
		reach(tip)
	}
	seen := map[string]bool{} // by oid and path: an object can sit at several paths
	found := map[string]bool{}
	var walk func(oid, p string)
	walk = func(oid, p string) {
		o := r.store.get(oid)
		if have[oid] || seen[oid+" "+p] || o == nil {
			return
		}
		seen[oid+" "+p] = true
		switch o.kind {
		case "commit":
			walk(o.tree, "")
			for _, parent := range o.parents {
				walk(parent, "")
			}
		case "tree":
			for _, e := range o.entries {
				walk(e.OID, path.Join(p, e.Name))
			}
		case "blob":
			if gitx.IsDenied(p) || maxSize > 0 && int64(len(o.data)) > maxSize {
				found[p] = true
			}
		}
	}
	walk(commit, "")
	return slices.Sorted(maps.Keys(found)), nil
}

// ref returns a local ref ("" when missing).
func (r *fakeRepo) ref(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refs[name]
}

func (r *fakeRepo) setRef(name, oid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[name] = oid
}

func (r *fakeRepo) pushLog() []gitx.PushOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pushes)
}

func (r *fakeRepo) fetchLog() []gitx.FetchOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.fetches)
}

func (r *fakeRepo) readerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readers
}

func (r *fakeRepo) bigReadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bigReads
}

// ---- test environment ----

const (
	agentA model.AgentID = "claude-code-laptop-3fa9c1"
	agentB model.AgentID = "codex-ci-1a2b3c"
	itemA  model.ItemID  = "8c1f0e2a9b3d"
	itemB  model.ItemID  = "0123456789ab"
	keyA                 = "00112233aabbccdd"
	keyB                 = "4455667788990011"
	keyC                 = "ffeeddccbbaa9988"
)

var t0 = time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)

type testEnv struct {
	t      *testing.T
	clock  *fakeClock
	store  *fakeStore
	remote *fakeRemote
}

func newEnv(t *testing.T) *testEnv {
	clock := &fakeClock{t: t0}
	store := &fakeStore{objects: map[string]*fakeObject{}, clock: clock}
	e := &testEnv{t: t, clock: clock, store: store, remote: &fakeRemote{store: store, refs: map[string]string{}}}
	e.remote.refs["refs/heads/main"] = store.commit("README", []byte("hello\n"), "initial")
	return e
}

// clone returns a fresh clone of the shared remote.
func (e *testEnv) clone() *fakeRepo {
	return &fakeRepo{store: e.store, remote: e.remote, refs: map[string]string{}}
}

// local returns a repository without a remote.
func (e *testEnv) local() *fakeRepo {
	r := &fakeRepo{store: e.store, refs: map[string]string{}}
	r.refs["refs/heads/main"] = e.remote.refs["refs/heads/main"]
	return r
}

type busOpt func(*Options)

func withLockDir(dir string) busOpt { return func(o *Options) { o.LockDir = dir } }
func withPushLog(l PushLog) busOpt  { return func(o *Options) { o.PushLog = l } }
func withMaxSnapshotFile(n int64) busOpt {
	return func(o *Options) { o.MaxSnapshotFile = n }
}

// bus returns a Bus for agent me on repo, with "origin" as the remote when
// repo has one, a lock directory of its own and the env's clock.
func (e *testEnv) bus(repo *fakeRepo, me model.AgentID, opts ...busOpt) *Bus {
	opt := Options{Main: "main", LockDir: e.t.TempDir(), Now: e.clock.Now}
	if repo.remote != nil {
		opt.Remote = "origin"
	}
	for _, o := range opts {
		o(&opt)
	}
	b := New(repo, me, opt)
	b.isNetwork = func(err error) bool { return errors.Is(err, errFakeNetwork) }
	b.isPermission = func(err error) bool { return errors.Is(err, errFakeDenied) }
	return b
}

// oid returns a valid object id derived from s, for documents that only
// need well-formed ids.
func oid(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func sampleState(id model.AgentID) model.AgentState {
	return model.AgentState{
		Agent:        model.Agent{ID: id, Client: "claude-code", Host: "laptop", User: "seena"},
		Heartbeat:    t0,
		LeaseSeconds: 600,
		Items: []model.WorkItem{{
			ID: itemA, Intent: "Add strict mode to config loading", Branch: "gitlet/add-strict-mode-8c1f",
			Base: oid("base"), Snapshot: oid("snap"), Head: oid("head"), Status: model.ItemActive,
			OpenedAt: t0, UpdatedAt: t0,
			Footprint: model.Footprint{
				Base: oid("base"), Head: oid("snap"),
				Files: []model.FileChange{{Path: "src/config.ts", Status: "M", Lang: "typescript", Parsed: true}},
				Symbols: []model.SymbolChange{{Change: model.ChangeSignature, OldSignature: "function parseConfig(string): Config",
					Symbol: model.Symbol{Key: keyA, Lang: "typescript", Path: "src/config.ts", Name: "parseConfig",
						Qualified: "parseConfig", Kind: "function", Signature: "function parseConfig(string, boolean?): Config",
						Params: []model.Param{{Type: "string"}, {Type: "boolean", Optional: true}}, Results: "Config",
						StartLine: 10, EndLine: 30, StartByte: 120, EndByte: 610, Exported: true}}},
				Refs:       []model.Ref{{Name: "loadFile", Kind: model.RefCall, Lang: "typescript", Path: "src/config.ts", Line: 12, Module: "src/fs", Resolved: true}},
				ComputedAt: t0,
			},
		}},
	}
}

func sampleOutbox(id model.AgentID) model.Outbox {
	return model.Outbox{
		Agent: id,
		Needs: []model.Need{{ID: "aaaaaaaaaaaa", Key: keyA, Name: "src/config.ts:parseConfig", Item: itemA, Requirement: "keep returning Config", CreatedAt: t0}},
		Messages: []model.Message{{ID: "bbbbbbbbbbbb", Thread: "t-cccccccccccc", Kind: model.ThreadSignature, Subject: keyA,
			SubjectName: "src/config.ts:parseConfig", Items: []model.ItemID{itemA, itemB}, From: id, Item: itemA,
			Text: "Do you need the flag?", Proposal: &model.Proposal{ID: "p-dddddddddddd", Owner: id, Summary: "I own it"},
			Seen: []string{"eeeeeeeeeeee"}, At: t0}},
		Accepts: []model.Accept{{Thread: "t-cccccccccccc", Proposal: "p-dddddddddddd", At: t0}},
	}
}

func sampleClaim(key string, owner model.AgentID, item model.ItemID) model.Claim {
	return model.Claim{
		Key:    key,
		Symbol: model.Symbol{Key: key, Lang: "typescript", Path: "src/config.ts", Name: "parseConfig", Qualified: "parseConfig", Kind: "function"},
		Owner:  owner, Item: item, LeaseSeconds: 600, UpdatedAt: t0,
		Requirements: []model.Requirement{{From: agentB, Text: "needs a strict flag"}},
	}
}

// isNet reports what the test buses classify as a transport failure.
func isNet(err error) bool { return errors.Is(err, errFakeNetwork) }

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
