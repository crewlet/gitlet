package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/footprint"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

const (
	snapshotMessage = "gitlet: snapshot"
	// maxParseBytes: larger files are tracked at file level only
	// (docs/SPEC.md, section 9.2).
	maxParseBytes = 2 << 20
	// mainParseLimit: when main changed more parseable files than this
	// since an item's base, symbol-level stale and new_callers are skipped.
	mainParseLimit = 300
	// activitySaveEvery throttles writing the activity time to the ledger.
	activitySaveEvery = 30 * time.Second
	// maxSnapshotErr caps the remote's text kept in snapshotError.
	maxSnapshotErr = 500
)

// snapRecord is the last snapshot this agent took in this clone. It lives
// in the ledger, so the CLI and the server of a worktree share it and do
// not snapshot the same tree twice.
type snapRecord struct {
	Fingerprint string       `json:"fingerprint"`
	Commit      string       `json:"commit"`
	Tree        string       `json:"tree"`
	Head        string       `json:"head"` // HEAD when it was taken: the snapshot's parent
	At          time.Time    `json:"at"`
	WipItem     model.ItemID `json:"wipItem,omitempty"`
	WipTree     string       `json:"wipTree,omitempty"` // tree of the last wip push (or rejection)
	WipAt       time.Time    `json:"wipAt,omitempty"`
}

func (e *Engine) snapKey() string { return "snapshot/" + string(e.me.ID) }

// loadSnap refreshes e.snap from the ledger, where another process of this
// worktree may have recorded a newer snapshot.
func (e *Engine) loadSnap(ctx context.Context) {
	v, ok, err := e.led.Get(ctx, e.snapKey())
	if err != nil || !ok {
		return
	}
	var r snapRecord
	if json.Unmarshal([]byte(v), &r) == nil && !r.At.Before(e.snap.At) {
		e.snap = r
	}
}

func (e *Engine) saveSnap(ctx context.Context) {
	if e.led.ReadOnly() {
		return
	}
	b, err := json.Marshal(e.snap)
	if err == nil {
		err = e.led.Set(ctx, e.snapKey(), string(b))
	}
	if err != nil {
		e.log.Debug("save snapshot record", "err", err)
	}
}

// snapMode says what snapshot may do.
type snapMode struct {
	force       bool // ignore the snapshot interval: a verb needs fresh state
	provisional bool // may open a provisional item when no item is active
}

// snapshotNow takes a snapshot when the work tree changed since the last
// one, whatever the snapshot interval, and brings this agent's items up to
// date: the active item's snapshot, base and footprint (head = the
// snapshot), submitted items' footprints (head = their branch head), the
// wip ref, and a provisional item when work appears with no item
// (docs/SPEC.md, sections 7 and 9.1). start, status, claim, publish and
// submit run it first, through beginVerb(ctx, true). Callers hold e.mu.
func (e *Engine) snapshotNow(ctx context.Context) error {
	return e.snapshot(ctx, snapMode{force: true, provisional: true})
}

// snapshot is snapshotNow with a mode; Tick passes force false, so a new
// snapshot also waits for the snapshot interval.
func (e *Engine) snapshot(ctx context.Context, mode snapMode) error {
	e.loadSnap(ctx)
	now := e.now()
	wfp, err := e.bg.WorktreeFingerprint(ctx)
	if err != nil {
		return fmt.Errorf("engine: snapshot: %w", err)
	}
	first := e.snap.Fingerprint == ""
	changed := wfp != e.snap.Fingerprint
	if changed && !first {
		e.touch(ctx, now)
	}
	took, dirty := false, false
	if changed && (mode.force || first || now.Sub(e.snap.At) >= e.cfg.SnapshotInterval) {
		head, err := e.bg.RevParse(ctx, "HEAD^{commit}")
		if err != nil {
			if errors.Is(err, gitx.ErrNotFound) {
				return nil // unborn branch: nothing to build on yet
			}
			return fmt.Errorf("engine: snapshot: %w", err)
		}
		res, err := e.bg.Snapshot(ctx, e.snapshotOptions(head))
		if err != nil {
			return fmt.Errorf("engine: snapshot: %w", err)
		}
		if first {
			headTree, _ := e.treeOf(ctx, head)
			dirty = headTree != res.Tree
		}
		e.snap.Fingerprint, e.snap.Commit, e.snap.Tree, e.snap.Head, e.snap.At = wfp, res.Commit, res.Tree, head, now
		e.saveSnap(ctx)
		took = true
	}
	main := e.mainOID(ctx)
	if !took && !mode.force && main == e.syncedMain {
		return nil
	}
	// A work-tree change opens a provisional item; the very first
	// observation counts only when the tree has uncommitted changes.
	mayOpen := mode.provisional && took && (!first || dirty)
	return e.syncItems(ctx, main, mayOpen)
}

