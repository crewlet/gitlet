package sim

import (
	"fmt"
	"go/ast"
	"math"
	"math/rand/v2"
	"path"
	"sort"
	"strings"

	"github.com/crewlet/gitlet/bench/internal/goedit"
)

// Task is one unit of work an agent takes from the backlog.
type Task struct {
	ID      string      `json:"id"`
	Intent  string      `json:"intent"`
	Ops     []goedit.Op `json:"ops"`
	DupOf   string      `json:"dupOf,omitempty"`
	Files   []string    `json:"files"`   // files it plans to touch, at the base
	Symbols []string    `json:"symbols"` // function keys it changes
	Calls   []string    `json:"calls"`   // function keys its new code calls
}

// Mix is the probability of each op kind; they need not sum to one.
type Mix struct {
	BodyInsert, BodyWrap, BodyLiteral, AddFunc, AddCall, AddParam, Rename float64
}

// DefaultMix is the op mix of the main experiment: mostly body edits and new
// code, with one op in five a breaking signature change or a rename.
var DefaultMix = Mix{BodyInsert: 0.24, BodyWrap: 0.14, BodyLiteral: 0.10, AddFunc: 0.16, AddCall: 0.14, AddParam: 0.16, Rename: 0.06}

// GenConfig controls the backlog.
type GenConfig struct {
	Tasks     int
	Seed      uint64
	Skew      float64 // Zipf exponent for how often a file is a target
	Focus     float64 // share of targets drawn from the hottest functions
	HotSet    int     // how many functions count as the hottest
	DupRate   float64 // share of tasks that duplicate an earlier one
	DupWindow int     // how far back a duplicate's original may be
	Changelog bool    // every task also adds a CHANGELOG entry
	Mix       Mix
	Exclude   []string // directory prefixes no task targets
}

var purposes = []string{"input validation", "error handling", "logging", "metrics", "timeout handling", "nil checks", "tracing", "rate limiting", "bounds checks", "retry logic", "cancellation", "caching"}
var features = []string{"batch", "cached", "streaming", "summary", "debug", "export", "health check", "dry run", "audit", "snapshot"}
var flags = []string{"strict", "verbose", "dry-run", "force", "legacy", "lenient"}

// paraphrase pairs: the first form is used for an original, the second for
// a reworded duplicate.
func intentFor(op goedit.Op, words []string, alt bool, rng *rand.Rand, t *goedit.Tree) string {
	sym := human(op.Target)
	switch op.Kind {
	case goedit.BodyInsert, goedit.BodyWrap:
		p := words[0]
		if alt {
			return fmt.Sprintf("Make %s do proper %s", sym, p)
		}
		return fmt.Sprintf("Add %s to %s", p, sym)
	case goedit.BodyLiteral:
		if alt {
			return fmt.Sprintf("Reword the messages %s returns", sym)
		}
		return fmt.Sprintf("Improve error messages in %s", sym)
	case goedit.AddFunc:
		callee := sym
		if len(op.Callees) > 0 {
			callee = human(op.Callees[0])
		}
		if alt {
			return fmt.Sprintf("Provide a %s variant built on %s", words[0], callee)
		}
		return fmt.Sprintf("Add a %s helper that uses %s", words[0], callee)
	case goedit.AddCall:
		callee := ""
		if len(op.Callees) > 0 {
			callee = human(op.Callees[0])
		}
		if alt {
			return fmt.Sprintf("Have %s invoke %s", sym, callee)
		}
		return fmt.Sprintf("Call %s from %s", callee, sym)
	case goedit.AddParam:
		if alt {
			return fmt.Sprintf("Let callers ask %s for %s behaviour", sym, words[0])
		}
		return fmt.Sprintf("Add a %s option to %s", words[0], sym)
	case goedit.RenameFunc:
		_, name, _ := strings.Cut(op.Target, ":")
		if alt {
			return fmt.Sprintf("Give %s a clearer name", sym)
		}
		return fmt.Sprintf("Rename %s to %s", sym, goedit.RenamedName(name))
	}
	return "Change " + sym
}

// human renders a key as pkg.Recv.Name.
func human(key string) string {
	dir, name, _ := strings.Cut(key, ":")
	return path.Base(dir) + "." + name
}

type candidate struct {
	fn     *goedit.Func
	weight float64
}

