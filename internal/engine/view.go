package engine

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/bus"
	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
)

const (
	// maxConflictRuns caps merge-tree runs per evaluation (docs/SPEC.md,
	// section 10); cached pairs do not count.
	maxConflictRuns = 20
	// verbEvalBudget bounds the costly parts of a verb's evaluation.
	verbEvalBudget = 1500 * time.Millisecond
	// tickEvalBudget bounds them in Tick.
	tickEvalBudget = 4 * time.Second
	// clashMemory is how long a clash that went away keeps its first-seen time.
	clashMemory = 7 * 24 * time.Hour
	// maxCacheEntries bounds every in-memory memo map.
	maxCacheEntries = 10000
)

// resetCaches (re)creates the in-memory caches.
func (e *Engine) resetCaches() {
	e.commits = map[string]bool{}
	e.contracts = map[string]bool{}
	e.mainViews = map[string]rules.MainView{}
	e.prevMain = map[model.ItemID]rules.MainView{}
	e.prevConflicts = map[string]rules.Conflict{}
	e.landChecks = map[string]bool{}
	e.ghChecked = map[model.ItemID]time.Time{}
}

// trimCaches clears memo maps that grew too large over a long session.
func (e *Engine) trimCaches() {
	if len(e.commits) > maxCacheEntries {
		e.commits = map[string]bool{}
	}
	if len(e.contracts) > maxCacheEntries {
		e.contracts = map[string]bool{}
	}
	if len(e.mainViews) > maxCacheEntries {
		e.mainViews = map[string]rules.MainView{}
	}
	if len(e.landChecks) > maxCacheEntries {
		e.landChecks = map[string]bool{}
	}
}

// lightView is the view without its costly parts (predicted conflicts,
// main, outside branches, contract landing): enough for threads, peers and
// claims. Never sync notes from it, since the notes those parts produce
// would resolve. Callers hold e.mu.
func (e *Engine) lightView(ctx context.Context) rules.View {
	return e.buildView(ctx, 0, false)
}

// fullView is the view the rules evaluate. Its costly parts run within
// budget, and whatever the budget does not reach keeps its last known
// value, so notes do not flicker. Callers hold e.mu.
func (e *Engine) fullView(ctx context.Context, budget time.Duration) rules.View {
	return e.buildView(ctx, budget, true)
}

func (e *Engine) buildView(ctx context.Context, budget time.Duration, full bool) rules.View {
	e.ensureShared()
	now := e.now()
	mine := e.myState()
	v := rules.View{
		Me:              e.me.ID,
		Mine:            mine,
		MyOutbox:        e.myOutbox(),
		Peers:           e.peers(ctx, now),
		ClashSeen:       e.clashSeen(ctx),
		SameWorktree:    e.sameWorktree,
		WorktreeCommand: e.worktreeCmd,
		Limits:          e.limits(),
		Now:             now,
	}
	if !full {
		v.Claims = e.claimViews(ctx, now, false)
		return v
	}
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	v.Claims = e.claimViews(bctx, now, true)
	v.Main = e.mainViewsFor(bctx, mine)
	v.Conflicts = e.predictConflicts(bctx, mine, v.Peers)
	v.Outside = slices.Clone(e.outside)
	return v
}

func (e *Engine) limits() rules.Limits {
	return rules.Limits{
		ThreadMessages: e.cfg.ThreadMessages,
		ThreadTimeout:  e.cfg.ThreadTimeout,
		StaleItem:      e.cfg.StaleItem,
		PublishTimeout: e.cfg.PublishTimeout,
		AutoClaim:      e.cfg.AutoClaim,
		Migrations:     e.cfg.Migrations,
	}
}

