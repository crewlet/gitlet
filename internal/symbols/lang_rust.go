package symbols

import (
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/crewlet/gitlet/internal/model"
)

// extractRust records Rust definitions, references and imports
// (docs/SPEC.md, sections 9.2 to 9.5).
//
// Definitions are the items at the top level and in inline modules:
// functions, structs and unions (struct), enums, traits, type aliases,
// consts, statics (var), the functions, consts and types of impl blocks, and
// the methods of traits. Items of an inline module have the module path as
// container ("outer.inner"), and so do impl types inside one
// ("outer.Config.load"). Methods of `impl Trait for T` are "T.method@Trait".
// Modules marked #[cfg(test)] are skipped with their refs and imports;
// macro_rules and extern blocks define nothing. Attributes are siblings of
// their item in the tree, so they are part of neither the node nor the
// signature.
//
// Exported means plain `pub`; restricted forms such as pub(crate) are not
// exported, but the visibility text is part of every signature, so widening
// or narrowing it changes the signature hash. Only the item's own visibility
// counts, not its module's or its type's, since `pub use` can re-export from
// private modules. Trait methods follow the trait; trait impl members are
// always exported.
//
// Signatures leave out parameter names, comments and attributes. Types and
// generics are normalized with Norm ("Result<Config,Error>", "<T:Clone>"),
// and the parts are joined with fixed separators:
//
//	pub async fn load(&str, bool) -> Result<Config,Error>
//	pub fn Config::get<T>(&self, &str) -> Option<T> where T:Clone
//	fn Config::fmt@Display(&self, &mut fmt::Formatter<'_>) -> fmt::Result
//	pub struct Config<T> {pub name: String, pub(crate) port: u16, ..}
//	pub struct Id(pub u64, _)
//	pub enum Shape {Circle(f64), Rect {w: f64, h: f64}, Empty}
//	pub trait Store: Send + Sync {fn get(&self, &str) -> Option<Vec<u8>>; type Item; const N: usize}
//	pub type Result<T> = std::result::Result<T,Error>
//	pub static mut COUNT: u32
//
// The receiver of a method is its impl's type name with the type's generic
// arguments ("Foo<T>"); a trait impl adds "@Trait" with the trait's own
// arguments. Private fields show as ".." (named) or "_" (tuple), so their
// types never count but adding the first one does.
func extractRust(b *builder, root *sitter.Node) {
	rsItems(b, root, "")
	rsRefs(b, root, rsScope{})
}

// rsItems records the definitions in a list of items: the source file, an
// inline module's body, or an ERROR node left by a syntax error. module is
// the path of the enclosing inline modules.
func rsItems(b *builder, list *sitter.Node, module string) {
	for _, n := range Named(list) {
		switch n.Kind() {
		case "function_item":
			rsFunction(b, n, module)
		case "struct_item", "union_item":
			rsStruct(b, n, module)
		case "enum_item":
			rsEnum(b, n, module)
		case "trait_item":
			rsTrait(b, n, module)
		case "type_item":
			rsTypeAlias(b, n, module)
		case "const_item", "static_item":
			rsConst(b, n, module)
		case "impl_item":
			rsImpl(b, n, module)
		case "mod_item":
			if body := Field(n, "body"); body != nil && !rsTestModule(b, n) {
				rsItems(b, body, rsJoin(module, b.Text(Field(n, "name")), "."))
			}
		case "ERROR":
			rsItems(b, n, module)
		}
	}
}

// rsName returns an item's name, or "" when it is missing or the item is
// anonymous (`const _: () = ...`).
func rsName(b *builder, n *sitter.Node) string {
	name := b.Text(Field(n, "name"))
	if name == "_" {
		return ""
	}
	return name
}

func rsFunction(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	f := rsFnParts(b, n)
	shown := rsWords(vis, f.mods, "fn") + " " + name
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: "function", Exported: vis == "pub",
		Signature: shown + f.shape(),
		Head:      shown + f.head(),
		Params:    f.params, Results: f.results,
	})
}

func rsStruct(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	keyword := "struct"
	if n.Kind() == "union_item" {
		keyword = "union"
	}
	head := rsWords(vis, keyword) + " " + name + rsNorm(b, Field(n, "type_parameters"))
	where := rsNorm(b, ChildOfKind(n, "where_clause"))
	// The head keeps the form of the field list, and the ".." that stands
	// for private fields, which are not members.
	var sig, shape string
	var members []model.Member
	switch body := Field(n, "body"); {
	case body == nil:
		sig = rsWords(head, where)
	case body.Kind() == "ordered_field_declaration_list":
		var fields string
		fields, members = rsTupleFields(b, body, false)
		sig, shape = rsWords(head+"("+fields+")", where), rsWords(head+"()", where)
	default:
		var fields string
		fields, members = rsFields(b, body, false)
		hidden := ""
		if fields == ".." || strings.HasSuffix(fields, ", ..") {
			hidden = ".."
		}
		sig, shape = rsWords(head, where, "{"+fields+"}"), rsWords(head, where, "{"+hidden+"}")
	}
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: "struct", Exported: vis == "pub",
		Signature: sig, Head: shape, Members: members,
	})
}

// rsFields renders a braced field list ("pub name: String, pub(crate) id:
// u64") and its members. Unless all is set, only fields with a visibility
// modifier are listed, and a trailing ".." stands for the private ones.
func rsFields(b *builder, list *sitter.Node, all bool) (string, []model.Member) {
	var parts []string
	var members []model.Member
	hidden := false
	for _, f := range Named(list) {
		if f.Kind() != "field_declaration" {
			continue
		}
		vis := rsVisibility(b, f)
		if vis == "" && !all {
			hidden = true
			continue
		}
		name, typ := b.Text(Field(f, "name")), rsNorm(b, Field(f, "type"))
		parts = append(parts, rsWords(vis, name+": "+typ))
		members = append(members, model.Member{Name: name, Type: typ})
	}
	if hidden {
		parts = append(parts, "..")
	}
	return strings.Join(parts, ", "), members
}

// rsTupleFields renders a tuple field list ("pub u64, _") and its members,
// named by position. Unless all is set, private fields show as "_".
func rsTupleFields(b *builder, list *sitter.Node, all bool) (string, []model.Member) {
	var parts []string
	var members []model.Member
	vis := ""
	for _, c := range Named(list) {
		switch c.Kind() {
		case "attribute_item":
			continue
		case "visibility_modifier":
			vis = b.Norm(c)
			continue
		}
		if vis == "" && !all {
			parts = append(parts, "_")
		} else {
			typ := rsNorm(b, c)
			members = append(members, model.Member{Name: strconv.Itoa(len(parts)), Type: typ})
			parts = append(parts, rsWords(vis, typ))
		}
		vis = ""
	}
	return strings.Join(parts, ", "), members
}

func rsEnum(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	var variants []string
	var members []model.Member
	for _, v := range Named(Field(n, "body")) {
		vname := b.Text(Field(v, "name"))
		if v.Kind() != "enum_variant" || vname == "" {
			continue
		}
		payload := ""
		if body := Field(v, "body"); body != nil {
			if body.Kind() == "ordered_field_declaration_list" {
				fields, _ := rsTupleFields(b, body, true)
				payload = "(" + fields + ")"
			} else {
				fields, _ := rsFields(b, body, true)
				payload = " {" + fields + "}"
			}
		}
		if value := Field(v, "value"); value != nil {
			payload += " = " + b.Norm(value)
		}
		variants = append(variants, vname+payload)
		members = append(members, model.Member{Name: vname, Type: strings.TrimSpace(payload)})
	}
	head := rsWords(vis, "enum") + " " + name + rsNorm(b, Field(n, "type_parameters"))
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: "enum", Exported: vis == "pub",
		Signature: rsWords(head, rsNorm(b, ChildOfKind(n, "where_clause")), "{"+strings.Join(variants, ", ")+"}"),
		Members:   members,
	})
}

// rsTrait records a trait and its methods. The trait's signature and
// members list every method and associated item; its body hash leaves the
// methods out, so editing a default method changes only that method.
func rsTrait(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	container := rsJoin(module, name, ".")
	var items []string
	var members []model.Member
	var nested []*sitter.Node
	var methods []Def
	for _, item := range Named(Field(n, "body")) {
		iname := rsName(b, item)
		if iname == "" {
			continue
		}
		switch item.Kind() {
		case "function_item", "function_signature_item":
			f := rsFnParts(b, item)
			items = append(items, rsWords(f.mods, "fn")+" "+iname+f.shape())
			members = append(members, model.Member{Name: iname, Type: rsWords(f.mods, f.shape()), Method: true})
			nested = append(nested, item)
			shown := rsWords(f.mods, "fn") + " " + name + "::" + iname
			methods = append(methods, Def{
				Node: item, Name: iname, Container: container, Kind: "method", Exported: vis == "pub",
				Signature: shown + f.shape(),
				Head:      shown + f.head(),
				Params:    f.params, Results: f.results,
			})
		case "associated_type", "type_item":
			typ := rsAssocType(b, item)
			items = append(items, "type "+iname+strings.TrimPrefix(typ, "type"))
			members = append(members, model.Member{Name: iname, Type: typ})
		case "const_item":
			typ := rsNorm(b, Field(item, "type"))
			items = append(items, "const "+iname+": "+typ)
			members = append(members, model.Member{Name: iname, Type: typ})
		}
	}
	head := rsWords(vis, rsKeyword(n, "unsafe"), "trait") + " " + name + rsNorm(b, Field(n, "type_parameters"))
	if bounds := Field(n, "bounds"); bounds != nil {
		head += ": " + rsBounds(b, bounds)
	}
	where := rsNorm(b, ChildOfKind(n, "where_clause"))
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: "trait", Exported: vis == "pub",
		Signature: rsWords(head, where, "{"+strings.Join(items, "; ")+"}"),
		Head:      rsWords(head, where, "{}"),
		Members:   members, Nested: nested,
	})
	for _, m := range methods {
		b.Add(m)
	}
}

// rsAssocType renders an associated type without its name, as a trait
// member's type: "type", "type: Clone", "type<'a>: Iterator where ...",
// "type = u8".
func rsAssocType(b *builder, n *sitter.Node) string {
	s := "type" + rsNorm(b, Field(n, "type_parameters"))
	if bounds := Field(n, "bounds"); bounds != nil {
		s += ": " + rsBounds(b, bounds)
	}
	if n.Kind() == "type_item" {
		s += " = " + rsNorm(b, Field(n, "type"))
	}
	return rsWords(s, rsNorm(b, ChildOfKind(n, "where_clause")))
}

func rsTypeAlias(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	sig := rsWords(vis, "type") + " " + name + rsNorm(b, Field(n, "type_parameters")) + " = " + rsNorm(b, Field(n, "type"))
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: "type", Exported: vis == "pub",
		Signature: rsWords(sig, rsNorm(b, ChildOfKind(n, "where_clause"))),
	})
}

// rsConst records a const or a static. The value is body, not signature.
func rsConst(b *builder, n *sitter.Node, module string) {
	name := rsName(b, n)
	if name == "" {
		return
	}
	vis := rsVisibility(b, n)
	kind, keyword := "const", "const"
	if n.Kind() == "static_item" {
		kind, keyword = "var", rsWords("static", b.Text(ChildOfKind(n, "mutable_specifier")))
	}
	b.Add(Def{
		Node: n, Name: name, Container: module, Kind: kind, Exported: vis == "pub",
		Signature: rsWords(vis, keyword) + " " + name + ": " + rsNorm(b, Field(n, "type")),
	})
}

// rsImpl records the functions, consts and types of an impl block. The impl
// itself is not a symbol.
func rsImpl(b *builder, n *sitter.Node, module string) {
	base, recv := rsImplType(b, Field(n, "type"))
	if base == "" {
		return
	}
	container := rsJoin(module, base, ".")
	suffix, display := "", ""
	trait := Field(n, "trait")
	if trait != nil {
		suffix = "@" + rsTypeName(b, trait)
		display = suffix + rsTypeArgs(b, trait)
	}
	for _, item := range Named(Field(n, "body")) {
		name := rsName(b, item)
		if name == "" {
			continue
		}
		vis := rsVisibility(b, item)
		d := Def{
			Node: item, Name: name, Container: container, Suffix: suffix,
			Exported: vis == "pub" || trait != nil,
		}
		shown := recv + "::" + name + display
		switch item.Kind() {
		case "function_item":
			f := rsFnParts(b, item)
			d.Kind = "method"
			d.Signature = rsWords(vis, f.mods, "fn") + " " + shown + f.shape()
			d.Head = rsWords(vis, f.mods, "fn") + " " + shown + f.head()
			d.Params, d.Results = f.params, f.results
		case "const_item":
			d.Kind = "const"
			d.Signature = rsWords(vis, "const") + " " + shown + ": " + rsNorm(b, Field(item, "type"))
		case "type_item":
			d.Kind = "type"
			d.Signature = rsWords(rsWords(vis, "type")+" "+shown+rsNorm(b, Field(item, "type_parameters"))+
				" = "+rsNorm(b, Field(item, "type")), rsNorm(b, ChildOfKind(item, "where_clause")))
		default:
			continue
		}
		b.Add(d)
	}
}

// rsImplType returns the base name of an impl's self type ("Foo" for
// `impl<T> a::Foo<T>` or `impl Trait for &Foo`) and the receiver shown in
// method signatures: the base name with the type's generic arguments.
func rsImplType(b *builder, n *sitter.Node) (base, recv string) {
	n = rsStripRefs(n)
	if n == nil {
		return "", ""
	}
	switch n.Kind() {
	case "generic_type":
		base = rsTypeName(b, n)
		return base, base + rsNorm(b, Field(n, "type_arguments"))
	case "type_identifier", "primitive_type", "scoped_type_identifier":
		base = rsTypeName(b, n)
		return base, base
	case "dynamic_type":
		return rsImplType(b, Field(n, "trait"))
	}
	base = rsNorm(b, n)
	return base, base
}

