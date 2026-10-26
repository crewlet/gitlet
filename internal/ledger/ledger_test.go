package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func openTest(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func tempLedger(t *testing.T) *Ledger {
	t.Helper()
	return openTest(t, filepath.Join(t.TempDir(), "gitlet", "ledger.db"))
}

// rawDB opens the database file directly, without the ledger.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenMigratesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "common", "gitlet", "ledger.db")
	l := openTest(t, path)
	if l.ReadOnly() {
		t.Fatal("new ledger is read-only")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	if v := userVersion(t, db); v != len(migrations) {
		t.Fatalf("user_version = %d, want %d", v, len(migrations))
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v), want wal", mode, err)
	}
	for _, table := range []string{"settings", "identities", "pushed", "ref_fetches", "ref_observations",
		"notes", "footprint_cache", "merge_cache", "events", "items"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: count %d, err %v", table, n, err)
		}
	}
}

func TestConnectionSettings(t *testing.T) {
	l := tempLedger(t)
	ctx := context.Background()
	conn, err := l.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for pragma, want := range map[string]int{"busy_timeout": 10000, "synchronous": 1, "foreign_keys": 1} {
		var got int
		if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %d (%v), want %d", pragma, got, err, want)
		}
	}
}

func TestReopenIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(ctx, path, Options{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Set(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		l := openTest(t, path)
		if l.ReadOnly() {
			t.Fatalf("reopen %d: read-only", i)
		}
		if v, ok, err := l.Get(ctx, "k"); err != nil || !ok || v != "v" {
			t.Fatalf("reopen %d: Get = %q, %v, %v", i, v, ok, err)
		}
	}
	if v := userVersion(t, rawDB(t, path)); v != len(migrations) {
		t.Fatalf("user_version = %d after reopening", v)
	}
}

func TestPathWithSpecialCharacters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a b?c#d%e&f=g", "ledger.db")
	l := openTest(t, path)
	if err := l.Set(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at the given path: %v", err)
	}
}

func TestNewerSchemaIsReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l := openTest(t, path)
	if err := l.Set(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := l.SyncNotes(ctx, "a", []model.Note{testNote("x", "1")}, t0); err != nil {
		t.Fatal(err)
	}
	l.Close()
	db := rawDB(t, path)
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	l = openTest(t, path)
	if !l.ReadOnly() {
		t.Fatal("ledger of a newer schema is not read-only")
	}
	if v, ok, err := l.Get(ctx, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("Get = %q, %v, %v", v, ok, err)
	}
	if notes, err := l.OpenNotes(ctx, "a"); err != nil || len(notes) != 1 {
		t.Fatalf("OpenNotes = %v, %v", notes, err)
	}

	writes := map[string]func() error{
		"Set":           func() error { return l.Set(ctx, "k", "w") },
		"Delete":        func() error { return l.Delete(ctx, "k") },
		"SetAgentFor":   func() error { return l.SetAgentFor(ctx, "/w", "cli-host-abcdef") },
		"SetLastPushed": func() error { return l.SetLastPushed(ctx, "refs/x", "abc") },
		"ObserveRefs": func() error {
			_, err := l.ObserveRefs(ctx, "refs/gitlet/", map[string]string{"r": "1"}, t0)
			return err
		},
		"ResetFirstSeen": func() error { return l.ResetFirstSeen(ctx, t0) },
		"SyncNotes":      func() error { return l.SyncNotes(ctx, "a", nil, t0) },
		"TakeUpdates": func() error {
			_, _, err := l.TakeUpdates(ctx, "a", t0)
			return err
		},
		"MarkShown":      func() error { return l.MarkShown(ctx, "a", []string{"n-x"}, t0) },
		"Acted":          func() error { return l.Acted(ctx, "a", testNote("x", "1").ID, t0) },
		"Dismiss":        func() error { return l.Dismiss(ctx, "a", testNote("x", "1").ID, "not_needed", "", t0) },
		"CacheFootprint": func() error { return l.CacheFootprint(ctx, "k", model.Footprint{}) },
		"CacheMerge":     func() error { return l.CacheMerge(ctx, "a", "b", MergeOutcome{}) },
		"AppendEvent":    func() error { return l.AppendEvent(ctx, Event{Agent: "a", Kind: "start"}) },
		"RememberItem":   func() error { return l.RememberItem(ctx, "a", model.WorkItem{ID: "0123456789ab"}) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", name, err)
		}
	}
	if v, _, _ := l.Get(ctx, "k"); v != "v" {
		t.Fatalf("read-only ledger changed: k = %q", v)
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	notDB := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(notDB, []byte("this is not a database, just text that is long enough"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		path string
	}{
		{"empty path", ctx, ""},
		{"not a database", ctx, notDB},
		{"parent is a file", ctx, filepath.Join(blocker, "ledger.db")},
		{"path is a directory", ctx, dir},
		{"cancelled context", cancelled, filepath.Join(dir, "new.db")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := Open(tt.ctx, tt.path, Options{BusyTimeout: 200 * time.Millisecond})
			if err == nil {
				l.Close()
				t.Fatal("Open succeeded")
			}
			t.Log(err)
		})
	}
}

func TestKeyValue(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	tests := []struct {
		name   string
		op     func() error
		key    string
		want   string
		wantOK bool
	}{
		{"unset", nil, "a", "", false},
		{"set", func() error { return l.Set(ctx, "a", "1") }, "a", "1", true},
		{"overwrite", func() error { return l.Set(ctx, "a", "2") }, "a", "2", true},
		{"empty value is set", func() error { return l.Set(ctx, "b", "") }, "b", "", true},
		{"delete", func() error { return l.Delete(ctx, "a") }, "a", "", false},
		{"delete missing", func() error { return l.Delete(ctx, "zzz") }, "zzz", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op != nil {
				if err := tt.op(); err != nil {
					t.Fatal(err)
				}
			}
			v, ok, err := l.Get(ctx, tt.key)
			if err != nil || v != tt.want || ok != tt.wantOK {
				t.Fatalf("Get(%q) = %q, %v, %v; want %q, %v", tt.key, v, ok, err, tt.want, tt.wantOK)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	if _, ok, err := l.AgentFor(ctx, "/repo"); err != nil || ok {
		t.Fatalf("AgentFor unknown = %v, %v", ok, err)
	}
	if err := l.SetAgentFor(ctx, "/repo", "claude-code-laptop-3fa9c1"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAgentFor(ctx, "/repo-2", "codex-ci-1a2b3c"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		worktree string
		want     model.AgentID
		ok       bool
	}{
		{"/repo", "claude-code-laptop-3fa9c1", true},
		{"/repo/", "claude-code-laptop-3fa9c1", true},
		{"/repo-2", "codex-ci-1a2b3c", true},
		{"/other", "", false},
	}
	for _, tt := range tests {
		id, ok, err := l.AgentFor(ctx, tt.worktree)
		if err != nil || id != tt.want || ok != tt.ok {
			t.Errorf("AgentFor(%q) = %q, %v, %v; want %q, %v", tt.worktree, id, ok, err, tt.want, tt.ok)
		}
	}
	if err := l.SetAgentFor(ctx, "/repo", "codex-ci-ffffff"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := l.AgentFor(ctx, "/repo"); id != "codex-ci-ffffff" {
		t.Fatalf("AgentFor after replace = %q", id)
	}
	for _, bad := range []model.AgentID{"", "Has Caps", "refs/../x", "-leading"} {
		if err := l.SetAgentFor(ctx, "/bad", bad); err == nil {
			t.Errorf("SetAgentFor(%q) accepted an invalid id", bad)
		}
	}
}

func TestLastPushed(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	const ref = "refs/gitlet/agents/a"
	steps := []struct {
		set  *string
		want string
	}{
		{nil, ""},
		{ptr("abc"), "abc"},
		{ptr("def"), "def"},
		{ptr(""), ""},
	}
	for i, s := range steps {
		if s.set != nil {
			if err := l.SetLastPushed(ctx, ref, *s.set); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := l.LastPushed(ctx, ref); err != nil || got != s.want {
			t.Fatalf("step %d: LastPushed = %q, %v; want %q", i, got, err, s.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestFootprintCache(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	l.keep.footprints = 3
	fp := func(head string) model.Footprint {
		return model.Footprint{
			Base: "base", Head: head,
			Files:      []model.FileChange{{Path: "a.go", Status: "M", Lang: "go", Parsed: true}},
			ComputedAt: t0.In(time.FixedZone("CEST", 2*3600)),
		}
	}
	if _, ok, err := l.CachedFootprint(ctx, "missing"); ok || err != nil {
		t.Fatalf("miss = %v, %v", ok, err)
	}
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		if err := l.CacheFootprint(ctx, "base.."+h, fp(h)); err != nil {
			t.Fatal(err)
		}
	}
	// Storing h2 again makes it the most recent, so h3 is evicted next.
	if err := l.CacheFootprint(ctx, "base..h2", fp("h2")); err != nil {
		t.Fatal(err)
	}
	if err := l.CacheFootprint(ctx, "base..h5", fp("h5")); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]bool{
		"base..h1": false, "base..h2": true, "base..h3": false, "base..h4": true, "base..h5": true,
	} {
		got, ok, err := l.CachedFootprint(ctx, key)
		if err != nil || ok != want {
			t.Errorf("CachedFootprint(%s) ok = %v, %v; want %v", key, ok, err, want)
			continue
		}
		if ok {
			w := fp(got.Head)
			w.ComputedAt = t0
			if !reflect.DeepEqual(got, w) {
				t.Errorf("CachedFootprint(%s) = %+v, want %+v", key, got, w)
			}
			if got.ComputedAt.Location() != time.UTC {
				t.Errorf("ComputedAt not in UTC: %v", got.ComputedAt)
			}
		}
	}
}

func TestMergeCache(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	l.keep.merges = 2
	if _, ok, err := l.CachedMerge(ctx, "a", "b"); ok || err != nil {
		t.Fatalf("miss = %v, %v", ok, err)
	}
	conflict := MergeOutcome{Clean: false, Conflicts: []string{"go.mod", "x.go"}}
	if err := l.CacheMerge(ctx, "b", "a", conflict); err != nil {
		t.Fatal(err)
	}
	if err := l.CacheMerge(ctx, "c", "d", MergeOutcome{Clean: true}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		a, b string
		want MergeOutcome
		ok   bool
	}{
		{"a", "b", conflict, true},
		{"b", "a", conflict, true},
		{"d", "c", MergeOutcome{Clean: true}, true},
		{"a", "c", MergeOutcome{}, false},
	}
	for _, tt := range tests {
		got, ok, err := l.CachedMerge(ctx, tt.a, tt.b)
		if err != nil || ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("CachedMerge(%s, %s) = %+v, %v, %v; want %+v, %v", tt.a, tt.b, got, ok, err, tt.want, tt.ok)
		}
	}
	if err := l.CacheMerge(ctx, "e", "f", MergeOutcome{Clean: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.CachedMerge(ctx, "a", "b"); ok {
		t.Fatal("oldest merge outcome was not evicted")
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	l.keep.events = 4
	zone := time.FixedZone("PDT", -7*3600)
	for i, e := range []Event{
		{At: t0, Agent: "a", Kind: "start", Detail: `{"n":1}`},
		{At: t0.Add(time.Second), Agent: "b", Kind: "claim"},
		{At: t0.Add(2 * time.Second).In(zone), Agent: "a", Kind: "snapshot", Detail: `{"n":3}`},
		{At: t0.Add(3 * time.Second), Agent: "a", Kind: "publish"},
	} {
		if err := l.AppendEvent(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	kinds := func(es []Event) []string {
		var ks []string
		for _, e := range es {
			ks = append(ks, e.Kind)
		}
		return ks
	}
	tests := []struct {
		agent model.AgentID
		limit int
		want  []string
	}{
		{"a", 10, []string{"publish", "snapshot", "start"}},
		{"a", 2, []string{"publish", "snapshot"}},
		{"b", 10, []string{"claim"}},
		{"", 0, []string{"publish", "snapshot", "claim", "start"}},
		{"", 3, []string{"publish", "snapshot", "claim"}},
		{"nobody", 5, nil},
	}
	for _, tt := range tests {
		got, err := l.Events(ctx, tt.agent, tt.limit)
		if err != nil || !reflect.DeepEqual(kinds(got), tt.want) {
			t.Errorf("Events(%q, %d) = %v, %v; want %v", tt.agent, tt.limit, kinds(got), err, tt.want)
		}
	}
	got, _ := l.Events(ctx, "a", 2)
	if e := got[1]; !e.At.Equal(t0.Add(2*time.Second)) || e.At.Location() != time.UTC ||
		e.Agent != "a" || e.Kind != "snapshot" || e.Detail != `{"n":3}` {
		t.Errorf("event = %+v", e)
	}
	// The log keeps the newest l.keep.events entries.
	if err := l.AppendEvent(ctx, Event{At: t0.Add(4 * time.Second), Agent: "b", Kind: "land"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Events(ctx, "", 0); !reflect.DeepEqual(kinds(got), []string{"land", "publish", "snapshot", "claim"}) {
		t.Errorf("after trim: %v", kinds(got))
	}
}

func TestItems(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	closed := t0.Add(time.Hour).In(time.FixedZone("X", 3600))
	item := model.WorkItem{
		ID: "0123456789ab", Intent: "strict config", Branch: "gitlet/strict", Base: "b1",
		Status: model.ItemActive, OpenedAt: t0, UpdatedAt: t0,
		Footprint: model.Footprint{Base: "b1", Head: "h1", ComputedAt: t0},
	}
	if _, _, ok, err := l.Item(ctx, item.ID); ok || err != nil {
		t.Fatalf("unknown item = %v, %v", ok, err)
	}
	if err := l.RememberItem(ctx, "a", item); err != nil {
		t.Fatal(err)
	}
	landed := item
	landed.Status, landed.UpdatedAt, landed.ClosedAt = model.ItemLanded, t0.Add(time.Hour), &closed
	if err := l.RememberItem(ctx, "a", landed); err != nil {
		t.Fatal(err)
	}
	// An older copy does not overwrite the newer one.
	if err := l.RememberItem(ctx, "a", item); err != nil {
		t.Fatal(err)
	}
	got, agent, ok, err := l.Item(ctx, item.ID)
	if err != nil || !ok || agent != "a" {
		t.Fatalf("Item = %v, %q, %v", ok, agent, err)
	}
	want := landed
	want.ClosedAt = ptr(closed.UTC())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Item = %+v\nwant %+v", got, want)
	}
	if err := l.RememberItem(ctx, "a", model.WorkItem{}); err == nil {
		t.Error("RememberItem accepted an item without id")
	}
	if err := l.RememberItem(ctx, "", item); err == nil {
		t.Error("RememberItem accepted an empty agent")
	}
}

func TestContextCancelled(t *testing.T) {
	l := tempLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Set(ctx, "k", "v"); !errors.Is(err, context.Canceled) {
		t.Errorf("Set: err = %v, want context.Canceled", err)
	}
	if _, _, err := l.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get: err = %v, want context.Canceled", err)
	}
}

func TestDSN(t *testing.T) {
	got := dsn("/tmp/a b/ledger.db", 1500*time.Millisecond, false)
	want := "file:///tmp/a%20b/ledger.db?_pragma=busy_timeout%281500%29&_pragma=synchronous%28NORMAL%29&_pragma=foreign_keys%281%29&_pragma=journal_mode%28WAL%29&_txlock=immediate"
	if got != want {
		t.Errorf("dsn =\n%s\nwant\n%s", got, want)
	}
	got = dsn("/x/ledger.db", 0, true)
	want = "file:///x/ledger.db?_pragma=busy_timeout%281%29&_pragma=synchronous%28NORMAL%29&_pragma=foreign_keys%281%29&mode=ro"
	if got != want {
		t.Errorf("read-only dsn =\n%s\nwant\n%s", got, want)
	}
}
