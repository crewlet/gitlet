package footprint

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

var testNow = time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)

const (
	baseRev = "base"
	headRev = "head"
)

func TestComputeFiles(t *testing.T) {
	r := newRepo(
		gitx.DiffEntry{Status: "M", Path: "pkg/a.go"},
		gitx.DiffEntry{Status: "A", Path: "pkg/new.go"},
		gitx.DiffEntry{Status: "D", Path: "pkg/gone.go"},
		gitx.DiffEntry{Status: "R", Path: "util/x.go", OldPath: "old/x.go"},
		gitx.DiffEntry{Status: "C", Path: "pkg/copy.go", OldPath: "pkg/a.go"},
		gitx.DiffEntry{Status: "M", Path: "README.md"},
		gitx.DiffEntry{Status: "M", Path: "big.go"},
		gitx.DiffEntry{Status: "M", Path: "bad.go"},
		gitx.DiffEntry{Status: "T", Path: "pkg/link.go"},
	)
	for _, p := range []string{"pkg/a.go", "pkg/gone.go", "old/x.go", "README.md", "big.go", "bad.go", "pkg/link.go"} {
		r.put(baseRev, p, "package x // base "+p+"\n", nil)
	}
	for _, p := range []string{"pkg/a.go", "pkg/new.go", "util/x.go", "pkg/copy.go", "README.md", "bad.go", "pkg/link.go"} {
		r.put(headRev, p, "package x // head "+p+"\n", nil)
	}
	r.put(headRev, "big.go", "package x\n"+strings.Repeat("// padding\n", 20), nil)
	r.ext.errs["bad.go"] = errors.New("extractor panic")

	opt := r.opts()
	opt.MaxFileBytes = 100
	res, err := Compute(context.Background(), r.src, baseRev, headRev, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.FileChange{
		{Path: "README.md", Status: "M"},
		{Path: "bad.go", Status: "M", Lang: "go"},
		{Path: "big.go", Status: "M", Lang: "go"},
		{Path: "pkg/a.go", Status: "M", Lang: "go", Parsed: true},
		{Path: "pkg/copy.go", OldPath: "pkg/a.go", Status: "C", Lang: "go", Parsed: true},
		{Path: "pkg/gone.go", Status: "D", Lang: "go", Parsed: true},
		{Path: "pkg/link.go", Status: "T", Lang: "go", Parsed: true},
		{Path: "pkg/new.go", Status: "A", Lang: "go", Parsed: true},
		{Path: "util/x.go", OldPath: "old/x.go", Status: "R", Lang: "go", Parsed: true},
	}
	if !reflect.DeepEqual(res.Footprint.Files, want) {
		t.Fatalf("files:\n got %+v\nwant %+v", res.Footprint.Files, want)
	}
	if r.ext.called("README.md") || r.ext.called("big.go") {
		t.Errorf("unsupported or oversized file was parsed: %v", r.ext.calls)
	}
	if r.src.read(baseRev+":pkg/new.go") || r.src.read(baseRev+":pkg/copy.go") || r.src.read(headRev+":pkg/gone.go") {
		t.Errorf("read a side that does not exist: %v", r.src.reads)
	}
	if !r.src.read(baseRev+":old/x.go") || !r.ext.called("old/x.go") {
		t.Errorf("renamed file's old side not read from its old path: %v", r.src.reads)
	}
	fp := res.Footprint
	if fp.Base != baseRev || fp.Head != headRev || !fp.ComputedAt.Equal(testNow) || res.Truncated || fp.Broad {
		t.Errorf("footprint header = %q %q %v truncated=%v broad=%v", fp.Base, fp.Head, fp.ComputedAt, res.Truncated, fp.Broad)
	}
}

func TestComputeSymbols(t *testing.T) {
	r := newRepo(
		gitx.DiffEntry{Status: "M", Path: "pkg/a.go"},
		gitx.DiffEntry{Status: "A", Path: "pkg/new.go"},
		gitx.DiffEntry{Status: "D", Path: "pkg/gone.go"},
		gitx.DiffEntry{Status: "R", Path: "util/x.go", OldPath: "pkg/x.go"},
		gitx.DiffEntry{Status: "R", Path: "pkg/y.go", OldPath: "pkg/y_old.go"},
		gitx.DiffEntry{Status: "M", Path: "pkg/vanished.go"},
	)
	intParam := []model.Param{{Type: "int"}}
	r.putFile(baseRev, "pkg/a.go", "a base", []def{
		{q: "Load", kind: "function", sig: "func Load()", body: "load", start: 1, end: 1},
		{q: "Parse", kind: "function", sig: "func Parse(int)", body: "parse", start: 2, end: 2, params: intParam},
		{q: "Strict", kind: "function", sig: "func Strict(int)", body: "strict", start: 3, end: 3, params: intParam},
		{q: "Old", kind: "function", sig: "func Old()", body: "old", start: 4, end: 4},
		{q: "helper", kind: "function", sig: "func helper()", body: "shared body", start: 5, end: 5},
		{q: "Same", kind: "function", sig: "func Same()", body: "same", start: 6, end: 6},
	}, nil)
	r.putFile(headRev, "pkg/a.go", "a head", []def{
		{q: "Load", kind: "function", sig: "func Load()", body: "load v2", start: 1, end: 1},
		{q: "Parse", kind: "function", sig: "func Parse(int, ...Option)", body: "parse", start: 2, end: 2,
			params: []model.Param{{Type: "int"}, {Type: "Option", Variadic: true}}},
		{q: "Strict", kind: "function", sig: "func Strict(int, bool)", body: "strict", start: 3, end: 3,
			params: []model.Param{{Type: "int"}, {Type: "bool"}}},
		{q: "Fresh", kind: "function", sig: "func Fresh()", body: "fresh", start: 4, end: 4},
		{q: "assist", kind: "function", sig: "func assist()", body: "shared body", start: 5, end: 5},
		{q: "Same", kind: "function", sig: "func Same()", body: "same", start: 6, end: 6},
	}, nil)
	r.putFile(headRev, "pkg/new.go", "new", []def{{q: "N", kind: "function", sig: "func N()", body: "n", start: 1, end: 1}}, nil)
	r.putFile(baseRev, "pkg/gone.go", "gone", []def{{q: "G", kind: "function", sig: "func G()", body: "g", start: 1, end: 1}}, nil)
	r.putFile(baseRev, "pkg/x.go", "x base", []def{
		{q: "X", kind: "function", sig: "func X()", body: "x", start: 1, end: 1},
		{q: "Gone", kind: "function", sig: "func Gone()", body: "gone", start: 2, end: 2},
	}, nil)
	r.putFile(headRev, "util/x.go", "x head", []def{
		{q: "X", kind: "function", sig: "func X()", body: "x v2", start: 1, end: 1},
		{q: "Born", kind: "function", sig: "func Born()", body: "born", start: 2, end: 2},
	}, nil)
	r.putFile(baseRev, "pkg/y_old.go", "y", []def{{q: "Y", kind: "function", sig: "func Y()", body: "y", start: 1, end: 1}}, nil)
	r.putFile(headRev, "pkg/y.go", "y", []def{{q: "Y", kind: "function", sig: "func Y()", body: "y", start: 1, end: 1}}, nil)
	// A modified file whose head side is missing counts as deleted.
	r.putFile(baseRev, "pkg/vanished.go", "v", []def{{q: "V", kind: "function", sig: "func V()", body: "v", start: 1, end: 1}}, nil)

	res, err := Compute(context.Background(), r.src, baseRev, headRev, r.opts())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pkg/a.go:Fresh added",
		"pkg/a.go:Load body",
		"pkg/a.go:Old removed!",
		"pkg/a.go:Parse signature",
		"pkg/a.go:Strict signature!",
		"pkg/a.go:assist renamed! from helper",
		"pkg/gone.go:G removed!",
		"pkg/new.go:N added",
		"pkg/vanished.go:V removed!",
		"pkg/x.go:Gone removed!",
		"pkg/y.go:Y moved from pkg/y_old.go sig=false body=false",
		"util/x.go:Born added",
		"util/x.go:X moved! from pkg/x.go sig=false body=true",
	}
	if got := summarize(res.Footprint.Symbols); !slices.Equal(got, want) {
		t.Fatalf("symbols:\n got %q\nwant %q", got, want)
	}
}

