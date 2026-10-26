package rules

import (
	"path"
	"slices"
	"strings"
)

// lockfiles are regenerated, never merged by hand (docs/SPEC.md, section 10.3).
var lockfiles = setOf(
	"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "go.sum", "Cargo.lock",
	"poetry.lock", "uv.lock", "Gemfile.lock", "composer.lock",
)

// changelogPrefixes start the names of changelogs. They are matched case
// sensitively, so source files such as history.go are not changelogs.
var changelogPrefixes = []string{"CHANGELOG", "CHANGES", "HISTORY"}

// hotFile implements HotFile.
func hotFile(p string) string {
	base := path.Base(p)
	if lockfiles[base] {
		return "lockfile"
	}
	for _, pre := range changelogPrefixes {
		if strings.HasPrefix(base, pre) {
			return "changelog"
		}
	}
	return ""
}

// migrationDirs are the built-in migration directories, matched against the
// last segments of a directory.
var migrationDirs = [][]string{
	{"migrations"}, {"migrate"}, {"alembic", "versions"}, {"prisma", "migrations"}, {"db", "migrate"},
}

// migrationSeq implements MigrationSeq. The migration directory is the
// deepest directory of the path that ends in a built-in migration directory
// or matches one of extraGlobs ("**" matches any number of segments). The
// sequence number comes from the first entry below it that starts with
// digits, after an optional Flyway "V": usually the file name, or the
// directory of a Prisma-style migration (prisma/migrations/<seq>_name/...).
// dir is the directory that holds that entry.
func migrationSeq(p string, extraGlobs []string) (dir, seq string, ok bool) {
	p = strings.Trim(path.Clean(p), "/")
	segs := strings.Split(p, "/")
	at := -1
	for i := len(segs) - 1; i >= 1 && at < 0; i-- {
		if isMigrationDir(segs[:i], extraGlobs) {
			at = i
		}
	}
	if at < 0 {
		return "", "", false
	}
	for j := at; j < len(segs); j++ {
		if s := leadingSeq(segs[j]); s != "" {
			return strings.Join(segs[:j], "/"), s, true
		}
	}
	return "", "", false
}

func isMigrationDir(segs []string, extraGlobs []string) bool {
	for _, suffix := range migrationDirs {
		if len(segs) >= len(suffix) && slices.Equal(segs[len(segs)-len(suffix):], suffix) {
			return true
		}
	}
	for _, g := range extraGlobs {
		if g = strings.Trim(g, "/"); g != "" && globMatch(strings.Split(g, "/"), segs) {
			return true
		}
	}
	return false
}

// leadingSeq returns the leading run of digits of a name, after an
// optional Flyway "V".
func leadingSeq(name string) string {
	if len(name) > 1 && name[0] == 'V' && isDigit(name[1]) {
		name = name[1:]
	}
	i := 0
	for i < len(name) && isDigit(name[i]) {
		i++
	}
	return name[:i]
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// sameSeq compares sequence numbers by value, so V3 and 0003 collide.
func sameSeq(a, b string) bool {
	trim := func(s string) string {
		if t := strings.TrimLeft(s, "0"); t != "" {
			return t
		}
		return "0"
	}
	return trim(a) == trim(b)
}

// globMatch matches path segments against glob segments; "**" matches any
// number of segments and every other segment uses path.Match.
func globMatch(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if globMatch(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
