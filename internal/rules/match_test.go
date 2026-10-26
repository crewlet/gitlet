package rules

import (
	"testing"

	"github.com/crewlet/gitlet/internal/model"
)

func TestMatch(t *testing.T) {
	tsFn := fn("typescript", "src/config.ts", "parseConfig", "function parseConfig(string): Config")
	tsIndex := fn("typescript", "src/util/index.ts", "slug", "function slug(string): string")
	tsMethod := fn("typescript", "src/config.ts", "Config.load", "load(string): void")
	goExp := fn("go", "internal/model/ids.go", "SymbolKey", "func SymbolKey(string, string, string) string")
	goUnexp := fn("go", "internal/model/ids.go", "hashHex", "func hashHex(int, ...string) string")
	goUnexp.Exported = false
	goMethod := fn("go", "internal/store/store.go", "Store.Get", "func (*Store) Get(string) ([]byte, error)")
	goMethodUnexp := fn("go", "internal/store/store.go", "Store.get", "func (*Store) get(string) []byte")
	goMethodUnexp.Exported = false
	pyFn := fn("python", "src/app/config.py", "load", "def load(path: str) -> Config")
	pyInit := fn("python", "pkg/__init__.py", "setup", "def setup()")
	pyMethod := fn("python", "pkg/models.py", "User.save", "def save(self)")
	rsMethod := fn("rust", "src/parser.rs", "Parser.new", "pub fn new() -> Parser")
	rsLib := fn("rust", "crates/core/src/lib.rs", "run", "pub fn run()")
	goCacheNew := fn("go", "internal/store/cache.go", "Cache.New", "func (*Cache) New(string, int) int")
	pyNested := fn("python", "pkg/models.py", "User.Meta", "class Meta")
	pyNested.Kind = "class"
	rsInline := fn("rust", "src/net.rs", "retry.run", "pub fn run()")
	rsInline.Kind = "function" // in the inline module retry
	rsAssoc := fn("rust", "src/parser.rs", "Parser.MAX", "pub const Parser::MAX: usize")
	rsAssoc.Kind = "const"
	tsNsFn := fn("typescript", "src/config.ts", "Config.load", "function load(string): string")
	tsNsFn.Kind = "function" // in namespace Config, or a namespace merged with class Config
	tsCtor := fn("typescript", "src/config.ts", "Config.constructor", "constructor(string)")
	nsCtor := fn("typescript", "src/config.ts", "conf.Config.constructor", "constructor(string)")
	pyCtor := fn("python", "pkg/config.py", "Config.__init__", "def __init__(self, path)")
	pyNestedInit := fn("python", "pkg/models.py", "User.Meta.__init__", "def __init__(self)")
	construct := func(r model.Ref) model.Ref {
		r.Kind = model.RefConstruct
		return r
	}
	typed := func(r model.Ref, qualifier, container string) model.Ref {
		r.Qualifier, r.Container = qualifier, container
		return r
	}
	newDefault := func(r model.Ref) model.Ref { // new X() of a default import or a require binding
		r.Kind, r.DefaultExport = model.RefConstruct, true
		return r
	}
	tsClass := fn("typescript", "src/config.ts", "Config", "class Config")
	tsClass.Kind = "class"

	tests := []struct {
		name           string
		ref            model.Ref
		sym            model.Symbol
		unique         bool
		match, resolve bool
	}{
		{"ts resolved", ref("parseConfig", "typescript", "src/app.ts", 3, "src/config"), tsFn, false, true, true},
		{"tsx refers to ts", ref("parseConfig", "tsx", "src/ui/App.tsx", 3, "src/config"), tsFn, false, true, true},
		{"javascript refers to ts", ref("parseConfig", "javascript", "src/a.js", 3, "src/config"), tsFn, false, true, true},
		{"other family", ref("parseConfig", "python", "src/a.py", 3, "src/config"), tsFn, false, false, false},
		{"other name", ref("parseConf", "typescript", "src/app.ts", 3, "src/config"), tsFn, false, false, false},
		{"other module", ref("parseConfig", "typescript", "src/app.ts", 3, "src/other"), tsFn, false, false, false},
		{"no suffix match in ts", ref("parseConfig", "typescript", "src/app.ts", 3, "config"), tsFn, false, false, false},
		{"ts index stands for its directory", ref("slug", "typescript", "src/a.ts", 1, "src/util"), tsIndex, false, true, true},
		{"ts index by full path", ref("slug", "typescript", "src/a.ts", 1, "src/util/index"), tsIndex, false, true, true},
		{"unqualified ref does not reach a method", ref("load", "typescript", "src/a.ts", 1, "src/config"), tsMethod, false, false, false},
		{"static call through the class", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Config", "Config"), tsMethod, false, true, true},
		{"static call through an aliased class", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "C", "Config"), tsMethod, false, true, true},
		{"static call through another class", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Store", "Store"), tsMethod, false, false, false},
		{"static call through a renamed default import", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"), tsMethod, false, true, true},
		{"static call through a default import of an anonymous class", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"),
			fn("typescript", "src/config.ts", "default.load", "static load(string): void"), false, true, true},
		{"default import does not reach a function", typed(ref("parseConfig", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"), tsFn, false, false, false},
		{"default import does not reach a class in a namespace", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"),
			fn("typescript", "src/config.ts", "conf.Config.load", "static load(string): void"), false, false, false},
		{"static call through a default import of a namespace", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Config", "default"), tsNsFn, false, true, true},
		{"static call through a renamed default import of a namespace", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"), tsNsFn, false, true, true},
		{"default import does not reach a function in a nested namespace", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "Cfg", "default"),
			model.Symbol{Lang: "typescript", Path: "src/config.ts", Name: "load", Qualified: "conf.Config.load", Container: "conf.Config", Kind: "function"}, false, false, false},
		{"default import in another module", typed(ref("load", "typescript", "src/a.ts", 1, "src/other"), "Cfg", "default"), tsMethod, false, false, false},
		{"default container is a JavaScript form", typed(ref("load", "python", "pkg/a.py", 1, "pkg/config"), "default", "default"),
			fn("python", "pkg/config.py", "Config.load", "def load(cls)"), false, false, false},
		{"class qualifier does not reach a function", typed(ref("parseConfig", "typescript", "src/a.ts", 1, "src/config"), "Config", "Config"), tsFn, false, false, false},
		{"unresolved unique method", methodRef("load", "cfg", "typescript", "src/a.ts", 1), tsMethod, true, true, false},
		{"unresolved method not unique", methodRef("load", "cfg", "typescript", "src/a.ts", 1), tsMethod, false, false, false},
		{"unresolved never matches a function", methodRef("parseConfig", "x", "typescript", "src/a.ts", 1), tsFn, true, false, false},
		{"go exported from another package", ref("SymbolKey", "go", "internal/rules/a.go", 1, "internal/model"), goExp, false, true, true},
		{"go unexported from another package", ref("hashHex", "go", "internal/rules/a.go", 1, "internal/model"), goUnexp, false, false, false},
		{"go unexported in its package", ref("hashHex", "go", "internal/model/model.go", 1, "internal/model"), goUnexp, false, true, true},
		{"go module suffix", ref("SymbolKey", "go", "svc/x/a.go", 1, "internal/model"),
			fn("go", "svc/internal/model/ids.go", "SymbolKey", "func SymbolKey() string"), false, true, true},
		{"go suffix needs a whole segment", ref("SymbolKey", "go", "a.go", 1, "model"),
			fn("go", "internal/xmodel/ids.go", "SymbolKey", "func SymbolKey() string"), false, false, false},
		{"go root package", ref("Main", "go", "main.go", 1, "."), fn("go", "app.go", "Main", "func Main()"), false, true, true},
		{"go value receiver unresolved unique", methodRef("Get", "s.store", "go", "cmd/a.go", 1), goMethod, true, true, false},
		{"go unexported method from another package", methodRef("get", "s", "go", "cmd/a.go", 1), goMethodUnexp, true, false, false},
		{"go unexported method in its package", methodRef("get", "s", "go", "internal/store/x.go", 1), goMethodUnexp, true, true, false},
		{"go package call does not reach a method", typed(ref("Get", "go", "cmd/a.go", 1, "internal/store"), "store", ""), goMethod, false, false, false},
		{"go package call does not reach a method of the same name", typed(ref("New", "go", "cmd/a.go", 1, "internal/store"), "store", ""), goCacheNew, false, false, false},
		{"go variable named like a package reaches a unique method unresolved", typed(ref("Get", "go", "cmd/a.go", 1, "internal/store"), "store", ""), goMethod, true, true, false},
		{"go variable named like another package does not reach a method", typed(ref("Get", "go", "cmd/a.go", 1, "internal/cache"), "cache", ""), goMethod, true, false, false},
		{"go unqualified ref does not reach a unique method", ref("Get", "go", "internal/store/x.go", 1, "internal/store"), goMethod, true, false, false},
		{"python variable named like a module reaches a unique method unresolved", typed(ref("save", "python", "pkg/views.py", 1, "pkg/models"), "models", ""), pyMethod, true, true, false},
		{"rust module path reaches a unique method unresolved", typed(ref("new", "rust", "src/main.rs", 1, "src/parser"), "parser", ""), rsMethod, true, true, false},
		{"ts parameter named like a namespace reaches a unique method unresolved", typed(ref("load", "typescript", "src/a.ts", 1, "src/config"), "config", ""), tsMethod, true, true, false},
		{"go method expression", typed(ref("Get", "go", "internal/store/x.go", 1, "internal/store"), "Store", "Store"), goMethod, false, true, true},
		{"go resolved ref to another package", ref("Get", "go", "cmd/a.go", 1, "internal/cache"), goMethod, true, false, false},
		{"python absolute", ref("load", "python", "src/app/main.py", 1, "src/app/config"), pyFn, false, true, true},
		{"python source root suffix", ref("load", "python", "src/app/main.py", 1, "app/config"), pyFn, false, true, true},
		{"python __init__ stands for its package", ref("setup", "python", "x.py", 1, "pkg"), pyInit, false, true, true},
		{"python method via self", methodRef("save", "self", "python", "pkg/views.py", 1), pyMethod, true, true, false},
		{"python class imported by name", typed(ref("save", "python", "pkg/views.py", 1, "pkg/models"), "User", "User"), pyMethod, false, true, true},
		{"python class qualifier does not reach a function", typed(ref("load", "python", "src/app/main.py", 1, "src/app/config"), "Config", "Config"), pyFn, false, false, false},
		{"python unqualified ref does not reach a nested class", ref("Meta", "python", "pkg/models.py", 1, "pkg/models"), pyNested, false, false, false},
		{"python nested class through its class", typed(ref("Meta", "python", "pkg/views.py", 1, "pkg/models"), "User", "User"), pyNested, false, true, true},
		{"rust type path", typed(ref("new", "rust", "src/main.rs", 1, "src/parser"), "Parser", "Parser"), rsMethod, false, true, true},
		{"rust expanded type path", typed(ref("new", "rust", "src/main.rs", 1, "src/parser"), "crate::parser::Parser", "Parser"), rsMethod, false, true, true},
		{"rust other module", typed(ref("new", "rust", "src/main.rs", 1, "src/lexer"), "Lexer", "Lexer"), rsMethod, false, false, false},
		{"rust module path does not reach a method", typed(ref("new", "rust", "src/main.rs", 1, "src/parser"), "parser", ""), rsMethod, false, false, false},
		{"rust inline module path", typed(ref("run", "rust", "src/net.rs", 1, "src/net"), "retry", "retry"), rsInline, false, true, true},
		{"rust unqualified ref inside an inline module", ref("run", "rust", "src/net.rs", 1, "src/net"), rsInline, false, true, true},
		{"rust type in an inline module", typed(ref("new", "rust", "src/main.rs", 1, "src/parser"), "inner::Parser", "inner.Parser"),
			fn("rust", "src/parser.rs", "inner.Parser.new", "pub fn new() -> Parser"), false, true, true},
		{"rust unqualified ref does not reach an associated const", ref("MAX", "rust", "src/parser.rs", 1, "src/parser"), rsAssoc, false, false, false},
		{"rust associated const through its type", typed(ref("MAX", "rust", "src/main.rs", 1, "src/parser"), "Parser", "Parser"), rsAssoc, false, true, true},
		{"rust lib stands for its directory", ref("run", "rust", "crates/cli/src/main.rs", 1, "crates/core/src"), rsLib, false, true, true},
		{"empty module", model.Ref{Name: "parseConfig", Lang: "typescript", Path: "a.ts", Resolved: true}, tsFn, false, false, false},
		{"ts new reaches the constructor", construct(ref("Config", "typescript", "src/app.ts", 1, "src/config")), tsCtor, false, true, true},
		{"javascript new reaches a ts constructor", construct(ref("Config", "javascript", "src/app.js", 1, "src/config")), tsCtor, false, true, true},
		{"ts new through a namespace import", construct(typed(ref("Config", "typescript", "src/app.ts", 1, "src/config"), "cfg", "")), tsCtor, false, true, true},
		{"ts new of another class", construct(ref("Store", "typescript", "src/app.ts", 1, "src/config")), tsCtor, false, false, false},
		{"ts new in another module", construct(ref("Config", "typescript", "src/app.ts", 1, "src/other")), tsCtor, false, false, false},
		{"ts call of the class does not reach the constructor", ref("Config", "typescript", "src/app.ts", 1, "src/config"), tsCtor, false, false, false},
		{"ts unresolved new does not reach the constructor", model.Ref{Name: "Config", Qualifier: "this.types", Kind: model.RefConstruct, Lang: "typescript", Path: "src/app.ts"}, tsCtor, true, false, false},
		{"ts new of a class in a namespace", construct(ref("Config", "typescript", "src/config.ts", 1, "src/config")), nsCtor, false, true, true},
		{"ts new of a renamed default import reaches the constructor", newDefault(ref("Cfg", "typescript", "src/app.ts", 1, "src/config")), tsCtor, false, true, true},
		{"ts new of a default import reaches an anonymous class's constructor", newDefault(ref("Cfg", "typescript", "src/app.ts", 1, "src/config")),
			fn("typescript", "src/config.ts", "default.constructor", "constructor(string)"), false, true, true},
		{"ts new of a default import named like the class", newDefault(ref("Config", "typescript", "src/app.ts", 1, "src/config")), tsCtor, false, true, true},
		{"ts new of a default import still reaches the class of its name", newDefault(ref("Config", "typescript", "src/app.ts", 1, "src/config")), tsClass, false, true, true},
		{"ts new of a default import does not reach another class", newDefault(ref("Cfg", "typescript", "src/app.ts", 1, "src/config")), tsClass, false, false, false},
		{"ts new of a default import does not reach a member of its name", newDefault(ref("load", "typescript", "src/app.ts", 1, "src/config")), tsMethod, true, false, false},
		{"ts new of a default import does not reach a class in a namespace", newDefault(ref("Cfg", "typescript", "src/app.ts", 1, "src/config")), nsCtor, false, false, false},
		{"ts new of a default import in another module", newDefault(ref("Cfg", "typescript", "src/app.ts", 1, "src/other")), tsCtor, false, false, false},
		{"javascript new of a renamed require reaches the constructor", newDefault(ref("Cfg", "javascript", "src/app.js", 1, "src/config")),
			fn("javascript", "src/config.js", "Config.constructor", "constructor(p)"), false, true, true},
		{"ts new of a default import from a version 4 footprint", construct(typed(ref("Cfg", "typescript", "src/app.ts", 1, "src/config"), "", "default")), tsCtor, false, true, true},
		{"ts new of a class through a default import", construct(typed(ref("Inner", "typescript", "src/app.ts", 1, "src/config"), "Cfg", "default")),
			fn("typescript", "src/config.ts", "Config.Inner.constructor", "constructor()"), false, true, true},
		{"ts new of a class through a default import does not reach another class", construct(typed(ref("Inner", "typescript", "src/app.ts", 1, "src/config"), "Cfg", "default")),
			fn("typescript", "src/config.ts", "Config.Other.constructor", "constructor()"), false, false, false},
		{"python call of the class reaches __init__", ref("Config", "python", "pkg/app.py", 1, "pkg/config"), pyCtor, false, true, true},
		{"python call through a module", typed(ref("Config", "python", "pkg/app.py", 1, "pkg/config"), "config", ""), pyCtor, false, true, true},
		{"python call through a class does not reach __init__", typed(ref("Config", "python", "pkg/app.py", 1, "pkg/config"), "Base", "Base"), pyCtor, false, false, false},
		{"python explicit __init__ call", typed(ref("__init__", "python", "pkg/app.py", 1, "pkg/config"), "Config", "Config"), pyCtor, false, true, true},
		{"python call of another class", ref("Settings", "python", "pkg/app.py", 1, "pkg/config"), pyCtor, false, false, false},
		{"python type annotation does not reach __init__", model.Ref{Name: "Config", Kind: model.RefType, Lang: "python", Path: "pkg/app.py", Module: "pkg/config", Resolved: true}, pyCtor, false, false, false},
		{"python nested class constructed through its class", typed(ref("Meta", "python", "pkg/app.py", 1, "pkg/models"), "User", "User"), pyNestedInit, false, true, true},
		{"python nested class constructed without its class", ref("Meta", "python", "pkg/app.py", 1, "pkg/models"), pyNestedInit, false, false, false},
		{"rust new is not a constructor", typed(ref("Parser", "rust", "src/main.rs", 1, "src/parser"), "", ""), rsMethod, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, r := Match(tt.ref, containerVersion, tt.sym, tt.unique)
			if m != tt.match || r != tt.resolve {
				t.Fatalf("Match = (%t, %t), want (%t, %t)", m, r, tt.match, tt.resolve)
			}
		})
	}
}

// A `new Config()` through a default import keeps no container and is
// marked DefaultExport. Gitlets that predate the mark read it as a plain
// `new Config()`, so they still match the class of its name and its
// constructor, as they did before default imports were told apart.
func TestMatchDefaultExportReadWithoutTheMark(t *testing.T) {
	ctor := fn("typescript", "src/config.ts", "Config.constructor", "constructor(string)")
	class := fn("typescript", "src/config.ts", "Config", "class Config")
	class.Kind = "class"
	published := model.Ref{Name: "Config", Kind: model.RefConstruct, Lang: "typescript", Path: "src/app.ts", Line: 2,
		Module: "src/config", Resolved: true, DefaultExport: true}
	older := published
	older.DefaultExport = false // what a gitlet without the field decodes
	for _, r := range []model.Ref{published, older} {
		for _, sym := range []model.Symbol{ctor, class} {
			if m, res := Match(r, containerVersion, sym, false); !m || !res {
				t.Errorf("Match(%+v, %s) = (%t, %t), want (true, true)", r, sym.Qualified, m, res)
			}
		}
	}
}

// A footprint computed before refs recorded their container (version 0, as
// stored by an older gitlet) leaves Container empty whatever its qualifier
// named, so its resolved refs match by module alone, as they did then.
func TestMatchOlderFootprint(t *testing.T) {
	method := fn("python", "cfg.py", "Config.load", "def load(cls)")
	modFn := fn("python", "cfg.py", "load", "def load()")
	elsewhere := fn("python", "other.py", "Config.load", "def load(cls)")
	call := model.Ref{Name: "load", Qualifier: "Config", Kind: model.RefCall, Lang: "python", Path: "app.py", Line: 4, Module: "cfg", Resolved: true}
	tests := []struct {
		name    string
		version int
		sym     model.Symbol
		match   bool
	}{
		{"older: a class-qualified call reaches the method", 0, method, true},
		{"older: and the module's function of that name", 0, modFn, true},
		{"older: not another module", 0, elsewhere, false},
		{"current: no container reaches no method", containerVersion, method, false},
		{"current: but the module's function", containerVersion, modFn, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, r := Match(call, tt.version, tt.sym, false); m != tt.match || r != tt.match {
				t.Fatalf("Match = (%t, %t), want (%t, %t)", m, r, tt.match, tt.match)
			}
		})
	}
}

// Rust tells a module from a value: from version 3 a method call's receiver
// no longer resolves, so a resolved path server::run() names the module's
// item and is no variable named like the module. Go cannot tell.
func TestMatchRustPathByVersion(t *testing.T) {
	method := fn("rust", "src/server.rs", "Server.run", "pub fn run(&self)")
	path := model.Ref{Name: "run", Qualifier: "server", Kind: model.RefCall, Lang: "rust", Path: "src/main.rs", Line: 2, Module: "src/server", Resolved: true}
	goMethod := fn("go", "internal/store/store.go", "Store.Get", "func (*Store) Get(string) ([]byte, error)")
	goCall := model.Ref{Name: "Get", Qualifier: "store", Kind: model.RefCall, Lang: "go", Path: "cmd/a.go", Line: 2, Module: "internal/store", Resolved: true}
	tests := []struct {
		name    string
		ref     model.Ref
		version int
		sym     model.Symbol
		match   bool
	}{
		{"rust receiver could still resolve like a path", path, containerVersion, method, true},
		{"rust path names a module item", path, rustReceiverVersion, method, false},
		{"go package named like a variable", goCall, rustReceiverVersion, goMethod, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, r := Match(tt.ref, tt.version, tt.sym, true); m != tt.match || r {
				t.Fatalf("Match = (%t, %t), want (%t, false)", m, r, tt.match)
			}
		})
	}
}

// Gitlets recorded containers before footprints recorded their version, so
// a footprint without a version whose refs carry a container counts as
// containerVersion, and only one with no container at all as older.
func TestFootprintVersion(t *testing.T) {
	plain := ref("load", "python", "app.py", 1, "cfg")
	classCall := plain
	classCall.Qualifier, classCall.Container = "Config", "Config"
	tests := []struct {
		name string
		fp   model.Footprint
		want int
	}{
		{"no version, no refs", model.Footprint{}, 0},
		{"no version, no container", model.Footprint{Refs: []model.Ref{plain}}, 0},
		{"no version, a container", model.Footprint{Refs: []model.Ref{plain, classCall}}, containerVersion},
		{"a version, no container", model.Footprint{Version: 3, Refs: []model.Ref{plain}}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FootprintVersion(tt.fp); got != tt.want {
				t.Fatalf("FootprintVersion = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBaseKeyAndOldSymbol(t *testing.T) {
	s := fn("go", "a/b.go", "Load", "func Load()")
	if got := baseKey(sigChange(s, "func Load(int)", true)); got != s.Key {
		t.Fatalf("signature change key = %s, want %s", got, s.Key)
	}
	rn := renamedChange(s, "LoadAll")
	if baseKey(rn) != s.Key {
		t.Fatalf("renamed base key = %s, want the old key %s", baseKey(rn), s.Key)
	}
	if old := oldSymbol(rn); old.Name != "Load" || old.Qualified != "Load" || old.Signature != "func Load()" {
		t.Fatalf("old symbol of rename = %+v", old)
	}
	mv := movedChange(s, "c/b.go", true)
	if baseKey(mv) != s.Key || oldSymbol(mv).Path != "a/b.go" {
		t.Fatalf("moved: key %s, old path %s", baseKey(mv), oldSymbol(mv).Path)
	}
	js := fn("javascript", "src/util.js", "parse", "function parse(_)")
	toTS := movedChange(js, "src/util.ts", false)
	toTS.Symbol = fn("typescript", "src/util.ts", "parse", "function parse(string)")
	toTS.OldLang = "javascript"
	if old := oldSymbol(toTS); baseKey(toTS) != js.Key || old.Key != js.Key || old.Lang != "javascript" || old.Path != "src/util.js" {
		t.Fatalf("moved from JavaScript to TypeScript: base key %s, old symbol %+v, want key %s", baseKey(toTS), old, js.Key)
	}
	m := fn("rust", "src/a.rs", "Shape.area@Area", "fn area(&self) -> f64")
	if m.Name != "area" || m.Container != "Shape" {
		t.Fatalf("unique-name suffix not dropped: %+v", m)
	}
	noKey := model.Symbol{Lang: "go", Path: "x.go", Qualified: "F"}
	if keyOf(noKey) != model.SymbolKey("go", "x.go", "F") {
		t.Fatal("keyOf does not derive a missing key")
	}
}
