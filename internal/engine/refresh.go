package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

// Polling constants (docs/SPEC.md, sections 6.2 and 8).
const (
	// verbRefreshAfter: a verb refreshes when the last fetch is older.
	verbRefreshAfter = 5 * time.Second
	// verbRefreshBudget bounds a verb's refresh; past it the verb uses the
	// cached state and reports its age.
	verbRefreshBudget = 2 * time.Second
	// maxBackoff caps the poll interval after errors.
	maxBackoff = 5 * time.Minute
	// pollJitter is the relative jitter of every poll interval.
	pollJitter = 0.2
	// tickSlack is how late a poll may come because Tick runs every few
	// seconds rather than exactly when a poll is due.
	tickSlack = 5 * time.Second
	// refsNamespace is the namespace bus.State.Refs covers.
	refsNamespace = bus.Prefix
	// outsideBudget bounds the footprints of outside branches per tick.
	outsideBudget = 3 * time.Second
)

// pollState is the polling schedule and the remote's reachability. It has
// its own lock because Tick fetches without holding e.mu.
type pollState struct {
	mu           sync.Mutex
	lastFetch    time.Time // when the fetch whose result we use ran
	fingerprint  string    // tracking refs fingerprint after it
	next         time.Time // when the next background poll is due
	failures     int       // consecutive failed fetches
	retryAt      time.Time // after a failure, verbs fetch again only from then on
	slow         bool      // the last fetch ran out of its budget
	offlineSince *time.Time
	needFlush    bool      // own refs were written while offline
	outsideAt    time.Time // last fetch of outside branches
}

// verbMayFetch reports whether a verb may spend its refresh budget on a
// fetch: not while the backoff after a failed fetch lasts.
func (p *pollState) verbMayFetch(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures == 0 || !now.Before(p.retryAt)
}

// isSlow reports whether the last fetch ran out of its budget.
func (p *pollState) isSlow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slow
}

// requestFlush asks the next successful poll to flush own refs.
func (p *pollState) requestFlush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.needFlush = true
}

// outsideDue reports whether outside branches should be fetched again, and
// if so books the fetch, so concurrent ticks do not fetch twice.
func (p *pollState) outsideDue(now time.Time, every time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.outsideAt.IsZero() && now.Sub(p.outsideAt) < every {
		return false
	}
	p.outsideAt = now
	return true
}

// markOffline records a network failure of a write or a fetch.
func (p *pollState) markOffline(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offlineSince == nil {
		t := now
		p.offlineSince = &t
	}
	p.needFlush = true
}

// wroteOK records a write the remote accepted.
func (p *pollState) wroteOK() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offlineSince != nil {
		p.needFlush = true
	}
}

// snapshot returns the fields results report.
func (p *pollState) snapshot() (lastFetch time.Time, offlineSince *time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offlineSince != nil {
		t := *p.offlineSince
		offlineSince = &t
	}
	return p.lastFetch, offlineSince
}

func (p *pollState) due(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !now.Before(p.next)
}

// takeFlush reports whether own refs need a flush, and clears the flag.
func (p *pollState) takeFlush() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.needFlush && p.offlineSince == nil
	if f {
		p.needFlush = false
	}
	return f
}

