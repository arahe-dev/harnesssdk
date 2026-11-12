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

// progressDef is a JSONL harness whose progress rules report the fake
// agent's tool calls and messages.
func progressDef(f *fixture) {
	f.def("stream", Definition{
		Args:    [][]string{{"{mode}"}, {"{*opts}"}, {"--sid", "{new_session}"}, {"--resume", "{resume_session}"}},
		Session: "mint",
		Output: OutputRule{Format: "jsonl",
			Answer:  &Selector{Where: map[string]string{"type": "done"}, Field: "text"},
			Metrics: map[string]*Selector{"turns": {Where: map[string]string{"type": "message"}, Pick: "count"}},
			Progress: []ProgressRule{
				{Kind: EvTool, Where: map[string]string{"type": "tool_start"}, Field: "tool", Detail: "args.path|args.command"},
				{Kind: EvSay, Where: map[string]string{"type": "message", "role": "assistant"}, Field: "text"},
			}},
	})
}

func kinds(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// Every turn leaves its lifecycle in the event log, in order, and a cursor
// reads only what came after it.
func TestEventsLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "echo", "hello")[0]
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	id := rep.Run.ID
	evs, cursor, err := f.home.Events(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(evs), []string{EvQueued, EvStarted, EvAttempt, EvFinished}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if evs[0].Text != "hello" || evs[3].Name != "success" || evs[3].Text != "echo: hello" || evs[2].Attempt != 1 {
		t.Fatalf("event details: %+v", evs)
	}
	if again, next, _ := f.home.Events(id, cursor); len(again) != 0 || next != cursor {
		t.Fatalf("read from the end: %v, cursor %d -> %d", again, cursor, next)
	}
	if _, err := f.r.Send(context.Background(), id, "more", TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	evs, _, _ = f.home.Events(id, cursor)
	if got := kinds(evs); len(got) != 4 || got[0] != EvQueued || evs[0].Turn != 2 || evs[3].Turn != 2 {
		t.Fatalf("second turn events: %+v", evs)
	}
}

// A retried attempt says why, and how long it backs off.
func TestEventsRetry(t *testing.T) {
	f := newFixture(t, nil)
	state := filepath.Join(t.TempDir(), "state")
	f.profile("flaky", "harness: fake\neffect: idempotent\nretry: transient x2\nbackoff: 10ms\nmode: flaky\nopts: state="+state)
	rep := f.start(context.Background(), "flaky", "go")[0]
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	evs, _, _ := f.home.Events(rep.Run.ID, 0)
	got := kinds(evs)
	if want := []string{EvQueued, EvStarted, EvAttempt, EvRetry, EvAttempt, EvFinished}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if evs[3].Name != "transient" || !strings.Contains(evs[3].Text, "attempt 2 of 2") || evs[4].Attempt != 2 {
		t.Fatalf("retry event: %+v", evs[3:5])
	}
}

// While the agent works, its progress is in the event log and its status
// says where it is, what it is doing and what it has changed; a waiter for
// events wakes on them, not only when the turn ends.
func TestStatusWhileRunning(t *testing.T) {
	f := newFixture(t, nil)
	progressDef(f)
	f.cwd = gitRepo(t)
	gate := filepath.Join(t.TempDir(), "gate")
	f.profile("worker", "harness: stream\neffect: idempotent\ntimeout: 5m\nretry: transient x3\nmode: progress\nopts: gate="+gate)

	var wg sync.WaitGroup
	var rep Report
	started := make(chan *Run, 1)
	wg.Go(func() {
		reps, err := f.r.Start(context.Background(), "worker", []string{"fix it"}, f.cwd, TurnOptions{}, func(r *Run) { started <- r })
		if err != nil {
			t.Error(err)
			close(started)
			return
		}
		rep = reps[0]
	})
	defer wg.Wait()
	defer os.WriteFile(gate, nil, 0o644) // never leave the agent waiting
	run := <-started
	if run == nil {
		return
	}

	// Follow the log until the agent has said something.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var seen []Event
	var cursor int64
	for !slices.ContainsFunc(seen, func(e Event) bool { return e.Kind == EvSay }) {
		evs, next, done, err := f.r.WaitEvents(ctx, run.ID, cursor)
		if err != nil || done {
			t.Fatalf("waiting for progress: done=%v err=%v, seen %v", done, err, kinds(seen))
		}
		if ctx.Err() != nil {
			t.Fatalf("no progress events; seen %v", kinds(seen))
		}
		seen, cursor = append(seen, evs...), next
	}
	tools := slices.DeleteFunc(slices.Clone(seen), func(e Event) bool { return e.Kind != EvTool })
	if len(tools) != 2 || tools[0].Name != "edit" || tools[0].Text != "a.go" || tools[1].Name != "bash" || tools[1].Text != "go test ./..." {
		t.Fatalf("tool events: %+v", tools)
	}

	st, err := f.r.Status(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "running" || st.Outcome != "running" || st.Attempt != 1 || st.MaxAttempts != 3 || st.Timeout != 5*time.Minute || !st.Live {
		t.Fatalf("status: %+v", st)
	}
	if st.Said != "fixing the race" || len(st.Activity) != 3 || st.LastOutput.IsZero() || st.AttemptAt.IsZero() {
		t.Fatalf("activity: said %q, %+v", st.Said, st.Activity)
	}
	if st.Metrics["turns"] != 1 {
		t.Fatalf("live metrics: %v", st.Metrics)
	}
	if ch := st.Changes; ch == nil || ch.Scope != "folder" || ch.New != 1 || !slices.Contains(ch.Files, "touched.txt") || ch.Summary() != "1 new" {
		t.Fatalf("changes: %+v", st.Changes)
	}
	if next := st.Next(); len(next) == 0 || !strings.HasPrefix(next[0], "harness wait "+run.ID) {
		t.Fatalf("next: %v", next)
	}

	os.WriteFile(gate, nil, 0o644)
	wg.Wait()
	if rep.Err != nil || rep.Output != "answer: fix it" {
		t.Fatalf("report: %+v", rep)
	}
	st, err = f.r.Status(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "success" || st.Said != "done" || st.Metrics["turns"] != 2 {
		t.Fatalf("finished status: phase %s said %q metrics %v", st.Phase, st.Said, st.Metrics)
	}
	if next := st.Next(); len(next) != 2 || next[0] != "harness result "+run.ID || !strings.HasPrefix(next[1], "harness send "+run.ID) {
		t.Fatalf("next: %v", next)
	}
	// Waiting for events after the end returns at once, done.
	evs, _, done, err := f.r.WaitEvents(context.Background(), run.ID, cursor)
	if err != nil || !done || evs[len(evs)-1].Kind != EvFinished {
		t.Fatalf("after the end: done=%v err=%v %v", done, err, kinds(evs))
	}
}

// A waiter for events that sees nothing new before its context ends says
// so, and does not claim the turn ended.
func TestWaitEventsTimesOut(t *testing.T) {
	f := newFixture(t, nil)
	progressDef(f)
	gate := filepath.Join(t.TempDir(), "gate")
	f.profile("worker", "harness: stream\neffect: idempotent\nmode: progress\nopts: gate="+gate)
	runs, err := f.r.Prepare("worker", []string{"x"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var wg sync.WaitGroup
	wg.Go(func() { f.r.Execute(context.Background(), id, 1) })
	defer wg.Wait()
	defer os.WriteFile(gate, nil, 0o644)
	// Read up to the agent's last message before the gate.
	var cursor int64
	for deadline := time.Now().Add(20 * time.Second); ; {
		evs, next, _ := f.home.Events(id, cursor)
		cursor = next
		if slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == EvSay }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	evs, next, done, err := f.r.WaitEvents(ctx, id, cursor)
	if err != nil || done || len(evs) != 0 || next != cursor {
		t.Fatalf("timed out wait: %v done=%v next=%d cursor=%d err=%v", evs, done, next, cursor, err)
	}
}

// A queued turn whose process never came, and a running one whose process
// died, are recorded as lost when someone looks, with an event.
func TestStatusReapsLostTurn(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("echo", []string{"x"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	if _, err := f.home.updateRun(id, func(r *Run) error {
		r.Turns[0].Outcome, r.Turns[0].Started = "running", time.Now().UTC() // its process is gone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := f.r.Status(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Outcome != "uncertain" || st.Phase != "uncertain" {
		t.Fatalf("status: %+v", st)
	}
	evs, _, _ := f.home.Events(id, 0)
	if last := evs[len(evs)-1]; last.Kind != EvLost || last.Name != "uncertain" {
		t.Fatalf("events: %+v", evs)
	}
}

// Progress rules are checked when the definition is saved.
func TestProgressRuleValidation(t *testing.T) {
	answer := `"answer": {"where": {"type": "done"}, "field": "text"}`
	for _, tc := range []struct{ output, want string }{
		{`{"format": "text", "progress": [{"kind": "tool", "where": {"type": "t"}, "field": "name"}]}`, "progress needs jsonl"},
		{`{"format": "jsonl", ` + answer + `, "progress": [{"kind": "think", "where": {"type": "t"}, "field": "x"}]}`, "kind must be"},
		{`{"format": "jsonl", ` + answer + `, "progress": [{"kind": "tool", "field": "name"}]}`, "where is required"},
		{`{"format": "jsonl", ` + answer + `, "progress": [{"kind": "say", "where": {"type": "m"}}]}`, "field is required"},
	} {
		_, err := ParseDefinition("x", []byte(`{"command": "x", "output": `+tc.output+`}`))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.output, err, tc.want)
		}
	}
	ok := `{"format": "jsonl", ` + answer + `, "progress": [{"kind": "step", "where": {"type": "turn_end"}}]}`
	if _, err := ParseDefinition("x", []byte(`{"command": "x", "output": `+ok+`}`)); err != nil {
		t.Fatal(err)
	}
}

// A reader never sees half a line, and skips what is not an event.
func TestReadEventsCompleteLinesOnly(t *testing.T) {
	dir := t.TempDir()
	noteAt(dir, Event{Turn: 1, Kind: EvQueued, Text: strings.Repeat("x", 1000)})
	f, _ := os.OpenFile(filepath.Join(dir, eventsFile), os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("not json\n" + `{"format":99,"kind":"future","turn":1}` + "\n" + `{"format":1,"kind":"started","tu`)
	f.Close()
	evs, next, err := readEvents(dir, 0)
	if err != nil || len(evs) != 1 || evs[0].Kind != EvQueued || len([]rune(evs[0].Text)) != maxEventText {
		t.Fatalf("%+v %v", evs, err)
	}
	f, _ = os.OpenFile(filepath.Join(dir, eventsFile), os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`rn":1}` + "\n")
	f.Close()
	evs, _, _ = readEvents(dir, next)
	if len(evs) != 1 || evs[0].Kind != EvStarted {
		t.Fatalf("the finished line: %+v", evs)
	}
}