// rsStripRefs removes references and pointers around a type.
func rsStripRefs(n *sitter.Node) *sitter.Node {
	for n != nil && (n.Kind() == "reference_type" || n.Kind() == "pointer_type") {
		n = Field(n, "type")
	}
	return n
}

// rsTypeName returns a type's last path segment without generic arguments:
// "Display" for fmt::Display, "From" for From<u8>.
func rsTypeName(b *builder, n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind() {
	case "generic_type", "generic_type_with_turbofish":
		return rsTypeName(b, Field(n, "type"))
	case "scoped_type_identifier", "scoped_identifier":
		return b.Text(Field(n, "name"))
	}
	return b.Text(n)
}

// rsTypeArgs returns a generic type's normalized arguments ("<u8>"), or "".
func rsTypeArgs(b *builder, n *sitter.Node) string {
	if n == nil || n.Kind() != "generic_type" {
		return ""
	}
	return rsNorm(b, Field(n, "type_arguments"))
}

// rsFnShape is what callers see of a function, apart from its visibility
// and name.
type rsFnShape struct {
	mods     string   // "async", "const unsafe", `extern "C"`
	generics string   // "<T:Clone>"
	list     []string // the parameters as shown in the signature, self first
	self     string   // the self parameter as shown, "" for none
	params   []model.Param
	results  string
	where    string
}

// shape renders generics, parameters, return type and where clause:
// "<T>(&self, &str) -> Option<T> where T:Clone".
func (f rsFnShape) shape() string {
	s := f.generics + "(" + strings.Join(f.list, ", ") + ")"
	if f.results != "" {
		s += " -> " + f.results
	}
	return rsWords(s, f.where)
}

// head renders what shape shows besides params and results: "<T>(&self)
// where T:Clone".
func (f rsFnShape) head() string {
	return rsWords(f.generics+"("+f.self+")", f.where)
}

// rsFnParts reads a function_item or function_signature_item. The self
// parameter is part of the signature but not of params.
func rsFnParts(b *builder, n *sitter.Node) rsFnShape {
	f := rsFnShape{
		mods:     rsModifiers(b, ChildOfKind(n, "function_modifiers")),
		generics: rsNorm(b, Field(n, "type_parameters")),
		results:  rsNorm(b, Field(n, "return_type")),
		where:    rsNorm(b, ChildOfKind(n, "where_clause")),
	}
	for _, p := range Named(Field(n, "parameters")) {
		switch p.Kind() {
		case "attribute_item", "ERROR":
		case "self_parameter":
			f.self = rsSelf(b, p)
			f.list = append(f.list, f.self)
		case "parameter":
			typ := rsNorm(b, Field(p, "type"))
			if pat := Field(p, "pattern"); pat != nil && pat.Kind() == "self" {
				f.self = "self: " + typ // self: Box<Self>
				f.list = append(f.list, f.self)
				continue
			}
			f.list = append(f.list, typ)
			f.params = append(f.params, model.Param{Type: typ})
		case "variadic_parameter":
			f.list = append(f.list, "...")
			f.params = append(f.params, model.Param{Variadic: true})
		default: // a bare type, as in the 2015 edition's `fn f(u8);`
			typ := rsNorm(b, p)
			f.list = append(f.list, typ)
			f.params = append(f.params, model.Param{Type: typ})
		}
	}
	return f
}

// rsSelf renders a self parameter as callers see it: "&self", "&mut self",
// "&'a self", or "self" for both `self` and `mut self`.
func rsSelf(b *builder, n *sitter.Node) string {
	if HasChild(n, "&") {
		return b.Norm(n)
	}
	return "self"
}

// rsModifiers renders function modifiers in source order: "const",
// "async unsafe", `extern "C"`.
func rsModifiers(b *builder, n *sitter.Node) string {
	if n == nil {
		return ""
	}
	var words []string
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || IsComment(c) {
			continue
		}
		if c.Kind() == "extern_modifier" {
			words = append(words, rsWords("extern", b.Text(ChildOfKind(c, "string_literal"))))
		} else {
			words = append(words, b.Text(c))
		}
	}
	return strings.Join(words, " ")
}

// rsBounds renders trait bounds as "Send + Sync + 'static".
func rsBounds(b *builder, n *sitter.Node) string {
	var parts []string
	for _, c := range Named(n) {
		parts = append(parts, rsNorm(b, c))
	}
	return strings.Join(parts, " + ")
}

