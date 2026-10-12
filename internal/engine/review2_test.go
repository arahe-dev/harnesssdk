package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Regression tests for the review of v2.0.0-next.

// worktreeRun runs one turn of profile in a worktree of a fresh repository.
func worktreeRun(t *testing.T, f *fixture, profile, sub string) (*Run, string) {
	t.Helper()
	repo := gitRepo(t)
	dir := filepath.Join(repo, sub)
	os.MkdirAll(dir, 0o755)
	reps, err := f.r.Start(context.Background(), profile, []string{"x"}, dir, TurnOptions{Worktree: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.home.LoadRun(reps[0].Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, repo
}

// H1: gc commits a worktree's uncommitted work before removing it, and
// keeps a worktree whose work cannot be committed.
func TestGCKeepsWorktreeWork(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, repo := worktreeRun(t, f, "writer", "sub")
	os.WriteFile(filepath.Join(run.Workdir, "late.txt"), []byte("left after the turn"), 0o644)
	if rep, err := f.home.GC(time.Now().Add(time.Hour), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if files, err := git(context.Background(), "-C", repo, "show", "--name-only", "--format=%s", run.Branch); err != nil ||
		!strings.Contains(files, "kept by gc") || !strings.Contains(files, "late.txt") {
		t.Fatalf("the late work was not kept on the branch: %q %v", files, err)
	}

	run, _ = worktreeRun(t, f, "writer", "sub")
	os.WriteFile(filepath.Join(run.Workdir, "late.txt"), []byte("cannot be committed"), 0o644)
	lock, err := git(context.Background(), "-C", run.Worktree, "rev-parse", "--path-format=absolute", "--git-path", "index.lock")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(lock, nil, 0o644) // git add now fails
	defer os.Remove(lock)
	if _, err := f.home.GC(time.Now().Add(time.Hour), false); err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("gc removed a worktree it could not commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.Workdir, "late.txt")); err != nil {
		t.Fatal("the uncommitted work was lost")
	}
	if _, err := f.home.LoadRun(run.ID); err != nil {
		t.Fatal("the run was removed")
	}
}

// H2: run records and config are format 2, which v1 refuses; format 1
// still reads.
func TestV2Formats(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "ok", "x")[0]
	data, _ := os.ReadFile(rep.Run.Path("run.json"))
	if !strings.Contains(string(data), `"format": 2`) {
		t.Fatalf("run record is not format 2: %.80s", data)
	}
	if err := checkFormat("run", 2); err == nil {
		t.Fatal("a v1 reader (format 1) would accept a v2 record")
	}
	f.home.SetConfig("max_queue", 3)
	cfg, _ := os.ReadFile(f.home.configPath())
	if !strings.Contains(string(cfg), `"format": 2`) {
		t.Fatalf("config is not format 2: %s", cfg)
	}
	old := oldRun(t, f.home, 1, "v1v1") // written as format 1
	if _, err := f.home.LoadRun(old.ID); err != nil {
		t.Fatalf("a format 1 record no longer reads: %v", err)
	}
}

// M3: a spending refusal between attempts is final, not retried.
func TestSpendRefusalIsNotRetried(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nretry: throttled x5\nbackoff: 1s\nmode: usage\nspend_limit_usd: 0.6")
	runs, err := f.r.Prepare("u", []string{"queued"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.start(context.Background(), "u", "first") // spends $0.50
	f.start(context.Background(), "u", "second")
	start := time.Now()
	rep := f.r.Execute(context.Background(), runs[0].ID, 1)
	if rep.Outcome != "throttled" || rep.Attempts != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("%s after %d attempts in %s: %v", rep.Outcome, rep.Attempts, time.Since(start), rep.Err)
	}
}

// M4, M5: spending counts each attempt as it is recorded, and by when it
// was spent, also on a run created long before.
func TestSpendingCountsAttemptsAndOldRuns(t *testing.T) {
	h := newGateHome(t)
	old := oldRun(t, h, 3, "spnd") // created three days ago
	old.Turns = append(old.Turns, TurnRecord{N: 2, Outcome: "running", Spent: time.Now().UTC(),
		Metrics: map[string]float64{"cost_usd": 0.4}}) // an attempt of a follow-up, today
	if err := old.Save(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	os.Chtimes(old.Dir(), now, now)
	all, mine, err := h.spending(time.Now().Add(-spendWindow), "p")
	if err != nil || all != 0.4 || mine != 0.4 {
		t.Fatalf("spent %v / %v %v, want 0.4", all, mine, err)
	}
}

// M6: a check runs with the run's identity.
func TestCheckKnowsItsRun(t *testing.T) {
	f := newFixture(t, nil)
	check := `[ -n "$HARNESS_RUN" ] && [ -n "$HARNESS_HOME" ]`
	if runtime.GOOS == "windows" {
		check = `if "%HARNESS_RUN%"=="" (exit 1) else if "%HARNESS_HOME%"=="" (exit 1) else (exit 0)`
	}
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo\ncheck: "+check)
	if rep := f.start(context.Background(), "ok", "x")[0]; rep.Checked != "passed" {
		t.Fatalf("check without the run's identity: %s %v", rep.Checked, rep.Err)
	}
}

// M7: max_wait holds even while a ledger's lock is held by another
// process, and a turn past it never runs.
func TestMaxWaitUnderLockContention(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.profile("w", "harness: fake\neffect: pure\nbudget: b=1\nmax_wait: 300ms\nmode: echo")
	g := f.home.Gate(nil)
	os.MkdirAll(g.ledger("b"), 0o700)
	lk, err := lockOpen(filepath.Join(g.ledger("b"), "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, func() { lk.Close() })
	start := time.Now()
	rep := f.start(context.Background(), "w", "x")[0]
	if rep.Outcome != "throttled" || rep.Attempts != 0 || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("%s, %d attempts, after %s: %v", rep.Outcome, rep.Attempts, time.Since(start), rep.Err)
	}
}

// M8: a git worktree add that fails leaves no branch or folder behind.
func TestFailedWorktreeAddIsUndone(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	defer func() { runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) } }()
	runID = func() string { return "261012-000000-ffff" }
	os.MkdirAll(filepath.Join(f.home.Dir, "worktrees"), 0o700)
	// Something already at the worktree's path makes git fail.
	os.WriteFile(filepath.Join(f.home.Dir, "worktrees", "261012-000000-ffff"), []byte("in the way"), 0o644)
	if _, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("a failed worktree add was not reported")
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "runs")); len(entries) != 0 {
		t.Fatalf("%d runs left behind", len(entries))
	}
}

// M9: gc undoes the worktree and branch of a creation whose creator died.
func TestGCUndoesCrashedWorktreeCreation(t *testing.T) {
	h := newGateHome(t)
	repo := gitRepo(t)
	s, err := h.stageRun("p", "fake", repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.addWorktree(context.Background(), s.Run); err != nil {
		t.Fatal(err)
	}
	s.lock.Close() // the creator dies here, before publishing
	if _, err := os.Stat(s.Worktree); err != nil {
		t.Fatal(err)
	}
	h.GC(time.Now().AddDate(0, 0, -30), false)
	if _, err := os.Stat(s.Worktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the crashed creation's worktree was kept")
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("its branch was kept: %q", out)
	}
	if _, err := os.Stat(s.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("its staging folder was kept")
	}
}

// M10: a worktree whose original folder is gone is removed through the
// repository's own git folder.
func TestWorktreeRemovedWithoutItsFolder(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, repo := worktreeRun(t, f, "writer", "sub")
	if err := os.RemoveAll(filepath.Join(repo, "sub")); err != nil { // run.Cwd
		t.Fatal(err)
	}
	if err := removeWorktree(context.Background(), run, false); err != nil {
		t.Fatal(err)
	}
	if out, _ := git(context.Background(), "-C", repo, "worktree", "list"); strings.Contains(out, run.ID) {
		t.Fatalf("worktree still registered: %s", out)
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", run.Branch); out == "" {
		t.Fatal("the branch was deleted")
	}
}

// M12: def check reports a reply that is not exactly the one asked for.
func TestAnsweredAsAsked(t *testing.T) {
	for out, want := range map[string]bool{
		"harness-check-ok": true, " harness-check-ok.\n": true, "`harness-check-ok`": true,
		"echo: Reply with exactly: harness-check-ok": false, "": false,
	} {
		if got := answeredAsAsked(out); got != want {
			t.Errorf("%q: %v", out, got)
		}
	}
}

// L15: a subfolder whose name starts with a space keeps it.
func TestWorktreeSubfolderWithSpace(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, _ := worktreeRun(t, f, "writer", " lead")
	if filepath.Base(run.Workdir) != " lead" {
		t.Fatalf("workdir %q", run.Workdir)
	}
	if _, err := os.Stat(filepath.Join(run.Workdir, "out.txt")); err != nil {
		t.Fatalf("the agent did not work in %q: %v", run.Workdir, err)
	}
}