// peers returns every other agent with its lapse judgement and the items
// in view (docs/SPEC.md, section 8): open items only, and no unsubmitted
// items of agents lapsed for longer than gitlet.staleItem.
func (e *Engine) peers(ctx context.Context, now time.Time) map[model.AgentID]rules.Peer {
	out := map[model.AgentID]rules.Peer{}
	for _, id := range e.peerIDs() {
		st, hasState := e.shared.Agents[id]
		var lp lapse
		if hasState {
			lp = e.refLapse(ctx, bus.StateRef(id), st.LeaseSeconds, now)
		} else {
			lp = e.refLapse(ctx, bus.OutboxRef(id), 0, now)
		}
		p := rules.Peer{State: st, Outbox: e.shared.Outboxes[id], Lapsed: lp.Lapsed}
		if lp.Lapsed {
			since := lp.Since
			p.IdleSince = &since
		}
		p.State.Items = visibleItems(st.Items, lp, e.cfg.StaleItem)
		out[id] = p
	}
	return out
}

// visibleItems returns the items of a peer the rules may see: open items,
// except unsubmitted items of an agent lapsed for longer than staleItem.
func visibleItems(items []model.WorkItem, lp lapse, staleItem time.Duration) []model.WorkItem {
	var out []model.WorkItem
	for _, it := range items {
		if !it.Status.Open() {
			continue
		}
		if it.Status != model.ItemSubmitted && lp.Lapsed && lp.For > staleItem {
			continue
		}
		out = append(out, it)
	}
	return out
}

// claimViews returns every claim with its lapse judgement and, when
// withLanded, whether its contract has landed on main.
func (e *Engine) claimViews(ctx context.Context, now time.Time, withLanded bool) map[string]rules.ClaimView {
	out := make(map[string]rules.ClaimView, len(e.shared.Claims))
	main := ""
	if withLanded {
		main = e.mainOID(ctx)
	}
	for key, cv := range e.shared.Claims {
		lp := e.refLapse(ctx, bus.ClaimRef(key), cv.Claim.LeaseSeconds, now)
		rv := rules.ClaimView{Claim: cv.Claim, OID: cv.OID, Lapsed: lp.Lapsed}
		if lp.Lapsed {
			since := lp.Since
			rv.IdleSince = &since
		}
		if withLanded {
			rv.Landed = e.contractLanded(ctx, cv.Claim, main)
		}
		out[key] = rv
	}
	return out
}

// contractLanded reports whether a claim's contract has landed on main
// (docs/SPEC.md, section 7): main's version of the symbol, parsed with the
// same extractor, has the contract's signature hash, or the stub commit is
// an ancestor of main. Results are memoized by key, contract and main;
// past the budget the last answer for the contract stands.
func (e *Engine) contractLanded(ctx context.Context, c model.Claim, main string) bool {
	if c.Contract == nil || main == "" {
		return false
	}
	contract := c.Key + "\x00" + c.Contract.SigHash + "\x00" + c.Contract.StubCommit
	k := contract + "\x00" + main
	if v, ok := e.contracts[k]; ok {
		return v
	}
	if ctx.Err() != nil {
		return e.contracts[contract]
	}
	landed := false
	if stub := c.Contract.StubCommit; stub != "" && e.hasCommit(ctx, stub) {
		if ok, err := e.bg.IsAncestor(ctx, stub, main); err == nil && ok {
			landed = true
		}
	}
	if !landed && c.Symbol.Path != "" {
		if sym, ok := e.symbolAt(ctx, main, c.Symbol.Path, c.Symbol.Qualified); ok && sym.SigHash == c.Contract.SigHash {
			landed = true
		}
	}
	if ctx.Err() != nil {
		return e.contracts[contract]
	}
	e.contracts[k], e.contracts[contract] = landed, landed
	return landed
}

// mainViewsFor returns main seen from each of my open items: the footprint
// of main against the item's base, computed once per (base, main) and
// skipped (symbol-level checks off) when main changed more than
// mainParseLimit parseable files since the base. An item the budget did not
// reach keeps its previous view.
func (e *Engine) mainViewsFor(ctx context.Context, mine model.AgentState) map[model.ItemID]rules.MainView {
	out := map[model.ItemID]rules.MainView{}
	main := e.mainOID(ctx)
	if main == "" {
		return out
	}
	for _, it := range mine.Items {
		if !it.Status.Open() || it.Base == "" {
			continue
		}
		mv, ok := e.mainView(ctx, it.Base, main)
		if !ok {
			mv, ok = e.prevMain[it.ID]
		}
		if ok {
			out[it.ID] = mv
			e.prevMain[it.ID] = mv
		}
	}
	return out
}

