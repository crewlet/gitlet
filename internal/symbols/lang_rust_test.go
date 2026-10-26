package symbols

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
)

func rsTestParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Extract("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if f == nil {
		t.Fatal("Extract returned nil for a .rs file")
	}
	return f
}

func rsTestFixture(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "rust", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

func rsTestSymbol(t *testing.T, f *File, qualified string) model.Symbol {
	t.Helper()
	s, ok := f.Find(qualified)
	if !ok {
		t.Fatalf("no symbol %q; have %v", qualified, rsTestNames(f))
	}
	return s
}

func rsTestNames(f *File) []string {
	var names []string
	for _, s := range f.Symbols {
		names = append(names, s.Qualified)
	}
	return names
}

func TestRustSymbols(t *testing.T) {
	f := rsTestParse(t, rsTestFixture(t, "lib.rs"))
	if f.Lang != "rust" || f.Package != "" {
		t.Errorf("Lang, Package = %q, %q; want rust and none", f.Lang, f.Package)
	}
	tests := []struct {
		qualified, kind, container string
		exported                   bool
		signature                  string
	}{
		{"MAX_PORT", "const", "", true, "pub const MAX_PORT: u16"},
		{"COUNT", "var", "", false, "pub(crate) static mut COUNT: u32"},
		{"NAME", "var", "", false, "static NAME: &str"},
		{"Result", "type", "", true, "pub type Result<T> = std::result::Result<T,Error>"},
		{"Config", "struct", "", true, "pub struct Config<T> {pub name: String, pub(crate) port: u16, ..}"},
		{"Id", "struct", "", true, "pub struct Id(pub u64, _)"},
		{"Marker", "struct", "", true, "pub struct Marker"},
		{"Shape", "enum", "", true, "pub enum Shape<T> {Circle(f64), Rect {w: f64, h: f64}, Point(T), Empty}"},
		{"Source", "trait", "", true, "pub trait Source: Send + Sync {type Item: Clone; const LIMIT: usize; " +
			"fn get(&self, &str) -> Option<Vec<u8>>; fn len(&self) -> usize}"},
		{"Source.get", "method", "Source", true, "fn Source::get(&self, &str) -> Option<Vec<u8>>"},
		{"Source.len", "method", "Source", true, "fn Source::len(&self) -> usize"},
		{"load", "function", "", true, "pub fn load(&str, bool) -> Result<Config<String>>"},
		{"fetch", "function", "", true, "pub async fn fetch<T,F:Fn(T)->u8>(Vec<T>, F) -> Vec<u8> where T:Clone"},
		{"parse", "function", "", false, "fn parse(&str, bool) -> Result<Config<String>>"},
		{"Config.new", "method", "Config", true, "pub fn Config<T>::new(&str, T) -> Self"},
		{"Config.port", "method", "Config", true, "pub fn Config<T>::port(&self) -> u16"},
		{"Config.set_port", "method", "Config", false, "pub(crate) fn Config<T>::set_port(&mut self, u16)"},
		{"Config.validate", "method", "Config", false, "fn Config<T>::validate(&self)"},
		{"Config.DEFAULT_PORT", "const", "Config", true, "pub const Config<T>::DEFAULT_PORT: u16"},
		{"Config.fmt@Display", "method", "Config", true,
			"fn Config<T>::fmt@Display(&self, &mut fmt::Formatter<'_>) -> fmt::Result"},
		{"Id.Item@Iterator", "type", "Id", true, "type Id::Item@Iterator = u64"},
		{"Id.next@Iterator", "method", "Id", true, "fn Id::next@Iterator(&mut self) -> Option<u64>"},
		{"helper.trim", "function", "helper", true, "pub fn trim(&str) -> &str"},
		{"helper.Buf", "struct", "helper", true, "pub struct Buf"},
		{"helper.Buf.clear", "method", "helper.Buf", true, "pub fn Buf::clear(self)"},
		{"helper.inner.deep", "function", "helper.inner", false, "pub(super) fn deep() -> super::Buf"},
	}
	// Every definition, in source order: nothing from `const _`, macro_rules,
	// the extern block or the #[cfg(test)] module.
	var want []string
	for _, tt := range tests {
		want = append(want, tt.qualified)
	}
	if got := rsTestNames(f); !reflect.DeepEqual(got, want) {
		t.Errorf("symbols:\n got %v\nwant %v", got, want)
	}
	for _, tt := range tests {
		t.Run(tt.qualified, func(t *testing.T) {
			s := rsTestSymbol(t, f, tt.qualified)
			if s.Kind != tt.kind || s.Container != tt.container || s.Exported != tt.exported {
				t.Errorf("kind, container, exported = %q, %q, %v; want %q, %q, %v",
					s.Kind, s.Container, s.Exported, tt.kind, tt.container, tt.exported)
			}
			if s.Signature != tt.signature {
				t.Errorf("signature:\n got %s\nwant %s", s.Signature, tt.signature)
			}
			if s.Key != model.SymbolKey("rust", "src/lib.rs", tt.qualified) || s.Lang != "rust" || s.Path != "src/lib.rs" {
				t.Errorf("key, lang, path = %q, %q, %q", s.Key, s.Lang, s.Path)
			}
			if s.SigHash != SigHash(s.Kind+"\x00"+s.Signature) || s.BodyHash == "" {
				t.Errorf("hashes = %q, %q", s.SigHash, s.BodyHash)
			}
		})
	}

	// Lines and byte ranges cover the item, attributes excepted.
	cfg := rsTestSymbol(t, f, "Config")
	if cfg.StartLine != 20 || cfg.EndLine != 24 {
		t.Errorf("Config lines = %d-%d; want 20-24", cfg.StartLine, cfg.EndLine)
	}
	src := rsTestFixture(t, "lib.rs")
	if text := src[cfg.StartByte:cfg.EndByte]; !strings.HasPrefix(text, "pub struct Config<T> {") || !strings.HasSuffix(text, "}") {
		t.Errorf("Config bytes = %q", text)
	}
}

func TestRustParamsResultsMembers(t *testing.T) {
	f := rsTestParse(t, rsTestFixture(t, "lib.rs"))
	tests := []struct {
		qualified string
		params    []model.Param
		results   string
		members   []model.Member
	}{
		{qualified: "load", params: []model.Param{{Type: "&str"}, {Type: "bool"}}, results: "Result<Config<String>>"},
		{qualified: "fetch", params: []model.Param{{Type: "Vec<T>"}, {Type: "F"}}, results: "Vec<u8>"},
		// self is in the signature, not in params.
		{qualified: "Config.set_port", params: []model.Param{{Type: "u16"}}},
		{qualified: "Config.port", results: "u16"},
		{qualified: "Config.new", params: []model.Param{{Type: "&str"}, {Type: "T"}}, results: "Self"},
		{qualified: "Config.fmt@Display", params: []model.Param{{Type: "&mut fmt::Formatter<'_>"}}, results: "fmt::Result"},
		{qualified: "Source.get", params: []model.Param{{Type: "&str"}}, results: "Option<Vec<u8>>"},
		{qualified: "Config", members: []model.Member{{Name: "name", Type: "String"}, {Name: "port", Type: "u16"}}},
		{qualified: "Id", members: []model.Member{{Name: "0", Type: "u64"}}},
		{qualified: "Marker"},
		{qualified: "Shape", members: []model.Member{
			{Name: "Circle", Type: "(f64)"},
			{Name: "Rect", Type: "{w: f64, h: f64}"},
			{Name: "Point", Type: "(T)"},
			{Name: "Empty"},
		}},
		{qualified: "Source", members: []model.Member{
			{Name: "Item", Type: "type: Clone"},
			{Name: "LIMIT", Type: "usize"},
			{Name: "get", Type: "(&self, &str) -> Option<Vec<u8>>", Method: true},
			{Name: "len", Type: "(&self) -> usize", Method: true},
		}},
		{qualified: "MAX_PORT"},
		{qualified: "Result"},
	}
	for _, tt := range tests {
		t.Run(tt.qualified, func(t *testing.T) {
			s := rsTestSymbol(t, f, tt.qualified)
			if !reflect.DeepEqual(s.Params, tt.params) {
				t.Errorf("params = %+v; want %+v", s.Params, tt.params)
			}
			if s.Results != tt.results {
				t.Errorf("results = %q; want %q", s.Results, tt.results)
			}
			if !reflect.DeepEqual(s.Members, tt.members) {
				t.Errorf("members = %+v; want %+v", s.Members, tt.members)
			}
		})
	}
}

func TestRustSignatures(t *testing.T) {
	tests := []struct {
		name, src, qualified, signature string
		params                          []model.Param
	}{
		{
			name:      "modifiers",
			src:       `pub const unsafe extern "C" fn raw(p: *const u8, n: usize) -> i32 { 0 }`,
			qualified: "raw", signature: `pub const unsafe extern "C" fn raw(*const u8, usize) -> i32`,
			params: []model.Param{{Type: "*const u8"}, {Type: "usize"}},
		},
		{
			name:      "generics and where",
			src:       "pub fn map<T, U, F: Fn(T) -> U>(v: Vec<T>, f: F) -> Vec<U> where T: Clone + Send, U: 'static { todo!() }",
			qualified: "map", signature: "pub fn map<T,U,F:Fn(T)->U>(Vec<T>, F) -> Vec<U> where T:Clone+Send,U:'static",
			params: []model.Param{{Type: "Vec<T>"}, {Type: "F"}},
		},
		{
			name:      "patterns and impl Trait",
			src:       "fn f((a, b): (u8, u8), mut s: impl Into<String>, _: &[u8]) {}",
			qualified: "f", signature: "fn f((u8,u8), impl Into<String>, &[u8])",
			params: []model.Param{{Type: "(u8,u8)"}, {Type: "impl Into<String>"}, {Type: "&[u8]"}},
		},
		{
			name:      "lifetimes",
			src:       "pub fn first<'a>(s: &'a str, t: &'a mut Vec<&'a str>) -> &'a str { s }",
			qualified: "first", signature: "pub fn first<'a>(&'a str, &'a mut Vec<&'a str>) -> &'a str",
			params: []model.Param{{Type: "&'a str"}, {Type: "&'a mut Vec<&'a str>"}},
		},
		{
			name:      "self by value",
			src:       "impl Foo { pub fn into_inner(mut self) -> u8 { 0 } }",
			qualified: "Foo.into_inner", signature: "pub fn Foo::into_inner(self) -> u8",
		},
		{
			name:      "self with lifetime",
			src:       "impl<'a> Parser<'a> { fn peek(&'a mut self, n: usize) {} }",
			qualified: "Parser.peek", signature: "fn Parser<'a>::peek(&'a mut self, usize)",
			params: []model.Param{{Type: "usize"}},
		},
		{
			name:      "typed self",
			src:       "impl Task { fn poll(self: Pin<&mut Self>, cx: &mut Context) {} }",
			qualified: "Task.poll", signature: "fn Task::poll(self: Pin<&mut Self>, &mut Context)",
			params: []model.Param{{Type: "&mut Context"}},
		},
		{
			name:      "associated function",
			src:       "impl Config { pub fn default_port() -> u16 { 80 } }",
			qualified: "Config.default_port", signature: "pub fn Config::default_port() -> u16",
		},
		{
			name:      "impl through a path and a reference",
			src:       "impl a::b::Foo { pub fn x(&self) {} } impl<'a> Display for &'a Bar { fn fmt(&self) {} }",
			qualified: "Foo.x", signature: "pub fn Foo::x(&self)",
		},
		{
			name:      "trait impl through a reference",
			src:       "impl<'a> Display for &'a Bar { fn fmt(&self) {} }",
			qualified: "Bar.fmt@Display", signature: "fn Bar::fmt@Display(&self)",
		},
		{
			name:      "generic trait impl",
			src:       "impl From<u8> for Id { fn from(v: u8) -> Self { Id(v) } }",
			qualified: "Id.from@From", signature: "fn Id::from@From<u8>(u8) -> Self",
			params: []model.Param{{Type: "u8"}},
		},
		{
			name:      "trait impl through a path",
			src:       "impl<T> std::ops::Deref for Wrap<T> { type Target = T; fn deref(&self) -> &T { &self.0 } }",
			qualified: "Wrap.deref@Deref", signature: "fn Wrap<T>::deref@Deref(&self) -> &T",
		},
		{
			name:      "associated type of a trait impl",
			src:       "impl<T> std::ops::Deref for Wrap<T> { type Target = T; }",
			qualified: "Wrap.Target@Deref", signature: "type Wrap<T>::Target@Deref = T",
		},
		{
			name:      "variadic",
			src:       "pub unsafe extern \"C\" fn printf(fmt: *const c_char, args: ...) -> c_int { 0 }",
			qualified: "printf", signature: "pub unsafe extern \"C\" fn printf(*const c_char, ...) -> c_int",
			params: []model.Param{{Type: "*const c_char"}, {Variadic: true}},
		},
		{
			name:      "restricted visibility",
			src:       "pub(in crate::store) fn sync() {}",
			qualified: "sync", signature: "pub(in crate::store) fn sync()",
		},
		{
			name:      "tuple struct with where",
			src:       "pub struct Pair<T>(pub T, pub(crate) T) where T: Copy;",
			qualified: "Pair", signature: "pub struct Pair<T>(pub T, pub(crate) T) where T:Copy",
		},
		{
			name:      "private tuple struct",
			src:       "pub struct Secret(String);",
			qualified: "Secret", signature: "pub struct Secret(_)",
		},
		{
			name:      "struct with only private fields",
			src:       "pub struct Opaque { inner: u8 }",
			qualified: "Opaque", signature: "pub struct Opaque {..}",
		},
		{
			name:      "union",
			src:       "#[repr(C)] pub union Bits { pub i: u32, pub f: f32 }",
			qualified: "Bits", signature: "pub union Bits {pub i: u32, pub f: f32}",
		},
		{
			name:      "enum discriminants",
			src:       "#[repr(u8)] pub enum Level { Low = 1, High = 2 }",
			qualified: "Level", signature: "pub enum Level {Low = 1, High = 2}",
		},
		{
			name:      "unsafe generic trait",
			src:       "pub unsafe trait Arena<T: ?Sized>: Send where T: 'static { type Iter<'a>: Iterator where Self: 'a; unsafe fn alloc(&self, n: usize) -> *mut T; }",
			qualified: "Arena",
			signature: "pub unsafe trait Arena<T:?Sized>: Send where T:'static {type Iter<'a>: Iterator where Self:'a; unsafe fn alloc(&self, usize) -> *mut T}",
		},
		{
			name:      "unsafe trait method",
			src:       "pub unsafe trait Arena<T> { unsafe fn alloc(&self, n: usize) -> *mut T; }",
			qualified: "Arena.alloc", signature: "unsafe fn Arena::alloc(&self, usize) -> *mut T",
			params: []model.Param{{Type: "usize"}},
		},
		{
			name:      "2015 anonymous trait parameters",
			src:       "trait Old { fn f(u8, &str); }",
			qualified: "Old.f", signature: "fn Old::f(u8, &str)",
			params: []model.Param{{Type: "u8"}, {Type: "&str"}},
		},
		{
			name:      "static",
			src:       "pub static TABLE: [u8; 4] = [0; 4];",
			qualified: "TABLE", signature: "pub static TABLE: [u8;4]",
		},
		{
			name:      "type alias with where",
			src:       "pub type Map<K> = HashMap<K, Vec<K>> where K: Eq;",
			qualified: "Map", signature: "pub type Map<K> = HashMap<K,Vec<K>> where K:Eq",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := rsTestSymbol(t, rsTestParse(t, tt.src), tt.qualified)
			if s.Signature != tt.signature {
				t.Errorf("signature:\n got %s\nwant %s", s.Signature, tt.signature)
			}
			if !reflect.DeepEqual(s.Params, tt.params) {
				t.Errorf("params = %+v; want %+v", s.Params, tt.params)
			}
		})
	}
}

