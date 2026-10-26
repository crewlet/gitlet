package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/model"
)

// ---- symbol arguments ----

func TestParseSymbolArg(t *testing.T) {
	files := map[string]bool{"src/config.ts": true, "pkg/server.go": true}
	isPath := func(p string) bool { return files[p] }
	tests := []struct {
		arg     string
		want    symbolQuery
		wantErr string
	}{
		{arg: "src/config.ts:parseConfig", want: symbolQuery{Path: "src/config.ts", Qualified: "parseConfig"}},
		{arg: "  ./src/config.ts:Config.load ", want: symbolQuery{Path: "src/config.ts", Qualified: "Config.load"}},
		{arg: "Config.load", want: symbolQuery{Qualified: "Config.load"}},
		{arg: "Config::load", want: symbolQuery{Qualified: "Config.load"}},
		{arg: "Config->load", want: symbolQuery{Qualified: "Config.load"}},
		{arg: "(*Server).Handle", want: symbolQuery{Qualified: "Server.Handle"}},
		{arg: "(Server).Handle", want: symbolQuery{Qualified: "Server.Handle"}},
		{arg: "pkg/server.go:(*Server).Handle", want: symbolQuery{Path: "pkg/server.go", Qualified: "Server.Handle"}},
		{arg: "pkg/server.go:ns::f", want: symbolQuery{Path: "pkg/server.go", Qualified: "ns.f"}},
		{arg: "", wantErr: "no symbol"},
		{arg: "missing.ts:f", wantErr: "not a file"},
		{arg: "src/config.ts:", wantErr: "no symbol name"},
		{arg: "two words", wantErr: "not a symbol name"},
	}
	for _, tt := range tests {
		got, err := parseSymbolArg(tt.arg, isPath)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseSymbolArg(%q) error = %v, want %q", tt.arg, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSymbolArg(%q): %v", tt.arg, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSymbolArg(%q) = %+v, want %+v", tt.arg, got, tt.want)
		}
	}
	if s := (symbolQuery{Path: "a.go", Qualified: "F"}).String(); s != "a.go:F" {
		t.Errorf("String() = %q", s)
	}
}

func sym(path, qualified string, line int) model.Symbol {
	name := lastPart(qualified)
	return model.Symbol{
		Lang: "go", Path: path, Qualified: qualified, Name: name, Kind: "func",
		StartLine: line, EndLine: line + 5, Key: model.SymbolKey("go", path, qualified),
	}
}

func TestPickSymbol(t *testing.T) {
	a := sym("a.go", "Config.Load", 10)
	b := sym("b.go", "Config.Load", 20)
	c := sym("c.go", "Load", 3)
	d := sym("d.go", "Other.Load", 7)

	// The first stage with an exact match decides.
	r := pickSymbol(symbolQuery{Qualified: "Config.Load"}, []model.Symbol{a, a}, []model.Symbol{b})
	if !r.Found || r.Symbol.Key != a.Key {
		t.Fatalf("first stage: %+v", r)
	}
	// Two exact matches in one stage are ambiguous, sorted canonically.
	r = pickSymbol(symbolQuery{Qualified: "Config.Load"}, []model.Symbol{b, a})
	if r.Found || !r.Ambiguous || len(r.Candidates) != 2 || r.Candidates[0].Path != "a.go" {
		t.Fatalf("ambiguous: %+v", r)
	}
	// A path narrows the match.
	r = pickSymbol(symbolQuery{Path: "b.go", Qualified: "Config.Load"}, []model.Symbol{a, b})
	if !r.Found || r.Symbol.Path != "b.go" {
		t.Fatalf("path: %+v", r)
	}
	// Near misses: suffix and same member name, ignoring case.
	r = pickSymbol(symbolQuery{Qualified: "load"}, []model.Symbol{a, c}, []model.Symbol{d})
	if r.Found || r.Ambiguous || len(r.Candidates) != 3 {
		t.Fatalf("near: %+v", r)
	}
	// Candidates are capped.
	var many []model.Symbol
	for i := range 15 {
		many = append(many, sym("f.go", "T"+string(rune('a'+i))+".Run", i))
	}
	r = pickSymbol(symbolQuery{Qualified: "Run"}, many)
	if len(r.Candidates) != maxCandidates {
		t.Fatalf("cap: %d candidates", len(r.Candidates))
	}
	if lastPart("A.b#2") != "b" || lastPart("A.x@setter") != "x" || lastPart("f") != "f" {
		t.Error("lastPart")
	}
}

