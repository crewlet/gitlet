package engine

// Branch verbs: update, submit, abandon (docs/SPEC.md, sections 7 and 12).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
	"github.com/crewlet/gitlet/internal/symbols"
)

// submitEvalBudget bounds submit's evaluation of main's changes (step 5),
// which may have to compute main's footprint since the item's base.
const submitEvalBudget = 10 * time.Second

// ---- update ----

func (e *Engine) update(ctx context.Context, in api.UpdateInput) (*api.Reply[api.UpdateData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doUpdate(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, in.Note)
	return r, nil
}

// doUpdate rebases the current branch onto main (docs/SPEC.md, section
// 12.3): it fetches main, predicts the result with merge-tree and refuses
// on a conflict, then runs git rebase --autostash, dropping picked stub
// commits whose contracts have landed. A rebase that stops anyway is
// aborted, so the branch is never left half rebased. Afterwards the item's
// base follows main, and the needs main now satisfies are done.
func (e *Engine) doUpdate(ctx context.Context, in api.UpdateInput) (*api.Reply[api.UpdateData], error) {
	var data api.UpdateData
	if refusal := e.worktreeRefusal("update", in.Where); refusal != nil {
		return reply(*refusal, data), nil
	}
	if e.sameWorktree {
		// The worktree's branch is the other session's to rebase. Other
		// read-only reasons only keep state unwritten (followMain).
		return reply(e.readOnlyRefusal("update"), data), nil
	}
	refusal, err := e.operationRefusal(ctx, "update")
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, data), nil
	}
	branch, err := e.repo.CurrentBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: update: %w", err)
	}
	if branch == "" {
		var opts []model.Option
		if it, ok := e.activeItem(); ok && it.Branch != "" {
			opts = append(opts, cmdOption("switch to your work branch", "git switch "+shellQuote(it.Branch)))
		}
		return reply(refused("HEAD is detached: update rebases a branch.", nil, opts...), data), nil
	}
	main, _, refusal, err := e.freshMain(ctx, "update")
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, data), nil
	}
	data.Main = main
	head, err := e.repo.RevParse(ctx, "HEAD^{commit}")
	if errors.Is(err, gitx.ErrNotFound) {
		return reply(refused(fmt.Sprintf("Branch %s has no commits yet.", branch), nil, statusOption()), data), nil
	}
	if err != nil {
		return nil, fmt.Errorf("engine: update: %w", err)
	}
	data.Head = head
	item, hasItem := e.branchItem(branch)
	upstream := e.mainUpstream()

	upToDate, err := e.repo.IsAncestor(ctx, main, head)
	if err != nil {
		return nil, fmt.Errorf("engine: update: %w", err)
	}
	if upToDate {
		done := e.finishNeeds(ctx, item, hasItem, main, head)
		summary := fmt.Sprintf("Branch %s is already up to date with %s.", branch, upstream)
		if len(done) == 0 {
			return reply(nothingToDo(summary), data), nil
		}
		return reply(okResult(summary, doneNeedLines(done)...), data), nil
	}

	drops := e.landedStubs(ctx, main, head)
	if e.gitOK {
		res, err := e.repo.MergeTree(ctx, head, main)
		if err != nil {
			return nil, fmt.Errorf("engine: update: %w", err)
		}
		// Conflicts in files only the dropped stubs touch are expected: the
		// stub's version of the code goes, main's stays.
		if conflicts := withoutPaths(res.Conflicts, e.stubFiles(ctx, drops)); len(conflicts) > 0 {
			data.Conflicts = res.Conflicts
			return reply(refused(fmt.Sprintf("Updating %s onto %s would conflict in %s; nothing was changed.",
				branch, upstream, pluralize(len(res.Conflicts), "file")),
				capLines(res.Conflicts, render.MaxRows), e.manualUpdateOptions()...), data), nil
		}
	}
	editor := ""
	if len(drops) > 0 {
		var oids []string
		for _, n := range drops {
			oids = append(oids, n.PinnedCommit)
		}
		if editor, err = e.dropEditor(oids); err != nil {
			e.verbWarnings = append(e.verbWarnings, "picked stub commits are not dropped: "+err.Error())
			drops = nil
		}
	}
	stash := e.stashTop(ctx)
	res, err := e.repo.Rebase(ctx, gitx.RebaseOptions{Upstream: main, Autostash: true, SequenceEditor: editor})
	var gerr *gitx.Error
	switch {
	case errors.Is(err, gitx.ErrBusy):
		refusal, oerr := e.operationRefusal(ctx, "update")
		if oerr != nil || refusal == nil {
			return nil, fmt.Errorf("engine: update: %w", errors.Join(err, oerr))
		}
		return reply(*refusal, data), nil
	case errors.As(err, &gerr):
		return reply(refused(fmt.Sprintf("git could not rebase %s onto %s: %s. Your branch is unchanged.", branch, upstream, gitReason(gerr.Stderr)),
			nil, cmdOption("see what is in the way", "git status")), data), nil
	case err != nil:
		return nil, fmt.Errorf("engine: update: %w", err)
	case !res.OK:
		data.Conflicts = res.Conflicts
		return reply(refused(fmt.Sprintf("The rebase of %s onto %s stopped on conflicts in %s; gitlet aborted it and your branch is as it was.",
			branch, upstream, pluralize(len(res.Conflicts), "file")),
			capLines(res.Conflicts, render.MaxRows), e.manualUpdateOptions()...), data), nil
	}
	data.Head, data.Rebased = res.Head, true
	if w := e.autostashWarning(ctx, stash); w != "" {
		e.verbWarnings = append(e.verbWarnings, w)
	}
	e.appendEvent(ctx, "update", map[string]any{"branch": branch, "main": main, "head": res.Head})

	details := []string{fmt.Sprintf("%s is now at %s, on top of %s %s", branch, shortOID(res.Head), upstream, shortOID(main))}
	for _, n := range drops {
		data.Dropped = append(data.Dropped, n.PinnedCommit)
		details = append(details, fmt.Sprintf("dropped stub commit %s: the contract for %s has landed", shortOID(n.PinnedCommit), n.Name))
	}
	e.followMain(ctx, item, hasItem, main, res.Head)
	details = append(details, doneNeedLines(e.finishNeeds(ctx, item, hasItem, main, res.Head))...)
	summary := fmt.Sprintf("Rebased %s onto %s.", branch, upstream)
	switch n := len(data.Dropped); {
	case n == 1:
		summary = fmt.Sprintf("Rebased %s onto %s, dropping the stub commit whose contract landed.", branch, upstream)
	case n > 1:
		summary = fmt.Sprintf("Rebased %s onto %s, dropping %d stub commits whose contracts landed.", branch, upstream, n)
	}
	return reply(okResult(summary, details...), data), nil
}

