package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// oldest first, and the runs whose creation was abandoned. A run that
// cannot be removed now is left for the next pass. Cancelling ctx stops
// it between runs, and stops git; what it was removing then is kept.
func (h *Home) GC(ctx context.Context, cutoff time.Time, dryRun bool) (GCReport, error) {
	var rep GCReport
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return rep, err
	}
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		id := e.Name()
		if strings.HasPrefix(id, stagePrefix) {
			if !dryRun {
				h.collectStage(ctx, id)
			}
			continue
		}
		if !e.IsDir() {
			continue
		}
		// A run still marked as being created is left to its creator; once
		// the creator is gone (it can then no longer remove the mark), it
		// was abandoned, and goes whatever its age, as its rollback would.
		marker := filepath.Join(h.Dir, "runs", id, creatingFile)
		abandoned := false
		if _, err := os.Stat(marker); err == nil {
			if !h.creatorGone(id) {
				continue
			}
			_, err := os.Stat(marker)
			abandoned = err == nil
		}
		// Ids start with their creation time: a run clearly created after
		// the cutoff was active after it, and is not read at all.
		if created, ok := idTime(id); !abandoned && (!ok || !created.Add(-idSlack).Before(cutoff)) {
			continue
		}
		size, err := h.collect(ctx, id, cutoff, dryRun, abandoned)
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
// or was abandoned, and returns its size, or -1 if it is kept. It holds the
// run's execution and state locks while it decides and removes, so the run
// cannot be sent a turn or executed meanwhile. An abandoned run is removed
// as its creation's rollback would have: its branch too.
func (h *Home) collect(ctx context.Context, id string, cutoff time.Time, dryRun, abandoned bool) (int64, error) {
	dir := filepath.Join(h.Dir, "runs", id)
	// Runs appear with their record (NewRun renames them into place), so
	// a folder without one is what an interrupted removal leaves: removed
	// once untouched since the cutoff. Nothing executes without a record,
	// so it takes no locks, which would also make the folder look new.
	if _, err := os.Stat(filepath.Join(dir, "run.json")); errors.Is(err, fs.ErrNotExist) {
		fi, err := os.Stat(dir)
		if err != nil || !abandoned && !fi.ModTime().Before(cutoff) {
			return -1, nil
		}
		size, _, err := dirSize(dir)
		if err != nil || dryRun {
			return size, err
		}
		return size, os.RemoveAll(dir)
	}
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
	// Under the locks, the record decides; one this harness cannot read is
	// left alone, and so is a run still taking shape.
	r, err := h.LoadRun(id)
	if err != nil || !abandoned && (!r.lastActive().Before(cutoff) || r.settling()) {
		release()
		return -1, nil
	}
	size, entries, err := dirSize(dir)
	if err != nil || dryRun {
		release()
		return size, err
	}
	// The worktree goes first, its work committed to its branch, which
	// stays; a worktree whose work cannot be kept keeps its run too.
	if err := removeWorktree(ctx, r, abandoned); err != nil {
		release()
		return -1, fmt.Errorf("run %s: %w", id, err)
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

// collectStage removes a run being prepared whose creator is gone: its
// lock is free. A live creator holds the lock from before it makes the
// folder until it has published or removed it. The lock file is removed
// under the creation lock (see stageRun).
func (h *Home) collectStage(ctx context.Context, name string) {
	stage := filepath.Join(h.Dir, "runs", strings.TrimSuffix(name, ".lock"))
	lockPath := stage + ".lock"
	coord, err := h.creationLock()
	if err != nil {
		return
	}
	lk, err := lockOpen(lockPath)
	coord()
	if err != nil {
		return // its creator is alive
	}
	// Its creator is gone, and this lock keeps any other from the id while
	// what it left is undone: a worktree and branch it made, then the folder.
	release := func(removeLock bool) {
		coord, err := h.creationLock()
		lk.Close()
		if err == nil {
			if removeLock {
				os.Remove(lockPath)
			}
			coord()
		}
	}
	if run := readStaged(stage); run != nil && run.Worktree != "" {
		if err := removeWorktree(ctx, run, true); err != nil {
			release(false) // tried again by the next pass
			return
		}
	}
	os.RemoveAll(stage)
	release(true)
}

// creatorGone reports whether the creator of published run id has let go
// of its creation lock, which it holds until the run is committed or
// rolled back, and then removes the lock file.
func (h *Home) creatorGone(id string) bool {
	coord, err := h.creationLock()
	if err != nil {
		return false
	}
	defer coord()
	path := filepath.Join(h.Dir, "runs", stagePrefix+id+".lock")
	lk, err := lockOpen(path)
	if err != nil {
		return false // held, or cannot tell
	}
	lk.Close()
	os.Remove(path)
	return true
}

// readStaged reads the record of a run being prepared, or nil.
func readStaged(stage string) *Run {
	data, err := os.ReadFile(filepath.Join(stage, "run.json"))
	if err != nil {
		return nil
	}
	var r Run
	if json.Unmarshal(data, &r) != nil || checkFormatUpTo("staged run", r.Format, runFormat) != nil {
		return nil
	}
	r.dir = stage
	return &r
}

// dirSize adds up the sizes of a run folder's files.
func dirSize(dir string) (int64, []os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1, nil, err
	}
	var size int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			size += info.Size()
		}
	}
	return size, entries, nil
}

// settling reports whether a run may still be taking shape without
// holding a lock: just created with no turn yet, or with a turn queued for
// a worker that may still start. Its age alone does not make it debris.
func (r *Run) settling() bool {
	if len(r.Turns) == 0 {
		return time.Since(r.Created) < lostAfter
	}
	last := r.Turns[len(r.Turns)-1]
	return last.Outcome == "queued" && time.Since(last.Queued) < lostAfter
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
func (h *Home) AutoGC(ctx context.Context) {
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
	h.GC(ctx, time.Now().AddDate(0, 0, -cfg.RetentionDays), false)
}
