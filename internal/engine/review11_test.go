package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the eleventh review of v2.0.0-next.

// A run record that cannot be read may hold a v1 cost: turns under a limit
// are refused; a folder whose record is gone (collected) is not one.
func TestUnreadableV1RecordRefuses(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("u", "harness: fakeusage\neffect: pure\nmode: usage\nspend_limit_usd: 0.5")
	ended := time.Now().Add(-2 * time.Hour)
	record := filepath.Join(v1Run(t, f.home, ended.Format("060102-150405")+"-v1v1", "u", f.cwd, ended, 0.6), "run.json")
	for name, spoil := range map[string]func(){
		"half written": func() { os.WriteFile(record, []byte(`{"format": 1, "turns": [`), 0o600) },
		"not a file":   func() { os.Remove(record); os.Mkdir(record, 0o700) },
	} {
		spoil()
		if _, err := f.r.Start(context.Background(), "u", []string{name}, f.cwd, TurnOptions{}, nil); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Fatalf("%s: %v", name, err)
		}
		os.RemoveAll(record)
	}
	if rep := f.start(context.Background(), "u", "gone")[0]; rep.Outcome != "success" {
		t.Fatalf("a folder without a record refused: %v", rep.Err)
	}
}