// rsVisibility returns an item's visibility as written ("pub",
// "pub(crate)", "pub(in crate::a)"), or "" when it is private.
func rsVisibility(b *builder, n *sitter.Node) string {
	return b.Norm(ChildOfKind(n, "visibility_modifier"))
}

// rsKeyword returns kind when n has a direct child token of that kind.
func rsKeyword(n *sitter.Node, kind string) string {
	if HasChild(n, kind) {
		return kind
	}
	return ""
}

// rsTestModule reports whether a mod item is compiled for tests only:
// #[cfg(test)] before it, or #![cfg(test)] at the top of its body.
func rsTestModule(b *builder, n *sitter.Node) bool {
	for p := n.PrevNamedSibling(); p != nil; p = p.PrevNamedSibling() {
		if IsComment(p) {
			continue
		}
		if p.Kind() != "attribute_item" {
			break
		}
		if b.Norm(ChildOfKind(p, "attribute")) == "cfg(test)" {
			return true
		}
	}
	for _, c := range Named(Field(n, "body")) {
		if c.Kind() != "inner_attribute_item" {
			break
		}
		if b.Norm(ChildOfKind(c, "attribute")) == "cfg(test)" {
			return true
		}
	}
	return false
}

// rsScope is what the refs walk knows about the code around a node.
type rsScope struct {
	generics map[string]bool // type parameters in scope: their uses are not refs
	self     string          // the impl's type as written, which Self stands for
}

// rsEnter returns the scope inside n: its type parameters are added, and
// impls and traits set what Self means.
func rsEnter(b *builder, n *sitter.Node, kind string, s rsScope) rsScope {
	switch kind {
	case "impl_item":
		s.self = rsPath(b, rsStripRefs(Field(n, "type")))
	case "trait_item":
		s.self = ""
	case "function_item", "function_signature_item", "struct_item", "union_item",
		"enum_item", "type_item", "associated_type":
	default:
		return s
	}
	params := Named(Field(n, "type_parameters"))
	if len(params) == 0 {
		return s
	}
	generics := make(map[string]bool, len(s.generics)+len(params))
	for name := range s.generics {
		generics[name] = true
	}
	for _, p := range params {
		if p.Kind() == "type_parameter" || p.Kind() == "const_parameter" {
			generics[b.Text(Field(p, "name"))] = true
		}
	}
	s.generics = generics
	return s
}

// rsRefs records the refs and imports under n.
func rsRefs(b *builder, n *sitter.Node, s rsScope) {
	if n == nil {
		return
	}
	kind := n.Kind()
	s = rsEnter(b, n, kind, s)
	var skip []*sitter.Node // children handled here
	switch kind {
	case "use_declaration":
		rsUseTree(b, Field(n, "argument"), "")
		return
	case "macro_definition", "attribute_item", "inner_attribute_item":
		return
	case "mod_item":
		if rsTestModule(b, n) {
			return
		}
		// `mod config;` binds config to a child module in another file, as
		// `use self::config;` would, so config::load() can be resolved.
		// Inline modules are found from their items' containers.
		if name := b.Text(Field(n, "name")); Field(n, "body") == nil && name != "" {
			b.Import(Import{Kind: ImportNamespace, Module: "self::" + name, Local: name}, n)
		}
	case "macro_invocation":
		rsMacroRefs(b, ChildOfKind(n, "token_tree"), s)
		return
	case "call_expression":
		rsCall(b, n, s)
	case "struct_expression":
		name := Field(n, "name")
		rsConstruct(b, name, s)
		skip = append(skip, name)
	case "type_identifier":
		if t := b.Text(n); !s.generics[t] && !rsBuiltinType(t) {
			b.Ref(t, "", model.RefType, n)
		}
		return
	case "scoped_type_identifier":
		rsScopedRef(b, n, model.RefType, s)
		return
	case "impl_item":
		trait := Field(n, "trait")
		if !HasChild(n, "!") { // impl !Send for T implements nothing
			rsExtends(b, trait, s)
		}
		skip = append(skip, trait)
	case "trait_item":
		bounds := Field(n, "bounds")
		for _, c := range Named(bounds) {
			rsExtends(b, c, s)
		}
		skip = append(skip, Field(n, "name"), bounds)
	case "struct_item", "union_item", "enum_item", "type_item", "associated_type", "type_parameter", "type_binding":
		skip = append(skip, Field(n, "name")) // a definition, not a use
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && !rsIsAny(c, skip) {
			rsRefs(b, c, s)
		}
	}
}

// rsIsAny reports whether n is one of nodes.
func rsIsAny(n *sitter.Node, nodes []*sitter.Node) bool {
	for _, x := range nodes {
		if x != nil && x.Id() == n.Id() {
			return true
		}
	}
	return false
}

