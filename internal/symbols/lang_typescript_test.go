package symbols

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
)

func tsTestExtract(t *testing.T, path, src string) *File {
	t.Helper()
	f, err := Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("Extract(%s): %v", path, err)
	}
	if f == nil {
		t.Fatalf("Extract(%s): no file", path)
	}
	return f
}

func tsTestFind(t *testing.T, f *File, qualified string) model.Symbol {
	t.Helper()
	s, ok := f.Find(qualified)
	if !ok {
		var names []string
		for _, s := range f.Symbols {
			names = append(names, s.Qualified)
		}
		t.Fatalf("no symbol %q; have %v", qualified, names)
	}
	return s
}

// tsTestSummary renders symbols as "Qualified kind container exported" lines.
func tsTestSummary(f *File) []string {
	var out []string
	for _, s := range f.Symbols {
		exp := ""
		if s.Exported {
			exp = " exported"
		}
		out = append(out, fmt.Sprintf("%s %s [%s]%s", s.Qualified, s.Kind, s.Container, exp))
	}
	return out
}

func tsTestRefs(refs []model.Ref) []string {
	var out []string
	for _, r := range refs {
		s := fmt.Sprintf("%d %s %s", r.Line, r.Kind, r.Name)
		if r.Qualifier != "" {
			s += " q=" + r.Qualifier
		}
		out = append(out, s)
	}
	return out
}

func tsTestImports(imports []Import) []string {
	var out []string
	for _, im := range imports {
		out = append(out, fmt.Sprintf("%d %s local=%s name=%s module=%s", im.Line, im.Kind, im.Local, im.Name, im.Module))
	}
	return out
}

func tsTestDiff(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\ngot:\n  %s\nwant:\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestTSDefinitions(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
		want []string
	}{
		{
			name: "kinds and containers",
			path: "a.ts",
			src: `export function load() {}
function* gen() {}
export class Config {
  constructor() {}
  get port() { return 1; }
  set port(v) {}
  static create() {}
  private hidden() {}
  protected guarded() {}
  #secret() {}
  onClick = () => {};
}
export abstract class Shape { abstract area(): number; }
export interface Opts { a: string }
export type ID = string;
export enum Color { Red }
export const arrow = () => 1;
let fn = function () {};
const notAFunction = 1;
function outer() { function inner() {} }`,
			want: []string{
				"load function [] exported",
				"gen function []",
				"Config class [] exported",
				"Config.constructor method [Config] exported",
				"Config.port method [Config] exported",
				"Config.port@set method [Config] exported",
				"Config.create method [Config] exported",
				"Config.hidden method [Config]",
				"Config.guarded method [Config]",
				"Config.#secret method [Config]",
				"Config.onClick method [Config] exported",
				"Shape class [] exported",
				"Shape.area method [Shape] exported",
				"Opts interface [] exported",
				"ID type [] exported",
				"Color enum [] exported",
				"arrow function [] exported",
				"fn function []",
				"outer function []",
			},
		},
		{
			name: "export clause, default export and export =",
			path: "a.ts",
			src: `function a() {}
function b() {}
class C { m() {} private p() {} }
function d() {}
export default function () {}
export { a, b as renamed };
export default C;`,
			want: []string{
				"a function [] exported",
				"b function [] exported",
				"C class [] exported",
				"C.m method [C] exported",
				"C.p method [C]",
				"d function []",
				"default function [] exported",
			},
		},
		{
			name: "default class",
			path: "a.ts",
			src:  `export default class { run() {} }`,
			want: []string{"default class [] exported", "default.run method [default] exported"},
		},
		{
			name: "export assignment",
			path: "a.ts",
			src:  "function f() {}\nexport = f;",
			want: []string{"f function [] exported"},
		},
		{
			name: "overloads merge, other duplicates are numbered",
			path: "a.ts",
			src: `export function parse(s: string): number;
export function parse(n: number): number;
export function parse(x: any): number { return 0; }
class K {
  m(a: string): void;
  m(a: number): void;
  m(a: any) {}
  static m() {}
}
function dup() {}
function dup() {}`,
			want: []string{
				"parse function [] exported",
				"K class []",
				"K.m method [K]",
				"K.m#2 method [K]",
				"dup function []",
				"dup#2 function []",
			},
		},
		{
			name: "namespaces and ambient declarations",
			path: "a.ts",
			src: `export namespace Util {
  export function a() {}
  function b() {}
  export class K { m() {} }
  export namespace Deep { export const c = () => 1; }
}
namespace Local { export function d() {} }
declare namespace Amb { function e(): void; }
declare module "pkg" { export function f(): void; }
declare global { interface Window { x: number } }
declare function g(): void;
export declare function h(): void;`,
			want: []string{
				"Util.a function [Util] exported",
				"Util.b function [Util]",
				"Util.K class [Util] exported",
				"Util.K.m method [Util.K] exported",
				"Util.Deep.c function [Util.Deep] exported",
				"Local.d function [Local]",
				"Amb.e function [Amb] exported",
				"pkg.f function [pkg] exported",
				"Window interface [] exported",
				"g function []",
				"h function [] exported",
			},
		},
		{
			name: "javascript",
			path: "a.js",
			src: `function load(a) {}
class Store { add(x) {} #drop() {} static make() {} }
const save = (s) => s, limit = 3;
var old = function* () {};
function unused() {}
module.exports = { load, Store, save: save };
exports.old = old;`,
			want: []string{
				"load function [] exported",
				"Store class [] exported",
				"Store.add method [Store] exported",
				"Store.#drop method [Store]",
				"Store.make method [Store] exported",
				"save function [] exported",
				"old function [] exported",
				"unused function []",
			},
		},
		{
			name: "tsx",
			path: "a.tsx",
			src: `export function App(): JSX.Element { return <div />; }
const Item = ({ label }: { label: string }) => <li>{label}</li>;`,
			want: []string{"App function [] exported", "Item function []"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, tt.path, tt.src)
			tsTestDiff(t, "symbols", tsTestSummary(f), tt.want)
		})
	}
}

