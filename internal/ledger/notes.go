package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/gitlet/internal/model"
)

// Note delivery (docs/SPEC.md, section 10.1). Besides its status, a stored
// note keeps two facts the lifecycle needs:
//
//   - shown_at: when the note id was last shown. It survives a version
//     change, so an agent that saw an earlier version still hears that the
//     note resolved.
//   - resolution_shown: whether the "resolved: <title>" line is delivered,
//     or not owed (never shown, acted on, dismissed).
//
// closed_at is when the note was resolved or dismissed; StoredNote reports
// it as ResolvedAt for both.

const (
	// noteRetention is how long resolved and dismissed notes are kept once
	// the rules no longer produce them.
	noteRetention = 7 * 24 * time.Hour

	// minNotePrefix is the shortest note id prefix accepted after "n-".
	minNotePrefix = 4

	dismissNotNeeded  = "not_needed"
	dismissFalseAlarm = "false_alarm"
)

// noteOrder sorts by severity, then creation (docs/SPEC.md, section 4.2).
const noteOrder = ` ORDER BY severity_rank, coalesce(created_at, first_seen), id`

// noteColumns are the columns scanStoredNote reads, in order.
const noteColumns = `agent, status, resolution, dismissed_as, dismiss_reason, first_seen, shown_at, closed_at, note`

// storedState is the part of a stored note that SyncNotes reconciles.
type storedState struct {
	version    string
	status     NoteStatus
	resolution Resolution
	shown      bool
	closedAt   sql.NullInt64
	note       string
}

// encodedNote is a produced note ready to store.
type encodedNote struct {
	note model.Note
	json string
}

