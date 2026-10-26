// Package goedit applies the benchmark's code changes to a Go module: body
// edits, new functions and calls, signature changes with their callers, and
// renames. Every change is described by an Op that can be applied again to a
// later version of the code, the way an agent redoes its change after main
// moved, so a rebase that conflicts can be resolved by applying the task's
// ops to the new main.
//
// It works on syntax only (go/parser), which is enough for the cases the
// task generator picks; the benchmark builds every generated task on its own
// and drops the ones this package gets wrong.
package goedit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Tree is a parsed Go module in a directory.
type Tree struct {
	Root   string
	Module string
	Fset   *token.FileSet
	Files  map[string]*File // by slash path relative to Root
	Pkgs   map[string]*Pkg  // by directory relative to Root ("." for the root)
	byPath map[string]*Pkg  // by import path
	funcs  map[string]*Func
	order  []string // file paths, sorted
	// members counts method, interface method and struct field names,
	// which calls by selector cannot tell apart by syntax.
	members map[string]int
}

// Pkg is one package directory.
type Pkg struct {
	Dir     string
	Name    string // package clause of its non-test files
	Path    string // import path
	Main    bool
	Files   []string        // non-test files
	Imports map[string]bool // module-internal import paths of its non-test files
}

// File is one parsed Go file.
type File struct {
	Rel  string
	Dir  string
	Src  []byte
	AST  *ast.File
	Test bool
}

// Func is a function or method declared with a body in a non-test file.
type Func struct {
	Key     string // "<dir>:<Name>" or "<dir>:<Recv>.<Name>"
	Dir     string
	File    string
	Name    string
	Recv    string // receiver base type, "" for a function
	Generic bool   // has type parameters, or a generic receiver
	Decl    *ast.FuncDecl
}

// FuncKey builds a key.
func FuncKey(dir, recv, name string) string {
	if recv == "" {
		return dir + ":" + name
	}
	return dir + ":" + recv + "." + name
}

// Load parses every Go file under root, skipping testdata, vendor and
// hidden directories.
func Load(root string) (*Tree, error) {
	mod, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	t := &Tree{Root: root, Module: mod, Fset: token.NewFileSet(), Files: map[string]*File{}, Pkgs: map[string]*Pkg{}, byPath: map[string]*Pkg{}, funcs: map[string]*Func{}, members: map[string]int{}}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			if p != root {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a nested module
				}
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(t.Fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		t.addFile(rel, src, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(t.order)
	for _, p := range t.Pkgs {
		sort.Strings(p.Files)
	}
	return t, nil
}

func modulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("no module line in %s/go.mod", root)
}

func (t *Tree) addFile(rel string, src []byte, f *ast.File) {
	dir := path.Dir(rel)
	file := &File{Rel: rel, Dir: dir, Src: src, AST: f, Test: strings.HasSuffix(rel, "_test.go")}
	t.Files[rel] = file
	t.order = append(t.order, rel)
	t.countMembers(f)
	if file.Test {
		return
	}
	if hasIgnoreBuildTag(f) {
		return
	}
	p := t.Pkgs[dir]
	if p == nil {
		ip := t.Module
		if dir != "." {
			ip = t.Module + "/" + dir
		}
		p = &Pkg{Dir: dir, Name: f.Name.Name, Path: ip, Imports: map[string]bool{}}
		t.Pkgs[dir] = p
		t.byPath[ip] = p
	}
	p.Main = p.Name == "main"
	p.Files = append(p.Files, rel)
	for _, im := range f.Imports {
		ipath, _ := strconv.Unquote(im.Path.Value)
		if ipath == t.Module || strings.HasPrefix(ipath, t.Module+"/") {
			p.Imports[ipath] = true
		}
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name == "init" || fd.Name.Name == "_" {
			continue
		}
		fn := &Func{Dir: dir, File: rel, Name: fd.Name.Name, Decl: fd, Generic: fd.Type.TypeParams != nil}
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			base, generic := recvBase(fd.Recv.List[0].Type)
			if base == "" {
				continue
			}
			fn.Recv = base
			fn.Generic = fn.Generic || generic
		}
		fn.Key = FuncKey(dir, fn.Recv, fn.Name)
		if _, dup := t.funcs[fn.Key]; dup {
			continue
		}
		t.funcs[fn.Key] = fn
	}
}

