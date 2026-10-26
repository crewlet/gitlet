package rules

import (
	"strings"

	"github.com/crewlet/gitlet/internal/model"
)

// keyOf returns a symbol's key, deriving it when the document left it out.
func keyOf(s model.Symbol) string {
	if s.Key != "" {
		return s.Key
	}
	if s.Path == "" || s.Qualified == "" {
		return ""
	}
	return model.SymbolKey(s.Lang, s.Path, s.Qualified)
}

// baseKey is the key of the symbol a change starts from: the old key for
// renamed and moved symbols, the symbol's own key otherwise. Changes that
// different items make to one symbol meet on this key, and claims use it.
func baseKey(c model.SymbolChange) string {
	s := c.Symbol
	switch {
	case c.Change == model.ChangeRenamed && c.OldName != "" && s.Path != "":
		return model.SymbolKey(s.Lang, s.Path, c.OldName)
	case c.Change == model.ChangeMoved && c.OldPath != "" && s.Qualified != "":
		return model.SymbolKey(s.Lang, c.OldPath, s.Qualified)
	}
	return keyOf(s)
}

// oldSymbol returns the symbol as it was at the base, which is what other
// code refers to: the old name of a renamed symbol, the old path of a moved
// one, and the old signature when known.
func oldSymbol(c model.SymbolChange) model.Symbol {
	s := c.Symbol
	switch c.Change {
	case model.ChangeRenamed:
		if c.OldName != "" {
			s.Qualified = c.OldName
			s.Container, s.Name = splitQualified(c.OldName)
		}
	case model.ChangeMoved:
		if c.OldPath != "" {
			s.Path = c.OldPath
		}
	}
	if c.OldSignature != "" {
		s.Signature = c.OldSignature
	}
	if c.OldSigHash != "" {
		s.SigHash = c.OldSigHash
	}
	s.Key = baseKey(c)
	return s
}

// splitQualified splits a qualified name into its container and short name,
// dropping the "@setter", "@Trait" and "#2" suffixes of unique names.
func splitQualified(q string) (container, name string) {
	if i := strings.IndexAny(q, "@#"); i > 0 {
		q = q[:i]
	}
	if i := strings.LastIndex(q, "."); i >= 0 {
		return q[:i], q[i+1:]
	}
	return "", q
}

// breaking reports whether a change breaks callers. Removed and renamed
// symbols always do (docs/SPEC.md, section 9.4), whatever the document says.
func breaking(c model.SymbolChange) bool {
	return c.Breaking || c.Change == model.ChangeRemoved || c.Change == model.ChangeRenamed
}

// contends reports whether a change decides what the symbol's signature
// becomes: a signature change, removal, rename, a move that callers see, or
// an addition. Items whose contending changes differ clash.
func contends(c model.SymbolChange) bool {
	return c.SignatureChanging() || c.Change == model.ChangeAdded
}

// touches reports whether a change edits a symbol that existed at the base.
func touches(c model.SymbolChange) bool { return c.Change != model.ChangeAdded }

// bodyOnly reports whether a change edits the body and leaves what callers
// see alone.
func bodyOnly(c model.SymbolChange) bool {
	switch c.Change {
	case model.ChangeBody:
		return true
	case model.ChangeMoved:
		return c.BodyChanged && !c.SignatureChanging()
	}
	return false
}

// outcome is what a contending change makes of the symbol, so two items
// that change a signature the same way do not clash.
func outcome(c model.SymbolChange) string {
	if c.Change == model.ChangeRemoved {
		return "removed"
	}
	s := c.Symbol
	return s.Path + "\x00" + s.Qualified + "\x00" + s.SigHash
}

// verbing describes a change for "<agent> is ... <symbol>".
func verbing(c model.SymbolChange) string {
	switch c.Change {
	case model.ChangeRemoved:
		return "removing"
	case model.ChangeRenamed:
		return "renaming"
	case model.ChangeMoved:
		if c.SigChanged {
			return "moving and changing the signature of"
		}
		return "moving"
	case model.ChangeBody:
		return "changing the body of"
	case model.ChangeAdded:
		return "adding"
	}
	return "changing the signature of"
}

// verbed describes a change for "main ... <symbol>".
func verbed(c model.SymbolChange) string {
	switch c.Change {
	case model.ChangeRemoved:
		return "removed"
	case model.ChangeRenamed:
		return "renamed"
	case model.ChangeMoved:
		return "moved"
	}
	return "changed the signature of"
}

// describe returns evidence lines for a change: old and new signatures, or
// what was removed, renamed or moved.
func describe(prefix string, c model.SymbolChange) []string {
	s := c.Symbol
	var out []string
	switch c.Change {
	case model.ChangeRemoved:
		out = []string{"removed: " + s.Signature}
	case model.ChangeRenamed:
		out = []string{"renamed: " + c.OldName + " -> " + s.Qualified, "now: " + s.Signature}
	case model.ChangeMoved:
		out = []string{"moved: " + c.OldPath + " -> " + s.Path}
		if c.SigChanged {
			out = append(out, "was: "+c.OldSignature, "now: "+s.Signature+compat(c))
		}
	case model.ChangeAdded:
		out = []string{"adds: " + s.Signature}
	case model.ChangeBody:
		out = []string{"body only: " + s.Signature}
	default:
		out = []string{"was: " + c.OldSignature, "now: " + s.Signature + compat(c)}
	}
	if prefix != "" {
		for i := range out {
			out[i] = prefix + ": " + out[i]
		}
	}
	return out
}

func compat(c model.SymbolChange) string {
	if breaking(c) {
		return "  (breaking)"
	}
	return "  (compatible)"
}
