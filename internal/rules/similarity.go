package rules

import (
	"math"
	"strings"
	"unicode"
)

// stopWords are dropped from intents before scoring.
var stopWords = setOf(
	"a", "about", "after", "all", "also", "an", "and", "any", "are", "as", "at",
	"be", "been", "before", "being", "between", "both", "but", "by", "can",
	"could", "do", "does", "each", "for", "from", "had", "has", "have", "here",
	"how", "i", "if", "in", "into", "is", "it", "its", "just", "made", "more",
	"my", "new", "no", "not", "of", "on", "onto", "only", "or", "other", "our",
	"out", "over", "per", "should", "so", "some", "such", "than", "that", "the",
	"then", "there", "these", "this", "those", "to", "under", "up", "us", "via",
	"was", "we", "were", "what", "when", "which", "while", "who", "will",
	"with", "would", "you", "your",
)

// genericVerbs say that work happens, not what it is about (docs/SPEC.md,
// section 10.2). They are compared by stem, so "updated" and "fixes" go too.
var genericVerbs = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{"add", "fix", "update", "implement", "support", "refactor", "make", "change", "improve"} {
		m[stem(w)] = true
	}
	return m
}()

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// similarity implements Similarity: TF-IDF cosine with smoothed inverse
// document frequency, idf(t) = ln((1+N)/(1+df(t))) + 1, so terms shared by
// every intent still count a little. Terms are summed in sorted order so
// the score is bit-for-bit reproducible.
func similarity(a, b string, corpus []string) (score float64, shared int) {
	ta, tb := terms(a), terms(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, 0
	}
	df := map[string]int{}
	for _, d := range corpus {
		for t := range setOf(terms(d)...) {
			df[t]++
		}
	}
	n := float64(len(corpus))
	weigh := func(ts []string) map[string]float64 {
		w := map[string]float64{}
		for _, t := range ts {
			w[t]++
		}
		for t, tf := range w {
			w[t] = tf * (math.Log((1+n)/(1+float64(df[t]))) + 1)
		}
		return w
	}
	wa, wb := weigh(ta), weigh(tb)
	var dot, na, nb float64
	for _, t := range sortedKeys(wa) {
		na += wa[t] * wa[t]
		if w, ok := wb[t]; ok {
			dot += wa[t] * w
			shared++
		}
	}
	for _, t := range sortedKeys(wb) {
		nb += wb[t] * wb[t]
	}
	if dot == 0 {
		return 0, shared
	}
	return min(1, dot/(math.Sqrt(na)*math.Sqrt(nb))), shared
}

// sharedTerms returns the content terms a and b have in common, sorted.
func sharedTerms(a, b string) []string {
	tb := setOf(terms(b)...)
	var out []string
	for _, t := range sortedKeys(setOf(terms(a)...)) {
		if tb[t] {
			out = append(out, t)
		}
	}
	return out
}

// terms returns the content terms of an intent: lowercase words with
// identifiers split, stop words and generic verbs dropped, suffixes stripped.
func terms(s string) []string {
	var out []string
	for _, w := range words(s) {
		if len([]rune(w)) < 2 || stopWords[w] {
			continue
		}
		st := stem(w)
		if genericVerbs[st] || stopWords[st] {
			continue
		}
		out = append(out, st)
	}
	return out
}

// words splits text into lowercase words at every non-alphanumeric rune
// (spaces, snake_case, kebab-case, dots) and at camelCase boundaries:
// "parseHTTPConfig" gives parse, http, config.
func words(s string) []string {
	var out []string
	rs := []rune(s)
	start := -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, strings.ToLower(string(rs[start:end])))
			start = -1
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		if start >= 0 && camelBoundary(rs, i) {
			flush(i)
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(rs))
	return out
}

// camelBoundary reports whether a new word starts at rs[i], which follows
// a letter or digit: "aB", "1B", and the last capital of an acronym that
// starts a word ("HTTPServer" splits before "S").
func camelBoundary(rs []rune, i int) bool {
	prev, r := rs[i-1], rs[i]
	if !unicode.IsUpper(r) {
		return false
	}
	if unicode.IsLower(prev) || unicode.IsDigit(prev) {
		return true
	}
	return unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
}

// stem strips the common suffixes s, es, ed and ing, carefully: plural
// first, then past or progressive, then a doubled final consonant and a
// final e, so parse, parses, parsed and parsing all become "pars". Stems
// keep at least three letters and a vowel ("string" stays), and "ss",
// "us" and "is" endings are not plurals ("class", "status", "analysis").
// Words with non-ASCII letters are left as they are.
func stem(w string) string {
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return w
		}
	}
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		w = w[:len(w)-3] + "y"
	case len(w) > 4 && hasAnySuffix(w, "sses", "shes", "ches", "xes", "zes"):
		w = w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !hasAnySuffix(w, "ss", "us", "is"):
		w = w[:len(w)-1]
	}
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ied"):
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ed") && stemmable(w[:len(w)-2]):
		w = undouble(w[:len(w)-2])
	case strings.HasSuffix(w, "ing") && stemmable(w[:len(w)-3]):
		w = undouble(w[:len(w)-3])
	}
	if len(w) >= 4 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

func stemmable(s string) bool { return len(s) >= 3 && strings.ContainsAny(s, "aeiouy") }

// undouble drops a doubled final consonant left by a suffix ("logg" from
// logging); l, s and z stay doubled ("call", "pass"), and short stems such
// as "add" are kept.
func undouble(s string) string {
	n := len(s)
	if n >= 4 && s[n-1] == s[n-2] && !strings.ContainsRune("aeiouylsz", rune(s[n-1])) {
		return s[:n-1]
	}
	return s
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, x := range suffixes {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}