func TestParseTarget(t *testing.T) {
	files := map[string]bool{"src/a.ts": true, "README": true}
	dirs := map[string]bool{"src": true}
	isFile := func(p string) bool { return files[p] }
	isDir := func(p string) bool { return dirs[p] }
	tests := []struct {
		arg       string
		allowLine bool
		kind      targetKind
		path      string
		line      int
		name      string
		wantErr   string
	}{
		{arg: "src/a.ts", kind: targetPath, path: "src/a.ts", name: "src/a.ts"},
		{arg: "./README", kind: targetPath, path: "README", name: "README"},
		{arg: "src/", kind: targetDir, path: "src", name: "src/"},
		{arg: "src", kind: targetDir, path: "src", name: "src/"},
		{arg: "src/a.ts:42", allowLine: true, kind: targetLine, path: "src/a.ts", line: 42, name: "src/a.ts:42"},
		{arg: "src/a.ts:42", wantErr: "path:line works with why"},
		{arg: "src/a.ts:0", allowLine: true, wantErr: "not a line number"},
		{arg: "src/a.ts:parse", kind: targetSymbol, name: "src/a.ts:parse"},
		{arg: "Config.load", kind: targetSymbol, name: "Config.load"},
		{arg: "  ", wantErr: "no target"},
	}
	for _, tt := range tests {
		got, err := parseTarget(tt.arg, isFile, isDir, tt.allowLine)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseTarget(%q) error = %v, want %q", tt.arg, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTarget(%q): %v", tt.arg, err)
			continue
		}
		if got.Kind != tt.kind || got.Path != tt.path || got.Line != tt.line || got.Name() != tt.name {
			t.Errorf("parseTarget(%q) = %+v (name %q)", tt.arg, got, got.Name())
		}
	}
	s := target{Kind: targetSymbol, Raw: "f", Symbol: sym("x.go", "f", 1)}
	if s.Name() != "x.go:f" {
		t.Errorf("resolved symbol name = %q", s.Name())
	}
}

// ---- id prefixes ----

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		arg  string
		want string
		err  error
	}{
		{"n-abcd", "n-abcd", nil},
		{"ABCD12", "n-abcd12", nil},
		{" n-0123456789ab ", "n-0123456789ab", nil},
		{"n-abc", "", errIDShort},
		{"abc", "", errIDShort},
		{"n-xyz1", "", errIDInvalid},
		{"t-abcd", "", errIDInvalid}, // another kind's prefix
	}
	for _, tt := range tests {
		got, err := normalizeID("n-", tt.arg)
		if !errors.Is(err, tt.err) || got != tt.want {
			t.Errorf("normalizeID(%q) = %q, %v; want %q, %v", tt.arg, got, err, tt.want, tt.err)
		}
	}
}

