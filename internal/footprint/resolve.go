package footprint

import (
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// resolve sets Module and Resolved on each ref (docs/SPEC.md, section 9.6)
// and returns them in order:
//
//   - a ref that names something outside the repository (an import of
//     another Go module or the standard library, a bare or aliased
//     JavaScript specifier, a Rust std crate or prelude type, a JavaScript
//     global or Python builtin the file does not define) is dropped;
//   - a ref whose qualifier is a variable, receiver, self, this or super, or
//     any expression that is not an import or a type of this file, is kept
//     with Resolved false and no module;
//   - every other ref gets the module it most likely names, in the form
//     model.ModulesOf uses. When the ref uses an aliased named import, its
//     name becomes the imported name, so it can match the definition.
//
// Python and Rust modules the file imports by absolute path are kept even
// when they are probably third-party: nothing in one file tells them apart
// from a package of the repository, and a module that matches no symbol is
// harmless.
func resolve(f *symbols.File, refs []model.Ref, goModule string) []model.Ref {
	if len(refs) == 0 {
		return nil
	}
	r := newResolver(f, goModule)
	out := make([]model.Ref, 0, len(refs))
	for _, ref := range refs {
		if ref.Lang == "" && f != nil {
			ref.Lang = f.Lang
		}
		ref.Module, ref.Resolved = "", false
		var keep bool
		switch model.LangFamily(ref.Lang) {
		case "go":
			ref, keep = r.goRef(ref)
		case "js":
			ref, keep = r.jsRef(ref)
		case "python":
			ref, keep = r.pyRef(ref)
		case "rust":
			ref, keep = r.rustRef(ref)
		default:
			keep = true // unknown language: leave it unresolved
		}
		if keep {
			out = append(out, ref)
		}
	}
	return out
}

// resolver holds one file's imports and top-level types.
type resolver struct {
	goModule string
	imports  []symbols.Import
	bindings map[string]symbols.Import // by local name; later imports win
	py       map[string]string         // Python qualifier prefixes to modules (pyBindings)
	types    map[string]bool           // top-level type-like symbols of the file
	defined  map[string]bool           // every top-level symbol of the file
	// rustInline holds the paths of the file's inline Rust modules
	// (rustInlineModules).
	rustInline map[string]bool
}

func newResolver(f *symbols.File, goModule string) *resolver {
	r := &resolver{goModule: goModule, bindings: map[string]symbols.Import{}, types: map[string]bool{}, defined: map[string]bool{}}
	if f == nil {
		return r
	}
	r.imports = f.Imports
	r.py = pyBindings(f.Imports)
	r.rustInline = rustInlineModules(f.Symbols)
	for _, im := range f.Imports {
		switch im.Local {
		case "", "_", ".":
			continue
		}
		if im.Kind == symbols.ImportGlob {
			continue
		}
		r.bindings[im.Local] = im
	}
	for _, s := range f.Symbols {
		if s.Container != "" {
			continue
		}
		r.defined[s.Name] = true
		if s.TypeLike() {
			r.types[s.Name] = true
		}
	}
	return r
}

// builtin reports whether an unqualified, unimported name is one of the
// language's builtins (a global, in JavaScript) that the file does not
// define itself.
func (r *resolver) builtin(names map[string]bool, name string) bool {
	return names[name] && !r.defined[name]
}

func resolvedTo(ref model.Ref, module string) (model.Ref, bool) {
	ref.Module, ref.Resolved = module, true
	return ref, true
}

func unresolved(ref model.Ref) (model.Ref, bool) { return ref, true }

func dropped() (model.Ref, bool) { return model.Ref{}, false }

// ownModule is the module of the file a ref is made in.
func ownModule(ref model.Ref) string { return model.ModulesOf(ref.Lang, ref.Path)[0] }

var identRe = regexp.MustCompile(`^[\p{L}_$][\p{L}\p{N}_$]*$`)

func isIdent(s string) bool { return identRe.MatchString(s) }

// dottedIdent reports whether s is a chain of identifiers joined by dots,
// such as "pkg.config"; "self.x[0]" and "super()" are not.
func dottedIdent(s string) bool {
	for part := range strings.SplitSeq(s, ".") {
		if !isIdent(part) {
			return false
		}
	}
	return true
}

func upperFirst(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// Go

// goRef resolves a Go ref. Only a qualifier that is exactly an import's name
// (or a type of this file, for a method expression) names a package; "x.y"
// and "a.b()" are values, so their refs are unresolved.
func (r *resolver) goRef(ref model.Ref) (model.Ref, bool) {
	own := path.Dir(ref.Path)
	q := ref.Qualifier
	if q == "" {
		return resolvedTo(ref, own)
	}
	if im, ok := r.goImport(q); ok {
		mod, ok := goImportModule(im.Module, r.goModule)
		if !ok {
			return dropped()
		}
		return resolvedTo(ref, mod)
	}
	if r.types[q] {
		return resolvedTo(ref, own) // method expression on a type of this package
	}
	return unresolved(ref)
}

// goImport finds the import a qualifier names by its local name. A blank
// import (no local name) binds nothing. When the extractor's guess at the
// bound name is not an identifier, as "go-yaml" for example.com/go-yaml,
// the package name is guessed from the path instead ("yaml").
func (r *resolver) goImport(name string) (symbols.Import, bool) {
	if im, ok := r.bindings[name]; ok {
		return im, true
	}
	for i := len(r.imports) - 1; i >= 0; i-- {
		im := r.imports[i]
		if im.Kind == symbols.ImportGlob || im.Local == "" || isIdent(im.Local) {
			continue
		}
		if goPackageName(im.Module) == name {
			return im, true
		}
	}
	return symbols.Import{}, false
}

var goMajorVersion = regexp.MustCompile(`^v[0-9]+$`)
var goPkgVersion = regexp.MustCompile(`\.v[0-9]+$`)

// goPackageName guesses the package name of an import path:
// "example.com/x/yaml.v3" is yaml, "example.com/x/redis/v9" is redis,
// "example.com/go-cmp" is cmp.
func goPackageName(p string) string {
	name := path.Base(p)
	if goMajorVersion.MatchString(name) && path.Dir(p) != "." {
		name = path.Base(path.Dir(p))
	}
	name = goPkgVersion.ReplaceAllString(name, "")
	name = strings.TrimPrefix(name, "go-")
	name = strings.TrimSuffix(strings.TrimSuffix(name, "-go"), ".go")
	return strings.NewReplacer("-", "_", ".", "_").Replace(name)
}

// goImportModule maps a Go import path to a package directory of the
// repository. With a module path, imports outside the module are external.
// Without one, paths that look like they live on a host are kept whole so
// the matcher can compare their suffix, and others (the standard library)
// are external.
func goImportModule(importPath, module string) (string, bool) {
	switch {
	case module != "" && importPath == module:
		return ".", true
	case module != "":
		rest, ok := strings.CutPrefix(importPath, module+"/")
		return rest, ok && rest != ""
	}
	first, _, _ := strings.Cut(importPath, "/")
	if strings.Contains(first, ".") && first != "." && first != ".." {
		return importPath, true
	}
	return "", false
}

// TypeScript, TSX and JavaScript

// jsGlobals are names that are always defined outside the repository.
var jsGlobals = setOf("console", "Math", "JSON", "Object", "Array", "Promise", "Number", "String",
	"Boolean", "Symbol", "BigInt", "Date", "RegExp", "Error", "Map", "Set", "WeakMap", "WeakSet",
	"Reflect", "Proxy", "Intl", "globalThis", "window", "document", "process", "Buffer")

// jsGlobalFuncs are global functions and constructors called without a
// qualifier.
var jsGlobalFuncs = setOf("parseInt", "parseFloat", "isNaN", "isFinite", "setTimeout", "setInterval",
	"clearTimeout", "clearInterval", "queueMicrotask", "structuredClone", "encodeURIComponent",
	"decodeURIComponent", "encodeURI", "decodeURI", "Error", "TypeError", "RangeError", "Promise",
	"Map", "Set", "WeakMap", "WeakSet", "Date", "RegExp", "Array", "Object", "Number", "String",
	"Boolean", "Symbol", "BigInt", "URL", "URLSearchParams")

func (r *resolver) jsRef(ref model.Ref) (model.Ref, bool) {
	if ref.Qualifier == "" {
		im, ok := r.bindings[ref.Name]
		if !ok {
			if r.builtin(jsGlobalFuncs, ref.Name) {
				return dropped()
			}
			return resolvedTo(ref, ownModule(ref))
		}
		mod, ok := jsSpecifier(ref.Path, im.Module)
		if !ok {
			return dropped()
		}
		if im.Kind == symbols.ImportNamed && im.Name != "" && im.Name != "default" {
			ref.Name = im.Name
		}
		return resolvedTo(ref, mod)
	}
	head, _, _ := strings.Cut(ref.Qualifier, ".")
	if !isIdent(head) || head == "this" || head == "super" {
		return unresolved(ref)
	}
	// A namespace import names its module; a named or default import used as
	// a qualifier (a class with static members, an exported object) is
	// defined in the module it comes from.
	if im, ok := r.bindings[head]; ok {
		mod, ok := jsSpecifier(ref.Path, im.Module)
		if !ok {
			return dropped()
		}
		return resolvedTo(ref, mod)
	}
	if r.types[head] {
		return resolvedTo(ref, ownModule(ref))
	}
	if jsGlobals[head] {
		return dropped()
	}
	return unresolved(ref)
}

var jsExts = setOf(".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts")

// jsSpecifier resolves a relative import specifier against the importing
// file, without extension: "./lib" in src/a.ts is "src/lib", which also
// stands for src/lib/index.ts. Bare and alias specifiers are external.
func jsSpecifier(file, spec string) (string, bool) {
	if spec != "." && spec != ".." && !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return "", false
	}
	p := path.Join(path.Dir(file), spec)
	if ext := path.Ext(p); jsExts[ext] {
		p = strings.TrimSuffix(p, ext)
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false // outside the repository
	}
	return p, true
}

// Python

// pyBuiltins are builtin types used as qualifiers (str.join, dict.fromkeys).
var pyBuiltins = setOf("str", "bytes", "int", "float", "complex", "bool", "list", "dict", "set",
	"frozenset", "tuple", "object", "type", "bytearray", "memoryview", "range")

// pyBuiltinFuncs are builtin functions and exceptions called without a
// qualifier.
var pyBuiltinFuncs = setOf("super", "print", "len", "isinstance", "issubclass", "range", "enumerate",
	"zip", "map", "filter", "sorted", "reversed", "min", "max", "sum", "any", "all", "abs", "round",
	"open", "iter", "next", "getattr", "setattr", "hasattr", "delattr", "repr", "hash", "id", "vars",
	"dir", "callable", "format", "divmod", "pow", "chr", "ord", "hex", "oct", "bin", "input",
	"str", "bytes", "int", "float", "complex", "bool", "list", "dict", "set", "frozenset", "tuple",
	"object", "type", "bytearray", "memoryview", "property", "staticmethod", "classmethod",
	"Exception", "BaseException", "ValueError", "TypeError", "KeyError", "IndexError", "RuntimeError",
	"NotImplementedError", "AttributeError", "OSError", "IOError", "StopIteration", "AssertionError",
	"ImportError", "LookupError", "PermissionError", "FileNotFoundError", "TimeoutError")

func (r *resolver) pyRef(ref model.Ref) (model.Ref, bool) {
	pkg := path.Dir(ref.Path)
	if ref.Qualifier == "" {
		im, ok := r.bindings[ref.Name]
		if !ok {
			if r.builtin(pyBuiltinFuncs, ref.Name) {
				return dropped()
			}
			return resolvedTo(ref, ownModule(ref))
		}
		mod, ok := pyModule(pkg, im.Module)
		if !ok {
			return dropped()
		}
		if im.Kind == symbols.ImportNamed && im.Name != "" {
			ref.Name = im.Name
		}
		return resolvedTo(ref, mod)
	}
	head, _, _ := strings.Cut(ref.Qualifier, ".")
	if head == "self" || head == "cls" || !dottedIdent(ref.Qualifier) {
		return unresolved(ref) // receivers, super(), calls and subscripts
	}
	if m, rest, ok := r.pyLookup(ref.Qualifier); ok {
		base, ok := pyModule(pkg, m)
		if !ok {
			return dropped()
		}
		return resolvedTo(ref, joinModule(base, rest))
	}
	switch {
	case r.types[head]:
		return resolvedTo(ref, ownModule(ref))
	case pyBuiltins[head]:
		return dropped()
	}
	return unresolved(ref)
}

// pyLookup finds the import binding that is the longest dotted prefix of a
// qualifier, and returns its module and the segments after it.
func (r *resolver) pyLookup(q string) (module string, rest []string, ok bool) {
	segs := strings.Split(q, ".")
	for n := len(segs); n > 0; n-- {
		if m, ok := r.py[strings.Join(segs[:n], ".")]; ok {
			return m, segs[n:], true
		}
	}
	return "", nil, false
}

// pyBindings maps every name a Python qualifier can start with to the module
// it stands for, as written (possibly relative):
//   - import a.b as c binds c to a.b;
//   - import a.b binds a.b to a.b and a to a;
//   - from m import x binds x to the submodule m.x, or to m when x is
//     capitalized and so most likely a class (Cls.method lives in m).
func pyBindings(imports []symbols.Import) map[string]string {
	out := map[string]string{}
	for _, im := range imports {
		switch im.Kind {
		case symbols.ImportNamespace, symbols.ImportDefault:
			first, _, _ := strings.Cut(im.Module, ".")
			if im.Local == "" {
				continue
			}
			if im.Local == im.Module || im.Local == first {
				out[im.Module] = im.Module
				out[first] = first
				continue
			}
			out[im.Local] = im.Module
		case symbols.ImportNamed:
			if im.Local == "" || im.Local == "_" {
				continue
			}
			m := im.Module
			if im.Name != "" && !upperFirst(im.Name) {
				if strings.Trim(m, ".") == "" {
					m += im.Name
				} else {
					m += "." + im.Name
				}
			}
			out[im.Local] = m
		}
	}
	return out
}

// pyModule turns a Python module as written into a path module: relative
// modules resolve against the importing file's package directory, absolute
// ones against the repository root. A relative import that climbs above the
// root names nothing in the repository.
func pyModule(pkg, m string) (string, bool) {
	dots := len(m) - len(strings.TrimLeft(m, "."))
	rest := m[dots:]
	base := "."
	if dots > 0 {
		base = pkg
		for range dots - 1 {
			if base == "." {
				return "", false
			}
			base = path.Dir(base)
		}
	}
	if rest == "" {
		return base, true
	}
	return path.Join(base, strings.ReplaceAll(rest, ".", "/")), true
}

// joinModule appends qualifier segments to a module up to the first
// capitalized one, which names a type rather than a module.
func joinModule(base string, segs []string) string {
	parts := []string{base}
	for _, s := range segs {
		if upperFirst(s) {
			break
		}
		parts = append(parts, s)
	}
	return path.Join(parts...)
}

// Rust

// rustExternal are crates that are never part of the repository.
var rustExternal = setOf("std", "core", "alloc", "proc_macro", "test")

// rustPrelude are types every Rust file sees without importing them.
var rustPrelude = setOf("Vec", "String", "Box", "Option", "Some", "None", "Result", "Ok", "Err",
	"Default", "Iterator", "IntoIterator", "From", "Into", "TryFrom", "TryInto", "ToString", "ToOwned")

var rustPathRe = regexp.MustCompile(`^(::)?[\p{L}_][\p{L}\p{N}_]*(::[\p{L}_][\p{L}\p{N}_]*)*$`)

func (r *resolver) rustRef(ref model.Ref) (model.Ref, bool) {
	if ref.Qualifier == "" {
		im, ok := r.bindings[ref.Name]
		if !ok {
			return resolvedTo(ref, ownModule(ref))
		}
		if im.Kind == symbols.ImportNamed && im.Name != "" && im.Name != "self" {
			ref.Name = im.Name
		}
		return r.rustPath(ref, strings.Split(im.Module, "::"))
	}
	// Receivers ("self", "self.store", "a.b()", "x?", "_") and a Self the
	// extractor could not replace with the impl's type are values.
	q := ref.Qualifier
	segs := strings.Split(strings.TrimPrefix(q, "::"), "::")
	if !rustPathRe.MatchString(q) || q == "self" || q == "Self" || slices.Contains(segs, "_") {
		return unresolved(ref)
	}
	if strings.HasPrefix(q, "::") {
		return dropped() // ::name is always an extern crate
	}
	head := segs[0]
	if im, ok := r.bindings[head]; ok {
		full := strings.Split(im.Module, "::")
		if im.Kind == symbols.ImportNamed && im.Name != "" {
			full = append(full, im.Name)
		}
		return r.rustPath(ref, append(full, segs[1:]...))
	}
	switch {
	case r.types[head]:
		return resolvedTo(ref, ownModule(ref))
	case upperFirst(head) && rustPrelude[head]:
		return dropped()
	case upperFirst(head):
		return unresolved(ref) // a type from a glob import or another crate
	case len(segs) == 1 && ref.Kind == model.RefCall && !rustPathKeyword[head] && !r.rustInline[head]:
		// x.f() and x::f() both arrive as qualifier "x". A child module is
		// bound by its `mod x;` declaration above, so what is left is a
		// method call on a value, or a call into another crate.
		return unresolved(ref)
	}
	return r.rustPath(ref, segs)
}

var rustPathKeyword = setOf("crate", "self", "super")

// rustPath resolves a Rust path to the module it names: crate is the
// nearest src directory, self and super are relative to the file's module,
// and a path that starts with any other lowercase name is a child module of
// the file's module (or an extern crate, which then matches nothing). The
// module ends at the first capitalized segment, which names a type. A path
// into an inline module of the file names the file itself, whose module its
// items belong to.
func (r *resolver) rustPath(ref model.Ref, segs []string) (model.Ref, bool) {
	own := ownModule(ref)
	base, rest := own, segs
	switch {
	case len(segs) == 0 || segs[0] == "":
		return dropped()
	case segs[0] == "crate":
		base, rest = rustCrateRoot(ref.Path), segs[1:]
	case segs[0] == "self":
		rest = segs[1:]
	case segs[0] == "super":
		for len(rest) > 0 && rest[0] == "super" {
			base, rest = path.Dir(base), rest[1:]
		}
	case rustExternal[segs[0]]:
		return dropped()
	}
	parts := []string{base}
	for _, s := range rest {
		if s == "self" {
			continue
		}
		if upperFirst(s) {
			break
		}
		parts = append(parts, s)
	}
	mod := path.Join(parts...)
	if inner, ok := strings.CutPrefix(mod, own+"/"); ok && r.rustInline[strings.ReplaceAll(inner, "/", ".")] {
		mod = own
	}
	return resolvedTo(ref, mod)
}

// rustInlineModules returns the paths of a Rust file's inline modules
// ("outer", "outer.inner"), read from its symbols' containers: the leading
// lowercase segments, since impl types (capitalized) follow them.
func rustInlineModules(syms []model.Symbol) map[string]bool {
	out := map[string]bool{}
	for _, s := range syms {
		if model.LangFamily(s.Lang) != "rust" || s.Container == "" {
			continue
		}
		var prefix []string
		for seg := range strings.SplitSeq(s.Container, ".") {
			if seg == "" || upperFirst(seg) {
				break
			}
			prefix = append(prefix, seg)
			out[strings.Join(prefix, ".")] = true
		}
	}
	return out
}

// rustCrateRoot returns the nearest src directory above a file, or the
// repository root.
func rustCrateRoot(file string) string {
	for d := path.Dir(file); d != "." && d != "/"; d = path.Dir(d) {
		if path.Base(d) == "src" {
			return d
		}
	}
	return "."
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
