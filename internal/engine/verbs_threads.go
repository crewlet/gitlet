package engine

// Thread verbs: discuss, accept, dismiss (docs/SPEC.md, sections 4, 10.1
// and 11).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

const (
	// maxMessageLen caps a message, and each text field of a proposal, in
	// characters: threads are for short, concrete exchanges.
	maxMessageLen = 2000
	// maxReasonLen caps the reason of dismiss and abandon.
	maxReasonLen = 500
)

// errQuotaUsed ends a discuss write when this agent's messages in a
// signature thread reached gitlet.threadMessages meanwhile.
var errQuotaUsed = errors.New("engine: thread quota used")

// ---- discuss ----

func (e *Engine) discuss(ctx context.Context, in api.DiscussInput) (*api.Reply[api.DiscussData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doDiscuss(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doDiscuss appends a message, optionally with a proposal, to my outbox in
// the thread about names (docs/SPEC.md, section 11). Signature threads are
// bounded at gitlet.threadMessages messages per participant, and no thread
// takes messages once it settled.
func (e *Engine) doDiscuss(ctx context.Context, in api.DiscussInput) (*api.Reply[api.DiscussData], error) {
	var none api.DiscussData
	if refusal := e.worktreeRefusal("discuss", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	t, refusal, err := e.discussTarget(ctx, in.About)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	none.Thread = t
	// Options repeat the agent's about: a note's thread has no id to name
	// until its first message.
	about := strings.TrimSpace(in.About)
	text := strings.TrimSpace(in.Message)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return reply(refused("discuss needs a message: say what you need or propose.", nil,
			toolOption("say what you want to tell them", "discuss", "about", about)), none), nil
	case n > maxMessageLen:
		return reply(refused(fmt.Sprintf("Your message has %d characters; keep it under %d.", n, maxMessageLen), nil,
			toolOption("say it in fewer words", "discuss", "about", about)), none), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("discuss"), none), nil
	}
	if t.Settled {
		return reply(e.settledRefusal(t), none), nil
	}
	limit := e.cfg.ThreadMessages
	bounded := t.Kind == model.ThreadSignature
	if bounded && messagesFrom(t.Messages, t.ID, e.me.ID) >= limit {
		return reply(e.quotaRefusal(t), none), nil
	}
	item := e.myThreadItem(t)
	if item == "" {
		return reply(refused("You have no task to speak for in this thread: call start first.", nil,
			toolOption("state your task", "start", "intent", "")), none), nil
	}
	var prop *model.Proposal
	if in.Proposal != nil {
		p, refusal := e.proposalOf(t, about, *in.Proposal)
		if refusal != nil {
			return reply(*refusal, none), nil
		}
		prop = &p
	}

	msg := e.threadMessage(t, item, text, prop)
	_, err = e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		if bounded && messagesFrom(ob.Messages, t.ID, e.me.ID) >= limit {
			return errQuotaUsed // another process of mine wrote meanwhile
		}
		ob.Messages = append(ob.Messages, msg)
		return nil
	})
	if errors.Is(err, errQuotaUsed) {
		return reply(e.quotaRefusal(t), none), nil
	}
	if rr, ok := e.writeRefusal("discuss", err); ok {
		return reply(rr, none), nil
	}
	if err != nil {
		return nil, err
	}
	detail := map[string]any{"thread": t.ID, "message": msg.ID}
	if prop != nil {
		detail["proposal"] = prop.ID
	}
	e.appendEvent(ctx, "discuss", detail)

	after := e.findThread(ctx, t.ID)
	if after.ID == "" {
		after = t
		after.Messages = append(slices.Clone(t.Messages), msg)
	}
	if after.Settled {
		e.applySettledThread(ctx, after.ID)
	}
	subject := threadSubject(after)
	summary := fmt.Sprintf("Posted your message in the thread about %s.", subject)
	if prop != nil {
		summary = fmt.Sprintf("Posted your proposal %s in the thread about %s: %s it.", prop.ID, subject, e.owns(prop.Owner))
	}
	if after.Settled {
		summary += fmt.Sprintf(" The thread is now settled: %s.", e.threadOutcome(after))
	}
	details := []string{fmt.Sprintf("thread %s: %s; with %s", after.ID, pluralize(len(after.Messages), "message"), e.participantList(after))}
	if prop != nil {
		details = append(details, fmt.Sprintf("the others accept it with: accept proposal=%s", prop.ID))
	}
	if bounded && !after.Settled {
		left := limit - messagesFrom(after.Messages, after.ID, e.me.ID)
		details = append(details, fmt.Sprintf("you have %d of %d messages left; the thread settles after %d messages each or at %s",
			max(left, 0), limit, limit, after.Deadline.UTC().Format("2006-01-02 15:04 UTC")))
	}
	return reply(okResult(summary, details...), api.DiscussData{Thread: after, Message: msg}), nil
}

