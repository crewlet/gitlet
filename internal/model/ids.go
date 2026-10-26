package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// hashHex returns the first n hex characters of sha256 over parts joined by NUL.
func hashHex(n int, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:n]
}

// SymbolKey is the stable key of a symbol, also used as its claim ref name.
func SymbolKey(lang, path, qualified string) string {
	return hashHex(16, lang, path, qualified)
}

// SigThreadID is the thread every server derives for a contested symbol at
// a claim epoch (0 when unclaimed).
func SigThreadID(symbolKey string, epoch int) string {
	return "t-" + hashHex(12, "sig", symbolKey, strconv.Itoa(epoch))
}

// NoteThreadID is the thread for a conversation about a note's subject
// between the given items.
func NoteThreadID(kind NoteKind, subject string, items ...ItemID) string {
	ss := make([]string, len(items))
	for i, it := range items {
		ss[i] = string(it)
	}
	sort.Strings(ss)
	return "t-" + hashHex(12, append([]string{"note", string(kind), subject}, ss...)...)
}

// NoteID is the stable id of a note: one note per kind, subject and recipient
// item, updated in place as counterparts come and go.
func NoteID(kind NoteKind, subject string, mine ItemID) string {
	return "n-" + hashHex(12, string(kind), subject, string(mine))
}

// NoteVersion hashes the material facts of a note. A note is shown again only
// when its version changes. Callers pass severity, sorted counterpart items,
// contract hashes and the breaking flag, in a fixed order.
func NoteVersion(parts ...string) string { return hashHex(12, parts...) }

// NewProposalID returns a fresh proposal id.
func NewProposalID() string { return "p-" + RandomHex(12) }

// NewMessageID returns a fresh message or need id.
func NewMessageID() string { return RandomHex(12) }

// ContentID is a short stable id derived from content.
func ContentID(parts ...string) string { return hashHex(12, parts...) }

// RandomHex returns n random hex characters.
func RandomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("gitlet: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// NewItemID returns a fresh work item id.
func NewItemID() ItemID { return ItemID(RandomHex(12)) }

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns free text into a lowercase ref-safe slug of at most max characters.
func Slug(s string, max int) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "-")
	}
	if s == "" {
		s = "work"
	}
	return s
}

// NewAgentID builds an agent id from the client name and host.
func NewAgentID(client, host string) AgentID {
	c := Slug(client, 16)
	h := Slug(host, 20)
	return AgentID(c + "-" + h + "-" + RandomHex(6))
}

var validAgentID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidAgentID reports whether id is safe to use in a ref name.
func ValidAgentID(id AgentID) bool { return validAgentID.MatchString(string(id)) }

var validItemID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidItemID reports whether id is a well-formed work item id.
func ValidItemID(id ItemID) bool { return validItemID.MatchString(string(id)) }

// LangFamily groups languages whose symbols can call each other.
func LangFamily(lang string) string {
	switch lang {
	case "typescript", "tsx", "javascript":
		return "js"
	default:
		return lang
	}
}

// ModulesOf returns the modules a symbol at path belongs to (docs/SPEC.md,
// section 9.5). Most files have one module; index, __init__, mod, lib and
// main files also stand for their directory.
func ModulesOf(lang, p string) []string {
	if lang == "go" {
		return []string{path.Dir(p)}
	}
	ext := path.Ext(p)
	noExt := strings.TrimSuffix(p, ext)
	base := path.Base(noExt)
	dir := path.Dir(p)
	switch lang {
	case "typescript", "tsx", "javascript":
		if base == "index" {
			return []string{noExt, dir}
		}
	case "python":
		if base == "__init__" {
			return []string{dir}
		}
	case "rust":
		if base == "mod" || base == "lib" || base == "main" {
			return []string{dir}
		}
	}
	return []string{noExt}
}

var (
	goMethodExpr = regexp.MustCompile(`^\(\*?\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)\.(.+)$`)
)

// NormalizeQualified turns the accepted aliases of a qualified name into the
// canonical form: "::" and "->" become ".", and "(*T).M" becomes "T.M".
func NormalizeQualified(q string) string {
	q = strings.TrimSpace(q)
	if m := goMethodExpr.FindStringSubmatch(q); m != nil {
		q = m[1] + "." + m[2]
	}
	q = strings.ReplaceAll(q, "::", ".")
	q = strings.ReplaceAll(q, "->", ".")
	q = strings.Trim(q, ".")
	return q
}

// SplitTarget splits "path:rest" at the first ":" into a path candidate and
// the rest. ok is false when s has no ":" or the part before it is empty.
// Callers decide whether the path part names a file.
func SplitTarget(s string) (pathPart, rest string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 || strings.HasPrefix(s[i:], "::") {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}
