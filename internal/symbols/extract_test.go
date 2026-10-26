package symbols

import "testing"

// TestBodyHashFormatting checks the language-independent body hash rules: a
// renamed definition keeps its body hash, and formatter-only tokens do not
// count.
func TestBodyHashFormatting(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		old, new string
		oldQ     string
		newQ     string
		same     bool
	}{
		{
			name: "go rename keeps body",
			path: "a.go",
			old:  "package a\n\nfunc load(p string) error { return read(p) }\n",
			new:  "package a\n\nfunc loadFile(p string) error { return read(p) }\n",
			oldQ: "load", newQ: "loadFile", same: true,
		},
		{
			name: "go statements split by gofmt",
			path: "a.go",
			old:  "package a\n\nfunc F() { a(); b() }\n",
			new:  "package a\n\nfunc F() {\n\ta()\n\tb()\n}\n",
			oldQ: "F", newQ: "F", same: true,
		},
		{
			name: "go edit changes body",
			path: "a.go",
			old:  "package a\n\nfunc F() { a(); b() }\n",
			new:  "package a\n\nfunc F() { a(); c() }\n",
			oldQ: "F", newQ: "F", same: false,
		},
		{
			name: "rust trailing comma",
			path: "a.rs",
			old:  "fn f() { g(1, 2) }\n",
			new:  "fn f() {\n    g(\n        1,\n        2,\n    )\n}\n",
			oldQ: "f", newQ: "f", same: true,
		},
		{
			name: "python rename keeps body",
			path: "a.py",
			old:  "def load(p):\n    return read(p)\n",
			new:  "def load_file(p):\n    return read(p)\n",
			oldQ: "load", newQ: "load_file", same: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			of, err := Extract(tt.path, []byte(tt.old))
			if err != nil {
				t.Fatal(err)
			}
			nf, err := Extract(tt.path, []byte(tt.new))
			if err != nil {
				t.Fatal(err)
			}
			oldSym, ok := of.Find(tt.oldQ)
			if !ok {
				t.Fatalf("old %s not found", tt.oldQ)
			}
			newSym, ok := nf.Find(tt.newQ)
			if !ok {
				t.Fatalf("new %s not found", tt.newQ)
			}
			if got := oldSym.BodyHash == newSym.BodyHash; got != tt.same {
				t.Errorf("same body hash = %v, want %v", got, tt.same)
			}
		})
	}
}
