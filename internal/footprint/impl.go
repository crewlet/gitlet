package footprint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// Defaults for zero Options fields (docs/SPEC.md, sections 9.2 and 15).
const (
	defaultMaxFileBytes = 2 << 20
	defaultBroadFiles   = 50
	defaultBroadSymbols = 200
)

// computer holds the state of one Compute call.
type computer struct {
	src        Source
	base, head string
	opt        Options

	goModule     string
	goModuleRead bool

	parsed    int // files handed to the extractor, for MaxParsed
	truncated bool

	files   []model.FileChange
	changes []model.SymbolChange
	refs    []model.Ref
}

func compute(ctx context.Context, src Source, base, head string, opt Options) (Result, error) {
	if src == nil {
		return Result{}, errors.New("footprint: nil source")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("footprint: %w", err)
	}
	opt = withDefaults(opt)
	entries, err := src.DiffNameStatus(ctx, base, head)
	if err != nil {
		return Result{}, fmt.Errorf("footprint: diff %s..%s: %w", base, head, err)
	}

	c := &computer{
		src: src, base: base, head: head, opt: opt,
		goModule: opt.GoModule, goModuleRead: opt.GoModule != "",
	}
	// Files are visited in path order so that MaxParsed always parses the
	// same files.
	for _, e := range sortedEntries(entries) {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("footprint: %w", err)
		}
		if err := c.file(ctx, e); err != nil {
			return Result{}, err
		}
	}

	sortFiles(c.files)
	sortChanges(c.changes)
	fp := model.Footprint{
		Base:       base,
		Head:       head,
		Files:      c.files,
		Symbols:    c.changes,
		Refs:       sortRefs(c.refs),
		ComputedAt: opt.Now,
	}
	fp.Broad = len(fp.Files) > opt.BroadFiles || len(fp.Symbols) > opt.BroadSymbols
	return Result{Footprint: fp, Truncated: c.truncated}, nil
}

func withDefaults(opt Options) Options {
	if opt.MaxFileBytes <= 0 {
		opt.MaxFileBytes = defaultMaxFileBytes
	}
	if opt.BroadFiles <= 0 {
		opt.BroadFiles = defaultBroadFiles
	}
	if opt.BroadSymbols <= 0 {
		opt.BroadSymbols = defaultBroadSymbols
	}
	if opt.Extract == nil {
		opt.Extract = symbols.Extract
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	return opt
}

// sortedEntries returns the entries with a path, sorted by path.
func sortedEntries(entries []gitx.DiffEntry) []gitx.DiffEntry {
	out := make([]gitx.DiffEntry, 0, len(entries))
	for _, e := range entries {
		if e.Path != "" {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b gitx.DiffEntry) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.OldPath, b.OldPath), cmp.Compare(a.Status, b.Status))
	})
	return out
}

// sides returns the paths of the base and head versions of a changed file,
// "" for a side that does not exist. A copy leaves its source untouched, so
// only the new path counts.
func sides(e gitx.DiffEntry) (oldPath, newPath string) {
	switch e.Status {
	case "A", "C":
		return "", e.Path
	case "D":
		return e.Path, ""
	case "R":
		if e.OldPath != "" {
			return e.OldPath, e.Path
		}
	}
	return e.Path, e.Path
}

// file records one changed file and, when it can be parsed, its symbol
// changes and the refs its new code makes.
func (c *computer) file(ctx context.Context, e gitx.DiffEntry) error {
	fc := model.FileChange{Path: e.Path, Status: e.Status, Lang: symbols.LangForPath(e.Path)}
	if e.Status == "R" || e.Status == "C" {
		fc.OldPath = e.OldPath
	}
	parsed, err := c.parse(ctx, e)
	if err != nil {
		return err
	}
	fc.Parsed = parsed
	c.files = append(c.files, fc)
	return nil
}

// version is one side of a changed file.
type version struct {
	path string
	src  []byte
	file *symbols.File
}

