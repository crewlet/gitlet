package rules

import (
	"fmt"
	"slices"

	"github.com/crewlet/gitlet/internal/model"
)

// calleeChanged: another item changes the signature of, removes, renames or
// moves a symbol my changed code references, and I do not change it myself.
// Suppressed while I have a need on it. act when breaking and resolved,
// info when compatible or matched by method name only.
func (c *ctx) calleeChanged(m *entry) {
	type hit struct {
		o        *entry
		ch       model.SymbolChange
		refs     []model.Ref
		resolved bool
	}
	byKey := map[string][]hit{}
	for _, o := range c.others(m) {
		for _, k := range o.keys {
			ch := o.changes[k]
			if !ch.SignatureChanging() {
				continue
			}
			if _, mineToo := m.changes[k]; mineToo || c.hasNeed(m, k) {
				continue
			}
			refs, resolved := c.matching(m.item.Footprint.Refs, oldSymbol(ch))
			if len(refs) == 0 {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			byKey[k] = append(byKey[k], hit{o, ch, refs, resolved})
		}
	}
	for _, k := range sortedKeys(byKey) {
		hits := byKey[k]
		var cps []cp
		var refs []model.Ref
		anyResolved, anyBreaking := false, false
		for _, h := range hits {
			sev := model.SeverityInfo
			if breaking(h.ch) && h.resolved {
				sev = model.SeverityAct
			}
			cps = append(cps, cp{
				e:    h.o,
				sev:  lower(sev, h.o),
				fact: fmt.Sprintf("%s:%t:%t", outcome(h.ch), breaking(h.ch), h.resolved),
				ev:   describe(who(h.o, len(hits) > 1), h.ch),
			})
			refs = append(refs, h.refs...)
			anyResolved = anyResolved || h.resolved
			anyBreaking = anyBreaking || (breaking(h.ch) && h.resolved)
		}
		var extra []string
		cv, claimed := c.claim(k)
		if claimed && cv.Claim.Contract != nil {
			extra = append(extra, "contract:"+cv.Claim.Contract.SigHash)
		}
		ref := symRef(oldSymbol(hits[0].ch), "")
		n := c.newNote(model.NoteCalleeChanged, m, k, ref)
		withCps(&n, cps, extra...)
		first := cps[0].e.changes[k]
		n.Title = fmt.Sprintf("%s %s %s %s, which your code %s", labels(cps), agree(cps, "is", "are"), verbing(first), ref, usesVerb(refs))
		n.Evidence = append(n.Evidence, "your references: "+sites(refs))
		if claimed && cv.Claim.Contract != nil {
			n.Evidence = append(n.Evidence, fmt.Sprintf("published by %s: %s", cv.Claim.Owner, cv.Claim.Contract.Signature))
		}
		switch {
		case anyBreaking:
			n.Body = "Their change breaks your code. Record a need to build on their published signature, or ask what they are changing."
		case !anyResolved:
			n.Body = "Matched by method name only; check that your code really refers to this symbol."
		default:
			n.Body = "The change keeps old callers working; record a need if you want their published signature."
		}
		n.Options = []model.Option{optNeed(n.ID, ref, "wait for their published signature")}
		c.discussable(&n, m, cps, "ask what they are changing")
		n.Options = append(n.Options, optDismiss(n.ID, "your code is not affected"))
		c.emit(n)
	}
}

// callersAffected: I change the signature of, remove, rename or move a
// symbol that another item's changed code references. act when breaking
// and resolved, info otherwise. Items that also change the symbol meet in
// signature_clash or body_overlap instead, and items that need it in
// needed and ready.
func (c *ctx) callersAffected(m *entry) {
	for _, k := range m.keys {
		ch := m.changes[k]
		if !ch.SignatureChanging() {
			continue
		}
		old := oldSymbol(ch)
		var cps []cp
		for _, o := range c.others(m) {
			if _, theirsToo := o.changes[k]; theirsToo || c.hasNeed(o, k) {
				continue
			}
			refs, resolved := c.matching(o.item.Footprint.Refs, old)
			if len(refs) == 0 {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			sev := model.SeverityInfo
			if breaking(ch) && resolved {
				sev = model.SeverityAct
			}
			prefix := string(o.agent) + " references it"
			if o.mine {
				prefix = "your other task references it"
			}
			cps = append(cps, cp{e: o, sev: lower(sev, o), fact: fmt.Sprint(resolved), ev: []string{prefix + ": " + sites(refs)}})
		}
		if len(cps) == 0 {
			continue
		}
		ref := symRef(old, "")
		n := c.newNote(model.NoteCallersAffected, m, k, ref)
		cv, claimed := c.claim(k)
		var extra []string
		extra = append(extra, fmt.Sprintf("breaking:%t", breaking(ch)))
		if claimed && cv.Claim.Contract != nil {
			extra = append(extra, "contract:"+cv.Claim.Contract.SigHash)
		}
		withCps(&n, cps, extra...)
		n.Evidence = append(describe("", ch), n.Evidence...)
		n.Title = fmt.Sprintf("Your change to %s affects %s, whose code %s it", ref, labels(cps), agree(cps, "uses", "use"))
		cur := symRef(ch.Symbol, ref)
		switch {
		case ch.Change == model.ChangeRemoved || ch.Change == model.ChangeRenamed:
			n.Body = "Tell them before their code stops building."
		case !claimed:
			n.Body = "Claim it and publish the new signature early, so they build against your version."
			n.Options = append(n.Options,
				optClaim(n.ID, cur, "become its owner"),
				optPublish(n.ID, cur, "publish the new signature for them to build on"))
		case cv.Claim.Owner == c.v.Me:
			if cv.Claim.Contract == nil || cv.Claim.Contract.SigHash != ch.Symbol.SigHash {
				n.Body = "You own it: publish the new signature so they can build on it."
				n.Options = append(n.Options, optPublish(n.ID, cur, "publish the new signature for them to build on"))
			} else {
				n.Body = "Your published contract matches; they can use it."
			}
		default:
			n.Body = fmt.Sprintf("%s owns this interface; build on its signature or discuss.", cv.Claim.Owner)
			n.Options = append(n.Options, optNeed(n.ID, ref, "let the owner decide its signature"))
		}
		c.discussable(&n, m, cps, "tell them what changes and ask what they need")
		c.emit(n)
	}
}

// signatureClash: both items change the signature of the same symbol, one
// changes it while the other removes it, or both add it with different
// signatures. act. The note offers the symbol's signature thread, and is
// replaced by an agreement note once the thread settles.
func (c *ctx) signatureClash(m *entry) {
	for _, k := range m.keys {
		ct := c.contests[k]
		t := c.sig[k]
		if ct == nil || t == nil || t.st.Settled || c.pendingPrev(k) != nil {
			continue
		}
		mine, ok := ct.member(m)
		if !ok {
			continue
		}
		var cps []cp
		for _, r := range ct.rivals(m) {
			if r.e.broad() {
				c.touchBroad(m, r.e)
				continue
			}
			cps = append(cps, cp{e: r.e, sev: lower(model.SeverityAct, r.e), fact: r.out})
		}
		if len(cps) == 0 {
			continue
		}
		ref := symRef(ct.sym, "")
		n := c.newNote(model.NoteSignatureClash, m, k, ref)
		withCps(&n, cps, "thread:"+t.st.ID)
		n.Evidence = describe("you", mine.ch)
		for _, x := range cps {
			r, _ := ct.member(x.e)
			n.Evidence = append(n.Evidence, describe(who(x.e, true), r.ch)...)
		}
		if h := ct.holder; h != nil {
			n.Evidence = append(n.Evidence, fmt.Sprintf("claim: held by %s (epoch %d)", h.Claim.Owner, h.Claim.Epoch))
		}
		n.Evidence = append(n.Evidence, fmt.Sprintf("thread %s settles after %d messages each or at %s",
			t.st.ID, c.lim.ThreadMessages, deadlineText(t.st.Deadline)))
		first, _ := ct.member(cps[0].e)
		switch {
		case first.ch.Change == model.ChangeAdded:
			n.Title = fmt.Sprintf("%s also %s %s, with a different signature", labels(cps), agree(cps, "adds", "add"), ref)
		case first.ch.Change == model.ChangeRemoved || first.ch.Change == model.ChangeRenamed:
			n.Title = fmt.Sprintf("%s %s removing %s, whose signature you change", labels(cps), agree(cps, "is", "are"), ref)
		case mine.ch.Change == model.ChangeRemoved || mine.ch.Change == model.ChangeRenamed:
			n.Title = fmt.Sprintf("%s %s changing the signature of %s, which you remove", labels(cps), agree(cps, "is", "are"), ref)
		default:
			n.Title = fmt.Sprintf("%s also %s the signature of %s", labels(cps), agree(cps, "changes", "change"), ref)
		}
		n.Body = "Say what you need from this interface, then propose an owner and a signature and accept. " +
			"Without agreement by the bound, the claim holder owns it."
		n.Thread = t.st.ID
		n.Options = []model.Option{
			optDiscuss(t.st.ID, "say what you need from this interface, or propose an owner and a signature"),
			optNeed(n.ID, ref, "let them own it and build on their published signature"),
			optDismiss(n.ID, "the clash does not matter"),
		}
		c.emit(n)
	}
}

// bodyOverlap: both items change the same symbol, at least one only its
// body. act for the body side when the other changes the signature; warn
// when merge-tree predicts a conflict in that file; info otherwise.
func (c *ctx) bodyOverlap(m *entry) {
	for _, k := range m.keys {
		mc := m.changes[k]
		var cps []cp
		for _, o := range c.others(m) {
			oc, ok := o.changes[k]
			if !ok || !overlaps(mc, oc) {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			sev := model.SeverityInfo
			switch {
			case bodyOnly(mc) && oc.SignatureChanging():
				sev = model.SeverityAct
			case c.conflictIn(m, o, mc.Symbol.Path, oc.Symbol.Path):
				sev = model.SeverityWarn
			}
			cps = append(cps, cp{e: o, sev: lower(sev, o), fact: string(oc.Change) + ":" + oc.Symbol.SigHash})
		}
		if len(cps) == 0 {
			continue
		}
		ref := symRef(oldSymbol(mc), "")
		n := c.newNote(model.NoteBodyOverlap, m, k, ref)
		withCps(&n, cps)
		n.Evidence = describe("you", mc)
		for _, x := range cps {
			n.Evidence = append(n.Evidence, describe(who(x.e, true), x.e.changes[k])...)
		}
		oc := cps[0].e.changes[k]
		switch n.Severity {
		case model.SeverityAct:
			n.Title = fmt.Sprintf("%s %s %s %s, whose body you edit", labels(cps), agree(cps, "is", "are"), verbing(oc), ref)
			n.Body = "Your edit must follow their new signature. Ask what is changing before you go further."
		case model.SeverityWarn:
			n.Title = fmt.Sprintf("%s also %s %s, and your changes are predicted to conflict", labels(cps), agree(cps, "edits", "edit"), ref)
			n.Body = "Agree on who changes what in this symbol, or expect to resolve the conflict by hand."
		default:
			n.Title = fmt.Sprintf("%s also %s %s", labels(cps), agree(cps, "edits", "edit"), ref)
			n.Body = "Both of you edit this symbol; merging may need care."
		}
		c.discussable(&n, m, cps, "agree on who changes what")
		n.Options = append(n.Options, optDismiss(n.ID, "the edits do not interfere"))
		c.emit(n)
	}
}

// overlaps reports whether two items' changes to one symbol are a body
// overlap: both edit an existing symbol and at least one edits only its
// body. Changes that both decide the signature are a clash instead.
func overlaps(a, b model.SymbolChange) bool {
	return touches(a) && touches(b) && (bodyOnly(a) || bodyOnly(b))
}

// symbolOverlap reports whether two items have a signature_clash or
// body_overlap relation on any symbol.
func (c *ctx) symbolOverlap(m, o *entry) bool {
	for _, k := range m.keys {
		oc, ok := o.changes[k]
		if !ok {
			continue
		}
		if overlaps(m.changes[k], oc) {
			return true
		}
		if ct := c.contests[k]; ct != nil && slices.ContainsFunc(ct.rivals(m), func(r contender) bool { return r.e == o }) {
			return true
		}
	}
	return false
}

// conflictIn reports whether merge-tree predicts a conflict between m and o
// in any of paths.
func (c *ctx) conflictIn(m, o *entry, paths ...string) bool {
	for _, cf := range c.v.Conflicts {
		if cf.Mine == m.id() && cf.Other == o.id() && slices.ContainsFunc(cf.Paths, func(p string) bool { return slices.Contains(paths, p) }) {
			return true
		}
	}
	return false
}

// stale: main changed, since my base, the signature of a symbol my changed
// code references (breaking), or removed, renamed or moved it. act; info
// when matched by method name only.
func (c *ctx) stale(m *entry) {
	mv, ok := c.v.Main[m.id()]
	if !ok || mv.Skipped {
		return
	}
	for _, ch := range uniqueChanges(mv.Footprint.Symbols) {
		if !ch.SignatureChanging() || !breaking(ch) {
			continue
		}
		old := oldSymbol(ch)
		refs, resolved := c.matching(m.item.Footprint.Refs, old)
		if len(refs) == 0 {
			continue
		}
		ref := symRef(old, "")
		n := c.newNote(model.NoteStale, m, old.Key, ref)
		n.Severity = model.SeverityAct
		if !resolved {
			n.Severity = model.SeverityInfo
		}
		n.Title = fmt.Sprintf("Main %s %s since your base; your code %s it", verbed(ch), ref, usesVerb(refs))
		n.Body = "Update your branch and fix your references before you submit."
		if !resolved {
			n.Body = "Matched by method name only; update and check your references."
		}
		n.Evidence = append(describe("main", ch), "your references: "+sites(refs))
		n.Options = []model.Option{optUpdate(n.ID, "bring your branch up to date with main, then fix your references")}
		n.Version = version(n.Severity, "main:"+outcome(ch))
		c.emit(n)
	}
}

// newCallers: main gained, since my base, references to a symbol I change
// in a breaking way. act; info when matched by method name only. When main
// made the same change (a stub I use landed, or the same signature change
// landed), its new references were written against my signature, and my
// change breaks none of them.
func (c *ctx) newCallers(m *entry) {
	mv, ok := c.v.Main[m.id()]
	if !ok || mv.Skipped {
		return
	}
	onMain := map[string]string{}
	for _, ch := range uniqueChanges(mv.Footprint.Symbols) {
		if ch.SignatureChanging() {
			onMain[baseKey(ch)] = outcome(ch)
		}
	}
	for _, k := range m.keys {
		ch := m.changes[k]
		if !ch.SignatureChanging() || !breaking(ch) {
			continue
		}
		if out, ok := onMain[k]; ok && out == outcome(ch) {
			continue
		}
		old := oldSymbol(ch)
		refs, resolved := c.matching(mv.Footprint.Refs, old)
		if len(refs) == 0 {
			continue
		}
		ref := symRef(old, "")
		n := c.newNote(model.NoteNewCallers, m, k, ref)
		n.Severity = model.SeverityAct
		if !resolved {
			n.Severity = model.SeverityInfo
		}
		n.Title = fmt.Sprintf("Main gained references to %s since your base, which your change breaks", ref)
		n.Body = "Update your branch, then fix the new call sites for your new signature."
		n.Evidence = append(describe("you", ch), "new references on main: "+sites(refs))
		n.Options = []model.Option{optUpdate(n.ID, "bring your branch up to date, then fix the new call sites")}
		var files []string
		for _, r := range refs {
			files = append(files, "site:"+r.Path)
		}
		slices.Sort(files)
		n.Version = version(n.Severity, slices.Compact(files)...)
		c.emit(n)
	}
}

// uniqueChanges returns changes with one per base key, sorted by key.
func uniqueChanges(chs []model.SymbolChange) []model.SymbolChange {
	byKey := map[string]model.SymbolChange{}
	for _, ch := range chs {
		if k := baseKey(ch); k != "" {
			if _, dup := byKey[k]; !dup {
				byKey[k] = ch
			}
		}
	}
	out := make([]model.SymbolChange, 0, len(byKey))
	for _, k := range sortedKeys(byKey) {
		out = append(out, byKey[k])
	}
	return out
}