func TestRustSkipped(t *testing.T) {
	src := `use crate::a;

#[cfg(test)]
mod tests {
    use super::*;
    fn helper() { crate::a::run(); }
}

// A comment between the attribute and the module.
#[cfg(test)]
// still a test module
mod more_tests { fn helper() {} }

mod inner_marked {
    #![cfg(test)]
    fn helper() {}
}

#[cfg(not(test))]
mod real { pub fn kept() {} }

macro_rules! square { ($x:expr) => { $x * $x }; }

extern "C" { fn abs(x: i32) -> i32; static errno: i32; }

#[cfg(test)]
mod declared_tests;

fn outer() {
    fn nested() {}
    struct Local;
    impl Local { fn m(&self) {} }
}
`
	f := rsTestParse(t, src)
	if got, want := rsTestNames(f), []string{"real.kept", "outer"}; !reflect.DeepEqual(got, want) {
		t.Errorf("symbols = %v; want %v", got, want)
	}
	for _, r := range f.Refs {
		if r.Name == "run" {
			t.Errorf("ref from a #[cfg(test)] module: %+v", r)
		}
	}
	if len(f.Imports) != 1 || f.Imports[0].Module != "crate" {
		t.Errorf("imports = %+v; want only use crate::a", f.Imports)
	}
}

