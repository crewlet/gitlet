package rules

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// maxSites caps the reference sites listed on one evidence line.
const maxSites = 10

// cp is one counterpart of a note: another item, the severity it brings,
// the facts about it that decide the note's version, and evidence.
type cp struct {
	e    *entry
	sev  model.Severity
	fact string
	ev   []string
}

// sortCps orders counterparts by severity, then agent and item, so the
// title names the most urgent one.
func sortCps(cps []cp) {
	slices.SortStableFunc(cps, func(a, b cp) int {
		return cmp.Or(cmp.Compare(a.sev.Rank(), b.sev.Rank()),
			cmp.Compare(a.e.agent, b.e.agent), cmp.Compare(a.e.id(), b.e.id()))
	})
}

// top returns the highest severity among counterparts (info when none).
func top(cps []cp) model.Severity {
	s := model.SeverityInfo
	for _, x := range cps {
		if x.sev.Rank() < s.Rank() {
			s = x.sev
		}
	}
	return s
}

// lower drops a severity one level for a lapsed, unsubmitted counterpart
// (docs/SPEC.md, section 8).
func lower(s model.Severity, e *entry) model.Severity {
	if e != nil && e.lapsed {
		return s.Lower()
	}
	return s
}

// newNote starts a note for my item m (nil for notes about no item).
func (c *ctx) newNote(kind model.NoteKind, m *entry, subject, subjectName string) model.Note {
	var item model.ItemID
	if m != nil {
		item = m.id()
	}
	return model.Note{
		ID:          model.NoteID(kind, subject, item),
		Kind:        kind,
		Item:        item,
		Subject:     subject,
		SubjectName: subjectName,
		CreatedAt:   c.v.Now,
	}
}

// withCps fills a note's severity, parties, version facts and evidence from
// its counterparts; extra facts join the version.
func withCps(n *model.Note, cps []cp, extra ...string) {
	sortCps(cps)
	n.Severity = top(cps)
	facts := extra
	for _, x := range cps {
		n.Parties = append(n.Parties, party(x.e))
		facts = append(facts, "item:"+string(x.e.id())+":"+x.fact)
		n.Evidence = append(n.Evidence, x.ev...)
	}
	n.Version = version(n.Severity, facts...)
}

// version hashes a note's severity and material facts, in sorted order so
// the hash does not depend on the order rules found them.
func version(sev model.Severity, facts ...string) string {
	fs := slices.Clone(facts)
	slices.Sort(fs)
	return model.NoteVersion(append([]string{string(sev)}, fs...)...)
}

func party(e *entry) model.Party {
	p := model.Party{Agent: e.agent, Item: e.id(), Intent: e.item.Intent, Branch: e.item.Branch, PR: e.item.PR}
	if e.lapsed {
		p.IdleSince = e.idle
	}
	return p
}

// label names an item in a title: its agent (or "your other task" for
// another item of mine) and its intent, with the idle time when its agent
// lapsed.
func label(e *entry) string {
	s := string(e.agent)
	if e.mine {
		s = "your other task"
	}
	if in := clip(e.item.Intent, 60); in != "" {
		s += ` ("` + in + `")`
	}
	if e.lapsed {
		if e.idle != nil {
			s += " (idle since " + e.idle.UTC().Format("2006-01-02 15:04 UTC") + ")"
		} else {
			s += " (idle)"
		}
	}
	return s
}

// labels names the first counterpart and counts the rest.
func labels(cps []cp) string {
	if len(cps) == 0 {
		return "another item"
	}
	s := label(cps[0].e)
	if len(cps) > 1 {
		s += fmt.Sprintf(" and %d more", len(cps)-1)
	}
	return s
}

// agree picks the verb form for a title's subject: the singular for one
// counterpart, the plural for "X and N more".
func agree(cps []cp, one, many string) string {
	if len(cps) > 1 {
		return many
	}
	return one
}

// plural counts a noun: "1 file", "2 files".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// who prefixes evidence lines when a note has several counterparts.
func who(e *entry, many bool) string {
	if !many {
		return ""
	}
	if e.mine {
		return "your other task"
	}
	return string(e.agent)
}

// sites lists reference sites as "path:line", deduplicated and capped.
func sites(refs []model.Ref) string {
	var ss []string
	for _, r := range refs {
		ss = append(ss, fmt.Sprintf("%s:%d", r.Path, r.Line))
	}
	slices.Sort(ss)
	ss = slices.Compact(ss)
	if len(ss) > maxSites {
		return strings.Join(ss[:maxSites], ", ") + fmt.Sprintf(" and %d more", len(ss)-maxSites)
	}
	return strings.Join(ss, ", ")
}

// usesVerb says how code refers to a symbol: "calls" when any ref is a call.
func usesVerb(refs []model.Ref) string {
	for _, r := range refs {
		if r.Kind == model.RefCall {
			return "calls"
		}
	}
	return "uses"
}

