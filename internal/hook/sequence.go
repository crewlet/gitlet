package hook

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// minPrefix is the shortest commit id prefix matched, as git's own minimum
// abbreviation.
const minPrefix = 4

// sequenceEditor rewrites a rebase todo for update (docs/SPEC.md, section
// 12.3). Unlike the other hooks it fails loudly, without recovering panics:
// git then aborts the rebase instead of running a todo gitlet could not edit.
func (r *runner) sequenceEditor(ctx context.Context, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(r.stderr, "usage: gitlet hook sequence-editor <todo-file>")
		return 2
	}
	if err := r.editTodo(ctx, args[0]); err != nil {
		fmt.Fprintf(r.stderr, "gitlet hook sequence-editor: %v\n", err)
		return 1
	}
	return 0
}

// editTodo drops the commits GITLET_DROP lists from the todo at path. The
// file is only rewritten, atomically, when a line changes.
func (r *runner) editTodo(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	drop, err := parseDrop(r.getenv("GITLET_DROP"))
	if err != nil || len(drop) == 0 {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	todo, n := rewriteTodo(string(data), drop)
	if n == 0 {
		return nil
	}
	return writeFileAtomic(path, []byte(todo))
}

// parseDrop splits GITLET_DROP into lowercase commit ids or prefixes. Any
// other word is an error: guessing could drop the wrong commit.
func parseDrop(s string) ([]string, error) {
	var ids []string
	for _, f := range strings.Fields(s) {
		f = strings.ToLower(f)
		if len(f) < minPrefix || len(f) > 64 || !isHex(f) {
			return nil, fmt.Errorf("GITLET_DROP: %q is not a commit id", f)
		}
		ids = append(ids, f)
	}
	return ids, nil
}

// rewriteTodo turns "pick <oid> ..." lines (or "p <oid> ...") whose commit
// matches drop into "drop <oid> ..." ("d <oid> ...") and returns the todo
// with how many lines changed. Every other line, including comments, blank
// lines and other commands, is kept byte for byte.
func rewriteTodo(todo string, drop []string) (string, int) {
	lines := strings.SplitAfter(todo, "\n")
	n := 0
	for i, l := range lines {
		if dl, ok := dropLine(l, drop); ok {
			lines[i] = dl
			n++
		}
	}
	return strings.Join(lines, ""), n
}

// dropLine rewrites one todo line when it picks a commit listed in drop.
func dropLine(line string, drop []string) (string, bool) {
	rest := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(rest)]
	cmd, args := cutSpace(rest)
	var repl string
	switch cmd {
	case "pick":
		repl = "drop"
	case "p":
		repl = "d"
	default:
		return line, false
	}
	oid, _ := cutSpace(strings.TrimLeft(args, " \t"))
	if !matchesAny(strings.TrimRight(oid, "\r\n"), drop) {
		return line, false
	}
	return indent + repl + rest[len(cmd):], true
}

// cutSpace splits s before its first space or tab.
func cutSpace(s string) (word, rest string) {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// matchesAny reports whether oid names one of ids: either may be a prefix
// of the other, since todo lines carry abbreviated ids.
func matchesAny(oid string, ids []string) bool {
	oid = strings.ToLower(oid)
	if len(oid) < minPrefix || !isHex(oid) {
		return false
	}
	for _, id := range ids {
		if strings.HasPrefix(id, oid) || strings.HasPrefix(oid, id) {
			return true
		}
	}
	return false
}

func isHex(s string) bool {
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return s != ""
}
