package tilesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileLock is a best-effort exclusive lock on the tile volume, used so bootstrap
// and the updater never mutate releases concurrently. It is an O_EXCL lockfile
// with a staleness timeout; init containers run to completion before sidecars
// start, so contention is rare, but the lock prevents a racing updater from
// corrupting an activation.
type FileLock struct {
	path string
}

// Lock acquires the volume lock, retrying until timeout. A lock older than
// staleAfter is broken (a previous holder crashed without releasing).
func Lock(root string, timeout, staleAfter time.Duration) (*FileLock, error) {
	path := filepath.Join(root, lockFile)
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), nowRFC3339())
			f.Close()
			return &FileLock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			if time.Since(fi.ModTime()) > staleAfter {
				os.Remove(path) // break a stale lock
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock: timed out after %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Unlock releases the lock.
func (l *FileLock) Unlock() error {
	if l == nil {
		return nil
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