func TestRustSigHashStable(t *testing.T) {
	tests := []struct{ name, before, after string }{
		{
			name:   "parameter names",
			before: "pub fn load(path: &str, strict: bool) -> u8 { 0 }\nimpl C { pub fn set(&mut self, port: u16) {} }",
			after:  "pub fn load(p: &str, s: bool) -> u8 { 0 }\nimpl C { pub fn set(&mut self, value: u16) {} }",
		},
		{
			name:   "comments and attributes",
			before: "pub struct S { pub a: u8 }\npub fn f(x: u8) {}",
			after:  "/// Docs.\n#[derive(Debug)]\npub struct S { /* the a */ pub a: u8 }\n#[inline]\npub fn f(x /* ignored */: u8) {}",
		},
		{
			name: "rustfmt line breaks with trailing commas",
			before: "pub fn f<T: Clone, U>(a: Vec<T>, b: U) -> HashMap<T, U> where T: Send, U: Sync { todo!() }\n" +
				"pub struct S<T> { pub a: T, pub b: u8 }\n" +
				"pub struct P(pub u8, pub u16);\n" +
				"pub enum E { A(u8, u16), B { x: u8 } }\n" +
				"pub trait Tr: Send + Sync { fn m(&self, a: u8); }\n",
			after: "pub fn f<\n    T: Clone,\n    U,\n>(\n    a: Vec<T>,\n    b: U,\n) -> HashMap<\n    T,\n    U,\n>\nwhere\n    T: Send,\n    U: Sync,\n{\n    todo!()\n}\n" +
				"pub struct S<T> {\n    pub a: T,\n    pub b: u8,\n}\n" +
				"pub struct P(\n    pub u8,\n    pub u16,\n);\n" +
				"pub enum E {\n    A(\n        u8,\n        u16,\n    ),\n    B {\n        x: u8,\n    },\n}\n" +
				"pub trait Tr:\n    Send\n    + Sync\n{\n    fn m(\n        &self,\n        a: u8,\n    );\n}\n",
		},
		{
			name:   "private fields",
			before: "pub struct S { pub a: u8, b: u8 }\npub struct T(pub u8, String);",
			after:  "pub struct S { pub a: u8, b: String, c: Vec<u8> }\npub struct T(pub u8, Vec<u8>);",
		},
		{
			name:   "bodies and values",
			before: "pub fn f() -> u8 { 1 }\npub const N: u8 = 1;\ntrait Tr { fn d(&self) -> u8 { 1 } }",
			after:  "pub fn f() -> u8 { let x = 2; x }\npub const N: u8 = 2;\ntrait Tr { fn d(&self) -> u8 { 2 } }",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := rsTestParse(t, tt.before), rsTestParse(t, tt.after)
			if got, want := rsTestNames(after), rsTestNames(before); !reflect.DeepEqual(got, want) {
				t.Fatalf("symbols = %v; want %v", got, want)
			}
			for i, b := range before.Symbols {
				a := after.Symbols[i]
				if a.Signature != b.Signature || a.SigHash != b.SigHash {
					t.Errorf("%s: signature changed:\n before %s\n after  %s", b.Qualified, b.Signature, a.Signature)
				}
			}
		})
	}

	// The fixture pair differs only in layout and comments: same symbols,
	// signatures and bodies.
	before := rsTestParse(t, rsTestFixture(t, "lib.rs"))
	after := rsTestParse(t, rsTestFixture(t, "lib_reformatted.rs"))
	if got, want := rsTestNames(after), rsTestNames(before); !reflect.DeepEqual(got, want) {
		t.Fatalf("reformatted symbols = %v; want %v", got, want)
	}
	for i, b := range before.Symbols {
		a := after.Symbols[i]
		if a.SigHash != b.SigHash || a.BodyHash != b.BodyHash {
			t.Errorf("%s changed when reformatted: sig %s -> %s, body %s -> %s",
				b.Qualified, b.SigHash, a.SigHash, b.BodyHash, a.BodyHash)
		}
	}
}

