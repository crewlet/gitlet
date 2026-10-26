package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are the schema steps in order: migrations[i] takes the schema
// from user_version i to i+1. Append new steps; never edit a released one.
//
// Times are INTEGER unix nanoseconds (UTC), NULL for the zero time. Tables
// that hold one agent's state carry its id in their key; the rest (settings,
// push bookkeeping, ref observations, caches) describe the clone and are
// shared by its agents.
var migrations = []string{
	// 1: the initial schema.
	`
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

CREATE TABLE identities (
	worktree TEXT PRIMARY KEY,
	agent    TEXT NOT NULL
) STRICT;

CREATE TABLE pushed (
	ref TEXT PRIMARY KEY,
	oid TEXT NOT NULL
) STRICT;

CREATE TABLE ref_fetches (
	namespace  TEXT PRIMARY KEY,
	last_fetch INTEGER
) STRICT;

CREATE TABLE ref_observations (
	namespace  TEXT NOT NULL,
	ref        TEXT NOT NULL,
	oid        TEXT NOT NULL,
	first_seen INTEGER,
	PRIMARY KEY (namespace, ref)
) STRICT, WITHOUT ROWID;

CREATE TABLE notes (
	agent            TEXT NOT NULL,
	id               TEXT NOT NULL,
	version          TEXT NOT NULL,
	kind             TEXT NOT NULL,
	severity_rank    INTEGER NOT NULL,
	created_at       INTEGER,
	status           TEXT NOT NULL CHECK (status IN ('new', 'open', 'resolved', 'dismissed')),
	resolution       TEXT NOT NULL DEFAULT '',
	dismissed_as     TEXT NOT NULL DEFAULT '',
	dismiss_reason   TEXT NOT NULL DEFAULT '',
	first_seen       INTEGER,
	shown_at         INTEGER,
	closed_at        INTEGER,
	resolution_shown INTEGER NOT NULL DEFAULT 0,
	note             TEXT NOT NULL,
	PRIMARY KEY (agent, id)
) STRICT, WITHOUT ROWID;

CREATE INDEX notes_by_status ON notes (agent, status, severity_rank, created_at);
CREATE INDEX notes_false_alarms ON notes (kind) WHERE status = 'dismissed' AND dismissed_as = 'false_alarm';

CREATE TABLE footprint_cache (
	key       TEXT PRIMARY KEY,
	seq       INTEGER NOT NULL,
	footprint TEXT NOT NULL
) STRICT;

CREATE INDEX footprint_cache_by_seq ON footprint_cache (seq);

CREATE TABLE merge_cache (
	a         TEXT NOT NULL,
	b         TEXT NOT NULL,
	seq       INTEGER NOT NULL,
	clean     INTEGER NOT NULL,
	conflicts TEXT NOT NULL,
	PRIMARY KEY (a, b)
) STRICT;

CREATE INDEX merge_cache_by_seq ON merge_cache (seq);

CREATE TABLE events (
	id     INTEGER PRIMARY KEY,
	at     INTEGER,
	agent  TEXT NOT NULL,
	kind   TEXT NOT NULL,
	detail TEXT NOT NULL
) STRICT;

CREATE INDEX events_by_agent ON events (agent, id);

CREATE TABLE items (
	id         TEXT PRIMARY KEY,
	agent      TEXT NOT NULL,
	updated_at INTEGER,
	item       TEXT NOT NULL
) STRICT;

CREATE INDEX items_by_agent ON items (agent);
`,
}

// schemaVersion is the user_version this binary writes.
var schemaVersion = len(migrations)

// migrate brings the schema to schemaVersion. It reads user_version first,
// so opening a current database takes no write lock (hooks open the ledger
// often). Otherwise it migrates in one immediate transaction, reading the
// version again there, because another process may have migrated in
// between. It returns errNewerSchema when a newer gitlet migrated.
func (l *Ledger) migrate(ctx context.Context) error {
	var v int
	err := l.retryBusy(ctx, func() error {
		return l.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	})
	switch {
	case err != nil:
		return err
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return errNewerSchema
	}
	return l.write(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		if v > schemaVersion {
			return errNewerSchema
		}
		for i := v; i < schemaVersion; i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("migrate to version %d: %w", i+1, err)
			}
		}
		// PRAGMA takes no bound parameters; the value is a constant of this binary.
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	})
}
