package rules

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// contest gathers the items whose changes decide one symbol's signature.
type contest struct {
	key     string
	sym     model.Symbol // as at the base, or the claim's
	holder  *ClaimView   // the claim, when someone holds it
	members []contender  // sorted by agent, then item
	clash   bool         // members disagree on the outcome
}

// contender is an item with a contending change on a contest's symbol.
type contender struct {
	e   *entry
	ch  model.SymbolChange
	out string // outcome(ch)
}

// member returns e's place in the contest, if it contends.
func (ct *contest) member(e *entry) (contender, bool) {
	for _, m := range ct.members {
		if m.e == e {
			return m, true
		}
	}
	return contender{}, false
}

// rivals returns the members whose outcome differs from e's.
func (ct *contest) rivals(e *entry) []contender {
	me, ok := ct.member(e)
	if !ok {
		return nil
	}
	var out []contender
	for _, m := range ct.members {
		if m.e != e && m.out != me.out {
			out = append(out, m)
		}
	}
	return out
}

// buildContests finds, for every symbol key, the items in view that change
// its signature, remove, rename or move it, or add it (docs/SPEC.md,
// sections 10 and 11). An item that recorded a need on the symbol has
// deferred to its owner and no longer contends, unless its agent holds the
// claim. The symbol is contested (a clash) when the remaining items do not
// all make the same thing of it.
func (c *ctx) buildContests() {
	c.contests = map[string]*contest{}
	byKey := map[string][]contender{}
	for _, e := range c.entries {
		for _, k := range e.keys {
			if ch := e.changes[k]; contends(ch) {
				byKey[k] = append(byKey[k], contender{e: e, ch: ch, out: outcome(ch)})
			}
		}
	}
	for _, k := range sortedKeys(byKey) {
		ct := &contest{key: k, sym: oldSymbol(byKey[k][0].ch)}
		if cv, ok := c.claim(k); ok {
			ct.holder = &cv
			if cv.Claim.Symbol.Qualified != "" {
				ct.sym = cv.Claim.Symbol
				ct.sym.Key = k
			}
		}
		outs := map[string]bool{}
		for _, m := range byKey[k] {
			if c.hasNeed(m.e, k) && (ct.holder == nil || ct.holder.Claim.Owner != m.e.agent) {
				continue
			}
			ct.members = append(ct.members, m)
			outs[m.out] = true
		}
		ct.clash = len(outs) > 1
		c.contests[k] = ct
	}
}

// accept is one accept with the agent that made it.
type accept struct {
	agent    model.AgentID
	proposal string
}

// thread is a thread being assembled: its state plus what the rules need
// to decide it.
type thread struct {
	st      model.ThreadState
	key     string          // signature threads: the symbol key
	sym     model.Symbol    // signature threads: the contested symbol
	holder  *ClaimView      // signature threads: the claim, when held
	agents  []model.AgentID // distinct participant agents, sorted
	counted []model.Message // messages within each author's quota, causal order
}

// loadOutboxes indexes every message and accept in view by thread. The
// outbox an entry came from names its author, whatever the entry says.
func (c *ctx) loadOutboxes() {
	c.msgs = map[string][]model.Message{}
	c.accepts = map[string][]accept{}
	seen := map[string]bool{}
	add := func(agent model.AgentID, ob model.Outbox) {
		for _, m := range ob.Messages {
			if m.Thread == "" || m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			m.From = agent
			c.msgs[m.Thread] = append(c.msgs[m.Thread], m)
		}
		for _, a := range ob.Accepts {
			if a.Thread != "" && a.Proposal != "" {
				c.accepts[a.Thread] = append(c.accepts[a.Thread], accept{agent: agent, proposal: a.Proposal})
			}
		}
	}
	add(c.v.Me, c.v.MyOutbox)
	for _, id := range sortedKeys(c.v.Peers) {
		if id != c.v.Me {
			add(id, c.v.Peers[id].Outbox)
		}
	}
}

// buildThreads assembles the signature thread of every current clash, the
// agreement of the epoch before each transferred claim, and the note
// threads that carry messages.
func (c *ctx) buildThreads() {
	c.sig = map[string]*thread{}
	c.prev = map[string]*thread{}
	sigIDs := map[string]bool{}
	for _, k := range sortedKeys(c.contests) {
		if ct := c.contests[k]; ct.clash {
			t := c.sigThread(ct)
			c.sig[k] = t
			sigIDs[t.st.ID] = true
			if c.participates(t) {
				c.threads = append(c.threads, t)
			}
		}
	}
	for _, k := range sortedKeys(c.v.Claims) {
		if t := c.prevAgreement(k, c.v.Claims[k]); t != nil {
			c.prev[k] = t
			sigIDs[t.st.ID] = true
		}
	}
	for _, id := range sortedKeys(c.msgs) {
		if sigIDs[id] {
			continue
		}
		if t := c.noteThread(id); t != nil && c.participates(t) {
			c.threads = append(c.threads, t)
		}
	}
	slices.SortFunc(c.threads, func(a, b *thread) int { return cmp.Compare(a.st.ID, b.st.ID) })
}

