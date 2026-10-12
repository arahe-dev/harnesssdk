package engine

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Regression tests for the second review of v2.0.0-next.1.

// 1: a ledger folder that cannot be read is one unknown cost, and none if
// it can be read by spending's second reading.
func TestUnreadableLedgerFolderCountsOnce(t *testing.T) {
	h := newGateHome(t)
	e := &spendEntry{Run: "r", Created: time.Now().UTC(), Turn: 1, Attempt: 1, Profile: "u", At: time.Now().UTC(), USD: dollars(0.3)}
	if err := h.writeSpend("close", e); err != nil {
		t.Fatal(err)
	}
	failing := true
	testHookLedgerRead = func(files []os.DirEntry, err error) ([]os.DirEntry, error) {
		if failing {
			return nil, errors.New("access denied")
		}
		return files, err
	}
	defer func() { testHookLedgerRead, testHookSpending = nil, nil }()
	if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || len(s.unknownAll) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	testHookSpending = func() { failing = false } // readable again before the second reading
	if s, err := h.spending(time.Now().Add(-spendWindow), "u", nil); err != nil || len(s.unknownAll) != 0 || s.mine != 0.3 {
		t.Fatalf("%+v %v", s, err)
	}
}

// 2, 3: a turn's own entry whose cost could not be written counts once,
// even when its file cannot be read either, and only within the window.
func TestOwnUnwrittenEntryCountsOnce(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 5\nspend_unknown: 1")
	p, _, err := f.r.load("u", f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	e := &spendEntry{Run: "r", Created: time.Now().UTC(), Turn: 1, Attempt: 1, Profile: "u", At: time.Now().UTC(), USD: dollars(0.2)}
	os.MkdirAll(f.home.spendDir(), 0o700)
	os.WriteFile(f.home.spendPath(e), []byte("{half"), 0o600) // neither replaced nor readable
	_, hint, err := f.home.checkSpend(p, []*spendEntry{e})
	if err != nil || !strings.Contains(hint, "count 1 cost(s)") {
		t.Fatalf("%q %v", hint, err)
	}
	e.At = time.Now().Add(-25 * time.Hour).UTC()
	os.Remove(f.home.spendPath(e))
	if _, hint, err := f.home.checkSpend(p, []*spendEntry{e}); err != nil || hint != "" {
		t.Fatalf("an entry from before the window counted: %q %v", hint, err)
	}
}
