package symbols

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/crewlet/gitlet/internal/model"
)

// extractGo records Go definitions, references and imports (docs/SPEC.md,
// sections 9.2 to 9.5).
//
// Definitions are the top-level functions, methods, type specs and const and
// var specs. Declarations inside function bodies are not symbols, and neither
// are blank names such as the "var _ I = (*T)(nil)" assertion.
//
// Signatures are rebuilt from the tree in one fixed style, so formatting,
// comments, struct tags and parameter or receiver names never change them:
//
//	func Load(string, bool) (Config, error)
//	func Map[T any, U any]([]T, func(T) U) []U
//	func Printf(string, ...any)
//	func (*Config) Load(string, bool) (Config, error)
//	type List[T any] struct {Items []T}
//	type Store interface {Get(string) ([]byte, error); io.Closer}
//	type Empty struct{}
//	type ID string
//	type Alias = pkg.T
//	const MaxSize int
//	var Default *Config
//
// Lists are joined with ", ", struct and interface elements with "; " and
// union terms with " | "; results follow their parameters after one space,
// and a single result has no parentheses. Grouped names are expanded: "a, b
// int" is two parameters, "[T, U any]" two type parameters, "A, B int" two
// fields. A struct signature lists its exported fields and every embedded
// type; a struct or interface literal nested in another type lists all its
// elements, since they all make up its identity. Within a const group, a spec
// with neither type nor value repeats the previous spec's type, as Go does.
//
// Params carry no names, since Go callers cannot pass arguments by name. A
// variadic parameter "...T" has Type "T" and Variadic set.
func extractGo(b *builder, root *sitter.Node) {
	if pc := ChildOfKind(root, "package_clause"); pc != nil {
		b.SetPackage(b.Text(ChildOfKind(pc, "package_identifier")))
	}
	goDecls(b, root)
	goWalker{b}.walk(root, nil)
}

// goDecls records the definitions among n's children. Declarations that
// tree-sitter wrapped in an ERROR node while recovering are still found.
func goDecls(b *builder, n *sitter.Node) {
	for _, c := range Named(n) {
		switch c.Kind() {
		case "import_declaration":
			Walk(c, func(s *sitter.Node) bool {
				if s.Kind() == "import_spec" {
					goImport(b, s)
					return false
				}
				return true
			})
		case "function_declaration", "method_declaration":
			goFunc(b, c)
		case "type_declaration":
			goTypeDecl(b, c)
		case "const_declaration", "var_declaration":
			goValueDecl(b, c)
		case "ERROR":
			goDecls(b, c)
		}
	}
}

// goFunc records a function or a method. A method's container is its
// receiver's base type, without pointer or type arguments.
func goFunc(b *builder, n *sitter.Node) {
	name := goName(b, Field(n, "name"))
	if name == "" {
		return
	}
	params, list := goParams(b, Field(n, "parameters"))
	results := goResults(b, Field(n, "result"))
	d := Def{Node: n, Name: name, Kind: "function", Exported: goExported(name), Params: params, Results: results}
	head := "func " + name + goTypeParams(b, Field(n, "type_parameters"))
	if n.Kind() == "method_declaration" {
		recv := goReceiver(n)
		d.Kind, d.Container = "method", goBaseName(b, recv)
		head = "func (" + goType(b, recv) + ") " + name
	}
	d.Signature, d.Head = head+list+goSpaced(results), head
	b.Add(d)
}

// goTypeDecl records each type spec and alias of a type declaration. A
// single ungrouped spec is recorded with its declaration, so the symbol's
// text starts with "type".
func goTypeDecl(b *builder, decl *sitter.Node) {
	var specs []*sitter.Node
	for _, c := range Named(decl) {
		if c.Kind() == "type_spec" || c.Kind() == "type_alias" {
			specs = append(specs, c)
		}
	}
	for _, spec := range specs {
		name := goName(b, Field(spec, "name"))
		if name == "" {
			continue
		}
		node := spec
		if len(specs) == 1 && !HasChild(decl, "(") {
			node = decl
		}
		typ := Field(spec, "type")
		inner := goUnparen(typ)
		var shape []string
		d := Def{Node: node, Name: name, Kind: "type", Exported: goExported(name)}
		switch goKind(inner) {
		case "struct_type":
			shape, d.Members = goStructFields(b, inner, true)
		case "interface_type":
			shape, d.Members = goInterfaceElems(b, inner)
		}
		head := "type " + name + goTypeParams(b, Field(spec, "type_parameters"))
		switch {
		case spec.Kind() == "type_alias":
			d.Signature = head + " = " + goType(b, typ)
		case goKind(inner) == "struct_type":
			d.Kind, d.Head = "struct", head+" struct"
			d.Signature = d.Head + goBody(shape)
		case goKind(inner) == "interface_type":
			d.Kind, d.Head = "interface", head+" interface"
			d.Signature = d.Head + goBody(shape)
		default:
			d.Signature = head + " " + goType(b, typ)
		}
		b.Add(d)
	}
}

