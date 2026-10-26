package goedit

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Kind is the kind of an Op.
type Kind string

// The op kinds.
const (
	BodyInsert  Kind = "body_insert"  // add statements to a function body
	BodyLiteral Kind = "body_literal" // change string literals in a body
	BodyWrap    Kind = "body_wrap"    // put a run of statements inside a new block
	AddFunc     Kind = "add_func"     // add a new function that calls others
	AddCall     Kind = "add_call"     // add a call to a function inside another
	AddParam    Kind = "add_param"    // add a required parameter and fix callers
	RenameFunc  Kind = "rename_func"  // rename a function and fix references
	Changelog   Kind = "changelog"    // add an entry to CHANGELOG.md
)

// Op is one change of a task. Ops name functions by key, so they can be
// applied again to a later tree.
type Op struct {
	Kind    Kind     `json:"kind"`
	Target  string   `json:"target,omitempty"`
	Callees []string `json:"callees,omitempty"`
	File    string   `json:"file,omitempty"`
	Anchor  float64  `json:"anchor,omitempty"`
	Size    int      `json:"size,omitempty"`
	Tag     string   `json:"tag"`
	Variant int      `json:"variant,omitempty"`
	NewName string   `json:"newName,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// Steps is how many increments the op is applied in.
func (o Op) Steps() int {
	switch o.Kind {
	case BodyInsert, BodyLiteral, AddFunc:
		return max(1, o.Size)
	case AddParam, RenameFunc, BodyWrap:
		return 2
	}
	return 1
}

// ErrGone means the op's target no longer exists in the tree.
var ErrGone = errors.New("target is gone")

// Renames maps a function key to the key it was renamed to.
type Renames map[string]string

func (r Renames) resolve(t *Tree, key string) *Func {
	for range 8 {
		if fn := t.Func(key); fn != nil {
			return fn
		}
		next, ok := r[key]
		if !ok {
			return nil
		}
		key = next
	}
	return nil
}

// Apply applies one step of op to the tree's directory and writes the
// changed files. The tree is stale afterwards; Load it again.
func Apply(t *Tree, op Op, step int, ren Renames) error {
	c := &change{t: t, files: map[string]*fileChange{}}
	var err error
	switch op.Kind {
	case BodyInsert:
		err = c.bodyInsert(op, step, ren)
	case BodyLiteral:
		err = c.bodyLiteral(op, step, ren)
	case BodyWrap:
		err = c.bodyWrap(op, step, ren)
	case AddFunc:
		err = c.addFunc(op, step, ren)
	case AddCall:
		err = c.addCall(op, ren)
	case AddParam:
		err = c.addParam(op, step, ren)
	case RenameFunc:
		err = c.rename(op, step, ren)
	case Changelog:
		return changelog(t.Root, op)
	default:
		err = fmt.Errorf("unknown op kind %q", op.Kind)
	}
	if err != nil {
		return err
	}
	return c.write()
}

// ApplyAll applies every step of every op in order, as a redo on a new
// base. Ops whose target is gone are skipped and reported.
func ApplyAll(root string, ops []Op, ren Renames) (gone []string, err error) {
	for _, op := range ops {
		for s := range op.Steps() {
			t, err := Load(root)
			if err != nil {
				return gone, err
			}
			if err := Apply(t, op, s, ren); err != nil {
				if errors.Is(err, ErrGone) {
					gone = append(gone, op.Tag)
					break
				}
				return gone, fmt.Errorf("%s step %d: %w", op.Tag, s, err)
			}
		}
	}
	return gone, nil
}

type edit struct {
	off, end int
	text     string
}

type fileChange struct {
	f       *File
	newFile bool
	src     []byte // for new files
	edits   []edit
	need    imports
}

type change struct {
	t     *Tree
	files map[string]*fileChange
	only  map[string]bool // when set, the only files fixCalls and rename touch
}

func (c *change) file(rel string) *fileChange {
	fc := c.files[rel]
	if fc == nil {
		fc = &fileChange{f: c.t.Files[rel], need: imports{}}
		c.files[rel] = fc
	}
	return fc
}

func (c *change) add(rel string, off, end int, text string) {
	fc := c.file(rel)
	fc.edits = append(fc.edits, edit{off, end, text})
}

func (c *change) needs(rel string, need imports) {
	fc := c.file(rel)
	for p, n := range need {
		fc.need[p] = n
	}
}

func (c *change) write() error {
	names := make([]string, 0, len(c.files))
	for n := range c.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, rel := range names {
		fc := c.files[rel]
		var src []byte
		if fc.newFile {
			src = fc.src
		} else {
			src = applyEdits(fc.f.Src, append(fc.edits, importEdits(c.t, fc.f, fc.need)...))
		}
		out, err := format.Source(src)
		if err != nil {
			return fmt.Errorf("format %s: %w\n%s", rel, err, src)
		}
		p := filepath.Join(c.t.Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func applyEdits(src []byte, edits []edit) []byte {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].off > edits[j].off })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.off], append([]byte(e.text), out[e.end:]...)...)
	}
	return out
}

// importEdits adds the imports a change needs to a file.
func importEdits(t *Tree, f *File, need imports) []edit {
	if len(need) == 0 {
		return nil
	}
	paths := make([]string, 0, len(need))
	for p := range need {
		if importName(f.AST, p, t) == "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	var lines []string
	for _, p := range paths {
		spec := strconv.Quote(p)
		if need[p] != defaultName(p, t) {
			spec = need[p] + " " + spec
		}
		lines = append(lines, spec)
	}
	// The last parenthesized import declaration gets the new lines before
	// its closing paren; gofmt sorts them into place.
	var last *ast.GenDecl
	for _, d := range f.AST.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			last = g
		}
	}
	if last != nil && last.Rparen.IsValid() {
		off := t.off(last.Rparen)
		return []edit{{off, off, "\t" + strings.Join(lines, "\n\t") + "\n"}}
	}
	if last != nil {
		// import "x" -> import ( "x" ... )
		off, end := t.off(last.Pos()), t.off(last.End())
		old := string(f.Src[off:end])
		spec := strings.TrimSpace(strings.TrimPrefix(old, "import"))
		return []edit{{off, end, "import (\n\t" + spec + "\n\t" + strings.Join(lines, "\n\t") + "\n)"}}
	}
	off := t.off(f.AST.Name.End())
	return []edit{{off, off, "\n\nimport (\n\t" + strings.Join(lines, "\n\t") + "\n)\n"}}
}

func marker(tag string, step int) string { return fmt.Sprintf("%s.%d", tag, step) }

// hasMarker reports whether the body holds a string literal containing m.
func hasMarker(n ast.Node, m string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if lit, ok := x.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, m) {
			found = true
		}
		return !found
	})
	return found
}

// insertPoint is where a step of op goes in a body: after the statement that
// holds the previous step's marker, else before statement round(anchor*(n-1)).
func (c *change) insertPoint(fn *Func, op Op, step int) int {
	body := fn.Decl.Body
	src := c.t.Files[fn.File].Src
	for s := step - 1; s >= 0; s-- {
		m := marker(op.Tag, s)
		for _, st := range body.List {
			if hasMarker(st, m) {
				end := c.t.off(st.End())
				if i := bytes.IndexByte(src[end:], '\n'); i >= 0 {
					return end + i + 1
				}
				return end
			}
		}
	}
	n := len(body.List)
	if n == 0 {
		return lineStart(src, c.t.off(body.Rbrace))
	}
	k := int(op.Anchor*float64(n-1) + 0.5)
	k = min(max(k, 0), n-1)
	return lineStart(src, c.t.off(body.List[k].Pos()))
}

func block(op Op, step int) string {
	m := marker(op.Tag, step)
	return fmt.Sprintf("if n := len(%q); n < 0 {\npanic(%q)\n}\n", m, fmt.Sprintf("%s: unreachable check v%d", m, op.Variant))
}

func (c *change) target(op Op, ren Renames) (*Func, error) {
	fn := ren.resolve(c.t, op.Target)
	if fn == nil {
		return nil, fmt.Errorf("%s: %w", op.Target, ErrGone)
	}
	return fn, nil
}

func (c *change) bodyInsert(op Op, step int, ren Renames) error {
	fn, err := c.target(op, ren)
	if err != nil {
		return err
	}
	if hasMarker(fn.Decl.Body, marker(op.Tag, step)) {
		return nil
	}
	off := c.insertPoint(fn, op, step)
	c.add(fn.File, off, off, block(op, step))
	return nil
}

// stringLits lists the string literals in a body, outside struct tags.
func stringLits(body *ast.BlockStmt) []*ast.BasicLit {
	tags := map[*ast.BasicLit]bool{}
	var lits []*ast.BasicLit
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			if x.Tag != nil {
				tags[x.Tag] = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && !tags[x] {
				lits = append(lits, x)
			}
		}
		return true
	})
	return lits
}

func (c *change) bodyLiteral(op Op, step int, ren Renames) error {
	fn, err := c.target(op, ren)
	if err != nil {
		return err
	}
	m := "(" + marker(op.Tag, step) + ")"
	if hasMarker(fn.Decl.Body, m) || hasMarker(fn.Decl.Body, marker(op.Tag, step)) {
		return nil
	}
	var free []*ast.BasicLit
	for _, l := range stringLits(fn.Decl.Body) {
		if !strings.Contains(l.Value, "("+op.Tag+".") {
			free = append(free, l)
		}
	}
	if len(free) == 0 {
		return c.bodyInsert(op, step, ren)
	}
	l := free[int(op.Anchor*float64(len(free)))%len(free)]
	end := c.t.off(l.End()) - 1 // before the closing quote
	suffix := " " + m
	if op.Variant > 0 {
		suffix = fmt.Sprintf(" %s v%d", m, op.Variant)
	}
	c.add(fn.File, end, end, suffix)
	return nil
}

// wrappable reports whether a top-level statement can move into a block:
// nothing it declares may be needed after it, and it must not be a label or
// a jump.
func wrappable(st ast.Stmt) bool {
	switch x := st.(type) {
	case *ast.DeclStmt, *ast.LabeledStmt, *ast.BranchStmt:
		return false
	case *ast.AssignStmt:
		return x.Tok != token.DEFINE
	}
	return true
}

// bodyWrap puts up to Size statements, from the anchor on, inside
// "if wrapTAG := "TAG.0"; wrapTAG != "" { ... }", the shape of a refactor
// that re-indents a block. Step 1 adds a statement at the top of the block.
// The last statement is never wrapped, so a function keeps its terminating
// return.
func (c *change) bodyWrap(op Op, step int, ren Renames) error {
	fn, err := c.target(op, ren)
	if err != nil {
		return err
	}
	m0 := marker(op.Tag, 0)
	body := fn.Decl.Body
	src := c.t.Files[fn.File].Src
	var wrapper *ast.IfStmt
	for _, st := range body.List {
		if ifs, ok := st.(*ast.IfStmt); ok && ifs.Init != nil && hasMarker(ifs.Init, m0) {
			wrapper = ifs
		}
	}
	if step == 0 {
		if wrapper != nil || hasMarker(body, m0) {
			return nil
		}
		n := len(body.List)
		if n < 2 {
			return c.bodyInsert(op, step, ren)
		}
		k := int(op.Anchor*float64(n-2) + 0.5)
		k = min(max(k, 0), n-2)
		end := k
		for end < n-1 && end-k < max(1, op.Size) && wrappable(body.List[end]) {
			end++
		}
		if end == k {
			return c.bodyInsert(op, step, ren)
		}
		start := lineStart(src, c.t.off(body.List[k].Pos()))
		stop := c.t.off(body.List[end-1].End())
		if i := bytes.IndexByte(src[stop:], '\n'); i >= 0 {
			stop += i + 1
		}
		v := "wrap" + op.Tag
		text := fmt.Sprintf("if %s := %q; %s != \"\" {\n%s}\n", v, m0, v, src[start:stop])
		c.add(fn.File, start, stop, text)
		return nil
	}
	m1 := marker(op.Tag, 1)
	if hasMarker(body, m1) {
		return nil
	}
	if wrapper == nil {
		return c.bodyInsert(op, step, ren)
	}
	off := c.t.off(wrapper.Body.Lbrace) + 1
	c.add(fn.File, off, off, "\n"+block(op, step))
	return nil
}

func featureName(tag string) string { return tag + "Feature" }

func (c *change) addFunc(op Op, step int, ren Renames) error {
	name := featureName(op.Tag)
	f := c.t.Files[op.File]
	var callee *Func
	if len(op.Callees) > 0 {
		callee = ren.resolve(c.t, op.Callees[step%len(op.Callees)])
	}
	stmt := func(to *File) (string, imports) {
		m := fmt.Sprintf("_ = %q\n", marker(op.Tag, step))
		if callee == nil || to == nil {
			return m, nil
		}
		call, need, err := c.t.callStmt(callee, to)
		if err != nil {
			return m, nil
		}
		return m + call + "\n", need
	}
	if f == nil {
		if step > 0 {
			return fmt.Errorf("%s: file %s: %w", op.Tag, op.File, ErrGone)
		}
		pkg := c.t.Pkgs[path.Dir(op.File)]
		if pkg == nil {
			return fmt.Errorf("no package in %s", path.Dir(op.File))
		}
		// Render against a stand-in file of the package, then build the
		// new file's import block from what the call needs.
		stand := &File{Rel: op.File, Dir: pkg.Dir, AST: &ast.File{Name: ast.NewIdent(pkg.Name)}}
		body, need := stmt(stand)
		var b strings.Builder
		fmt.Fprintf(&b, "package %s\n\n", pkg.Name)
		if len(need) > 0 {
			b.WriteString("import (\n")
			paths := make([]string, 0, len(need))
			for p := range need {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				if need[p] != defaultName(p, c.t) {
					fmt.Fprintf(&b, "\t%s %q\n", need[p], p)
				} else {
					fmt.Fprintf(&b, "\t%q\n", p)
				}
			}
			b.WriteString(")\n\n")
		}
		fmt.Fprintf(&b, "// %s is the change of task %s.\nfunc %s() {\n%s}\n", name, op.Tag, name, body)
		c.files[op.File] = &fileChange{newFile: true, src: []byte(b.String())}
		return nil
	}
	var decl *ast.FuncDecl
	for _, d := range f.AST.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			decl = fd
		}
	}
	if decl != nil && hasMarker(decl.Body, marker(op.Tag, step)) {
		return nil
	}
	body, need := stmt(f)
	c.needs(f.Rel, need)
	if decl == nil {
		end := len(f.Src)
		c.add(f.Rel, end, end, fmt.Sprintf("\n// %s is the change of task %s.\nfunc %s() {\n%s}\n", name, op.Tag, name, body))
		return nil
	}
	off := lineStart(f.Src, c.t.off(decl.Body.Rbrace))
	c.add(f.Rel, off, off, body)
	return nil
}

func (c *change) addCall(op Op, ren Renames) error {
	host, err := c.target(op, ren)
	if err != nil {
		return err
	}
	if hasMarker(host.Decl.Body, marker(op.Tag, 0)) {
		return nil
	}
	f := c.t.Files[host.File]
	text := fmt.Sprintf("_ = %q\n", marker(op.Tag, 0))
	if len(op.Callees) > 0 {
		if callee := ren.resolve(c.t, op.Callees[0]); callee != nil {
			if call, need, err := c.t.callStmt(callee, f); err == nil {
				text += call + "\n"
				c.needs(f.Rel, need)
			}
		}
	}
	off := c.insertPoint(host, op, 0)
	c.add(host.File, off, off, text)
	return nil
}

// ParamName is the parameter an AddParam op adds.
func ParamName(tag string) string { return tag + "Opt" }

func (c *change) addParam(op Op, step int, ren Renames) error {
	fn, err := c.target(op, ren)
	if err != nil {
		return err
	}
	if step == 0 {
		if !hasParam(fn.Decl.Type, ParamName(op.Tag)) {
			pl := fn.Decl.Type.Params
			text := ParamName(op.Tag) + " bool"
			if len(pl.List) == 0 {
				off := c.t.off(pl.Opening) + 1
				c.add(fn.File, off, off, text)
			} else {
				off := c.t.off(pl.List[len(pl.List)-1].End())
				c.add(fn.File, off, off, ", "+text)
			}
		}
		return c.fixCalls(fn, extraParam{name: ParamName(op.Tag)}, fn.File)
	}
	return c.fixCalls(fn, extraParam{}, "")
}

func hasParam(ft *ast.FuncType, name string) bool {
	for _, f := range ft.Params.List {
		for _, n := range f.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// extraParam is a bool parameter being added in this same change, so that it
// is not in the declaration the tree holds yet.
type extraParam struct{ name string }

// fixCalls appends zero values to every call of fn that passes fewer
// arguments than fn declares. With only set, it fixes that file alone.
func (c *change) fixCalls(fn *Func, extra extraParam, only string) error {
	types, variadic := params(fn.Decl.Type)
	if variadic || fn.Recv != "" && !c.t.UniqueMethod(fn.Name) {
		return nil
	}
	if extra.name != "" && !hasParam(fn.Decl.Type, extra.name) {
		types = append(types, ast.NewIdent("bool"))
	}
	from := c.t.Files[fn.File]
	for _, rel := range c.t.FilePaths() {
		if only != "" && rel != only || c.only != nil && !c.only[rel] {
			continue
		}
		f := c.t.Files[rel]
		q := &qualifier{t: c.t, from: from, to: f, need: imports{}}
		for _, call := range c.callsOf(fn, f) {
			if call.Ellipsis.IsValid() || len(call.Args) >= len(types) {
				continue
			}
			var zs []string
			ok := true
			for _, pt := range types[len(call.Args):] {
				z, err := q.zero(pt)
				if err != nil {
					ok = false
					break
				}
				zs = append(zs, z)
			}
			if !ok {
				continue
			}
			text := strings.Join(zs, ", ")
			if len(call.Args) == 0 {
				off := c.t.off(call.Lparen) + 1
				c.add(rel, off, off, text)
			} else {
				off := c.t.off(call.Args[len(call.Args)-1].End())
				c.add(rel, off, off, ", "+text)
			}
		}
		c.needs(rel, q.need)
	}
	return nil
}

// callsOf finds the calls of fn in a file, by syntax: same-package calls by
// name, other packages through their import name, and methods by name (the
// generator only changes methods whose name is unique in the module).
func (c *change) callsOf(fn *Func, f *File) []*ast.CallExpr {
	var out []*ast.CallExpr
	pkg := c.t.Pkgs[fn.Dir]
	samePkg := f.Dir == fn.Dir && !strings.HasSuffix(f.AST.Name.Name, "_test")
	alias := ""
	if pkg != nil && !samePkg {
		alias = importName(f.AST, pkg.Path, c.t)
	}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fn.Recv == "" && samePkg && fun.Name == fn.Name {
				out = append(out, call)
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name != fn.Name {
				return true
			}
			x, isIdent := fun.X.(*ast.Ident)
			if fn.Recv == "" {
				if isIdent && alias != "" && x.Name == alias {
					out = append(out, call)
				}
				return true
			}
			if isIdent && importPathOf(f.AST, x.Name, c.t) != "" {
				return true // a package-qualified function of the same name
			}
			out = append(out, call)
		}
		return true
	})
	return out
}

// RenamedName is the name a RenameFunc op gives.
func RenamedName(name string) string { return name + "V2" }

func (c *change) rename(op Op, step int, ren Renames) error {
	fn := c.t.Func(op.Target)
	dir, oldName, _ := strings.Cut(op.Target, ":")
	newName := op.NewName
	if newName == "" {
		newName = RenamedName(oldName)
	}
	if fn == nil {
		if c.t.Func(FuncKey(dir, "", newName)) == nil {
			return fmt.Errorf("%s: %w", op.Target, ErrGone)
		}
	} else if step == 0 {
		off := c.t.off(fn.Decl.Name.Pos())
		c.add(fn.File, off, off+len(oldName), newName)
	}
	pkg := c.t.Pkgs[dir]
	for _, rel := range c.t.FilePaths() {
		f := c.t.Files[rel]
		if step == 0 && fn != nil && rel != fn.File || c.only != nil && !c.only[rel] {
			continue
		}
		samePkg := f.Dir == dir && !strings.HasSuffix(f.AST.Name.Name, "_test")
		alias := ""
		if pkg != nil && !samePkg {
			alias = importName(f.AST, pkg.Path, c.t)
		}
		if !samePkg && alias == "" {
			continue
		}
		skip := map[*ast.Ident]bool{}
		if fn != nil {
			skip[fn.Decl.Name] = true
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && alias != "" && id.Name == alias && x.Sel.Name == oldName {
					off := c.t.off(x.Sel.Pos())
					c.add(rel, off, off+len(oldName), newName)
				}
				skip[x.Sel] = true
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok {
					skip[id] = true
				}
			case *ast.Field:
				for _, nm := range x.Names {
					skip[nm] = true
				}
			case *ast.FuncDecl:
				if x.Recv != nil {
					skip[x.Name] = true
				}
			case *ast.Ident:
				if samePkg && x.Name == oldName && !skip[x] {
					off := c.t.off(x.Pos())
					c.add(rel, off, off+len(oldName), newName)
				}
			}
			return true
		})
	}
	return nil
}

// changelog adds op.Text under "## Unreleased" in CHANGELOG.md.
func changelog(root string, op Op) error {
	p := filepath.Join(root, "CHANGELOG.md")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("- %s: %s\n", op.Tag, op.Text)
	if bytes.Contains(b, []byte("- "+op.Tag+": ")) {
		return nil
	}
	head := []byte("## Unreleased\n\n")
	i := bytes.Index(b, head)
	if i < 0 {
		return fmt.Errorf("CHANGELOG.md has no Unreleased section")
	}
	at := i + len(head)
	out := append(append(append([]byte(nil), b[:at]...), line...), b[at:]...)
	return os.WriteFile(p, out, 0o644)
}

// CallerFiles lists the files that call fn, or for a function also refer to
// it, which a signature change or rename must touch.
func CallerFiles(t *Tree, fn *Func) []string {
	c := &change{t: t, files: map[string]*fileChange{}}
	var out []string
	for _, rel := range t.FilePaths() {
		if rel == fn.File {
			continue
		}
		if len(c.callsOf(fn, t.Files[rel])) > 0 {
			out = append(out, rel)
		}
	}
	return out
}

// FixCalls appends zero values to every call of the function with the key
// that passes too few arguments, in files (all files when nil), and renames
// references that ren maps to a new name there. It is what an agent does to
// its own code after taking in a changed signature from someone else.
func FixCalls(t *Tree, keys []string, ren Renames, files []string) error {
	c := &change{t: t, files: map[string]*fileChange{}}
	if files != nil {
		c.only = map[string]bool{}
		for _, f := range files {
			c.only[f] = true
		}
	}
	for _, key := range keys {
		if fn := t.Func(key); fn != nil {
			if err := c.fixCalls(fn, extraParam{}, ""); err != nil {
				return err
			}
		}
	}
	for old, nw := range ren {
		if t.Func(old) != nil {
			continue
		}
		dir, newName, _ := strings.Cut(nw, ":")
		if t.Func(FuncKey(dir, "", newName)) == nil {
			continue
		}
		if err := c.rename(Op{Kind: RenameFunc, Target: old, NewName: newName}, 1, nil); err != nil {
			return err
		}
	}
	return c.write()
}
