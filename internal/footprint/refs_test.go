package footprint

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// side builds one version of a file for changedRefs.
func side(path, src string, syms []model.Symbol, refs ...model.Ref) *version {
	f := &symbols.File{Lang: symbols.LangForPath(path), Symbols: syms}
	for _, r := range refs {
		r.Lang, r.Path = f.Lang, path
		f.Refs = append(f.Refs, r)
	}
	return &version{path: path, src: []byte(src), file: f}
}

// span builds a symbol over the given lines whose byte range starts at the
// first occurrence of from in src and ends after the first following
// occurrence of to.
func span(path, src, q string, startLine, endLine int, from, to string) model.Symbol {
	s := mkSymbol(symbols.LangForPath(path), path, "", def{q: q, kind: "function", start: startLine, end: endLine})
	s.StartByte = strings.Index(src, from)
	s.EndByte = s.StartByte + strings.Index(src[s.StartByte:], to) + len(to)
	return s
}

func refLines(refs []model.Ref) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func changeOf(kind model.ChangeKind, s model.Symbol) model.SymbolChange {
	return model.SymbolChange{Change: kind, Symbol: s}
}

func TestChangedRefsSharedLine(t *testing.T) {
	const src = "class A extends Base { m() { foo() } }\n"
	a := span("a.ts", src, "A", 1, 1, "class", "} }")
	m := span("a.ts", src, "A.m", 1, 1, "m()", "foo() }")
	newV := side("a.ts", src, []model.Symbol{a, m}, model.Ref{Name: "Base", Kind: model.RefExtends, Line: 1}, call("foo", "", 1))
	oldV := side("a.ts", src, []model.Symbol{a, m})
	tests := []struct {
		name    string
		changes []model.SymbolChange
		want    []string
	}{
		{"class changed", []model.SymbolChange{changeOf(model.ChangeSignature, a)}, []string{"Base", "foo"}},
		{"method changed", []model.SymbolChange{changeOf(model.ChangeBody, m)}, []string{"Base", "foo"}},
		{"nothing changed", nil, nil},
		{"removed symbols bring nothing", []model.SymbolChange{changeOf(model.ChangeRemoved, a)}, nil},
		{"moved without change", []model.SymbolChange{{Change: model.ChangeMoved, Symbol: a}}, nil},
		{"moved with a new body", []model.SymbolChange{{Change: model.ChangeMoved, BodyChanged: true, Symbol: m}}, []string{"Base", "foo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refLines(changedRefs(oldV, newV, tt.changes)); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedRefsInnermostSymbol(t *testing.T) {
	const src = `class Service:
    retries = compute()

    def run(self):
        helper()
`
	cls := span("s.py", src, "Service", 1, 5, "class", "helper()")
	run := span("s.py", src, "Service.run", 4, 5, "def run", "helper()")
	newV := side("s.py", src, []model.Symbol{cls, run}, call("compute", "", 2), call("helper", "", 5))
	oldV := side("s.py", src, []model.Symbol{cls, run})
	if got := refLines(changedRefs(oldV, newV, []model.SymbolChange{changeOf(model.ChangeBody, cls)})); !slices.Equal(got, []string{"compute"}) {
		t.Errorf("class changed: got %v", got)
	}
	if got := refLines(changedRefs(oldV, newV, []model.SymbolChange{changeOf(model.ChangeBody, run)})); !slices.Equal(got, []string{"helper"}) {
		t.Errorf("method changed: got %v", got)
	}
}

func TestChangedRefsTopLevel(t *testing.T) {
	tests := []struct {
		name     string
		old, new string // nil old when old == "-"
		refs     []model.Ref
		want     []string
	}{
		{"unchanged lines", "setup()\nregister(a)\n", "setup()\nregister(a)\n",
			[]model.Ref{call("setup", "", 1), call("register", "", 2)}, nil},
		{"moved lines are not changed", "setup()\nregister(a)\n", "register(a)\nsetup()\n",
			[]model.Ref{call("register", "", 1), call("setup", "", 2)}, nil},
		{"indentation does not count", "  setup()\r\n", "setup()\n",
			[]model.Ref{call("setup", "", 1)}, nil},
		{"each old line answers once", "register(a)\n", "register(a)\nregister(a)\n",
			[]model.Ref{call("register", "a1", 1), call("register", "a2", 2)}, []string{"register"}},
		{"new and edited lines", "register(a)\n", "register(b)\nboot()\n",
			[]model.Ref{call("register", "", 1), call("boot", "", 2)}, []string{"register", "boot"}},
		{"added file", "-", "setup()\n",
			[]model.Ref{call("setup", "", 1)}, []string{"setup"}},
		{"ref past the end", "setup()\n", "setup()\n",
			[]model.Ref{call("ghost", "", 9)}, []string{"ghost"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var oldV *version
			if tt.old != "-" {
				oldV = side("a.py", tt.old, nil)
			}
			newV := side("a.py", tt.new, nil, tt.refs...)
			if got := refLines(changedRefs(oldV, newV, nil)); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedRefsLinePartlyOutsideSymbol(t *testing.T) {
	oldSrc := "function f() { a() }; b()\n"
	newSrc := "function f() { a() }; c()\n"
	oldF := span("a.js", oldSrc, "f", 1, 1, "function", "a() }")
	newF := span("a.js", newSrc, "f", 1, 1, "function", "a() }")
	oldV := side("a.js", oldSrc, []model.Symbol{oldF})
	newV := side("a.js", newSrc, []model.Symbol{newF}, call("a", "", 1), call("c", "", 1))
	if got := refLines(changedRefs(oldV, newV, nil)); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("edited line: got %v", got)
	}
	same := side("a.js", oldSrc, []model.Symbol{oldF}, call("a", "", 1), call("b", "", 1))
	if got := changedRefs(oldV, same, nil); got != nil {
		t.Errorf("unchanged line: got %v", refLines(got))
	}
}

func TestChangedRefsWithoutByteRanges(t *testing.T) {
	const src = "class A {\n  x = make()\n}\nboot()\n"
	cls := mkSymbol("typescript", "a.ts", "", def{q: "A", kind: "class", start: 1, end: 3})
	oldV := side("a.ts", src, []model.Symbol{cls})
	newV := side("a.ts", src, []model.Symbol{cls}, call("A", "", 1), call("make", "", 2), call("boot", "", 4))
	if got := changedRefs(oldV, newV, nil); got != nil {
		t.Errorf("unchanged: got %v", refLines(got))
	}
	if got := refLines(changedRefs(oldV, newV, []model.SymbolChange{changeOf(model.ChangeBody, cls)})); !slices.Equal(got, []string{"A", "make"}) {
		t.Errorf("changed: got %v", got)
	}
}

func TestChangedRefsNilInput(t *testing.T) {
	if got := changedRefs(nil, nil, nil); got != nil {
		t.Errorf("nil: %v", got)
	}
	if got := changedRefs(nil, &version{path: "a.go"}, nil); got != nil {
		t.Errorf("no file: %v", got)
	}
}

func TestTopLevelLines(t *testing.T) {
	const src = `import x

def f():
    pass
x = 1  # tail
def g(): pass; y()
`
	f := span("a.py", src, "f", 3, 4, "def f", "pass")
	g := span("a.py", src, "g", 6, 6, "def g", "pass")
	got := topLevelLines([]model.Symbol{f, g}, newLines([]byte(src)))
	want := map[string]int{"import x": 1, "x = 1  # tail": 1, "def g(): pass; y()": 1}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := topLevelLines(nil, newLines(nil)); len(got) != 0 {
		t.Errorf("empty file: %v", got)
	}
}

func TestLines(t *testing.T) {
	tests := []struct {
		src   string
		count int
		texts []string
	}{
		{"", 0, nil},
		{"a", 1, []string{"a"}},
		{"a\n", 1, []string{"a"}},
		{"  a  \r\n\n\tb", 3, []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		l := newLines([]byte(tt.src))
		if l.count() != tt.count {
			t.Errorf("%q: count = %d, want %d", tt.src, l.count(), tt.count)
		}
		for i, want := range tt.texts {
			if got := l.text(i + 1); got != want {
				t.Errorf("%q: line %d = %q, want %q", tt.src, i+1, got, want)
			}
		}
		if l.text(0) != "" || l.text(tt.count+1) != "" {
			t.Errorf("%q: lines out of range are not empty", tt.src)
		}
	}
	l := newLines([]byte("ab\n  cd  \n"))
	if s, e, ok := l.bounds(2); !ok || s != 5 || e != 7 {
		t.Errorf("bounds(2) = %d %d %v", s, e, ok)
	}
}
