package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the fifth review of v2.0.0-next.

// 1 (and the sixth review's 3): a turn stopped before its cost could be
// written leaves the cost unknown, and turns under a limit are refused
// until its entry is removed.
func TestUnknownCostRefusesLimitedTurns(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nbudget: b=1\nmode: usage\nspend_limit_usd: 5\nspend_unknown: refuse")
	runs, err := f.r.Prepare("u", []string{"first", "second", "third"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var closes atomic.Int32
	testHookSpend = func(stage string) error {
		if stage == "close" {
			closes.Add(1)
			return errors.New("disk full")
		}
		return nil
	}
	defer func() { testHookSpend = nil }()
	ctx, cancel := context.WithCancel(context.Background())
	first, second := make(chan Report), make(chan Report)
	go func() { first <- f.r.Execute(ctx, runs[0].ID, 1) }()
	for closes.Load() < 2 { // it stays admitted, trying again
		time.Sleep(50 * time.Millisecond)
	}
	go func() { second <- f.r.Execute(context.Background(), runs[1].ID, 1) }()
	time.Sleep(300 * time.Millisecond) // the second waits for the budget
	cancel()
	rep := <-first
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "spending ledger") {
		t.Fatalf("first: %s %v", rep.Outcome, rep.Err)
	}
	if rep = <-second; rep.Outcome != "throttled" || !strings.Contains(rep.Err.Error(), "unknown") {
		t.Fatalf("second: %s %v", rep.Outcome, rep.Err)
	}
	testHookSpend = nil
	left, _ := filepath.Glob(filepath.Join(f.home.spendDir(), runs[0].ID+"-*.json"))
	if len(left) != 1 {
		t.Fatalf("entries: %v", left)
	}
	os.Remove(left[0])
	if rep := f.r.Execute(context.Background(), runs[2].ID, 1); rep.Outcome != "success" {
		t.Fatalf("third: %s %v", rep.Outcome, rep.Err)
	}
}

// The sixth review's 5: a ledger entry that cannot be read refuses turns
// under a limit; a file being written does not count.
func TestUnreadableLedgerEntryRefuses(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5\nspend_unknown: refuse")
	f.profile("free", "harness: fakeusage\neffect: pure\nmode: usage")
	os.MkdirAll(f.home.spendDir(), 0o700)
	os.WriteFile(filepath.Join(f.home.spendDir(), ".tmp-123"), []byte("{half"), 0o600)
	if rep := f.start(context.Background(), "u", "x")[0]; rep.Outcome != "success" {
		t.Fatalf("a file being written counted: %v", rep.Err)
	}
	os.WriteFile(filepath.Join(f.home.spendDir(), "r-1-1.json"), []byte("{half"), 0o600)
	if _, err := f.r.Start(context.Background(), "u", []string{"y"}, f.cwd, TurnOptions{}, nil); err == nil || !strings.Contains(err.Error(), "cannot be read") || OutcomeName(err) != "throttled" {
		t.Fatalf("an unreadable entry did not refuse: %v", err)
	}
	if rep := f.start(context.Background(), "free", "z")[0]; rep.Outcome != "success" {
		t.Fatalf("a turn under no limit was refused: %v", rep.Err)
	}
}

// 2: a creation commits by one decision: a crash after it leaves every run
// created, even with some marks still there; a crash before it leaves
// every run to gc's rollback.
func TestCreationCommitIsOneDecision(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	ctx := context.Background()
	nr, err := f.r.check("writer", []string{"a", "b"}, repo, TurnOptions{Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.r.create(ctx, nr, "committed")
	if err != nil {
		t.Fatal(err)
	}
	// The creator committed, removed one mark, and died.
	if err := f.home.commit("committed"); err != nil {
		t.Fatal(err)
	}
	os.Remove(staged[0].Path(creatingFile))
	for _, s := range staged {
		s.release()
	}
	if rep, err := f.home.GC(ctx, time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 0 {
		t.Fatalf("gc removed committed runs: %+v %v", rep, err)
	}
	for _, s := range staged {
		if rep := f.r.Execute(ctx, s.ID, 1); rep.Outcome != "success" {
			t.Fatalf("a committed run did not execute: %v", rep.Err)
		}
	}

	nr, err = f.r.check("writer", []string{"c", "d"}, repo, TurnOptions{Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	if staged, err = f.r.create(ctx, nr, "lost"); err != nil {
		t.Fatal(err)
	}
	// The creator died before it committed.
	for _, s := range staged {
		s.release()
	}
	if rep, err := f.home.GC(ctx, time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 2 {
		t.Fatalf("gc did not roll back the uncommitted runs: %+v %v", rep, err)
	}
	if out, _ := git(ctx, "-C", repo, "branch", "--list", "harness/*"); strings.Count(out, "harness/") != 2 {
		t.Fatalf("branches: %q", out)
	}
}

// 3: waits for the creation lock end with the caller, in Start and in gc.
func TestCreateLockWaitIsCancellable(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	lk, err := lockOpen(filepath.Join(f.home.Dir, "create.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	if _, err := f.r.Start(ctx, "echo", []string{"x"}, f.cwd, TurnOptions{}, nil); err == nil {
		t.Fatal("a run was created without the creation lock")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Start returned after %s", d)
	}
	os.WriteFile(filepath.Join(f.home.Dir, "runs", stagePrefix+"261012-000000-abcd.lock"), nil, 0o600)
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start = time.Now()
	f.home.GC(ctx, time.Now(), false)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("gc returned after %s", d)
	}
}

// 4: a watch ends between takes: a take in progress finishes first, and a
// request after the end is left for the next watch.
func TestStopTakenOnlyWhileWatchLives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stop-1")
	w := &stopWatch{path: path}
	ctx, cancel := context.WithCancel(context.Background())
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.hook = func() { once.Do(func() { close(paused); <-resume }) }
	took := make(chan bool)
	go func() { took <- w.take(ctx) }()
	<-paused // past its context check
	ended := make(chan struct{})
	go func() { w.end(cancel); close(ended) }()
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("the watch ended in the middle of a take")
	}
	close(resume)
	if <-took {
		t.Fatal("took a request that was not there")
	}
	<-ended
	os.WriteFile(path, []byte("now"), 0o600)
	if w.take(ctx) {
		t.Fatal("an ended watch took a request")
	}
	if !w.take(context.Background()) {
		t.Fatal("the next watch did not get the request")
	}
}