func (e *Engine) mainView(ctx context.Context, base, main string) (rules.MainView, bool) {
	k := base + ".." + main
	if mv, ok := e.mainViews[k]; ok {
		return mv, true
	}
	if base == main {
		mv := rules.MainView{Main: main, Footprint: model.Footprint{Base: base, Head: main}}
		e.mainViews[k] = mv
		return mv, true
	}
	if ctx.Err() != nil {
		return rules.MainView{}, false
	}
	fp, err := e.cachedFootprint(ctx, "main:", base, main, mainParseLimit)
	if err != nil {
		e.log.Debug("main footprint", "base", base, "main", main, "err", err)
		return rules.MainView{}, false
	}
	mv := rules.MainView{Main: main, Footprint: fp, Skipped: parseableFiles(fp) > mainParseLimit}
	e.mainViews[k] = mv
	return mv, true
}

// predictConflicts predicts textual conflicts between each of my open items
// and every other item in view whose changed files intersect (docs/SPEC.md,
// section 10): git merge-tree of both heads, cached by the pair of heads in
// the ledger, at most maxConflictRuns runs per evaluation, restricted to
// files both change with lockfiles excluded. A peer's snapshot is fetched on
// demand (bus.FetchWip). A pair the budget or the network did not reach
// keeps its last known result. Disabled before git 2.38.
func (e *Engine) predictConflicts(ctx context.Context, mine model.AgentState, peers map[model.AgentID]rules.Peer) []rules.Conflict {
	if !e.gitOK {
		return nil
	}
	type other struct {
		agent model.AgentID
		item  model.WorkItem
	}
	var others []other
	for _, id := range sortedKeys(peers) {
		for _, it := range peers[id].State.Items {
			others = append(others, other{id, it})
		}
	}
	for _, it := range mine.Items {
		if it.Status.Open() {
			others = append(others, other{e.me.ID, it})
		}
	}
	var out []rules.Conflict
	next := map[string]rules.Conflict{}
	runs := 0
	for _, my := range mine.Items {
		head := itemHead(my)
		if !my.Status.Open() || head == "" {
			continue
		}
		myFiles := changedFiles(my.Footprint)
		for _, o := range others {
			if o.item.ID == my.ID {
				continue
			}
			shared := sharedFiles(myFiles, changedFiles(o.item.Footprint))
			if len(shared) == 0 {
				continue
			}
			pair := string(my.ID) + "/" + string(o.item.ID)
			paths, ok := e.mergeConflicts(ctx, head, e.otherHead(ctx, o.agent, o.item), &runs)
			c := rules.Conflict{Mine: my.ID, Other: o.item.ID, Paths: intersectSorted(paths, shared)}
			if !ok {
				if c, ok = e.prevConflicts[pair]; !ok {
					continue
				}
			}
			next[pair] = c
			if len(c.Paths) > 0 {
				out = append(out, c)
			}
		}
	}
	e.prevConflicts = next
	return out
}

// otherHead returns the commit to merge for another item: the branch head
// of a submitted item when present locally, else its snapshot, fetched on
// demand. "" when unavailable.
func (e *Engine) otherHead(ctx context.Context, agent model.AgentID, it model.WorkItem) string {
	if agent == e.me.ID {
		return itemHead(it)
	}
	if it.Status == model.ItemSubmitted && e.hasCommit(ctx, it.Head) {
		return it.Head
	}
	want := it.Snapshot
	if want == "" {
		return ""
	}
	if e.hasCommit(ctx, want) {
		return want
	}
	if ctx.Err() != nil {
		return ""
	}
	oid, err := e.bus.FetchWip(ctx, agent, it.ID, want)
	if err != nil {
		if gitx.IsNetwork(err) {
			e.poll.markOffline(e.now())
		}
		return ""
	}
	return oid
}

