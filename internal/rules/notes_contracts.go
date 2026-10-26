package rules

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/gitlet/internal/model"
)

// needed: another item needs a symbol I own or change, and no contract is
// published. act. Symbols someone else holds are theirs to publish.
func (c *ctx) needed(m *entry) {
	keys := map[string]string{}
	for _, k := range m.keys {
		if m.changes[k].SignatureChanging() {
			keys[k] = "change"
		}
	}
	for _, k := range sortedKeys(c.v.Claims) {
		if cl := c.v.Claims[k].Claim; cl.Owner == c.v.Me && cl.Item == m.id() {
			keys[k] = "own"
		}
	}
	for _, k := range sortedKeys(keys) {
		cv, claimed := c.claim(k)
		if claimed && (cv.Claim.Owner != c.v.Me || cv.Claim.Contract != nil) {
			continue
		}
		needers := c.needers(k, m)
		if len(needers) == 0 {
			continue
		}
		var cps []cp
		var quotes []model.Quote
		for _, nn := range needers {
			req := strings.TrimSpace(nn.need.Requirement)
			cps = append(cps, cp{e: nn.e, sev: lower(model.SeverityAct, nn.e), fact: req})
			if req != "" {
				quotes = append(quotes, model.Quote{From: nn.e.agent, Text: req})
			}
		}
		if claimed {
			for _, r := range cv.Claim.Requirements {
				q := model.Quote(r)
				if r.Text != "" && r.From != c.v.Me && !slices.Contains(quotes, q) {
					quotes = append(quotes, q)
				}
			}
		}
		ch, changed := m.changes[k]
		sym := c.claimSymbol(k, needers[0].need.Name)
		if changed {
			sym = oldSymbol(ch)
		}
		ref := symRef(sym, needers[0].need.Name)
		n := c.newNote(model.NoteNeeded, m, k, ref)
		withCps(&n, cps)
		n.Quotes = quotes
		n.Title = fmt.Sprintf("%s %s %s, which you %s; publish its signature", labels(cps), agree(cps, "needs", "need"), ref, keys[k])
		n.Body = "They wait for your published signature. Publish it (gitlet claims it first if needed), or discuss what they need."
		cur := ref
		if changed {
			n.Evidence = append(describe("you", ch), n.Evidence...)
			if ch.Change != model.ChangeRemoved {
				cur = symRef(ch.Symbol, ref)
			}
		}
		n.Options = []model.Option{optPublish(n.ID, cur, "publish the signature they wait for")}
		c.discussable(&n, m, cps, "ask what they need from it")
		c.emit(n)
	}
}

// ownerEntry returns the entry of a claim's item when it is in view.
func (c *ctx) ownerEntry(cv ClaimView) *entry {
	if e := c.byItem[cv.Claim.Item]; e != nil && e.agent == cv.Claim.Owner {
		return e
	}
	return nil
}

// ownerLabel names a claim's owner in a title.
func (c *ctx) ownerLabel(cv ClaimView) string {
	if e := c.ownerEntry(cv); e != nil {
		return label(e)
	}
	if cv.Claim.Owner == c.v.Me {
		return "you"
	}
	return string(cv.Claim.Owner)
}

// ownerCps returns the claim's owner as a counterpart with severity sev,
// when its item is in view.
func (c *ctx) ownerCps(cv ClaimView, sev model.Severity, fact string) []cp {
	if e := c.ownerEntry(cv); e != nil {
		return []cp{{e: e, sev: lower(sev, e), fact: fact}}
	}
	return nil
}

// withOwner fills severity, parties and version for a note whose
// counterpart is a claim's owner, who may be out of view.
func withOwner(n *model.Note, cps []cp, cv ClaimView, sev model.Severity, facts ...string) {
	facts = append(facts, "owner:"+string(cv.Claim.Owner)+":"+string(cv.Claim.Item))
	if len(cps) > 0 {
		withCps(n, cps, facts...)
		return
	}
	n.Severity = sev
	n.Parties = []model.Party{{Agent: cv.Claim.Owner, Item: cv.Claim.Item, IdleSince: cv.IdleSince}}
	n.Version = version(sev, facts...)
}

