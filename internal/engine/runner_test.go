package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake harness CLI: with HARNESS_FAKE=1 it runs
// fakeHarness instead of the tests, so end-to-end tests spend no tokens.
func TestMain(m *testing.M) {
	if code, ok := Subcommand(); ok {
		os.Exit(code)
	}
	if os.Getenv("HARNESS_FAKE") == "1" {
		os.Exit(fakeHarness(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeHarness: <mode> [key=value ...] [--sid id] [--resume id]
func fakeHarness(args []string) int {
	mode, opts, flags := args[0], map[string]string{}, []string{}
	for i := 1; i < len(args); i++ {
		if k, v, ok := strings.Cut(args[i], "="); ok && !strings.HasPrefix(args[i], "-") {
			opts[k] = v
			continue
		}
		flags = append(flags, args[i])
	}
	in, _ := io.ReadAll(os.Stdin)
	ms, _ := strconv.Atoi(opts["ms"])
	switch mode {
	case "usage":
		// Two model calls reporting usage, then the answer as the last part
		// of the final message; with a fresh state file, one call and a
		// transient failure first.
		step := `{"type":"step","usage":{"input":100,"cache":20},"cost":0.25}`
		fmt.Println(step)
		if opts["state"] != "" {
			if _, err := os.Stat(opts["state"]); err != nil {
				os.WriteFile(opts["state"], nil, 0o644)
				fmt.Fprintln(os.Stderr, "read ECONNRESET")
				return 1
			}
		}
		fmt.Println(step)
		fmt.Printf(`{"type":"done","content":[{"type":"thinking"},{"type":"text","text":"answer: %s"}]}`+"\n", strings.TrimSpace(string(in)))
	case "echo":
		fmt.Printf("echo: %s\n", in)
	case "args":
		fmt.Printf("flags: %s\n", strings.Join(flags, " "))
	case "json":
		out, _ := json.Marshal(map[string]any{"result": string(in), "session_id": "cap-123", "is_error": false})
		fmt.Println(string(out))
	case "jsonerr":
		fmt.Println(`{"result":"model refused","is_error":true}`)
	case "jsonfailsess":
		fmt.Println(`{"is_error":true,"session_id":"s-9","error":"crashed"}`)
	case "flakythenfail":
		if _, err := os.Stat(opts["state"]); err != nil {
			os.WriteFile(opts["state"], nil, 0o644)
			fmt.Fprintln(os.Stderr, "read ECONNRESET")
			return 1
		}
		fmt.Fprintln(os.Stderr, "boom")
		return 3
	case "fail":
		fmt.Fprintln(os.Stderr, opts["msg"])
		code, _ := strconv.Atoi(opts["code"])
		return code
	case "flaky":
		if _, err := os.Stat(opts["state"]); err != nil {
			os.WriteFile(opts["state"], nil, 0o644)
			fmt.Fprintln(os.Stderr, "read ECONNRESET")
			return 1
		}
		fmt.Println("ok after retry")
	case "sleep":
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Println("slept")
	case "nested":
		// Act as a harness command run by the agent; with strip=1, as one
		// whose harness removed harness's environment variables.
		if opts["strip"] == "1" {
			os.Unsetenv(EnvRun)
			os.Unsetenv(EnvLineage)
		}
		h, err := NewHome(opts["home"])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		p := h.DiscoverParentage()
		fmt.Printf("parent=%s lineage=%s found=%s\n", p.Parent, strings.Join(p.Lineage, ","), p.Found)
	case "sleeppid":
		os.WriteFile(opts["pidfile"], []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Println("slept")
	case "env":
		fmt.Println(os.Getenv("HARNESS_TEST_SECRET"))
	case "chain":
		// A helper that starts a long-lived grandchild and exits soon after.
		exe, _ := os.Executable()
		exec.Command(exe, "helper", "pidfile="+opts["pidfile"]).Start()
		time.Sleep(60 * time.Second)
	case "helper":
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep", "ms=60000")
		child.Start()
		os.WriteFile(opts["pidfile"], []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		time.Sleep(600 * time.Millisecond)
	case "orphan":
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep", "ms=60000")
		child.Start()
		os.WriteFile(opts["pidfile"], []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		fmt.Println("spawned")
	case "tree":
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep", "ms=60000")
		child.Start()
		os.WriteFile(opts["pidfile"], []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		time.Sleep(60 * time.Second)
	case "interval":
		start := time.Now().UnixNano()
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.WriteFile(filepath.Join(opts["dir"], strconv.Itoa(os.Getpid())), fmt.Appendf(nil, "%d %d", start, time.Now().UnixNano()), 0o644)
		fmt.Println("done")
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", mode)
		return 2
	}
	return 0
}

type fixture struct {
	t    *testing.T
	home *Home
	r    *Runner
	cwd  string
}

func newFixture(t *testing.T, budgets map[string]int64) *fixture {
	t.Helper()
	h, err := NewHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for n, c := range budgets {
		if err := h.SetBudget(n, c); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, home: h, cwd: t.TempDir()}
	f.def("fake", Definition{
		Args:    [][]string{{"{mode}"}, {"{*opts}"}, {"--sid", "{new_session}"}, {"--resume", "{resume_session}"}},
		Session: "mint",
		Output:  OutputRule{Format: "text"},
		Failures: []FailureRule{
			{Match: "(?i)rate limit", Outcome: "throttled"},
			{Match: "ECONNRESET", Outcome: "transient"},
		},
	})
	return f
}

func (f *fixture) def(name string, d Definition) {
	f.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	d.Command = exe
	d.Env = map[string]string{"HARNESS_FAKE": "1"}
	data, _ := json.Marshal(d)
	if _, err := f.home.SaveDefinition(name, data, ""); err != nil {
		f.t.Fatal(err)
	}
}

// profile saves a profile and starts a fresh runner.
func (f *fixture) profile(name, header string) {
	f.t.Helper()
	f.saveProfile(name, header)
	var err error
	if f.r, err = NewRunner(f.home); err != nil {
		f.t.Fatal(err)
	}
}

// saveProfile saves a profile and keeps the current runner.
func (f *fixture) saveProfile(name, header string) {
	f.t.Helper()
	p, err := ParseProfile(name, []byte("---\n"+header+"\n---\nYou are a test agent.\n"))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.home.SaveProfile(p, "", f.cwd); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) start(ctx context.Context, profile string, tasks ...string) []Report {
	f.t.Helper()
	reps, err := f.r.Start(ctx, profile, tasks, f.cwd, nil, "", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return reps
}

func TestRunEcho(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "echo", "hello")[0]
	if rep.Err != nil || rep.Outcome != "success" || rep.Output != "echo: hello" {
		t.Fatalf("%+v", rep)
	}
	run, err := f.home.LoadRun(rep.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Turns) != 1 || run.Turns[0].Outcome != "success" || run.Turns[0].Attempts != 1 {
		t.Fatalf("record: %+v", run.Turns)
	}
	if out, _ := run.Output(1); strings.TrimSpace(out) != "echo: hello" {
		t.Fatalf("saved output %q", out)
	}
	if sys, _ := os.ReadFile(run.Path("system.md")); strings.TrimSpace(string(sys)) != "You are a test agent." {
		t.Fatalf("system prompt %q", sys)
	}
}

func TestSessionMintAndResume(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("args", "harness: fake\neffect: pure\nmode: args")
	rep := f.start(context.Background(), "args", "first")[0]
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	sid := rep.Run.Session
	if len(sid) != 36 || rep.Output != "flags: --sid "+sid {
		t.Fatalf("first turn: session %q output %q", sid, rep.Output)
	}
	rep2, err := f.r.Send(context.Background(), rep.Run.ID, "second", nil)
	if err != nil || rep2.Err != nil {
		t.Fatal(err, rep2.Err)
	}
	if rep2.Turn != 2 || rep2.Output != "flags: --resume "+sid {
		t.Fatalf("second turn %d output %q", rep2.Turn, rep2.Output)
	}
}

func TestJSONOutput(t *testing.T) {
	f := newFixture(t, nil)
	f.def("jsonfake", Definition{
		Args:   [][]string{{"{mode}"}},
		Output: OutputRule{Format: "json", Field: "result", ErrorField: "is_error", SessionField: "session_id"},
	})
	f.profile("j", "harness: jsonfake\neffect: pure\nmode: json")
	rep := f.start(context.Background(), "j", "the answer")[0]
	if rep.Err != nil || rep.Output != "the answer" || rep.Run.Session != "cap-123" {
		t.Fatalf("%+v session %q", rep, rep.Run.Session)
	}
	f.profile("jerr", "harness: jsonfake\neffect: pure\nmode: jsonerr")
	rep = f.start(context.Background(), "jerr", "x")[0]
	if rep.Outcome != "permanent" || !strings.Contains(rep.Err.Error(), "model refused") {
		t.Fatalf("%+v", rep)
	}
}

func TestFailureRuleThrottled(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("limited", "harness: fake\neffect: idempotent\nretry: transient x3\nmode: fail\nopts: code=1 msg=Rate_limit_exceeded")
	f.def("fake", Definition{
		Args:     [][]string{{"{mode}"}, {"{*opts}"}},
		Output:   OutputRule{Format: "text"},
		Failures: []FailureRule{{Match: "(?i)rate.limit", Outcome: "throttled"}},
	})
	rep := f.start(context.Background(), "limited", "x")[0]
	if rep.Outcome != "throttled" || rep.Attempts != 1 || ExitCode(rep.Outcome) != ExitThrottled {
		t.Fatalf("%+v", rep)
	}
	if !strings.Contains(rep.Err.Error(), "exited with status 1: Rate_limit_exceeded") {
		t.Fatalf("error %v", rep.Err)
	}
}

func TestRetryTransient(t *testing.T) {
	f := newFixture(t, nil)
	state := filepath.Join(t.TempDir(), "state")
	f.profile("flaky", "harness: fake\neffect: idempotent\nretry: transient x2\nmode: flaky\nopts: state="+state)
	rep := f.start(context.Background(), "flaky", "x")[0]
	if rep.Err != nil || rep.Attempts != 2 || rep.Output != "ok after retry" {
		t.Fatalf("%+v", rep)
	}
}

func TestUnmatchedFailureIsPermanentAndNotRetried(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("crash", "harness: fake\neffect: idempotent\nretry: transient,uncertain x3\nmode: fail\nopts: code=3 msg=boom")
	rep := f.start(context.Background(), "crash", "x")[0]
	if rep.Outcome != "permanent" || rep.Attempts != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestTimeoutIsUncertainAndKillsTree(t *testing.T) {
	f := newFixture(t, nil)
	pidfile := filepath.Join(t.TempDir(), "pid")
	f.profile("hang", "harness: fake\neffect: non-idempotent\ntimeout: 2s\nmode: tree\nopts: pidfile="+pidfile)
	start := time.Now()
	rep := f.start(context.Background(), "hang", "x")[0]
	if rep.Outcome != "uncertain" {
		t.Fatalf("%+v", rep)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %v: the tree was not killed promptly", time.Since(start))
	}
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(data))
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCancelStopsRun(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("slow", "harness: fake\neffect: pure\nmode: sleep\nopts: ms=30000")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	rep := f.start(ctx, "slow", "x")[0]
	if rep.Outcome != "cancelled" || ExitCode(rep.Outcome) != ExitCancelled {
		t.Fatalf("%+v", rep)
	}
}

func TestBudgetLimitsFanOut(t *testing.T) {
	f := newFixture(t, map[string]int64{"fake": 1})
	dir := t.TempDir()
	f.profile("one", "harness: fake\neffect: pure\nbudget: fake=1\nmode: interval\nopts: ms=300 dir="+dir)
	reps := f.start(context.Background(), "one", "a", "b", "c")
	for _, rep := range reps {
		if rep.Err != nil {
			t.Fatal(rep.Err)
		}
	}
	if peak := peakOverlap(t, dir); peak != 1 {
		t.Fatalf("peak concurrency %d with capacity 1", peak)
	}
}

// peakOverlap reads the [start, end] intervals the fake wrote and returns
// the largest number that overlap at one instant.
func peakOverlap(t *testing.T, dir string) int {
	entries, _ := os.ReadDir(dir)
	type ev struct {
		at    int64
		delta int
	}
	var evs []ev
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		var s, en int64
		fmt.Sscanf(string(data), "%d %d", &s, &en)
		evs = append(evs, ev{s, 1}, ev{en, -1})
	}
	if len(evs) != 6 {
		t.Fatalf("want 3 intervals, got %d", len(evs)/2)
	}
	for i := range evs {
		for j := i + 1; j < len(evs); j++ {
			if evs[j].at < evs[i].at || evs[j].at == evs[i].at && evs[j].delta < evs[i].delta {
				evs[i], evs[j] = evs[j], evs[i]
			}
		}
	}
	cur, peak := 0, 0
	for _, e := range evs {
		cur += e.delta
		peak = max(peak, cur)
	}
	return peak
}

func TestUnfinishedTurnRefusesAnother(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "echo", "hi")[0]
	if _, err := f.r.PrepareSend(rep.Run.ID, "queued, never executed", nil); err != nil {
		t.Fatal(err)
	}
	_, err := f.r.PrepareSend(rep.Run.ID, "again", nil)
	if err == nil || OutcomeName(err) != "throttled" || !strings.Contains(err.Error(), "busy with turn 2") {
		t.Fatalf("want a throttled busy error, got %v", err)
	}
	run, _ := f.home.LoadRun(rep.Run.ID)
	if len(run.Turns) != 2 {
		t.Fatalf("turns: %d", len(run.Turns))
	}
}

func TestDryRun(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	plan, err := f.r.DryRun("echo", "hi", f.cwd, Vars{"opts": "a=1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Argv) != 5 || plan.Argv[1] != "echo" || plan.Argv[2] != "a=1" || plan.Argv[3] != "--sid" || !plan.Stdin {
		t.Fatalf("argv %v", plan.Argv)
	}
	if len(plan.Dropped) != 1 || plan.Dropped[0].Missing != "resume_session" {
		t.Fatalf("dropped %v", plan.Dropped)
	}
}

func TestSaveProfileNeedsDefinition(t *testing.T) {
	f := newFixture(t, nil)
	p, _ := ParseProfile("x", []byte("---\nharness: missing\neffect: pure\n---\n"))
	if err := f.home.SaveProfile(p, "", f.cwd); err == nil || !strings.Contains(err.Error(), "harness def set missing") {
		t.Fatalf("got %v", err)
	}
}

// Review finding 4: only the current attempt's own output is evidence.
func TestClassifyUsesOnlyCurrentAttempt(t *testing.T) {
	f := newFixture(t, nil)
	state := filepath.Join(t.TempDir(), "state")
	f.profile("tf", "harness: fake\neffect: idempotent\nretry: transient x5\nmode: flakythenfail\nopts: state="+state)
	rep := f.start(context.Background(), "tf", "x")[0]
	if rep.Outcome != "permanent" || rep.Attempts != 2 {
		t.Fatalf("transient then permanent: want permanent after 2 attempts, got %s after %d", rep.Outcome, rep.Attempts)
	}
	// Text that matches a rule but appears only in the command line.
	f.profile("argv", "harness: fake\neffect: idempotent\nretry: transient x3\nmode: fail\nopts: code=3 msg=boom note=ECONNRESET")
	rep = f.start(context.Background(), "argv", "x")[0]
	if rep.Outcome != "permanent" || rep.Attempts != 1 {
		t.Fatalf("argv text must not classify: got %s after %d", rep.Outcome, rep.Attempts)
	}
}

// Review finding 5: an edited profile runs under its new policy.
func TestEditedProfileUsesNewPolicy(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("p", "harness: fake\neffect: pure\nmode: echo")
	if rep := f.start(context.Background(), "p", "x")[0]; rep.Err != nil {
		t.Fatal(rep.Err)
	}
	f.saveProfile("p", "harness: fake\neffect: pure\ntimeout: 1s\nmode: sleep\nopts: ms=5000")
	if rep := f.start(context.Background(), "p", "x")[0]; rep.Outcome != "uncertain" {
		t.Fatalf("the edited profile's timeout was not applied: %+v", rep)
	}
}

// Review finding 7: a session cannot move to another harness.
func TestSendRejectsHarnessChange(t *testing.T) {
	f := newFixture(t, nil)
	f.def("other", Definition{Args: [][]string{{"{mode}"}}, Output: OutputRule{Format: "text"}})
	f.profile("p", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "p", "x")[0]
	f.saveProfile("p", "harness: other\neffect: pure\nmode: echo")
	if _, err := f.r.Send(context.Background(), rep.Run.ID, "y", nil); err == nil || !strings.Contains(err.Error(), "cannot move between harnesses") {
		t.Fatalf("got %v", err)
	}
}

// Review finding 8: values from outside cannot set built-in placeholders.
func TestExtraCannotSetBuiltins(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("args", "harness: fake\neffect: pure\nmode: args")
	rep := f.start(context.Background(), "args", "x")[0]
	rep2, err := f.r.Send(context.Background(), rep.Run.ID, "y", Vars{"new_session": "forged", "prompt": "forged"})
	if err != nil || rep2.Err != nil {
		t.Fatal(err, rep2.Err)
	}
	if rep2.Output != "flags: --resume "+rep.Run.Session {
		t.Fatalf("output %q", rep2.Output)
	}
}

// Review finding 9: a failure document keeps its session.
func TestFailedJSONKeepsSession(t *testing.T) {
	f := newFixture(t, nil)
	f.def("jsonfake", Definition{
		Args:   [][]string{{"{mode}"}},
		Output: OutputRule{Format: "json", Field: "result", ErrorField: "is_error", SessionField: "session_id"},
	})
	f.profile("jf", "harness: jsonfake\neffect: pure\nmode: jsonfailsess")
	rep := f.start(context.Background(), "jf", "x")[0]
	if rep.Outcome != "permanent" || rep.Run.Session != "s-9" {
		t.Fatalf("outcome %s session %q", rep.Outcome, rep.Run.Session)
	}
}

// Review finding 10: a relative home becomes absolute.
func TestRelativeHomeIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	h, err := NewHome("data")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(h.Dir) {
		t.Fatalf("home %q is relative", h.Dir)
	}
}

// Review finding 12: concurrent budget updates do not lose each other.
func TestConcurrentBudgetUpdates(t *testing.T) {
	h, err := NewHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			if err := h.SetBudget(fmt.Sprintf("b%d", i), int64(i+1)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	b, err := h.Budgets()
	if err != nil || len(b) != 10 {
		t.Fatalf("got %d budgets (%v), want 10", len(b), err)
	}
}
