package engine

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the first review of v2.0.0-next.1, against the
// ledger's guarantees: every attempt's cost counts once (its own if
// written, else as spend_unknown says); an attempt's ledger handling
// follows the rule it was admitted by; an unknown cost is always shown.

// failSpend fails the first n writes of stage ("open" or "close").
func failSpend(t *testing.T, stage string, n int32) {
	var calls atomic.Int32
	testHookSpend = func(s string) error {
		if s == stage && calls.Add(1) <= n {
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { testHookSpend = nil })
}

// 1, 4: an attempt whose entry could not be made still has its cost
// written once the ledger can be, and the turn says what happened.
func TestEntryMadeLaterKeepsTheCost(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5\nspend_unknown: 1")
	failSpend(t, "open", 1)
	rep := f.start(context.Background(), "u", "x")[0]
	if rep.Outcome != "success" || !strings.Contains(rep.Hint, "spending ledger could not be written") || !strings.Contains(rep.Hint, "spend_unknown") {
		t.Fatalf("%s %v %q", rep.Outcome, rep.Err, rep.Hint)
	}
	if s, err := f.home.spending(time.Now().Add(-time.Hour), "u"); err != nil || s.mine != 0.5 || len(s.unknownMine) != 0 {
		t.Fatalf("spent %+v %v, want $0.50 known", s, err)
	}
}

// 2, 3: a retry counts the attempts before it whose costs are not written
// (spend_unknown dollars each), and a cost written late counts once.
func TestRetryCountsItsUnwrittenAttempts(t *testing.T) {
	for _, c := range []struct {
		name    string
		limit   string
		outcome string
	}{
		{"refused by the assumed cost", "0.4", "throttled"},
		{"written late, counted once", "5", "success"},
	} {
		f := newFixture(t, nil)
		usageDef(f) // attempt 1: $0.25 and a transient failure; attempt 2: $0.50
		state := filepath.Join(t.TempDir(), "state")
		f.profile("u", "harness: fakeusage\neffect: idempotent\nretry: transient x2\nbackoff: 1s\nmode: usage\nopts: state="+state+
			"\nspend_limit_usd: "+c.limit+"\nspend_unknown: 1")
		failSpend(t, "close", 1) // attempt 1's cost is not written then
		rep := f.start(context.Background(), "u", "x")[0]
		if rep.Outcome != c.outcome || !strings.Contains(rep.Hint, "could not be written") {
			t.Fatalf("%s: %s %v %q", c.name, rep.Outcome, rep.Err, rep.Hint)
		}
		s, err := f.home.spending(time.Now().Add(-time.Hour), "u")
		want := map[string]float64{"0.4": 0.25, "5": 0.75}[c.limit]
		if err != nil || math.Abs(s.mine-want) > 1e-9 || len(s.unknownMine) != 0 {
			t.Fatalf("%s: spent %+v %v, want $%.2f known", c.name, s, err, want)
		}
	}
}

// 5: the rule an attempt is admitted by is the one read after it was
// admitted, and its ledger handling follows it.
func TestAttemptFollowsTheRuleItWasAdmittedBy(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nbudget: b=1\nmode: usage\nspend_limit_usd: 5")
	held, err := f.home.Gate(nil).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Report)
	go func() { done <- f.start(context.Background(), "u", "x")[0] }()
	time.Sleep(500 * time.Millisecond) // queued under warn
	if err := f.home.SetConfigValue("spend_unknown", "refuse"); err != nil {
		t.Fatal(err)
	}
	failSpend(t, "open", 100)
	held.Release()
	rep := <-done
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "spend_unknown is refuse") {
		t.Fatalf("%s %v", rep.Outcome, rep.Err)
	}
}
