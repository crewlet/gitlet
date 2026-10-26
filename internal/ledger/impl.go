package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	defaultBusyTimeout = 10 * time.Second

	// maxConns bounds the pool. Writers are serialized anyway (writeSem
	// within a Ledger, BEGIN IMMEDIATE between Ledgers and processes); the
	// extra connections let readers run beside a writer, which WAL allows.
	maxConns = 4

	footprintCacheSize = 500
	mergeCacheSize     = 2000
	eventLogSize       = 5000
)

// impl is the state behind a Ledger.
type impl struct {
	db       *sql.DB
	ro       bool
	busy     time.Duration
	writeSem chan struct{} // one writer of this Ledger at a time
	keep     limits
}

// limits are the sizes of the trimmed tables; tests lower them.
type limits struct {
	footprints, merges, events int
}

// errNewerSchema means the database was migrated by a newer gitlet.
var errNewerSchema = errors.New("schema is newer than this gitlet")

func open(ctx context.Context, path string, opt Options) (*Ledger, error) {
	if path == "" {
		return nil, errors.New("ledger: open: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", abs, err)
	}
	busy := opt.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	l := &Ledger{impl{
		busy:     busy,
		writeSem: make(chan struct{}, 1),
		keep:     limits{footprints: footprintCacheSize, merges: mergeCacheSize, events: eventLogSize},
	}}

	if l.db, err = openDB(abs, busy, false); err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", abs, err)
	}
	err = l.migrate(ctx)
	if errors.Is(err, errNewerSchema) {
		// A newer gitlet owns the schema: read what we can, write nothing.
		l.db.Close()
		l.ro = true
		if l.db, err = openDB(abs, busy, true); err != nil {
			return nil, fmt.Errorf("ledger: open %s: %w", abs, err)
		}
		err = l.retryBusy(ctx, func() error { return l.db.PingContext(ctx) })
	}
	if err != nil {
		l.db.Close()
		return nil, fmt.Errorf("ledger: open %s: %w", abs, err)
	}
	return l, nil
}

// openDB opens a pool on the database at the absolute path abs.
func openDB(abs string, busy time.Duration, readOnly bool) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(abs, busy, readOnly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	return db, nil
}

// dsn builds the driver's data source name: a SQLite URI whose path is
// percent-encoded (so "?", "#" and "%" in directory names are safe), with the
// connection pragmas of docs/SPEC.md, section 15. Read-only databases are
// opened with mode=ro and keep their journal mode.
func dsn(abs string, busy time.Duration, readOnly bool) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive path becomes file:///C:/...
	}
	ms := busy.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", ms))
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	return u.String()
}

func (l *Ledger) close() error {
	if err := l.db.Close(); err != nil {
		return fmt.Errorf("ledger: close: %w", err)
	}
	return nil
}

func (l *Ledger) readOnly() bool { return l.ro }

// write runs fn in an immediate transaction and commits it. Writers of this
// Ledger queue on writeSem, so they hand over in turn instead of polling
// SQLite's lock; other Ledgers and processes wait under busy_timeout. fn
// may run more than once (see retryBusy): it must only use tx, and assign
// rather than append to variables outside it.
func (l *Ledger) write(ctx context.Context, fn func(*sql.Tx) error) error {
	if l.ro {
		return ErrReadOnly
	}
	select {
	case l.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.writeSem }()
	return l.retryBusy(ctx, func() error {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

// read runs fn in a deferred read-only transaction, so every query in it
// sees one snapshot. fn may run more than once, as for write.
func (l *Ledger) read(ctx context.Context, fn func(*sql.Tx) error) error {
	return l.retryBusy(ctx, func() error {
		tx, err := l.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return fn(tx)
	})
}

// retryBusy runs op again while it fails with SQLITE_BUSY before the busy
// timeout has passed. busy_timeout makes SQLite wait for a lock by itself,
// but SQLite returns SQLITE_BUSY at once where waiting could deadlock: two
// connections racing to create the database or switch it to WAL, a stale
// WAL snapshot, recovery after a crash. Only starting over helps there. A
// SQLITE_BUSY that arrives after the timeout means the lock really was held
// that long, and is returned like any other error.
func (l *Ledger) retryBusy(ctx context.Context, op func() error) error {
	deadline := time.Now().Add(l.busy)
	backoff := time.Millisecond
	for {
		err := op()
		if err == nil || !isBusy(err) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return err
		}
		t := time.NewTimer(backoff/2 + rand.N(backoff))
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(2*backoff, 50*time.Millisecond)
	}
}

// isBusy reports whether err is SQLite's SQLITE_BUSY or SQLITE_LOCKED, in any
// of their extended forms.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// fail adds "ledger: <op>: " to err. ErrReadOnly already names the ledger
// and is returned as is.
func fail(op string, err error) error {
	if err == nil || errors.Is(err, ErrReadOnly) {
		return err
	}
	return fmt.Errorf("ledger: %s: %w", op, err)
}

// errEmptyAgent rejects writes that would store rows under no agent.
var errEmptyAgent = errors.New("empty agent id")

// ---- encoding helpers ----

// Times are stored as INTEGER unix nanoseconds in UTC; the zero time is NULL.

func nanos(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

func fromNanos(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(0, n.Int64).UTC()
}

func fromNanosPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n)
	return &t
}

// utc returns t in UTC without its monotonic reading, as it reads back from
// the database.
func utc(t time.Time) time.Time { return fromNanos(nanos(t)) }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := utc(*t)
	return &u
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
