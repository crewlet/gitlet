package engine

// Interface verbs: claim, publish, need, use, release (docs/SPEC.md,
// sections 4, 6.1, 7, 8, 11 and 12). An interface has at most one owner,
// the holder of its claim. The owner publishes a contract (the signature
// and a stub commit on top of main) on the claim, and the agents that need
// the interface pick the stub into their branches and pin it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

const (
	// stubSubject starts the message of every stub gitlet builds; stubTrailer
	// names the symbol key and signature hash it carries.
	stubSubject = "gitlet stub: "
	stubTrailer = "Gitlet-Stub"
)

// ---- claim ----

func (e *Engine) claim(ctx context.Context, in api.ClaimInput) (*api.Reply[api.ClaimData], error) {
	release, err := e.beginVerb(ctx, true)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doClaim(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doClaim makes this agent the owner of an interface (docs/SPEC.md,
// sections 6.1 and 8): a create-only claim when nobody holds it, or the
// takeover of a lapsed claim with a lease on the exact oid judged lapsed and
// epoch + 1. A claim of mine is left alone, except that claiming an
// automatic claim makes it a deliberate one. A claim held by a live agent
// is refused with need and discuss, and so is a lost race.
func (e *Engine) doClaim(ctx context.Context, in api.ClaimInput) (*api.Reply[api.ClaimData], error) {
	var none api.ClaimData
	if refusal := e.worktreeRefusal("claim", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	sym, refusal, err := e.resolveSymbol(ctx, "claim", in.Symbol, "note", in.Note)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	ref := sym.Ref()
	item := e.myItemFor(sym.Key, "")
	if item == "" {
		return reply(noTaskRefusal("claim", ref), none), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("claim"), none), nil
	}
	cv, held := e.claimFor(sym.Key)
	mine := held && cv.Claim.Owner == e.me.ID
	switch {
	case mine && !cv.Claim.Auto:
		ci := e.claimInfo(ctx, cv, e.now())
		return reply(nothingToDo(fmt.Sprintf("You already hold the claim on %s.", ref), claimLines(ci)...), api.ClaimData{Claim: ci}), nil
	case held && !mine && !e.claimLapsed(ctx, cv):
		return reply(e.heldRefusal(ctx, "claim", sym, cv, in.Note), none), nil
	}
	retry := []any{"symbol", ref, "note", in.Note}
	if e.claimsOffline() {
		r, _ := e.verbWriteRefusal("claim", bus.ErrUnreachable, retry...)
		return reply(r, none), nil
	}

	now := e.now().UTC()
	var (
		view    bus.ClaimView
		summary string
	)
	switch {
	case !held:
		var won bool
		won, view, err = e.createClaim(ctx, e.newClaim(sym, item, false, nil), "")
		if err == nil && !won {
			return reply(e.heldRefusal(ctx, "claim", sym, view, in.Note), none), nil
		}
		summary = fmt.Sprintf("You now own %s: publish its signature early so others can build on it.", ref)
	case mine:
		// An automatic claim the agent now claims itself: the rules no longer
		// release it when the signature change goes away.
		c := cv.Claim
		c.Auto, c.LeaseSeconds, c.UpdatedAt = false, leaseSeconds(e.cfg.Lease), now
		view, err = e.updateClaim(ctx, c, cv.OID, contractParent(c))
		summary = fmt.Sprintf("You hold the claim on %s yourself now (gitlet had taken it automatically).", ref)
	default:
		// Takeover of a lapsed claim: the lease names the exact oid judged
		// lapsed, so a holder that renewed meanwhile wins.
		c := cv.Claim
		c.Owner, c.Item, c.Epoch, c.Auto = e.me.ID, item, cv.Claim.Epoch+1, false
		c.Contract = nil // the new owner publishes its own
		c.Requirements = requirementsBesides(c.Requirements, e.me.ID)
		c.LeaseSeconds, c.UpdatedAt = leaseSeconds(e.cfg.Lease), now
		view, err = e.updateClaim(ctx, c, cv.OID, "")
		summary = fmt.Sprintf("You took over the claim on %s from %s, whose lease lapsed.", ref, cv.Claim.Owner)
	}
	if err != nil {
		if rr, ok := e.claimWriteRefusal(ctx, "claim", sym, in.Note, err, retry...); ok {
			return reply(rr, none), nil
		}
		return nil, err
	}
	var details []string
	if !mine {
		details = append(details, e.dropNeedAsOwner(ctx, sym.Key)...)
	}
	e.appendEvent(ctx, "claim", map[string]any{"symbol": ref, "epoch": view.Claim.Epoch, "item": item, "takeover": held && !mine})
	ci := e.claimInfo(ctx, view, e.now())
	// Its lease starts now. The ref observations still describe the oid it
	// replaced until the next refresh, which would call a takeover lapsed.
	ci.Lapsed = false
	details = append(claimLines(ci), details...)
	return reply(okResult(summary, details...), api.ClaimData{Claim: ci}), nil
}

// noTaskRefusal refuses verb on ref while this agent has no open task:
// claims and needs belong to a task.
func noTaskRefusal(verb, ref string) api.Result {
	return refused(fmt.Sprintf("Cannot %s %s: you have no active task.", verb, ref),
		[]string{"claims and needs belong to a task: state yours first"},
		toolOption("state your task, then call "+verb+" again", "start"))
}

// heldRefusal refuses verb on an interface another agent holds
// (docs/SPEC.md, section 11). It names the holder and offers to build on
// its version (need, or use once published and needed) and to discuss the
// signature, plus the takeover when the holder's lease lapsed.
func (e *Engine) heldRefusal(ctx context.Context, verb string, sym model.Symbol, cv bus.ClaimView, note string) api.Result {
	ref, c := sym.Ref(), cv.Claim
	summary := fmt.Sprintf("Cannot %s %s: %s holds its claim.", verb, ref, c.Owner)
	if c.Owner == "" {
		summary = fmt.Sprintf("Cannot %s %s: another agent holds its claim.", verb, ref)
	}
	var details []string
	if _, it, ok := e.findItem(c.Item); ok && c.Owner != "" && it.Intent != "" {
		details = append(details, fmt.Sprintf("%s's task: %q", c.Owner, it.Intent))
	}
	if ct := c.Contract; ct != nil {
		details = append(details, "published: "+ct.Signature)
	} else if c.Owner != "" {
		details = append(details, "no signature published yet")
	}
	var opts []model.Option
	need, needed := e.myNeed(sym.Key)
	switch {
	case !needed:
		opts = append(opts, toolOption("let them own it: gitlet tells you when its signature is published", "need", "symbol", ref, "note", note))
	case c.Contract != nil && need.PinnedSigHash != c.Contract.SigHash:
		opts = append(opts, toolOption("bring their published signature into your branch", "use", "symbol", ref, "note", note))
	default:
		details = append(details, "you already need it")
	}
	opts = append(opts, e.discussOption(ctx, sym.Key, note, "discuss the signature with them")...)
	if cv.OID != "" && e.claimLapsed(ctx, cv) {
		details = append(details, "its lease lapsed")
		opts = append(opts, toolOption("take the lapsed claim over", "claim", "symbol", ref, "note", note))
	}
	return refused(summary, details, opts...)
}

// claimWriteRefusal turns a failed claim write into a refusal when it is
// one (writeRefusal). A claim that changed meanwhile (bus.ErrClaimLost; the
// engine has reloaded it) names its new holder, or asks to try again when
// nobody else holds it. retry are the call's arguments, for the options
// that repeat it. ok is false for real failures.
func (e *Engine) claimWriteRefusal(ctx context.Context, verb string, sym model.Symbol, note string, err error, retry ...any) (api.Result, bool) {
	if !errors.Is(err, bus.ErrClaimLost) {
		return e.verbWriteRefusal(verb, err, retry...)
	}
	if cv, ok := e.claimFor(sym.Key); ok && cv.Claim.Owner != e.me.ID {
		return e.heldRefusal(ctx, verb, sym, cv, note), true
	}
	return refused(fmt.Sprintf("Cannot %s %s: its claim changed meanwhile.", verb, sym.Ref()), nil,
		toolOption("try again", verb, retry...)), true
}

// verbWriteRefusal is writeRefusal whose "try again" option repeats the
// call with its arguments (key/value pairs, as for toolOption) rather than
// naming the bare verb.
func (e *Engine) verbWriteRefusal(verb string, err error, args ...any) (api.Result, bool) {
	r, ok := e.writeRefusal(verb, err)
	if !ok {
		return r, false
	}
	for i, o := range r.Options {
		if o.Tool == verb && len(o.Args) == 0 {
			r.Options[i] = toolOption(o.Label, verb, args...)
		}
	}
	return r, true
}

// claimsOffline reports whether claim writes are refused without trying: a
// remote is configured and was unreachable at this verb's refresh or at a
// write since (docs/SPEC.md, section 6.3). The bus never falls back to a
// local compare-and-swap anyway; this only spares a push that cannot work.
// verbWriteRefusal with bus.ErrUnreachable words the refusal.
func (e *Engine) claimsOffline() bool {
	if e.bus.Remote() == "" {
		return false
	}
	_, offline := e.poll.snapshot()
	return offline != nil
}

// claimLapsed judges a claim's lease on observed time (docs/SPEC.md,
// section 8).
func (e *Engine) claimLapsed(ctx context.Context, cv bus.ClaimView) bool {
	return e.refLapse(ctx, bus.ClaimRef(cv.Claim.Key), cv.Claim.LeaseSeconds, e.now()).Lapsed
}

// contractParent is the parent a claim commit keeps: the stub of its
// contract, so fetching the claim brings the stub's objects.
func contractParent(c model.Claim) string {
	if c.Contract != nil {
		return c.Contract.StubCommit
	}
	return ""
}

// requirementsBesides drops the requirement an agent stated itself, for a
// claim that agent now owns.
func requirementsBesides(reqs []model.Requirement, agent model.AgentID) []model.Requirement {
	var out []model.Requirement
	for _, r := range reqs {
		if r.From != agent {
			out = append(out, r)
		}
	}
	return out
}

// claimLines describes a claim in a result: its epoch, contract,
// requirements (other agents' text, quoted as data) and who needs it.
func claimLines(ci api.ClaimInfo) []string {
	lines := []string{fmt.Sprintf("claim epoch %d", ci.Epoch)}
	if ct := ci.Contract; ct != nil {
		lines = append(lines, fmt.Sprintf("published: %s (stub %s)", ct.Signature, shortOID(ct.StubCommit)))
	}
	var reqs []string
	for _, r := range ci.Requirements {
		reqs = append(reqs, "requirement: "+quoteLine(r.From, r.Text))
	}
	lines = append(lines, capLines(reqs, render.MaxRows)...)
	if len(ci.Waiting) > 0 {
		lines = append(lines, "needed by "+joinIDs(ci.Waiting))
	}
	return lines
}

// dropNeedAsOwner drops my need on a symbol I have just come to own, which
// no longer makes sense, and says so. A failure becomes a warning.
func (e *Engine) dropNeedAsOwner(ctx context.Context, key string) []string {
	if _, ok := e.myNeed(key); !ok {
		return nil
	}
	if _, dropped, err := e.dropNeed(ctx, key); err != nil {
		e.verbWarnings = append(e.verbWarnings, "your need on it was not dropped: "+err.Error())
		return nil
	} else if !dropped {
		return nil
	}
	return []string{"dropped your need on it: you own it now"}
}

// ---- publish ----

func (e *Engine) publish(ctx context.Context, in api.PublishInput) (*api.Reply[api.PublishData], error) {
	release, err := e.beginVerb(ctx, true)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doPublish(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doPublish publishes the signature of an interface I own (docs/SPEC.md,
// section 12.1). It claims the interface first when nobody holds it, and
// refuses when someone else does. The stub is the commit given, or one
// gitlet builds on top of main from my latest work. Either way the
// signature is read back from the stub and must be my current one; the
// contract then goes onto the claim, whose commit takes the stub as parent.
func (e *Engine) doPublish(ctx context.Context, in api.PublishInput) (*api.Reply[api.PublishData], error) {
	var none api.PublishData
	if refusal := e.worktreeRefusal("publish", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	commit := strings.TrimSpace(in.Commit)
	sym, refusal, err := e.resolveSymbol(ctx, "publish", in.Symbol, "commit", commit, "note", in.Note)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	ref := sym.Ref()
	none.Symbol = ref
	if e.readOnly() {
		return reply(e.readOnlyRefusal("publish"), none), nil
	}
	cv, held := e.claimFor(sym.Key)
	if held && cv.Claim.Owner != e.me.ID {
		return reply(e.heldRefusal(ctx, "publish", sym, cv, in.Note), none), nil
	}
	var prefer model.ItemID
	if held {
		prefer = cv.Claim.Item
	}
	itemID := e.myItemFor(sym.Key, prefer)
	if itemID == "" {
		return reply(noTaskRefusal("publish", ref), none), nil
	}
	items := e.myState().Items
	cur, head, ok := e.currentSymbol(ctx, items[itemIndex(items, itemID)], sym)
	if !ok {
		return reply(e.undefinedRefusal(ctx, sym, held, in.Note), none), nil
	}
	main := e.mainOID(ctx)
	if main == "" {
		return reply(refused(fmt.Sprintf("Cannot publish %s: gitlet does not know %s yet.", ref, e.cfg.Main), nil,
			cmdOption("fetch it", "git fetch")), none), nil
	}
	if old, ok := e.symbolAt(ctx, main, cur.Path, cur.Qualified); ok && old.SigHash == cur.SigHash {
		return reply(refused(fmt.Sprintf("Cannot publish %s: %s already has your signature, so there is nothing to publish.", ref, e.cfg.Main),
			[]string{"signature: " + cur.Signature, "publish shares a new signature before your work lands: change it first"},
			e.discussOption(ctx, sym.Key, in.Note, "discuss the signature with the others")...), none), nil
	}
	if ct := cv.Claim.Contract; held && commit == "" && ct != nil && ct.SigHash == cur.SigHash {
		return reply(e.publishedAlready(ctx, cv), api.PublishData{Symbol: ref, Contract: *ct, Waiting: e.othersWaiting(sym.Key)}), nil
	}
	if e.claimsOffline() {
		r, _ := e.verbWriteRefusal("publish", bus.ErrUnreachable, "symbol", ref, "commit", commit, "note", in.Note)
		return reply(r, none), nil
	}

	var stub, warning string
	if commit == "" {
		if stub, warning, err = e.buildStub(ctx, cur, head, main); err != nil {
			return nil, err
		}
	} else {
		var problem string
		if stub, problem, err = e.explicitStub(ctx, commit, main); err != nil {
			return nil, err
		}
		if problem != "" {
			return reply(refused(fmt.Sprintf("Cannot publish %s: %s.", ref, problem),
				[]string{"a stub is a single commit whose parent is on " + e.cfg.Main + " and which defines the symbol"},
				toolOption("let gitlet build the stub from your work", "publish", "symbol", ref, "note", in.Note)), none), nil
		}
		if ct := cv.Claim.Contract; held && ct != nil && ct.StubCommit == stub && ct.SigHash == cur.SigHash {
			return reply(e.publishedAlready(ctx, cv), api.PublishData{Symbol: ref, Contract: *ct, Waiting: e.othersWaiting(sym.Key)}), nil
		}
	}
	got, refusal := e.verifyStub(ctx, cur, stub, commit != "", in.Note)
	if refusal != nil {
		return reply(*refusal, none), nil
	}

	now := e.now().UTC()
	_, ch, changed := e.myChange(sym.Key)
	contract := &model.Contract{
		Signature:   got.Signature,
		SigHash:     got.SigHash,
		StubCommit:  stub,
		Breaking:    changed && ch.Breaking,
		PublishedAt: now,
	}
	var view bus.ClaimView
	if held {
		c := cv.Claim
		c.Contract, c.Auto, c.LeaseSeconds, c.UpdatedAt = contract, false, leaseSeconds(e.cfg.Lease), now
		view, err = e.updateClaim(ctx, c, cv.OID, stub)
	} else {
		c := e.newClaim(cur, itemID, false, nil)
		c.Contract = contract
		var won bool
		won, view, err = e.createClaim(ctx, c, stub)
		if err == nil && !won {
			return reply(e.heldRefusal(ctx, "publish", sym, view, in.Note), none), nil
		}
	}
	if err != nil {
		// The stub stays valid: a retry publishes the same commit.
		if rr, ok := e.claimWriteRefusal(ctx, "publish", sym, in.Note, err, "symbol", ref, "commit", stub, "note", in.Note); ok {
			return reply(rr, none), nil
		}
		return nil, err
	}

	data := api.PublishData{Symbol: ref, Contract: *contract, Waiting: e.othersWaiting(sym.Key)}
	summary := fmt.Sprintf("Published %s: %s.", ref, contract.Signature)
	details := []string{
		fmt.Sprintf("contract: %s (%s)", contract.Signature, compatibility(contract.Breaking)),
		fmt.Sprintf("stub commit %s: one commit on top of %s changing %s", stub, e.cfg.Main, cur.Path),
		fmt.Sprintf("the stub must build on its own: check out %s and build it", stub),
	}
	if !held {
		summary = fmt.Sprintf("Claimed and published %s: %s.", ref, contract.Signature)
		details = append(details, "nobody held its claim, so you hold it now")
	}
	if len(data.Waiting) > 0 {
		details = append(details, "waiting for it: "+joinIDs(data.Waiting)+" (they get a ready note)")
	}
	details = append(details, e.dropNeedAsOwner(ctx, sym.Key)...)
	r := reply(okResult(summary, details...), data)
	if warning != "" {
		r.Warnings = append(r.Warnings, warning)
	}
	if p, from, ok := e.agreedProposal(ctx, sym.Key, view.Claim.Epoch); ok && p.Signature != "" {
		r.Data.ProposalSignature = p.Signature
		if !sameSignatureText(contract.Signature, p.Signature) {
			agreed := p.Signature
			if from != e.me.ID {
				agreed = quoteLine(from, p.Signature)
			}
			r.Warnings = append(r.Warnings, fmt.Sprintf("the published signature differs from the agreed proposal %s: %s", p.ID, agreed))
		}
	}
	e.appendEvent(ctx, "publish", map[string]any{"symbol": ref, "sigHash": contract.SigHash, "stub": stub, "claimed": !held})
	return r, nil
}

// publishedAlready is publish's result when my current signature is
// published already.
func (e *Engine) publishedAlready(ctx context.Context, cv bus.ClaimView) api.Result {
	ct := cv.Claim.Contract
	ci := e.claimInfo(ctx, cv, e.now())
	return nothingToDo(fmt.Sprintf("Your signature of %s is already published.", ci.Symbol),
		append([]string{fmt.Sprintf("stub commit %s: the stub must build on its own", ct.StubCommit)}, claimLines(ci)...)...)
}

// currentSymbol returns my current version of sym, as the footprints parse
// it, and the commit it was read from: my latest snapshot while the item
// is active, its branch head once submitted.
func (e *Engine) currentSymbol(ctx context.Context, it model.WorkItem, sym model.Symbol) (model.Symbol, string, bool) {
	head := itemHead(it)
	if it.Status == model.ItemActive && e.snap.Commit != "" {
		head = e.snap.Commit
	}
	if head == "" || sym.Path == "" {
		return model.Symbol{}, "", false
	}
	cur, ok := e.symbolAt(ctx, head, sym.Path, sym.Qualified)
	return cur, head, ok
}

// undefinedRefusal refuses publish of a symbol my work does not define.
func (e *Engine) undefinedRefusal(ctx context.Context, sym model.Symbol, held bool, note string) api.Result {
	ref := sym.Ref()
	summary := fmt.Sprintf("Cannot publish %s: your work does not define it.", ref)
	if _, ch, ok := e.myChange(sym.Key); ok && ch.Change == model.ChangeRemoved {
		summary = fmt.Sprintf("Cannot publish %s: your work removes it, so there is no signature to publish.", ref)
	}
	var opts []model.Option
	if held {
		opts = append(opts, toolOption("give up the claim if you no longer change it", "release", "symbol", ref, "note", note))
	} else {
		opts = append(opts, toolOption("build on whoever defines it instead", "need", "symbol", ref, "note", note))
	}
	opts = append(opts, e.discussOption(ctx, sym.Key, note, "discuss it with the others")...)
	return refused(summary, nil, opts...)
}

// buildStub builds the stub of cur (docs/SPEC.md, section 12.1): main's
// version of its file with the symbol's byte range replaced by the
// symbol's full text from head, or with that text appended after a blank
// line when main lacks the symbol, committed as the only change on top of
// main. When main lacks the whole file, the stub takes the file from head
// and warning says that its other content comes along.
func (e *Engine) buildStub(ctx context.Context, cur model.Symbol, head, main string) (stub, warning string, err error) {
	mine, err := e.bg.ReadFile(ctx, head, cur.Path)
	if err != nil {
		return "", "", fmt.Errorf("engine: publish: %w", err)
	}
	if cur.StartByte < 0 || cur.StartByte >= cur.EndByte || cur.EndByte > len(mine) {
		return "", "", fmt.Errorf("engine: publish: %s: byte range %d-%d outside the file", cur.Ref(), cur.StartByte, cur.EndByte)
	}
	text := mine[cur.StartByte:cur.EndByte]
	base, err := e.bg.ReadFile(ctx, main, cur.Path)
	var content []byte
	switch {
	case errors.Is(err, gitx.ErrNotFound):
		content = mine
		warning = fmt.Sprintf("%s does not have %s yet, so the stub adds the whole file from your work, its other content included", e.cfg.Main, cur.Path)
	case err != nil:
		return "", "", fmt.Errorf("engine: publish: %w", err)
	default:
		var old *model.Symbol
		if f, err := e.extract.extract(cur.Path, base); err == nil && f != nil {
			if s, ok := f.Find(cur.Qualified); ok {
				old = &s
			}
		}
		content = spliceSymbol(base, old, text)
	}
	stub, err = e.repo.CommitFile(ctx, main, cur.Path, content, stubMessage(cur), gitx.Identity{})
	if err != nil {
		return "", "", fmt.Errorf("engine: publish: build stub: %w", err)
	}
	return stub, warning, nil
}

// spliceSymbol returns base with the definition old (parsed from base)
// replaced by text, or with text appended after a blank line when old is
// nil.
func spliceSymbol(base []byte, old *model.Symbol, text []byte) []byte {
	var b bytes.Buffer
	if old != nil && old.StartByte >= 0 && old.StartByte <= old.EndByte && old.EndByte <= len(base) {
		b.Write(base[:old.StartByte])
		b.Write(text)
		b.Write(base[old.EndByte:])
		return b.Bytes()
	}
	if trimmed := bytes.TrimRight(base, "\r\n"); len(trimmed) > 0 {
		b.Write(trimmed)
		b.WriteString("\n\n")
	}
	b.Write(text)
	b.WriteString("\n")
	return b.Bytes()
}

// stubMessage is the message of a stub gitlet builds: "gitlet stub:
// <path:Qualified>" and the trailer "Gitlet-Stub: <key>@<sigHash>".
func stubMessage(s model.Symbol) string {
	return fmt.Sprintf("%s%s\n\n%s: %s@%s\n", stubSubject, s.Ref(), stubTrailer, s.Key, s.SigHash)
}

// explicitStub resolves publish's commit argument and checks that it is a
// stub (docs/SPEC.md, section 12.1): one commit whose only parent is on
// main. verifyStub checks that it defines the symbol. problem explains a
// commit that does not qualify; err is for gitlet failures.
func (e *Engine) explicitStub(ctx context.Context, arg, main string) (oid, problem string, err error) {
	oid, err = e.bg.RevParse(ctx, arg+"^{commit}")
	if err != nil {
		return "", fmt.Sprintf("%q is not a commit in this repository", arg), nil
	}
	out, err := e.bg.Run(ctx, "rev-parse", oid+"^@")
	if err != nil {
		return "", "", fmt.Errorf("engine: publish: parents of %s: %w", oid, err)
	}
	parents := strings.Fields(out)
	if len(parents) != 1 {
		return oid, fmt.Sprintf("commit %s has %d parents, but a stub is one commit on top of %s", shortOID(oid), len(parents), e.cfg.Main), nil
	}
	onMain, err := e.bg.IsAncestor(ctx, parents[0], main)
	if err != nil {
		return "", "", fmt.Errorf("engine: publish: %w", err)
	}
	if !onMain {
		return oid, fmt.Sprintf("the parent of commit %s is not on %s, but a stub is one commit on top of %s", shortOID(oid), e.cfg.Main, e.cfg.Main), nil
	}
	return oid, "", nil
}

// verifyStub reads the symbol back from a stub (docs/SPEC.md, section
// 12.1): the stub must define it with my current signature, since that is
// what the contract promises. explicit says the stub is the commit the
// caller gave rather than one gitlet built.
func (e *Engine) verifyStub(ctx context.Context, cur model.Symbol, stub string, explicit bool, note string) (model.Symbol, *api.Result) {
	ref := cur.Ref()
	what := "the stub gitlet built"
	hint := "build a stub yourself (one commit on top of " + e.cfg.Main + " that defines it) and publish it with commit"
	var opts []model.Option
	if explicit {
		what = "commit " + shortOID(stub)
		hint = "a stub defines the symbol with the signature your work has now"
		opts = append(opts, toolOption("let gitlet build the stub from your work", "publish", "symbol", ref, "note", note))
	}
	got, ok := e.symbolAt(ctx, stub, cur.Path, cur.Qualified)
	if !ok {
		r := refused(fmt.Sprintf("Cannot publish %s: %s does not define it in %s.", ref, what, cur.Path), []string{hint}, opts...)
		return got, &r
	}
	if got.SigHash != cur.SigHash {
		r := refused(fmt.Sprintf("Cannot publish %s: the signature in %s differs from yours.", ref, what),
			[]string{"in the stub: " + got.Signature, "yours now: " + cur.Signature, hint}, opts...)
		return got, &r
	}
	return got, nil
}

// othersWaiting returns the other agents that need a symbol, sorted.
func (e *Engine) othersWaiting(key string) []model.AgentID {
	var out []model.AgentID
	for _, id := range e.waitingOn(key) {
		if id != e.me.ID {
			out = append(out, id)
		}
	}
	return out
}

func compatibility(breaking bool) string {
	if breaking {
		return "breaking"
	}
	return "compatible"
}

// agreedProposal returns the proposal agreed about a symbol's signature,
// with its author, which publish compares the stub with: the agreement of
// a current signature thread (rules.Threads), or else one reached in the
// signature thread at the claim's epoch or the epoch before. The latter
// outlive the clash, which ends once the other sides record their needs,
// so their agreement is worked out from the outboxes (threadAgreement).
func (e *Engine) agreedProposal(ctx context.Context, key string, epoch int) (model.Proposal, model.AgentID, bool) {
	for _, t := range e.threads(ctx) {
		if t.Kind == model.ThreadSignature && t.Subject == key && t.Agreement != nil {
			for _, m := range t.Messages {
				if m.Proposal != nil && m.Proposal.ID == t.Agreement.ID {
					return *t.Agreement, m.From, true
				}
			}
			return *t.Agreement, "", true
		}
	}
	for ep := epoch; ep >= 0 && ep >= epoch-1; ep-- {
		if p, from, ok := e.threadAgreement(model.SigThreadID(key, ep)); ok {
			return p, from, true
		}
	}
	return model.Proposal{}, "", false
}

// threadAgreement works out a signature thread's agreement from the
// outboxes as the rules do (docs/SPEC.md, section 11): the first proposal,
// in time order and within its author's message quota, that every agent of
// the thread accepted, its author implicitly. The thread's agents are the
// authors of its messages and the agents of the items they were sent to.
func (e *Engine) threadAgreement(thread string) (model.Proposal, model.AgentID, bool) {
	if e.shared == nil {
		return model.Proposal{}, "", false
	}
	var msgs []model.Message
	agents := map[model.AgentID]bool{}
	accepted := map[string]map[model.AgentID]bool{}
	accept := func(proposal string, agent model.AgentID) {
		if accepted[proposal] == nil {
			accepted[proposal] = map[model.AgentID]bool{}
		}
		accepted[proposal][agent] = true
	}
	for _, agent := range append([]model.AgentID{e.me.ID}, e.peerIDs()...) {
		ob := e.shared.Outboxes[agent]
		for _, m := range ob.Messages {
			if m.Thread != thread {
				continue
			}
			m.From = agent // the outbox names the author, whatever the entry says
			msgs = append(msgs, m)
			agents[agent] = true
			for _, it := range m.Items {
				if a, _, ok := e.findItem(it); ok {
					agents[a] = true
				}
			}
		}
		for _, a := range ob.Accepts {
			if a.Thread == thread {
				accept(a.Proposal, agent)
			}
		}
	}
	if len(agents) < 2 {
		return model.Proposal{}, "", false
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		if !msgs[i].At.Equal(msgs[j].At) {
			return msgs[i].At.Before(msgs[j].At)
		}
		return msgs[i].ID < msgs[j].ID
	})
	counts := map[model.AgentID]int{}
	var proposals []model.Message
	for _, m := range msgs {
		if counts[m.From] >= e.cfg.ThreadMessages {
			continue // past its author's quota: it carries no proposal
		}
		counts[m.From]++
		if m.Proposal != nil && m.Proposal.ID != "" {
			accept(m.Proposal.ID, m.From)
			proposals = append(proposals, m)
		}
	}
	for _, m := range proposals {
		all := true
		for a := range agents {
			all = all && accepted[m.Proposal.ID][a]
		}
		if all {
			return *m.Proposal, m.From, true
		}
	}
	return model.Proposal{}, "", false
}

// sameSignatureText compares a declaration with a proposal's free-text
// signature loosely, as the diverged rule does: whitespace and a trailing
// semicolon do not count, and either may leave out a leading part such as
// the keyword or receiver.
func sameSignatureText(decl, proposal string) bool {
	squash := func(s string) string {
		return strings.TrimSuffix(strings.Join(strings.Fields(s), ""), ";")
	}
	a, b := squash(decl), squash(proposal)
	return a == b || (b != "" && strings.HasSuffix(a, b)) || (a != "" && strings.HasSuffix(b, a))
}

// ---- need ----

func (e *Engine) need(ctx context.Context, in api.NeedInput) (*api.Reply[api.NeedData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doNeed(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doNeed records in my outbox that my active task depends on an interface,
// or updates the requirement of that need (docs/SPEC.md, section 11). The
// requirement reaches the owner's claim through the owner's own engine,
// since only the holder writes its claim.
func (e *Engine) doNeed(ctx context.Context, in api.NeedInput) (*api.Reply[api.NeedData], error) {
	var none api.NeedData
	if refusal := e.worktreeRefusal("need", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	sym, refusal, err := e.resolveSymbol(ctx, "need", in.Symbol, "note", in.Note)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	ref := sym.Ref()
	it, ok := e.activeItem()
	if !ok {
		return reply(noTaskRefusal("need", ref), none), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("need"), none), nil
	}
	cv, held := e.claimFor(sym.Key)
	if held && cv.Claim.Owner == e.me.ID {
		return reply(refused(fmt.Sprintf("Cannot need %s: you hold its claim yourself.", ref),
			[]string{"the others build on your version of it"},
			toolOption("publish its signature for them", "publish", "symbol", ref),
			toolOption("give it up so another agent can own it", "release", "symbol", ref)), none), nil
	}

	requirement := strings.TrimSpace(in.Requirement)
	now := e.now().UTC()
	var (
		need    model.Need
		outcome string // "added", "updated", or "" when nothing changed
	)
	_, err = e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		outcome = ""
		for i, n := range ob.Needs {
			if n.Key != sym.Key {
				continue
			}
			need = n
			if requirement == "" || requirement == n.Requirement {
				return errNoChange
			}
			ob.Needs[i].Requirement = requirement
			need, outcome = ob.Needs[i], "updated"
			return nil
		}
		need = model.Need{ID: model.NewMessageID(), Key: sym.Key, Name: ref, Item: it.ID, Requirement: requirement, CreatedAt: now}
		ob.Needs = append(ob.Needs, need)
		outcome = "added"
		return nil
	})
	if rr, ok := e.verbWriteRefusal("need", err, "symbol", ref, "note", in.Note); ok {
		return reply(rr, none), nil
	}
	if err != nil {
		return nil, err
	}

	data := api.NeedData{Need: need}
	var details []string
	switch {
	case held:
		data.Owner, data.Contract = cv.Claim.Owner, cv.Claim.Contract
		line := "owner: " + string(cv.Claim.Owner)
		if _, oit, ok := e.findItem(cv.Claim.Item); ok && oit.Intent != "" {
			line += fmt.Sprintf(" (%q)", oit.Intent)
		}
		details = append(details, line)
		if need.Requirement != "" {
			details = append(details, "your requirement goes onto their claim: "+need.Requirement)
		}
	default:
		details = append(details, "nobody holds its claim yet")
	}
	ct := data.Contract
	if ct != nil {
		details = append(details, fmt.Sprintf("published: %s (stub %s)", ct.Signature, shortOID(ct.StubCommit)))
	}
	var r api.Result
	switch outcome {
	case "added":
		summary := fmt.Sprintf("You now need %s: gitlet tells you when its signature is published.", ref)
		if ct != nil {
			summary = fmt.Sprintf("You now need %s, and its signature is published: call use to bring it into your branch.", ref)
		}
		r = okResult(summary, details...)
	case "updated":
		r = okResult(fmt.Sprintf("Updated what you need from %s.", ref), details...)
	default:
		r = nothingToDo(fmt.Sprintf("You already need %s.", ref), details...)
	}
	if outcome != "" {
		e.appendEvent(ctx, "need", map[string]any{"symbol": ref, "item": need.Item, "outcome": outcome})
	}
	return reply(r, data), nil
}

// dropNeed removes my need on a symbol key from my outbox and returns it.
func (e *Engine) dropNeed(ctx context.Context, key string) (model.Need, bool, error) {
	var dropped model.Need
	found := false
	_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		found = false
		kept := ob.Needs[:0:0]
		for _, n := range ob.Needs {
			if n.Key == key {
				dropped, found = n, true
				continue
			}
			kept = append(kept, n)
		}
		if !found {
			return errNoChange
		}
		ob.Needs = kept
		return nil
	})
	if err != nil {
		return model.Need{}, false, err
	}
	return dropped, found, nil
}

// ---- use ----

func (e *Engine) use(ctx context.Context, in api.UseInput) (*api.Reply[api.UseData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doUse(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doUse cherry-picks a published contract's stub onto the current branch
// and pins the contract on my need, creating the need when missing
// (docs/SPEC.md, section 12.2). After a drift it first reverts the stub it
// picked before. Uncommitted changes to the files involved are refused,
// and a conflict is aborted and reported by file with the branch as it was.
func (e *Engine) doUse(ctx context.Context, in api.UseInput) (*api.Reply[api.UseData], error) {
	var none api.UseData
	if refusal := e.worktreeRefusal("use", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	sym, refusal, err := e.resolveSymbol(ctx, "use", in.Symbol, "note", in.Note)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	ref := sym.Ref()
	none.Symbol = ref
	it, ok := e.activeItem()
	if !ok {
		return reply(noTaskRefusal("use", ref), none), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("use"), none), nil
	}
	cv, held := e.claimFor(sym.Key)
	switch {
	case held && cv.Claim.Owner == e.me.ID:
		return reply(refused(fmt.Sprintf("Cannot use %s: you own it, so your branch has your own version.", ref), nil,
			toolOption("publish its signature for the others", "publish", "symbol", ref)), none), nil
	case !held || cv.Claim.Contract == nil:
		return reply(e.unpublishedRefusal(ctx, sym, cv, held, in.Note), none), nil
	}
	ct := *cv.Claim.Contract
	need, needed := e.myNeed(sym.Key)
	if needed && need.PinnedSigHash == ct.SigHash {
		details := []string{"contract: " + ct.Signature}
		if need.PinnedCommit != "" {
			details = append(details, "picked as commit "+need.PinnedCommit)
		}
		return reply(nothingToDo(fmt.Sprintf("You already use the published signature of %s.", ref), details...),
			api.UseData{Symbol: ref, SigHash: ct.SigHash, Stub: need.PinnedStub, Commit: need.PinnedCommit}), nil
	}
	if !e.hasCommit(ctx, ct.StubCommit) {
		return reply(refused(fmt.Sprintf("Cannot use %s: its stub %s is not in this clone yet.", ref, shortOID(ct.StubCommit)), nil,
			toolOption("try again after the next refresh", "use", "symbol", ref, "note", in.Note)), none), nil
	}
	if op, err := e.repo.Operation(ctx); err != nil {
		return nil, fmt.Errorf("engine: use: %w", err)
	} else if op != "" {
		return reply(refused(fmt.Sprintf("Cannot use %s: a %s is in progress in this worktree.", ref, op), nil,
			cmdOption("finish or abort it first", "git status")), none), nil
	}
	branch, err := e.repo.CurrentBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: use: %w", err)
	}
	if branch == e.cfg.Main {
		var opts []model.Option
		if it.Branch != "" && it.Branch != e.cfg.Main {
			opts = append(opts, cmdOption("switch to your task's branch, then call use again", "git switch "+shellQuote(it.Branch)))
		}
		return reply(refused(fmt.Sprintf("Cannot use %s on %s: stubs go onto your work branch.", ref, e.cfg.Main), nil, opts...), none), nil
	}

	files, err := e.bg.DiffNames(ctx, ct.StubCommit+"^", ct.StubCommit)
	if err != nil {
		return nil, fmt.Errorf("engine: use: files of stub %s: %w", ct.StubCommit, err)
	}
	// After a drift, the stub picked before is reverted first, if it is
	// still on the branch.
	revert := ""
	touched := files
	if needed && need.PinnedCommit != "" && e.hasCommit(ctx, need.PinnedCommit) {
		if on, err := e.bg.IsAncestor(ctx, need.PinnedCommit, "HEAD"); err == nil && on {
			revert = need.PinnedCommit
			old, err := e.bg.DiffNames(ctx, revert+"^", revert)
			if err != nil {
				return nil, fmt.Errorf("engine: use: files of %s: %w", revert, err)
			}
			touched = append(append([]string(nil), files...), old...)
		}
	}
	dirty, err := e.dirtyPaths(ctx, touched)
	if err != nil {
		return nil, err
	}
	if len(dirty) > 0 {
		quoted := make([]string, len(dirty))
		for i, p := range dirty {
			quoted[i] = shellQuote(p)
		}
		return reply(refused(fmt.Sprintf("Cannot use %s: commit your changes to %s first.", ref, joinCapped(dirty, maxDetailNames)),
			[]string{"use cherry-picks the stub onto your branch, so the files it touches must have no uncommitted changes"},
			cmdOption("commit them, then call use again", "git add -- "+strings.Join(quoted, " ")+" && git commit")), none), nil
	}
	before, err := e.repo.RevParse(ctx, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("engine: use: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("engine: use: %w", err)
	}

	if revert != "" {
		res, err := e.repo.Revert(ctx, revert)
		if refusal, err := applyFailure("use", ref, err); err != nil || refusal != nil {
			return replyOrErr(refusal, none, err)
		}
		if !res.OK {
			return reply(refused(fmt.Sprintf("Cannot use %s: reverting the stub you used before (%s) conflicts in %s.", ref, shortOID(revert), joinCapped(res.Conflicts, maxDetailNames)),
				append(conflictLines(res.Conflicts), "nothing changed: gitlet aborted the revert"),
				append([]model.Option{cmdOption("revert it yourself and resolve the conflicts, then call use again", "git revert "+revert)},
					e.discussOption(ctx, sym.Key, in.Note, "discuss the change with the owner")...)...), none), nil
		}
	}
	head := ""
	if !e.hasStubContent(ctx, ct.StubCommit, files) {
		res, err := e.repo.CherryPick(ctx, ct.StubCommit)
		undo := []string{"nothing changed: gitlet aborted the cherry-pick"}
		if revert != "" && (err != nil || !res.OK) {
			undo = e.undoRevert(ctx, before)
		}
		if refusal, err := applyFailure("use", ref, err); err != nil || refusal != nil {
			if refusal != nil && revert != "" {
				refusal.Details = append(refusal.Details, undo...)
			}
			return replyOrErr(refusal, none, err)
		}
		if !res.OK {
			return reply(refused(fmt.Sprintf("Cannot use %s: its stub conflicts with your branch in %s.", ref, joinCapped(res.Conflicts, maxDetailNames)),
				append(conflictLines(res.Conflicts), undo...),
				append([]model.Option{cmdOption("pick it yourself and resolve the conflicts", "git cherry-pick -x "+ct.StubCommit)},
					e.discussOption(ctx, sym.Key, in.Note, "discuss the conflict with the owner")...)...), none), nil
		}
		head = res.Head
	}

	now := e.now().UTC()
	_, err = e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		for i := range ob.Needs {
			if n := &ob.Needs[i]; n.Key == sym.Key {
				n.PinnedSigHash, n.PinnedStub, n.PinnedCommit = ct.SigHash, ct.StubCommit, head
				return nil
			}
		}
		ob.Needs = append(ob.Needs, model.Need{
			ID: model.NewMessageID(), Key: sym.Key, Name: ref, Item: it.ID,
			PinnedSigHash: ct.SigHash, PinnedStub: ct.StubCommit, PinnedCommit: head, CreatedAt: now,
		})
		return nil
	})
	if err != nil {
		picked := fmt.Sprintf("the stub is on your branch (commit %s) but is not pinned yet: call use again to pin it", head)
		if rr, ok := e.verbWriteRefusal("use", err, "symbol", ref, "note", in.Note); ok {
			rr.Details = append(rr.Details, picked)
			return reply(rr, none), nil
		}
		return nil, fmt.Errorf("engine: use: %s: %w", picked, err)
	}

	data := api.UseData{Symbol: ref, SigHash: ct.SigHash, Stub: ct.StubCommit, Commit: head, Reverted: revert}
	where := branchName(branch)
	summary := fmt.Sprintf("Picked the published signature of %s onto %s.", ref, where)
	if head == "" {
		summary = fmt.Sprintf("Your branch already has the published signature of %s: pinned it.", ref)
	}
	details := []string{
		fmt.Sprintf("contract: %s (owner %s)", ct.Signature, cv.Claim.Owner),
		"stub commit: " + ct.StubCommit,
	}
	if head != "" {
		details = append(details, "picked as commit "+head)
	}
	if revert != "" {
		details = append(details, fmt.Sprintf("reverted %s, the stub you used before", revert))
	}
	if !needed {
		details = append(details, "recorded that you need it")
	}
	details = append(details, "update drops the picked stub once the contract lands on "+e.cfg.Main)
	e.appendEvent(ctx, "use", map[string]any{"symbol": ref, "sigHash": ct.SigHash, "stub": ct.StubCommit, "commit": head, "reverted": revert})
	return reply(okResult(summary, details...), data), nil
}

// unpublishedRefusal refuses use of an interface that has no published
// contract.
func (e *Engine) unpublishedRefusal(ctx context.Context, sym model.Symbol, cv bus.ClaimView, held bool, note string) api.Result {
	ref := sym.Ref()
	details := []string{"nobody holds its claim"}
	if held {
		details = []string{fmt.Sprintf("%s holds its claim but has not published a signature", cv.Claim.Owner)}
	}
	var opts []model.Option
	if _, needed := e.myNeed(sym.Key); needed {
		details = append(details, "you need it already: gitlet tells you when its signature is published")
	} else {
		opts = append(opts, toolOption("wait for it: gitlet tells you when its signature is published", "need", "symbol", ref, "note", note))
	}
	opts = append(opts, e.discussOption(ctx, sym.Key, note, "ask when it will be published")...)
	return refused(fmt.Sprintf("Cannot use %s: nothing is published for it yet.", ref), details, opts...)
}

// dirtyPaths returns which of paths have uncommitted changes, staged or
// not, untracked files included, sorted.
func (e *Engine) dirtyPaths(ctx context.Context, paths []string) ([]string, error) {
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[p] = true
	}
	entries, err := e.bg.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: use: %w", err)
	}
	var out []string
	for _, s := range entries {
		if want[s.Path] || (s.OrigPath != "" && want[s.OrigPath]) {
			out = append(out, s.Path)
		}
	}
	sort.Strings(out)
	return compactStrings(out), nil
}

// hasStubContent reports whether HEAD already has the stub's version of
// every file the stub changes, so that there is nothing to pick.
func (e *Engine) hasStubContent(ctx context.Context, stub string, files []string) bool {
	for _, p := range files {
		mine, errMine := e.bg.RevParse(ctx, "HEAD:"+p)
		theirs, errTheirs := e.bg.RevParse(ctx, stub+":"+p)
		if errors.Is(errMine, gitx.ErrNotFound) && errors.Is(errTheirs, gitx.ErrNotFound) {
			continue
		}
		if errMine != nil || errTheirs != nil || mine != theirs {
			return false
		}
	}
	return len(files) > 0
}

// undoRevert takes the branch back to before when use reverted the stub it
// used before but could not pick the new one, so that the refusal leaves
// the branch as it was. reset --keep never discards local changes, and the
// files involved have none. It runs to the end whatever ctx says, and
// returns detail lines for the refusal.
func (e *Engine) undoRevert(ctx context.Context, before string) []string {
	if _, err := e.repo.Run(context.WithoutCancel(ctx), "reset", "--quiet", "--keep", before); err != nil {
		e.log.Debug("undo revert", "err", err)
		return []string{fmt.Sprintf("the stub used before stays reverted; to restore your branch run: git reset --keep %s", before)}
	}
	return []string{"nothing changed: gitlet aborted the cherry-pick and undid the revert of the stub used before"}
}

// applyFailure turns a cherry-pick or revert error into a refusal when
// another git operation is in the way, and wraps any other error.
func applyFailure(verb, ref string, err error) (*api.Result, error) {
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, gitx.ErrBusy):
		r := refused(fmt.Sprintf("Cannot %s %s: another git operation is in progress in this worktree.", verb, ref), nil,
			cmdOption("finish or abort it first", "git status"))
		return &r, nil
	}
	return nil, fmt.Errorf("engine: %s %s: %w", verb, ref, err)
}

// replyOrErr returns a refusal as a reply, or err.
func replyOrErr[D any](refusal *api.Result, data D, err error) (*api.Reply[D], error) {
	if err != nil {
		return nil, err
	}
	return reply(*refusal, data), nil
}

// conflictLines lists conflicted files for a refusal.
func conflictLines(paths []string) []string {
	lines := make([]string, 0, len(paths))
	for _, p := range paths {
		lines = append(lines, "conflict: "+p)
	}
	return capLines(lines, render.MaxRows)
}

// ---- release ----

func (e *Engine) release(ctx context.Context, in api.ReleaseInput) (*api.Reply[api.ReleaseData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doRelease(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doRelease drops my claim on a symbol, deleted with a lease on the oid
// seen, and my need on it. A claim already gone, or taken over meanwhile,
// is no longer mine to release.
func (e *Engine) doRelease(ctx context.Context, in api.ReleaseInput) (*api.Reply[api.ReleaseData], error) {
	var none api.ReleaseData
	if refusal := e.worktreeRefusal("release", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	key, ref, refusal, err := e.releaseTarget(ctx, in)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, none), nil
	}
	none.Symbol = ref
	if e.readOnly() {
		return reply(e.readOnlyRefusal("release"), none), nil
	}
	cv, held := e.claimFor(key)
	mine := held && cv.Claim.Owner == e.me.ID
	need, needed := e.myNeed(key)
	if !mine && !needed {
		var details []string
		if held {
			details = append(details, fmt.Sprintf("%s holds its claim", cv.Claim.Owner))
		}
		return reply(nothingToDo(fmt.Sprintf("You hold no claim or need on %s.", ref), details...), api.ReleaseData{Symbol: ref, Released: []string{}}), nil
	}

	var released, details []string
	if mine {
		if e.claimsOffline() {
			r, _ := e.verbWriteRefusal("release", bus.ErrUnreachable, "symbol", ref, "note", in.Note)
			return reply(r, none), nil
		}
		err := e.deleteClaim(ctx, key, cv.OID)
		switch {
		case errors.Is(err, bus.ErrClaimLost):
			// Taken over after a lapse: the claim is no longer mine.
			if now, ok := e.claimFor(key); ok {
				details = append(details, fmt.Sprintf("the claim had already passed to %s", now.Claim.Owner))
			}
		case err != nil:
			if rr, ok := e.verbWriteRefusal("release", err, "symbol", ref, "note", in.Note); ok {
				return reply(rr, none), nil
			}
			return nil, err
		default:
			released = append(released, "claim")
			if waiting := e.othersWaiting(key); len(waiting) > 0 {
				details = append(details, "they needed it from you: "+joinIDs(waiting))
			}
		}
	}
	if needed {
		dropped, ok, err := e.dropNeed(ctx, key)
		if err != nil {
			if rr, ok := e.verbWriteRefusal("release", err, "symbol", ref, "note", in.Note); ok {
				rr.Details = append(rr.Details, released...)
				return reply(rr, none), nil
			}
			return nil, err
		}
		if ok {
			released = append(released, "need")
			if dropped.PinnedCommit != "" {
				details = append(details, fmt.Sprintf("the stub you picked stays on your branch as commit %s; git revert %s takes it out", dropped.PinnedCommit, dropped.PinnedCommit))
			}
		} else if need.ID != "" {
			details = append(details, "your need was already gone")
		}
	}
	data := api.ReleaseData{Symbol: ref, Released: released}
	if len(released) == 0 {
		data.Released = []string{}
		return reply(nothingToDo(fmt.Sprintf("You hold no claim or need on %s any more.", ref), details...), data), nil
	}
	e.appendEvent(ctx, "release", map[string]any{"symbol": ref, "released": released})
	return reply(okResult(fmt.Sprintf("Released your %s on %s.", strings.Join(released, " and "), ref), details...), data), nil
}

// releaseTarget resolves release's symbol argument. A name that is exactly
// one of my needs or claims is taken as it is, so a symbol that no longer
// exists anywhere can still be released; anything else goes through
// resolveSymbol.
func (e *Engine) releaseTarget(ctx context.Context, in api.ReleaseInput) (key, ref string, refusal *api.Result, err error) {
	arg := strings.TrimSpace(in.Symbol)
	for _, n := range e.myOutbox().Needs {
		if arg != "" && n.Name == arg {
			return n.Key, n.Name, nil, nil
		}
	}
	for _, cv := range e.myClaims() {
		if r := cv.Claim.Symbol.Ref(); arg != "" && r == arg {
			return cv.Claim.Key, r, nil, nil
		}
	}
	sym, refusal, err := e.resolveSymbol(ctx, "release", in.Symbol, "note", in.Note)
	if err != nil || refusal != nil {
		return "", "", refusal, err
	}
	return sym.Key, sym.Ref(), nil, nil
}

// ---- shared ----

// discussOption offers discuss about a symbol (discussAbout), or nothing
// when there is no thread or note to name.
func (e *Engine) discussOption(ctx context.Context, key, note, label string) []model.Option {
	if about := e.discussAbout(ctx, key, note); about != "" {
		return []model.Option{toolOption(label, "discuss", "about", about)}
	}
	return nil
}

// discussAbout returns what a discuss option about a symbol names: an
// unsettled thread about it (a signature thread first), else the note the
// call acts on, else my most urgent open note about the symbol that offers
// discussion. "" when there is none.
func (e *Engine) discussAbout(ctx context.Context, key, note string) string {
	var open []model.ThreadState
	for _, t := range e.threads(ctx) {
		if t.Subject == key && !t.Settled {
			open = append(open, t)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		return open[i].Kind == model.ThreadSignature && open[j].Kind != model.ThreadSignature
	})
	if len(open) > 0 {
		return open[0].ID
	}
	if note != "" {
		if sn, err := e.notes.Note(ctx, e.me.ID, note); err == nil {
			return sn.Note.ID
		}
	}
	stored, err := e.notes.OpenNotes(ctx, e.me.ID)
	if err != nil {
		return ""
	}
	var cands []model.Note
	for _, s := range stored {
		if s.Note.Subject == key && s.Note.Thread != "" {
			cands = append(cands, s.Note)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if a, b := cands[i].Severity.Rank(), cands[j].Severity.Rank(); a != b {
			return a < b
		}
		return cands[i].ID < cands[j].ID
	})
	if len(cands) == 0 {
		return ""
	}
	return cands[0].ID
}
