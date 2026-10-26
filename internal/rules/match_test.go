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
		{"resolved ref reaches a method in its module", ref("load", "typescript", "src/a.ts", 1, "src/config"), tsMethod, false, true, true},
		{"static call through the class", model.Ref{Name: "load", Qualifier: "Config", Kind: model.RefCall, Lang: "typescript", Path: "src/a.ts", Module: "src/config", Resolved: true}, tsMethod, false, true, true},
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
		{"go resolved ref to the method's package", ref("Get", "go", "cmd/a.go", 1, "internal/store"), goMethod, false, true, true},
		{"go resolved ref to another package", ref("Get", "go", "cmd/a.go", 1, "internal/cache"), goMethod, true, false, false},
		{"python absolute", ref("load", "python", "src/app/main.py", 1, "src/app/config"), pyFn, false, true, true},
		{"python source root suffix", ref("load", "python", "src/app/main.py", 1, "app/config"), pyFn, false, true, true},
		{"python __init__ stands for its package", ref("setup", "python", "x.py", 1, "pkg"), pyInit, false, true, true},
		{"python method via self", methodRef("save", "self", "python", "pkg/views.py", 1), pyMethod, true, true, false},
		{"python class imported by name", model.Ref{Name: "save", Qualifier: "User", Kind: model.RefCall, Lang: "python", Path: "pkg/views.py", Module: "pkg/models", Resolved: true}, pyMethod, false, true, true},
		{"rust type path", model.Ref{Name: "new", Qualifier: "Parser", Kind: model.RefCall, Lang: "rust", Path: "src/main.rs", Module: "src/parser", Resolved: true}, rsMethod, false, true, true},
		{"rust expanded type path", model.Ref{Name: "new", Qualifier: "crate::parser::Parser", Kind: model.RefCall, Lang: "rust", Path: "src/main.rs", Module: "src/parser", Resolved: true}, rsMethod, false, true, true},
		{"rust other module", model.Ref{Name: "new", Qualifier: "Lexer", Kind: model.RefCall, Lang: "rust", Path: "src/main.rs", Module: "src/lexer", Resolved: true}, rsMethod, false, false, false},
		{"rust lib stands for its directory", ref("run", "rust", "crates/cli/src/main.rs", 1, "crates/core/src"), rsLib, false, true, true},
		{"empty module", model.Ref{Name: "parseConfig", Lang: "typescript", Path: "a.ts", Resolved: true}, tsFn, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, r := Match(tt.ref, tt.sym, tt.unique)
			if m != tt.match || r != tt.resolve {
				t.Fatalf("Match = (%t, %t), want (%t, %t)", m, r, tt.match, tt.resolve)
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
	m := fn("rust", "src/a.rs", "Shape.area@Area", "fn area(&self) -> f64")
	if m.Name != "area" || m.Container != "Shape" {
		t.Fatalf("unique-name suffix not dropped: %+v", m)
	}
	noKey := model.Symbol{Lang: "go", Path: "x.go", Qualified: "F"}
	if keyOf(noKey) != model.SymbolKey("go", "x.go", "F") {
		t.Fatal("keyOf does not derive a missing key")
	}
}