func TestTSSignatures(t *testing.T) {
	str, num, boolean := model.Param{Type: "string"}, model.Param{Type: "number"}, model.Param{Type: "boolean"}
	tests := []struct {
		name      string
		path      string
		src       string
		qualified string
		sig       string
		params    []model.Param
		results   string
		members   []model.Member
	}{
		{
			name:      "async function",
			path:      "a.ts",
			src:       `export async function load(path: string, strict?: boolean): Promise<Config> { return x; }`,
			qualified: "load",
			sig:       "export async function load(string, boolean?): Promise<Config>",
			params:    []model.Param{str, {Type: "boolean", Optional: true}},
			results:   "Promise<Config>",
		},
		{
			name:      "defaulted and rest parameters",
			path:      "a.ts",
			src:       `function f(a: number = 1, b = "x", ...rest: string[]): void {}`,
			qualified: "f",
			sig:       "function f(number?, ?, ...string[]): void",
			params:    []model.Param{{Type: "number", Optional: true}, {Optional: true}, {Type: "string[]", Variadic: true}},
			results:   "void",
		},
		{
			name:      "untyped javascript parameters",
			path:      "a.js",
			src:       `function f(a, b = 1, {c}, [d], ...rest) {}`,
			qualified: "f",
			sig:       "function f(_, ?, _, _, ..._)",
			params:    []model.Param{{}, {Optional: true}, {}, {}, {Variadic: true}},
		},
		{
			name:      "untyped typescript parameters",
			path:      "a.ts",
			src:       `function f(a, b?, c = 1, ...rest) {}`,
			qualified: "f",
			sig:       "function f(_, ?, ?, ..._)",
			params:    []model.Param{{}, {Optional: true}, {Optional: true}, {Variadic: true}},
		},
		{
			name:      "generics and function types",
			path:      "a.ts",
			src:       `function map<T, U>(xs: T[], fn: (x: T, i: number) => U): U[] { return []; }`,
			qualified: "map",
			sig:       "function map<T, U>(T[], (T, number) => U): U[]",
			params:    []model.Param{{Type: "T[]"}, {Type: "(T, number) => U"}},
			results:   "U[]",
		},
		{
			name:      "constrained type parameters",
			path:      "a.ts",
			src:       `function pick<T extends object = {}, K extends keyof T = keyof T>(o: T, k: K): T[K] { return o[k]; }`,
			qualified: "pick",
			sig:       "function pick<T extends object = {}, K extends keyof T = keyof T>(T, K): T[K]",
			params:    []model.Param{{Type: "T"}, {Type: "K"}},
			results:   "T[K]",
		},
		{
			name:      "generator",
			path:      "a.ts",
			src:       `function* gen(): Iterator<number> {}`,
			qualified: "gen",
			sig:       "function* gen(): Iterator<number>",
			results:   "Iterator<number>",
		},
		{
			name:      "async generator",
			path:      "a.ts",
			src:       `export async function* stream() {}`,
			qualified: "stream",
			sig:       "export async function* stream()",
		},
		{
			name:      "anonymous default function",
			path:      "a.ts",
			src:       `export default function (a: string) {}`,
			qualified: "default",
			sig:       "export default function(string)",
			params:    []model.Param{str},
		},
		{
			name:      "named default function",
			path:      "a.js",
			src:       `export default function main() {}`,
			qualified: "main",
			sig:       "export default function main()",
		},
		{
			name:      "arrow function is written as a function",
			path:      "a.ts",
			src:       `export const load = async (path: string, strict?: boolean): Promise<Config> => read(path);`,
			qualified: "load",
			sig:       "export async function load(string, boolean?): Promise<Config>",
			params:    []model.Param{str, {Type: "boolean", Optional: true}},
			results:   "Promise<Config>",
		},
		{
			name:      "arrow function without parentheses",
			path:      "a.js",
			src:       `const inc = x => x + 1;`,
			qualified: "inc",
			sig:       "function inc(_)",
			params:    []model.Param{{}},
		},
		{
			name:      "annotated arrow function variable",
			path:      "a.ts",
			src:       `export const handler: Handler<Req> = (req) => {};`,
			qualified: "handler",
			sig:       "export const handler: Handler<Req>",
		},
		{
			name:      "function expression and generator variables",
			path:      "a.ts",
			src:       "const f = function (a: string) {};\nlet g = function* (n: number) {};",
			qualified: "g",
			sig:       "function* g(number)",
			params:    []model.Param{num},
		},
		{
			name:      "this parameter is shown but is not a parameter",
			path:      "a.ts",
			src:       `function f(this: Window, a: string) {}`,
			qualified: "f",
			sig:       "function f(this: Window, string)",
			params:    []model.Param{str},
		},
		{
			name:      "type predicate leaves out the parameter name",
			path:      "a.ts",
			src:       `export function isFoo(x: unknown): x is Foo { return true; }`,
			qualified: "isFoo",
			sig:       "export function isFoo(unknown): _ is Foo",
			params:    []model.Param{{Type: "unknown"}},
			results:   "_ is Foo",
		},
		{
			name:      "destructured parameter with type and default",
			path:      "a.ts",
			src:       `function f({ a, b }: Opts = {}, [c]: number[]) {}`,
			qualified: "f",
			sig:       "function f(Opts?, number[])",
			params:    []model.Param{{Type: "Opts", Optional: true}, {Type: "number[]"}},
		},
		{
			name:      "tuple, union, intersection and literal types",
			path:      "a.ts",
			src:       `function f(t: [name: string, age?: number, ...rest: boolean[]], u: | 'a' | "b", i: A & { x: 1 }) {}`,
			qualified: "f",
			sig:       `function f([string, number?, ...boolean[]], "a" | "b", A & {x: 1})`,
			params:    []model.Param{{Type: "[string, number?, ...boolean[]]"}, {Type: `"a" | "b"`}, {Type: "A & {x: 1}"}},
		},
		{
			name:      "public static async method",
			path:      "a.ts",
			src:       `class Config { public static async load(s: string): Promise<Config> { return new Config(); } }`,
			qualified: "Config.load",
			sig:       "static async Config.load(string): Promise<Config>",
			params:    []model.Param{str},
			results:   "Promise<Config>",
		},
		{
			name:      "private method",
			path:      "a.ts",
			src:       `class C { private m(a: number, b?: string) {} }`,
			qualified: "C.m",
			sig:       "private C.m(number, string?)",
			params:    []model.Param{num, {Type: "string", Optional: true}},
		},
		{
			name:      "protected abstract method",
			path:      "a.ts",
			src:       `abstract class C { protected abstract m(): void; }`,
			qualified: "C.m",
			sig:       "protected abstract C.m(): void",
			results:   "void",
		},
		{
			name:      "getter",
			path:      "a.ts",
			src:       "class Config { get x(): string { return \"\"; }\n set x(v: string) {} }",
			qualified: "Config.x",
			sig:       "get Config.x(): string",
			results:   "string",
		},
		{
			name:      "setter",
			path:      "a.ts",
			src:       "class Config { get x(): string { return \"\"; }\n set x(v: string) {} }",
			qualified: "Config.x@set",
			sig:       "set Config.x(string)",
			params:    []model.Param{str},
		},
		{
			name:      "generator method",
			path:      "a.ts",
			src:       `class C { *items(): Iterator<number> {} static async *stream() {} }`,
			qualified: "C.items",
			sig:       "*C.items(): Iterator<number>",
			results:   "Iterator<number>",
		},
		{
			name:      "constructor",
			path:      "a.ts",
			src:       `class C { constructor(public a: string, b?: number) {} }`,
			qualified: "C.constructor",
			sig:       "C.constructor(string, number?)",
			params:    []model.Param{str, {Type: "number", Optional: true}},
		},
		{
			name:      "arrow function field",
			path:      "a.ts",
			src:       `class C { private onClick = async (e: MouseEvent): Promise<void> => {}; }`,
			qualified: "C.onClick",
			sig:       "private async C.onClick(MouseEvent): Promise<void>",
			params:    []model.Param{{Type: "MouseEvent"}},
			results:   "Promise<void>",
		},
		{
			name:      "javascript method",
			path:      "a.js",
			src:       `class Store { static async *all(filter, limit = 10) {} }`,
			qualified: "Store.all",
			sig:       "static async *Store.all(_, ?)",
			params:    []model.Param{{}, {Optional: true}},
		},
		{
			name: "class",
			path: "a.ts",
			src: `export class Config<T> extends Base<T> implements A, ns.B {
  name: string;
  port?: number = 80;
  readonly id: ID;
  private secret: string;
  protected guarded = 1;
  #hidden = 2;
  static count: number;
  load() {}
}`,
			qualified: "Config",
			sig:       "export class Config<T> extends Base<T> implements A, ns.B {name: string; port?: number; readonly id: ID; static count: number}",
			members: []model.Member{
				{Name: "name", Type: "string"},
				{Name: "port", Type: "number", Optional: true},
				{Name: "id", Type: "ID"},
				{Name: "count", Type: "number"},
			},
		},
		{
			name:      "abstract class with parameter properties",
			path:      "a.ts",
			src:       `export abstract class Repo { constructor(public readonly db: Db, private log: Logger, public name?: string, plain: number) {} }`,
			qualified: "Repo",
			sig:       "export abstract class Repo {readonly db: Db; name?: string}",
			members:   []model.Member{{Name: "db", Type: "Db"}, {Name: "name", Type: "string", Optional: true}},
		},
		{
			name:      "javascript class",
			path:      "a.js",
			src:       `class Store extends mixin(Base) { count = 0; #items = []; static s; add() {} }`,
			qualified: "Store",
			sig:       "class Store extends mixin(Base) {count; static s}",
			members:   []model.Member{{Name: "count"}, {Name: "s"}},
		},
		{
			name: "interface",
			path: "a.ts",
			src: `export interface Opts<T> extends Base, Other<T> {
  name: string;
  port?: number;
  load(path: string): Config;
  readonly r: T;
  close?(): void
}`,
			qualified: "Opts",
			sig:       "export interface Opts<T> extends Base, Other<T> {name: string; port?: number; load(string): Config; readonly r: T; close?(): void}",
			members: []model.Member{
				{Name: "name", Type: "string"},
				{Name: "port", Type: "number", Optional: true},
				{Name: "load", Type: "(string): Config", Method: true},
				{Name: "r", Type: "T"},
				{Name: "close", Type: "(): void", Optional: true, Method: true},
			},
		},
		{
			name:      "interface call, construct and index signatures",
			path:      "a.ts",
			src:       `interface F { (x: number): string; new (s: string): F; [key: string]: unknown }`,
			qualified: "F",
			sig:       "interface F {(number): string; new (string): F; [string]: unknown}",
			members: []model.Member{
				{Name: "()", Type: "(number): string", Method: true},
				{Name: "new()", Type: "(string): F", Method: true},
				{Name: "[string]", Type: "unknown"},
			},
		},
		{
			name:      "type alias",
			path:      "a.ts",
			src:       `export type ID = string | number;`,
			qualified: "ID",
			sig:       "export type ID = string | number",
		},
		{
			name:      "generic type alias of a function type",
			path:      "a.ts",
			src:       `type Fn<T extends object = {}> = (value: T, ...rest: unknown[]) => void;`,
			qualified: "Fn",
			sig:       "type Fn<T extends object = {}> = (T, ...unknown[]) => void",
		},
		{
			name:      "object type alias has members",
			path:      "a.ts",
			src:       `export type Point = { x: number, y?: number, };`,
			qualified: "Point",
			sig:       "export type Point = {x: number; y?: number}",
			members:   []model.Member{{Name: "x", Type: "number"}, {Name: "y", Type: "number", Optional: true}},
		},
		{
			name:      "mapped type",
			path:      "a.ts",
			src:       "type RO<T> = { readonly [K in keyof T]?: T[K] };",
			qualified: "RO",
			sig:       "type RO<T> = {readonly [K in keyof T]?: T[K]}",
			members:   []model.Member{{Name: "[K in keyof T]", Type: "T[K]", Optional: true}},
		},
		{
			name:      "conditional type",
			path:      "a.ts",
			src:       "type Elem<T> = T extends Array<infer E> ? E : never;",
			qualified: "Elem",
			sig:       "type Elem<T> = T extends Array<infer E> ? E : never",
		},
		{
			name:      "enum",
			path:      "a.ts",
			src:       `export enum Color { Red, Green = 'g', Blue = 1 << 2, }`,
			qualified: "Color",
			sig:       `export enum Color {Red, Green = "g", Blue = 1<<2}`,
			members:   []model.Member{{Name: "Red"}, {Name: "Green"}, {Name: "Blue"}},
		},
		{
			name:      "const enum",
			path:      "a.ts",
			src:       `export const enum Dir { Up = 1, Down }`,
			qualified: "Dir",
			sig:       "export const enum Dir {Up = 1, Down}",
			members:   []model.Member{{Name: "Up"}, {Name: "Down"}},
		},
		{
			name: "overloads join their signatures and drop params",
			path: "a.ts",
			src: `export function parse(s: string): Config;
export function parse(b: Buffer, strict?: boolean): Config;
export function parse(x: any, strict = false): Config { return x; }`,
			qualified: "parse",
			sig:       "export function parse(string): Config | export function parse(Buffer, boolean?): Config",
		},
		{
			name:      "a single overload gives its params",
			path:      "a.ts",
			src:       "function f(a: string): void;\nfunction f(a: any) {}",
			qualified: "f",
			sig:       "function f(string): void",
			params:    []model.Param{str},
			results:   "void",
		},
		{
			name: "method overloads",
			path: "a.ts",
			src: `class C {
  m(a: string): void;
  m(a: number, b: boolean): void;
  m(a: any, b?: any) {}
}`,
			qualified: "C.m",
			sig:       "C.m(string): void | C.m(number, boolean): void",
		},
		{
			name:      "ambient function",
			path:      "a.ts",
			src:       `export declare function g(a: string): void;`,
			qualified: "g",
			sig:       "export function g(string): void",
			params:    []model.Param{str},
			results:   "void",
		},
		{
			name:      "declared overloads without implementation",
			path:      "a.d.ts",
			src:       "declare function h(a: string): void;\ndeclare function h(a: number): void;",
			qualified: "h",
			sig:       "function h(string): void | function h(number): void",
		},
		{
			name:      "namespace method",
			path:      "a.ts",
			src:       `export namespace Util { export class K { m(a: boolean) {} } }`,
			qualified: "Util.K.m",
			sig:       "Util.K.m(boolean)",
			params:    []model.Param{boolean},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, tt.path, tt.src)
			s := tsTestFind(t, f, tt.qualified)
			if s.Signature != tt.sig {
				t.Errorf("Signature\n got %s\nwant %s", s.Signature, tt.sig)
			}
			if s.SigHash != SigHash(s.Kind+"\x00"+tt.sig) {
				t.Errorf("SigHash does not hash the signature")
			}
			if !reflect.DeepEqual(s.Params, tt.params) {
				t.Errorf("Params\n got %+v\nwant %+v", s.Params, tt.params)
			}
			if s.Results != tt.results {
				t.Errorf("Results = %q, want %q", s.Results, tt.results)
			}
			if !reflect.DeepEqual(s.Members, tt.members) {
				t.Errorf("Members\n got %+v\nwant %+v", s.Members, tt.members)
			}
		})
	}
}

