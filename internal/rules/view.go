package rules

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// Defaults for zero Limits fields (docs/SPEC.md, section 15).
const (
	defaultThreadMessages = 3
	defaultThreadTimeout  = 10 * time.Minute
	defaultStaleItem      = 24 * time.Hour
	defaultPublishTimeout = 15 * time.Minute
)

// withDefaults fills zero bounds with their defaults. AutoClaim and
// Migrations are taken as given.
func (l Limits) withDefaults() Limits {
	if l.ThreadMessages <= 0 {
		l.ThreadMessages = defaultThreadMessages
	}
	if l.ThreadTimeout <= 0 {
		l.ThreadTimeout = defaultThreadTimeout
	}
	if l.StaleItem <= 0 {
		l.StaleItem = defaultStaleItem
	}
	if l.PublishTimeout <= 0 {
		l.PublishTimeout = defaultPublishTimeout
	}
	return l
}

// entry is one open work item in view: one of mine, or a peer's item that
// section 8 keeps visible.
type entry struct {
	agent  model.AgentID
	item   model.WorkItem
	mine   bool       // the item belongs to View.Me
	lapsed bool       // its agent lapsed and it is not submitted: notes drop one level
	idle   *time.Time // when its agent went idle, when lapsed

	changes map[string]model.SymbolChange // by base key (baseKey)
	keys    []string                      // sorted keys of changes
	files   map[string]model.FileChange   // by path, old paths of renames included
}

func (e *entry) id() model.ItemID { return e.item.ID }

// broad reports whether the item's footprint is too large for symbol-level
// notes; rules emit one broad note for it instead.
func (e *entry) broad() bool { return e.item.Footprint.Broad }

// pairKey names one of my items and another item.
type pairKey struct{ mine, other model.ItemID }

// ctx is one evaluation of a view: the entries, indexes and threads that
// every rule shares, and the notes produced so far. Building it does all
// the sorting, so rules iterate slices and sorted keys only.
type ctx struct {
	v   View
	lim Limits

	entries []*entry // every open item in view, sorted by agent, then item
	mine    []*entry // my open items, sorted by item
	byItem  map[model.ItemID]*entry
	names   map[string]map[string]bool // language family + "\x00" + name -> keys of changed symbols

	contests map[string]*contest // by symbol key: items whose signature changes meet there
	sig      map[string]*thread  // current signature threads by symbol key
	prev     map[string]*thread  // agreements of the epoch before a transfer, by symbol key
	threads  []*thread           // threads View.Me takes part in, sorted by id
	msgs     map[string][]model.Message
	accepts  map[string][]accept

	notes      []model.Note
	broadTouch map[pairKey]bool // broad items a footprint rule matched
}

func newCtx(v View) *ctx {
	c := &ctx{
		v:          v,
		lim:        v.Limits.withDefaults(),
		byItem:     map[model.ItemID]*entry{},
		broadTouch: map[pairKey]bool{},
	}
	for _, it := range v.Mine.Items {
		c.add(v.Me, it, Peer{})
	}
	for _, id := range sortedKeys(v.Peers) {
		if id == v.Me {
			continue // my own state comes from v.Mine
		}
		p := v.Peers[id]
		for _, it := range p.State.Items {
			c.add(id, it, p)
		}
	}
	slices.SortFunc(c.entries, func(a, b *entry) int {
		return cmp.Or(cmp.Compare(a.agent, b.agent), cmp.Compare(a.item.ID, b.item.ID))
	})
	for _, e := range c.entries {
		if e.mine {
			c.mine = append(c.mine, e)
		}
	}
	c.indexNames()
	c.loadOutboxes()
	c.buildContests()
	c.buildThreads()
	return c
}

