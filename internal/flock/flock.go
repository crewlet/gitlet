// Package flock provides advisory file locks shared by gitlet processes on
// one machine: the per-agent write lock, the session lock held by a running
// MCP server, and the fetch lock (docs/SPEC.md, sections 3, 5 and 6.2).
// Locks are released when the process exits, even if it crashes.
package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ErrLocked is returned by TryLock when another process holds the lock.
var ErrLocked = errors.New("lock held by another process")

// Lock is a held lock. Unlock releases it.
type Lock struct {
	f *os.File
}

// TryLock takes the exclusive lock at path without waiting. It creates the
// file and its directory when missing.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !ok {
		f.Close()
		return nil, ErrLocked
	}
	return &Lock{f: f}, nil
}

// Acquire waits for the lock at path until ctx is done, polling every 20 ms.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		l, err := TryLock(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// Unlock releases the lock. It is safe to call more than once.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlock(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
