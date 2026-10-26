package symbols

import (
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/crewlet/gitlet/internal/model"
)

// extractTypeScript records TypeScript, TSX and JavaScript definitions,
// references and imports (docs/SPEC.md, sections 9.2 to 9.5). The three
// grammars share one extractor; JavaScript simply has no types.
//
// Definitions are functions, classes and their methods (constructors,
// accessors, abstract and overloaded signatures, and fields holding a
// function), interfaces, type aliases, enums and variables bound to an arrow
// function or function expression, at the top level and inside namespaces
// and ambient blocks. Overload signatures merge into the implementation that
// follows them. Class properties are members of their class, not symbols.
//
// Signatures leave out parameter names, default values, comments and
// decorators, and types are rendered by typ so that formatting never
// matters:
//
//	export async function load(string, boolean?): Promise<Config>
//	function f(_, ?, ..._)                     untyped, defaulted and rest parameters
//	export function f(string): void | export function f(number): void
//	export function load(string): Config      const load = (s: string): Config => ...
//	export const load: Loader                 const load: Loader = ...
//	private static async Config.load(string): Promise<Config>
//	get Config.x(): string                    set Config.x(string)
//	export abstract class Config<T> extends Base<T> implements A, B {name: string; port?: number}
//	export interface Opts extends Base {name: string; load(string): Config}
//	export type ID = string | number
//	export const enum Color {Red, Green = "g"}
//
// Public accessibility is never written, so adding or removing a "public"
// keyword changes nothing; private and protected are written.
func extractTypeScript(b *builder, root *sitter.Node) {
	e := &tsExtractor{b: b, exported: map[string]bool{}, tparams: map[string]int{}}
	e.collectExports(root)
	e.statements(root, tsScope{top: true, exported: true})
	for _, d := range tsMergeOverloads(e.defs) {
		b.Add(d)
	}
	e.refs(root)
}

// tsExtractor holds one file's extraction state.
type tsExtractor struct {
	b        *builder
	exported map[string]bool // top-level names exported by a clause, export default, export = or CommonJS
	tparams  map[string]int  // type parameters in scope while collecting refs
	defs     []tsDef
}

// tsScope is where a list of statements sits.
type tsScope struct {
	container string // namespace path, "" outside namespaces
	top       bool   // the file's top level, where export clauses apply
	exported  bool   // the enclosing namespace is visible outside the file
	ambient   bool   // inside declare, where every member is exported
}

// tsDecl is one statement with export and declare unwrapped.
type tsDecl struct {
	node      *sitter.Node // the declaration itself
	outer     *sitter.Node // the whole statement, export keyword included
	export    bool
	isDefault bool
	ambient   bool
}

// tsDef is a definition before overloads are merged.
type tsDef struct {
	Def
	key      string // overload group: container, name, static and accessor; "" never merges
	overload bool   // a signature without a body
}

// tsPredefined are type names that never refer to a repository symbol. The
// grammar parses most of them as predefined types already; the list guards
// error recovery. Library globals such as Promise or Record are kept: they
// resolve to nothing unless the repository defines them.
var tsPredefined = map[string]bool{
	"string": true, "number": true, "boolean": true, "any": true, "unknown": true, "never": true,
	"void": true, "object": true, "symbol": true, "bigint": true, "undefined": true, "null": true,
}

// collectExports finds the top-level names exported away from their
// declaration: export { a, b as c }, export default a, export = a,
// module.exports = { a } and exports.b = b.
func (e *tsExtractor) collectExports(root *sitter.Node) {
	for _, st := range Named(root) {
		switch st.Kind() {
		case "export_statement":
			if Field(st, "source") != nil || Field(st, "declaration") != nil {
				continue
			}
			if v := Field(st, "value"); v != nil && v.Kind() == "identifier" {
				e.exported[e.b.Text(v)] = true
			}
			if HasChild(st, "=") {
				if id := ChildOfKind(st, "identifier"); id != nil {
					e.exported[e.b.Text(id)] = true
				}
			}
			for _, s := range Named(ChildOfKind(st, "export_clause")) {
				if n := Field(s, "name"); n != nil && n.Kind() == "identifier" {
					e.exported[e.b.Text(n)] = true
				}
			}
		case "expression_statement":
			e.commonJSExports(tsFirstNamed(st))
		}
	}
}

func (e *tsExtractor) commonJSExports(x *sitter.Node) {
	if x == nil || x.Kind() != "assignment_expression" {
		return
	}
	left, right := Field(x, "left"), Field(x, "right")
	if left == nil || right == nil || left.Kind() != "member_expression" {
		return
	}
	switch {
	case e.isModuleExports(left):
		if right.Kind() == "identifier" {
			e.exported[e.b.Text(right)] = true
			return
		}
		if right.Kind() != "object" {
			return
		}
		for _, p := range Named(right) {
			switch p.Kind() {
			case "shorthand_property_identifier":
				e.exported[e.b.Text(p)] = true
			case "pair":
				if v := Field(p, "value"); v != nil && v.Kind() == "identifier" {
					e.exported[e.b.Text(v)] = true
				}
			}
		}
	case e.isModuleExports(Field(left, "object")) || e.b.Text(Field(left, "object")) == "exports":
		if right.Kind() == "identifier" {
			e.exported[e.b.Text(right)] = true
		}
	}
}

func (e *tsExtractor) isModuleExports(n *sitter.Node) bool {
	return n != nil && n.Kind() == "member_expression" &&
		e.b.Text(Field(n, "object")) == "module" && e.b.Text(Field(n, "property")) == "exports"
}

func (e *tsExtractor) statements(body *sitter.Node, sc tsScope) {
	for _, st := range Named(body) {
		e.statement(st, sc)
	}
}

