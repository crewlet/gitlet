package goedit

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

var predeclared = map[string]string{
	"bool": "false", "string": `""`, "error": "nil", "any": "nil",
	"int": "0", "int8": "0", "int16": "0", "int32": "0", "int64": "0",
	"uint": "0", "uint8": "0", "uint16": "0", "uint32": "0", "uint64": "0", "uintptr": "0",
	"float32": "0", "float64": "0", "complex64": "0", "complex128": "0",
	"byte": "0", "rune": "0",
}

// imports collects the imports a change needs in one file: import path to
// the name the change uses for it.
type imports map[string]string

// qualifier renders type expressions declared in one file (from) so that
// they are valid in another (to).
type qualifier struct {
	t        *Tree
	from, to *File
	need     imports
}

// pkgName returns the name to use in q.to for an import path, adding the
// import when needed. It fails when the default name is taken by another
// import of q.to.
func (q *qualifier) pkgName(ipath string) (string, error) {
	if q.to.Dir == dirOfPath(q.t, ipath) && !strings.HasSuffix(q.to.AST.Name.Name, "_test") {
		return "", nil
	}
	if n := importName(q.to.AST, ipath, q.t); n != "" {
		return n, nil
	}
	if n, ok := q.need[ipath]; ok {
		return n, nil
	}
	n := defaultName(ipath, q.t)
	if other := importPathOf(q.to.AST, n, q.t); other != "" && other != ipath {
		return "", fmt.Errorf("name %s already imported in %s", n, q.to.Rel)
	}
	for p, used := range q.need {
		if used == n && p != ipath {
			return "", fmt.Errorf("name %s needed twice in %s", n, q.to.Rel)
		}
	}
	q.need[ipath] = n
	return n, nil
}

func dirOfPath(t *Tree, ipath string) string {
	if p := t.byPath[ipath]; p != nil {
		return p.Dir
	}
	return "\x00" // not in the module
}

// typeText renders a type expression from q.from for use in q.to.
func (q *qualifier) typeText(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.Ident:
		if _, ok := predeclared[x.Name]; ok || x.Name == "comparable" {
			return x.Name, nil
		}
		fromPkg := q.t.Pkgs[q.from.Dir]
		if fromPkg == nil {
			return "", fmt.Errorf("no package for %s", q.from.Dir)
		}
		name, err := q.pkgName(fromPkg.Path)
		if err != nil {
			return "", err
		}
		if name == "" {
			return x.Name, nil
		}
		if !Exported(x.Name) {
			return "", fmt.Errorf("unexported type %s", x.Name)
		}
		return name + "." + x.Name, nil
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("odd selector type")
		}
		ipath := importPathOf(q.from.AST, id.Name, q.t)
		if ipath == "" {
			return "", fmt.Errorf("unknown package %s in %s", id.Name, q.from.Rel)
		}
		name, err := q.pkgName(ipath)
		if err != nil {
			return "", err
		}
		if name == "" {
			return x.Sel.Name, nil
		}
		return name + "." + x.Sel.Name, nil
	case *ast.StarExpr:
		s, err := q.typeText(x.X)
		return "*" + s, err
	case *ast.ParenExpr:
		return q.typeText(x.X)
	case *ast.ArrayType:
		elem, err := q.typeText(x.Elt)
		if err != nil {
			return "", err
		}
		if x.Len == nil {
			return "[]" + elem, nil
		}
		if lit, ok := x.Len.(*ast.BasicLit); ok && lit.Kind == token.INT {
			return "[" + lit.Value + "]" + elem, nil
		}
		return "", fmt.Errorf("array length is not a literal")
	case *ast.MapType:
		k, err := q.typeText(x.Key)
		if err != nil {
			return "", err
		}
		v, err := q.typeText(x.Value)
		if err != nil {
			return "", err
		}
		return "map[" + k + "]" + v, nil
	case *ast.ChanType:
		v, err := q.typeText(x.Value)
		if err != nil {
			return "", err
		}
		switch x.Dir {
		case ast.SEND:
			return "chan<- " + v, nil
		case ast.RECV:
			return "<-chan " + v, nil
		}
		return "chan " + v, nil
	case *ast.FuncType:
		if x.TypeParams != nil {
			return "", fmt.Errorf("generic func type")
		}
		ps, err := q.fieldTypes(x.Params, true)
		if err != nil {
			return "", err
		}
		rs, err := q.fieldTypes(x.Results, false)
		if err != nil {
			return "", err
		}
		s := "func(" + strings.Join(ps, ", ") + ")"
		switch len(rs) {
		case 0:
		case 1:
			s += " " + rs[0]
		default:
			s += " (" + strings.Join(rs, ", ") + ")"
		}
		return s, nil
	case *ast.InterfaceType:
		if x.Methods == nil || len(x.Methods.List) == 0 {
			return "interface{}", nil
		}
		return "", fmt.Errorf("inline interface type")
	case *ast.StructType:
		if x.Fields == nil || len(x.Fields.List) == 0 {
			return "struct{}", nil
		}
		return "", fmt.Errorf("inline struct type")
	case *ast.Ellipsis:
		s, err := q.typeText(x.Elt)
		return "..." + s, err
	}
	return "", fmt.Errorf("unsupported type %T", e)
}