// add puts an item in view when section 8 says it takes part in the rules:
// open, and not an unsubmitted item of an agent lapsed for longer than
// StaleItem. p is the zero Peer for my own items.
func (c *ctx) add(agent model.AgentID, it model.WorkItem, p Peer) {
	if it.ID == "" || !it.Status.Open() || c.byItem[it.ID] != nil {
		return
	}
	e := &entry{agent: agent, item: it, mine: agent == c.v.Me}
	if p.Lapsed && it.Status != model.ItemSubmitted {
		if c.staleSince(p) {
			return
		}
		e.lapsed, e.idle = true, p.IdleSince
	}
	e.changes = map[string]model.SymbolChange{}
	for _, ch := range it.Footprint.Symbols {
		k := baseKey(ch)
		if k == "" {
			continue
		}
		if _, dup := e.changes[k]; !dup {
			e.changes[k] = ch
		}
	}
	e.keys = sortedKeys(e.changes)
	e.files = map[string]model.FileChange{}
	for _, f := range it.Footprint.Files {
		for _, fp := range []string{f.Path, f.OldPath} {
			if _, dup := e.files[fp]; fp != "" && !dup {
				e.files[fp] = f
			}
		}
	}
	c.entries = append(c.entries, e)
	c.byItem[it.ID] = e
}

// staleSince reports whether a lapsed peer has been lapsed for longer than
// StaleItem: it went idle at IdleSince and lapsed one lease later.
func (c *ctx) staleSince(p Peer) bool {
	if p.IdleSince == nil {
		return false
	}
	lapsedAt := p.IdleSince.Add(time.Duration(p.State.LeaseSeconds) * time.Second)
	return c.v.Now.Sub(lapsedAt) > c.lim.StaleItem
}

// indexNames records the names of every changed symbol in view (items and
// main), so an unresolved ref can match a method only when its name is
// unique (docs/SPEC.md, section 9.6).
func (c *ctx) indexNames() {
	c.names = map[string]map[string]bool{}
	add := func(ch model.SymbolChange) {
		for _, s := range []model.Symbol{oldSymbol(ch), ch.Symbol} {
			if s.Name == "" {
				continue
			}
			n := model.LangFamily(s.Lang) + "\x00" + s.Name
			if c.names[n] == nil {
				c.names[n] = map[string]bool{}
			}
			c.names[n][keyOf(s)] = true
		}
	}
	for _, e := range c.entries {
		for _, ch := range e.item.Footprint.Symbols {
			add(ch)
		}
	}
	for _, id := range sortedKeys(c.v.Main) {
		if mv := c.v.Main[id]; !mv.Skipped {
			for _, ch := range mv.Footprint.Symbols {
				add(ch)
			}
		}
	}
}

// uniqueName reports whether s's name belongs to one changed symbol only.
func (c *ctx) uniqueName(s model.Symbol) bool {
	return len(c.names[model.LangFamily(s.Lang)+"\x00"+s.Name]) <= 1
}

// matching returns the refs that refer to sym, and whether any of them
// matched through a resolved module rather than a unique method name.
func (c *ctx) matching(refs []model.Ref, sym model.Symbol) (out []model.Ref, resolved bool) {
	unique := c.uniqueName(sym)
	for _, r := range refs {
		if ok, res := matchRef(r, sym, unique); ok {
			out = append(out, r)
			resolved = resolved || res
		}
	}
	return out, resolved
}

// others returns every entry except m: rules skip my own item, not my agent.
func (c *ctx) others(m *entry) []*entry {
	out := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		if e != m {
			out = append(out, e)
		}
	}
	return out
}

// needsOf returns an agent's needs from its outbox.
func (c *ctx) needsOf(agent model.AgentID) []model.Need {
	if agent == c.v.Me {
		return c.v.MyOutbox.Needs
	}
	return c.v.Peers[agent].Outbox.Needs
}

// hasNeed reports whether e's agent needs key for e (or for no item in
// particular).
func (c *ctx) hasNeed(e *entry, key string) bool {
	for _, n := range c.needsOf(e.agent) {
		if n.Key == key && (n.Item == e.id() || n.Item == "") {
			return true
		}
	}
	return false
}

// hasMyNeed reports whether I need key for any item.
func (c *ctx) hasMyNeed(key string) bool {
	for _, n := range c.v.MyOutbox.Needs {
		if n.Key == key {
			return true
		}
	}
	return false
}

