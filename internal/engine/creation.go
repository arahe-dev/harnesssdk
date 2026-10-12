package engine

import (
	"errors"
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
// mark still names is kept.

// creatingFile, in a run's folder, names the creation that made the run.
const creatingFile = "creating"

func (h *Home) commitPath(tx string) string { return filepath.Join(h.Dir, "created", tx) }

// readMark reads the creation that marked the run in dir. A mark that
// cannot be read names no creation, so it never counts as committed.
func readMark(dir string) (tx string, marked bool) {
	data, err := os.ReadFile(filepath.Join(dir, creatingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// committed reports whether creation tx committed.
func (h *Home) committed(tx string) bool {
	if !validName(tx) {
		return false
	}
	_, err := os.Stat(h.commitPath(tx))
	return err == nil
}

// creating reports whether the run in dir was marked by a creation that
// has not committed.
func (h *Home) creating(dir string) bool {
	tx, marked := readMark(dir)
	return marked && !h.committed(tx)
}

// commit records that creation tx committed.
func (h *Home) commit(tx string) error {
	if err := os.MkdirAll(filepath.Join(h.Dir, "created"), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(h.commitPath(tx), nil)
}

// pruneCommits removes the commit files that no mark names (refs), once
// they are an hour old: a creation removes its marks and then its commit
// file within moments, so an older one is a leftover.
func (h *Home) pruneCommits(refs map[string]bool) {
	dir := filepath.Join(h.Dir, "created")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !refs[e.Name()] && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
