package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/rules"
	"github.com/crewlet/gitlet/internal/symbols"
)

const (
	// maxCandidates caps the candidates a refusal lists (docs/SPEC.md,
	// section 4.1).
	maxCandidates = 10
	// grepMax caps the files git grep returns per revision.
	grepMax = 50
	// minIDPrefix is the shortest id prefix accepted after n-, p- or t-.
	minIDPrefix = 4
)

// ---- symbols ----

// symbolQuery is a parsed symbol argument (docs/SPEC.md, section 4.1).
type symbolQuery struct {
	Path      string // "" when the argument names no file
	Qualified string // canonical Qualified, aliases normalized
}

func (q symbolQuery) String() string {
	if q.Path != "" {
		return q.Path + ":" + q.Qualified
	}
	return q.Qualified
}

// parseSymbolArg parses a symbol argument in canonical form
// [path:]Qualified, accepting the aliases "::", "->" and "(*T).M". The text
// before the first ":" is a path when isPath says it names a file; a single
// ":" after anything else is an error, so a mistyped path is reported
// rather than searched for as a name.
func parseSymbolArg(arg string, isPath func(string) bool) (symbolQuery, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return symbolQuery{}, errors.New("no symbol given")
	}
	if p, rest, ok := model.SplitTarget(s); ok {
		p = cleanRelPath(p)
		if !isPath(p) {
			return symbolQuery{}, fmt.Errorf("%s is not a file tracked at HEAD, changed in the work tree, or changed by anyone", p)
		}
		q := model.NormalizeQualified(rest)
		if q == "" {
			return symbolQuery{}, fmt.Errorf("no symbol name after %q", p+":")
		}
		return symbolQuery{Path: p, Qualified: q}, nil
	}
	q := model.NormalizeQualified(s)
	if q == "" || strings.ContainsAny(q, " \t\n:") {
		return symbolQuery{}, fmt.Errorf("%q is not a symbol name; use [path:]Qualified, e.g. src/config.ts:parseConfig or Config.load", s)
	}
	return symbolQuery{Qualified: q}, nil
}

// symbolMatches reports whether s is exactly the symbol q names.
func (q symbolQuery) matches(s model.Symbol) bool {
	return s.Qualified == q.Qualified && (q.Path == "" || s.Path == q.Path)
}

// near reports whether s is a plausible candidate for q that does not match
// exactly: its qualified name ends in "."+q, or its name equals q's last
// part, ignoring case.
func (q symbolQuery) near(s model.Symbol) bool {
	if q.Path != "" && s.Path != q.Path {
		return false
	}
	return strings.HasSuffix(s.Qualified, "."+q.Qualified) ||
		strings.EqualFold(s.Qualified, q.Qualified) ||
		strings.EqualFold(s.Name, lastPart(q.Qualified))
}

// lastPart returns the member name of a qualified name, without the
// "#2" or "@setter" disambiguation suffixes.
func lastPart(q string) string {
	if i := strings.IndexAny(q, "#@"); i >= 0 {
		q = q[:i]
	}
	if i := strings.LastIndexByte(q, '.'); i >= 0 {
		q = q[i+1:]
	}
	return q
}

// pickResult is the outcome of symbol resolution.
type pickResult struct {
	Symbol     model.Symbol
	Found      bool
	Ambiguous  bool
	Candidates []model.Symbol // when not found: the ambiguous matches or near misses
}

