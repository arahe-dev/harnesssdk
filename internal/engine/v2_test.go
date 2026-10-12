package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A check decides whether the turn succeeded; a failed one keeps the
// agent's answer and session.
func TestCheck(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo\ncheck: exit 0")
	rep := f.start(context.Background(), "ok", "x")[0]
	if rep.Err != nil || rep.Checked != "passed" || rep.Check != "exit 0" {
		t.Fatalf("passing check: %+v", rep)
	}
	f.profile("bad", "harness: fake\neffect: idempotent\nretry: transient x3\nmode: echo\ncheck: exit 3")
	rep = f.start(context.Background(), "bad", "x")[0]
	if rep.Outcome != "permanent" || rep.Checked != "failed" || !strings.Contains(rep.Err.Error(), "status 3") || rep.Attempts != 1 {
		t.Fatalf("failing check: %s %v %d", rep.Outcome, rep.Err, rep.Attempts)
	}
	run, _ := f.home.LoadRun(rep.Run.ID)
	if out, _ := run.Output(1); !strings.Contains(out, "echo: x") || run.Session == "" || rep.Output == "" {
		t.Fatalf("the answer or session of a failed check was lost: %q %q", out, run.Session)
	}
	// The caller's check overrides the profile's, as written: quotes and
	// spaces reach the shell intact.
	quoted := `[ "a b" = "a b" ]`
	if runtime.GOOS == "windows" {
		quoted = `if "a b"=="a b" (exit 0) else (exit 1)`
	}
	reps, err := f.r.Start(context.Background(), "bad", []string{"y"}, f.cwd, TurnOptions{Check: quoted}, nil)
	if err != nil || reps[0].Err != nil || reps[0].Checked != "passed" {
		t.Fatalf("quoted override: %v %+v", err, reps)
	}
}

// gitRepo makes a repository with one commit and a subfolder.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := git(context.Background(), append([]string{"-C", repo}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	os.WriteFile(filepath.Join(repo, "sub", "README"), []byte("hello\n"), 0o644)
	if _, err := git(context.Background(), "-C", repo, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), "-C", repo, "commit", "-q", "-m", "first"); err != nil {
		t.Fatal(err)
	}
	return repo
}