func (e *Engine) snapshotOptions(parent string) gitx.SnapshotOptions {
	return gitx.SnapshotOptions{
		Parent:      parent,
		Message:     snapshotMessage,
		Untracked:   gitx.UntrackedPolicy(e.cfg.SnapshotUntracked),
		Include:     e.cfg.SnapshotInclude,
		IsSource:    symbols.Supported,
		MaxFileSize: e.cfg.MaxSnapshotFile,
	}
}

// itemUpdate is a change to one of my items computed outside the agent
// lock and applied inside it by id.
type itemUpdate struct {
	id       model.ItemID
	base     string
	snapshot string
	head     string
	branch   string
	fp       model.Footprint
	setFP    bool
}

// syncItems brings my items in line with the latest snapshot and main, and
// writes state when anything changed (one push, after the wip push of its
// own).
func (e *Engine) syncItems(ctx context.Context, main string, mayOpen bool) error {
	st := e.myState()
	mainTree := ""
	if main != "" {
		mainTree, _ = e.treeOf(ctx, main)
	}
	var (
		ups     []itemUpdate
		opened  *model.WorkItem
		closeID model.ItemID
		wipFor  model.ItemID
	)
	if ai := activeIndex(st.Items); ai >= 0 && e.snap.Commit != "" {
		it := st.Items[ai]
		if it.Provisional && mainTree != "" && e.snap.Tree == mainTree {
			closeID = it.ID // its tree went back to main: close it silently
		} else {
			wipFor = it.ID
			if u, ok := e.activeUpdate(ctx, it, main); ok {
				ups = append(ups, u)
			}
		}
	} else if ai < 0 && mayOpen && e.snap.Commit != "" && e.snap.Tree != mainTree && !e.readOnly() && !e.onSubmittedBranch(ctx, st.Items) {
		it := e.provisionalItem(ctx, main)
		opened, wipFor = &it, it.ID
	}
	for _, it := range st.Items {
		if it.Status == model.ItemSubmitted {
			if u, ok := e.submittedUpdate(ctx, it, main); ok {
				ups = append(ups, u)
			}
		}
	}
	e.syncedMain = main
	if e.readOnly() {
		return nil
	}
	snapErr, wipChanged := e.pushWip(ctx, wipFor)
	if len(ups) == 0 && opened == nil && closeID == "" && !wipChanged {
		return nil
	}
	_, err := e.mutateState(ctx, func(st *model.AgentState) error {
		now := e.now().UTC()
		changed := false
		if closeID != "" {
			if i := itemIndex(st.Items, closeID); i >= 0 && st.Items[i].Provisional && st.Items[i].Status == model.ItemActive {
				st.Items = slices.Delete(st.Items, i, i+1)
				changed = true
			}
		}
		for _, u := range ups {
			i := itemIndex(st.Items, u.id)
			if i < 0 {
				continue
			}
			it := &st.Items[i]
			if u.base != "" {
				it.Base = u.base
			}
			if u.snapshot != "" {
				it.Snapshot = u.snapshot
			}
			if u.head != "" {
				it.Head = u.head
			}
			if u.branch != "" {
				it.Branch = u.branch
			}
			if u.setFP {
				it.Footprint = u.fp
			}
			it.UpdatedAt = now
			changed = true
		}
		if opened != nil && activeIndex(st.Items) < 0 {
			st.Items = append(st.Items, *opened)
			changed = true
		}
		if wipChanged {
			if i := itemIndex(st.Items, wipFor); i >= 0 && st.Items[i].SnapshotErr != snapErr {
				st.Items[i].SnapshotErr = snapErr
				changed = true
			}
		}
		if !changed {
			return errNoChange
		}
		return nil
	})
	if closeID != "" {
		if err := e.bus.DeleteWip(ctx, closeID); err != nil {
			e.log.Debug("delete wip", "item", closeID, "err", err)
		}
	}
	return err
}