// operationRefusal refuses while a rebase, merge, cherry-pick, revert or
// bisect is in progress, with the commands that finish or abort it.
func (e *Engine) operationRefusal(ctx context.Context, verb string) (*api.Result, error) {
	op, err := e.repo.Operation(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: %s: %w", verb, err)
	}
	if op == "" {
		return nil, nil
	}
	opts := []model.Option{
		cmdOption("finish it once its conflicts are resolved", "git "+op+" --continue"),
		cmdOption("or abort it", "git "+op+" --abort"),
	}
	if op == "bisect" {
		opts = []model.Option{cmdOption("end the bisect", "git bisect reset")}
	}
	r := refused(fmt.Sprintf("Cannot %s: a %s is in progress in this worktree.", verb, op), nil, opts...)
	return &r, nil
}

// freshMain fetches main in the foreground for update and submit
// (bus.RefreshMain, which also moves the user's remote-tracking ref;
// docs/SPEC.md, section 6.2). When the remote is unreachable it falls back
// to main as last fetched, with a warning (offline). A missing main branch
// or a fetch the remote refuses is a refusal.
func (e *Engine) freshMain(ctx context.Context, verb string) (main string, offline bool, refusal *api.Result, err error) {
	oid, err := e.bus.RefreshMain(ctx)
	e.mainOIDs = "" // main may have moved
	switch {
	case err == nil:
		return oid, false, nil, nil
	case gitx.IsNetwork(err):
		e.poll.markOffline(e.now())
		if m := e.mainOID(ctx); m != "" {
			e.verbWarnings = append(e.verbWarnings, fmt.Sprintf("remote unreachable: using %s as last fetched", e.mainUpstream()))
			return m, true, nil, nil
		}
		r := refused(fmt.Sprintf("Cannot %s: the remote is unreachable and %s was never fetched.", verb, e.mainUpstream()), nil,
			toolOption("try again once the remote is back", verb))
		return "", true, &r, nil
	case gitx.IsPermission(err):
		r := refused(fmt.Sprintf("Cannot %s: the remote refused to let this clone fetch %s.", verb, e.cfg.Main), nil,
			cmdOption("check your access to the remote", "gitlet doctor"))
		return "", false, &r, nil
	case errors.Is(err, gitx.ErrNotFound):
		r := refused(fmt.Sprintf("Cannot %s: there is no branch %s.", verb, e.mainUpstream()), nil,
			cmdOption("check the remote and gitlet.main", "gitlet doctor"))
		return "", false, &r, nil
	}
	return "", false, nil, fmt.Errorf("engine: %s: fetch main: %w", verb, err)
}

// mainUpstream names main the way git commands take it: <remote>/<main>,
// or the local main branch without a remote.
func (e *Engine) mainUpstream() string {
	if r := e.bus.Remote(); r != "" {
		return r + "/" + e.cfg.Main
	}
	return e.cfg.Main
}

// manualUpdateOptions are the commands for bringing the branch up to date
// by hand when gitlet predicts or meets a conflict.
func (e *Engine) manualUpdateOptions() []model.Option {
	up := shellQuote(e.mainUpstream())
	return []model.Option{
		cmdOption("rebase by hand and resolve the conflicts", "git rebase --autostash "+up),
		cmdOption("or merge main into your branch and resolve them", "git merge --autostash "+up),
	}
}

// branchItem returns my open item on branch: the active item when it is on
// that branch (or on none yet), else a submitted item of that branch.
func (e *Engine) branchItem(branch string) (model.WorkItem, bool) {
	items := e.myState().Items
	if i := activeIndex(items); i >= 0 && (items[i].Branch == branch || items[i].Branch == "") {
		return items[i], true
	}
	for _, it := range items {
		if it.Status == model.ItemSubmitted && it.Branch == branch {
			return it, true
		}
	}
	return model.WorkItem{}, false
}

