package symbols

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/crewlet/gitlet/internal/model"
)

// language is one supported grammar with its extractor and a small pool of
// parsers. tree-sitter objects have no finalizers, so parsers beyond the pool
// size are closed rather than left to the garbage collector.
type language struct {
	name    string
	lang    *sitter.Language
	extract func(b *builder, root *sitter.Node)
	pool    chan *sitter.Parser
}

const parserPoolSize = 4

var languages = map[string]*language{
	"go":         newLanguage("go", sitter.NewLanguage(tsgo.Language()), extractGo),
	"typescript": newLanguage("typescript", sitter.NewLanguage(tsts.LanguageTypescript()), extractTypeScript),
	"tsx":        newLanguage("tsx", sitter.NewLanguage(tsts.LanguageTSX()), extractTypeScript),
	"javascript": newLanguage("javascript", sitter.NewLanguage(tsjs.Language()), extractTypeScript),
	"python":     newLanguage("python", sitter.NewLanguage(tspy.Language()), extractPython),
	"rust":       newLanguage("rust", sitter.NewLanguage(tsrust.Language()), extractRust),
}

func newLanguage(name string, l *sitter.Language, fn func(*builder, *sitter.Node)) *language {
	return &language{name: name, lang: l, extract: fn, pool: make(chan *sitter.Parser, parserPoolSize)}
}

func (l *language) parser() (*sitter.Parser, error) {
	select {
	case p := <-l.pool:
		return p, nil
	default:
	}
	p := sitter.NewParser()
	if err := p.SetLanguage(l.lang); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (l *language) release(p *sitter.Parser) {
	p.Reset()
	select {
	case l.pool <- p:
	default:
		p.Close()
	}
}

var extLang = map[string]string{
	".go":  "go",
	".ts":  "typescript",
	".mts": "typescript",
	".cts": "typescript",
	".tsx": "tsx",
	".js":  "javascript",
	".mjs": "javascript",
	".cjs": "javascript",
	".jsx": "javascript",
	".py":  "python",
	".pyi": "python",
	".rs":  "rust",
}

func langForPath(p string) string { return extLang[strings.ToLower(path.Ext(p))] }

func extract(p string, src []byte) (f *File, err error) {
	name := langForPath(p)
	if name == "" {
		return nil, nil
	}
	l := languages[name]
	parser, err := l.parser()
	if err != nil {
		return nil, fmt.Errorf("symbols: %s: %w", p, err)
	}
	defer l.release(parser)
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("symbols: %s: parse failed", p)
	}
	defer tree.Close()

	b := &builder{lang: name, path: p, src: src, f: &File{Lang: name}, used: map[string]int{}}
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("symbols: %s: extractor panic: %v", p, r)
		}
	}()
	l.extract(b, tree.RootNode())
	b.finish()
	return b.f, nil
}

// builder collects one file's symbols, refs and imports. Language extractors
// call Add, Ref and Import; the builder computes keys, unique qualified names,
// hashes and ordering so every language behaves the same way.
type builder struct {
	lang string
	path string
	src  []byte
	f    *File
	used map[string]int
}

// Def describes one definition for builder.Add.
type Def struct {
	// Node is the whole definition. Its byte range and lines are recorded and
	// its tokens form the body hash. Include decorators, attributes and the
	// export keyword when they belong to the definition.
	Node      *sitter.Node
	Name      string
	Container string // receiver, class, impl or trait type; "" for top level
	Kind      string // function, method, class, struct, interface, type, enum, trait, const, var
	Exported  bool
	// Suffix disambiguates definitions that share a qualified name on purpose,
	// such as "@setter" for a Python property setter or "@Display" for a Rust
	// trait impl method. Remaining duplicates get "#2", "#3", ... in order.
	Suffix string
	// Signature is the normalized structural signature (docs/SPEC.md,
	// section 9.3). It must leave out parameter names (except in Python),
	// receiver names, comments and decorators.
	Signature string
	Params    []model.Param
	Results   string
	Members   []model.Member
	// Nested are definitions inside Node (methods of a class, items of an
	// impl). Their tokens are left out of this definition's body hash, so
	// editing one method does not change its class.
	Nested []*sitter.Node
}

