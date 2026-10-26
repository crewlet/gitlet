package footprint

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/gitlet/internal/gitx"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

// fakeSource serves a diff and file contents from in-memory revisions.
type fakeSource struct {
	entries []gitx.DiffEntry
	diffErr error
	revs    map[string]map[string]string // rev -> path -> content
	errs    map[string]error             // "rev:path" -> read error
	onRead  func()                       // called before every read

	mu    sync.Mutex
	reads []string
}

func (s *fakeSource) DiffNameStatus(ctx context.Context, from, to string) ([]gitx.DiffEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.entries, s.diffErr
}

func (s *fakeSource) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	if s.onRead != nil {
		s.onRead()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.reads = append(s.reads, rev+":"+path)
	s.mu.Unlock()
	if err := s.errs[rev+":"+path]; err != nil {
		return nil, err
	}
	if src, ok := s.revs[rev][path]; ok {
		return []byte(src), nil
	}
	return nil, fmt.Errorf("fake: %s:%s: %w", rev, path, gitx.ErrNotFound)
}

func (s *fakeSource) read(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reads {
		if r == key {
			return true
		}
	}
	return false
}

// fakeExtractor returns hand-built files by path and content.
type fakeExtractor struct {
	files map[extKey]*symbols.File
	errs  map[string]error // by path

	mu    sync.Mutex
	calls []string
}

type extKey struct{ path, src string }

func (x *fakeExtractor) extract(path string, src []byte) (*symbols.File, error) {
	x.mu.Lock()
	x.calls = append(x.calls, path)
	x.mu.Unlock()
	if err := x.errs[path]; err != nil {
		return nil, err
	}
	if f, ok := x.files[extKey{path, string(src)}]; ok {
		return f, nil
	}
	if !symbols.Supported(path) {
		return nil, nil
	}
	return &symbols.File{Lang: symbols.LangForPath(path)}, nil
}

func (x *fakeExtractor) called(path string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, c := range x.calls {
		if c == path {
			return true
		}
	}
	return false
}

// repo ties revisions to what the fake extractor finds in each file.
type repo struct {
	src *fakeSource
	ext *fakeExtractor
}

func newRepo(entries ...gitx.DiffEntry) *repo {
	return &repo{
		src: &fakeSource{entries: entries, revs: map[string]map[string]string{}, errs: map[string]error{}},
		ext: &fakeExtractor{files: map[extKey]*symbols.File{}, errs: map[string]error{}},
	}
}

// put stores content at rev; f, when not nil, is what extraction returns.
func (r *repo) put(rev, path, content string, f *symbols.File) {
	if r.src.revs[rev] == nil {
		r.src.revs[rev] = map[string]string{}
	}
	r.src.revs[rev][path] = content
	if f != nil {
		r.ext.files[extKey{path, content}] = f
	}
}

// putFile stores content at rev along with the symbols, refs and imports
// extraction finds in it.
func (r *repo) putFile(rev, path, content string, defs []def, refs []model.Ref, imports ...symbols.Import) {
	r.put(rev, path, content, mkFile(path, content, defs, refs, imports...))
}

func (r *repo) opts() Options {
	return Options{Extract: r.ext.extract, Now: testNow, GoModule: "example.com/m"}
}

// def describes one symbol for mkFile.
type def struct {
	q        string // qualified name: "name" or "Container.name"
	kind     string
	sig      string
	head     string // the signature outside params, results and members; kind and name when empty
	body     string
	start    int
	end      int
	exported bool
	params   []model.Param
	results  string
	members  []model.Member
}

// mkFile builds what an extractor would return for src: symbols with keys,
// hashes, lines and byte ranges, refs with their language and path.
func mkFile(path, src string, defs []def, refs []model.Ref, imports ...symbols.Import) *symbols.File {
	lang := symbols.LangForPath(path)
	f := &symbols.File{Lang: lang, Imports: imports}
	for _, d := range defs {
		f.Symbols = append(f.Symbols, mkSymbol(lang, path, src, d))
	}
	for _, r := range refs {
		r.Lang, r.Path = lang, path
		f.Refs = append(f.Refs, r)
	}
	return f
}

func mkSymbol(lang, path, src string, d def) model.Symbol {
	name, container := d.q, ""
	if i := strings.LastIndex(d.q, "."); i >= 0 {
		container, name = d.q[:i], d.q[i+1:]
	}
	s := model.Symbol{
		Key:       model.SymbolKey(lang, path, d.q),
		Lang:      lang,
		Path:      path,
		Name:      name,
		Qualified: d.q,
		Container: container,
		Kind:      d.kind,
		Signature: d.sig,
		Head:      cmp.Or(d.head, d.kind+" "+d.q),
		SigHash:   symbols.SigHash(d.kind + "|" + d.sig),
		BodyHash:  symbols.BodyHash(d.body),
		Params:    d.params,
		Results:   d.results,
		Members:   d.members,
		StartLine: d.start,
		EndLine:   d.end,
		Exported:  d.exported,
	}
	if src != "" {
		s.StartByte, _ = lineSpan(src, d.start)
		_, s.EndByte = lineSpan(src, d.end)
	}
	return s
}

// lineSpan returns the byte range of line n of src without surrounding
// whitespace.
func lineSpan(src string, n int) (start, end int) {
	off := 0
	for i, line := range strings.SplitAfter(src, "\n") {
		if i+1 == n {
			trimmed := strings.TrimLeft(line, " \t")
			start = off + len(line) - len(trimmed)
			end = start + len(strings.TrimRight(trimmed, " \t\r\n"))
			return start, end
		}
		off += len(line)
	}
	return 0, 0
}

func call(name, qualifier string, line int) model.Ref {
	return model.Ref{Name: name, Qualifier: qualifier, Kind: model.RefCall, Line: line}
}

// summarize renders changes as "path:Qualified change" lines, with "!" for
// breaking changes and the old name or path when set.
func summarize(cs []model.SymbolChange) []string {
	var out []string
	for _, c := range cs {
		s := c.Symbol.Path + ":" + c.Symbol.Qualified + " " + string(c.Change)
		if c.Breaking {
			s += "!"
		}
		if c.OldName != "" {
			s += " from " + c.OldName
		}
		if c.OldPath != "" {
			s += " from " + c.OldPath
		}
		if c.Change == model.ChangeMoved {
			s += fmt.Sprintf(" sig=%t body=%t", c.SigChanged, c.BodyChanged)
		}
		out = append(out, s)
	}
	return out
}

// summarizeRefs renders refs as "path:line name module" lines, with "?" for
// unresolved refs.
func summarizeRefs(refs []model.Ref) []string {
	var out []string
	for _, r := range refs {
		name := r.Name
		if r.Qualifier != "" {
			name = r.Qualifier + "." + r.Name
		}
		mod := r.Module
		if !r.Resolved {
			mod = "?"
		}
		out = append(out, fmt.Sprintf("%s:%d %s %s", r.Path, r.Line, name, mod))
	}
	return out
}
