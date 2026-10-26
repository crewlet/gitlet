package rules

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/gitlet/internal/model"
)

// predictedConflict: git merge-tree of the two heads conflicts in files both
// footprints change, lockfiles excluded. warn. Files where the pair already
// has a body_overlap note are left to that note.
func (c *ctx) predictedConflict(m *entry) {
	byOther := map[model.ItemID][]string{}
	for _, cf := range c.v.Conflicts {
		if cf.Mine != m.id() {
			continue
		}
		o := c.byItem[cf.Other]
		if o == nil || o == m {
			continue
		}
		for _, p := range cf.Paths {
			if p != "" && hotFile(p) != "lockfile" && !c.bodyOverlapIn(m, o, p) {
				byOther[o.id()] = append(byOther[o.id()], p)
			}
		}
	}
	for _, id := range sortedKeys(byOther) {
		o := c.byItem[id]
		if o.broad() {
			c.touchBroad(m, o)
			continue
		}
		paths := byOther[id]
		slices.Sort(paths)
		paths = slices.Compact(paths)
		n := c.newNote(model.NotePredictedConflict, m, pairSubject(m.id(), o.id()), paths[0])
		cps := []cp{{e: o, sev: lower(model.SeverityWarn, o), fact: strings.Join(paths, ",")}}
		withCps(&n, cps)
		n.Title = fmt.Sprintf("Your changes and %s are predicted to conflict in %s", label(o), moreFiles(paths))
		n.Body = "git merge-tree of both heads conflicts. Agree on who changes what, or plan to resolve it by hand."
		n.Evidence = []string{"conflicting files: " + capList(paths)}
		c.discussable(&n, m, cps, "agree on who changes what in these files")
		n.Options = append(n.Options, optDismiss(n.ID, "you will resolve the conflict when merging"))
		c.emit(n)
	}
}

// bodyOverlapIn reports whether m and o both edit a symbol in path with at
// least one side body only.
func (c *ctx) bodyOverlapIn(m, o *entry, path string) bool {
	for _, k := range m.keys {
		mc := m.changes[k]
		if oc, ok := o.changes[k]; ok && overlaps(mc, oc) && (mc.Symbol.Path == path || oc.Symbol.Path == path) {
			return true
		}
	}
	return false
}

