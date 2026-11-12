package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// An agent asks from inside its run and waits; its caller's wait wakes on
// the question, status shows it, and the reply reaches the agent, which
// goes on. Waiting for the answer does not count as idle.
func TestAskReply(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("asker", "harness: fake\neffect: idempotent\nidle: 600ms\nmode: ask\nopts: home="+f.home.Dir)
	runs, err := f.r.Prepare("asker", []string{"go"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var rep Report
	var wg sync.WaitGroup
	wg.Go(func() { rep = f.r.Execute(context.Background(), id, 1) })
	defer wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, finished, alerts, err := f.r.WaitAttention(ctx, id)
	if err != nil || finished || len(alerts) != 1 || alerts[0].Kind != EvAsk || alerts[0].Text != "use v1 or v2?" {
		t.Fatalf("finished=%v alerts=%+v err=%v", finished, alerts, err)
	}
	time.Sleep(time.Second) // longer than the idle limit: an ask is not idleness
	st, err := f.r.Status(context.Background(), id)
	if err != nil || st.Phase != "running" || len(st.Asks) != 1 || !strings.HasPrefix(st.Next()[0], "harness reply "+id+" "+alerts[0].Name) {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, err := f.home.Reply(id, "q-nope", "x"); err == nil {
		t.Fatal("replied to a question that does not exist")
	}
	if qid, err := f.home.Reply(id, "", "v2, and keep v1 working"); err != nil || qid != alerts[0].Name {
		t.Fatalf("reply: %s %v", qid, err)
	}
	wg.Wait()
	if rep.Err != nil || rep.Output != "answer: v2, and keep v1 working" {
		t.Fatalf("report: %+v", rep)
	}
	if _, err := f.home.Reply(id, "", "again"); err == nil || !strings.Contains(err.Error(), "no open question") {
		t.Fatalf("second reply: %v", err)
	}
}

// An ask whose wait ends unanswered is closed: the agent goes on, and it
// can no longer be answered.
func TestAskExpires(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("asker", "harness: fake\neffect: idempotent\nmode: ask\nopts: home="+f.home.Dir+" wait=300ms")
	rep := f.start(context.Background(), "asker", "go")[0]
	if rep.Err != nil || !strings.HasPrefix(rep.Output, "no answer to q-") {
		t.Fatalf("report: %+v", rep)
	}
	evs, _, _ := f.home.Events(rep.Run.ID, 0)
	if !hasEvent(evs, EvExpired, "") || len(openAsks(evs)) != 0 {
		t.Fatalf("events: %v", kinds(evs))
	}
	if _, err := f.home.Reply(rep.Run.ID, "", "late"); err == nil {
		t.Fatal("answered an expired question")
	}
}

// What is posted to an agent reaches it once: through its inbox while it
// works, or else at the top of its next turn.
func TestSteerDelivery(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("reader", "harness: fake\neffect: pure\nmode: inbox\nopts: home="+f.home.Dir)
	runs, err := f.r.Prepare("reader", []string{"start"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	if _, err := f.home.Steer(id, "use tabs"); err != nil {
		t.Fatal(err)
	}
	if rep := f.r.Execute(context.Background(), id, 1); rep.Output != "inbox: [use tabs] prompt: start" {
		t.Fatalf("turn 1: %q %v", rep.Output, rep.Err)
	}
	// Read in turn 1: not delivered again.
	rep, err := f.r.Send(context.Background(), id, "more", TurnOptions{})
	if err != nil || rep.Output != "inbox: [] prompt: more" {
		t.Fatalf("turn 2: %q %v", rep.Output, err)
	}
	// Not read by the agent: it leads the next turn, once.
	f.home.Steer(id, "also add tests")
	f.profile("reader", "harness: fake\neffect: pure\nmode: echo")
	rep, _ = f.r.Send(context.Background(), id, "go on", TurnOptions{})
	if !strings.Contains(rep.Output, "sent while you worked:\n- also add tests\n\ngo on") {
		t.Fatalf("turn 3: %q", rep.Output)
	}
	evs, _, _ := f.home.Events(id, 0)
	if !hasEvent(evs, EvDelivery, "") {
		t.Fatalf("events: %v", kinds(evs))
	}
	if rep, _ = f.r.Send(context.Background(), id, "last", TurnOptions{}); rep.Output != "echo: last" {
		t.Fatalf("turn 4: %q", rep.Output)
	}
}

// steer --now stops the running turn and continues the session with the
// message.
func TestSteerNow(t *testing.T) {
	f := newFixture(t, nil)
	progressDef(f)
	gate := filepath.Join(t.TempDir(), "gate")
	f.profile("w", "harness: stream\neffect: idempotent\nmode: progress\nopts: gate="+gate)
	runs, err := f.r.Prepare("w", []string{"first"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var wg sync.WaitGroup
	var first Report
	wg.Go(func() { first = f.r.Execute(context.Background(), id, 1) })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if evs, _, _ := f.home.Events(id, 0); hasEvent(evs, EvSay, "") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress")
		}
	}
	n, err := f.r.SteerNow(context.Background(), id, "stop that; fix the parser instead")
	if err != nil || n != 2 {
		t.Fatalf("steer now: %d %v", n, err)
	}
	wg.Wait()
	if first.Outcome != "cancelled" {
		t.Fatalf("turn 1: %s", first.Outcome)
	}
	os.WriteFile(gate, nil, 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep, done, err := f.r.WaitTurn(ctx, id, 2)
	if err != nil || !done || rep.Err != nil || !strings.Contains(rep.Output, "fix the parser instead") || !strings.Contains(rep.Output, "carry on") {
		t.Fatalf("turn 2: %q %v %v", rep.Output, rep.Err, err)
	}
}

// The agent is told how to reach its caller, unless the profile says not
// to; and only an agent inside a run can note, ask or read its inbox.
func TestIntercomNote(t *testing.T) {
	f := newFixture(t, nil)
	f.def("instr", Definition{Args: [][]string{{"echo"}}, Stdin: "system+prompt", Output: OutputRule{Format: "text"}})
	f.profile("on", "harness: instr\neffect: pure")
	rep := f.start(context.Background(), "on", "hi")[0]
	if !strings.Contains(rep.Output, "You are running as harness run "+rep.Run.ID) || !strings.Contains(rep.Output, "harness ask") {
		t.Fatalf("with the note: %q", rep.Output)
	}
	f.profile("off", "harness: instr\neffect: pure\nintercom: false")
	if rep := f.start(context.Background(), "off", "hi")[0]; strings.Contains(rep.Output, "harness run") {
		t.Fatalf("intercom false: %q", rep.Output)
	}
	t.Setenv(EnvRun, "")
	t.Setenv(EnvLineage, "")
	if _, err := f.home.Note("x"); err == nil || !strings.Contains(err.Error(), "not running inside a harness run") {
		t.Fatalf("note outside a run: %v", err)
	}
}
