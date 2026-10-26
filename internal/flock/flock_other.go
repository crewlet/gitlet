//go:build !unix

package flock

import "os"

// On platforms without flock, locks are advisory no-ops. gitlet targets
// macOS and Linux; this keeps the package building elsewhere.
func tryLock(f *os.File) (bool, error) { return true, nil }

func unlock(f *os.File) error { return nil }