// pollDelay returns the wait before the next poll: the poll interval, or
// after failures an exponential backoff from it capped at maxBackoff, with
// ±20% jitter. r is a uniform random number in [0, 1).
func pollDelay(interval time.Duration, failures int, r float64) time.Duration {
	d := interval
	for i := 0; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	if failures > 0 && d > maxBackoff {
		d = maxBackoff
	}
	d = time.Duration(float64(d) * (1 + pollJitter*(2*r-1)))
	if failures > 0 && d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// needsRefresh is the verb-time refresh rule: refresh when the last
// successful fetch is older than verbRefreshAfter.
func needsRefresh(lastFetch, now time.Time) bool {
	return lastFetch.IsZero() || now.Sub(lastFetch) > verbRefreshAfter
}

// fetch runs one poll, bus.Fetch, and updates the schedule. On success the
// next poll is one jittered interval away, the backoff ends and an end of
// offline time schedules a flush. Every failure backs verbs off
// (verbMayFetch) for an exponential delay. A network failure also sets
// offlineSince and delays the background poll by the same backoff; a fetch
// cut short by ctx (a verb's budget) marks the remote slow, not gone, and
// leaves the background poll on schedule, since Tick has no such budget.
// Only one fetch runs at a time; waiting for it honors ctx. fetch does not
// need e.mu, and callers holding it must pass a budget.
func (e *Engine) fetch(ctx context.Context) (bus.FetchResult, error) {
	select {
	case e.pollSem <- struct{}{}:
	case <-ctx.Done():
		return bus.FetchResult{}, ctx.Err()
	}
	defer func() { <-e.pollSem }()
	e.fetches.Add(1)
	res, err := e.bus.Fetch(ctx)
	now := e.now()
	p := &e.poll
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		if res.At.IsZero() {
			res.At = now
		}
		p.lastFetch, p.fingerprint = res.At, res.Fingerprint
		p.failures, p.retryAt, p.slow = 0, time.Time{}, false
		if p.offlineSince != nil {
			p.offlineSince, p.needFlush = nil, true
		}
		p.next = now.Add(pollDelay(e.cfg.PollInterval, 0, rand.Float64()))
		return res, nil
	}
	p.failures++
	backoff := now.Add(pollDelay(e.cfg.PollInterval, p.failures, rand.Float64()))
	p.retryAt = backoff
	p.slow = ctx.Err() != nil
	switch {
	case p.slow:
		return res, fmt.Errorf("engine: poll: %w", ctx.Err())
	case gitx.IsNetwork(err):
		if p.offlineSince == nil {
			t := now
			p.offlineSince = &t
		}
	}
	p.next = backoff
	return res, fmt.Errorf("engine: poll: %w", err)
}

// applyFetch makes a successful fetch visible: it reloads the shared state
// when the fingerprint changed and records the ref observations lease
// judgements use. Main may have moved even when gitlet's refs did not, so
// the memoized main oid is dropped. Callers hold e.mu.
func (e *Engine) applyFetch(ctx context.Context, res bus.FetchResult) error {
	e.mainOIDs = ""
	if e.shared == nil || res.Fingerprint == "" || res.Fingerprint != e.loadedFP {
		if err := e.reload(ctx); err != nil {
			return err
		}
		e.loadedFP = res.Fingerprint
	}
	e.observe(ctx, res.At)
	return nil
}

// flushIfNeeded pushes own refs written while the remote was unreachable
// (bus.Flush), once a poll has succeeded again. Tick runs it within its
// budget; verbs leave it to Tick. Callers hold e.mu.
func (e *Engine) flushIfNeeded(ctx context.Context) {
	if e.readOnly() || !e.poll.takeFlush() {
		return
	}
	if err := e.bus.Flush(ctx); err != nil {
		e.log.Debug("flush", "err", err)
		if gitx.IsNetwork(err) || errBudget(err) {
			e.poll.markOffline(e.now())
		} else {
			e.poll.requestFlush()
		}
	}
}

// reload decodes the shared state from the refs (bus.Load), keeping what
// this agent wrote since. Callers hold e.mu.
func (e *Engine) reload(ctx context.Context) error {
	st, err := e.bus.Load(ctx)
	if err != nil {
		return fmt.Errorf("engine: load shared state: %w", err)
	}
	e.shared = st
	e.ensureShared()
	e.mainOIDs = ""
	return nil
}

// observe records a full refresh of the refs namespace at fetchedAt in the
// ledger (docs/SPEC.md, section 8): refs keep their first-seen time while
// their oid stays, and the last fetch time advances. After a gap of more
// than three poll intervals since the last fetch (the machine slept, or the
// remote was away) every first-seen time restarts first, so lapses are only
// ever judged on time this clone actually watched.
func (e *Engine) observe(ctx context.Context, fetchedAt time.Time) {
	// Re-read: another process of this clone may have observed meanwhile.
	prev, err := e.led.Observed(ctx, refsNamespace)
	if err != nil {
		prev = e.obs
	} else {
		e.obs, e.obsLoaded = prev, true
	}
	if !fetchedAt.After(prev.LastFetch) || e.led.ReadOnly() {
		return
	}
	if !prev.LastFetch.IsZero() && fetchedAt.Sub(prev.LastFetch) > 3*e.cfg.PollInterval {
		if err := e.led.ResetFirstSeen(ctx, fetchedAt); err != nil {
			e.log.Debug("reset first seen", "err", err)
		}
	}
	refs := map[string]string{}
	if e.shared != nil {
		refs = e.shared.Refs
	}
	obs, err := e.led.ObserveRefs(ctx, refsNamespace, refs, fetchedAt)
	if err != nil {
		e.log.Debug("observe refs", "err", err)
		return
	}
	e.obs, e.obsLoaded = obs, true
}

