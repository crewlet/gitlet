package footprint

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

type resolveCase struct {
	name     string
	path     string
	ref      string // "name" or "qualifier.name" ("qualifier::name" in Rust)
	imports  []symbols.Import
	types    []string // top-level types the file defines
	inline   []string // containers of functions in inline Rust modules ("outer.inner")
	kind     model.RefKind
	receiver bool // the qualifier is a method call's receiver (model.Ref.Receiver)
	goModule GoModule
	want     string // "name module" ("Container.name module"; ", default export" after it when DefaultExport is set), "name ?" when unresolved, "" when dropped
}

func ns(local, module string) symbols.Import {
	return symbols.Import{Local: local, Module: module, Kind: symbols.ImportNamespace}
}

func nm(local, name, module string) symbols.Import {
	return symbols.Import{Local: local, Name: name, Module: module, Kind: symbols.ImportNamed}
}

func dflt(local, module string) symbols.Import {
	return symbols.Import{Local: local, Module: module, Kind: symbols.ImportDefault}
}

func glob(module string) symbols.Import {
	return symbols.Import{Module: module, Kind: symbols.ImportGlob}
}

// splitRef splits "q.name" or "q::name" at the last separator.
func splitRef(s string) (qualifier, name string) {
	for i := len(s) - 1; i > 0; i-- {
		switch {
		case s[i] == '.':
			return s[:i], s[i+1:]
		case s[i] == ':' && s[i-1] == ':':
			return s[:i-1], s[i+1:]
		}
	}
	return "", s
}

func runResolveCases(t *testing.T, tests []resolveCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lang := symbols.LangForPath(tt.path)
			f := &symbols.File{Lang: lang, Imports: tt.imports}
			for _, name := range tt.types {
				kind := "class"
				if lang == "go" || lang == "rust" {
					kind = "struct"
				}
				f.Symbols = append(f.Symbols, model.Symbol{Lang: lang, Path: tt.path, Name: name, Qualified: name, Kind: kind})
			}
			for _, c := range tt.inline {
				f.Symbols = append(f.Symbols, model.Symbol{Lang: lang, Path: tt.path, Name: "f", Qualified: c + ".f", Container: c, Kind: "function"})
			}
			kind := tt.kind
			if kind == "" {
				kind = model.RefCall
			}
			q, name := splitRef(tt.ref)
			ref := model.Ref{Name: name, Qualifier: q, Kind: kind, Lang: lang, Path: tt.path, Line: 3, Receiver: tt.receiver}
			out := Resolve(f, []model.Ref{ref}, tt.goModule)
			got := ""
			if len(out) == 1 {
				r := out[0]
				switch {
				case r.Resolved && r.Container != "":
					got = r.Container + "." + r.Name + " " + r.Module
				case r.Resolved:
					got = r.Name + " " + r.Module
				case r.Module != "" || r.Container != "":
					got = "unresolved with module " + r.Module + " in " + r.Container
				default:
					got = r.Name + " ?"
				}
				if r.DefaultExport {
					got += ", default export"
				}
				if r.Line != 3 || r.Path != tt.path || r.Qualifier != q || r.Kind != kind || r.Receiver {
					t.Errorf("ref fields changed: %+v", r)
				}
			} else if len(out) > 1 {
				t.Fatalf("Resolve returned %d refs", len(out))
			}
			if got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}