// rsCall records a call: f(), a::b::f(), Type::new(), x.f(), f::<T>().
// Self::f() inside an impl is qualified with the impl's type. A method
// call x.f() is recorded on a receiver, which keeps it apart from the path
// x::f() although both have the qualifier "x".
func rsCall(b *builder, n *sitter.Node, s rsScope) {
	fn := Field(n, "function")
	if fn != nil && fn.Kind() == "generic_function" {
		fn = Field(fn, "function")
	}
	if fn == nil {
		return
	}
	switch fn.Kind() {
	case "identifier":
		b.Ref(b.Text(fn), "", model.RefCall, fn)
	case "scoped_identifier":
		name := Field(fn, "name")
		b.Ref(b.Text(name), rsSelfPath(rsPath(b, Field(fn, "path")), s), model.RefCall, name)
	case "field_expression":
		if field := Field(fn, "field"); field != nil && field.Kind() == "field_identifier" {
			b.MethodCall(b.Text(field), rsReceiver(b, Field(fn, "value"), rsReceiverDepth), field)
		}
	}
}

// rsReceiverDepth is how many steps of a method call's receiver are shown.
const rsReceiverDepth = 12

// rsReceiver renders a method call's receiver compactly: "x", "self.store",
// "a.b()", "Config::load()?". Any other expression becomes "_", so that a
// method call never looks like an unqualified function call. Steps beyond
// depth become "_" too, which keeps long builder chains linear.
func rsReceiver(b *builder, n *sitter.Node, depth int) string {
	if n == nil || depth == 0 {
		return "_"
	}
	depth--
	switch n.Kind() {
	case "identifier", "self":
		return b.Text(n)
	case "scoped_identifier":
		return rsPath(b, n)
	case "field_expression":
		return rsReceiver(b, Field(n, "value"), depth) + "." + b.Text(Field(n, "field"))
	case "call_expression":
		return rsReceiver(b, Field(n, "function"), depth) + "()"
	case "generic_function":
		return rsReceiver(b, Field(n, "function"), depth)
	case "try_expression":
		return rsReceiver(b, n.NamedChild(0), depth) + "?"
	case "await_expression":
		return rsReceiver(b, n.NamedChild(0), depth) + ".await"
	case "index_expression":
		return rsReceiver(b, n.NamedChild(0), depth) + "[]"
	case "parenthesized_expression", "unary_expression":
		return rsReceiver(b, n.NamedChild(0), depth)
	case "reference_expression":
		return rsReceiver(b, Field(n, "value"), depth)
	}
	return "_"
}

// rsConstruct records a struct literal: Config { .. }, a::Config { .. },
// Shape::Rect { .. }, Self { .. } (the impl's type).
func rsConstruct(b *builder, n *sitter.Node, s rsScope) {
	if n == nil {
		return
	}
	if n.Kind() == "generic_type_with_turbofish" {
		rsRefs(b, Field(n, "type_arguments"), s)
		n = Field(n, "type")
	}
	switch {
	case n == nil:
	case n.Kind() == "type_identifier" && b.Text(n) == "Self":
		if s.self != "" {
			q, name := rsSplitPath(s.self)
			b.Ref(name, q, model.RefConstruct, n)
		}
	case n.Kind() == "type_identifier":
		if t := b.Text(n); !s.generics[t] {
			b.Ref(t, "", model.RefConstruct, n)
		}
	case n.Kind() == "scoped_type_identifier" || n.Kind() == "scoped_identifier":
		rsScopedRef(b, n, model.RefConstruct, s)
	}
}

// rsScopedRef records a ref to a path such as a::b::T, skipping associated
// types reached through Self, a type parameter or a qualified type
// (<T as Trait>::Output). Generic arguments inside the path are walked.
func rsScopedRef(b *builder, n *sitter.Node, kind model.RefKind, s rsScope) {
	path := Field(n, "path")
	q := rsPath(b, path)
	first, _, _ := strings.Cut(q, "::")
	switch {
	case path != nil && path.Kind() == "bracketed_type":
	case s.generics[first]:
	case first == "Self" && (kind == model.RefType || s.self == ""):
	default:
		b.Ref(b.Text(Field(n, "name")), rsSelfPath(q, s), kind, n)
	}
	rsRefs(b, path, s)
}