func TestTSOverloadNode(t *testing.T) {
	f := tsTestExtract(t, "a.ts", `function f(a: string): void;
function f(a: number): void;
function f(a: any) {
  return a;
}
declare function g(a: string): void;
declare function g(a: number): void;`)
	if s := tsTestFind(t, f, "f"); s.StartLine != 3 || s.EndLine != 5 {
		t.Errorf("f spans %d-%d, want the implementation 3-5", s.StartLine, s.EndLine)
	}
	if s := tsTestFind(t, f, "g"); s.StartLine != 6 || s.EndLine != 6 {
		t.Errorf("g spans %d-%d, want the first declaration 6-6", s.StartLine, s.EndLine)
	}
	if len(f.Symbols) != 2 {
		t.Errorf("got %d symbols, want 2", len(f.Symbols))
	}
}

func TestTSNodeSpans(t *testing.T) {
	f := tsTestExtract(t, "a.ts", `// leading comment
@decorated
export class A {
  m() {}
}
export const a = () => 1,
  b = () => 2;`)
	tests := []struct {
		qualified  string
		start, end int
	}{
		{"A", 2, 5}, // the export statement, decorator included
		{"A.m", 4, 4},
		{"a", 6, 6}, // one declarator of two
		{"b", 7, 7},
	}
	for _, tt := range tests {
		s := tsTestFind(t, f, tt.qualified)
		if s.StartLine != tt.start || s.EndLine != tt.end {
			t.Errorf("%s spans %d-%d, want %d-%d", tt.qualified, s.StartLine, s.EndLine, tt.start, tt.end)
		}
	}
}

