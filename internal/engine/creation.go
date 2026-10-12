package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A creation (one run, or several: start's tasks, compare's profiles) is
// all or nothing. Each run is staged with a mark (creatingFile) naming the
// creation, and published marked. Once every run is published, the
// creation commits by making one file, <home>/created/<creation>: a single
// decision, which a crash cannot leave half made. A marked run executes
// only if its creation committed, and gc rolls back one whose creation did
// not once its creator is gone. After the commit the marks, and then the
// commit file, are only clutter and are removed; a commit file that a
// mark still names is kept. Whatever cannot be read leaves the run alone:
// only a creation known not to have committed is rolled back.

// creatingFile, in a run's folder, names the creation that made the run.
const creatingFile = "creating"

func (h *Home) commitPath(tx string) string { return filepath.Join(h.Dir, "created", tx) }

// readMark reads the creation that marked the run in dir: whether it is
// marked, and by which creation, or why that cannot be told.
func readMark(dir string) (tx string, marked bool, err error) {
	data, err := readShared(filepath.Join(dir, creatingFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", true, err
	}
	if tx = strings.TrimSpace(string(data)); !validName(tx) {
		return "", true, fmt.Errorf("the creation mark in %s names no creation", dir)
	}
	return tx, true, nil
}

// committed reports whether creation tx committed, or why that cannot be
// told.
func (h *Home) committed(tx string) (bool, error) {
	_, err := os.Stat(h.commitPath(tx))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// creating reports whether the run in dir was marked by a creation that
// has not committed, or why that cannot be told.
func (h *Home) creating(dir string) (bool, error) {
	tx, marked, err := readMark(dir)
	if !marked || err != nil {
		return marked, err
	}
	c, err := h.committed(tx)
	return !c, err
}

// commit records that creation tx committed.
func (h *Home) commit(tx string) error {
	if err := os.MkdirAll(filepath.Join(h.Dir, "created"), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(h.commitPath(tx), nil)
}

// pruneCommits removes the commit files that no mark names (refs) of a
// gc scan that started at scanned and read every mark. Only files from an
// hour before the scan go: a creation publishes its runs, marked, before it
// commits, so the runs of any older commit were there for the scan to read,
// however long it took.
func (h *Home) pruneCommits(refs map[string]bool, scanned time.Time) {
	dir := filepath.Join(h.Dir, "created")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !refs[e.Name()] && info.ModTime().Before(scanned.Add(-time.Hour)) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