func (e *tsExtractor) statement(st *sitter.Node, sc tsScope) {
	d := tsDecl{node: st, outer: st}
	if st.Kind() == "export_statement" {
		if Field(st, "source") != nil {
			return
		}
		d.export, d.isDefault = true, HasChild(st, "default")
		if d.node = Field(st, "declaration"); d.node == nil {
			d.node = Field(st, "value")
		}
		if d.node == nil {
			return
		}
	}
	if d.node.Kind() == "ambient_declaration" {
		d.ambient = true
		if d.node = tsFirstNamed(d.node); d.node == nil {
			return
		}
		if d.node.Kind() == "statement_block" { // declare global { ... }
			e.statements(d.node, tsScope{container: sc.container, exported: true, ambient: true})
			return
		}
	}
	if d.node.Kind() == "expression_statement" { // namespace N { ... } parses as one
		if d.node = tsFirstNamed(d.node); d.node == nil {
			return
		}
	}
	switch d.node.Kind() {
	case "ERROR":
		// Recover the definitions a syntax error swallowed.
		e.statements(d.node, sc)
	case "function_declaration", "generator_function_declaration", "function_signature":
		e.function(d, sc)
	case "function_expression", "generator_function", "arrow_function":
		// An anonymous default export, or a declaration that error
		// recovery parsed as an expression.
		if d.isDefault || Field(d.node, "name") != nil {
			e.function(d, sc)
		}
	case "class_declaration", "abstract_class_declaration":
		e.class(d, sc)
	case "class":
		if d.isDefault || Field(d.node, "name") != nil {
			e.class(d, sc)
		}
	case "interface_declaration":
		e.iface(d, sc)
	case "type_alias_declaration":
		e.typeAlias(d, sc)
	case "enum_declaration":
		e.enum(d, sc)
	case "lexical_declaration", "variable_declaration":
		e.variables(d, sc)
	case "internal_module", "module":
		e.namespace(d, sc)
	}
}

// isExported reports whether a declaration in scope sc is visible outside the
// file: through export at the top level, or through export (implicit inside
// declare) in an exported namespace.
func (e *tsExtractor) isExported(d tsDecl, sc tsScope, name string) bool {
	if sc.top {
		return d.export || e.exported[name]
	}
	return (d.export || sc.ambient) && sc.exported
}

func (e *tsExtractor) namespace(d tsDecl, sc tsScope) {
	nameNode, body := Field(d.node, "name"), Field(d.node, "body")
	if nameNode == nil || body == nil {
		return
	}
	name := e.b.Norm(nameNode)
	if nameNode.Kind() == "string" { // declare module "m" { ... }
		name = tsUnquote(e.b.Text(nameNode))
	}
	e.statements(body, tsScope{
		container: tsJoinName(sc.container, name),
		exported:  e.isExported(d, sc, name) || (sc.top && d.ambient),
		ambient:   sc.ambient || d.ambient,
	})
}

// declName returns the name of a function or class declaration, and the name
// to show in its signature, which is empty for an anonymous default export.
func (e *tsExtractor) declName(d tsDecl) (name, shown string) {
	shown = e.b.Text(Field(d.node, "name"))
	if shown == "" && d.isDefault {
		return "default", ""
	}
	return shown, shown
}

func (e *tsExtractor) function(d tsDecl, sc tsScope) {
	fn := d.node
	name, shown := e.declName(d)
	if name == "" {
		return
	}
	exported := e.isExported(d, sc, name)
	rest, params, results := e.callable(fn)
	sig := e.funcPrefix(exported, d.isDefault, fn)
	if shown != "" {
		sig += " " + shown
	}
	e.defs = append(e.defs, tsDef{
		Def: Def{
			Node: d.outer, Name: name, Container: sc.container, Kind: "function", Exported: exported,
			Signature: sig + rest, Params: params, Results: results, Head: sig + e.callableHead(fn),
		},
		key:      "function\x00" + sc.container + "\x00" + name,
		overload: fn.Kind() == "function_signature",
	})
}

// funcPrefix renders "export default async function*" with the parts that apply.
func (e *tsExtractor) funcPrefix(exported, isDefault bool, fn *sitter.Node) string {
	var sb strings.Builder
	if exported {
		sb.WriteString("export ")
	}
	if isDefault {
		sb.WriteString("default ")
	}
	if HasChild(fn, "async") {
		sb.WriteString("async ")
	}
	sb.WriteString("function")
	if HasChild(fn, "*") {
		sb.WriteString("*")
	}
	return sb.String()
}

// variables records variables bound to an arrow function, function expression
// or generator. Their signature is written like a function declaration's, so
// switching between the two forms is not a signature change; a variable with
// its own type annotation shows that type instead.
func (e *tsExtractor) variables(d tsDecl, sc tsScope) {
	var decls []*sitter.Node
	for _, v := range Named(d.node) {
		if v.Kind() == "variable_declarator" {
			decls = append(decls, v)
		}
	}
	for _, v := range decls {
		nameNode, fn := Field(v, "name"), tsFunctionValue(Field(v, "value"))
		name := e.b.Text(nameNode)
		if name == "" || nameNode.Kind() != "identifier" || fn == nil {
			continue
		}
		exported := e.isExported(d, sc, name)
		def := Def{Node: d.outer, Name: name, Container: sc.container, Kind: "function", Exported: exported}
		if len(decls) > 1 {
			def.Node = v
		}
		if t := Field(v, "type"); t != nil {
			prefix := ""
			if exported {
				prefix = "export "
			}
			def.Signature = prefix + "const " + name + ": " + e.typ(t)
		} else {
			rest, params, results := e.callable(fn)
			head := e.funcPrefix(exported, false, fn) + " " + name
			def.Signature, def.Head = head+rest, head+e.callableHead(fn)
			def.Params, def.Results = params, results
		}
		e.defs = append(e.defs, tsDef{Def: def, key: "function\x00" + sc.container + "\x00" + name})
	}
}

