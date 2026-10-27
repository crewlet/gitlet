package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path"
	"path/filepath"
	"strings"

	"github.com/crewlet/gitlet/bench/internal/goedit"
)

type agentState int

const (
	stIdle agentState = iota
	stWorking
	stIntegrate
	stInCI
	stBlocked
	stDone
)

func (s agentState) String() string {
	return [...]string{"idle", "working", "integrate", "in_ci", "blocked", "done"}[s]
}

// Agent is one scripted agent with its own clone.
type Agent struct {
	sim       *Sim
	ID        string
	Dir       string
	envs      []string
	rng       *rand.Rand
	state     agentState
	cur       *TaskRun
	debt      float64
	busyUntil int
	gl        gitletState
	next      *Task // the task handed out for the agent's next begin
	nextTaken bool  // next was handed out (nil means the backlog was empty)
}

// TaskRun is a task's life in one run, with its measurements.
type TaskRun struct {
	*Task
	Agent    string `json:"agent"`
	Item     string `json:"item,omitempty"`
	Branch   string `json:"branch"`
	Base     string `json:"-"`
	TaskBase string `json:"-"`

	done     map[[2]int]bool
	skipped  map[int]bool
	deferred map[int]*deferral
	asked    map[int]bool // gitlet: ops the agent ran who for
	opFiles  [][]string
	verified string
	// blockStreak counts rounds blocked on contracts since the agent last
	// gave up waiting.
	blockStreak int

	Outcome   string `json:"outcome"` // landed, abandoned_dup, empty, stuck, unfinished
	Started   int    `json:"started"`
	FirstDone int    `json:"firstDone"`
	Submitted int    `json:"submitted"`
	Landed    int    `json:"landed"`
	Ended     int    `json:"ended"` // the round the task left its agent, landed or not
	Submits   int    `json:"submits"`

	WorkLines      int            `json:"workLines"` // size of the task's own change when first done
	Conflicts      int            `json:"conflicts"` // conflicts the agent resolved by hand
	ConflictFiles  int            `json:"conflictFiles"`
	ConflictHunks  int            `json:"conflictHunks"`
	ConflictLines  int            `json:"conflictLines"`
	BuildBreaks    int            `json:"buildBreaks"`    // local builds broken after a clean rebase
	FixLines       int            `json:"fixLines"`       // lines changed to fix them
	MergeConflicts int            `json:"mergeConflicts"` // PRs sent back for a conflict with main
	CIFailures     int            `json:"ciFailures"`     // PRs sent back by CI
	EarlyFixLines  int            `json:"earlyFixLines"`  // lines changed adapting to a gitlet stub or update
	DeferredLines  int            `json:"deferredLines"`  // lines taken out to wait for an owner
	WaitRounds     int            `json:"waitRounds"`     // rounds with nothing runnable
	BusyRounds     int            `json:"busyRounds"`     // rounds spent resolving, fixing or coordinating
	BlockedRounds  int            `json:"blockedRounds"`  // rounds waiting for a contract to land
	CoordCalls     int            `json:"coordCalls"`     // gitlet calls other than status
	Defers         int            `json:"defers"`
	Notes          map[string]int `json:"notes,omitempty"`
}

type deferral struct {
	waitFor []string // task ids
	since   int
	reason  string
}

// maxDefer bounds how long an agent waits on another task before going on.
const maxDefer = 25

func (a *Agent) fail(why string) {
	if a.cur != nil {
		a.sim.event(a, "stuck", map[string]any{"why": why})
		if a.sim.cfg.Mode == ModeGitlet {
			// Stop the gitlet item too, or every later start is refused.
			a.glCall(context.Background(), "abandon", "--reason", "giving up: "+trim(why, 200))
		}
		a.finish("stuck")
		return
	}
	a.state = stIdle
}

// finish ends the agent's current task with an outcome.
func (a *Agent) finish(outcome string) {
	s := a.sim
	s.mu.Lock()
	a.cur.Outcome = outcome
	a.cur.Ended = s.round
	s.mu.Unlock()
	a.cur = nil
	a.state = stIdle
}

