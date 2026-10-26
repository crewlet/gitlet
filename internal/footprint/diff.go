package footprint

import (
	"cmp"
	"slices"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

func diffSymbols(old, new *symbols.File) []model.SymbolChange {
	oldSyms, newSyms := symbolsOf(old), symbolsOf(new)
	newByKey := make(map[string]int, len(newSyms))
	for i, s := range newSyms {
		if _, dup := newByKey[keyOf(s)]; !dup {
			newByKey[keyOf(s)] = i
		}
	}

	var out []model.SymbolChange
	var removed, added []model.Symbol
	matched := make([]bool, len(newSyms))
	for _, o := range oldSyms {
		i, ok := newByKey[keyOf(o)]
		if !ok || matched[i] {
			removed = append(removed, o)
			continue
		}
		matched[i] = true
		if c, changed := compareSymbols(o, newSyms[i]); changed {
			out = append(out, c)
		}
	}
	for i, s := range newSyms {
		if !matched[i] {
			added = append(added, s)
		}
	}
	out = append(out, pairRenamed(removed, added)...)
	sortChanges(out)
	return out
}

func diffMoved(old, new *symbols.File) []model.SymbolChange {
	oldSyms, newSyms := symbolsOf(old), symbolsOf(new)
	newByName := make(map[string]int, len(newSyms))
	for i, s := range newSyms {
		if _, dup := newByName[s.Qualified]; !dup {
			newByName[s.Qualified] = i
		}
	}

	var out []model.SymbolChange
	matched := make([]bool, len(newSyms))
	for _, o := range oldSyms {
		i, ok := newByName[o.Qualified]
		if !ok || matched[i] {
			out = append(out, removedChange(o))
			continue
		}
		matched[i] = true
		out = append(out, movedChange(o, newSyms[i]))
	}
	for i, s := range newSyms {
		if !matched[i] {
			out = append(out, model.SymbolChange{Change: model.ChangeAdded, Symbol: s})
		}
	}
	sortChanges(out)
	return out
}

func symbolsOf(f *symbols.File) []model.Symbol {
	if f == nil {
		return nil
	}
	return f.Symbols
}

// keyOf returns the symbol's key, computing it when an extractor left it out.
func keyOf(s model.Symbol) string {
	if s.Key != "" {
		return s.Key
	}
	return model.SymbolKey(s.Lang, s.Path, s.Qualified)
}

// compareSymbols compares two versions of one symbol, reporting false when
// neither hash changed.
func compareSymbols(o, n model.Symbol) (model.SymbolChange, bool) {
	sig, body := o.SigHash != n.SigHash, o.BodyHash != n.BodyHash
	switch {
	case sig:
		return model.SymbolChange{
			Change:       model.ChangeSignature,
			Breaking:     Classify(o, n),
			SigChanged:   true,
			BodyChanged:  body,
			OldSignature: o.Signature,
			OldSigHash:   o.SigHash,
			Symbol:       n,
		}, true
	case body:
		return model.SymbolChange{Change: model.ChangeBody, BodyChanged: true, Symbol: n}, true
	}
	return model.SymbolChange{}, false
}

func removedChange(o model.Symbol) model.SymbolChange {
	return model.SymbolChange{Change: model.ChangeRemoved, Breaking: true, Symbol: o}
}

// movedChange describes a symbol that kept its qualified name in a renamed
// file. It breaks callers when its module changes or its signature changes
// in a breaking way.
func movedChange(o, n model.Symbol) model.SymbolChange {
	c := model.SymbolChange{
		Change:      model.ChangeMoved,
		SigChanged:  o.SigHash != n.SigHash,
		BodyChanged: o.BodyHash != n.BodyHash,
		OldPath:     o.Path,
		Symbol:      n,
	}
	if c.SigChanged {
		c.OldSignature, c.OldSigHash = o.Signature, o.SigHash
	}
	c.Breaking = moduleChanged(o, n) || (c.SigChanged && Classify(o, n))
	return c
}

// moduleChanged reports whether callers that reached o through one of its
// modules can no longer reach n there. Moving a.ts to a/index.ts keeps the
// module "a"; the reverse loses "a/index".
func moduleChanged(o, n model.Symbol) bool {
	if model.LangFamily(o.Lang) != model.LangFamily(n.Lang) {
		return true
	}
	newMods := model.ModulesOf(n.Lang, n.Path)
	for _, m := range model.ModulesOf(o.Lang, o.Path) {
		if !slices.Contains(newMods, m) {
			return true
		}
	}
	return false
}

// pairRenamed turns removed and added symbols of one file that share a kind
// and a body hash into renames. Each symbol pairs at most once: removed
// symbols are taken in source order, and each takes the first added
// candidate in source order, preferring one with the same container.
func pairRenamed(removed, added []model.Symbol) []model.SymbolChange {
	removed, added = slices.Clone(removed), slices.Clone(added)
	slices.SortStableFunc(removed, compareSource)
	slices.SortStableFunc(added, compareSource)

	var out []model.SymbolChange
	used := make([]bool, len(added))
	for _, o := range removed {
		pick := -1
		for j, a := range added {
			if used[j] || o.BodyHash == "" || a.Kind != o.Kind || a.BodyHash != o.BodyHash {
				continue
			}
			if pick < 0 {
				pick = j
			}
			if a.Container == o.Container {
				pick = j
				break
			}
		}
		if pick < 0 {
			out = append(out, removedChange(o))
			continue
		}
		used[pick] = true
		a := added[pick]
		out = append(out, model.SymbolChange{
			Change:       model.ChangeRenamed,
			Breaking:     true,
			SigChanged:   o.SigHash != a.SigHash,
			OldSignature: o.Signature,
			OldSigHash:   o.SigHash,
			OldName:      o.Qualified,
			Symbol:       a,
		})
	}
	for j, a := range added {
		if !used[j] {
			out = append(out, model.SymbolChange{Change: model.ChangeAdded, Symbol: a})
		}
	}
	return out
}

func compareSource(a, b model.Symbol) int {
	return cmp.Or(cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.StartByte, b.StartByte), cmp.Compare(a.Qualified, b.Qualified))
}

// sortChanges orders changes by path, qualified name and change kind.
func sortChanges(cs []model.SymbolChange) {
	slices.SortStableFunc(cs, func(a, b model.SymbolChange) int {
		return cmp.Or(
			cmp.Compare(a.Symbol.Path, b.Symbol.Path),
			cmp.Compare(a.Symbol.Qualified, b.Symbol.Qualified),
			cmp.Compare(a.Change, b.Change),
			cmp.Compare(a.OldName, b.OldName),
			cmp.Compare(a.OldPath, b.OldPath),
			cmp.Compare(keyOf(a.Symbol), keyOf(b.Symbol)),
		)
	})
}
