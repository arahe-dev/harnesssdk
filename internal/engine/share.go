package engine

import (
	"context"
	"os"
	"time"
)

// Records (run.json, gate ledgers, budgets, trust) are replaced atomically
// by a rename while other processes read them. On Windows the two can
// collide for an instant: an open fails with a sharing violation while the
// file is being replaced, and a rename fails while a reader has the target
// open. Both are retried briefly; elsewhere they never happen.

// readShared reads a file that another process may be replacing.
func readShared(path string) ([]byte, error) {
	return readSharedCtx(context.Background(), path)
}

// readSharedCtx is readShared that stops retrying when ctx ends.
func readSharedCtx(ctx context.Context, path string) ([]byte, error) {
	for i := 0; ; i++ {
		data, err := os.ReadFile(path)
		if err == nil || !transientShare(err) || i == 100 || !pause(ctx) {
			return data, err
		}
	}
}

// renameShared renames over a file that other processes may be reading,
// retrying until ctx ends.
func renameShared(ctx context.Context, from, to string) error {
	for i := 0; ; i++ {
		err := os.Rename(from, to)
		if err == nil || !transientShare(err) || i == 100 || !pause(ctx) {
			return err
		}
	}
}

// pause waits a moment before a retry, and reports false if ctx ended.
func pause(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(10 * time.Millisecond):
		return true
	}
}