func (a *Agent) git(ctx context.Context, args ...string) (string, error) {
	return a.sim.env.Git(ctx, a.Dir, args...)
}

func (a *Agent) mustGit(ctx context.Context, args ...string) string {
	out, err := a.git(ctx, args...)
	if err != nil {
		panic(err)
	}
	return out
}

func (a *Agent) act(ctx context.Context) {
	s := a.sim
	if a.state == stDone {
		return
	}
	if a.busyUntil > s.round {
		if a.cur != nil {
			a.cur.BusyRounds++
		}
		return
	}
	if a.debt >= 1 {
		a.debt--
		if a.cur != nil {
			a.cur.BusyRounds++
		}
		return
	}
	gitlet := s.cfg.Mode == ModeGitlet
	switch a.state {
	case stIdle:
		a.begin(ctx)
	case stWorking:
		if gitlet {
			a.policy(ctx)
			if a.state != stWorking || a.busyUntil > s.round {
				return
			}
		}
		if s.cfg.Mode == ModeGitSync {
			a.sync(ctx)
			if a.state != stWorking || a.busyUntil > s.round {
				return
			}
		}
		a.work(ctx)
	case stIntegrate:
		if gitlet {
			a.policy(ctx)
			if a.state != stIntegrate || a.busyUntil > s.round {
				return
			}
		}
		a.integrate(ctx)
	case stInCI:
		if gitlet {
			a.policy(ctx)
		}
	case stBlocked:
		a.cur.BlockedRounds++
		a.policy(ctx)
		if a.state == stBlocked {
			a.integrate(ctx)
		}
	}
}

// ready reports whether the agent will call begin this round, so that the
// simulation can hand out tasks in agent order before the agents run.
func (a *Agent) ready() bool {
	return a.state == stIdle && a.busyUntil <= a.sim.round && a.debt < 1
}

func (a *Agent) begin(ctx context.Context) {
	s := a.sim
	task := a.next
	if !a.nextTaken {
		task = s.take()
	}
	a.next, a.nextTaken = nil, false
	if task == nil {
		a.state = stDone
		return
	}
	tr := &TaskRun{Task: task, Agent: a.ID, Started: s.round, done: map[[2]int]bool{}, skipped: map[int]bool{}, deferred: map[int]*deferral{}, asked: map[int]bool{}, Notes: map[string]int{}, Outcome: "unfinished"}
	s.mu.Lock()
	s.runs[task.ID] = tr
	s.mu.Unlock()
	a.cur = tr
	a.mustGit(ctx, "fetch", "-q", "origin")
	a.mustGit(ctx, "reset", "-q", "--hard")
	a.mustGit(ctx, "clean", "-qfd")
	a.mustGit(ctx, "checkout", "-q", "--detach", "origin/main")
	tr.Base = a.mustGit(ctx, "rev-parse", "HEAD")
	tr.TaskBase = tr.Base
	t, err := goedit.Load(a.Dir)
	if err != nil {
		panic(err)
	}
	for _, op := range task.Ops {
		tr.opFiles = append(tr.opFiles, opFiles(t, op))
	}
	s.event(a, "start", map[string]any{"intent": task.Intent, "dupOf": task.DupOf})
	if s.cfg.Mode == ModeGitlet {
		a.gl.reset()
		res := a.glCall(ctx, "start", task.Intent, "--paths", strings.Join(task.Files, ","))
		if res == nil || res.Outcome != "ok" {
			a.fail("gitlet start failed")
			return
		}
		var data struct {
			Item struct {
				ID     string `json:"id"`
				Branch string `json:"branch"`
			} `json:"item"`
		}
		_ = res.data(&data)
		tr.Item, tr.Branch = data.Item.ID, data.Item.Branch
		s.mu.Lock()
		s.byItem[tr.Item] = task.ID
		s.mu.Unlock()
		a.state = stWorking
		a.handleNotes(ctx, res.Notes)
		return
	}
	tr.Branch = "work/" + task.ID
	a.mustGit(ctx, "checkout", "-q", "-B", tr.Branch)
	a.state = stWorking
}