// rsExtends records an extends ref to the trait t of an impl or a supertrait
// bound, and type refs for its generic arguments.
func rsExtends(b *builder, t *sitter.Node, s rsScope) {
	if t == nil {
		return
	}
	switch t.Kind() {
	case "type_identifier":
		b.Ref(b.Text(t), "", model.RefExtends, t)
	case "scoped_type_identifier":
		rsScopedRef(b, t, model.RefExtends, s)
	case "generic_type":
		rsExtends(b, Field(t, "type"), s)
		rsRefs(b, Field(t, "type_arguments"), s)
	case "higher_ranked_trait_bound":
		rsExtends(b, Field(t, "type"), s)
	default: // lifetimes, ?Sized, Fn(T) -> U
		rsRefs(b, t, s)
	}
}

// rsPath renders a path without generic arguments: "a::b", "Vec" for
// Vec::<u8>, "::std::mem", "<T as Trait>".
func rsPath(b *builder, n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind() {
	case "scoped_identifier", "scoped_type_identifier":
		name := b.Text(Field(n, "name"))
		if p := Field(n, "path"); p != nil {
			return rsPath(b, p) + "::" + name
		}
		if HasChild(n, "::") {
			return "::" + name
		}
		return name
	case "generic_type", "generic_type_with_turbofish":
		return rsPath(b, Field(n, "type"))
	}
	return b.Norm(n)
}

// rsSelfPath replaces a leading Self in a path with the impl's type.
func rsSelfPath(path string, s rsScope) string {
	if s.self == "" {
		return path
	}
	if path == "Self" {
		return s.self
	}
	if rest, ok := strings.CutPrefix(path, "Self::"); ok {
		return s.self + "::" + rest
	}
	return path
}

// rsSplitPath splits "a::b::C" into "a::b" and "C".
func rsSplitPath(path string) (qualifier, name string) {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[:i], path[i+2:]
	}
	return "", path
}

// rsBuiltinType reports whether name is a primitive type or Self, which are
// never refs. Keywords count too: error recovery can leave one where a type
// was expected.
func rsBuiltinType(name string) bool {
	switch name {
	case "Self", "bool", "char", "str", "u8", "u16", "u32", "u64", "u128", "usize",
		"i8", "i16", "i32", "i64", "i128", "isize", "f32", "f64":
		return true
	}
	return rsKeywords[name]
}

// rsMacroRefs records the calls written inside a macro invocation, whose
// arguments tree-sitter leaves as unparsed tokens: an identifier directly
// followed by a parenthesized group, as in assert_eq!(parse(x), y) or
// vec![a::b(), x.c()]. The qualifier is the `::` path or the `.` receiver
// in front of it, kept apart as rsCall does. This finds most calls in
// format!, assert! and vec! arguments; struct literals and types inside
// macros are not recorded.
func rsMacroRefs(b *builder, tt *sitter.Node, s rsScope) {
	if tt == nil {
		return
	}
	toks := make([]*sitter.Node, 0, tt.ChildCount())
	for i := uint(0); i < tt.ChildCount(); i++ {
		if c := tt.Child(i); c != nil && !IsComment(c) {
			toks = append(toks, c)
		}
	}
	for i, c := range toks {
		if c.Kind() == "token_tree" {
			rsMacroRefs(b, c, s)
			continue
		}
		if c.Kind() != "identifier" || i+1 >= len(toks) || !rsParenGroup(toks[i+1]) || rsKeywords[b.Text(c)] {
			continue
		}
		q, ok := rsMacroQualifier(b, toks[:i])
		switch {
		case !ok:
		case i > 0 && toks[i-1].Kind() == ".":
			b.MethodCall(b.Text(c), q, c)
		default:
			b.Ref(b.Text(c), rsSelfPath(q, s), model.RefCall, c)
		}
	}
}

// rsParenGroup reports whether n is a token tree in parentheses.
func rsParenGroup(n *sitter.Node) bool {
	if n.Kind() != "token_tree" {
		return false
	}
	open := n.Child(0)
	return open != nil && open.Kind() == "("
}

// rsMacroQualifier reads the path or receiver at the end of toks, the
// tokens before a called identifier. It reports false when the identifier
// is not a call: a definition (`fn name(`), a metavariable, or a path it
// cannot read (Vec::<u8>::new).
func rsMacroQualifier(b *builder, toks []*sitter.Node) (string, bool) {
	n := len(toks)
	if n == 0 {
		return "", true
	}
	sep := toks[n-1].Kind()
	if sep != "::" && sep != "." {
		switch sep {
		case "fn", "struct", "enum", "union", "trait", "type", "mod", "$":
			return "", false
		}
		return "", true
	}
	var segs []string
	for i := n - 1; i >= 1 && toks[i].Kind() == sep && rsPathSegment(toks[i-1]); i -= 2 {
		segs = append([]string{b.Text(toks[i-1])}, segs...)
	}
	switch {
	case len(segs) > 0:
		return strings.Join(segs, sep), true
	case sep == ".":
		return "_", true // the receiver is a call or another expression
	}
	return "", false
}

