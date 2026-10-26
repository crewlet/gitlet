package symbols

import (
	"strings"
	"unicode"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/crewlet/gitlet/internal/model"
)

// extractPython records Python definitions, references and imports
// (docs/SPEC.md, sections 9.2 to 9.5) for .py and .pyi files.
//
// Definitions are module-level functions, classes and assignments to
// UPPER_CASE names, functions in a class body (methods) and classes nested in
// classes. Statements under if, try and with at module or class level count
// as module or class level, since version checks and import fallbacks live
// there. Functions nested in functions are not definitions.
//
// Signatures keep parameter names, because callers can pass them by keyword.
// A default is written "=?" (" = ?" after an annotation), since its value is
// body; self and cls are left out; a marker says how a method is called:
//
//	def load(path: str, strict: bool = ?, *args: Any, key: int, **kw) -> Config
//	async def fetch(url, /, *, retries=?) -> bytes
//	generator def walk(root: str) -> Iterator[str]
//	def Config.save(path: str) -> None
//	classmethod def Config.from_env(prefix: str = ?) -> Config
//	staticmethod def Config.parse(text: str) -> Config
//	property def Config.port -> int
//	setter def Config.port(value: int)
//	deleter def Config.port
//	class Config(Base, metaclass=Meta) {name: str; port: int = ?}
//	MAX: int
//
// @overload stubs merge into one symbol whose signature joins theirs with
// " | ".
func extractPython(b *builder, root *sitter.Node) {
	p := &pyFile{b: b}
	stmts := pyStatements(root)
	p.all = p.dunderAll(stmts)
	p.scope(stmts, "", true)
	p.refs(root)
}

// pyFile is the state of one Python extraction.
type pyFile struct {
	b   *builder
	all map[string]bool // names listed in a literal __all__; nil when there is none
}

// pyTransparent lists the compound statements and clauses whose blocks
// belong to the enclosing module or class body.
var pyTransparent = map[string]bool{
	"if_statement": true, "elif_clause": true, "else_clause": true,
	"try_statement": true, "except_clause": true, "except_group_clause": true, "finally_clause": true,
	"with_statement": true,
}

// pyStatements returns the statements of a module or block, with those of
// if/elif/else, try/except/else/finally and with blocks spliced in place.
func pyStatements(block *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for _, s := range Named(block) {
		if pyTransparent[s.Kind()] {
			out = append(out, pyClauseStatements(s)...)
		} else {
			out = append(out, s)
		}
	}
	return out
}

func pyClauseStatements(n *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for _, c := range Named(n) {
		switch {
		case c.Kind() == "block":
			out = append(out, pyStatements(c)...)
		case pyTransparent[c.Kind()]:
			out = append(out, pyClauseStatements(c)...)
		}
	}
	return out
}

// pyDefinition returns the function or class definition a statement makes,
// looking through decorators, or nil.
func pyDefinition(s *sitter.Node) *sitter.Node {
	d := s
	if s.Kind() == "decorated_definition" {
		d = Field(s, "definition")
	}
	if d != nil && (d.Kind() == "function_definition" || d.Kind() == "class_definition") {
		return d
	}
	return nil
}

// scope adds the definitions among stmts, the statements of the module
// (container "") or of a class body. @overload stubs wait for the
// implementation that follows them; groups without one, as in .pyi stubs, are
// added at the end.
func (p *pyFile) scope(stmts []*sitter.Node, container string, exported bool) {
	pending := map[string][]Def{}
	var order []string
	for _, s := range stmts {
		def := pyDefinition(s)
		switch {
		case def == nil:
			if container == "" {
				p.constants(s)
			}
		case def.Kind() == "class_definition":
			p.class(s, def, container, exported)
		default:
			d, overload, ok := p.function(s, def, container, exported)
			if !ok {
				continue
			}
			key := d.Name + d.Suffix
			if overload {
				if pending[key] == nil {
					order = append(order, key)
				}
				pending[key] = append(pending[key], d)
				continue
			}
			if stubs := pending[key]; stubs != nil {
				d = pyMergeOverloads(stubs, &d)
				delete(pending, key)
			}
			p.b.Add(d)
		}
	}
	for _, key := range order {
		if stubs := pending[key]; stubs != nil {
			p.b.Add(pyMergeOverloads(stubs, nil))
			delete(pending, key)
		}
	}
}