// pickSymbol applies the resolution order to candidate symbols gathered in
// stages: the first stage with exact matches decides, resolving when they
// are one symbol (by key) and being ambiguous otherwise. Without any exact
// match, near misses from every stage become the candidates.
func pickSymbol(q symbolQuery, stages ...[]model.Symbol) pickResult {
	for _, stage := range stages {
		var exact []model.Symbol
		for _, s := range stage {
			if q.matches(s) {
				exact = append(exact, s)
			}
		}
		exact = uniqueSymbols(exact)
		switch {
		case len(exact) == 1:
			return pickResult{Symbol: exact[0], Found: true}
		case len(exact) > 1:
			return pickResult{Ambiguous: true, Candidates: capSymbols(exact)}
		}
	}
	var near []model.Symbol
	for _, stage := range stages {
		for _, s := range stage {
			if q.near(s) {
				near = append(near, s)
			}
		}
	}
	return pickResult{Candidates: capSymbols(uniqueSymbols(near))}
}

// uniqueSymbols drops repeated symbols (same key, else same path and
// qualified name), keeping the first occurrence.
func uniqueSymbols(in []model.Symbol) []model.Symbol {
	seen := map[string]bool{}
	var out []model.Symbol
	for _, s := range in {
		k := s.Key
		if k == "" {
			k = s.Lang + "\x00" + s.Ref()
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// capSymbols sorts candidates canonically and keeps maxCandidates.
func capSymbols(in []model.Symbol) []model.Symbol {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Ref() != in[j].Ref() {
			return in[i].Ref() < in[j].Ref()
		}
		return in[i].StartLine < in[j].StartLine
	})
	if len(in) > maxCandidates {
		in = in[:maxCandidates]
	}
	return in
}

// resolveSymbol resolves a symbol argument for verb (docs/SPEC.md, section
// 4.1). It looks in my footprints, then in peers' footprints and claims,
// then in the files git grep -w finds at HEAD and at main (or, with a
// path, in that file in my latest snapshot, at HEAD and at main), parsing
// only those. On an unknown or ambiguous name it returns a refusal listing
// up to 10 candidates in canonical form with kind and line, each an option
// calling verb again with symbol set; extra are key/value pairs added to
// those options (toolOption's form, e.g. "note", id). err is only for
// gitlet failures. Callers hold e.mu.
func (e *Engine) resolveSymbol(ctx context.Context, verb, arg string, extra ...any) (model.Symbol, *api.Result, error) {
	isPath := e.pathChecker(ctx)
	q, err := parseSymbolArg(arg, isPath)
	if err != nil {
		r := refused(fmt.Sprintf("Cannot %s %q: %v.", verb, strings.TrimSpace(arg), err), nil,
			toolOption("name the symbol as path:Qualified", verb, extra...))
		return model.Symbol{}, &r, nil
	}
	mine, theirs := e.footprintSymbols()
	res := pickSymbol(q, mine, theirs)
	if !res.Found && !res.Ambiguous {
		found, err := e.grepSymbols(ctx, q)
		if err != nil {
			return model.Symbol{}, nil, err
		}
		res = pickSymbol(q, mine, theirs, found)
	}
	if res.Found {
		return res.Symbol, nil, nil
	}
	opts := make([]model.Option, 0, len(res.Candidates))
	var details []string
	for _, s := range res.Candidates {
		label := fmt.Sprintf("%s, line %d", s.Kind, s.StartLine)
		details = append(details, fmt.Sprintf("%s (%s)", s.Ref(), label))
		opts = append(opts, toolOption(label, verb, append([]any{"symbol", s.Ref()}, extra...)...))
	}
	var summary string
	if res.Ambiguous {
		summary = fmt.Sprintf("%q is ambiguous: name one of these symbols.", q.String())
	} else {
		summary = fmt.Sprintf("Unknown symbol %q.", q.String())
		if len(opts) > 0 {
			summary += " Did you mean one of these?"
		} else {
			details = append(details, "use the canonical form path:Qualified, e.g. src/config.ts:parseConfig or internal/server.go:Server.Start")
		}
	}
	r := refused(summary, details, opts...)
	return model.Symbol{}, &r, nil
}

// footprintSymbols returns the symbols my open items change, and those
// that other items in view change or that are claimed.
func (e *Engine) footprintSymbols() (mine, theirs []model.Symbol) {
	if e.shared == nil {
		return nil, nil
	}
	for _, it := range e.myState().Items {
		if it.Status.Open() {
			for _, sc := range it.Footprint.Symbols {
				mine = append(mine, sc.Symbol)
			}
		}
	}
	for _, id := range e.peerIDs() {
		for _, it := range e.shared.Agents[id].Items {
			if it.Status.Open() {
				for _, sc := range it.Footprint.Symbols {
					theirs = append(theirs, sc.Symbol)
				}
			}
		}
	}
	keys := make([]string, 0, len(e.shared.Claims))
	for k := range e.shared.Claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		theirs = append(theirs, e.shared.Claims[k].Claim.Symbol)
	}
	return mine, theirs
}