func TestMatchID(t *testing.T) {
	ids := []string{"t-abcd1234", "t-abcd9999", "t-ffff0000", "t-abcd", "t-ffff0000"}
	tests := []struct {
		arg   string
		want  string
		cands []string
		err   error
	}{
		{"t-ffff", "t-ffff0000", nil, nil},       // unique prefix (duplicates collapse)
		{"FFFF0000", "t-ffff0000", nil, nil},     // prefix optional, case ignored
		{" t-abcd1234 ", "t-abcd1234", nil, nil}, // full id, spaces trimmed
		{"t-abcd", "t-abcd", nil, nil},           // exact beats longer ids
		{"t-abcd1", "t-abcd1234", nil, nil},      // unique among the longer ones
		{"abcd99", "t-abcd9999", nil, nil},       // short form
		{"t-0000", "", nil, errIDUnknown},        // no match
		{"t-bcd1", "", nil, errIDUnknown},        // infixes do not match
		{"t-abcd12345", "", nil, errIDUnknown},   // longer than any id
		{"t-ab", "", nil, errIDShort},            // too short
		{"", "", nil, errIDShort},                // nothing
		{"t-ab_d", "", nil, errIDInvalid},        // not hex
	}
	for _, tt := range tests {
		got, cands, err := matchID("t-", tt.arg, ids)
		if !errors.Is(err, tt.err) || got != tt.want || !reflect.DeepEqual(cands, tt.cands) {
			t.Errorf("matchID(%q) = %q, %v, %v; want %q, %v, %v", tt.arg, got, cands, err, tt.want, tt.cands, tt.err)
		}
	}
	// Ambiguous prefixes return the sorted candidates.
	got, cands, err := matchID("t-", "t-abcd", []string{"t-abcd9999", "t-abcd1234"})
	if !errors.Is(err, errIDAmbiguous) || got != "" || !reflect.DeepEqual(cands, []string{"t-abcd1234", "t-abcd9999"}) {
		t.Errorf("ambiguous: %q, %v, %v", got, cands, err)
	}
}

func TestIDRefusalAlwaysOffersAWayOut(t *testing.T) {
	for _, err := range []error{errIDShort, errIDInvalid, errIDUnknown, errIDAmbiguous} {
		r := idRefusal("dismiss", "note", "note", "n-ab", err, []string{"n-abcd1", "n-abcd2"}, nil)
		if r.Outcome != api.Refused || len(r.Options) == 0 || r.Summary == "" {
			t.Errorf("%v: %+v", err, r)
		}
	}
}

// ---- lapses, polling and leases ----

func TestJudgeLapse(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lease := 3 * time.Minute
	fresh := fetchFresh(30 * time.Second)
	tests := []struct {
		name                      string
		firstSeen, lastFetch, now time.Time
		lapsed                    bool
		forDur                    time.Duration
	}{
		{"never seen", time.Time{}, t0, t0, false, 0},
		{"never fetched", t0, time.Time{}, t0, false, 0},
		{"within the lease", t0, t0.Add(2 * time.Minute), t0.Add(2 * time.Minute), false, 0},
		{"exactly the lease", t0, t0.Add(lease), t0.Add(lease), true, 0},
		{"past the lease", t0, t0.Add(5 * time.Minute), t0.Add(5*time.Minute + 10*time.Second), true, 2 * time.Minute},
		{"stale fetch: offline clones judge nobody", t0, t0.Add(5 * time.Minute), t0.Add(5*time.Minute + fresh + time.Second), false, 0},
		{"fetch at the freshness limit", t0, t0.Add(5 * time.Minute), t0.Add(5*time.Minute + fresh), true, 2 * time.Minute},
	}
	for _, tt := range tests {
		got := judgeLapse(tt.firstSeen, tt.lastFetch, tt.now, lease, fresh)
		if got.Lapsed != tt.lapsed || got.For != tt.forDur {
			t.Errorf("%s: %+v, want lapsed=%v for=%v", tt.name, got, tt.lapsed, tt.forDur)
		}
		if got.Lapsed && !got.Since.Equal(tt.firstSeen) {
			t.Errorf("%s: since %v", tt.name, got.Since)
		}
	}
	if judgeLapse(t0, t0.Add(time.Hour), t0.Add(time.Hour), 0, fresh).Lapsed {
		t.Error("a zero lease never lapses")
	}
}

func TestFetchFresh(t *testing.T) {
	if got, want := fetchFresh(30*time.Second), 30*time.Second+6*time.Second+tickSlack; got != want {
		t.Errorf("fetchFresh(30s) = %v, want %v", got, want)
	}
}