func TestRustSigHashChanges(t *testing.T) {
	tests := []struct{ name, before, after, qualified string }{
		{"visibility narrowed", "pub fn f() {}", "pub(crate) fn f() {}", "f"},
		{"visibility widened", "fn f() {}", "pub fn f() {}", "f"},
		{"parameter type", "pub fn f(a: u8) {}", "pub fn f(a: u16) {}", "f"},
		{"parameter added", "pub fn f(a: u8) {}", "pub fn f(a: u8, b: u8) {}", "f"},
		{"return type", "pub fn f() -> u8 { 0 }", "pub fn f() -> u16 { 0 }", "f"},
		{"async", "pub fn f() {}", "pub async fn f() {}", "f"},
		{"generic bound", "pub fn f<T: Clone>(t: T) {}", "pub fn f<T: Copy>(t: T) {}", "f"},
		{"self kind", "impl S { pub fn f(&self) {} }", "impl S { pub fn f(&mut self) {} }", "S.f"},
		{"self added", "impl S { pub fn f() {} }", "impl S { pub fn f(&self) {} }", "S.f"},
		{"impl type arguments", "impl S<u8> { pub fn f() {} }", "impl S<u16> { pub fn f() {} }", "S.f"},
		{"pub field type", "pub struct S { pub a: u8 }", "pub struct S { pub a: u16 }", "S"},
		{"pub field added", "pub struct S { pub a: u8 }", "pub struct S { pub a: u8, pub b: u8 }", "S"},
		{"first private field", "pub struct S { pub a: u8 }", "pub struct S { pub a: u8, b: u8 }", "S"},
		{"field visibility", "pub struct S { pub(crate) a: u8 }", "pub struct S { pub a: u8 }", "S"},
		{"tuple field made public", "pub struct S(u8);", "pub struct S(pub u8);", "S"},
		{"variant added", "pub enum E { A }", "pub enum E { A, B }", "E"},
		{"variant payload", "pub enum E { A(u8) }", "pub enum E { A(u16) }", "E"},
		{"trait method added", "pub trait T { fn a(&self); }", "pub trait T { fn a(&self); fn b(&self); }", "T"},
		{"trait default method added", "pub trait T { fn a(&self); }", "pub trait T { fn a(&self); fn b(&self) {} }", "T"},
		{"trait method changed", "pub trait T { fn a(&self); }", "pub trait T { fn a(&self) -> u8; }", "T"},
		{"supertrait added", "pub trait T: Send {}", "pub trait T: Send + Sync {}", "T"},
		{"associated type bound", "pub trait T { type I; }", "pub trait T { type I: Clone; }", "T"},
		{"const type", "pub const N: u8 = 1;", "pub const N: u16 = 1;", "N"},
		{"static mut", "pub static N: u8 = 1;", "pub static mut N: u8 = 1;", "N"},
		{"alias target", "pub type R = Result<u8>;", "pub type R = Result<u16>;", "R"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := rsTestSymbol(t, rsTestParse(t, tt.before), tt.qualified)
			after := rsTestSymbol(t, rsTestParse(t, tt.after), tt.qualified)
			if before.SigHash == after.SigHash {
				t.Errorf("signature hash unchanged: %s -> %s", before.Signature, after.Signature)
			}
		})
	}
}

