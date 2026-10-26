package ledger

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

const testItem model.ItemID = "0123456789ab"

// testNote returns the note named name at version; its title names both.
func testNote(name, version string) model.Note {
	return model.Note{
		ID:        model.NoteID(model.NoteBodyOverlap, name, testItem),
		Version:   version,
		Kind:      model.NoteBodyOverlap,
		Severity:  model.SeverityWarn,
		Item:      testItem,
		Subject:   name,
		Title:     name + " v" + version,
		CreatedAt: t0,
	}
}

func noteID(name string) string { return testNote(name, "").ID }

func titles(notes []model.Note) []string {
	var ts []string
	for _, n := range notes {
		ts = append(ts, n.Title)
	}
	return ts
}

// harness drives one agent's notes through a lifecycle, one minute per step.
type harness struct {
	t     *testing.T
	ctx   context.Context
	l     *Ledger
	agent model.AgentID
	now   time.Time
}

type step func(h *harness)

func produce(notes ...model.Note) step {
	return func(h *harness) {
		h.t.Helper()
		if err := h.l.SyncNotes(h.ctx, h.agent, notes, h.now); err != nil {
			h.t.Fatalf("SyncNotes: %v", err)
		}
	}
}

func take(fresh, resolved []string) step {
	return func(h *harness) {
		h.t.Helper()
		f, r, err := h.l.TakeUpdates(h.ctx, h.agent, h.now)
		if err != nil {
			h.t.Fatalf("TakeUpdates: %v", err)
		}
		if !reflect.DeepEqual(titles(f), fresh) || !reflect.DeepEqual(titles(r), resolved) {
			h.t.Fatalf("TakeUpdates = fresh %q, resolved %q; want fresh %q, resolved %q", titles(f), titles(r), fresh, resolved)
		}
	}
}

func act(name string) step {
	return func(h *harness) {
		h.t.Helper()
		if err := h.l.Acted(h.ctx, h.agent, noteID(name), h.now); err != nil {
			h.t.Fatalf("Acted: %v", err)
		}
	}
}

func dismiss(name, as string) step {
	return func(h *harness) {
		h.t.Helper()
		if err := h.l.Dismiss(h.ctx, h.agent, noteID(name), as, "because", h.now); err != nil {
			h.t.Fatalf("Dismiss: %v", err)
		}
	}
}

func markShown(names ...string) step {
	return func(h *harness) {
		h.t.Helper()
		var ids []string
		for _, n := range names {
			ids = append(ids, noteID(n))
		}
		if err := h.l.MarkShown(h.ctx, h.agent, ids, h.now); err != nil {
			h.t.Fatalf("MarkShown: %v", err)
		}
	}
}

// want checks a note's status, resolution and stored title.
func want(name string, status NoteStatus, res Resolution, title string) step {
	return func(h *harness) {
		h.t.Helper()
		sn, err := h.l.Note(h.ctx, h.agent, noteID(name))
		if err != nil {
			h.t.Fatalf("Note(%s): %v", name, err)
		}
		if sn.Status != status || sn.Resolution != res || sn.Note.Title != title {
			h.t.Fatalf("note %s = %s/%q %q; want %s/%q %q", name, sn.Status, sn.Resolution, sn.Note.Title, status, res, title)
		}
	}
}

func gone(name string) step {
	return func(h *harness) {
		h.t.Helper()
		if _, err := h.l.Note(h.ctx, h.agent, noteID(name)); !errors.Is(err, ErrNotFound) {
			h.t.Fatalf("Note(%s): err = %v, want ErrNotFound", name, err)
		}
	}
}

func wait(d time.Duration) step { return func(h *harness) { h.now = h.now.Add(d) } }

var none []string