func opFiles(t *goedit.Tree, op goedit.Op) []string {
	files := map[string]bool{}
	switch op.Kind {
	case goedit.Changelog:
		files["CHANGELOG.md"] = true
	case goedit.AddFunc:
		files[op.File] = true
	default:
		if fn := t.Func(op.Target); fn != nil {
			files[fn.File] = true
			if op.Kind == goedit.AddParam || op.Kind == goedit.RenameFunc {
				for _, f := range goedit.CallerFiles(t, fn) {
					files[f] = true
				}
			}
		}
	}
	return sortedKeys(files)
}

// renames is what the agent knows of renamed functions: those landed on
// main, and its own.
func (a *Agent) renames() goedit.Renames {
	s := a.sim
	s.mu.Lock()
	r := goedit.Renames{}
	for k, v := range s.renames {
		r[k] = v
	}
	s.mu.Unlock()
	if a.cur != nil {
		for _, op := range a.cur.Ops {
			if op.Kind == goedit.RenameFunc {
				dir, _, _ := strings.Cut(op.Target, ":")
				r[op.Target] = goedit.FuncKey(dir, "", op.NewName)
			}
		}
		for k, v := range a.gl.renames {
			r[k] = v
		}
	}
	return r
}

// ownFiles lists the files the task's own changes touch: where an agent
// fixes its calls after taking in someone else's signature.
func (tr *TaskRun) ownFiles() []string {
	files := map[string]bool{}
	for _, fs := range tr.opFiles {
		for _, f := range fs {
			files[f] = true
		}
	}
	return sortedKeys(files)
}

// midOp reports whether some op has steps done and steps left.
func (tr *TaskRun) midOp() bool {
	for i := range tr.Ops {
		if st := tr.nextStep(i); st > 0 && !tr.skipped[i] {
			return true
		}
	}
	return false
}

func (tr *TaskRun) opDone(i int) bool {
	if tr.skipped[i] {
		return true
	}
	for st := range tr.Ops[i].Steps() {
		if !tr.done[[2]int{i, st}] {
			return false
		}
	}
	return true
}

func (tr *TaskRun) nextStep(i int) int {
	for st := range tr.Ops[i].Steps() {
		if !tr.done[[2]int{i, st}] {
			return st
		}
	}
	return -1
}

func (a *Agent) work(ctx context.Context) {
	s := a.sim
	tr := a.cur
	remaining := false
	for i, op := range tr.Ops {
		if tr.opDone(i) {
			continue
		}
		remaining = true
		if d := tr.deferred[i]; d != nil {
			if !a.deferralOver(d) {
				continue
			}
			delete(tr.deferred, i)
			s.event(a, "undefer", map[string]any{"op": op.Tag, "waited": s.round - d.since})
			if s.cfg.Mode == ModeGitlet && !a.bringInMain(ctx, false) {
				return
			}
		}
		st := tr.nextStep(i)
		if s.cfg.Mode == ModeFileLock && st == 0 {
			if holder := a.lockHolder(tr.opFiles[i]); holder != "" {
				continue
			}
			// With the lock held, start from the latest version of its
			// files, as a lock protocol says.
			if !a.freshFiles(ctx, tr.opFiles[i]) {
				return
			}
		}
		if s.cfg.Mode == ModeGitlet && st == 0 && !tr.asked[i] {
			tr.asked[i] = true
			if a.askWho(ctx, i) {
				continue
			}
			if a.state != stWorking || a.busyUntil > s.round {
				return
			}
		}
		t, err := goedit.Load(a.Dir)
		if err != nil {
			panic(err)
		}
		err = goedit.Apply(t, op, st, a.renames())
		if errors.Is(err, goedit.ErrGone) {
			tr.skipped[i] = true
			s.event(a, "op_gone", map[string]any{"op": op.Tag})
			return
		}
		if err != nil {
			panic(fmt.Sprintf("apply %s step %d: %v", op.Tag, st, err))
		}
		tr.done[[2]int{i, st}] = true
		a.commit(ctx, fmt.Sprintf("%s: %s step %d", tr.ID, op.Tag, st))
		return
	}
	if remaining {
		tr.WaitRounds++
		return
	}
	tr.FirstDone = s.round
	if n, err := diffLines(ctx, s.env, a.Dir, tr.Base, "HEAD"); err == nil && tr.WorkLines == 0 {
		tr.WorkLines = n
	}
	a.state = stIntegrate
}