// discussTarget resolves discuss's about argument (docs/SPEC.md, section
// 4.1): a thread of mine, or a note whose thread it opens or continues. An
// id without its t- or n- prefix is tried as a thread, then as a note.
func (e *Engine) discussTarget(ctx context.Context, about string) (model.ThreadState, *api.Result, error) {
	arg := strings.ToLower(strings.TrimSpace(about))
	if !strings.HasPrefix(arg, "n-") {
		t, refusal := e.resolveThreadID(ctx, "discuss", "about", about)
		if refusal == nil || strings.HasPrefix(arg, "t-") {
			return t, refusal, nil
		}
	}
	sn, refusal, err := e.resolveNoteID(ctx, "discuss", "about", about)
	if err != nil || refusal != nil {
		return model.ThreadState{}, refusal, err
	}
	t, refusal := e.threadOfNote(ctx, sn.Note)
	return t, refusal, nil
}

// threadOfNote returns the thread a note offers: the current one when it
// exists (a signature thread, or a note thread someone already wrote in),
// otherwise a new note thread between my item and the note's counterparts
// (docs/SPEC.md, section 11).
func (e *Engine) threadOfNote(ctx context.Context, n model.Note) (model.ThreadState, *api.Result) {
	if n.Thread == "" {
		opts := slices.Clone(n.Options)
		r := refused(fmt.Sprintf("Note %s has no thread to discuss: %s.", n.ID, strings.TrimSuffix(n.Title, ".")), nil,
			append(opts, statusOption())...)
		return model.ThreadState{}, &r
	}
	if t := e.findThread(ctx, n.Thread); t.ID != "" {
		return t, nil
	}
	switch n.Kind {
	case model.NoteSignatureClash, model.NoteMessage, model.NoteAgreement:
		// Their thread is derived, not opened: it is gone with the clash.
		r := refused(fmt.Sprintf("The thread of note %s is over: the situation it discussed has changed.", n.ID), nil,
			statusOption())
		return model.ThreadState{}, &r
	}
	item := n.Item
	if item == "" {
		if it, ok := e.activeItem(); ok {
			item = it.ID
		}
	}
	parts := []model.Participant{{Agent: e.me.ID, Item: item}}
	others := 0
	for _, p := range n.Parties {
		if p.Agent != "" && p.Item != "" && p.Item != item {
			parts = append(parts, model.Participant{Agent: p.Agent, Item: p.Item})
			others++
		}
	}
	if others == 0 {
		r := refused(fmt.Sprintf("Note %s names nobody to talk to.", n.ID), nil, statusOption())
		return model.ThreadState{}, &r
	}
	slices.SortFunc(parts, func(a, b model.Participant) int {
		if a.Agent != b.Agent {
			return strings.Compare(string(a.Agent), string(b.Agent))
		}
		return strings.Compare(string(a.Item), string(b.Item))
	})
	return model.ThreadState{
		ID:           n.Thread,
		Kind:         model.ThreadNote,
		Subject:      n.Subject,
		SubjectName:  n.SubjectName,
		NoteKind:     n.Kind,
		Participants: slices.Compact(parts),
		OpenedAt:     e.now().UTC(),
	}, nil
}