// rsPathSegment reports whether a macro token can be one segment of a path
// or a receiver chain.
func rsPathSegment(n *sitter.Node) bool {
	switch n.Kind() {
	case "identifier", "self", "super", "crate":
		return true
	}
	return false
}

// rsKeywords are words tree-sitter can leave as identifiers inside macro
// arguments; "if (x)" or "match (a, b)" are not calls.
var rsKeywords = map[string]bool{
	"as": true, "async": true, "await": true, "break": true, "const": true, "continue": true,
	"crate": true, "dyn": true, "else": true, "enum": true, "extern": true, "false": true,
	"fn": true, "for": true, "if": true, "impl": true, "in": true, "let": true, "loop": true,
	"match": true, "mod": true, "move": true, "mut": true, "pub": true, "ref": true,
	"return": true, "self": true, "Self": true, "static": true, "struct": true, "super": true,
	"trait": true, "true": true, "type": true, "unsafe": true, "use": true, "where": true,
	"while": true, "yield": true,
}

// rsUseTree records the imports of one use tree under the path prefix
// written before it:
//
//	use a::b::c;          named c from a::b
//	use a::b::c as d;     named c from a::b, bound to d
//	use a::b::{self, c};  namespace a::b bound to b, named c from a::b
//	use a::b::*;          glob a::b
//	use foo;              namespace foo
func rsUseTree(b *builder, n *sitter.Node, prefix string) {
	if n == nil {
		return
	}
	switch n.Kind() {
	case "identifier", "crate", "super":
		name := b.Text(n)
		if prefix == "" {
			b.Import(Import{Kind: ImportNamespace, Module: name, Local: name}, n)
		} else {
			b.Import(Import{Kind: ImportNamed, Module: prefix, Name: name, Local: name}, n)
		}
	case "self":
		if prefix != "" {
			_, last := rsSplitPath(prefix)
			b.Import(Import{Kind: ImportNamespace, Module: prefix, Local: last}, n)
		}
	case "scoped_identifier":
		path := Field(n, "path")
		if path == nil { // use ::name;
			rsUseTree(b, Field(n, "name"), prefix)
			return
		}
		name := b.Text(Field(n, "name"))
		b.Import(Import{Kind: ImportNamed, Module: rsJoin(prefix, b.Norm(path), "::"), Name: name, Local: name}, n)
	case "use_as_clause":
		path, alias := Field(n, "path"), b.Text(Field(n, "alias"))
		if alias == "_" {
			alias = "" // use Trait as _ binds no name
		}
		switch {
		case path == nil:
		case path.Kind() == "scoped_identifier" && Field(path, "path") != nil:
			module := rsJoin(prefix, b.Norm(Field(path, "path")), "::")
			b.Import(Import{Kind: ImportNamed, Module: module, Name: b.Text(Field(path, "name")), Local: alias}, n)
		case path.Kind() == "self":
			b.Import(Import{Kind: ImportNamespace, Module: prefix, Local: alias}, n)
		case prefix == "":
			b.Import(Import{Kind: ImportNamespace, Module: b.Norm(path), Local: alias}, n)
		default:
			b.Import(Import{Kind: ImportNamed, Module: prefix, Name: b.Norm(path), Local: alias}, n)
		}
	case "use_wildcard":
		module := prefix
		if path := n.NamedChild(0); path != nil {
			module = rsJoin(prefix, b.Norm(path), "::")
		}
		b.Import(Import{Kind: ImportGlob, Module: module}, n)
	case "scoped_use_list":
		rsUseTree(b, Field(n, "list"), rsJoin(prefix, b.Norm(Field(n, "path")), "::"))
	case "use_list":
		for _, c := range Named(n) {
			rsUseTree(b, c, prefix)
		}
	}
}

// rsJoin joins two path parts with sep, leaving out empty parts.
func rsJoin(a, b, sep string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + sep + b
}

// rsTrailingComma matches a comma before a closing bracket in Norm output.
var rsTrailingComma = strings.NewReplacer(",>", ">", ",)", ")", ",]", "]", ",}", "}")

// rsNorm is Norm for types, generics and where clauses, without the
// trailing commas rustfmt adds when it breaks a list over several lines:
// "<T:Clone,U,>" and "where T:Clone," become "<T:Clone,U>" and
// "where T:Clone".
func rsNorm(b *builder, n *sitter.Node) string {
	return strings.TrimSuffix(rsTrailingComma.Replace(b.Norm(n)), ",")
}

// rsWords joins the non-empty words with spaces.
func rsWords(words ...string) string {
	var sb strings.Builder
	for _, w := range words {
		if w == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(w)
	}
	return sb.String()
}