// onSubmittedBranch reports whether the worktree is on the branch of a
// submitted item: work there is a review fix, which the submitted item
// follows (submittedUpdate), not a new provisional item.
func (e *Engine) onSubmittedBranch(ctx context.Context, items []model.WorkItem) bool {
	branch, _ := e.bg.CurrentBranch(ctx)
	return branch != "" && slices.ContainsFunc(items, func(it model.WorkItem) bool {
		return it.Status == model.ItemSubmitted && it.Branch == branch
	})
}

// activeUpdate computes the active item's snapshot, base (merge base of the
// snapshot with main), branch and footprint.
func (e *Engine) activeUpdate(ctx context.Context, it model.WorkItem, main string) (itemUpdate, bool) {
	snap := e.snap.Commit
	base := it.Base
	if main != "" {
		if b, err := e.bg.MergeBase(ctx, main, snap); err == nil {
			base = b
		}
	}
	// The item follows the worktree to another work branch, but not to main
	// or a detached HEAD, where the agent only looks around.
	branch, _ := e.bg.CurrentBranch(ctx)
	if branch == e.cfg.Main {
		branch = ""
	}
	u := itemUpdate{id: it.ID, base: base, snapshot: snap, head: e.snap.Head, branch: branch}
	if base != "" && (it.Footprint.Base != base || it.Footprint.Head != snap) {
		if fp, err := e.footprint(ctx, base, snap); err == nil {
			u.fp, u.setFP = fp, true
		} else {
			e.log.Debug("footprint", "item", it.ID, "err", err)
		}
	}
	changed := it.Snapshot != snap || it.Base != base || it.Head != e.snap.Head ||
		(branch != "" && branch != it.Branch) || u.setFP
	return u, changed
}

// submittedUpdate follows a submitted item's branch head (review fixes keep
// landing on it) and recomputes its footprint when head or base moved.
func (e *Engine) submittedUpdate(ctx context.Context, it model.WorkItem, main string) (itemUpdate, bool) {
	head := it.Head
	if it.Branch != "" {
		if h, err := e.bg.RevParse(ctx, "refs/heads/"+it.Branch+"^{commit}"); err == nil {
			head = h
		}
	}
	if head == "" {
		return itemUpdate{}, false
	}
	base := it.Base
	if main != "" {
		if b, err := e.bg.MergeBase(ctx, main, head); err == nil {
			base = b
		}
	}
	if it.Head == head && it.Base == base && it.Footprint.Base == base && it.Footprint.Head == head {
		return itemUpdate{}, false
	}
	u := itemUpdate{id: it.ID, base: base, head: head}
	if base != "" {
		if fp, err := e.footprint(ctx, base, head); err == nil {
			u.fp, u.setFP = fp, true
		}
	}
	return u, true
}

// provisionalItem builds the item Tick opens when the work tree changes and
// no item is active: its intent comes from the branch name, and the rules
// add an untitled note asking for start (docs/SPEC.md, section 7).
func (e *Engine) provisionalItem(ctx context.Context, main string) model.WorkItem {
	branch, _ := e.bg.CurrentBranch(ctx)
	now := e.now().UTC()
	it := model.WorkItem{
		ID:          model.NewItemID(),
		Intent:      intentFromBranch(branch, e.cfg.Main, e.cfg.BranchPrefix),
		Provisional: true,
		Branch:      branch,
		Snapshot:    e.snap.Commit,
		Head:        e.snap.Head,
		Worktree:    e.repo.Root,
		Status:      model.ItemActive,
		OpenedAt:    now,
		UpdatedAt:   now,
	}
	if main != "" {
		if base, err := e.bg.MergeBase(ctx, main, e.snap.Commit); err == nil {
			it.Base = base
			if fp, err := e.footprint(ctx, base, e.snap.Commit); err == nil {
				it.Footprint = fp
			}
		}
	}
	return it
}

// intentFromBranch derives a provisional item's intent from its branch:
// "gitlet/add-strict-mode-8c1f" becomes "add strict mode".
func intentFromBranch(branch, main, prefix string) string {
	switch branch {
	case "":
		return "untitled work on a detached HEAD"
	case main:
		return "untitled work on " + main
	}
	name, gitletBranch := strings.CutPrefix(branch, prefix)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if n := len(words); gitletBranch && n > 1 && isHex(words[n-1]) && len(words[n-1]) == 4 {
		words = words[:n-1]
	}
	if len(words) == 0 {
		return "untitled work on " + branch
	}
	return strings.Join(words, " ")
}