// A worktree run edits its own copy, on its own branch, which keeps the
// work when the run is collected.
func TestWorktree(t *testing.T) {
	f := newFixture(t, nil)
	repo := gitRepo(t)
	sub := filepath.Join(repo, "sub")
	exists := "test -f out.txt"
	if runtime.GOOS == "windows" {
		exists = "if exist out.txt (exit 0) else (exit 1)"
	}
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt\ncheck: "+exists)
	reps, err := f.r.Start(context.Background(), "writer", []string{"x"}, sub, TurnOptions{Worktree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := reps[0]
	run, _ := f.home.LoadRun(rep.Run.ID)
	if run.Worktree == "" || run.Branch != "harness/"+run.ID || run.Workdir != filepath.Join(run.Worktree, "sub") || run.Cwd != sub {
		t.Fatalf("run %+v", run)
	}
	// The check ran in the agent's folder, in the worktree.
	if rep.Checked != "passed" {
		t.Fatalf("check: %s %v", rep.Checked, rep.Err)
	}
	if _, err := os.Stat(filepath.Join(sub, "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the agent wrote into the original folder")
	}
	if b, err := os.ReadFile(filepath.Join(run.Workdir, "out.txt")); err != nil || string(b) != "written by the agent" {
		t.Fatalf("worktree file: %q %v", b, err)
	}
	if !strings.Contains(rep.Changes, "1 file changed") {
		t.Fatalf("changes %q", rep.Changes)
	}
	if log, err := git(context.Background(), "-C", repo, "log", "--oneline", run.Branch); err != nil || !strings.Contains(log, "harness: run "+run.ID) {
		t.Fatalf("branch log: %q %v", log, err)
	}
	// Collected: the worktree goes, the branch stays.
	if rep, err := f.home.GC(time.Now().Add(time.Hour), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if _, err := os.Stat(run.Worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktree kept")
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", run.Branch); out == "" {
		t.Fatal("the branch was deleted")
	}
	// A creation that fails removes the worktree and branch it made.
	defer func() { testHookCreate = nil }()
	testHookCreate = func(stage string) error {
		if stage == "queue" {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	if _, err := f.r.Start(context.Background(), "writer", []string{"y"}, sub, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("failed creation reported success")
	}
	if wts, _ := os.ReadDir(filepath.Join(f.home.Dir, "worktrees")); len(wts) != 0 {
		t.Fatalf("%d worktrees left behind", len(wts))
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); strings.Count(out, "harness/") != 1 {
		t.Fatalf("branches: %q", out)
	}
	// Outside a repository, --worktree is refused.
	testHookCreate = nil
	if _, err := f.r.Start(context.Background(), "writer", []string{"z"}, t.TempDir(), TurnOptions{Worktree: true}, nil); err == nil || !strings.Contains(err.Error(), "--worktree needs") {
		t.Fatalf("outside a repository: %v", err)
	}
}

// Spending limits stop new turns once a profile, or the machine, has spent
// its limit over the last day.
func TestSpendLimits(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f) // every turn costs $0.50
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.9")
	for i := range 2 {
		if rep := f.start(context.Background(), "u", fmt.Sprint(i))[0]; rep.Err != nil {
			t.Fatalf("turn %d: %v", i, rep.Err)
		}
	}
	_, err := f.r.Start(context.Background(), "u", []string{"third"}, f.cwd, TurnOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "spent $1.00") || OutcomeName(err) != "throttled" {
		t.Fatalf("profile limit: %v", err)
	}
	f.profile("v", "harness: fakeusage\neffect: pure\nmode: usage")
	f.home.SetConfig("spend_limit_usd", 1)
	if _, err := f.r.Start(context.Background(), "v", []string{"x"}, f.cwd, TurnOptions{}, nil); err == nil || !strings.Contains(err.Error(), "spend_limit_usd is $1") {
		t.Fatalf("machine limit: %v", err)
	}
}

func TestDefinitionCheck(t *testing.T) {
	f := newFixture(t, nil)
	f.def("fake", Definition{Args: [][]string{{"{mode}"}, {"{*opts}"}}, Output: OutputRule{Format: "text"}, Version: []string{"version"}})
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	f.profile("exact", "harness: fake\neffect: pure\nmode: reply\nopts: text=harness-check-ok")
	c, err := f.r.CheckDefinition(context.Background(), "fake", f.cwd, "")
	if err != nil || c.Version != "fake 1.2.3" || c.Path == "" || c.Live != nil {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = f.r.CheckDefinition(context.Background(), "fake", f.cwd, "exact")
	if err != nil || !c.Answered || c.Live.Run.Kind != "check" {
		t.Fatalf("live: %+v %v", c, err)
	}
	// An agent that only echoes the prompt has not answered as asked.
	if c, err = f.r.CheckDefinition(context.Background(), "fake", f.cwd, "echo"); err != nil || c.Answered {
		t.Fatalf("echo counted as an answer: %+v %v", c, err)
	}
	// A declared metric that does not come out is reported missing.
	usageDef(f)
	f.def("fakeusage2", Definition{Args: [][]string{{"{mode}"}, {"{*opts}"}}, Output: OutputRule{Format: "jsonl",
		Answer:  &Selector{Where: map[string]string{"type": "done"}, Field: "content.-1.text"},
		Metrics: map[string]*Selector{"cost_usd": {Where: map[string]string{"type": "step"}, Field: "cost", Pick: "sum"}, "renamed": {Where: map[string]string{"type": "step"}, Field: "gone"}}}, Version: []string{"version"}})
	f.profile("u2", "harness: fakeusage2\neffect: pure\nmode: usage")
	if c, err = f.r.CheckDefinition(context.Background(), "fakeusage2", f.cwd, "u2"); err != nil || len(c.Missing) != 1 || c.Missing[0] != "renamed" {
		t.Fatalf("missing metrics: %+v %v", c, err)
	}
	f.def("gone", Definition{Output: OutputRule{Format: "text"}})
	d, _ := f.home.Definition("gone", f.cwd)
	d.Command = "no-such-cli-xyz"
	data := []byte(`{"command":"no-such-cli-xyz","output":{"format":"text"}}`)
	f.home.SaveDefinition("gone", data, "")
	if _, err := f.r.CheckDefinition(context.Background(), "gone", f.cwd, ""); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("missing CLI: %v", err)
	}
}

// A CLI that writes its usage to a report file feeds metrics and the
// session id from there.
func TestReportFile(t *testing.T) {
	f := newFixture(t, nil)
	f.def("fakerep", Definition{
		Args: [][]string{{"{mode}"}, {"--usage-file", "{report_file}"}},
		Output: OutputRule{Format: "text", Report: &ReportRule{SessionField: "session_id", Metrics: map[string]*Selector{
			"input_tokens": {Field: "input_tokens+cache_read_tokens"},
			"cost_usd":     {Field: "estimated_cost_usd"},
		}}},
	})
	f.profile("r", "harness: fakerep\neffect: pure\nmode: report")
	rep := f.start(context.Background(), "r", "x")[0]
	if rep.Err != nil || rep.Output != "reported" || rep.Metrics["input_tokens"] != 120 || rep.Metrics["cost_usd"] != 0.25 {
		t.Fatalf("%+v", rep)
	}
	if run, _ := f.home.LoadRun(rep.Run.ID); run.Session != "rep-7" {
		t.Fatalf("session %q", run.Session)
	}
	if _, err := ParseDefinition("x", []byte(`{"command":"x","output":{"format":"text","report":{}}}`)); err == nil {
		t.Fatal("an empty report rule was accepted")
	}
}