func TestNoteLifecycle(t *testing.T) {
	a1, a2, b1 := testNote("a", "1"), testNote("a", "2"), testNote("b", "1")
	tests := []struct {
		name  string
		steps []step
	}{
		{"new, shown, open", []step{
			produce(a1), want("a", NoteNew, "", "a v1"),
			take([]string{"a v1"}, none), want("a", NoteOpen, "", "a v1"),
			produce(a1), want("a", NoteOpen, "", "a v1"),
			take(none, none),
		}},
		{"version change of an open note makes it new", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(a2), want("a", NoteNew, "", "a v2"),
			take([]string{"a v2"}, none), want("a", NoteOpen, "", "a v2"),
		}},
		{"version change of a new note keeps it new with the new text", []step{
			produce(a1), produce(a2), want("a", NoteNew, "", "a v2"),
			take([]string{"a v2"}, none),
		}},
		{"same version replaces the text and keeps the status", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(func() model.Note { n := a1; n.Title = "a v1 reworded"; return n }()),
			want("a", NoteOpen, "", "a v1 reworded"),
			take(none, none),
		}},
		{"no longer produced resolves by rule and says so once", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(), want("a", NoteResolved, ResolvedByRule, "a v1"),
			take(none, []string{"a v1"}),
			take(none, none),
			produce(), want("a", NoteResolved, ResolvedByRule, "a v1"),
			take(none, none),
		}},
		{"never shown and no longer produced resolves silently", []step{
			produce(a1), produce(), want("a", NoteResolved, ResolvedByRule, "a v1"),
			take(none, none),
		}},
		{"shown, changed, then gone still says resolved", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(a2), produce(), want("a", NoteResolved, ResolvedByRule, "a v2"),
			take(none, []string{"a v2"}),
		}},
		{"resolved by rule and produced again is new again", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(), take(none, []string{"a v1"}),
			produce(a1), want("a", NoteNew, "", "a v1"),
			take([]string{"a v1"}, none),
		}},
		{"produced again before the resolved line drops the line", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(), produce(a1),
			take([]string{"a v1"}, none),
		}},
		{"version change of a rule-resolved note makes it new", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(), take(none, []string{"a v1"}),
			produce(a2), want("a", NoteNew, "", "a v2"),
			take([]string{"a v2"}, none),
		}},
		{"acted stays resolved while the version is unchanged", []step{
			produce(a1), take([]string{"a v1"}, none),
			act("a"), want("a", NoteResolved, ResolvedByAgent, "a v1"),
			produce(a1), want("a", NoteResolved, ResolvedByAgent, "a v1"),
			produce(), want("a", NoteResolved, ResolvedByAgent, "a v1"),
			take(none, none),
		}},
		{"version change of an acted note makes it new", []step{
			produce(a1), take([]string{"a v1"}, none),
			act("a"), produce(a2), want("a", NoteNew, "", "a v2"),
			take([]string{"a v2"}, none),
		}},
		{"dismissed stays dismissed while the version is unchanged", []step{
			produce(a1), take([]string{"a v1"}, none),
			dismiss("a", "not_needed"), want("a", NoteDismissed, "", "a v1"),
			produce(a1), want("a", NoteDismissed, "", "a v1"),
			produce(), want("a", NoteDismissed, "", "a v1"),
			produce(a1), want("a", NoteDismissed, "", "a v1"),
			take(none, none),
		}},
		{"version change of a dismissed note makes it new", []step{
			produce(a1), take([]string{"a v1"}, none),
			dismiss("a", "false_alarm"), produce(a2), want("a", NoteNew, "", "a v2"),
			take([]string{"a v2"}, none),
		}},
		{"a new note can be dismissed before it is shown", []step{
			produce(a1), dismiss("a", "not_needed"),
			take(none, none),
		}},
		{"mark shown opens new notes", []step{
			produce(a1, b1), markShown("a"),
			want("a", NoteOpen, "", "a v1"), want("b", NoteNew, "", "b v1"),
			take([]string{"b v1"}, none),
		}},
		{"mark shown does not reopen resolved notes", []step{
			produce(a1), take([]string{"a v1"}, none),
			produce(), markShown("a"), want("a", NoteResolved, ResolvedByRule, "a v1"),
		}},
		{"closed notes are deleted 7 days after they close, once no longer produced", []step{
			produce(a1, b1, testNote("c", "1")), markShown("a", "b", "c"),
			dismiss("b", "not_needed"), act("c"),
			wait(8 * 24 * time.Hour),
			produce(a1, testNote("c", "1")),
			want("a", NoteOpen, "", "a v1"),
			gone("b"),
			want("c", NoteResolved, ResolvedByAgent, "c v1"), // still produced
			produce(testNote("c", "1")), want("a", NoteResolved, ResolvedByRule, "a v1"),
			wait(6 * 24 * time.Hour), produce(),
			want("a", NoteResolved, ResolvedByRule, "a v1"), // closed 6 days ago
			gone("c"), // acted 14 days ago, no longer produced
			wait(2 * 24 * time.Hour), produce(),
			gone("a"),
		}},
		{"dismissed and still produced is kept after 7 days", []step{
			produce(a1), dismiss("a", "not_needed"),
			wait(30 * 24 * time.Hour), produce(a1),
			want("a", NoteDismissed, "", "a v1"),
			take(none, none),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &harness{t: t, ctx: context.Background(), l: tempLedger(t), agent: "agent-a", now: t0}
			for _, s := range tt.steps {
				s(h)
				h.now = h.now.Add(time.Minute)
			}
		})
	}
}

