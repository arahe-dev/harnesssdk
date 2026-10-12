package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the eighth review of v2.0.0-next.

// 1, 2: a rollback keeps an untracked file that git is configured not to
// show, and a folder with files but no worktree; it removes an empty one.
func TestRollbackKeepsWhatGitDoesNotShow(t *testing.T) {
	h := newGateHome(t)
	repo := gitRepo(t)
	stage := func() *stagedRun {
		s, err := h.stageRun(context.Background(), "p", "fake", repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.addWorktree(context.Background(), s.Run); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.release)
		return s
	}
	if _, err := git(context.Background(), "-C", repo, "config", "status.showUntrackedFiles", "no"); err != nil {
		t.Fatal(err)
	}
	s := stage()
	hidden := filepath.Join(s.Worktree, "hidden.txt")
	os.WriteFile(hidden, []byte("work"), 0o644)
	if err := removeWorktree(context.Background(), s.Run, true); err == nil {
		t.Fatal("a worktree with an untracked file was removed")
	}
	if _, err := os.Stat(hidden); err != nil {
		t.Fatal("the untracked file was lost")
	}

	s = stage()
	os.Remove(filepath.Join(s.Worktree, ".git")) // its git identity is lost
	if err := removeWorktree(context.Background(), s.Run, true); err == nil || !strings.Contains(err.Error(), "no worktree") {
		t.Fatalf("a folder with files was removed: %v", err)
	}

	empty := &Run{ID: "e", Cwd: repo, Worktree: filepath.Join(t.TempDir(), "empty")}
	os.Mkdir(empty.Worktree, 0o700)
	if err := removeWorktree(context.Background(), empty, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty.Worktree); !os.IsNotExist(err) {
		t.Fatal("an empty folder was kept")
	}
}

// v1Run writes a literal v1 record of a turn that ended at ended and cost
// cost.
func v1Run(t *testing.T, h *Home, id, profile, cwd string, ended time.Time, cost float64) string {
	t.Helper()
	dir := filepath.Join(h.Dir, "runs", id)
	os.MkdirAll(dir, 0o700)
	created := ended.Add(-time.Minute).UTC().Format(time.RFC3339)
	at := ended.UTC().Format(time.RFC3339)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte(fmt.Sprintf(
		`{"format": 1, "id": %q, "profile": %q, "harness": "fakeusage", "cwd": %q, "created": %q, "session": "s1",
		  "turns": [{"n": 1, "outcome": "success", "started": %q, "ended": %q, "attempts": 1, "exit_code": 0, "metrics": {"cost_usd": %g}}]}`,
		id, profile, cwd, created, created, at, cost)), 0o600)
	return dir
}

// 3, 4: a v1 cost counts once whether it is in the record, in the ledger,
// or both (a move that did not finish), and survives gc of its record.
func TestV1CostsCountOnce(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.5")
	ended := time.Now().Add(-2 * time.Hour)
	id := ended.Format("060102-150405") + "-v1v1"
	v1Run(t, f.home, id, "u", f.cwd, ended, 0.6)
	run, err := f.home.LoadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := moveV1Spend(f.home.Dir, run); err != nil { // moved, but the record never saved
		t.Fatal(err)
	}
	if s, err := f.home.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || s.mine != 0.6 {
		t.Fatalf("spent %+v %v, want 0.6", s, err)
	}
	os.Remove(v1SpendPath(f.home.Dir, run, 1))
	if rep, err := f.home.GC(context.Background(), time.Now().Add(-time.Hour), false); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if _, err := f.r.Start(context.Background(), "u", []string{"x"}, f.cwd, TurnOptions{}, nil); err == nil || OutcomeName(err) != "throttled" {
		t.Fatalf("the collected v1 run's cost no longer counts: %v", err)
	}
}

// 5: an entry left running by a run is unknown even while a later run with
// the same id runs a turn of the same number.
func TestUnknownEntryOfAnEarlierRunWithTheID(t *testing.T) {
	h := newGateHome(t)
	fixedRunIDs(t, "261012-000000-aaaa")
	run, err := h.NewRun("q", "fake", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.updateRun(run.ID, func(r *Run) error {
		r.Turns = []TurnRecord{{N: 1, Outcome: "running", Started: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lk, err := lockOpen(run.Path("exec.lock")) // it is executing
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	earlier := &spendEntry{Run: run.ID, Created: run.Created.Add(-time.Hour), Turn: 1, Attempt: 1, Profile: "u", At: time.Now().UTC(), Running: true}
	if err := h.writeSpend("open", earlier); err != nil {
		t.Fatal(err)
	}
	if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || len(s.unknownMine) == 0 {
		t.Fatalf("the earlier run's entry was taken for the running turn: %+v %v", s, err)
	}
}

// 6: a closed entry without a cost, or with a null one, is not valid.
func TestEntryWithoutCostIsUnknown(t *testing.T) {
	h := newGateHome(t)
	at := time.Now().UTC().Format(time.RFC3339)
	os.MkdirAll(h.spendDir(), 0o700)
	for _, usd := range []string{``, `, "usd": null`} {
		path := filepath.Join(h.spendDir(), "r-1-1-x.json")
		os.WriteFile(path, []byte(fmt.Sprintf(`{"run": "r", "created": %q, "turn": 1, "attempt": 1, "profile": "u", "at": %q%s}`, at, at, usd)), 0o600)
		if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || !strings.Contains(strings.Join(s.unknownMine, " "), "not a valid entry") {
			t.Fatalf("%q: %+v %v", usd, s, err)
		}
		os.Remove(path)
	}
}