// option builds a tool call from name/value pairs. Free-text arguments the
// agent writes itself (message, intent, reason, requirement) are left out;
// renderers show them as placeholders.
func option(label, tool string, kv ...string) model.Option {
	o := model.Option{Label: label, Tool: tool}
	for i := 0; i+1 < len(kv); i += 2 {
		if o.Args == nil {
			o.Args = map[string]any{}
		}
		o.Args[kv[i]] = kv[i+1]
	}
	return o
}

func optNeed(note, symbol, label string) model.Option {
	return option(label, "need", "symbol", symbol, "note", note)
}

func optClaim(note, symbol, label string) model.Option {
	return option(label, "claim", "symbol", symbol, "note", note)
}

func optPublish(note, symbol, label string) model.Option {
	return option(label, "publish", "symbol", symbol, "note", note)
}

func optUse(note, symbol string) model.Option {
	return option("bring their published signature into your branch", "use", "symbol", symbol, "note", note)
}

func optRelease(note, symbol string) model.Option {
	return option("stop waiting and adapt to the current signature", "release", "symbol", symbol, "note", note)
}

func optUpdate(note, label string) model.Option {
	return option(label, "update", "note", note)
}

func optDiscuss(about, label string) model.Option {
	return option(label, "discuss", "about", about)
}

func optAccept(proposal, label string) model.Option {
	return option(label, "accept", "proposal", proposal)
}

func optDismiss(note, label string) model.Option {
	return option(label, "dismiss", "note", note, "as", "not_needed")
}

// discussThread returns the thread for discussing a note: an existing note
// thread about the same subject between my item and one of the
// counterparts (the other side may have opened it from its own note), or
// the thread derived from the note's kind, subject and items.
func (c *ctx) discussThread(kind model.NoteKind, subject string, m *entry, cps []cp) string {
	items := []model.ItemID{m.id()}
	for _, x := range cps {
		items = append(items, x.e.id())
	}
	for _, t := range c.threads {
		if t.st.Kind != model.ThreadNote || t.st.Subject != subject {
			continue
		}
		in := func(it model.ItemID) bool {
			return slices.ContainsFunc(t.st.Participants, func(p model.Participant) bool { return p.Item == it })
		}
		if in(m.id()) && slices.ContainsFunc(items[1:], in) {
			return t.st.ID
		}
	}
	return model.NoteThreadID(kind, subject, items...)
}

// discussable sets a note's thread and adds a discuss option about it.
func (c *ctx) discussable(n *model.Note, m *entry, cps []cp, label string) {
	n.Thread = c.discussThread(n.Kind, n.Subject, m, cps)
	n.Options = append(n.Options, optDiscuss(n.ID, label))
}

func (c *ctx) emit(n model.Note) { c.notes = append(c.notes, n) }

// touchBroad records that a footprint rule matched a broad item; it gets
// one broad note instead (docs/SPEC.md, section 10).
func (c *ctx) touchBroad(m, o *entry) { c.broadTouch[pairKey{m.id(), o.id()}] = true }

// sortedNotes returns the notes by severity, kind and id, one per id.
func (c *ctx) sortedNotes() []model.Note {
	ns := slices.Clone(c.notes)
	slices.SortStableFunc(ns, func(a, b model.Note) int {
		return cmp.Or(cmp.Compare(a.Severity.Rank(), b.Severity.Rank()),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	return slices.CompactFunc(ns, func(a, b model.Note) bool { return a.ID == b.ID })
}

// pairSubject names a pair of items the same way from both sides, so both
// derive one note thread for it.
func pairSubject(a, b model.ItemID) string {
	if b < a {
		a, b = b, a
	}
	return string(a) + "+" + string(b)
}

// duration formats a wait for titles: "16m", "2h05m".
func duration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// untitled: my work has no stated task.
func (c *ctx) untitled(m *entry) {
	if !m.item.Provisional && strings.TrimSpace(m.item.Intent) != "" {
		return
	}
	n := c.newNote(model.NoteUntitled, m, string(m.id()), m.item.Branch)
	n.Severity = model.SeverityAct
	where := m.item.Branch
	if where == "" {
		where = "this worktree"
	}
	n.Title = "Your work on " + where + " has no stated task"
	n.Body = "Call start with a one-sentence intent so other agents can see what you are doing and gitlet can find similar work."
	if in := clip(m.item.Intent, 80); in != "" {
		n.Evidence = []string{"provisional intent from the branch name: " + in}
	}
	n.Options = []model.Option{option("state your task", "start")}
	n.Version = version(n.Severity, "item:"+string(m.id()))
	c.emit(n)
}

// sameWorktree: another gitlet session already serves this worktree.
func (c *ctx) sameWorktree() {
	if !c.v.SameWorktree {
		return
	}
	n := c.newNote(model.NoteSameWorktree, nil, "worktree", "")
	n.Severity = model.SeverityAct
	n.Title = "Another gitlet session already serves this worktree; this one is read-only"
	n.Body = "Work in a separate git worktree so this session can publish its own work."
	cmd := c.v.WorktreeCommand
	if cmd == "" {
		cmd = "git worktree add ../<new-worktree> -b <branch>"
	}
	n.Options = []model.Option{{Label: "create a worktree of your own and work there", Command: cmd}}
	n.Version = version(n.Severity, "command:"+cmd)
	c.emit(n)
}