// grepSymbols parses the files that may define q: q's file in my latest
// snapshot, at HEAD and at main, or else the files git grep -w finds for
// q's member name at HEAD and at main.
func (e *Engine) grepSymbols(ctx context.Context, q symbolQuery) ([]model.Symbol, error) {
	revs := []string{"HEAD"}
	if main := e.mainOID(ctx); main != "" {
		revs = append(revs, main)
	}
	type file struct{ rev, path string }
	var files []file
	if q.Path != "" {
		if e.snap.Commit != "" {
			files = append(files, file{e.snap.Commit, q.Path})
		}
		for _, rev := range revs {
			files = append(files, file{rev, q.Path})
		}
	} else {
		word := lastPart(q.Qualified)
		for _, rev := range revs {
			paths, err := e.bg.Grep(ctx, rev, word, grepMax)
			if err != nil {
				e.log.Debug("grep", "rev", rev, "word", word, "err", err)
				continue
			}
			for _, p := range paths {
				if symbols.Supported(p) {
					files = append(files, file{rev, p})
				}
			}
		}
	}
	var out []model.Symbol
	for _, f := range files {
		parsed, err := e.fileSymbols(ctx, f.rev, f.path)
		if err != nil {
			e.log.Debug("parse", "rev", f.rev, "path", f.path, "err", err)
			continue
		}
		if parsed != nil {
			out = append(out, parsed.Symbols...)
		}
	}
	return out, nil
}

// pathChecker returns the test parseSymbolArg and parseTarget use for "is
// this a file": tracked at HEAD, changed in the work tree, or changed in a
// footprint or claimed in view. The work tree status is read once.
func (e *Engine) pathChecker(ctx context.Context) func(string) bool {
	var changed map[string]bool
	return func(p string) bool {
		if p == "" || p == "." {
			return false
		}
		if e.pathInView(p) {
			return true
		}
		if changed == nil {
			changed = map[string]bool{}
			if entries, err := e.bg.Status(ctx); err == nil {
				for _, s := range entries {
					changed[s.Path] = true
					if s.OrigPath != "" {
						changed[s.OrigPath] = true
					}
				}
			}
		}
		return changed[p] || e.objectType(ctx, "HEAD:"+p) == "blob"
	}
}

// dirChecker returns the test for "is this a directory" (at HEAD or in the
// work tree).
func (e *Engine) dirChecker(ctx context.Context) func(string) bool {
	return func(p string) bool {
		if p == "" || p == "." {
			return false
		}
		if fi, err := os.Stat(filepath.Join(e.repo.Root, filepath.FromSlash(p))); err == nil && fi.IsDir() {
			return true
		}
		return e.objectType(ctx, "HEAD:"+p) == "tree"
	}
}

// pathInView reports whether a footprint in view changes p or a claim is on a symbol in p.
func (e *Engine) pathInView(p string) bool {
	if e.shared == nil {
		return false
	}
	for _, st := range e.shared.Agents {
		for _, it := range st.Items {
			if it.Status.Open() && changedFiles(it.Footprint)[p] {
				return true
			}
		}
	}
	for _, cv := range e.shared.Claims {
		if cv.Claim.Symbol.Path == p {
			return true
		}
	}
	return false
}