// findThread returns one of my threads by id, as the rules assemble it now
// (the zero thread when there is none).
func (e *Engine) findThread(ctx context.Context, id string) model.ThreadState {
	for _, t := range e.threads(ctx) {
		if t.ID == id {
			return t
		}
	}
	return model.ThreadState{}
}

// myThreadItem returns the item I speak for in a thread: my open
// participant item, else any participant item of mine, else the item of my
// latest message there, else my active item.
func (e *Engine) myThreadItem(t model.ThreadState) model.ItemID {
	items := e.myState().Items
	for _, p := range t.Participants {
		if i := itemIndex(items, p.Item); p.Agent == e.me.ID && i >= 0 && items[i].Status.Open() {
			return p.Item
		}
	}
	for _, p := range t.Participants {
		if p.Agent == e.me.ID && p.Item != "" {
			return p.Item
		}
	}
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if m := t.Messages[i]; m.From == e.me.ID && m.Item != "" {
			return m.Item
		}
	}
	if it, ok := e.activeItem(); ok {
		return it.ID
	}
	return ""
}

// proposalOf turns a proposal argument into a proposal with a fresh id and
// a resolved owner: "me" is this agent, "them" the one other participant,
// and an agent id must take part in the thread (docs/SPEC.md, section 4.1).
// about is discuss's argument, for the options of a refusal: each proposes
// again with the agent's summary and signature, or "..." in place of the
// one that was missing or too long.
func (e *Engine) proposalOf(t model.ThreadState, about string, in api.ProposalInput) (model.Proposal, *api.Result) {
	agents := threadAgents(t)
	var others []model.AgentID
	for _, a := range agents {
		if a != e.me.ID {
			others = append(others, a)
		}
	}
	summary := strings.TrimSpace(in.Summary)
	signature := strings.TrimSpace(in.Signature)
	summaryOK := summary != "" && utf8.RuneCountInString(summary) <= maxMessageLen
	signatureOK := utf8.RuneCountInString(signature) <= maxMessageLen
	propose := func(label, owner string) model.Option {
		pr := map[string]any{"owner": owner, "summary": "..."}
		if summaryOK {
			pr["summary"] = summary
		}
		switch {
		case signature == "":
		case signatureOK:
			pr["signature"] = signature
		default:
			pr["signature"] = "..."
		}
		return toolOption(label, "discuss", "about", about, "proposal", pr)
	}
	ownerOptions := func() []model.Option {
		opts := []model.Option{propose("propose that you own it", "me")}
		for _, a := range others {
			opts = append(opts, propose("propose that "+string(a)+" owns it", string(a)))
		}
		return opts
	}
	owner := strings.ToLower(strings.TrimSpace(in.Owner))
	var id model.AgentID
	switch owner {
	case "":
		r := refused("A proposal needs an owner: me, them, or a participant's agent id.", nil, ownerOptions()...)
		return model.Proposal{}, &r
	case "me":
		id = e.me.ID
	case "them":
		if len(others) != 1 {
			r := refused(fmt.Sprintf(`"them" names the one other participant, but this thread has %d: name the owner.`, len(others)),
				nil, ownerOptions()...)
			return model.Proposal{}, &r
		}
		id = others[0]
	default:
		id = model.AgentID(owner)
		if !slices.Contains(agents, id) {
			r := refused(fmt.Sprintf("%q does not take part in the thread about %s.", in.Owner, threadSubject(t)),
				[]string{"participants: " + joinIDs(agents)}, ownerOptions()...)
			return model.Proposal{}, &r
		}
	}
	switch again := propose("propose again", owner); {
	case summary == "":
		r := refused("A proposal needs a one-sentence summary.", nil, again)
		return model.Proposal{}, &r
	case !summaryOK || !signatureOK:
		r := refused(fmt.Sprintf("Keep the proposal's summary and signature under %d characters each.", maxMessageLen), nil, again)
		return model.Proposal{}, &r
	}
	return model.Proposal{ID: model.NewProposalID(), Owner: id, Signature: signature, Summary: summary}, nil
}