func TestRustBodyHash(t *testing.T) {
	const trait = `pub trait Store {
    fn get(&self) -> u8;
    fn len(&self) -> usize {
        %s
    }
}

pub fn load() -> u8 {
    %s
}
`
	parse := func(lenBody, loadBody string) *File {
		return rsTestParse(t, strings.Replace(strings.Replace(trait, "%s", lenBody, 1), "%s", loadBody, 1))
	}
	base := parse("0", "1")
	tests := []struct {
		name               string
		file               *File
		changed, unchanged []string
	}{
		{"comments and layout", parse("/* none */ 0", "// one\n    1"), nil, []string{"Store", "Store.get", "Store.len", "load"}},
		{"default method body", parse("1", "1"), []string{"Store.len"}, []string{"Store", "Store.get", "load"}},
		{"function body", parse("0", "2"), []string{"load"}, []string{"Store", "Store.get", "Store.len"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, q := range tt.changed {
				if rsTestSymbol(t, base, q).BodyHash == rsTestSymbol(t, tt.file, q).BodyHash {
					t.Errorf("%s: body hash unchanged", q)
				}
			}
			for _, q := range tt.unchanged {
				if rsTestSymbol(t, base, q).BodyHash != rsTestSymbol(t, tt.file, q).BodyHash {
					t.Errorf("%s: body hash changed", q)
				}
				if rsTestSymbol(t, base, q).SigHash != rsTestSymbol(t, tt.file, q).SigHash {
					t.Errorf("%s: signature hash changed", q)
				}
			}
		})
	}
}

