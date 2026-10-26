package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
	"github.com/crewlet/gitlet/internal/rules"
)

const (
	// similarScore and similarTerms are the intent signal thresholds
	// (docs/SPEC.md, section 10.2).
	similarScore = 0.5
	similarTerms = 2
	// alreadyDoneWindow and alreadyDoneMax: commits on main start checks.
	alreadyDoneWindow = 7 * 24 * time.Hour
	alreadyDoneMax    = 5
	alreadyDoneScan   = 200
	// maxDetailNames caps names listed inside one detail.
	maxDetailNames = 3
	// reflogScan caps the reflog entries of a closed item's branch searched
	// for the commit it had when the item closed.
	reflogScan = 100
)

var errActiveExists = errors.New("engine: an active item exists")

// ---- start ----

func (e *Engine) start(ctx context.Context, in api.StartInput) (*api.Reply[api.StartData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doStart(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doStart opens or adopts the item (docs/SPEC.md, section 7).
func (e *Engine) doStart(ctx context.Context, in api.StartInput) (*api.Reply[api.StartData], error) {
	var none api.StartData
	if refusal := e.worktreeRefusal("start", in.Where); refusal != nil {
		return reply(*refusal, none), nil
	}
	intent := strings.Join(strings.Fields(in.Intent), " ")
	if intent == "" {
		return reply(refused("start needs your task in one sentence.", nil,
			toolOption("state your task in one sentence", "start")), none), nil
	}
	if e.readOnly() {
		return reply(e.readOnlyRefusal("start"), none), nil
	}
	paths, bad := cleanPaths(e.repo.Root, in.Paths)
	if len(bad) > 0 {
		return reply(refused(fmt.Sprintf("These paths are outside the worktree: %s.", strings.Join(bad, ", ")), nil,
			toolOption("start without them", "start", "intent", intent)), none), nil
	}
	st := e.myState()
	var adopt *model.WorkItem
	if i := activeIndex(st.Items); i >= 0 {
		if !st.Items[i].Provisional {
			return reply(e.activeRefusal(st.Items[i]), none), nil
		}
		it := st.Items[i]
		adopt = &it
	}
	if op, err := e.repo.Operation(ctx); err == nil && op != "" {
		return reply(refused(fmt.Sprintf("A %s is in progress in this worktree.", op), nil,
			cmdOption("finish or abort it first", "git status")), none), nil
	}
	cur, err := e.repo.CurrentBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	id := model.NewItemID()
	if adopt != nil {
		id = adopt.ID
	}
	branch, newBranch := cur, false
	var details []string
	left, leaving := e.finishedBranchItem(ctx, cur)
	if cur == "" || cur == e.cfg.Main || leaving {
		name := strings.TrimSpace(in.Branch)
		if name == "" {
			name = defaultBranchName(e.cfg.BranchPrefix, intent, id)
		}
		from, fromName := "", ""
		if leaving {
			// The branch is a finished task's: HEAD carries its commits, and
			// pushing it would feed its pull request. The new task starts
			// from main.
			from, fromName = e.mainOID(ctx), e.mainUpstream()
			if from == "" {
				from = e.cfg.Main
			} else {
				fromName += " " + shortOID(from)
			}
			// The refusal has the agent create the branch and call start
			// again. With no item to adopt, a fresh id would name another
			// branch on every call, so it names one from the task left and
			// the intent.
			offer := name
			if strings.TrimSpace(in.Branch) == "" && adopt == nil {
				offer = defaultBranchName(e.cfg.BranchPrefix, intent, model.ItemID(model.ContentID(string(left.Item.ID), intent)))
			}
			if refusal, err := e.leftBehindRefusal(ctx, left, cur, offer, from, intent); err != nil || refusal != nil {
				if refusal != nil {
					return reply(*refusal, none), nil
				}
				return nil, err
			}
			if refusal, err := e.carryRefusal(ctx, left.Item, from, intent, in.Branch); err != nil || refusal != nil {
				if refusal != nil {
					return reply(*refusal, none), nil
				}
				return nil, err
			}
		}
		if refusal, err := e.createWorkBranch(ctx, name, from, intent); err != nil || refusal != nil {
			if refusal != nil {
				return reply(*refusal, none), nil
			}
			return nil, err
		}
		branch, newBranch = name, true
		if leaving {
			details = append(details, fmt.Sprintf("the new branch starts from %s: %s stays with your %s task %q%s",
				fromName, cur, left.Item.Status, left.Item.Intent, prSuffix(left.Item.PR)))
		}
	} else if b := strings.TrimSpace(in.Branch); b != "" && b != cur {
		details = append(details, fmt.Sprintf("stayed on branch %s: a branch is only created on %s or a detached HEAD", cur, e.cfg.Main))
	}

	if err := e.snapshot(ctx, snapMode{force: true}); err != nil {
		e.verbWarnings = append(e.verbWarnings, "snapshot failed: "+err.Error())
	}
	base, fp := e.snapshotFootprint(ctx)
	now := e.now().UTC()
	var item model.WorkItem
	_, err = e.mutateState(ctx, func(st *model.AgentState) error {
		i := activeIndex(st.Items)
		if i >= 0 && !st.Items[i].Provisional {
			item = st.Items[i]
			return errActiveExists // another process started meanwhile
		}
		if i < 0 {
			st.Items = append(st.Items, model.WorkItem{ID: id, Status: model.ItemActive, OpenedAt: now, Worktree: e.repo.Root})
			i = len(st.Items) - 1
		}
		it := &st.Items[i]
		it.Intent, it.Provisional, it.Branch, it.UpdatedAt = intent, false, branch, now
		if e.snap.Commit != "" {
			it.Snapshot, it.Head = e.snap.Commit, e.snap.Head
		}
		if base != "" {
			it.Base, it.Footprint = base, fp
		}
		item = *it
		return nil
	})
	if errors.Is(err, errActiveExists) {
		return reply(e.activeRefusal(item), none), nil
	}
	if rr, ok := e.writeRefusal("start", err); ok {
		return reply(rr, none), nil
	}
	if err != nil {
		return nil, err
	}
	if snapErr, changed := e.pushWip(ctx, item.ID); changed && snapErr != item.SnapshotErr {
		_, _ = e.mutateState(ctx, func(st *model.AgentState) error {
			if i := itemIndex(st.Items, item.ID); i >= 0 {
				st.Items[i].SnapshotErr = snapErr
				return nil
			}
			return errNoChange
		})
	}
	e.appendEvent(ctx, "start", map[string]any{"item": item.ID, "intent": intent, "branch": branch, "adopted": adopt != nil})

	data := api.StartData{Item: item, Branch: branch, NewBranch: newBranch, Adopted: adopt != nil}
	data.Similar = e.similarItems(ctx, intent, item.ID)
	data.Touching = e.touching(ctx, touchedPaths(paths, item.Footprint), item.ID)
	data.AlreadyDone = e.alreadyDone(ctx, intent)

	summary := fmt.Sprintf("Started %q on branch %s.", intent, branch)
	switch {
	case adopt != nil:
		summary = fmt.Sprintf("Started %q on branch %s, adopting the work already in progress.", intent, branch)
	case newBranch:
		summary = fmt.Sprintf("Started %q on new branch %s.", intent, branch)
	}
	var lines []string
	for _, s := range data.Similar {
		line := fmt.Sprintf("similar work: %s is doing %q", s.Agent, s.Intent)
		if len(s.Files) > 0 {
			line += " (" + joinCapped(s.Files, maxDetailNames) + ")"
		}
		lines = append(lines, line)
	}
	for _, w := range data.Touching {
		lines = append(lines, whoLine(w))
	}
	for _, c := range data.AlreadyDone {
		lines = append(lines, fmt.Sprintf("already done? %s %q is on %s", shortOID(c.OID), c.Subject, e.cfg.Main))
	}
	details = append(details, capLines(lines, render.MaxRows)...)
	return reply(okResult(summary, details...), data), nil
}

// activeRefusal refuses start while a stated item is active.
func (e *Engine) activeRefusal(it model.WorkItem) api.Result {
	return refused(fmt.Sprintf("You already have an active task: %q on branch %s.", it.Intent, it.Branch),
		[]string{"one active task per worktree: submit it or abandon it, then start again"},
		toolOption("your work is ready: check it, push it and open a pull request", "submit"),
		fillIn(toolOption("stop that task (its branch is kept)", "abandon"), "reason"))
}

// finishedBranchItem returns my item on branch that is no longer active
// (submitted, landed or abandoned), preferring a submitted one. A new task
// never stays on such a branch. A closed item keeps its branch for as long
// as the branch holds its work (holdsClosed), judged by the commit the
// ledger recorded when it closed (rememberFinished), so a branch made again
// under the same name is ordinary, and items closed so long ago that my
// state no longer has them still count. Head is that commit, or "" for a
// submitted item. A provisional item does not count: gitlet opened it on
// its own for the branch the agent was on.
func (e *Engine) finishedBranchItem(ctx context.Context, branch string) (finishedBranch, bool) {
	if branch == "" || branch == e.cfg.Main {
		return finishedBranch{}, false
	}
	var closed *model.WorkItem
	for _, it := range e.myState().Items {
		if it.Branch != branch || it.Provisional {
			continue
		}
		if it.Status == model.ItemSubmitted {
			return finishedBranch{Item: it}, true
		}
		if !it.Status.Open() && closed == nil {
			closed = &it
		}
	}
	if f, ok := e.finishedRecord(ctx, branch); ok {
		return f, e.holdsClosed(ctx, branch, f.Head)
	}
	if closed != nil {
		return finishedBranch{Item: *closed}, true // unrecorded: by name alone
	}
	return finishedBranch{}, false
}

// holdsClosed reports whether branch still holds the work of an item that
// closed with the branch at head: the branch is at head or, while main does
// not contain head, builds on it, holds one of the item's commits main
// lacks as it is or by patch (holdsCommitOf), or was rewritten in place
// since it was at head (rewrittenSince), so an amend or a rebase does not
// turn it into another branch. A branch made again for other work, deleted
// and created anew or reset onto main, does none of these. Once main
// contains head (the item landed by a merge or a fast-forward), a branch
// made again from main contains it too, and commits on top of it no longer
// feed anything main lacks, so only head itself counts.
func (e *Engine) holdsClosed(ctx context.Context, branch, head string) bool {
	tip, err := e.bg.RevParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return false
	}
	if tip == head {
		return true
	}
	main := e.mainOID(ctx)
	if main != "" {
		if inMain, err := e.bg.IsAncestor(ctx, head, main); err == nil && inMain {
			return false
		}
	}
	builds, err := e.bg.IsAncestor(ctx, head, tip)
	if err != nil {
		return false
	}
	return builds || e.holdsCommitOf(ctx, tip, head, main) || e.rewrittenSince(ctx, branch, head, main)
}

// holdsCommitOf reports whether tip holds one of the commits head has and
// main lacks, as it is or by patch (cherry-picked, or rebased onto another
// base): fewer of those commits remain once the ones tip holds are left
// out.
func (e *Engine) holdsCommitOf(ctx context.Context, tip, head, main string) bool {
	count := func(args ...string) int {
		args = append([]string{"rev-list", "--count", "--no-merges"}, args...)
		if main != "" {
			args = append(args, "^"+main)
		}
		out, err := e.bg.Run(ctx, append(args, "--")...)
		if err != nil {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return -1
		}
		return n
	}
	lacking := count("--cherry-pick", "--right-only", tip+"..."+head)
	return lacking >= 0 && lacking < count(head)
}

// rewrittenSince reports whether branch was rewritten in place (amended,
// rebased, reset) since it was at head rather than made again: its reflog,
// which goes with the branch when it is deleted, has head, and no later
// entry is a commit main contains, which resetting the branch onto main for
// other work (git switch -C) would leave.
func (e *Engine) rewrittenSince(ctx context.Context, branch, head, main string) bool {
	out, err := e.bg.Run(ctx, "log", "-g", "--no-show-signature", "--format=%H", "-n", strconv.Itoa(reflogScan), "refs/heads/"+branch, "--")
	if err != nil {
		return false
	}
	entries := strings.Fields(out) // newest first
	i := slices.Index(entries, head)
	if i < 0 {
		return false
	}
	if main == "" {
		return true
	}
	// rev-list lists each later entry, unless main contains it.
	out, err = e.bg.Run(ctx, append(append([]string{"rev-list"}, entries[:i]...), "^"+main, "--")...)
	if err != nil {
		return false
	}
	listed := strings.Fields(out)
	for _, oid := range entries[:i] {
		if !slices.Contains(listed, oid) {
			return false
		}
	}
	return true
}

// finishedBranch is what the ledger remembers about a closed item's branch.
type finishedBranch struct {
	Item model.WorkItem `json:"item"` // id, intent, branch, status and PR only
	Head string         `json:"head"` // the branch's commit when the item closed
}

// finishedRecord returns what the ledger remembers about branch: the last
// item that closed on it.
func (e *Engine) finishedRecord(ctx context.Context, branch string) (finishedBranch, bool) {
	v, have, err := e.led.Get(ctx, e.finishedKey(branch))
	if err != nil || !have {
		return finishedBranch{}, false
	}
	var f finishedBranch
	if json.Unmarshal([]byte(v), &f) != nil || f.Head == "" {
		return finishedBranch{}, false
	}
	return f, true
}

func (e *Engine) finishedKey(branch string) string {
	return "finished-branch/" + string(e.me.ID) + "/" + branch
}

// rememberFinished records the branch of an item that landed or was
// abandoned, so start still leaves it after the item is trimmed from my
// state. A provisional item's branch was never its own and is left out.
// Best effort: without it start only knows recent items.
func (e *Engine) rememberFinished(ctx context.Context, id model.ItemID) {
	items := e.myState().Items
	i := itemIndex(items, id)
	if i < 0 || items[i].Provisional || items[i].Branch == "" || items[i].Branch == e.cfg.Main || e.led.ReadOnly() {
		return
	}
	it := items[i]
	head, err := e.repo.RevParse(ctx, "refs/heads/"+it.Branch)
	if err != nil {
		return
	}
	b, err := json.Marshal(finishedBranch{
		Item: model.WorkItem{ID: it.ID, Intent: it.Intent, Branch: it.Branch, Status: it.Status, PR: it.PR},
		Head: head,
	})
	if err == nil {
		err = e.led.Set(ctx, e.finishedKey(it.Branch), string(b))
	}
	if err != nil {
		e.log.Debug("remember finished branch", "item", id, "err", err)
	}
}

// carryRefusal refuses start when uncommitted changes cannot come along to
// a new branch at from, because from has other versions of those files.
func (e *Engine) carryRefusal(ctx context.Context, left model.WorkItem, from, intent, branch string) (*api.Result, error) {
	differ, err := e.repo.DiffNames(ctx, "HEAD", from)
	if err != nil {
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	blocked, err := e.dirtyPaths(ctx, differ)
	if err != nil || len(blocked) == 0 {
		return nil, err
	}
	r := refused(fmt.Sprintf("Cannot start a new branch from %s: it would overwrite your uncommitted changes to %s.", e.mainUpstream(), joinCapped(blocked, maxDetailNames)),
		[]string{fmt.Sprintf("branch %s belongs to your %s task %q, so a new task starts from %s, where these files differ", left.Branch, left.Status, left.Intent, e.mainUpstream())},
		cmdOption("set your changes aside", "git stash push --include-untracked"),
		toolOption("start again", "start", "intent", intent, "branch", strings.TrimSpace(branch)),
		cmdOption("then bring your changes back onto the new branch", "git stash pop"))
	return &r, nil
}

// leftBehindRefusal refuses start when a closed item's branch has commits
// made after the item closed: they are new work, not the item's, and a new
// branch from main would leave them behind on the old one, while start
// rewrites no commits. Commits main has, merged in to keep the branch up to
// date, merges, and the item's own commits rewritten by an amend or a
// rebase (the same patch) are no such work. It offers to move the commits,
// named one by one, onto the new branch name from main, or to keep them on
// top of the item's commits on that name, setting uncommitted changes
// aside around that when the move would overwrite them. A submitted item's
// later commits are its review fixes and stay with it.
func (e *Engine) leftBehindRefusal(ctx context.Context, left finishedBranch, cur, name, from, intent string) (*api.Result, error) {
	if left.Head == "" {
		return nil, nil
	}
	// %h is git's unique abbreviation: the command stays short enough to
	// show in full however many commits it names.
	out, err := e.bg.Run(ctx, "log", "--no-show-signature", "--no-merges", "--cherry-pick", "--left-only", "--topo-order", "--reverse",
		"--format=%H %h %s", "HEAD..."+left.Head, "^"+from, "--")
	if err != nil {
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	var picks, names []string
	for _, l := range strings.Split(out, "\n") {
		oid, rest, _ := strings.Cut(l, " ")
		abbrev, subject, _ := strings.Cut(rest, " ")
		if abbrev == "" {
			continue
		}
		picks = append(picks, abbrev)
		names = append(names, fmt.Sprintf("%s %q", shortOID(oid), subject))
	}
	if len(picks) == 0 {
		return nil, nil
	}
	// git switch refuses to overwrite uncommitted changes to a file main
	// has otherwise, and cherry-pick refuses to start over staged ones.
	differ, err := e.repo.DiffNames(ctx, "HEAD", from)
	if err != nil {
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	blocked, err := e.dirtyPaths(ctx, differ)
	if err != nil {
		return nil, err
	}
	staged, err := e.stagedPaths(ctx)
	if err != nil {
		return nil, err
	}
	aside := append(blocked, staged...)
	sort.Strings(aside)
	aside = compactStrings(aside)

	it := left.Item
	details := []string{fmt.Sprintf("branch %s belongs to your %s task%s, so a new task starts from %s, without %s",
		cur, it.Status, prSuffix(it.PR), e.mainUpstream(), joinCapped(names, maxDetailNames))}
	move := cmdOption("move them onto a new branch from "+e.mainUpstream(),
		"git switch -c "+shellQuote(name)+" "+shellQuote(from)+" && git cherry-pick "+strings.Join(picks, " "))
	keep := cmdOption("or keep them on top of that task's commits, on a new branch", "git switch -c "+shellQuote(name))
	again := toolOption("then start again", "start", "intent", intent)
	opts := []model.Option{move, keep, again}
	if len(aside) > 0 {
		details = append(details, fmt.Sprintf("your uncommitted changes to %s would stop the move: set them aside around it", joinCapped(aside, maxDetailNames)))
		move.Label = "then " + move.Label
		opts = []model.Option{cmdOption("set your uncommitted changes aside", "git stash push --include-untracked"),
			move, keep, cmdOption("then bring your changes back", "git stash pop"), again}
	}
	r := refused(fmt.Sprintf("Cannot start a new branch from %s: it would leave behind %s made on %s since your %s task %q ended.",
		e.mainUpstream(), pluralize(len(picks), "commit"), cur, it.Status, it.Intent), details, opts...)
	return &r, nil
}

// prSuffix is " (<pr>)", or "" without a pull request.
func prSuffix(pr string) string {
	if pr == "" {
		return ""
	}
	return " (PR " + pr + ")"
}

// createWorkBranch creates and switches to branch at from ("" means HEAD),
// carrying uncommitted changes along. Problems the agent can fix are
// refusals.
func (e *Engine) createWorkBranch(ctx context.Context, name, from, intent string) (*api.Result, error) {
	if _, err := e.repo.Run(ctx, "check-ref-format", "--branch", name); err != nil {
		r := refused(fmt.Sprintf("%q is not a valid branch name.", name), nil,
			toolOption("let gitlet name the branch", "start", "intent", intent))
		return &r, nil
	}
	exists, err := e.repo.BranchExists(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	if exists {
		opts := []model.Option{toolOption("use another branch name", "start", "intent", intent, "branch", name+"-2")}
		if _, finished := e.finishedBranchItem(ctx, name); !finished {
			opts = append(opts, cmdOption("or switch to it and start there", "git switch "+shellQuote(name)))
		}
		r := refused(fmt.Sprintf("Branch %s already exists.", name), nil, opts...)
		return &r, nil
	}
	if err := e.repo.CreateBranch(ctx, name, from); err != nil {
		var gerr *gitx.Error
		if errors.As(err, &gerr) {
			r := refused(fmt.Sprintf("Could not create branch %s: %s", name, firstLine(gerr.Stderr)), nil,
				cmdOption("see what is in the way", "git status"),
				toolOption("start again once it is fixed", "start", "intent", intent, "branch", name))
			return &r, nil
		}
		return nil, fmt.Errorf("engine: start: %w", err)
	}
	return nil, nil
}

// defaultBranchName is <prefix><slug>-<4 hex of the item>.
func defaultBranchName(prefix, intent string, id model.ItemID) string {
	short := string(id)
	if len(short) > 4 {
		short = short[:4]
	}
	return prefix + model.Slug(intent, 40) + "-" + short
}

// snapshotFootprint returns the base and footprint of the latest snapshot
// against main ("" when either is unknown).
func (e *Engine) snapshotFootprint(ctx context.Context) (string, model.Footprint) {
	main := e.mainOID(ctx)
	if main == "" || e.snap.Commit == "" {
		return "", model.Footprint{}
	}
	base, err := e.bg.MergeBase(ctx, main, e.snap.Commit)
	if err != nil {
		return "", model.Footprint{}
	}
	fp, err := e.footprint(ctx, base, e.snap.Commit)
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "footprint not computed: "+err.Error())
		return base, model.Footprint{}
	}
	return base, fp
}

// similarItems lists open items in view (provisional ones excepted, as
// their intent is only a branch name) whose intent looks like the same
// task: rules.Similarity of at least 0.5 with at least two shared terms
// (docs/SPEC.md, section 10.2). Best first.
func (e *Engine) similarItems(ctx context.Context, intent string, self model.ItemID) []api.Similar {
	type cand struct {
		agent model.AgentID
		it    model.WorkItem
	}
	var cands []cand
	corpus := []string{intent}
	add := func(agent model.AgentID, it model.WorkItem) {
		if it.ID != self && it.Status.Open() && !it.Provisional {
			cands = append(cands, cand{agent, it})
			corpus = append(corpus, it.Intent)
		}
	}
	peers := e.peers(ctx, e.now())
	for _, id := range sortedKeys(peers) {
		for _, it := range peers[id].State.Items {
			add(id, it)
		}
	}
	for _, it := range e.myState().Items {
		add(e.me.ID, it)
	}
	var out []api.Similar
	for _, c := range cands {
		score, shared := rules.Similarity(intent, c.it.Intent, corpus)
		if score < similarScore || shared < similarTerms {
			continue
		}
		var files []string
		for _, f := range c.it.Footprint.Files {
			files = append(files, f.Path)
		}
		out = append(out, api.Similar{Agent: c.agent, Item: c.it.ID, Intent: c.it.Intent, Branch: c.it.Branch, Score: score, Files: files})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// touchedPaths is what start checks for overlap: the paths it was given,
// then the files its snapshot already changes (old paths of renames
// included), each once. An agent that edited before calling start sees the
// overlaps of those edits too.
func touchedPaths(paths []string, fp model.Footprint) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		add(p)
	}
	var changed []string
	for _, f := range fp.Files {
		changed = append(changed, f.Path, f.OldPath)
	}
	sort.Strings(changed)
	for _, p := range changed {
		add(p)
	}
	return out
}

// touching lists the other agents' items and claims that change or own
// any of paths (start's touchedPaths), each entry once, in who's order.
func (e *Engine) touching(ctx context.Context, paths []string, self model.ItemID) []api.WhoEntry {
	if len(paths) == 0 {
		return nil
	}
	isDir := e.dirChecker(ctx)
	v := e.lightView(ctx)
	seen := map[api.WhoEntry]bool{}
	var out []api.WhoEntry
	for _, p := range paths {
		t := target{Kind: targetPath, Path: p, Raw: p}
		if isDir(p) {
			t.Kind = targetDir
		}
		for _, w := range e.whoEntries(ctx, t, v, self) {
			if (w.Role == "changes" || w.Role == "owns") && w.Agent != e.me.ID && !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	sortWhoEntries(out)
	return out
}

// alreadyDone lists commits on main from the last 7 days whose subject
// looks like the intent ("already done?").
func (e *Engine) alreadyDone(ctx context.Context, intent string) []api.CommitRef {
	main := e.mainOID(ctx)
	if main == "" {
		return nil
	}
	commits, err := e.bg.Log(ctx, main, e.now().Add(-alreadyDoneWindow), alreadyDoneScan)
	if err != nil {
		e.log.Debug("log main", "err", err)
		return nil
	}
	corpus := []string{intent}
	for _, c := range commits {
		corpus = append(corpus, c.Subject)
	}
	type scored struct {
		ref   api.CommitRef
		score float64
	}
	var hits []scored
	for _, c := range commits {
		if score, shared := rules.Similarity(intent, c.Subject, corpus); score >= similarScore && shared >= similarTerms {
			hits = append(hits, scored{api.CommitRef{OID: c.OID, Subject: c.Subject}, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []api.CommitRef
	for i, h := range hits {
		if i == alreadyDoneMax {
			break
		}
		out = append(out, h.ref)
	}
	return out
}

// ---- status ----

func (e *Engine) status(ctx context.Context, in api.StatusInput) (*api.Reply[api.StatusData], error) {
	release, err := e.beginVerb(ctx, true)
	if err != nil {
		return nil, err
	}
	defer release()
	if refusal := e.worktreeRefusal("status", in.Where); refusal != nil {
		r := reply(*refusal, api.StatusData{})
		e.finish(ctx, &r.Result, "")
		return r, nil
	}
	v, _, err := e.evaluate(ctx, verbEvalBudget)
	if err != nil {
		e.verbWarnings = append(e.verbWarnings, "notes not updated: "+err.Error())
	}
	data := e.statusData(ctx, v)
	r := reply(okResult(e.statusSummary(data), statusLines(data, e.me.ID)...), data)
	for _, iv := range data.Items {
		if iv.PRClosed {
			r.Options = append(r.Options, fillIn(toolOption(
				fmt.Sprintf("stop task %s: its pull request was closed without merging", iv.Item.ID),
				"abandon", "item", string(iv.Item.ID)), "reason"))
		}
	}
	e.decorate(ctx, &r.Result, true)
	return r, nil
}

// statusData gathers everything api.StatusData holds from a view.
func (e *Engine) statusData(ctx context.Context, v rules.View) api.StatusData {
	data := api.StatusData{
		Agent:    e.me,
		Worktree: e.repo.Root,
		Remote:   e.bus.Remote(),
		Main:     e.cfg.Main,
		ReadOnly: e.readOnly(),
	}
	items := append([]model.WorkItem(nil), v.Mine.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := statusRank(items[i].Status), statusRank(items[j].Status); a != b {
			return a < b
		}
		return items[i].OpenedAt.Before(items[j].OpenedAt)
	})
	dirty := false
	if clean, err := e.bg.IsClean(ctx); err == nil {
		dirty = !clean
	}
	for _, it := range items {
		data.Items = append(data.Items, api.ItemView{
			Item:        it,
			Overlaps:    overlaps(it, v),
			Uncommitted: it.Status == model.ItemActive && dirty,
			PRClosed:    e.prClosed(ctx, it),
		})
	}
	data.Claims = e.relevantClaims(ctx, v)
	for _, n := range v.MyOutbox.Needs {
		ni := api.NeedInfo{Need: n}
		if cv, ok := v.Claims[n.Key]; ok {
			ni.Owner, ni.Contract = cv.Claim.Owner, cv.Claim.Contract
		}
		data.Needs = append(data.Needs, ni)
	}
	sort.SliceStable(data.Needs, func(i, j int) bool { return data.Needs[i].Need.Name < data.Needs[j].Need.Name })
	data.Threads = rules.Threads(v)
	sort.Slice(data.Threads, func(i, j int) bool { return data.Threads[i].ID < data.Threads[j].ID })
	for _, id := range sortedKeys(v.Peers) {
		p := v.Peers[id]
		if len(p.State.Items) == 0 {
			continue
		}
		agent := p.State.Agent
		if agent.ID == "" {
			agent.ID = id
		}
		info := api.PeerInfo{Agent: agent, Lapsed: p.Lapsed, IdleSince: p.IdleSince}
		for _, it := range p.State.Items {
			info.Items = append(info.Items, it.Intent)
		}
		data.Others = append(data.Others, info)
	}
	return data
}

func statusRank(s model.ItemStatus) int {
	switch s {
	case model.ItemActive:
		return 0
	case model.ItemSubmitted:
		return 1
	case model.ItemLanded:
		return 2
	}
	return 3
}

// relevantClaims returns the claims status shows: mine, those I need, and
// those on symbols my open items change, sorted by symbol.
func (e *Engine) relevantClaims(ctx context.Context, v rules.View) []api.ClaimInfo {
	want := map[string]bool{}
	for _, n := range v.MyOutbox.Needs {
		want[n.Key] = true
	}
	for _, it := range v.Mine.Items {
		if it.Status.Open() {
			for _, sc := range it.Footprint.Symbols {
				want[sc.Symbol.Key] = true
			}
		}
	}
	var out []api.ClaimInfo
	for key, cv := range e.shared.Claims {
		if cv.Claim.Owner == e.me.ID || want[key] {
			out = append(out, e.claimInfo(ctx, cv, v.Now))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// overlaps lists the other items in view that change symbols or files my
// item changes.
func overlaps(it model.WorkItem, v rules.View) []api.Overlap {
	if !it.Status.Open() {
		return nil
	}
	mineSyms := map[string]string{}
	for _, sc := range it.Footprint.Symbols {
		mineSyms[sc.Symbol.Key] = sc.Symbol.Ref()
	}
	mineFiles := changedFiles(it.Footprint)
	var out []api.Overlap
	check := func(agent model.AgentID, o model.WorkItem) {
		if o.ID == it.ID || !o.Status.Open() {
			return
		}
		var syms []string
		for _, sc := range o.Footprint.Symbols {
			if name, ok := mineSyms[sc.Symbol.Key]; ok {
				syms = append(syms, name)
			}
		}
		files := sharedFiles(mineFiles, changedFiles(o.Footprint))
		if len(syms) == 0 && len(files) == 0 {
			return
		}
		sort.Strings(syms)
		out = append(out, api.Overlap{Agent: agent, Item: o.ID, Intent: o.Intent, Symbols: compactStrings(syms), Files: files})
	}
	for _, id := range sortedKeys(v.Peers) {
		for _, o := range v.Peers[id].State.Items {
			check(id, o)
		}
	}
	for _, o := range v.Mine.Items {
		check(v.Me, o)
	}
	return out
}

func (e *Engine) statusSummary(d api.StatusData) string {
	others := ""
	switch n := len(d.Others); n {
	case 0:
	case 1:
		others = " 1 other agent is working here."
	default:
		others = fmt.Sprintf(" %d other agents are working here.", n)
	}
	for _, iv := range d.Items {
		if iv.Item.Status != model.ItemActive {
			continue
		}
		if iv.Item.Provisional {
			return fmt.Sprintf("Untitled work on %s: call start with your task.%s", branchName(iv.Item.Branch), others)
		}
		return fmt.Sprintf("Working on %q on %s.%s", iv.Item.Intent, branchName(iv.Item.Branch), others)
	}
	return "No active task: call start before you edit code." + others
}

func branchName(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return "branch " + b
}

// statusLines are the facts of a status result as text, each list capped.
func statusLines(d api.StatusData, me model.AgentID) []string {
	var out []string
	var items []string
	for _, iv := range d.Items {
		it := iv.Item
		line := fmt.Sprintf("task %s: %q on %s, %s", it.ID, it.Intent, branchName(it.Branch), it.Status)
		if it.Provisional {
			line += " (untitled: call start)"
		}
		if iv.Uncommitted {
			line += ", uncommitted changes"
		}
		if it.PR != "" {
			line += ", PR " + it.PR
		}
		if iv.PRClosed {
			line += " (closed without merging)"
		}
		items = append(items, line)
		for _, o := range iv.Overlaps {
			what := joinCapped(o.Symbols, maxDetailNames)
			if what == "" {
				what = joinCapped(o.Files, maxDetailNames)
			}
			items = append(items, fmt.Sprintf("  overlaps %s %q: %s", o.Agent, o.Intent, what))
		}
	}
	out = append(out, capLines(items, render.MaxRows)...)
	var claims []string
	for _, c := range d.Claims {
		owner := string(c.Owner)
		if c.Owner == me {
			owner = "you"
		}
		line := fmt.Sprintf("claim %s: owned by %s (epoch %d)", c.Symbol, owner, c.Epoch)
		if c.Lapsed {
			line += ", lapsed"
		}
		if c.Contract != nil {
			line += ", published: " + c.Contract.Signature
		}
		if len(c.Waiting) > 0 {
			line += ", needed by " + joinIDs(c.Waiting)
		}
		claims = append(claims, line)
	}
	out = append(out, capLines(claims, render.MaxRows)...)
	var needs []string
	for _, n := range d.Needs {
		line := "need " + n.Need.Name
		if n.Owner != "" {
			line += " (owner " + string(n.Owner) + ")"
		}
		switch {
		case n.Need.PinnedSigHash != "":
			line += ", in use"
		case n.Contract != nil:
			line += ", published: " + n.Contract.Signature
		default:
			line += ", not published yet"
		}
		needs = append(needs, line)
	}
	out = append(out, capLines(needs, render.MaxRows)...)
	var threads []string
	for _, t := range d.Threads {
		line := fmt.Sprintf("thread %s about %s: %d messages", t.ID, firstNonEmpty(t.SubjectName, t.Subject), len(t.Messages))
		if t.Settled {
			line += ", settled"
			if t.Owner != "" {
				line += ": owner " + string(t.Owner)
			}
		}
		threads = append(threads, line)
	}
	out = append(out, capLines(threads, render.MaxRows)...)
	var others []string
	for _, p := range d.Others {
		line := fmt.Sprintf("agent %s: %s", p.Agent.ID, quoteAll(p.Items))
		if p.Lapsed && p.IdleSince != nil {
			line += ", idle since " + p.IdleSince.Local().Format("Jan 2 15:04")
		}
		others = append(others, line)
	}
	out = append(out, capLines(others, render.MaxRows)...)
	return out
}

// ---- who ----

func (e *Engine) who(ctx context.Context, in api.WhoInput) (*api.Reply[api.WhoData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	refusal := e.worktreeRefusal("who", in.Where)
	var t target
	if refusal == nil {
		if t, refusal, err = e.resolveTarget(ctx, "who", in.Target, false); err != nil {
			return nil, err
		}
	}
	if refusal != nil {
		r := reply(*refusal, api.WhoData{Target: strings.TrimSpace(in.Target)})
		e.finish(ctx, &r.Result, "")
		return r, nil
	}
	entries := e.whoEntries(ctx, t, e.lightView(ctx), "")
	data := api.WhoData{Target: t.Name(), Entries: entries}
	var summary string
	switch agents := distinctAgents(entries); len(agents) {
	case 0:
		summary = fmt.Sprintf("Nobody else is changing, using, owning or needing %s.", t.Name())
	case 1:
		summary = fmt.Sprintf("1 agent touches %s.", t.Name())
	default:
		summary = fmt.Sprintf("%d agents touch %s.", len(agents), t.Name())
	}
	var lines []string
	for _, w := range entries {
		lines = append(lines, whoLine(w))
	}
	r := reply(okResult(summary, capLines(lines, render.MaxRows)...), data)
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// whoRoles orders who entries.
var whoRoles = map[string]int{"owns": 0, "changes": 1, "needs": 2, "references": 3, "branch": 4}

// whoEntries lists who touches a target (docs/SPEC.md, section 4): other
// items that change it or whose new code references it, the claim holder
// (me included), every agent that needs it, and outside branches that
// change it. skip leaves one item out. Sorted by role, agent and item.
func (e *Engine) whoEntries(ctx context.Context, t target, v rules.View, skip model.ItemID) []api.WhoEntry {
	var out []api.WhoEntry
	defs := e.targetDefinitions(ctx, t, v)
	counts := changedNames(v)
	for _, id := range sortedKeys(v.Peers) {
		p := v.Peers[id]
		for _, it := range p.State.Items {
			if it.ID == skip {
				continue
			}
			base := api.WhoEntry{Agent: id, Item: it.ID, Intent: it.Intent, Lapsed: p.Lapsed, PR: it.PR}
			if detail, ok := changesTarget(it.Footprint, t); ok {
				w := base
				w.Role, w.Detail = "changes", detail
				out = append(out, w)
			}
			if detail, ok := referencesTarget(it.Footprint, defs, counts); ok {
				w := base
				w.Role, w.Detail = "references", detail
				out = append(out, w)
			}
		}
	}
	for key, cv := range v.Claims {
		c := cv.Claim
		if !claimOnTarget(c, t) {
			continue
		}
		detail := c.Symbol.Ref() + ", no contract published yet"
		if c.Contract != nil {
			detail = c.Symbol.Ref() + ", published: " + c.Contract.Signature
		}
		w := api.WhoEntry{Agent: c.Owner, Item: c.Item, Role: "owns", Detail: detail, Lapsed: v.Claims[key].Lapsed}
		if _, it, ok := e.findItem(c.Item); ok {
			w.Intent, w.PR = it.Intent, it.PR
		}
		out = append(out, w)
	}
	for id, ob := range e.shared.Outboxes {
		for _, n := range ob.Needs {
			if !needOnTarget(n, t) {
				continue
			}
			detail := n.Name
			if n.Requirement != "" {
				detail += ": " + quoteLine(id, n.Requirement)
			}
			w := api.WhoEntry{Agent: id, Item: n.Item, Role: "needs", Detail: detail}
			if p, ok := v.Peers[id]; ok {
				w.Lapsed = p.Lapsed
			}
			if _, it, ok := e.findItem(n.Item); ok {
				w.Intent = it.Intent
			}
			out = append(out, w)
		}
	}
	for _, o := range e.outside {
		if detail, ok := changesTarget(o.Footprint, t); ok {
			out = append(out, api.WhoEntry{Role: "branch", Detail: "branch " + o.Branch + " (not a gitlet task): " + detail})
		}
	}
	sortWhoEntries(out)
	return out
}

// sortWhoEntries orders entries by role (owns, changes, needs, references,
// branch), then agent, item and detail.
func sortWhoEntries(out []api.WhoEntry) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if whoRoles[a.Role] != whoRoles[b.Role] {
			return whoRoles[a.Role] < whoRoles[b.Role]
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Item != b.Item {
			return a.Item < b.Item
		}
		return a.Detail < b.Detail
	})
}

// targetDefinitions returns the symbols references to a target are matched
// against: the symbol itself, or every symbol a file defines at HEAD or in
// a footprint in view. Directories match none (too broad).
func (e *Engine) targetDefinitions(ctx context.Context, t target, v rules.View) []model.Symbol {
	switch t.Kind {
	case targetSymbol:
		return []model.Symbol{t.Symbol}
	case targetPath, targetLine:
	default:
		return nil
	}
	var defs []model.Symbol
	if f, err := e.fileSymbols(ctx, "HEAD", t.Path); err == nil && f != nil {
		defs = append(defs, f.Symbols...)
	}
	add := func(items []model.WorkItem) {
		for _, it := range items {
			for _, sc := range it.Footprint.Symbols {
				if sc.Symbol.Path == t.Path {
					defs = append(defs, sc.Symbol)
				}
			}
		}
	}
	add(v.Mine.Items)
	for _, id := range sortedKeys(v.Peers) {
		add(v.Peers[id].State.Items)
	}
	return uniqueSymbols(defs)
}

// changedNames counts the distinct changed symbols per name in view, for
// matching unresolved method references (docs/SPEC.md, section 9.6).
func changedNames(v rules.View) map[string]int {
	keys := map[string]map[string]bool{}
	add := func(items []model.WorkItem) {
		for _, it := range items {
			for _, sc := range it.Footprint.Symbols {
				if keys[sc.Symbol.Name] == nil {
					keys[sc.Symbol.Name] = map[string]bool{}
				}
				keys[sc.Symbol.Name][sc.Symbol.Key] = true
			}
		}
	}
	add(v.Mine.Items)
	for _, p := range v.Peers {
		add(p.State.Items)
	}
	out := make(map[string]int, len(keys))
	for name, ks := range keys {
		out[name] = len(ks)
	}
	return out
}

// changesTarget reports whether a footprint changes the target, with a
// short description of how.
func changesTarget(fp model.Footprint, t target) (string, bool) {
	switch t.Kind {
	case targetSymbol:
		for _, sc := range fp.Symbols {
			s := sc.Symbol
			if s.Key == t.Symbol.Key || (s.Path == t.Symbol.Path && s.Qualified == t.Symbol.Qualified) {
				return describeChange(sc), true
			}
		}
	case targetPath, targetLine:
		for _, f := range fp.Files {
			if f.Path != t.Path && f.OldPath != t.Path {
				continue
			}
			var names []string
			for _, sc := range fp.Symbols {
				if sc.Symbol.Path == t.Path {
					names = append(names, sc.Symbol.Qualified)
				}
			}
			if len(names) > 0 {
				return "changes " + joinCapped(names, maxDetailNames), true
			}
			return fileStatusWord(f.Status) + " the file", true
		}
	case targetDir:
		prefix := strings.TrimSuffix(t.Path, "/") + "/"
		var files []string
		for _, f := range fp.Files {
			if strings.HasPrefix(f.Path, prefix) {
				files = append(files, f.Path)
			}
		}
		if len(files) > 0 {
			return "changes " + joinCapped(files, maxDetailNames), true
		}
	}
	return "", false
}

func describeChange(sc model.SymbolChange) string {
	s := string(sc.Change) + " change"
	switch sc.Change {
	case model.ChangeAdded:
		s = "adds it"
	case model.ChangeRemoved:
		s = "removes it"
	case model.ChangeBody:
		s = "changes its body"
	case model.ChangeRenamed:
		s = "renames it"
	case model.ChangeMoved:
		s = "moves it from " + sc.OldPath
	case model.ChangeSignature:
		s = "changes its signature"
		if sc.Breaking {
			s += " (breaking)"
		} else {
			s += " (compatible)"
		}
		if sc.Symbol.Signature != "" {
			s += " to " + sc.Symbol.Signature
		}
	}
	return s
}

func fileStatusWord(status string) string {
	switch status {
	case "A":
		return "adds"
	case "D":
		return "deletes"
	case "R":
		return "renames"
	}
	return "changes"
}

// referencesTarget reports whether the new code of fp refers to any of
// defs, with up to three of the referring places.
func referencesTarget(fp model.Footprint, defs []model.Symbol, counts map[string]int) (string, bool) {
	if len(defs) == 0 {
		return "", false
	}
	var places []string
	seen := map[string]bool{}
	for _, ref := range fp.Refs {
		for _, d := range defs {
			if ok, _ := rules.Match(ref, fp.Version, d, counts[d.Name] <= 1); !ok {
				continue
			}
			place := fmt.Sprintf("%s %s at %s:%d", refVerb(ref.Kind), d.Qualified, ref.Path, ref.Line)
			if !seen[place] {
				seen[place] = true
				places = append(places, place)
			}
			break
		}
	}
	if len(places) == 0 {
		return "", false
	}
	return joinCapped(places, maxDetailNames), true
}

func refVerb(k model.RefKind) string {
	switch k {
	case model.RefCall:
		return "calls"
	case model.RefType:
		return "uses the type"
	case model.RefConstruct:
		return "constructs"
	case model.RefExtends:
		return "extends"
	}
	return "refers to"
}

func claimOnTarget(c model.Claim, t target) bool {
	switch t.Kind {
	case targetSymbol:
		return c.Key == t.Symbol.Key
	case targetPath, targetLine:
		return c.Symbol.Path == t.Path
	case targetDir:
		return strings.HasPrefix(c.Symbol.Path, strings.TrimSuffix(t.Path, "/")+"/")
	}
	return false
}

func needOnTarget(n model.Need, t target) bool {
	switch t.Kind {
	case targetSymbol:
		return n.Key == t.Symbol.Key
	case targetPath, targetLine:
		return strings.HasPrefix(n.Name, t.Path+":")
	case targetDir:
		return strings.HasPrefix(n.Name, strings.TrimSuffix(t.Path, "/")+"/")
	}
	return false
}

// whoLine renders one who entry as a detail line.
func whoLine(w api.WhoEntry) string {
	var b strings.Builder
	if w.Agent != "" {
		b.WriteString(string(w.Agent) + " ")
	}
	b.WriteString(w.Role)
	if w.Intent != "" {
		fmt.Fprintf(&b, " (%q)", w.Intent)
	}
	if w.Detail != "" {
		b.WriteString(": " + w.Detail)
	}
	if w.Lapsed {
		b.WriteString(" [idle]")
	}
	if w.PR != "" {
		b.WriteString(", PR " + w.PR)
	}
	return b.String()
}

func distinctAgents(entries []api.WhoEntry) map[string]bool {
	out := map[string]bool{}
	for _, w := range entries {
		k := string(w.Agent)
		if k == "" {
			k = "branch:" + w.Detail
		}
		out[k] = true
	}
	return out
}

// ---- why ----

func (e *Engine) why(ctx context.Context, in api.WhyInput) (*api.Reply[api.WhyData], error) {
	release, err := e.beginVerb(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := e.doWhy(ctx, in)
	if err != nil {
		return nil, err
	}
	e.finish(ctx, &r.Result, "")
	return r, nil
}

// doWhy explains a target (docs/SPEC.md, section 12.5): the commit that
// last changed it (blame of the line, or of a symbol's first line at HEAD,
// or the file's last commit), the item and agent named by Gitlet-Item and
// Gitlet-Agent lines anywhere in its message, the item's intent and PR
// from agents' states or the ledger's history, and the claim with its
// contract and requirements.
func (e *Engine) doWhy(ctx context.Context, in api.WhyInput) (*api.Reply[api.WhyData], error) {
	if refusal := e.worktreeRefusal("why", in.Where); refusal != nil {
		return reply(*refusal, api.WhyData{}), nil
	}
	t, refusal, err := e.resolveTarget(ctx, "why", in.Target, true)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return reply(*refusal, api.WhyData{Target: strings.TrimSpace(in.Target)}), nil
	}
	data := api.WhyData{Target: t.Name()}
	var commit string
	var claimSym model.Symbol
	switch t.Kind {
	case targetLine:
		oid, refusal := e.blame(ctx, t.Path, t.Line)
		if refusal != nil {
			return reply(*refusal, data), nil
		}
		commit = oid
		if f, err := e.fileSymbols(ctx, "HEAD", t.Path); err == nil && f != nil {
			claimSym, _ = innermost(f.Symbols, t.Line)
		}
	case targetSymbol:
		claimSym = t.Symbol
		if sym, ok := e.symbolAt(ctx, "HEAD", t.Symbol.Path, t.Symbol.Qualified); ok {
			oid, refusal := e.blame(ctx, sym.Path, sym.StartLine)
			if refusal != nil {
				return reply(*refusal, data), nil
			}
			commit = oid
		}
	case targetPath, targetDir:
		out, err := e.bg.Run(ctx, "log", "-1", "--format=%H", "HEAD", "--", t.Path)
		if err == nil {
			commit = strings.TrimSpace(out)
		}
	}
	if claimSym.Key != "" {
		if cv, ok := e.claimFor(claimSym.Key); ok {
			ci := e.claimInfo(ctx, cv, e.now())
			data.Claim = &ci
		}
	}

	var details []string
	var summary string
	switch {
	case gitx.IsZeroOID(commit):
		summary = fmt.Sprintf("%s is not committed yet.", t.Name())
	case commit == "":
		summary = fmt.Sprintf("%s is not on HEAD yet.", t.Name())
		if t.Kind == targetSymbol {
			for _, w := range e.whoEntries(ctx, t, e.lightView(ctx), "") {
				if w.Role == "changes" {
					details = append(details, whoLine(w))
				}
			}
		}
	default:
		info, err := e.bg.ShowCommit(ctx, commit)
		if err != nil {
			return nil, fmt.Errorf("engine: why: %w", err)
		}
		when := info.When
		data.Commit, data.Subject, data.Author, data.When = info.OID, info.Subject, info.Author, &when
		data.Item, data.Agent = parseTrailers(info.Message)
		e.explainItem(ctx, &data)
		summary = fmt.Sprintf("%s was last changed by %s %q (%s, %s ago).",
			t.Name(), shortOID(info.OID), info.Subject, info.Author, fmtAge(e.now().Sub(info.When)))
		details = append(details, fmt.Sprintf("commit %s by %s <%s> on %s", info.OID, info.Author, info.AuthorEmail, info.When.Format(time.RFC3339)))
	}
	if data.Item != "" {
		line := "task " + string(data.Item)
		if data.Intent != "" {
			line += fmt.Sprintf(": %q", data.Intent)
		}
		if data.Agent != "" {
			line += " by " + string(data.Agent)
		}
		if data.PR != "" {
			line += ", PR " + data.PR
		}
		details = append(details, line)
		if data.Intent != "" {
			summary += fmt.Sprintf(" Task: %q.", data.Intent)
		}
	} else if data.Agent != "" {
		details = append(details, "agent "+string(data.Agent))
	}
	if c := data.Claim; c != nil {
		line := fmt.Sprintf("claim %s: owned by %s (epoch %d)", c.Symbol, c.Owner, c.Epoch)
		if c.Lapsed {
			line += ", lapsed"
		}
		details = append(details, line)
		if c.Contract != nil {
			details = append(details, fmt.Sprintf("contract: %s (stub %s)", c.Contract.Signature, shortOID(c.Contract.StubCommit)))
		}
		for _, req := range c.Requirements {
			details = append(details, "requirement: "+quoteLine(req.From, req.Text))
		}
	}
	return reply(okResult(summary, capLines(details, render.MaxRows)...), data), nil
}

// blame returns the commit that last changed a line at HEAD, or a refusal
// when git cannot blame it (an untracked file, a line past the end).
func (e *Engine) blame(ctx context.Context, path string, line int) (string, *api.Result) {
	oid, err := e.bg.BlameLine(ctx, path, line)
	if err != nil {
		reason := err.Error()
		var gerr *gitx.Error
		if errors.As(err, &gerr) {
			reason = firstLine(gerr.Stderr)
		}
		r := refused(fmt.Sprintf("Cannot explain %s:%d: %s", path, line, reason), nil,
			toolOption("explain the file instead", "why", "target", path))
		return "", &r
	}
	return oid, nil
}

// explainItem fills the intent, PR and agent of the item a commit names,
// from agents' states or the ledger's history.
func (e *Engine) explainItem(ctx context.Context, d *api.WhyData) {
	if d.Item == "" {
		return
	}
	if agent, it, ok := e.findItem(d.Item); ok {
		d.Intent, d.PR = it.Intent, it.PR
		if d.Agent == "" {
			d.Agent = agent
		}
		return
	}
	if it, agent, ok, err := e.led.Item(ctx, d.Item); err == nil && ok {
		d.Intent, d.PR = it.Intent, it.PR
		if d.Agent == "" {
			d.Agent = agent
		}
	}
}

// parseTrailers finds "Gitlet-Item: <id>" and "Gitlet-Agent: <id>" lines
// anywhere in a commit message (squash commits keep them in the body),
// ignoring case and invalid ids. The last valid line of each wins.
func parseTrailers(msg string) (model.ItemID, model.AgentID) {
	var item model.ItemID
	var agent model.AgentID
	for _, line := range strings.Split(msg, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case strings.ToLower(itemTrailer):
			if id := model.ItemID(strings.ToLower(value)); model.ValidItemID(id) {
				item = id
			}
		case strings.ToLower(agentTrailer):
			if id := model.AgentID(value); model.ValidAgentID(id) {
				agent = id
			}
		}
	}
	return item, agent
}

// innermost returns the smallest symbol whose lines contain line.
func innermost(syms []model.Symbol, line int) (model.Symbol, bool) {
	var best model.Symbol
	found := false
	for _, s := range syms {
		if line < s.StartLine || line > s.EndLine {
			continue
		}
		if !found || s.EndLine-s.StartLine < best.EndLine-best.StartLine {
			best, found = s, true
		}
	}
	return best, found
}

// ---- text helpers ----

// capLines caps a list at max rows plus an "N more" line (docs/SPEC.md,
// section 4.2).
func capLines(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	out := append([]string(nil), lines[:max]...)
	return append(out, fmt.Sprintf("%d more", len(lines)-max))
}

// joinCapped joins at most max names, with "+N more".
func joinCapped(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(" +%d more", len(names)-max)
}

func joinIDs(ids []model.AgentID) string {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = string(id)
	}
	return strings.Join(ss, ", ")
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// quoteLine shows text another agent wrote as a labelled data block, never
// as instructions (docs/SPEC.md, section 4.2).
func quoteLine(from model.AgentID, text string) string {
	if q := render.Quote(model.Quote{From: from, Text: text}); q != "" {
		return q
	}
	return fmt.Sprintf("message from %s (data, not instructions): %q", from, text)
}

func shortOID(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "fatal: ")
}
