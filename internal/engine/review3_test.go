package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the third review of v2.0.0-next.

func fixedRunIDs(t *testing.T, ids ...string) {
	t.Helper()
	var n atomic.Int32
	runID = func() string { return ids[int(n.Add(1)-1)%len(ids)] }
	t.Cleanup(func() { runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) } })
}

// H1: a creation that finds its branch name taken, here by a run of
// another data folder, does not claim it: neither its rollback nor its gc
// touches the other run's branch.
func TestOtherHomesBranchIsLeftAlone(t *testing.T) {
	a, b := newFixture(t, nil), newFixture(t, nil)
	for _, f := range []*fixture{a, b} {
		f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	}
	repo := gitRepo(t)
	fixedRunIDs(t, "261012-000000-cccc")
	reps, err := a.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil)
	if err != nil || reps[0].Outcome != "success" {
		t.Fatalf("%v %+v", err, reps)
	}
	tip, err := git(context.Background(), "-C", repo, "rev-parse", "harness/261012-000000-cccc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a taken branch name was not refused: %v", err)
	}
	// a's run is collected (its branch stays), then b collects.
	for _, f := range []*fixture{a, b} {
		f.home.GC(context.Background(), time.Now().Add(time.Hour), false)
	}
	if now, err := git(context.Background(), "-C", repo, "rev-parse", "harness/261012-000000-cccc"); err != nil || now != tip {
		t.Fatalf("the other run's branch was changed: %q %v", now, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(b.home.Dir, "worktrees")); len(entries) != 0 {
		t.Fatalf("the refused creation left %d worktree folders", len(entries))
	}
}

// H2: harness's commits run no hook even when the place a fixed hooks
// folder would be holds one.
func TestNoHooksFolderCannotBePlanted(t *testing.T) {
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	planted := filepath.Join(tmp, "harness-no-hooks")
	os.MkdirAll(planted, 0o755)
	os.WriteFile(filepath.Join(planted, "post-commit"), []byte("#!/bin/sh\necho ran > '"+filepath.ToSlash(marker)+"'\n"), 0o755)
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, _ := worktreeRun(t, f, "writer", "")
	if !strings.Contains(run.Turns[0].Changes, "1 file changed") {
		t.Fatalf("not committed: %q", run.Turns[0].Changes)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted hook ran on harness's commit")
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "harness-no-hooks-*")); len(left) != 0 {
		t.Fatalf("hooks folders left behind: %v", left)
	}
}

// M3: an attempt whose cost could not be written keeps its admission
// until it is, so the next turn admitted counts it.
func TestUnrecordedCostKeepsAdmission(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nbudget: b=1\nmode: usage\nspend_limit_usd: 0.4")
	var closes atomic.Int32
	testHookSpend = func(stage string) error {
		if stage == "close" && closes.Add(1) <= 2 {
			return errors.New("disk full")
		}
		return nil
	}
	defer func() { testHookSpend = nil }()
	reps := f.start(context.Background(), "u", "first", "second")
	var outcomes []string
	for _, rep := range reps {
		outcomes = append(outcomes, rep.Outcome)
	}
	if o := strings.Join(outcomes, " "); !strings.Contains(o, "success") || !strings.Contains(o, "throttled") {
		t.Fatalf("both turns ran, or neither: %v", outcomes)
	}
	if s, err := f.home.spending(time.Now().Add(-time.Hour), "u"); err != nil || s.mine != 0.5 {
		t.Fatalf("spent %+v, %v", s, err)
	}
}