func isHex(s string) bool {
	_, err := strconv.ParseUint(s, 16, 64)
	return err == nil && s != ""
}

// pushWip publishes the latest snapshot as item's wip ref, in a push of its
// own, when snapshots are published, the tree changed since the last wip
// push and the snapshot interval passed since it (a new item is pushed at
// once). A rejection becomes the item's snapshotError, and the same tree is
// not pushed again. changed reports whether snapshotError must be set to
// snapErr ("" clears it). docs/SPEC.md, section 6.1.
func (e *Engine) pushWip(ctx context.Context, item model.ItemID) (snapErr string, changed bool) {
	if item == "" || !e.cfg.PublishSnapshots || e.readOnly() || e.snap.Commit == "" {
		return "", false
	}
	now := e.now()
	if e.snap.WipItem == item && (e.snap.WipTree == e.snap.Tree || now.Sub(e.snap.WipAt) < e.cfg.SnapshotInterval) {
		return "", false
	}
	err := e.bus.WriteWip(ctx, item, e.snap.Commit)
	var rej *bus.RejectedError
	switch {
	case err == nil:
	case errors.As(err, &rej):
		snapErr = rej.Reason
		if len(rej.Remote) > 0 {
			snapErr += ": " + strings.Join(rej.Remote, " ")
		}
		if len(snapErr) > maxSnapshotErr {
			snapErr = snapErr[:maxSnapshotErr]
		}
		e.appendEvent(ctx, "snapshot_rejected", map[string]any{"item": item, "reason": snapErr})
	case gitx.IsNetwork(err):
		e.poll.markOffline(now)
		return "", false
	case errors.Is(err, bus.ErrObserver):
		e.enterObserver()
		return "", false
	default:
		e.log.Debug("write wip", "item", item, "err", err)
		return "", false
	}
	e.snap.WipItem, e.snap.WipTree, e.snap.WipAt = item, e.snap.Tree, now
	e.saveSnap(ctx)
	return snapErr, true
}

// footprint returns the footprint of head against base (footprint.Compute,
// docs/SPEC.md, section 9), from the ledger's cache when it holds the pair
// (key base..head), storing new ones. Files are parsed through the
// in-memory extraction cache. Use it for any item, branch or stub.
func (e *Engine) footprint(ctx context.Context, base, head string) (model.Footprint, error) {
	return e.cachedFootprint(ctx, "", base, head, 0)
}

// cachedFootprint is footprint with a cache key prefix and a cap on parsed
// files (main's footprint uses both).
func (e *Engine) cachedFootprint(ctx context.Context, prefix, base, head string, maxParsed int) (model.Footprint, error) {
	key := prefix + base + ".." + head
	if fp, ok, err := e.led.CachedFootprint(ctx, key); err == nil && ok {
		return fp, nil
	}
	res, err := footprint.Compute(ctx, e.bg, base, head, footprint.Options{
		MaxFileBytes: maxParseBytes,
		BroadFiles:   e.cfg.BroadFiles,
		BroadSymbols: e.cfg.BroadSymbols,
		MaxParsed:    maxParsed,
		Extract:      e.extract.extract,
		Now:          e.now(),
	})
	if err != nil {
		return model.Footprint{}, fmt.Errorf("engine: footprint %s: %w", key, err)
	}
	if !e.led.ReadOnly() {
		if err := e.led.CacheFootprint(ctx, key, res.Footprint); err != nil {
			e.log.Debug("cache footprint", "key", key, "err", err)
		}
	}
	return res.Footprint, nil
}

// parseableFiles counts a footprint's files in supported languages.
func parseableFiles(fp model.Footprint) int {
	n := 0
	for _, f := range fp.Files {
		if symbols.Supported(f.Path) {
			n++
		}
	}
	return n
}

// fileSymbols parses path at rev through the extraction cache. It returns
// nil for unsupported, missing and oversized files.
func (e *Engine) fileSymbols(ctx context.Context, rev, path string) (*symbols.File, error) {
	if rev == "" || !symbols.Supported(path) {
		return nil, nil
	}
	src, err := e.bg.ReadFile(ctx, rev, path)
	if errors.Is(err, gitx.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("engine: read %s:%s: %w", rev, path, err)
	}
	if len(src) > maxParseBytes {
		return nil, nil
	}
	return e.extract.extract(path, src)
}

