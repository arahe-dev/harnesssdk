package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	harness "github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// The test binary doubles as the harness CLI (HARNESS_TEST_CLI=1) and as a
// fake agent CLI (HARNESS_TEST_FAKE=1) for subprocess tests.
func TestMain(m *testing.M) {
	if code, ok := harness.Subcommand(); ok {
		os.Exit(code)
	}
	switch {
	case os.Getenv("HARNESS_TEST_FAKE") == "1":
		os.Exit(fakeAgent(os.Args[1:]))
	case os.Getenv("HARNESS_TEST_CLI") == "1":
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	// The processes the tests start (guards, fake agents, workers) are this
	// binary; built with -race, each would sleep a second as it exits.
	if os.Getenv("GORACE") == "" {
		os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	os.Exit(m.Run())
}

// fakeAgent "tree <pidfile>" starts a sleeping grandchild, records its pid
// and sleeps.
func fakeAgent(args []string) int {
	if len(args) == 2 && args[0] == "reply" {
		fmt.Println(args[1])
		return 0
	}
	if len(args) == 1 && args[0] == "echo" {
		in, _ := io.ReadAll(os.Stdin)
		os.Stdout.Write(append([]byte("echo: "), in...))
		return 0
	}
	if len(args) == 2 && args[0] == "stream" {
		// JSONL progress, then the answer once the gate file exists.
		fmt.Println(`{"type":"tool","name":"edit","path":"a.go"}`)
		fmt.Println(`{"type":"msg","text":"working on it"}`)
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(args[1]); err == nil {
				break
			}
		}
		fmt.Println(`{"type":"done","text":"all done"}`)
		return 0
	}
	if len(args) == 2 && args[0] == "tree" {
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep")
		child.Env = append(os.Environ(), "HARNESS_TEST_FAKE=1")
		child.Start()
		os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}
	time.Sleep(60 * time.Second)
	return 0
}

func harnessCLI(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestParseInterleaves(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var files multi
	fs.Var(&files, "file", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, []string{"review", "--file", "a", "look", "--json", "here", "--file", "b", "--", "-dash"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pos, []string{"review", "look", "here", "-dash"}) || !*asJSON || !slices.Equal(files, multi{"a", "b"}) {
		t.Fatalf("pos %v json %v files %v", pos, *asJSON, files)
	}
}

func TestAttachFencesFiles(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.md")
	os.WriteFile(f, []byte("has ``` fence\n"), 0o600)
	got, err := attach("Review this.", []string{f})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\n````") || !strings.HasPrefix(got, "Review this.\n\nx.md\n") {
		t.Fatalf("attach:\n%s", got)
	}
}

func TestCLIEndToEnd(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	if code, out, _ := harnessCLI(t, "", "guide"); code != 0 || !strings.Contains(out, "## Workflow") {
		t.Fatalf("guide: %d", code)
	}
	def := `{"command":"definitely-not-installed-cli","args":[["-p"],["--model","{model}"]],"output":{"format":"text"}}`
	if code, _, errs := harnessCLI(t, def, "def", "set", "x", "--file", "-"); code != 0 {
		t.Fatalf("def set: %d %s", code, errs)
	}
	profile := "---\nharness: x\neffect: pure\nmodel: m1\n---\nBe brief.\n"
	code, out, errs := harnessCLI(t, profile, "profile", "set", "p", "--file", "-")
	if code != 0 || !strings.Contains(out, "command: definitely-not-installed-cli -p --model m1") {
		t.Fatalf("profile set: %d %s %s", code, out, errs)
	}
	if code, _, errs := harnessCLI(t, "", "profile", "set", "p", "model=m2"); code != 0 {
		t.Fatalf("profile set key: %d %s", code, errs)
	}
	if code, out, _ := harnessCLI(t, "", "run", "p", "--dry-run", "hi"); code != 0 || !strings.Contains(out, "--model m2") {
		t.Fatalf("dry run: %d %s", code, out)
	}
	if code, _, errs := harnessCLI(t, "", "run", "p", "--set", "prompt=x", "hi"); code != 2 || !strings.Contains(errs, "filled in by harness") {
		t.Fatalf("--set builtin: %d %s", code, errs)
	}
	if code, _, errs := harnessCLI(t, "", "run", "p", "--cwd", filepath.Join(t.TempDir(), "missing"), "hi"); code != 2 || !strings.Contains(errs, "is not a folder") {
		t.Fatalf("--cwd: %d %s", code, errs)
	}
	// The CLI is not installed: a permanent failure, recorded as a run.
	code, _, errs = harnessCLI(t, "", "run", "p", "hi")
	if code != 3 || !strings.Contains(errs, "permanent") {
		t.Fatalf("missing CLI: %d %s", code, errs)
	}
	if code, out, _ := harnessCLI(t, "", "runs"); code != 0 || !strings.Contains(out, "permanent") {
		t.Fatalf("runs: %d %s", code, out)
	}
	if code, _, _ := harnessCLI(t, "", "nonsense"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
}

func TestCLICompareAndStats(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	exe, _ := os.Executable()
	def := `{"command":` + strconv.Quote(exe) + `,"args":[["echo"]],"env":{"HARNESS_TEST_FAKE":"1"},"output":{"format":"text"}}`
	if code, _, errs := harnessCLI(t, def, "def", "set", "fake", "--file", "-"); code != 0 {
		t.Fatalf("def set: %d %s", code, errs)
	}
	for _, p := range []string{"a", "b"} {
		profile := "---\nharness: fake\neffect: pure\nmodel: m-" + p + "\n---\nBe brief.\n"
		if code, _, errs := harnessCLI(t, profile, "profile", "set", p, "--file", "-"); code != 0 {
			t.Fatalf("profile set %s: %d %s", p, code, errs)
		}
	}
	code, out, errs := harnessCLI(t, "", "compare", "a,b", "--kind", "eval", "hello")
	if code != 0 || strings.Count(out, "echo: hello") != 2 || !strings.Contains(out, "PROFILE") || !strings.Contains(out, "m-b") {
		t.Fatalf("compare: %d %s %s", code, out, errs)
	}
	if code, _, errs := harnessCLI(t, "", "compare", "a", "hello"); code != 2 {
		t.Fatalf("compare one profile: %d %s", code, errs)
	}
	code, out, _ = harnessCLI(t, "", "stats")
	if code != 0 || !strings.Contains(out, "m-a") || !strings.Contains(out, "eval") || !strings.Contains(out, "100%") {
		t.Fatalf("stats: %d %s", code, out)
	}
	if code, out, _ := harnessCLI(t, "", "stats", "--json", "--profile", "b"); code != 0 || !strings.Contains(out, `"profile": "b"`) || strings.Contains(out, `"profile": "a"`) {
		t.Fatalf("stats --json: %d %s", code, out)
	}
	// Checks decide the outcome.
	if code, _, errs := harnessCLI(t, "", "run", "a", "--check", "exit 0", "hi"); code != 0 || !strings.Contains(errs, "check passed: exit 0") {
		t.Fatalf("passing check: %d %s", code, errs)
	}
	if code, out, errs := harnessCLI(t, "", "run", "a", "--check", "exit 3", "hi"); code != 3 || !strings.Contains(errs, "check failed") || !strings.Contains(out, "echo: hi") {
		t.Fatalf("failing check: %d %q %s", code, out, errs)
	}
	code, out, _ = harnessCLI(t, "", "stats", "--profile", "a")
	if code != 0 || !strings.Contains(out, "1/2") {
		t.Fatalf("stats checks: %s", out)
	}
	// def check: the CLI and its version; with a profile, a live turn.
	withVersion := `{"command":` + strconv.Quote(exe) + `,"args":[["reply","harness-check-ok"]],"env":{"HARNESS_TEST_FAKE":"1"},"version":["echo"],"output":{"format":"text"}}`
	if code, _, errs := harnessCLI(t, withVersion, "def", "set", "fake", "--file", "-"); code != 0 {
		t.Fatalf("def set: %d %s", code, errs)
	}
	if code, out, errs := harnessCLI(t, "", "def", "check", "fake", "--profile", "a"); code != 0 || !strings.Contains(out, "version:") || !strings.Contains(out, "as asked") {
		t.Fatalf("def check: %d %s %s", code, out, errs)
	}
	if code, out, _ := harnessCLI(t, "", "config"); code != 0 || !strings.Contains(out, "spend_limit_usd") || !strings.Contains(out, "max_queue") {
		t.Fatalf("config: %s", out)
	}
}

func TestCostPerSuccess(t *testing.T) {
	cost := func(turns int) map[string]harness.MetricStats {
		return map[string]harness.MetricStats{"cost_usd": {Sum: 1, Turns: turns}}
	}
	for _, c := range []struct {
		s    harness.Stats
		want string
	}{
		{harness.Stats{Turns: 2, Outcomes: map[string]int{"success": 2}, Metrics: cost(2)}, "$0.5000"},
		{harness.Stats{Turns: 2, Outcomes: map[string]int{"success": 2}, Metrics: cost(1)}, "-"}, // one turn did not report
		{harness.Stats{Turns: 2, Outcomes: map[string]int{"permanent": 2}, Metrics: cost(2)}, "-"},
		{harness.Stats{Turns: 2, Outcomes: map[string]int{"success": 2}}, "-"},
	} {
		if got := costPerSuccess(c.s); got != c.want {
			t.Errorf("%+v: %s, want %s", c.s, got, c.want)
		}
	}
}

func TestCLIConfigAndGC(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	if code, out, _ := harnessCLI(t, "", "config"); code != 0 || !strings.Contains(out, "max_depth") || !regexp.MustCompile(`retention_days +30 `).MatchString(out) {
		t.Fatalf("config: %d %q", code, out)
	}
	if code, _, errs := harnessCLI(t, "", "config", "set", "max_depth", "x"); code != 2 {
		t.Fatalf("config set non-number: %d %s", code, errs)
	}
	if code, _, errs := harnessCLI(t, "", "config", "set", "retention_days", "0"); code != 0 {
		t.Fatalf("config set: %d %s", code, errs)
	}
	if code, out, _ := harnessCLI(t, "", "config"); code != 0 || !regexp.MustCompile(`spend_unknown +warn `).MatchString(out) {
		t.Fatalf("spend_unknown's default: %q", out)
	}
	if code, _, errs := harnessCLI(t, "", "config", "set", "spend_unknown", "1.5"); code != 0 {
		t.Fatalf("config set spend_unknown: %d %s", code, errs)
	}
	if code, out, _ := harnessCLI(t, "", "config"); code != 0 || !regexp.MustCompile(`spend_unknown +1\.5 `).MatchString(out) {
		t.Fatalf("spend_unknown set: %q", out)
	}
	if code, _, errs := harnessCLI(t, "", "config", "set", "spend_unknown", "sometimes"); code != 2 || !strings.Contains(errs, "warn, refuse") {
		t.Fatalf("config set spend_unknown sometimes: %d %s", code, errs)
	}
	if code, _, errs := harnessCLI(t, "", "gc"); code != 2 || !strings.Contains(errs, "--days") {
		t.Fatalf("gc with retention off: %d %s", code, errs)
	}
	if code, _, errs := harnessCLI(t, "", "gc", "--days", "1", "--dry-run"); code != 0 || !strings.Contains(errs, "would remove 0 run(s)") {
		t.Fatalf("gc --dry-run: %d %s", code, errs)
	}
	if code, out, _ := harnessCLI(t, "", "status"); code != 0 || !strings.Contains(out, "(max_running)") {
		t.Fatalf("status: %d %s", code, out)
	}
}

func TestCLIBackgroundAndProject(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	exe, _ := os.Executable()
	def := `{"command":` + strconv.Quote(exe) + `,"args":[["echo"]],"env":{"HARNESS_TEST_FAKE":"1"},"output":{"format":"text"}}`
	if code, _, errs := harnessCLI(t, def, "def", "set", "fake", "--file", "-"); code != 0 {
		t.Fatal(errs)
	}
	repo := t.TempDir()
	profile := "---\nharness: fake\neffect: pure\n---\n"
	if code, out, errs := harnessCLI(t, profile, "profile", "set", "p", "--project", "--cwd", repo, "--file", "-"); code != 0 || !strings.Contains(out, "project") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if code, out, _ := harnessCLI(t, "", "trust", "--cwd", repo); code != 0 || !strings.Contains(out, "trusted  profiles/p.md") {
		t.Fatalf("trust: %d %s", code, out)
	}
	code, out, errs := harnessCLI(t, "", "start", "p", "--cwd", repo, "hello")
	id := strings.TrimSpace(out)
	if code != 0 || id == "" {
		t.Fatalf("start: %d %q %s", code, out, errs)
	}
	if code, out, errs := harnessCLI(t, "", "wait", id, "--timeout", "60s"); code != 0 || strings.TrimSpace(out) != "echo: hello" {
		t.Fatalf("wait: %d %q %s", code, out, errs)
	}
	if code, out, _ := harnessCLI(t, "", "result", id, "--json"); code != 0 || !strings.Contains(out, `"output": "echo: hello"`) {
		t.Fatalf("result: %d %s", code, out)
	}
	if code, out, _ := harnessCLI(t, "", "status"); code != 0 || !strings.Contains(out, "0 active run(s)") {
		t.Fatalf("status: %d %s", code, out)
	}
	if code, _, errs := harnessCLI(t, "", "stop", id); code != 2 || !strings.Contains(errs, "no unfinished turn") {
		t.Fatalf("stop: %d %s", code, errs)
	}
}

// A caller following a background run sees where it is and what it is
// doing: wait --timeout prints its status, status and events show it, and
// events --wait wakes on what happens next.
func TestCLIFollowRun(t *testing.T) {
	t.Setenv("HARNESS_HOME", t.TempDir())
	exe, _ := os.Executable()
	gate := filepath.Join(t.TempDir(), "gate")
	def := `{"command":` + strconv.Quote(exe) + `,"args":[["stream",` + strconv.Quote(gate) + `]],"env":{"HARNESS_TEST_FAKE":"1"},
	  "output":{"format":"jsonl","answer":{"where":{"type":"done"},"field":"text"},
	    "progress":[{"kind":"tool","where":{"type":"tool"},"field":"name","detail":"path"},
	                {"kind":"say","where":{"type":"msg"},"field":"text"}]}}`
	if code, _, errs := harnessCLI(t, def, "def", "set", "stream", "--file", "-"); code != 0 {
		t.Fatal(errs)
	}
	if code, _, errs := harnessCLI(t, "---\nharness: stream\neffect: idempotent\ntimeout: 10m\n---\n", "profile", "set", "w", "--file", "-"); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := harnessCLI(t, "", "start", "w", "go")
	id := strings.TrimSpace(out)
	if code != 0 || id == "" {
		t.Fatalf("start: %d %q %s", code, out, errs)
	}
	defer os.WriteFile(gate, nil, 0o644)

	// events --wait wakes on progress, long before the turn ends.
	var cursor string
	var seen string
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(seen, "working on it"); {
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %s", seen)
		}
		args := []string{"events", id, "--wait", "10s"}
		if cursor != "" {
			args = append(args, "--since", cursor)
		}
		code, out, errs := harnessCLI(t, "", args...)
		if code != 0 && code != harness.ExitRunning {
			t.Fatalf("events: %d %s", code, errs)
		}
		seen += out
		m := regexp.MustCompile(`cursor (\d+)`).FindStringSubmatch(errs)
		if m == nil {
			t.Fatalf("no cursor: %s", errs)
		}
		cursor = m[1]
	}
	if !strings.Contains(seen, "queued  go") || !strings.Contains(seen, "tool  edit  a.go") {
		t.Fatalf("events:\n%s", seen)
	}

	code, out, errs = harnessCLI(t, "", "wait", id, "--timeout", "200ms")
	if code != harness.ExitRunning || !strings.Contains(errs, "still running") ||
		!strings.Contains(out, "phase:    running · attempt 1 of 1") || !strings.Contains(out, "said:     working on it") ||
		!strings.Contains(out, "left of its 10m0s timeout") || !strings.Contains(out, "next:     harness wait "+id) {
		t.Fatalf("wait --timeout: %d\n%s\n%s", code, out, errs)
	}
	t.Logf("wait --timeout prints:\n%s%s", errs, out)
	code, out, _ = harnessCLI(t, "", "wait", id, "--timeout", "200ms", "--json")
	if code != harness.ExitRunning || !strings.Contains(out, `"exit_code": 7`) || !strings.Contains(out, `"phase": "running"`) || !strings.Contains(out, `"said": "working on it"`) {
		t.Fatalf("wait --json: %d %s", code, out)
	}
	if code, out, _ := harnessCLI(t, "", "status", id, "--json"); code != 0 || !strings.Contains(out, `"live": true`) || !strings.Contains(out, `"time_left_ms"`) {
		t.Fatalf("status --json: %d %s", code, out)
	}

	os.WriteFile(gate, nil, 0o644)
	if code, out, errs := harnessCLI(t, "", "wait", id, "--timeout", "60s"); code != 0 || strings.TrimSpace(out) != "all done" {
		t.Fatalf("wait: %d %q %s", code, out, errs)
	}
	code, out, errs = harnessCLI(t, "", "events", id, "--since", cursor, "--wait", "5s")
	if code != 0 || !strings.Contains(out, "finished  success  all done") || !strings.Contains(errs, "has finished") {
		t.Fatalf("events after the end: %d %s %s", code, out, errs)
	}
	if code, out, _ := harnessCLI(t, "", "status", id); code != 0 || !strings.Contains(out, "phase:    success") || !strings.Contains(out, "harness result "+id) {
		t.Fatalf("status: %d %s", code, out)
	}
}