// fileOverlap: both items change the same file that gitlet cannot parse,
// with no predicted conflict. info. Lockfiles and changelogs get hot_file.
func (c *ctx) fileOverlap(m *entry) {
	for _, p := range sortedKeys(m.files) {
		if hotFile(p) != "" {
			continue
		}
		mf := m.files[p]
		var cps []cp
		for _, o := range c.others(m) {
			of, ok := o.files[p]
			if !ok || (mf.Parsed && of.Parsed) || c.conflictIn(m, o, p) {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			cps = append(cps, cp{e: o, sev: lower(model.SeverityInfo, o)})
		}
		if len(cps) == 0 {
			continue
		}
		n := c.newNote(model.NoteFileOverlap, m, p, p)
		withCps(&n, cps)
		n.Title = fmt.Sprintf("%s also %s %s, which gitlet cannot parse", labels(cps), agree(cps, "changes", "change"), p)
		n.Body = "gitlet tracks this file only as a whole; check that your edits do not overlap."
		n.Evidence = []string{"file: " + p}
		n.Options = []model.Option{optDismiss(n.ID, "the edits do not overlap")}
		c.emit(n)
	}
}

// hotFiles: both items change a lockfile or changelog. info.
func (c *ctx) hotFiles(m *entry) {
	for _, p := range sortedKeys(m.files) {
		class := hotFile(p)
		if class == "" {
			continue
		}
		var cps []cp
		for _, o := range c.others(m) {
			if _, ok := o.files[p]; !ok {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			cps = append(cps, cp{e: o, sev: lower(model.SeverityInfo, o)})
		}
		if len(cps) == 0 {
			continue
		}
		n := c.newNote(model.NoteHotFile, m, p, p)
		withCps(&n, cps)
		n.Title = fmt.Sprintf("%s also %s the %s %s", labels(cps), agree(cps, "changes", "change"), class, p)
		if class == "lockfile" {
			n.Body = "Do not merge this lockfile by hand: regenerate it after you update."
		} else {
			n.Body = "Keep both changelog entries when you merge."
		}
		n.Evidence = []string{class + ": " + p}
		n.Options = []model.Option{optDismiss(n.ID, "nothing to coordinate")}
		c.emit(n)
	}
}

// migration is one migration file an item or main adds.
type migration struct {
	path, dir, seq string
}

// migrationsAdded returns the migrations a footprint's files add.
func (c *ctx) migrationsAdded(files []model.FileChange) []migration {
	var out []migration
	for _, f := range files {
		if !strings.HasPrefix(f.Status, "A") {
			continue
		}
		if dir, seq, ok := migrationSeq(f.Path, c.lim.Migrations); ok {
			out = append(out, migration{f.Path, dir, seq})
		}
	}
	slices.SortFunc(out, func(a, b migration) int { return cmp.Compare(a.path, b.path) })
	return out
}

// migrations: both items, or my item and main since my base, add a
// migration in the same directory with the same sequence number. act.
func (c *ctx) migrations(m *entry) {
	var onMain []migration
	if mv, ok := c.v.Main[m.id()]; ok {
		onMain = c.migrationsAdded(mv.Footprint.Files)
	}
	for _, mine := range c.migrationsAdded(m.item.Footprint.Files) {
		same := func(x migration) bool { return x.dir == mine.dir && sameSeq(x.seq, mine.seq) }
		var cps []cp
		for _, o := range c.others(m) {
			var paths []string
			for _, x := range c.migrationsAdded(o.item.Footprint.Files) {
				if same(x) {
					paths = append(paths, x.path)
				}
			}
			if len(paths) == 0 {
				continue
			}
			if o.broad() {
				c.touchBroad(m, o)
				continue
			}
			cps = append(cps, cp{e: o, sev: lower(model.SeverityAct, o), fact: strings.Join(paths, ","),
				ev: []string{fmt.Sprintf("%s adds %s", label(o), strings.Join(paths, ", "))}})
		}
		var mainPaths []string
		for _, x := range onMain {
			if same(x) {
				mainPaths = append(mainPaths, x.path)
			}
		}
		if len(cps) == 0 && len(mainPaths) == 0 {
			continue
		}
		n := c.newNote(model.NoteMigration, m, mine.path, mine.path)
		var extra []string
		for _, p := range mainPaths {
			extra = append(extra, "main:"+p)
		}
		withCps(&n, cps, extra...)
		if len(mainPaths) > 0 {
			n.Severity = model.SeverityAct
			n.Version = version(n.Severity, append(extra, cpFacts(cps)...)...)
			n.Evidence = append(n.Evidence, "main adds since your base: "+strings.Join(mainPaths, ", "))
		}
		n.Evidence = append([]string{fmt.Sprintf("you add %s (sequence %s in %s)", mine.path, mine.seq, mine.dir)}, n.Evidence...)
		if len(cps) > 0 {
			n.Title = fmt.Sprintf("Your migration %s uses sequence %s in %s, as %s %s", mine.path, mine.seq, mine.dir, agree(cps, "does", "do"), labels(cps))
			n.Body = "Two migrations with one sequence number break the migration order. Agree on who renumbers."
			c.discussable(&n, m, cps, "agree on who renumbers")
		} else {
			n.Title = fmt.Sprintf("Your migration %s uses sequence %s in %s, which main already has", mine.path, mine.seq, mine.dir)
			n.Body = "Main added a migration with the same sequence number since your base. Update and renumber yours."
			n.Options = append(n.Options, optUpdate(n.ID, "bring your branch up to date, then renumber your migration"))
		}
		n.Options = append(n.Options, optDismiss(n.ID, "the migrations do not collide"))
		c.emit(n)
	}
}

// cpFacts returns the version facts of counterparts, as withCps writes them.
func cpFacts(cps []cp) []string {
	var out []string
	for _, x := range cps {
		out = append(out, "item:"+string(x.e.id())+":"+x.fact)
	}
	return out
}

// duplicates: another item looks like the same task (docs/SPEC.md, section
// 10.2). act when an intent signal and a footprint signal agree, info for
// one. Provisional items use footprint signals only; broad footprints give
// no footprint signal. Pairs with a signature_clash or body_overlap
// relation get no duplicate note.
func (c *ctx) duplicates(m *entry) {
	var corpus []string
	for _, e := range c.entries {
		if !e.item.Provisional && strings.TrimSpace(e.item.Intent) != "" {
			corpus = append(corpus, e.item.Intent)
		}
	}
	for _, o := range c.others(m) {
		if c.symbolOverlap(m, o) {
			continue
		}
		var signals, ev []string
		intent := false
		if !m.item.Provisional && !o.item.Provisional {
			if score, shared := similarity(m.item.Intent, o.item.Intent, corpus); score >= 0.5 && shared >= 2 {
				intent = true
				signals = append(signals, "intent")
				ev = append(ev, fmt.Sprintf("similar intent (%.2f), shared terms: %s",
					score, strings.Join(sharedTerms(m.item.Intent, o.item.Intent), ", ")))
			}
		}
		footprint := false
		if !m.broad() && !o.broad() {
			for _, s := range footprintSignals(m, o) {
				footprint = true
				signals = append(signals, s.kind)
				ev = append(ev, s.text)
			}
		}
		if !intent && !footprint {
			continue
		}
		sev := model.SeverityInfo
		if intent && footprint {
			sev = model.SeverityAct
		}
		n := c.newNote(model.NoteDuplicate, m, pairSubject(m.id(), o.id()), "")
		cps := []cp{{e: o, sev: lower(sev, o), fact: strings.Join(signals, ","), ev: ev}}
		withCps(&n, cps)
		n.Title = fmt.Sprintf("%s looks like the same task", label(o))
		n.Body = "Agree on who does it. If theirs covers yours, abandon yours or start with a new scope."
		c.discussable(&n, m, cps, "agree on who does the task")
		n.Options = append(n.Options,
			option("stop your task; theirs covers it", "abandon"),
			optDismiss(n.ID, "the tasks are different"))
		c.emit(n)
	}
}

// signal is one footprint signal of a duplicate.
type signal struct{ kind, text string }

// footprintSignals returns the footprint signals between two items:
// changed-file Jaccard of at least 0.5 over at least 2 shared files, a
// symbol with the same qualified name added by both in one language family,
// or the same new file added by both.
func footprintSignals(m, o *entry) []signal {
	var out []signal
	mine, theirs := changedPaths(m), changedPaths(o)
	var shared []string
	for _, p := range sortedKeys(mine) {
		if theirs[p] {
			shared = append(shared, p)
		}
	}
	union := len(mine) + len(theirs) - len(shared)
	if len(shared) >= 2 && union > 0 && float64(len(shared))/float64(union) >= 0.5 {
		out = append(out, signal{"files", fmt.Sprintf("both change %d of %d files: %s",
			len(shared), union, capList(shared))})
	}
	addedSyms := func(e *entry) map[string]string {
		s := map[string]string{}
		for _, ch := range e.item.Footprint.Symbols {
			if ch.Change == model.ChangeAdded && ch.Symbol.Qualified != "" {
				s[model.LangFamily(ch.Symbol.Lang)+"\x00"+ch.Symbol.Qualified] = ch.Symbol.Qualified
			}
		}
		return s
	}
	ma, oa := addedSyms(m), addedSyms(o)
	var both []string
	for _, k := range sortedKeys(ma) {
		if _, ok := oa[k]; ok {
			both = append(both, ma[k])
		}
	}
	if len(both) > 0 {
		out = append(out, signal{"symbols", "both add " + capList(both)})
	}
	var newFiles []string
	for _, p := range sortedKeys(m.files) {
		mf, of := m.files[p], o.files[p]
		if mf.Path == p && of.Path == p && strings.HasPrefix(mf.Status, "A") && strings.HasPrefix(of.Status, "A") {
			newFiles = append(newFiles, p)
		}
	}
	if len(newFiles) > 0 {
		out = append(out, signal{"new files", "both add " + capList(newFiles)})
	}
	return out
}

// changedPaths returns the paths a footprint changes (new paths only).
func changedPaths(e *entry) map[string]bool {
	s := map[string]bool{}
	for _, f := range e.item.Footprint.Files {
		if f.Path != "" {
			s[f.Path] = true
		}
	}
	return s
}

// outsideBranches: a branch that is not a gitlet item changes a symbol I
// change, claim or need. warn.
func (c *ctx) outsideBranches(m *entry) {
	items := map[string]bool{}
	for _, e := range c.entries {
		items[e.item.Branch] = true
	}
	for _, it := range c.v.Mine.Items {
		items[it.Branch] = true
	}
	mineKeys := map[string]string{}
	for _, k := range m.keys {
		mineKeys[k] = "you change it"
	}
	for _, n := range c.myNeeds(m) {
		mineKeys[n.Key] = "you need it"
	}
	for _, k := range sortedKeys(c.v.Claims) {
		if cl := c.v.Claims[k].Claim; cl.Owner == c.v.Me && cl.Item == m.id() {
			mineKeys[k] = "you own it"
		}
	}
	outs := slices.Clone(c.v.Outside)
	slices.SortStableFunc(outs, func(a, b Outside) int { return cmp.Compare(a.Branch, b.Branch) })
	for i, ob := range outs {
		if ob.Branch == "" || items[ob.Branch] || (i > 0 && outs[i-1].Branch == ob.Branch) {
			continue
		}
		var facts, ev []string
		var first string
		for _, ch := range uniqueChanges(ob.Footprint.Symbols) {
			k := baseKey(ch)
			why, ok := mineKeys[k]
			if !ok {
				continue
			}
			ref := symRef(oldSymbol(ch), "")
			if first == "" {
				first = ref
			}
			facts = append(facts, "key:"+k)
			ev = append(ev, fmt.Sprintf("%s: %s on %s (%s)", ref, ch.Change, ob.Branch, why))
		}
		if len(facts) == 0 {
			continue
		}
		n := c.newNote(model.NoteOutsideBranch, m, "branch:"+ob.Branch, ob.Branch)
		n.Severity = model.SeverityWarn
		n.Parties = []model.Party{{Branch: ob.Branch}}
		what := first
		if len(facts) > 1 {
			what += fmt.Sprintf(" and %d more symbols", len(facts)-1)
		}
		n.Title = fmt.Sprintf("Branch %s, which is not a gitlet item, changes %s", ob.Branch, what)
		n.Body = "Claims do not bind work outside gitlet; check how that branch changes these symbols."
		n.Evidence = ev
		n.Options = []model.Option{optDismiss(n.ID, "the branch does not matter")}
		n.Version = version(n.Severity, facts...)
		c.emit(n)
	}
}

// broadNotes: another item is a broad change that touches my files, or that
// a footprint rule matched. warn.
func (c *ctx) broadNotes(m *entry) {
	for _, o := range c.others(m) {
		if !o.broad() {
			continue
		}
		var shared []string
		for _, p := range sortedKeys(m.files) {
			if _, ok := o.files[p]; ok {
				shared = append(shared, p)
			}
		}
		if len(shared) == 0 && !c.broadTouch[pairKey{m.id(), o.id()}] {
			continue
		}
		n := c.newNote(model.NoteBroad, m, string(o.id()), o.item.Branch)
		cps := []cp{{e: o, sev: lower(model.SeverityWarn, o)}}
		withCps(&n, cps)
		fp := o.item.Footprint
		n.Title = fmt.Sprintf("%s is a broad change that touches your work", label(o))
		n.Body = "It changes too much for symbol-level notes. Update often and check its changes before you merge."
		n.Evidence = []string{fmt.Sprintf("it changes %s and %s", plural(len(fp.Files), "file"), plural(len(fp.Symbols), "symbol"))}
		if len(shared) > 0 {
			n.Evidence = append(n.Evidence, "files you both change: "+capList(shared))
		}
		n.Options = []model.Option{optDismiss(n.ID, "it does not affect you")}
		c.emit(n)
	}
}

// capList joins names, capped at maxSites.
func capList(ss []string) string {
	if len(ss) > maxSites {
		return strings.Join(ss[:maxSites], ", ") + fmt.Sprintf(" and %d more", len(ss)-maxSites)
	}
	return strings.Join(ss, ", ")
}

// moreFiles names the first file and counts the rest.
func moreFiles(paths []string) string {
	if len(paths) == 1 {
		return paths[0]
	}
	if len(paths) == 2 {
		return paths[0] + " and 1 more file"
	}
	return fmt.Sprintf("%s and %d more files", paths[0], len(paths)-1)
}