func TestTSHashes(t *testing.T) {
	const base = `export async function load(path: string, strict?: boolean): Promise<Map<string, Config>> {
  return read(path, strict);
}`
	tests := []struct {
		name      string
		path      string
		old, new  string
		qualified string
		sameSig   bool
		sameBody  bool
	}{
		{
			name: "reformatting and comments",
			path: "a.ts",
			old:  base,
			new: `/** Load reads a config. */
export async function load(
  path: string, // the file
  strict ? : boolean,
): Promise<Map<string,Config>>
{ /* read it */ return read( path , strict ) ; }`,
			qualified: "load",
			sameSig:   true,
			sameBody:  true, // a trailing comma and semicolons are formatting
		},
		{
			name: "whitespace and comments only",
			path: "a.ts",
			old:  base,
			new: `export   async function load(path: string, /* why */ strict?: boolean)
    : Promise<Map<string, Config>> {
  // comment
  return read(path,
    strict);
}`,
			qualified: "load",
			sameSig:   true,
			sameBody:  true,
		},
		{
			name: "parameter renames",
			path: "a.ts",
			old:  base,
			new: `export async function load(file: string, exact?: boolean): Promise<Map<string, Config>> {
  return read(file, exact);
}`,
			qualified: "load",
			sameSig:   true,
		},
		{
			name: "body edit",
			path: "a.ts",
			old:  base,
			new: `export async function load(path: string, strict?: boolean): Promise<Map<string, Config>> {
  return read(path, !strict);
}`,
			qualified: "load",
			sameSig:   true,
		},
		{
			name: "type change",
			path: "a.ts",
			old:  base,
			new: `export async function load(path: string, strict?: number): Promise<Map<string, Config>> {
  return read(path, strict);
}`,
			qualified: "load",
		},
		{
			name:      "parameter becomes required",
			path:      "a.ts",
			old:       base,
			new:       strings.Replace(base, "strict?:", "strict:", 1),
			qualified: "load",
		},
		{
			name:      "export removed",
			path:      "a.ts",
			old:       base,
			new:       strings.TrimPrefix(base, "export "),
			qualified: "load",
		},
		{
			name:      "declaration turned into an arrow function",
			path:      "a.ts",
			old:       "export function f(a: string): number { return 1; }",
			new:       "export const f = (a: string): number => 1;",
			qualified: "f",
			sameSig:   true,
		},
		{
			name:      "quote style and trailing commas in types",
			path:      "a.ts",
			old:       `export type Mode = "a" | "b"; export type Fn = (a: string, b: Map<string, number>) => void;`,
			new:       "export type Mode = 'a' | 'b'; export type Fn = (x: string, y: Map<string, number,>,) => void;",
			qualified: "Fn",
			sameSig:   true,
		},
		{
			name:      "quote style in literal types",
			path:      "a.ts",
			old:       `export type Mode = "a" | "b";`,
			new:       "export type Mode =\n  | 'a'\n  | 'b';",
			qualified: "Mode",
			sameSig:   true,
		},
		{
			name: "interface reformatted with other separators",
			path: "a.ts",
			old:  "export interface Opts { name: string; load(path: string): Config; }",
			new: `export interface Opts {
  // the name
  name: string,
  load(file: string): Config
}`,
			qualified: "Opts",
			sameSig:   true,
		},
		{
			name:      "class method renamed parameter in javascript",
			path:      "a.js",
			old:       "class S { add(item, count = 1) { return item; } }",
			new:       "class S {\n  add(thing, n = 1) {\n    return thing;\n  }\n}",
			qualified: "S.add",
			sameSig:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := tsTestFind(t, tsTestExtract(t, tt.path, tt.old), tt.qualified)
			cur := tsTestFind(t, tsTestExtract(t, tt.path, tt.new), tt.qualified)
			if got := old.SigHash == cur.SigHash; got != tt.sameSig {
				t.Errorf("same SigHash = %v, want %v\n old %s\n new %s", got, tt.sameSig, old.Signature, cur.Signature)
			}
			if got := old.BodyHash == cur.BodyHash; got != tt.sameBody {
				t.Errorf("same BodyHash = %v, want %v", got, tt.sameBody)
			}
		})
	}
}

