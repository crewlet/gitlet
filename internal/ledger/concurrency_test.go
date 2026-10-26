package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// TestConcurrentOpen races several "processes" to create and migrate one
// new database.
func TestConcurrentOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitlet", "ledger.db")
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	ledgers := make(chan *Ledger, n)
	for range n {
		wg.Go(func() {
			l, err := Open(ctx, path, Options{})
			if err != nil {
				errs <- err
				return
			}
			ledgers <- l
		})
	}
	wg.Wait()
	close(errs)
	close(ledgers)
	for err := range errs {
		t.Error(err)
	}
	for l := range ledgers {
		if l.ReadOnly() {
			t.Error("a racing open ended read-only")
		}
		l.Close()
	}
}

// TestConcurrentWriters runs two ledgers on one file, as two processes
// would, each with many goroutines writing at once. None may fail with
// SQLITE_BUSY.
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	procs := []*Ledger{openTest(t, path), openTest(t, path)}
	const workers, rounds = 6, 15

	var wg sync.WaitGroup
	errs := make(chan error, len(procs)*workers)
	for p, l := range procs {
		for w := range workers {
			wg.Go(func() {
				agent := model.AgentID(fmt.Sprintf("agent-%d-%d", p, w))
				now := t0
				for r := range rounds {
					now = now.Add(time.Second)
					notes := []model.Note{testNote(fmt.Sprintf("n%d", r%3), fmt.Sprint(r/3))}
					ops := []func() error{
						func() error { return l.Set(ctx, fmt.Sprintf("k%d", w), fmt.Sprint(r)) },
						func() error { return l.SyncNotes(ctx, agent, notes, now) },
						func() error { _, _, err := l.TakeUpdates(ctx, agent, now); return err },
						func() error { return l.AppendEvent(ctx, Event{At: now, Agent: agent, Kind: "tick"}) },
						func() error {
							_, err := l.ObserveRefs(ctx, "refs/gitlet/", map[string]string{"r": fmt.Sprint(r)}, now)
							return err
						},
						func() error { return l.CacheMerge(ctx, fmt.Sprint(p), fmt.Sprint(r), MergeOutcome{Clean: true}) },
						func() error { _, _, err := l.Counts(ctx, agent); return err },
					}
					for i, op := range ops {
						if err := op(); err != nil {
							errs <- fmt.Errorf("process %d worker %d round %d op %d: %w", p, w, r, i, err)
							return
						}
					}
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	events, err := procs[0].Events(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(procs) * workers * rounds; len(events) != want {
		t.Fatalf("%d events, want %d", len(events), want)
	}
}

// TestTakeUpdatesExclusive checks that two processes taking updates at once
// never deliver the same note twice.
func TestTakeUpdatesExclusive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	procs := []*Ledger{openTest(t, path), openTest(t, path)}
	const agent, total = "agent-a", 40
	var notes []model.Note
	for i := range total {
		notes = append(notes, testNote(fmt.Sprintf("n%d", i), "1"))
	}
	if err := procs[0].SyncNotes(ctx, agent, notes, t0); err != nil {
		t.Fatal(err)
	}
	// Show half, then resolve them, so resolved lines are owed too.
	if err := procs[0].MarkShown(ctx, agent, ids(notes[:total/2]), t0); err != nil {
		t.Fatal(err)
	}
	if err := procs[1].SyncNotes(ctx, agent, notes[total/2:], t0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	fresh, resolved := map[string]int{}, map[string]int{}
	var wg sync.WaitGroup
	for _, l := range procs {
		for range 4 {
			wg.Go(func() {
				f, r, err := l.TakeUpdates(ctx, agent, t0)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, n := range f {
					fresh[n.ID]++
				}
				for _, n := range r {
					resolved[n.ID]++
				}
			})
		}
	}
	wg.Wait()
	if len(fresh) != total/2 || len(resolved) != total/2 {
		t.Fatalf("delivered %d fresh and %d resolved, want %d each", len(fresh), len(resolved), total/2)
	}
	for id, n := range fresh {
		if n != 1 {
			t.Errorf("fresh note %s delivered %d times", id, n)
		}
	}
	for id, n := range resolved {
		if n != 1 {
			t.Errorf("resolved note %s delivered %d times", id, n)
		}
	}
}

func ids(notes []model.Note) []string {
	var s []string
	for _, n := range notes {
		s = append(s, n.ID)
	}
	return s
}

// TestBusyTimeoutNotHidden holds the write lock from outside the ledger:
// writes wait for the busy timeout and then fail with SQLITE_BUSY instead
// of retrying forever, reads keep working, and writes succeed once the
// lock is released.
func TestBusyTimeoutNotHidden(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	openTest(t, path) // create the schema
	l, err := Open(ctx, path, Options{BusyTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Set(ctx, "k", "before"); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dsn(path, time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = l.Set(ctx, "k", "during")
	elapsed := time.Since(start)
	if !isBusy(err) {
		t.Fatalf("Set under a held lock: err = %v, want SQLITE_BUSY", err)
	}
	if elapsed < 100*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("Set gave up after %v, want about the busy timeout", elapsed)
	}
	if v, _, err := l.Get(ctx, "k"); err != nil || v != "before" {
		t.Fatalf("Get under a held write lock = %q, %v", v, err)
	}

	// A cancelled wait ends early with the context's error.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	slow, err := Open(ctx, path, Options{BusyTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	start = time.Now()
	if err := slow.Set(cctx, "k", "cancelled"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Set with an expiring context: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancelled Set returned after %v", elapsed)
	}

	if _, err := holder.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set(ctx, "k", "after"); err != nil {
		t.Fatalf("Set after the lock was released: %v", err)
	}
}

// busyError returns a real SQLITE_BUSY error from the driver.
func busyError(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	openTest(t, path)
	holder, err := rawDB(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, "ROLLBACK")
	impatient, err := sql.Open("sqlite", dsn(path, time.Millisecond, false))
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()
	_, err = impatient.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('k', 'v')`)
	if !isBusy(err) {
		t.Fatalf("want SQLITE_BUSY, got %v", err)
	}
	return err
}

func TestRetryBusy(t *testing.T) {
	busy := busyError(t)
	other := errors.New("constraint failed")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name      string
		ctx       context.Context
		timeout   time.Duration
		errs      []error // returned by successive calls; the last one repeats
		want      error
		wantCalls int // 0: more than one
	}{
		{"success", context.Background(), time.Second, []error{nil}, nil, 1},
		{"quick busy is retried", context.Background(), time.Second, []error{busy, busy, nil}, nil, 3},
		{"other errors are not retried", context.Background(), time.Second, []error{other, nil}, other, 1},
		{"busy past the timeout is returned", context.Background(), 30 * time.Millisecond, []error{busy}, busy, 0},
		{"cancelled context stops retrying", cancelled, time.Second, []error{busy, nil}, busy, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &Ledger{impl{busy: tt.timeout}}
			calls := 0
			err := l.retryBusy(tt.ctx, func() error {
				err := tt.errs[min(calls, len(tt.errs)-1)]
				calls++
				return err
			})
			if err != tt.want {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if (tt.wantCalls > 0 && calls != tt.wantCalls) || (tt.wantCalls == 0 && calls < 2) {
				t.Fatalf("%d calls, want %d", calls, tt.wantCalls)
			}
		})
	}
}