// pyMergeOverloads folds @overload stubs into one definition: the
// implementation when there is one, else the last stub. The signature joins
// the stub signatures with " | "; with several stubs Params and Results stay
// empty, and callers compare signature hashes instead.
func pyMergeOverloads(stubs []Def, impl *Def) Def {
	d := stubs[len(stubs)-1]
	if impl != nil {
		d = *impl
	}
	sigs := make([]string, len(stubs))
	for i, s := range stubs {
		sigs[i] = s.Signature
	}
	d.Signature = strings.Join(sigs, " | ")
	d.Params, d.Results = nil, ""
	if len(stubs) == 1 {
		d.Params, d.Results = stubs[0].Params, stubs[0].Results
	}
	return d
}

// constants adds a module-level assignment to UPPER_CASE names: one symbol
// per name for identifier targets, chained ones included (A = B = 0), and
// annotated declarations without a value (MAX: int). Destructuring,
// attribute and subscript targets are not definitions.
func (p *pyFile) constants(s *sitter.Node) {
	if s.Kind() != "expression_statement" {
		return
	}
	for _, a := range Named(s) {
		for ; a != nil && a.Kind() == "assignment"; a = Field(a, "right") {
			left := Field(a, "left")
			if left == nil || left.Kind() != "identifier" {
				continue
			}
			name := p.b.Text(left)
			if !pyUpperCase(name) {
				continue
			}
			sig := name
			if t := Field(a, "type"); t != nil {
				sig += ": " + p.typeText(t)
			}
			p.b.Add(Def{Node: s, Name: name, Kind: "const", Exported: p.topExported(name), Signature: sig})
		}
	}
}

// class adds a class, then its methods and nested classes. The signature
// lists the bases as written and the exported class-level annotated
// attributes (dataclass-style fields); methods are separate symbols and are
// left out of the class's body hash.
func (p *pyFile) class(node, cls *sitter.Node, container string, containerExported bool) {
	name := p.b.Text(Field(cls, "name"))
	if name == "" {
		return
	}
	exported := containerExported && pyExportedName(name)
	if container == "" {
		exported = p.topExported(name)
	}
	qualified := pyJoin(container, name)
	stmts := pyStatements(Field(cls, "body"))

	var nested []*sitter.Node
	for _, s := range stmts {
		if pyDefinition(s) != nil {
			nested = append(nested, s)
		}
	}
	members, fields := p.members(stmts)
	sig := "class " + qualified + p.b.Norm(Field(cls, "type_parameters")) + p.bases(Field(cls, "superclasses"))
	if len(fields) > 0 {
		sig += " {" + strings.Join(fields, "; ") + "}"
	}
	p.b.Add(Def{
		Node:      node,
		Name:      name,
		Container: container,
		Kind:      "class",
		Exported:  exported,
		Signature: sig,
		Members:   members,
		Nested:    nested,
	})
	p.scope(stmts, qualified, exported)
}

