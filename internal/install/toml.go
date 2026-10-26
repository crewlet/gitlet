package install

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Codex reads TOML, and no TOML library is available, so the config is
// edited as text: gitlet changes only the command and args keys of its own
// [mcp_servers.gitlet] table, or appends the table, and keeps every other
// line as it was. A scanner tracks strings, arrays and inline tables across
// lines so that only real table headers and keys are recognized.

const codexTable = "mcp_servers.gitlet"

// editCodexConfig sets [mcp_servers.gitlet] in a Codex config, keeping a
// gitlet binary its command already names (see binary). It returns a
// reason instead when gitlet is already defined in a form it does not edit
// (an inline table or dotted keys); the caller then reports a skip.
func editCodexConfig(old []byte, bin binary) (out []byte, skipReason string) {
	text := string(old)
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	var lines []string
	if text != "" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	want := []tomlEntry{
		{key: "command", value: tomlString(bin.name)},
		{key: "args", value: `["mcp"]`},
	}

	top := tomlTopLevel(lines)
	tables := tomlTables(lines, top)
	target := -1
	for i, t := range tables {
		switch {
		case t.name == codexTable && !t.array && target < 0:
			target = i
		case t.name == codexTable && t.array:
			return nil, "[[" + codexTable + "]] is an array of tables"
		case t.name == "mcp_servers" && !t.array, t.name == "" && t.start < 0:
			// gitlet defined inside [mcp_servers] or by dotted keys at the root
			for k := t.start + 1; k < t.end; k++ {
				key := tomlKey(lines[k], top[k])
				prefix := "gitlet"
				if t.name == "" {
					prefix = codexTable
					if key == "mcp_servers" {
						return nil, "mcp_servers is an inline table"
					}
				}
				if key == prefix || strings.HasPrefix(key, prefix+".") {
					return nil, codexTable + " is defined with dotted keys or an inline table"
				}
			}
		}
	}

	var result []string
	if target >= 0 {
		t := tables[target]
		for k := t.start + 1; k < t.end; k++ {
			if tomlKey(lines[k], top[k]) != "command" {
				continue
			}
			if raw, value, ok := tomlStringValue(lines[k]); ok && bin.command(value) == value {
				want[0].value = raw // kept as written
			}
		}
		result = append(result, lines[:t.start+1]...)
		result = append(result, setTomlEntries(lines[t.start+1:t.end], top[t.start+1:t.end], want)...)
		result = append(result, lines[t.end:]...)
	} else {
		result = append(result, lines...)
		if len(result) > 0 && strings.TrimSpace(result[len(result)-1]) != "" {
			result = append(result, "")
		}
		result = append(result, "["+codexTable+"]")
		for _, e := range want {
			result = append(result, e.line())
		}
	}
	return []byte(strings.Join(result, nl) + nl), ""
}

// tomlEntry is one key = value line gitlet owns.
type tomlEntry struct{ key, value string }

func (e tomlEntry) line() string { return e.key + " = " + e.value }

// setTomlEntries rewrites a table body so it holds the wanted entries: an
// existing entry is replaced in place (keeping its indentation, and kept
// as is when it already has the value), and missing ones are added after
// the last non-blank line.
func setTomlEntries(body []string, top []bool, want []tomlEntry) []string {
	var out []string
	done := make([]bool, len(want))
	for k := 0; k < len(body); {
		end := k + 1 // an entry spans its continuation lines
		for end < len(body) && !top[end] {
			end++
		}
		i, key := -1, tomlKey(body[k], top[k])
		for j, e := range want {
			if !done[j] && key == e.key {
				i = j
			}
		}
		switch {
		case i < 0:
			out = append(out, body[k:end]...)
		case end == k+1 && tomlHasValue(body[k], want[i].value):
			out = append(out, body[k])
			done[i] = true
		default:
			indent := body[k][:len(body[k])-len(strings.TrimLeft(body[k], " \t"))]
			out = append(out, indent+want[i].line())
			done[i] = true
		}
		k = end
	}
	at := len(out)
	for at > 0 && strings.TrimSpace(out[at-1]) == "" {
		at--
	}
	var missing []string
	for j, e := range want {
		if !done[j] {
			missing = append(missing, e.line())
		}
	}
	return append(out[:at:at], append(missing, out[at:]...)...)
}

// tomlHasValue reports whether a single-line entry already holds value,
// optionally followed by a comment.
func tomlHasValue(line, value string) bool {
	_, rest, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(strings.TrimSpace(rest), value)
	rest = strings.TrimSpace(rest)
	return ok && (rest == "" || strings.HasPrefix(rest, "#"))
}