func (e *tsExtractor) class(d tsDecl, sc tsScope) {
	c := d.node
	name, shown := e.declName(d)
	if name == "" {
		return
	}
	exported := e.isExported(d, sc, name)
	container := tsJoinName(sc.container, name)

	var props []string
	var members []model.Member
	var nodes []*sitter.Node
	var methods []tsDef
	addProp := func(text string, m model.Member) {
		props = append(props, text)
		members = append(members, m)
	}
	for _, m := range Named(Field(c, "body")) {
		if e.propName(tsMemberName(m)) == "" && m.Kind() != "index_signature" {
			continue // a name lost to error recovery
		}
		switch m.Kind() {
		case "method_definition", "method_signature", "abstract_method_signature":
			nodes = append(nodes, m)
			methods = append(methods, e.method(m, container, exported))
			if m.Kind() == "method_definition" && e.propName(Field(m, "name")) == "constructor" {
				for _, p := range Named(Field(m, "parameters")) {
					if text, mem, ok := e.paramProperty(p); ok {
						addProp(text, mem)
					}
				}
			}
		case "public_field_definition", "field_definition":
			if fn := tsFunctionValue(Field(m, "value")); fn != nil {
				nodes = append(nodes, m)
				methods = append(methods, e.fieldMethod(m, fn, container, exported))
			} else if text, mem, ok := e.property(m); ok {
				addProp(text, mem)
			}
		case "index_signature":
			addProp(e.indexSignature(m))
		}
	}

	var sb strings.Builder
	if exported {
		sb.WriteString("export ")
	}
	if d.isDefault {
		sb.WriteString("default ")
	}
	if c.Kind() == "abstract_class_declaration" {
		sb.WriteString("abstract ")
	}
	sb.WriteString("class")
	if shown != "" {
		sb.WriteString(" " + shown)
	}
	sb.WriteString(e.typ(Field(c, "type_parameters")))
	for _, h := range Named(ChildOfKind(c, "class_heritage")) {
		switch h.Kind() {
		case "extends_clause", "implements_clause":
			sb.WriteString(" " + e.typ(h))
		default: // JavaScript: the heritage holds the base expression itself
			sb.WriteString(" extends " + e.typ(h))
		}
	}
	head := sb.String()
	sb.WriteString(" {" + strings.Join(props, "; ") + "}")

	e.defs = append(e.defs, tsDef{Def: Def{
		Node: d.outer, Name: name, Container: sc.container, Kind: "class", Exported: exported,
		Signature: sb.String(), Head: head, Members: members, Nested: nodes,
	}})
	e.defs = append(e.defs, methods...)
}

// modifiers reads a class member's accessibility and static, abstract and
// async keywords and renders the ones that belong in a signature. private is
// also true for #names.
func (e *tsExtractor) modifiers(m *sitter.Node, fn *sitter.Node) (prefix string, private, static bool) {
	access := e.b.Text(ChildOfKind(m, "accessibility_modifier"))
	name := tsMemberName(m)
	private = access == "private" || access == "protected" ||
		(name != nil && name.Kind() == "private_property_identifier")
	static = HasChild(m, "static")
	var sb strings.Builder
	if access == "private" || access == "protected" {
		sb.WriteString(access + " ")
	}
	if static {
		sb.WriteString("static ")
	}
	if HasChild(m, "abstract") || m.Kind() == "abstract_method_signature" {
		sb.WriteString("abstract ")
	}
	if HasChild(fn, "async") {
		sb.WriteString("async ")
	}
	return sb.String(), private, static
}

func (e *tsExtractor) method(m *sitter.Node, container string, classExported bool) tsDef {
	name := e.propName(Field(m, "name"))
	prefix, private, static := e.modifiers(m, m)
	accessor := tsAccessor(m)
	var sb strings.Builder
	sb.WriteString(prefix)
	if accessor != "" {
		sb.WriteString(accessor + " ")
	}
	if HasChild(m, "*") {
		sb.WriteString("*")
	}
	sb.WriteString(container + "." + name)
	if HasChild(m, "?") {
		sb.WriteString("?")
	}
	rest, params, results := e.callable(m)
	d := Def{
		Node: m, Name: name, Container: container, Kind: "method", Exported: classExported && !private,
		Signature: sb.String() + rest, Params: params, Results: results, Head: sb.String() + e.callableHead(m),
	}
	if accessor == "set" {
		d.Suffix = "@set"
	}
	key := "method\x00" + container + "\x00" + accessor + "\x00" + name
	if static {
		key += "\x00static"
	}
	return tsDef{Def: d, key: key, overload: m.Kind() != "method_definition"}
}

// tsAccessor returns "get" or "set" for an accessor, "" otherwise.
func tsAccessor(m *sitter.Node) string {
	switch {
	case HasChild(m, "get"):
		return "get"
	case HasChild(m, "set"):
		return "set"
	}
	return ""
}

// fieldMethod records a class field holding a function as a method, written
// like one; a field with its own type annotation shows that type instead.
func (e *tsExtractor) fieldMethod(m, fn *sitter.Node, container string, classExported bool) tsDef {
	name := e.propName(tsMemberName(m))
	t := Field(m, "type")
	if t != nil {
		fn = nil // the annotation, not the function, is what callers see
	}
	prefix, private, _ := e.modifiers(m, fn)
	d := Def{Node: m, Name: name, Container: container, Kind: "method", Exported: classExported && !private}
	if t != nil {
		d.Signature = prefix + container + "." + name + ": " + e.typ(t)
		return tsDef{Def: d}
	}
	if HasChild(fn, "*") {
		prefix += "*"
	}
	rest, params, results := e.callable(fn)
	head := prefix + container + "." + name
	d.Signature, d.Head = head+rest, head+e.callableHead(fn)
	d.Params, d.Results = params, results
	return tsDef{Def: d}
}

