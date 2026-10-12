package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestMaxDepth(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	if err := f.home.SetConfig("max_depth", 1); err != nil {
		t.Fatal(err)
	}
	top, err := f.r.Prepare("echo", []string{"x"}, f.cwd, nil, "")
	if err != nil || top[0].Depth != 0 {
		t.Fatalf("top-level run: %v %v", top, err)
	}
	child, _ := NewRunnerFor(f.home, top[0].ID, nil)
	mid, err := child.Prepare("echo", []string{"x"}, f.cwd, nil, "")
	if err != nil || mid[0].Depth != 1 || mid[0].Parent != top[0].ID {
		t.Fatalf("nested run: %+v %v", mid, err)
	}
	grandchild, _ := NewRunnerFor(f.home, mid[0].ID, nil)
	_, err = grandchild.Prepare("echo", []string{"x"}, f.cwd, nil, "")
	if !errors.Is(err, ErrTooDeep) || OutcomeName(err) != "permanent" {
		t.Fatalf("run below max_depth: err = %v (%s)", err, OutcomeName(err))
	}
}

func TestMaxRunning(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	if err := f.home.SetConfig("max_running", 1); err != nil {
		t.Fatal(err)
	}
	slot := Claim{Name: runningGate, Units: 1, Capacity: 1}
	held, err := f.home.Gate(nil).Acquire(context.Background(), []Claim{slot}, false, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	// A top-level turn waits for the slot.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	rep := f.start(ctx, "echo", "x")[0]
	if OutcomeName(rep.Err) != "cancelled" {
		t.Fatalf("top-level turn with no slot free: %s %v", OutcomeName(rep.Err), rep.Err)
	}
	// A nested turn whose ancestors hold no slot fails as throttled at once.
	nested, _ := NewRunnerFor(f.home, rep.Run.ID, []string{"tsomeoneelse"})
	start := time.Now()
	reps, err := nested.Start(context.Background(), "echo", []string{"x"}, f.cwd, nil, "", nil)
	if err != nil || OutcomeName(reps[0].Err) != "throttled" || time.Since(start) > 5*time.Second {
		t.Fatalf("nested turn with no slot free: %v %s %v", err, OutcomeName(reps[0].Err), reps[0].Err)
	}
	// One whose ancestor holds the only slot could never run.
	nested, _ = NewRunnerFor(f.home, rep.Run.ID, []string{held.ID})
	reps, _ = nested.Start(context.Background(), "echo", []string{"x"}, f.cwd, nil, "", nil)
	if !errors.Is(reps[0].Err, ErrWouldDeadlock) {
		t.Fatalf("nested turn under the holder: %v", reps[0].Err)
	}
	held.Release()
	if rep := f.start(context.Background(), "echo", "x")[0]; rep.Err != nil {
		t.Fatalf("with the slot free: %v", rep.Err)
	}
}

// Discovery answers at once when no ancestor executes a turn, however
// many turns run elsewhere.
func TestDiscoveryIsImmediateOutsideRuns(t *testing.T) {
	f := newFixture(t, nil)
	run, err := f.home.NewRun("p", "fake", f.cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	lk, err := lockOpen(run.Path("exec.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	// A turn executed by this process, which is no ancestor of itself.
	if err := f.home.writeActive(activeMarker{Run: run.ID, Turn: 1, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if len(f.home.activeMarkers()) != 1 {
		t.Fatal("marker not listed")
	}
	start := time.Now()
	if id := f.home.ancestorRun(); id != "" {
		t.Fatalf("found parent %s", id)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("discovery took %v", d)
	}
}

// oldRun writes a run created days ago, with one turn that ended then.
func oldRun(t *testing.T, h *Home, days int, suffix string) *Run {
	t.Helper()
	at := time.Now().AddDate(0, 0, -days)
	id := at.Format("060102-150405") + "-" + suffix
	dir := filepath.Join(h.Dir, "runs", id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Run{Format: formatVersion, ID: id, Profile: "p", Harness: "fake", Created: at.UTC(), dir: dir,
		Turns: []TurnRecord{{N: 1, Queued: at, Started: at, Ended: at, Outcome: "success"}}}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.Path("turn-1.out.md"), []byte("secret answer\n"), 0o600)
	return r
}

func TestGC(t *testing.T) {
	h := newGateHome(t)
	expired := oldRun(t, h, 40, "aaaa")
	recent := oldRun(t, h, 5, "bbbb")
	resumed := oldRun(t, h, 40, "cccc") // sent a new turn yesterday
	resumed.Turns = append(resumed.Turns, TurnRecord{N: 2, Queued: time.Now().AddDate(0, 0, -1), Outcome: "success"})
	resumed.Save()
	busy := oldRun(t, h, 40, "dddd") // a turn is executing
	lk, err := lockOpen(busy.Path("exec.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	leftover := oldRun(t, h, 40, "eeee") // an interrupted removal
	os.Remove(leftover.Path("run.json"))
	old := time.Now().AddDate(0, 0, -40)
	os.Chtimes(leftover.Dir(), old, old)
	creating := oldRun(t, h, 40, "ffff") // a folder being created right now
	os.Remove(creating.Path("run.json"))

	cutoff := time.Now().AddDate(0, 0, -30)
	rep, err := h.GC(cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{expired.ID, leftover.ID}
	if !slices.Equal(rep.Runs, want) || rep.Bytes == 0 {
		t.Fatalf("dry run: %+v, want %v", rep, want)
	}
	if _, err := os.Stat(expired.Dir()); err != nil {
		t.Fatal("dry run removed a run")
	}
	if rep, err = h.GC(cutoff, false); err != nil || !slices.Equal(rep.Runs, want) {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	for _, r := range []*Run{expired, leftover} {
		if _, err := os.Stat(r.Dir()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not removed: %v", r.ID, err)
		}
	}
	for _, r := range []*Run{recent, resumed, busy} {
		if _, err := h.LoadRun(r.ID); err != nil {
			t.Errorf("%s removed: %v", r.ID, err)
		}
	}
	if _, err := os.Stat(creating.Dir()); err != nil {
		t.Error("a folder being created was removed")
	}
}

func TestAutoGCOncePerDay(t *testing.T) {
	h := newGateHome(t)
	first := oldRun(t, h, 40, "aaaa")
	h.AutoGC()
	if _, err := os.Stat(first.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired run kept")
	}
	second := oldRun(t, h, 40, "bbbb")
	h.AutoGC() // within a day of the last pass
	if _, err := os.Stat(second.Dir()); err != nil {
		t.Fatal("collected twice in a day")
	}
	if err := h.SetConfig("retention_days", 0); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(h.Dir, "gc.stamp"))
	h.AutoGC()
	if _, err := os.Stat(second.Dir()); err != nil {
		t.Fatal("collected with retention off")
	}
}