// itemNeeds returns my needs that belong to an item (a need without an item
// belongs to every item), sorted by symbol.
func (e *Engine) itemNeeds(item model.ItemID) []model.Need {
	var out []model.Need
	for _, n := range e.myOutbox().Needs {
		if n.Item == item || n.Item == "" {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pinnedLanded reports whether the contract a need pinned has landed on
// main (docs/SPEC.md, section 7). It judges the pinned signature hash and
// stub, as contractLanded does for a claim, so it still answers after the
// owner released its claim.
func (e *Engine) pinnedLanded(ctx context.Context, n model.Need, main string) bool {
	c := model.Claim{Key: n.Key, Contract: &model.Contract{SigHash: n.PinnedSigHash, StubCommit: n.PinnedStub}}
	if cv, ok := e.claimFor(n.Key); ok {
		c.Symbol = cv.Claim.Symbol
	} else if p, q, ok := model.SplitTarget(n.Name); ok {
		c.Symbol = model.Symbol{Path: p, Qualified: q}
	}
	return e.contractLanded(ctx, c, main)
}

// needDone reports whether main now satisfies a need: the contract it
// pinned, or the one published for it, has landed.
func (e *Engine) needDone(ctx context.Context, n model.Need, main string) bool {
	if n.PinnedSigHash != "" {
		return e.pinnedLanded(ctx, n, main)
	}
	cv, ok := e.claimFor(n.Key)
	return ok && cv.Claim.Contract != nil && e.contractLanded(ctx, cv.Claim, main)
}

// landedStubs returns my needs whose picked stub commit is on the branch
// (in main..head) and whose pinned contract has landed on main: update
// drops those commits from the rebase.
func (e *Engine) landedStubs(ctx context.Context, main, head string) []model.Need {
	var out []model.Need
	for _, n := range e.myOutbox().Needs {
		c := n.PinnedCommit
		if c == "" || !e.hasCommit(ctx, c) {
			continue
		}
		if onBranch, err := e.repo.IsAncestor(ctx, c, head); err != nil || !onBranch {
			continue
		}
		if onMain, err := e.repo.IsAncestor(ctx, c, main); err != nil || onMain {
			continue
		}
		if e.pinnedLanded(ctx, n, main) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// stubFiles returns the files the picked stub commits of needs change.
func (e *Engine) stubFiles(ctx context.Context, needs []model.Need) map[string]bool {
	out := map[string]bool{}
	for _, n := range needs {
		files, err := e.repo.DiffNames(ctx, n.PinnedCommit+"^", n.PinnedCommit)
		if err != nil {
			continue
		}
		for _, f := range files {
			out[f] = true
		}
	}
	return out
}

// dropEditor returns the GIT_SEQUENCE_EDITOR command that drops oids from
// the rebase todo: gitlet hook sequence-editor, which reads GITLET_DROP
// (docs/SPEC.md, section 12.3). git runs it with sh, so the list and the
// binary are quoted.
func (e *Engine) dropEditor(oids []string) (string, error) {
	exe := e.opt.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", fmt.Errorf("engine: the gitlet binary is unknown: %w", err)
		}
	}
	return "GITLET_DROP=" + shellQuote(strings.Join(oids, " ")) + " " + shellQuote(exe) + " hook sequence-editor", nil
}

// stashTop returns the newest stash entry ("" when there is none).
func (e *Engine) stashTop(ctx context.Context) string {
	oid, err := e.repo.RevParse(ctx, "refs/stash")
	if err != nil {
		return ""
	}
	return oid
}

// autostashWarning tells the agent when git could not re-apply its
// uncommitted changes after the rebase. git then still exits 0, leaves the
// conflicts in the work tree and keeps the changes as a new stash entry
// named "autostash".
func (e *Engine) autostashWarning(ctx context.Context, before string) string {
	if after := e.stashTop(ctx); after == "" || after == before {
		return ""
	}
	if subject, err := e.repo.Run(ctx, "log", "-g", "-1", "--format=%gs", "refs/stash"); err != nil || strings.TrimSpace(subject) != "autostash" {
		return ""
	}
	return "your uncommitted changes conflicted with the new base: git left conflict markers in the work tree and kept the changes " +
		"in the stash as stash@{0}; resolve the conflicts (git status), then run git stash drop"
}

// followMain brings the item's base, head and footprint in line with the
// rebased branch: a snapshot does it, and the base is set directly when the
// snapshot could not. Problems become warnings; a read-only engine writes
// nothing and says so.
func (e *Engine) followMain(ctx context.Context, it model.WorkItem, hasItem bool, main, head string) {
	if err := e.snapshotNow(ctx); err != nil {
		e.verbWarnings = append(e.verbWarnings, "snapshot failed: "+err.Error())
	}
	if !hasItem {
		return
	}
	if e.readOnly() {
		e.verbWarnings = append(e.verbWarnings, fmt.Sprintf("gitlet is read-only here (%s): your task's base and needs are not updated", e.readOnlyReason()))
		return
	}
	if i := itemIndex(e.myState().Items, it.ID); i >= 0 && e.myState().Items[i].Base == main {
		return
	}
	now := e.now().UTC()
	_, err := e.mutateState(ctx, func(st *model.AgentState) error {
		i := itemIndex(st.Items, it.ID)
		if i < 0 || !st.Items[i].Status.Open() || st.Items[i].Base == main {
			return errNoChange
		}
		st.Items[i].Base, st.Items[i].Head, st.Items[i].UpdatedAt = main, head, now
		return nil
	})
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "your task's base is not updated: "+err.Error())
	}
}

// finishNeeds settles the item's needs after its branch caught up with
// main: a need whose contract has landed is done and dropped (its picked
// stub is gone or redundant), and a picked stub that the rebase rewrote is
// pinned by its new commit. It returns the needs it dropped. Problems
// become warnings; a read-only engine leaves them as they are.
func (e *Engine) finishNeeds(ctx context.Context, it model.WorkItem, hasItem bool, main, head string) []model.Need {
	if !hasItem || e.readOnly() {
		return nil
	}
	done := map[string]bool{}
	repicked := map[string]string{}
	for _, n := range e.itemNeeds(it.ID) {
		if e.needDone(ctx, n, main) {
			done[n.ID] = true
			continue
		}
		if c := n.PinnedCommit; c != "" {
			if ok, err := e.repo.IsAncestor(ctx, c, head); err == nil && !ok {
				repicked[n.ID] = e.repickedStub(ctx, n.PinnedStub, main, head)
			}
		}
	}
	if len(done) == 0 && len(repicked) == 0 {
		return nil
	}
	var dropped []model.Need
	_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		dropped = nil
		kept := ob.Needs[:0:0]
		changed := false
		for _, n := range ob.Needs {
			if done[n.ID] {
				dropped = append(dropped, n)
				changed = true
				continue
			}
			if c, ok := repicked[n.ID]; ok && c != n.PinnedCommit {
				n.PinnedCommit, changed = c, true
			}
			kept = append(kept, n)
		}
		if !changed {
			return errNoChange
		}
		ob.Needs = kept
		return nil
	})
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "your needs are not updated: "+err.Error())
		return nil
	}
	sort.SliceStable(dropped, func(i, j int) bool { return dropped[i].Name < dropped[j].Name })
	return dropped
}

// repickedStub finds the commit in main..head that a rebase made of a
// picked stub: cherry-pick -x recorded the stub in its message.
func (e *Engine) repickedStub(ctx context.Context, stub, main, head string) string {
	if stub == "" {
		return ""
	}
	oids, err := e.repo.LogGrep(ctx, main+".."+head, "cherry picked from commit "+stub)
	if err != nil || len(oids) == 0 {
		return ""
	}
	return oids[0]
}

func doneNeedLines(needs []model.Need) []string {
	var out []string
	for _, n := range needs {
		out = append(out, fmt.Sprintf("need %s is done: its contract is on main", n.Name))
	}
	return out
}

// withoutPaths returns the paths not in skip.
func withoutPaths(paths []string, skip map[string]bool) []string {
	var out []string
	for _, p := range paths {
		if !skip[p] {
			out = append(out, p)
		}
	}
	return out
}

// gitReason picks the line of git's stderr that says what went wrong.
func gitReason(stderr string) string {
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		for _, prefix := range []string{"fatal: ", "error: "} {
			if s, ok := strings.CutPrefix(l, prefix); ok {
				return strings.TrimSuffix(s, ".")
			}
		}
	}
	return strings.TrimSuffix(firstLine(stderr), ".")
}

// pluralize counts a noun: "1 file", "2 files".
func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ---- submit ----

