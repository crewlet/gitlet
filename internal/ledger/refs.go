package ledger

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Ref observations are keyed by the namespace string the caller passes, as
// is: a refresh of one namespace never touches the refs recorded under
// another, even when their names overlap.

type observedRef struct {
	oid       string
	firstSeen sql.NullInt64
}

func (l *Ledger) observeRefs(ctx context.Context, namespace string, current map[string]string, fetchedAt time.Time) (Observation, error) {
	at := nanos(fetchedAt)
	var obs Observation
	err := l.write(ctx, func(tx *sql.Tx) error {
		stored, err := loadObserved(ctx, tx, namespace)
		if err != nil {
			return err
		}
		upsert, err := tx.PrepareContext(ctx, `
INSERT INTO ref_observations (namespace, ref, oid, first_seen) VALUES (?, ?, ?, ?)
ON CONFLICT (namespace, ref) DO UPDATE SET oid = excluded.oid, first_seen = excluded.first_seen`)
		if err != nil {
			return err
		}
		defer upsert.Close()
		forget, err := tx.PrepareContext(ctx, `DELETE FROM ref_observations WHERE namespace = ? AND ref = ?`)
		if err != nil {
			return err
		}
		defer forget.Close()

		firstSeen := make(map[string]time.Time, len(current))
		oids := make(map[string]string, len(current))
		for ref, oid := range current {
			oids[ref] = oid
			if old, ok := stored[ref]; ok && old.oid == oid {
				firstSeen[ref] = fromNanos(old.firstSeen)
				continue
			}
			if _, err := upsert.ExecContext(ctx, namespace, ref, oid, at); err != nil {
				return err
			}
			firstSeen[ref] = fromNanos(at)
		}
		for ref := range stored {
			if _, ok := current[ref]; ok {
				continue
			}
			if _, err := forget.ExecContext(ctx, namespace, ref); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ref_fetches (namespace, last_fetch) VALUES (?, ?)
ON CONFLICT (namespace) DO UPDATE SET last_fetch = excluded.last_fetch`, namespace, at); err != nil {
			return err
		}
		obs = Observation{FirstSeen: firstSeen, OIDs: oids, LastFetch: fromNanos(at)}
		return nil
	})
	if err != nil {
		return Observation{}, fail("observe refs "+namespace, err)
	}
	return obs, nil
}

func (l *Ledger) observed(ctx context.Context, namespace string) (Observation, error) {
	var obs Observation
	err := l.read(ctx, func(tx *sql.Tx) error {
		stored, err := loadObserved(ctx, tx, namespace)
		if err != nil {
			return err
		}
		var last sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT last_fetch FROM ref_fetches WHERE namespace = ?`, namespace).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		firstSeen := make(map[string]time.Time, len(stored))
		oids := make(map[string]string, len(stored))
		for ref, o := range stored {
			firstSeen[ref], oids[ref] = fromNanos(o.firstSeen), o.oid
		}
		obs = Observation{FirstSeen: firstSeen, OIDs: oids, LastFetch: fromNanos(last)}
		return nil
	})
	if err != nil {
		return Observation{}, fail("observed "+namespace, err)
	}
	return obs, nil
}

func (l *Ledger) resetFirstSeen(ctx context.Context, now time.Time) error {
	return fail("reset first seen", l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE ref_observations SET first_seen = ?`, nanos(now))
		return err
	}))
}

func loadObserved(ctx context.Context, tx *sql.Tx, namespace string) (map[string]observedRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ref, oid, first_seen FROM ref_observations WHERE namespace = ?`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stored := make(map[string]observedRef)
	for rows.Next() {
		var ref string
		var o observedRef
		if err := rows.Scan(&ref, &o.oid, &o.firstSeen); err != nil {
			return nil, err
		}
		stored[ref] = o
	}
	return stored, rows.Err()
}
