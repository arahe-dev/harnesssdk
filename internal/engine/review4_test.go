package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the fourth review of v2.0.0-next.

// 2: a run is marked as being created before it is published, so one whose
// mark cannot be written is never published, and gc leaves runs alone
// while their creator is at work.
func TestRunsBeingCreatedAreNotTouched(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("w1", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	f.profile("w2", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	testHookCreate = func(stage string) error {
		if stage == "mark" {
			return errors.New("disk full")
		}
		return nil
	}
	defer func() { testHookCreate = nil }()
	if _, err := f.r.Start(context.Background(), "w1", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("a run whose mark failed was created")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
	}

	// gc during a compare's creation, with its first run published.
	var queued atomic.Int32
	testHookCreate = func(stage string) error {
		if stage == "queue" && queued.Add(1) == 2 {
			if _, err := f.home.GC(context.Background(), time.Now().Add(time.Hour), false); err != nil {
				return err
			}
		}
		return nil
	}
	reps, err := f.r.Compare(context.Background(), []string{"w1", "w2"}, "x", repo, TurnOptions{Worktree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if rep.Outcome != "success" {
			t.Fatalf("%s: %v", rep.Outcome, rep.Err)
		}
	}
}

// 2: a commit that cannot remove a mark rolls every run back.
func TestFailedCommitRollsBackEveryRun(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("w1", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	f.profile("w2", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	var commits atomic.Int32
	testHookCreate = func(stage string) error {
		if stage == "commit" && commits.Add(1) == 2 {
			return errors.New("disk full")
		}
		return nil
	}
	defer func() { testHookCreate = nil }()
	if _, err := f.r.Compare(context.Background(), []string{"w1", "w2"}, "x", repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("a failed commit reported success")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
}

// 3: Start's caller can cancel creation while it waits for the worktree
// lock.
func TestCreationLockWaitIsCancellable(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	lk, err := lockOpen(filepath.Join(f.home.Dir, "worktrees.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	if _, err := f.r.Start(ctx, "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("creation went on without the lock")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("returned %s after it was cancelled", d-300*time.Millisecond)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
	}
}

// 4: a stop asked for after the agent's turn ended, before its commit
// started, stops the commit.
func TestStopBetweenTurnAndCommit(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	var id atomic.Value
	testHookTurnEnd = func() {
		if _, err := f.r.Stop(id.Load().(string)); err != nil {
			t.Error(err)
		}
	}
	stopped := make(chan bool, 1)
	testHookFinalize = func(ctx context.Context) {
		select {
		case <-ctx.Done():
			stopped <- true
		case <-time.After(5 * time.Second):
			stopped <- false
		}
	}
	defer func() { testHookTurnEnd, testHookFinalize = nil, nil }()
	reps, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, func(r *Run) { id.Store(r.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if !<-stopped {
		t.Fatal("the stop was lost")
	}
	if !strings.Contains(reps[0].Changes, "NOT committed") {
		t.Fatalf("changes: %q", reps[0].Changes)
	}
}

// 5: a rollback does not delete its branch while another worktree has it
// checked out.
func TestRollbackKeepsCheckedOutBranch(t *testing.T) {
	repo := gitRepo(t)
	base, err := git(context.Background(), "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), "-C", repo, "branch", "harness/x", base); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := git(context.Background(), "-C", repo, "worktree", "add", "-q", other, "harness/x"); err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "x", Cwd: repo, Worktree: filepath.Join(t.TempDir(), "mine"), Branch: "harness/x", Base: base}
	if err := removeWorktree(context.Background(), run, true); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("%v", err)
	}
	if tip, err := branchTip(context.Background(), []string{"-C", repo}, "harness/x"); err != nil || tip != base {
		t.Fatalf("the checked-out branch was deleted: %q %v", tip, err)
	}
}
