// Package ledger is gitlet's local cache in SQLite (docs/SPEC.md, section 15).
// It is shared by the MCP server, the CLI and hook processes of every
// worktree of one clone. Everything in it can be rebuilt from refs except
// local note delivery state, which only matters locally.
//
// The database is opened in WAL mode with busy_timeout, synchronous=NORMAL,
// foreign keys on and immediate write transactions (_txlock=immediate), so
// concurrent processes wait instead of failing. The schema is migrated under
// PRAGMA user_version inside an immediate transaction; a binary that finds a
// newer schema opens the database read-only and ReadOnly reports true.
package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// Errors returned by the ledger.
var (
	ErrNotFound  = errors.New("not found")
	ErrAmbiguous = errors.New("ambiguous id prefix")
	ErrReadOnly  = errors.New("ledger is read-only (written by a newer gitlet)")
)

// NoteStatus is the local delivery state of a note (docs/SPEC.md, section 10.1).
type NoteStatus string

const (
	NoteNew       NoteStatus = "new"       // produced, not yet shown
	NoteOpen      NoteStatus = "open"      // shown, unresolved
	NoteResolved  NoteStatus = "resolved"  // the rule stopped producing it, or the agent acted on it
	NoteDismissed NoteStatus = "dismissed" // the agent dismissed it
)

// Resolution says why a note was resolved.
type Resolution string

const (
	ResolvedByRule  Resolution = "rule"  // the rule stopped producing it
	ResolvedByAgent Resolution = "acted" // the agent took one of its options
)

// StoredNote is a note with its local state.
type StoredNote struct {
	Note          model.Note
	Agent         model.AgentID
	Status        NoteStatus
	Resolution    Resolution // set when Status is NoteResolved
	DismissedAs   string     // "not_needed" or "false_alarm" when dismissed
	DismissReason string
	FirstSeen     time.Time // when this server first produced this note id
	ShownAt       *time.Time
	ResolvedAt    *time.Time
}

// Options controls Open.
type Options struct {
	BusyTimeout time.Duration // default 10 s; hooks use 1 s
}

// Ledger is an open ledger database. It is safe for concurrent use.
type Ledger struct {
	impl
}

// Open opens (creating if needed, with parent directories) the ledger at path.
func Open(ctx context.Context, path string, opt Options) (*Ledger, error) {
	return open(ctx, path, opt)
}

// Close closes the database.
func (l *Ledger) Close() error { return l.close() }

// ReadOnly reports whether the ledger was opened read-only because a newer
// gitlet migrated it. Writes then return ErrReadOnly.
func (l *Ledger) ReadOnly() bool { return l.readOnly() }

// ---- key/value ----

// Get returns a setting; ok is false when unset.
func (l *Ledger) Get(ctx context.Context, key string) (value string, ok bool, err error) {
	return l.get(ctx, key)
}

// Set stores a setting.
func (l *Ledger) Set(ctx context.Context, key, value string) error { return l.set(ctx, key, value) }

// Delete removes a setting.
func (l *Ledger) Delete(ctx context.Context, key string) error { return l.del(ctx, key) }

// ---- identity ----

// AgentFor returns the agent id stored for a worktree root.
func (l *Ledger) AgentFor(ctx context.Context, worktree string) (model.AgentID, bool, error) {
	return l.agentFor(ctx, worktree)
}

// SetAgentFor stores the agent id for a worktree root.
func (l *Ledger) SetAgentFor(ctx context.Context, worktree string, id model.AgentID) error {
	return l.setAgentFor(ctx, worktree, id)
}

// ---- push bookkeeping ----

// LastPushed returns the oid this clone last pushed to a ref ("" when never).
func (l *Ledger) LastPushed(ctx context.Context, ref string) (string, error) {
	return l.lastPushed(ctx, ref)
}

// SetLastPushed records the oid pushed to a ref ("" forgets it).
func (l *Ledger) SetLastPushed(ctx context.Context, ref, oid string) error {
	return l.setLastPushed(ctx, ref, oid)
}

// ---- ref observations (docs/SPEC.md, section 8) ----

// Observation is what a server knows about refs in one namespace.
type Observation struct {
	FirstSeen map[string]time.Time // ref -> when its current oid was first seen
	LastFetch time.Time            // last successful fetch of the namespace
}

// ObserveRefs records a full refresh of one namespace (for example
// "refs/gitlet/"): refs whose oid changed or that are new get firstSeen =
// fetchedAt, unchanged refs keep their firstSeen, refs of the namespace
// missing from current are forgotten, and the namespace's last fetch time
// becomes fetchedAt. It returns the namespace's observation.
func (l *Ledger) ObserveRefs(ctx context.Context, namespace string, current map[string]string, fetchedAt time.Time) (Observation, error) {
	return l.observeRefs(ctx, namespace, current, fetchedAt)
}

// Observed returns the observation of a namespace without changing it.
func (l *Ledger) Observed(ctx context.Context, namespace string) (Observation, error) {
	return l.observed(ctx, namespace)
}

// ResetFirstSeen sets every first-seen time to now, after the machine slept.
func (l *Ledger) ResetFirstSeen(ctx context.Context, now time.Time) error {
	return l.resetFirstSeen(ctx, now)
}

// ---- notes (docs/SPEC.md, section 10.1) ----

