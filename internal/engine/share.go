package engine

import (
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
	for i := 0; ; i++ {
		data, err := os.ReadFile(path)
		if err == nil || !transientShare(err) || i == 100 {
			return data, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// renameShared renames over a file that other processes may be reading.
func renameShared(from, to string) error {
	for i := 0; ; i++ {
		err := os.Rename(from, to)
		if err == nil || !transientShare(err) || i == 100 {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
