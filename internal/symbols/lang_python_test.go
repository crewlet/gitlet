package symbols

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
)

func pyTestExtract(t *testing.T, path, src string) *File {
	t.Helper()
	f, err := Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("Extract(%s): %v", path, err)
	}
	if f == nil {
		t.Fatalf("Extract(%s) = nil", path)
	}
	return f
}

func pyTestFixture(t *testing.T, name string) *File {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "python", name))
	if err != nil {
		t.Fatal(err)
	}
	return pyTestExtract(t, name, string(src))
}

func pyTestSymbol(t *testing.T, f *File, qualified string) model.Symbol {
	t.Helper()
	s, ok := f.Find(qualified)
	if !ok {
		t.Fatalf("no symbol %q; have %q", qualified, pyTestNames(f))
	}
	return s
}

func pyTestNames(f *File) []string {
	var names []string
	for _, s := range f.Symbols {
		names = append(names, s.Qualified)
	}
	return names
}

// pyTestRefs renders refs as "kind qualifier.name", sorted.
func pyTestRefs(refs []model.Ref) []string {
	var out []string
	for _, r := range refs {
		name := r.Name
		if r.Qualifier != "" {
			name = r.Qualifier + "." + name
		}
		out = append(out, string(r.Kind)+" "+name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func TestPythonLangForPath(t *testing.T) {
	for _, p := range []string{"a.py", "pkg/b.pyi", "C.PY"} {
		if got := LangForPath(p); got != "python" {
			t.Errorf("LangForPath(%q) = %q, want python", p, got)
		}
	}
}

func TestPythonFixtureDefinitions(t *testing.T) {
	f := pyTestFixture(t, "config.py")
	if f.Lang != "python" || f.Package != "" {
		t.Fatalf("lang/package = %q/%q", f.Lang, f.Package)
	}
	want := []struct {
		qualified, kind, container string
		exported                   bool
		start, end                 int
	}{
		{"MAX_RETRIES", "const", "", true, 16, 16},
		{"TIMEOUT", "const", "", true, 17, 17},
		{"_CACHE_SIZE", "const", "", false, 18, 18},
		{"DEFAULT_NAME", "const", "", true, 19, 19},
		{"Config", "class", "", true, 23, 68},
		{"Config.__init__", "method", "Config", true, 32, 35},
		{"Config.from_env", "method", "Config", true, 37, 39},
		{"Config.parse", "method", "Config", true, 41, 43},
		{"Config.address", "method", "Config", true, 45, 47},
		{"Config.address@setter", "method", "Config", true, 49, 52},
		{"Config.address@deleter", "method", "Config", true, 54, 56},
		{"Config.refresh", "method", "Config", true, 58, 59},
		{"Config._reload", "method", "Config", false, 61, 62},
		{"Config.Options", "class", "Config", true, 64, 68},
		{"Config.Options.describe", "method", "Config.Options", true, 67, 68},
		{"_Private", "class", "", false, 71, 73},
		{"_Private.visible", "method", "_Private", false, 72, 73},
		{"load", "function", "", true, 80, 85},
		{"walk", "function", "", true, 88, 90},
		{"_private_helper", "function", "", false, 93, 94},
		{"__getattr__", "function", "", true, 97, 98},
	}
	if len(f.Symbols) != len(want) {
		t.Fatalf("symbols = %q, want %d", pyTestNames(f), len(want))
	}
	for i, w := range want {
		s := f.Symbols[i]
		if s.Qualified != w.qualified || s.Kind != w.kind || s.Container != w.container ||
			s.Exported != w.exported || s.StartLine != w.start || s.EndLine != w.end {
			t.Errorf("symbol %d = %s %s container=%q exported=%v lines %d-%d, want %+v",
				i, s.Qualified, s.Kind, s.Container, s.Exported, s.StartLine, s.EndLine, w)
		}
		if s.Lang != "python" || s.Path != "config.py" || s.Key != model.SymbolKey("python", "config.py", s.Qualified) {
			t.Errorf("%s: lang/path/key = %q/%q/%q", s.Qualified, s.Lang, s.Path, s.Key)
		}
		if s.Name != w.qualified[strings.LastIndexByte(w.qualified, '.')+1:] && !strings.Contains(w.qualified, "@") {
			t.Errorf("%s: name = %q", s.Qualified, s.Name)
		}
	}
	if s := pyTestSymbol(t, f, "Config.address@setter"); s.Name != "address" {
		t.Errorf("setter name = %q, want address", s.Name)
	}
}

func TestPythonSignatures(t *testing.T) {
	tests := []struct {
		name, src, qualified, want string
	}{
		{"every parameter form",
			"def load(path: str, strict: bool = True, *args: Any, key: int, **kw) -> Config: ...",
			"load", "def load(path: str, strict: bool = ?, *args: Any, key: int, **kw) -> Config"},
		{"keyword-only marker", "def f(a, *, b=1): pass", "f", "def f(a, *, b=?)"},
		{"positional-only marker", "def f(a, b=2, /, c=3): pass", "f", "def f(a, b=?, /, c=?)"},
		{"no parameters", "def f(): pass", "f", "def f()"},
		{"async", "async def fetch(url, *, retries=3) -> bytes: pass", "fetch", "async def fetch(url, *, retries=?) -> bytes"},
		{"generator", "def gen(n: int) -> Iterator[int]:\n    yield n", "gen", "generator def gen(n: int) -> Iterator[int]"},
		{"async generator", "async def agen():\n    x = yield", "agen", "async generator def agen()"},
		{"yield in nested function only", "def outer():\n    def inner():\n        yield 1\n    return inner", "outer", "def outer()"},
		{"union and generics", "def f(x: int | None = None) -> dict[str, list[int]]: ...", "f", "def f(x: int|None = ?) -> dict[str,list[int]]"},
		{"quoted forward reference", "def f(x: 'm.Config') -> \"Config\": ...", "f", "def f(x: m.Config) -> Config"},
		{"quoted complex annotation kept", "def f(x: \"list[int]\"): ...", "f", "def f(x: \"list[int]\")"},
		{"type parameters", "def first[T](xs: list[T]) -> T: ...", "first", "def first[T](xs: list[T]) -> T"},
		{"comments decorators and docstrings left out",
			"@app.route(\"/x\")  # route\ndef handler(\n    request,  # the request\n    *,\n    timeout: float = 1.0,  # seconds\n) -> Response:\n    \"\"\"Handle it.\"\"\"\n    return Response()\n",
			"handler", "def handler(request, *, timeout: float = ?) -> Response"},
		{"method drops self", "class C:\n    def m(self, x: int) -> int: ...", "C.m", "def C.m(x: int) -> int"},
		{"method drops annotated self", "class C:\n    def m(self: \"C\", x): ...", "C.m", "def C.m(x)"},
		{"method drops self before /", "class C:\n    def m(self, /, x): ...", "C.m", "def C.m(x)"},
		{"method keeps / after other params", "class C:\n    def m(self, a, /, b): ...", "C.m", "def C.m(a, /, b)"},
		{"classmethod", "class C:\n    @classmethod\n    def make(cls, n: int = 1) -> C: ...", "C.make", "classmethod def C.make(n: int = ?) -> C"},
		{"staticmethod keeps first", "class C:\n    @staticmethod\n    def parse(s: str) -> C: ...", "C.parse", "staticmethod def C.parse(s: str) -> C"},
		{"property", "class C:\n    @property\n    def port(self) -> int: ...", "C.port", "property def C.port -> int"},
		{"cached_property", "class C:\n    @functools.cached_property\n    def size(self) -> int: ...", "C.size", "property def C.size -> int"},
		{"setter", "class C:\n    @port.setter\n    def port(self, value: int): ...", "C.port@setter", "setter def C.port(value: int)"},
		{"deleter", "class C:\n    @port.deleter\n    def port(self): ...", "C.port@deleter", "deleter def C.port"},
		{"async method", "class C:\n    async def run(self) -> None: ...", "C.run", "async def C.run() -> None"},
		{"other decorators ignored", "class C:\n    @abc.abstractmethod\n    @functools.cache\n    def m(self, a): ...", "C.m", "def C.m(a)"},
		{"staticmethod is not a marker at top level", "@staticmethod\ndef f(a): ...", "f", "def f(a)"},
		{"nested class method", "class A:\n    class B:\n        def m(self): ...", "A.B.m", "def A.B.m()"},
		{"class", "class Config(Base, Mixin, metaclass=Meta):\n    name: str\n    port: int = 1\n", "Config",
			"class Config(Base, Mixin, metaclass=Meta) {name: str; port: int = ?}"},
		{"class without bases or fields", "class Empty:\n    pass", "Empty", "class Empty"},
		{"object base dropped", "class Plain(object):\n    x = 1", "Plain", "class Plain"},
		{"empty parentheses dropped", "class Plain():\n    pass", "Plain", "class Plain"},
		{"attribute and generic bases", "class C(m.Base, Generic[T]):\n    pass", "C", "class C(m.Base, Generic[T])"},
		{"class fields: private, unannotated and methods left out",
			"class C:\n    \"\"\"Doc.\"\"\"\n    a: int\n    _b: int = 1\n    c = 2\n    d: list[str] = []\n    def m(self):\n        self.e: int = 3\n",
			"C", "class C {a: int; d: list[str] = ?}"},
		{"nested class", "class Outer:\n    class Inner(Base):\n        x: int\n", "Outer.Inner", "class Outer.Inner(Base) {x: int}"},
		{"generic class", "class Box[T](Base[T]):\n    item: T\n", "Box", "class Box[T](Base[T]) {item: T}"},
		{"decorated class", "@dataclass(frozen=True)\nclass P:\n    x: int\n", "P", "class P {x: int}"},
		{"constant", "MAX = 3", "MAX", "MAX"},
		{"annotated constant", "MAX: int = 3", "MAX", "MAX: int"},
		{"declared constant", "MAX: Final[int]", "MAX", "MAX: Final[int]"},
		{"chained constant", "A = B = 0", "B", "B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, "m.py", tt.src)
			if got := pyTestSymbol(t, f, tt.qualified).Signature; got != tt.want {
				t.Errorf("signature\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestPythonParams(t *testing.T) {
	tests := []struct {
		name, src, qualified string
		params               []model.Param
		results              string
	}{
		{"every form",
			"def load(path: str, strict: bool = True, *args: Any, key: int, **kw) -> Config: ...", "load",
			[]model.Param{
				{Name: "path", Type: "str"},
				{Name: "strict", Type: "bool", Optional: true},
				{Name: "args", Type: "Any", Variadic: true},
				{Name: "key", Type: "int", KeywordOnly: true},
				{Name: "kw", Variadic: true, KeywordOnly: true},
			}, "Config"},
		{"keyword-only after bare star", "def f(a, *, b=1, c): pass", "f",
			[]model.Param{{Name: "a"}, {Name: "b", Optional: true, KeywordOnly: true}, {Name: "c", KeywordOnly: true}}, ""},
		{"positional-only names are not part of the contract", "def f(a: int, b=2, /, c=3): pass", "f",
			[]model.Param{{Type: "int"}, {Optional: true}, {Name: "c", Optional: true}}, ""},
		{"no parameters", "def f() -> None: pass", "f", nil, "None"},
		{"method without self", "class C:\n    def m(self, x: int = 0) -> str: ...", "C.m",
			[]model.Param{{Name: "x", Type: "int", Optional: true}}, "str"},
		{"classmethod without cls", "class C:\n    @classmethod\n    def m(cls, *args): ...", "C.m",
			[]model.Param{{Name: "args", Variadic: true}}, ""},
		{"staticmethod keeps every parameter", "class C:\n    @staticmethod\n    def m(a, b): ...", "C.m",
			[]model.Param{{Name: "a"}, {Name: "b"}}, ""},
		{"method whose first parameter is variadic", "class C:\n    def m(*args): ...", "C.m",
			[]model.Param{{Name: "args", Variadic: true}}, ""},
		{"property getter", "class C:\n    @property\n    def p(self) -> int: ...", "C.p", nil, "int"},
		{"property setter value is anonymous", "class C:\n    @p.setter\n    def p(self, v: int) -> None: ...", "C.p@setter",
			[]model.Param{{Type: "int"}}, "None"},
		{"forward reference type", "def f(c: \"Config\") -> \"m.Result\": ...", "f",
			[]model.Param{{Name: "c", Type: "Config"}}, "m.Result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := pyTestSymbol(t, pyTestExtract(t, "m.py", tt.src), tt.qualified)
			if !reflect.DeepEqual(s.Params, tt.params) {
				t.Errorf("params\n got %+v\nwant %+v", s.Params, tt.params)
			}
			if s.Results != tt.results {
				t.Errorf("results = %q, want %q", s.Results, tt.results)
			}
		})
	}
}

func TestPythonMembers(t *testing.T) {
	src := `class C(Base):
    """Doc."""
    name: str
    port: int = 8080
    _secret: str = ""
    plain = 3
    tags: ClassVar[list[str]] = []
    if sys.version_info >= (3, 11):
        extra: bytes
    else:
        extra: str

    def __init__(self):
        self.later: int = 1
`
	want := []model.Member{
		{Name: "name", Type: "str"},
		{Name: "port", Type: "int", Optional: true},
		{Name: "tags", Type: "ClassVar[list[str]]", Optional: true},
		{Name: "extra", Type: "bytes"},
	}
	s := pyTestSymbol(t, pyTestExtract(t, "m.py", src), "C")
	if !reflect.DeepEqual(s.Members, want) {
		t.Errorf("members\n got %+v\nwant %+v", s.Members, want)
	}
	if s.Params != nil || s.Results != "" {
		t.Errorf("class params/results = %+v/%q", s.Params, s.Results)
	}
}

func TestPythonExported(t *testing.T) {
	tests := []struct {
		name, src string
		want      map[string]bool
	}{
		{"naming conventions", `
def public(): pass
def _private(): pass
def __getattr__(name): pass
MAX = 1
_HIDDEN = 2
class Shown:
    def m(self): pass
    def _m(self): pass
    def __init__(self): pass
    def __call__(self): pass
    class Inner:
        def m(self): pass
    class _Inner:
        def m(self): pass
class _Hidden:
    def m(self): pass
    def __init__(self): pass
    class Inner:
        def m(self): pass
`, map[string]bool{
			"public": true, "_private": false, "__getattr__": true, "MAX": true, "_HIDDEN": false,
			"Shown": true, "Shown.m": true, "Shown._m": false, "Shown.__init__": true, "Shown.__call__": true,
			"Shown.Inner": true, "Shown.Inner.m": true, "Shown._Inner": false, "Shown._Inner.m": false,
			"_Hidden": false, "_Hidden.m": false, "_Hidden.__init__": false, "_Hidden.Inner": false, "_Hidden.Inner.m": false,
		}},
		{"literal __all__", `
__all__ = ["listed", "C", "_private", 'MAX']
def listed(): pass
def unlisted(): pass
def _private(): pass
class C:
    def m(self): pass
class D:
    def m(self): pass
MAX = 1
MIN = 0
`, map[string]bool{
			"listed": true, "unlisted": false, "_private": true, "C": true, "C.m": true,
			"D": false, "D.m": false, "MAX": true, "MIN": false,
		}},
		{"__all__ extended", `
__all__ = ("a",)
__all__ += ["b"]
__all__.append("c")
__all__.extend(["d"])
def a(): pass
def b(): pass
def c(): pass
def d(): pass
def e(): pass
`, map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": false}},
		{"empty __all__", "__all__ = []\ndef a(): pass\n", map[string]bool{"a": false}},
		{"non-literal __all__ falls back to conventions", `
__all__ = ["a"] + other.__all__
def a(): pass
def b(): pass
def _c(): pass
`, map[string]bool{"a": true, "b": true, "_c": false}},
		{"non-literal extend falls back to conventions", `
__all__ = ["a"]
__all__.extend(other.__all__)
def b(): pass
`, map[string]bool{"b": true}},
		{"non-string element falls back to conventions", `
__all__ = ["a", name]
def b(): pass
`, map[string]bool{"b": true}},
		{"removal falls back to conventions", `
__all__ = ["a"]
__all__ -= ["a"]
def a(): pass
def b(): pass
`, map[string]bool{"a": true, "b": true}},
		{"other method falls back to conventions", `
__all__ = ["a"]
__all__.remove("a")
def b(): pass
`, map[string]bool{"b": true}},
		{"other lists are not __all__", `
__all__ = ["a"]
names = ["b"]
names.append("b")
names += ["b"]
register("b")
def a(): pass
def b(): pass
`, map[string]bool{"a": true, "b": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, "m.py", tt.src)
			if len(f.Symbols) != len(tt.want) {
				t.Errorf("symbols = %q, want %d", pyTestNames(f), len(tt.want))
			}
			for q, want := range tt.want {
				if got := pyTestSymbol(t, f, q).Exported; got != want {
					t.Errorf("%s exported = %v, want %v", q, got, want)
				}
			}
		})
	}
}

func TestPythonQualifiedNames(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
	}{
		{"property accessors", `
class C:
    @property
    def x(self) -> int: ...
    @x.setter
    def x(self, v: int) -> None: ...
    @x.deleter
    def x(self) -> None: ...
`, []string{"C", "C.x", "C.x@setter", "C.x@deleter"}},
		{"redefinitions", `
def f(): pass
def f(a): pass
class C:
    def m(self): pass
    def m(self, a): pass
`, []string{"f", "f#2", "C", "C.m", "C.m#2"}},
		{"conditional definitions are module level", `
if sys.version_info >= (3, 11):
    def f(a): pass
else:
    def f(a, b): pass
try:
    from fast import g
except ImportError:
    def g(): pass
finally:
    LIMIT = 3
with lock:
    class C: pass
`, []string{"f", "f#2", "g", "LIMIT", "C"}},
		{"not definitions", `
def outer():
    def inner(): pass
    class Local: pass
    MAX = 3
class C:
    LIMIT = 3
    lambda_ = lambda self: 1
for i in range(3):
    def loop(): pass
lower = 1
Mixed_Case = 2
A, B = 1, 2
obj.ATTR = 3
items[0] = 4
`, []string{"outer", "C"}},
		{"constants", "MAX = 1\nMAX_2: int = 2\nA = B = 3\n_P = 4\nX1 = 5\n", []string{"MAX", "MAX_2", "A", "B", "_P", "X1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pyTestNames(pyTestExtract(t, "m.py", tt.src))
			if !slices.Equal(got, tt.want) {
				t.Errorf("symbols = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPythonOverloads(t *testing.T) {
	tests := []struct {
		name, path, src, qualified, sig string
		start, end                      int // lines of the stored symbol
		params                          []model.Param
		results                         string
		symbols                         []string
	}{
		{"implementation", "m.py", `@overload
def load(path: str) -> Config: ...
@overload
def load(path: str, strict: bool) -> Config: ...
def load(path, strict=False):
    return Config()
`, "load", "def load(path: str) -> Config | def load(path: str, strict: bool) -> Config",
			5, 6, nil, "", []string{"load"}},
		{"stub without implementation", "m.pyi", `@overload
def load(path: str) -> Config: ...
@overload
def load(path: bytes) -> Config: ...
def other() -> None: ...
`, "load", "def load(path: str) -> Config | def load(path: bytes) -> Config",
			3, 4, nil, "", []string{"load", "other"}},
		{"typing.overload in a class", `m.py`, `class C:
    @typing.overload
    @classmethod
    def make(cls, n: int) -> C: ...
    @typing.overload
    @classmethod
    def make(cls, s: str) -> C: ...
    @classmethod
    def make(cls, x):
        return cls()
`, "C.make", "classmethod def C.make(n: int) -> C | classmethod def C.make(s: str) -> C",
			8, 10, nil, "", []string{"C", "C.make"}},
		{"single overload keeps its params", "m.py", `@overload
def f(a: int) -> int: ...
def f(a):
    return a
`, "f", "def f(a: int) -> int", 3, 4, []model.Param{{Name: "a", Type: "int"}}, "int", []string{"f"}},
		{"stubs separated by other definitions", "m.py", `@overload
def f(a: int) -> int: ...
def g(): pass
@overload
def f(a: str) -> str: ...
def f(a):
    return a
def f(b):
    return b
`, "f", "def f(a: int) -> int | def f(a: str) -> str", 6, 7, nil, "", []string{"g", "f", "f#2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, tt.path, tt.src)
			s := pyTestSymbol(t, f, tt.qualified)
			if s.Signature != tt.sig {
				t.Errorf("signature\n got %s\nwant %s", s.Signature, tt.sig)
			}
			if s.StartLine != tt.start || s.EndLine != tt.end {
				t.Errorf("lines = %d-%d, want %d-%d", s.StartLine, s.EndLine, tt.start, tt.end)
			}
			if !reflect.DeepEqual(s.Params, tt.params) || s.Results != tt.results {
				t.Errorf("params/results = %+v/%q, want %+v/%q", s.Params, s.Results, tt.params, tt.results)
			}
			if got := pyTestNames(f); !slices.Equal(got, tt.symbols) {
				t.Errorf("symbols = %q, want %q", got, tt.symbols)
			}
		})
	}
}

func TestPythonStubFixture(t *testing.T) {
	f := pyTestFixture(t, "config.pyi")
	want := map[string]struct {
		exported bool
		sig      string
	}{
		"VERSION":       {true, "VERSION: str"},
		"LIMIT":         {false, "LIMIT: int"},
		"Config":        {true, "class Config(Base) {name: str; port: int}"},
		"Config.get":    {true, "def Config.get(key: str) -> str | def Config.get(key: str, default: int) -> int"},
		"Config.reload": {true, "def Config.reload() -> None"},
		"load":          {true, "def load(path: str) -> Config | def load(path: bytes) -> Config"},
		"helper":        {false, "def helper() -> None"},
	}
	if len(f.Symbols) != len(want) {
		t.Fatalf("symbols = %q", pyTestNames(f))
	}
	for q, w := range want {
		s := pyTestSymbol(t, f, q)
		if s.Exported != w.exported || s.Signature != w.sig {
			t.Errorf("%s = exported %v %q, want %v %q", q, s.Exported, s.Signature, w.exported, w.sig)
		}
	}
	if s := pyTestSymbol(t, f, "Config.get"); s.StartLine != 16 || s.EndLine != 17 {
		t.Errorf("Config.get lines = %d-%d, want the last overload 16-17", s.StartLine, s.EndLine)
	}
}

func TestPythonSigHash(t *testing.T) {
	const base = `class Config(Base):
    """Doc."""
    name: str
    port: int = 1

    @classmethod
    def load(cls, path: str, strict: bool = False, *, key: int = 0) -> "Config":
        """Load it."""
        return cls(path)

MAX: int = 3
`
	tests := []struct {
		name, src string
		same      map[string]bool // qualified -> SigHash unchanged
	}{
		{"reformatted", `class Config(  Base ):
    """Doc."""
    name : str
    port:int=1
    @classmethod
    def load(
        cls,
        path: str,
        strict: bool=False,
        *,
        key:int = 0,
    ) -> Config:
        """Load it."""
        return cls(path)
MAX : int = 3
`, map[string]bool{"Config": true, "Config.load": true, "MAX": true}},
		{"comments and docstrings", `class Config(Base):  # the config
    """Another doc."""
    # the name
    name: str
    port: int = 1  # default port

    @classmethod  # marker
    def load(cls, path: str, strict: bool = False, *, key: int = 0) -> "Config":  # load
        """Load it, differently."""
        # comment
        return cls(path)

MAX: int = 3  # limit
`, map[string]bool{"Config": true, "Config.load": true, "MAX": true}},
		{"default values and bodies", `class Config(Base):
    """Doc."""
    name: str
    port: int = 2

    @classmethod
    @functools.cache
    def load(cls, path: str, strict: bool = True, *, key: int = 9) -> "Config":
        return cls(path.strip())

    def extra(self): pass

MAX: int = 4
`, map[string]bool{"Config": true, "Config.load": true, "MAX": true}},
		{"parameter renamed", strings.Replace(base, "path: str", "file: str", 1),
			map[string]bool{"Config": true, "Config.load": false, "MAX": true}},
		{"annotation added and field added", strings.NewReplacer("strict: bool", "strict: bool | None", "port: int = 1", "port: int = 1\n    host: str = \"\"", "MAX: int", "MAX: Final[int]").Replace(base),
			map[string]bool{"Config": false, "Config.load": false, "MAX": false}},
		{"marker changed and base changed", strings.NewReplacer("@classmethod", "@staticmethod", "(Base)", "(Base, Mixin)").Replace(base),
			map[string]bool{"Config": false, "Config.load": false, "MAX": true}},
	}
	old := pyTestExtract(t, "m.py", base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, "m.py", tt.src)
			for q, same := range tt.same {
				o, n := pyTestSymbol(t, old, q), pyTestSymbol(t, f, q)
				if (o.SigHash == n.SigHash) != same || (o.Signature == n.Signature) != same {
					t.Errorf("%s: signature %q -> %q, want same=%v", q, o.Signature, n.Signature, same)
				}
			}
		})
	}
}

func TestPythonBodyHash(t *testing.T) {
	const base = `@dataclass
class Config:
    """Doc."""
    port: int = 1

    def load(self, path):
        """Load it."""
        return read(path)

    @property
    def size(self) -> int:
        return 1

def helper(x):
    return x + 1
`
	tests := []struct {
		name, src string
		same      map[string]bool // qualified -> BodyHash unchanged
	}{
		{"reformatted with comments", `@dataclass
class Config:  # config
    """Doc."""
    port:int=1  # port

    def load(self,path):
        """Load it."""
        # read it
        return read( path )

    @property
    def size(self)->int:
        return 1
def helper( x ):
    return x+1  # plus one
`, map[string]bool{"Config": true, "Config.load": true, "Config.size": true, "helper": true}},
		{"method body edited", strings.Replace(base, "return read(path)", "return read(path, strict=True)", 1),
			map[string]bool{"Config": true, "Config.load": false, "Config.size": true, "helper": true}},
		{"method decorator edited", strings.Replace(base, "@property", "@functools.cached_property", 1),
			map[string]bool{"Config": true, "Config.load": true, "Config.size": false, "helper": true}},
		{"method docstring edited", strings.Replace(base, "Load it.", "Load the file.", 1),
			map[string]bool{"Config": true, "Config.load": false, "Config.size": true, "helper": true}},
		{"class docstring edited", strings.Replace(base, "Doc.", "Docs.", 1),
			map[string]bool{"Config": false, "Config.load": true, "Config.size": true, "helper": true}},
		{"class field edited", strings.Replace(base, "port: int = 1", "port: int = 2", 1),
			map[string]bool{"Config": false, "Config.load": true, "Config.size": true, "helper": true}},
		{"class decorator edited", strings.Replace(base, "@dataclass", "@dataclass(frozen=True)", 1),
			map[string]bool{"Config": false, "Config.load": true, "Config.size": true, "helper": true}},
		{"function edited", strings.Replace(base, "x + 1", "x + 2", 1),
			map[string]bool{"Config": true, "Config.load": true, "Config.size": true, "helper": false}},
	}
	old := pyTestExtract(t, "m.py", base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, "m.py", tt.src)
			for q, same := range tt.same {
				if got := pyTestSymbol(t, old, q).BodyHash == pyTestSymbol(t, f, q).BodyHash; got != same {
					t.Errorf("%s: body hash same = %v, want %v", q, got, same)
				}
			}
		})
	}
}

func TestPythonRefs(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
	}{
		{"calls", `
def f():
    g()
    a.b.h()
    self.m()
    cls.make()
    super().__init__()
    Config(port=1)
    make().run()
    items[0].close()
    "-".join(parts)
`, []string{
			`call "-".join`, "call Config", "call a.b.h", "call cls.make", "call g", "call items[].close",
			"call make", "call make().run", "call self.m", "call super", "call super().__init__",
		}},
		{"annotations", `
def f(a: Config, b: list[Config], c: Optional[m.Config], d: "Fwd", e: dict[str, int], *args: int, **kw: m.Opt) -> m.Result | None:
    x: Local = None
    y: tuple[int, ...] = ()
type Alias = list[Item]
`, []string{
			"type Config", "type Fwd", "type Item", "type Local", "type Optional", "type m.Config", "type m.Opt", "type m.Result",
		}},
		{"subscript annotations", `
v: typing.Optional[Config] = None
w: Callable[[Request], Response]
z: "pkg.mod.Thing"
`, []string{
			"type Callable", "type Config", "type Request", "type Response", "type pkg.mod.Thing", "type typing.Optional",
		}},
		{"unusual annotations", `
a: Outer[int].Inner
b: get().Attr
c: "not a name"
d: "int"
`, []string{"call get", "type Outer", "type Outer[].Inner"}},
		{"Literal and Annotated arguments", `
x: Literal["a", "b"]
y: Annotated[Config, "the user", Field(gt=0)]
`, []string{"call Field", "type Annotated", "type Config", "type Literal"}},
		{"class bases", `
class C(Base, m.Mixin, Generic[T], Repo[Item], object, *more, metaclass=Meta, **kw):
    pass
class D(dict):
    pass
class E(make_base()):
    pass
`, []string{
			"call make_base", "extends Base", "extends Generic", "extends Repo", "extends m.Mixin", "type Item", "type T",
		}},
		{"decorators", `
@dataclass
@app.route("/x")
@functools.wraps(fn)
class C:
    @property
    def x(self): ...
    @x.setter
    def x(self, v): ...
    @overload
    def m(self): ...
    @typing.overload
    def m(self, a): ...
    @staticmethod
    @cache
    def s(): ...
`, []string{"call app.route", "call cache", "call dataclass", "call functools.wraps"}},
		{"nested functions and lambdas", `
def outer():
    def inner(x: Config):
        return helper(x)
    return lambda: other()
`, []string{"call helper", "call other", "type Config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := pyTestExtract(t, "m.py", tt.src)
			if got := pyTestRefs(f.Refs); !slices.Equal(got, tt.want) {
				t.Errorf("refs\n got %q\nwant %q", got, tt.want)
			}
			for _, r := range f.Refs {
				if r.Lang != "python" || r.Path != "m.py" || r.Module != "" || r.Resolved || r.Line < 1 {
					t.Errorf("ref %+v: want lang, path, line and no resolution", r)
				}
			}
		})
	}
}

func TestPythonRefLines(t *testing.T) {
	src := `import os

def f(x: Config) -> None:
    result = (client
        .fetch()
        .parse())
    return os.path.join(
        "a",
    )
`
	want := []model.Ref{
		{Name: "Config", Kind: model.RefType, Line: 3},
		{Name: "fetch", Qualifier: "client", Kind: model.RefCall, Line: 5},
		{Name: "parse", Qualifier: "client.fetch()", Kind: model.RefCall, Line: 6},
		{Name: "join", Qualifier: "os.path", Kind: model.RefCall, Line: 7},
	}
	for i := range want {
		want[i].Lang, want[i].Path = "python", "m.py"
	}
	f := pyTestExtract(t, "m.py", src)
	if !reflect.DeepEqual(f.Refs, want) {
		t.Errorf("refs\n got %+v\nwant %+v", f.Refs, want)
	}
	if got := RefsWithin(f.Refs, 4, 6); len(got) != 2 {
		t.Errorf("RefsWithin(4, 6) = %+v", got)
	}
}

func TestPythonImports(t *testing.T) {
	src := `from __future__ import annotations
import a.b
import a.b as c, d
from m import x
from m import x as y, z
from . import w
from ..pkg.mod import v
from .sib import (
    p,
    q as r,
)
from m import *
if TYPE_CHECKING:
    from typing_only import T
def f():
    import lazy
    from .inner import thing
`
	want := []Import{
		{Local: "a.b", Module: "a.b", Kind: ImportNamespace, Line: 2},
		{Local: "c", Module: "a.b", Kind: ImportNamespace, Line: 3},
		{Local: "d", Module: "d", Kind: ImportNamespace, Line: 3},
		{Local: "x", Name: "x", Module: "m", Kind: ImportNamed, Line: 4},
		{Local: "y", Name: "x", Module: "m", Kind: ImportNamed, Line: 5},
		{Local: "z", Name: "z", Module: "m", Kind: ImportNamed, Line: 5},
		{Local: "w", Name: "w", Module: ".", Kind: ImportNamed, Line: 6},
		{Local: "v", Name: "v", Module: "..pkg.mod", Kind: ImportNamed, Line: 7},
		{Local: "p", Name: "p", Module: ".sib", Kind: ImportNamed, Line: 9},
		{Local: "r", Name: "q", Module: ".sib", Kind: ImportNamed, Line: 10},
		{Module: "m", Kind: ImportGlob, Line: 12},
		{Local: "T", Name: "T", Module: "typing_only", Kind: ImportNamed, Line: 14},
		{Local: "lazy", Module: "lazy", Kind: ImportNamespace, Line: 16},
		{Local: "thing", Name: "thing", Module: ".inner", Kind: ImportNamed, Line: 17},
	}
	f := pyTestExtract(t, "m.py", src)
	if !reflect.DeepEqual(f.Imports, want) {
		t.Errorf("imports\n got %+v\nwant %+v", f.Imports, want)
	}
	if len(f.Refs) != 0 {
		t.Errorf("imports made refs: %+v", f.Refs)
	}
}

func TestPythonFixtureRefsAndImports(t *testing.T) {
	f := pyTestFixture(t, "config.py")
	for _, want := range []string{
		"call dataclass", "extends Base", "extends Mixin", "call super().__init__", "call cls", "call self._reload",
		"call defaults.registry", "call osp.join", "call Config.parse", "type Registry", "type Config", "type Iterator",
	} {
		if !slices.Contains(pyTestRefs(f.Refs), want) {
			t.Errorf("missing ref %q in %q", want, pyTestRefs(f.Refs))
		}
	}
	var modules []string
	for _, im := range f.Imports {
		modules = append(modules, im.Module+":"+im.Local)
	}
	want := []string{
		"os:os", "os.path:osp", "dataclasses:dataclass", "dataclasses:field", "typing:TYPE_CHECKING", "typing:Any",
		"typing:Iterator", "typing:Optional", "typing:overload", ".:defaults", ".base:Base", ".base:Mixin", ".registry:Registry",
	}
	if !slices.Equal(modules, want) {
		t.Errorf("imports = %q, want %q", modules, want)
	}
}

func TestPythonMalformed(t *testing.T) {
	f := pyTestFixture(t, "broken.py")
	for _, q := range []string{"good", "Fine", "Fine.method", "after"} {
		pyTestSymbol(t, f, q)
	}
	if s := pyTestSymbol(t, f, "good"); s.Signature != "def good(a: int) -> int" {
		t.Errorf("good signature = %q", s.Signature)
	}

	inputs := []string{
		"", "def", "def f", "def f(", "def f(a, *, **): pass", "class", "class C(:", "class C(Base", "@", "@overload\n",
		"@x.setter\n", "from", "from . import", "from m import (", "import", "import a as", "x: ", "MAX: =", "__all__ = [",
		"__all__ += ", "__all__.append(", "class C:\n    @property\n", "class C:\n    def m(self, /", "def f(*args: *",
		"async", "async def", "lambda: (" + strings.Repeat("(", 2000), strings.Repeat("class A:\n  ", 300) + "pass",
		"\x00\xff\xfe", "def f() -> \"unterminated: pass", "type X =", "def f[T: ](x): pass",
	}
	for _, src := range inputs {
		if _, err := Extract("m.py", []byte(src)); err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
	}

	// Every prefix of a real file parses without an error.
	full, err := os.ReadFile(filepath.Join("testdata", "python", "config.py"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n <= len(full); n += 5 {
		if _, err := Extract("config.py", full[:n]); err != nil {
			t.Fatalf("prefix %d: %v", n, err)
		}
	}
}

func TestPythonConcurrent(t *testing.T) {
	var srcs [][]byte
	names := []string{"config.py", "config.pyi", "broken.py"}
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join("testdata", "python", name))
		if err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, src)
	}
	want := make([]*File, len(names))
	for i, name := range names {
		want[i] = pyTestExtract(t, name, string(srcs[i]))
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := range 8 {
		wg.Go(func() {
			for i := range 20 {
				k := (g + i) % len(names)
				f, err := Extract(names[k], srcs[k])
				if err != nil || !reflect.DeepEqual(f, want[k]) {
					errs <- names[k]
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for name := range errs {
		t.Errorf("concurrent Extract(%s) differs from the sequential result", name)
	}
}