// property renders a public class property as "static readonly name?: T".
func (e *tsExtractor) property(m *sitter.Node) (string, model.Member, bool) {
	nameNode := tsMemberName(m)
	prefix, private, _ := e.modifiers(m, nil)
	if nameNode == nil || private {
		return "", model.Member{}, false
	}
	mem := model.Member{Name: e.propName(nameNode), Type: e.typ(Field(m, "type")), Optional: HasChild(m, "?")}
	if HasChild(m, "readonly") {
		prefix += "readonly "
	}
	return prefix + tsMemberText(mem), mem, true
}

// paramProperty renders a public constructor parameter property such as
// "public readonly id: ID" as a class property.
func (e *tsExtractor) paramProperty(p *sitter.Node) (string, model.Member, bool) {
	access := e.b.Text(ChildOfKind(p, "accessibility_modifier"))
	readonly := HasChild(p, "readonly")
	pat := Field(p, "pattern")
	if (access == "" && !readonly) || access == "private" || access == "protected" ||
		pat == nil || pat.Kind() != "identifier" {
		return "", model.Member{}, false
	}
	mem := model.Member{Name: e.b.Text(pat), Type: e.typ(Field(p, "type")), Optional: p.Kind() == "optional_parameter"}
	prefix := ""
	if readonly {
		prefix = "readonly "
	}
	return prefix + tsMemberText(mem), mem, true
}

// indexSignature renders "[string]: T" or a mapped type member such as
// "readonly [K in keyof T]?: T[K]". The key's name is a parameter name and is
// left out.
func (e *tsExtractor) indexSignature(n *sitter.Node) (string, model.Member) {
	key := "[" + e.typ(Field(n, "index_type")) + "]"
	if mc := ChildOfKind(n, "mapped_type_clause"); mc != nil {
		key = "[" + e.typ(mc) + "]"
	}
	prefix := e.b.Text(Field(n, "sign"))
	if HasChild(n, "readonly") {
		prefix += "readonly "
	}
	mark := ""
	t := Field(n, "type")
	if t != nil {
		switch t.Kind() {
		case "opting_type_annotation":
			mark = "?"
		case "omitting_type_annotation":
			mark = "-?"
		case "adding_type_annotation":
			mark = "+?"
		}
	}
	typ := e.typ(t)
	return prefix + key + mark + ": " + typ, model.Member{Name: key, Type: typ, Optional: mark == "?" || mark == "+?"}
}

func (e *tsExtractor) iface(d tsDecl, sc tsScope) {
	n := d.node
	name := e.b.Text(Field(n, "name"))
	if name == "" {
		return
	}
	exported := e.isExported(d, sc, name)
	texts, members := e.objectMembers(Field(n, "body"))
	var sb strings.Builder
	if exported {
		sb.WriteString("export ")
	}
	if d.isDefault {
		sb.WriteString("default ")
	}
	sb.WriteString("interface " + name + e.typ(Field(n, "type_parameters")))
	if ext := ChildOfKind(n, "extends_type_clause"); ext != nil {
		sb.WriteString(" " + e.typ(ext))
	}
	head := sb.String()
	sb.WriteString(" {" + strings.Join(texts, "; ") + "}")
	e.defs = append(e.defs, tsDef{Def: Def{
		Node: d.outer, Name: name, Container: sc.container, Kind: "interface", Exported: exported,
		Signature: sb.String(), Head: head, Members: members,
	}})
}

func (e *tsExtractor) typeAlias(d tsDecl, sc tsScope) {
	n := d.node
	name := e.b.Text(Field(n, "name"))
	if name == "" {
		return
	}
	exported := e.isExported(d, sc, name)
	value := Field(n, "value")
	prefix := "type " + name + e.typ(Field(n, "type_parameters")) + " = "
	if exported {
		prefix = "export " + prefix
	}
	var members []model.Member
	head := ""
	if value != nil && value.Kind() == "object_type" {
		_, members = e.objectMembers(value)
		head = prefix + "{}"
	}
	e.defs = append(e.defs, tsDef{Def: Def{
		Node: d.outer, Name: name, Container: sc.container, Kind: "type", Exported: exported,
		Signature: prefix + e.typ(value), Head: head, Members: members,
	}})
}

func (e *tsExtractor) enum(d tsDecl, sc tsScope) {
	n := d.node
	name := e.b.Text(Field(n, "name"))
	if name == "" {
		return
	}
	exported := e.isExported(d, sc, name)
	var variants []string
	var members []model.Member
	for _, v := range Named(Field(n, "body")) {
		nameNode, value := v, (*sitter.Node)(nil)
		if v.Kind() == "enum_assignment" {
			nameNode, value = Field(v, "name"), Field(v, "value")
		}
		variant := e.propName(nameNode)
		members = append(members, model.Member{Name: variant})
		if value != nil {
			variant += " = " + e.typ(value)
		}
		variants = append(variants, variant)
	}
	var sb strings.Builder
	if exported {
		sb.WriteString("export ")
	}
	if HasChild(n, "const") {
		sb.WriteString("const ")
	}
	sb.WriteString("enum " + name + " {" + strings.Join(variants, ", ") + "}")
	e.defs = append(e.defs, tsDef{Def: Def{
		Node: d.outer, Name: name, Container: sc.container, Kind: "enum", Exported: exported,
		Signature: sb.String(), Members: members,
	}})
}

