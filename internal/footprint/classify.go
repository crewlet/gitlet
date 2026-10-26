package footprint

import (
	"regexp"
	"strings"

	"github.com/crewlet/gitlet/internal/model"
)

// classify compares the structured parts of two signatures (docs/SPEC.md,
// section 9.4). A change is compatible only when every difference it can see
// is an allowed one; when the signature hashes differ but the structured
// fields show no difference at all (type parameters, async, receivers,
// bases, overload groups), the change is breaking.
//
// Hidden differences that come together with an allowed one, such as a
// changed receiver plus an added variadic parameter, cannot be seen.
func classify(old, new model.Symbol) bool {
	if old.SigHash == new.SigHash {
		return false
	}
	if old.Kind != new.Kind || old.Name != new.Name || old.Container != new.Container ||
		model.LangFamily(old.Lang) != model.LangFamily(new.Lang) {
		return true
	}
	if old.Exported && !new.Exported {
		return true // visibility narrowed
	}

	c := newComparison(new.Lang, new.Kind)
	resultsOK, resultsChanged := c.results(old.Results, new.Results)
	paramsOK, paramsChanged := c.params(old.Params, new.Params)
	membersOK, membersChanged := c.members(old.Members, new.Members)
	if !resultsOK || !paramsOK || !membersOK {
		return true
	}
	if resultsChanged || paramsChanged || membersChanged {
		return false
	}
	// Nothing structured explains the new hash. Wider visibility may, but
	// only when the signatures differ in nothing else.
	if !old.Exported && new.Exported {
		return stripVisibility(old.Signature) != stripVisibility(new.Signature)
	}
	return true
}

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
func (c comparison) params(old, new []model.Param) (ok, changed bool) {
	if len(new) < len(old) {
		return false, false
	}
	for i, o := range old {
		n := new[i]
		if c.lang != "python" {
			o.Name, n.Name = "", "" // only Python callers pass arguments by name
		}
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
// new members are allowed only where memberAddable says so.
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
	return true, changed
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
