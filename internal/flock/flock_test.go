package flock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "a.lock")
	l1, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: got %v, want ErrLocked", err)
	}
	if err := l1.Unlock(); err != nil {
		t.Fatal(err)
	}
	l2, err := TryLock(path)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	l2.Unlock()
	l2.Unlock() // idempotent
}

func TestAcquireWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.lock")
	l1, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		l1.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l2, err := Acquire(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	l2.Unlock()
}

func TestAcquireTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.lock")
	l1, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}
