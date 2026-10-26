package ledger

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/gitlet/internal/model"
)

// Environment of a child process started by TestMultiProcessWriters.
const (
	childPathEnv = "GITLET_LEDGER_TEST_CHILD_PATH"
	childIDEnv   = "GITLET_LEDGER_TEST_CHILD_ID"
	childRounds  = 25
)

// TestMultiProcessWriters starts real processes (this test binary again)
// that create, migrate and write one new ledger at the same time. SQLite
// locks between processes differently than between connections of one
// process, so this complements TestConcurrentWriters.
func TestMultiProcessWriters(t *testing.T) {
	if os.Getenv(childPathEnv) != "" {
		t.Skip("running as a child")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no test executable:", err)
	}
	path := filepath.Join(t.TempDir(), "gitlet", "ledger.db")
	const children = 4
	var wg sync.WaitGroup
	for i := range children {
		wg.Go(func() {
			cmd := exec.Command(exe, "-test.run=^TestLedgerChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), childPathEnv+"="+path, childIDEnv+"="+strconv.Itoa(i))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child %d: %v\n%s", i, err, out)
			}
		})
	}
	wg.Wait()

	l := openTest(t, path)
	events, err := l.Events(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != children*childRounds {
		t.Fatalf("%d events, want %d", len(events), children*childRounds)
	}
}

// TestLedgerChild is the work of one child process of
// TestMultiProcessWriters; run directly, it does nothing.
func TestLedgerChild(t *testing.T) {
	path := os.Getenv(childPathEnv)
	if path == "" {
		t.Skip("only runs as a child of TestMultiProcessWriters")
	}
	id := os.Getenv(childIDEnv)
	ctx := context.Background()
	l, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	agent := model.AgentID("agent-" + id)
	now := t0
	for r := range childRounds {
		now = now.Add(time.Second)
		if err := l.Set(ctx, "child-"+id, strconv.Itoa(r)); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
		if err := l.SyncNotes(ctx, agent, []model.Note{testNote(fmt.Sprint(r%4), fmt.Sprint(r/4))}, now); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
		if _, _, err := l.TakeUpdates(ctx, agent, now); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
		if _, err := l.ObserveRefs(ctx, "refs/gitlet/", map[string]string{"r": strconv.Itoa(r)}, now); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
		if err := l.AppendEvent(ctx, Event{At: now, Agent: agent, Kind: "tick"}); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}
}
