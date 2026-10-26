package rules

import (
	"slices"
	"testing"
)

func TestTerms(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"Add strict mode to config loading", []string{"strict", "mod", "config", "load"}},
		{"Make config parsing strict", []string{"config", "pars", "strict"}},
		{"parseHTTPConfig", []string{"pars", "http", "config"}},
		{"snake_case_name kebab-case-name", []string{"snak", "cas", "nam", "kebab", "cas", "nam"}},
		{"utf8Decoder", []string{"utf8", "decoder"}},
		{"Fixes, updated, implementing, supports, refactored, changes, improving, made", nil},
		{"parse parses parsed parsing", []string{"pars", "pars", "pars", "pars"}},
		{"cache caches cached caching", []string{"cach", "cach", "cach", "cach"}},
		{"settings set", []string{"set", "set"}},
		{"class classes status statuses", []string{"class", "class", "status", "status"}},
		{"retry retries retried retrying", []string{"retry", "retry", "retry", "retry"}},
		{"string strings bring", []string{"string", "string", "bring"}},
		{"log logs logging logged", []string{"log", "log", "log", "log"}},
		{"call called pass passed", []string{"call", "call", "pass", "pass"}},
		{"the a of with and", nil},
		{"Ünïcode wörds", []string{"ünïcode", "wörds"}},
	}
	for _, tt := range tests {
		if got := terms(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("terms(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	corpus := []string{
		"Add strict mode to config loading",
		"Make config loading strict with a mode flag",
		"Rewrite the HTTP router",
		"Document the release process",
		"Speed up snapshot hashing",
	}
	tests := []struct {
		name       string
		a, b       string
		min, max   float64
		wantShared int
	}{
		{"identical", corpus[0], corpus[0], 0.999, 1, 4},
		{"same task", corpus[0], corpus[1], 0.5, 1, 4},
		{"disjoint", corpus[0], corpus[2], 0, 0, 0},
		{"generic verbs only", "Fix and update", "Fix and update", 0, 0, 0},
		{"one shared term", "Rewrite the HTTP router", "Cache HTTP responses", 0.01, 0.5, 1},
		{"camel case meets words", "parseHTTPConfig", "parse the http config", 0.999, 1, 3},
		{"empty", "", corpus[0], 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, shared := Similarity(tt.a, tt.b, corpus)
			if score < tt.min || score > tt.max || shared != tt.wantShared {
				t.Fatalf("Similarity = (%.3f, %d), want [%.2f, %.2f] with %d shared", score, shared, tt.min, tt.max, tt.wantShared)
			}
			if back, n := Similarity(tt.b, tt.a, corpus); back != score || n != shared {
				t.Fatalf("not symmetric: %.6f/%d vs %.6f/%d", score, shared, back, n)
			}
		})
	}
}

func TestSimilarityIDF(t *testing.T) {
	// A term every intent uses weighs less than a rare one.
	common := []string{"config strict", "config router", "config cache", "config docs"}
	rare, _ := Similarity("config strict", "strict mode", common)
	usual, _ := Similarity("config strict", "config mode", common)
	if rare <= usual {
		t.Fatalf("rare shared term %.3f should score above common one %.3f", rare, usual)
	}
	if got, _ := Similarity("config strict", "config strict", nil); got < 0.999 {
		t.Fatalf("empty corpus: identical intents score %.3f", got)
	}
}