// observation returns the ref observations, loading them from the ledger
// once (another process may have recorded them).
func (e *Engine) observation(ctx context.Context) ledger.Observation {
	if !e.obsLoaded {
		if obs, err := e.led.Observed(ctx, refsNamespace); err == nil {
			e.obs, e.obsLoaded = obs, true
		}
	}
	return e.obs
}

// refreshForVerb is the verb-time refresh rule (docs/SPEC.md, section 6.2):
// when the last successful fetch is older than five seconds the verb polls
// with a two-second budget; otherwise, or when the budget runs out or the
// remote is unreachable, it uses the cached state. After a failed fetch,
// verbs do not poll again until its backoff has passed, so a slow or
// unreachable remote costs one budget, not one per verb; a successful poll
// (Tick's) ends the backoff. Results report the state's age (stateAge),
// offlineSince and a warning. It loads the shared state when nothing is
// cached yet. Errors mean the local refs could not be read. beginVerb
// calls it; verbs need not.
func (e *Engine) refreshForVerb(ctx context.Context) error {
	last, _ := e.poll.snapshot()
	if now := e.now(); needsRefresh(last, now) && e.poll.verbMayFetch(now) {
		bctx, cancel := context.WithTimeout(ctx, verbRefreshBudget)
		res, err := e.fetch(bctx)
		cancel()
		if err == nil {
			return e.applyFetch(ctx, res)
		}
		e.log.Debug("verb refresh", "err", err)
	}
	if e.shared == nil {
		return e.reload(ctx)
	}
	return nil
}

// fetchFresh is how recent the last fetch must be for lapse judgements:
// one poll interval, plus its jitter and Tick's granularity, so a lapse
// does not flicker between two polls.
func fetchFresh(poll time.Duration) time.Duration {
	return poll + time.Duration(float64(poll)*pollJitter) + tickSlack
}

// lapse is the judgement of one ref on observed time.
type lapse struct {
	Lapsed bool
	Since  time.Time     // when its current oid was first seen (idle since)
	For    time.Duration // how long it has been lapsed: observed - lease
}

// judgeLapse applies docs/SPEC.md, section 8: a ref has lapsed when
// lastFetch - firstSeen >= lease and the last fetch is recent (fresh), so a
// clone that is offline or asleep never declares others gone.
func judgeLapse(firstSeen, lastFetch, now time.Time, lease, fresh time.Duration) lapse {
	if firstSeen.IsZero() || lastFetch.IsZero() || lease <= 0 {
		return lapse{}
	}
	if now.Sub(lastFetch) > fresh {
		return lapse{}
	}
	observed := lastFetch.Sub(firstSeen)
	if observed < lease {
		return lapse{}
	}
	return lapse{Lapsed: true, Since: firstSeen, For: observed - lease}
}

// refLapse judges a canonical ref (bus.StateRef, bus.ClaimRef, ...) with
// the lease its document states (seconds; 0 means the configured lease).
func (e *Engine) refLapse(ctx context.Context, ref string, leaseSecs int, now time.Time) lapse {
	obs := e.observation(ctx)
	lease := time.Duration(leaseSecs) * time.Second
	if lease <= 0 {
		lease = e.cfg.Lease
	}
	return judgeLapse(obs.FirstSeen[ref], obs.LastFetch, now, lease, fetchFresh(e.cfg.PollInterval))
}

// stateAge returns how old the shared state is ("" when it was never
// fetched) and since when the remote is unreachable.
func (e *Engine) stateAge(ctx context.Context, now time.Time) (string, *time.Time) {
	last, offline := e.poll.snapshot()
	if obs := e.observation(ctx); obs.LastFetch.After(last) {
		last = obs.LastFetch
	}
	if last.IsZero() {
		return "", offline
	}
	return fmtAge(now.Sub(last)), offline
}