func TestPollDelay(t *testing.T) {
	iv := 30 * time.Second
	if d := pollDelay(iv, 0, 0); d != 24*time.Second {
		t.Errorf("low jitter: %v", d)
	}
	if d := pollDelay(iv, 0, 0.5); d != iv {
		t.Errorf("mid jitter: %v", d)
	}
	if d := pollDelay(iv, 0, 0.999999); d < 35*time.Second || d > 36*time.Second {
		t.Errorf("high jitter: %v", d)
	}
	if d := pollDelay(iv, 1, 0.5); d != 2*iv {
		t.Errorf("one failure: %v", d)
	}
	if d := pollDelay(iv, 3, 0.5); d != 8*iv {
		t.Errorf("three failures: %v", d)
	}
	for _, f := range []int{6, 10, 100, 1000} {
		for _, r := range []float64{0, 0.5, 0.999} {
			d := pollDelay(iv, f, r)
			if d > maxBackoff || d < time.Duration(float64(maxBackoff)*(1-pollJitter))-time.Second {
				t.Errorf("failures=%d r=%v: %v", f, r, d)
			}
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	if !needsRefresh(time.Time{}, now) {
		t.Error("never fetched must refresh")
	}
	if needsRefresh(now.Add(-4*time.Second), now) {
		t.Error("a 4s old fetch is fresh enough")
	}
	if needsRefresh(now.Add(-verbRefreshAfter), now) {
		t.Error("exactly 5s is still fresh")
	}
	if !needsRefresh(now.Add(-6*time.Second), now) {
		t.Error("a 6s old fetch must refresh")
	}
}

func TestRenewDueAndActive(t *testing.T) {
	now := time.Now()
	lease := 3 * time.Minute
	if renewDue(now.Add(-59*time.Second), now, lease) {
		t.Error("renewed before a third of the lease")
	}
	if !renewDue(now.Add(-time.Minute), now, lease) {
		t.Error("not renewed after a third of the lease")
	}
	if !isActive(now.Add(-lease), now, lease) || isActive(now.Add(-lease-time.Second), now, lease) || isActive(time.Time{}, now, lease) {
		t.Error("isActive")
	}
	if leaseSeconds(180*time.Second) != 180 || leaseSeconds(0) != 1 {
		t.Error("leaseSeconds")
	}
}

func TestPollStateFlush(t *testing.T) {
	var p pollState
	now := time.Now()
	if p.takeFlush() {
		t.Fatal("flush without writes")
	}
	p.markOffline(now)
	if p.takeFlush() {
		t.Fatal("flush while offline")
	}
	if last, off := p.snapshot(); !last.IsZero() || off == nil || !off.Equal(now) {
		t.Fatalf("snapshot: %v %v", last, off)
	}
	p.mu.Lock()
	p.offlineSince = nil
	p.mu.Unlock()
	if !p.takeFlush() || p.takeFlush() {
		t.Fatal("flush once back online")
	}
	if !p.outsideDue(now, time.Minute) || p.outsideDue(now.Add(30*time.Second), time.Minute) || !p.outsideDue(now.Add(time.Minute), time.Minute) {
		t.Fatal("outsideDue")
	}
}

func TestVisibleItems(t *testing.T) {
	items := []model.WorkItem{
		{ID: "a", Status: model.ItemActive},
		{ID: "s", Status: model.ItemSubmitted},
		{ID: "l", Status: model.ItemLanded},
		{ID: "x", Status: model.ItemAbandoned},
	}
	ids := func(its []model.WorkItem) (out []string) {
		for _, it := range its {
			out = append(out, string(it.ID))
		}
		return out
	}
	if got := ids(visibleItems(items, lapse{}, time.Hour)); !reflect.DeepEqual(got, []string{"a", "s"}) {
		t.Errorf("live peer: %v", got)
	}
	if got := ids(visibleItems(items, lapse{Lapsed: true, For: 30 * time.Minute}, time.Hour)); !reflect.DeepEqual(got, []string{"a", "s"}) {
		t.Errorf("recently lapsed peer: %v", got)
	}
	if got := ids(visibleItems(items, lapse{Lapsed: true, For: 2 * time.Hour}, time.Hour)); !reflect.DeepEqual(got, []string{"s"}) {
		t.Errorf("stale peer: %v", got)
	}
}

func TestFmtAge(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:                  "0s",
		4 * time.Second:               "4s",
		2*time.Minute + 5*time.Second: "2m",
		time.Hour:                     "1h",
		time.Hour + 5*time.Minute:     "1h5m",
		75 * time.Hour:                "3d",
	}
	for d, want := range tests {
		if got := fmtAge(d); got != want {
			t.Errorf("fmtAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// ---- own documents ----

func TestTrimItems(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-10 * time.Minute)
	items := []model.WorkItem{
		{ID: "active", Status: model.ItemActive, UpdatedAt: old},
		{ID: "landed-old", Status: model.ItemLanded, ClosedAt: &old},
		{ID: "landed-new", Status: model.ItemLanded, ClosedAt: &recent},
		{ID: "abandoned-old-no-close", Status: model.ItemAbandoned, UpdatedAt: old},
		{ID: "submitted", Status: model.ItemSubmitted, UpdatedAt: old},
	}
	var got []string
	for _, it := range trimItems(items, now) {
		got = append(got, string(it.ID))
	}
	if want := []string{"active", "landed-new", "submitted"}; !reflect.DeepEqual(got, want) {
		t.Errorf("trimItems = %v, want %v", got, want)
	}
}

func TestShrinkState(t *testing.T) {
	st := &model.AgentState{Items: []model.WorkItem{{
		Footprint: model.Footprint{
			Refs:    []model.Ref{{}},
			Symbols: []model.SymbolChange{{}},
		},
	}}}
	if !shrinkState(st) || st.Items[0].Footprint.Refs != nil || st.Items[0].Footprint.Symbols == nil {
		t.Fatalf("first pass: %+v", st.Items[0].Footprint)
	}
	if !shrinkState(st) || st.Items[0].Footprint.Symbols != nil || !st.Items[0].Footprint.Broad {
		t.Fatalf("second pass: %+v", st.Items[0].Footprint)
	}
	if shrinkState(st) {
		t.Fatal("nothing left to drop")
	}
}

func TestTrimOutbox(t *testing.T) {
	now := time.Now()
	ob := &model.Outbox{
		Needs: []model.Need{{ID: "need"}},
		Messages: []model.Message{
			{ID: "m1", Thread: "t-settled-old", At: now.Add(-3 * time.Hour)},
			{ID: "m2", Thread: "t-settled-new", At: now.Add(-10 * time.Minute)},
			{ID: "m3", Thread: "t-open", At: now.Add(-48 * time.Hour)},
			{ID: "m4", Thread: "t-gone-old", At: now.Add(-25 * time.Hour)},
			{ID: "m5", Thread: "t-gone-new", At: now.Add(-time.Hour)},
		},
		Accepts: []model.Accept{
			{Thread: "t-settled-old", Proposal: "p-1", At: now.Add(-3 * time.Hour)},
			{Thread: "t-open", Proposal: "p-2", At: now.Add(-48 * time.Hour)},
		},
	}
	threads := map[string]threadInfo{
		"t-settled-old": {settled: true, settledAt: now.Add(-2 * time.Hour)},
		"t-settled-new": {settled: true, settledAt: now.Add(-10 * time.Minute)},
		"t-open":        {},
	}
	trimOutbox(ob, threads, now)
	var msgs []string
	for _, m := range ob.Messages {
		msgs = append(msgs, m.ID)
	}
	if want := []string{"m2", "m3", "m5"}; !reflect.DeepEqual(msgs, want) {
		t.Errorf("messages = %v, want %v", msgs, want)
	}
	if len(ob.Accepts) != 1 || ob.Accepts[0].Proposal != "p-2" {
		t.Errorf("accepts = %+v", ob.Accepts)
	}
	if len(ob.Needs) != 1 {
		t.Errorf("needs must stay: %+v", ob.Needs)
	}
}

func TestShrinkOutbox(t *testing.T) {
	now := time.Now()
	ob := &model.Outbox{}
	for i := range keepPerThread + 3 {
		ob.Messages = append(ob.Messages, model.Message{ID: "a" + string(rune('a'+i)), Thread: "t-a", At: now.Add(time.Duration(i) * time.Minute)})
	}
	ob.Messages = append(ob.Messages, model.Message{ID: "b", Thread: "t-b", At: now})
	if !shrinkOutbox(ob) {
		t.Fatal("nothing dropped")
	}
	if len(ob.Messages) != keepPerThread+1 {
		t.Fatalf("%d messages kept", len(ob.Messages))
	}
	if ob.Messages[0].ID != "ad" || ob.Messages[len(ob.Messages)-1].ID != "b" {
		t.Fatalf("kept the wrong ones: first %s, last %s", ob.Messages[0].ID, ob.Messages[len(ob.Messages)-1].ID)
	}
	if shrinkOutbox(ob) {
		t.Fatal("second shrink dropped more")
	}
}

func TestMergeRequirements(t *testing.T) {
	got := mergeRequirements(
		[]model.Requirement{{From: "b", Text: "old b"}, {From: "c", Text: "c"}},
		[]model.Requirement{{From: "b", Text: "new b"}, {From: "a", Text: "a"}},
	)
	want := []model.Requirement{{From: "a", Text: "a"}, {From: "b", Text: "new b"}, {From: "c", Text: "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeRequirements = %+v", got)
	}
}

// ---- snapshots, branches, provenance ----

func TestIntentFromBranch(t *testing.T) {
	tests := []struct{ branch, want string }{
		{"gitlet/add-strict-mode-8c1f", "add strict mode"},
		{"gitlet/add-strict-mode", "add strict mode"},
		{"feature/fix_login.flow", "fix login flow"},
		{"user/topic-beef", "topic beef"}, // the hex suffix only goes on gitlet branches
		{"main", "untitled work on main"},
		{"", "untitled work on a detached HEAD"},
		{"gitlet/8c1f", "8c1f"},
		{"---", "untitled work on ---"},
	}
	for _, tt := range tests {
		if got := intentFromBranch(tt.branch, "main", "gitlet/"); got != tt.want {
			t.Errorf("intentFromBranch(%q) = %q, want %q", tt.branch, got, tt.want)
		}
	}
}

func TestDefaultBranchName(t *testing.T) {
	got := defaultBranchName("gitlet/", "Add strict mode to Config loading!", "8c1f00112233")
	if got != "gitlet/add-strict-mode-to-config-loading-8c1f" {
		t.Errorf("defaultBranchName = %q", got)
	}
	if got := defaultBranchName("", "", "ab"); got != "work-ab" {
		t.Errorf("empty intent: %q", got)
	}
}

func TestParseTrailers(t *testing.T) {
	msg := "Fix the parser (#12)\n\n* step one\n\nGitlet-Item: 0123456789ab\ngitlet-agent: claude-code-host-a1b2c3\nGitlet-Item: not-an-id\n"
	item, agent := parseTrailers(msg)
	if item != "0123456789ab" || agent != "claude-code-host-a1b2c3" {
		t.Errorf("parseTrailers = %q, %q", item, agent)
	}
	if item, agent := parseTrailers("plain message"); item != "" || agent != "" {
		t.Errorf("no trailers: %q, %q", item, agent)
	}
	item, _ = parseTrailers("x\n\nGitlet-Item: 0123456789AB\n  Gitlet-Item: ffffffffffff  ")
	if item != "ffffffffffff" {
		t.Errorf("last valid wins, case folded: %q", item)
	}
}

func TestInnermost(t *testing.T) {
	outer := model.Symbol{Qualified: "T", StartLine: 1, EndLine: 50}
	inner := model.Symbol{Qualified: "T.m", StartLine: 10, EndLine: 20}
	if s, ok := innermost([]model.Symbol{outer, inner}, 15); !ok || s.Qualified != "T.m" {
		t.Errorf("inner: %+v %v", s, ok)
	}
	if s, ok := innermost([]model.Symbol{outer, inner}, 30); !ok || s.Qualified != "T" {
		t.Errorf("outer: %+v %v", s, ok)
	}
	if _, ok := innermost([]model.Symbol{inner}, 30); ok {
		t.Error("no symbol contains line 30")
	}
}

func TestCleanPaths(t *testing.T) {
	root := filepath.FromSlash("/repo/work")
	out, bad := cleanPaths(root, []string{
		"src/a.ts", "./src/a.ts", " src//b.ts ", filepath.FromSlash("/repo/work/c.go"),
		"../outside", filepath.FromSlash("/elsewhere/d.go"), ".", "",
	})
	if want := []string{"src/a.ts", "src/b.ts", "c.go"}; !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v, want %v", out, want)
	}
	if len(bad) != 3 {
		t.Errorf("bad = %v", bad)
	}
}

func TestInsideWorktree(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"src/deep", "nested/inner", "other"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "nested", ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := map[string]bool{
		root:                                    true,
		filepath.Join(root, "src"):              true,
		filepath.Join(root, "src", "deep"):      true,
		filepath.Join(root, "src", "..", "src"): true,
		filepath.Join(root, "nested"):           false,
		filepath.Join(root, "nested", "inner"):  false,
		filepath.Dir(root):                      false,
		filepath.Join(filepath.Dir(root), filepath.Base(root)+"-sibling"): false,
	}
	for dir, want := range tests {
		if got := insideWorktree(root, dir); got != want {
			t.Errorf("insideWorktree(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestWorktreeCommand(t *testing.T) {
	cmd := worktreeCommand("/home/me/my repo", "origin/main")
	if !strings.HasPrefix(cmd, "git worktree add --detach '/home/me/my repo-") || !strings.HasSuffix(cmd, "' origin/main") {
		t.Errorf("worktreeCommand = %q", cmd)
	}
	tests := map[string]string{
		"plain/path-1.2": "plain/path-1.2",
		"":               "''",
		"it's":           `'it'\''s'`,
		"a b":            "'a b'",
		"$HOME":          "'$HOME'",
	}
	for in, want := range tests {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBranchOfRef(t *testing.T) {
	tests := map[string]string{
		"refs/gitlet-heads/origin/feature/x": "feature/x",
		"refs/gitlet-heads/origin":           "",
		"refs/heads/main":                    "main",
		"refs/tags/v1":                       "",
	}
	for ref, want := range tests {
		if got := branchOfRef(ref); got != want {
			t.Errorf("branchOfRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestBlobID(t *testing.T) {
	// git hash-object of "hello\n".
	if got := blobID([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("blobID = %s", got)
	}
}

// ---- result builders ----

func TestRefusedAlwaysHasAnOption(t *testing.T) {
	r := refused("No.", nil)
	if r.Outcome != api.Refused || len(r.Options) != 1 || r.Options[0].Tool != "status" {
		t.Errorf("refused without options: %+v", r)
	}
	r = refused("No.", []string{"why"}, cmdOption("do this", "git status"))
	if len(r.Options) != 1 || r.Options[0].Command != "git status" {
		t.Errorf("refused with an option: %+v", r)
	}
}

func TestToolOption(t *testing.T) {
	o := toolOption("label", "claim", "symbol", "a.go:F", "note", "", "count", 2, "", "x")
	want := model.Option{Label: "label", Tool: "claim", Args: map[string]any{"symbol": "a.go:F", "count": 2}}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("toolOption = %+v", o)
	}
	if o := toolOption("l", "status"); o.Args != nil {
		t.Errorf("no args: %+v", o)
	}
}

func TestTextHelpers(t *testing.T) {
	if got := capLines([]string{"a", "b", "c"}, 2); !reflect.DeepEqual(got, []string{"a", "b", "1 more"}) {
		t.Errorf("capLines = %v", got)
	}
	if got := capLines([]string{"a"}, 2); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("capLines short = %v", got)
	}
	if got := dedupe([]string{"a", "b", "a", "c", "b"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("dedupe = %v", got)
	}
	if got := intersectSorted([]string{"a", "c", "d"}, []string{"b", "c", "d", "e"}); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Errorf("intersectSorted = %v", got)
	}
	if shortOID("0123456789abcdef") != "0123456" || firstLine("one\ntwo") != "one" {
		t.Error("shortOID or firstLine")
	}
}

// ---- Call decoding ----

func TestDecodeInput(t *testing.T) {
	v, ok := api.Lookup("start")
	if !ok {
		t.Fatal("no start verb")
	}
	check := func(name string, input any, want api.StartInput) {
		t.Helper()
		got, err := decodeInput(v, input)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p, ok := got.(*api.StartInput)
		if !ok {
			t.Fatalf("%s: got %T", name, got)
		}
		if !reflect.DeepEqual(*p, want) {
			t.Fatalf("%s: got %+v, want %+v", name, *p, want)
		}
	}
	in := api.StartInput{Intent: "fix it", Paths: []string{"a.go"}}
	check("nil", nil, api.StartInput{})
	check("value", in, in)
	check("nil pointer", (*api.StartInput)(nil), api.StartInput{})
	check("raw JSON", json.RawMessage(`{"intent":"fix it","paths":["a.go"]}`), in)
	check("bytes", []byte(` {"intent":"fix it","paths":["a.go"]} `), in)
	check("null", json.RawMessage(`null`), api.StartInput{})
	check("empty", []byte(``), api.StartInput{})
	check("map", map[string]any{"intent": "fix it", "paths": []string{"a.go"}}, in)
	check("embedded where", json.RawMessage(`{"intent":"x","worktree":"/w"}`), api.StartInput{Intent: "x", Where: api.Where{Worktree: "/w"}})

	ptr := &api.StartInput{Intent: "same"}
	got, err := decodeInput(v, ptr)
	if err != nil || got != any(ptr) {
		t.Fatalf("pointer: %v %v", got, err)
	}

	for name, input := range map[string]any{
		"unknown field": json.RawMessage(`{"intent":"x","intnet":"y"}`),
		"wrong type":    json.RawMessage(`{"intent":3}`),
		"trailing data": json.RawMessage(`{"intent":"x"} {"intent":"y"}`),
		"bad json":      []byte(`{"intent":`),
		"map unknown":   map[string]any{"bogus": true},
	} {
		_, err := decodeInput(v, input)
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: error %v is not a usage error", name, err)
		}
	}

	_, err = decodeInput(v, 42)
	var ue *usageError
	if err == nil || errors.As(err, &ue) || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Errorf("Go type mismatch: %v", err)
	}
	_, err = decodeInput(v, api.StatusInput{})
	if err == nil || errors.As(err, &ue) {
		t.Errorf("another verb's input: %v", err)
	}
}

func TestHandlerForEveryVerb(t *testing.T) {
	for _, v := range api.Verbs {
		if handlerFor(v.Name) == nil {
			t.Errorf("no handler for %s", v.Name)
		}
		in := v.NewInput()
		if _, ok := whereOf(in); !ok {
			t.Errorf("%s input embeds no Where", v.Name)
		}
		if len(usageDetails(v)) == 0 {
			t.Errorf("%s has no usage details", v.Name)
		}
		o := usageOption(v)
		if o.Tool != v.Name {
			t.Errorf("%s usage option: %+v", v.Name, o)
		}
		for _, a := range v.Args {
			if _, ok := o.Args[a.Name]; ok != a.Required {
				t.Errorf("%s usage option args: %+v", v.Name, o.Args)
			}
		}
	}
	if handlerFor("bogus") != nil {
		t.Error("handler for an unknown verb")
	}
	if _, ok := whereOf(api.StatusInput{}); ok {
		t.Error("whereOf on a value")
	}
	w, ok := whereOf(&api.WhoInput{Where: api.Where{Worktree: "/x"}})
	if !ok || w.Worktree != "/x" {
		t.Errorf("whereOf = %+v %v", w, ok)
	}
}