func (t *Tree) countMembers(f *ast.File) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Recv != nil {
				t.members[x.Name.Name]++
			}
		case *ast.InterfaceType:
			for _, m := range x.Methods.List {
				for _, nm := range m.Names {
					t.members[nm.Name]++
				}
			}
		case *ast.StructType:
			for _, fl := range x.Fields.List {
				for _, nm := range fl.Names {
					t.members[nm.Name]++
				}
			}
		}
		return true
	})
}

// UniqueMethod reports whether name is declared once in the module as a
// method, interface method or struct field, so that a selector call with
// that name can only call that method.
func (t *Tree) UniqueMethod(name string) bool { return t.members[name] <= 1 }

func hasIgnoreBuildTag(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:build") && strings.Contains(c.Text, "ignore") {
				return true
			}
		}
	}
	return false
}

func recvBase(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvBase(x.X)
	case *ast.ParenExpr:
		return recvBase(x.X)
	case *ast.Ident:
		return x.Name, false
	case *ast.IndexExpr:
		b, _ := recvBase(x.X)
		return b, true
	case *ast.IndexListExpr:
		b, _ := recvBase(x.X)
		return b, true
	}
	return "", false
}

// Func returns the function with the key, or nil.
func (t *Tree) Func(key string) *Func { return t.funcs[key] }

// Funcs returns every indexed function, sorted by key.
func (t *Tree) Funcs() []*Func {
	out := make([]*Func, 0, len(t.funcs))
	for _, f := range t.funcs {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// FilePaths returns every file path, sorted.
func (t *Tree) FilePaths() []string { return t.order }

// PkgByPath returns the package with an import path.
func (t *Tree) PkgByPath(ip string) *Pkg { return t.byPath[ip] }

// importName is the name a file uses for an import path, or "".
func importName(f *ast.File, ipath string, t *Tree) string {
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if p != ipath {
			continue
		}
		if im.Name != nil {
			if im.Name.Name == "_" || im.Name.Name == "." {
				return ""
			}
			return im.Name.Name
		}
		return defaultName(ipath, t)
	}
	return ""
}

// importPathOf maps a name used in f to its import path.
func importPathOf(f *ast.File, name string, t *Tree) string {
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		n := ""
		if im.Name != nil {
			n = im.Name.Name
		} else {
			n = defaultName(p, t)
		}
		if n == name {
			return p
		}
	}
	return ""
}

// defaultName is the package name an import path binds without an alias.
func defaultName(ipath string, t *Tree) string {
	if p := t.byPath[ipath]; p != nil {
		return p.Name
	}
	base := path.Base(ipath)
	if len(base) > 1 && base[0] == 'v' && strings.Trim(base[1:], "0123456789") == "" {
		base = path.Base(path.Dir(ipath)) // gopkg style major version suffix
	}
	if i := strings.IndexAny(base, ".-"); i >= 0 {
		if strings.HasPrefix(base, "go-") {
			base = base[3:]
		} else {
			base = base[:i]
		}
	}
	return base
}

// lineStart returns the offset of the start of the line holding off.
func lineStart(src []byte, off int) int {
	i := bytes.LastIndexByte(src[:off], '\n')
	return i + 1
}

func (t *Tree) off(p token.Pos) int { return t.Fset.Position(p).Offset }

// Exported reports whether a Go name is exported.
func Exported(name string) bool { return name != "" && name[0] >= 'A' && name[0] <= 'Z' }
