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
	for range 2 {
		f.home.noteRepeat(fp, "permanent")
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
		h.noteRepeat("fp", "permanent")
	}
	if n := len(h.failures("fp")); n != 2 {
		t.Fatalf("failures within the hour: %d, want 2", n)
	}
	if err := h.checkRepeat("fp"); err != nil {
		t.Fatalf("refused with two failures in the hour: %v", err)
	}
	now = func() time.Time { return start.Add(4 * time.Hour) }
	h.noteRepeat("other", "permanent")
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
	calls := 0
	testHookCreate = func() error {
		if calls++; calls == 2 {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	defer func() { testHookCreate = nil }()
	if _, err := f.r.Compare(context.Background(), []string{"a", "b"}, "q", f.cwd, nil, "", nil); err == nil {
		t.Fatal("failed creation reported success")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
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
