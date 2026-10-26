package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultBinary = "gitlet"

// Change actions.
const (
	actCreate    = "create"
	actUpdate    = "update"
	actUnchanged = "unchanged"
	actSkip      = "skip"
)

// claudeHooks are the Claude Code hook events gitlet answers, with the
// argument of "gitlet hook claude-code" and the tool matcher, if any.
var claudeHooks = []struct{ event, arg, matcher string }{
	{"SessionStart", "session-start", ""},
	{"UserPromptSubmit", "user-prompt-submit", ""},
	{"PostToolUse", "post-tool-use", "*"},
}

// gitHooks are the git hooks gitlet installs, in order.
var gitHooks = []string{"prepare-commit-msg", "post-commit", "pre-push"}

// hookMarker starts the line that marks a git hook file as gitlet's own.
// gitlet rewrites only hook files that carry it.
const hookMarker = "# gitlet-managed:"

// plan is one file Install writes or would write.
type plan struct {
	change Change
	target string // the file written: change.Path with symlinks resolved
	data   []byte
	mode   fs.FileMode
}

// fileSpec says how to produce one file. edit gets the current content (nil
// when the file does not exist) and returns the new content; new files get
// mode, existing ones keep theirs.
type fileSpec struct {
	path string
	mode fs.FileMode
	edit func(old []byte, exists bool) ([]byte, error)
}

func install(ctx context.Context, opt Options) ([]Change, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if opt.Root == "" {
		return nil, errors.New("install: no root directory given")
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	if fi, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("install: %s is not a directory", root)
	}
	clients, err := chooseClients(root, opt.Clients)
	if err != nil {
		return nil, err
	}
	bin := strings.TrimSpace(opt.Binary)
	if bin == "" {
		bin = defaultBinary
	}
	hooksDir := opt.HooksDir
	if opt.GitHooks {
		if hooksDir == "" {
			return nil, errors.New("install: git hooks need the hooks directory")
		}
		if !filepath.IsAbs(hooksDir) {
			hooksDir = filepath.Join(root, hooksDir)
		}
	}

	var specs []fileSpec
	for _, c := range clients {
		switch c {
		case ClaudeCode:
			specs = append(specs, claudeCodeFiles(root, bin)...)
		case Codex:
			specs = append(specs, codexFiles(root, bin)...)
		case Cursor:
			specs = append(specs, cursorFiles(root, bin)...)
		}
	}
	// Plan every file before writing any, so a file gitlet refuses to edit
	// stops the install before it is half done.
	var plans []plan
	for _, s := range specs {
		p, err := planFile(s)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	if opt.GitHooks {
		hooks, err := planGitHooks(hooksDir, bin)
		if err != nil {
			return nil, err
		}
		plans = append(plans, hooks...)
	}

	changes := make([]Change, 0, len(plans))
	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return changes, fmt.Errorf("install: %w", err)
		}
		if !opt.DryRun && (p.change.Action == actCreate || p.change.Action == actUpdate) {
			if err := writeAtomic(p.target, p.data, p.mode); err != nil {
				return changes, fmt.Errorf("install: %s: %w", p.change.Path, err)
			}
		}
		changes = append(changes, p.change)
	}
	return changes, nil
}

func detect(root string) []string {
	var out []string
	if isDir(filepath.Join(root, ".claude")) || exists(filepath.Join(root, ".mcp.json")) {
		out = append(out, ClaudeCode)
	}
	if isDir(filepath.Join(root, ".codex")) || exists(filepath.Join(root, "AGENTS.md")) {
		out = append(out, Codex)
	}
	if isDir(filepath.Join(root, ".cursor")) {
		out = append(out, Cursor)
	}
	sort.Strings(out)
	return out
}

