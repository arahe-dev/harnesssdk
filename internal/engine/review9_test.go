package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the ninth review of v2.0.0-next.

// 1: a rollback keeps a worktree with an initialized submodule, even when
// the repository is configured to ignore submodule changes.
func TestRollbackKeepsSubmodules(t *testing.T) {
	ctx := context.Background()
	h := newGateHome(t)
	repo, lib := gitRepo(t), gitRepo(t)
	allow := []string{"-c", "protocol.file.allow=always"}
	if _, err := git(ctx, append(append([]string{"-C", repo}, allow...), "submodule", "add", "-q", filepath.ToSlash(lib), "lib")...); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, "-C", repo, "commit", "-q", "-m", "lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, "-C", repo, "config", "diff.ignoreSubmodules", "all"); err != nil {
		t.Fatal(err)
	}
	s, err := h.stageRun(ctx, "p", "fake", repo, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if err := h.addWorktree(ctx, s.Run); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, append(append([]string{"-C", s.Worktree}, allow...), "submodule", "update", "-q", "--init")...); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(s.Worktree, "lib", "changed.txt")
	os.WriteFile(work, []byte("work"), 0o644)
	if err := removeWorktree(ctx, s.Run, true); err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("a worktree with a changed submodule was removed: %v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the submodule's change was lost")
	}
}

// 2: a v1 cost moved to the ledger while spending reads, with or without
// its record upgraded in between, counts once.
func TestV1CostMovedDuringAScanCountsOnce(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		h := newGateHome(t)
		ended := time.Now().Add(-2 * time.Hour)
		id := ended.Format("060102-150405") + "-v1v1"
		v1Run(t, h, id, "u", t.TempDir(), ended, 0.6)
		testHookSpending = func() {
			run, err := h.LoadRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if upgrade {
				err = run.Save() // moves the cost, then upgrades the record
			} else {
				err = moveV1Spend(h.Dir, run) // a save that did not finish
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		s, err := h.spending(time.Now().Add(-spendWindow), "u")
		testHookSpending = nil
		if err != nil || s.mine != 0.6 {
			t.Fatalf("upgraded %v: spent %+v %v, want 0.6", upgrade, s, err)
		}
	}
}
