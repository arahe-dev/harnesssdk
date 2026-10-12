package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	if rep, err := f.home.GC(context.Background(), time.Now().Add(time.Hour), false); err != nil || len(rep.Runs) != 1 {
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
	if _, err := f.home.GC(context.Background(), time.Now().Add(time.Hour), false); err == nil || !strings.Contains(err.Error(), "kept") {
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
	// Records and config as v1.7 wrote them, format 1, still read.
	dir := filepath.Join(f.home.Dir, "runs", "261001-120000-v1v1")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"format": 1, "id": "261001-120000-v1v1", "profile": "p", "harness": "fake", "turns": []}`), 0o600)
	if r, err := f.home.LoadRun("261001-120000-v1v1"); err != nil || r.Format != 1 {
		t.Fatalf("a format 1 record no longer reads: %v", err)
	}
	os.WriteFile(f.home.configPath(), []byte(`{"format": 1, "max_depth": 2}`), 0o600)
	if c, err := f.home.Config(); err != nil || c.MaxDepth != 2 {
		t.Fatalf("a format 1 config no longer reads: %+v %v", c, err)
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

// M4, M5: spending counts each attempt by when it was spent, also on a
// run created long before, and turns recorded before v2 by their records.
func TestSpendingCountsAttemptsAndOldRuns(t *testing.T) {
	h := newGateHome(t)
	// A follow-up whose first attempt spent $10 yesterday, more than a
	// day ago, and whose second spent $0.40 today: only today's counts.
	yesterday := time.Now().Add(-25 * time.Hour)
	for _, e := range []spendEntry{
		{Run: "r", Created: yesterday.UTC(), Turn: 2, Attempt: 1, Profile: "p", At: yesterday.UTC(), USD: dollars(10)},
		{Run: "r", Created: yesterday.UTC(), Turn: 2, Attempt: 2, Profile: "p", At: time.Now().UTC(), USD: dollars(0.4)},
	} {
		if err := h.writeSpend("close", &e); err != nil {
			t.Fatal(err)
		}
		if e.Attempt == 1 {
			os.Chtimes(h.spendPath(&e), yesterday, yesterday)
		}
	}
	legacy := oldRun(t, h, 3, "spnd") // a v1 record, ended today
	os.WriteFile(filepath.Join(legacy.Dir(), "run.json"), []byte(fmt.Sprintf(
		`{"format": 1, "id": %q, "profile": "q", "harness": "fake", "cwd": ".", "created": %q, "turns": [{"n": 1, "outcome": "success", "ended": %q, "metrics": {"cost_usd": 0.3}}]}`,
		legacy.ID, legacy.Created.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))), 0o600)
	now := time.Now()
	os.Chtimes(legacy.Dir(), now, now)
	s, err := h.spending(time.Now().Add(-spendWindow), "p")
	if err != nil || math.Abs(s.all-0.7) > 1e-9 || math.Abs(s.mine-0.4) > 1e-9 || s.unknownAll != "" {
		t.Fatalf("spent %+v %v, want 0.7 in all, 0.4 by p", s, err)
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
	s, err := h.stageRun(context.Background(), "p", "fake", repo, "")
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
	h.GC(context.Background(), time.Now().AddDate(0, 0, -30), false)
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

// Round two of the review.

// H1: a branch or folder that already has the worktree's name is never
// touched: the run is refused.
func TestWorktreeNamesInUseAreLeftAlone(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	defer func() { runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) } }()
	runID = func() string { return "261012-000000-eeee" }
	if _, err := git(context.Background(), "-C", repo, "branch", "harness/261012-000000-eeee"); err != nil {
		t.Fatal(err)
	}
	tip, _ := git(context.Background(), "-C", repo, "rev-parse", "harness/261012-000000-eeee")
	if _, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a branch in use was not refused: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home.Dir, "worktrees")); len(entries) != 0 {
		t.Fatalf("the refused creation left %d worktree folders", len(entries))
	}
	if now, err := git(context.Background(), "-C", repo, "rev-parse", "harness/261012-000000-eeee"); err != nil || now != tip {
		t.Fatalf("the existing branch was changed: %q %v", now, err)
	}
}

// H2: a worktree whose HEAD left its branch is kept, with its run.
func TestDetachedWorktreeIsKept(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	run, _ := worktreeRun(t, f, "writer", "sub")
	if _, err := git(context.Background(), "-C", run.Worktree, "checkout", "-q", "--detach"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.home.GC(context.Background(), time.Now().Add(time.Hour), false); err == nil || !strings.Contains(err.Error(), "not on its branch") {
		t.Fatalf("gc removed a detached worktree: %v", err)
	}
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Fatal("the detached worktree was removed")
	}
}

// H3: harness's commits run no hook, and a removal that would lose work is
// refused rather than forced.
func TestHarnessCommitsRunNoHooks(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	hooks := filepath.Join(repo, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	os.WriteFile(filepath.Join(hooks, "post-commit"), []byte("#!/bin/sh\necho ran > '"+filepath.ToSlash(marker)+"'\n"), 0o755)
	os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte("#!/bin/sh\necho ran > '"+filepath.ToSlash(marker)+"'\n"), 0o755)
	reps, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil)
	if err != nil || !strings.Contains(reps[0].Changes, "1 file changed") {
		t.Fatalf("%v %+v", err, reps)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran on harness's commit")
	}
}

// M3: a turn whose wait ends while it is queued, and a ledger lock is held,
// leaves at once.
func TestExpiredWaitLeavesAtOnce(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.profile("q", "harness: fake\neffect: pure\nbudget: b=1\nmode: echo")
	g := f.home.Gate(nil)
	held, err := g.Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Report)
	go func() { done <- f.start(ctx, "q", "x")[0] }()
	time.Sleep(400 * time.Millisecond) // queued
	lk, err := lockOpen(filepath.Join(g.ledger("b"), "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	start := time.Now()
	cancel()
	select {
	case rep := <-done:
		if d := time.Since(start); d > time.Second {
			t.Fatalf("left after %s", d)
		}
		if rep.Outcome != "cancelled" {
			t.Fatalf("%s %v", rep.Outcome, rep.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not leave")
	}
}

// M4: a rollback that cannot remove what it made keeps the staged record,
// and gc finishes it later.
func TestFailedRollbackIsFinishedByGC(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	defer func() {
		testHookCreate = nil
		runID = func() string { return time.Now().Format("060102-150405") + "-" + randHex(2) }
	}()
	runID = func() string { return "261012-000000-dddd" }
	refLock := filepath.Join(repo, ".git", "refs", "heads", "harness", "261012-000000-dddd.lock")
	testHookCreate = func(stage string) error {
		if stage == "queue" {
			os.WriteFile(refLock, nil, 0o644) // git cannot delete the branch now
			return fmt.Errorf("disk full")
		}
		return nil
	}
	if _, err := f.r.Start(context.Background(), "writer", []string{"x"}, repo, TurnOptions{Worktree: true}, nil); err == nil {
		t.Fatal("failed creation reported success")
	}
	testHookCreate = nil
	stage := filepath.Join(f.home.Dir, "runs", stagePrefix+"261012-000000-dddd")
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("the record of what to undo was removed with the rollback unfinished")
	}
	os.Remove(refLock)
	f.home.GC(context.Background(), time.Now().AddDate(0, 0, -30), false)
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("gc did not finish the rollback")
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
}