// objectMembers renders the members of an interface body or object type, and
// returns them as model members: properties with their types, and methods,
// call, construct and index signatures with Type holding the rest of their
// signature.
func (e *tsExtractor) objectMembers(body *sitter.Node) ([]string, []model.Member) {
	var texts []string
	var members []model.Member
	for _, m := range Named(body) {
		var text string
		var mem model.Member
		switch m.Kind() {
		case "property_signature":
			mem = model.Member{Name: e.propName(Field(m, "name")), Type: e.typ(Field(m, "type")), Optional: HasChild(m, "?")}
			text = tsMemberText(mem)
			if HasChild(m, "readonly") {
				text = "readonly " + text
			}
		case "method_signature": // load(string): Config, opt?(): void, get x(): string
			rest, _, _ := e.callable(m)
			accessor := tsAccessor(m)
			mem = model.Member{Name: e.propName(Field(m, "name")), Type: accessor + rest, Optional: HasChild(m, "?"), Method: true}
			text = mem.Name
			if mem.Optional {
				text += "?"
			}
			text += rest
			if accessor != "" {
				text = accessor + " " + text
			}
		case "call_signature":
			rest, _, _ := e.callable(m)
			mem, text = model.Member{Name: "()", Type: rest, Method: true}, rest
		case "construct_signature":
			rest, _, _ := e.callable(m)
			mem, text = model.Member{Name: "new()", Type: rest, Method: true}, "new "+rest
		case "index_signature":
			text, mem = e.indexSignature(m)
		default:
			continue
		}
		texts = append(texts, text)
		members = append(members, mem)
	}
	return texts, members
}

// callable renders the part of a function or method signature after its name,
// "<T>(string, number?): R", and returns its parameters and return type.
// A this parameter is shown but is not one of the parameters.
func (e *tsExtractor) callable(fn *sitter.Node) (string, []model.Param, string) {
	var texts []string
	var params []model.Param
	if p := Field(fn, "parameter"); p != nil { // x => ...
		texts, params = []string{"_"}, []model.Param{{}}
	} else {
		texts, params = e.paramList(Field(fn, "parameters"))
	}
	ret := Field(fn, "return_type")
	if ret == nil && fn.Kind() == "construct_signature" {
		ret = Field(fn, "type")
	}
	results := e.typ(ret)
	sig := e.typ(Field(fn, "type_parameters")) + "(" + strings.Join(texts, ", ") + ")"
	if results != "" {
		sig += ": " + results
	}
	return sig, params, results
}

// callableHead renders what callable shows besides the parameters and the
// return type: the type parameters and a this parameter, "<T>(this: Foo)".
func (e *tsExtractor) callableHead(fn *sitter.Node) string {
	this := ""
	for _, p := range Named(Field(fn, "parameters")) {
		if text, _, isThis, ok := e.param(p); ok && isThis {
			this = text
		}
	}
	return e.typ(Field(fn, "type_parameters")) + "(" + this + ")"
}

func (e *tsExtractor) paramList(n *sitter.Node) ([]string, []model.Param) {
	var texts []string
	var params []model.Param
	for _, p := range Named(n) {
		text, param, this, ok := e.param(p)
		if !ok {
			continue
		}
		texts = append(texts, text)
		if !this {
			params = append(params, param)
		}
	}
	return texts, params
}

// param renders one parameter as its type with markers: "T", "T?" for an
// optional or defaulted parameter, "...T" for a rest parameter. Untyped
// parameters show "_", a defaulted one just "?", and a rest one "..._".
func (e *tsExtractor) param(p *sitter.Node) (text string, param model.Param, this, ok bool) {
	switch p.Kind() {
	case "required_parameter", "optional_parameter":
		pat := Field(p, "pattern")
		if pat == nil { // tuple element labels use the name field
			pat = Field(p, "name")
		}
		typ := e.typ(Field(p, "type"))
		if pat != nil && pat.Kind() == "this" {
			return strings.TrimSuffix("this: "+typ, ": "), model.Param{}, true, true
		}
		param = model.Param{
			Type:     typ,
			Optional: p.Kind() == "optional_parameter" || Field(p, "value") != nil,
			Variadic: pat != nil && pat.Kind() == "rest_pattern",
		}
	case "identifier", "object_pattern", "array_pattern":
	case "assignment_pattern":
		param.Optional = true
	case "rest_pattern":
		param.Variadic = true
	default:
		return "", model.Param{}, false, false
	}
	return tsParamText(param), param, false, true
}

func tsParamText(p model.Param) string {
	t := p.Type
	switch {
	case t == "" && p.Optional:
		return "?"
	case t == "":
		t = "_"
	case p.Optional:
		t += "?"
	}
	if p.Variadic {
		t = "..." + t
	}
	return t
}

// tsMemberText renders a property member as "name?: T".
func tsMemberText(m model.Member) string {
	text := m.Name
	if m.Optional {
		text += "?"
	}
	if m.Type != "" {
		text += ": " + m.Type
	}
	return text
}

// tsMergeOverloads merges each run of overload signatures with the
// implementation that follows it into one definition, keeping the
// implementation's node. Its signature is the overload signatures joined
// with " | ", since the implementation's own signature is not visible to
// callers. With two or more overloads, Params and Results stay empty so
// compatibility falls back to comparing signature hashes. A run with no
// implementation (declare, .d.ts, abstract) keeps its first signature's node.
func tsMergeOverloads(defs []tsDef) []Def {
	out := make([]Def, 0, len(defs))
	for i := 0; i < len(defs); {
		if !defs[i].overload || defs[i].key == "" {
			out = append(out, defs[i].Def)
			i++
			continue
		}
		j := i + 1
		for j < len(defs) && defs[j].overload && defs[j].key == defs[i].key {
			j++
		}
		sigs := defs[i:j]
		d := sigs[0].Def
		if j < len(defs) && defs[j].key == defs[i].key {
			d = defs[j].Def
			j++
		}
		texts := make([]string, len(sigs))
		for k, s := range sigs {
			texts[k] = s.Signature
		}
		d.Signature = strings.Join(texts, " | ")
		d.Params, d.Results, d.Head = nil, "", ""
		if len(sigs) == 1 {
			d.Params, d.Results, d.Head = sigs[0].Params, sigs[0].Results, sigs[0].Head
		}
		out = append(out, d)
		i = j
	}
	return out
}

