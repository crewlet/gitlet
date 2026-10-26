package footprint

import (
	"testing"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// shape is the part of a symbol Classify looks at.
type shape struct {
	lang      string
	kind      string
	name      string // "f" when empty
	container string
	sig       string
	exported  bool
	params    []model.Param
	results   string
	members   []model.Member
}

func (s shape) symbol() model.Symbol {
	if s.name == "" {
		s.name = "f"
	}
	return model.Symbol{
		Lang:      s.lang,
		Kind:      s.kind,
		Name:      s.name,
		Container: s.container,
		Signature: s.sig,
		SigHash:   symbols.SigHash(s.kind + "|" + s.sig),
		Exported:  s.exported,
		Params:    s.params,
		Results:   s.results,
		Members:   s.members,
	}
}

func p(typ string) model.Param        { return model.Param{Type: typ} }
func named(n, typ string) model.Param { return model.Param{Name: n, Type: typ} }
func opt(p model.Param) model.Param   { p.Optional = true; return p }
func rest(p model.Param) model.Param  { p.Variadic = true; return p }
func field(n, typ string) model.Member {
	return model.Member{Name: n, Type: typ}
}
func optField(n, typ string) model.Member { return model.Member{Name: n, Type: typ, Optional: true} }
func method(n, sig string) model.Member   { return model.Member{Name: n, Type: sig, Method: true} }

func TestClassify(t *testing.T) {
	goFn := func(sig string, params []model.Param, results string) shape {
		return shape{lang: "go", kind: "function", sig: sig, params: params, results: results, exported: true}
	}
	tsFn := func(sig string, params ...model.Param) shape {
		return shape{lang: "typescript", kind: "function", sig: sig, params: params, exported: true}
	}
	pyFn := func(sig string, params ...model.Param) shape {
		return shape{lang: "python", kind: "function", sig: sig, params: params, exported: true}
	}
	rsFn := func(sig string, params ...model.Param) shape {
		return shape{lang: "rust", kind: "function", sig: sig, params: params, exported: true}
	}
	typ := func(lang, kind, sig string, members ...model.Member) shape {
		return shape{lang: lang, kind: kind, name: "T", sig: sig, members: members, exported: true}
	}
	withResults := func(s shape, r string) shape { s.results = r; return s }
	withExported := func(s shape, e bool) shape { s.exported = e; return s }
	withLang := func(s shape, l string) shape { s.lang = l; return s }

	tests := []struct {
		name     string
		old, new shape
		want     bool
	}{
		// Hashes decide first.
		{"equal hashes never break", goFn("func f(int)", []model.Param{p("int")}, ""),
			goFn("func f(int)", []model.Param{p("string")}, "error"), false},

		// Go.
		{"go: variadic appended", goFn("func f(int)", []model.Param{p("int")}, ""),
			goFn("func f(int, ...Option)", []model.Param{p("int"), rest(p("Option"))}, ""), false},
		{"go: parameter appended", goFn("func f(int)", []model.Param{p("int")}, ""),
			goFn("func f(int, bool)", []model.Param{p("int"), p("bool")}, ""), true},
		{"go: parameter removed", goFn("func f(int, bool)", []model.Param{p("int"), p("bool")}, ""),
			goFn("func f(int)", []model.Param{p("int")}, ""), true},
		{"go: parameters reordered", goFn("func f(int, string)", []model.Param{p("int"), p("string")}, ""),
			goFn("func f(string, int)", []model.Param{p("string"), p("int")}, ""), true},
		{"go: parameter type changed", goFn("func f(int)", []model.Param{p("int")}, ""),
			goFn("func f(int64)", []model.Param{p("int64")}, ""), true},
		{"go: variadic became plain", goFn("func f(...int)", []model.Param{rest(p("int"))}, ""),
			goFn("func f([]int)", []model.Param{p("[]int")}, ""), true},
		{"go: result changed", goFn("func f() error", nil, "error"), goFn("func f() (int, error)", nil, "(int,error)"), true},
		{"go: result added", goFn("func f()", nil, ""), goFn("func f() error", nil, "error"), true},
		{"go: receiver changed, nothing structured differs",
			shape{lang: "go", kind: "method", name: "M", container: "T", sig: "func (T) M()", exported: true},
			shape{lang: "go", kind: "method", name: "M", container: "T", sig: "func (*T) M()", exported: true}, true},
		{"go: type parameters changed", goFn("func f[T any](T)", []model.Param{p("T")}, ""),
			goFn("func f[T comparable](T)", []model.Param{p("T")}, ""), true},
		{"go: struct field added", typ("go", "struct", "type T struct{A int}", field("A", "int")),
			typ("go", "struct", "type T struct{A int;B string}", field("A", "int"), field("B", "string")), false},
		{"go: struct field removed", typ("go", "struct", "type T struct{A int;B string}", field("A", "int"), field("B", "string")),
			typ("go", "struct", "type T struct{A int}", field("A", "int")), true},
		{"go: struct field type changed", typ("go", "struct", "type T struct{A int}", field("A", "int")),
			typ("go", "struct", "type T struct{A int64}", field("A", "int64")), true},
		{"go: interface method added", typ("go", "interface", "type T interface{Read()}", method("Read", "Read()")),
			typ("go", "interface", "type T interface{Read();Close()}", method("Read", "Read()"), method("Close", "Close()")), true},
		{"go: interface optional-looking member added", typ("go", "interface", "type T interface{}"),
			typ("go", "interface", "type T interface{io.Reader}", optField("Reader", "io.Reader")), true},
		{"go: defined type changed", typ("go", "type", "type T int"), typ("go", "type", "type T int64"), true},
		{"go: kind changed", typ("go", "struct", "type T struct{}"), typ("go", "interface", "type T interface{}"), true},

		// TypeScript and JavaScript.
		{"ts: optional parameter appended", tsFn("function f(string)", p("string")),
			tsFn("function f(string,number?)", p("string"), opt(p("number"))), false},
		{"ts: rest parameter appended", tsFn("function f(string)", p("string")),
			tsFn("function f(string,...number[])", p("string"), rest(p("number[]"))), false},
		{"ts: required parameter appended", tsFn("function f(string)", p("string")),
			tsFn("function f(string,number)", p("string"), p("number")), true},
		{"ts: parameter names do not count", tsFn("function f(string)", named("a", "string")),
			tsFn("function f(string,number?)", named("b", "string"), opt(named("c", "number"))), false},
		{"ts: annotation added", tsFn("function f(?)", p("")), tsFn("function f(string)", p("string")), false},
		{"ts: annotation changed", tsFn("function f(string)", p("string")), tsFn("function f(number)", p("number")), true},
		{"ts: annotation removed", tsFn("function f(string)", p("string")), tsFn("function f(?)", p("")), true},
		{"ts: parameter made optional", tsFn("function f(string)", p("string")), tsFn("function f(string?)", opt(p("string"))), true},
		{"ts: return annotation added", tsFn("function f()"), withResults(tsFn("function f():Config"), "Config"), false},
		{"ts: return type changed", withResults(tsFn("function f():Config"), "Config"),
			withResults(tsFn("function f():Settings"), "Settings"), true},
		{"ts: visibility widened", withExported(tsFn("function f():void"), false), tsFn("export function f():void"), false},
		{"ts: default export widened", withExported(tsFn("function f():void"), false), tsFn("export default function f():void"), false},
		{"ts: visibility narrowed", tsFn("export function f():void"), withExported(tsFn("function f():void"), false), true},
		{"ts: visibility widened with a hidden change", withExported(tsFn("function f():void"), false),
			tsFn("export async function f():void"), true},
		{"ts: visibility widened with an allowed change", withExported(tsFn("function f(string)", p("string")), false),
			tsFn("export function f(string,number?)", p("string"), opt(p("number"))), false},
		{"ts: overload group changed", tsFn("function f(string):void;function f(number):void"),
			tsFn("function f(string):void;function f(boolean):void"), true},
		{"ts: interface optional member added", typ("typescript", "interface", "interface T{a:string}", field("a", "string")),
			typ("typescript", "interface", "interface T{a:string;b?:number}", field("a", "string"), optField("b", "number")), false},
		{"ts: interface optional method added", typ("typescript", "interface", "interface T{}"),
			typ("typescript", "interface", "interface T{m?():void}", model.Member{Name: "m", Type: "():void", Method: true, Optional: true}), false},
		{"ts: interface required member added", typ("typescript", "interface", "interface T{a:string}", field("a", "string")),
			typ("typescript", "interface", "interface T{a:string;b:number}", field("a", "string"), field("b", "number")), true},
		{"ts: interface member made optional", typ("typescript", "interface", "interface T{a:string}", field("a", "string")),
			typ("typescript", "interface", "interface T{a?:string}", optField("a", "string")), true},
		{"ts: interface member removed", typ("typescript", "interface", "interface T{a:string}", field("a", "string")),
			typ("typescript", "interface", "interface T{}"), true},
		{"ts: interface overload added", typ("typescript", "interface", "interface T{m(a)}", method("m", "(a)")),
			typ("typescript", "interface", "interface T{m(a);m(b)}", method("m", "(a)"), method("m", "(b)")), true},
		{"ts: interface member annotation added", typ("typescript", "interface", "interface T{a}", field("a", "")),
			typ("typescript", "interface", "interface T{a:string}", field("a", "string")), false},
		{"ts: class optional field added", typ("typescript", "class", "class T{a:string}", field("a", "string")),
			typ("typescript", "class", "class T{a:string;b?:number}", field("a", "string"), optField("b", "number")), false},
		{"ts: class required field added", typ("typescript", "class", "class T{a:string}", field("a", "string")),
			typ("typescript", "class", "class T{a:string;b:number}", field("a", "string"), field("b", "number")), true},
		{"ts: class bases changed", typ("typescript", "class", "class T extends A"), typ("typescript", "class", "class T extends B"), true},
		{"ts: type alias member added", typ("typescript", "type", "type T={a:string}", field("a", "string")),
			typ("typescript", "type", "type T={a:string;b?:number}", field("a", "string"), optField("b", "number")), true},
		{"ts: enum member added", typ("typescript", "enum", "enum T{A}", field("A", "")),
			typ("typescript", "enum", "enum T{A,B}", field("A", ""), field("B", "")), true},
		{"js: defaulted parameter appended", withLang(tsFn("function f()"), "javascript"),
			withLang(tsFn("function f(?=)", opt(p(""))), "javascript"), false},
		{"js to ts: annotation added", withLang(tsFn("function f(?)", p("")), "javascript"), tsFn("function f(string)", p("string")), false},

		// Python.
		{"py: defaulted parameter appended", pyFn("def f(a)", named("a", "")), pyFn("def f(a,b=)", named("a", ""), opt(named("b", ""))), false},
		{"py: required parameter appended", pyFn("def f(a)", named("a", "")), pyFn("def f(a,b)", named("a", ""), named("b", "")), true},
		{"py: parameter renamed", pyFn("def f(a)", named("a", "")), pyFn("def f(b)", named("b", "")), true},
		{"py: annotation added", pyFn("def f(a)", named("a", "")), pyFn("def f(a:int)", named("a", "int")), false},
		{"py: annotation changed", pyFn("def f(a:int)", named("a", "int")), pyFn("def f(a:str)", named("a", "str")), true},
		{"py: return annotation added", pyFn("def f()"), withResults(pyFn("def f()->int"), "int"), false},
		{"py: *args appended", pyFn("def f(a)", named("a", "")), pyFn("def f(a,*args)", named("a", ""), rest(named("args", ""))), false},
		{"py: **kwargs appended", pyFn("def f(a)", named("a", "")), pyFn("def f(a,**kw)", named("a", ""), rest(named("kw", ""))), false},
		{"py: *args renamed", pyFn("def f(*args)", rest(named("args", ""))), pyFn("def f(*rest)", rest(named("rest", ""))), true},
		{"py: keyword-only optional appended", pyFn("def f(a)", named("a", "")),
			pyFn("def f(a,*,k=)", named("a", ""), model.Param{Name: "k", Optional: true, KeywordOnly: true}), false},
		{"py: keyword-only required appended", pyFn("def f(a)", named("a", "")),
			pyFn("def f(a,*,k)", named("a", ""), model.Param{Name: "k", KeywordOnly: true}), true},
		{"py: parameter became keyword-only", pyFn("def f(a)", named("a", "")),
			pyFn("def f(*,a)", model.Param{Name: "a", KeywordOnly: true}), true},
		{"py: class optional attribute added", typ("python", "class", "class T:a", field("a", "")),
			typ("python", "class", "class T:a;b=", field("a", ""), optField("b", "")), false},
		{"py: class required field added", typ("python", "class", "class T:a", field("a", "")),
			typ("python", "class", "class T:a;b:int", field("a", ""), field("b", "int")), true},
		{"py: class attribute annotated", typ("python", "class", "class T:a", field("a", "")),
			typ("python", "class", "class T:a:int", field("a", "int")), false},

		// Rust.
		{"rust: parameter appended", rsFn("fn f(i32)", p("i32")), rsFn("fn f(i32,bool)", p("i32"), p("bool")), true},
		{"rust: pub added", withExported(rsFn("fn f()"), false), rsFn("pub fn f()"), false},
		{"rust: pub(crate) added", withExported(rsFn("fn f()"), false), rsFn("pub(crate)fn f()"), false},
		{"rust: pub removed", rsFn("pub fn f()"), withExported(rsFn("fn f()"), false), true},
		{"rust: const type changed while widened",
			shape{lang: "rust", kind: "const", name: "X", sig: "const X:u32"},
			shape{lang: "rust", kind: "const", name: "X", sig: "pub const X:u64", exported: true}, true},
		{"rust: trait method added", typ("rust", "trait", "trait T{fn a()}", method("a", "fn a()")),
			typ("rust", "trait", "trait T{fn a();fn b()}", method("a", "fn a()"), method("b", "fn b()")), true},
		{"rust: trait provided method added", typ("rust", "trait", "trait T{}"),
			typ("rust", "trait", "trait T{fn b(){}}", model.Member{Name: "b", Type: "fn b()", Method: true, Optional: true}), true},
		{"rust: struct field added", typ("rust", "struct", "struct T{a:i32}", field("a", "i32")),
			typ("rust", "struct", "struct T{a:i32,b:i32}", field("a", "i32"), field("b", "i32")), true},
		{"rust: enum variant added", typ("rust", "enum", "enum T{A}", field("A", "")),
			typ("rust", "enum", "enum T{A,B}", field("A", ""), field("B", "")), true},

		// Reordering.
		{"go: interface methods reordered", typ("go", "interface", "type T interface{A();B()}", method("A", "()"), method("B", "()")),
			typ("go", "interface", "type T interface{B();A()}", method("B", "()"), method("A", "()")), false},
		{"go: struct fields reordered", typ("go", "struct", "type T struct{A int;B int}", field("A", "int"), field("B", "int")),
			typ("go", "struct", "type T struct{B int;A int}", field("B", "int"), field("A", "int")), false},
		{"go: interface reordered with a method added", typ("go", "interface", "type T interface{A();B()}", method("A", "()"), method("B", "()")),
			typ("go", "interface", "type T interface{B();A();C()}", method("B", "()"), method("A", "()"), method("C", "()")), true},
		{"rust: trait items reordered", typ("rust", "trait", "trait T{fn a();fn b()}", method("a", "fn a()"), method("b", "fn b()")),
			typ("rust", "trait", "trait T{fn b();fn a()}", method("b", "fn b()"), method("a", "fn a()")), false},
		{"rust: enum variants reordered", typ("rust", "enum", "enum T{A,B}", field("A", ""), field("B", "")),
			typ("rust", "enum", "enum T{B,A}", field("B", ""), field("A", "")), true},
		{"ts: enum members reordered", typ("typescript", "enum", "enum T{A,B}", field("A", ""), field("B", "")),
			typ("typescript", "enum", "enum T{B,A}", field("B", ""), field("A", "")), true},

		// Python parameter names.
		{"py: positional-only parameter gets a name", pyFn("def f(a, /)", named("", "")), pyFn("def f(a)", named("a", "")), false},
		{"py: parameter becomes positional-only", pyFn("def f(a)", named("a", "")), pyFn("def f(a, /)", named("", "")), true},
		{"py: setter value annotated", pyFn("setter def T.x(value)", named("", "")), pyFn("setter def T.x(value: int)", named("", "int")), false},
		{"py: overloads changed", pyFn("def f(a: int) | def f(a: str)"), pyFn("def f(a: int) | def f(a: bytes)"), true},

		// Visibility ranks.
		{"rust: pub(crate) widened to pub", withExported(rsFn("pub(crate) fn f()"), false), rsFn("pub fn f()"), false},
		{"rust: private widened to pub(crate)", withExported(rsFn("fn f()"), false), withExported(rsFn("pub(crate) fn f()"), false), false},
		{"rust: pub(super) widened to pub(crate)", withExported(rsFn("pub(super) fn f()"), false), withExported(rsFn("pub(crate) fn f()"), false), false},
		{"rust: pub narrowed to pub(crate)", rsFn("pub fn f()"), withExported(rsFn("pub(crate) fn f()"), false), true},
		{"rust: pub(crate) narrowed to private", withExported(rsFn("pub(crate) fn f()"), false), withExported(rsFn("fn f()"), false), true},
		{"rust: pub(crate) narrowed while a parameter type changes", withExported(rsFn("pub(crate) fn f(u8)", p("u8")), false),
			withExported(rsFn("fn f(u16)", p("u16")), false), true},
		{"ts: private widened to protected",
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "private C.m()"},
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "protected C.m()"}, false},
		{"ts: protected widened to public",
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "protected C.m()"},
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "C.m()", exported: true}, false},
		{"ts: protected narrowed to private",
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "protected C.m()"},
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "private C.m()"}, true},
		{"ts: private member with an optional parameter added",
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "private C.m()"},
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "private C.m(?)", params: []model.Param{opt(p(""))}}, false},

		// Identity.
		{"name changed", goFn("func f()", nil, ""), shape{lang: "go", kind: "function", name: "g", sig: "func g()", exported: true}, true},
		{"container changed",
			shape{lang: "go", kind: "method", name: "M", container: "T", sig: "func (T) M()"},
			shape{lang: "go", kind: "method", name: "M", container: "U", sig: "func (U) M()"}, true},
		{"language family changed", pyFn("def f()"), withLang(pyFn("fn f()"), "rust"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.old.symbol(), tt.new.symbol()); got != tt.want {
				t.Errorf("Classify = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripVisibility(t *testing.T) {
	tests := []struct{ in, want string }{
		{"export function f():void", "function f():void"},
		{"export default class A", "class A"},
		{"pub fn f()", "fn f()"},
		{"pub(crate)fn f()", "fn f()"},
		{"pub(in crate::a) fn f()", "fn f()"},
		{"public  m(string)", "m(string)"},
		{"private readonly x", "readonly x"},
		{"function publicly()", "function publicly()"},
		{"fn republish()", "fn republish()"},
	}
	for _, tt := range tests {
		if got := stripVisibility(tt.in); got != tt.want {
			t.Errorf("stripVisibility(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
