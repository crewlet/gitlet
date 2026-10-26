package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/gitlet/internal/model"
)

func (l *Ledger) appendEvent(ctx context.Context, e Event) error {
	keep := l.keep.events
	return fail("append event", l.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (at, agent, kind, detail) VALUES (?, ?, ?, ?)`,
			nanos(e.At), string(e.Agent), e.Kind, e.Detail); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE id <= (SELECT id FROM events ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
		return err
	}))
}

// events returns the newest events first, in the order they were appended.
// A limit of zero or less returns every kept event.
func (l *Ledger) events(ctx context.Context, agent model.AgentID, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	var events []Event
	err := l.read(ctx, func(tx *sql.Tx) error {
		var rows *sql.Rows
		var err error
		if agent == "" {
			rows, err = tx.QueryContext(ctx, `SELECT at, agent, kind, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
		} else {
			rows, err = tx.QueryContext(ctx, `
SELECT at, agent, kind, detail FROM events WHERE agent = ? ORDER BY id DESC LIMIT ?`, string(agent), limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		events = nil
		for rows.Next() {
			var e Event
			var at sql.NullInt64
			var agent string
			if err := rows.Scan(&at, &agent, &e.Kind, &e.Detail); err != nil {
				return err
			}
			e.At, e.Agent = fromNanos(at), model.AgentID(agent)
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fail("events", err)
	}
	return events, nil
}

// rememberItem keeps the item with the newest UpdatedAt: a process holding
// an older copy never overwrites a newer one.
func (l *Ledger) rememberItem(ctx context.Context, agent model.AgentID, item model.WorkItem) error {
	const op = "remember item"
	if agent == "" {
		return fail(op, errEmptyAgent)
	}
	if item.ID == "" {
		return fail(op, errors.New("item has no id"))
	}
	item = utcItem(item)
	b, err := json.Marshal(item)
	if err != nil {
		return fail(op, fmt.Errorf("encode item %s: %w", item.ID, err))
	}
	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO items (id, agent, updated_at, item) VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET agent = excluded.agent, updated_at = excluded.updated_at, item = excluded.item
WHERE coalesce(excluded.updated_at, 0) >= coalesce(items.updated_at, 0)`,
			string(item.ID), string(agent), nanos(item.UpdatedAt), string(b))
		return err
	}))
}

func (l *Ledger) item(ctx context.Context, id model.ItemID) (model.WorkItem, model.AgentID, bool, error) {
	const op = "item"
	var agent, js string
	err := l.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT agent, item FROM items WHERE id = ?`, string(id)).Scan(&agent, &js)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return model.WorkItem{}, "", false, nil
	}
	if err != nil {
		return model.WorkItem{}, "", false, fail(op, err)
	}
	var item model.WorkItem
	if err := json.Unmarshal([]byte(js), &item); err != nil {
		return model.WorkItem{}, "", false, fail(op, fmt.Errorf("decode item %s: %w", id, err))
	}
	return item, model.AgentID(agent), true, nil
}

// utcItem returns item with its times in UTC.
func utcItem(item model.WorkItem) model.WorkItem {
	item.OpenedAt = utc(item.OpenedAt)
	item.UpdatedAt = utc(item.UpdatedAt)
	item.ClosedAt = utcPtr(item.ClosedAt)
	item.Footprint.ComputedAt = utc(item.Footprint.ComputedAt)
	return item
}
