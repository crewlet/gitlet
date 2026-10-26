package engine

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
)

// tickBudget bounds the part of Tick that holds the engine lock, so a verb
// arriving meanwhile never waits long. Work cut short continues next tick;
// every step is cached or idempotent.
const tickBudget = 6 * time.Second

// tick does the background work in two phases. First, without the engine
// lock, the network: the poll when it is due (jittered, backing off on
// errors) and the outside branches every gitlet.outsideInterval. Then,
// under the lock and within tickBudget: apply the poll (reload, ref
// observations, flush after being offline), prune peers' stale wip refs,
// outside footprints, a snapshot when the work tree changed and the
// snapshot interval passed (opening a provisional item when needed),
// landing, the rules, their decisions, lease renewal, reaping and the
// observer re-probe. Network failures and exhausted budgets are not errors.
func (e *Engine) tick(ctx context.Context) error {
	var errs []error
	keep := func(err error) {
		if err != nil && !gitx.IsNetwork(err) && !errBudget(err) {
			errs = append(errs, err)
			e.log.Debug("tick", "err", err)
		}
	}
	now := e.now()
	var fetched *bus.FetchResult
	if e.poll.due(now) {
		res, err := e.fetch(ctx)
		if err == nil {
			fetched = &res
		}
		keep(err)
	}
	var outside []gitx.RefInfo
	outsideOK := false
	if e.bus.Remote() != "" && e.poll.outsideDue(now, e.cfg.OutsideInterval) {
		refs, err := e.fetchOutside(ctx)
		outside, outsideOK = refs, err == nil
		keep(err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, tickBudget)
	defer cancel()
	// The gap between ticks runs from the end of the last one to the start
	// of this one, on the wall clock: a tick that waited for a long verb or
	// a slow fetch was not a gap, and Go's monotonic clock stops while the
	// machine sleeps.
	if !e.lastTick.IsZero() && now.Round(0).Sub(e.lastTick) > 3*e.cfg.PollInterval && !e.led.ReadOnly() {
		// The machine slept: time nobody watched must not count toward lapses.
		keep(e.led.ResetFirstSeen(lctx, now))
	}
	defer func() { e.lastTick = e.now().Round(0) }()

	before := e.loadedFP
	switch {
	case fetched != nil:
		keep(e.applyFetch(lctx, *fetched))
		if e.loadedFP != before {
			keep(e.pruneWips(lctx))
		}
	case e.shared == nil:
		keep(e.reload(lctx))
	}
	if e.shared == nil {
		return errors.Join(errs...)
	}
	e.flushIfNeeded(lctx)
	if outsideOK {
		e.outsideRefs = outside
	}
	e.updateOutside(lctx)
	keep(e.snapshot(lctx, snapMode{provisional: true}))
	keep(e.detectLanding(lctx))
	v, _, err := e.evaluate(lctx, tickEvalBudget)
	keep(err)
	keep(e.applyDecisions(lctx, v))
	keep(e.renew(lctx))
	keep(e.reap(lctx))
	e.reprobe(lctx)
	e.trimCaches()
	return errors.Join(errs...)
}