// SyncNotes reconciles the notes the rules produce now for agent with the
// stored ones, in one immediate transaction:
//   - an unknown id is inserted as new, with FirstSeen = now;
//   - a known id whose Version changed becomes new again (even if it was
//     resolved or dismissed); its stored note is replaced;
//   - a known id with the same Version keeps its status, except one resolved
//     by rule, which becomes new again; its stored note text is replaced;
//   - a new or open note that is no longer produced becomes resolved by rule;
//     if it was never shown its resolution counts as already shown.
//
// Dismissed and acted-on notes that are no longer produced stay as they are.
// Resolved and dismissed notes older than 7 days are deleted.
func (l *Ledger) SyncNotes(ctx context.Context, agent model.AgentID, notes []model.Note, now time.Time) error {
	return l.syncNotes(ctx, agent, notes, now)
}

// TakeUpdates atomically returns agent's new notes (ordered by severity,
// then creation) and marks them open, and returns notes resolved since they
// were shown, marking their resolution as shown. Two processes never get the
// same update.
func (l *Ledger) TakeUpdates(ctx context.Context, agent model.AgentID, now time.Time) (fresh, resolved []model.Note, err error) {
	return l.takeUpdates(ctx, agent, now)
}

// OpenNotes returns agent's new and open notes, ordered by severity then
// creation. It does not change their state.
func (l *Ledger) OpenNotes(ctx context.Context, agent model.AgentID) ([]StoredNote, error) {
	return l.openNotes(ctx, agent)
}

// MarkShown marks the given notes of agent open (used by status).
func (l *Ledger) MarkShown(ctx context.Context, agent model.AgentID, ids []string, now time.Time) error {
	return l.markShown(ctx, agent, ids, now)
}

// Counts returns how many of agent's notes are new or open, and how many of
// those are act notes.
func (l *Ledger) Counts(ctx context.Context, agent model.AgentID) (open, openAct int, err error) {
	return l.counts(ctx, agent)
}

// Note returns one of agent's notes by full id or unique prefix (with or
// without "n-", at least 4 characters after it). ErrNotFound or ErrAmbiguous
// otherwise.
func (l *Ledger) Note(ctx context.Context, agent model.AgentID, idOrPrefix string) (StoredNote, error) {
	return l.note(ctx, agent, idOrPrefix)
}

// Acted marks a note resolved because the agent took one of its options.
func (l *Ledger) Acted(ctx context.Context, agent model.AgentID, id string, now time.Time) error {
	return l.acted(ctx, agent, id, now)
}

// Dismiss marks a note dismissed. as is "not_needed" or "false_alarm".
func (l *Ledger) Dismiss(ctx context.Context, agent model.AgentID, id, as, reason string, now time.Time) error {
	return l.dismiss(ctx, agent, id, as, reason, now)
}

// FalseAlarms counts notes dismissed as false alarms, by kind, across agents.
func (l *Ledger) FalseAlarms(ctx context.Context) (map[model.NoteKind]int, error) {
	return l.falseAlarms(ctx)
}

// ---- caches ----

// CacheFootprint stores a computed footprint under a caller-chosen key (for
// example base + ".." + head), and CachedFootprint returns it. The cache keeps
// the 500 most recently stored entries.
func (l *Ledger) CacheFootprint(ctx context.Context, key string, fp model.Footprint) error {
	return l.cacheFootprint(ctx, key, fp)
}

// CachedFootprint returns a cached footprint; ok is false on a miss.
func (l *Ledger) CachedFootprint(ctx context.Context, key string) (fp model.Footprint, ok bool, err error) {
	return l.cachedFootprint(ctx, key)
}

// MergeOutcome is a cached merge-tree result.
type MergeOutcome struct {
	Clean     bool
	Conflicts []string
}

// CacheMerge stores the merge-tree outcome of two commits (order-insensitive),
// keeping the 2000 most recent.
func (l *Ledger) CacheMerge(ctx context.Context, a, b string, m MergeOutcome) error {
	return l.cacheMerge(ctx, a, b, m)
}

// CachedMerge returns a cached merge outcome; ok is false on a miss.
func (l *Ledger) CachedMerge(ctx context.Context, a, b string) (m MergeOutcome, ok bool, err error) {
	return l.cachedMerge(ctx, a, b)
}

// ---- history ----

// Event is one entry of the local event log.
type Event struct {
	At     time.Time
	Agent  model.AgentID
	Kind   string // the verb or background action: "start", "snapshot", "claim", "publish", ...
	Detail string // JSON
}

// AppendEvent appends to the event log (the 5000 newest are kept).
func (l *Ledger) AppendEvent(ctx context.Context, e Event) error { return l.appendEvent(ctx, e) }

// Events returns the newest events for agent ("" for all), at most limit.
func (l *Ledger) Events(ctx context.Context, agent model.AgentID, limit int) ([]Event, error) {
	return l.events(ctx, agent, limit)
}

// RememberItem stores the latest version of an item, so why can explain
// commits after the item left the agent's state.
func (l *Ledger) RememberItem(ctx context.Context, agent model.AgentID, item model.WorkItem) error {
	return l.rememberItem(ctx, agent, item)
}

// Item returns a remembered item and its agent.
func (l *Ledger) Item(ctx context.Context, id model.ItemID) (model.WorkItem, model.AgentID, bool, error) {
	return l.item(ctx, id)
}
