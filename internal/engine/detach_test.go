package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Detached runs execute in separate worker processes (this test binary,
// started through Detach), so the budget must hold across processes.
func TestDetachedRunsShareBudgetAcrossProcesses(t *testing.T) {
	f := newFixture(t, map[string]int64{"fake": 1})
	dir := t.TempDir()
	f.profile("one", "harness: fake\neffect: pure\nbudget: fake=1\nmode: interval\nopts: ms=400 dir="+dir)
	runs, err := f.r.Prepare("one", []string{"a", "b", "c"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if err := f.r.Detach(run.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, run := range runs {
		rep, finished, err := f.r.Wait(ctx, run.ID)
		if err != nil || !finished || rep.Outcome != "success" {
			log, _ := os.ReadFile(run.Path("worker.log"))
			t.Fatalf("run %s: finished=%v %+v %v\nworker.log:\n%s", run.ID, finished, rep, err, log)
		}
	}
	if peak := peakOverlap(t, dir); peak != 1 {
		t.Fatalf("peak concurrency %d across worker processes with capacity 1", peak)
	}
}

func TestDetachedStop(t *testing.T) {
	f := newFixture(t, nil)
	pidfile := filepath.Join(t.TempDir(), "pid")
	f.profile("hang", "harness: fake\neffect: pure\nmode: tree\nopts: pidfile="+pidfile)
	runs, err := f.r.Prepare("hang", []string{"x"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	if err := f.r.Detach(id, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(pidfile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached worker never started the CLI")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := f.r.Stop(id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, finished, err := f.r.Wait(ctx, id)
	if err != nil || !finished || rep.Outcome != "cancelled" {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
	data, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(string(data))
	waitDead(t, pid)
}

func TestWaitReapsLostTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("echo", []string{"x"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Running, but no process holds the execution lock: its worker died.
	f.home.updateRun(runs[0].ID, func(run *Run) error {
		run.Turns[0].Outcome, run.Turns[0].Started = "running", time.Now()
		return nil
	})
	rep, finished, err := f.r.Wait(context.Background(), runs[0].ID)
	if err != nil || !finished || rep.Outcome != "uncertain" || !strings.Contains(rep.Err.Error(), "exited without finishing") {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
}

func TestWaitTimesOutWhileRunning(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("echo", []string{"x"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rep, finished, err := f.r.Wait(ctx, runs[0].ID)
	if err != nil || finished || rep.Run.Turns[0].Outcome != "queued" {
		t.Fatalf("finished=%v %+v %v", finished, rep, err)
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Processes the CLI leaves running when it exits are killed with it.
func TestLeftoverProcessesAreKilled(t *testing.T) {
	f := newFixture(t, nil)
	pidfile := filepath.Join(t.TempDir(), "pid")
	f.profile("orphan", "harness: fake\neffect: pure\nmode: orphan\nopts: pidfile="+pidfile)
	rep := f.start(context.Background(), "orphan", "x")[0]
	if rep.Err != nil || rep.Output != "spawned" {
		t.Fatalf("%+v", rep)
	}
	data, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(string(data))
	waitDead(t, pid)
}