func (e *Engine) submit(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	release, err := e.beginVerb(ctx, true)
	if err != nil {
		return nil, err
	}
	defer release()
	var r *api.Reply[api.SubmitData]
	if strings.TrimSpace(in.Symbol) != "" {
		r, err = e.submitInterface(ctx, in)
	} else {
		r, err = e.doSubmit(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// submission is one run of submit (docs/SPEC.md, section 12.4): what the
// steps found so far.
type submission struct {
	in      api.SubmitInput
	item    model.WorkItem
	head    string // HEAD, committed
	main    string // main as just fetched
	offline bool   // the remote could not be reached for main
	existed bool   // the pull request was already open
	data    api.SubmitData
}

// check records the outcome of one step. ok false with a successful submit
// is a warning.
func (s *submission) check(name string, ok bool, detail string) {
	s.data.Checks = append(s.data.Checks, api.Check{Name: name, OK: ok, Detail: detail})
}

// doSubmit runs submit's nine steps in order; the first that refuses ends
// it, with the checks so far in the data.
func (e *Engine) doSubmit(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	s := &submission{in: in}
	if refusal := e.worktreeRefusal("submit", in.Where); refusal != nil {
		return reply(*refusal, s.data), nil
	}
	if refusal, err := e.submitTarget(ctx, s); err != nil || refusal != nil {
		if err != nil {
			return nil, err
		}
		return reply(*refusal, s.data), nil
	}
	steps := []func(context.Context, *submission) (*api.Result, error){
		e.checkCommitted,      // 1
		e.checkMergesWithMain, // 2
		e.checkUsedContracts,  // 3
		e.checkOwnedContracts, // 4
		e.checkMainChanges,    // 5
		e.checkActNotes,       // 6
		e.pushSubmission,      // 7
		e.openPullRequest,     // 8
	}
	for _, step := range steps {
		refusal, err := step(ctx, s)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return reply(*refusal, s.data), nil
		}
	}

	// 9. Mark the item submitted with its pull request, and publish.
	now := e.now().UTC()
	_, err := e.mutateState(ctx, func(st *model.AgentState) error {
		i := itemIndex(st.Items, s.item.ID)
		if i < 0 || !st.Items[i].Status.Open() {
			return errNoChange
		}
		it := &st.Items[i]
		it.Status, it.Head, it.UpdatedAt = model.ItemSubmitted, s.head, now
		if s.data.PR != "" {
			it.PR = s.data.PR
		}
		return nil
	})
	if rr, ok := e.writeRefusal("submit", err); ok {
		return reply(rr, s.data), nil
	}
	if err != nil {
		return nil, err
	}
	if s.data.PR != "" {
		e.rememberPRClosed(ctx, s.item.ID, "") // gh found it open, or opened it
	}
	e.appendEvent(ctx, "submit", map[string]any{"item": s.item.ID, "branch": s.item.Branch, "head": s.head, "pr": s.data.PR})

	intent, branch := s.item.Intent, s.item.Branch
	var summary string
	switch {
	case e.bus.Remote() == "":
		summary = fmt.Sprintf("Submitted %q: the checks passed and the task is marked ready; there is no remote, so nothing was pushed.", intent)
	case s.data.PR != "" && s.existed:
		summary = fmt.Sprintf("Submitted %q: pushed %s; its pull request is %s.", intent, branch, s.data.PR)
	case s.data.PR != "":
		summary = fmt.Sprintf("Submitted %q: pushed %s and opened %s.", intent, branch, s.data.PR)
	case s.data.CompareURL != "":
		summary = fmt.Sprintf("Submitted %q: pushed %s; open the pull request at %s.", intent, branch, s.data.CompareURL)
	default:
		summary = fmt.Sprintf("Submitted %q: pushed %s to %s; open a pull request on your git host.", intent, branch, e.bus.Remote())
	}
	var details []string
	for _, c := range s.data.Checks {
		line := c.Name + ": ok"
		if !c.OK {
			line = c.Name + ": warning"
		}
		if c.Detail != "" {
			line += ", " + c.Detail
		}
		details = append(details, line)
	}
	return reply(okResult(summary, details...), s.data), nil
}

// submitTarget picks the item submit works on: the active item, or the
// submitted item of the current branch (pushing review fixes). It must
// have a stated task and be checked out.
func (e *Engine) submitTarget(ctx context.Context, s *submission) (*api.Result, error) {
	if e.readOnly() {
		r := e.readOnlyRefusal("submit")
		return &r, nil
	}
	branch, err := e.repo.CurrentBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: submit: %w", err)
	}
	it, ok := e.activeItem()
	if !ok {
		if it, ok = e.branchItem(branch); !ok || branch == "" {
			r := refused("You have no task to submit: start one, and commit your work on its branch.", nil,
				toolOption("state your task", "start", "intent", ""), statusOption())
			return &r, nil
		}
	}
	switch {
	case it.Provisional || strings.TrimSpace(it.Intent) == "":
		r := refused("Your work has no stated task: call start with it first, so the pull request says what it does.", nil,
			toolOption("state your task", "start", "intent", ""))
		return &r, nil
	case it.Branch == "" || it.Branch == e.cfg.Main:
		r := refused(fmt.Sprintf("Your task %q has no work branch: submit pushes a branch, not %s.", it.Intent, e.cfg.Main), nil,
			cmdOption("create a branch for it", "git switch -c "+shellQuote(defaultBranchName(e.cfg.BranchPrefix, it.Intent, it.ID))))
		return &r, nil
	case branch != it.Branch:
		r := refused(fmt.Sprintf("Your task %q is on branch %s, but %s is checked out.", it.Intent, it.Branch, branchName(branch)), nil,
			cmdOption("switch to its branch", "git switch "+shellQuote(it.Branch)))
		return &r, nil
	}
	s.item, s.data.Branch = it, it.Branch
	return nil, nil
}

// checkCommitted is step 1: nothing uncommitted, untracked files included,
// and no git operation half done.
func (e *Engine) checkCommitted(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "committed"
	refusal, err := e.operationRefusal(ctx, "submit")
	if err != nil || refusal != nil {
		if refusal != nil {
			s.check(name, false, "a git operation is in progress")
		}
		return refusal, err
	}
	entries, err := e.repo.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: submit: %w", err)
	}
	if len(entries) > 0 {
		var lines []string
		for _, en := range entries {
			lines = append(lines, strings.TrimSpace(strings.ReplaceAll(en.XY, ".", " "))+" "+en.Path)
		}
		s.check(name, false, pluralize(len(entries), "file")+" not committed")
		summary := "Commit with git first: 1 file is not committed."
		if n := len(entries); n > 1 {
			summary = fmt.Sprintf("Commit with git first: %d files are not committed.", n)
		}
		r := refused(summary, capLines(lines, render.MaxRows),
			cmdOption("see what is not committed", "git status"),
			toolOption("submit again once everything is committed", "submit"))
		return &r, nil
	}
	head, err := e.repo.RevParse(ctx, "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("engine: submit: %w", err)
	}
	s.head = head
	s.check(name, true, "HEAD is "+shortOID(head))
	return nil, nil
}

