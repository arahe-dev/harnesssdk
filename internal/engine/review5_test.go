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

// 1: a cost that cannot go into the run record is recorded apart and
// counts at once; a turn whose cost is on record nowhere stays admitted
// until it is.
func TestCostRecordedApart(t *testing.T) {
	for _, c := range []struct {
		name                         string
		saves, apart, finish, wantIn int32 // failures of each; files expected in spend/
	}{
		{"record fails, apart works", 1 << 20, 0, 1 << 20, 1},
		{"both fail, then the record works", 1, 1 << 20, 2, 0},
	} {
		f := newFixture(t, map[string]int64{"b": 1})
		usageDef(f) // $0.50 a turn
		f.profile("u", "harness: fakeusage\neffect: pure\nbudget: b=1\nmode: usage\nspend_limit_usd: 0.4")
		var saves, apart, finishes atomic.Int32
		fail := func(n *atomic.Int32, limit int32) func() error {
			return func() error {
				if n.Add(1) <= limit {
					return errors.New("disk full")
				}
				return nil
			}
		}
		testHookSaveMetrics, testHookSaveApart, testHookFinish = fail(&saves, c.saves), fail(&apart, c.apart), fail(&finishes, c.finish)
		testHookFinalize = func(context.Context) { time.Sleep(time.Second) } // a slow end of turn
		reps, err := f.r.Start(context.Background(), "u", []string{"first", "second"}, gitRepo(t), TurnOptions{Worktree: true}, nil)
		testHookSaveMetrics, testHookSaveApart, testHookFinish, testHookFinalize = nil, nil, nil, nil
		if err != nil {
			t.Fatal(err)
		}
		var outcomes []string
		for _, rep := range reps {
			outcomes = append(outcomes, rep.Outcome)
		}
		if !strings.Contains(strings.Join(outcomes, " "), "throttled") {
			t.Fatalf("%s: the second turn was admitted against a total without the first's cost: %v", c.name, outcomes)
		}
		if _, spent, err := f.home.spending(time.Now().Add(-time.Hour), "u"); err != nil || spent != 0.5 {
			t.Fatalf("%s: spent %v, %v", c.name, spent, err)
		}
		if files, _ := os.ReadDir(f.home.spendDir()); int32(len(files)) != c.wantIn {
			t.Fatalf("%s: %d costs recorded apart", c.name, len(files))
		}
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
	testHookTake = func() { once.Do(func() { close(paused); <-resume }) }
	defer func() { testHookTake = nil }()
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
