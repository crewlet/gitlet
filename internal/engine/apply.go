package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

const (
	// reapEvery is how often a server looks for long-gone agents.
	reapEvery = time.Hour
	// observerReprobe is how often observer mode re-probes the remote.
	observerReprobe = time.Hour
)

// applyDecisions carries out what the rules decided for this agent
// (docs/SPEC.md, sections 10 and 11): the outcome of settled signature
// threads (claim, transfer with epoch + 1, need, race), automatic claims
// (gitlet.autoClaim) and the release of claims that should go. Each action
// is idempotent: the rules keep returning it until the shared state shows
// it done, so failures are left for the next tick. Callers hold e.mu.
func (e *Engine) applyDecisions(ctx context.Context, v rules.View) error {
	if e.readOnly() {
		return nil
	}
	var errs []error
	keep := func(err error) {
		if err != nil && !errors.Is(err, bus.ErrClaimLost) && !errors.Is(err, bus.ErrUnreachable) &&
			!gitx.IsNetwork(err) && !errBudget(err) {
			errs = append(errs, err)
		}
	}
	for _, d := range rules.Decisions(v) {
		for _, a := range d.Actions {
			keep(e.applyAction(ctx, d, a))
		}
	}
	for _, ac := range rules.AutoClaims(v) {
		if _, held := e.claimFor(ac.Symbol.Key); held {
			continue
		}
		_, _, err := e.createClaim(ctx, e.newClaim(ac.Symbol, ac.Item, true, nil), "")
		keep(err)
	}
	for _, r := range rules.Releases(v) {
		keep(e.deleteClaim(ctx, r.Key, r.OID))
	}
	return errors.Join(errs...)
}

// applyAction carries out one action of a settled thread.
func (e *Engine) applyAction(ctx context.Context, d rules.Decision, a rules.Action) error {
	cv, held := e.claimFor(d.Key)
	switch a {
	case rules.ActClaim:
		// I own the outcome. A holder that is not me hands it over itself.
		if d.Owner != e.me.ID || held {
			return nil
		}
		_, _, err := e.createClaim(ctx, e.newClaim(d.Symbol, e.myItemFor(d.Key, d.OwnerItem), false, d.Requirements), "")
		return err
	case rules.ActRace:
		if held {
			return nil
		}
		_, _, err := e.createClaim(ctx, e.newClaim(d.Symbol, e.myItemFor(d.Key, ""), false, d.Requirements), "")
		return err
	case rules.ActTransfer:
		if !held || cv.Claim.Owner != e.me.ID || d.Owner == "" || d.Owner == e.me.ID {
			return nil
		}
		if d.ClaimOID != "" && d.ClaimOID != cv.OID {
			return nil // the claim moved since the rules looked; they will decide again
		}
		c := cv.Claim
		c.Owner, c.Item = d.Owner, d.OwnerItem
		c.Epoch = cv.Claim.Epoch + 1
		c.Auto = false
		c.Contract = nil // the new owner publishes its own
		c.Requirements = mergeRequirements(c.Requirements, d.Requirements)
		c.LeaseSeconds = leaseSeconds(e.cfg.Lease)
		c.UpdatedAt = e.now().UTC()
		_, err := e.updateClaim(ctx, c, cv.OID, "")
		return err
	case rules.ActNeed:
		item := e.myItemFor(d.Key, "")
		if item == "" {
			return nil
		}
		_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
			for i, n := range ob.Needs {
				if n.Key == d.Key {
					if n.Requirement != "" || d.Requirement == "" {
						return errNoChange
					}
					ob.Needs[i].Requirement = d.Requirement
					return nil
				}
			}
			ob.Needs = append(ob.Needs, model.Need{
				ID:          model.NewMessageID(),
				Key:         d.Key,
				Name:        d.Symbol.Ref(),
				Item:        item,
				Requirement: d.Requirement,
				CreatedAt:   e.now().UTC(),
			})
			return nil
		})
		return err
	}
	return nil
}

// myItemFor returns the item of mine a symbol belongs to: prefer, if it
// is mine and open; else the item whose footprint changes the symbol; else
// the active item. "" when I have no open item.
func (e *Engine) myItemFor(key string, prefer model.ItemID) model.ItemID {
	items := e.myState().Items
	if i := itemIndex(items, prefer); prefer != "" && i >= 0 && items[i].Status.Open() {
		return prefer
	}
	if it, _, ok := e.myChange(key); ok {
		return it.ID
	}
	if it, ok := e.activeItem(); ok {
		return it.ID
	}
	return ""
}

