package footprint

import (
	"path"
	"slices"
	"strconv"
	"strings"
)

const byteOrderMark = "\xef\xbb\xbf"

// GoModule says how the Go import paths of one file map to directories of
// the repository.
type GoModule struct {
	Path string // module path of the go.mod that holds the file; "" when unknown
	Dir  string // directory of that go.mod; "" or "." is the repository root
	// Others maps the paths of other modules of the repository, from local
	// replace directives and go.work use directives, to their directories.
	Others map[string]string
}

// known reports whether m names any module.
func (m GoModule) known() bool { return m.Path != "" || len(m.Others) > 0 }

// goModFile is what Compute needs from a go.mod or go.work file.
type goModFile struct {
	module   string      // the first module directive
	uses     []string    // use directives, as written
	replaces []goReplace // replace directives with a relative directory as target
}

// goReplace is a replace directive whose target is a directory.
type goReplace struct {
	module, dir string // dir as written, relative to the file
}

// goModulePath returns the module path declared by a go.mod file, or "" when
// it declares none.
func goModulePath(data []byte) string { return parseGoMod(data).module }

// parseGoMod reads the module, use and replace directives of a go.mod or
// go.work file. It tolerates comments, quoted paths, the block form and
// syntax it does not know.
func parseGoMod(data []byte) goModFile {
	var f goModFile
	moduleSeen := false
	block := "" // verb of the block being read
	text := strings.TrimPrefix(string(data), byteOrderMark)
	for line := range strings.Lines(text) {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		verb, rest := block, line
		switch {
		case block != "" && line == ")":
			block = ""
			continue
		case block == "":
			end := strings.IndexAny(line, " \t\"`(")
			if end < 0 {
				end = len(line)
			}
			verb, rest = line[:end], strings.TrimSpace(line[end:])
			if rest == "(" {
				block = verb
				continue
			}
		}
		args, ok := goFields(rest)
		switch verb {
		case "module":
			if !moduleSeen && ok && len(args) > 0 {
				f.module = args[0]
			}
			moduleSeen = true
		case "use":
			if ok && len(args) > 0 {
				f.uses = append(f.uses, args[0])
			}
		case "replace":
			// old [version] => new [version]; only a relative directory
			// can be part of the repository.
			if i := slices.Index(args, "=>"); ok && i >= 1 && i+1 < len(args) && relativeDir(args[i+1]) {
				f.replaces = append(f.replaces, goReplace{module: args[0], dir: args[i+1]})
			}
		}
	}
	return f
}

// goFields splits the arguments of a directive into fields; a quoted or raw
// string is one field. ok is false when a string is not terminated or does
// not unquote.
func goFields(s string) (fields []string, ok bool) {
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return fields, true
		}
		if s[0] == '"' || s[0] == '`' {
			end := strings.IndexByte(s[1:], s[0])
			if end < 0 {
				return nil, false
			}
			f, err := strconv.Unquote(s[:end+2])
			if err != nil {
				return nil, false
			}
			fields = append(fields, strings.TrimSpace(f))
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
}

// relativeDir reports whether a replace target is a relative directory
// rather than a module path.
func relativeDir(p string) bool {
	return p == "." || p == ".." || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../")
}

// repoDir joins a directory written in a go.mod or go.work file to the
// directory of that file. ok is false when the result is outside the
// repository.
func repoDir(base, dir string) (string, bool) {
	if path.IsAbs(dir) {
		return "", false
	}
	p := path.Join(base, dir)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}