// threadMessage builds my next message in a thread: it names every
// participant's item, so the others can rebuild the thread from outboxes,
// and every message so far as seen, so order is causal.
func (e *Engine) threadMessage(t model.ThreadState, item model.ItemID, text string, p *model.Proposal) model.Message {
	seen := make([]string, 0, len(t.Messages))
	for _, m := range t.Messages {
		seen = append(seen, m.ID)
	}
	items := []model.ItemID{item}
	for _, pt := range t.Participants {
		if pt.Item != "" {
			items = append(items, pt.Item)
		}
	}
	slices.Sort(items)
	return model.Message{
		ID:          model.NewMessageID(),
		Thread:      t.ID,
		Kind:        t.Kind,
		Subject:     t.Subject,
		SubjectName: t.SubjectName,
		NoteKind:    t.NoteKind,
		Items:       slices.Compact(items),
		From:        e.me.ID,
		Item:        item,
		Text:        text,
		Proposal:    p,
		Seen:        seen,
		At:          e.now().UTC(),
	}
}

// quotaRefusal refuses a message beyond my quota in a signature thread. The
// way forward is to accept a proposal that is open, or to wait.
func (e *Engine) quotaRefusal(t model.ThreadState) api.Result {
	var opts []model.Option
	for _, id := range openProposals(t) {
		if !slices.Contains(t.Accepted[id], e.me.ID) {
			opts = append(opts, toolOption("accept proposal "+id, "accept", "proposal", id))
		}
	}
	return refused(fmt.Sprintf("You have used your %d messages in the thread about %s.", e.cfg.ThreadMessages, threadSubject(t)),
		[]string{fmt.Sprintf("the thread settles when every participant used theirs, or at %s; without agreement the claim holder owns it",
			t.Deadline.UTC().Format("2006-01-02 15:04 UTC"))},
		append(opts, statusOption())...)
}

// settledRefusal refuses to discuss or accept in a settled thread, saying
// how it ended and offering the step my side takes next.
func (e *Engine) settledRefusal(t model.ThreadState) api.Result {
	return refused(fmt.Sprintf("The thread about %s is settled: %s.", threadSubject(t), e.threadOutcome(t)), nil,
		e.threadNextSteps(t)...)
}

// threadOutcome says how a settled thread ended, from my side.
func (e *Engine) threadOutcome(t model.ThreadState) string {
	switch {
	case t.Agreement != nil:
		return "everyone agreed that " + e.owns(t.Owner) + " it"
	case t.Owner == e.me.ID:
		return "it reached its bound without agreement, so you keep it as the claim holder"
	case t.Owner != "":
		return "it reached its bound without agreement, so the claim holder " + string(t.Owner) + " keeps it"
	case t.FellBack:
		return "it reached its bound without agreement and nobody holds the claim, so the first to claim it owns it"
	}
	return "it is over"
}