// propName returns a property, method or enum member name: identifiers and
// #names as written, strings without quotes, computed names normalized.
func (e *tsExtractor) propName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind() {
	case "string":
		return tsUnquote(e.b.Text(n))
	case "computed_property_name":
		return e.typ(n)
	}
	return e.b.Text(n)
}

// tsMemberName returns a class field's or method's name node in either grammar.
func tsMemberName(m *sitter.Node) *sitter.Node {
	if n := Field(m, "name"); n != nil {
		return n
	}
	return Field(m, "property") // JavaScript field_definition
}

// tsFunctionValue returns v when it is a function value a variable or field
// can be bound to.
func tsFunctionValue(v *sitter.Node) *sitter.Node {
	if v == nil {
		return nil
	}
	switch v.Kind() {
	case "arrow_function", "function_expression", "generator_function":
		return v
	}
	return nil
}

// typ renders a type, or another small piece of a declaration such as a base
// class expression or an enum initializer, from its leaf tokens so that
// formatting and comments never matter. It works like builder.Norm, except
// that parameter names inside function and method types are dropped, object
// types list their members with "; ", trailing commas and a leading | are
// dropped, string literals take double quotes, and spacing is fixed: one
// space between words, after commas and around |, &, = and =>, as in
// "Map<string, (T) => U>".
func (e *tsExtractor) typ(n *sitter.Node) string {
	if n == nil || IsComment(n) {
		return ""
	}
	kind := n.Kind()
	switch {
	case kind == "string":
		return tsString(e.b.Text(n))
	case n.ChildCount() == 0 || isStringLike(kind) || kind == "template_literal_type":
		return e.b.Text(n)
	}
	switch kind {
	case "type_annotation", "opting_type_annotation", "omitting_type_annotation", "adding_type_annotation",
		"type_predicate_annotation", "asserts_annotation":
		return e.typ(tsFirstNamed(n))
	case "formal_parameters":
		texts, _ := e.paramList(n)
		return "(" + strings.Join(texts, ", ") + ")"
	case "required_parameter", "optional_parameter":
		text, _, _, _ := e.param(n)
		return text
	case "object_type":
		texts, _ := e.objectMembers(n)
		return "{" + strings.Join(texts, "; ") + "}"
	case "conditional_type":
		return e.typ(Field(n, "left")) + " extends " + e.typ(Field(n, "right")) +
			" ? " + e.typ(Field(n, "consequence")) + " : " + e.typ(Field(n, "alternative"))
	case "type_predicate": // x is T: the parameter name is left out
		return tsSubject(Field(n, "name")) + " is " + e.typ(Field(n, "type"))
	case "asserts":
		inner := tsFirstNamed(n)
		if inner != nil && inner.Kind() == "type_predicate" {
			return "asserts " + e.typ(inner)
		}
		return "asserts " + tsSubject(inner)
	}
	parts := make([]string, 0, n.ChildCount())
	for i := uint(0); i < n.ChildCount(); i++ {
		parts = append(parts, e.typ(n.Child(i)))
	}
	return tsJoin(parts)
}

// tsSubject renders the subject of a type predicate: "this", or "_" for a
// parameter, whose name does not count.
func tsSubject(n *sitter.Node) string {
	if n != nil && n.Kind() == "this" {
		return "this"
	}
	return "_"
}

// tsOps are the tokens tsJoin surrounds with spaces.
var tsOps = map[string]bool{"|": true, "&": true, "=": true, "=>": true}