// symbolAt returns the symbol with the qualified name in path at rev,
// parsed with the same extractor as footprints (contract landing, why).
func (e *Engine) symbolAt(ctx context.Context, rev, path, qualified string) (model.Symbol, bool) {
	f, err := e.fileSymbols(ctx, rev, path)
	if err != nil || f == nil {
		return model.Symbol{}, false
	}
	return f.Find(qualified)
}

// treeOf returns a commit's tree.
func (e *Engine) treeOf(ctx context.Context, commit string) (string, error) {
	t, err := e.bg.TreeOf(ctx, commit)
	if err != nil {
		return "", fmt.Errorf("engine: tree of %s: %w", commit, err)
	}
	return t, nil
}

// hasCommit reports whether a commit exists locally. Positive answers are
// memoized: objects do not go away while we look at them.
func (e *Engine) hasCommit(ctx context.Context, oid string) bool {
	if oid == "" {
		return false
	}
	if e.commits[oid] {
		return true
	}
	if _, err := e.bg.RevParse(ctx, oid+"^{commit}"); err != nil {
		return false
	}
	e.commits[oid] = true
	return true
}

// touch records activity: a gitlet call or a work-tree change. Heartbeats
// and claims are renewed only while the agent is active (docs/SPEC.md,
// section 8). The time is shared with the worktree's other processes
// through the ledger, at most every activitySaveEvery.
func (e *Engine) touch(ctx context.Context, now time.Time) {
	e.activity = now
	if now.Sub(e.activitySaved) < activitySaveEvery || e.led.ReadOnly() {
		return
	}
	e.activitySaved = now
	if err := e.led.Set(ctx, "activity/"+string(e.me.ID), now.UTC().Format(time.RFC3339Nano)); err != nil {
		e.log.Debug("save activity", "err", err)
	}
}

// lastActivity returns this agent's latest activity in any process.
func (e *Engine) lastActivity(ctx context.Context) time.Time {
	t := e.activity
	if v, ok, err := e.led.Get(ctx, "activity/"+string(e.me.ID)); err == nil && ok {
		if p, err := time.Parse(time.RFC3339Nano, v); err == nil && p.After(t) {
			t = p
		}
	}
	return t
}

// appendEvent adds an entry to the ledger's local event log (best effort).
func (e *Engine) appendEvent(ctx context.Context, kind string, detail any) {
	if e.led.ReadOnly() {
		return
	}
	b, _ := json.Marshal(detail)
	if err := e.led.AppendEvent(ctx, ledger.Event{At: e.now().UTC(), Agent: e.me.ID, Kind: kind, Detail: string(b)}); err != nil {
		e.log.Debug("append event", "kind", kind, "err", err)
	}
}

// extractCache memoizes symbol extraction by blob id and path (the path is
// part of every symbol key, so equal content at two paths parses twice). It
// is safe for concurrent use and hands out copies, so callers may change
// what they get.
type extractCache struct {
	mu    sync.Mutex
	max   int
	m     map[extractKey]*symbols.File
	order []extractKey
}

type extractKey struct{ blob, path string }

func newExtractCache(max int) *extractCache {
	return &extractCache{max: max, m: map[extractKey]*symbols.File{}}
}

// extract has the footprint.Extractor signature.
func (c *extractCache) extract(path string, src []byte) (*symbols.File, error) {
	k := extractKey{blobID(src), path}
	c.mu.Lock()
	f, ok := c.m[k]
	c.mu.Unlock()
	if ok {
		return copyFile(f), nil
	}
	f, err := symbols.Extract(path, src)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if _, ok := c.m[k]; !ok {
		if len(c.order) >= c.max {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
		c.m[k] = f
		c.order = append(c.order, k)
	}
	c.mu.Unlock()
	return copyFile(f), nil
}

func copyFile(f *symbols.File) *symbols.File {
	if f == nil {
		return nil
	}
	c := *f
	c.Symbols = slices.Clone(f.Symbols)
	c.Refs = slices.Clone(f.Refs)
	c.Imports = slices.Clone(f.Imports)
	return &c
}

// blobID is git's object id for src as a blob (SHA-1 object format).
func blobID(src []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(src))
	h.Write(src)
	return hex.EncodeToString(h.Sum(nil))
}