// tomlStringValue returns the value of a single-line entry that holds a
// basic or literal string, as written and as read; ok is false for any
// other value.
func tomlStringValue(line string) (raw, value string, ok bool) {
	_, rest, found := strings.Cut(line, "=")
	rest = strings.TrimSpace(rest)
	switch {
	case !found, strings.HasPrefix(rest, `"""`), strings.HasPrefix(rest, "'''"):
	case strings.HasPrefix(rest, "'"):
		if end := strings.IndexByte(rest[1:], '\''); end >= 0 {
			return rest[:end+2], rest[1 : end+1], true
		}
	case strings.HasPrefix(rest, `"`):
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				// Go reads every TOML escape but TOML 1.1's \e.
				v, err := strconv.Unquote(rest[:i+1])
				return rest[:i+1], v, err == nil
			}
		}
	}
	return "", "", false
}

// tomlTable is a table: its normalized name and the range of its lines.
// The root table has start -1 and an empty name.
type tomlTable struct {
	name       string
	array      bool
	start, end int // header line, and one past the last body line
}

func tomlTables(lines []string, top []bool) []tomlTable {
	tables := []tomlTable{{start: -1}}
	for i, l := range lines {
		if !top[i] {
			continue
		}
		name, array, ok := tomlHeader(l)
		if !ok {
			continue
		}
		tables[len(tables)-1].end = i
		tables = append(tables, tomlTable{name: name, array: array, start: i})
	}
	tables[len(tables)-1].end = len(lines)
	return tables
}

// tomlHeader parses "[a.b]" or "[[a.b]]", with an optional comment.
func tomlHeader(line string) (name string, array, ok bool) {
	t := strings.TrimSpace(line)
	open, closing := "[", "]"
	if strings.HasPrefix(t, "[[") {
		open, closing, array = "[[", "]]", true
	}
	if !strings.HasPrefix(t, open) {
		return "", false, false
	}
	end := strings.Index(t, closing)
	if end < 0 {
		return "", false, false
	}
	return normalizeTomlKey(t[len(open):end]), array, true
}

// tomlKey returns the normalized key a top-level line assigns, or "".
func tomlKey(line string, top bool) string {
	t := strings.TrimSpace(line)
	if !top || t == "" || t[0] == '#' || t[0] == '[' {
		return ""
	}
	key, _, ok := strings.Cut(t, "=")
	if !ok {
		return ""
	}
	return normalizeTomlKey(key)
}

// normalizeTomlKey drops whitespace and quotes: ` mcp_servers . "gitlet" `
// becomes mcp_servers.gitlet. Good enough for the keys gitlet looks for.
func normalizeTomlKey(k string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '"', '\'':
			return -1
		}
		return r
	}, k)
}

// tomlTopLevel reports, for each line, whether it starts outside any
// multi-line string, array or inline table: only such a line can hold a
// table header or start a key.
func tomlTopLevel(lines []string) []bool {
	const (
		none = iota
		basic
		literal
		mlBasic
		mlLiteral
	)
	top := make([]bool, len(lines))
	state, depth := none, 0
	for i, l := range lines {
		top[i] = state == none && depth == 0
		if _, _, header := tomlHeader(l); top[i] && header {
			continue
		}
	scan:
		for j := 0; j < len(l); j++ {
			switch state {
			case none:
				switch {
				case l[j] == '#':
					break scan
				case strings.HasPrefix(l[j:], `"""`):
					state, j = mlBasic, j+2
				case strings.HasPrefix(l[j:], `'''`):
					state, j = mlLiteral, j+2
				case l[j] == '"':
					state = basic
				case l[j] == '\'':
					state = literal
				case l[j] == '[' || l[j] == '{':
					depth++
				case (l[j] == ']' || l[j] == '}') && depth > 0:
					depth--
				}
			case basic, mlBasic:
				switch {
				case l[j] == '\\':
					j++
				case state == basic && l[j] == '"':
					state = none
				case state == mlBasic && strings.HasPrefix(l[j:], `"""`):
					state, j = none, j+2
				}
			case literal:
				if l[j] == '\'' {
					state = none
				}
			case mlLiteral:
				if strings.HasPrefix(l[j:], `'''`) {
					state, j = none, j+2
				}
			}
		}
		if state == basic || state == literal { // single-line strings end with the line
			state = none
		}
	}
	return top
}

// tomlString writes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(s, string(utf8.RuneError)) {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
