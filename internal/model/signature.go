package model

import "strings"

// SameSignature reports whether proposal, a signature an agent wrote by hand
// in a proposal, describes the same interface as decl, a normalized signature
// in lang (docs/SPEC.md, section 9.2). It forgives what a hand-written
// signature spells differently: whitespace, a trailing semicolon, colon or
// brace, leading keywords, the receiver or container, parameter names (except
// in Python, where callers pass them as keywords) and default values. The
// proposal may also leave out the name, the type parameters and a Rust where
// clause. What remains is compared: the name, the parameter types and what
// follows the parameters.
func SameSignature(lang, decl, proposal string) bool {
	a, b := shapeOf(lang, decl), shapeOf(lang, proposal)
	if a.text == b.text {
		return true
	}
	if !a.function || !b.function {
		return sameSuffix(a.text, b.text)
	}
	return (b.name == "" || a.name == b.name) &&
		(b.typeParams == "" || a.typeParams == b.typeParams) &&
		a.params == b.params && a.tail == b.tail &&
		(b.where == "" || a.where == b.where)
}

// sigShape is a signature taken apart for SameSignature. Every part is
// normalized: no parameter names, no default values, minimal whitespace.
type sigShape struct {
	text       string // the whole signature
	function   bool   // a parameter list was found
	name       string // the last identifier before the parameters
	typeParams string // "<T>" or "[T any]" right before the parameters
	params     string
	tail       string // results or return type
	where      string // a Rust where clause
}