func TestComputeRefs(t *testing.T) {
	const aBase = `package pkg

import "example.com/m/util"

var registry = util.NewRegistry()

func Load() error {
	return util.Read()
}

func Keep() {
	util.Keep()
}
`
	const aHead = `package pkg

import "example.com/m/util"

var registry = util.NewRegistry()
var extra = util.Extra()

func Load() error {
	return util.ReadAll(local())
}

func Keep() {
	util.Keep()
}
`
	const svcBase = `class Service:
    retries = 3

    def run(self):
        helper()
`
	const svcHead = `class Service:
    retries = compute()

    def run(self):
        helper()

    def log(self):
        self.write()
`
	const fresh = `import { load as loadConfig } from "./config"
import * as fs from "node:fs"
loadConfig()
export function main() { fs.readFileSync(); this.x() }
`
	r := newRepo(
		gitx.DiffEntry{Status: "M", Path: "pkg/a.go"},
		gitx.DiffEntry{Status: "M", Path: "svc/app.py"},
		gitx.DiffEntry{Status: "A", Path: "web/main.ts"},
	)
	utilImport := symbols.Import{Local: "util", Module: "example.com/m/util", Kind: symbols.ImportNamespace, Line: 3}
	r.putFile(baseRev, "pkg/a.go", aBase, []def{
		{q: "registry", kind: "var", sig: "var registry", body: "registry", start: 5, end: 5},
		{q: "Load", kind: "function", sig: "func Load() error", body: "load", start: 7, end: 9},
		{q: "Keep", kind: "function", sig: "func Keep()", body: "keep", start: 11, end: 13},
	}, []model.Ref{call("NewRegistry", "util", 5), call("Read", "util", 8), call("Keep", "util", 12)}, utilImport)
	r.putFile(headRev, "pkg/a.go", aHead, []def{
		{q: "registry", kind: "var", sig: "var registry", body: "registry", start: 5, end: 5},
		{q: "Load", kind: "function", sig: "func Load() error", body: "load v2", start: 8, end: 10},
		{q: "Keep", kind: "function", sig: "func Keep()", body: "keep", start: 12, end: 14},
	}, []model.Ref{
		call("NewRegistry", "util", 5), call("Extra", "util", 6),
		call("ReadAll", "util", 9), call("local", "", 9), call("Keep", "util", 13),
	}, utilImport)

	r.putFile(baseRev, "svc/app.py", svcBase, []def{
		{q: "Service", kind: "class", sig: "class Service", body: "class 3", start: 1, end: 5},
		{q: "Service.run", kind: "method", sig: "def run(self)", body: "run", start: 4, end: 5},
	}, []model.Ref{call("helper", "", 5)})
	r.putFile(headRev, "svc/app.py", svcHead, []def{
		{q: "Service", kind: "class", sig: "class Service", body: "class compute", start: 1, end: 8},
		{q: "Service.run", kind: "method", sig: "def run(self)", body: "run", start: 4, end: 5},
		{q: "Service.log", kind: "method", sig: "def log(self)", body: "log", start: 7, end: 8},
	}, []model.Ref{call("compute", "", 2), call("helper", "", 5), call("write", "self", 8)})

	r.putFile(headRev, "web/main.ts", fresh, []def{
		{q: "main", kind: "function", sig: "function main()", body: "main", start: 4, end: 4, exported: true},
	}, []model.Ref{call("loadConfig", "", 3), call("readFileSync", "fs", 4), call("x", "this", 4)},
		symbols.Import{Local: "loadConfig", Name: "load", Module: "./config", Kind: symbols.ImportNamed, Line: 1},
		symbols.Import{Local: "fs", Module: "node:fs", Kind: symbols.ImportNamespace, Line: 2},
	)
	r.put(headRev, "go.mod", "module example.com/m // the module\n\ngo 1.26\n", nil)

	opt := r.opts()
	opt.GoModule = ""
	res, err := Compute(context.Background(), r.src, baseRev, headRev, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pkg/a.go:6 util.Extra util",
		"pkg/a.go:9 util.ReadAll util",
		"pkg/a.go:9 local pkg",
		"svc/app.py:2 compute svc/app",
		"svc/app.py:8 self.write ?",
		"web/main.ts:3 load web/config",
		"web/main.ts:4 this.x ?",
	}
	if got := summarizeRefs(res.Footprint.Refs); !slices.Equal(got, want) {
		t.Fatalf("refs:\n got %q\nwant %q", got, want)
	}
	if !r.src.read(headRev + ":go.mod") {
		t.Error("go.mod was not read")
	}
}