// parse extracts both versions of a changed file and records what changed.
// It reports false when the file is tracked at file level only: an
// unsupported language, a file over MaxFileBytes, the MaxParsed cap, or an
// extractor that fails on either version.
func (c *computer) parse(ctx context.Context, e gitx.DiffEntry) (bool, error) {
	oldPath, newPath := sides(e)
	if (oldPath != "" && !symbols.Supported(oldPath)) || (newPath != "" && !symbols.Supported(newPath)) {
		return false, nil
	}
	if c.opt.MaxParsed > 0 && c.parsed >= c.opt.MaxParsed {
		c.truncated = true
		return false, nil
	}

	oldV, err := c.read(ctx, c.base, oldPath)
	if err != nil {
		return false, err
	}
	newV, err := c.read(ctx, c.head, newPath)
	if err != nil {
		return false, err
	}
	if oldV == nil && newV == nil {
		return false, nil
	}
	for _, v := range []*version{oldV, newV} {
		if v != nil && int64(len(v.src)) > c.opt.MaxFileBytes {
			return false, nil
		}
	}

	c.parsed++
	for _, v := range []*version{oldV, newV} {
		if v == nil {
			continue
		}
		f, err := c.opt.Extract(v.path, v.src)
		if err != nil || f == nil {
			// A file the extractor cannot handle is still tracked at file
			// level; one bad file does not cost the whole footprint.
			return false, nil
		}
		v.file = f
	}

	var changes []model.SymbolChange
	if e.Status == "R" && oldV != nil && newV != nil {
		changes = DiffMoved(oldV.file, newV.file)
	} else {
		changes = DiffSymbols(fileOf(oldV), fileOf(newV))
	}
	c.changes = append(c.changes, changes...)

	if newV != nil {
		refs := changedRefs(oldV, newV, changes)
		if len(refs) > 0 {
			goModule := ""
			if newV.file.Lang == "go" || symbols.LangForPath(newV.path) == "go" {
				if goModule, err = c.goMod(ctx); err != nil {
					return false, err
				}
			}
			c.refs = append(c.refs, Resolve(newV.file, refs, goModule)...)
		}
	}
	return true, nil
}

func fileOf(v *version) *symbols.File {
	if v == nil {
		return nil
	}
	return v.file
}

// read returns the file at rev, or nil when path is "" or the file does not
// exist there.
func (c *computer) read(ctx context.Context, rev, path string) (*version, error) {
	if path == "" {
		return nil, nil
	}
	src, err := c.src.ReadFile(ctx, rev, path)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("footprint: read %s at %s: %w", path, rev, err)
	}
	return &version{path: path, src: src}, nil
}

// goMod returns the module path of the root go.mod at head, read once.
func (c *computer) goMod(ctx context.Context) (string, error) {
	if c.goModuleRead {
		return c.goModule, nil
	}
	data, err := c.src.ReadFile(ctx, c.head, "go.mod")
	switch {
	case err == nil:
		c.goModule = goModulePath(data)
	case !isNotFound(err):
		return "", fmt.Errorf("footprint: read go.mod at %s: %w", c.head, err)
	}
	c.goModuleRead = true
	return c.goModule, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, gitx.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

func sortFiles(files []model.FileChange) {
	slices.SortStableFunc(files, func(a, b model.FileChange) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.OldPath, b.OldPath), cmp.Compare(a.Status, b.Status))
	})
}

// sortRefs sorts refs and drops exact duplicates.
func sortRefs(refs []model.Ref) []model.Ref {
	slices.SortFunc(refs, compareRefs)
	return slices.Compact(refs)
}

func compareRefs(a, b model.Ref) int {
	return cmp.Or(
		cmp.Compare(a.Path, b.Path),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Qualifier, b.Qualifier),
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.Lang, b.Lang),
		cmp.Compare(a.Module, b.Module),
		boolCompare(a.Resolved, b.Resolved),
	)
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}
