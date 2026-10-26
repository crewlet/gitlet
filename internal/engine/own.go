package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/flock"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

const (
	// agentLockWait bounds the wait for the per-agent write lock.
	agentLockWait = 10 * time.Second
	// closedRetention is how long landed and abandoned items stay in
	// state.json, and how long messages of settled threads stay in the
	// outbox (docs/SPEC.md, section 6.4).
	closedRetention = time.Hour
	// orphanRetention is how long messages of threads that no longer exist
	// (a clash that went away) stay in the outbox.
	orphanRetention = 24 * time.Hour
	// keepPerThread is how many of my newest messages per thread survive
	// when an outbox must shrink under bus.MaxWriteDoc.
	keepPerThread = 10
)

var (
	// errNoChange, returned by a mutateState or mutateOutbox callback, ends
	// the cycle without writing. The helper then returns the document as
	// read and a nil error.
	errNoChange = errors.New("engine: no change")
	// errReadOnly is wrapped by every write refused because the engine is
	// read-only; readOnlyRefusal turns it into a refusal.
	errReadOnly = errors.New("engine: read-only")
)

// lockAgent takes this agent's write lock, <common>/gitlet/locks/<agent>.lock,
// waiting at most agentLockWait. CLI and server processes of one worktree
// share it, so their read-modify-write cycles never lose each other's
// updates. It is reentrant within the engine (callers hold e.mu), so
// mutateState may be called from a mutateOutbox callback and vice versa.
// The returned function releases one level and is safe to call twice.
func (e *Engine) lockAgent(ctx context.Context) (func(), error) {
	if e.agentLockDepth == 0 {
		lctx, cancel := context.WithTimeout(ctx, agentLockWait)
		defer cancel()
		l, err := flock.Acquire(lctx, filepath.Join(e.stateDir, "locks", string(e.me.ID)+".lock"))
		if err != nil {
			return nil, fmt.Errorf("engine: agent lock: %w", err)
		}
		e.agentLock = l
	}
	e.agentLockDepth++
	released := false
	return func() {
		if released {
			return
		}
		released = true
		if e.agentLockDepth--; e.agentLockDepth == 0 && e.agentLock != nil {
			_ = e.agentLock.Unlock()
			e.agentLock = nil
		}
	}, nil
}

// mutateState is the read-modify-write cycle for this agent's state.json
// that every verb uses (docs/SPEC.md, section 3). Under the per-agent lock
// it reads the local state ref, lets fn change the document, then stamps
// format, agent, heartbeat and lease, drops landed and abandoned items
// closed for more than an hour, and writes it with bus.WriteState, which
// pushes it. The cached shared state is updated with the result, and new or
// changed items go to the ledger's history.
//
// fn returns errNoChange to skip the write. Every other write is also a
// heartbeat, so a callback that returns nil without changes renews it. fn
// may run twice: when another process changed the local ref between read
// and write, the cycle re-reads and retries once, so fn must derive its
// changes from the document it is given.
//
// Outcomes:
//   - remote unreachable: the local write stands, offlineSince is set and
//     the next successful poll flushes it; mutateState returns nil;
//   - permission denied: the engine enters observer mode (read-only, with a
//     warning); the local write stands and mutateState returns nil;
//   - identity collision, or a read-only engine: an error wrapping
//     errReadOnly, which writeRefusal turns into a refusal;
//   - a document over bus.MaxWriteDoc: footprint details are dropped and the
//     write retried once;
//   - a local document that cannot be decoded is replaced.
//
// Callers hold e.mu (every verb and Tick do).
func (e *Engine) mutateState(ctx context.Context, fn func(*model.AgentState) error) (model.AgentState, error) {
	var before map[model.ItemID]string
	st, err := ownCycle(ctx, e, "state", ownDoc[model.AgentState]{
		read: e.bus.ReadOwnState,
		stamp: func(st *model.AgentState) {
			now := e.now()
			st.Format = model.Format
			st.Agent = e.me
			st.Heartbeat = now.UTC()
			st.LeaseSeconds = leaseSeconds(e.cfg.Lease)
			st.Items = trimItems(st.Items, now)
		},
		write:  e.bus.WriteState,
		shrink: shrinkState,
		cache:  e.setOwnState,
	}, func(st *model.AgentState) error {
		before = itemDigests(st.Items)
		return fn(st)
	})
	if err == nil {
		e.rememberItems(ctx, before, st.Items)
	}
	return st, err
}