func TestTSClassBodyHash(t *testing.T) {
	const base = `export class Config {
  name: string;
  load(path: string): string {
    return path;
  }
  save() {}
}`
	tests := []struct {
		name          string
		new           string
		sameClassSig  bool
		sameClassBody bool
		sameLoadBody  bool
		sameLoadSig   bool
	}{
		{
			name:          "method body edit",
			new:           strings.Replace(base, "return path;", "return path.trim();", 1),
			sameClassSig:  true,
			sameClassBody: true,
			sameLoadSig:   true,
		},
		{
			name:          "method signature edit",
			new:           strings.Replace(base, "load(path: string)", "load(path: string, strict?: boolean)", 1),
			sameClassSig:  true,
			sameClassBody: true,
		},
		{
			name:          "comment added between members",
			new:           strings.Replace(base, "  save() {}", "  // save writes it\n  save() {}", 1),
			sameClassSig:  true,
			sameClassBody: true,
			sameLoadBody:  true,
			sameLoadSig:   true,
		},
		{
			name:         "property added",
			new:          strings.Replace(base, "name: string;", "name: string;\n  port?: number;", 1),
			sameLoadBody: true,
			sameLoadSig:  true,
		},
		{
			name:         "private property added",
			new:          strings.Replace(base, "name: string;", "name: string;\n  private port = 1;", 1),
			sameClassSig: true,
			sameLoadBody: true,
			sameLoadSig:  true,
		},
	}
	old := tsTestExtract(t, "a.ts", base)
	oldClass, oldLoad := tsTestFind(t, old, "Config"), tsTestFind(t, old, "Config.load")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, "a.ts", tt.new)
			class, load := tsTestFind(t, f, "Config"), tsTestFind(t, f, "Config.load")
			if got := class.SigHash == oldClass.SigHash; got != tt.sameClassSig {
				t.Errorf("same class SigHash = %v, want %v (%s)", got, tt.sameClassSig, class.Signature)
			}
			if got := class.BodyHash == oldClass.BodyHash; got != tt.sameClassBody {
				t.Errorf("same class BodyHash = %v, want %v", got, tt.sameClassBody)
			}
			if got := load.BodyHash == oldLoad.BodyHash; got != tt.sameLoadBody {
				t.Errorf("same method BodyHash = %v, want %v", got, tt.sameLoadBody)
			}
			if got := load.SigHash == oldLoad.SigHash; got != tt.sameLoadSig {
				t.Errorf("same method SigHash = %v, want %v", got, tt.sameLoadSig)
			}
		})
	}
}