// checkMergesWithMain is step 2: fetch main and merge HEAD into it in
// memory (git merge-tree); a conflict sends the agent to update.
func (e *Engine) checkMergesWithMain(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "merge"
	main, offline, refusal, err := e.freshMain(ctx, "submit")
	if err != nil || refusal != nil {
		if refusal != nil {
			s.check(name, false, refusal.Summary)
		}
		return refusal, err
	}
	s.main, s.offline = main, offline
	up := e.mainUpstream()
	if !e.gitOK {
		s.check(name, true, "not checked: git is older than 2.38")
		return nil, nil
	}
	res, err := e.repo.MergeTree(ctx, s.head, main)
	if err != nil {
		return nil, fmt.Errorf("engine: submit: %w", err)
	}
	if !res.Clean {
		s.data.Conflicts = res.Conflicts
		s.check(name, false, "conflicts with "+up+" in "+joinCapped(res.Conflicts, maxDetailNames))
		r := refused(fmt.Sprintf("Your branch conflicts with %s in %s: update it first.", up, pluralize(len(res.Conflicts), "file")),
			capLines(res.Conflicts, render.MaxRows), toolOption("bring your branch up to date with main", "update"))
		return &r, nil
	}
	s.check(name, true, "merges cleanly with "+up)
	return nil, nil
}

// checkUsedContracts is step 3: every contract the item pinned has landed
// on main, so the work never lands on an interface main lacks.
func (e *Engine) checkUsedContracts(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "contracts used"
	type pending struct {
		need      model.Need
		owner, pr string
	}
	var waiting []pending
	landed := 0
	for _, n := range e.itemNeeds(s.item.ID) {
		if n.PinnedSigHash == "" {
			continue
		}
		if e.pinnedLanded(ctx, n, s.main) {
			landed++
			continue
		}
		p := pending{need: n, owner: "its owner"}
		if cv, ok := e.claimFor(n.Key); ok {
			p.owner = string(cv.Claim.Owner)
			if cv.Claim.Contract != nil {
				p.pr = cv.Claim.Contract.PR
			}
		}
		waiting = append(waiting, p)
	}
	if len(waiting) == 0 {
		detail := "your work pins no contract"
		if landed > 0 {
			detail = pluralize(landed, "pinned contract") + " landed"
		}
		s.check(name, true, detail)
		return nil, nil
	}
	var details []string
	var opts []model.Option
	for _, p := range waiting {
		line := fmt.Sprintf("%s: the contract from %s has not landed on %s; ", p.need.Name, p.owner, e.cfg.Main)
		if p.pr != "" {
			line += "its interface pull request is " + p.pr
		} else {
			line += "it has no interface pull request yet"
		}
		details = append(details, line)
		opts = append(opts, toolOption("stop depending on it and adapt to main's signature", "release", "symbol", p.need.Name))
		if about := e.discussAbout(ctx, p.need.Key, ""); about != "" {
			opts = append(opts, toolOption("ask "+p.owner+" when it lands", "discuss", "about", about))
		} else {
			opts = append(opts, toolOption("see who owns it and where it stands", "who", "target", p.need.Name))
		}
	}
	s.check(name, false, fmt.Sprintf("%s not landed", pluralize(len(waiting), "pinned contract")))
	summary := fmt.Sprintf("Cannot submit yet: %d contracts your work builds on have not landed on %s.", len(waiting), e.cfg.Main)
	if p := waiting[0]; len(waiting) == 1 {
		summary = fmt.Sprintf("Cannot submit yet: the contract for %s from %s has not landed on %s", p.need.Name, p.owner, e.cfg.Main)
		if p.pr != "" {
			summary += " (interface pull request " + p.pr + ")"
		}
		summary += "."
	}
	r := refused(summary, details, opts...)
	return &r, nil
}

// checkOwnedContracts is step 4: HEAD's signature of every symbol whose
// contract the item publishes equals the contract, so dependents get what
// they built against.
func (e *Engine) checkOwnedContracts(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "contracts owned"
	var details, refs []string
	var opts []model.Option
	kept := 0
	for _, cv := range e.myClaims() {
		c := cv.Claim
		if c.Item != s.item.ID || c.Contract == nil {
			continue
		}
		sym, ok := e.symbolAt(ctx, s.head, c.Symbol.Path, c.Symbol.Qualified)
		if ok && sym.SigHash == c.Contract.SigHash {
			kept++
			continue
		}
		now := "no such symbol"
		if ok {
			now = sym.Signature
		}
		ref := c.Symbol.Ref()
		refs = append(refs, ref)
		details = append(details, fmt.Sprintf("%s: published %s; your HEAD has %s", ref, c.Contract.Signature, now))
		opts = append(opts, toolOption("publish your current signature of "+ref, "publish", "symbol", ref))
	}
	if len(details) == 0 {
		detail := "your work publishes no contract"
		if kept > 0 {
			detail = pluralize(kept, "published contract") + " kept"
		}
		s.check(name, true, detail)
		return nil, nil
	}
	s.check(name, false, pluralize(len(details), "published contract")+" not kept")
	summary := fmt.Sprintf("Cannot submit: your code no longer matches %d contracts you published.", len(details))
	if len(refs) == 1 {
		summary = "Cannot submit: your code no longer matches the contract you published for " + refs[0] + "."
	}
	r := refused(summary, details, opts...)
	return &r, nil
}

// checkMainChanges is step 5: stale and new_callers against current main.
// They block in Python and JavaScript, where no compiler catches them, and
// are warnings in the other languages.
func (e *Engine) checkMainChanges(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "main's changes"
	v, notes, err := e.evaluate(ctx, submitEvalBudget)
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "notes not updated: "+err.Error())
	}
	var block, warn []model.Note
	for _, n := range notes {
		if n.Item != s.item.ID || (n.Kind != model.NoteStale && n.Kind != model.NoteNewCallers) {
			continue
		}
		if n.Severity == model.SeverityAct && uncompiled(n) {
			block = append(block, n)
		} else {
			warn = append(warn, n)
		}
	}
	if len(block) > 0 {
		var details []string
		for _, n := range block {
			details = append(details, n.Title)
			details = append(details, n.Evidence...)
		}
		s.check(name, false, pluralize(len(block), "unchecked break")+" in Python or JavaScript")
		r := refused("Cannot submit: main changed code your work depends on, in Python or JavaScript where no compiler catches it. Update first.",
			capLines(details, render.MaxRows),
			toolOption("bring your branch up to date with main, then fix the references", "update", "note", block[0].ID))
		return &r, nil
	}
	for _, n := range warn {
		e.verbWarnings = append(e.verbWarnings, n.Title+": update and let the compiler check it")
	}
	switch mv, ok := v.Main[s.item.ID]; {
	case ok && mv.Skipped:
		s.check(name, false, "not checked symbol by symbol: main changed too much since your base")
	case len(warn) > 0:
		s.check(name, false, pluralize(len(warn), "change")+" on main may break your work; the compiler checks them")
	default:
		s.check(name, true, "nothing on main breaks your work")
	}
	return nil, nil
}