// goValueDecl records one symbol per name in each spec of a top-level const
// or var declaration. A single ungrouped spec is recorded with its
// declaration, so the symbol's text starts with "const" or "var".
func goValueDecl(b *builder, decl *sitter.Node) {
	kind, specKind, parent := "const", "const_spec", decl
	if decl.Kind() == "var_declaration" {
		kind, specKind = "var", "var_spec"
		if list := ChildOfKind(decl, "var_spec_list"); list != nil {
			parent = list
		}
	}
	var specs []*sitter.Node
	for _, c := range Named(parent) {
		if c.Kind() == specKind {
			specs = append(specs, c)
		}
	}
	inherited := ""
	for _, spec := range specs {
		typ := goType(b, Field(spec, "type"))
		if kind == "const" {
			if typ == "" && Field(spec, "value") == nil {
				typ = inherited
			} else {
				inherited = typ
			}
		}
		node := spec
		if len(specs) == 1 && !HasChild(parent, "(") {
			node = decl
		}
		for _, id := range goNamedFields(spec, "name") {
			if name := goName(b, id); name != "" {
				b.Add(Def{
					Node:      node,
					Name:      name,
					Kind:      kind,
					Exported:  goExported(name),
					Signature: kind + " " + name + goSpaced(typ),
				})
			}
		}
	}
}

// goImport records one import spec. The bound name defaults to the package
// name guessed from the path (goDefaultName).
func goImport(b *builder, spec *sitter.Node) {
	module := goUnquote(b.Text(Field(spec, "path")))
	if module == "" {
		return
	}
	im := Import{Module: module, Kind: ImportNamespace}
	switch name := Field(spec, "name"); goKind(name) {
	case "dot":
		im.Kind = ImportGlob
	case "blank_identifier":
	case "package_identifier":
		im.Local = b.Text(name)
	default:
		im.Local = goDefaultName(module)
	}
	b.Import(im, spec)
}

// goDefaultName guesses the package name an import path binds: its last
// element, skipping a major version suffix ("example.com/mod/v2" binds "mod")
// and dropping a gopkg.in version ("gopkg.in/yaml.v3" binds "yaml"). Other
// elements are kept as they are, "go-" prefixes and dashes included. "v0" and
// "v1" are never module suffixes, so "k8s.io/api/core/v1" binds "v1".
func goDefaultName(importPath string) string {
	elems := strings.Split(strings.TrimRight(importPath, "/"), "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && goMajorVersion(last) {
		last = elems[len(elems)-2]
	}
	if i := strings.LastIndex(last, ".v"); i > 0 && goDigits(last[i+2:]) {
		last = last[:i]
	}
	return last
}

func goMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' || s[1] == '0' || !goDigits(s[1:]) {
		return false
	}
	return s != "v1"
}

func goDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func goUnquote(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return strings.Trim(s, "\"`")
}

// goParams returns a parameter list's parameters and the list as written in
// signatures: "(string, ...any)".
func goParams(b *builder, list *sitter.Node) ([]model.Param, string) {
	var params []model.Param
	var parts []string
	for _, p := range Named(list) {
		typ := goType(b, Field(p, "type"))
		switch p.Kind() {
		case "variadic_parameter_declaration":
			params = append(params, model.Param{Type: typ, Variadic: true})
			parts = append(parts, "..."+typ)
		case "parameter_declaration":
			for range max(1, len(goNamedFields(p, "name"))) {
				params = append(params, model.Param{Type: typ})
				parts = append(parts, typ)
			}
		}
	}
	return params, "(" + strings.Join(parts, ", ") + ")"
}