func TestTSRefs(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
		want []string
	}{
		{
			name: "calls",
			path: "a.ts",
			src: `f();
a.b.c();
a?.g();
x!.h();
foo().bar();
obj[k]();
require("m");
import("m2");
class K extends B {
  m() { this.x(); super.y(); this.#z(); }
}`,
			want: []string{
				"1 call f",
				"2 call c q=a.b",
				"3 call g q=a",
				"4 call h q=x",
				"5 call bar q=<expr>",
				"5 call foo",
				"9 extends B",
				"10 call #z q=this",
				"10 call x q=this",
				"10 call y q=super",
			},
		},
		{
			name: "multi-line chains take the line of the name",
			path: "a.js",
			src:  "client\n  .connect()\n  .send(data);",
			want: []string{"2 call connect q=client", "3 call send q=<expr>"},
		},
		{
			name: "construct",
			path: "a.js",
			src:  "new Foo();\nnew ns.Bar(1);\nnew Baz;\nnew (pick())();",
			want: []string{"1 construct Foo", "2 construct Bar q=ns", "3 construct Baz", "4 call pick"},
		},
		{
			name: "jsx",
			path: "a.tsx",
			src: `const a = <Foo x={1} />;
const b = <ns.Bar>
  <div>{render()}</div>
  <a.b.Baz />
</ns.Bar>;`,
			want: []string{"1 call Foo", "2 call Bar q=ns", "3 call render", "4 call Baz q=a.b"},
		},
		{
			name: "jsx in javascript",
			path: "a.jsx",
			src:  "const a = <List items={xs}><Item /></List>;",
			want: []string{"1 call Item", "1 call List"},
		},
		{
			name: "types",
			path: "a.ts",
			src: `let a: Foo<Bar, string>;
let b: ns.Baz = c as Cast, d: deep.ns.Qux;
function f<T>(x: T): T { return x satisfies Quux; }
type M<K> = { [P in keyof K]: Zed };
type E<T> = T extends Box<infer U> ? U : Other;
interface I<V> { v: V; w: W }`,
			want: []string{
				"1 type Bar",
				"1 type Foo",
				"2 type Baz q=ns",
				"2 type Cast",
				"2 type Qux q=deep.ns",
				"3 type Quux",
				"4 type Zed",
				"5 type Box",
				"5 type Other",
				"6 type W",
			},
		},
		{
			name: "extends and implements",
			path: "a.ts",
			src: `class A extends B<C> implements D, ns.E, F<G> {}
interface H extends I, ns.J<K> {}
class L extends mixin(M) {}`,
			want: []string{ // sorted by line, then name
				"1 extends B",
				"1 type C",
				"1 extends D",
				"1 extends E q=ns",
				"1 extends F",
				"1 type G",
				"2 extends I",
				"2 extends J q=ns",
				"2 type K",
				"3 call mixin",
			},
		},
		{
			name: "javascript extends",
			path: "a.js",
			src:  "class A extends ns.Base {}\nclass B extends A {}",
			want: []string{"1 extends Base q=ns", "2 extends A"},
		},
		{
			name: "decorators",
			path: "a.ts",
			src: `@Injectable
class S {
  @Input() name: string;
  @ns.Log
  run() {}
}`,
			want: []string{"1 call Injectable", "3 call Input", "4 call Log q=ns"},
		},
		{
			name: "re-exports make no refs",
			path: "a.ts",
			src:  "export { a } from \"./a\";\nexport * from \"./b\";\nexport class C extends D {}",
			want: []string{"3 extends D"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, tt.path, tt.src)
			tsTestDiff(t, "refs", tsTestRefs(f.Refs), tt.want)
			for _, r := range f.Refs {
				if r.Lang != f.Lang || r.Path != tt.path || r.Resolved || r.Module != "" {
					t.Errorf("ref %+v: want lang %s, path %s, unresolved", r, f.Lang, tt.path)
				}
			}
		})
	}
}