// uncompiled reports whether a note is about Python or JavaScript code.
func uncompiled(n model.Note) bool {
	p, _, ok := model.SplitTarget(n.SubjectName)
	if !ok {
		return false
	}
	switch symbols.LangForPath(p) {
	case "python", "javascript":
		return true
	}
	return false
}

// checkActNotes is step 6: open act notes are worth a look, not a refusal.
func (e *Engine) checkActNotes(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "act notes"
	stored, err := e.notes.OpenNotes(ctx, e.me.ID)
	if err != nil {
		s.check(name, false, "not checked: "+err.Error())
		return nil, nil
	}
	var titles []string
	for _, sn := range stored {
		n := sn.Note
		if n.Severity != model.SeverityAct || (n.Item != "" && n.Item != s.item.ID) ||
			n.Kind == model.NoteStale || n.Kind == model.NoteNewCallers {
			continue
		}
		titles = append(titles, n.Title)
	}
	if len(titles) == 0 {
		s.check(name, true, "none open")
		return nil, nil
	}
	s.check(name, false, pluralize(len(titles), "act note")+" open")
	e.verbWarnings = append(e.verbWarnings, fmt.Sprintf("%s open: %s; run status to see them",
		pluralize(len(titles), "act note"), joinCapped(titles, maxDetailNames)))
	return nil, nil
}

// pushSubmission is step 7: push the branch. Without a remote nothing is
// pushed.
func (e *Engine) pushSubmission(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "push"
	remote := e.bus.Remote()
	if remote == "" {
		s.check(name, true, "no remote: nothing pushed")
		return nil, nil
	}
	if s.offline {
		s.check(name, false, "remote unreachable")
		r := refused(fmt.Sprintf("Cannot submit: %s is unreachable, and your branch must be pushed.", remote), nil,
			toolOption("submit again once the remote is back", "submit"))
		return &r, nil
	}
	detail, refusal, err := e.pushBranch(ctx, s.head, s.item.Branch)
	if err != nil || refusal != nil {
		if refusal != nil {
			s.check(name, false, refusal.Summary)
		}
		return refusal, err
	}
	s.data.Pushed = true
	s.check(name, true, detail)
	return nil, nil
}

// pushBranch pushes commit to branch on the remote (bus.PushBranch): a
// fast-forward, or else a lease on the oid this clone pushed there last,
// which it records (docs/SPEC.md, section 12.4, step 7). A branch someone
// else pushed to, or a push the remote rejects, is a refusal.
func (e *Engine) pushBranch(ctx context.Context, commit, branch string) (string, *api.Result, error) {
	remote := e.bus.Remote()
	ref := "refs/heads/" + branch
	last, err := e.led.LastPushed(ctx, ref)
	if err != nil {
		e.log.Debug("last pushed", "ref", ref, "err", err)
		last = ""
	}
	res, err := e.bus.PushBranch(ctx, commit, branch, last)
	if err == nil {
		if !e.led.ReadOnly() {
			if err := e.led.SetLastPushed(ctx, ref, commit); err != nil {
				e.log.Debug("record pushed", "ref", ref, "err", err)
			}
		}
		if res.UpToDate {
			return fmt.Sprintf("%s on %s is already up to date", branch, remote), nil, nil
		}
		return fmt.Sprintf("pushed %s to %s", branch, remote), nil, nil
	}
	var rej *bus.RejectedError
	var gerr *gitx.Error
	var r api.Result
	switch {
	case errors.As(err, &rej) && (rej.Kind == gitx.RejectStale || rej.Kind == gitx.RejectNonFF):
		r = refused(fmt.Sprintf("Cannot push %s: %s has commits on it that this clone did not push; someone else pushed there.", branch, remote),
			[]string{"git said: " + rej.Reason},
			cmdOption("look at what is there", fmt.Sprintf("git fetch %s %s && git log --oneline HEAD..FETCH_HEAD", remote, shellQuote(branch))),
			cmdOption("bring it into your branch", fmt.Sprintf("git pull --rebase %s %s", remote, shellQuote(branch))),
			cmdOption("or, if those commits are yours to replace, overwrite them", fmt.Sprintf("git push --force-with-lease %s %s", remote, shellQuote(branch))))
	case errors.As(err, &rej):
		r = refused(fmt.Sprintf("Cannot push %s: %s rejected it.", branch, remote), append([]string{rej.Reason}, rej.Remote...),
			cmdOption("check what the remote allows", "gitlet doctor"))
	case gitx.IsNetwork(err):
		e.poll.markOffline(e.now())
		r = refused(fmt.Sprintf("Cannot push %s: %s is unreachable.", branch, remote), nil,
			toolOption("submit again once the remote is back", "submit"))
	case gitx.IsPermission(err):
		r = refused(fmt.Sprintf("Cannot push %s: %s refused this clone's push.", branch, remote), nil,
			cmdOption("check your access to the remote", "gitlet doctor"))
	case errors.As(err, &gerr):
		// For example a pre-push hook that declined: submit's push runs them.
		r = refused(fmt.Sprintf("git could not push %s: %s.", branch, gitReason(gerr.Stderr)), nil,
			cmdOption("check the pre-push hook and the remote", "gitlet doctor"),
			toolOption("submit again once it is fixed", "submit"))
	default:
		return "", nil, fmt.Errorf("engine: push %s: %w", branch, err)
	}
	return "", &r, nil
}

// openPullRequest is step 8: a pull request through gh, or the compare URL
// for the forge. Without a remote there is none.
func (e *Engine) openPullRequest(ctx context.Context, s *submission) (*api.Result, error) {
	const name = "pull request"
	if e.bus.Remote() == "" {
		s.check(name, true, "no remote: no pull request")
		return nil, nil
	}
	title := strings.Join(strings.Fields(s.in.Title), " ")
	if title == "" {
		title = s.item.Intent
	}
	pr := e.pullRequest(ctx, prRequest{
		Branch: s.item.Branch,
		Base:   e.cfg.Main,
		Title:  title,
		Body:   e.prBody(s.item, s.in.Body),
		Draft:  s.in.Draft,
	})
	s.data.PR, s.data.CompareURL, s.existed = pr.URL, pr.Compare, pr.Existing
	if pr.Warning != "" {
		e.verbWarnings = append(e.verbWarnings, pr.Warning)
	}
	switch {
	case pr.URL != "" && pr.Existing:
		s.check(name, true, "already open: "+pr.URL)
	case pr.URL != "":
		s.check(name, true, "opened "+pr.URL)
	case pr.Compare != "":
		s.check(name, false, "not opened: open it at "+pr.Compare)
	default:
		s.check(name, false, "not opened: open it on your git host")
	}
	return nil, nil
}