func (l *Ledger) syncNotes(ctx context.Context, agent model.AgentID, notes []model.Note, now time.Time) error {
	const op = "sync notes"
	if agent == "" {
		return fail(op, errEmptyAgent)
	}
	// Encode outside the transaction. A later duplicate id replaces an
	// earlier one; order keeps the first position of each id.
	produced := make(map[string]encodedNote, len(notes))
	order := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.ID == "" {
			return fail(op, fmt.Errorf("note %q has no id", n.Title))
		}
		n = utcNote(n)
		b, err := json.Marshal(n)
		if err != nil {
			return fail(op, fmt.Errorf("encode note %s: %w", n.ID, err))
		}
		if _, dup := produced[n.ID]; !dup {
			order = append(order, n.ID)
		}
		produced[n.ID] = encodedNote{note: n, json: string(b)}
	}
	at := nanos(now)
	cutoff := nanos(now.Add(-noteRetention))

	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		stored, err := loadNoteStates(ctx, tx, agent)
		if err != nil {
			return err
		}
		insert, err := tx.PrepareContext(ctx, `
INSERT INTO notes (agent, id, version, kind, severity_rank, created_at, status, first_seen, note)
VALUES (?, ?, ?, ?, ?, ?, 'new', ?, ?)`)
		if err != nil {
			return err
		}
		defer insert.Close()
		// renew makes a note new again, forgetting how it was resolved or
		// dismissed; first_seen and shown_at stay.
		renew, err := tx.PrepareContext(ctx, `
UPDATE notes SET version = ?, kind = ?, severity_rank = ?, created_at = ?, note = ?,
	status = 'new', resolution = '', dismissed_as = '', dismiss_reason = '',
	closed_at = NULL, resolution_shown = 0
WHERE agent = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer renew.Close()
		replace, err := tx.PrepareContext(ctx, `
UPDATE notes SET kind = ?, severity_rank = ?, created_at = ?, note = ? WHERE agent = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer replace.Close()
		resolve, err := tx.PrepareContext(ctx, `
UPDATE notes SET status = 'resolved', resolution = 'rule', closed_at = ?, resolution_shown = ?
WHERE agent = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer resolve.Close()
		remove, err := tx.PrepareContext(ctx, `DELETE FROM notes WHERE agent = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer remove.Close()

		for _, id := range order {
			e := produced[id]
			n := e.note
			kind, rank, created := string(n.Kind), n.Severity.Rank(), nanos(n.CreatedAt)
			old, known := stored[id]
			var err error
			switch {
			case !known:
				_, err = insert.ExecContext(ctx, agent, id, n.Version, kind, rank, created, at, e.json)
			case old.version != n.Version || (old.status == NoteResolved && old.resolution == ResolvedByRule):
				_, err = renew.ExecContext(ctx, n.Version, kind, rank, created, e.json, agent, id)
			case old.note != e.json:
				_, err = replace.ExecContext(ctx, kind, rank, created, e.json, agent, id)
			}
			if err != nil {
				return err
			}
		}
		for id, old := range stored {
			if _, ok := produced[id]; ok {
				continue
			}
			var err error
			switch old.status {
			case NoteNew, NoteOpen:
				// A note never shown owes no "resolved" line.
				_, err = resolve.ExecContext(ctx, at, boolInt(!old.shown), agent, id)
			case NoteResolved, NoteDismissed:
				if old.closedAt.Valid && cutoff.Valid && old.closedAt.Int64 < cutoff.Int64 {
					_, err = remove.ExecContext(ctx, agent, id)
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	}))
}

func loadNoteStates(ctx context.Context, tx *sql.Tx, agent model.AgentID) (map[string]storedState, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, version, status, resolution, shown_at IS NOT NULL, closed_at, note FROM notes WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stored := make(map[string]storedState)
	for rows.Next() {
		var id string
		var s storedState
		if err := rows.Scan(&id, &s.version, &s.status, &s.resolution, &s.shown, &s.closedAt, &s.note); err != nil {
			return nil, err
		}
		stored[id] = s
	}
	return stored, rows.Err()
}

func (l *Ledger) takeUpdates(ctx context.Context, agent model.AgentID, now time.Time) ([]model.Note, []model.Note, error) {
	const op = "take updates"
	if l.ro {
		return nil, nil, ErrReadOnly
	}
	// Hooks call this on every prompt and tool use; look before taking the
	// write lock, which is only needed when there is something to deliver.
	var pending bool
	err := l.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM notes WHERE agent = ? AND (status = 'new' OR status = 'resolved' AND resolution_shown = 0))`,
			agent).Scan(&pending)
	})
	if err != nil || !pending {
		return nil, nil, fail(op, err)
	}

	var fresh, resolved []model.Note
	err = l.write(ctx, func(tx *sql.Tx) error {
		var err error
		if fresh, err = queryNotes(ctx, tx, `SELECT note FROM notes WHERE agent = ? AND status = 'new'`+noteOrder, agent); err != nil {
			return err
		}
		if resolved, err = queryNotes(ctx, tx, `
SELECT note FROM notes WHERE agent = ? AND status = 'resolved' AND resolution_shown = 0
ORDER BY closed_at, id`, agent); err != nil {
			return err
		}
		if len(fresh) > 0 {
			if _, err := tx.ExecContext(ctx, `
UPDATE notes SET status = 'open', shown_at = ? WHERE agent = ? AND status = 'new'`, nanos(now), agent); err != nil {
				return err
			}
		}
		if len(resolved) > 0 {
			if _, err := tx.ExecContext(ctx, `
UPDATE notes SET resolution_shown = 1 WHERE agent = ? AND status = 'resolved' AND resolution_shown = 0`, agent); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fail(op, err)
	}
	return fresh, resolved, nil
}

// queryNotes runs a query whose only column is a note's JSON.
func queryNotes(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]model.Note, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []model.Note
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			return nil, err
		}
		var n model.Note
		if err := json.Unmarshal([]byte(js), &n); err != nil {
			return nil, fmt.Errorf("decode stored note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

func (l *Ledger) openNotes(ctx context.Context, agent model.AgentID) ([]StoredNote, error) {
	var notes []StoredNote
	err := l.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT `+noteColumns+` FROM notes WHERE agent = ? AND status IN ('new', 'open')`+noteOrder, agent)
		if err != nil {
			return err
		}
		defer rows.Close()
		notes = nil
		for rows.Next() {
			sn, err := scanStoredNote(rows)
			if err != nil {
				return err
			}
			notes = append(notes, sn)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fail("open notes", err)
	}
	return notes, nil
}

func (l *Ledger) markShown(ctx context.Context, agent model.AgentID, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return fail("mark shown", l.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
UPDATE notes SET status = 'open', shown_at = ? WHERE agent = ? AND id = ? AND status IN ('new', 'open')`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, id := range ids {
			if _, err := stmt.ExecContext(ctx, nanos(now), agent, id); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (l *Ledger) counts(ctx context.Context, agent model.AgentID) (int, int, error) {
	var open, openAct int
	err := l.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
SELECT count(*), coalesce(sum(severity_rank = 0), 0) FROM notes WHERE agent = ? AND status IN ('new', 'open')`,
			agent).Scan(&open, &openAct)
	})
	if err != nil {
		return 0, 0, fail("counts", err)
	}
	return open, openAct, nil
}

func (l *Ledger) note(ctx context.Context, agent model.AgentID, idOrPrefix string) (StoredNote, error) {
	var sn StoredNote
	err := l.read(ctx, func(tx *sql.Tx) error {
		var err error
		sn, err = findNote(ctx, tx, agent, idOrPrefix)
		return err
	})
	if err != nil {
		return StoredNote{}, fail("note", err)
	}
	return sn, nil
}

