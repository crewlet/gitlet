package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/crewlet/gitlet/internal/model"
)

// ---- key/value ----

func (l *Ledger) get(ctx context.Context, key string) (string, bool, error) {
	var value string
	var ok bool
	err := l.read(ctx, func(tx *sql.Tx) error {
		var err error
		value, ok, err = scanOptional(tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key))
		return err
	})
	if err != nil {
		return "", false, fail("get "+key, err)
	}
	return value, ok, nil
}

func (l *Ledger) set(ctx context.Context, key, value string) error {
	return fail("set "+key, l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	}))
}

func (l *Ledger) del(ctx context.Context, key string) error {
	return fail("delete "+key, l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	}))
}

// ---- identity ----

// worktreeKey normalizes a worktree root, so "/repo" and "/repo/" are one key.
func worktreeKey(worktree string) string {
	if worktree == "" {
		return ""
	}
	return filepath.Clean(worktree)
}

func (l *Ledger) agentFor(ctx context.Context, worktree string) (model.AgentID, bool, error) {
	var id string
	var ok bool
	err := l.read(ctx, func(tx *sql.Tx) error {
		var err error
		id, ok, err = scanOptional(tx.QueryRowContext(ctx,
			`SELECT agent FROM identities WHERE worktree = ?`, worktreeKey(worktree)))
		return err
	})
	if err != nil {
		return "", false, fail("agent for "+worktree, err)
	}
	return model.AgentID(id), ok, nil
}

func (l *Ledger) setAgentFor(ctx context.Context, worktree string, id model.AgentID) error {
	op := "set agent for " + worktree
	if !model.ValidAgentID(id) {
		return fail(op, fmt.Errorf("invalid agent id %q", id))
	}
	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO identities (worktree, agent) VALUES (?, ?)
ON CONFLICT (worktree) DO UPDATE SET agent = excluded.agent`, worktreeKey(worktree), string(id))
		return err
	}))
}

// ---- push bookkeeping ----

func (l *Ledger) lastPushed(ctx context.Context, ref string) (string, error) {
	var oid string
	err := l.read(ctx, func(tx *sql.Tx) error {
		var err error
		oid, _, err = scanOptional(tx.QueryRowContext(ctx, `SELECT oid FROM pushed WHERE ref = ?`, ref))
		return err
	})
	if err != nil {
		return "", fail("last pushed "+ref, err)
	}
	return oid, nil
}

func (l *Ledger) setLastPushed(ctx context.Context, ref, oid string) error {
	return fail("set last pushed "+ref, l.write(ctx, func(tx *sql.Tx) error {
		if oid == "" {
			_, err := tx.ExecContext(ctx, `DELETE FROM pushed WHERE ref = ?`, ref)
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO pushed (ref, oid) VALUES (?, ?)
ON CONFLICT (ref) DO UPDATE SET oid = excluded.oid`, ref, oid)
		return err
	}))
}

// scanOptional scans a single string column; ok is false when there is no row.
func scanOptional(row *sql.Row) (s string, ok bool, err error) {
	switch err := row.Scan(&s); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return s, true, nil
}