// sigThread assembles the signature thread of a clash: every server derives
// the same id from the symbol and the claim epoch, and the participants are
// the contending items plus the claim holder (docs/SPEC.md, section 11).
func (c *ctx) sigThread(ct *contest) *thread {
	epoch := 0
	if ct.holder != nil {
		epoch = ct.holder.Claim.Epoch
	}
	id := model.SigThreadID(ct.key, epoch)
	var parts []model.Participant
	for _, m := range ct.members {
		parts = append(parts, model.Participant{Agent: m.e.agent, Item: m.e.id()})
	}
	if h := ct.holder; h != nil && h.Claim.Owner != "" {
		parts = append(parts, model.Participant{Agent: h.Claim.Owner, Item: h.Claim.Item})
	}
	seen, ok := c.v.ClashSeen[id]
	if !ok {
		seen = c.v.Now
	}
	t := &thread{
		st: model.ThreadState{
			ID:           id,
			Kind:         model.ThreadSignature,
			Subject:      ct.key,
			SubjectName:  symRef(ct.sym, ""),
			Participants: sortParticipants(parts),
			Messages:     causal(c.msgs[id]),
			OpenedAt:     seen,
			Deadline:     seen.Add(c.lim.ThreadTimeout),
		},
		key:    ct.key,
		sym:    ct.sym,
		holder: ct.holder,
	}
	c.settle(t)
	return t
}

// prevAgreement returns the signature thread of the epoch before a claim's
// current one when it ended in an agreement whose owner now holds the
// claim: the transfer happened, and non-owners may still have to record
// their needs. Its participants are the items its messages name.
func (c *ctx) prevAgreement(key string, cv ClaimView) *thread {
	if cv.Claim.Epoch <= 0 || cv.Claim.Owner == "" {
		return nil
	}
	id := model.SigThreadID(key, cv.Claim.Epoch-1)
	if len(c.msgs[id]) == 0 {
		return nil
	}
	t := c.assemble(id, model.ThreadSignature)
	t.key, t.holder = key, &cv
	t.sym = c.claimSymbol(key, "")
	t.st.Subject, t.st.SubjectName = key, symRef(t.sym, "")
	if t.st.Agreement == nil || t.st.Owner != cv.Claim.Owner {
		return nil
	}
	return t
}

// agreementOn returns the latest agreement about key's signature: from the
// current signature thread, or else from the messages of the thread at the
// claim's current or previous epoch. Messages outlive the clash itself,
// which ends as soon as the non-owners record their needs.
func (c *ctx) agreementOn(key string) *model.Proposal {
	if t := c.sig[key]; t != nil && t.st.Agreement != nil {
		return t.st.Agreement
	}
	epoch := 0
	if cv, ok := c.claim(key); ok {
		epoch = cv.Claim.Epoch
	}
	for e := epoch; e >= 0 && e >= epoch-1; e-- {
		id := model.SigThreadID(key, e)
		if len(c.msgs[id]) == 0 {
			continue
		}
		if t := c.assemble(id, model.ThreadSignature); t.st.Agreement != nil {
			return t.st.Agreement
		}
	}
	return nil
}

// noteThread assembles a plain conversation from its messages. Messages of
// signature threads that are no longer current are left out.
func (c *ctx) noteThread(id string) *thread {
	for _, m := range c.msgs[id] {
		if m.Kind == model.ThreadSignature {
			return nil
		}
	}
	return c.assemble(id, model.ThreadNote)
}

// assemble builds a thread whose participants are the items its messages
// name (their authors' items and the items each message was sent to).
func (c *ctx) assemble(id string, kind model.ThreadKind) *thread {
	msgs := causal(c.msgs[id])
	owners := map[model.ItemID]model.AgentID{}
	for _, m := range msgs {
		if m.Item != "" {
			owners[m.Item] = m.From
		}
	}
	for _, m := range msgs {
		for _, it := range m.Items {
			if _, ok := owners[it]; !ok && it != "" {
				if a, ok := c.agentOf(it); ok {
					owners[it] = a
				}
			}
		}
	}
	var parts []model.Participant
	for _, it := range sortedKeys(owners) {
		parts = append(parts, model.Participant{Agent: owners[it], Item: it})
	}
	t := &thread{st: model.ThreadState{ID: id, Kind: kind, Participants: sortParticipants(parts), Messages: msgs}}
	for _, m := range msgs {
		if t.st.Subject == "" && m.Subject != "" {
			t.st.Subject, t.st.SubjectName, t.st.NoteKind = m.Subject, m.SubjectName, m.NoteKind
		}
		if t.st.OpenedAt.IsZero() || m.At.Before(t.st.OpenedAt) {
			t.st.OpenedAt = m.At
		}
	}
	c.settle(t)
	return t
}

// settle works out acceptances, agreement and, for signature threads, the
// bound and fallback (docs/SPEC.md, section 11):
//
//   - Only each author's first ThreadMessages messages count in a signature
//     thread; later ones are shown but carry no proposal.
//   - Agreement: every participant agent accepted the same proposal, the
//     proposer implicitly. The first such proposal in causal order wins.
//   - Bound: every participant used its quota, or ThreadTimeout passed since
//     this server first saw the clash. The fallback owner is the claim
//     holder, or nobody (a race the remote referees), which every server
//     derives alike whichever reaches the bound first.
func (c *ctx) settle(t *thread) {
	for _, p := range t.st.Participants {
		if !slices.Contains(t.agents, p.Agent) {
			t.agents = append(t.agents, p.Agent)
		}
	}
	slices.Sort(t.agents)
	sig := t.st.Kind == model.ThreadSignature
	counts := map[model.AgentID]int{}
	for _, m := range t.st.Messages {
		if sig && counts[m.From] >= c.lim.ThreadMessages {
			continue
		}
		counts[m.From]++
		t.counted = append(t.counted, m)
	}

	var proposals []model.Proposal
	accepted := map[string]map[model.AgentID]bool{}
	for _, m := range t.counted {
		if m.Proposal == nil || m.Proposal.ID == "" {
			continue
		}
		p := resolveProposal(*m.Proposal, m.From, t.agents)
		if accepted[p.ID] == nil {
			accepted[p.ID] = map[model.AgentID]bool{}
			proposals = append(proposals, p)
		}
		accepted[p.ID][m.From] = true
	}
	for _, a := range c.accepts[t.st.ID] {
		if accepted[a.proposal] != nil {
			accepted[a.proposal][a.agent] = true
		}
	}
	for _, id := range sortedKeys(accepted) {
		if t.st.Accepted == nil {
			t.st.Accepted = map[string][]model.AgentID{}
		}
		t.st.Accepted[id] = sortedKeys(accepted[id])
	}
	if len(t.st.Participants) >= 2 {
		for _, p := range proposals {
			if p.Owner != "" && allIn(t.agents, accepted[p.ID]) {
				t.st.Agreement = &p
				t.st.Owner, t.st.Settled = p.Owner, true
				break
			}
		}
	}
	if !sig || t.st.Settled {
		return
	}
	byMessages := len(t.agents) > 0
	for _, a := range t.agents {
		byMessages = byMessages && counts[a] >= c.lim.ThreadMessages
	}
	if byMessages || !c.v.Now.Before(t.st.Deadline) {
		t.st.Settled, t.st.FellBack = true, true
		if t.holder != nil {
			t.st.Owner = t.holder.Claim.Owner
		}
	}
}

// resolveProposal turns "me" and "them" into agent ids, in case the writer
// left them unresolved; an owner that cannot be resolved is cleared.
func resolveProposal(p model.Proposal, from model.AgentID, agents []model.AgentID) model.Proposal {
	switch p.Owner {
	case "me":
		p.Owner = from
	case "them":
		p.Owner = ""
		var others []model.AgentID
		for _, a := range agents {
			if a != from {
				others = append(others, a)
			}
		}
		if len(others) == 1 {
			p.Owner = others[0]
		}
	}
	if !model.ValidAgentID(p.Owner) {
		p.Owner = ""
	}
	return p
}

func allIn(agents []model.AgentID, set map[model.AgentID]bool) bool {
	for _, a := range agents {
		if !set[a] {
			return false
		}
	}
	return len(agents) > 0
}

// participates reports whether View.Me takes part in a thread: one of the
// participants is mine, or (note threads) I wrote in it.
func (c *ctx) participates(t *thread) bool {
	if slices.Contains(t.agents, c.v.Me) {
		return true
	}
	if t.st.Kind == model.ThreadSignature {
		return false
	}
	return slices.ContainsFunc(t.st.Messages, func(m model.Message) bool { return m.From == c.v.Me })
}

// myItem returns my item in a thread: my first open participant item, else
// the item I wrote from.
func (c *ctx) myItem(t *thread) (model.ItemID, *entry) {
	for _, p := range t.st.Participants {
		if e := c.byItem[p.Item]; p.Agent == c.v.Me && e != nil && e.mine {
			return p.Item, e
		}
	}
	for _, p := range t.st.Participants {
		if p.Agent == c.v.Me {
			return p.Item, nil
		}
	}
	for _, m := range t.st.Messages {
		if m.From == c.v.Me && m.Item != "" {
			return m.Item, c.byItem[m.Item]
		}
	}
	return "", nil
}

