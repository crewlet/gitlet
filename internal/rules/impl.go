package rules

import (
	"cmp"
	"slices"

	"github.com/crewlet/gitlet/internal/model"
)

// evaluate runs every rule of docs/SPEC.md, section 10, for each of my
// items. Rules that match a broad item record it, and broadNotes runs last
// for each item so it can emit one broad note instead.
func evaluate(v View) []model.Note {
	c := newCtx(v)
	for _, m := range c.mine {
		c.calleeChanged(m)
		c.callersAffected(m)
		c.signatureClash(m)
		c.bodyOverlap(m)
		c.predictedConflict(m)
		c.fileOverlap(m)
		c.hotFiles(m)
		c.migrations(m)
		c.needed(m)
		c.ready(m)
		c.drift(m)
		c.diverged(m)
		c.stillWaiting(m)
		c.lapsedOwners(m)
		c.duplicates(m)
		c.outsideBranches(m)
		c.stale(m)
		c.newCallers(m)
		c.untitled(m)
		c.broadNotes(m)
	}
	c.messageNotes()
	c.agreementNotes()
	c.sameWorktree()
	return c.sortedNotes()
}

func threads(v View) []model.ThreadState {
	c := newCtx(v)
	out := make([]model.ThreadState, 0, len(c.threads))
	for _, t := range c.threads {
		out = append(out, t.st)
	}
	return out
}

// decisions returns my actions for settled signature threads, then the
// needs I still have to record under the agreement that moved a claim to a
// new epoch (its thread is no longer current once the epoch changed).
func decisions(v View) []Decision {
	c := newCtx(v)
	var out []Decision
	decided := map[string]bool{}
	for _, t := range c.threads {
		if t.st.Kind != model.ThreadSignature || !t.st.Settled {
			continue
		}
		decided[t.key] = true
		if d := c.decide(t); len(d.Actions) > 0 {
			out = append(out, d)
		}
	}
	for _, k := range sortedKeys(c.prev) {
		if t := c.pendingPrev(k); t != nil && !decided[k] {
			if d := c.decide(t); len(d.Actions) > 0 {
				out = append(out, d)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Decision) int {
		return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Thread, b.Thread))
	})
	return out
}

func autoClaims(v View) []AutoClaim { return newCtx(v).autoClaims() }

func releases(v View) []Release { return newCtx(v).releases() }
