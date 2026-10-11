package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	harness "github.com/arahe-dev/harnesssdk"
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
	os.Exit(m.Run())
}

// fakeAgent "tree <pidfile>" starts a sleeping grandchild, records its pid
// and sleeps.
func fakeAgent(args []string) int {
	if len(args) == 1 && args[0] == "echo" {
		in, _ := io.ReadAll(os.Stdin)
		os.Stdout.Write(append([]byte("echo: "), in...))
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