// mergeConflicts returns the conflicted paths of merging a and b, from the
// ledger's cache or one merge-tree run counted in runs. ok is false when
// the pair could not be evaluated this time.
func (e *Engine) mergeConflicts(ctx context.Context, a, b string, runs *int) (paths []string, ok bool) {
	if a == "" || b == "" {
		return nil, false
	}
	if m, ok, err := e.led.CachedMerge(ctx, a, b); err == nil && ok {
		return m.Conflicts, true
	}
	if *runs >= maxConflictRuns || ctx.Err() != nil {
		return nil, false
	}
	*runs++
	res, err := e.bg.MergeTree(ctx, a, b)
	if err != nil {
		e.log.Debug("merge-tree", "a", a, "b", b, "err", err)
		return nil, false
	}
	if !e.led.ReadOnly() {
		_ = e.led.CacheMerge(ctx, a, b, ledger.MergeOutcome{Clean: res.Clean, Conflicts: res.Conflicts})
	}
	return res.Conflicts, true
}

// itemHead is the commit an item's footprint describes: the latest snapshot
// of an active item, the branch head of a submitted one.
func itemHead(it model.WorkItem) string {
	if it.Status == model.ItemActive && it.Snapshot != "" {
		return it.Snapshot
	}
	return it.Head
}

// changedFiles returns the paths a footprint changes, renamed-from paths included.
func changedFiles(fp model.Footprint) map[string]bool {
	out := make(map[string]bool, len(fp.Files))
	for _, f := range fp.Files {
		out[f.Path] = true
		if f.OldPath != "" {
			out[f.OldPath] = true
		}
	}
	return out
}