// Generate builds a backlog against the tree at the base. Every task's ops
// are valid on the base when generated; callers still validate by building.
func Generate(t *goedit.Tree, cfg GenConfig) ([]*Task, error) {
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x9e3779b97f4a7c15))
	cands := targets(t, cfg.Exclude)
	if len(cands) < 20 {
		return nil, fmt.Errorf("only %d candidate functions", len(cands))
	}
	// Hotness: files ranked by size, with some noise, get Zipf weights;
	// within a file a function is picked by the length of its body. Churn
	// concentrates the same way in real code: a few large central files take
	// most of the changes.
	fileSize := map[string]float64{}
	for _, c := range cands {
		fileSize[c.fn.File] += bodyLen(c.fn) + 1
	}
	files := sortedKeys(func() map[string]bool {
		m := map[string]bool{}
		for f := range fileSize {
			m[f] = true
		}
		return m
	}())
	noise := map[string]float64{}
	for _, f := range files {
		noise[f] = 0.5 + rng.Float64()
	}
	sort.SliceStable(files, func(i, j int) bool {
		return fileSize[files[i]]*noise[files[i]] > fileSize[files[j]]*noise[files[j]]
	})
	fileWeight := map[string]float64{}
	totalFiles := 0.0
	for i, f := range files {
		fileWeight[f] = 1 / math.Pow(float64(i+1), cfg.Skew)
		totalFiles += fileWeight[f]
	}
	inFile := map[string][]candidate{}
	for _, c := range cands {
		inFile[c.fn.File] = append(inFile[c.fn.File], c)
	}
	// The hot set: the functions a backlog about one feature area keeps
	// coming back to.
	hot := append([]candidate(nil), cands...)
	sort.SliceStable(hot, func(i, j int) bool {
		return fileWeight[hot[i].fn.File]*bodyLen(hot[i].fn) > fileWeight[hot[j].fn.File]*bodyLen(hot[j].fn)
	})
	hot = hot[:min(len(hot), max(1, cfg.HotSet))]
	pick := func() *goedit.Func {
		if rng.Float64() < cfg.Focus {
			// Zipf over the hot set too.
			sum := 0.0
			for i := range hot {
				sum += 1 / float64(i+1)
			}
			y := rng.Float64() * sum
			for i := range hot {
				y -= 1 / float64(i+1)
				if y <= 0 {
					return hot[i].fn
				}
			}
			return hot[len(hot)-1].fn
		}
		x := rng.Float64() * totalFiles
		file := files[len(files)-1]
		for _, f := range files {
			x -= fileWeight[f]
			if x <= 0 {
				file = f
				break
			}
		}
		fs := inFile[file]
		sum := 0.0
		for _, c := range fs {
			sum += bodyLen(c.fn)
		}
		y := rng.Float64() * sum
		for _, c := range fs {
			y -= bodyLen(c.fn)
			if y <= 0 {
				return c.fn
			}
		}
		return fs[len(fs)-1].fn
	}
	byFile := map[string][]*goedit.Func{}
	byDir := map[string][]*goedit.Func{}
	for _, c := range cands {
		byFile[c.fn.File] = append(byFile[c.fn.File], c.fn)
		byDir[c.fn.Dir] = append(byDir[c.fn.Dir], c.fn)
	}
	sigOK := sigEligible(t)
	renameOK := renameEligible(t)

	var tasks []*Task
	for i := 0; len(tasks) < cfg.Tasks; i++ {
		if i > cfg.Tasks*50 {
			return nil, fmt.Errorf("could not generate %d tasks", cfg.Tasks)
		}
		id := fmt.Sprintf("t%02d", len(tasks)+1)
		if len(tasks) > 0 && rng.Float64() < cfg.DupRate {
			lo := max(0, len(tasks)-cfg.DupWindow)
			orig := tasks[lo+rng.IntN(len(tasks)-lo)]
			if orig.DupOf == "" {
				tasks = append(tasks, duplicate(orig, id, rng, t))
				continue
			}
		}
		primary := pick()
		nops := 1
		switch r := rng.Float64(); {
		case r < 0.20:
			nops = 1
		case r < 0.55:
			nops = 2
		case r < 0.80:
			nops = 3
		case r < 0.95:
			nops = 4
		default:
			nops = 5
		}
		task := &Task{ID: id}
		words := []string{}
		used := map[string]bool{}
		for k := 0; k < nops; k++ {
			fn := primary
			if k > 0 {
				switch r := rng.Float64(); {
				case r < 0.6 && len(byFile[primary.File]) > 1:
					fs := byFile[primary.File]
					fn = fs[rng.IntN(len(fs))]
				case r < 0.85 && len(byDir[primary.Dir]) > 1:
					fs := byDir[primary.Dir]
					fn = fs[rng.IntN(len(fs))]
				default:
					fn = pick()
				}
			}
			tag := fmt.Sprintf("%s%c", id, 'a'+k)
			op, w := makeOp(t, fn, tag, cfg.Mix, rng, pick, sigOK, renameOK, used)
			if op.Kind == "" {
				continue
			}
			used[string(op.Kind)+op.Target] = true
			task.Ops = append(task.Ops, op)
			words = append(words, w)
		}
		if len(task.Ops) == 0 {
			continue
		}
		task.Intent = intentFor(task.Ops[0], words[:1], false, rng, t)
		if cfg.Changelog {
			task.Ops = append(task.Ops, goedit.Op{Kind: goedit.Changelog, Tag: id + "z", Text: task.Intent})
		}
		plan(t, task)
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func duplicate(orig *Task, id string, rng *rand.Rand, t *goedit.Tree) *Task {
	d := &Task{ID: id, DupOf: orig.ID, Files: orig.Files, Symbols: orig.Symbols, Calls: orig.Calls}
	// Two agents doing the same task write different code: their own
	// names, parameters and statements, at the same places.
	for k, op := range orig.Ops {
		op.Variant = 1
		op.Tag = fmt.Sprintf("%s%c", id, 'a'+k)
		if op.Kind == goedit.AddFunc && strings.Contains(op.File, orig.ID) {
			op.File = strings.ReplaceAll(op.File, orig.ID, id)
		}
		d.Ops = append(d.Ops, op)
	}
	if rng.Float64() < 0.5 {
		d.Intent = orig.Intent
	} else {
		w := wordsOf(orig.Intent)
		d.Intent = intentFor(orig.Ops[0], []string{w}, true, rng, t)
	}
	d.Files = nil
	plan(t, d)
	return d
}

// wordsOf recovers the purpose word an intent was built from.
func wordsOf(intent string) string {
	for _, list := range [][]string{purposes, features, flags} {
		for _, w := range list {
			if strings.Contains(intent, " "+w+" ") {
				return w
			}
		}
	}
	return purposes[0]
}

func bodyLen(fn *goedit.Func) float64 { return float64(len(fn.Decl.Body.List)) }

func targets(t *goedit.Tree, exclude []string) []candidate {
	var out []candidate
	for _, fn := range t.Funcs() {
		pkg := t.Pkgs[fn.Dir]
		if pkg == nil || pkg.Main || fn.Generic || len(fn.Decl.Body.List) < 2 {
			continue
		}
		skip := false
		for _, ex := range exclude {
			if fn.Dir == ex || strings.HasPrefix(fn.Dir, ex+"/") {
				skip = true
			}
		}
		if !skip {
			out = append(out, candidate{fn: fn})
		}
	}
	return out
}

// Methods that probably satisfy an interface outside the module.
var wellKnown = map[string]bool{"String": true, "Error": true, "Write": true, "Read": true, "Close": true, "ServeHTTP": true, "MarshalJSON": true, "UnmarshalJSON": true, "Len": true, "Less": true, "Swap": true, "Unwrap": true, "Is": true, "As": true, "Format": true, "RoundTrip": true, "Token": true, "Next": true, "Err": true, "Done": true, "Value": true, "Deadline": true}

// sigEligible lists functions whose callers this package can find and fix:
// functions, and methods whose name nothing else in the module uses.
func sigEligible(t *goedit.Tree) map[string]bool {
	names := map[string]int{}    // method and interface-method names
	nonCall := map[string]bool{} // names used other than as a call
	for _, rel := range t.FilePaths() {
		f := t.Files[rel]
		calls := map[ast.Expr]bool{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Recv != nil {
					names[x.Name.Name]++
				}
			case *ast.InterfaceType:
				for _, m := range x.Methods.List {
					for _, nm := range m.Names {
						names[nm.Name] += 2
					}
				}
			case *ast.StructType:
				for _, fl := range x.Fields.List {
					for _, nm := range fl.Names {
						names[nm.Name] += 2
					}
				}
			case *ast.CallExpr:
				calls[x.Fun] = true
			case *ast.SelectorExpr:
				if !calls[x] {
					nonCall[x.Sel.Name] = true
				}
			case *ast.Ident:
				if !calls[x] {
					nonCall[x.Name] = true
				}
			}
			return true
		})
	}
	out := map[string]bool{}
	for _, fn := range t.Funcs() {
		types, variadic := paramsOf(fn)
		if variadic || fn.Generic || len(types) > 6 || wellKnown[fn.Name] {
			continue
		}
		if fn.Recv != "" && names[fn.Name] != 1 {
			continue
		}
		if nonCallUse(t, fn) || fn.Recv != "" && nonCall[fn.Name] && nonCallSelector(t, fn.Name) {
			continue
		}
		out[fn.Key] = true
	}
	return out
}