func TestComputeBroadAndTruncated(t *testing.T) {
	entries := []gitx.DiffEntry{
		{Status: "M", Path: "b.go"},
		{Status: "M", Path: "a.go"},
		{Status: "M", Path: "notes.txt"},
	}
	setup := func() *repo {
		r := newRepo(entries...)
		for _, p := range []string{"a.go", "b.go"} {
			r.putFile(baseRev, p, "base "+p, []def{{q: "F", kind: "function", sig: "func F()", body: "1", start: 1, end: 1}}, nil)
			r.putFile(headRev, p, "head "+p, []def{{q: "F", kind: "function", sig: "func F()", body: "2", start: 1, end: 1}}, nil)
		}
		r.put(baseRev, "notes.txt", "a", nil)
		r.put(headRev, "notes.txt", "b", nil)
		return r
	}
	tests := []struct {
		name          string
		tune          func(*Options)
		wantBroad     bool
		wantTruncated bool
		wantParsed    []string
	}{
		{"defaults", func(*Options) {}, false, false, []string{"a.go", "b.go"}},
		{"too many files", func(o *Options) { o.BroadFiles = 2 }, true, false, []string{"a.go", "b.go"}},
		{"files at the bound", func(o *Options) { o.BroadFiles = 3 }, false, false, []string{"a.go", "b.go"}},
		{"too many symbols", func(o *Options) { o.BroadSymbols = 1 }, true, false, []string{"a.go", "b.go"}},
		{"symbols at the bound", func(o *Options) { o.BroadSymbols = 2 }, false, false, []string{"a.go", "b.go"}},
		{"capped", func(o *Options) { o.MaxParsed = 1 }, false, true, []string{"a.go"}},
		{"cap not reached", func(o *Options) { o.MaxParsed = 2 }, false, false, []string{"a.go", "b.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setup()
			opt := r.opts()
			tt.tune(&opt)
			res, err := Compute(context.Background(), r.src, baseRev, headRev, opt)
			if err != nil {
				t.Fatal(err)
			}
			if res.Footprint.Broad != tt.wantBroad || res.Truncated != tt.wantTruncated {
				t.Errorf("broad=%v truncated=%v, want %v %v", res.Footprint.Broad, res.Truncated, tt.wantBroad, tt.wantTruncated)
			}
			var parsed []string
			for _, f := range res.Footprint.Files {
				if f.Parsed {
					parsed = append(parsed, f.Path)
				}
			}
			if !slices.Equal(parsed, tt.wantParsed) {
				t.Errorf("parsed = %v, want %v", parsed, tt.wantParsed)
			}
			if len(res.Footprint.Symbols) != len(tt.wantParsed) {
				t.Errorf("symbols = %v", summarize(res.Footprint.Symbols))
			}
		})
	}
}

