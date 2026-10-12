package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression tests for the sixth review of v2.0.0-next.

// 1: a mark that cannot be read, or one that says a run that ran was never
// committed, never makes gc roll a run back: its worktree and work stay.
func TestUnreadableMarkLeavesRunAlone(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, _ := worktreeRun(t, f, "writer", "")
	work := filepath.Join(run.Workdir, "work.txt")
	os.WriteFile(work, []byte("not committed yet"), 0o644)
	mark := filepath.Join(run.Dir(), creatingFile)
	os.Mkdir(mark, 0o700) // cannot be read as a file
	if rep, err := f.home.GC(context.Background(), time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 0 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the work was removed")
	}
	if rep := f.r.Execute(context.Background(), run.ID, 1); rep.Err == nil {
		t.Fatal("a run whose creation cannot be told executed")
	}
	os.Remove(mark)
	os.WriteFile(mark, []byte("nevercommitted\n"), 0o600) // a run that ran was committed, whatever this says
	if rep, err := f.home.GC(context.Background(), time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 0 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the work was removed")
	}
}

// 1: a rollback keeps a worktree that holds changes.
func TestRollbackKeepsChangedWorktree(t *testing.T) {
	h := newGateHome(t)
	repo := gitRepo(t)
	s, err := h.stageRun(context.Background(), "p", "fake", repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.addWorktree(context.Background(), s.Run); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.Worktree, "someone.txt"), []byte("work"), 0o644)
	if err := removeWorktree(context.Background(), s.Run, true); err == nil {
		t.Fatal("a worktree with changes was removed")
	}
	if _, err := os.Stat(filepath.Join(s.Worktree, "someone.txt")); err != nil {
		t.Fatal("the change was lost")
	}
	s.release()
}

// 2: commit files are pruned only when they are an hour older than the
// scan that found no mark naming them, however long the scan took; and
// not at all after a scan that could not read every mark.
func TestCommitFilesOutliveLongScans(t *testing.T) {
	h := newGateHome(t)
	write := func(tx string, age time.Duration) string {
		if err := h.commit(tx); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		os.Chtimes(h.commitPath(tx), at, at)
		return h.commitPath(tx)
	}
	recent := write("recent", 90*time.Minute) // committed during a scan that started two hours ago
	named := write("named", 5*time.Hour)
	h.pruneCommits(map[string]bool{"named": true}, time.Now().Add(-2*time.Hour))
	for _, p := range []string{recent, named} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was pruned", filepath.Base(p))
		}
	}
	h.pruneCommits(map[string]bool{"named": true}, time.Now())
	if _, err := os.Stat(recent); err == nil {
		t.Fatal("a leftover commit file was kept")
	}

	// A scan that cannot read a mark prunes nothing.
	old := write("old", 5*time.Hour)
	dir := filepath.Join(h.Dir, "runs", "261012-000000-eeee")
	os.MkdirAll(filepath.Join(dir, creatingFile), 0o700)
	h.GC(context.Background(), time.Now().AddDate(0, 0, -30), false)
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a scan that could not read a mark pruned a commit file")
	}
}
