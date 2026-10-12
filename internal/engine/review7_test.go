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

// Regression tests for the seventh review of v2.0.0-next.

// 1: a rollback keeps a worktree whose state git cannot tell.
func TestRollbackKeepsWorktreeItCannotInspect(t *testing.T) {
	h := newGateHome(t)
	repo := gitRepo(t)
	s, err := h.stageRun(context.Background(), "p", "fake", repo, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if err := h.addWorktree(context.Background(), s.Run); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(s.Worktree, "someone.txt")
	os.WriteFile(work, []byte("work"), 0o644)
	index, err := git(context.Background(), "-C", s.Worktree, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(index, []byte("not an index"), 0o644) // git status fails now
	if err := removeWorktree(context.Background(), s.Run, true); err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the work was removed")
	}
}

// 2: a v1 run's costs still count once v2 saves its record, so a follow-up
// over the limit is refused.
func TestV1CostsOutliveTheUpgrade(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.4")
	id := time.Now().Format("060102-150405") + "-v1v1"
	dir := filepath.Join(f.home.Dir, "runs", id)
	os.MkdirAll(dir, 0o700)
	at := time.Now().UTC().Format(time.RFC3339)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte(fmt.Sprintf(
		`{"format": 1, "id": %q, "profile": "u", "harness": "fakeusage", "cwd": %q, "created": %q, "session": "s1",
		  "turns": [{"n": 1, "outcome": "success", "started": %q, "ended": %q, "attempts": 1, "exit_code": 0, "metrics": {"cost_usd": 1}}]}`,
		id, f.cwd, at, at, at)), 0o600)
	if _, err := f.r.PrepareSend(id, "more", TurnOptions{}); err == nil || OutcomeName(err) != "throttled" {
		t.Fatalf("a follow-up over the limit was queued: %v", err)
	}
	if moved, _ := filepath.Glob(filepath.Join(f.home.spendDir(), id+"-*-1-v1.json")); len(moved) != 1 {
		t.Fatal("the v1 turn's cost is not in the ledger")
	}
	if run, err := f.home.LoadRun(id); err != nil || run.Format != runFormat {
		t.Fatalf("the record was not upgraded: %v", err)
	}
}

// 3: a run id used again after its run is gone does not write over the
// first run's ledger entries.
func TestReusedRunIDKeepsEarlierCosts(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage")
	fixedRunIDs(t, "261012-000000-aaaa")
	f.start(context.Background(), "u", "first")
	if rep, err := f.home.GC(context.Background(), time.Now().Add(time.Hour), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	f.start(context.Background(), "u", "second")
	if s, err := f.home.spending(time.Now().Add(-time.Hour), "u", nil); err != nil || s.mine != 1 {
		t.Fatalf("spent %+v %v, want $1", s, err)
	}
}

// 4: a turn under no limit keeps the entry whose cost it could not write:
// a limit set afterwards sees the cost as unknown.
func TestUnlimitedTurnKeepsUnknownEntry(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage")
	testHookSpend = func(stage string) error {
		if stage == "close" {
			return errors.New("disk full")
		}
		return nil
	}
	rep := f.start(context.Background(), "u", "x")[0]
	testHookSpend = nil
	if rep.Outcome != "success" {
		t.Fatalf("%s %v", rep.Outcome, rep.Err)
	}
	f.home.SetConfig("spend_limit_usd", 5)
	f.home.SetConfigValue("spend_unknown", "refuse")
	if _, err := f.r.Start(context.Background(), "u", []string{"y"}, f.cwd, TurnOptions{}, nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("a limit set later did not see the unknown cost: %v", err)
	}
}

// 5: a run of an uncommitted creation whose queued turn was ended as lost
// without running is still rolled back.
func TestLostQueuedTurnIsStillAbandoned(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("writer", "harness: fake\neffect: non-idempotent\nmode: write\nopts: file=out.txt")
	repo := gitRepo(t)
	nr, err := f.r.check("writer", []string{"x"}, repo, TurnOptions{Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := f.r.create(context.Background(), nr, "lost")
	if err != nil {
		t.Fatal(err)
	}
	staged[0].release() // the creator died before it committed
	if _, err := f.home.updateRun(staged[0].ID, func(run *Run) error {
		run.Turns[0].Outcome, run.Turns[0].Ended = "transient", time.Now().UTC() // as Wait ends a lost one
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.home.GC(context.Background(), time.Now().AddDate(0, 0, -30), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if out, _ := git(context.Background(), "-C", repo, "branch", "--list", "harness/*"); out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
}

// 6: a ledger entry that is JSON but not an entry is unknown, never zero,
// and a turn number of 0 is not looked up.
func TestInvalidLedgerEntriesAreUnknown(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5\nspend_unknown: refuse")
	empty, err := f.home.NewRun("u", "fakeusage", f.cwd, "") // a run with no turn
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339)
	os.MkdirAll(f.home.spendDir(), 0o700)
	for _, entry := range []string{
		`{}`,
		`null`,
		fmt.Sprintf(`{"run": %q, "turn": 0, "attempt": 1, "profile": "u", "at": %q, "running": true}`, empty.ID, at),
		fmt.Sprintf(`{"run": "r", "turn": 1, "attempt": 1, "profile": "", "at": %q, "usd": 9}`, at),
		fmt.Sprintf(`{"run": "r", "turn": 1, "attempt": 1, "profile": "u", "at": %q, "usd": -9}`, at),
	} {
		path := filepath.Join(f.home.spendDir(), "r-1-1-x.json")
		os.WriteFile(path, []byte(entry), 0o600)
		if _, err := f.r.Start(context.Background(), "u", []string{"y"}, f.cwd, TurnOptions{}, nil); err == nil || !strings.Contains(err.Error(), "not a valid entry") {
			t.Fatalf("%s: %v", entry, err)
		}
		os.Remove(path)
	}
}
