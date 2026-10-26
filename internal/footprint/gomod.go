package footprint

import (
	"strconv"
	"strings"
)

const byteOrderMark = "\xef\xbb\xbf"

// goModulePath returns the module path declared by a go.mod file, or "" when
// it declares none. It reads only the module directive, so it tolerates
// comments, quoted paths, the block form and syntax it does not know.
func goModulePath(data []byte) string {
	text := strings.TrimPrefix(string(data), byteOrderMark)
	inBlock := false
	for line := range strings.Lines(text) {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			return unquoteModule(line)
		}
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || (rest != "" && !strings.ContainsAny(rest[:1], " \t\"`(")) {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "(" {
			inBlock = true
			continue
		}
		return unquoteModule(rest)
	}
	return ""
}

// unquoteModule returns the module path at the start of s, which may be a
// quoted or raw string.
func unquoteModule(s string) string {
	if s == "" {
		return ""
	}
	if s[0] == '"' || s[0] == '`' {
		end := strings.IndexByte(s[1:], s[0])
		if end < 0 {
			return ""
		}
		p, err := strconv.Unquote(s[:end+2])
		if err != nil {
			return ""
		}
		return strings.TrimSpace(p)
	}
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}
