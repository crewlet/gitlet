package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/gitlet/internal/model"
)

// Cache rows carry seq, one more than the newest row's when stored, so
// trimming keeps the most recently stored entries. Reads do not refresh
// it: a read never needs the write lock.

func (l *Ledger) cacheFootprint(ctx context.Context, key string, fp model.Footprint) error {
	const op = "cache footprint"
	fp.ComputedAt = utc(fp.ComputedAt)
	b, err := json.Marshal(fp)
	if err != nil {
		return fail(op, fmt.Errorf("encode footprint: %w", err))
	}
	keep := l.keep.footprints
	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO footprint_cache (key, seq, footprint)
VALUES (?, (SELECT coalesce(max(seq), 0) + 1 FROM footprint_cache), ?)
ON CONFLICT (key) DO UPDATE SET seq = excluded.seq, footprint = excluded.footprint`, key, string(b)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
DELETE FROM footprint_cache
WHERE seq <= (SELECT seq FROM footprint_cache ORDER BY seq DESC LIMIT 1 OFFSET ?)`, keep)
		return err
	}))
}

func (l *Ledger) cachedFootprint(ctx context.Context, key string) (model.Footprint, bool, error) {
	const op = "cached footprint"
	var js string
	var ok bool
	err := l.read(ctx, func(tx *sql.Tx) error {
		var err error
		js, ok, err = scanOptional(tx.QueryRowContext(ctx, `SELECT footprint FROM footprint_cache WHERE key = ?`, key))
		return err
	})
	if err != nil || !ok {
		return model.Footprint{}, false, fail(op, err)
	}
	var fp model.Footprint
	if err := json.Unmarshal([]byte(js), &fp); err != nil {
		return model.Footprint{}, false, fail(op, fmt.Errorf("decode footprint %s: %w", key, err))
	}
	return fp, true, nil
}

// mergeKey orders a commit pair, so (a, b) and (b, a) share an entry.
func mergeKey(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

func (l *Ledger) cacheMerge(ctx context.Context, a, b string, m MergeOutcome) error {
	const op = "cache merge"
	conflicts, err := json.Marshal(m.Conflicts)
	if err != nil {
		return fail(op, fmt.Errorf("encode conflicts: %w", err))
	}
	a, b = mergeKey(a, b)
	keep := l.keep.merges
	return fail(op, l.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO merge_cache (a, b, seq, clean, conflicts)
VALUES (?, ?, (SELECT coalesce(max(seq), 0) + 1 FROM merge_cache), ?, ?)
ON CONFLICT (a, b) DO UPDATE SET seq = excluded.seq, clean = excluded.clean, conflicts = excluded.conflicts`,
			a, b, boolInt(m.Clean), string(conflicts)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
DELETE FROM merge_cache
WHERE seq <= (SELECT seq FROM merge_cache ORDER BY seq DESC LIMIT 1 OFFSET ?)`, keep)
		return err
	}))
}

func (l *Ledger) cachedMerge(ctx context.Context, a, b string) (MergeOutcome, bool, error) {
	const op = "cached merge"
	a, b = mergeKey(a, b)
	var clean bool
	var conflicts string
	err := l.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT clean, conflicts FROM merge_cache WHERE a = ? AND b = ?`, a, b).
			Scan(&clean, &conflicts)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return MergeOutcome{}, false, nil
	}
	if err != nil {
		return MergeOutcome{}, false, fail(op, err)
	}
	m := MergeOutcome{Clean: clean}
	if err := json.Unmarshal([]byte(conflicts), &m.Conflicts); err != nil {
		return MergeOutcome{}, false, fail(op, fmt.Errorf("decode conflicts: %w", err))
	}
	return m, true, nil
}