// Add records a definition and returns the symbol as stored.
func (b *builder) Add(d Def) model.Symbol {
	qualified := d.Name
	if d.Container != "" {
		qualified = d.Container + "." + d.Name
	}
	qualified += d.Suffix
	if n := b.used[qualified]; n > 0 {
		b.used[qualified] = n + 1
		qualified += "#" + strconv.Itoa(n+1)
	} else {
		b.used[qualified] = 1
	}
	start, end := d.Node.ByteRange()
	s := model.Symbol{
		Key:       model.SymbolKey(b.lang, b.path, qualified),
		Lang:      b.lang,
		Path:      b.path,
		Name:      d.Name,
		Qualified: qualified,
		Container: d.Container,
		Kind:      d.Kind,
		Signature: d.Signature,
		SigHash:   SigHash(d.Kind + "\x00" + d.Signature),
		BodyHash:  BodyHash(b.bodyTokens(d.Node, d.Nested, d.Name)),
		Params:    d.Params,
		Results:   d.Results,
		Members:   d.Members,
		StartLine: int(d.Node.StartPosition().Row) + 1,
		EndLine:   int(d.Node.EndPosition().Row) + 1,
		StartByte: int(start),
		EndByte:   int(end),
		Exported:  d.Exported,
	}
	b.f.Symbols = append(b.f.Symbols, s)
	return s
}

// Ref records a reference at node n. Empty names are ignored.
func (b *builder) Ref(name, qualifier string, kind model.RefKind, n *sitter.Node) {
	if name == "" {
		return
	}
	b.f.Refs = append(b.f.Refs, model.Ref{
		Name:      name,
		Qualifier: qualifier,
		Kind:      kind,
		Lang:      b.lang,
		Path:      b.path,
		Line:      Line(n),
	})
}

// Import records an import binding at node n.
func (b *builder) Import(im Import, n *sitter.Node) {
	im.Line = Line(n)
	b.f.Imports = append(b.f.Imports, im)
}

// SetPackage records the Go package name.
func (b *builder) SetPackage(name string) { b.f.Package = name }

// Text returns the source text of n.
func (b *builder) Text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Utf8Text(b.src)
}

// Norm returns n's leaf tokens without comments, joined with a space only
// between two word characters: "map[string]int", "*Config",
// "Result<Config,Error>", "impl Trait". Use it for types and other pieces of
// signatures so formatting never matters.
func (b *builder) Norm(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	var sb strings.Builder
	b.leaves(n, nil, func(tok string) { joinToken(&sb, tok) })
	return sb.String()
}

// NormSkip is Norm leaving out any subtree for which skip returns true.
func (b *builder) NormSkip(n *sitter.Node, skip func(*sitter.Node) bool) string {
	if n == nil {
		return ""
	}
	var sb strings.Builder
	b.leaves(n, skip, func(tok string) { joinToken(&sb, tok) })
	return sb.String()
}

// bodyTokens returns the tokens the body hash covers. Nested definitions
// become a placeholder, the first occurrence of the definition's own name is
// left out (so a renamed symbol keeps its body hash and can be paired), and
// tokens that formatters add or remove without changing meaning are dropped:
// statement semicolons in Go, TypeScript and JavaScript, and a comma right
// before a closing bracket. Python's indentation tokens have no text, so each
// block is wrapped in markers instead: moving a statement into or out of a
// block changes the hash, reindenting does not.
func (b *builder) bodyTokens(n *sitter.Node, nested []*sitter.Node, name string) string {
	type span struct{ start, end uint }
	skip := make(map[span]bool, len(nested))
	for _, x := range nested {
		if x != nil {
			s, e := x.ByteRange()
			skip[span{s, e}] = true
		}
	}
	var toks []string
	b.tokens(n, func(c *sitter.Node) bool {
		if len(skip) == 0 || c.Id() == n.Id() {
			return false
		}
		s, e := c.ByteRange()
		if skip[span{s, e}] {
			toks = append(toks, "\x01")
			return true
		}
		return false
	}, func(tok string) { toks = append(toks, tok) }, b.lang == "python")

	dropSemicolons := optionalSemicolons(b.lang)
	var sb strings.Builder
	for i, tok := range toks {
		switch {
		case name != "" && tok == name:
			name = ""
			continue
		case tok == ";" && dropSemicolons:
			continue
		case tok == "," && i+1 < len(toks) && isClosing(toks[i+1]):
			continue
		}
		sb.WriteString(tok)
		sb.WriteByte(' ')
	}
	return sb.String()
}

// optionalSemicolons reports whether formatters insert or remove statement
// semicolons freely in lang (gofmt, and ASI in JavaScript and TypeScript).
func optionalSemicolons(lang string) bool {
	switch lang {
	case "go", "typescript", "tsx", "javascript":
		return true
	}
	return false
}

func isClosing(tok string) bool {
	switch tok {
	case ")", "]", "}", ">":
		return true
	}
	return false
}

// leaves calls emit for every leaf token under n in source order, dropping
// comments. String-like nodes are emitted whole so their inner spacing counts.
func (b *builder) leaves(n *sitter.Node, skip func(*sitter.Node) bool, emit func(string)) {
	b.tokens(n, skip, emit, false)
}

