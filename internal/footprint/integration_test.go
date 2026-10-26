package footprint

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/symbols"
)

// These tests run the real extractors (symbols.Extract) through Compute over
// two in-memory revisions, so they check footprint against the conventions
// the language extractors actually follow.

// change is one changed file for computeReal: Status "A" has no base, "D" no
// head, "R" reads its base from oldPath.
type change struct {
	status, path, oldPath string
	base, head            string
}

// computeReal runs Compute with the real extractors and expects every
// changed file in a supported language to be parsed.
func computeReal(t *testing.T, goModule string, changes ...change) Result {
	t.Helper()
	res := computeExtracted(t, goModule, changes...)
	for _, f := range res.Footprint.Files {
		if symbols.Supported(f.Path) && !f.Parsed {
			t.Errorf("%s was not parsed", f.Path)
		}
	}
	return res
}

// computeExtracted runs Compute with the real extractors.
func computeExtracted(t *testing.T, goModule string, changes ...change) Result {
	t.Helper()
	r := newRepo()
	for _, c := range changes {
		r.src.entries = append(r.src.entries, gitx.DiffEntry{Status: c.status, Path: c.path, OldPath: c.oldPath})
		basePath := c.path
		if c.oldPath != "" {
			basePath = c.oldPath
		}
		if c.status != "A" {
			r.put(baseRev, basePath, c.base, nil)
		}
		if c.status != "D" {
			r.put(headRev, c.path, c.head, nil)
		}
	}
	res, err := Compute(context.Background(), r.src, baseRev, headRev,
		Options{Extract: symbols.Extract, GoModule: goModule, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func checkLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

const goBase = `package engine

import (
	"fmt"

	"example.com/m/internal/model"
)

// Load reads a config.
func Load(path string) (model.Config, error) {
	return model.Parse(path)
}

func Strict(path string) error {
	return nil
}

func Body() int {
	return 1
}

func oldName(a int) int {
	return a * 2
}

func Same() {
	fmt.Println("same")
}

type Store interface {
	Get(string) error
	Put(string) error
}

type Config struct {
	Name string
}
`

const goHead = `package engine

import (
	"fmt"

	"example.com/m/internal/model"
	"example.com/m/internal/store"
)

// Load reads a config.
func Load(path string, opts ...model.Option) (model.Config, error) {
	return model.Parse(path)
}

func Strict(path string, strict bool) error {
	return store.Open(path)
}

func Body() int {
	return helper(2).Value()
}

func newName(a int) int {
	return a * 2
}

func Same() {
	fmt.Println("same")
}

type Store interface {
	Put(string) error
	Get(string) error
}

type Config struct {
	Name string
	Port int
}
`

func TestIntegrationGo(t *testing.T) {
	changes := []change{
		{status: "M", path: "internal/engine/run.go", base: goBase, head: goHead},
		{status: "R", path: "internal/util/util.go", oldPath: "internal/engine/util.go",
			base: "package engine\n\nfunc Helper() {}\n", head: "package util\n\nfunc Helper() {}\n"},
		{status: "R", path: "internal/engine/b.go", oldPath: "internal/engine/a.go",
			base: "package engine\n\nfunc Kept() int { return 1 }\n", head: "package engine\n\nfunc Kept() int { return 2 }\n"},
	}
	wantSymbols := []string{
		"internal/engine/b.go:Kept moved from internal/engine/a.go sig=false body=true",
		"internal/engine/run.go:Body body",
		"internal/engine/run.go:Config signature",
		"internal/engine/run.go:Load signature",
		"internal/engine/run.go:Store signature",
		"internal/engine/run.go:Strict signature!",
		"internal/engine/run.go:newName renamed! from oldName",
		"internal/util/util.go:Helper moved! from internal/engine/util.go sig=false body=false",
	}

	t.Run("with go.mod", func(t *testing.T) {
		res := computeReal(t, "example.com/m", changes...)
		checkLines(t, "symbols", summarize(res.Footprint.Symbols), wantSymbols)
		// fmt.Println sits in an unchanged function; fmt would be dropped anyway.
		checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
			"internal/engine/run.go:11 model.Config internal/model",
			"internal/engine/run.go:11 model.Option internal/model",
			"internal/engine/run.go:12 model.Parse internal/model",
			"internal/engine/run.go:16 store.Open internal/store",
			"internal/engine/run.go:20 helper().Value ?",
			"internal/engine/run.go:20 helper internal/engine",
		})
	})
	t.Run("without go.mod", func(t *testing.T) {
		res := computeReal(t, "", changes...)
		checkLines(t, "symbols", summarize(res.Footprint.Symbols), wantSymbols)
		checkLines(t, "refs", summarizeRefs(res.Footprint.Refs), []string{
			"internal/engine/run.go:11 model.Config example.com/m/internal/model",
			"internal/engine/run.go:11 model.Option example.com/m/internal/model",
			"internal/engine/run.go:12 model.Parse example.com/m/internal/model",
			"internal/engine/run.go:16 store.Open example.com/m/internal/store",
			"internal/engine/run.go:20 helper().Value ?",
			"internal/engine/run.go:20 helper internal/engine",
		})
	})
	t.Run("go.mod read from head", func(t *testing.T) {
		r := newRepo(gitx.DiffEntry{Status: "M", Path: "internal/engine/run.go"})
		r.put(baseRev, "internal/engine/run.go", goBase, nil)
		r.put(headRev, "internal/engine/run.go", goHead, nil)
		r.put(headRev, "go.mod", "module example.com/m\n\ngo 1.26\n", nil)
		res, err := Compute(context.Background(), r.src, baseRev, headRev, Options{Now: testNow})
		if err != nil {
			t.Fatal(err)
		}
		if got := summarizeRefs(res.Footprint.Refs); len(got) != 6 || got[3] != "internal/engine/run.go:16 store.Open internal/store" {
			t.Errorf("refs = %q", got)
		}
	})
}

// TestIntegrationSyntaxErrors checks that a half-written edit, which
// tree-sitter recovers from by swallowing or dropping the definitions after
// it, never reads as removed symbols: the file is tracked at file level until
// it parses again.
func TestIntegrationSyntaxErrors(t *testing.T) {
	const goLib = "package lib\n\nfunc A(x int) int {\n\tif x > 0 {\n\t\treturn x\n\t}\n\treturn 0\n}\n\n" +
		"func B(s string) string {\n\treturn s\n}\n\ntype C struct{}\n\nfunc (C) M() {}\n"
	const tsLib = "export function a(x: number): number {\n  if (x > 0) {\n    return x;\n  }\n  return 0;\n}\n\n" +
		"export function b(s: string): string {\n  return s;\n}\n\nexport class C {\n  m(): void {}\n}\n"
	const pyLib = "def a(x):\n    return f(x)\n\n\ndef b(s):\n    return s\n\n\nclass C:\n    def m(self):\n        pass\n"
	const rsLib = "pub fn a(x: u8) -> u8 {\n    if x > 0 {\n        return x;\n    }\n    0\n}\n\n" +
		"pub fn b(s: &str) -> &str {\n    s\n}\n\npub struct C;\n\nimpl C {\n    pub fn m(&self) {}\n}\n"
	goBroken := strings.Replace(goLib, "\t\treturn x\n\t}\n", "\t\treturn x\n", 1)
	pyBroken := strings.Replace(pyLib, "return s", "return (s", 1)
	tests := []struct {
		name    string
		c       change
		parsed  bool
		symbols []string
	}{
		{"go unclosed block", change{status: "M", path: "lib/lib.go", base: goLib, head: goBroken}, false, nil},
		{"typescript unclosed block", change{status: "M", path: "src/lib.ts", base: tsLib,
			head: strings.Replace(tsLib, "    return x;\n  }\n", "    return x;\n", 1)}, false, nil},
		{"python unclosed paren", change{status: "M", path: "pkg/lib.py", base: pyLib,
			head: strings.Replace(pyLib, "return f(x)", "return f(x", 1)}, false, nil},
		{"rust unclosed block", change{status: "M", path: "src/lib.rs", base: rsLib,
			head: strings.Replace(rsLib, "        return x;\n    }\n", "        return x;\n", 1)}, false, nil},
		{"renamed file", change{status: "R", path: "lib/lib2.go", oldPath: "lib/lib.go", base: goLib, head: goBroken}, false, nil},
		{"base already broken", change{status: "M", path: "pkg/lib.py", base: pyBroken,
			head: pyBroken + "\n\ndef d():\n    pass\n"}, true, []string{"pkg/lib.py:d added"}},
		{"added file", change{status: "A", path: "lib/new.go",
			head: "package lib\n\nfunc N() int {\n\treturn 1\n}\n\nfunc broken( {\n"}, true, []string{"lib/new.go:N added"}},
		{"parses again", change{status: "M", path: "lib/lib.go", base: goLib,
			head: strings.Replace(goLib, "return s\n", "return s + s\n", 1)}, true, []string{"lib/lib.go:B body"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := computeExtracted(t, "example.com/m", tt.c).Footprint
			if len(fp.Files) != 1 || fp.Files[0].Parsed != tt.parsed {
				t.Fatalf("files = %+v, want parsed %v", fp.Files, tt.parsed)
			}
			checkLines(t, "symbols", summarize(fp.Symbols), tt.symbols)
			if !tt.parsed && len(fp.Refs) > 0 {
				t.Errorf("refs from a file tracked at file level: %q", summarizeRefs(fp.Refs))
			}
		})
	}
}