func TestResolveGo(t *testing.T) {
	const file = "internal/engine/run.go"
	const modPath = "example.com/m"
	mod := GoModule{Path: modPath}
	model_ := ns("model", "example.com/m/internal/model")
	runResolveCases(t, []resolveCase{
		{name: "unqualified is the own package", path: file, ref: "run", goModule: mod, want: "run internal/engine"},
		{name: "root package file", path: "main.go", ref: "run", goModule: mod, want: "run ."},
		{name: "import in the module", path: file, ref: "model.Load", imports: []symbols.Import{model_}, goModule: mod, want: "Load internal/model"},
		{name: "aliased import", path: file, ref: "md.Load", imports: []symbols.Import{ns("md", "example.com/m/internal/model")}, goModule: mod, want: "Load internal/model"},
		{name: "module root import", path: file, ref: "m.Run", imports: []symbols.Import{ns("m", modPath)}, goModule: mod, want: "Run ."},
		{name: "selector chain is a value", path: file, ref: "model.Default.Load", imports: []symbols.Import{model_}, goModule: mod, want: "Load ?"},
		{name: "call result is a value", path: file, ref: "model.New().Load", imports: []symbols.Import{model_}, goModule: mod, want: "Load ?"},
		{name: "standard library dropped", path: file, ref: "fmt.Println", imports: []symbols.Import{ns("fmt", "fmt")}, goModule: mod},
		{name: "other module dropped", path: file, ref: "cobra.Command", imports: []symbols.Import{ns("cobra", "github.com/spf13/cobra")}, goModule: mod},
		{name: "module prefix must end at a slash", path: file, ref: "y.F", imports: []symbols.Import{ns("y", "example.com/mx/y")}, goModule: mod},
		{name: "no go.mod keeps hosted paths", path: file, ref: "util.F", imports: []symbols.Import{ns("util", "example.com/m/util")}, want: "F example.com/m/util"},
		{name: "no go.mod drops the standard library", path: file, ref: "http.Get", imports: []symbols.Import{ns("http", "net/http")}},
		{name: "gopkg.in import", path: file, ref: "yaml.Marshal", imports: []symbols.Import{ns("yaml", "gopkg.in/yaml.v3")}, want: "Marshal gopkg.in/yaml.v3"},
		{name: "major version import", path: file, ref: "redis.NewClient", imports: []symbols.Import{ns("redis", "example.com/m/lib/redis/v9")}, goModule: mod, want: "NewClient lib/redis/v9"},
		{name: "package name guessed when the bound name is not an identifier", path: file, ref: "yaml.Marshal", imports: []symbols.Import{ns("go-yaml", "example.com/m/go-yaml")}, goModule: mod, want: "Marshal go-yaml"},
		{name: "explicit alias hides the package name", path: file, ref: "yaml.Marshal", imports: []symbols.Import{ns("y", "example.com/m/yaml")}, goModule: mod, want: "Marshal ?"},
		{name: "blank import binds nothing", path: file, ref: "drivers.Open", imports: []symbols.Import{ns("", "example.com/m/drivers")}, goModule: mod, want: "Open ?"},
		{name: "dot import binds nothing", path: file, ref: "util.F", imports: []symbols.Import{{Module: "example.com/m/util", Kind: symbols.ImportGlob}}, goModule: mod, want: "F ?"},
		{name: "receiver is unresolved", path: file, ref: "c.Load", imports: []symbols.Import{model_}, goModule: mod, want: "Load ?"},
		{name: "method expression on a local type", path: file, ref: "Config.Load", types: []string{"Config"}, goModule: mod, want: "Config.Load internal/engine"},
	})
}

// TestResolveGoModules resolves Go imports of a module in a subdirectory
// and of the repository's other modules to repository paths.
func TestResolveGoModules(t *testing.T) {
	const file = "svc/cmd/app/main.go"
	svc := GoModule{Path: "example.com/svc", Dir: "svc"}
	repo := GoModule{Path: "example.com/svc", Dir: "svc", Others: map[string]string{"example.com/lib": "lib", "example.com/lib/tools": "tools"}}
	runResolveCases(t, []resolveCase{
		{name: "unqualified is the own package", path: file, ref: "run", goModule: svc, want: "run svc/cmd/app"},
		{name: "package of a nested module", path: file, ref: "store.Open", imports: []symbols.Import{ns("store", "example.com/svc/internal/store")}, goModule: svc, want: "Open svc/internal/store"},
		{name: "root of a nested module", path: file, ref: "svc.Run", imports: []symbols.Import{ns("svc", "example.com/svc")}, goModule: svc, want: "Run svc"},
		{name: "module outside the repository dropped", path: file, ref: "x.F", imports: []symbols.Import{ns("x", "example.com/lib/x")}, goModule: svc},
		{name: "other module of the repository", path: file, ref: "x.F", imports: []symbols.Import{ns("x", "example.com/lib/x")}, goModule: repo, want: "F lib/x"},
		{name: "longest module wins", path: file, ref: "gen.Run", imports: []symbols.Import{ns("gen", "example.com/lib/tools/gen")}, goModule: repo, want: "Run tools/gen"},
		{name: "other modules without an own one", path: file, ref: "x.F", imports: []symbols.Import{ns("x", "example.com/lib/x")}, goModule: GoModule{Others: repo.Others}, want: "F lib/x"},
		{name: "hosted path outside every module dropped", path: file, ref: "cobra.Execute", imports: []symbols.Import{ns("cobra", "github.com/spf13/cobra")}, goModule: GoModule{Others: repo.Others}},
	})
}

