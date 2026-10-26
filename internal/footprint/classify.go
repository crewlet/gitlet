package footprint

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/gitlet/internal/model"
)

// classify compares the structured parts of two signatures (docs/SPEC.md,
// section 9.4). A change is compatible only when every difference is an
// allowed one: the structured fields (Params, Results, Members) may differ
// only in allowed ways, and the rest of the signature, the Head (type
// parameters, async, receivers, method markers, bases), only in visibility.
// When the signature hashes differ but nothing structured explains it
// (overload groups, a renamed receiver type), the change is breaking.
func classify(old, new model.Symbol) bool {
	if old.SigHash == new.SigHash {
		return false
	}
	if old.Kind != new.Kind || old.Name != new.Name || old.Container != new.Container ||
		model.LangFamily(old.Lang) != model.LangFamily(new.Lang) {
		return true
	}
	c := newComparison(new.Lang, new.Kind)
	oldVis, newVis := c.visibility(old), c.visibility(new)
	if (old.Exported && !new.Exported) || newVis < oldVis {
		return true // visibility narrowed
	}

	resultsOK, resultsChanged := c.results(old.Results, new.Results)
	paramsOK, paramsChanged := c.params(old.Params, new.Params)
	membersOK, membersChanged := c.members(old.Members, new.Members)
	if !resultsOK || !paramsOK || !membersOK {
		return true
	}
	if stripVisibility(head(old)) != stripVisibility(head(new)) {
		return true // async, a receiver, a constraint or a base changed too
	}
	if resultsChanged || paramsChanged || membersChanged {
		return false
	}
	// Nothing structured explains the new hash. Wider visibility may, but
	// only when the signatures differ in nothing else.
	if widened := (!old.Exported && new.Exported) || newVis > oldVis; widened {
		return stripVisibility(old.Signature) != stripVisibility(new.Signature)
	}
	return true
}

// head returns the part of s's signature the structured fields leave out.
// Without a Head, as from an extractor that sets none, that is the whole
// signature, so no change to the structured parts can pass for compatible.
func head(s model.Symbol) string { return cmp.Or(s.Head, s.Signature) }

// visibility ranks the visibility a symbol's signature shows, from 0 (most
// private), where the language writes it there: Rust's pub forms, and
// TypeScript's export and private or protected members. Go and Python say it
// with the name, so they always rank 0.
func (c comparison) visibility(s model.Symbol) int {
	sig := s.Signature
	switch c.family {
	case "rust":
		switch {
		case sig == "pub" || strings.HasPrefix(sig, "pub "):
			return 4
		case strings.HasPrefix(sig, "pub(crate)"):
			return 3
		case strings.HasPrefix(sig, "pub(super)"), strings.HasPrefix(sig, "pub(in "):
			return 2
		}
	case "js":
		switch {
		case tsPrivate.MatchString(sig):
			return 0
		case tsProtected.MatchString(sig):
			return 1
		case s.Container != "" || tsExport.MatchString(sig):
			return 2 // a public member, or an exported declaration
		}
	}
	return 0
}

var (
	tsPrivate   = regexp.MustCompile(`(^|\s)private\s`)
	tsProtected = regexp.MustCompile(`(^|\s)protected\s`)
	tsExport    = regexp.MustCompile(`^export\s`)
)

// comparison holds the language rules for one symbol.
type comparison struct {
	lang   string
	family string
	kind   string
	// annotations is set where adding a type annotation to an untyped
	// parameter, result or member is compatible (Python, TypeScript).
	annotations bool
}

func newComparison(lang, kind string) comparison {
	family := model.LangFamily(lang)
	return comparison{lang: lang, family: family, kind: kind, annotations: family == "js" || family == "python"}
}

// results compares return types: only an annotation added where there was
// none is allowed.
func (c comparison) results(old, new string) (ok, changed bool) {
	switch {
	case old == new:
		return true, false
	case c.annotations && old == "":
		return true, true
	}
	return false, false
}

// params compares parameter lists: existing parameters keep their position,
// type and markers (an annotation may be added), names count only in
// Python, and new parameters may only be appended when optional or variadic.
//
// A Python parameter without a name is positional-only (or a property
// setter's value): giving it a name is compatible, while renaming a
// parameter or making it positional-only breaks keyword callers.
func (c comparison) params(old, new []model.Param) (ok, changed bool) {
	if len(new) < len(old) {
		return false, false
	}
	for i, o := range old {
		n := new[i]
		allowed := false
		switch {
		case c.lang != "python":
			o.Name, n.Name = "", "" // only Python callers pass arguments by name
		case o.Name == "" && n.Name != "":
			n.Name, allowed = "", true
		}
		if c.annotations && o.Type == "" && n.Type != "" {
			o.Type, allowed = n.Type, true
		}
		if o != n {
			return false, false
		}
		changed = changed || allowed
	}
	for _, n := range new[len(old):] {
		if !n.Optional && !n.Variadic {
			return false, false
		}
		changed = true
	}
	return true, changed
}