// mergeRequirements combines a claim's requirements with new ones, one per
// agent, the newer text winning, sorted by agent.
func mergeRequirements(old, new []model.Requirement) []model.Requirement {
	by := map[model.AgentID]string{}
	for _, r := range old {
		by[r.From] = r.Text
	}
	for _, r := range new {
		by[r.From] = r.Text
	}
	out := make([]model.Requirement, 0, len(by))
	for from, text := range by {
		out = append(out, model.Requirement{From: from, Text: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// needRequirements picks the requirement each other agent states in its
// needs on one claim key: one per agent, from its newest need with a
// requirement text. The holder's own needs and needs without text are left
// out. The result is sorted by agent.
func needRequirements(me model.AgentID, needs []agentNeed) []model.Requirement {
	newest := map[model.AgentID]model.Need{}
	for _, an := range needs {
		if an.Agent == me || strings.TrimSpace(an.Need.Requirement) == "" {
			continue
		}
		cur, ok := newest[an.Agent]
		if !ok || an.Need.CreatedAt.After(cur.CreatedAt) ||
			an.Need.CreatedAt.Equal(cur.CreatedAt) && an.Need.ID > cur.ID {
			newest[an.Agent] = an.Need
		}
	}
	out := make([]model.Requirement, 0, len(newest))
	for from, n := range newest {
		out = append(out, model.Requirement{From: from, Text: n.Requirement})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// claimRequirements returns the requirements a claim of mine carries once
// fromNeeds are merged in (mergeRequirements: one per agent, the needs'
// text winning), and whether that differs from what it carries now,
// compared as sets, so a claim is never rewritten for nothing.
func claimRequirements(c model.Claim, fromNeeds []model.Requirement) ([]model.Requirement, bool) {
	merged := mergeRequirements(c.Requirements, fromNeeds)
	return merged, !slices.Equal(merged, mergeRequirements(c.Requirements, nil))
}

// renewDue reports whether a lease written at last needs renewing: a third
// of it has passed (docs/SPEC.md, section 8).
func renewDue(last, now time.Time, lease time.Duration) bool {
	return now.Sub(last) >= lease/3
}

// isActive reports whether an agent whose last activity was at last is
// active: a gitlet call or a work-tree change within the lease.
func isActive(last, now time.Time, lease time.Duration) bool {
	return !last.IsZero() && now.Sub(last) <= lease
}

// renew keeps this agent's leases while it is active: once a third of the
// lease has passed, the heartbeat (a state write) and every claim it holds
// are renewed. It also merges the requirements peers state in their needs
// into the claims it holds (claimRequirements); a claim whose requirements
// changed is written at once, and with the renewals when they are due. All
// claim writes go in one push (bus.UpdateClaims), each with a lease on the
// oid seen. An idle agent writes nothing and lets its leases lapse, which
// frees its claims for takeover; that includes requirement merges, since
// any write would keep a claim alive.
func (e *Engine) renew(ctx context.Context) error {
	if e.readOnly() {
		return nil
	}
	now, lease := e.now(), e.cfg.Lease
	if !isActive(e.lastActivity(ctx), now, lease) {
		return nil
	}
	var errs []error
	if st := e.myState(); !st.Heartbeat.IsZero() && renewDue(st.Heartbeat, now, lease) {
		if _, err := e.mutateState(ctx, func(*model.AgentState) error { return nil }); err != nil {
			errs = append(errs, err)
		}
	}
	var ups []bus.ClaimUpdate
	for _, cv := range e.myClaims() {
		c := cv.Claim
		reqs, reqsChanged := claimRequirements(c, needRequirements(e.me.ID, e.needsOn(c.Key)))
		if !reqsChanged && !renewDue(c.UpdatedAt, now, lease) {
			continue
		}
		if reqsChanged {
			c.Requirements = reqs
		}
		c.UpdatedAt, c.LeaseSeconds = now.UTC(), leaseSeconds(lease)
		parent := ""
		if c.Contract != nil {
			parent = c.Contract.StubCommit // a claim with a contract keeps its stub as parent
		}
		ups = append(ups, bus.ClaimUpdate{Claim: c, SeenOID: cv.OID, Parent: parent})
	}
	if len(ups) == 0 {
		return errors.Join(errs...)
	}
	outs, err := e.bus.UpdateClaims(ctx, ups)
	if err != nil {
		if gitx.IsNetwork(err) || errors.Is(err, bus.ErrUnreachable) {
			e.poll.markOffline(now)
			return errors.Join(errs...)
		}
		return errors.Join(append(errs, fmt.Errorf("engine: renew claims: %w", err))...)
	}
	lost := false
	for _, o := range outs {
		switch {
		case o.Err == nil:
			view := o.View
			e.setClaim(o.Key, &view)
		case errors.Is(o.Err, bus.ErrClaimLost):
			lost = true // taken over after a lapse: the rules tell the agent
		default:
			e.log.Debug("renew claim", "key", o.Key, "err", o.Err)
		}
	}
	if lost {
		if err := e.reload(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reap deletes, at most hourly and only while online, the refs of agents
// whose state ref has been lapsed for gitlet.reapAfter of observed time,
// and lapsed claims that nobody needs after the same period, each with a
// lease on the oid seen (docs/SPEC.md, section 6.4).
func (e *Engine) reap(ctx context.Context) error {
	now := e.now()
	if e.readOnly() || e.shared == nil || (!e.lastReap.IsZero() && now.Sub(e.lastReap) < reapEvery) {
		return nil
	}
	if _, offline := e.poll.snapshot(); offline != nil {
		return nil
	}
	e.lastReap = now
	gone := func(ref string, lease int) bool {
		lp := e.refLapse(ctx, ref, lease, now)
		return lp.Lapsed && lp.For >= e.cfg.ReapAfter
	}
	refs := map[string]string{}
	for _, id := range e.peerIDs() {
		ref, lease := bus.OutboxRef(id), 0
		if st, ok := e.shared.Agents[id]; ok {
			ref, lease = bus.StateRef(id), st.LeaseSeconds
		}
		if !gone(ref, lease) {
			continue
		}
		for _, r := range []string{bus.StateRef(id), bus.OutboxRef(id)} {
			if oid := e.shared.Refs[r]; oid != "" {
				refs[r] = oid
			}
		}
		for r, oid := range e.knownWips(ctx, id) {
			refs[r] = oid
		}
	}
	needed := map[string]bool{}
	for _, ob := range e.shared.Outboxes {
		for _, n := range ob.Needs {
			needed[n.Key] = true
		}
	}
	for key, cv := range e.shared.Claims {
		if cv.Claim.Owner != e.me.ID && !needed[key] && gone(bus.ClaimRef(key), cv.Claim.LeaseSeconds) {
			refs[bus.ClaimRef(key)] = cv.OID
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if err := e.bus.Reap(ctx, refs); err != nil {
		return fmt.Errorf("engine: reap: %w", err)
	}
	e.appendEvent(ctx, "reap", refs)
	return e.reload(ctx)
}

// knownWips returns the canonical wip refs of an agent known locally (wips
// are fetched on demand, so this may miss some), with their oids.
func (e *Engine) knownWips(ctx context.Context, agent model.AgentID) map[string]string {
	canonical := bus.WipPrefix + string(agent) + "/"
	prefix := bus.TrackingRef(e.bus.Remote(), canonical)
	got, err := e.bg.ListRefs(ctx, prefix)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(got))
	for name, oid := range got {
		if item, ok := strings.CutPrefix(name, prefix); ok && model.ValidItemID(model.ItemID(item)) {
			out[canonical+item] = oid
		}
	}
	return out
}

// reprobe leaves observer mode when the remote accepts gitlet's refs again,
// checked at most hourly.
func (e *Engine) reprobe(ctx context.Context) {
	if !e.observer.Load() || e.now().Sub(e.observerSince) < observerReprobe {
		return
	}
	e.observerSince = e.now()
	res, err := e.bus.Probe(ctx)
	if err == nil && res.Create {
		e.observer.Store(false)
		e.poll.requestFlush()
		e.log.Info("the remote accepts gitlet's refs again", "agent", e.me.ID)
	}
}

// pruneWips deletes local tracking refs of peers' snapshots whose items are
// no longer open in their agent's state.
func (e *Engine) pruneWips(ctx context.Context) error {
	if e.shared == nil {
		return nil
	}
	keep := map[string]bool{}
	for id, st := range e.shared.Agents {
		for _, it := range st.Items {
			if it.Status.Open() {
				keep[bus.WipRef(id, it.ID)] = true
			}
		}
	}
	if err := e.bus.PruneWips(ctx, keep); err != nil {
		return fmt.Errorf("engine: prune wips: %w", err)
	}
	return nil
}