func TestRustRefs(t *testing.T) {
	f := rsTestParse(t, rsTestFixture(t, "refs.rs"))
	type ref struct {
		line            int
		kind            model.RefKind
		name, qualifier string
	}
	want := []ref{
		{3, model.RefType, "Store", ""},
		{5, model.RefType, "Config", "crate::model"},
		{8, model.RefType, "Store", ""},
		{8, model.RefType, "Wrapper", ""}, // the impl's self type; T is a type parameter
		{9, model.RefType, "Error", ""},
		{9, model.RefType, "Path", ""},
		{9, model.RefType, "Result", ""},
		{10, model.RefCall, "read", "fs"},
		{11, model.RefType, "Config", ""}, // turbofish
		{11, model.RefCall, "parse", "model"},
		{12, model.RefCall, "with", "Wrapper"},  // Self::with
		{13, model.RefConstruct, "Wrapper", ""}, // Self { .. }
		{13, model.RefCall, "clone", "w.cfg"},
		{13, model.RefCall, "default", "T"},
		{14, model.RefCall, "helper", ""}, // inside assert_eq!
		{14, model.RefCall, "name", "s.cfg"},
		{15, model.RefCall, "Ok", ""},
		{19, model.RefExtends, "Drop", ""},
		{19, model.RefType, "Mem", ""},
		{19, model.RefType, "Wrapper", ""},
		{21, model.RefCall, "flush", "self.inner"},
		{21, model.RefCall, "unwrap", "self.inner.flush()"},
		{22, model.RefConstruct, "Entry", "crate::log"},
		{26, model.RefExtends, "Default", ""},
		{26, model.RefExtends, "Read", "io"},
		{27, model.RefType, "Option", ""}, // Self::Out is left out
	}
	var got []ref
	for _, r := range f.Refs {
		if r.Lang != "rust" || r.Path != "src/lib.rs" || r.Module != "" || r.Resolved {
			t.Errorf("ref not left unresolved: %+v", r)
		}
		got = append(got, ref{r.Line, r.Kind, r.Name, r.Qualifier})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refs:\n got %+v\nwant %+v", got, want)
	}
}

