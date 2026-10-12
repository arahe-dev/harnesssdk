package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the review of v1.6.0.

// runFingerprint is the fingerprint of a top-level run of profile with
// prompt, as Prepare computes it.
func runFingerprint(t *testing.T, f *fixture, profile, prompt string) string {
	t.Helper()
	p, d, err := f.r.load(profile, f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	return requestFingerprint("run", "", p, d, f.cwd, prompt, nil)
}

// A where value with a character JSON escapes briefly is spelled
// differently in the raw line; the prefilter must not skip it.
func TestPrefilterKeepsEscapedValues(t *testing.T) {
	for _, esc := range []struct{ raw, value string }{
		{`\t`, "\t"}, {`\n`, "\n"}, {`\r`, "\r"}, {`\b`, "\b"}, {`\f`, "\f"}, {`\"`, `"`}, {`\\`, `\`}, {`\/`, "/"},
	} {
		d := &Definition{Output: OutputRule{Format: "jsonl",
			Answer: &Selector{Where: map[string]string{"type": "a" + esc.value + "b"}, Field: "v"}}}
		x, err := d.extract(strings.NewReader(`{"type":"a` + esc.raw + `b","v":"found"}` + "\n"))
		if err != nil || x.Answer != "found" {
			t.Errorf("where value with %s: %q %v", esc.raw, x.Answer, err)
		}
	}
}

// A metric that is not a finite number is not recorded, and the turn's
// record is still saved.
func TestNonFiniteMetrics(t *testing.T) {
	d := &Definition{Output: OutputRule{Format: "jsonl", Answer: &Selector{Where: map[string]string{"type": "a"}, Field: "v"},
		Metrics: map[string]*Selector{
			"nan":  {Where: map[string]string{"type": "m"}, Field: "s"},
			"big":  {Where: map[string]string{"type": "m"}, Field: "n", Pick: "sum"},
			"fine": {Where: map[string]string{"type": "m"}, Field: "k", Pick: "sum"},
		}}}
	stream := `{"type":"m","s":"NaN","n":1e308,"k":1}` + "\n" + `{"type":"m","s":"Inf","n":1e308,"k":2}` + "\n" + `{"type":"a","v":"ok"}` + "\n"
	x, err := d.extract(strings.NewReader(stream))
	if err != nil || x.Metrics["fine"] != 3 {
		t.Fatalf("%+v %v", x, err)
	}
	for _, k := range []string{"nan", "big"} {
		if _, ok := x.Metrics[k]; ok {
			t.Errorf("non-finite %s recorded: %v", k, x.Metrics[k])
		}
	}
	// End to end: the fake's answer is the prompt, read as a metric.
	f := newFixture(t, nil)
	f.def("fakejson", Definition{Args: [][]string{{"json"}},
		Output: OutputRule{Format: "json", Field: "result", Metrics: map[string]*Selector{"weird": {Field: "result"}}}})
	f.profile("j", "harness: fakejson\neffect: pure")
	for _, prompt := range []string{"NaN", "-Inf"} {
		rep := f.start(context.Background(), "j", prompt)[0]
		run, err := f.home.LoadRun(rep.Run.ID)
		if rep.Err != nil || err != nil || run.Turns[0].Outcome != "success" {
			t.Fatalf("%s: report %v, record %v %+v", prompt, rep.Err, err, run)
		}
		if _, ok := run.Turns[0].Metrics["weird"]; ok {
			t.Fatalf("%s recorded", prompt)
		}
	}
}

// Editing a refused profile makes a new request.
func TestRepeatGuardFollowsProfile(t *testing.T) {
	f := newFixture(t, nil)
	f.home.SetConfig("repeat_refuse", 2)
	f.profile("bad", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=boom")
	for range 2 {
		f.start(context.Background(), "bad", "same")
	}
	if _, err := f.r.Start(context.Background(), "bad", []string{"same"}, f.cwd, nil, "", nil); !errors.Is(err, ErrRepeated) {
		t.Fatalf("not refused: %v", err)
	}
	f.profile("bad", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=other")
	if _, err := f.r.Start(context.Background(), "bad", []string{"same"}, f.cwd, nil, "", nil); err != nil {
		t.Fatalf("edited profile refused: %v", err)
	}
}

// A turn queued before its request reached the limit is refused when it
// executes, and the refusal is not counted as another failure.
func TestQueuedTurnRefusedAtExecution(t *testing.T) {
	f := newFixture(t, nil)
	f.home.SetConfig("repeat_refuse", 2)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("ok", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	fp := runFingerprint(t, f, "ok", "x")
	for i := range 2 {
		f.home.noteRepeat(fp, fmt.Sprint("other/", i), "permanent")
	}
	rep := f.r.Execute(context.Background(), runs[0].ID, 1)
	if !errors.Is(rep.Err, ErrRepeated) || rep.Outcome != "permanent" {
		t.Fatalf("%s %v", rep.Outcome, rep.Err)
	}
	if n := len(f.home.failures(fp)); n != 2 {
		t.Fatalf("refusal counted: %d failures", n)
	}
}

// Failures older than an hour drop out one by one.
func TestRepeatWindowSlides(t *testing.T) {
	h := newGateHome(t)
	h.SetConfig("repeat_refuse", 3)
	start := time.Now()
	defer func() { now = time.Now }()
	for _, at := range []time.Duration{0, 50 * time.Minute, 100 * time.Minute} {
		now = func() time.Time { return start.Add(at) }
		h.noteRepeat("fp", at.String(), "permanent")
	}
	if n := len(h.failures("fp")); n != 2 {
		t.Fatalf("failures within the hour: %d, want 2", n)
	}
	if err := h.checkRepeat("fp"); err != nil {
		t.Fatalf("refused with two failures in the hour: %v", err)
	}
	now = func() time.Time { return start.Add(4 * time.Hour) }
	h.noteRepeat("other", "r/1", "permanent")
	h.pruneRepeats()
	if _, err := os.Stat(h.repeatPath("fp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired record kept")
	}
	if _, err := os.Stat(h.repeatPath("other")); err != nil {
		t.Fatal("current record pruned")
	}
	if _, err := os.Stat(h.repeatLock()); err != nil {
		t.Fatal("the lock file was pruned")
	}
}

// A turn that is found lost counts as a failure of its request.
func TestLostTurnCounts(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	runs, err := f.r.Prepare("ok", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// A worker that started the turn and died.
	if _, err := f.home.updateRun(runs[0].ID, func(run *Run) error {
		run.Turns[0].Outcome = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rep, done, err := f.r.WaitTurn(context.Background(), runs[0].ID, 1)
	if err != nil || !done || rep.Outcome != "uncertain" {
		t.Fatalf("%+v %v %v", rep, done, err)
	}
	if n := len(f.home.failures(runFingerprint(t, f, "ok", "x"))); n != 1 {
		t.Fatalf("lost turn counted %d times", n)
	}
}

// Compare refuses before creating anything when one profile's policy is
// invalid, and removes what it created when creation fails part way.
func TestCompareIsAllOrNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("a", "harness: fake\neffect: pure\nmode: echo")
	// A file written outside harness, with a policy SaveProfile refuses: an
	// action that may have edited files must not be retried blind.
	unsafe := "---\nharness: fake\neffect: non-idempotent\nretry: uncertain x2\nmode: echo\n---\nx\n"
	if err := os.WriteFile(filepath.Join(f.home.Dir, "profiles", "unsafe.md"), []byte(unsafe), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Compare(context.Background(), []string{"a", "unsafe"}, "q", f.cwd, nil, "", nil); err == nil {
		t.Fatal("compare with an unsafe policy accepted")
	}
	f.profile("b", "harness: fake\neffect: pure\nmode: echo")
	defer func() { testHookCreate = nil }()
	// Fail each stage of creating the second run, after the first run is
	// complete: the record's own save, the queued turn, the final save.
	for _, stage := range []string{"record", "queue", "save"} {
		seen := 0
		testHookCreate = func(s string) error {
			if s == stage {
				if seen++; seen == 2 {
					return fmt.Errorf("disk full at %s", s)
				}
			}
			return nil
		}
		if _, err := f.r.Compare(context.Background(), []string{"a", "b"}, "q", f.cwd, nil, "", nil); err == nil {
			t.Fatalf("%s: failed creation reported success", stage)
		}
		if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
			t.Fatalf("%s: %d runs left behind", stage, len(entries))
		}
	}
}

// A run id spells local time without a zone; stats must count a run whose
// id reads as older than it is.
func TestStatsAcrossZones(t *testing.T) {
	h := newGateHome(t)
	created := time.Now().UTC()
	id := created.Add(-14*time.Hour).In(time.Local).Format("060102-150405") + "-zone"
	dir := filepath.Join(h.Dir, "runs", id)
	os.Mkdir(dir, 0o700)
	r := &Run{Format: formatVersion, ID: id, Profile: "p", Harness: "fake", Created: created, dir: dir,
		Turns: []TurnRecord{{N: 1, Started: created, Ended: created.Add(time.Second), Outcome: "success"}}}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := h.Stats(StatsFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil || len(st) != 1 || st[0].Turns != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	// And a run created before the window is still left out.
	r.Created = created.Add(-2 * time.Hour)
	r.Save()
	if st, _ := h.Stats(StatsFilter{Since: time.Now().Add(-time.Hour)}); len(st) != 0 {
		t.Fatalf("old run counted: %+v", st)
	}
}

// Round two of the review.

// A turn queued under one profile and executed after the profile was
// edited is checked and counted as what it runs.
func TestQueuedTurnFollowsEditedProfile(t *testing.T) {
	f := newFixture(t, nil)
	f.home.SetConfig("repeat_refuse", 2)
	f.profile("p", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=a")
	runs, err := f.r.Prepare("p", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	before := runFingerprint(t, f, "p", "x")
	for i := range 2 {
		f.home.noteRepeat(before, fmt.Sprint("other/", i), "permanent")
	}
	f.profile("p", "harness: fake\neffect: pure\nmode: echo") // the fix
	after := runFingerprint(t, f, "p", "x")
	rep := f.r.Execute(context.Background(), runs[0].ID, 1)
	if rep.Err != nil {
		t.Fatalf("edited profile refused under its old identity: %v", rep.Err)
	}
	f.profile("p", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=b")
	runs, _ = f.r.Prepare("p", []string{"x"}, f.cwd, nil, "")
	f.r.Execute(context.Background(), runs[0].ID, 1)
	third := runFingerprint(t, f, "p", "x")
	if len(f.home.failures(before)) != 2 || len(f.home.failures(after)) != 0 || len(f.home.failures(third)) != 1 {
		t.Fatalf("failures: before %d, after %d, third %d", len(f.home.failures(before)), len(f.home.failures(after)), len(f.home.failures(third)))
	}
	if run, _ := f.home.LoadRun(runs[0].ID); run.Turns[0].Repeat != third {
		t.Fatal("the executed fingerprint was not recorded")
	}
}

// Two identical turns behind a budget of one: the first fails and is
// counted before it releases the budget, so the second, admitted after it,
// is refused.
func TestRepeatCheckedAfterAdmission(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.home.SetConfig("repeat_refuse", 1)
	f.profile("bad", "harness: fake\neffect: pure\nbudget: b=1\nmode: fail\nopts: code=3 msg=boom")
	reps := f.start(context.Background(), "bad", "same", "same")
	refused := 0
	for _, rep := range reps {
		if errors.Is(rep.Err, ErrRepeated) {
			refused++
		} else if rep.Outcome != "permanent" {
			t.Fatalf("%s %v", rep.Outcome, rep.Err)
		}
	}
	if refused != 1 || len(f.home.failures(runFingerprint(t, f, "bad", "same"))) != 1 {
		t.Fatalf("refused %d, failures %d", refused, len(f.home.failures(runFingerprint(t, f, "bad", "same"))))
	}
}

// A lost turn is counted once, however often it is observed, even when the
// follow-up that found it is refused.
func TestLostTurnCountedOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.def("nosession", Definition{Args: [][]string{{"echo"}}, Output: OutputRule{Format: "text"}})
	f.profile("ns", "harness: nosession\neffect: pure")
	for _, lost := range []func(*TurnRecord){
		func(r *TurnRecord) { r.Outcome = "running" },                           // its worker died
		func(r *TurnRecord) { r.Queued = time.Now().Add(-2 * lostAfter).UTC() }, // no worker started
	} {
		runs, err := f.r.Prepare("ns", []string{"x"}, f.cwd, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		f.home.updateRun(runs[0].ID, func(run *Run) error {
			lost(&run.Turns[0])
			return nil
		})
		for range 3 { // refused each time: the run has no session
			if _, err := f.r.PrepareSend(runs[0].ID, "more", nil); err == nil || !strings.Contains(err.Error(), "no session") {
				t.Fatalf("send: %v", err)
			}
			f.r.WaitTurn(context.Background(), runs[0].ID, 1)
		}
		run, _ := f.home.LoadRun(runs[0].ID)
		if !run.Turns[0].finished() {
			t.Fatalf("lost turn not recorded as ended: %s", run.Turns[0].Outcome)
		}
	}
	if n := len(f.home.failures(runFingerprint(t, f, "ns", "x"))); n != 2 {
		t.Fatalf("two lost turns counted %d times", n)
	}
}

// Counting from many processes at once, with cleanup running, loses
// nothing: one lock guards every record.
func TestRepeatCountingIsAtomic(t *testing.T) {
	h := newGateHome(t)
	h.SetConfig("repeat_refuse", 0)
	h.SetConfig("repeat_warn", 1)
	done := make(chan bool)
	for i := range 20 {
		go func() {
			h.noteRepeat("fp", fmt.Sprint("r/", i), "permanent")
			h.pruneRepeats()
			done <- true
		}()
	}
	for range 20 {
		<-done
	}
	if n := len(h.failures("fp")); n != 20 {
		t.Fatalf("counted %d of 20 failures", n)
	}
}
