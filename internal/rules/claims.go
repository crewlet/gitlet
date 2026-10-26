package rules

import (
	"fmt"

	"github.com/crewlet/gitlet/internal/model"
)

// autoClaims implements AutoClaims (gitlet.autoClaim). Only a resolved,
// breaking change of a signature counts: unresolved ref matches never
// trigger a claim, and removed or renamed symbols have no new signature to
// publish. A symbol is skipped when I need it (I deferred) or a settled
// thread gave it to someone else.
func (c *ctx) autoClaims() []AutoClaim {
	if !c.lim.AutoClaim {
		return nil
	}
	done := map[string]bool{}
	var out []AutoClaim
	for _, m := range c.mine {
		for _, k := range m.keys {
			ch := m.changes[k]
			if done[k] || ch.Change != model.ChangeSignature || !ch.Breaking {
				continue
			}
			if _, claimed := c.claim(k); claimed || c.hasMyNeed(k) {
				continue
			}
			if t := c.sig[k]; t != nil && t.st.Settled && t.st.Owner != "" && t.st.Owner != c.v.Me {
				continue
			}
			reason := c.autoReason(m, k, ch)
			if reason == "" {
				continue
			}
			done[k] = true
			sym := ch.Symbol
			sym.Key = k
			out = append(out, AutoClaim{Symbol: sym, Item: m.id(), Reason: reason})
		}
	}
	return out
}

// autoReason says why my breaking change of k should be claimed: it clashes
// with another item, or another item's changed code references it through
// a resolved module.
func (c *ctx) autoReason(m *entry, k string, ch model.SymbolChange) string {
	if ct := c.contests[k]; ct != nil {
		if rivals := ct.rivals(m); len(rivals) > 0 {
			return fmt.Sprintf("breaking signature change clashes with %s", rivals[0].e.agent)
		}
	}
	old := oldSymbol(ch)
	for _, o := range c.others(m) {
		if _, resolved := c.matching(o.item.Footprint, old); resolved {
			return fmt.Sprintf("breaking signature change meets references in %s's item %s", o.agent, o.id())
		}
	}
	return ""
}

// releases implements Releases.
func (c *ctx) releases() []Release {
	items := map[model.ItemID]model.WorkItem{}
	for _, it := range c.v.Mine.Items {
		if _, dup := items[it.ID]; !dup {
			items[it.ID] = it
		}
	}
	// A state with no agent and no items was not loaded; judging items gone
	// from it would drop every claim.
	loaded := c.v.Mine.Agent.ID != "" || len(c.v.Mine.Items) > 0
	var out []Release
	for _, k := range sortedKeys(c.v.Claims) {
		cv := c.v.Claims[k]
		if cv.Claim.Owner != c.v.Me {
			continue
		}
		it, known := items[cv.Claim.Item]
		reason := ""
		switch {
		case !known && loaded:
			reason = "its item is gone"
		case known && it.Status == model.ItemLanded:
			reason = "its item landed"
		case known && it.Status == model.ItemAbandoned:
			reason = "its item was abandoned"
		case cv.Landed:
			reason = "its contract landed on main"
		case known && cv.Claim.Auto && cv.Claim.Contract == nil && !c.stillChanges(cv.Claim.Item, k, cv.Claim.Symbol):
			reason = "automatic claim, and your work no longer changes its signature"
		}
		if reason != "" {
			out = append(out, Release{Key: k, OID: cv.OID, Reason: reason})
		}
	}
	return out
}

// stillChanges reports whether my item changes the signature of key, or may
// still: a claimed symbol's file that my footprint tracks at file level only,
// as one that does not parse mid-edit, tells nothing about its symbols.
func (c *ctx) stillChanges(item model.ItemID, key string, sym model.Symbol) bool {
	e := c.byItem[item]
	if e == nil || !e.mine {
		return false
	}
	if ch, ok := e.changes[key]; ok && ch.SignatureChanging() {
		return true
	}
	for _, f := range e.item.Footprint.Files {
		if !f.Parsed && sym.Path != "" && f.Path == sym.Path {
			return true
		}
	}
	return false
}
