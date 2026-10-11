package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Regression tests for the findings of the v1.1 review.

func waitFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Finding 2: a worker that dies takes its agent's whole tree with it
// (Windows: the job closes; Unix: the guard sees its pipe close and kills
// the process group), and the turn is reported lost once the tree is gone.
func TestWorkerDeathEndsAgentTree(t *testing.T) {
	f := newFixture(t, nil)
	pidfile := filepath.Join(t.TempDir(), "pid")
	f.profile("hang", "harness: fake\neffect: pure\nmode: tree\nopts: pidfile="+pidfile)
	runs, err := f.r.Prepare("hang", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	if err := f.r.Detach(id, 1); err != nil {
		t.Fatal(err)
	}
	grandchild := waitFile(t, pidfile)
	run, _ := f.home.LoadRun(id)
	worker, err := os.FindProcess(run.Turns[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	waitDead(t, grandchild)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, finished, err := f.r.WaitTurn(ctx, id, 1)
	if err != nil || !finished || rep.Outcome != "uncertain" {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
}

// Finding 5: a policy Herta rejects fails before waiting at the gate, even
// in a trusted project file that never passed through SaveProfile.
func TestInvalidProjectPolicyFailsFast(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	repo := t.TempDir()
	proj := filepath.Join(repo, ".harness")
	os.MkdirAll(filepath.Join(proj, "profiles"), 0o755)
	// Herta refuses retrying Uncertain on a non-idempotent profile; the
	// budget is held elsewhere, so checking only after the gate would wait.
	os.WriteFile(filepath.Join(proj, "profiles", "unsafe.md"),
		[]byte("---\nharness: fake\neffect: non-idempotent\nretry: uncertain x2\nbudget: b=1\nmode: echo\n---\n"), 0o644)
	f.home.ApproveProject(proj)
	holder, err := f.home.Gate(nil).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reps, err := f.r.Start(ctx, "unsafe", []string{"x"}, repo, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reps[0].Outcome == "cancelled" || !strings.Contains(reps[0].Err.Error(), "unsafe retry") {
		t.Fatalf("want an immediate policy error, got %+v", reps[0])
	}
}

// Finding 6: waiting for a turn is not redirected to a later turn.
func TestWaitTurnStaysOnItsTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "echo", "first")[0]
	if _, err := f.r.PrepareSend(rep.Run.ID, "second, queued", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, finished, err := f.r.WaitTurn(ctx, rep.Run.ID, 1)
	if err != nil || !finished || got.Turn != 1 || got.Output != "echo: first" {
		t.Fatalf("finished=%v %+v %v", finished, got, err)
	}
}

// Finding 7: a detached worker runs with its starter's environment, even
// when started through WMI.
func TestDetachedWorkerGetsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Setenv("HARNESS_DETACH", "wmi")
	}
	t.Setenv("HARNESS_TEST_SECRET", "s3cret-from-the-shell")
	f := newFixture(t, nil)
	f.profile("env", "harness: fake\neffect: pure\nmode: env")
	runs, err := f.r.Prepare("env", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Detach(runs[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, finished, err := f.r.Wait(ctx, runs[0].ID)
	if err != nil || !finished || rep.Output != "s3cret-from-the-shell" {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
	if _, err := os.Stat(runs[0].Path("turn-1.env")); err == nil {
		t.Fatal("the worker must delete its environment file")
	}
}

// Finding 8: Shutdown withdraws a turn waiting at the gate.
func TestShutdownWithdrawsGateWaiters(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.profile("p", "harness: fake\neffect: pure\nbudget: b=1\nmode: echo")
	holder, err := f.home.Gate(nil).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	runs, err := f.r.Prepare("p", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	reps := make(chan Report, 1)
	go func() { reps <- f.r.Execute(context.Background(), runs[0].ID, 1) }()
	time.Sleep(500 * time.Millisecond) // the turn is now waiting at the gate
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.r.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown did not drain: %v", err)
	}
	rep := <-reps
	if rep.Outcome != "stopped" {
		t.Fatalf("%+v", rep)
	}
	if rep := f.r.Execute(context.Background(), runs[0].ID, 1); rep.Err != hertasdk.ErrRuntimeStopping {
		t.Fatalf("after Shutdown: %+v", rep)
	}
}

// Second review: the environment handed to a worker holds the caller's
// secrets; a turn whose worker never started does not leave it behind.
func TestLostTurnRemovesEnvironmentFile(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("echo", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	env := runs[0].Path("turn-1.env")
	os.WriteFile(env, []byte(`["API_KEY=secret"]`), 0o600) // as Detach writes it
	f.home.updateRun(runs[0].ID, func(run *Run) error {
		run.Turns[0].Queued = time.Now().Add(-2 * lostAfter) // its worker never came
		return nil
	})
	rep, finished, err := f.r.WaitTurn(context.Background(), runs[0].ID, 1)
	if err != nil || !finished || rep.Outcome != "transient" {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
	if _, err := os.Stat(env); err == nil {
		t.Fatal("the environment file of a turn whose worker never started was left behind")
	}
}
