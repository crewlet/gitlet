package engine

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/ledger"
	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/render"
)

// fakeNotes is an in-memory noteStore with the ledger's delivery states.
type fakeNotes struct {
	order    []string
	notes    map[string]*ledger.StoredNote
	resolved []model.Note // resolved, not reported yet
	readOnly bool
	marked   [][]string // MarkShown calls
	takes    int        // TakeUpdates calls
}

func newFakeNotes(notes ...model.Note) *fakeNotes {
	f := &fakeNotes{notes: map[string]*ledger.StoredNote{}}
	for _, n := range notes {
		f.add(n, ledger.NoteNew)
	}
	return f
}

func (f *fakeNotes) add(n model.Note, st ledger.NoteStatus) {
	f.order = append(f.order, n.ID)
	f.notes[n.ID] = &ledger.StoredNote{Note: n, Status: st}
}

func (f *fakeNotes) SyncNotes(context.Context, model.AgentID, []model.Note, time.Time) error {
	return nil
}

func (f *fakeNotes) TakeUpdates(_ context.Context, _ model.AgentID, now time.Time) (fresh, resolved []model.Note, err error) {
	if f.readOnly {
		return nil, nil, ledger.ErrReadOnly
	}
	f.takes++
	for _, id := range f.order {
		if s := f.notes[id]; s.Status == ledger.NoteNew {
			s.Status, s.ShownAt = ledger.NoteOpen, &now
			fresh = append(fresh, s.Note)
		}
	}
	resolved, f.resolved = f.resolved, nil
	return fresh, resolved, nil
}