// myNeeds returns my needs that belong to m, sorted by key. A need with no
// item belongs to my first item.
func (c *ctx) myNeeds(m *entry) []model.Need {
	var out []model.Need
	for _, n := range c.v.MyOutbox.Needs {
		if n.Key == "" {
			continue
		}
		if n.Item == m.id() || (n.Item == "" && len(c.mine) > 0 && c.mine[0] == m) {
			out = append(out, n)
		}
	}
	slices.SortStableFunc(out, func(a, b model.Need) int {
		return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// needEntry returns the entry a need belongs to: its item when that is in
// view and belongs to agent, or the agent's first item for a need with no
// item.
func (c *ctx) needEntry(agent model.AgentID, n model.Need) *entry {
	if n.Item != "" {
		if e := c.byItem[n.Item]; e != nil && e.agent == agent {
			return e
		}
		return nil
	}
	for _, e := range c.entries {
		if e.agent == agent {
			return e
		}
	}
	return nil
}

// needers returns the entries other than m that need key, with their needs,
// sorted by agent and item.
func (c *ctx) needers(key string, m *entry) []neederNeed {
	var out []neederNeed
	agents := append([]model.AgentID{c.v.Me}, sortedKeys(c.v.Peers)...)
	for i, a := range agents {
		if i > 0 && a == c.v.Me {
			continue
		}
		for _, n := range c.needsOf(a) {
			if n.Key != key {
				continue
			}
			if e := c.needEntry(a, n); e != nil && e != m && !slices.ContainsFunc(out, func(x neederNeed) bool { return x.e == e }) {
				out = append(out, neederNeed{e, n})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b neederNeed) int {
		return cmp.Or(cmp.Compare(a.e.agent, b.e.agent), cmp.Compare(a.e.id(), b.e.id()))
	})
	return out
}

// neederNeed is an item that needs a symbol, with its need.
type neederNeed struct {
	e    *entry
	need model.Need
}

// claim returns the claim on key, if any.
func (c *ctx) claim(key string) (ClaimView, bool) {
	cv, ok := c.v.Claims[key]
	return cv, ok
}

// claimSymbol returns the best known symbol for key: the claim's, then a
// changed symbol in view, then a bare symbol named by fallback.
func (c *ctx) claimSymbol(key, fallback string) model.Symbol {
	if cv, ok := c.claim(key); ok && cv.Claim.Symbol.Qualified != "" {
		s := cv.Claim.Symbol
		s.Key = key
		return s
	}
	if ct := c.contests[key]; ct != nil {
		return ct.sym
	}
	for _, e := range c.entries {
		if ch, ok := e.changes[key]; ok {
			return oldSymbol(ch)
		}
	}
	s := model.Symbol{Key: key}
	if p, q, ok := model.SplitTarget(fallback); ok {
		s.Path, s.Qualified = p, q
		s.Container, s.Name = splitQualified(q)
	}
	return s
}

// agentOf returns the agent that owns an item, open or not.
func (c *ctx) agentOf(item model.ItemID) (model.AgentID, bool) {
	if e := c.byItem[item]; e != nil {
		return e.agent, true
	}
	for _, it := range c.v.Mine.Items {
		if it.ID == item {
			return c.v.Me, true
		}
	}
	for _, id := range sortedKeys(c.v.Peers) {
		for _, it := range c.v.Peers[id].State.Items {
			if it.ID == item {
				return id, true
			}
		}
	}
	return "", false
}

// symRef returns the canonical "path:Qualified" of s, or fallback when s is
// incomplete.
func symRef(s model.Symbol, fallback string) string {
	if s.Path != "" && s.Qualified != "" {
		return s.Ref()
	}
	if fallback != "" {
		return fallback
	}
	if s.Qualified != "" {
		return s.Qualified
	}
	return s.Key
}

// sortedKeys returns a map's keys in order, so no rule depends on map
// iteration order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}

// clip collapses whitespace and cuts s to n runes, so text from other agents
// stays on one short line in titles.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}
