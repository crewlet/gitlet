package hook

import (
	"regexp"
	"strings"
)

// scissors follows the comment prefix on the line below which git ignores
// a commit message (git commit -v, commit.cleanup=scissors).
const scissors = " ------------------------ >8 ------------------------"

// trailerLine matches a "Key: value" trailer, or the line cherry-pick -x adds.
var trailerLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*\s*:(\s|$)|\(cherry picked from commit )`)

// addTrailers adds "Gitlet-Item: <item>" and "Gitlet-Agent: <agent>" to the
// content of a commit message file the way git interpret-trailers would:
// right after the message text, before the trailing comments and the
// scissors section, joining a trailer block that ends the message or
// starting a new paragraph. Everything else is kept byte for byte.
//
// It returns msg unchanged and false when the message has no text yet (an
// untouched message must still abort the commit) or already names an item;
// a Gitlet-Agent line already present is kept instead of adding another.
func addTrailers(msg, comment, item, agent string) (string, bool) {
	lines := strings.SplitAfter(msg, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	end := len(lines)
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == comment+scissors {
			end = i
			break
		}
	}
	last := -1 // the message text ends at lines[last]
	for i := end - 1; i >= 0; i-- {
		if !isBlank(lines[i]) && !strings.HasPrefix(lines[i], comment) {
			last = i
			break
		}
	}
	text := lines[:last+1]
	if emptyMessage(text, comment) || hasTrailer(text, comment, "Gitlet-Item") {
		return msg, false
	}
	add := []string{"Gitlet-Item: " + item}
	if !hasTrailer(text, comment, "Gitlet-Agent") {
		add = append(add, "Gitlet-Agent: "+agent)
	}

	nl := "\n"
	if strings.HasSuffix(text[last], "\r\n") {
		nl = "\r\n"
	}
	var b strings.Builder
	for _, l := range text {
		b.WriteString(l)
	}
	if !strings.HasSuffix(text[last], "\n") {
		b.WriteString(nl)
	}
	if !endsWithTrailers(text, comment) {
		b.WriteString(nl)
	}
	for _, t := range add {
		b.WriteString(t + nl)
	}
	for _, l := range lines[last+1:] {
		b.WriteString(l)
	}
	return b.String(), true
}

// emptyMessage reports whether text holds nothing but blank lines, comments
// and Signed-off-by lines, which is what git calls an empty message.
func emptyMessage(text []string, comment string) bool {
	for _, l := range text {
		if !isBlank(l) && !strings.HasPrefix(l, comment) && !strings.HasPrefix(l, "Signed-off-by:") {
			return false
		}
	}
	return true
}

// hasTrailer reports whether a line of text, comments aside, starts with
// "<key>:" (case-insensitive, as git compares trailer keys).
func hasTrailer(text []string, comment, key string) bool {
	prefix := strings.ToLower(key) + ":"
	for _, l := range text {
		if !strings.HasPrefix(l, comment) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), prefix) {
			return true
		}
	}
	return false
}

// endsWithTrailers reports whether text ends with a trailer block: a last
// paragraph, after the subject, made only of trailers, their indented
// continuation lines and comments.
func endsWithTrailers(text []string, comment string) bool {
	i := len(text) - 1
	trailers := 0
	for ; i >= 0 && !isBlank(text[i]); i-- {
		l := text[i]
		switch {
		case strings.HasPrefix(l, comment):
		case trailerLine.MatchString(l):
			trailers++
		case l[0] == ' ' || l[0] == '\t':
		default:
			return false
		}
	}
	if trailers == 0 {
		return false
	}
	// The first paragraph is the subject, never a trailer block.
	for ; i >= 0; i-- {
		if !isBlank(text[i]) && !strings.HasPrefix(text[i], comment) {
			return true
		}
	}
	return false
}

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }
