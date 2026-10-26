// Package symbols extracts definitions, references and imports from source
// files with tree-sitter (docs/SPEC.md, sections 9.2 to 9.5). It knows Go,
// TypeScript, TSX, JavaScript, Python and Rust; other files are reported as
// unsupported and tracked at file level by callers.
//
// Each language lives in its own file (lang_go.go, lang_ts.go, lang_python.go,
// lang_rust.go) and implements one extract function on top of the builder in
// extract.go, which owns keys, unique names, hashing and ordering.
package symbols

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/crewlet/gitlet/internal/model"
)

// ImportKind says how an import binds names.
type ImportKind string

const (
	ImportNamed     ImportKind = "named"     // import { a as b } from "m"; from m import a as b; use m::a as b
	ImportDefault   ImportKind = "default"   // import b from "m"
	ImportNamespace ImportKind = "namespace" // import * as b from "m"; import m as b; Go import; use m (module)
	ImportGlob      ImportKind = "glob"      // from m import *; use m::*; Go dot import
)

// Import is one name binding created by an import statement.
type Import struct {
	Local  string     // the name bound in this file ("" for side-effect imports)
	Name   string     // the imported name for named imports, "" otherwise
	Module string     // the module as written: "./config", "github.com/x/y/pkg", "pkg.config", ".config", "crate::config"
	Kind   ImportKind //
	Line   int
}

// File is what Extract finds in one file.
type File struct {
	Lang    string
	Package string         // Go package name, "" for other languages
	Symbols []model.Symbol // every definition, nested ones included, sorted by StartByte
	Refs    []model.Ref    // every reference in the file, unresolved (Module "" and Resolved false), sorted by line
	Imports []Import       // sorted by line
}

// LangForPath returns gitlet's language name for a path ("go", "typescript",
// "tsx", "javascript", "python", "rust"), or "" when unsupported.
func LangForPath(path string) string { return langForPath(path) }

// Supported reports whether path can be parsed.
func Supported(path string) bool { return langForPath(path) != "" }

// Extract parses src as the file at path. Unsupported paths return (nil, nil).
// Extract never panics on malformed input: tree-sitter recovers from syntax
// errors, and a panic inside a language extractor is returned as an error.
// It is safe for concurrent use.
func Extract(path string, src []byte) (*File, error) { return extract(path, src) }

// SigHash hashes a normalized structural signature.
func SigHash(sig string) string { return hash16(sig) }

// BodyHash hashes a normalized body (the joined leaf tokens).
func BodyHash(body string) string { return hash16(body) }

// RefsWithin returns the refs whose line falls in [start, end].
func RefsWithin(refs []model.Ref, start, end int) []model.Ref {
	var out []model.Ref
	for _, r := range refs {
		if r.Line >= start && r.Line <= end {
			out = append(out, r)
		}
	}
	return out
}

// Find returns the symbol with the given qualified name, if any.
func (f *File) Find(qualified string) (model.Symbol, bool) {
	if f == nil {
		return model.Symbol{}, false
	}
	for _, s := range f.Symbols {
		if s.Qualified == qualified {
			return s, true
		}
	}
	return model.Symbol{}, false
}

func hash16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}