// threadNextSteps are the options after a thread settled (docs/SPEC.md,
// section 11): the owner of a signature publishes it, the others need it
// (or use it once published), anyone may claim an unowned one; on a
// duplicate task the non-owner abandons or starts over.
func (e *Engine) threadNextSteps(t model.ThreadState) []model.Option {
	var opts []model.Option
	name := t.SubjectName
	switch {
	case t.Kind == model.ThreadSignature && name != "":
		switch t.Owner {
		case "":
			opts = append(opts, toolOption("claim it", "claim", "symbol", name))
		case e.me.ID:
			opts = append(opts, toolOption("publish the signature the others build on", "publish", "symbol", name))
		default:
			cv, held := e.claimFor(t.Subject)
			n, needed := e.myNeed(t.Subject)
			if held && cv.Claim.Contract != nil && (!needed || n.PinnedSigHash == "") {
				opts = append(opts, toolOption("bring their published signature into your branch", "use", "symbol", name))
			} else if !needed {
				opts = append(opts, toolOption("record that you need it; gitlet tells you when it is published", "need", "symbol", name))
			}
		}
	case t.NoteKind == model.NoteDuplicate && t.Owner != "" && t.Owner != e.me.ID:
		opts = append(opts,
			toolOption("stop your task", "abandon"),
			toolOption("start again with a new scope", "start", "intent", ""))
	}
	return append(opts, statusOption())
}

// applySettledThread carries out at once what a thread this verb settled
// decides for this agent (claim, transfer or need; docs/SPEC.md, section
// 11), which Tick would otherwise do on its next run: the CLI never ticks.
// What fails is left to Tick and, unless it is a lost race or the network,
// reported as a warning.
func (e *Engine) applySettledThread(ctx context.Context, id string) {
	if e.readOnly() {
		return
	}
	for _, d := range rules.Decisions(e.lightView(ctx)) {
		if d.Thread != id {
			continue
		}
		for _, a := range d.Actions {
			err := e.applyAction(ctx, d, a)
			if err == nil || errors.Is(err, bus.ErrClaimLost) || errors.Is(err, bus.ErrUnreachable) ||
				gitx.IsNetwork(err) || errBudget(err) {
				continue
			}
			e.verbWarnings = append(e.verbWarnings, fmt.Sprintf("the outcome of thread %s is not applied yet (%s): %v", id, a, err))
		}
	}
}

// ownerName names an agent in a sentence: "you" for this agent.
func (e *Engine) ownerName(a model.AgentID) string {
	if a == e.me.ID {
		return "you"
	}
	if a == "" {
		return "nobody"
	}
	return string(a)
}

// owns says that a owns something: "you own", "<agent> owns".
func (e *Engine) owns(a model.AgentID) string {
	if a == e.me.ID {
		return "you own"
	}
	return e.ownerName(a) + " owns"
}

// participantList names a thread's participants, me as "you".
func (e *Engine) participantList(t model.ThreadState) string {
	var names []string
	for _, a := range threadAgents(t) {
		names = append(names, e.ownerName(a))
	}
	if len(names) == 0 {
		return "nobody yet"
	}
	return strings.Join(names, ", ")
}