// prBody is a pull request's body: the agent's own text, then the intent,
// the contracts the work uses and owns, and the provenance lines landing
// detection and why look for (docs/SPEC.md, sections 7 and 12.5).
func (e *Engine) prBody(it model.WorkItem, body string) string {
	var b strings.Builder
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString(body + "\n\n")
	}
	b.WriteString("Task: " + it.Intent + "\n")
	var used []string
	for _, n := range e.itemNeeds(it.ID) {
		if n.PinnedSigHash == "" {
			continue
		}
		line := n.Name + " (signature hash " + n.PinnedSigHash + ")"
		if cv, ok := e.claimFor(n.Key); ok {
			if c := cv.Claim.Contract; c != nil && c.SigHash == n.PinnedSigHash {
				line = n.Name + ": " + c.Signature
			}
			line += ", owned by " + string(cv.Claim.Owner)
		}
		used = append(used, line)
	}
	writeList(&b, "Contracts used", used)
	var owned []string
	for _, cv := range e.myClaims() {
		if c := cv.Claim; c.Item == it.ID && c.Contract != nil {
			line := c.Symbol.Ref() + ": " + c.Contract.Signature
			if c.Contract.PR != "" {
				line += " (interface pull request " + c.Contract.PR + ")"
			}
			owned = append(owned, line)
		}
	}
	writeList(&b, "Contracts owned", owned)
	fmt.Fprintf(&b, "\n%s: %s\n%s: %s\n", itemTrailer, it.ID, agentTrailer, e.me.ID)
	return b.String()
}

func writeList(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n" + heading + ":\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
}

// submitInterface opens an interface-only pull request for a contract I
// published (docs/SPEC.md, section 12.4): the stub commit pushed as
// <branchPrefix>interface-<slug>, titled "interface: <symbol>", and the
// pull request recorded on the contract. Its body names no item, so its
// merge does not land the item.
func (e *Engine) submitInterface(ctx context.Context, in api.SubmitInput) (*api.Reply[api.SubmitData], error) {
	var data api.SubmitData
	if refusal := e.worktreeRefusal("submit", in.Where); refusal != nil {
		return reply(*refusal, data), nil
	}
	sym, refusal, err := e.resolveSymbol(ctx, "submit", in.Symbol)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, data), nil
	}
	ref := sym.Ref()
	cv, held := e.claimFor(sym.Key)
	switch {
	case held && cv.Claim.Owner != e.me.ID:
		return reply(refused(fmt.Sprintf("%s is owned by %s: only its owner opens its interface pull request.", ref, cv.Claim.Owner), nil,
			toolOption("see who owns and needs it", "who", "target", ref)), data), nil
	case !held || cv.Claim.Contract == nil:
		return reply(refused(fmt.Sprintf("You have not published %s: publish its signature first.", ref), nil,
			toolOption("publish its signature (gitlet claims it first if needed)", "publish", "symbol", ref)), data), nil
	}
	ct := *cv.Claim.Contract
	data.Branch = e.cfg.BranchPrefix + "interface-" + model.Slug(ref, 40)
	switch {
	case e.contractLanded(ctx, cv.Claim, e.mainOID(ctx)):
		return reply(nothingToDo(fmt.Sprintf("The contract for %s has already landed on %s.", ref, e.cfg.Main)), data), nil
	case ct.PR != "":
		data.PR = ct.PR
		return reply(nothingToDo(fmt.Sprintf("The interface pull request for %s is already open: %s.", ref, ct.PR)), data), nil
	case e.readOnly():
		return reply(e.readOnlyRefusal("submit"), data), nil
	case e.bus.Remote() == "":
		return reply(refused("An interface pull request needs a remote, and this repository has none.", nil,
			toolOption("submit your whole task instead", "submit")), data), nil
	case !e.hasCommit(ctx, ct.StubCommit):
		return reply(refused(fmt.Sprintf("The stub commit %s of %s is not in this clone.", shortOID(ct.StubCommit), ref), nil,
			statusOption()), data), nil
	}
	detail, refusal, err := e.pushBranch(ctx, ct.StubCommit, data.Branch)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, data), nil
	}
	data.Pushed = true
	pr := e.pullRequest(ctx, prRequest{
		Branch: data.Branch,
		Base:   e.cfg.Main,
		Title:  "interface: " + ref,
		Body:   e.interfaceBody(ref, ct, in.Body),
		Draft:  in.Draft,
	})
	data.PR, data.CompareURL = pr.URL, pr.Compare
	if pr.Warning != "" {
		e.verbWarnings = append(e.verbWarnings, pr.Warning)
	}
	if pr.URL != "" {
		c := cv.Claim
		ct.PR = pr.URL
		c.Contract = &ct
		c.LeaseSeconds, c.UpdatedAt = leaseSeconds(e.cfg.Lease), e.now().UTC()
		if _, err := e.updateClaim(ctx, c, cv.OID, ct.StubCommit); err != nil {
			rr, ok := e.writeRefusal("submit", err)
			if !ok {
				return nil, err
			}
			e.verbWarnings = append(e.verbWarnings, "the pull request is open but not recorded on the claim: "+rr.Summary)
		}
	}
	e.appendEvent(ctx, "submit-interface", map[string]any{"symbol": ref, "branch": data.Branch, "pr": data.PR})
	var summary string
	switch {
	case pr.URL != "" && pr.Existing:
		summary = fmt.Sprintf("The interface pull request for %s is %s.", ref, pr.URL)
	case pr.URL != "":
		summary = fmt.Sprintf("Opened the interface pull request for %s: %s.", ref, pr.URL)
	case pr.Compare != "":
		summary = fmt.Sprintf("Pushed the stub of %s as %s; open the interface pull request at %s.", ref, data.Branch, pr.Compare)
	default:
		summary = fmt.Sprintf("Pushed the stub of %s as %s; open a pull request for it on your git host.", ref, data.Branch)
	}
	return reply(okResult(summary, detail, "signature: "+ct.Signature, "stub commit: "+ct.StubCommit,
		"the stub must build on its own: check it out and build it"), data), nil
}

// interfaceBody is an interface-only pull request's body: the agent's
// text, the signature and the stub, and the agent line. It carries no
// Gitlet-Item line, which would make its merge land the whole item.
func (e *Engine) interfaceBody(ref string, ct model.Contract, body string) string {
	var b strings.Builder
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString(body + "\n\n")
	}
	fmt.Fprintf(&b, "Interface-only change: the new signature of %s, published so that work depending on it can build on it before the implementation lands.\n\n", ref)
	fmt.Fprintf(&b, "Signature: %s\nStub commit: %s\n", ct.Signature, ct.StubCommit)
	fmt.Fprintf(&b, "\n%s: %s\n", agentTrailer, e.me.ID)
	return b.String()
}