// Markers around the statements of a Python block in body tokens. They
// cannot occur in source text.
const (
	blockOpen  = "\x02"
	blockClose = "\x03"
)

// tokens is leaves, also emitting blockOpen and blockClose around the
// children of every Python block when blocks is set.
func (b *builder) tokens(n *sitter.Node, skip func(*sitter.Node) bool, emit func(string), blocks bool) {
	if n == nil {
		return
	}
	if skip != nil && skip(n) {
		return
	}
	kind := n.Kind()
	if IsComment(n) {
		return
	}
	if n.ChildCount() == 0 || isStringLike(kind) {
		if t := n.Utf8Text(b.src); t != "" {
			emit(t)
		}
		return
	}
	block := blocks && kind == "block"
	if block {
		emit(blockOpen)
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		b.tokens(n.Child(i), skip, emit, blocks)
	}
	if block {
		emit(blockClose)
	}
}

func isStringLike(kind string) bool {
	switch kind {
	case "string", "string_literal", "raw_string_literal", "interpreted_string_literal",
		"template_string", "char_literal", "rune_literal", "concatenated_string":
		return true
	}
	return false
}

// IsComment reports whether n is a comment node in any supported grammar.
func IsComment(n *sitter.Node) bool {
	switch n.Kind() {
	case "comment", "line_comment", "block_comment", "doc_comment", "html_comment":
		return true
	}
	return false
}

func joinToken(sb *strings.Builder, tok string) {
	if sb.Len() > 0 {
		prev := sb.String()
		last, _ := utf8.DecodeLastRuneInString(prev)
		first, _ := utf8.DecodeRuneInString(tok)
		if isWord(last) && isWord(first) {
			sb.WriteByte(' ')
		}
	}
	sb.WriteString(tok)
}

func isWord(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func (b *builder) finish() {
	sort.SliceStable(b.f.Symbols, func(i, j int) bool {
		if b.f.Symbols[i].StartByte != b.f.Symbols[j].StartByte {
			return b.f.Symbols[i].StartByte < b.f.Symbols[j].StartByte
		}
		return b.f.Symbols[i].Qualified < b.f.Symbols[j].Qualified
	})
	sort.SliceStable(b.f.Refs, func(i, j int) bool {
		a, c := b.f.Refs[i], b.f.Refs[j]
		if a.Line != c.Line {
			return a.Line < c.Line
		}
		if a.Name != c.Name {
			return a.Name < c.Name
		}
		if a.Qualifier != c.Qualifier {
			return a.Qualifier < c.Qualifier
		}
		return a.Kind < c.Kind
	})
	refs := b.f.Refs[:0]
	for i, r := range b.f.Refs {
		if i > 0 && r == b.f.Refs[i-1] {
			continue
		}
		refs = append(refs, r)
	}
	b.f.Refs = refs
	sort.SliceStable(b.f.Imports, func(i, j int) bool { return b.f.Imports[i].Line < b.f.Imports[j].Line })
}

// Line returns the 1-based line where n starts.
func Line(n *sitter.Node) int {
	if n == nil {
		return 0
	}
	return int(n.StartPosition().Row) + 1
}

// Walk visits n and its descendants in pre-order. fn returns false to skip a
// node's children.
func Walk(n *sitter.Node, fn func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !fn(n) {
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		Walk(n.NamedChild(i), fn)
	}
}

// Named returns n's named children.
func Named(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, n.NamedChildCount())
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && !IsComment(c) {
			out = append(out, c)
		}
	}
	return out
}

// Field returns n's child for a field name, or nil.
func Field(n *sitter.Node, name string) *sitter.Node {
	if n == nil {
		return nil
	}
	return n.ChildByFieldName(name)
}

// Fields returns every child of n under a field name, in order.
func Fields(n *sitter.Node, name string) []*sitter.Node {
	if n == nil {
		return nil
	}
	c := n.Walk()
	defer c.Close()
	nodes := n.ChildrenByFieldName(name, c)
	out := make([]*sitter.Node, len(nodes))
	for i := range nodes {
		out[i] = &nodes[i]
	}
	return out
}

// HasChild reports whether n has a direct child (named or not) of the kind.
func HasChild(n *sitter.Node, kind string) bool {
	if n == nil {
		return false
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Kind() == kind {
			return true
		}
	}
	return false
}

// ChildOfKind returns n's first direct child of the kind, or nil.
func ChildOfKind(n *sitter.Node, kind string) *sitter.Node {
	if n == nil {
		return nil
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Kind() == kind {
			return c
		}
	}
	return nil
}