// goResults returns a function's results without names: "", "error" or
// "(Config, error)".
func goResults(b *builder, n *sitter.Node) string {
	if goKind(n) != "parameter_list" {
		return goType(b, n)
	}
	params, list := goParams(b, n)
	switch len(params) {
	case 0:
		return ""
	case 1:
		return params[0].Type
	}
	return list
}

// goTypeParams returns a type parameter list as "[K comparable, V any]", or
// "" when there is none.
func goTypeParams(b *builder, list *sitter.Node) string {
	var parts []string
	for _, d := range Named(list) {
		if d.Kind() != "type_parameter_declaration" {
			continue
		}
		constraint := goType(b, Field(d, "type"))
		for _, id := range goNamedFields(d, "name") {
			parts = append(parts, b.Text(id)+" "+constraint)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// goStructFields returns a struct's fields as written in signatures ("Name
// string", "*Base") and as members. Embedded types are always included;
// exportedOnly leaves out unexported named fields.
func goStructFields(b *builder, st *sitter.Node, exportedOnly bool) ([]string, []model.Member) {
	var parts []string
	var members []model.Member
	for _, f := range Named(ChildOfKind(st, "field_declaration_list")) {
		if f.Kind() != "field_declaration" {
			continue
		}
		typ := goType(b, Field(f, "type"))
		names := goNamedFields(f, "name")
		if len(names) == 0 {
			if HasChild(f, "*") {
				typ = "*" + typ
			}
			name := goBaseName(b, Field(f, "type"))
			if name == "" {
				name = typ
			}
			parts = append(parts, typ)
			members = append(members, model.Member{Name: name, Type: typ})
			continue
		}
		for _, id := range names {
			name := b.Text(id)
			if exportedOnly && !goExported(name) {
				continue
			}
			parts = append(parts, name+" "+typ)
			members = append(members, model.Member{Name: name, Type: typ})
		}
	}
	return parts, members
}

// goInterfaceElems returns an interface's methods ("Get(string) error") and
// embedded types or unions ("io.Closer", "~int | ~string"), as written in
// signatures and as members.
func goInterfaceElems(b *builder, it *sitter.Node) ([]string, []model.Member) {
	var parts []string
	var members []model.Member
	for _, e := range Named(it) {
		switch e.Kind() {
		case "method_elem":
			name := b.Text(Field(e, "name"))
			_, list := goParams(b, Field(e, "parameters"))
			fn := list + goSpaced(goResults(b, Field(e, "result")))
			parts = append(parts, name+fn)
			members = append(members, model.Member{Name: name, Type: fn, Method: true})
		case "type_elem":
			typ := goType(b, e)
			name := typ
			if ts := Named(e); len(ts) == 1 && goNamedType(ts[0]) {
				name = goBaseName(b, ts[0])
			}
			parts = append(parts, typ)
			members = append(members, model.Member{Name: name, Type: typ})
		}
	}
	return parts, members
}

// goType renders a type in the signature style: no comments or parameter
// names, ", " between list items and a space before a function's results.
func goType(b *builder, n *sitter.Node) string {
	switch goKind(n) {
	case "":
		return ""
	case "parenthesized_type":
		return goType(b, goFirstNamed(n)) // (*T) is the same type as *T
	case "pointer_type":
		return "*" + goType(b, goFirstNamed(n))
	case "negated_type":
		return "~" + goType(b, goFirstNamed(n))
	case "slice_type":
		return "[]" + goType(b, Field(n, "element"))
	case "array_type":
		return "[" + b.Norm(Field(n, "length")) + "]" + goType(b, Field(n, "element"))
	case "implicit_length_array_type":
		return "[...]" + goType(b, Field(n, "element"))
	case "map_type":
		return "map[" + goType(b, Field(n, "key")) + "]" + goType(b, Field(n, "value"))
	case "channel_type":
		dir := "chan "
		if goKind(n.Child(0)) == "<-" {
			dir = "<-chan "
		} else if HasChild(n, "<-") {
			dir = "chan<- "
		}
		return dir + goType(b, Field(n, "value"))
	case "function_type":
		_, list := goParams(b, Field(n, "parameters"))
		return "func" + list + goSpaced(goResults(b, Field(n, "result")))
	case "generic_type":
		return goType(b, Field(n, "type")) + goType(b, Field(n, "type_arguments"))
	case "type_arguments":
		return "[" + goJoin(b, Named(n), ", ") + "]"
	case "type_elem", "type_constraint":
		return goJoin(b, Named(n), " | ")
	case "struct_type":
		parts, _ := goStructFields(b, n, false)
		return "struct" + goBody(parts)
	case "interface_type":
		parts, _ := goInterfaceElems(b, n)
		return "interface" + goBody(parts)
	}
	return b.Norm(n)
}

func goJoin(b *builder, nodes []*sitter.Node, sep string) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		parts[i] = goType(b, n)
	}
	return strings.Join(parts, sep)
}

// goBody writes struct or interface elements: " {A int; B}", or "{}".
func goBody(parts []string) string {
	if len(parts) == 0 {
		return "{}"
	}
	return " {" + strings.Join(parts, "; ") + "}"
}

func goSpaced(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// goReceiver returns the type of a method's receiver.
func goReceiver(method *sitter.Node) *sitter.Node {
	for _, p := range Named(Field(method, "receiver")) {
		if t := Field(p, "type"); t != nil {
			return t
		}
	}
	return nil
}

// goBaseName returns the name of the type behind pointers, parentheses,
// type arguments and package qualifiers: "Config" for *pkg.Config or
// List[T]. It returns "" for unnamed types.
func goBaseName(b *builder, t *sitter.Node) string {
	for {
		switch goKind(t) {
		case "pointer_type", "parenthesized_type":
			t = goFirstNamed(t)
		case "generic_type":
			t = Field(t, "type")
		case "qualified_type":
			t = Field(t, "name")
		case "type_identifier":
			return b.Text(t)
		default:
			return ""
		}
	}
}

func goUnparen(t *sitter.Node) *sitter.Node {
	for goKind(t) == "parenthesized_type" {
		t = goFirstNamed(t)
	}
	return t
}

// goName returns a definition's name, or "" when it is missing or blank.
func goName(b *builder, n *sitter.Node) string {
	if n == nil || n.IsMissing() {
		return ""
	}
	if name := b.Text(n); name != "_" {
		return name
	}
	return ""
}

func goExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func goKind(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Kind()
}

func goFirstNamed(n *sitter.Node) *sitter.Node {
	if ns := Named(n); len(ns) > 0 {
		return ns[0]
	}
	return nil
}

// goNamedFields is Fields without the punctuation that some grammar rules
// put under the same field name.
func goNamedFields(n *sitter.Node, name string) []*sitter.Node {
	var out []*sitter.Node
	for _, c := range Fields(n, name) {
		if c.IsNamed() {
			out = append(out, c)
		}
	}
	return out
}

// goNamedType reports whether t names a type: T, pkg.T or T[int].
func goNamedType(t *sitter.Node) bool {
	switch goKind(t) {
	case "type_identifier", "qualified_type", "generic_type":
		return true
	}
	return false
}

// goPredeclared holds Go's predeclared types, builtin functions and the
// blank identifier, which are never references.
var goPredeclared = map[string]bool{
	"bool": true, "string": true, "error": true, "any": true, "comparable": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"byte": true, "rune": true,
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	"_": true,
}

// goWalker records the references in a Go file. The scope passed along holds
// the type parameters in view, which are not references either.
type goWalker struct{ b *builder }

func (w goWalker) walk(n *sitter.Node, scope map[string]bool) {
	if n == nil {
		return
	}
	var skip *sitter.Node
	switch n.Kind() {
	case "function_declaration", "method_declaration":
		scope = goScope(w.b, n, scope)
	case "type_spec", "type_alias":
		scope = goScope(w.b, n, scope)
		skip = Field(n, "name")
	case "type_identifier", "qualified_type", "generic_type":
		w.named(n, model.RefType, scope)
		return
	case "call_expression":
		w.call(n, scope)
		return
	case "type_conversion_expression":
		// F[int](x) parses as a conversion to a generic type, but it is
		// almost always a call of a generic function.
		if t := Field(n, "type"); goKind(t) == "generic_type" {
			w.named(t, model.RefCall, scope)
			w.walk(Field(n, "operand"), scope)
			return
		}
	case "composite_literal":
		w.construct(Field(n, "type"), scope)
		w.walk(Field(n, "body"), scope)
		return
	case "field_declaration":
		if Field(n, "name") == nil { // embedded field
			w.named(Field(n, "type"), model.RefExtends, scope)
			return
		}
	case "interface_type":
		for _, c := range Named(n) {
			if ts := Named(c); c.Kind() == "type_elem" && len(ts) == 1 && goNamedType(ts[0]) {
				w.named(ts[0], model.RefExtends, scope)
			} else {
				w.walk(c, scope)
			}
		}
		return
	}
	for _, c := range Named(n) {
		if skip == nil || c.Id() != skip.Id() {
			w.walk(c, scope)
		}
	}
}

// named records a reference of the given kind to T, pkg.T or the base of
// T[int], whose type arguments are type references. Other types are walked.
func (w goWalker) named(t *sitter.Node, kind model.RefKind, scope map[string]bool) {
	switch goKind(t) {
	case "type_identifier":
		if name := w.b.Text(t); !goPredeclared[name] && !scope[name] {
			w.b.Ref(name, "", kind, t)
		}
	case "qualified_type":
		w.b.Ref(w.b.Text(Field(t, "name")), w.b.Text(Field(t, "package")), kind, t)
	case "generic_type":
		w.named(Field(t, "type"), kind, scope)
		w.walk(Field(t, "type_arguments"), scope)
	default:
		w.walk(t, scope)
	}
}

// call records f(x), pkg.F(x) and x.y.Z(), and new(T) as constructing T.
// Calls of parenthesized, indexed or literal functions are not recorded.
func (w goWalker) call(n *sitter.Node, scope map[string]bool) {
	fn, args := Field(n, "function"), Field(n, "arguments")
	switch goKind(fn) {
	case "identifier":
		name := w.b.Text(fn)
		if name == "new" {
			for i, a := range Named(args) {
				if i == 0 && goNamedType(a) {
					w.named(a, model.RefConstruct, scope)
				} else {
					w.walk(a, scope)
				}
			}
			return
		}
		if !goPredeclared[name] {
			w.b.Ref(name, "", model.RefCall, fn)
		}
	case "selector_expression":
		if q, ok := goQualifier(w.b, Field(fn, "operand")); ok {
			field := Field(fn, "field")
			w.b.Ref(w.b.Text(field), q, model.RefCall, field)
		}
	}
	w.walk(fn, scope)
	w.walk(Field(n, "type_arguments"), scope)
	w.walk(args, scope)
}

// construct records the type a composite literal builds: T{}, pkg.T{},
// List[int]{}, and the element type of []T{}, [...]T{} or []*T{}. Map and
// struct literals only refer to their types.
func (w goWalker) construct(t *sitter.Node, scope map[string]bool) {
	switch goKind(t) {
	case "slice_type", "array_type", "implicit_length_array_type":
		w.walk(Field(t, "length"), scope)
		w.construct(Field(t, "element"), scope)
	case "pointer_type":
		w.construct(goFirstNamed(t), scope)
	default:
		w.named(t, model.RefConstruct, scope)
	}
}

// goQualifier returns the text before the called name: "pkg", "x.y", or
// "a.b()" for a call on another call's result. Other operands, such as index
// or parenthesized expressions, report false.
func goQualifier(b *builder, n *sitter.Node) (string, bool) {
	switch goKind(n) {
	case "identifier":
		return b.Text(n), true
	case "selector_expression":
		q, ok := goQualifier(b, Field(n, "operand"))
		return q + "." + b.Text(Field(n, "field")), ok
	case "call_expression":
		q, ok := goQualifier(b, Field(n, "function"))
		return q + "()", ok
	}
	return "", false
}

// goScope returns scope plus the type parameters n declares: those of a
// generic function or type, or those a method names in its receiver, as T in
// func (l *List[T]).
func goScope(b *builder, n *sitter.Node, scope map[string]bool) map[string]bool {
	var names []string
	if n.Kind() == "method_declaration" {
		t := goReceiver(n)
		for goKind(t) == "pointer_type" || goKind(t) == "parenthesized_type" {
			t = goFirstNamed(t)
		}
		if goKind(t) == "generic_type" {
			for _, e := range Named(Field(t, "type_arguments")) {
				if id := goFirstNamed(e); goKind(id) == "type_identifier" {
					names = append(names, b.Text(id))
				}
			}
		}
	} else {
		for _, d := range Named(Field(n, "type_parameters")) {
			for _, id := range goNamedFields(d, "name") {
				names = append(names, b.Text(id))
			}
		}
	}
	if len(names) == 0 {
		return scope
	}
	out := make(map[string]bool, len(scope)+len(names))
	for name := range scope {
		out[name] = true
	}
	for _, name := range names {
		out[name] = true
	}
	return out
}