// objectType returns git's type of an object expression ("blob", "tree",
// ...), or "" when it does not exist.
func (e *Engine) objectType(ctx context.Context, spec string) string {
	t, err := e.bg.Run(ctx, "cat-file", "-t", spec)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(t)
}

// cleanRelPath normalizes a repository-relative path argument.
func cleanRelPath(p string) string {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	return strings.TrimPrefix(p, "./")
}

// cleanPaths turns path arguments into clean slash-separated paths relative
// to the worktree root. Absolute paths must lie inside root; bad lists the
// arguments that do not.
func cleanPaths(root string, in []string) (out, bad []string) {
	seen := map[string]bool{}
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				bad = append(bad, raw)
				continue
			}
			p = rel
		}
		p = cleanRelPath(p)
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			bad = append(bad, raw)
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, bad
}

// ---- who and why targets ----

type targetKind int

const (
	targetPath   targetKind = iota // a file
	targetDir                      // a directory: every file under it
	targetLine                     // path:line (why only)
	targetSymbol                   // a symbol, resolved by resolveSymbol
)

// target is a parsed who or why argument.
type target struct {
	Kind   targetKind
	Path   string
	Line   int
	Raw    string       // the argument as given, trimmed
	Symbol model.Symbol // for targetSymbol, once resolved
}

// Name is the target in canonical form.
func (t target) Name() string {
	switch t.Kind {
	case targetLine:
		return t.Path + ":" + strconv.Itoa(t.Line)
	case targetSymbol:
		if t.Symbol.Path != "" {
			return t.Symbol.Ref()
		}
		return t.Raw
	case targetDir:
		return strings.TrimSuffix(t.Path, "/") + "/"
	}
	return t.Path
}

// parseTarget classifies a who or why argument (docs/SPEC.md, section 4.1):
// a file, a directory, path:line when allowLine, or else a symbol for
// resolveSymbol.
func parseTarget(arg string, isFile, isDir func(string) bool, allowLine bool) (target, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return target{}, errors.New("no target given")
	}
	t := target{Raw: s}
	if p := cleanRelPath(s); isFile(p) {
		t.Kind, t.Path = targetPath, p
		return t, nil
	}
	if p := cleanRelPath(strings.TrimSuffix(s, "/")); isDir(p) {
		t.Kind, t.Path = targetDir, p
		return t, nil
	}
	if pp, rest, ok := model.SplitTarget(s); ok && isDigits(rest) && isFile(cleanRelPath(pp)) {
		if !allowLine {
			return target{}, errors.New("give a path or a symbol; path:line works with why")
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			return target{}, fmt.Errorf("line %q is not a line number", rest)
		}
		t.Kind, t.Path, t.Line = targetLine, cleanRelPath(pp), n
		return t, nil
	}
	t.Kind = targetSymbol
	return t, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveTarget parses and resolves a who or why argument for verb. A
// symbol target is resolved with resolveSymbol; its refusal is returned as
// is. Callers hold e.mu.
func (e *Engine) resolveTarget(ctx context.Context, verb, arg string, allowLine bool) (target, *api.Result, error) {
	t, err := parseTarget(arg, e.pathChecker(ctx), e.dirChecker(ctx), allowLine)
	if err != nil {
		example := "src/config.ts or src/config.ts:parseConfig"
		if allowLine {
			example = "src/config.ts, src/config.ts:42 or src/config.ts:parseConfig"
		}
		r := refused(fmt.Sprintf("Cannot %s %q: %v.", verb, strings.TrimSpace(arg), err), []string{"targets look like " + example},
			toolOption("name a file or symbol", verb))
		return target{}, &r, nil
	}
	if t.Kind != targetSymbol {
		return t, nil, nil
	}
	sym, refusal, err := e.resolveSymbolArg(ctx, verb, "target", t.Raw)
	if err != nil || refusal != nil {
		return t, refusal, err
	}
	t.Symbol, t.Path = sym, sym.Path
	return t, nil, nil
}