// findNote looks a note up by full id or unique prefix, with or without "n-".
func findNote(ctx context.Context, tx *sql.Tx, agent model.AgentID, idOrPrefix string) (StoredNote, error) {
	p := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(idOrPrefix)), "n-")
	if len(p) < minNotePrefix {
		return StoredNote{}, fmt.Errorf("note %q: %w (give at least %d characters after n-)", idOrPrefix, ErrNotFound, minNotePrefix)
	}
	full := "n-" + p
	// An exact match sorts first, so a full id wins over longer ids it prefixes.
	rows, err := tx.QueryContext(ctx, `
SELECT `+noteColumns+` FROM notes WHERE agent = ? AND substr(id, 1, ?) = ?
ORDER BY id = ? DESC, id LIMIT 2`, agent, utf8.RuneCountInString(full), full, full)
	if err != nil {
		return StoredNote{}, err
	}
	defer rows.Close()
	var found []StoredNote
	for rows.Next() {
		sn, err := scanStoredNote(rows)
		if err != nil {
			return StoredNote{}, err
		}
		found = append(found, sn)
	}
	if err := rows.Err(); err != nil {
		return StoredNote{}, err
	}
	switch {
	case len(found) == 0:
		return StoredNote{}, fmt.Errorf("note %q: %w", idOrPrefix, ErrNotFound)
	case len(found) == 1 || found[0].Note.ID == full:
		return found[0], nil
	}
	return StoredNote{}, fmt.Errorf("note %q: %w", idOrPrefix, ErrAmbiguous)
}

func scanStoredNote(rows *sql.Rows) (StoredNote, error) {
	var sn StoredNote
	var agent, js string
	var firstSeen, shownAt, closedAt sql.NullInt64
	if err := rows.Scan(&agent, &sn.Status, &sn.Resolution, &sn.DismissedAs, &sn.DismissReason,
		&firstSeen, &shownAt, &closedAt, &js); err != nil {
		return StoredNote{}, err
	}
	if err := json.Unmarshal([]byte(js), &sn.Note); err != nil {
		return StoredNote{}, fmt.Errorf("decode stored note: %w", err)
	}
	sn.Agent = model.AgentID(agent)
	sn.FirstSeen = fromNanos(firstSeen)
	sn.ShownAt = fromNanosPtr(shownAt)
	sn.ResolvedAt = fromNanosPtr(closedAt)
	return sn, nil
}

func (l *Ledger) acted(ctx context.Context, agent model.AgentID, id string, now time.Time) error {
	return fail("acted", l.write(ctx, func(tx *sql.Tx) error {
		sn, err := findNote(ctx, tx, agent, id)
		if err != nil {
			return err
		}
		if sn.Status == NoteResolved && sn.Resolution == ResolvedByAgent {
			return nil
		}
		// The agent acted itself, so no "resolved" line is owed.
		_, err = tx.ExecContext(ctx, `
UPDATE notes SET status = 'resolved', resolution = 'acted', dismissed_as = '', dismiss_reason = '',
	closed_at = ?, resolution_shown = 1
WHERE agent = ? AND id = ?`, nanos(now), agent, sn.Note.ID)
		return err
	}))
}

func (l *Ledger) dismiss(ctx context.Context, agent model.AgentID, id, as, reason string, now time.Time) error {
	const op = "dismiss"
	if as != dismissNotNeeded && as != dismissFalseAlarm {
		return fail(op, fmt.Errorf("dismiss as %q: want %q or %q", as, dismissNotNeeded, dismissFalseAlarm))
	}
	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		sn, err := findNote(ctx, tx, agent, id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
UPDATE notes SET status = 'dismissed', resolution = '', dismissed_as = ?, dismiss_reason = ?,
	closed_at = ?, resolution_shown = 1
WHERE agent = ? AND id = ?`, as, reason, nanos(now), agent, sn.Note.ID)
		return err
	}))
}

func (l *Ledger) falseAlarms(ctx context.Context) (map[model.NoteKind]int, error) {
	var counts map[model.NoteKind]int
	err := l.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT kind, count(*) FROM notes WHERE status = 'dismissed' AND dismissed_as = 'false_alarm' GROUP BY kind`)
		if err != nil {
			return err
		}
		defer rows.Close()
		counts = make(map[model.NoteKind]int)
		for rows.Next() {
			var kind string
			var n int
			if err := rows.Scan(&kind, &n); err != nil {
				return err
			}
			counts[model.NoteKind(kind)] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fail("false alarms", err)
	}
	return counts, nil
}

// utcNote returns n with its times in UTC, copying what it changes.
func utcNote(n model.Note) model.Note {
	n.CreatedAt = utc(n.CreatedAt)
	if len(n.Parties) > 0 {
		parties := make([]model.Party, len(n.Parties))
		for i, p := range n.Parties {
			p.IdleSince = utcPtr(p.IdleSince)
			parties[i] = p
		}
		n.Parties = parties
	}
	return n
}
