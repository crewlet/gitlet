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
	head      string // the signature outside params, results and members; kind and name when empty
	exported  bool
	params    []model.Param
	results   string
	members   []model.Member
}

func (s shape) symbol() model.Symbol {
	if s.name == "" {
		s.name = "f"
	}
	if s.head == "" {
		s.head = s.kind + " " + s.name
	}
	return model.Symbol{
		Lang:      s.lang,
		Kind:      s.kind,
		Name:      s.name,
		Container: s.container,
		Signature: s.sig,
		Head:      s.head,
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
	withHead := func(s shape, h string) shape { s.head = h; return s }

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

		// What the structured fields cannot see must stay as it was, even
		// next to an allowed change.
		{"go: receiver changed with a variadic appended",
			shape{lang: "go", kind: "method", name: "M", container: "T", sig: "func (T) M(int)", head: "func (T) M", params: []model.Param{p("int")}, exported: true},
			shape{lang: "go", kind: "method", name: "M", container: "T", sig: "func (*T) M(int, ...int)", head: "func (*T) M", params: []model.Param{p("int"), rest(p("int"))}, exported: true}, true},
		{"go: type parameter constraint changed with a variadic appended",
			withHead(goFn("func f[T any](T)", []model.Param{p("T")}, ""), "func f[T any]"),
			withHead(goFn("func f[T comparable](T, ...int)", []model.Param{p("T"), rest(p("int"))}, ""), "func f[T comparable]"), true},
		{"go: struct type parameters changed with a field added",
			withHead(typ("go", "struct", "type T[E any] struct {A E}", field("A", "E")), "type T[E any] struct"),
			withHead(typ("go", "struct", "type T[E comparable] struct {A E; B int}", field("A", "E"), field("B", "int")), "type T[E comparable] struct"), true},
		{"ts: async added with an optional parameter",
			withHead(tsFn("function f(string)", p("string")), "function f()"),
			withHead(tsFn("async function f(string, object?)", p("string"), opt(p("object"))), "async function f()"), true},
		{"ts: static added with an optional parameter",
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "C.m(string)", head: "C.m()", params: []model.Param{p("string")}, exported: true},
			shape{lang: "typescript", kind: "method", name: "m", container: "C", sig: "static C.m(string, number?)", head: "static C.m()", params: []model.Param{p("string"), opt(p("number"))}, exported: true}, true},
		{"ts: class base changed with an optional field added",
			withHead(typ("typescript", "class", "class T extends A {a: string}", field("a", "string")), "class T extends A"),
			withHead(typ("typescript", "class", "class T extends B {a: string; b?: number}", field("a", "string"), optField("b", "number")), "class T extends B"), true},
		{"ts: export added with an optional parameter",
			withExported(withHead(tsFn("function f(string)", p("string")), "function f()"), false),
			withHead(tsFn("export function f(string, number?)", p("string"), opt(p("number"))), "export function f()"), false},
		{"py: async added with a defaulted parameter",
			withHead(pyFn("def f(a)", named("a", "")), "def f()"),
			withHead(pyFn("async def f(a, b=?)", named("a", ""), opt(named("b", ""))), "async def f()"), true},
		{"py: property became a method with a defaulted parameter",
			shape{lang: "python", kind: "method", name: "port", container: "C", sig: "property def C.port -> int", head: "property def C.port", results: "int", exported: true},
			shape{lang: "python", kind: "method", name: "port", container: "C", sig: "def C.port(strict=?) -> int", head: "def C.port()",
				params: []model.Param{opt(named("strict", ""))}, results: "int", exported: true}, true},
		{"py: became a generator with a defaulted parameter",
			withHead(pyFn("def f(n)", named("n", "")), "def f()"),
			withHead(pyFn("generator def f(n, step=?)", named("n", ""), opt(named("step", ""))), "generator def f()"), true},
		{"py: class base changed with an optional attribute added",
			withHead(typ("python", "class", "class T(A) {a: int = ?}", optField("a", "int")), "class T(A)"),
			withHead(typ("python", "class", "class T(B) {a: int = ?; b: int = ?}", optField("a", "int"), optField("b", "int")), "class T(B)"), true},
		{"rust: supertrait added with items reordered",
			withHead(typ("rust", "trait", "pub trait T {fn a(); fn b()}", method("a", "fn a()"), method("b", "fn b()")), "pub trait T {}"),
			withHead(typ("rust", "trait", "pub trait T: Send {fn b(); fn a()}", method("b", "fn b()"), method("a", "fn a()")), "pub trait T: Send {}"), true},
		{"a symbol without a head is compared by its whole signature",
			shape{lang: "go", kind: "function", sig: "func f(int)", head: "-", params: []model.Param{p("int")}, exported: true},
			shape{lang: "go", kind: "function", sig: "func f(int, ...int)", head: "-", params: []model.Param{p("int"), rest(p("int"))}, exported: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, new := tt.old.symbol(), tt.new.symbol()
			if old.Head == "-" {
				old.Head, new.Head = "", ""
			}
			if got := Classify(old, new); got != tt.want {
				t.Errorf("Classify = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClassifyExtracted classifies changes between real extractions, so the
// signature parts each extractor leaves outside params, results and members
// are covered.
func TestClassifyExtracted(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		old, new  string
		qualified string
		want      bool
	}{
		// Hidden next to an allowed change.
		{"js: async with a defaulted parameter", "a.js",
			"export function load(path) {}\n", "export async function load(path, opts = {}) {}\n", "load", true},
		{"ts: async with an optional parameter", "a.ts",
			"export function load(path: string) { return 1 }\n", "export async function load(path: string, opts?: object) { return 1 }\n", "load", true},
		{"ts: static with an optional parameter", "a.ts",
			"export class C {\n  m(a: string) {}\n}\n", "export class C {\n  static m(a: string, b?: number) {}\n}\n", "C.m", true},
		{"ts: this type with an optional parameter", "a.ts",
			"export function f(this: A, x: string) {}\n", "export function f(this: B, x: string, y?: number) {}\n", "f", true},
		{"ts: class base with an optional field", "a.ts",
			"export class C extends A {\n  a?: string\n}\n", "export class C extends B {\n  a?: string\n  b?: number\n}\n", "C", true},
		{"py: async with a defaulted parameter", "a.py",
			"def load(path):\n    pass\n", "async def load(path, opts=None):\n    pass\n", "load", true},
		{"py: property became a method with a defaulted parameter", "a.py",
			"class C:\n    @property\n    def port(self) -> int:\n        return 1\n",
			"class C:\n    def port(self, strict=False) -> int:\n        return 1\n", "C.port", true},
		{"py: classmethod with a defaulted parameter", "a.py",
			"class C:\n    def m(self, a):\n        pass\n", "class C:\n    @classmethod\n    def m(cls, a, b=None):\n        pass\n", "C.m", true},
		{"py: generator with a defaulted parameter", "a.py",
			"def gen(n):\n    return [n]\n", "def gen(n, step=1):\n    yield n\n", "gen", true},
		{"py: class base with an optional attribute", "a.py",
			"class C(A):\n    a: int = 1\n", "class C(B):\n    a: int = 1\n    b: int = 2\n", "C", true},
		{"go: type parameter constraint with a variadic parameter", "a.go",
			"package a\n\nfunc F[T any](x T) {}\n", "package a\n\nfunc F[T comparable](x T, o ...int) {}\n", "F", true},
		{"go: pointer receiver with a variadic parameter", "a.go",
			"package a\n\ntype T struct{}\n\nfunc (T) M(x int) {}\n", "package a\n\ntype T struct{}\n\nfunc (*T) M(x int, o ...int) {}\n", "T.M", true},
		{"go: struct type parameters with a field", "a.go",
			"package a\n\ntype L[T any] struct {\n\tA T\n}\n", "package a\n\ntype L[T comparable] struct {\n\tA T\n\tB int\n}\n", "L", true},
		{"rust: supertrait with items reordered", "a.rs",
			"pub trait T {\n    fn a();\n    fn b();\n}\n", "pub trait T: Send {\n    fn b();\n    fn a();\n}\n", "T", true},
		{"rust: private field with fields reordered", "a.rs",
			"pub struct S {\n    pub a: u8,\n    pub b: u8,\n}\n", "pub struct S {\n    pub b: u8,\n    pub a: u8,\n    c: u8,\n}\n", "S", true},

		// Allowed changes alone stay compatible.
		{"js: defaulted parameter", "a.js",
			"export function load(path) {}\n", "export function load(path, opts = {}) {}\n", "load", false},
		{"ts: optional parameter", "a.ts",
			"export function load(path: string) {}\n", "export function load(path: string, opts?: object) {}\n", "load", false},
		{"ts: optional parameter in a method", "a.ts",
			"export class C {\n  private m(a: string) {}\n}\n", "export class C {\n  private m(a: string, b?: number) {}\n}\n", "C.m", false},
		{"ts: optional parameter in a single overload", "a.ts",
			"export function f(a: string): void;\nexport function f(a: any) {}\n",
			"export function f(a: string, b?: number): void;\nexport function f(a: any, b?: any) {}\n", "f", false},
		{"ts: interface optional member", "a.ts",
			"export interface O {\n  a: string\n}\n", "export interface O {\n  a: string\n  b?: number\n}\n", "O", false},
		{"ts: class optional field", "a.ts",
			"export class C extends A {\n  a?: string\n}\n", "export class C extends A {\n  a?: string\n  b?: number\n}\n", "C", false},
		{"ts: return annotation", "a.ts",
			"export function f(a: string) { return a }\n", "export function f(a: string): string { return a }\n", "f", false},
		{"ts: class field function optional parameter", "a.ts",
			"export class C {\n  m = (a: string) => a\n}\n", "export class C {\n  m = (a: string, b?: number) => a\n}\n", "C.m", false},
		{"ts: type literal member annotation", "a.ts",
			"export type T = {\n  a\n}\n", "export type T = {\n  a: string\n}\n", "T", false},
		{"ts: arrow function optional parameter", "a.ts",
			"export const f = <T>(a: T) => a\n", "export const f = <T>(a: T, b?: number) => a\n", "f", false},
		{"py: defaulted parameter", "a.py",
			"def load(path):\n    pass\n", "def load(path, opts=None):\n    pass\n", "load", false},
		{"py: async defaulted parameter", "a.py",
			"async def load(path):\n    pass\n", "async def load(path, opts=None):\n    pass\n", "load", false},
		{"py: property return annotation", "a.py",
			"class C:\n    @property\n    def port(self):\n        return 1\n",
			"class C:\n    @property\n    def port(self) -> int:\n        return 1\n", "C.port", false},
		{"py: class optional attribute", "a.py",
			"class C(A):\n    a: int = 1\n", "class C(A):\n    a: int = 1\n    b: int = 2\n", "C", false},
		{"py: optional parameter in a single overload", "a.py",
			"from typing import overload\n\n@overload\ndef f(a: int) -> int: ...\ndef f(a):\n    return a\n",
			"from typing import overload\n\n@overload\ndef f(a: int, b: int = 0) -> int: ...\ndef f(a, b=0):\n    return a\n", "f", false},
		{"go: variadic parameter", "a.go",
			"package a\n\nfunc F(x int) {}\n", "package a\n\nfunc F(x int, o ...int) {}\n", "F", false},
		{"go: variadic parameter in a method", "a.go",
			"package a\n\ntype T struct{}\n\nfunc (t *T) M(x int) {}\n", "package a\n\ntype T struct{}\n\nfunc (t *T) M(x int, o ...int) {}\n", "T.M", false},
		{"go: struct field", "a.go",
			"package a\n\ntype L[T any] struct {\n\tA T\n}\n", "package a\n\ntype L[T any] struct {\n\tA T\n\tB int\n}\n", "L", false},
		{"rust: trait items reordered", "a.rs",
			"pub trait T: Send {\n    fn a();\n    fn b();\n}\n", "pub trait T: Send {\n    fn b();\n    fn a();\n}\n", "T", false},
		{"rust: fields reordered", "a.rs",
			"pub struct S {\n    pub a: u8,\n    pub b: u8,\n    c: u8,\n}\n", "pub struct S {\n    pub b: u8,\n    pub a: u8,\n    c: u8,\n}\n", "S", false},
		{"rust: pub added", "a.rs", "fn f() {}\n", "pub fn f() {}\n", "f", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, err := symbols.Extract(tt.path, []byte(tt.old))
			if err != nil {
				t.Fatal(err)
			}
			new, err := symbols.Extract(tt.path, []byte(tt.new))
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, c := range DiffSymbols(old, new) {
				if c.Symbol.Qualified != tt.qualified {
					continue
				}
				found = true
				if c.Change != model.ChangeSignature || c.Breaking != tt.want {
					t.Errorf("%s %s breaking=%v, want signature breaking=%v (%q -> %q)",
						c.Symbol.Qualified, c.Change, c.Breaking, tt.want, c.OldSignature, c.Symbol.Signature)
				}
			}
			if !found {
				t.Errorf("no change to %s", tt.qualified)
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