// mutateOutbox is mutateState for outbox.json (needs, thread messages and
// accepts). Before writing it drops messages and accepts of threads settled
// more than an hour ago, and of threads that no longer exist after a day
// (docs/SPEC.md, section 6.4). The same outcomes apply, and fn may run
// twice too.
func (e *Engine) mutateOutbox(ctx context.Context, fn func(*model.Outbox) error) (model.Outbox, error) {
	return ownCycle(ctx, e, "outbox", ownDoc[model.Outbox]{
		read: e.bus.ReadOwnOutbox,
		stamp: func(ob *model.Outbox) {
			ob.Format = model.Format
			ob.Agent = e.me.ID
			trimOutbox(ob, e.threadTimes(ctx, ob.Accepts), e.now())
		},
		write:  e.bus.WriteOutbox,
		shrink: shrinkOutbox,
		cache:  e.setOwnOutbox,
	}, fn)
}

// ownDoc describes one of this agent's own documents for ownCycle.
type ownDoc[T any] struct {
	read   func(context.Context) (T, string, error)
	stamp  func(*T) // fields every write sets
	write  func(context.Context, T, string) (string, error)
	shrink func(*T) bool // makes the document smaller; false when it cannot
	cache  func(T)       // records the document in the shared state
}

// ownCycle is the read-modify-write cycle behind mutateState and
// mutateOutbox (see mutateState for the outcomes).
func ownCycle[T any](ctx context.Context, e *Engine, what string, d ownDoc[T], fn func(*T) error) (T, error) {
	var zero T
	if e.readOnly() {
		return zero, e.readOnlyErr()
	}
	unlock, err := e.lockAgent(ctx)
	if err != nil {
		return zero, err
	}
	defer unlock()
	for attempt := 0; ; attempt++ {
		doc, oid, err := d.read(ctx)
		if err != nil {
			if oid == "" {
				return doc, fmt.Errorf("engine: read own %s: %w", what, err)
			}
			// The bus returns the oid of a document it cannot decode, so a
			// broken document is replaced rather than blocking every write.
			e.log.Warn("own document unreadable; replacing it", "doc", what, "err", err)
			doc = zero
		}
		if err := fn(&doc); err != nil {
			if errors.Is(err, errNoChange) {
				d.cache(doc)
				return doc, nil
			}
			return doc, err
		}
		d.stamp(&doc)
		newOID, err := d.write(ctx, doc, oid)
		if errors.Is(err, bus.ErrTooLarge) && d.shrink(&doc) {
			newOID, err = d.write(ctx, doc, oid)
		}
		if err != nil && newOID == "" && attempt == 0 && localConflict(ctx, err) {
			continue // the local ref changed since we read it: read again, once
		}
		if err := e.ownWriteResult(what, newOID, err); err != nil {
			return doc, err
		}
		d.cache(doc)
		return doc, nil
	}
}

// localConflict reports whether a failed own write may be a lost local
// compare-and-swap, which the bus reports without a sentinel: anything that
// is not a network, permission, collision, size or rejection error.
func localConflict(ctx context.Context, err error) bool {
	var rej *bus.RejectedError
	return ctx.Err() == nil && !gitx.IsNetwork(err) && !errors.Is(err, bus.ErrObserver) &&
		!errors.Is(err, bus.ErrIdentityCollision) && !errors.Is(err, bus.ErrTooLarge) &&
		!errors.As(err, &rej) && !errBudget(err)
}

// ownWriteResult classifies the outcome of a write to an own ref. A
// non-empty oid means the local write stood even when the push failed.
func (e *Engine) ownWriteResult(what, oid string, err error) error {
	switch {
	case err == nil:
		e.poll.wroteOK()
		return nil
	case gitx.IsNetwork(err):
		e.poll.markOffline(e.now())
		return nil
	case errors.Is(err, bus.ErrObserver):
		e.enterObserver()
		return nil
	case errors.Is(err, bus.ErrIdentityCollision):
		if !e.collision.Swap(true) {
			e.log.Warn("identity collision: running read-only", "agent", e.me.ID, "err", err)
		}
		return fmt.Errorf("engine: write %s: %w: %w", what, errReadOnly, err)
	case oid != "":
		e.log.Warn("push of own document failed; it stays local until the next flush", "doc", what, "err", err)
		e.poll.requestFlush()
		return nil
	}
	return fmt.Errorf("engine: write %s: %w", what, err)
}

