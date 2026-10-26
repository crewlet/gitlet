package footprint

import (
	"cmp"
	"math"
	"slices"

	"github.com/crewlet/gitlet/internal/model"
)

// changedRefs returns the refs of the new version of a file that are made
// from added or changed code (docs/SPEC.md, section 9.5). Source gives no
// line diff, so the choice is made line by line from the two versions:
//
//   - a line inside a symbol belongs to the innermost symbol that covers it
//     whole, plus any smaller symbol that shares it (a one-line method next
//     to its class header). Its refs count when one of those symbols is
//     added or changed. An unchanged method of a changed class therefore
//     contributes nothing, because its tokens are not in the class's body
//     hash;
//   - a line no symbol covers whole is a top-level line. Its refs count when
//     a symbol sharing it changed, or when the line itself changed: when the
//     old version has no top-level line with the same text (ignoring
//     surrounding whitespace), each old line answering for at most one new
//     line. Every top-level line of an added file is changed.
//
// The line test can only miss a ref whose line text already stood at top
// level in the old version, which makes the same reference.
func changedRefs(oldV, newV *version, changes []model.SymbolChange) []model.Ref {
	if newV == nil || newV.file == nil || len(newV.file.Refs) == 0 {
		return nil
	}
	changed := make(map[string]bool)
	for _, c := range changes {
		switch c.Change {
		case model.ChangeAdded, model.ChangeSignature, model.ChangeBody, model.ChangeRenamed:
			changed[keyOf(c.Symbol)] = true
		case model.ChangeMoved:
			if c.SigChanged || c.BodyChanged {
				changed[keyOf(c.Symbol)] = true
			}
		}
	}

	nl := newLines(newV.src)
	syms := slices.Clone(newV.file.Symbols)
	slices.SortStableFunc(syms, func(a, b model.Symbol) int { return cmp.Compare(a.StartLine, b.StartLine) })
	var oldTop map[string]int
	if oldV != nil && oldV.file != nil {
		oldTop = topLevelLines(oldV.file.Symbols, newLines(oldV.src))
	}

	// Lines are decided in order, so that of two equal new top-level lines
	// the first one answers to the old line.
	var lineNums []int
	for _, r := range newV.file.Refs {
		lineNums = append(lineNums, r.Line)
	}
	slices.Sort(lineNums)
	keep := make(map[int]bool, len(lineNums))
	for _, n := range slices.Compact(lineNums) {
		keep[n] = lineChanged(syms, nl, n, changed, oldTop)
	}

	var out []model.Ref
	for _, r := range newV.file.Refs {
		if keep[r.Line] {
			out = append(out, r)
		}
	}
	return out
}

// lineChanged reports whether line n of the new version holds added or
// changed code. syms must be sorted by start line. A top-level line that
// matches an old one consumes it from oldTop.
func lineChanged(syms []model.Symbol, nl lines, n int, changed map[string]bool, oldTop map[string]int) bool {
	owner := -1
	var touching []int
	for i, s := range syms {
		if s.StartLine > n {
			break
		}
		if s.EndLine < n {
			continue
		}
		touching = append(touching, i)
		if covers(s, nl, n) && (owner < 0 || smaller(s, syms[owner])) {
			owner = i
		}
	}
	if owner >= 0 && changed[keyOf(syms[owner])] {
		return true
	}
	for _, i := range touching {
		if i != owner && (owner < 0 || smaller(syms[i], syms[owner])) && changed[keyOf(syms[i])] {
			return true
		}
	}
	if owner >= 0 {
		return false
	}
	if text := nl.text(n); text != "" && oldTop[text] > 0 {
		oldTop[text]--
		return false
	}
	return true
}

// topLevelLines counts the texts of the lines no symbol covers whole.
func topLevelLines(syms []model.Symbol, ol lines) map[string]int {
	count := ol.count()
	inside := make([]int, count+2) // difference array over lines strictly inside a symbol
	covered := make([]bool, count+2)
	for _, s := range syms {
		start, end := max(s.StartLine, 1), min(s.EndLine, count)
		if start > end {
			continue
		}
		if start+1 <= end-1 {
			inside[start+1]++
			inside[end]--
		}
		for _, n := range []int{start, end} {
			if covers(s, ol, n) {
				covered[n] = true
			}
		}
	}
	top := make(map[string]int)
	depth := 0
	for n := 1; n <= count; n++ {
		depth += inside[n]
		if depth > 0 || covered[n] {
			continue
		}
		if t := ol.text(n); t != "" {
			top[t]++
		}
	}
	return top
}

// covers reports whether symbol s spans the whole text of line n. Without a
// usable byte range, a symbol covers every line it touches.
func covers(s model.Symbol, l lines, n int) bool {
	if n < s.StartLine || n > s.EndLine {
		return false
	}
	if (s.StartLine < n && n < s.EndLine) || !validBytes(s, l) {
		return true
	}
	start, end, ok := l.bounds(n)
	return !ok || (s.StartByte <= start && end <= s.EndByte)
}

func validBytes(s model.Symbol, l lines) bool {
	return s.StartByte >= 0 && s.StartByte < s.EndByte && s.EndByte <= len(l.src)
}

// smaller orders symbols by how many lines, then bytes, they span.
func smaller(a, b model.Symbol) bool {
	if la, lb := a.EndLine-a.StartLine, b.EndLine-b.StartLine; la != lb {
		return la < lb
	}
	return byteSpan(a) < byteSpan(b)
}

func byteSpan(s model.Symbol) int {
	if s.StartByte < 0 || s.EndByte <= s.StartByte {
		return math.MaxInt
	}
	return s.EndByte - s.StartByte
}

// lines indexes the lines of a source file (1-based).
type lines struct {
	src   []byte
	start []int // byte offset of each line's first byte
}

func newLines(src []byte) lines {
	start := []int{0}
	for i, b := range src {
		if b == '\n' && i+1 < len(src) {
			start = append(start, i+1)
		}
	}
	return lines{src: src, start: start}
}

func (l lines) count() int {
	if len(l.src) == 0 {
		return 0
	}
	return len(l.start)
}

// bounds returns the byte range of line n without surrounding whitespace;
// ok is false for blank or missing lines.
func (l lines) bounds(n int) (start, end int, ok bool) {
	if n < 1 || n > l.count() {
		return 0, 0, false
	}
	start, end = l.start[n-1], len(l.src)
	if n < len(l.start) {
		end = l.start[n] - 1
	}
	for start < end && isSpace(l.src[start]) {
		start++
	}
	for end > start && isSpace(l.src[end-1]) {
		end--
	}
	return start, end, start < end
}

// text returns line n without surrounding whitespace.
func (l lines) text(n int) string {
	start, end, ok := l.bounds(n)
	if !ok {
		return ""
	}
	return string(l.src[start:end])
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	}
	return false
}