func TestResolveTypeScript(t *testing.T) {
	const file = "src/app/main.ts"
	runResolveCases(t, []resolveCase{
		{name: "unqualified is the file itself", path: file, ref: "run", want: "run src/app/main"},
		{name: "index file is itself", path: "src/app/index.ts", ref: "run", want: "run src/app/index"},
		{name: "javascript file", path: "web/a.jsx", ref: "run", want: "run web/a"},
		{name: "named import", path: file, ref: "load", imports: []symbols.Import{nm("load", "load", "./config")}, want: "load src/app/config"},
		{name: "aliased named import uses the imported name", path: file, ref: "lc", imports: []symbols.Import{nm("lc", "loadConfig", "../lib/config")}, want: "loadConfig src/lib/config"},
		{name: "default import", path: file, ref: "Cfg", imports: []symbols.Import{dflt("Cfg", "./cfg")}, want: "Cfg src/app/cfg"},
		{name: "default as a named import keeps the local name", path: file, ref: "Cfg", imports: []symbols.Import{nm("Cfg", "default", "./cfg")}, want: "Cfg src/app/cfg"},
		{name: "new of a default import builds the default export", path: file, ref: "Cfg", kind: model.RefConstruct, imports: []symbols.Import{dflt("Cfg", "./cfg")}, want: "Cfg src/app/cfg, default export"},
		{name: "new of default as a named import builds the default export", path: file, ref: "Cfg", kind: model.RefConstruct, imports: []symbols.Import{nm("Cfg", "default", "./cfg")}, want: "Cfg src/app/cfg, default export"},
		{name: "new of an aliased named import uses the imported name", path: file, ref: "C", kind: model.RefConstruct, imports: []symbols.Import{nm("C", "Config", "./cfg")}, want: "Config src/app/cfg"},
		{name: "specifier with an extension", path: file, ref: "u", imports: []symbols.Import{nm("u", "u", "./util.js")}, want: "u src/app/util"},
		{name: "directory specifier", path: file, ref: "x", imports: []symbols.Import{nm("x", "x", ".")}, want: "x src/app"},
		{name: "parent specifier", path: file, ref: "x", imports: []symbols.Import{nm("x", "x", "..")}, want: "x src"},
		{name: "bare specifier dropped", path: file, ref: "React", imports: []symbols.Import{dflt("React", "react")}},
		{name: "alias specifier dropped", path: file, ref: "x", imports: []symbols.Import{nm("x", "x", "@/lib/x")}},
		{name: "specifier outside the repository dropped", path: file, ref: "x", imports: []symbols.Import{nm("x", "x", "../../../x")}},
		{name: "namespace import", path: file, ref: "api.get", imports: []symbols.Import{ns("api", "./api")}, want: "get src/app/api"},
		{name: "named import as a qualifier", path: file, ref: "Store.create", imports: []symbols.Import{nm("Store", "Store", "./store")}, want: "Store.create src/app/store"},
		{name: "aliased class as a qualifier", path: file, ref: "S.create", imports: []symbols.Import{nm("S", "Store", "./store")}, want: "Store.create src/app/store"},
		{name: "default class as a qualifier", path: file, ref: "Store.create", imports: []symbols.Import{dflt("Store", "./store")}, want: "default.create src/app/store"},
		{name: "renamed default class as a qualifier", path: file, ref: "Cfg.load", imports: []symbols.Import{dflt("Cfg", "./config")}, want: "default.load src/app/config"},
		{name: "default as a named import as a qualifier", path: file, ref: "Cfg.load", imports: []symbols.Import{nm("Cfg", "default", "./config")}, want: "default.load src/app/config"},
		{name: "default export through a namespace import", path: file, ref: "cfg.default.load", imports: []symbols.Import{ns("cfg", "./config")}, want: "default.load src/app/config"},
		{name: "lowercase import as a qualifier is an object of its module", path: file, ref: "utils.slug", imports: []symbols.Import{dflt("utils", "./utils")}, want: "slug src/app/utils"},
		{name: "class through a namespace import", path: file, ref: "api.Client.create", imports: []symbols.Import{ns("api", "./api")}, want: "Client.create src/app/api"},
		{name: "bare namespace dropped", path: file, ref: "fs.readFile", imports: []symbols.Import{ns("fs", "node:fs")}},
		{name: "this is unresolved", path: file, ref: "this.load", want: "load ?"},
		{name: "member chain on this is unresolved", path: file, ref: "this.client.get", want: "get ?"},
		{name: "super is unresolved", path: file, ref: "super.load", want: "load ?"},
		{name: "variable is unresolved", path: file, ref: "client.get", want: "get ?"},
		{name: "call expression is unresolved", path: file, ref: "make().get", want: "get ?"},
		{name: "static member of a local class", path: file, ref: "Foo.create", types: []string{"Foo"}, want: "Foo.create src/app/main"},
		{name: "global dropped", path: file, ref: "console.log"},
		{name: "global shadowed by an import", path: file, ref: "JSON.parse", imports: []symbols.Import{ns("JSON", "./json")}, want: "parse src/app/json"},
	})
}

