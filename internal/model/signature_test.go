package model

import "testing"

func TestSameSignature(t *testing.T) {
	tests := []struct {
		lang, decl, proposal string
		same                 bool
	}{
		// Go
		{"go", "func Load(string, bool) error", "func Load(string, bool) error", true},
		{"go", "func Load(string, bool) error", "func Load(path string, strict bool) error", true},
		{"go", "func Load(string, bool) error", "Load(path string, strict bool) error", true},
		{"go", "func Load(string, string, bool) error", "func Load(a, b string, strict bool) error", true},
		{"go", "func Load(string, ...int) (Config, error)", "func Load(p string, n ...int) (cfg Config, err error)", true},
		{"go", "func (*Config) Load(string) error", "func (c *Config) Load(path string) error", true},
		{"go", "func (*Config) Load(string) error", "Config.Load(path string) error", true},
		{"go", "func Watch(chan int, func(int) error)", "func Watch(c chan int, fn func(n int) error)", true},
		{"go", "func Watch(<-chan int)", "func Watch(<-chan int)", true},
		{"go", "func Map[T any](T) T", "func Map[T any](v T) T", true},
		{"go", "func Load(string, bool) error", "func Load(path string) error", false},
		{"go", "func Load(string, bool) error", "func Load(path string, strict int) error", false},
		{"go", "func Load(string, bool) error", "func Load(path string, strict bool) (Config, error)", false},
		{"go", "func Load(string, bool) error", "func Read(path string, strict bool) error", false},
		{"go", "func Load(string) error", "func ad(string) error", false},
		{"go", "func Map(int) int", "func Map[T any](T) T", false},
		{"go", "type Config struct {Name string; Port int}", "Config struct {Name string; Port int}", true},
		{"go", "type Config struct {Name string; Port int}", "type Config struct {Name string}", false},

		// TypeScript and JavaScript
		{"typescript", "export async function load(string, boolean?): Promise<Config>", "load(path: string, strict?: boolean): Promise<Config>", true},
		{"typescript", "export async function load(string, boolean?): Promise<Config>", "function load(path: string, strict: boolean = false): Promise<Config>;", true},
		{"typescript", "function f(number?, ?, ...string[]): void", "function f(a?: number, b = 1, ...rest: string[]): void", true},
		{"typescript", "function map<T, U>(T[], (T, number) => U): U[]", "function map<T, U>(xs: T[], fn: (x: T, i: number) => U): U[]", true},
		{"typescript", "function parseConfig(string): Config", "parseConfig(text: string, strict: boolean): Config", false},
		{"typescript", "function parseConfig(string): Config", "parseConfig(text: string): Partial<Config>", false},
		{"tsx", "export function Button({label: string}): JSX.Element", "export function Button(props: {label: string}): JSX.Element", true},
		{"javascript", "function load(_, ?, ..._)", "function load(path, opts = {}, ...rest)", true},
		{"javascript", "function load(_, ?)", "function load(path)", false},

		// Python keeps names
		{"python", "def load(path: str, strict: bool = ?, *args: Any, key: int, **kw) -> Config",
			"def load(path: str, strict: bool = True, *args: Any, key: int, **kw) -> Config:", true},
		{"python", "def C.m(x: int) -> int", "def m(self, x: int) -> int", true},
		{"python", "classmethod def C.make(n: int = ?) -> C", "def make(cls, n: int = 1) -> C", true},
		{"python", "def load(path: str) -> Config", "def load(p: str) -> Config", false},
		{"python", "def load(path: str) -> Config", "def load(path: str) -> dict", false},

		// Rust
		{"rust", "pub fn first<'a>(&'a str, &'a mut Vec<&'a str>) -> &'a str", "fn first<'a>(s: &'a str, v: &'a mut Vec<&'a str>) -> &'a str", true},
		{"rust", "pub fn Foo::into_inner(self) -> u8", "pub fn into_inner(self) -> u8", true},
		{"rust", "fn Task::poll(self: Pin<&mut Self>, &mut Context)", "fn poll(self: Pin<&mut Self>, cx: &mut Context)", true},
		{"rust", "fn Wrap<T>::deref@Deref(&self) -> &T", "fn deref(&self) -> &T", true},
		{"rust", "fn f((u8,u8), impl Into<String>, &[u8])", "fn f((a, b): (u8, u8), s: impl Into<String>, data: &[u8])", true},
		{"rust", "pub fn map<T,U,F:Fn(T)->U>(Vec<T>, F) -> Vec<U> where T:Clone+Send,U:'static",
			"pub fn map<T, U, F: Fn(T) -> U>(v: Vec<T>, f: F) -> Vec<U>", true},
		{"rust", "pub fn load(&std::path::Path) -> Result<Config>", "pub fn load(path: &std::path::Path) -> Result<Config>", true},
		{"rust", "pub fn load(&Path) -> Config", "pub fn load(path: &Path) -> Result<Config>", false},
	}
	for _, tt := range tests {
		if got := SameSignature(tt.lang, tt.decl, tt.proposal); got != tt.same {
			t.Errorf("SameSignature(%s, %q, %q) = %v, want %v", tt.lang, tt.decl, tt.proposal, got, tt.same)
		}
	}
}