// members compares the exported shape of type-like symbols by member name:
// existing members must stay as they were (an annotation may be added), and
// new members are allowed only where memberAddable says so, in a Python
// class only after the existing ones. Reordering is allowed where members
// form a set to callers, and breaking where the order carries meaning
// (orderedMembers).
func (c comparison) members(old, new []model.Member) (ok, changed bool) {
	oldBy, newBy := groupMembers(old), groupMembers(new)
	for name, os := range oldBy {
		ns := newBy[name]
		if len(ns) != len(os) {
			return false, false
		}
		for i, o := range os {
			n := ns[i]
			if o == n {
				continue
			}
			if c.annotations && o.Type == "" && n.Type != "" {
				o.Type = n.Type
				if o == n {
					changed = true
					continue
				}
			}
			return false, false
		}
	}
	for name, ns := range newBy {
		if _, ok := oldBy[name]; ok {
			continue
		}
		for _, n := range ns {
			if !c.memberAddable(n) {
				return false, false
			}
		}
		changed = true
	}
	if c.family == "python" && c.kind == "class" && !addedLast(old, new) {
		return false, false // the positional constructor's fields shift
	}
	if reordered(old, new) {
		if c.orderedMembers() {
			return false, false
		}
		changed = true
	}
	return true, changed
}

// orderedMembers reports whether callers depend on the order of the
// members: the fields of a Go struct (unkeyed composite literals) and of a
// Python class (the positional constructor of a dataclass or NamedTuple,
// which the signature cannot tell from a plain class), enum values and
// aliases. The members of interfaces, traits, Rust structs and TypeScript
// classes form a set.
func (c comparison) orderedMembers() bool {
	switch c.kind {
	case "interface", "trait":
		return false
	case "struct":
		return c.lang == "go"
	case "class":
		return c.family == "python"
	}
	return true
}

// addedLast reports whether every member new adds comes after all the
// members it keeps from old.
func addedLast(old, new []model.Member) bool {
	inOld := make(map[string]bool, len(old))
	for _, m := range old {
		inOld[m.Name] = true
	}
	added := false
	for _, m := range new {
		switch {
		case !inOld[m.Name]:
			added = true
		case added:
			return false
		}
	}
	return true
}

// reordered reports whether the members both lists share appear in a
// different order.
func reordered(old, new []model.Member) bool {
	inNew := make(map[string]bool, len(new))
	for _, m := range new {
		inNew[m.Name] = true
	}
	inOld := make(map[string]bool, len(old))
	for _, m := range old {
		inOld[m.Name] = true
	}
	var a, b []string
	for _, m := range old {
		if inNew[m.Name] {
			a = append(a, m.Name)
		}
	}
	for _, m := range new {
		if inOld[m.Name] {
			b = append(b, m.Name)
		}
	}
	return !slices.Equal(a, b)
}

// memberAddable reports whether adding m keeps old users working: any field
// of a Go struct, and optional members of other structs, classes and
// TypeScript interfaces. Go interfaces, Rust traits, enums and type aliases
// take no additions.
func (c comparison) memberAddable(m model.Member) bool {
	switch {
	case c.lang == "go":
		return c.kind == "struct"
	case c.kind == "struct" || c.kind == "class":
		return m.Optional
	case c.kind == "interface":
		return c.family == "js" && m.Optional
	}
	return false
}

// groupMembers groups members by name, keeping their order, so overloaded
// interface methods compare as a list.
func groupMembers(ms []model.Member) map[string][]model.Member {
	by := make(map[string][]model.Member, len(ms))
	for _, m := range ms {
		by[m.Name] = append(by[m.Name], m)
	}
	return by
}

var visibilityWords = regexp.MustCompile(`\bpub\s*\([^)]*\)|\bpub\b|\bexport(\s+default)?\b|\b(public|private|protected|internal)\b`)

// stripVisibility removes visibility keywords from a signature and
// normalizes spacing, so signatures that differ only in visibility compare
// equal.
func stripVisibility(sig string) string {
	return strings.Join(strings.Fields(visibilityWords.ReplaceAllString(sig, " ")), " ")
}