func TestResolvePython(t *testing.T) {
	const file = "pkg/sub/mod.py"
	runResolveCases(t, []resolveCase{
		{name: "unqualified is the file itself", path: file, ref: "run", want: "run pkg/sub/mod"},
		{name: "__init__ is its directory", path: "pkg/sub/__init__.py", ref: "run", want: "run pkg/sub"},
		{name: "absolute from import", path: file, ref: "load", imports: []symbols.Import{nm("load", "load", "pkg.config")}, want: "load pkg/config"},
		{name: "aliased from import uses the imported name", path: file, ref: "lc", imports: []symbols.Import{nm("lc", "load_config", "pkg.config")}, want: "load_config pkg/config"},
		{name: "relative from import", path: file, ref: "load", imports: []symbols.Import{nm("load", "load", ".config")}, want: "load pkg/sub/config"},
		{name: "relative parent from import", path: file, ref: "parse", imports: []symbols.Import{nm("parse", "parse", "..util")}, want: "parse pkg/util"},
		{name: "from the package itself", path: file, ref: "x", imports: []symbols.Import{nm("x", "x", ".")}, want: "x pkg/sub"},
		{name: "relative import from __init__", path: "pkg/__init__.py", ref: "x", imports: []symbols.Import{nm("x", "x", ".sub")}, want: "x pkg/sub"},
		{name: "relative import above the root dropped", path: "top.py", ref: "x", imports: []symbols.Import{nm("x", "x", "..")}},
		{name: "submodule imported from a package", path: file, ref: "helpers.run", imports: []symbols.Import{nm("helpers", "helpers", ".")}, want: "run pkg/sub/helpers"},
		{name: "class imported from a module", path: file, ref: "User.create", imports: []symbols.Import{nm("User", "User", ".models")}, want: "User.create pkg/sub/models"},
		{name: "aliased class", path: file, ref: "U.create", imports: []symbols.Import{nm("U", "User", ".models")}, want: "User.create pkg/sub/models"},
		{name: "nested class imported from a module", path: file, ref: "User.Meta.build", imports: []symbols.Import{nm("User", "User", ".models")}, want: "User.Meta.build pkg/sub/models"},
		{name: "import a.b with the dotted local", path: file, ref: "a.b.f", imports: []symbols.Import{ns("a.b", "a.b")}, want: "f a/b"},
		{name: "import a.b with the bound name", path: file, ref: "a.b.f", imports: []symbols.Import{ns("a", "a.b")}, want: "f a/b"},
		{name: "import a.b reaching a sibling", path: file, ref: "a.c.f", imports: []symbols.Import{ns("a.b", "a.b")}, want: "f a/c"},
		{name: "import as alias", path: file, ref: "cfg.load", imports: []symbols.Import{ns("cfg", "pkg.config")}, want: "load pkg/config"},
		{name: "class through a module path", path: file, ref: "pkg.models.User.get", imports: []symbols.Import{ns("pkg.models", "pkg.models")}, want: "User.get pkg/models"},
		{name: "third-party module is kept", path: file, ref: "np.array", imports: []symbols.Import{ns("np", "numpy")}, want: "array numpy"},
		{name: "standard library module is kept", path: file, ref: "os.path.join", imports: []symbols.Import{ns("os", "os")}, want: "join os/path"},
		{name: "self is unresolved", path: file, ref: "self.load", want: "load ?"},
		{name: "cls is unresolved", path: file, ref: "cls.load", want: "load ?"},
		{name: "super() is unresolved", path: file, ref: "super().load", want: "load ?"},
		{name: "classmethod of a local class", path: file, ref: "Config.load", types: []string{"Config"}, want: "Config.load pkg/sub/mod"},
		{name: "builtin type dropped", path: file, ref: "str.join"},
		{name: "glob import binds nothing", path: file, ref: "load", imports: []symbols.Import{glob("pkg.config")}, want: "load pkg/sub/mod"},
	})
}