// enterObserver switches to observer mode: read-only with one warning,
// re-probed hourly by Tick (docs/SPEC.md, section 6.1).
func (e *Engine) enterObserver() {
	if !e.observer.Swap(true) {
		e.observerSince = e.now()
		e.log.Warn("the remote refused gitlet's writes: running read-only", "agent", e.me.ID)
	}
}

// readOnlyReason says why the engine is read-only ("" when it is not).
func (e *Engine) readOnlyReason() string {
	switch {
	case e.sameWorktree:
		return "another gitlet session already serves this worktree"
	case e.collision.Load():
		return "another process is writing this agent's refs (identity collision)"
	case e.observer.Load():
		return "the remote refused gitlet's writes (observer mode)"
	case e.led != nil && e.led.ReadOnly():
		return "the ledger was written by a newer gitlet"
	}
	return ""
}

func (e *Engine) readOnlyErr() error {
	return fmt.Errorf("%w: %s", errReadOnly, e.readOnlyReason())
}

// ensureShared makes e.shared usable before the first load.
func (e *Engine) ensureShared() {
	if e.shared == nil {
		e.shared = &bus.State{}
	}
	if e.shared.Agents == nil {
		e.shared.Agents = map[model.AgentID]model.AgentState{}
	}
	if e.shared.Outboxes == nil {
		e.shared.Outboxes = map[model.AgentID]model.Outbox{}
	}
	if e.shared.Claims == nil {
		e.shared.Claims = map[string]bus.ClaimView{}
	}
	if e.shared.Refs == nil {
		e.shared.Refs = map[string]string{}
	}
}

func (e *Engine) setOwnState(st model.AgentState) {
	e.ensureShared()
	e.shared.Agents[e.me.ID] = st
}

func (e *Engine) setOwnOutbox(ob model.Outbox) {
	e.ensureShared()
	e.shared.Outboxes[e.me.ID] = ob
}

// setClaim updates the cached claim for key after a claim write (nil: it
// was deleted), so the rest of the verb sees it without a reload. The bus
// has already moved the local tracking ref.
func (e *Engine) setClaim(key string, cv *bus.ClaimView) {
	e.ensureShared()
	if cv == nil {
		delete(e.shared.Claims, key)
		return
	}
	e.shared.Claims[key] = *cv
}

// myState returns this agent's state as last read or written.
func (e *Engine) myState() model.AgentState {
	if e.shared == nil {
		return model.AgentState{}
	}
	return e.shared.Agents[e.me.ID]
}

// myOutbox returns this agent's outbox as last read or written.
func (e *Engine) myOutbox() model.Outbox {
	if e.shared == nil {
		return model.Outbox{}
	}
	return e.shared.Outboxes[e.me.ID]
}

// activeItem returns this agent's active item (stated or provisional).
func (e *Engine) activeItem() (model.WorkItem, bool) {
	i := activeIndex(e.myState().Items)
	if i < 0 {
		return model.WorkItem{}, false
	}
	return e.myState().Items[i], true
}

// activeIndex returns the index of the active item, or -1.
func activeIndex(items []model.WorkItem) int {
	for i, it := range items {
		if it.Status == model.ItemActive {
			return i
		}
	}
	return -1
}

// itemIndex returns the index of item id, or -1.
func itemIndex(items []model.WorkItem, id model.ItemID) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// leaseSeconds converts the configured lease for state.json and claims.
func leaseSeconds(d time.Duration) int {
	if s := int(d / time.Second); s > 0 {
		return s
	}
	return 1
}