func paramsOf(fn *goedit.Func) ([]ast.Expr, bool) {
	var types []ast.Expr
	variadic := false
	for _, f := range fn.Decl.Type.Params.List {
		n := max(1, len(f.Names))
		for range n {
			types = append(types, f.Type)
		}
		if _, ok := f.Type.(*ast.Ellipsis); ok {
			variadic = true
		}
	}
	return types, variadic
}

// nonCallUse reports whether a package function is used as a value
// anywhere, which adding a parameter would break in ways callers cannot fix
// by passing an argument.
func nonCallUse(t *goedit.Tree, fn *goedit.Func) bool {
	if fn.Recv != "" {
		return false
	}
	pkg := t.Pkgs[fn.Dir]
	found := false
	for _, rel := range t.FilePaths() {
		f := t.Files[rel]
		same := f.Dir == fn.Dir && !strings.HasSuffix(f.AST.Name.Name, "_test")
		alias := ""
		if !same {
			for _, im := range f.AST.Imports {
				if strings.Trim(im.Path.Value, `"`) == pkg.Path {
					alias = pkg.Name
					if im.Name != nil {
						alias = im.Name.Name
					}
				}
			}
			if alias == "" {
				continue
			}
		}
		called := map[ast.Node]bool{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				called[x.Fun] = true
			case *ast.FuncDecl:
				called[x.Name] = true
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && !same && id.Name == alias && x.Sel.Name == fn.Name && !called[x] {
					found = true
				}
				called[x.Sel] = true
			case *ast.Ident:
				if same && x.Name == fn.Name && !called[x] {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func nonCallSelector(t *goedit.Tree, name string) bool {
	found := false
	for _, rel := range t.FilePaths() {
		f := t.Files[rel]
		called := map[ast.Node]bool{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				called[x.Fun] = true
			case *ast.SelectorExpr:
				if x.Sel.Name == name && !called[x] {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func renameEligible(t *goedit.Tree) map[string]bool {
	out := map[string]bool{}
	taken := map[string]bool{}
	for _, fn := range t.Funcs() {
		taken[fn.Dir+":"+fn.Name] = true
	}
	for _, fn := range t.Funcs() {
		if fn.Recv != "" || !goedit.Exported(fn.Name) || fn.Generic || taken[fn.Dir+":"+goedit.RenamedName(fn.Name)] {
			continue
		}
		out[fn.Key] = true
	}
	return out
}

// makeOp picks an op kind for fn and fills it in. It returns a zero op when
// nothing fits.
func makeOp(t *goedit.Tree, fn *goedit.Func, tag string, mix Mix, rng *rand.Rand, pick func() *goedit.Func, sigOK, renameOK map[string]bool, used map[string]bool) (goedit.Op, string) {
	sum := mix.BodyInsert + mix.BodyWrap + mix.BodyLiteral + mix.AddFunc + mix.AddCall + mix.AddParam + mix.Rename
	r := rng.Float64() * sum
	kind := goedit.BodyInsert
	for _, c := range []struct {
		k goedit.Kind
		p float64
	}{{goedit.BodyInsert, mix.BodyInsert}, {goedit.BodyWrap, mix.BodyWrap}, {goedit.BodyLiteral, mix.BodyLiteral}, {goedit.AddFunc, mix.AddFunc}, {goedit.AddCall, mix.AddCall}, {goedit.AddParam, mix.AddParam}, {goedit.RenameFunc, mix.Rename}} {
		if r < c.p {
			kind = c.k
			break
		}
		r -= c.p
	}
	word := purposes[rng.IntN(len(purposes))]
	anchor := rng.Float64()
	switch kind {
	case goedit.AddParam:
		if !sigOK[fn.Key] || used["add_param"+fn.Key] || used["rename_func"+fn.Key] {
			kind = goedit.BodyInsert
		} else {
			return goedit.Op{Kind: kind, Target: fn.Key, Tag: tag}, flags[rng.IntN(len(flags))]
		}
	case goedit.RenameFunc:
		if !renameOK[fn.Key] || used["add_param"+fn.Key] || used["rename_func"+fn.Key] {
			kind = goedit.BodyInsert
		} else {
			return goedit.Op{Kind: kind, Target: fn.Key, Tag: tag, NewName: goedit.RenamedName(fn.Name)}, ""
		}
	case goedit.AddFunc:
		file := fn.File
		if rng.Float64() < 0.5 {
			file = path.Join(fn.Dir, tag[:3]+"_"+strings.ReplaceAll(features[rng.IntN(len(features))], " ", "_")+".go")
			if t.Files[file] != nil {
				file = fn.File
			}
		}
		callees := []string{fn.Key}
		for range rng.IntN(3) {
			if c := pick(); c.Key != fn.Key && callableFrom(t, c, fn.File) {
				callees = append(callees, c.Key)
			}
		}
		if !callableFrom(t, fn, fn.File) {
			callees = callees[1:]
		}
		if len(callees) == 0 {
			kind = goedit.BodyInsert
			break
		}
		return goedit.Op{Kind: kind, Target: fn.Key, File: file, Callees: callees, Size: 1 + rng.IntN(3), Tag: tag}, features[rng.IntN(len(features))]
	case goedit.AddCall:
		var callee *goedit.Func
		for range 12 {
			if c := pick(); c.Key != fn.Key && callableFrom(t, c, fn.File) {
				callee = c
				break
			}
		}
		if callee == nil {
			kind = goedit.BodyInsert
			break
		}
		return goedit.Op{Kind: kind, Target: fn.Key, Callees: []string{callee.Key}, Anchor: anchor, Tag: tag}, word
	case goedit.BodyLiteral:
		return goedit.Op{Kind: kind, Target: fn.Key, Anchor: anchor, Size: 1 + rng.IntN(2), Tag: tag}, word
	case goedit.BodyWrap:
		return goedit.Op{Kind: kind, Target: fn.Key, Anchor: anchor, Size: 2 + rng.IntN(5), Tag: tag}, word
	}
	return goedit.Op{Kind: goedit.BodyInsert, Target: fn.Key, Anchor: anchor, Size: 1 + rng.IntN(5), Tag: tag}, word
}

func callableFrom(t *goedit.Tree, fn *goedit.Func, file string) bool {
	if fn.Dir != t.Files[file].Dir && !goedit.Exported(fn.Name) {
		return false
	}
	return t.CanCall(fn, file)
}

// plan fills in the files and symbols a task touches at the base.
func plan(t *goedit.Tree, task *Task) {
	files := map[string]bool{}
	syms := map[string]bool{}
	calls := map[string]bool{}
	for _, op := range task.Ops {
		switch op.Kind {
		case goedit.Changelog:
			files["CHANGELOG.md"] = true
			continue
		case goedit.AddFunc:
			files[op.File] = true
		}
		for _, c := range op.Callees {
			calls[c] = true
		}
		fn := t.Func(op.Target)
		if fn == nil {
			continue
		}
		if op.Kind != goedit.AddFunc {
			files[fn.File] = true
			syms[fn.Key] = true
		}
		if op.Kind == goedit.AddParam || op.Kind == goedit.RenameFunc {
			for _, f := range goedit.CallerFiles(t, fn) {
				files[f] = true
			}
		}
	}
	task.Files = sortedKeys(files)
	task.Symbols = sortedKeys(syms)
	task.Calls = sortedKeys(calls)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