func TestResolveRust(t *testing.T) {
	const file = "src/net/client.rs"
	runResolveCases(t, []resolveCase{
		{name: "unqualified is the file itself", path: file, ref: "run", want: "run src/net/client"},
		{name: "lib.rs is its directory", path: "src/lib.rs", ref: "run", want: "run src"},
		{name: "use crate path", path: file, ref: "load", imports: []symbols.Import{nm("load", "load", "crate::config")}, want: "load src/config"},
		{name: "use as alias uses the imported name", path: file, ref: "l", imports: []symbols.Import{nm("l", "load", "crate::config")}, want: "load src/config"},
		{name: "use super path", path: file, ref: "parse", imports: []symbols.Import{nm("parse", "parse", "super::util")}, want: "parse src/net/util"},
		{name: "use self path", path: file, ref: "f", imports: []symbols.Import{nm("f", "f", "self::inner")}, want: "f src/net/client/inner"},
		{name: "use from the crate root", path: "src/main.rs", ref: "load", imports: []symbols.Import{nm("load", "load", "config")}, want: "load src/config"},
		{name: "crate in a workspace member", path: "crates/core/src/a/b.rs", ref: "y", imports: []symbols.Import{nm("y", "y", "crate::x")}, want: "y crates/core/src/x"},
		{name: "crate without a src directory", path: "tools/gen.rs", ref: "y", imports: []symbols.Import{nm("y", "y", "crate::x")}, want: "y x"},
		{name: "module imported as a namespace", path: file, ref: "config::load", imports: []symbols.Import{ns("config", "crate::config")}, want: "load src/config"},
		{name: "module imported by name", path: file, ref: "config::load", imports: []symbols.Import{nm("config", "config", "crate")}, want: "load src/config"},
		{name: "module imported with self", path: file, ref: "config::load", imports: []symbols.Import{ns("config", "crate::config")}, want: "load src/config"},
		{name: "type imported by name", path: file, ref: "Config::new", imports: []symbols.Import{nm("Config", "Config", "crate::config")}, want: "Config.new src/config"},
		{name: "type imported under an alias", path: file, ref: "Cfg::new", imports: []symbols.Import{nm("Cfg", "Config", "crate::config")}, want: "Config.new src/config"},
		{name: "crate path qualifier", path: file, ref: "crate::config::load", want: "load src/config"},
		{name: "crate path through a type", path: file, ref: "crate::config::Config::new", want: "Config.new src/config"},
		{name: "super qualifier", path: file, ref: "super::parse", want: "parse src/net"},
		{name: "super super qualifier", path: file, ref: "super::super::x::f", want: "f src/x"},
		{name: "self path qualifier", path: file, ref: "self::inner::f", want: "f src/net/client/inner"},
		{name: "Self left in a trait is unresolved", path: file, ref: "Self::new", want: "new ?"},
		{name: "Self rewritten to an impl type elsewhere", path: file, ref: "super::Foo::new", want: "Foo.new src/net"},
		{name: "local type", path: file, ref: "Client::new", types: []string{"Client"}, want: "Client.new src/net/client"},
		{name: "child module type", path: file, ref: "helpers::Thing", kind: model.RefType, want: "Thing src/net/client/helpers"},
		{name: "child module path", path: file, ref: "helpers::util::run", want: "run src/net/client/helpers/util"},
		{name: "child module declared with mod", path: file, ref: "retry::run", imports: []symbols.Import{ns("retry", "self::retry")}, want: "run src/net/client/retry"},
		{name: "module declared in the crate root", path: "src/main.rs", ref: "config::load", imports: []symbols.Import{ns("config", "self::config")}, want: "load src/config"},
		{name: "undeclared one-segment call is unresolved", path: file, ref: "retry::run", want: "run ?"},
		{name: "method call on a value", path: file, ref: "c.send", receiver: true, want: "send ?"},
		{name: "method call on a value named like a module", path: file, ref: "retry.run", receiver: true, imports: []symbols.Import{ns("retry", "self::retry")}, want: "run ?"},
		{name: "method call on a value named like an import", path: file, ref: "config.load", receiver: true, imports: []symbols.Import{ns("config", "crate::config")}, want: "load ?"},
		{name: "method call on a value named like a type", path: file, ref: "Client.send", receiver: true, types: []string{"Client"}, want: "send ?"},
		{name: "method call on a path", path: file, ref: "config::DEFAULT.load", receiver: true, imports: []symbols.Import{ns("config", "crate::config")}, want: "load ?"},
		{name: "underscore receiver", path: file, ref: "_.send", receiver: true, want: "send ?"},
		{name: "receiver with try", path: file, ref: "load()?.send", receiver: true, want: "send ?"},
		{name: "inline module call", path: file, ref: "inner::f", inline: []string{"inner"}, want: "inner.f src/net/client"},
		{name: "inline module through self", path: file, ref: "self::inner::f", inline: []string{"inner"}, want: "inner.f src/net/client"},
		{name: "nested inline module", path: file, ref: "self::outer::inner::f", inline: []string{"outer.inner", "outer.Config"}, want: "outer.inner.f src/net/client"},
		{name: "type in an inline module", path: file, ref: "self::outer::Config::new", inline: []string{"outer.inner", "outer.Config"}, want: "outer.Config.new src/net/client"},
		{name: "inline module through crate", path: file, ref: "crate::net::client::inner::f", inline: []string{"inner"}, want: "inner.f src/net/client"},
		{name: "use of an inline module item", path: file, ref: "f", imports: []symbols.Import{nm("f", "f", "self::inner")}, inline: []string{"inner"}, want: "inner.f src/net/client"},
		{name: "impl type is not an inline module", path: file, ref: "self::config::f", inline: []string{"Config"}, want: "f src/net/client/config"},
		{name: "crate root call", path: file, ref: "crate::init", want: "init src"},
		{name: "std import dropped", path: file, ref: "HashMap::new", imports: []symbols.Import{nm("HashMap", "HashMap", "std::collections")}},
		{name: "std path dropped", path: file, ref: "std::mem::swap"},
		{name: "core path dropped", path: file, ref: "core::mem::swap"},
		{name: "extern crate path dropped", path: file, ref: "::serde::to_string"},
		{name: "prelude type dropped", path: file, ref: "Vec::new"},
		{name: "unknown type is unresolved", path: file, ref: "Foo::bar", want: "bar ?"},
		{name: "self receiver is unresolved", path: file, ref: "self.send", receiver: true, want: "send ?"},
		{name: "field chain is unresolved", path: file, ref: "self.client.send", receiver: true, want: "send ?"},
		{name: "glob import binds nothing", path: file, ref: "load", imports: []symbols.Import{glob("crate::config")}, want: "load src/net/client"},
	})
}

