package footprint

import (
	"slices"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// fileOfDefs builds a file at path from defs, with no source text.
func fileOfDefs(path string, defs ...def) *symbols.File { return mkFile(path, "", defs, nil) }

func fn(q, sig, body string, line int, params ...model.Param) def {
	return def{q: q, kind: "function", sig: sig, body: body, start: line, end: line, params: params}
}

func TestDiffSymbols(t *testing.T) {
	const p = "pkg/a.go"
	tests := []struct {
		name     string
		old, new *symbols.File
		want     []string
	}{
		{"both nil", nil, nil, nil},
		{"added file", nil, fileOfDefs(p, fn("B", "func B()", "b", 2), fn("A", "func A()", "a", 1)),
			[]string{"pkg/a.go:A added", "pkg/a.go:B added"}},
		{"deleted file", fileOfDefs(p, fn("A", "func A()", "a", 1)), nil,
			[]string{"pkg/a.go:A removed!"}},
		{"unchanged, even when moved within the file",
			fileOfDefs(p, fn("A", "func A()", "a", 1)), fileOfDefs(p, fn("A", "func A()", "a", 9)), nil},
		{"body", fileOfDefs(p, fn("A", "func A()", "a", 1)), fileOfDefs(p, fn("A", "func A()", "a2", 1)),
			[]string{"pkg/a.go:A body"}},
		{"compatible signature",
			fileOfDefs(p, fn("A", "func A(int)", "a", 1, model.Param{Type: "int"})),
			fileOfDefs(p, fn("A", "func A(int, ...int)", "a", 1, model.Param{Type: "int"}, model.Param{Type: "int", Variadic: true})),
			[]string{"pkg/a.go:A signature"}},
		{"breaking signature",
			fileOfDefs(p, fn("A", "func A(int)", "a", 1, model.Param{Type: "int"})),
			fileOfDefs(p, fn("A", "func A(string)", "a", 1, model.Param{Type: "string"})),
			[]string{"pkg/a.go:A signature!"}},
		{"renamed",
			fileOfDefs(p, fn("old", "func old()", "same", 1)),
			fileOfDefs(p, fn("fresh", "func fresh()", "same", 1)),
			[]string{"pkg/a.go:fresh renamed! from old"}},
		{"rename needs the same kind",
			fileOfDefs(p, fn("old", "func old()", "same", 1)),
			fileOfDefs(p, def{q: "Fresh", kind: "type", sig: "type Fresh", body: "same", start: 1, end: 1}),
			[]string{"pkg/a.go:Fresh added", "pkg/a.go:old removed!"}},
		{"rename needs a body hash",
			&symbols.File{Symbols: []model.Symbol{{Lang: "go", Path: p, Name: "old", Qualified: "old", Kind: "function"}}},
			&symbols.File{Symbols: []model.Symbol{{Lang: "go", Path: p, Name: "fresh", Qualified: "fresh", Kind: "function"}}},
			[]string{"pkg/a.go:fresh added", "pkg/a.go:old removed!"}},
		{"each symbol pairs once, in source order",
			fileOfDefs(p, fn("second", "func second()", "same", 2), fn("first", "func first()", "same", 1)),
			fileOfDefs(p, fn("one", "func one()", "same", 1)),
			[]string{"pkg/a.go:one renamed! from first", "pkg/a.go:second removed!"}},
		{"rename prefers the same container",
			fileOfDefs(p,
				def{q: "T.m", kind: "method", sig: "func (T) m()", body: "same", start: 1, end: 1}),
			fileOfDefs(p,
				def{q: "S.n", kind: "method", sig: "func (S) n()", body: "same", start: 1, end: 1},
				def{q: "T.n", kind: "method", sig: "func (T) n()", body: "same", start: 2, end: 2}),
			[]string{"pkg/a.go:S.n added", "pkg/a.go:T.n renamed! from T.m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarize(DiffSymbols(tt.old, tt.new))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestDiffSymbolsFields(t *testing.T) {
	old := fileOfDefs("a.ts", fn("f", "function f(string)", "body", 1, model.Param{Type: "string"}))
	new := fileOfDefs("a.ts", fn("f", "function f(string, number?)", "body 2", 1,
		model.Param{Type: "string"}, model.Param{Type: "number", Optional: true}))
	cs := DiffSymbols(old, new)
	if len(cs) != 1 {
		t.Fatalf("changes = %v", summarize(cs))
	}
	c := cs[0]
	if c.Change != model.ChangeSignature || c.Breaking || !c.SigChanged || !c.BodyChanged ||
		c.OldSignature != "function f(string)" || c.OldSigHash != old.Symbols[0].SigHash ||
		!sameSymbol(c.Symbol, new.Symbols[0]) {
		t.Errorf("change = %+v", c)
	}

	renamed := DiffSymbols(fileOfDefs("a.ts", fn("f", "function f()", "same", 1)), fileOfDefs("a.ts", fn("g", "function g()", "same", 1)))
	if len(renamed) != 1 {
		t.Fatalf("changes = %v", summarize(renamed))
	}
	r := renamed[0]
	if r.OldName != "f" || r.OldSignature != "function f()" || r.OldSigHash == "" || !r.SigChanged || r.BodyChanged || !r.Breaking {
		t.Errorf("renamed = %+v", r)
	}

	removed := DiffSymbols(old, nil)
	if len(removed) != 1 || removed[0].Symbol.Path != "a.ts" || removed[0].Symbol.Qualified != "f" {
		t.Errorf("removed = %+v", removed)
	}
}

func sameSymbol(a, b model.Symbol) bool {
	return a.Key == b.Key && a.SigHash == b.SigHash && a.BodyHash == b.BodyHash && a.Signature == b.Signature
}

func TestDiffSymbolsWithoutKeys(t *testing.T) {
	strip := func(f *symbols.File) *symbols.File {
		for i := range f.Symbols {
			f.Symbols[i].Key = ""
		}
		return f
	}
	old := strip(fileOfDefs("a.py", fn("f", "def f()", "1", 1)))
	new := strip(fileOfDefs("a.py", fn("f", "def f()", "2", 1)))
	if got := summarize(DiffSymbols(old, new)); !slices.Equal(got, []string{"a.py:f body"}) {
		t.Errorf("got %q", got)
	}
}

func TestDiffMoved(t *testing.T) {
	str := model.Param{Type: "string"}
	optNum := model.Param{Type: "number", Optional: true}
	num := model.Param{Type: "number"}
	tests := []struct {
		name     string
		old, new *symbols.File
		want     []string
	}{
		{"same Go package", fileOfDefs("pkg/a.go", fn("F", "func F()", "f", 1)), fileOfDefs("pkg/b.go", fn("F", "func F()", "f", 1)),
			[]string{"pkg/b.go:F moved from pkg/a.go sig=false body=false"}},
		{"other Go package", fileOfDefs("pkg/a.go", fn("F", "func F()", "f", 1)), fileOfDefs("lib/a.go", fn("F", "func F()", "f", 1)),
			[]string{"lib/a.go:F moved! from pkg/a.go sig=false body=false"}},
		{"body changed", fileOfDefs("pkg/a.go", fn("F", "func F()", "f", 1)), fileOfDefs("pkg/b.go", fn("F", "func F()", "f2", 1)),
			[]string{"pkg/b.go:F moved from pkg/a.go sig=false body=true"}},
		{"compatible signature change",
			fileOfDefs("src/a.ts", fn("f", "function f(string)", "f", 1, str)),
			fileOfDefs("src/a/index.ts", fn("f", "function f(string, number?)", "f", 1, str, optNum)),
			[]string{"src/a/index.ts:f moved from src/a.ts sig=true body=false"}},
		{"breaking signature change",
			fileOfDefs("src/a.ts", fn("f", "function f(string)", "f", 1, str)),
			fileOfDefs("src/a/index.ts", fn("f", "function f(string, number)", "f", 1, str, num)),
			[]string{"src/a/index.ts:f moved! from src/a.ts sig=true body=false"}},
		{"index file loses its own module", fileOfDefs("src/a/index.ts", fn("f", "function f()", "f", 1)), fileOfDefs("src/a.ts", fn("f", "function f()", "f", 1)),
			[]string{"src/a.ts:f moved! from src/a/index.ts sig=false body=false"}},
		{"JavaScript to TypeScript", fileOfDefs("src/a.js", fn("f", "function f()", "f", 1)), fileOfDefs("src/a.ts", fn("f", "function f()", "f", 1)),
			[]string{"src/a.ts:f moved from src/a.js sig=false body=false"}},
		{"Python module to package", fileOfDefs("app/a.py", fn("f", "def f()", "f", 1)), fileOfDefs("app/a/__init__.py", fn("f", "def f()", "f", 1)),
			[]string{"app/a/__init__.py:f moved from app/a.py sig=false body=false"}},
		{"Rust module to mod.rs", fileOfDefs("src/net.rs", fn("f", "fn f()", "f", 1)), fileOfDefs("src/net/mod.rs", fn("f", "fn f()", "f", 1)),
			[]string{"src/net/mod.rs:f moved from src/net.rs sig=false body=false"}},
		{"one-sided symbols",
			fileOfDefs("pkg/a.go", fn("Old", "func Old()", "o", 1), fn("Kept", "func Kept()", "k", 2)),
			fileOfDefs("pkg/b.go", fn("Kept", "func Kept()", "k", 1), fn("New", "func New()", "o", 2)),
			[]string{"pkg/a.go:Old removed!", "pkg/b.go:Kept moved from pkg/a.go sig=false body=false", "pkg/b.go:New added"}},
		{"nil sides", nil, fileOfDefs("pkg/b.go", fn("New", "func New()", "n", 1)), []string{"pkg/b.go:New added"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarize(DiffMoved(tt.old, tt.new)); !slices.Equal(got, tt.want) {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestDiffMovedFields(t *testing.T) {
	old := fileOfDefs("a/x.py", fn("f", "def f(a)", "f", 1, model.Param{Name: "a"}))
	new := fileOfDefs("b/x.py", fn("f", "def f(b)", "f", 1, model.Param{Name: "b"}))
	cs := DiffMoved(old, new)
	if len(cs) != 1 {
		t.Fatalf("changes = %v", summarize(cs))
	}
	c := cs[0]
	if c.Change != model.ChangeMoved || !c.Breaking || !c.SigChanged || c.BodyChanged || c.OldPath != "a/x.py" ||
		c.OldSignature != "def f(a)" || c.OldSigHash != old.Symbols[0].SigHash || c.Symbol.Path != "b/x.py" {
		t.Errorf("moved = %+v", c)
	}
	if !c.SignatureChanging() {
		t.Error("a moved symbol with a new signature must count as signature changing")
	}

	same := DiffMoved(fileOfDefs("a.py", fn("f", "def f()", "f", 1)), fileOfDefs("b.py", fn("f", "def f()", "f", 1)))
	if len(same) != 1 || same[0].OldSignature != "" || same[0].OldSigHash != "" || !same[0].Breaking {
		t.Errorf("moved across modules = %+v", same)
	}
}