func TestTSImports(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
		want []string
	}{
		{
			name: "es modules",
			path: "a.ts",
			src: `import x from "m1";
import * as ns from 'm2';
import { a, b as c } from "m3";
import y, { d } from "m4";
import type { T } from "m5";
import "m6";
import fs = require("fs");
export { e } from "m7";
export * from "m8";
const lazy = import("m9");
import { default as z, type U } from "m10";
import w, * as all from "m11";`,
			want: []string{
				"1 default local=x name= module=m1",
				"2 namespace local=ns name= module=m2",
				"3 named local=a name=a module=m3",
				"3 named local=c name=b module=m3",
				"4 default local=y name= module=m4",
				"4 named local=d name=d module=m4",
				"5 named local=T name=T module=m5",
				"6 namespace local= name= module=m6",
				"7 namespace local=fs name= module=fs",
				"11 default local=z name= module=m10",
				"11 named local=U name=U module=m10",
				"12 default local=w name= module=m11",
				"12 namespace local=all name= module=m11",
			},
		},
		{
			name: "multi-line named imports take each specifier's line",
			path: "a.ts",
			src:  "import {\n  a,\n  b as c,\n} from \"./x\";",
			want: []string{"2 named local=a name=a module=./x", "3 named local=c name=b module=./x"},
		},
		{
			name: "commonjs",
			path: "a.js",
			src: `const p = require("path");
const { join, resolve: res, sep = "/" } = require("path");
const helper = require("./helper").helper;
let notRequire = load("x");
function f() { const lazy = require("./lazy"); }`,
			want: []string{
				"1 namespace local=p name= module=path",
				"2 named local=join name=join module=path",
				"2 named local=res name=resolve module=path",
				"2 named local=sep name=sep module=path",
				"3 named local=helper name=helper module=./helper",
				"5 namespace local=lazy name= module=./lazy",
			},
		},
		{
			name: "commonjs in typescript",
			path: "a.cts",
			src:  `const cfg = require("./config");`,
			want: []string{"1 namespace local=cfg name= module=./config"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, tt.path, tt.src)
			tsTestDiff(t, "imports", tsTestImports(f.Imports), tt.want)
		})
	}
}

func TestTSMalformed(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
		want []string // qualified names that must survive
	}{
		{
			name: "unclosed parameter list",
			path: "a.ts",
			src:  "export function good(a: string): number { return 1; }\nfunction bad(a: string {\n",
			want: []string{"good"},
		},
		{
			name: "garbage between definitions",
			path: "a.ts",
			src:  "export class A { m() {} }\n%%% @@@ ;;; )))\nexport function b() {}\n",
			want: []string{"A", "A.m", "b"},
		},
		{
			// tree-sitter keeps only loose tokens of the class, but the
			// function after it is recovered from the error node.
			name: "broken class member",
			path: "a.tsx",
			src:  "class A {\n  ok() {}\n  broken( {\n}\nfunction after() {}\n",
			want: []string{"after"},
		},
		{
			name: "broken class member followed by more code",
			path: "a.tsx",
			src:  "class A {\n  ok() {}\n  broken( {\n}\nfunction after() {}\nlet x: T = 1;\n",
			want: []string{"A", "A.ok"},
		},
		{
			name: "unclosed jsx",
			path: "a.jsx",
			src:  "function App() { return <div><Foo>; }\nfunction next() {}\n",
			want: []string{"App"},
		},
		{name: "only garbage", path: "a.ts", src: "}}}{{{ class ))) function ((( => <<<"},
		{name: "empty", path: "a.js", src: ""},
		{name: "binary", path: "a.ts", src: "\x00\xff\xfe export function \x00 ("},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tsTestExtract(t, tt.path, tt.src)
			for _, q := range tt.want {
				tsTestFind(t, f, q)
			}
		})
	}
}