// threadAgents returns a thread's distinct participant agents, sorted.
func threadAgents(t model.ThreadState) []model.AgentID {
	var out []model.AgentID
	for _, p := range t.Participants {
		if p.Agent != "" {
			out = append(out, p.Agent)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// messagesFrom counts the messages agent wrote in thread.
func messagesFrom(msgs []model.Message, thread string, agent model.AgentID) int {
	n := 0
	for _, m := range msgs {
		if m.Thread == thread && m.From == agent {
			n++
		}
	}
	return n
}

// openProposals lists the proposals that count in a thread (those within
// their author's quota), in causal order.
func openProposals(t model.ThreadState) []string {
	var out []string
	for _, m := range t.Messages {
		if p := m.Proposal; p != nil && t.Accepted[p.ID] != nil && !slices.Contains(out, p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

// proposalAuthor returns the author of the message that carries a proposal.
func proposalAuthor(t model.ThreadState, id string) model.AgentID {
	for _, m := range t.Messages {
		if m.Proposal != nil && m.Proposal.ID == id {
			return m.From
		}
	}
	return ""
}

// agreedOn reports whether a thread settled by agreeing on proposal id.
func agreedOn(t model.ThreadState, id string) bool {
	return t.Settled && t.Agreement != nil && t.Agreement.ID == id
}

// threadSubject names what a thread is about.
func threadSubject(t model.ThreadState) string {
	if s := firstNonEmpty(t.SubjectName, t.Subject); s != "" {
		return s
	}
	return t.ID
}

// ---- accept ----

func (e *Engine) accept(ctx context.Context, in api.AcceptInput) (*api.Reply[api.AcceptData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doAccept(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doAccept records my accept of a proposal in my outbox. When every
// participant accepted it the thread settles, and what it decides for me is
// applied at once (docs/SPEC.md, section 11).
func (e *Engine) doAccept(ctx context.Context, in api.AcceptInput) (*api.Reply[api.AcceptData], error) {
	if refusal := e.worktreeRefusal("accept", in.Where); refusal != nil {
		return reply(*refusal, api.AcceptData{}), nil
	}
	p, t, refusal := e.resolveProposalID(ctx, "accept", "proposal", in.Proposal)
	if refusal != nil {
		return reply(*refusal, api.AcceptData{}), nil
	}
	data := api.AcceptData{Thread: t, Agreed: agreedOn(t, p.ID)}
	subject := threadSubject(t)
	waiting := func(t model.ThreadState) string {
		var pending []model.AgentID
		for _, a := range threadAgents(t) {
			if !slices.Contains(t.Accepted[p.ID], a) {
				pending = append(pending, a)
			}
		}
		if len(pending) == 0 {
			return "every participant accepted it"
		}
		return "waiting for " + joinIDs(pending)
	}
	switch {
	case data.Agreed:
		return reply(nothingToDo(fmt.Sprintf("Proposal %s is already agreed: %s %s.", p.ID, e.owns(t.Owner), subject)), data), nil
	case t.Settled:
		return reply(e.settledRefusal(t), data), nil
	case proposalAuthor(t, p.ID) == e.me.ID:
		return reply(nothingToDo(fmt.Sprintf("Proposal %s is yours: as its proposer you accept it implicitly.", p.ID), waiting(t)), data), nil
	case t.Accepted[p.ID] == nil:
		return reply(refused(fmt.Sprintf("Proposal %s does not count: its author had used up their %d messages in the thread.", p.ID, e.cfg.ThreadMessages),
			nil, statusOption()), data), nil
	case slices.Contains(t.Accepted[p.ID], e.me.ID):
		return reply(nothingToDo(fmt.Sprintf("You already accepted proposal %s.", p.ID), waiting(t)), data), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("accept"), data), nil
	}
	_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		for _, a := range ob.Accepts {
			if a.Thread == t.ID && a.Proposal == p.ID {
				return errNoChange
			}
		}
		ob.Accepts = append(ob.Accepts, model.Accept{Thread: t.ID, Proposal: p.ID, At: e.now().UTC()})
		return nil
	})
	if rr, ok := e.writeRefusal("accept", err); ok {
		return reply(rr, data), nil
	}
	if err != nil {
		return nil, err
	}
	e.appendEvent(ctx, "accept", map[string]any{"thread": t.ID, "proposal": p.ID})

	if after := e.findThread(ctx, t.ID); after.ID != "" {
		data.Thread = after
	}
	data.Agreed = agreedOn(data.Thread, p.ID)
	if !data.Agreed {
		return reply(okResult(fmt.Sprintf("Accepted proposal %s in the thread about %s.", p.ID, subject), waiting(data.Thread)), data), nil
	}
	e.applySettledThread(ctx, data.Thread.ID)
	var next string
	switch owner := data.Thread.Owner; {
	case data.Thread.Kind == model.ThreadSignature && owner == e.me.ID:
		next = "you own it: publish its signature so the others can build on it"
	case data.Thread.Kind == model.ThreadSignature:
		next = "gitlet records your need; build on their signature once it is published (a ready note tells you)"
	case data.Thread.NoteKind == model.NoteDuplicate && owner != e.me.ID:
		next = "abandon your task, or start again with a new scope"
	}
	details := []string{fmt.Sprintf("thread %s: with %s", data.Thread.ID, e.participantList(data.Thread))}
	if next != "" {
		details = append(details, "next: "+next)
	}
	return reply(okResult(fmt.Sprintf("Accepted proposal %s: every participant agreed that %s %s.",
		p.ID, e.owns(data.Thread.Owner), subject), details...), data), nil
}

// ---- dismiss ----

// dismissAs are the ways to dismiss a note (docs/SPEC.md, section 4.1).
var dismissAs = []string{"not_needed", "false_alarm"}

func (e *Engine) dismiss(ctx context.Context, in api.DismissInput) (*api.Reply[api.DismissData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doDismiss(ctx, in)
	if err != nil {
		return nil, err
	}
	// The note goes to the ledger as dismissed, not to finish, which would
	// mark it acted on instead.
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doDismiss closes a note the agent will not act on (docs/SPEC.md, section
// 10.1). It stays dismissed until its version changes.
func (e *Engine) doDismiss(ctx context.Context, in api.DismissInput) (*api.Reply[api.DismissData], error) {
	as := strings.ToLower(strings.TrimSpace(in.As))
	data := api.DismissData{Note: strings.TrimSpace(in.Note), As: as}
	if refusal := e.worktreeRefusal("dismiss", in.Where); refusal != nil {
		return reply(*refusal, data), nil
	}
	valid := slices.Contains(dismissAs, as)
	sn, refusal, err := e.resolveNoteID(ctx, "dismiss", "note", in.Note)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		// The candidates repeat the call: keep a valid as on them.
		for i, o := range refusal.Options {
			if o.Tool == "dismiss" && valid && o.Args != nil {
				refusal.Options[i].Args["as"] = as
			}
		}
		return reply(*refusal, data), nil
	}
	n := sn.Note
	data.Note = n.ID
	if !valid {
		r := refused(fmt.Sprintf("%q is not a way to dismiss a note: use not_needed or false_alarm.", in.As),
			[]string{"not_needed: the note is right, but nothing needs doing", "false_alarm: gitlet is wrong about it"},
			toolOption("it is real but needs no action", "dismiss", "note", n.ID, "as", "not_needed"),
			toolOption("gitlet is wrong about it", "dismiss", "note", n.ID, "as", "false_alarm"))
		return reply(r, data), nil
	}
	switch {
	case sn.Status == ledger.NoteDismissed && sn.DismissedAs == as:
		return reply(nothingToDo(fmt.Sprintf("Note %s is already dismissed as %s.", n.ID, as)), data), nil
	case sn.Status == ledger.NoteResolved:
		return reply(nothingToDo(fmt.Sprintf("Note %s is already resolved: %s", n.ID, n.Title)), data), nil
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > maxReasonLen {
		return reply(refused(fmt.Sprintf("Keep the reason under %d characters.", maxReasonLen), nil,
			toolOption("dismiss it with a shorter reason", "dismiss", "note", n.ID, "as", as)), data), nil
	}
	if e.sameWorktree {
		// The worktree's live session delivers, and closes, this agent's notes.
		return reply(e.readOnlyRefusal("dismiss"), data), nil
	}
	if err := e.led.Dismiss(ctx, e.me.ID, n.ID, as, reason, e.now()); err != nil {
		if errors.Is(err, ledger.ErrReadOnly) {
			return reply(e.readOnlyRefusal("dismiss"), data), nil
		}
		return nil, fmt.Errorf("engine: dismiss %s: %w", n.ID, err)
	}
	e.appendEvent(ctx, "dismiss", map[string]any{"note": n.ID, "kind": n.Kind, "as": as})
	how := "not needed"
	if as == "false_alarm" {
		how = "a false alarm"
	}
	return reply(okResult(fmt.Sprintf("Dismissed note %s as %s: %s", n.ID, how, n.Title)), data), nil
}