// ---- abandon ----

func (e *Engine) abandon(ctx context.Context, in api.AbandonInput) (*api.Reply[api.AbandonData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doAbandon(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doAbandon stops an open item (docs/SPEC.md, section 7): the one item
// names, else the active item, else the submitted item of the current
// branch, such as one whose pull request was closed unmerged. It tells the
// item's open threads, marks it abandoned, and releases its claims, needs
// and snapshot ref. The branch and any pull request stay as they are.
func (e *Engine) doAbandon(ctx context.Context, in api.AbandonInput) (*api.Reply[api.AbandonData], error) {
	var data api.AbandonData
	if refusal := e.worktreeRefusal("abandon", in.Where); refusal != nil {
		return reply(*refusal, data), nil
	}
	it, other, err := e.abandonTarget(ctx, in)
	if err != nil {
		return nil, err
	}
	if other != nil {
		return reply(*other, data), nil
	}
	data.Item = it.ID
	reason := strings.Join(strings.Fields(in.Reason), " ")
	if utf8.RuneCountInString(reason) > maxReasonLen {
		return reply(refused(fmt.Sprintf("Keep the reason under %d characters.", maxReasonLen), nil,
			fillIn(toolOption("abandon it with a shorter reason", "abandon", "item", strings.TrimSpace(in.Item)), "reason")), data), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("abandon"), data), nil
	}
	told, err := e.tellAbandoned(ctx, it, reason)
	if rr, ok := e.writeRefusal("abandon", err); ok {
		return reply(rr, data), nil
	}
	if err != nil {
		return nil, err
	}
	now := e.now().UTC()
	_, err = e.mutateState(ctx, func(st *model.AgentState) error {
		i := itemIndex(st.Items, it.ID)
		if i < 0 || !st.Items[i].Status.Open() {
			return errNoChange
		}
		st.Items[i].Status, st.Items[i].ClosedAt, st.Items[i].UpdatedAt = model.ItemAbandoned, &now, now
		return nil
	})
	if rr, ok := e.writeRefusal("abandon", err); ok {
		return reply(rr, data), nil
	}
	if err != nil {
		return nil, err
	}
	released, err := e.releaseItem(ctx, it.ID)
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "not everything was released: "+err.Error())
	}
	data.Released = released
	e.appendEvent(ctx, "abandon", map[string]any{"item": it.ID, "intent": it.Intent, "reason": reason, "released": released})

	details := []string{"its snapshot ref is deleted"}
	for _, r := range released {
		details = append(details, "released "+r)
	}
	if told > 0 {
		details = append(details, fmt.Sprintf("told the others in %s", pluralize(told, "thread")))
	}
	if it.PR != "" {
		details = append(details, fmt.Sprintf("gitlet does not close %s: close it on your git host if it is still open", it.PR))
	}
	where := "it had no branch"
	if it.Branch != "" {
		where = "branch " + it.Branch + " and its commits are kept"
	}
	return reply(okResult(fmt.Sprintf("Abandoned %q; %s.", it.Intent, where), details...), data), nil
}

// abandonTarget picks the item abandon stops: the open item in.Item names
// (its id or a unique prefix), else the active item, else the submitted
// item of the current branch. When there is none it returns the result to
// give instead: a refusal for an item that matches no open item, or
// nothing_to_do, with a way to abandon each submitted item elsewhere.
func (e *Engine) abandonTarget(ctx context.Context, in api.AbandonInput) (model.WorkItem, *api.Result, error) {
	items := e.myState().Items
	reason := strings.TrimSpace(in.Reason)
	var opts []model.Option
	for _, it := range items {
		if it.Status.Open() {
			opts = append(opts, toolOption(fmt.Sprintf("stop your %s task %q", it.Status, it.Intent), "abandon", "item", string(it.ID), "reason", reason))
		}
	}
	if arg := strings.TrimSpace(in.Item); arg != "" {
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = string(it.ID)
		}
		id, _, err := matchID("", arg, ids)
		if err != nil {
			summary := fmt.Sprintf("No task of yours matches %q.", arg)
			switch {
			case errors.Is(err, errIDShort):
				summary = fmt.Sprintf("%q is too short: give at least %d characters of the task id.", arg, minIDPrefix)
			case errors.Is(err, errIDAmbiguous):
				summary = fmt.Sprintf("%q matches several of your tasks: give more characters.", arg)
			}
			r := refused(summary, []string{"item takes the id of an open task of yours, as status shows it"}, append(opts, statusOption())...)
			return model.WorkItem{}, &r, nil
		}
		it := items[itemIndex(items, model.ItemID(id))]
		if !it.Status.Open() {
			r := nothingToDo(fmt.Sprintf("Your task %q is %s already.", it.Intent, it.Status))
			return model.WorkItem{}, &r, nil
		}
		return it, nil, nil
	}
	if it, ok := e.activeItem(); ok {
		return it, nil, nil
	}
	branch, err := e.repo.CurrentBranch(ctx)
	if err != nil {
		return model.WorkItem{}, nil, fmt.Errorf("engine: abandon: %w", err)
	}
	if it, ok := e.branchItem(branch); ok && branch != "" {
		return it, nil, nil
	}
	r := nothingToDo("You have no active task to abandon.")
	if len(opts) > 0 {
		r.Details = []string{"your submitted tasks stay open until they land; abandon one by its id if its pull request was closed or you dropped it"}
		r.Options = opts
	}
	return model.WorkItem{}, &r, nil
}

// tellAbandoned posts an "abandoned" message, with the reason, into every
// open thread the item takes part in, in one outbox write. It returns how
// many threads it told.
func (e *Engine) tellAbandoned(ctx context.Context, it model.WorkItem, reason string) (int, error) {
	text := fmt.Sprintf("abandoned: I stopped my task %q.", it.Intent)
	if reason != "" {
		text += " Reason: " + reason
	}
	var msgs []model.Message
	for _, t := range e.threads(ctx) {
		in := slices.ContainsFunc(t.Participants, func(p model.Participant) bool { return p.Item == it.ID })
		if t.Settled || !in {
			continue
		}
		msgs = append(msgs, e.threadMessage(t, it.ID, text, nil))
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	_, err := e.mutateOutbox(ctx, func(ob *model.Outbox) error {
		ob.Messages = append(ob.Messages, msgs...)
		return nil
	})
	return len(msgs), err
}
