package footprint

import (
	"context"
	"slices"
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

func computeReal(t *testing.T, goModule string, changes ...change) Result {
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
	for _, f := range res.Footprint.Files {
		if symbols.Supported(f.Path) && !f.Parsed {
			t.Errorf("%s was not parsed", f.Path)
		}
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
