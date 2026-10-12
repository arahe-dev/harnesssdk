package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// What a cost harness could not record counts as is spend_unknown: warn
// by default, a number of dollars, or refuse; a profile's wins over the
// machine's.
func TestSpendUnknownPolicies(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f) // $0.50 a turn
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.4")
	f.profile("strict", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.4\nspend_unknown: refuse")
	f.profile("lax", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.4\nspend_unknown: warn")
	for _, p := range []string{"u", "strict", "lax"} {
		// An attempt whose cost never reached the ledger: its turn is over.
		lost := &spendEntry{Run: "lost", Created: time.Now().UTC(), Turn: 1, Attempt: 1, Profile: p, At: time.Now().UTC(), Running: true}
		if err := f.home.writeSpend("open", lost); err != nil {
			t.Fatal(err)
		}
	}
	start := func(profile string) (Report, error) {
		reps, err := f.r.Start(context.Background(), profile, []string{time.Now().String()}, f.cwd, TurnOptions{}, nil)
		if err != nil {
			return Report{}, err
		}
		return reps[0], nil
	}

	rep, err := start("u") // the default: warn
	if err != nil || rep.Outcome != "success" || !strings.Contains(rep.Hint, "could not record") || !strings.Contains(rep.Hint, "spend_unknown") {
		t.Fatalf("warn: %v %s %q", err, rep.Outcome, rep.Hint)
	}
	if _, err := start("strict"); err == nil || !strings.Contains(err.Error(), "spend_unknown is refuse") || OutcomeName(err) != "throttled" {
		t.Fatalf("a profile's refuse: %v", err)
	}
	if err := f.home.SetConfigValue("spend_unknown", "refuse"); err != nil {
		t.Fatal(err)
	}
	if rep, err := start("lax"); err != nil || rep.Outcome != "success" {
		t.Fatalf("a profile's warn over the machine's refuse: %v", err)
	}
	if _, err := start("u"); err == nil || !strings.Contains(err.Error(), "spend_unknown is refuse") {
		t.Fatalf("the machine's refuse: %v", err)
	}
	if err := f.home.SetConfigValue("spend_unknown", "$0.45"); err != nil {
		t.Fatal(err)
	}
	if _, err := start("u"); err == nil || !strings.Contains(err.Error(), "counted as $0.45 each") {
		t.Fatalf("assumed dollars: %v", err)
	}

	for _, bad := range []string{"maybe", "-1", "NaN"} {
		if err := f.home.SetConfigValue("spend_unknown", bad); err == nil {
			t.Fatalf("spend_unknown %q was accepted", bad)
		}
		if _, err := ParseProfile("p", []byte("---\nharness: fake\nspend_unknown: "+bad+"\n---\n")); err == nil {
			t.Fatalf("profile spend_unknown %q was accepted", bad)
		}
	}
	if err := f.home.SetConfigValue("max_depth", "two"); err == nil {
		t.Fatal("a limit that is not a number was accepted")
	}
	if cfg, err := f.home.Config(); err != nil || cfg.SpendUnknown != "$0.45" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

// Only refuse keeps a turn under a limit from running when its ledger
// entry cannot be made; under warn it runs, its cost unknown.
func TestLedgerFailureFollowsThePolicy(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("warn", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5")
	f.profile("refuse", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5\nspend_unknown: refuse")
	testHookSpend = func(stage string) error {
		if stage == "open" {
			return errors.New("disk full")
		}
		return nil
	}
	defer func() { testHookSpend = nil }()
	if rep := f.start(context.Background(), "warn", "x")[0]; rep.Outcome != "success" {
		t.Fatalf("warn: %s %v", rep.Outcome, rep.Err)
	}
	if rep := f.start(context.Background(), "refuse", "x")[0]; rep.Err == nil || !strings.Contains(rep.Err.Error(), "spending ledger cannot be written") {
		t.Fatalf("refuse: %s %v", rep.Outcome, rep.Err)
	}
}