func (a *Agent) commit(ctx context.Context, msg string) bool {
	a.mustGit(ctx, "add", "-A")
	if st := a.mustGit(ctx, "status", "--porcelain"); st == "" {
		return false
	}
	a.mustGit(ctx, "commit", "-q", "--no-verify", "-m", msg)
	return true
}

// lockHolder returns the task holding any of the files: the earliest
// started task still in flight that plans to touch it.
func (a *Agent) lockHolder(files []string) string {
	s := a.sim
	s.mu.Lock()
	defer s.mu.Unlock()
	mine := a.cur
	for _, other := range s.runs {
		if other == mine || other.Outcome != "unfinished" {
			continue
		}
		if other.Started > mine.Started || other.Started == mine.Started && other.Agent > mine.Agent {
			continue
		}
		for _, f := range files {
			for _, g := range other.Files {
				if f == g {
					return other.ID
				}
			}
		}
	}
	return ""
}

func (a *Agent) deferralOver(d *deferral) bool {
	s := a.sim
	if s.round-d.since > maxDefer {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range d.waitFor {
		if r := s.runs[id]; r != nil && r.Outcome == "unfinished" && !s.blocked[id] {
			return false
		}
	}
	return true
}

// freshFiles rebases onto main when main changed any of files since the
// branch's base. It reports whether the agent can go on this round.
func (a *Agent) freshFiles(ctx context.Context, files []string) bool {
	tr := a.cur
	a.mustGit(ctx, "fetch", "-q", "origin")
	main := a.mustGit(ctx, "rev-parse", "origin/main")
	if main == tr.Base || len(files) == 0 {
		return true
	}
	changed, err := a.git(ctx, append([]string{"diff", "--name-only", tr.Base, main, "--"}, files...)...)
	if err != nil || strings.TrimSpace(changed) == "" {
		return true
	}
	return a.rebase(ctx, main)
}

// sync is gitsync's habit: whenever main moves, rebase onto it and build.
// It waits for an op in progress to be finished, as its half-done change
// would not build.
func (a *Agent) sync(ctx context.Context) {
	if a.cur.midOp() {
		return
	}
	a.mustGit(ctx, "fetch", "-q", "origin")
	main := a.mustGit(ctx, "rev-parse", "origin/main")
	if isAncestor(ctx, a.sim.env, a.Dir, main, "HEAD") {
		return
	}
	if !a.rebase(ctx, main) {
		return
	}
	a.check(ctx, main)
}

// rebase brings main into the branch with plain git. A conflict is
// resolved by hand, which costs rounds. It reports whether the branch is on
// main without needing more time.
func (a *Agent) rebase(ctx context.Context, main string) bool {
	head := a.mustGit(ctx, "rev-parse", "HEAD")
	_, conflict, err := mergeTree(ctx, a.sim.env, a.Dir, main, head)
	if err != nil {
		panic(err)
	}
	if conflict != nil {
		a.resolve(ctx, main, conflict)
		return false
	}
	r, err := a.sim.env.Run(ctx, a.Dir, nil, "git", "rebase", "-q", main)
	if err != nil {
		panic(err)
	}
	if r.Code != 0 {
		_, _ = a.git(ctx, "rebase", "--abort")
		a.resolve(ctx, main, &ConflictStats{})
		return false
	}
	a.cur.Base = main
	return true
}

// check builds the branch when it holds commits from main the task was not
// generated against, and fixes a broken build by redoing the task on main.
func (a *Agent) check(ctx context.Context, main string) bool {
	tr := a.cur
	head := a.mustGit(ctx, "rev-parse", "HEAD")
	if head == tr.verified || isAncestor(ctx, a.sim.env, a.Dir, "HEAD", tr.TaskBase) || a.mustGit(ctx, "merge-base", "HEAD", "origin/main") == tr.TaskBase && main == tr.TaskBase {
		tr.verified = head
		return true
	}
	if a.hasStubs(ctx, main) {
		return true // a picked stub without its callers does not build; submit decides
	}
	pass, errs, err := Build(ctx, a.sim.env, a.Dir)
	if err != nil {
		panic(err)
	}
	if pass {
		tr.verified = head
		return true
	}
	a.fixBuild(ctx, main, errs)
	return false
}

func (a *Agent) hasStubs(ctx context.Context, main string) bool {
	out, err := a.git(ctx, "log", "--format=%H", "--grep=^Gitlet-Stub:", main+"..HEAD")
	return err == nil && strings.TrimSpace(out) != ""
}

// resolve handles a conflict the way an agent does: read the conflicting
// hunks and produce the merged change, here by redoing the task's ops on
// main. It costs a round plus the conflict's size.
func (a *Agent) resolve(ctx context.Context, main string, cs *ConflictStats) {
	s := a.sim
	tr := a.cur
	if a.duplicateLanded(ctx) {
		return
	}
	tr.Conflicts++
	tr.ConflictFiles += len(cs.Files)
	tr.ConflictHunks += cs.Hunks
	tr.ConflictLines += cs.Lines
	s.event(a, "conflict", map[string]any{"files": cs.Files, "hunks": cs.Hunks, "lines": cs.Lines})
	a.redo(ctx, main)
	a.spend(cs.Lines)
}

// spend keeps the agent busy for a fix of n lines: a round to read the
// failure and redo the work, and a round per ResolveSpeed lines. Every mode
// pays for a fix of the same size the same way.
func (a *Agent) spend(n int) {
	s := a.sim
	a.busyUntil = max(a.busyUntil, s.round) + 1 + int(math.Ceil(float64(n)/float64(s.cfg.ResolveSpeed)))
}

func (a *Agent) fixBuild(ctx context.Context, main string, errs []string) {
	s := a.sim
	tr := a.cur
	if a.duplicateLanded(ctx) {
		return
	}
	before := a.mustGit(ctx, "rev-parse", "HEAD")
	tr.BuildBreaks++
	if tr.BuildBreaks > 6 {
		a.fail("build keeps breaking: " + strings.Join(errs[:min(2, len(errs))], " | "))
		return
	}
	a.redo(ctx, main)
	n, _ := diffLines(ctx, s.env, a.Dir, before, "HEAD")
	tr.FixLines += n
	s.event(a, "build_break", map[string]any{"errors": errs[:min(4, len(errs))], "fixLines": n})
	a.spend(n)
}

// duplicateLanded models what an agent sees while resolving a conflict or
// a broken build: when the same task already landed, it drops its own copy
// instead of merging the two. The work is wasted either way.
func (a *Agent) duplicateLanded(ctx context.Context) bool {
	s := a.sim
	tr := a.cur
	s.mu.Lock()
	found := ""
	for _, o := range s.runs {
		if o != tr && o.Outcome == "landed" && (o.DupOf == tr.ID || tr.DupOf == o.ID || o.DupOf != "" && o.DupOf == tr.DupOf) {
			found = o.ID
		}
	}
	s.mu.Unlock()
	if found == "" {
		return false
	}
	wasted, _ := diffLines(ctx, s.env, a.Dir, tr.Base, "HEAD")
	s.event(a, "dup_late", map[string]any{"of": found, "wastedLines": wasted})
	if s.cfg.Mode == ModeGitlet {
		a.glCall(ctx, "abandon", "--reason", "duplicate of "+found+", which landed")
	}
	a.finish("dup_late")
	return true
}

// redo rebuilds the branch as main plus the task's ops (and any stubs the
// agent took from gitlet), which is the merged result an agent would write.
func (a *Agent) redo(ctx context.Context, main string) {
	s := a.sim
	tr := a.cur
	a.mustGit(ctx, "reset", "-q", "--hard", main)
	a.mustGit(ctx, "clean", "-qfd")
	for _, stub := range a.gl.stubs {
		if isAncestor(ctx, s.env, a.Dir, stub.commit, main) {
			continue
		}
		r, err := s.env.Run(ctx, a.Dir, []string{"GITLET_INTERNAL=1"}, "git", "cherry-pick", "--allow-empty", "--keep-redundant-commits", stub.commit)
		if err != nil || r.Code != 0 {
			_, _ = a.git(ctx, "cherry-pick", "--abort")
			s.event(a, "stub_repick_failed", map[string]any{"symbol": stub.symbol})
		}
	}
	var ops []goedit.Op
	for i, op := range tr.Ops {
		if !tr.skipped[i] && tr.nextStep(i) != 0 {
			ops = append(ops, op)
		}
	}
	// Ops are redone only as far as the agent had got.
	for _, op := range ops {
		i := indexOf(tr.Ops, op.Tag)
		for st := range op.Steps() {
			if !tr.done[[2]int{i, st}] {
				break
			}
			t, err := goedit.Load(a.Dir)
			if err != nil {
				panic(err)
			}
			if err := goedit.Apply(t, op, st, a.renames()); err != nil {
				if errors.Is(err, goedit.ErrGone) {
					tr.skipped[i] = true
					break
				}
				panic(fmt.Sprintf("redo %s step %d: %v", op.Tag, st, err))
			}
		}
	}
	if keys := a.gl.neededKeys(); len(keys) > 0 {
		t, err := goedit.Load(a.Dir)
		if err != nil {
			panic(err)
		}
		if err := goedit.FixCalls(t, keys, a.renames(), a.cur.ownFiles()); err != nil {
			panic(err)
		}
	}
	a.commit(ctx, fmt.Sprintf("%s: redo on %s", tr.ID, short(main)))
	tr.Base = main
	tr.verified = ""
}

func indexOf(ops []goedit.Op, tag string) int {
	for i, op := range ops {
		if op.Tag == tag {
			return i
		}
	}
	return -1
}

func short(oid string) string { return oid[:min(8, len(oid))] }

// integrate brings main in, builds, and submits.
func (a *Agent) integrate(ctx context.Context) {
	s := a.sim
	tr := a.cur
	a.mustGit(ctx, "fetch", "-q", "origin")
	main := a.mustGit(ctx, "rev-parse", "origin/main")
	if !isAncestor(ctx, s.env, a.Dir, main, "HEAD") {
		if s.cfg.Mode == ModeGitlet {
			if !a.bringInMain(ctx, false) {
				return
			}
		} else if !a.rebase(ctx, main) {
			return
		}
	}
	if !a.check(ctx, main) {
		return
	}
	head := a.mustGit(ctx, "rev-parse", "HEAD")
	if head == main || isAncestor(ctx, s.env, a.Dir, "HEAD", main) {
		// Nothing left: the change is already on main (a duplicate landed).
		s.event(a, "empty", nil)
		if s.cfg.Mode == ModeGitlet {
			a.glCall(ctx, "abandon", "--reason", "already on main")
		}
		a.finish("empty")
		return
	}
	if s.cfg.Mode == ModeGitlet {
		a.gitletSubmit(ctx, head)
		return
	}
	a.mustGit(ctx, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+tr.Branch)
	a.submitted(head)
}

func (a *Agent) submitted(head string) {
	s := a.sim
	tr := a.cur
	tr.Submits++
	if tr.Submitted == 0 {
		tr.Submitted = s.round
	}
	s.submit(a, head)
	a.state = stInCI
	s.event(a, "submit", map[string]any{"head": head})
}

// bounced is called by the merge bot between rounds.
func (a *Agent) bounced(reason string, tr *TaskRun) {
	a.state = stIntegrate
	if tr.MergeConflicts+tr.CIFailures > 8 {
		a.fail("bounced too often: " + reason)
	}
}

// landed is called by the merge bot between rounds.
func (a *Agent) landed() {
	a.finish("landed")
}

func pathKey(sym string) string {
	// "mcp/server.go:Server.AddTool" -> "mcp:Server.AddTool"
	file, q, ok := strings.Cut(sym, ":")
	if !ok {
		return sym
	}
	return path.Dir(filepath.ToSlash(file)) + ":" + q
}