// tsJoin joins the rendered pieces of a node with typ's spacing. A piece
// that starts with an operator, such as "= {}" for a type parameter default,
// is spaced like the operator.
func tsJoin(parts []string) string {
	var sb strings.Builder
	space := false // the next piece follows a comma or an operator
	for i, p := range parts {
		switch {
		case p == "":
			continue
		case p == "," || p == ";":
			if !tsClosing(parts[i+1:]) {
				sb.WriteString(p)
				space = true
			}
			continue
		case tsOps[p]:
			if sb.Len() == 0 && (p == "|" || p == "&") {
				continue // leading | or & of a union or intersection
			}
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(p)
			space = true
			continue
		}
		if sb.Len() > 0 {
			op, _, spaced := strings.Cut(p, " ")
			last, _ := utf8.DecodeLastRuneInString(sb.String())
			first, _ := utf8.DecodeRuneInString(p)
			if space || (spaced && tsOps[op]) || (tsWordish(last) && tsWordish(first)) {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(p)
		space = false
	}
	return sb.String()
}

// tsWordish reports whether r takes a space next to another word: word
// characters, and quotes, so that `T extends "a"` keeps its space.
func tsWordish(r rune) bool { return isWord(r) || r == '"' || r == '\'' || r == '`' }

// tsClosing reports whether the first non-empty piece closes a bracket, or
// there is none: a separator before it is a trailing one.
func tsClosing(rest []string) bool {
	for _, p := range rest {
		if p == "" {
			continue
		}
		switch p[0] {
		case ')', ']', '}', '>':
			return true
		}
		return false
	}
	return true
}

// tsString renders a quoted string literal with double quotes, so the quote
// style a formatter picks does not matter.
func tsString(raw string) string {
	if len(raw) < 2 || (raw[0] != '\'' && raw[0] != '"') || raw[len(raw)-1] != raw[0] {
		return raw
	}
	body := raw[1 : len(raw)-1]
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' && i+1 < len(body):
			if body[i+1] != '\'' {
				sb.WriteByte(c)
			}
			sb.WriteByte(body[i+1])
			i++
		case c == '"':
			sb.WriteString(`\"`)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// tsUnquote strips the quotes around a string literal.
func tsUnquote(s string) string {
	if len(s) >= 2 && strings.ContainsRune(`'"`+"`", rune(s[0])) && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func tsJoinName(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}

// tsFirstNamed returns n's first named child that is not a comment.
func tsFirstNamed(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && !IsComment(c) {
			return c
		}
	}
	return nil
}

// refs records calls, constructions, JSX elements, type references, extends
// and implements clauses and imports from the whole file. Names declared by
// a class, interface, type alias or type parameter are not references, and
// type parameters in scope are not recorded where they are used.
func (e *tsExtractor) refs(n *sitter.Node) {
	if n == nil || IsComment(n) {
		return
	}
	kind := n.Kind()
	if tsTypeScopes[kind] {
		pushed := e.pushTypeParams(n, kind)
		defer e.popTypeParams(pushed)
	}
	var skip *sitter.Node // a child that declares a name
	switch kind {
	case "import_statement":
		e.importStatement(n)
		return
	case "export_statement":
		if Field(n, "source") != nil { // re-exports are neither refs nor imports
			return
		}
	case "call_expression":
		fn := Field(n, "function")
		if fn != nil && fn.Kind() == "identifier" && e.b.Text(fn) == "require" {
			break // recorded as an import by its declarator
		}
		e.targetRef(fn, model.RefCall)
	case "new_expression":
		e.targetRef(Field(n, "constructor"), model.RefConstruct)
	case "decorator":
		if x := tsFirstNamed(n); x != nil && x.Kind() != "call_expression" {
			e.targetRef(x, model.RefCall) // @Injectable calls Injectable
		}
	case "jsx_opening_element", "jsx_self_closing_element":
		name := Field(n, "name")
		if name != nil && name.Kind() == "identifier" {
			if r, _ := utf8.DecodeRuneInString(e.b.Text(name)); !unicode.IsUpper(r) {
				break // <div>: an intrinsic element
			}
		}
		if name != nil && name.Kind() != "jsx_namespace_name" {
			e.targetRef(name, model.RefCall)
		}
	case "variable_declarator":
		e.requireImport(n)
	case "type_identifier":
		if name := e.b.Text(n); e.tparams[name] == 0 && !tsPredefined[name] {
			e.b.Ref(name, "", model.RefType, n)
		}
		return
	case "nested_type_identifier":
		e.b.Ref(e.b.Text(Field(n, "name")), e.b.Text(Field(n, "module")), model.RefType, n)
		return
	case "extends_clause":
		e.targetRef(Field(n, "value"), model.RefExtends)
	case "class_heritage":
		for _, h := range Named(n) {
			if h.Kind() != "extends_clause" && h.Kind() != "implements_clause" {
				e.targetRef(h, model.RefExtends) // JavaScript: the base expression
			}
		}
	case "implements_clause", "extends_type_clause":
		for _, t := range Named(n) {
			e.heritageType(t)
		}
		return
	case "class_declaration", "abstract_class_declaration", "class", "interface_declaration",
		"type_alias_declaration", "type_parameter", "mapped_type_clause":
		skip = Field(n, "name")
	case "infer_type":
		skip = ChildOfKind(n, "type_identifier")
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && (skip == nil || c.Id() != skip.Id()) {
			e.refs(c)
		}
	}
}

// heritageType records an implemented or extended type as an extends ref and
// its type arguments as type refs.
func (e *tsExtractor) heritageType(t *sitter.Node) {
	if t == nil {
		return
	}
	switch t.Kind() {
	case "type_identifier":
		e.b.Ref(e.b.Text(t), "", model.RefExtends, t)
	case "nested_type_identifier":
		e.b.Ref(e.b.Text(Field(t, "name")), e.b.Text(Field(t, "module")), model.RefExtends, t)
	case "generic_type":
		e.heritageType(Field(t, "name"))
		e.refs(Field(t, "type_arguments"))
	default:
		e.refs(t)
	}
}

// targetRef records a ref to the callee or constructor x: f, a.b.f (qualifier
// "a.b"), this.f, super.f, a?.f. A qualifier that is not a plain path, as in
// f().g(), is recorded as "<expr>" so it never resolves to a module.
func (e *tsExtractor) targetRef(x *sitter.Node, kind model.RefKind) {
	x = tsUnwrap(x)
	if x == nil {
		return
	}
	switch x.Kind() {
	case "identifier":
		e.b.Ref(e.b.Text(x), "", kind, x)
	case "member_expression":
		prop := Field(x, "property")
		if prop == nil {
			return
		}
		q, ok := e.path(Field(x, "object"))
		if !ok {
			q = "<expr>"
		}
		e.b.Ref(e.b.Text(prop), q, kind, prop)
	}
}

// path renders a.b.c, this.x or super.x without optional chaining and
// non-null assertions, or reports false for any other expression.
func (e *tsExtractor) path(x *sitter.Node) (string, bool) {
	x = tsUnwrap(x)
	if x == nil {
		return "", false
	}
	switch x.Kind() {
	case "identifier", "this", "super":
		return e.b.Text(x), true
	case "member_expression":
		q, ok := e.path(Field(x, "object"))
		prop := Field(x, "property")
		if !ok || prop == nil {
			return "", false
		}
		return q + "." + e.b.Text(prop), true
	}
	return "", false
}

// tsUnwrap looks through parentheses and non-null assertions.
func tsUnwrap(x *sitter.Node) *sitter.Node {
	for x != nil && (x.Kind() == "parenthesized_expression" || x.Kind() == "non_null_expression") {
		x = tsFirstNamed(x)
	}
	return x
}

// tsTypeScopes are the node kinds that can declare type parameters.
var tsTypeScopes = map[string]bool{
	"function_declaration": true, "generator_function_declaration": true, "function_signature": true,
	"function_expression": true, "generator_function": true, "arrow_function": true,
	"method_definition": true, "method_signature": true, "abstract_method_signature": true,
	"class_declaration": true, "abstract_class_declaration": true, "class": true,
	"interface_declaration": true, "type_alias_declaration": true,
	"call_signature": true, "construct_signature": true, "function_type": true, "constructor_type": true,
	"index_signature": true, "conditional_type": true,
}

// pushTypeParams puts the type parameters n declares in scope: those of a
// generic declaration, a mapped type's key and a conditional type's infer
// names.
func (e *tsExtractor) pushTypeParams(n *sitter.Node, kind string) []string {
	var names []string
	for _, p := range Named(Field(n, "type_parameters")) {
		if name := Field(p, "name"); name != nil {
			names = append(names, e.b.Text(name))
		}
	}
	switch kind {
	case "index_signature":
		if mc := ChildOfKind(n, "mapped_type_clause"); mc != nil {
			names = append(names, e.b.Text(Field(mc, "name")))
		}
	case "conditional_type":
		Walk(Field(n, "right"), func(c *sitter.Node) bool {
			if c.Kind() == "infer_type" {
				names = append(names, e.b.Text(ChildOfKind(c, "type_identifier")))
			}
			return true
		})
	}
	for _, name := range names {
		e.tparams[name]++
	}
	return names
}

func (e *tsExtractor) popTypeParams(names []string) {
	for _, name := range names {
		e.tparams[name]--
	}
}

// importStatement records the bindings of one import statement. Type-only
// imports count like value imports; a side-effect import binds "".
func (e *tsExtractor) importStatement(n *sitter.Node) {
	if rc := ChildOfKind(n, "import_require_clause"); rc != nil { // import fs = require("fs")
		e.b.Import(Import{
			Local:  e.b.Text(ChildOfKind(rc, "identifier")),
			Module: tsUnquote(e.b.Text(Field(rc, "source"))),
			Kind:   ImportNamespace,
		}, rc)
		return
	}
	src := Field(n, "source")
	if src == nil {
		return
	}
	module := tsUnquote(e.b.Text(src))
	clause := ChildOfKind(n, "import_clause")
	if clause == nil {
		e.b.Import(Import{Module: module, Kind: ImportNamespace}, n)
		return
	}
	for _, c := range Named(clause) {
		switch c.Kind() {
		case "identifier":
			e.b.Import(Import{Local: e.b.Text(c), Module: module, Kind: ImportDefault}, c)
		case "namespace_import":
			e.b.Import(Import{Local: e.b.Text(ChildOfKind(c, "identifier")), Module: module, Kind: ImportNamespace}, c)
		case "named_imports":
			for _, s := range Named(c) {
				if s.Kind() == "import_specifier" {
					e.namedImport(Field(s, "name"), Field(s, "alias"), module, s)
				}
			}
		}
	}
}

// namedImport records import { name as alias }; { default as x } is a
// default import.
func (e *tsExtractor) namedImport(nameNode, alias *sitter.Node, module string, at *sitter.Node) {
	name := e.propName(nameNode)
	local := name
	if alias != nil {
		local = e.b.Text(alias)
	}
	if name == "" || local == "" {
		return
	}
	if name == "default" {
		e.b.Import(Import{Local: local, Module: module, Kind: ImportDefault}, at)
		return
	}
	e.b.Import(Import{Local: local, Name: name, Module: module, Kind: ImportNamed}, at)
}

// requireImport records CommonJS imports: const x = require("m") binds a
// namespace, const { a, b: c } = require("m") and const a = require("m").a
// bind names.
func (e *tsExtractor) requireImport(v *sitter.Node) {
	name, value := Field(v, "name"), Field(v, "value")
	if name == nil || value == nil {
		return
	}
	var member *sitter.Node
	if value.Kind() == "member_expression" {
		member, value = Field(value, "property"), Field(value, "object")
	}
	module, ok := e.requireCall(value)
	if !ok {
		return
	}
	switch name.Kind() {
	case "identifier":
		if member != nil {
			e.b.Import(Import{Local: e.b.Text(name), Name: e.b.Text(member), Module: module, Kind: ImportNamed}, v)
		} else {
			e.b.Import(Import{Local: e.b.Text(name), Module: module, Kind: ImportNamespace}, v)
		}
	case "object_pattern":
		if member != nil {
			return
		}
		for _, p := range Named(name) {
			switch p.Kind() {
			case "shorthand_property_identifier_pattern":
				e.namedImport(p, nil, module, p)
			case "object_assignment_pattern": // { a = 1 }
				if left := Field(p, "left"); left != nil && left.Kind() == "shorthand_property_identifier_pattern" {
					e.namedImport(left, nil, module, p)
				}
			case "pair_pattern": // { a: b } or { a: b = 1 }
				local := Field(p, "value")
				if local != nil && local.Kind() == "assignment_pattern" {
					local = Field(local, "left")
				}
				if local != nil && local.Kind() == "identifier" {
					e.namedImport(Field(p, "key"), local, module, p)
				}
			}
		}
	}
}

// requireCall returns the module of a require("m") call.
func (e *tsExtractor) requireCall(x *sitter.Node) (string, bool) {
	if x == nil || x.Kind() != "call_expression" {
		return "", false
	}
	fn := Field(x, "function")
	if fn == nil || fn.Kind() != "identifier" || e.b.Text(fn) != "require" {
		return "", false
	}
	arg := tsFirstNamed(Field(x, "arguments"))
	if arg == nil || arg.Kind() != "string" {
		return "", false
	}
	return tsUnquote(e.b.Text(arg)), true
}