// resolveSymbolArg is resolveSymbol for verbs whose symbol argument has
// another name (who and why call it target): candidate options set argName.
func (e *Engine) resolveSymbolArg(ctx context.Context, verb, argName, arg string) (model.Symbol, *api.Result, error) {
	sym, r, err := e.resolveSymbol(ctx, verb, arg)
	if r != nil && argName != "symbol" {
		for i, o := range r.Options {
			if v, ok := o.Args["symbol"]; ok && o.Tool == verb {
				delete(o.Args, "symbol")
				o.Args[argName] = v
				r.Options[i] = o
			}
		}
	}
	return sym, r, err
}

// ---- id prefixes ----

var (
	errIDShort     = errors.New("too short")
	errIDInvalid   = errors.New("not an id")
	errIDUnknown   = errors.New("unknown")
	errIDAmbiguous = errors.New("ambiguous")
)

// normalizeID checks an id argument: prefix ("n-", "p-" or "t-") may be
// left out, at least minIDPrefix hex characters must follow it. It returns
// the lowercased id with its prefix.
func normalizeID(prefix, arg string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(arg))
	if !strings.HasPrefix(s, prefix) {
		s = prefix + s
	}
	rest := s[len(prefix):]
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", errIDInvalid
		}
	}
	if len(rest) < minIDPrefix {
		return "", errIDShort
	}
	return s, nil
}

// matchID resolves arg, a full id or a unique prefix of at least four
// characters after prefix, against ids, like git short oids. An exact match
// wins over longer ids sharing it as a prefix. When ambiguous, the matching
// ids are returned, sorted.
func matchID(prefix, arg string, ids []string) (string, []string, error) {
	s, err := normalizeID(prefix, arg)
	if err != nil {
		return "", nil, err
	}
	var matches []string
	for _, id := range ids {
		if id == s {
			return id, nil, nil
		}
		if strings.HasPrefix(id, s) {
			matches = append(matches, id)
		}
	}
	sort.Strings(matches)
	matches = compactStrings(matches)
	switch len(matches) {
	case 0:
		return "", nil, errIDUnknown
	case 1:
		return matches[0], nil, nil
	}
	return "", matches, errIDAmbiguous
}