// chooseClients validates the requested clients, or picks the detected
// ones, or Claude Code. The result is sorted and has no duplicates.
func chooseClients(root string, names []string) ([]string, error) {
	if len(names) == 0 {
		if names = detect(root); len(names) == 0 {
			return []string{ClaudeCode}, nil
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		switch n {
		case ClaudeCode, Codex, Cursor:
		default:
			return nil, fmt.Errorf("install: unknown client %q (want %s, %s or %s)", n, ClaudeCode, Codex, Cursor)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- clients ----

// mcpServer is gitlet's MCP server entry, in the order the keys are written.
func mcpServer(bin string, withType bool) []field {
	var f []field
	if withType {
		f = append(f, field{"type", "stdio"})
	}
	return append(f, field{"command", bin}, field{"args", []any{"mcp"}})
}

func claudeCodeFiles(root, bin string) []fileSpec {
	mcp := filepath.Join(root, ".mcp.json")
	settings := filepath.Join(root, ".claude", "settings.json")
	return []fileSpec{
		{mcp, 0o644, editJSON(mcp, mergeServer(mcpServer(bin, true)))},
		{filepath.Join(root, ".claude", "skills", "gitlet", "SKILL.md"), 0o644, content(skill)},
		{settings, 0o644, editJSON(settings, mergeClaudeHooks(bin))},
	}
}

func codexFiles(root, bin string) []fileSpec {
	agents := filepath.Join(root, "AGENTS.md")
	return []fileSpec{
		{filepath.Join(root, ".codex", "config.toml"), 0o644, func(old []byte, _ bool) ([]byte, error) {
			out, skip := editCodexConfig(old, bin)
			if skip != "" {
				return nil, &skipError{
					summary: fmt.Sprintf("%s = { command = %s, args = [\"mcp\"] }", codexTable, tomlString(bin)),
					reason:  skip,
				}
			}
			return out, nil
		}},
		{agents, 0o644, func(old []byte, _ bool) ([]byte, error) {
			return replaceSection(agents, old, agentsSection())
		}},
	}
}

func cursorFiles(root, bin string) []fileSpec {
	mcp := filepath.Join(root, ".cursor", "mcp.json")
	return []fileSpec{
		{mcp, 0o644, editJSON(mcp, mergeServer(mcpServer(bin, false)))},
		{filepath.Join(root, ".cursor", "rules", "gitlet.mdc"), 0o644, content(cursorRule())},
	}
}

// content is an edit that writes fixed content: a file gitlet owns.
func content(s string) func([]byte, bool) ([]byte, error) {
	return func([]byte, bool) ([]byte, error) { return []byte(s), nil }
}

// field is one key and value gitlet sets in a JSON object.
type field struct {
	key   string
	value any
}

// mergeServer sets mcpServers.gitlet, keeping any other keys of the entry
// (such as env) and every other server.
func mergeServer(fields []field) func(*object) (bool, error) {
	return func(root *object) (bool, error) {
		servers, added, err := childObject(root, "mcpServers", "mcpServers")
		if err != nil {
			return false, err
		}
		entry, addedEntry, err := childObject(servers, "gitlet", "mcpServers.gitlet")
		if err != nil {
			return false, err
		}
		changed := added || addedEntry
		for _, f := range fields {
			if v, ok := entry.get(f.key); !ok || !jsonEqual(v, f.value) {
				entry.set(f.key, f.value)
				changed = true
			}
		}
		return changed, nil
	}
}

// mergeClaudeHooks adds gitlet's Claude Code hooks to settings. An event
// that already runs gitlet's hook keeps its entry, with the command brought
// up to date; other hooks are left alone. Like the git hooks, the commands
// do nothing when gitlet is missing, since the settings are shared with
// teammates who may not have it.
func mergeClaudeHooks(bin string) func(*object) (bool, error) {
	return func(root *object) (bool, error) {
		hooks, changed, err := childObject(root, "hooks", "hooks")
		if err != nil {
			return false, err
		}
		for _, h := range claudeHooks {
			cmd := guarded(shellQuote(bin), "hook claude-code "+h.arg)
			groups, err := childArray(hooks, h.event, "hooks."+h.event)
			if err != nil {
				return false, err
			}
			var found []*object
			exact := false
			for _, g := range groups {
				group, _ := g.(*object)
				if group == nil {
					continue
				}
				list, _ := group.vals["hooks"].([]any)
				for _, e := range list {
					entry, _ := e.(*object)
					if entry == nil {
						continue
					}
					if c, _ := entry.vals["command"].(string); runsHook(c, "claude-code", h.arg) {
						found = append(found, entry)
						exact = exact || c == cmd
					}
				}
			}
			switch {
			case exact:
			case len(found) > 0:
				found[0].set("command", cmd)
				changed = true
			default:
				entry := newObject()
				entry.set("type", "command")
				entry.set("command", cmd)
				group := newObject()
				if h.matcher != "" {
					group.set("matcher", h.matcher)
				}
				group.set("hooks", []any{entry})
				hooks.set(h.event, append(groups, group))
				changed = true
			}
		}
		return changed, nil
	}
}

// runsHook reports whether a command runs "gitlet hook <client> <event>",
// whatever the binary is called.
func runsHook(command, client, event string) bool {
	f := strings.Fields(command)
	for i := 0; i+2 < len(f); i++ {
		if f[i] == "hook" && f[i+1] == client && f[i+2] == event {
			return true
		}
	}
	return false
}

// editJSON turns a merge into a JSON object into an edit. An existing file
// that is not a JSON object is an error: gitlet never overwrites it. A file
// the merge does not change is returned byte for byte.
func editJSON(path string, merge func(*object) (bool, error)) func([]byte, bool) ([]byte, error) {
	return func(old []byte, exists bool) ([]byte, error) {
		root := newObject()
		if len(bytes.TrimSpace(old)) > 0 {
			v, err := decodeJSON(old)
			if err != nil {
				return nil, fmt.Errorf("install: %s is not valid JSON (%v); fix it and run init again", path, err)
			}
			o, ok := v.(*object)
			if !ok {
				return nil, fmt.Errorf("install: %s does not hold a JSON object", path)
			}
			root = o
		}
		changed, err := merge(root)
		if err != nil {
			return nil, fmt.Errorf("install: %s: %w", path, err)
		}
		if exists && !changed {
			return old, nil
		}
		return encodeJSON(root), nil
	}
}

// replaceSection puts section between the gitlet markers of a Markdown
// file, or appends it after a blank line.
func replaceSection(path string, old []byte, section string) ([]byte, error) {
	text := string(old)
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
		section = strings.ReplaceAll(section, "\n", nl)
	}
	start := strings.Index(text, markerStart)
	end := -1
	if start >= 0 {
		if i := strings.Index(text[start:], markerEnd); i >= 0 {
			end = start + i + len(markerEnd)
		}
	}
	switch {
	case start >= 0 && end >= 0:
		return []byte(text[:start] + section + text[end:]), nil
	case start >= 0:
		return nil, fmt.Errorf("install: %s has %s without %s after it; fix it by hand", path, markerStart, markerEnd)
	case strings.Contains(text, markerEnd):
		return nil, fmt.Errorf("install: %s has %s without %s before it; fix it by hand", path, markerEnd, markerStart)
	case strings.TrimSpace(text) == "":
		return []byte(section + nl), nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += nl
	}
	if !strings.HasSuffix(strings.TrimSuffix(text, nl), "\n") {
		text += nl
	}
	return []byte(text + section + nl), nil
}

// ---- git hooks ----

func planGitHooks(dir, bin string) ([]plan, error) {
	var plans []plan
	for _, name := range gitHooks {
		var foreign, calls bool
		p, err := planFile(fileSpec{filepath.Join(dir, name), 0o755, func(old []byte, exists bool) ([]byte, error) {
			if exists && !isGitletHook(old) {
				foreign, calls = true, callsGitHook(old, name)
				return old, nil
			}
			return []byte(hookScript(bin, name)), nil
		}})
		if err != nil {
			return nil, err
		}
		switch {
		case foreign && calls: // someone added gitlet's line by hand
			p.change.Action = actUnchanged
		case foreign:
			p.change.Action, p.change.Summary = actSkip, hookLine(bin, name)
		case p.change.Action != actCreate && p.mode&0o111 != 0o111:
			p.mode |= 0o111 // a hook runs only when executable
			p.change.Action = actUpdate
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// hookScript is the git hook gitlet installs. It never fails git: a missing
// binary is skipped and every error is swallowed.
func hookScript(bin, name string) string {
	q := shellQuote(bin)
	return "#!/bin/sh\n" +
		hookMarker + ` installed by "gitlet init". Run it again to update this file, or delete the file to remove the hook.` + "\n" +
		"# It never fails git: it does nothing when gitlet is missing and ignores gitlet's errors.\n" +
		"command -v " + q + " >/dev/null 2>&1 || exit 0\n" +
		q + " hook git " + name + ` "$@" || true` + "\n" +
		"exit 0\n"
}

// hookLine is the line to add by hand to a hook gitlet does not own.
func hookLine(bin, name string) string {
	return guarded(shellQuote(bin), "hook git "+name+` "$@"`)
}

// guarded is a shell command that runs the gitlet binary q (in shell
// syntax) with args and never fails: it does nothing when q is missing and
// ignores gitlet's exit status (docs/SPEC.md, section 14.2).
func guarded(q, args string) string {
	return "command -v " + q + " >/dev/null 2>&1 && " + q + " " + args + " || true"
}

func isGitletHook(data []byte) bool {
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), hookMarker) {
			return true
		}
	}
	return false
}

func callsGitHook(data []byte, name string) bool {
	for _, l := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") && runsHook(l, "git", name) {
			return true
		}
	}
	return false
}

// ---- files ----

// skipError is returned by an edit that leaves a file for the user to change
// by hand.
type skipError struct{ summary, reason string }

func (e *skipError) Error() string { return e.reason }

// planFile reads a file, runs its edit, and decides the action.
func planFile(s fileSpec) (plan, error) {
	target, fi, err := resolve(s.path)
	if err != nil {
		return plan{}, fmt.Errorf("install: %w", err)
	}
	p := plan{change: Change{Path: s.path}, target: target, mode: s.mode}
	var old []byte
	if fi != nil {
		if old, err = os.ReadFile(target); err != nil {
			return plan{}, fmt.Errorf("install: %w", err)
		}
		p.mode = fi.Mode().Perm()
	}
	data, err := s.edit(old, fi != nil)
	var skip *skipError
	switch {
	case errors.As(err, &skip):
		p.change.Action, p.change.Summary = actSkip, skip.summary
		return p, nil
	case err != nil:
		return plan{}, err
	}
	p.data = data
	switch {
	case fi == nil:
		p.change.Action = actCreate
	case bytes.Equal(old, data):
		p.change.Action = actUnchanged
	default:
		p.change.Action = actUpdate
	}
	return p, nil
}

// resolve follows a symlink to the file it names, so writing replaces the
// target and keeps the link. It returns nil info when nothing is there.
func resolve(path string) (string, fs.FileInfo, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	target := path
	if fi.Mode()&fs.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return "", nil, fmt.Errorf("%s: cannot follow the symlink: %w", path, err)
		}
		if fi, err = os.Stat(target); err != nil {
			return "", nil, err
		}
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", path)
	}
	return target, fi, nil
}

// writeAtomic writes data to a temporary file in path's directory and
// renames it over path, so readers never see a partial file.
func writeAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
	}) < 0
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