// latest returns agent's latest counted message in a thread.
func (t *thread) latest(agent model.AgentID) (model.Message, bool) {
	for i := len(t.counted) - 1; i >= 0; i-- {
		if t.counted[i].From == agent {
			return t.counted[i], true
		}
	}
	return model.Message{}, false
}

// ownerItem returns the item that owns the outcome: the owner's participant
// item, else the claim's item when the owner holds it, else an item of the
// owner that changes the symbol.
func (c *ctx) ownerItem(t *thread, owner model.AgentID) model.ItemID {
	if owner == "" {
		return ""
	}
	for _, p := range t.st.Participants {
		if p.Agent == owner {
			return p.Item
		}
	}
	if t.holder != nil && t.holder.Claim.Owner == owner {
		return t.holder.Claim.Item
	}
	for _, e := range c.entries {
		if _, ok := e.changes[t.key]; ok && e.agent == owner {
			return e.id()
		}
	}
	return ""
}

// causal orders messages so each comes after the messages it lists as seen,
// breaking ties by time, author and id. A cycle in malformed documents is
// broken at the earliest remaining message.
func causal(msgs []model.Message) []model.Message {
	base := slices.Clone(msgs)
	slices.SortStableFunc(base, func(a, b model.Message) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.From, b.From), cmp.Compare(a.ID, b.ID))
	})
	present := map[string]bool{}
	for _, m := range base {
		present[m.ID] = true
	}
	emitted := map[string]bool{}
	ready := func(m model.Message) bool {
		for _, s := range m.Seen {
			if s != m.ID && present[s] && !emitted[s] {
				return false
			}
		}
		return true
	}
	out := make([]model.Message, 0, len(base))
	done := make([]bool, len(base))
	for len(out) < len(base) {
		next, fallback := -1, -1
		for i, m := range base {
			if done[i] {
				continue
			}
			if fallback < 0 {
				fallback = i
			}
			if ready(m) {
				next = i
				break
			}
		}
		if next < 0 {
			next = fallback
		}
		done[next], emitted[base[next].ID] = true, true
		out = append(out, base[next])
	}
	return out
}

func sortParticipants(ps []model.Participant) []model.Participant {
	slices.SortFunc(ps, func(a, b model.Participant) int {
		return cmp.Or(cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Item, b.Item))
	})
	return slices.CompactFunc(ps, func(a, b model.Participant) bool { return a == b })
}

// decide works out View.Me's actions for a settled signature thread. Each
// action is returned until the shared state shows it done: ActClaim until I
// hold the claim, ActTransfer until I no longer do, ActNeed until my outbox
// has a need on the symbol, ActRace until anybody holds it.
func (c *ctx) decide(t *thread) Decision {
	me := c.v.Me
	owner := t.st.Owner
	d := Decision{
		Thread:    t.st.ID,
		Key:       t.key,
		Symbol:    t.sym,
		Owner:     owner,
		OwnerItem: c.ownerItem(t, owner),
		Proposal:  t.st.Agreement,
		FellBack:  t.st.FellBack,
	}
	var holder *ClaimView // the claim as seen now, which a transfer may have changed
	if cv, ok := c.claim(t.key); ok {
		holder = &cv
		d.ClaimOID, d.Epoch = holder.OID, holder.Claim.Epoch
	}
	for _, a := range t.agents {
		if m, ok := t.latest(a); ok && a != owner && m.Text != "" {
			d.Requirements = append(d.Requirements, model.Requirement{From: a, Text: m.Text})
		}
	}
	switch owner {
	case "":
		if holder == nil {
			d.Actions = append(d.Actions, ActRace)
		}
	case me:
		if holder == nil || holder.Claim.Owner != me {
			d.Actions = append(d.Actions, ActClaim)
		}
	default:
		if holder != nil && holder.Claim.Owner == me {
			d.Actions = append(d.Actions, ActTransfer)
		}
		if !c.hasMyNeed(t.key) {
			d.Actions = append(d.Actions, ActNeed)
			if m, ok := t.latest(me); ok {
				d.Requirement = m.Text
			}
		}
	}
	return d
}

// pendingPrev returns the previous epoch's agreement on key while I still
// have to record my need under it.
func (c *ctx) pendingPrev(key string) *thread {
	t := c.prev[key]
	if t == nil || !c.participates(t) || t.st.Owner == c.v.Me || c.hasMyNeed(key) {
		return nil
	}
	return t
}

// deadlineText formats a thread deadline for evidence.
func deadlineText(d time.Time) string { return d.UTC().Format("2006-01-02 15:04 UTC") }