func (q *qualifier) fieldTypes(fl *ast.FieldList, params bool) ([]string, error) {
	if fl == nil {
		return nil, nil
	}
	var out []string
	for _, f := range fl.List {
		s, err := q.typeText(f.Type)
		if err != nil {
			return nil, err
		}
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, s)
		}
	}
	return out, nil
}

// zero renders a zero value of a parameter type for use in q.to.
func (q *qualifier) zero(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.Ident:
		if z, ok := predeclared[x.Name]; ok {
			return z, nil
		}
	case *ast.ParenExpr:
		return q.zero(x.X)
	case *ast.StarExpr, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return "nil", q.check(e)
	case *ast.ArrayType:
		if x.Len == nil {
			return "nil", q.check(e)
		}
	}
	s, err := q.typeText(e)
	if err != nil {
		return "", err
	}
	return "*new(" + s + ")", nil
}

// check reports whether a type could be written in q.to, without adding the
// imports writing it would need: a nil needs none.
func (q *qualifier) check(e ast.Expr) error {
	scratch := &qualifier{t: q.t, from: q.from, to: q.to, need: imports{}}
	for k, v := range q.need {
		scratch.need[k] = v
	}
	_, err := scratch.typeText(e)
	return err
}

// params lists a function's parameter types in order, and whether the last is
// variadic.
func params(ft *ast.FuncType) (types []ast.Expr, variadic bool) {
	if ft.Params == nil {
		return nil, false
	}
	for _, f := range ft.Params.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			types = append(types, f.Type)
		}
		if _, ok := f.Type.(*ast.Ellipsis); ok {
			variadic = true
		}
	}
	return types, variadic
}

func resultCount(ft *ast.FuncType) int {
	if ft.Results == nil {
		return 0
	}
	n := 0
	for _, f := range ft.Results.List {
		if len(f.Names) == 0 {
			n++
		} else {
			n += len(f.Names)
		}
	}
	return n
}

// callStmt renders a statement that calls fn from file to with zero-valued
// arguments, and the imports it needs.
func (t *Tree) callStmt(fn *Func, to *File) (string, imports, error) {
	if fn.Generic {
		return "", nil, fmt.Errorf("%s is generic", fn.Key)
	}
	from := t.Files[fn.File]
	q := &qualifier{t: t, from: from, to: to, need: imports{}}
	pkg := t.Pkgs[fn.Dir]
	if pkg == nil || pkg.Main && to.Dir != fn.Dir {
		return "", nil, fmt.Errorf("%s is in a main package", fn.Key)
	}
	pname, err := q.pkgName(pkg.Path)
	if err != nil {
		return "", nil, err
	}
	cross := pname != ""
	var callee string
	if fn.Recv == "" {
		if cross && !Exported(fn.Name) {
			return "", nil, fmt.Errorf("%s is unexported", fn.Key)
		}
		callee = fn.Name
		if cross {
			callee = pname + "." + fn.Name
		}
	} else {
		if cross && (!Exported(fn.Name) || !Exported(fn.Recv)) {
			return "", nil, fmt.Errorf("%s is unexported", fn.Key)
		}
		recv := fn.Recv
		if cross {
			recv = pname + "." + fn.Recv
		}
		callee = "new(" + recv + ")." + fn.Name
	}
	types, variadic := params(fn.Decl.Type)
	if variadic {
		types = types[:len(types)-1]
	}
	args := make([]string, 0, len(types))
	for _, pt := range types {
		z, err := q.zero(pt)
		if err != nil {
			return "", nil, err
		}
		args = append(args, z)
	}
	call := callee + "(" + strings.Join(args, ", ") + ")"
	switch n := resultCount(fn.Decl.Type); n {
	case 0:
		return call, q.need, nil
	default:
		blanks := strings.TrimSuffix(strings.Repeat("_, ", n), ", ")
		return blanks + " = " + call, q.need, nil
	}
}

// CanCall reports whether code in file can call fn without a new package
// dependency: same package, or a package the file's package already imports.
func (t *Tree) CanCall(fn *Func, file string) bool {
	to := t.Files[file]
	if to == nil || to.Test {
		return false
	}
	if to.Dir != fn.Dir {
		p := t.Pkgs[to.Dir]
		callee := t.Pkgs[fn.Dir]
		if p == nil || callee == nil || !p.Imports[callee.Path] {
			return false
		}
	}
	_, _, err := t.callStmt(fn, to)
	return err == nil
}