func TestRustRefForms(t *testing.T) {
	tests := []struct {
		name, src string
		want      []model.Ref // Name, Qualifier and Kind
	}{
		{"call", "fn f() { g(); }", []model.Ref{{Name: "g", Kind: model.RefCall}}},
		{"path call", "fn f() { a::b::g(); }", []model.Ref{{Name: "g", Qualifier: "a::b", Kind: model.RefCall}}},
		{"crate call", "fn f() { crate::a::g(); super::h(); }", []model.Ref{
			{Name: "g", Qualifier: "crate::a", Kind: model.RefCall},
			{Name: "h", Qualifier: "super", Kind: model.RefCall},
		}},
		{"associated function", "fn f() { Type::new(); }", []model.Ref{{Name: "new", Qualifier: "Type", Kind: model.RefCall}}},
		{"generic function", "fn f() { g::<u8>(); Vec::<u8>::with_capacity(1); }", []model.Ref{
			{Name: "Vec", Kind: model.RefType},
			{Name: "g", Kind: model.RefCall},
			{Name: "with_capacity", Qualifier: "Vec", Kind: model.RefCall},
		}},
		{"method call", "fn f(x: X) { x.go(); }", []model.Ref{
			{Name: "X", Kind: model.RefType},
			{Name: "go", Qualifier: "x", Kind: model.RefCall},
		}},
		{"self method", "impl S { fn f(&self) { self.go(); } }", []model.Ref{
			{Name: "S", Kind: model.RefType},
			{Name: "go", Qualifier: "self", Kind: model.RefCall},
		}},
		{"awaited and tried receivers", "async fn f() { load().await?.run(); (a + b).abs(); }", []model.Ref{
			{Name: "abs", Qualifier: "_", Kind: model.RefCall},
			{Name: "load", Kind: model.RefCall},
			{Name: "run", Qualifier: "load().await?", Kind: model.RefCall},
		}},
		{"macro arguments", `fn f() { println!("{} {}", fmt(x), a::b(y.c())); vec![T::new()]; }`, []model.Ref{
			{Name: "b", Qualifier: "a", Kind: model.RefCall},
			{Name: "c", Qualifier: "y", Kind: model.RefCall},
			{Name: "fmt", Kind: model.RefCall},
			{Name: "new", Qualifier: "T", Kind: model.RefCall},
		}},
		{"no calls from keywords in macros", `fn f() { assert!(if (x) { true } else { false }); }`, nil},
		{"no calls from definitions in macros", `mock! { fn get(&self) -> u8; struct Pt(u8); }`, nil},
		{"Self in an impl", "impl a::S { fn f() { Self::g(); vec![Self::h()]; Self { x: 1 }; } }", []model.Ref{
			{Name: "S", Qualifier: "a", Kind: model.RefConstruct},
			{Name: "S", Qualifier: "a", Kind: model.RefType},
			{Name: "g", Qualifier: "a::S", Kind: model.RefCall},
			{Name: "h", Qualifier: "a::S", Kind: model.RefCall},
		}},
		{"Self in a trait", "trait T { fn f() { Self::g(); } }", []model.Ref{
			{Name: "g", Qualifier: "Self", Kind: model.RefCall},
		}},
		{"construct", "fn f() { Config { a: 1 }; a::Config { a: 1 }; Shape::Rect { w: 1 }; }", []model.Ref{
			{Name: "Config", Kind: model.RefConstruct},
			{Name: "Config", Qualifier: "a", Kind: model.RefConstruct},
			{Name: "Rect", Qualifier: "Shape", Kind: model.RefConstruct},
		}},
		{"types", "fn f(a: a::b::T, b: &mut Vec<Box<dyn Any>>, c: impl Fn(u8) -> Out) -> [Item; 2] { let x: Local = y as Cast; }", []model.Ref{
			{Name: "Any", Kind: model.RefType},
			{Name: "Box", Kind: model.RefType},
			{Name: "Cast", Kind: model.RefType},
			{Name: "Fn", Kind: model.RefType},
			{Name: "Item", Kind: model.RefType},
			{Name: "Local", Kind: model.RefType},
			{Name: "Out", Kind: model.RefType},
			{Name: "T", Qualifier: "a::b", Kind: model.RefType},
			{Name: "Vec", Kind: model.RefType},
		}},
		{"primitives, Self and type parameters", "impl<T> S<T> { fn f<U>(self, a: u8, b: &str, c: Self, d: T, e: U, f: T::Item) -> bool { true } }", []model.Ref{
			{Name: "S", Kind: model.RefType},
		}},
		{"generic arguments and bindings", "fn f(i: impl Iterator<Item = Entry>) {}", []model.Ref{
			{Name: "Entry", Kind: model.RefType},
			{Name: "Iterator", Kind: model.RefType},
		}},
		{"extends", "impl fmt::Display for S {} impl<T: Clone> From<T> for W<T> {} trait X: A + b::B + for<'a> C<'a> {}", []model.Ref{
			{Name: "A", Kind: model.RefExtends},
			{Name: "B", Qualifier: "b", Kind: model.RefExtends},
			{Name: "C", Kind: model.RefExtends},
			{Name: "Clone", Kind: model.RefType},
			{Name: "Display", Qualifier: "fmt", Kind: model.RefExtends},
			{Name: "From", Kind: model.RefExtends},
			{Name: "S", Kind: model.RefType},
			{Name: "W", Kind: model.RefType},
		}},
		{"negative impl", "impl !Send for S {}", []model.Ref{{Name: "S", Kind: model.RefType}}},
		{"definition names are not refs", "struct A; enum B {} trait C { type D; } type E = u8; union F { x: u8 }", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := rsTestParse(t, tt.src)
			var got []model.Ref
			for _, r := range f.Refs {
				got = append(got, model.Ref{Name: r.Name, Qualifier: r.Qualifier, Kind: r.Kind})
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("refs:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestRustImports(t *testing.T) {
	named := func(module, name, local string) Import {
		return Import{Kind: ImportNamed, Module: module, Name: name, Local: local, Line: 1}
	}
	tests := []struct {
		src  string
		want []Import
	}{
		{"use a::b::c;", []Import{named("a::b", "c", "c")}},
		{"use a::b::c as d;", []Import{named("a::b", "c", "d")}},
		{"use a::b::{c, d as e, self};", []Import{
			named("a::b", "c", "c"),
			named("a::b", "d", "e"),
			{Kind: ImportNamespace, Module: "a::b", Local: "b", Line: 1},
		}},
		{"use a::{b::{c, d}, e, f::g::*};", []Import{
			named("a::b", "c", "c"),
			named("a::b", "d", "d"),
			named("a", "e", "e"),
			{Kind: ImportGlob, Module: "a::f::g", Line: 1},
		}},
		{"use a::b::*;", []Import{{Kind: ImportGlob, Module: "a::b", Line: 1}}},
		{"use foo;", []Import{{Kind: ImportNamespace, Module: "foo", Local: "foo", Line: 1}}},
		{"use foo as bar;", []Import{{Kind: ImportNamespace, Module: "foo", Local: "bar", Line: 1}}},
		{"pub use crate::model::Config;", []Import{named("crate::model", "Config", "Config")}},
		{"pub(crate) use super::super::x;", []Import{named("super::super", "x", "x")}},
		{"use self::inner::{self as i, Thing};", []Import{
			{Kind: ImportNamespace, Module: "self::inner", Local: "i", Line: 1},
			named("self::inner", "Thing", "Thing"),
		}},
		{"use super::*;", []Import{{Kind: ImportGlob, Module: "super", Line: 1}}},
		{"use ::std::fmt;", []Import{named("::std", "fmt", "fmt")}},
		{"use std::io::Write as _;", []Import{named("std::io", "Write", "")}},
		{"use {a, b::c};", []Import{
			{Kind: ImportNamespace, Module: "a", Local: "a", Line: 1},
			named("b", "c", "c"),
		}},
		// A module declaration binds a child module, so config::load()
		// resolves like use self::config.
		{"extern crate serde;\nmod config;", []Import{{Kind: ImportNamespace, Module: "self::config", Local: "config", Line: 2}}},
		{"pub(crate) mod net;", []Import{{Kind: ImportNamespace, Module: "self::net", Local: "net", Line: 1}}},
		{"#[cfg(test)]\nmod tests;", nil},
		{"mod inline { pub fn f() {} }", nil},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			if got := rsTestParse(t, tt.src).Imports; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("imports:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}

	// Imports inside modules and functions count, each on its own line.
	src := "mod m {\n    use crate::a::B;\n}\nfn f() {\n    use std::mem::{\n        swap,\n        take,\n    };\n}\n"
	want := []Import{
		named("crate::a", "B", "B"),
		{Kind: ImportNamed, Module: "std::mem", Name: "swap", Local: "swap", Line: 6},
		{Kind: ImportNamed, Module: "std::mem", Name: "take", Local: "take", Line: 7},
	}
	want[0].Line = 2
	if got := rsTestParse(t, src).Imports; !reflect.DeepEqual(got, want) {
		t.Errorf("nested imports:\n got %+v\nwant %+v", got, want)
	}
}

func TestRustMalformed(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string // definitions that must survive
	}{
		{"empty", "", nil},
		{"comments only", "// nothing\n/* here */", nil},
		{
			name: "broken function between good items",
			src:  "pub fn good(x: u8) -> u8 { x }\nfn broken( {\nstruct Good;\n",
			want: []string{"good", "Good"},
		},
		{
			name: "broken items everywhere",
			src: "pub fn good() {}\nimpl Foo {\n    pub fn ok(&self) -> u8 { 1 }\n    fn bad(&self -> u8 { }\n}\n" +
				"pub struct Half {\n    pub name: String,\n    pub port:\n\npub enum E { A, B( }\npub trait T { fn m(&self); fn n(&self\n",
			want: []string{"good", "Foo.ok", "Half"},
		},
		{"unterminated", "pub struct Config { pub name: String", []string{"Config"}},
		{"broken use", "use a::{b, ;\nuse c::d;\npub fn f() {}", []string{"f"}},
		{"stray braces", "}}}{{{ pub fn\nimpl for {} trait {} struct ;", nil},
		{"binary", "\x00\xff\xfe fn \x01(", nil},
		{"nested generics", "pub fn f<T: Fn(<T as X>::Y) -> [u8; { 1 + }]>() {}", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := rsTestParse(t, tt.src)
			for _, q := range tt.want {
				rsTestSymbol(t, f, q)
			}
		})
	}
}

func TestRustLongChain(t *testing.T) {
	// A builder chain renders only its last steps as the receiver, so the
	// work and the qualifiers stay linear in the chain's length.
	f := rsTestParse(t, "fn f() { x"+strings.Repeat(".step()", 3000)+"; }")
	if len(f.Refs) > 8 {
		t.Errorf("refs = %d; want the long receivers to collapse into one", len(f.Refs))
	}
	qualifiers := map[string]bool{}
	for _, r := range f.Refs {
		qualifiers[r.Qualifier] = true
		if r.Name != "step" || len(r.Qualifier) > 64 {
			t.Errorf("ref = %+v", r)
		}
	}
	if !qualifiers["x"] || !qualifiers["x.step()"] {
		t.Errorf("qualifiers = %v; want the short receivers in full", qualifiers)
	}
}

func TestRustConcurrentExtract(t *testing.T) {
	sources := []string{
		rsTestFixture(t, "lib.rs"),
		rsTestFixture(t, "lib_reformatted.rs"),
		rsTestFixture(t, "refs.rs"),
	}
	want := make([]*File, len(sources))
	for i, src := range sources {
		want[i] = rsTestParse(t, src)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				k := (g + i) % len(sources)
				f, err := Extract("src/lib.rs", []byte(sources[k]))
				if err != nil {
					errs <- err.Error()
					return
				}
				if !reflect.DeepEqual(f, want[k]) {
					errs <- "concurrent result differs for source " + string(rune('0'+k))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
