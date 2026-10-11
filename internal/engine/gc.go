package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A run's folder keeps its prompts, answers and raw output, so finished runs
// are removed once they are older than config retention_days: by harness gc,
// and by harness itself at most once a day as runs are started.

// GCReport is what GC removed or, on a dry run, would remove.
type GCReport struct {
	Runs  []string
	Bytes int64
}

// GC removes the runs last active before cutoff that no process executes,
// oldest first. A run that cannot be removed now is left for the next pass.
func (h *Home) GC(cutoff time.Time, dryRun bool) (GCReport, error) {
	var rep GCReport
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return rep, err
	}
	var errs []error
	for _, e := range entries {
		id := e.Name()
		// Ids start with their creation time: a run created after the
		// cutoff was active after it, and is not read at all.
		if created, err := time.ParseInLocation("060102-150405", id[:min(len(id), 13)], time.Local); !e.IsDir() || err != nil || !created.Before(cutoff) {
			continue
		}
		size, err := h.collect(id, cutoff, dryRun)
		switch {
		case err != nil:
			errs = append(errs, err)
		case size >= 0:
			rep.Runs = append(rep.Runs, id)
			rep.Bytes += size
		}
	}
	sort.Strings(rep.Runs)
	h.pruneLive(cutoff)
	h.pruneRepeats()
	return rep, errors.Join(errs...)
}

// collect removes one run if it is idle and was last active before cutoff,
// and returns its size, or -1 if it is kept. It holds the run's execution
// and state locks while it decides and removes, so the run cannot be sent a
// turn or executed meanwhile.
func (h *Home) collect(id string, cutoff time.Time, dryRun bool) (int64, error) {
	dir := filepath.Join(h.Dir, "runs", id)
	exec, err := lockOpen(filepath.Join(dir, "exec.lock"))
	if err != nil {
		return -1, nil // executing
	}
	state, err := lockOpen(filepath.Join(dir, "state.lock"))
	if err != nil {
		exec.Close()
		return -1, nil // being changed
	}
	locks := []string{"exec.lock", "state.lock"}
	release := func() { state.Close(); exec.Close() }
	// A folder without a record is what an interrupted removal leaves; a
	// record this harness cannot read is left alone.
	if _, err := os.Stat(filepath.Join(dir, "run.json")); err == nil {
		if r, err := h.LoadRun(id); err != nil || !r.lastActive().Before(cutoff) {
			release()
			return -1, nil
		}
	}
	var size int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		release()
		return -1, err
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			size += info.Size()
		}
	}
	if dryRun {
		release()
		return size, nil
	}
	// The record goes first: from then on the run does not exist. The lock
	// files, open here, go last (Windows cannot delete an open file).
	var errs []error
	if err := os.Remove(filepath.Join(dir, "run.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		release()
		return -1, err
	}
	for _, e := range entries {
		if n := e.Name(); n != "run.json" && n != locks[0] && n != locks[1] {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, n)))
		}
	}
	release()
	errs = append(errs, os.RemoveAll(dir))
	return size, errors.Join(errs...)
}

// lastActive is when the run was created or a turn last queued, started or
// ended.
func (r *Run) lastActive() time.Time {
	last := r.Created
	for _, t := range r.Turns {
		for _, at := range []time.Time{t.Queued, t.Started, t.Ended} {
			if at.After(last) {
				last = at
			}
		}
	}
	return last
}

// pruneLive removes gate liveness files left by dead processes. Readers of
// a ledger remove the files of the tickets they find; a ticket that never
// reached a ledger leaves its file behind. Only files older than cutoff are
// touched, so a ticket being created is never mistaken for a dead one.
func (h *Home) pruneLive(cutoff time.Time) {
	dir := filepath.Join(h.Dir, "gate", "live")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	g := h.Gate(nil)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".live")
		if info, err := e.Info(); ok && err == nil && info.ModTime().Before(cutoff) {
			g.alive(id) // removes the file of a dead ticket
		}
	}
}

// AutoGC applies config retention_days, at most once a day across every
// harness process. It is best effort: errors are left for harness gc.
func (h *Home) AutoGC() {
	cfg, err := h.Config()
	if err != nil || cfg.RetentionDays == 0 {
		return
	}
	stamp := filepath.Join(h.Dir, "gc.stamp")
	fresh := func() bool {
		fi, err := os.Stat(stamp)
		return err == nil && time.Since(fi.ModTime()) < 24*time.Hour
	}
	if fresh() {
		return
	}
	unlock, err := lockFile(filepath.Join(h.Dir, "gc.lock"))
	if err != nil {
		return
	}
	defer unlock()
	if fresh() {
		return
	}
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return
	}
	h.GC(time.Now().AddDate(0, 0, -cfg.RetentionDays), false)
}