func TestComputeGoModule(t *testing.T) {
	const src = "package pkg\n\nfunc F() { util.Do(); fmt.Println() }\n"
	imports := []symbols.Import{
		{Local: "util", Module: "example.com/m/util", Kind: symbols.ImportNamespace, Line: 1},
		{Local: "fmt", Module: "fmt", Kind: symbols.ImportNamespace, Line: 1},
	}
	tests := []struct {
		name     string
		option   string
		gomod    string // "" for none
		readErr  error
		wantRefs []string
		wantRead bool
		wantErr  bool
	}{
		{"from options", "example.com/m", "module other.org/x\n", nil, []string{"pkg/f.go:3 util.Do util"}, false, false},
		{"from go.mod", "", "module \"example.com/m\"\n", nil, []string{"pkg/f.go:3 util.Do util"}, true, false},
		{"no go.mod keeps hosted paths", "", "", nil, []string{"pkg/f.go:3 util.Do example.com/m/util"}, true, false},
		{"go.mod outside the module", "", "module example.com/other\n", nil, nil, true, false},
		{"go.mod unreadable", "", "", errors.New("disk on fire"), nil, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(gitx.DiffEntry{Status: "A", Path: "pkg/f.go"})
			r.putFile(headRev, "pkg/f.go", src, []def{{q: "F", kind: "function", sig: "func F()", body: "f", start: 3, end: 3}},
				[]model.Ref{call("Do", "util", 3), call("Println", "fmt", 3)}, imports...)
			if tt.gomod != "" {
				r.put(headRev, "go.mod", tt.gomod, nil)
			}
			if tt.readErr != nil {
				r.src.errs[headRev+":go.mod"] = tt.readErr
			}
			opt := r.opts()
			opt.GoModule = tt.option
			res, err := Compute(context.Background(), r.src, baseRev, headRev, opt)
			if tt.wantErr {
				if err == nil || !errors.Is(err, tt.readErr) || !strings.HasPrefix(err.Error(), "footprint: ") {
					t.Fatalf("err = %v, want a wrapped %v", err, tt.readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := summarizeRefs(res.Footprint.Refs); !slices.Equal(got, tt.wantRefs) {
				t.Errorf("refs = %q, want %q", got, tt.wantRefs)
			}
			if got := r.src.read(headRev + ":go.mod"); got != tt.wantRead {
				t.Errorf("read go.mod = %v, want %v", got, tt.wantRead)
			}
		})
	}
}

func TestComputeSkipsGoModWithoutGoFiles(t *testing.T) {
	r := newRepo(gitx.DiffEntry{Status: "A", Path: "a.py"})
	r.putFile(headRev, "a.py", "f()\n", nil, []model.Ref{call("f", "", 1)})
	opt := r.opts()
	opt.GoModule = ""
	res, err := Compute(context.Background(), r.src, baseRev, headRev, opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.src.read(headRev + ":go.mod") {
		t.Error("go.mod read for a footprint without Go files")
	}
	if got := summarizeRefs(res.Footprint.Refs); !slices.Equal(got, []string{"a.py:1 f a"}) {
		t.Errorf("refs = %q", got)
	}
}

func TestComputeErrors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("nil source", func(t *testing.T) {
		if _, err := Compute(context.Background(), nil, baseRev, headRev, Options{}); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("diff", func(t *testing.T) {
		r := newRepo()
		r.src.diffErr = boom
		_, err := Compute(context.Background(), r.src, baseRev, headRev, r.opts())
		if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "footprint: diff base..head") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("read", func(t *testing.T) {
		r := newRepo(gitx.DiffEntry{Status: "M", Path: "a.go"})
		r.put(baseRev, "a.go", "x", nil)
		r.src.errs[headRev+":a.go"] = boom
		_, err := Compute(context.Background(), r.src, baseRev, headRev, r.opts())
		if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "footprint: read a.go at head") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("canceled before", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := newRepo(gitx.DiffEntry{Status: "A", Path: "a.go"})
		_, err := Compute(ctx, r.src, baseRev, headRev, r.opts())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("canceled during", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := newRepo(gitx.DiffEntry{Status: "A", Path: "a.go"}, gitx.DiffEntry{Status: "A", Path: "b.go"})
		r.put(headRev, "a.go", "a", nil)
		r.put(headRev, "b.go", "b", nil)
		r.src.onRead = cancel
		_, err := Compute(ctx, r.src, baseRev, headRev, r.opts())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing both sides", func(t *testing.T) {
		r := newRepo(gitx.DiffEntry{Status: "M", Path: "a.go"})
		res, err := Compute(context.Background(), r.src, baseRev, headRev, r.opts())
		if err != nil {
			t.Fatal(err)
		}
		if f := res.Footprint.Files; len(f) != 1 || f[0].Parsed {
			t.Fatalf("files = %+v", f)
		}
	})
}

func TestComputeNow(t *testing.T) {
	r := newRepo()
	before := time.Now()
	res, err := Compute(context.Background(), r.src, baseRev, headRev, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if at := res.Footprint.ComputedAt; at.Before(before) || at.After(time.Now()) {
		t.Errorf("ComputedAt = %v", at)
	}
	if res.Footprint.Files != nil || res.Footprint.Symbols != nil || res.Footprint.Refs != nil {
		t.Errorf("empty diff produced %+v", res.Footprint)
	}
}

func TestComputeDeterministic(t *testing.T) {
	entries := []gitx.DiffEntry{
		{Status: "M", Path: "z.ts"},
		{Status: "A", Path: "a.ts"},
		{Status: "M", Path: "m.ts"},
	}
	build := func(order []int, reverseRefs bool) *repo {
		var es []gitx.DiffEntry
		for _, i := range order {
			es = append(es, entries[i])
		}
		r := newRepo(es...)
		for _, p := range []string{"z.ts", "m.ts"} {
			r.putFile(baseRev, p, "old\n", []def{
				{q: "b", kind: "function", sig: "function b()", body: "b", start: 1, end: 1},
				{q: "a", kind: "function", sig: "function a()", body: "a", start: 1, end: 1},
			}, nil)
		}
		for _, p := range []string{"z.ts", "m.ts", "a.ts"} {
			refs := []model.Ref{call("y", "", 1), call("x", "", 1), call("w", "", 2)}
			if reverseRefs {
				slices.Reverse(refs)
			}
			r.putFile(headRev, p, "new one\nnew two\n", []def{
				{q: "c", kind: "function", sig: "function c()", body: "c", start: 2, end: 2},
				{q: "b", kind: "function", sig: "function b(x)", body: "b2", start: 1, end: 1, params: []model.Param{{}}},
			}, refs)
		}
		return r
	}
	var first Result
	for i, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}} {
		r := build(order, i%2 == 1)
		res, err := Compute(context.Background(), r.src, baseRev, headRev, r.opts())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = res
			if len(res.Footprint.Symbols) == 0 || len(res.Footprint.Refs) == 0 {
				t.Fatalf("degenerate footprint %+v", res.Footprint)
			}
			continue
		}
		if !reflect.DeepEqual(res, first) {
			t.Fatalf("order %v:\n got %+v\nwant %+v", order, res.Footprint, first.Footprint)
		}
	}
	paths := func(fs []model.FileChange) (out []string) {
		for _, f := range fs {
			out = append(out, f.Path)
		}
		return out
	}
	if got := paths(first.Footprint.Files); !slices.IsSorted(got) {
		t.Errorf("files not sorted: %v", got)
	}
	if !slices.IsSortedFunc(first.Footprint.Refs, compareRefs) {
		t.Errorf("refs not sorted: %v", summarizeRefs(first.Footprint.Refs))
	}
}