// fmtAge formats a duration the way results show ages: 4s, 2m, 1h5m, 3d.
func fmtAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// fetchOutside fetches every branch head (bus.FetchOutside) without e.mu.
func (e *Engine) fetchOutside(ctx context.Context) ([]gitx.RefInfo, error) {
	refs, err := e.bus.FetchOutside(ctx, e.cfg.OutsideWindow, e.cfg.OutsideMax)
	if err != nil {
		return nil, fmt.Errorf("engine: fetch outside branches: %w", err)
	}
	return refs, nil
}

// updateOutside turns the last fetched branch heads into outside branches
// for the rules: branches that are no gitlet item, with their footprint
// against the merge base with main. Footprints come from the ledger cache
// when present; new ones are computed within outsideBudget, and the rest
// wait for the next tick. Callers hold e.mu.
func (e *Engine) updateOutside(ctx context.Context) {
	if len(e.outsideRefs) == 0 {
		e.outside = nil
		return
	}
	main := e.mainOID(ctx)
	if main == "" {
		return
	}
	items := map[string]bool{}
	if e.shared != nil {
		for _, st := range e.shared.Agents {
			for _, it := range st.Items {
				if it.Branch != "" {
					items[it.Branch] = true
				}
			}
		}
	}
	known := map[string]rules.Outside{}
	for _, o := range e.outside {
		known[o.Branch+"\x00"+o.Head] = o
	}
	bctx, cancel := context.WithTimeout(ctx, outsideBudget)
	defer cancel()
	var out []rules.Outside
	for _, ref := range e.outsideRefs {
		branch := branchOfRef(ref.Name)
		if branch == "" || branch == e.cfg.Main || items[branch] || ref.OID == main {
			continue
		}
		if o, ok := known[branch+"\x00"+ref.OID]; ok {
			out = append(out, o)
			continue
		}
		if bctx.Err() != nil {
			continue
		}
		base, err := e.bg.MergeBase(bctx, main, ref.OID)
		if err != nil {
			continue
		}
		fp, err := e.footprint(bctx, base, ref.OID)
		if err != nil {
			e.log.Debug("outside footprint", "branch", branch, "err", err)
			continue
		}
		out = append(out, rules.Outside{Branch: branch, Head: ref.OID, Footprint: fp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	e.outside = out
}

// branchOfRef returns the branch name of a fetched head:
// refs/gitlet-heads/<remote>/<branch> or refs/heads/<branch>.
func branchOfRef(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "refs/gitlet-heads/"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[i+1:]
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return rest
	}
	return ""
}

// mainOID returns gitlet's view of main: gitlet's main ref, else the
// remote-tracking branch, the local branch, or the oid the last load saw.
// "" when none exists. Memoized until the next fetch or reload.
func (e *Engine) mainOID(ctx context.Context) string {
	if e.mainOIDs != "" {
		return e.mainOIDs
	}
	var refs []string
	if r := e.bus.MainRef(); r != "" {
		refs = append(refs, r)
	}
	if e.cfg.Remote != "" {
		refs = append(refs, "refs/remotes/"+e.cfg.Remote+"/"+e.cfg.Main)
	}
	refs = append(refs, "refs/heads/"+e.cfg.Main)
	for _, r := range refs {
		if oid, err := e.bg.RevParse(ctx, r+"^{commit}"); err == nil && oid != "" {
			e.mainOIDs = oid
			return oid
		}
	}
	if e.shared != nil {
		e.mainOIDs = e.shared.Main
	}
	return e.mainOIDs
}

// errBudget reports whether err is a context deadline or cancellation.
func errBudget(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// peerIDs returns every other agent in the shared state, sorted.
func (e *Engine) peerIDs() []model.AgentID {
	if e.shared == nil {
		return nil
	}
	seen := map[model.AgentID]bool{}
	for id := range e.shared.Agents {
		seen[id] = true
	}
	for id := range e.shared.Outboxes {
		seen[id] = true
	}
	delete(seen, e.me.ID)
	ids := make([]model.AgentID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
