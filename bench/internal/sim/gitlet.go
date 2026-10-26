package sim

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/gitlet/bench/internal/goedit"
)

// gitletState is what a gitlet agent remembers about its current task.
type gitletState struct {
	handled map[string]bool
	tries   map[string]int    // times a note was put back unhandled
	needs   map[string]string // gitlet symbol -> function key
	stubs   []pickedStub
	renames goedit.Renames
}

type pickedStub struct {
	symbol, key, commit string
}

func (g *gitletState) reset() {
	*g = gitletState{handled: map[string]bool{}, tries: map[string]int{}, needs: map[string]string{}, renames: goedit.Renames{}}
}

func (g *gitletState) neededKeys() []string {
	var keys []string
	for _, st := range g.stubs {
		keys = append(keys, st.key)
	}
	return keys
}

type glOption struct {
	Label   string            `json:"label"`
	Tool    string            `json:"tool"`
	Args    map[string]string `json:"args"`
	Command string            `json:"command"`
}

type glParty struct {
	Agent  string `json:"agent"`
	Item   string `json:"item"`
	Intent string `json:"intent"`
}

type glNote struct {
	ID          string     `json:"id"`
	Version     string     `json:"version"`
	Kind        string     `json:"kind"`
	Severity    string     `json:"severity"`
	Subject     string     `json:"subject"`
	SubjectName string     `json:"subjectName"`
	Thread      string     `json:"thread"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Parties     []glParty  `json:"parties"`
	Evidence    []string   `json:"evidence"`
	Options     []glOption `json:"options"`
}

func (n *glNote) option(tool string) *glOption {
	for i := range n.Options {
		if n.Options[i].Tool == tool {
			return &n.Options[i]
		}
	}
	return nil
}

func (n *glNote) size() int {
	b, _ := json.Marshal(struct {
		T, B string
		E    []string
		O    []glOption
	}{n.Title, n.Body, n.Evidence, n.Options})
	return len(b)
}

type glResult struct {
	Outcome  string          `json:"outcome"`
	Summary  string          `json:"summary"`
	Details  []string        `json:"details"`
	Options  []glOption      `json:"options"`
	Notes    []glNote        `json:"notes"`
	Warnings []string        `json:"warnings"`
	Data     json.RawMessage `json:"data"`
}

func (r *glResult) data(v any) error { return json.Unmarshal(r.Data, v) }

func (r *glResult) text() string {
	return r.Summary + " " + strings.Join(r.Details, " ")
}

// GitletStats records the cost of gitlet calls in a run.
type GitletStats struct {
	mu        sync.Mutex
	Calls     map[string]int       `json:"calls"`
	Refused   map[string]int       `json:"refused"`
	Errors    map[string]int       `json:"errors"`
	Durations map[string][]float64 `json:"-"`
	NoteChars int                  `json:"noteChars"`
	Notes     int                  `json:"notes"`
}

func (g *GitletStats) record(verb string, d time.Duration, outcome string, code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Calls == nil {
		g.Calls, g.Refused, g.Errors, g.Durations = map[string]int{}, map[string]int{}, map[string]int{}, map[string][]float64{}
	}
	g.Calls[verb]++
	g.Durations[verb] = append(g.Durations[verb], d.Seconds())
	switch {
	case code == 1 || code == 2 || outcome == "":
		g.Errors[verb]++
	case outcome == "refused":
		g.Refused[verb]++
	}
}

// positional arguments of each verb, in order.
var positional = map[string][]string{
	"start": {"intent"}, "who": {"target"}, "why": {"target"}, "claim": {"symbol"}, "publish": {"symbol"},
	"need": {"symbol"}, "use": {"symbol"}, "release": {"symbol"}, "discuss": {"about", "message"},
	"accept": {"proposal"}, "dismiss": {"note", "as"},
}

var replacesGit = map[string]bool{"start": true, "update": true, "submit": true}

// glCall runs a gitlet verb in the agent's clone. Every call but status
// costs the agent CoordCost rounds of attention.
func (a *Agent) glCall(ctx context.Context, verb string, args ...string) *glResult {
	s := a.sim
	full := append(append([]string{verb}, args...), "--json")
	r, err := s.env.Run(ctx, a.Dir, a.envs, s.cfg.Gitlet, full...)
	if err != nil {
		s.stats.record(verb, r.Duration, "", -1)
		s.event(a, "gitlet_error", map[string]any{"verb": verb, "error": err.Error()})
		return nil
	}
	var res glResult
	if r.Code == 1 || r.Code == 2 || json.Unmarshal([]byte(r.Stdout), &res) != nil {
		s.stats.record(verb, r.Duration, "", r.Code)
		s.event(a, "gitlet_error", map[string]any{"verb": verb, "args": args, "code": r.Code, "stderr": trim(r.Stderr, 400)})
		return nil
	}
	s.stats.record(verb, r.Duration, res.Outcome, r.Code)
	if verb != "status" {
		// start, update and submit stand in for git checkout, rebase and
		// push, which the git modes do for free; the rest is coordination
		// work an agent would not do without gitlet.
		if !replacesGit[verb] {
			if a.cur != nil {
				a.cur.CoordCalls++
			}
			a.debt += s.cfg.CoordCost
		}
		s.event(a, "gitlet", map[string]any{"verb": verb, "args": args, "outcome": res.Outcome, "summary": trim(res.Summary, 300), "secs": r.Duration.Seconds()})
	}
	return &res
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// glOpt runs an option exactly as a note offered it.
func (a *Agent) glOpt(ctx context.Context, o *glOption) *glResult {
	var args []string
	used := map[string]bool{}
	for _, p := range positional[o.Tool] {
		args = append(args, o.Args[p])
		used[p] = true
	}
	keys := make([]string, 0, len(o.Args))
	for k := range o.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if used[k] || o.Args[k] == "" {
			continue
		}
		args = append(args, "--"+k, o.Args[k])
	}
	return a.glCall(ctx, o.Tool, args...)
}

// policy is the gitlet agent's habit each round: read its notes and act on
// them the way the skill says.
func (a *Agent) policy(ctx context.Context) {
	if a.cur == nil {
		return
	}
	res := a.glCall(ctx, "status")
	if res == nil {
		return
	}
	a.handleNotes(ctx, res.Notes)
}

func (a *Agent) partyTasks(n *glNote) []string {
	s := a.sim
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range n.Parties {
		if id, ok := s.byItem[p.Item]; ok {
			out = append(out, id)
		} else {
			out = append(out, "?"+p.Item)
		}
	}
	return out
}

func (a *Agent) handleNotes(ctx context.Context, notes []glNote) {
	s := a.sim
	for i := range notes {
		n := &notes[i]
		if a.cur == nil {
			return
		}
		key := n.ID + "@" + n.Version
		if a.gl.handled[key] {
			continue
		}
		a.gl.handled[key] = true
		parties := a.partyTasks(n)
		a.cur.Notes[n.Kind+"/"+n.Severity]++
		size := n.size()
		s.stats.mu.Lock()
		s.stats.NoteChars += size
		s.stats.Notes++
		s.stats.mu.Unlock()
		s.event(a, "note", map[string]any{"id": n.ID, "noteKind": n.Kind, "severity": n.Severity, "parties": parties, "subject": n.SubjectName, "chars": size, "state": a.state.String()})
		if n.Severity == "info" && !(n.Kind == "duplicate" && a.sim.cfg.DupJudge == "oracle") {
			continue
		}
		if a.rng.Float64() >= s.cfg.Compliance {
			s.event(a, "ignored", map[string]any{"id": n.ID, "noteKind": n.Kind})
			continue
		}
		if !a.react(ctx, n, parties) && a.gl.tries[key] < 3 {
			// Not actionable now: look at it again next time, a few times.
			a.gl.tries[key]++
			delete(a.gl.handled, key)
		}
	}
}

// react acts on a note. It reports false when the agent could not act on
// it in its current state, so that the note is read again later.
func (a *Agent) react(ctx context.Context, n *glNote, parties []string) bool {
	working := a.state == stWorking || a.state == stIntegrate || a.state == stBlocked
	switch n.Kind {
	case "callee_changed":
		if !working {
			return false
		}
		if o := n.option("need"); o != nil {
			if res := a.glOpt(ctx, o); res != nil && res.Outcome != "refused" {
				a.gl.needs[o.Args["symbol"]] = pathKey(o.Args["symbol"])
			}
		}
	case "ready", "drift":
		if !working {
			return false
		}
		return a.useStub(ctx, n, parties)
	case "callers_affected":
		if o := n.option("claim"); o != nil {
			a.glOpt(ctx, o)
		}
		if o := n.option("publish"); o != nil {
			a.glOpt(ctx, o)
		}
	case "needed", "diverged", "agreement":
		if o := n.option("publish"); o != nil {
			a.glOpt(ctx, o)
		}
	case "message":
		if o := n.option("accept"); o != nil {
			a.glOpt(ctx, o)
		}
	case "signature_clash":
		if working && a.later(parties) {
			a.yieldSignature(ctx, n, parties)
		}
	case "body_overlap", "predicted_conflict":
		if a.sim.cfg.Overlap != "ignore" && a.state == stWorking && a.later(parties) {
			a.deferFor(n, parties)
		}
	case "stale", "new_callers":
		if working {
			a.bringInMain(ctx, true)
		}
	case "duplicate":
		if working && a.later(parties) && (n.Severity == "act" || a.sim.cfg.DupJudge == "oracle" && a.trueDuplicate(parties)) {
			a.abandonDuplicate(ctx, n, parties)
		}
	case "still_waiting":
		if o := n.option("release"); o != nil {
			a.glOpt(ctx, o)
		}
	}
	return true
}

// later reports whether any counterpart task started before this agent's.
func (a *Agent) later(parties []string) bool {
	return len(a.earlier(parties)) > 0
}

// earlier lists the counterpart tasks that started before this agent's
// and are still in flight: the ones it makes way for.
func (a *Agent) earlier(parties []string) []string {
	s := a.sim
	s.mu.Lock()
	defer s.mu.Unlock()
	mine := a.cur
	var out []string
	for _, id := range parties {
		o := s.runs[id]
		// A task blocked on a contract waits for someone, often this
		// agent: making way for it would deadlock.
		if o == nil || o == mine || o.Outcome != "unfinished" || s.blocked[id] {
			continue
		}
		if o.Started < mine.Started || o.Started == mine.Started && o.Agent < mine.Agent {
			out = append(out, id)
		}
	}
	return out
}

func (a *Agent) useStub(ctx context.Context, n *glNote, parties []string) bool {
	s := a.sim
	tr := a.cur
	o := n.option("use")
	if o == nil {
		return true
	}
	a.commit(ctx, tr.ID+": work in progress")
	before := a.mustGit(ctx, "rev-parse", "HEAD")
	res := a.glOpt(ctx, o)
	if res == nil || res.Outcome == "refused" {
		if res != nil {
			s.event(a, "use_refused", map[string]any{"symbol": o.Args["symbol"], "why": trim(res.text(), 300)})
		}
		return false
	}
	after := a.mustGit(ctx, "rev-parse", "HEAD")
	key := pathKey(o.Args["symbol"])
	// What the owner renames the function to, if the change is a rename.
	s.mu.Lock()
	for _, id := range parties {
		if r := s.runs[id]; r != nil {
			for _, op := range r.Ops {
				if op.Kind == goedit.RenameFunc && op.Target == key {
					dir, _, _ := strings.Cut(key, ":")
					a.gl.renames[key] = goedit.FuncKey(dir, "", op.NewName)
				}
			}
		}
	}
	s.mu.Unlock()
	if after != before {
		a.gl.stubs = append(a.gl.stubs, pickedStub{symbol: o.Args["symbol"], key: key, commit: after})
	}
	a.adapt(ctx, []string{key}, "use")
	return true
}

// adapt changes the agent's calls to fit signatures it just took in.
func (a *Agent) adapt(ctx context.Context, keys []string, why string) {
	s := a.sim
	tr := a.cur
	t, err := goedit.Load(a.Dir)
	if err != nil {
		panic(err)
	}
	before := a.mustGit(ctx, "rev-parse", "HEAD")
	// Calls of functions whose signature this task changes are the
	// agent's to fix everywhere, new callers on main included; calls of
	// other changed functions only in its own code.
	own, ownRen := map[string]bool{}, goedit.Renames{}
	for _, op := range tr.Ops {
		switch op.Kind {
		case goedit.AddParam:
			own[op.Target] = true
		case goedit.RenameFunc:
			dir, _, _ := strings.Cut(op.Target, ":")
			ownRen[op.Target] = goedit.FuncKey(dir, "", op.NewName)
		}
	}
	var mine, others []string
	for _, k := range keys {
		if own[k] {
			mine = append(mine, k)
		} else {
			others = append(others, k)
		}
	}
	if len(mine) > 0 || len(ownRen) > 0 {
		if err := goedit.FixCalls(t, mine, ownRen, nil); err != nil {
			panic(err)
		}
		if t, err = goedit.Load(a.Dir); err != nil {
			panic(err)
		}
	}
	if err := goedit.FixCalls(t, others, a.renames(), tr.ownFiles()); err != nil {
		panic(err)
	}
	if !a.commit(ctx, tr.ID+": adapt to "+why) {
		return
	}
	n, _ := diffLines(ctx, s.env, a.Dir, before, "HEAD")
	tr.EarlyFixLines += n
	a.spend(n)
	s.event(a, "adapt", map[string]any{"why": why, "lines": n})
}

// deferFor postpones the agent's remaining work on what overlaps another
// task that started earlier, until that task lands.
func (a *Agent) deferFor(n *glNote, parties []string) {
	s := a.sim
	tr := a.cur
	files := map[string]bool{}
	key := ""
	if n.Kind == "body_overlap" {
		key = pathKey(n.SubjectName)
	} else {
		for _, e := range n.Evidence {
			if rest, ok := strings.CutPrefix(e, "conflicting files: "); ok {
				for _, f := range strings.Split(rest, ",") {
					files[strings.TrimSpace(f)] = true
				}
			}
		}
		if len(files) == 0 && n.SubjectName != "" {
			files[n.SubjectName] = true
		}
	}
	var deferred []string
	for i, op := range tr.Ops {
		if tr.opDone(i) || tr.deferred[i] != nil {
			continue
		}
		hit := key != "" && op.Target == key
		for _, f := range tr.opFiles[i] {
			if files[f] {
				hit = true
			}
		}
		if hit {
			tr.deferred[i] = &deferral{waitFor: a.earlier(parties), since: s.round, reason: n.Kind}
			tr.Defers++
			deferred = append(deferred, op.Tag)
		}
	}
	s.event(a, "defer", map[string]any{"noteKind": n.Kind, "ops": deferred, "waitFor": parties})
}

// yieldSignature lets the earlier task own a contested signature: the agent
// takes its own change of it out, records a need, and puts the change back
// on top once the owner has landed.
func (a *Agent) yieldSignature(ctx context.Context, n *glNote, parties []string) {
	s := a.sim
	tr := a.cur
	key := pathKey(n.SubjectName)
	var ops []string
	for i, op := range tr.Ops {
		if (op.Kind == goedit.AddParam || op.Kind == goedit.RenameFunc) && op.Target == key && tr.deferred[i] == nil {
			tr.deferred[i] = &deferral{waitFor: a.earlier(parties), since: s.round, reason: n.Kind}
			for st := range op.Steps() {
				delete(tr.done, [2]int{i, st})
			}
			tr.Defers++
			ops = append(ops, op.Tag)
		}
	}
	if len(ops) > 0 {
		before := a.mustGit(ctx, "rev-parse", "HEAD")
		a.redo(ctx, tr.Base)
		n, _ := diffLines(ctx, s.env, a.Dir, before, "HEAD")
		tr.DeferredLines += n
		a.spend(n)
	}
	if o := n.option("need"); o != nil {
		if res := a.glOpt(ctx, o); res != nil && res.Outcome != "refused" {
			a.gl.needs[o.Args["symbol"]] = key
		}
	}
	s.event(a, "yield", map[string]any{"symbol": n.SubjectName, "ops": ops, "to": parties})
}

// trueDuplicate is the agent's judgment of a possible duplicate: having
// read the other task's intent, it knows whether it is the same work. The
// benchmark models a judge that is always right.
func (a *Agent) trueDuplicate(parties []string) bool {
	s := a.sim
	s.mu.Lock()
	defer s.mu.Unlock()
	tr := a.cur
	for _, id := range parties {
		if o := s.runs[id]; o != nil && (o.DupOf == tr.ID || tr.DupOf == o.ID || o.DupOf != "" && o.DupOf == tr.DupOf) {
			return true
		}
	}
	return false
}

func (a *Agent) abandonDuplicate(ctx context.Context, n *glNote, parties []string) {
	s := a.sim
	tr := a.cur
	res := a.glCall(ctx, "abandon", "--reason", "duplicate of "+strings.Join(parties, ","))
	if res == nil || res.Outcome == "refused" {
		return
	}
	truth := false
	s.mu.Lock()
	for _, id := range parties {
		if o := s.runs[id]; o != nil && (o.DupOf == tr.ID || tr.DupOf == o.ID || o.DupOf != "" && o.DupOf == tr.DupOf) {
			truth = true
		}
	}
	s.mu.Unlock()
	wasted, _ := diffLines(ctx, s.env, a.Dir, tr.Base, "HEAD")
	s.event(a, "abandon", map[string]any{"true": truth, "parties": parties, "wastedLines": wasted})
	if truth {
		a.finish("abandoned_dup")
	} else {
		a.finish("abandoned_false_dup")
	}
}

// bringInMain runs gitlet's update. With adaptCalls, it then fixes the
// agent's calls, as a stale or new_callers note asks. It reports whether
// the branch is on main without more time needed.
func (a *Agent) bringInMain(ctx context.Context, adaptCalls bool) bool {
	s := a.sim
	tr := a.cur
	a.mustGit(ctx, "fetch", "-q", "origin")
	main := a.mustGit(ctx, "rev-parse", "origin/main")
	if isAncestor(ctx, s.env, a.Dir, main, "HEAD") {
		return true
	}
	a.commit(ctx, tr.ID+": work in progress")
	res := a.glCall(ctx, "update")
	switch {
	case res == nil:
		return a.rebase(ctx, main)
	case res.Outcome == "ok" || res.Outcome == "nothing_to_do":
		if isAncestor(ctx, s.env, a.Dir, main, "HEAD") {
			tr.Base = main
			tr.verified = ""
			a.refreshStubs(ctx, main)
			if adaptCalls {
				keys := append(append([]string{}, tr.Calls...), tr.Symbols...)
				a.adapt(ctx, keys, "main")
			}
			return true
		}
		return a.rebase(ctx, main)
	default:
		var data struct {
			Conflicts []string `json:"conflicts"`
		}
		_ = res.data(&data)
		head := a.mustGit(ctx, "rev-parse", "HEAD")
		_, cs, err := mergeTree(ctx, s.env, a.Dir, main, head)
		if err != nil {
			panic(err)
		}
		if cs == nil {
			cs = &ConflictStats{Files: data.Conflicts}
		}
		s.event(a, "update_refused", map[string]any{"why": trim(res.text(), 300)})
		a.resolve(ctx, main, cs)
		return false
	}
}

func (a *Agent) gitletSubmit(ctx context.Context, head string) {
	s := a.sim
	tr := a.cur
	res := a.glCall(ctx, "submit")
	if res == nil {
		a.mustGit(ctx, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+tr.Branch)
		a.submitted(head)
		return
	}
	if res.Outcome != "refused" {
		a.submitted(a.mustGit(ctx, "rev-parse", "HEAD"))
		return
	}
	text := res.text()
	switch {
	case strings.Contains(text, "has not landed"):
		if a.state != stBlocked {
			s.event(a, "blocked", map[string]any{"why": trim(text, 300)})
		}
		a.state = stBlocked
		tr.blockStreak++
		if tr.blockStreak > 30 {
			a.dropStubs(ctx)
		}
	case strings.Contains(text, "conflict"):
		a.state = stIntegrate
	default:
		s.event(a, "submit_refused", map[string]any{"why": trim(text, 400)})
		if o := firstOption(res.Options, "publish"); o != nil {
			a.glOpt(ctx, o)
			return
		}
		// gitlet would not submit for a reason the agent cannot act on:
		// push as plain git does.
		a.mustGit(ctx, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+tr.Branch)
		a.submitted(head)
	}
}

func firstOption(opts []glOption, tool string) *glOption {
	for i := range opts {
		if opts[i].Tool == tool {
			return &opts[i]
		}
	}
	return nil
}

// dropStubs gives up waiting on contracts: release the needs and rebuild
// the branch as main plus the task's own work, without the stubs.
func (a *Agent) dropStubs(ctx context.Context) {
	s := a.sim
	for _, st := range a.gl.stubs {
		a.glCall(ctx, "release", st.symbol)
	}
	s.event(a, "drop_stubs", map[string]any{"count": len(a.gl.stubs)})
	a.gl.stubs = nil
	a.gl.needs = map[string]string{}
	a.mustGit(ctx, "fetch", "-q", "origin")
	before := a.mustGit(ctx, "rev-parse", "HEAD")
	a.redo(ctx, a.mustGit(ctx, "rev-parse", "origin/main"))
	n, _ := diffLines(ctx, s.env, a.Dir, before, "HEAD")
	a.cur.FixLines += n
	a.spend(n)
	a.state = stIntegrate
	a.cur.blockStreak = 0
}

// refreshStubs finds the picked stubs again after gitlet's update rebased
// the branch: update drops a stub once its contract lands and rewrites the
// others, so they are known by the stub commits' subjects.
func (a *Agent) refreshStubs(ctx context.Context, main string) {
	out, err := a.git(ctx, "log", "--format=%H %s", "--grep=^Gitlet-Stub:", main+"..HEAD")
	if err != nil {
		return
	}
	commits := map[string]string{} // subject -> commit
	for _, line := range strings.Split(out, "\n") {
		if oid, subj, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			commits[subj] = oid
		}
	}
	var keep []pickedStub
	for _, st := range a.gl.stubs {
		subj, ok := a.stubSubject(ctx, st.commit)
		if !ok {
			continue
		}
		if oid, ok := commits[subj]; ok {
			st.commit = oid
			keep = append(keep, st)
		}
	}
	a.gl.stubs = keep
}

func (a *Agent) stubSubject(ctx context.Context, commit string) (string, bool) {
	out, err := a.git(ctx, "log", "-1", "--format=%s", commit)
	return strings.TrimSpace(out), err == nil
}

// askWho follows the skill's rule to call who before editing a shared
// function: it asks about the function op i edits and, under the defer
// policy, makes way for an earlier task already changing it. It reports
// whether op i was deferred.
func (a *Agent) askWho(ctx context.Context, i int) bool {
	s := a.sim
	tr := a.cur
	op := tr.Ops[i]
	if op.Kind == goedit.AddFunc || op.Kind == goedit.Changelog {
		return false
	}
	t, err := goedit.Load(a.Dir)
	if err != nil {
		panic(err)
	}
	fn := t.Func(op.Target)
	if fn == nil {
		return false
	}
	name := fn.Name
	if fn.Recv != "" {
		name = fn.Recv + "." + fn.Name
	}
	res := a.glCall(ctx, "who", fn.File+":"+name)
	if res == nil {
		return false
	}
	a.handleNotes(ctx, res.Notes)
	if a.cur != tr || s.cfg.Overlap == "ignore" {
		return false
	}
	var data struct {
		Entries []struct {
			Item   string `json:"item"`
			Role   string `json:"role"`
			Lapsed bool   `json:"lapsed"`
		} `json:"entries"`
	}
	_ = res.data(&data)
	var others []string
	s.mu.Lock()
	for _, e := range data.Entries {
		if e.Role != "changes" || e.Lapsed || e.Item == tr.Item {
			continue
		}
		if id, ok := s.byItem[e.Item]; ok {
			others = append(others, id)
		}
	}
	s.mu.Unlock()
	wait := a.earlier(others)
	if len(wait) == 0 {
		return false
	}
	tr.deferred[i] = &deferral{waitFor: wait, since: s.round, reason: "who"}
	tr.Defers++
	s.event(a, "defer", map[string]any{"noteKind": "who", "ops": []string{op.Tag}, "waitFor": wait})
	return true
}
