package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func hasEvent(evs []Event, kind, name string) bool {
	return slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == kind && (name == "" || e.Name == name) })
}

// An agent that streams events and then goes quiet for its idle limit is
// stopped and parked: not retried, its session kept, and told how to go on.
func TestIdleParks(t *testing.T) {
	f := newFixture(t, nil)
	progressDef(f)
	gate := filepath.Join(t.TempDir(), "gate") // never opened: the agent hangs
	f.profile("w", "harness: stream\neffect: idempotent\nretry: uncertain x3\nidle: 700ms\nmode: progress\nopts: gate="+gate)
	start := time.Now()
	rep := f.start(context.Background(), "w", "go")[0]
	if rep.Outcome != "uncertain" || rep.Attempts != 1 || !strings.Contains(rep.Err.Error(), "idle limit") {
		t.Fatalf("report: %s after %d attempt(s): %v", rep.Outcome, rep.Attempts, rep.Err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	if !strings.Contains(rep.Hint, "parked") || !strings.Contains(rep.Hint, "harness send "+rep.Run.ID) {
		t.Fatalf("hint: %q", rep.Hint)
	}
	if rep.Run.Session == "" {
		t.Fatal("the parked turn's session was not kept")
	}
	evs, _, _ := f.home.Events(rep.Run.ID, 0)
	if !hasEvent(evs, EvTimeoutSoon, "idle") || !hasEvent(evs, EvFinished, "uncertain") {
		t.Fatalf("events: %v", kinds(evs))
	}
}

// A CLI that prints only at the end has no idle limit unless the profile
// sets one: its silence means nothing.
func TestIdleDefaults(t *testing.T) {
	text := &Definition{Output: OutputRule{Format: "text"}}
	stream := &Definition{Output: OutputRule{Format: "jsonl"}}
	p := &Profile{}
	if idleFor(p, text) != 0 || idleFor(p, stream) != defaultIdle {
		t.Fatal("defaults")
	}
	off, err := ParseProfile("x", []byte("---\nharness: h\neffect: pure\nidle: none\n---\n"))
	if err != nil || idleFor(off, stream) != 0 {
		t.Fatalf("idle: none: %v", err)
	}
}

// A turn that runs past its timeout ends uncertain, as before (and is
// retried if the profile says so), after a timeout_soon warning; one that
// is extended in time finishes.
func TestTimeoutAndExtend(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("slow", "harness: fake\neffect: idempotent\ntimeout: 1s\nretry: uncertain x2\nmode: sleep\nopts: ms=2500")
	rep := f.start(context.Background(), "slow", "go")[0]
	if rep.Outcome != "uncertain" || rep.Attempts != 2 || !strings.Contains(rep.Hint, "parked") {
		t.Fatalf("timed out: %s after %d attempt(s), hint %q", rep.Outcome, rep.Attempts, rep.Hint)
	}
	evs, _, _ := f.home.Events(rep.Run.ID, 0)
	if !hasEvent(evs, EvTimeoutSoon, "timeout") || !hasEvent(evs, EvRetry, "uncertain") {
		t.Fatalf("events: %v", kinds(evs))
	}

	runs, err := f.r.Prepare("slow", []string{"again"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var wg sync.WaitGroup
	wg.Go(func() { rep = f.r.Execute(context.Background(), id, 1) })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if evs, _, _ := f.home.Events(id, 0); hasEvent(evs, EvAttempt, "") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the attempt never began")
		}
	}
	until, err := f.r.Extend(id, 4*time.Second, "long build", "caller")
	if err != nil || time.Until(until) < 3*time.Second {
		t.Fatalf("extend: %v %v", until, err)
	}
	st, err := f.r.Status(context.Background(), id)
	if err != nil || !st.Extended || !st.Deadline.Equal(until) {
		t.Fatalf("status after extend: %+v %v", st, err)
	}
	wg.Wait()
	if rep.Outcome != "success" || rep.Output != "slept" || rep.Attempts != 1 {
		t.Fatalf("extended: %s after %d attempt(s): %v", rep.Outcome, rep.Attempts, rep.Err)
	}
	evs, _, _ = f.home.Events(id, 0)
	if !hasEvent(evs, EvExtend, "caller") {
		t.Fatalf("events: %v", kinds(evs))
	}
	if _, err := f.r.Extend(id, time.Minute, "", "caller"); err == nil || !strings.Contains(err.Error(), "no unfinished turn") {
		t.Fatalf("extending a finished run: %v", err)
	}
}

// wait wakes early when the turn needs its caller: its timeout is near.
func TestWaitAttention(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("slow", "harness: fake\neffect: idempotent\ntimeout: 1500ms\nmode: sleep\nopts: ms=3000")
	runs, err := f.r.Prepare("slow", []string{"go"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var wg sync.WaitGroup
	wg.Go(func() { f.r.Execute(context.Background(), id, 1) })
	defer wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, finished, alerts, err := f.r.WaitAttention(ctx, id)
	if err != nil || finished || len(alerts) != 1 || alerts[0].Kind != EvTimeoutSoon || !strings.Contains(alerts[0].Text, "harness extend "+id) {
		t.Fatalf("finished=%v alerts=%+v err=%v", finished, alerts, err)
	}
	f.r.Extend(id, 10*time.Second, "", "caller")
	rep, finished, alerts, err := f.r.WaitAttention(ctx, id)
	if err != nil || !finished || len(alerts) != 0 || rep.Outcome != "success" {
		t.Fatalf("after extending: %s finished=%v alerts=%v err=%v", rep.Outcome, finished, alerts, err)
	}
}

// A turn that ends with its context past handover_at says to hand over;
// handover_at: none says nothing.
func TestHandoverHint(t *testing.T) {
	f := newFixture(t, nil)
	progressDef(f)
	gate := filepath.Join(t.TempDir(), "gate")
	os.WriteFile(gate, nil, 0o644)
	// 400 tokens of a 500-token window: 80%.
	f.profile("w", "harness: stream\neffect: idempotent\ncontext_window: 500\nmode: progress\nopts: gate="+gate)
	rep := f.start(context.Background(), "w", "go")[0]
	if rep.Err != nil || !strings.Contains(rep.Hint, "80% full") || !strings.Contains(rep.Hint, "harness handover "+rep.Run.ID) {
		t.Fatalf("hint: %q (%v)", rep.Hint, rep.Err)
	}
	st, _ := f.r.Status(context.Background(), rep.Run.ID)
	if !slices.ContainsFunc(st.Next(), func(s string) bool { return strings.HasPrefix(s, "harness handover") }) {
		t.Fatalf("next: %v", st.Next())
	}
	f.profile("w", "harness: stream\neffect: idempotent\ncontext_window: 500\nhandover_at: none\nmode: progress\nopts: gate="+gate)
	if rep := f.start(context.Background(), "w", "go")[0]; strings.Contains(rep.Hint, "handover") {
		t.Fatalf("handover_at none: %q", rep.Hint)
	}
}

// Handover asks the agent for notes in its session, then starts a new run
// of its profile with them, which records where it came from.
func TestHandover(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nkind: review\nmode: echo")
	old := f.start(context.Background(), "echo", "do the thing")[0]
	nr, notes, err := f.r.Handover(context.Background(), old.Run.ID, "write the tests")
	if err != nil {
		t.Fatal(err)
	}
	if notes.Turn != 2 || !strings.Contains(notes.Output, "handover notes") {
		t.Fatalf("notes: %+v", notes)
	}
	if nr.From != old.Run.ID || nr.Profile != "echo" || nr.Cwd != f.cwd {
		t.Fatalf("new run: %+v", nr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, finished, err := f.r.Wait(ctx, nr.ID)
	if err != nil || !finished || rep.Outcome != "success" {
		t.Fatalf("new run: %s %v %v", rep.Outcome, finished, err)
	}
	if !strings.Contains(rep.Output, "continuing work started in harness run "+old.Run.ID) ||
		!strings.Contains(rep.Output, "Next: write the tests") || !strings.Contains(rep.Output, "Write handover notes") {
		t.Fatalf("new run's prompt: %s", rep.Output)
	}
	evs, _, _ := f.home.Events(old.Run.ID, 0)
	if !hasEvent(evs, EvHandover, nr.ID) {
		t.Fatalf("old run's events: %v", kinds(evs))
	}
}

func TestParseShare(t *testing.T) {
	for in, want := range map[string]float64{"80%": 0.8, "0.8": 0.8, "none": 0, "100%": 1} {
		if got, err := parseShare(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"150%", "-1", "most"} {
		if _, err := parseShare(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}
