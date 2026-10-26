package ledger

// TODO(implement): everything below (SQLite via modernc.org/sqlite).

import (
	"context"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

type impl struct{}

func notImplemented() { panic("ledger: not implemented") }

func open(ctx context.Context, path string, opt Options) (*Ledger, error) {
	notImplemented()
	return nil, nil
}
func (l *Ledger) close() error   { notImplemented(); return nil }
func (l *Ledger) readOnly() bool { notImplemented(); return false }
func (l *Ledger) get(ctx context.Context, key string) (string, bool, error) {
	notImplemented()
	return "", false, nil
}
func (l *Ledger) set(ctx context.Context, key, value string) error { notImplemented(); return nil }
func (l *Ledger) del(ctx context.Context, key string) error        { notImplemented(); return nil }
func (l *Ledger) agentFor(ctx context.Context, worktree string) (model.AgentID, bool, error) {
	notImplemented()
	return "", false, nil
}
func (l *Ledger) setAgentFor(ctx context.Context, worktree string, id model.AgentID) error {
	notImplemented()
	return nil
}
func (l *Ledger) lastPushed(ctx context.Context, ref string) (string, error) {
	notImplemented()
	return "", nil
}
func (l *Ledger) setLastPushed(ctx context.Context, ref, oid string) error {
	notImplemented()
	return nil
}
func (l *Ledger) observeRefs(ctx context.Context, namespace string, current map[string]string, fetchedAt time.Time) (Observation, error) {
	notImplemented()
	return Observation{}, nil
}
func (l *Ledger) observed(ctx context.Context, namespace string) (Observation, error) {
	notImplemented()
	return Observation{}, nil
}
func (l *Ledger) resetFirstSeen(ctx context.Context, now time.Time) error {
	notImplemented()
	return nil
}
func (l *Ledger) syncNotes(ctx context.Context, agent model.AgentID, notes []model.Note, now time.Time) error {
	notImplemented()
	return nil
}
func (l *Ledger) takeUpdates(ctx context.Context, agent model.AgentID, now time.Time) ([]model.Note, []model.Note, error) {
	notImplemented()
	return nil, nil, nil
}
func (l *Ledger) openNotes(ctx context.Context, agent model.AgentID) ([]StoredNote, error) {
	notImplemented()
	return nil, nil
}
func (l *Ledger) markShown(ctx context.Context, agent model.AgentID, ids []string, now time.Time) error {
	notImplemented()
	return nil
}
func (l *Ledger) counts(ctx context.Context, agent model.AgentID) (int, int, error) {
	notImplemented()
	return 0, 0, nil
}
func (l *Ledger) note(ctx context.Context, agent model.AgentID, idOrPrefix string) (StoredNote, error) {
	notImplemented()
	return StoredNote{}, nil
}
func (l *Ledger) acted(ctx context.Context, agent model.AgentID, id string, now time.Time) error {
	notImplemented()
	return nil
}
func (l *Ledger) dismiss(ctx context.Context, agent model.AgentID, id, as, reason string, now time.Time) error {
	notImplemented()
	return nil
}
func (l *Ledger) falseAlarms(ctx context.Context) (map[model.NoteKind]int, error) {
	notImplemented()
	return nil, nil
}
func (l *Ledger) cacheFootprint(ctx context.Context, key string, fp model.Footprint) error {
	notImplemented()
	return nil
}
func (l *Ledger) cachedFootprint(ctx context.Context, key string) (model.Footprint, bool, error) {
	notImplemented()
	return model.Footprint{}, false, nil
}
func (l *Ledger) cacheMerge(ctx context.Context, a, b string, m MergeOutcome) error {
	notImplemented()
	return nil
}
func (l *Ledger) cachedMerge(ctx context.Context, a, b string) (MergeOutcome, bool, error) {
	notImplemented()
	return MergeOutcome{}, false, nil
}
func (l *Ledger) appendEvent(ctx context.Context, e Event) error { notImplemented(); return nil }
func (l *Ledger) events(ctx context.Context, agent model.AgentID, limit int) ([]Event, error) {
	notImplemented()
	return nil, nil
}
func (l *Ledger) rememberItem(ctx context.Context, agent model.AgentID, item model.WorkItem) error {
	notImplemented()
	return nil
}
func (l *Ledger) item(ctx context.Context, id model.ItemID) (model.WorkItem, model.AgentID, bool, error) {
	notImplemented()
	return model.WorkItem{}, "", false, nil
}
