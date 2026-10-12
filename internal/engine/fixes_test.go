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

// Counting from many goroutines at once, with cleanup running, loses
// nothing: one lock guards every record. Each lockOpen is its own open
// file, and the locks belong to the open file, so goroutines contend for
// it as processes do.
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

// Round three of the review.

// A counted turn whose end is observed again, after an identical request
// succeeded and cleared the failures, is not counted again.
func TestCountedTurnNotRecounted(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("bad", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=boom")
	rep := f.start(context.Background(), "bad", "x")[0]
	fp := runFingerprint(t, f, "bad", "x")
	if len(f.home.failures(fp)) != 1 {
		t.Fatal("failure not counted")
	}
	// Its final save is lost: the record still says running.
	f.home.updateRun(rep.Run.ID, func(run *Run) error {
		run.Turns[0].Outcome, run.Turns[0].Ended = "running", time.Time{}
		return nil
	})
	f.home.noteRepeat(fp, "other/1", "success") // an identical request succeeded
	if _, done, err := f.r.WaitTurn(context.Background(), rep.Run.ID, 1); !done || err != nil {
		t.Fatalf("reap: %v %v", done, err)
	}
	if n := len(f.home.failures(fp)); n != 0 {
		t.Fatalf("the turn was counted again: %d", n)
	}
}

// gc, even with a cutoff in the future, never removes a run being
// created or one whose turn waits for its worker.
func TestGCSparesRunsTakingShape(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	future := time.Now().Add(time.Hour)
	defer func() { testHookCreate = nil }()
	testHookCreate = func(stage string) error {
		if stage == "record" { // the folder exists, its record does not yet
			if rep, err := f.home.GC(future, false); err != nil || len(rep.Runs) != 0 {
				t.Errorf("gc during creation: %+v %v", rep, err)
			}
		}
		return nil
	}
	runs, err := f.r.Prepare("ok", []string{"x"}, f.cwd, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	testHookCreate = nil
	if rep, err := f.home.GC(future, false); err != nil || len(rep.Runs) != 0 {
		t.Fatalf("gc of a queued run: %+v %v", rep, err)
	}
	if _, err := f.home.LoadRun(runs[0].ID); err != nil {
		t.Fatal(err)
	}
	// A staging folder is removed only once its creator is gone, however
	// old it is.
	runsDir := filepath.Join(f.home.Dir, "runs")
	live, crashed := filepath.Join(runsDir, stagePrefix+"live"), filepath.Join(runsDir, stagePrefix+"crashed")
	os.Mkdir(live, 0o700)
	os.Mkdir(crashed, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(live, old, old)
	lk, err := lockOpen(live + ".lock") // a creator paused for two days
	if err != nil {
		t.Fatal(err)
	}
	f.home.GC(time.Now().AddDate(0, 0, -30), false)
	if _, err := os.Stat(live); err != nil {
		t.Fatal("a live creation was removed")
	}
	if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a crashed creation was kept")
	}
	lk.Close() // the creator dies
	f.home.GC(time.Now().AddDate(0, 0, -30), false)
	if _, err := os.Stat(live); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an abandoned creation was kept")
	}
	if _, err := os.Stat(live + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("its lock file was kept")
	}
}

// Round four of the review.

// The turn that was reaped is counted, even when another turn has been
// queued after it in the meantime.
func TestReapedTurnCountedByNumber(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "ok", "x")[0] // a session to send to
	n, err := f.r.PrepareSend(rep.Run.ID, "y", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.home.updateRun(rep.Run.ID, func(run *Run) error {
		run.Turns[n-1].Outcome = "running" // its worker died
		f.r.reapLost(run)
		return nil
	})
	// Another caller queues the next turn before the reaped one is counted.
	if _, err := f.r.PrepareSend(rep.Run.ID, "z", nil); err != nil {
		t.Fatal(err)
	}
	f.r.countReaped(rep.Run.ID, n)
	p, d, _ := f.r.load("ok", f.cwd)
	if got := len(f.home.failures(requestFingerprint("send", rep.Run.ID, p, d, f.cwd, "y", nil))); got != 1 {
		t.Fatalf("reaped turn counted %d times", got)
	}
}

// Ids that collide with a published run or a creation in progress are
// skipped, never replaced.
func TestRunIDCollisions(t *testing.T) {
	h := newGateHome(t)
	defer func() { runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) } }()
	ids := []string{"261012-000000-aaaa", "261012-000000-aaaa", "261012-000000-bbbb", "261012-000000-cccc"}
	runID = func() string { id := ids[0]; ids = ids[1:]; return id }
	first, err := h.NewRun("p", "h", ".", "")
	if err != nil || first.ID != "261012-000000-aaaa" {
		t.Fatal(first, err)
	}
	lk, _ := lockOpen(filepath.Join(h.Dir, "runs", stagePrefix+"261012-000000-bbbb.lock")) // being created
	defer lk.Close()
	second, err := h.NewRun("q", "h", ".", "")
	if err != nil || second.ID != "261012-000000-cccc" {
		t.Fatalf("got %v %v, want the third id", second, err)
	}
	if r, err := h.LoadRun("261012-000000-aaaa"); err != nil || r.Profile != "p" {
		t.Fatal("the first run was replaced")
	}
}

// Round five of the review.

// Taking a creation lock and removing a lock file never interleave: a
// creator waits while cleanup holds the coordination lock.
func TestCreationWaitsForCleanup(t *testing.T) {
	h := newGateHome(t)
	unlock, err := h.creationLock() // cleanup, between unlocking and removing a lock file
	if err != nil {
		t.Fatal(err)
	}
	created := make(chan error, 1)
	go func() {
		_, err := h.NewRun("p", "h", ".", "")
		created <- err
	}()
	select {
	case err := <-created:
		t.Fatalf("a run was created during cleanup: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, "create.lock")); err != nil {
		t.Fatal("the coordination lock file was removed")
	}
}

// After cleanup removes an abandoned creation, a creator reusing its id
// holds a lock that another cleanup respects.
func TestReusedCreationIDIsProtected(t *testing.T) {
	h := newGateHome(t)
	runs := filepath.Join(h.Dir, "runs")
	id := "261012-000000-dddd"
	stage := filepath.Join(runs, stagePrefix+id)
	os.Mkdir(stage, 0o700) // abandoned: its lock file exists, unlocked
	os.WriteFile(stage+".lock", nil, 0o600)
	h.collectStage(stagePrefix + id)
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned creation kept")
	}
	defer func() {
		testHookCreate = nil
		runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) }
	}()
	runID = func() string { return id }
	testHookCreate = func(s string) error {
		if s == "record" { // the creator holds its lock and its folder exists
			h.collectStage(stagePrefix + id)
			h.collectStage(stagePrefix + id + ".lock")
			if _, err := os.Stat(stage); err != nil {
				t.Error("a live creation was removed")
			}
		}
		return nil
	}
	if r, err := h.NewRun("p", "h", ".", ""); err != nil || r.ID != id {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := os.Stat(stage + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the creation lock file was left behind")
	}
}
