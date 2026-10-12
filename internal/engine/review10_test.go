package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression tests for the tenth review of v2.0.0-next.

// 1: two v1 runs with the same id, one after the other, keep both costs
// through migration and collection.
func TestV1CostsOfTwoRunsWithOneID(t *testing.T) {
	h := newGateHome(t)
	ended := time.Now().Add(-3 * time.Hour)
	id := ended.Format("060102-150405") + "-v1v1"
	v1Run(t, h, id, "u", t.TempDir(), ended, 0.6)
	if rep, err := h.GC(context.Background(), time.Now().Add(-time.Hour), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	v1Run(t, h, id, "u", t.TempDir(), time.Now().Add(-2*time.Hour), 0.4) // the id again, created later
	if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || math.Abs(s.mine-1) > 1e-9 {
		t.Fatalf("before the second move: spent %+v %v, want $1", s, err)
	}
	run, err := h.LoadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Save(); err != nil { // moves the second run's cost
		t.Fatal(err)
	}
	if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || math.Abs(s.mine-1) > 1e-9 {
		t.Fatalf("after: spent %+v %v, want $1", s, err)
	}
}

// 2: the search for repositories in a worktree stops when its caller
// cancels.
func TestRepositorySearchIsCancellable(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "b", "f"), nil, 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := holdsRepository(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	os.MkdirAll(filepath.Join(root, "a", ".git"), 0o755)
	if found, err := holdsRepository(context.Background(), root); err != nil || found == "" {
		t.Fatalf("%q %v", found, err)
	}
}