func TestResolveEdges(t *testing.T) {
	if got := Resolve(nil, nil, GoModule{}); got != nil {
		t.Errorf("Resolve(nil, nil) = %v", got)
	}
	// Refs without a language take the file's; refs in unknown languages stay
	// unresolved.
	f := &symbols.File{Lang: "go"}
	out := Resolve(f, []model.Ref{{Name: "F", Path: "a/b.go", Line: 1}}, GoModule{})
	if len(out) != 1 || !out[0].Resolved || out[0].Module != "a" || out[0].Lang != "go" {
		t.Errorf("lang fallback = %+v", out)
	}
	out = Resolve(nil, []model.Ref{{Name: "F", Path: "a/b.c", Line: 1, Module: "stale", Resolved: true}}, GoModule{})
	if len(out) != 1 || out[0].Resolved || out[0].Module != "" {
		t.Errorf("unknown language = %+v", out)
	}
	// Resolve leaves its input alone.
	in := []model.Ref{{Name: "f", Lang: "python", Path: "a.py", Line: 1}}
	before := slices.Clone(in)
	Resolve(nil, in, GoModule{})
	if !reflect.DeepEqual(in, before) {
		t.Errorf("input modified: %+v", in)
	}
}

func TestGoPackageName(t *testing.T) {
	tests := map[string]string{
		"fmt":                          "fmt",
		"example.com/m/internal/model": "model",
		"gopkg.in/yaml.v3":             "yaml",
		"github.com/redis/go-redis/v9": "redis",
		"github.com/google/go-cmp":     "cmp",
		"example.com/x/client-go":      "client",
		"example.com/x/my-lib":         "my_lib",
		"v2":                           "v2",
	}
	for in, want := range tests {
		if got := goPackageName(in); got != want {
			t.Errorf("goPackageName(%q) = %q, want %q", in, got, want)
		}
	}
}