func shapeOf(lang, s string) sigShape {
	s = strings.TrimSpace(s)
	for s != "" && strings.ContainsRune(";:{", rune(s[len(s)-1])) {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	sh := sigShape{text: normText(lang, s)}
	open, end := paramGroup(s)
	if open < 0 {
		return sh
	}
	head := strings.TrimRight(s[:open], ":= \t") // "const f: (" and "const f = ("
	if g := lastGroup(head); g >= 0 {
		sh.typeParams = normText(lang, head[g:])
		head = strings.TrimSpace(head[:g])
	}
	if i := strings.LastIndexByte(head, '@'); i >= 0 { // Rust trait or Python accessor disambiguator
		head = head[:i]
	}
	i := len(head)
	for i > 0 && identByte(head[i-1]) {
		i--
	}
	tail := s[end+1:]
	if lang == "rust" {
		if i := topWord(tail, "where"); i >= 0 {
			tail, sh.where = tail[:i], normText(lang, tail[i:])
		}
	}
	sh.function, sh.name = true, head[i:]
	sh.params = normList(lang, s[open+1:end], true)
	sh.tail = normText(lang, tail)
	return sh
}

// paramGroup finds the parameter list: the first top-level parenthesis
// written right after a name, type parameters or "function*", else the first
// top-level parenthesis. A Go receiver comes before the name with a space, so
// it is skipped.
func paramGroup(s string) (open, end int) {
	open, end = -1, -1
	depth := 0
	for i := 0; i < len(s); i++ {
		if depth == 0 && s[i] == '(' {
			j := matching(s, i)
			if j < 0 {
				return -1, -1
			}
			if i > 0 && (identByte(s[i-1]) || strings.IndexByte(">]*", s[i-1]) >= 0) {
				return i, j
			}
			if open < 0 {
				open, end = i, j
			}
			i = j
			continue
		}
		depth += bracket(s, i)
	}
	return open, end
}

// lastGroup returns where a "<...>" or "[...]" group that ends s starts, or
// -1.
func lastGroup(s string) int {
	if s == "" || (s[len(s)-1] != '>' && s[len(s)-1] != ']') {
		return -1
	}
	depth, start := 0, -1
	for i := 0; i < len(s); i++ {
		d := bracket(s, i)
		if depth == 0 && d > 0 {
			start = i
		}
		depth += d
	}
	if depth != 0 || start < 0 || (s[start] != '<' && s[start] != '[') {
		return -1
	}
	return start
}

// normList normalizes the inside of a parenthesized list: each element in
// the language's form without names, joined by commas. top is the parameter
// list itself, where Python drops self and cls.
func normList(lang, inner string, top bool) string {
	var segs []string
	for _, seg := range splitTop(inner, ',') {
		if seg = strings.TrimSpace(seg); seg != "" {
			segs = append(segs, seg)
		}
	}
	switch lang {
	case "go":
		segs = goParamTypes(segs)
	case "python":
		segs = pyParams(segs, top)
	default:
		for i, seg := range segs {
			segs[i] = paramType(lang, seg)
		}
	}
	for i, seg := range segs {
		segs[i] = normText(lang, seg)
	}
	return strings.Join(segs, ",")
}

// goTypeWords start a Go type that has a space in it.
var goTypeWords = map[string]bool{"chan": true, "func": true, "map": true, "struct": true, "interface": true}

// goParamTypes drops Go parameter names. Either every parameter has a name or
// none has, and in "a, b int" the bare name a takes the type of the next
// named parameter.
func goParamTypes(segs []string) []string {
	names := make([]bool, len(segs))
	types := make([]string, len(segs))
	named := false
	for i, seg := range segs {
		first, rest, ok := cutTop(seg, ' ')
		if ok && isIdent(first) && !goTypeWords[first] {
			names[i], types[i], named = true, strings.TrimSpace(rest), true
		}
	}
	if !named {
		return segs
	}
	out := make([]string, len(segs))
	typ := ""
	for i := len(segs) - 1; i >= 0; i-- {
		if names[i] {
			typ = types[i]
		}
		out[i] = typ
	}
	return out
}

// pyParams keeps Python parameter names, which are part of the interface,
// and writes every default value as "?", as the normalized signature does.
func pyParams(segs []string, top bool) []string {
	var out []string
	for i, seg := range segs {
		if l, _, ok := cutTop(seg, '='); ok {
			seg = strings.TrimSpace(l) + "=?"
		}
		name := seg
		if l, _, ok := cutTop(name, ':'); ok {
			name = l
		}
		if l, _, ok := cutTop(name, '='); ok {
			name = l
		}
		if name = strings.TrimSpace(name); top && i == 0 && (name == "self" || name == "cls") {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// paramType reduces one TypeScript, JavaScript or Rust parameter to its type,
// with TypeScript's "T?" and "...T" markers.
func paramType(lang, seg string) string {
	if lang == "rust" {
		if _, typ, ok := cutTop(seg, ':'); ok {
			return typ
		}
		return seg
	}
	optional := false
	if l, _, ok := cutTop(seg, '='); ok {
		seg, optional = strings.TrimSpace(l), true
	}
	name, typ, typed := cutTop(seg, ':')
	if !typed {
		if lang != "javascript" { // a type without a name, or a name without a type
			if optional {
				return "?"
			}
			return seg
		}
		switch { // JavaScript has no types: "_", "?" or "..._"
		case strings.HasPrefix(seg, "..."):
			return "..._"
		case optional || strings.HasSuffix(seg, "?"):
			return "?"
		}
		return "_"
	}
	name, typ = strings.TrimSpace(name), strings.TrimSpace(typ)
	switch {
	case strings.HasPrefix(name, "..."):
		return "..." + typ
	case optional || strings.HasSuffix(name, "?"):
		return typ + "?"
	}
	return typ
}

// normText rewrites every parenthesized list in s with normList and squeezes
// whitespace: none next to punctuation, one space between words.
func normText(lang, s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '(' {
			if j := matching(s, i); j > 0 {
				sb.WriteString("(" + normList(lang, s[i+1:j], false) + ")")
				i = j
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	words := strings.Fields(sb.String())
	var out strings.Builder
	for i, w := range words {
		if i > 0 && identByte(words[i-1][len(words[i-1])-1]) && identByte(w[0]) {
			out.WriteByte(' ')
		}
		out.WriteString(w)
	}
	return out.String()
}

// sameSuffix reports whether one normalized text ends with the other at a
// word boundary, so a proposal may leave out leading keywords.
func sameSuffix(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	if b == "" || !strings.HasSuffix(a, b) {
		return false
	}
	k := len(a) - len(b)
	return k == 0 || !identByte(a[k-1]) || !identByte(b[0])
}

// bracket returns +1 for an opening bracket at s[i], -1 for a closing one and
// 0 otherwise. The arrows "->", "=>" and "<-" are not brackets.
func bracket(s string, i int) int {
	switch s[i] {
	case '(', '[', '{':
		return 1
	case ')', ']', '}':
		return -1
	case '<':
		if i+1 < len(s) && s[i+1] == '-' {
			return 0
		}
		return 1
	case '>':
		if i > 0 && (s[i-1] == '-' || s[i-1] == '=') {
			return 0
		}
		return -1
	}
	return 0
}

// matching returns the index of the bracket closing the one at s[open], or
// -1.
func matching(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		depth += bracket(s, i)
		if depth == 0 {
			return i
		}
		if depth < 0 {
			return -1
		}
	}
	return -1
}

// splitTop splits s at sep outside brackets.
func splitTop(s string, sep byte) []string {
	var parts []string
	depth, from := 0, 0
	for i := 0; i < len(s); i++ {
		if depth == 0 && s[i] == sep {
			parts = append(parts, s[from:i])
			from = i + 1
			continue
		}
		depth += bracket(s, i)
	}
	return append(parts, s[from:])
}

// cutTop cuts s around the first sep outside brackets. A colon in "::" and an
// equals sign in "==", "=>", "<=", ">=" or "!=" do not count.
func cutTop(s string, sep byte) (before, after string, found bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		if depth == 0 && s[i] == sep && !partOfOperator(s, i) {
			return s[:i], s[i+1:], true
		}
		depth += bracket(s, i)
	}
	return s, "", false
}

func partOfOperator(s string, i int) bool {
	prev, next := byte(0), byte(0)
	if i > 0 {
		prev = s[i-1]
	}
	if i+1 < len(s) {
		next = s[i+1]
	}
	switch s[i] {
	case ':':
		return prev == ':' || next == ':'
	case '=':
		return next == '=' || next == '>' || prev == '=' || strings.IndexByte("<>!", prev) >= 0
	}
	return false
}

// topWord returns where the word w starts outside brackets in s, or -1.
func topWord(s, w string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		if depth == 0 && strings.HasPrefix(s[i:], w) &&
			(i == 0 || !identByte(s[i-1])) && (i+len(w) == len(s) || !identByte(s[i+len(w)])) {
			return i
		}
		depth += bracket(s, i)
	}
	return -1
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !identByte(s[i]) {
			return false
		}
	}
	return true
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