func (f *fakeNotes) OpenNotes(context.Context, model.AgentID) ([]ledger.StoredNote, error) {
	var out []ledger.StoredNote
	for _, id := range f.order {
		if s := f.notes[id]; s.Status == ledger.NoteNew || s.Status == ledger.NoteOpen {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeNotes) MarkShown(_ context.Context, _ model.AgentID, ids []string, now time.Time) error {
	if f.readOnly {
		return ledger.ErrReadOnly
	}
	f.marked = append(f.marked, append([]string(nil), ids...))
	for _, id := range ids {
		if s, ok := f.notes[id]; ok && s.Status == ledger.NoteNew {
			s.Status, s.ShownAt = ledger.NoteOpen, &now
		}
	}
	return nil
}

func (f *fakeNotes) Counts(context.Context, model.AgentID) (open, openAct int, err error) {
	for _, s := range f.notes {
		if s.Status == ledger.NoteNew || s.Status == ledger.NoteOpen {
			open++
			if s.Note.Severity == model.SeverityAct {
				openAct++
			}
		}
	}
	return open, openAct, nil
}

func (f *fakeNotes) Note(_ context.Context, _ model.AgentID, id string) (ledger.StoredNote, error) {
	if s, ok := f.notes[id]; ok {
		return *s, nil
	}
	return ledger.StoredNote{}, errors.New("no such note")
}

func (f *fakeNotes) Acted(_ context.Context, _ model.AgentID, id string, _ time.Time) error {
	if s, ok := f.notes[id]; ok {
		s.Status = ledger.NoteResolved
		return nil
	}
	return errors.New("no such note")
}

func (f *fakeNotes) status(id string) ledger.NoteStatus { return f.notes[id].Status }

func note(id string, sev model.Severity) model.Note {
	return model.Note{ID: id, Severity: sev, Title: "title " + id}
}

func ids(notes []model.Note) []string {
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.ID
	}
	return out
}

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func TestDecorateFewNewNotes(t *testing.T) {
	f := newFakeNotes(note("n-info", model.SeverityInfo), note("n-act", model.SeverityAct))
	f.resolved = []model.Note{note("n-gone", model.SeverityWarn)}
	var r api.Result
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil {
		t.Fatal(err)
	}
	if got := ids(r.Notes); !reflect.DeepEqual(got, []string{"n-act", "n-info"}) {
		t.Errorf("notes = %v, want severity order", got)
	}
	if r.MoreNotes != 0 || !reflect.DeepEqual(r.Resolved, []string{"title n-gone"}) {
		t.Errorf("more %d, resolved %v", r.MoreNotes, r.Resolved)
	}
	if f.status("n-info") != ledger.NoteOpen || f.status("n-act") != ledger.NoteOpen {
		t.Error("shown notes not marked")
	}
	if r.Open != 2 || r.OpenAct != 1 || r.AllNotes {
		t.Errorf("counts %d/%d, all %v", r.Open, r.OpenAct, r.AllNotes)
	}

	// The next result has nothing new.
	r = api.Result{}
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil {
		t.Fatal(err)
	}
	if len(r.Notes) != 0 || len(r.Resolved) != 0 || r.Open != 2 {
		t.Errorf("second result: %+v", r)
	}
}

func TestDecorateCapsNewNotes(t *testing.T) {
	f := newFakeNotes(
		note("n-i1", model.SeverityInfo),
		note("n-w1", model.SeverityWarn),
		note("n-a1", model.SeverityAct),
		note("n-i2", model.SeverityInfo),
		note("n-w2", model.SeverityWarn),
	)
	f.resolved = []model.Note{note("n-gone", model.SeverityWarn)}
	var r api.Result
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil {
		t.Fatal(err)
	}
	want := []string{"n-a1", "n-w1", "n-w2"}
	if got := ids(r.Notes); !reflect.DeepEqual(got, want) || len(r.Notes) != render.MaxFullNotes {
		t.Errorf("notes = %v, want %v", got, want)
	}
	if r.MoreNotes != 2 {
		t.Errorf("more = %d", r.MoreNotes)
	}
	if len(f.marked) != 1 || !reflect.DeepEqual(f.marked[0], want) {
		t.Errorf("marked %v, want only the shown notes", f.marked)
	}
	if f.status("n-i1") != ledger.NoteNew || f.status("n-i2") != ledger.NoteNew {
		t.Error("notes beyond the cap must stay new")
	}
	if f.takes != 0 {
		t.Error("TakeUpdates would mark notes that were not shown")
	}
	if r.Open != 5 || r.OpenAct != 1 {
		t.Errorf("counts %d/%d", r.Open, r.OpenAct)
	}

	// The two left over come with the next result, with the resolved title.
	r = api.Result{}
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil {
		t.Fatal(err)
	}
	if got := ids(r.Notes); !reflect.DeepEqual(got, []string{"n-i1", "n-i2"}) || r.MoreNotes != 0 {
		t.Errorf("next result notes = %v (+%d)", got, r.MoreNotes)
	}
	if !reflect.DeepEqual(r.Resolved, []string{"title n-gone"}) {
		t.Errorf("resolved = %v", r.Resolved)
	}
}

func TestDecorateStatusListsAndMarksEveryOpenNote(t *testing.T) {
	f := newFakeNotes(
		note("n-i1", model.SeverityInfo),
		note("n-w1", model.SeverityWarn),
		note("n-a1", model.SeverityAct),
		note("n-i2", model.SeverityInfo),
	)
	f.add(note("n-old", model.SeverityAct), ledger.NoteOpen)
	f.add(note("n-done", model.SeverityAct), ledger.NoteResolved)
	f.resolved = []model.Note{note("n-done", model.SeverityAct)}
	var r api.Result
	if err := decorateNotes(context.Background(), f, "me", t0, &r, true); err != nil {
		t.Fatal(err)
	}
	if !r.AllNotes || r.MoreNotes != 0 {
		t.Errorf("all %v more %d", r.AllNotes, r.MoreNotes)
	}
	want := []string{"n-a1", "n-old", "n-w1", "n-i1", "n-i2"}
	if got := ids(r.Notes); !reflect.DeepEqual(got, want) {
		t.Errorf("notes = %v, want %v", got, want)
	}
	if len(f.marked) != 1 {
		t.Fatalf("MarkShown calls: %v", f.marked)
	}
	marked := append([]string(nil), f.marked[0]...)
	sort.Strings(marked)
	if !reflect.DeepEqual(marked, []string{"n-a1", "n-i1", "n-i2", "n-old", "n-w1"}) {
		t.Errorf("marked %v, want exactly the listed notes", marked)
	}
	if !reflect.DeepEqual(r.Resolved, []string{"title n-done"}) {
		t.Errorf("resolved = %v", r.Resolved)
	}
	if r.Open != 5 || r.OpenAct != 2 {
		t.Errorf("counts %d/%d", r.Open, r.OpenAct)
	}
}

func TestDecorateReadOnlyLedger(t *testing.T) {
	f := newFakeNotes(note("n-1", model.SeverityWarn), note("n-2", model.SeverityAct))
	f.readOnly = true
	var r api.Result
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil {
		t.Fatalf("read-only ledger: %v", err)
	}
	if got := ids(r.Notes); !reflect.DeepEqual(got, []string{"n-2", "n-1"}) {
		t.Errorf("notes = %v", got)
	}
	if f.status("n-1") != ledger.NoteNew {
		t.Error("read-only ledger was written")
	}
	r = api.Result{}
	if err := decorateNotes(context.Background(), f, "me", t0, &r, true); err != nil {
		t.Fatalf("read-only status: %v", err)
	}
	if !r.AllNotes || len(r.Notes) != 2 {
		t.Errorf("status on a read-only ledger: %+v", r)
	}
	// Beyond the cap, MarkShown's ErrReadOnly is not an error either.
	f = newFakeNotes(note("a", model.SeverityInfo), note("b", model.SeverityInfo), note("c", model.SeverityInfo), note("d", model.SeverityInfo))
	f.readOnly = true
	r = api.Result{}
	if err := decorateNotes(context.Background(), f, "me", t0, &r, false); err != nil || len(r.Notes) != 3 || r.MoreNotes != 1 {
		t.Errorf("capped read-only: %v %+v", err, r)
	}
}

func TestCapNotes(t *testing.T) {
	in := []model.Note{
		note("i1", model.SeverityInfo),
		note("w1", model.SeverityWarn),
		note("a1", model.SeverityAct),
		note("i2", model.SeverityInfo),
		note("a2", model.SeverityAct),
	}
	orig := ids(in)
	shown, more := capNotes(in, 3)
	if got := ids(shown); !reflect.DeepEqual(got, []string{"a1", "a2", "w1"}) || more != 2 {
		t.Errorf("capNotes = %v, %d", got, more)
	}
	if !reflect.DeepEqual(ids(in), orig) {
		t.Error("capNotes reordered its input")
	}
	shown, more = capNotes(in[:2], 3)
	if got := ids(shown); !reflect.DeepEqual(got, []string{"w1", "i1"}) || more != 0 {
		t.Errorf("under the cap = %v, %d", got, more)
	}
	if shown, more := capNotes(nil, 3); len(shown) != 0 || more != 0 {
		t.Errorf("empty = %v, %d", shown, more)
	}
}

func TestNewNotesOnlyUnshown(t *testing.T) {
	stored := []ledger.StoredNote{
		{Note: note("open", model.SeverityAct), Status: ledger.NoteOpen},
		{Note: note("new-info", model.SeverityInfo), Status: ledger.NoteNew},
		{Note: note("new-act", model.SeverityAct), Status: ledger.NoteNew},
	}
	if got := ids(newNotes(stored)); !reflect.DeepEqual(got, []string{"new-act", "new-info"}) {
		t.Errorf("newNotes = %v", got)
	}
}
