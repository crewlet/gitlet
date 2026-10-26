// Package footprint computes what a work item changes (docs/SPEC.md,
// sections 9.2 to 9.6): files, symbols with breaking or compatible
// classification, and the resolved references its new code makes.
package footprint

import (
	"context"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// Source reads files at revisions and lists changed files. *gitx.Repo
// satisfies it; tests can use fakes.
type Source interface {
	DiffNameStatus(ctx context.Context, from, to string) ([]gitx.DiffEntry, error)
	ReadFile(ctx context.Context, rev, path string) ([]byte, error)
}

// Extractor parses one file; symbols.Extract by default. Callers pass a
// caching extractor to reuse work across footprints.
type Extractor func(path string, src []byte) (*symbols.File, error)

// Options tunes Compute.
type Options struct {
	MaxFileBytes int64 // larger files are tracked at file level only; 0 means 2 MiB
	BroadFiles   int   // more changed files than this marks the footprint broad; 0 means 50
	BroadSymbols int   // more changed symbols than this marks it broad; 0 means 200
	// MaxParsed caps how many changed files are parsed; beyond it files are
	// tracked at file level only and Truncated is reported. 0 means no cap.
	MaxParsed int
	// GoModule is the module path from the root go.mod; "" means Compute
	// reads go.mod at head itself.
	GoModule string
	Extract  Extractor
	Now      time.Time // ComputedAt; zero means time.Now()
}

// Result is a footprint plus facts about how it was computed.
type Result struct {
	Footprint model.Footprint
	Truncated bool // MaxParsed was reached
}

// Compute returns the footprint of head against base:
//   - Files from DiffNameStatus (with renames);
//   - Symbols from comparing old and new versions of each changed file in a
//     supported language: by key for modified files, by qualified name for
//     renamed files (moved), with renamed symbols paired by body hash, and
//     Breaking set by Classify;
//   - Refs made from added or changed code (inside added or changed symbols
//     of the new version, plus refs outside every symbol), resolved with the
//     file's imports (Resolve).
//
// The result is deterministic: files, symbols and refs are sorted.
func Compute(ctx context.Context, src Source, base, head string, opt Options) (Result, error) {
	return compute(ctx, src, base, head, opt)
}

// DiffSymbols compares the symbols of one file before and after, by key.
// old or new may be nil (file added or deleted). Renamed symbols (removed and
// added with the same kind and body hash) are paired as ChangeRenamed.
func DiffSymbols(old, new *symbols.File) []model.SymbolChange { return diffSymbols(old, new) }

// DiffMoved compares the symbols of a renamed file by qualified name: each
// pair becomes ChangeMoved with SigChanged and BodyChanged set; symbols only
// on one side are removed or added.
func DiffMoved(old, new *symbols.File) []model.SymbolChange { return diffMoved(old, new) }

// Classify reports whether the change from old to new breaks callers
// (docs/SPEC.md, section 9.4). Equal signature hashes are never breaking.
func Classify(old, new model.Symbol) (breaking bool) { return classify(old, new) }

// Resolve sets Module and Resolved on refs made in f (docs/SPEC.md,
// section 9.6), using f's imports and the Go module path.
func Resolve(f *symbols.File, refs []model.Ref, goModule string) []model.Ref {
	return resolve(f, refs, goModule)
}