// ready: a contract is published for a symbol I need, and I have not used
// it. act. A contract that landed on main comes in with update instead.
func (c *ctx) ready(m *entry) {
	for _, nd := range c.myNeeds(m) {
		cv, ok := c.claim(nd.Key)
		if !ok || cv.Claim.Contract == nil || nd.PinnedSigHash != "" {
			continue
		}
		ct := cv.Claim.Contract
		sym := c.claimSymbol(nd.Key, nd.Name)
		ref := symRef(sym, nd.Name)
		n := c.newNote(model.NoteReady, m, nd.Key, ref)
		withOwner(&n, c.ownerCps(cv, model.SeverityAct, ""), cv, model.SeverityAct, "contract:"+ct.SigHash, fmt.Sprintf("landed:%t", cv.Landed))
		n.Evidence = []string{"contract: " + ct.Signature}
		if ct.StubCommit != "" {
			n.Evidence = append(n.Evidence, "stub commit: "+ct.StubCommit)
		}
		if ct.PR != "" {
			n.Evidence = append(n.Evidence, "interface PR: "+ct.PR)
		}
		if cv.Landed {
			n.Title = fmt.Sprintf("The contract for %s from %s has landed on main", ref, c.ownerLabel(cv))
			n.Body = "Update your branch to build on it."
			n.Options = []model.Option{optUpdate(n.ID, "bring your branch up to date with main, which has the contract")}
		} else {
			n.Title = fmt.Sprintf("%s published %s: use it", c.ownerLabel(cv), ref)
			n.Body = "Bring the published signature into your branch and build against it."
			n.Options = []model.Option{optUse(n.ID, ref)}
		}
		n.Options = append(n.Options, optDismiss(n.ID, "you no longer need it"))
		c.emit(n)
	}
}

// drift: the contract I pinned now has a different signature hash. act.
func (c *ctx) drift(m *entry) {
	for _, nd := range c.myNeeds(m) {
		cv, ok := c.claim(nd.Key)
		if !ok || cv.Claim.Contract == nil || nd.PinnedSigHash == "" || cv.Claim.Contract.SigHash == nd.PinnedSigHash {
			continue
		}
		ct := cv.Claim.Contract
		ref := symRef(c.claimSymbol(nd.Key, nd.Name), nd.Name)
		n := c.newNote(model.NoteDrift, m, nd.Key, ref)
		cps := c.ownerCps(cv, model.SeverityAct, "")
		withOwner(&n, cps, cv, model.SeverityAct, "contract:"+ct.SigHash, "pinned:"+nd.PinnedSigHash)
		n.Title = fmt.Sprintf("%s changed the published signature of %s", c.ownerLabel(cv), ref)
		n.Body = "The contract you use changed. Use the new one (gitlet reverts the old stub), or discuss the change."
		n.Evidence = []string{"you pinned signature hash " + nd.PinnedSigHash, "now: " + ct.Signature}
		n.Options = []model.Option{optUse(n.ID, ref)}
		if len(cps) > 0 {
			c.discussable(&n, m, cps, "ask why the signature changed")
		}
		c.emit(n)
	}
}

// diverged: my current signature of a symbol I own differs from my
// published contract, or from the agreed proposal before any contract
// exists. act. Proposals are free text, so they are compared loosely.
func (c *ctx) diverged(m *entry) {
	for _, k := range sortedKeys(c.v.Claims) {
		cv := c.v.Claims[k]
		if cv.Claim.Owner != c.v.Me || cv.Claim.Item != m.id() || cv.Landed {
			continue
		}
		ch, ok := m.changes[k]
		if !ok {
			continue // unchanged: the base signature is not in view
		}
		cur, curHash := ch.Symbol.Signature, ch.Symbol.SigHash
		if ch.Change == model.ChangeRemoved {
			cur, curHash = "(removed)", "removed"
		}
		var against, fact, what string
		if ct := cv.Claim.Contract; ct != nil {
			if ct.SigHash == "" || ct.SigHash == curHash {
				continue
			}
			against, fact, what = ct.Signature, "contract:"+ct.SigHash, "your published contract"
		} else {
			p := c.agreementOn(k)
			if p == nil || p.Owner != c.v.Me || p.Signature == "" || sameSignature(cur, p.Signature) {
				continue
			}
			against, fact, what = p.Signature, "proposal:"+p.ID, "the agreed proposal"
		}
		var cps []cp
		for _, nn := range c.needers(k, m) {
			cps = append(cps, cp{e: nn.e, sev: model.SeverityAct})
		}
		ref := symRef(oldSymbol(ch), "")
		n := c.newNote(model.NoteDiverged, m, k, ref)
		withCps(&n, cps, fact)
		n.Severity = model.SeverityAct
		n.Version = version(n.Severity, append(cpFacts(cps), fact)...)
		n.Title = fmt.Sprintf("Your signature of %s differs from %s", ref, what)
		n.Body = "Others build against what you published or agreed. Publish your current signature, or change your code back."
		n.Evidence = []string{"agreed: " + against, "yours now: " + cur}
		n.Options = []model.Option{optPublish(n.ID, symRef(ch.Symbol, ref), "publish your current signature")}
		if len(cps) > 0 {
			c.discussable(&n, m, cps, "tell the agents waiting on it what changed")
		}
		c.emit(n)
	}
}

// sameSignature compares a declaration with a proposal's free-text
// signature, ignoring whitespace and a trailing semicolon, and accepting a
// proposal that leaves out the leading keyword or receiver
// ("parseConfig(string): Config" for "function parseConfig(string): Config").
func sameSignature(decl, proposal string) bool {
	squash := func(s string) string {
		return strings.TrimSuffix(strings.Join(strings.Fields(s), ""), ";")
	}
	a, b := squash(decl), squash(proposal)
	return a == b || (b != "" && strings.HasSuffix(a, b)) || (a != "" && strings.HasSuffix(b, a))
}

// stillWaiting: I have needed a symbol for longer than PublishTimeout and
// nothing is published. act.
func (c *ctx) stillWaiting(m *entry) {
	for _, nd := range c.myNeeds(m) {
		cv, claimed := c.claim(nd.Key)
		if (claimed && cv.Claim.Contract != nil) || nd.CreatedAt.IsZero() {
			continue
		}
		waited := c.v.Now.Sub(nd.CreatedAt)
		if waited < c.lim.PublishTimeout {
			continue
		}
		ref := symRef(c.claimSymbol(nd.Key, nd.Name), nd.Name)
		n := c.newNote(model.NoteStillWaiting, m, nd.Key, ref)
		var cps []cp
		if claimed {
			cps = c.ownerCps(cv, model.SeverityAct, "")
		} else if ct := c.contests[nd.Key]; ct != nil {
			for _, x := range ct.members {
				if x.e != m {
					cps = append(cps, cp{e: x.e, sev: model.SeverityAct})
				}
			}
		}
		if claimed {
			withOwner(&n, cps, cv, model.SeverityAct)
		} else {
			withCps(&n, cps)
		}
		n.Severity = model.SeverityAct
		n.Version = version(n.Severity, append(cpFacts(cps), "claimed:"+fmt.Sprint(claimed))...)
		n.Title = fmt.Sprintf("Still waiting for %s after %s: nothing is published", ref, duration(waited))
		n.Body = "Release the need and adapt to the current signature, or ask the owner when it will publish."
		if claimed {
			n.Evidence = []string{"owner: " + string(cv.Claim.Owner)}
		} else {
			n.Evidence = []string{"nobody holds a claim on it"}
		}
		n.Options = []model.Option{optRelease(n.ID, ref)}
		if len(cps) > 0 {
			c.discussable(&n, m, cps, "ask when it will be published")
		}
		c.emit(n)
	}
}

// lapsedOwners: the owner of a claim I need or contest has lapsed. warn.
func (c *ctx) lapsedOwners(m *entry) {
	for _, k := range sortedKeys(c.v.Claims) {
		cv := c.v.Claims[k]
		if !cv.Lapsed || cv.Claim.Owner == c.v.Me {
			continue
		}
		why := ""
		if ch, ok := m.changes[k]; ok && contends(ch) {
			why = "you change its signature"
		} else if c.hasNeed(m, k) {
			why = "you need it"
		}
		if why == "" {
			continue
		}
		ref := symRef(c.claimSymbol(k, ""), "")
		n := c.newNote(model.NoteLapsed, m, k, ref)
		n.Severity = model.SeverityWarn
		n.Parties = []model.Party{{Agent: cv.Claim.Owner, Item: cv.Claim.Item, IdleSince: cv.IdleSince}}
		if e := c.ownerEntry(cv); e != nil {
			n.Parties[0].Intent, n.Parties[0].Branch, n.Parties[0].PR = e.item.Intent, e.item.Branch, e.item.PR
		}
		idle := ""
		if cv.IdleSince != nil {
			idle = " since " + cv.IdleSince.UTC().Format("2006-01-02 15:04 UTC")
		}
		n.Title = fmt.Sprintf("%s holds the claim on %s but has been idle%s", cv.Claim.Owner, ref, idle)
		n.Body = "Its lease lapsed, so the claim is free to take over."
		n.Evidence = []string{fmt.Sprintf("claim epoch %d; %s", cv.Claim.Epoch, why)}
		if cv.Claim.Contract != nil {
			n.Evidence = append(n.Evidence, "contract: "+cv.Claim.Contract.Signature)
		}
		n.Options = []model.Option{
			optClaim(n.ID, ref, "take the claim over"),
			optDismiss(n.ID, "leave it with them"),
		}
		n.Version = version(n.Severity, "owner:"+string(cv.Claim.Owner)+":"+string(cv.Claim.Item))
		c.emit(n)
	}
}