func compactStrings(ss []string) []string {
	out := ss[:0]
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// idRefusal explains why an id argument did not resolve. what names the
// kind ("note", "proposal", "thread"); candidates become options calling
// verb with argName set to each.
func idRefusal(verb, argName, what, arg string, err error, candidates []string, labels map[string]string) api.Result {
	prefix := map[string]string{"note": "n-", "proposal": "p-", "thread": "t-"}[what]
	var summary string
	switch {
	case errors.Is(err, errIDShort):
		summary = fmt.Sprintf("%q is too short: give at least %d characters after %s.", arg, minIDPrefix, prefix)
	case errors.Is(err, errIDInvalid):
		summary = fmt.Sprintf("%q is not a %s id (%s followed by hex digits).", arg, what, prefix)
	case errors.Is(err, errIDAmbiguous):
		summary = fmt.Sprintf("%q matches several %ss: give more characters.", arg, what)
	default:
		summary = fmt.Sprintf("No %s matches %q.", what, arg)
	}
	var opts []model.Option
	for i, id := range candidates {
		if i == maxCandidates {
			break
		}
		label := labels[id]
		if label == "" {
			label = id
		}
		opts = append(opts, toolOption(label, verb, argName, id))
	}
	opts = append(opts, statusOption())
	return refused(summary, nil, opts...)
}

// resolveNoteID resolves a note argument (full id or unique prefix of at
// least four characters after "n-") to one of this agent's stored notes,
// open or not. Otherwise it returns a refusal for verb whose options
// repeat the call with argName set to each candidate. err is only for
// gitlet failures.
func (e *Engine) resolveNoteID(ctx context.Context, verb, argName, arg string) (ledger.StoredNote, *api.Result, error) {
	id, err := normalizeID("n-", arg)
	if err != nil {
		r := idRefusal(verb, argName, "note", arg, err, nil, nil)
		return ledger.StoredNote{}, &r, nil
	}
	sn, err := e.notes.Note(ctx, e.me.ID, id)
	switch {
	case err == nil:
		return sn, nil, nil
	case errors.Is(err, ledger.ErrNotFound), errors.Is(err, ledger.ErrAmbiguous):
		var cands []string
		labels := map[string]string{}
		if open, oerr := e.notes.OpenNotes(ctx, e.me.ID); oerr == nil {
			for _, s := range open {
				if strings.HasPrefix(s.Note.ID, id) {
					cands = append(cands, s.Note.ID)
					labels[s.Note.ID] = s.Note.Title
				}
			}
		}
		kind := errIDUnknown
		if errors.Is(err, ledger.ErrAmbiguous) {
			kind = errIDAmbiguous
		}
		r := idRefusal(verb, argName, "note", arg, kind, cands, labels)
		return ledger.StoredNote{}, &r, nil
	}
	return ledger.StoredNote{}, nil, fmt.Errorf("engine: note %s: %w", arg, err)
}

// threads returns the threads this agent takes part in, assembled from a
// light view (rules.Threads), sorted by id.
func (e *Engine) threads(ctx context.Context) []model.ThreadState {
	ts := rules.Threads(e.lightView(ctx))
	sort.Slice(ts, func(i, j int) bool { return ts[i].ID < ts[j].ID })
	return ts
}

// resolveThreadID resolves a thread argument ("t-" plus a unique prefix of
// at least four characters) among this agent's threads, or returns a
// refusal for verb with candidate options setting argName.
func (e *Engine) resolveThreadID(ctx context.Context, verb, argName, arg string) (model.ThreadState, *api.Result) {
	ts := e.threads(ctx)
	ids := make([]string, len(ts))
	labels := map[string]string{}
	for i, t := range ts {
		ids[i] = t.ID
		labels[t.ID] = "thread about " + firstNonEmpty(t.SubjectName, t.Subject)
	}
	id, cands, err := matchID("t-", arg, ids)
	if err != nil {
		r := idRefusal(verb, argName, "thread", arg, err, cands, labels)
		return model.ThreadState{}, &r
	}
	for _, t := range ts {
		if t.ID == id {
			return t, nil
		}
	}
	return model.ThreadState{}, nil
}

// resolveProposalID resolves a proposal argument ("p-" plus a unique prefix
// of at least four characters) among the proposals in this agent's threads,
// returning it with its thread, or a refusal for verb.
func (e *Engine) resolveProposalID(ctx context.Context, verb, argName, arg string) (model.Proposal, model.ThreadState, *api.Result) {
	type found struct {
		p model.Proposal
		t model.ThreadState
	}
	by := map[string]found{}
	var ids []string
	labels := map[string]string{}
	for _, t := range e.threads(ctx) {
		for _, m := range t.Messages {
			if m.Proposal != nil && m.Proposal.ID != "" {
				if _, ok := by[m.Proposal.ID]; !ok {
					ids = append(ids, m.Proposal.ID)
				}
				by[m.Proposal.ID] = found{*m.Proposal, t}
				// The summary is the author's text: quoted and attributed.
				labels[m.Proposal.ID] = fmt.Sprintf("%s's proposal: %q", m.From, m.Proposal.Summary)
			}
		}
	}
	id, cands, err := matchID("p-", arg, ids)
	if err != nil {
		r := idRefusal(verb, argName, "proposal", arg, err, cands, labels)
		return model.Proposal{}, model.ThreadState{}, &r
	}
	f := by[id]
	return f.p, f.t, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