func TestStoredNoteFields(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	n := testNote("a", "1")
	n.CreatedAt = t0.In(time.FixedZone("CEST", 2*3600))
	idle := t0.Add(-time.Hour).In(time.FixedZone("EST", -5*3600))
	n.Parties = []model.Party{{Agent: "codex-ci-1a2b3c", IdleSince: &idle}}
	if err := l.SyncNotes(ctx, "a", []model.Note{n}, t0); err != nil {
		t.Fatal(err)
	}
	if n.Parties[0].IdleSince != &idle {
		t.Fatal("SyncNotes changed the caller's note")
	}
	sn, err := l.Note(ctx, "a", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sn.Agent != "a" || !sn.FirstSeen.Equal(t0) || sn.ShownAt != nil || sn.ResolvedAt != nil {
		t.Fatalf("new note = %+v", sn)
	}
	if sn.Note.CreatedAt.Location() != time.UTC || !sn.Note.CreatedAt.Equal(t0) ||
		sn.Note.Parties[0].IdleSince.Location() != time.UTC || !sn.Note.Parties[0].IdleSince.Equal(idle) {
		t.Fatalf("times not stored in UTC: %v, %v", sn.Note.CreatedAt, sn.Note.Parties[0].IdleSince)
	}

	shown := t0.Add(time.Minute)
	if _, _, err := l.TakeUpdates(ctx, "a", shown); err != nil {
		t.Fatal(err)
	}
	dismissed := t0.Add(2 * time.Minute)
	if err := l.Dismiss(ctx, "a", n.ID[:6], "false_alarm", "not mine", dismissed); err != nil {
		t.Fatal(err)
	}
	sn, err = l.Note(ctx, "a", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sn.Status != NoteDismissed || sn.DismissedAs != "false_alarm" || sn.DismissReason != "not mine" ||
		sn.ShownAt == nil || !sn.ShownAt.Equal(shown) || sn.ResolvedAt == nil || !sn.ResolvedAt.Equal(dismissed) ||
		!sn.FirstSeen.Equal(t0) || sn.FirstSeen.Location() != time.UTC {
		t.Fatalf("dismissed note = %+v", sn)
	}

	// A version change clears the dismissal.
	if err := l.SyncNotes(ctx, "a", []model.Note{testNote("a", "2")}, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	sn, _ = l.Note(ctx, "a", n.ID)
	if sn.Status != NoteNew || sn.DismissedAs != "" || sn.DismissReason != "" || sn.ResolvedAt != nil || !sn.FirstSeen.Equal(t0) {
		t.Fatalf("renewed note = %+v", sn)
	}
}

func TestNoteOrderAndCounts(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	mk := func(name string, sev model.Severity, created time.Time) model.Note {
		n := testNote(name, "1")
		n.Severity, n.CreatedAt, n.Title = sev, created, name
		return n
	}
	notes := []model.Note{
		mk("info-early", model.SeverityInfo, t0),
		mk("act-late", model.SeverityAct, t0.Add(2*time.Minute)),
		mk("warn", model.SeverityWarn, t0),
		mk("act-early", model.SeverityAct, t0.Add(time.Minute)),
		mk("unknown-severity", "", t0.Add(-time.Minute)),
	}
	if err := l.SyncNotes(ctx, "a", notes, t0); err != nil {
		t.Fatal(err)
	}
	if err := l.SyncNotes(ctx, "b", notes[:1], t0); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"act-early", "act-late", "warn", "unknown-severity", "info-early"}

	open, err := l.OpenNotes(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sn := range open {
		got = append(got, sn.Note.Title)
	}
	if !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("OpenNotes order = %q, want %q", got, wantOrder)
	}
	if o, act, err := l.Counts(ctx, "a"); err != nil || o != 5 || act != 2 {
		t.Fatalf("Counts(a) = %d, %d, %v; want 5, 2", o, act, err)
	}
	if o, act, err := l.Counts(ctx, "b"); err != nil || o != 1 || act != 0 {
		t.Fatalf("Counts(b) = %d, %d, %v; want 1, 0", o, act, err)
	}
	if o, act, err := l.Counts(ctx, "nobody"); err != nil || o != 0 || act != 0 {
		t.Fatalf("Counts(nobody) = %d, %d, %v", o, act, err)
	}

	fresh, _, err := l.TakeUpdates(ctx, "a", t0)
	if err != nil || !reflect.DeepEqual(titles(fresh), wantOrder) {
		t.Fatalf("TakeUpdates = %q, %v; want %q", titles(fresh), err, wantOrder)
	}
	// Open notes still count after they are shown; b's are untouched.
	if o, _, _ := l.Counts(ctx, "a"); o != 5 {
		t.Fatalf("Counts(a) after shown = %d", o)
	}
	if fresh, _, _ := l.TakeUpdates(ctx, "b", t0); !reflect.DeepEqual(titles(fresh), []string{"info-early"}) {
		t.Fatalf("TakeUpdates(b) = %q", titles(fresh))
	}
	if err := l.Acted(ctx, "a", notes[1].ID, t0); err != nil {
		t.Fatal(err)
	}
	if o, act, _ := l.Counts(ctx, "a"); o != 4 || act != 1 {
		t.Fatalf("Counts(a) after acted = %d, %d", o, act)
	}
}

func TestSyncNotesInput(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	if err := l.SyncNotes(ctx, "", []model.Note{testNote("a", "1")}, t0); err == nil {
		t.Error("SyncNotes accepted an empty agent")
	}
	if err := l.SyncNotes(ctx, "a", []model.Note{{Title: "no id"}}, t0); err == nil {
		t.Error("SyncNotes accepted a note without id")
	}
	bad := testNote("a", "1")
	bad.Options = []model.Option{{Label: "x", Args: map[string]any{"ch": make(chan int)}}}
	if err := l.SyncNotes(ctx, "a", []model.Note{bad}, t0); err == nil {
		t.Error("SyncNotes accepted a note it cannot encode")
	}
	// A later duplicate replaces an earlier one.
	dup := testNote("a", "1")
	dup.Title = "second"
	if err := l.SyncNotes(ctx, "a", []model.Note{testNote("a", "1"), dup}, t0); err != nil {
		t.Fatal(err)
	}
	if open, _ := l.OpenNotes(ctx, "a"); len(open) != 1 || open[0].Note.Title != "second" {
		t.Fatalf("duplicates stored as %+v", open)
	}
}

func TestNoteLookup(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	mk := func(id string) model.Note { return model.Note{ID: id, Version: "1", Title: id} }
	if err := l.SyncNotes(ctx, "a", []model.Note{
		mk("n-abcd11111111"), mk("n-abcd22222222"), mk("n-ffff00000000"), mk("n-ffff0000"),
	}, t0); err != nil {
		t.Fatal(err)
	}
	if err := l.SyncNotes(ctx, "b", []model.Note{mk("n-0123456789ab")}, t0); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		agent model.AgentID
		in    string
		want  string
		err   error
	}{
		{"a", "n-abcd11111111", "n-abcd11111111", nil},
		{"a", "abcd11111111", "n-abcd11111111", nil},
		{"a", "abcd1", "n-abcd11111111", nil},
		{"a", "n-abcd2", "n-abcd22222222", nil},
		{"a", " N-ABCD2 ", "n-abcd22222222", nil},
		{"a", "abcd", "", ErrAmbiguous},
		{"a", "n-abcd", "", ErrAmbiguous},
		{"a", "ffff0000", "n-ffff0000", nil}, // exact id beats a longer one it prefixes
		{"a", "ffff00000", "n-ffff00000000", nil},
		{"a", "abc", "", ErrNotFound},
		{"a", "n-", "", ErrNotFound},
		{"a", "", "", ErrNotFound},
		{"a", "eeee", "", ErrNotFound},
		{"a", "0123456789ab", "", ErrNotFound}, // b's note
		{"b", "0123", "n-0123456789ab", nil},
		{"a", "ab%_", "", ErrNotFound},
	}
	for _, tt := range tests {
		sn, err := l.Note(ctx, tt.agent, tt.in)
		if tt.err != nil {
			if !errors.Is(err, tt.err) {
				t.Errorf("Note(%s, %q): err = %v, want %v", tt.agent, tt.in, err, tt.err)
			} else if !strings.HasPrefix(err.Error(), "ledger: ") {
				t.Errorf("Note(%s, %q): error %q lacks context", tt.agent, tt.in, err)
			}
			continue
		}
		if err != nil || sn.Note.ID != tt.want {
			t.Errorf("Note(%s, %q) = %q, %v; want %q", tt.agent, tt.in, sn.Note.ID, err, tt.want)
		}
	}
	// Acted and Dismiss resolve prefixes the same way.
	if err := l.Acted(ctx, "a", "abcd", t0); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("Acted(abcd): err = %v, want ErrAmbiguous", err)
	}
	if err := l.Dismiss(ctx, "a", "9999", "not_needed", "", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Dismiss(9999): err = %v, want ErrNotFound", err)
	}
	if err := l.Acted(ctx, "a", "abcd1", t0); err != nil {
		t.Fatal(err)
	}
	if sn, _ := l.Note(ctx, "a", "n-abcd11111111"); sn.Status != NoteResolved || sn.Resolution != ResolvedByAgent {
		t.Errorf("Acted by prefix: %s/%s", sn.Status, sn.Resolution)
	}
}

