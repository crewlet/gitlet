package symbols

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
)

const goTestPath = "internal/config/config.go"

func goTestParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Extract(goTestPath, []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if f == nil {
		t.Fatal("Extract returned no file")
	}
	return f
}

func goTestFixture(t *testing.T) ([]byte, *File) {
	t.Helper()
	src, err := os.ReadFile("testdata/go/config.go")
	if err != nil {
		t.Fatal(err)
	}
	return src, goTestParse(t, string(src))
}

func goTestSymbol(t *testing.T, f *File, qualified string) model.Symbol {
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

func TestGoFixtureSymbols(t *testing.T) {
	src, f := goTestFixture(t)
	if f.Lang != "go" || f.Package != "config" {
		t.Errorf("Lang, Package = %q, %q; want go, config", f.Lang, f.Package)
	}
	want := []struct {
		qualified, kind, container string
		exported                   bool
		start, end                 int
		signature                  string
	}{
		{"MaxSize", "const", "", true, 19, 19, "const MaxSize int"},
		{"Kind", "type", "", true, 22, 22, "type Kind int"},
		{"KindFile", "const", "", true, 25, 25, "const KindFile Kind"},
		{"KindEnv", "const", "", true, 26, 26, "const KindEnv Kind"},
		{"kindNone", "const", "", false, 27, 27, "const kindNone Kind"},
		{"Default", "var", "", true, 31, 31, "var Default *Config"},
		{"Count", "var", "", true, 32, 32, "var Count"},
		{"loaded", "var", "", false, 32, 32, "var loaded"},
		{"Config", "struct", "", true, 38, 45, "type Config struct {Name string; Alias string; Port int; Base; *model.Agent; Labels map[string]List[string]}"},
		{"Base", "struct", "", true, 48, 48, "type Base struct {ID ID}"},
		{"ID", "type", "", true, 51, 51, "type ID string"},
		{"Alias", "type", "", true, 52, 52, "type Alias = model.Symbol"},
		{"List", "struct", "", true, 53, 56, "type List[T any] struct {Items []T}"},
		{"Store", "interface", "", true, 60, 64, "type Store interface {Get(context.Context, string) ([]byte, error); put(string, []byte) error; io.Closer}"},
		{"Number", "interface", "", true, 67, 69, "type Number interface {~int | ~float64}"},
		{"memStore", "struct", "", false, 71, 71, "type memStore struct{}"},
		{"init", "function", "", false, 73, 73, "func init()"},
		{"init#2", "function", "", false, 75, 75, "func init()"},
		{"Load", "function", "", true, 78, 89, "func Load(string, bool) (Config, error)"},
		{"Map", "function", "", true, 92, 98, "func Map[T any, U any]([]T, func(T) U) []U"},
		{"Printf", "function", "", true, 101, 101, "func Printf(string, ...any)"},
		{"Config.Load", "method", "Config", true, 104, 106, "func (*Config) Load(string, bool) (Config, error)"},
		{"List.Len", "method", "List", true, 108, 108, "func (List[T]) Len() int"},
		{"memStore.Get", "method", "memStore", true, 110, 112, "func (*memStore) Get(context.Context, string) ([]byte, error)"},
		{"memStore.put", "method", "memStore", false, 114, 117, "func (*memStore) put(string, []byte) error"},
		{"memStore.Close", "method", "memStore", true, 119, 119, "func (*memStore) Close() error"},
	}
	if len(f.Symbols) != len(want) {
		var got []string
		for _, s := range f.Symbols {
			got = append(got, s.Qualified)
		}
		t.Fatalf("got %d symbols %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		s := f.Symbols[i]
		if s.Qualified != w.qualified || s.Kind != w.kind || s.Container != w.container || s.Exported != w.exported {
			t.Errorf("symbol %d = %s %s container=%q exported=%v; want %s %s container=%q exported=%v",
				i, s.Qualified, s.Kind, s.Container, s.Exported, w.qualified, w.kind, w.container, w.exported)
		}
		if s.StartLine != w.start || s.EndLine != w.end {
			t.Errorf("%s lines = %d-%d, want %d-%d", s.Qualified, s.StartLine, s.EndLine, w.start, w.end)
		}
		if s.Signature != w.signature {
			t.Errorf("%s signature\n got %q\nwant %q", s.Qualified, s.Signature, w.signature)
		}
		if s.Lang != "go" || s.Path != goTestPath || s.Key != model.SymbolKey("go", goTestPath, s.Qualified) {
			t.Errorf("%s: lang %q, path %q, key %q", s.Qualified, s.Lang, s.Path, s.Key)
		}
		if s.SigHash != SigHash(s.Kind+"\x00"+s.Signature) || s.BodyHash == "" {
			t.Errorf("%s: sigHash %q, bodyHash %q", s.Qualified, s.SigHash, s.BodyHash)
		}
	}

	// An ungrouped declaration is the symbol's whole text; in a group, the
	// symbol is its spec.
	text := func(q string) string {
		s := goTestSymbol(t, f, q)
		return string(src[s.StartByte:s.EndByte])
	}
	for q, prefix := range map[string]string{
		"MaxSize":  "const MaxSize int",
		"Kind":     "type Kind int",
		"KindEnv":  "KindEnv",
		"Count":    "loaded, Count",
		"Config":   "type Config struct {",
		"ID":       "ID          string",
		"List":     "List[T any] struct {",
		"Load":     "func Load(",
		"List.Len": "func (l List[T]) Len()",
	} {
		if got := text(q); !strings.HasPrefix(got, prefix) {
			t.Errorf("%s text = %q, want prefix %q", q, got, prefix)
		}
	}
}

func TestGoFixtureRefsImports(t *testing.T) {
	_, f := goTestFixture(t)
	imports := []Import{
		{Local: "context", Module: "context", Kind: ImportNamespace, Line: 5},
		{Local: "", Module: "embed", Kind: ImportNamespace, Line: 6},
		{Local: "fmt", Module: "fmt", Kind: ImportNamespace, Line: 7},
		{Local: "io", Module: "io", Kind: ImportNamespace, Line: 8},
		{Local: "os", Module: "os", Kind: ImportNamespace, Line: 9},
		{Local: "", Module: "strings", Kind: ImportGlob, Line: 10},
		{Local: "mod", Module: "example.com/mod/v2", Kind: ImportNamespace, Line: 12},
		{Local: "yaml", Module: "gopkg.in/yaml.v3", Kind: ImportNamespace, Line: 13},
		{Local: "model", Module: "github.com/crewlet/gitlet/internal/model", Kind: ImportNamespace, Line: 15},
	}
	if !reflect.DeepEqual(f.Imports, imports) {
		t.Errorf("imports\n got %+v\nwant %+v", f.Imports, imports)
	}

	tests := []struct {
		qualified string
		refs      []goTestRef
	}{
		{"Config", []goTestRef{
			{42, "Base", "", model.RefExtends},
			{43, "Agent", "model", model.RefExtends},
			{44, "List", "", model.RefType},
		}},
		{"Store", []goTestRef{
			{61, "Context", "context", model.RefType},
			{63, "Closer", "io", model.RefExtends},
		}},
		{"List", nil}, // T is a type parameter
		{"init", []goTestRef{{73, "Config", "", model.RefConstruct}}},
		{"init#2", []goTestRef{{75, "Register", "mod", model.RefCall}}},
		{"Load", []goTestRef{
			{78, "Config", "", model.RefType},
			{79, "ReadFile", "os", model.RefCall},
			{81, "Config", "", model.RefConstruct},
			{81, "Errorf", "fmt", model.RefCall},
			{83, "Config", "", model.RefType},
			{84, "Unmarshal", "yaml", model.RefCall},
			{85, "Config", "", model.RefConstruct},
			{87, "TrimSpace", "", model.RefCall},
		}},
		{"Map", []goTestRef{{95, "f", "", model.RefCall}}},
		{"Config.Load", []goTestRef{
			{104, "Config", "", model.RefType},
			{105, "Load", "", model.RefCall},
		}},
		{"memStore.Get", []goTestRef{
			{110, "Context", "context", model.RefType},
			{110, "memStore", "", model.RefType},
		}},
	}
	for _, tt := range tests {
		s := goTestSymbol(t, f, tt.qualified)
		var got []goTestRef
		for _, r := range RefsWithin(f.Refs, s.StartLine, s.EndLine) {
			got = append(got, goTestRef{r.Line, r.Name, r.Qualifier, r.Kind})
		}
		if !reflect.DeepEqual(got, tt.refs) {
			t.Errorf("%s refs:\n%s", tt.qualified, goTestDiff(got, tt.refs))
		}
	}
}

func TestGoSignatures(t *testing.T) {
	tests := []struct {
		name, src, qualified, want string
	}{
		{"func", "func Load(path string, strict bool) (Config, error) { return Config{}, nil }", "Load", "func Load(string, bool) (Config, error)"},
		{"no params", "func Init() {}", "Init", "func Init()"},
		{"single result", "func Len() int { return 0 }", "Len", "func Len() int"},
		{"parenthesized result", "func Err() (error) { return nil }", "Err", "func Err() error"},
		{"named result", "func Err() (err error) { return }", "Err", "func Err() error"},
		{"named results", "func Div(a, b int) (q, r int, err error) { return }", "Div", "func Div(int, int) (int, int, error)"},
		{"generic", "func Map[T any, U any](xs []T, f func(T) U) []U { return nil }", "Map", "func Map[T any, U any]([]T, func(T) U) []U"},
		{"grouped type params", "func Keys[K, V any](m map[K]V) []K { return nil }", "Keys", "func Keys[K any, V any](map[K]V) []K"},
		{"constraint", "func Sum[S ~[]E, E interface{ ~int | ~float64 }](s S) E { return 0 }", "Sum", "func Sum[S ~[]E, E interface {~int | ~float64}](S) E"},
		{"variadic", "func Printf(format string, args ...any) {}", "Printf", "func Printf(string, ...any)"},
		{"unnamed variadic", "func Join(...string) string", "Join", "func Join(...string) string"},
		{"func param", "func Do(f func(ctx context.Context, n int) (string, error)) {}", "Do", "func Do(func(context.Context, int) (string, error))"},
		{"channels", "func Pipe(in <-chan int, out chan<- string, both chan error) {}", "Pipe", "func Pipe(<-chan int, chan<- string, chan error)"},
		{"arrays", "func Sum(a [4]int, b [N * 2]byte, c [][]*pkg.T) {}", "Sum", "func Sum([4]int, [N*2]byte, [][]*pkg.T)"},
		{"maps", "func Index(m map[string][]int, n map[Key]struct{}) {}", "Index", "func Index(map[string][]int, map[Key]struct{})"},
		{"method", "func (c *Config) Load(path string, strict bool) (Config, error) { return *c, nil }", "Config.Load", "func (*Config) Load(string, bool) (Config, error)"},
		{"generic receiver", "func (l List[T]) Len() int { return 0 }", "List.Len", "func (List[T]) Len() int"},
		{"generic pointer receiver", "func (m *Map[K, V]) Put(k K, v V) {}", "Map.Put", "func (*Map[K, V]) Put(K, V)"},
		{"unnamed receiver", "func (Config) String() string { return \"\" }", "Config.String", "func (Config) String() string"},
		{"parenthesized receiver", "func (c (*Config)) Reset() {}", "Config.Reset", "func (*Config) Reset()"},
		{"struct", "type Config struct {\n\tName string `json:\"name\"`\n\tport int\n\tPort int\n\tBase\n\t*Other\n}", "Config", "type Config struct {Name string; Port int; Base; *Other}"},
		{"struct grouped fields", "type Point struct{ X, Y, z float64 }", "Point", "type Point struct {X float64; Y float64}"},
		{"struct generic", "type List[T any] struct {\n\tItems []T\n\tsize int\n}", "List", "type List[T any] struct {Items []T}"},
		{"struct embedded qualified", "type Wrap struct {\n\tio.Reader\n\t*pkg.Other\n\tList[int]\n\tbase\n}", "Wrap", "type Wrap struct {io.Reader; *pkg.Other; List[int]; base}"},
		{"struct empty", "type Empty struct{}", "Empty", "type Empty struct{}"},
		{"struct unexported only", "type hidden struct{ a, b int }", "hidden", "type hidden struct{}"},
		{"struct nested literal", "type Opts struct {\n\tLimits struct {\n\t\tmax int\n\t\tMin int\n\t}\n}", "Opts", "type Opts struct {Limits struct {max int; Min int}}"},
		{"struct func field", "type Hook struct{ Run func(name string) error }", "Hook", "type Hook struct {Run func(string) error}"},
		{"interface", "type Store interface {\n\tGet(key string) ([]byte, error)\n\tio.Closer\n}", "Store", "type Store interface {Get(string) ([]byte, error); io.Closer}"},
		{"interface unexported method", "type sealed interface {\n\tName() string\n\tseal()\n}", "sealed", "type sealed interface {Name() string; seal()}"},
		{"interface union", "type Number interface {\n\t~int | ~float64\n}", "Number", "type Number interface {~int | ~float64}"},
		{"interface generic", "type Getter[T any] interface{ Get() T }", "Getter", "type Getter[T any] interface {Get() T}"},
		{"interface empty", "type Any interface{}", "Any", "type Any interface{}"},
		{"defined type", "type ID string", "ID", "type ID string"},
		{"defined generic type", "type Set[T comparable] map[T]struct{}", "Set", "type Set[T comparable] map[T]struct{}"},
		{"func type", "type Handler func(w io.Writer, r *Request) error", "Handler", "type Handler func(io.Writer, *Request) error"},
		{"pointer type", "type Ref *Config", "Ref", "type Ref *Config"},
		{"alias", "type Alias = pkg.T", "Alias", "type Alias = pkg.T"},
		{"generic alias", "type Vec[T any] = []T", "Vec", "type Vec[T any] = []T"},
		{"grouped types", "type (\n\tA int\n\tB = A\n)", "B", "type B = A"},
		{"const typed", "const MaxSize int = 10", "MaxSize", "const MaxSize int"},
		{"const untyped", "const MaxSize = 10", "MaxSize", "const MaxSize"},
		{"const iota repeats type", "const (\n\tRed Color = iota\n\tGreen\n)", "Green", "const Green Color"},
		{"const untyped iota", "const (\n\tA = iota\n\tB\n)", "B", "const B"},
		{"const new type", "const (\n\tA Color = 1\n\tB = 2\n\tC\n)", "C", "const C"},
		{"var typed", "var Default *Config", "Default", "var Default *Config"},
		{"var untyped", "var Default = &Config{}", "Default", "var Default"},
		{"var func", "var Hook func(int) error", "Hook", "var Hook func(int) error"},
		{"var multi", "var a, B int = 1, 2", "B", "var B int"},
		{"var group", "var (\n\tx int\n\tY, z string\n)", "Y", "var Y string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := goTestParse(t, "package p\n\n"+tt.src+"\n")
			if got := goTestSymbol(t, f, tt.qualified).Signature; got != tt.want {
				t.Errorf("signature\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestGoParamsResults(t *testing.T) {
	tests := []struct {
		src, qualified string
		params         []model.Param
		results        string
	}{
		{"func Init() {}", "Init", nil, ""},
		{"func Load(path string, strict bool) (Config, error)", "Load",
			[]model.Param{{Type: "string"}, {Type: "bool"}}, "(Config, error)"},
		{"func Add(a, b int) (sum int)", "Add",
			[]model.Param{{Type: "int"}, {Type: "int"}}, "int"},
		{"func Printf(format string, args ...any) error", "Printf",
			[]model.Param{{Type: "string"}, {Type: "any", Variadic: true}}, "error"},
		{"func Join(sep string, parts ...[]byte) []byte", "Join",
			[]model.Param{{Type: "string"}, {Type: "[]byte", Variadic: true}}, "[]byte"},
		{"func Div(int, int) (q, r int)", "Div",
			[]model.Param{{Type: "int"}, {Type: "int"}}, "(int, int)"},
		{"func Map[T, U any](xs []T, f func(x T) (U, error)) map[string]U", "Map",
			[]model.Param{{Type: "[]T"}, {Type: "func(T) (U, error)"}}, "map[string]U"},
		{"func (c *Config) Get(_ context.Context, key /* the key */ string) ([]byte, error)", "Config.Get",
			[]model.Param{{Type: "context.Context"}, {Type: "string"}}, "([]byte, error)"},
	}
	for _, tt := range tests {
		t.Run(tt.qualified, func(t *testing.T) {
			s := goTestSymbol(t, goTestParse(t, "package p\n\n"+tt.src+"\n"), tt.qualified)
			if !reflect.DeepEqual(s.Params, tt.params) {
				t.Errorf("params = %+v, want %+v", s.Params, tt.params)
			}
			if s.Results != tt.results {
				t.Errorf("results = %q, want %q", s.Results, tt.results)
			}
		})
	}
}

func TestGoMembers(t *testing.T) {
	_, f := goTestFixture(t)
	tests := []struct {
		qualified string
		members   []model.Member
	}{
		{"Config", []model.Member{
			{Name: "Name", Type: "string"},
			{Name: "Alias", Type: "string"},
			{Name: "Port", Type: "int"},
			{Name: "Base", Type: "Base"},
			{Name: "Agent", Type: "*model.Agent"},
			{Name: "Labels", Type: "map[string]List[string]"},
		}},
		{"List", []model.Member{{Name: "Items", Type: "[]T"}}},
		{"memStore", nil},
		{"Store", []model.Member{
			{Name: "Get", Type: "(context.Context, string) ([]byte, error)", Method: true},
			{Name: "put", Type: "(string, []byte) error", Method: true},
			{Name: "Closer", Type: "io.Closer"},
		}},
		{"Number", []model.Member{{Name: "~int | ~float64", Type: "~int | ~float64"}}},
		{"ID", nil},
		{"Alias", nil},
		{"Load", nil},
	}
	for _, tt := range tests {
		if got := goTestSymbol(t, f, tt.qualified).Members; !reflect.DeepEqual(got, tt.members) {
			t.Errorf("%s members\n got %+v\nwant %+v", tt.qualified, got, tt.members)
		}
	}

	f = goTestParse(t, `package p

type Box[T any] interface {
	Container[T]
	pkg.Sizer
	Put(item T) (ok bool)
}

type Raw = struct {
	Data []byte
	n    int
}
`)
	box := []model.Member{
		{Name: "Container", Type: "Container[T]"},
		{Name: "Sizer", Type: "pkg.Sizer"},
		{Name: "Put", Type: "(T) bool", Method: true},
	}
	if got := goTestSymbol(t, f, "Box").Members; !reflect.DeepEqual(got, box) {
		t.Errorf("Box members\n got %+v\nwant %+v", got, box)
	}
	raw := goTestSymbol(t, f, "Raw")
	if want := []model.Member{{Name: "Data", Type: "[]byte"}}; !reflect.DeepEqual(raw.Members, want) {
		t.Errorf("Raw members = %+v, want %+v", raw.Members, want)
	}
	if want := "type Raw = struct {Data []byte; n int}"; raw.Signature != want || raw.Kind != "type" {
		t.Errorf("Raw = %s %q, want type %q", raw.Kind, raw.Signature, want)
	}
}

func TestGoQualifiedNames(t *testing.T) {
	f := goTestParse(t, `package p

func init() {}
func Load() {}
func init() {}
func (c *Config) Load() {}
func (Other) Load() {}
func init() {}
func (c Config) Load(int) {}
func _() {}
var _ = Load
var _, Named = 1, 2
const _ = iota
type _ int
`)
	var got []string
	for _, s := range f.Symbols {
		got = append(got, s.Qualified)
	}
	want := []string{"init", "Load", "init#2", "Config.Load", "Other.Load", "init#3", "Config.Load#2", "Named"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("qualified names = %v, want %v", got, want)
	}
}

func TestGoExported(t *testing.T) {
	f := goTestParse(t, `package p

func Upper() {}
func lower() {}
func _under() {}
func Ünicode() {}
func ñame() {}
func (t *thing) Do() {}
func (t *Thing) do() {}
type thing struct{}
var Exported, unexported = 1, 2
`)
	want := map[string]bool{
		"Upper": true, "lower": false, "_under": false, "Ünicode": true, "ñame": false,
		"thing.Do": true, "Thing.do": false, "thing": false, "Exported": true, "unexported": false,
	}
	for q, exported := range want {
		if got := goTestSymbol(t, f, q).Exported; got != exported {
			t.Errorf("%s exported = %v, want %v", q, got, exported)
		}
	}
}

const goTestFormatted = `package p

import "io"

// Config is a config.
type Config struct {
	Name string ` + "`json:\"name\"`" + `
	Port int
	Base
	*io.SectionReader
	secret string
}

type Store interface {
	Get(key string) ([]byte, error)
	io.Closer
}

type (
	ID    string
	Alias = io.Reader
)

func (c *Config) Load(path string, strict bool) (cfg Config, err error) {
	if strict {
		return Config{}, nil
	}
	return *c, nil
}

func Map[T any, U any](xs []T, f func(T) U) []U { return nil }

func Printf(format string, args ...any) {}

const MaxSize int = 10

var Default *Config
`

// goTestUgly is goTestFormatted with odd spacing, line breaks and parentheses.
const goTestUgly = `package p
import "io"
type Config struct{
  Name    string   ` + "`json:\"name\"`" + `
      Port int
  Base
   * io.SectionReader
  secret string }
type Store interface{ Get( key string )( []byte,error )
	io.Closer }
type ( ID string
	Alias=io.Reader )
func (c * Config) Load( path string,strict bool )( cfg Config,err error ){
	if strict { return Config{ }, nil }
	return * c, nil
}
func Map[ T any,U any ]( xs [ ]T,f func( T )U )[ ]U{ return nil }
func Printf( format string, args ... any ){ }
const MaxSize int=10
var Default * Config
`

// goTestCommented is goTestFormatted with comments everywhere.
const goTestCommented = `// Package p is commented.
package p

import "io" // for Closer

// Config is a config.
// It has a long doc comment.
type Config struct {
	// Name is the name.
	Name string ` + "`json:\"name\"`" + ` // trailing
	Port int /* port */
	Base // embedded
	*io.SectionReader
	secret string // hidden
}

// Store stores.
type Store interface {
	// Get gets.
	Get(key /* k */ string) ([]byte /* data */, error)
	io.Closer // closes
}

type (
	ID    string        // an id
	Alias = io.Reader // an alias
)

// Load loads.
func (c *Config) Load(path string /* p */, strict bool) (cfg Config, err error) {
	// strict mode
	if strict {
		return Config{}, nil // empty
	}
	/* otherwise */ return *c, nil
}

func Map[T /* in */ any, U any](xs []T, f func(T) U) []U { return nil } // map

func Printf(format string, args ...any /* values */) {}

const MaxSize int = 10 // bytes

var Default *Config // the default
`

// goTestRenamed is goTestFormatted with parameter, result and receiver names
// changed, unnamed or blank, and grouped differently. Bodies change with them.
const goTestRenamed = `package p

import "io"

// Config is a config.
type Config struct {
	Name string ` + "`json:\"different\"`" + `
	Port int
	Base
	*io.SectionReader
	secret, other string
}

type Store interface {
	Get(string) (data []byte, e error)
	io.Closer
}

type (
	ID    string
	Alias = io.Reader
)

func (cfg *Config) Load(p string, s bool) (Config, error) {
	if s {
		return Config{}, nil
	}
	return *cfg, nil
}

func Map[T any, U any](in []T, fn func(item T) (out U)) []U { return nil }

func Printf(_ string, values ...any) {}

const MaxSize int = 10

var Default *Config
`

// goTestEdited is goTestFormatted with code edits that keep every signature.
const goTestEdited = `package p

import "io"

// Config is a config.
type Config struct {
	Name string ` + "`json:\"name\"`" + `
	Port int
	Base
	*io.SectionReader
	secret string
	cache  map[string]int
}

type Store interface {
	Get(key string) ([]byte, error)
	io.Closer
}

type (
	ID    string
	Alias = io.Reader
)

func (c *Config) Load(path string, strict bool) (cfg Config, err error) {
	if !strict {
		return Config{}, nil
	}
	return *c, nil
}

func Map[T any, U any](xs []T, f func(T) U) []U { return make([]U, 0) }

func Printf(format string, args ...any) { println(format) }

const MaxSize int = 20

var Default *Config = nil
`

func TestGoSignatureStability(t *testing.T) {
	base := goTestParse(t, goTestFormatted)
	if len(base.Symbols) != 9 {
		t.Fatalf("got %d symbols, want 9", len(base.Symbols))
	}
	tests := []struct {
		name     string
		src      string
		sameBody bool
	}{
		{"reformatted", goTestUgly, true},
		{"commented", goTestCommented, true},
		{"renamed", goTestRenamed, false},
		{"edited", goTestEdited, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := goTestParse(t, tt.src)
			if len(f.Symbols) != len(base.Symbols) {
				t.Fatalf("got %d symbols, want %d", len(f.Symbols), len(base.Symbols))
			}
			bodyChanged := 0
			for _, want := range base.Symbols {
				got := goTestSymbol(t, f, want.Qualified)
				if got.Signature != want.Signature || got.SigHash != want.SigHash {
					t.Errorf("%s signature changed\n got %q\nwant %q", want.Qualified, got.Signature, want.Signature)
				}
				if !reflect.DeepEqual(got.Params, want.Params) || got.Results != want.Results || !reflect.DeepEqual(got.Members, want.Members) {
					t.Errorf("%s structure changed: %+v %q %+v", want.Qualified, got.Params, got.Results, got.Members)
				}
				if got.BodyHash != want.BodyHash {
					bodyChanged++
					if tt.sameBody {
						t.Errorf("%s body hash changed", want.Qualified)
					}
				}
			}
			if !tt.sameBody && bodyChanged == 0 {
				t.Error("no body hash changed")
			}
		})
	}

	// Each code edit changes the body hash of the symbol it touches.
	edited := goTestParse(t, goTestEdited)
	for _, q := range []string{"Config", "Config.Load", "Map", "Printf", "MaxSize", "Default"} {
		if goTestSymbol(t, edited, q).BodyHash == goTestSymbol(t, base, q).BodyHash {
			t.Errorf("%s body hash unchanged by an edit", q)
		}
	}
	for _, q := range []string{"Store", "ID", "Alias"} {
		if goTestSymbol(t, edited, q).BodyHash != goTestSymbol(t, base, q).BodyHash {
			t.Errorf("%s body hash changed without an edit", q)
		}
	}
}

func TestGoSignatureChanges(t *testing.T) {
	tests := []struct {
		name, before, after, qualified string
		changed                        bool
	}{
		{"param type", "func F(a int) {}", "func F(a int64) {}", "F", true},
		{"param added", "func F(a int) {}", "func F(a int, b ...int) {}", "F", true},
		{"slice to variadic", "func F(a []int) {}", "func F(a ...int) {}", "F", true},
		{"result", "func F() int { return 0 }", "func F() (int, error) { return 0, nil }", "F", true},
		{"receiver pointer", "func (c Config) F() {}", "func (c *Config) F() {}", "Config.F", true},
		{"type param", "func F[T any]() {}", "func F[T comparable]() {}", "F", true},
		{"exported field added", "type S struct{ A int }", "type S struct {\n\tA int\n\tB int\n}", "S", true},
		{"unexported field added", "type S struct{ A int }", "type S struct {\n\tA int\n\tb int\n}", "S", false},
		{"field tag", "type S struct{ A int }", "type S struct {\n\tA int `json:\"a\"`\n}", "S", false},
		{"embedded unexported", "type S struct{ A int }", "type S struct {\n\tA int\n\tbase\n}", "S", true},
		{"interface method added", "type I interface{ A() }", "type I interface {\n\tA()\n\tb()\n}", "I", true},
		{"const type", "const C = 1", "const C int = 1", "C", true},
		{"const value", "const C = 1", "const C = 2", "C", false},
		{"var value", "var V = f()", "var V = g()", "V", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := goTestSymbol(t, goTestParse(t, "package p\n\n"+tt.before+"\n"), tt.qualified)
			b := goTestSymbol(t, goTestParse(t, "package p\n\n"+tt.after+"\n"), tt.qualified)
			if changed := a.SigHash != b.SigHash; changed != tt.changed {
				t.Errorf("sig hash changed = %v, want %v (%q -> %q)", changed, tt.changed, a.Signature, b.Signature)
			}
			if a.BodyHash == b.BodyHash {
				t.Errorf("body hash unchanged by %q -> %q", tt.before, tt.after)
			}
		})
	}
}

// goTestRef is the part of a model.Ref the Go extractor decides.
type goTestRef struct {
	Line      int
	Name      string
	Qualifier string
	Kind      model.RefKind
}

func TestGoRefs(t *testing.T) {
	f := goTestParse(t, `package p

import "example.com/pkg"

type Config struct {
	Base
	*pkg.Other
	Items List[Item]
	n     int
	Box[int]
}

type Store interface {
	pkg.Closer
	Get(key Key) (*Value, error)
}

func Map[T any, U Mapper](xs []T, f func(T) U) []U {
	var out []U
	return append(out, len(xs))
}

func (c *Config) Load(path string) error {
	data := read(path)
	v := pkg.Parse(data)
	c.store.Put(v)
	w := &Config{}
	ws := []Item{{}, {}}
	o := pkg.Options{Debug: true}
	n := new(Counter)
	r := Convert[Item](v)
	s := pkg.Convert[Item, Value](v)
	t, ok := v.(Value)
	_ = make([]Value, 0)
	m := map[Key]Value{}
	b := strings.NewReplacer("a", "b").Replace(path)
	fns[0](v)
	(read)(path)
	func() {}()
	x := string(data) + Name(data)
	return errors.New("x")
}

func (l *List[E]) Each(fn func(E) bool) {
	var _ E
	var ptr *pkg.Node[E]
}

var registry = map[string]Factory{"a": NewA}
`)
	want := []goTestRef{
		{6, "Base", "", model.RefExtends},
		{7, "Other", "pkg", model.RefExtends},
		{8, "Item", "", model.RefType},
		{8, "List", "", model.RefType},
		{10, "Box", "", model.RefExtends},
		{14, "Closer", "pkg", model.RefExtends},
		{15, "Key", "", model.RefType},
		{15, "Value", "", model.RefType},
		{18, "Mapper", "", model.RefType},
		{23, "Config", "", model.RefType},
		{24, "read", "", model.RefCall},
		{25, "Parse", "pkg", model.RefCall},
		{26, "Put", "c.store", model.RefCall},
		{27, "Config", "", model.RefConstruct},
		{28, "Item", "", model.RefConstruct},
		{29, "Options", "pkg", model.RefConstruct},
		{30, "Counter", "", model.RefConstruct},
		{31, "Convert", "", model.RefCall},
		{31, "Item", "", model.RefType},
		{32, "Convert", "pkg", model.RefCall},
		{32, "Item", "", model.RefType},
		{32, "Value", "", model.RefType},
		{33, "Value", "", model.RefType},
		{34, "Value", "", model.RefType},
		{35, "Key", "", model.RefType},
		{35, "Value", "", model.RefType},
		{36, "NewReplacer", "strings", model.RefCall},
		{36, "Replace", "strings.NewReplacer()", model.RefCall},
		{40, "Name", "", model.RefCall},
		{41, "New", "errors", model.RefCall},
		{44, "List", "", model.RefType},
		{46, "Node", "pkg", model.RefType},
		{49, "Factory", "", model.RefType},
	}
	var got []goTestRef
	for _, r := range f.Refs {
		if r.Lang != "go" || r.Path != goTestPath || r.Module != "" || r.Resolved {
			t.Errorf("ref %+v: want lang go, path %s, unresolved", r, goTestPath)
		}
		got = append(got, goTestRef{r.Line, r.Name, r.Qualifier, r.Kind})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refs:\n%s", goTestDiff(got, want))
	}
}

// goTestDiff lists the refs only in got (+) or only in want (-).
func goTestDiff(got, want []goTestRef) string {
	count := map[goTestRef]int{}
	for _, r := range got {
		count[r]++
	}
	for _, r := range want {
		count[r]--
	}
	var lines []string
	for r, n := range count {
		switch {
		case n > 0:
			lines = append(lines, fmt.Sprintf("+ %+v", r))
		case n < 0:
			lines = append(lines, fmt.Sprintf("- %+v", r))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "same refs in a different order"
	}
	return strings.Join(lines, "\n")
}

func TestGoImports(t *testing.T) {
	f := goTestParse(t, `package p

import "fmt"
import (
	yml "gopkg.in/yaml.v3"
	"gopkg.in/check.v1"
	. "strings"
	_ "embed"
	"example.com/mod/v2"
	"example.com/mod/v2/sub"
	"k8s.io/api/core/v1"
	"github.com/mattn/go-isatty"
	`+"`raw/path`"+`
	"C"
)
`)
	want := []Import{
		{Local: "fmt", Module: "fmt", Kind: ImportNamespace, Line: 3},
		{Local: "yml", Module: "gopkg.in/yaml.v3", Kind: ImportNamespace, Line: 5},
		{Local: "check", Module: "gopkg.in/check.v1", Kind: ImportNamespace, Line: 6},
		{Local: "", Module: "strings", Kind: ImportGlob, Line: 7},
		{Local: "", Module: "embed", Kind: ImportNamespace, Line: 8},
		{Local: "mod", Module: "example.com/mod/v2", Kind: ImportNamespace, Line: 9},
		{Local: "sub", Module: "example.com/mod/v2/sub", Kind: ImportNamespace, Line: 10},
		{Local: "v1", Module: "k8s.io/api/core/v1", Kind: ImportNamespace, Line: 11},
		{Local: "go-isatty", Module: "github.com/mattn/go-isatty", Kind: ImportNamespace, Line: 12},
		{Local: "path", Module: "raw/path", Kind: ImportNamespace, Line: 13},
		{Local: "C", Module: "C", Kind: ImportNamespace, Line: 14},
	}
	if !reflect.DeepEqual(f.Imports, want) {
		t.Errorf("imports\n got %+v\nwant %+v", f.Imports, want)
	}
	if len(f.Symbols) != 0 || len(f.Refs) != 0 {
		t.Errorf("imports produced symbols %v or refs %v", f.Symbols, f.Refs)
	}
}

func TestGoDefaultName(t *testing.T) {
	tests := map[string]string{
		"fmt":                         "fmt",
		"net/http":                    "http",
		"github.com/crewlet/gitlet":   "gitlet",
		"example.com/mod/v2":          "mod",
		"example.com/mod/v10":         "mod",
		"example.com/mod/v2/sub":      "sub",
		"example.com/mod/v1":          "v1",
		"example.com/mod/v0":          "v0",
		"example.com/mod/v2beta":      "v2beta",
		"example.com/mod/v02":         "v02",
		"v2":                          "v2",
		"gopkg.in/yaml.v3":            "yaml",
		"gopkg.in/user/pkg.v2":        "pkg",
		"gopkg.in/yaml.v":             "yaml.v",
		"github.com/mattn/go-isatty":  "go-isatty",
		"github.com/x/y/":             "y",
		"github.com/hashicorp/go.mod": "go.mod",
	}
	for path, want := range tests {
		if got := goDefaultName(path); got != want {
			t.Errorf("goDefaultName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestGoMalformed(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string // qualified names that must be found
	}{
		{"empty", "", nil},
		{"package only", "package p", nil},
		{"garbage", "}}}{{{ ))) func func type", nil},
		{"binary", "\x00\xff\xfe package \x01 func", nil},
		{"truncated", "package p\n\nfunc Good() {}\n\nfunc Broken(", []string{"Good"}},
		{"stray tokens", "package p\n\nfunc Good() int { return 1 }\n\n)))) garbage (((\n\ntype T struct{ A int }\n\nfunc Last() {}\n", []string{"Good", "T", "Last"}},
		{"broken body", "package p\n\nfunc Bad() {\n\tx := := 3\n}\n\nvar V = 1\n\nfunc Good(s string) error { return nil }\n", []string{"Bad", "V", "Good"}},
		{"broken interface", "package p\n\ntype I interface {\n\tM(\n}\n\nfunc Good() {}\n", []string{"I", "Good"}},
		{"missing names", "package p\n\nfunc () {}\nfunc (r *R) () {}\ntype = int\nconst = 1\nfunc Good() {}\n", []string{"Good"}},
		{"unterminated string", "package p\n\nimport \"fmt\n\nfunc Good() {}\n", nil},
		{"deep nesting", "package p\n\nfunc Deep() { x := " + strings.Repeat("(", 2000) + "1" + strings.Repeat(")", 2000) + " }\n\nfunc Good() {}\n", []string{"Deep", "Good"}},
		{"nested types", "package p\n\nvar V " + strings.Repeat("[]", 500) + "map[string]" + strings.Repeat("*", 500) + "struct{ A func(func() chan<- int) }\n", []string{"V"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Extract(goTestPath, []byte(tt.src))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			for _, q := range tt.want {
				goTestSymbol(t, f, q)
			}
			for _, s := range f.Symbols {
				if s.Name == "" || s.StartByte > s.EndByte || s.EndByte > len(tt.src) {
					t.Errorf("bad symbol %+v", s)
				}
			}
		})
	}
}

func TestGoConcurrent(t *testing.T) {
	src, want := goTestFixture(t)
	other := []byte(goTestFormatted)
	wantOther := goTestParse(t, goTestFormatted)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				in, exp := src, want
				if (i+j)%2 == 1 {
					in, exp = other, wantOther
				}
				got, err := Extract(goTestPath, in)
				if err != nil {
					errs <- err.Error()
					return
				}
				if !reflect.DeepEqual(got, exp) {
					errs <- "concurrent Extract returned a different result"
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