// sharedFiles returns the sorted paths in both sets, lockfiles excluded.
func sharedFiles(a, b map[string]bool) []string {
	var out []string
	for p := range a {
		if b[p] && rules.HotFile(p) != "lockfile" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// intersectSorted returns the elements of a that are in the sorted list b, sorted.
func intersectSorted(a, b []string) []string {
	var out []string
	for _, p := range a {
		if i := sort.SearchStrings(b, p); i < len(b) && b[i] == p {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func sortedKeys[V any](m map[model.AgentID]V) []model.AgentID {
	keys := make([]model.AgentID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (e *Engine) clashKey() string { return "clashseen/" + string(e.me.ID) }

// clashSeen returns when this server first saw each signature thread's
// clash, persisted in the ledger so thread deadlines survive restarts.
func (e *Engine) clashSeen(ctx context.Context) map[string]time.Time {
	out := map[string]time.Time{}
	if v, ok, err := e.led.Get(ctx, e.clashKey()); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &out)
	}
	return out
}

// recordClashes stores when each signature thread of rules.Threads first
// appeared, and forgets threads gone for clashMemory.
func (e *Engine) recordClashes(ctx context.Context, v rules.View) {
	seen := make(map[string]time.Time, len(v.ClashSeen))
	for k, t := range v.ClashSeen {
		seen[k] = t
	}
	current := map[string]bool{}
	for _, t := range rules.Threads(v) {
		if t.Kind == model.ThreadSignature {
			current[t.ID] = true
		}
	}
	changed := false
	for id := range current {
		if _, ok := seen[id]; !ok {
			seen[id], changed = v.Now, true
		}
	}
	for id, at := range seen {
		if !current[id] && v.Now.Sub(at) > clashMemory {
			delete(seen, id)
			changed = true
		}
	}
	if !changed || e.led.ReadOnly() {
		return
	}
	b, err := json.Marshal(seen)
	if err == nil {
		err = e.led.Set(ctx, e.clashKey(), string(b))
	}
	if err != nil {
		e.log.Debug("record clashes", "err", err)
	}
}

// ---- lookups the verbs share ----

// claimFor returns the claim on a symbol key as last loaded (or written by
// this engine).
func (e *Engine) claimFor(key string) (bus.ClaimView, bool) {
	if e.shared == nil {
		return bus.ClaimView{}, false
	}
	cv, ok := e.shared.Claims[key]
	return cv, ok
}

// myClaims returns the claims this agent holds, sorted by symbol.
func (e *Engine) myClaims() []bus.ClaimView {
	var out []bus.ClaimView
	if e.shared == nil {
		return out
	}
	for _, cv := range e.shared.Claims {
		if cv.Claim.Owner == e.me.ID {
			out = append(out, cv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Claim.Symbol.Ref() < out[j].Claim.Symbol.Ref() })
	return out
}

// agentNeed is a need with the agent that stated it.
type agentNeed struct {
	Agent model.AgentID
	Need  model.Need
}

// needsOn returns every need on a symbol key across all outboxes, mine
// included, sorted by agent.
func (e *Engine) needsOn(key string) []agentNeed {
	var out []agentNeed
	if e.shared == nil {
		return out
	}
	for id, ob := range e.shared.Outboxes {
		for _, n := range ob.Needs {
			if n.Key == key {
				out = append(out, agentNeed{id, n})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Need.ID < out[j].Need.ID
	})
	return out
}

// myNeed returns my need on a symbol key, if any.
func (e *Engine) myNeed(key string) (model.Need, bool) {
	for _, n := range e.myOutbox().Needs {
		if n.Key == key {
			return n, true
		}
	}
	return model.Need{}, false
}

// waitingOn returns the agents that need a symbol key, sorted, without duplicates.
func (e *Engine) waitingOn(key string) []model.AgentID {
	var out []model.AgentID
	for _, n := range e.needsOn(key) {
		if len(out) == 0 || out[len(out)-1] != n.Agent {
			out = append(out, n.Agent)
		}
	}
	return out
}

// claimInfo describes a claim for results: canonical symbol, owner, whether
// it is mine, lapse, epoch, contract, requirements and the agents waiting
// for it.
func (e *Engine) claimInfo(ctx context.Context, cv bus.ClaimView, now time.Time) api.ClaimInfo {
	c := cv.Claim
	lp := e.refLapse(ctx, bus.ClaimRef(c.Key), c.LeaseSeconds, now)
	return api.ClaimInfo{
		Symbol:       c.Symbol.Ref(),
		Owner:        c.Owner,
		Mine:         c.Owner == e.me.ID,
		Auto:         c.Auto,
		Lapsed:       lp.Lapsed,
		Epoch:        c.Epoch,
		Contract:     c.Contract,
		Requirements: c.Requirements,
		Waiting:      e.waitingOn(c.Key),
	}
}

// findItem looks an item up in every agent's state (mine included).
func (e *Engine) findItem(id model.ItemID) (model.AgentID, model.WorkItem, bool) {
	if e.shared == nil {
		return "", model.WorkItem{}, false
	}
	for _, agent := range append([]model.AgentID{e.me.ID}, e.peerIDs()...) {
		for _, it := range e.shared.Agents[agent].Items {
			if it.ID == id {
				return agent, it, true
			}
		}
	}
	return "", model.WorkItem{}, false
}

// myChange returns the change one of my open items makes to a symbol key
// (the active item first).
func (e *Engine) myChange(key string) (model.WorkItem, model.SymbolChange, bool) {
	items := e.myState().Items
	order := make([]model.WorkItem, 0, len(items))
	if i := activeIndex(items); i >= 0 {
		order = append(order, items[i])
	}
	for _, it := range items {
		if it.Status == model.ItemSubmitted {
			order = append(order, it)
		}
	}
	for _, it := range order {
		for _, sc := range it.Footprint.Symbols {
			if sc.Symbol.Key == key {
				return it, sc, true
			}
		}
	}
	return model.WorkItem{}, model.SymbolChange{}, false
}