func TestDismissAndFalseAlarms(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	mk := func(kind model.NoteKind, subject string) model.Note {
		return model.Note{ID: model.NoteID(kind, subject, testItem), Version: "1", Kind: kind, Title: subject}
	}
	notesA := []model.Note{mk(model.NoteBodyOverlap, "x"), mk(model.NoteBodyOverlap, "y"), mk(model.NoteDuplicate, "z"), mk(model.NoteFileOverlap, "w")}
	notesB := []model.Note{mk(model.NoteBodyOverlap, "x")}
	if err := l.SyncNotes(ctx, "a", notesA, t0); err != nil {
		t.Fatal(err)
	}
	if err := l.SyncNotes(ctx, "b", notesB, t0); err != nil {
		t.Fatal(err)
	}
	if err := l.Dismiss(ctx, "a", notesA[0].ID, "maybe", "", t0); err == nil {
		t.Error("Dismiss accepted an unknown kind of dismissal")
	}
	for _, d := range []struct {
		agent model.AgentID
		id    string
		as    string
	}{
		{"a", notesA[0].ID, "false_alarm"},
		{"a", notesA[1].ID, "false_alarm"},
		{"a", notesA[2].ID, "false_alarm"},
		{"a", notesA[3].ID, "not_needed"},
		{"b", notesB[0].ID, "false_alarm"},
	} {
		if err := l.Dismiss(ctx, d.agent, d.id, d.as, "", t0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.FalseAlarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[model.NoteKind]int{model.NoteBodyOverlap: 3, model.NoteDuplicate: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FalseAlarms = %v, want %v", got, want)
	}
	empty, err := tempLedger(t).FalseAlarms(ctx)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("FalseAlarms of an empty ledger = %v, %v", empty, err)
	}
}

func TestMarkShownIgnoresUnknown(t *testing.T) {
	ctx := context.Background()
	l := tempLedger(t)
	if err := l.MarkShown(ctx, "a", nil, t0); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkShown(ctx, "a", []string{"n-000000000000", ""}, t0); err != nil {
		t.Fatal(err)
	}
}