// trimItems drops landed and abandoned items closed more than an hour ago.
func trimItems(items []model.WorkItem, now time.Time) []model.WorkItem {
	out := make([]model.WorkItem, 0, len(items))
	for _, it := range items {
		if !it.Status.Open() {
			closed := it.UpdatedAt
			if it.ClosedAt != nil {
				closed = *it.ClosedAt
			}
			if now.Sub(closed) > closedRetention {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// shrinkState drops footprint details so state.json fits under
// bus.MaxWriteDoc: first every ref list, then every symbol list (marking
// the footprints broad). It reports whether anything was dropped.
func shrinkState(st *model.AgentState) bool {
	shrunk := false
	for i := range st.Items {
		if fp := &st.Items[i].Footprint; len(fp.Refs) > 0 {
			fp.Refs, shrunk = nil, true
		}
	}
	if shrunk {
		return true
	}
	for i := range st.Items {
		if fp := &st.Items[i].Footprint; len(fp.Symbols) > 0 {
			fp.Symbols, fp.Broad, shrunk = nil, true, true
		}
	}
	return shrunk
}

// threadInfo is what outbox trimming needs to know about a thread.
type threadInfo struct {
	settled   bool
	settledAt time.Time
}

// threadTimes assembles the threads this agent takes part in from a light
// view and says which are settled and since when. accepts are my accepts
// as they are about to be written.
func (e *Engine) threadTimes(ctx context.Context, accepts []model.Accept) map[string]threadInfo {
	v := e.lightView(ctx)
	out := map[string]threadInfo{}
	for _, t := range rules.Threads(v) {
		out[t.ID] = threadInfo{settled: t.Settled, settledAt: settledAt(t, accepts)}
	}
	return out
}

// settledAt estimates when a settled thread settled: its last message, my
// last accept in it, or its deadline when it fell back.
func settledAt(t model.ThreadState, accepts []model.Accept) time.Time {
	var at time.Time
	for _, m := range t.Messages {
		if m.At.After(at) {
			at = m.At
		}
	}
	for _, a := range accepts {
		if a.Thread == t.ID && a.At.After(at) {
			at = a.At
		}
	}
	if t.FellBack && t.Deadline.After(at) {
		at = t.Deadline
	}
	return at
}

// trimOutbox drops my messages and accepts of threads settled more than
// closedRetention ago, and of threads that no longer exist and saw no
// activity of mine for orphanRetention. Needs are never dropped here.
func trimOutbox(ob *model.Outbox, threads map[string]threadInfo, now time.Time) {
	last := map[string]time.Time{}
	for _, m := range ob.Messages {
		if m.At.After(last[m.Thread]) {
			last[m.Thread] = m.At
		}
	}
	for _, a := range ob.Accepts {
		if a.At.After(last[a.Thread]) {
			last[a.Thread] = a.At
		}
	}
	drop := func(thread string) bool {
		if t, ok := threads[thread]; ok {
			return t.settled && now.Sub(t.settledAt) > closedRetention
		}
		return now.Sub(last[thread]) > orphanRetention
	}
	msgs := ob.Messages[:0:0]
	for _, m := range ob.Messages {
		if !drop(m.Thread) {
			msgs = append(msgs, m)
		}
	}
	accepts := ob.Accepts[:0:0]
	for _, a := range ob.Accepts {
		if !drop(a.Thread) {
			accepts = append(accepts, a)
		}
	}
	ob.Messages, ob.Accepts = msgs, accepts
}

// shrinkOutbox keeps only my newest keepPerThread messages per thread, for
// an outbox over bus.MaxWriteDoc. It reports whether anything was dropped.
func shrinkOutbox(ob *model.Outbox) bool {
	idx := make([]int, len(ob.Messages))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return ob.Messages[idx[a]].At.After(ob.Messages[idx[b]].At) })
	kept := map[string]int{}
	keep := make([]bool, len(ob.Messages))
	for _, i := range idx {
		t := ob.Messages[i].Thread
		if kept[t] < keepPerThread {
			kept[t]++
			keep[i] = true
		}
	}
	msgs := ob.Messages[:0:0]
	for i, m := range ob.Messages {
		if keep[i] {
			msgs = append(msgs, m)
		}
	}
	shrunk := len(msgs) < len(ob.Messages)
	ob.Messages = msgs
	return shrunk
}

// itemDigests summarizes items for rememberItems.
func itemDigests(items []model.WorkItem) map[model.ItemID]string {
	out := make(map[model.ItemID]string, len(items))
	for _, it := range items {
		out[it.ID] = itemDigest(it)
	}
	return out
}

func itemDigest(it model.WorkItem) string {
	return string(it.Status) + "\x00" + it.Intent + "\x00" + it.Branch + "\x00" + it.PR + "\x00" + it.Head
}

// rememberItems stores new and changed items in the ledger's history, so
// why can explain commits after an item left state.json.
func (e *Engine) rememberItems(ctx context.Context, before map[model.ItemID]string, items []model.WorkItem) {
	if e.led.ReadOnly() {
		return
	}
	for _, it := range items {
		if d, ok := before[it.ID]; ok && d == itemDigest(it) {
			continue
		}
		if err := e.led.RememberItem(ctx, e.me.ID, it); err != nil {
			e.log.Debug("remember item", "item", it.ID, "err", err)
		}
	}
}