// bases renders a class's argument list without object, which every class
// has: "(Base, m.Mixin, metaclass=Meta)", or "" when nothing is left.
func (p *pyFile) bases(args *sitter.Node) string {
	var parts []string
	for _, a := range Named(args) {
		if a.Kind() == "identifier" && p.b.Text(a) == "object" {
			continue
		}
		parts = append(parts, p.b.Norm(a))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// members returns the exported class-level annotated attributes in order,
// and their signature forms ("port: int = ?"). Attributes assigned on self
// in methods are not members.
func (p *pyFile) members(stmts []*sitter.Node) ([]model.Member, []string) {
	var members []model.Member
	var fields []string
	seen := map[string]bool{}
	for _, s := range stmts {
		if s.Kind() != "expression_statement" {
			continue
		}
		for _, a := range Named(s) {
			left, typ := Field(a, "left"), Field(a, "type")
			if a.Kind() != "assignment" || left == nil || typ == nil || left.Kind() != "identifier" {
				continue
			}
			name := p.b.Text(left)
			if !pyExportedName(name) || seen[name] {
				continue
			}
			seen[name] = true
			m := model.Member{Name: name, Type: p.typeText(typ), Optional: Field(a, "right") != nil}
			members = append(members, m)
			field := m.Name + ": " + m.Type
			if m.Optional {
				field += " = ?"
			}
			fields = append(fields, field)
		}
	}
	return members, fields
}

// function prepares the definition of a function or method. node is the
// statement (the decorated_definition when decorated), fn the
// function_definition. overload reports an @overload stub; ok is false when
// the function has no name.
func (p *pyFile) function(node, fn *sitter.Node, container string, classExported bool) (d Def, overload, ok bool) {
	name := p.b.Text(Field(fn, "name"))
	if name == "" {
		return Def{}, false, false
	}
	d = Def{Node: node, Name: name, Container: container, Kind: "function", Exported: p.topExported(name)}
	if container != "" {
		d.Kind = "method"
		d.Exported = classExported && pyExportedName(name)
	}

	marker := ""
	for _, dec := range pyDecorators(node) {
		qual, dname := p.nameParts(dec)
		if dname == "overload" {
			overload = true
		} else if m := pyMarker(qual, dname); m != "" && container != "" && marker == "" {
			marker = m
		}
	}
	if marker == "setter" || marker == "deleter" {
		d.Suffix = "@" + marker
	}

	var words []string
	if marker != "" {
		words = append(words, marker)
	}
	if HasChild(fn, "async") {
		words = append(words, "async")
	}
	if pyGenerator(Field(fn, "body")) {
		words = append(words, "generator")
	}
	words = append(words, "def", pyJoin(container, name)+p.b.Norm(Field(fn, "type_parameters")))
	sig := strings.Join(words, " ")

	// Getters and deleters are used as attributes: no parameter list.
	if marker != "property" && marker != "deleter" {
		dropFirst := container != "" && marker != "staticmethod"
		list, params := p.params(Field(fn, "parameters"), dropFirst, marker == "setter")
		sig += "(" + list + ")"
		d.Params = params
	}
	if r := Field(fn, "return_type"); r != nil {
		d.Results = p.typeText(r)
		sig += " -> " + d.Results
	}
	d.Signature = sig
	return d, overload, true
}

// params renders a parameter list and returns the parameters callers see.
// dropFirst leaves out self or cls. Positional-only parameters (before "/")
// keep their name in the list but get an empty Name in Params, since callers
// cannot pass them by keyword; anonymous does the same for every parameter
// (a property setter's value). **kwargs is both Variadic and KeywordOnly,
// which tells it apart from *args.
func (p *pyFile) params(n *sitter.Node, dropFirst, anonymous bool) (string, []model.Param) {
	cs := Named(n)
	if dropFirst && len(cs) > 0 && pyPlainParam(cs[0]) {
		cs = cs[1:]
		if len(cs) > 0 && cs[0].Kind() == "positional_separator" {
			cs = cs[1:]
		}
	}
	var parts []string
	var params []model.Param
	keywordOnly := false
	for _, c := range cs {
		var prm model.Param
		prefix := ""
		switch c.Kind() {
		case "positional_separator":
			parts = append(parts, "/")
			for i := range params {
				params[i].Name = ""
			}
			continue
		case "keyword_separator":
			parts = append(parts, "*")
			keywordOnly = true
			continue
		case "identifier", "tuple_pattern":
			prm.Name = p.b.Norm(c)
		case "default_parameter", "typed_default_parameter":
			prm.Name = p.b.Norm(Field(c, "name"))
			prm.Optional = true
		case "list_splat_pattern", "dictionary_splat_pattern", "typed_parameter":
			pat := c
			if c.Kind() == "typed_parameter" {
				if pat = pyTypedPattern(c); pat == nil {
					continue
				}
			}
			prm.Name = p.b.Norm(pat)
			switch pat.Kind() {
			case "list_splat_pattern":
				prefix, prm.Name = "*", strings.TrimPrefix(prm.Name, "*")
				prm.Variadic = true
			case "dictionary_splat_pattern":
				prefix, prm.Name = "**", strings.TrimPrefix(prm.Name, "**")
				prm.Variadic, prm.KeywordOnly = true, true
			}
		default:
			continue // ERROR and other recovery nodes
		}
		if t := Field(c, "type"); t != nil {
			prm.Type = p.typeText(t)
		}
		if keywordOnly && !prm.Variadic {
			prm.KeywordOnly = true
		}
		if prefix == "*" {
			keywordOnly = true
		}

		text := prefix + prm.Name
		if prm.Type != "" {
			text += ": " + prm.Type
		}
		switch {
		case prm.Optional && prm.Type != "":
			text += " = ?"
		case prm.Optional:
			text += "=?"
		}
		parts = append(parts, text)
		if anonymous {
			prm.Name = ""
		}
		params = append(params, prm)
	}
	return strings.Join(parts, ", "), params
}

// pyPlainParam reports whether a parameter can be self or cls: a single
// name, with or without an annotation or default.
func pyPlainParam(n *sitter.Node) bool {
	switch n.Kind() {
	case "identifier", "default_parameter", "typed_default_parameter":
		return true
	case "typed_parameter":
		pat := pyTypedPattern(n)
		return pat != nil && pat.Kind() == "identifier"
	}
	return false
}

// pyTypedPattern returns what a typed_parameter annotates: a name, *args or
// **kwargs, or nil.
func pyTypedPattern(n *sitter.Node) *sitter.Node {
	for _, c := range Named(n) {
		switch c.Kind() {
		case "identifier", "list_splat_pattern", "dictionary_splat_pattern":
			return c
		}
	}
	return nil
}

// pyGenerator reports whether a function body yields, outside nested
// functions, lambdas and classes.
func pyGenerator(body *sitter.Node) bool {
	found := false
	Walk(body, func(n *sitter.Node) bool {
		switch n.Kind() {
		case "yield":
			found = true
		case "function_definition", "class_definition", "lambda":
			return false
		}
		return !found
	})
	return found
}

// pyDecorators returns the expressions of a decorated definition's
// decorators, outermost first.
func pyDecorators(node *sitter.Node) []*sitter.Node {
	if node.Kind() != "decorated_definition" {
		return nil
	}
	var out []*sitter.Node
	for _, c := range Named(node) {
		if c.Kind() != "decorator" {
			continue
		}
		if e := Named(c); len(e) > 0 {
			out = append(out, e[0])
		}
	}
	return out
}

// nameParts splits a name or attribute, such as a decorator, into qualifier
// and name: ("", "property"), ("x", "setter"), ("typing", "overload"). Calls,
// other expressions and nil return empty strings.
func (p *pyFile) nameParts(e *sitter.Node) (qual, name string) {
	if e == nil {
		return "", ""
	}
	switch e.Kind() {
	case "identifier":
		return "", p.b.Text(e)
	case "attribute":
		return p.b.Norm(Field(e, "object")), p.b.Text(Field(e, "attribute"))
	}
	return "", ""
}

// pyMarker returns the signature marker a decorator gives a method, because
// it changes how callers call it: staticmethod, classmethod, property (also
// functools.cached_property), setter or deleter. Other decorators are left
// out of signatures.
func pyMarker(qual, name string) string {
	switch {
	case qual == "" && (name == "staticmethod" || name == "classmethod" || name == "property"):
		return name
	case name == "cached_property":
		return "property"
	case qual != "" && (name == "setter" || name == "deleter"):
		return name
	}
	return ""
}

// typeText normalizes an annotation. A quoted forward reference to a dotted
// name ("Config") reads as the bare name, so adding or removing the quotes is
// no change.
func (p *pyFile) typeText(t *sitter.Node) string {
	if cs := Named(t); t.Kind() == "type" && len(cs) == 1 && cs[0].Kind() == "string" {
		if v, ok := p.stringValue(cs[0]); ok && pyDottedName(strings.TrimSpace(v)) {
			return strings.TrimSpace(v)
		}
	}
	return p.b.Norm(t)
}

// stringValue returns the content of a string literal without
// interpolations, as written between the quotes.
func (p *pyFile) stringValue(n *sitter.Node) (string, bool) {
	if n == nil || n.Kind() != "string" || HasChild(n, "interpolation") {
		return "", false
	}
	start, end := ChildOfKind(n, "string_start"), ChildOfKind(n, "string_end")
	if start == nil || end == nil {
		return "", false
	}
	from, to := start.EndByte(), end.StartByte()
	if from > to || to > uint(len(p.b.src)) {
		return "", false
	}
	return string(p.b.src[from:to]), true
}

// dunderAll returns the names a module lists in __all__, or nil when it has
// no __all__ or builds it from anything but string literals; naming
// conventions decide then. Assignments, += and .append/.extend calls with
// literals count; any other change to __all__ makes it unknown.
func (p *pyFile) dunderAll(stmts []*sitter.Node) map[string]bool {
	var all map[string]bool
	for _, s := range stmts {
		if s.Kind() != "expression_statement" {
			continue
		}
		for _, e := range Named(s) {
			// value stays nil for changes that are not literal names.
			var target, value *sitter.Node
			reset := false
			switch e.Kind() {
			case "assignment":
				target, value, reset = Field(e, "left"), Field(e, "right"), true
			case "augmented_assignment":
				target = Field(e, "left")
				if p.b.Text(Field(e, "operator")) == "+=" {
					value = Field(e, "right")
				}
			case "call":
				fn, args := Field(e, "function"), Field(e, "arguments")
				if fn == nil || fn.Kind() != "attribute" {
					continue
				}
				target = Field(fn, "object")
				switch p.b.Text(Field(fn, "attribute")) {
				case "append":
					value = args
				case "extend":
					if a := Named(args); len(a) == 1 {
						value = a[0]
					}
				}
			default:
				continue
			}
			if target == nil || target.Kind() != "identifier" || p.b.Text(target) != "__all__" {
				continue
			}
			names, ok := p.stringList(value)
			if !ok {
				return nil
			}
			if reset || all == nil {
				all = map[string]bool{}
			}
			for _, name := range names {
				all[name] = true
			}
		}
	}
	return all
}

// stringList returns the strings of a list or tuple of string literals, or
// of the arguments of an __all__.append call.
func (p *pyFile) stringList(n *sitter.Node) ([]string, bool) {
	if n == nil {
		return nil, false
	}
	switch n.Kind() {
	case "list", "tuple", "argument_list":
	default:
		return nil, false
	}
	elems := Named(n)
	names := make([]string, 0, len(elems))
	for _, e := range elems {
		v, ok := p.stringValue(e)
		if !ok {
			return nil, false
		}
		names = append(names, v)
	}
	return names, true
}

// topExported reports whether a top-level name is exported: listed in a
// literal __all__ when the module has one, else not private by convention.
func (p *pyFile) topExported(name string) bool {
	if p.all != nil {
		return p.all[name]
	}
	return pyExportedName(name)
}

// pyExportedName reports whether a name is public by convention: no leading
// underscore, or a dunder such as __init__.
func pyExportedName(name string) bool {
	return !strings.HasPrefix(name, "_") || (len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__"))
}

// pyUpperCase reports whether a name is UPPER_CASE: capital letters, digits
// and underscores, with at least one letter.
func pyUpperCase(name string) bool {
	letter := false
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			letter = true
		case r == '_', r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return letter
}

// pyDottedName reports whether s is a name or dotted name such as "m.Config".
func pyDottedName(s string) bool {
	for part := range strings.SplitSeq(s, ".") {
		if part == "" {
			return false
		}
		for i, r := range part {
			if r != '_' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
				return false
			}
		}
	}
	return true
}

func pyJoin(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}

// refs records the references and imports of the whole file, function
// bodies included: calls (decorators too), annotation types and class bases.
// Python has no construct kind: a call of a class is a call.
func (p *pyFile) refs(root *sitter.Node) {
	Walk(root, func(n *sitter.Node) bool {
		switch n.Kind() {
		case "import_statement", "import_from_statement":
			p.imports(n)
			return false
		case "call":
			p.callRef(Field(n, "function"))
		case "decorator":
			if e := Named(n); len(e) > 0 {
				if qual, name := p.nameParts(e[0]); name != "overload" && pyMarker(qual, name) == "" {
					p.callRef(e[0])
				}
			}
		case "class_definition":
			p.extendsRefs(Field(n, "superclasses"))
		case "type":
			p.typeRefs(n)
			return false
		case "type_alias_statement":
			p.typeRefs(Field(n, "right"))
			return false
		}
		return true
	})
}

// callRef records a call of fn: f() as f, a.b.f() as f with qualifier "a.b",
// self.f() with "self", super().f() with "super()".
func (p *pyFile) callRef(fn *sitter.Node) {
	if fn == nil {
		return
	}
	switch fn.Kind() {
	case "identifier":
		p.b.Ref(p.b.Text(fn), "", model.RefCall, fn)
	case "attribute":
		attr := Field(fn, "attribute")
		p.b.Ref(p.b.Text(attr), p.qualifier(Field(fn, "object")), model.RefCall, attr)
	}
}

// qualifier renders the object of an attribute: a dotted name as written
// ("a.b", "self"), "()" after calls ("super()", "make().x"), "[]" after
// subscripts ("items[]"), and other expressions as normalized text.
func (p *pyFile) qualifier(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind() {
	case "identifier":
		return p.b.Text(n)
	case "attribute":
		return p.qualifier(Field(n, "object")) + "." + p.b.Text(Field(n, "attribute"))
	case "call":
		return p.qualifier(Field(n, "function")) + "()"
	case "subscript":
		return p.qualifier(Field(n, "value")) + "[]"
	case "generic_type":
		if cs := Named(n); len(cs) > 0 {
			return p.qualifier(cs[0]) + "[]"
		}
	case "type":
		if cs := Named(n); len(cs) == 1 {
			return p.qualifier(cs[0])
		}
	}
	return p.b.Norm(n)
}

// pyBuiltinTypes are builtin names that type refs leave out.
var pyBuiltinTypes = map[string]bool{
	"int": true, "float": true, "complex": true, "str": true, "bytes": true, "bytearray": true,
	"bool": true, "list": true, "dict": true, "set": true, "frozenset": true, "tuple": true,
	"object": true, "type": true, "None": true,
}

// typeRefs records the names an annotation refers to: names and dotted
// names, generic arguments (list[Config], Optional[m.Config]), union members
// and quoted forward references to dotted names ("Config"). Builtin types,
// Literal arguments and Annotated metadata are skipped; calls inside go
// through refs.
func (p *pyFile) typeRefs(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Kind() {
	case "identifier":
		if name := p.b.Text(n); !pyBuiltinTypes[name] {
			p.b.Ref(name, "", model.RefType, n)
		}
	case "attribute":
		if !p.dotted(n) {
			p.refs(n)
			return
		}
		attr := Field(n, "attribute")
		p.b.Ref(p.b.Text(attr), p.qualifier(Field(n, "object")), model.RefType, attr)
	case "member_type":
		cs := Named(n)
		if len(cs) == 2 {
			p.b.Ref(p.b.Text(cs[1]), p.qualifier(cs[0]), model.RefType, cs[1])
			p.typeRefs(cs[0])
		}
	case "string":
		v, ok := p.stringValue(n)
		if v = strings.TrimSpace(v); !ok || !pyDottedName(v) {
			return
		}
		qual, name := "", v
		if i := strings.LastIndexByte(v, '.'); i >= 0 {
			qual, name = v[:i], v[i+1:]
		}
		if qual != "" || !pyBuiltinTypes[name] {
			p.b.Ref(name, qual, model.RefType, n)
		}
	case "generic_type", "subscript":
		var base *sitter.Node
		var args []*sitter.Node
		if n.Kind() == "subscript" {
			base, args = Field(n, "value"), Fields(n, "subscript")
		} else if cs := Named(n); len(cs) > 0 {
			base = cs[0]
			for _, tp := range cs[1:] {
				args = append(args, Named(tp)...)
			}
		}
		p.typeRefs(base)
		_, name := p.nameParts(base)
		switch {
		case name == "Literal":
			args = nil
		case name == "Annotated" && len(args) > 1:
			for _, meta := range args[1:] {
				p.refs(meta)
			}
			args = args[:1]
		}
		for _, a := range args {
			p.typeRefs(a)
		}
	case "type", "union_type", "constrained_type", "splat_type", "type_parameter",
		"binary_operator", "list", "tuple", "parenthesized_expression":
		for _, c := range Named(n) {
			p.typeRefs(c)
		}
	case "none", "ellipsis":
	default:
		p.refs(n)
	}
}

// dotted reports whether n is a chain of names joined by dots.
func (p *pyFile) dotted(n *sitter.Node) bool {
	for n != nil && n.Kind() == "attribute" {
		n = Field(n, "object")
	}
	return n != nil && n.Kind() == "identifier"
}

// extendsRefs records the bases of a class: names and dotted names, and the
// base of a subscripted one (Generic[T], Base[Config]) whose arguments are
// type refs. Keyword arguments such as metaclass= and builtins are skipped.
func (p *pyFile) extendsRefs(args *sitter.Node) {
	for _, a := range Named(args) {
		base := a
		if a.Kind() == "subscript" {
			base = Field(a, "value")
			for _, t := range Fields(a, "subscript") {
				p.typeRefs(t)
			}
		}
		switch {
		case base == nil:
		case base.Kind() == "identifier":
			if name := p.b.Text(base); !pyBuiltinTypes[name] {
				p.b.Ref(name, "", model.RefExtends, base)
			}
		case base.Kind() == "attribute" && p.dotted(base):
			attr := Field(base, "attribute")
			p.b.Ref(p.b.Text(attr), p.qualifier(Field(base, "object")), model.RefExtends, attr)
		}
	}
}

// imports records the bindings of an import statement:
//
//	import a.b         {Local "a.b", Module "a.b", namespace}
//	import a.b as c    {Local "c", Module "a.b", namespace}
//	from m import x    {Local "x", Name "x", Module "m", named}
//	from .. import x   {Local "x", Name "x", Module "..", named}
//	from m import *    {Module "m", glob}
//
// from __future__ imports have their own node kind and are not recorded.
func (p *pyFile) imports(n *sitter.Node) {
	if n.Kind() == "import_statement" {
		for _, name := range Fields(n, "name") {
			switch name.Kind() {
			case "dotted_name":
				module := p.b.Norm(name)
				p.b.Import(Import{Local: module, Module: module, Kind: ImportNamespace}, name)
			case "aliased_import":
				p.b.Import(Import{
					Local:  p.b.Text(Field(name, "alias")),
					Module: p.b.Norm(Field(name, "name")),
					Kind:   ImportNamespace,
				}, name)
			}
		}
		return
	}
	module := p.b.Norm(Field(n, "module_name"))
	if module == "" {
		return
	}
	if w := ChildOfKind(n, "wildcard_import"); w != nil {
		p.b.Import(Import{Module: module, Kind: ImportGlob}, w)
		return
	}
	for _, name := range Fields(n, "name") {
		switch name.Kind() {
		case "dotted_name":
			x := p.b.Norm(name)
			p.b.Import(Import{Local: x, Name: x, Module: module, Kind: ImportNamed}, name)
		case "aliased_import":
			p.b.Import(Import{
				Local:  p.b.Text(Field(name, "alias")),
				Name:   p.b.Norm(Field(name, "name")),
				Module: module,
				Kind:   ImportNamed,
			}, name)
		}
	}
}