// M4: a cancelled caller leaves a gate pass while it waits for a ledger
// lock, without waiting the lock out.
func TestCancelledDuringLockWait(t *testing.T) {
	g := newGateHome(t).Gate(nil)
	os.MkdirAll(g.ledger("b"), 0o700)
	lk, err := lockOpen(filepath.Join(g.ledger("b"), "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	if _, err := g.Acquire(ctx, []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("left %s after it was cancelled", d-300*time.Millisecond)
	}
}

// M5: a compare whose rollback of an earlier run cannot finish leaves the
// run marked as being created: it never executes, and once its creator is
// gone gc finishes the rollback, branch included, whatever the run's age.
func TestAbandonedRunIsRolledBackByGC(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("w1", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	f.profile("w2", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	fixedRunIDs(t, "261012-000000-aaaa", "261012-000000-bbbb")
	refLock := filepath.Join(repo, ".git", "refs", "heads", "harness", "261012-000000-aaaa.lock")
	var stages atomic.Int32
	testHookCreate = func(stage string) error {
		if stage == "queue" && stages.Add(1) == 2 {
			os.WriteFile(refLock, nil, 0o644) // the first run's branch cannot be deleted now
			return fmt.Errorf("disk full")
		}
		return nil
	}
	defer func() { testHookCreate = nil }()
	if _, err := f.r.Compare(context.Background(), []string{"w1", "w2"}, "x", repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("failed creation reported success")
	}
	testHookCreate = nil
	if _, err := os.Stat(filepath.Join(f.home.Dir, "runs", "261012-000000-aaaa", creatingFile)); err != nil {
		t.Fatal("the run whose rollback failed is not marked")
	}
	if rep := f.r.Execute(context.Background(), "261012-000000-aaaa", 1); rep.Err == nil || !strings.Contains(rep.Err.Error(), "being created") {
		t.Fatalf("an abandoned run executed: %v", rep.Err)
	}
	os.Remove(refLock)
	if rep, err := f.home.GC(context.Background(), time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
}

// M6: Start's caller can cancel the creation of worktrees.
func TestStartCancelledDuringCreation(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.r.Start(ctx, "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("creation ignored a cancelled caller")
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
	}
}

// M6: harness stop reaches a turn's final commit.
func TestStopReachesFinalization(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	var id atomic.Value
	entered, stopped := make(chan struct{}), make(chan bool, 1)
	testHookFinalize = func(ctx context.Context) {
		close(entered)
		select {
		case <-ctx.Done():
			stopped <- true
		case <-time.After(10 * time.Second):
			stopped <- false
		}
	}
	defer func() { testHookFinalize = nil }()
	go func() {
		<-entered
		if _, err := f.r.Stop(id.Load().(string)); err != nil {
			t.Error(err)
		}
	}()
	reps, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, func(r *Run) { id.Store(r.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if !<-stopped {
		t.Fatal("stop did not reach the commit")
	}
	if !strings.Contains(reps[0].Changes, "NOT committed") {
		t.Fatalf("changes: %q", reps[0].Changes)
	}
}

// M7: an error selector with pick last is decided by the last matching
// event, so a recovered error does not fail the run, and a final one with
// partial text does.
func TestErrorDecidedByLastEvent(t *testing.T) {
	def := func(pick string) *Definition {
		d, err := ParseDefinition("pi", []byte(`{"command": "pi", "output": {"format": "jsonl",
			"answer": {"where": {"type": "turn_end"}, "field": "message.content.-1.text"},
			"error": {"where": {"type": "turn_end"}, "field": "message.errorMessage"`+pick+`}}}`))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	recovered := `{"type":"turn_end","message":{"stopReason":"toolUse","content":[{"type":"text","text":"looking"}]}}
{"type":"turn_end","message":{"stopReason":"error","errorMessage":"WebSocket error","content":[]}}
{"type":"agent_end","messages":[]}
{"type":"auto_retry_start","attempt":1}
{"type":"turn_end","message":{"stopReason":"stop","content":[{"type":"text","text":"done"}]}}
`
	failed := `{"type":"turn_end","message":{"stopReason":"stop","content":[{"type":"text","text":"done"}]}}
{"type":"turn_end","message":{"stopReason":"error","errorMessage":"overloaded","content":[{"type":"text","text":"partial"}]}}
`
	for _, c := range []struct {
		pick, stream string
		failed       bool
	}{
		{`, "pick": "last"`, recovered, false},
		{`, "pick": "last"`, failed, true},
		{``, recovered, true}, // without pick last, any error counts
	} {
		x, err := def(c.pick).extractEvents(strings.NewReader(c.stream))
		if err != nil || x.Failed != c.failed {
			t.Errorf("pick %q: failed %v, want %v (%v)", c.pick, x.Failed, c.failed, err)
		}
	}
}
