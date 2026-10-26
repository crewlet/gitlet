package rules

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/model"
)

// matchRef implements Match (docs/SPEC.md, section 9.6).
//
// A resolved ref matches when the language families and names agree and its
// module is one of the symbol's modules; in Go, Python and Rust the symbol's
// module may also end in "/" + the ref's module, for packages below the
// repository root (a go.mod or source root in a subdirectory). An
// unresolved ref (its qualifier is a variable, receiver, self or this)
// matches a member (method, field) by name only, when that name is unique.
// In Go the symbol must be exported unless the ref is in its package.
// Qualifiers and modules arrive resolved by the footprint package; nothing
// here resolves them again.
func matchRef(ref model.Ref, sym model.Symbol, uniqueMethod bool) (match, resolved bool) {
	if ref.Name == "" || ref.Name != sym.Name {
		return false, false
	}
	if model.LangFamily(ref.Lang) != model.LangFamily(sym.Lang) {
		return false, false
	}
	if sym.Lang == "go" && !goExported(sym) && path.Dir(ref.Path) != path.Dir(sym.Path) {
		return false, false
	}
	if !ref.Resolved {
		return member(sym) && uniqueMethod, false
	}
	ok := moduleMatch(ref.Module, sym)
	return ok, ok
}

// member reports whether a symbol lives inside a type: a method or field.
func member(s model.Symbol) bool { return s.Container != "" || s.Kind == "method" }

// goExported applies Go's rule even when a document left Exported unset.
func goExported(s model.Symbol) bool {
	if s.Exported {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s.Name)
	return unicode.IsUpper(r)
}

// moduleMatch reports whether mod is one of sym's modules.
func moduleMatch(mod string, sym model.Symbol) bool {
	mod = strings.Trim(mod, "/")
	if mod == "" {
		return false
	}
	suffix := sym.Lang == "go" || sym.Lang == "python" || sym.Lang == "rust"
	for _, m := range model.ModulesOf(sym.Lang, sym.Path) {
		if m == mod || (suffix && strings.HasSuffix(m, "/"+mod)) {
			return true
		}
	}
	return false
}