// TestTSTruncated parses prefixes of the fixtures, and the fixtures with one
// byte deleted, at a few byte steps: none may fail or panic.
func TestTSTruncated(t *testing.T) {
	for path, src := range tsTestFixtures(t) {
		for i := 0; i <= len(src); i += 5 {
			if _, err := Extract(path, src[:i]); err != nil {
				t.Fatalf("%s cut at %d: %v", path, i, err)
			}
		}
		for i := 0; i < len(src); i += 3 {
			mutated := append(append([]byte{}, src[:i]...), src[i+1:]...)
			if _, err := Extract(path, mutated); err != nil {
				t.Fatalf("%s without byte %d: %v", path, i, err)
			}
		}
	}
}

func tsTestFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "ts", "*"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(p)] = src
	}
	return out
}

func TestTSFixtures(t *testing.T) {
	tests := []struct {
		path    string
		lang    string
		symbols []string // "Qualified kind exported signature"
		refs    []string // a subset of the refs
		imports int
	}{
		{
			path: "testdata/ts/config.ts",
			lang: "typescript",
			symbols: []string{
				"ID type true export type ID = string | number",
				"Source interface true export interface Source<T> extends Base {name: string; port?: number; load(string): Promise<T>}",
				`Mode enum true export enum Mode {Dev, Prod = "prod"}`,
				"Config class true export class Config<T> extends Base implements Source<T> {name: string; port?: number; readonly id: ID; readonly root: string}",
				"Config.constructor method true Config.constructor(string, Logger?)",
				"Config.open method true static async Config.open(string, Options?): Promise<Config<unknown>>",
				"Config.size method true get Config.size(): number",
				"Config.size@set method true set Config.size(number)",
				"Config.load method true Config.load(string): Promise<T> | Config.load(string, T): Promise<T>",
				"Config.reset method false protected Config.reset(): void",
				"Config.#wipe method false Config.#wipe()",
				"merge function true export function merge<T>(T, Partial<T>, ...Partial<T>[]): T",
				"loadConfig function true export async function loadConfig(string, ?): Promise<Config<unknown>>",
				"helper function true export function helper(number): number | export function helper(string): string",
			},
			refs: []string{
				"20 call sealed",
				"21 extends Base",
				"21 extends Source",
				"33 call join q=path",
				"33 call readFile q=fs.promises",
				"34 construct Config",
				"34 call parse",
				"43 call info q=this.log",
				"44 call load q=super",
				"55 call open q=Config",
			},
			imports: 8,
		},
		{
			path: "testdata/ts/widget.tsx",
			lang: "tsx",
			symbols: []string{
				"Props interface false interface Props {title: string; onClose?: () => void}",
				"Widget function true export default function Widget(Props): JSX.Element",
				"Panel function true export function Panel({title: string; children?: React.ReactNode})",
			},
			refs: []string{
				"7 type Element q=JSX",
				"7 type Props",
				"8 call useState q=React",
				"10 call Panel",
				"11 call Close q=icons",
				"12 call Button",
				"13 call format",
				"18 type ReactNode q=React",
			},
			imports: 3,
		},
		{
			path: "testdata/ts/legacy.js",
			lang: "javascript",
			symbols: []string{
				"load function true export function load(_, ?, ..._)",
				"Store class true export class Store extends EventEmitter {count}",
				"Store.create method true static Store.create()",
				"Store.add method true Store.add(_)",
				"Store.size method true get Store.size()",
				"Store.#secret method false Store.#secret()",
				"save function true export function save(_)",
			},
			refs: []string{
				"6 call helper",
				"6 call readFileSync q=fs",
				"9 extends EventEmitter",
				"12 construct Store",
				"13 call push q=this.#items",
			},
			imports: 4,
		},
	}
	fixtures := tsTestFixtures(t)
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			f, err := Extract(tt.path, fixtures[tt.path])
			if err != nil {
				t.Fatal(err)
			}
			if f.Lang != tt.lang {
				t.Errorf("Lang = %s, want %s", f.Lang, tt.lang)
			}
			var got []string
			for _, s := range f.Symbols {
				got = append(got, fmt.Sprintf("%s %s %v %s", s.Qualified, s.Kind, s.Exported, s.Signature))
				if s.Key != model.SymbolKey(f.Lang, tt.path, s.Qualified) || s.Path != tt.path || s.Lang != f.Lang {
					t.Errorf("%s: bad key, path or lang: %+v", s.Qualified, s)
				}
			}
			tsTestDiff(t, "symbols", got, tt.symbols)
			refs := map[string]bool{}
			for _, r := range tsTestRefs(f.Refs) {
				refs[r] = true
			}
			for _, r := range tt.refs {
				if !refs[r] {
					t.Errorf("missing ref %q; have %v", r, tsTestRefs(f.Refs))
				}
			}
			if len(f.Imports) != tt.imports {
				t.Errorf("got %d imports, want %d: %v", len(f.Imports), tt.imports, tsTestImports(f.Imports))
			}
		})
	}
}

// TestTSConcurrent runs Extract from many goroutines; run it with -race.
func TestTSConcurrent(t *testing.T) {
	fixtures := tsTestFixtures(t)
	want := map[string]*File{}
	for path, src := range fixtures {
		f, err := Extract(path, src)
		if err != nil {
			t.Fatal(err)
		}
		want[path] = f
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				for path, src := range fixtures {
					f, err := Extract(path, src)
					if err != nil {
						errs <- err
						return
					}
					if !reflect.DeepEqual(f, want[path]) {
						errs <- fmt.Errorf("%s: concurrent result differs", path)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
