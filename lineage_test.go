package harnesssdk

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestAncestors(t *testing.T) {
	up := ancestors(os.Getpid(), 8)
	if len(up) == 0 || up[0] == os.Getpid() {
		t.Fatalf("ancestors of %d: %v", os.Getpid(), up)
	}
	if up[0] != os.Getppid() {
		t.Fatalf("nearest ancestor %d, want the parent %d", up[0], os.Getppid())
	}
}

// A nested harness finds its parent run and lineage from the environment,
// and from process ancestry when a harness stripped the environment.
func TestNestedRunFindsItsParent(t *testing.T) {
	f := newFixture(t, map[string]int64{"fake": 2})
	for _, strip := range []string{"0", "1"} {
		f.profile("nest", "harness: fake\neffect: pure\nbudget: fake=1\nmode: nested\nopts: strip="+strip+" home="+f.home.Dir)
		rep := f.start(context.Background(), "nest", "x")[0]
		if rep.Err != nil {
			t.Fatal(rep.Err)
		}
		run, _ := f.home.LoadRun(rep.Run.ID)
		lineage := run.Turns[0].Lineage
		if len(lineage) != 1 || !strings.HasPrefix(lineage[0], "t") {
			t.Fatalf("recorded lineage %v", lineage)
		}
		found := map[string]string{"0": "env", "1": "ancestry"}[strip]
		want := "parent=" + rep.Run.ID + " lineage=" + lineage[0] + " found=" + found
		if rep.Output != want {
			t.Fatalf("strip=%s: got %q, want %q", strip, rep.Output, want)
		}
	}
}

func TestLineageAndHomePlaceholders(t *testing.T) {
	f := newFixture(t, map[string]int64{"fake": 1})
	f.def("fake", Definition{
		Args:   [][]string{{"args"}, {"--lineage", "{lineage}"}, {"--home", "{harness_home}"}},
		Output: OutputRule{Format: "text"},
	})
	f.profile("p", "harness: fake\neffect: pure\nbudget: fake=1")
	rep := f.start(context.Background(), "p", "x")[0]
	run, _ := f.home.LoadRun(rep.Run.ID)
	want := "flags: --lineage " + run.Turns[0].Lineage[0] + " --home " + f.home.Dir
	if rep.Err != nil || rep.Output != want {
		t.Fatalf("got %q (%v), want %q", rep.Output, rep.Err, want)
	}
	if err := CheckVar("lineage"); err == nil {
		t.Fatal("lineage must not be settable with --set")
	}
}

// Records are replaced by a rename while other processes read them; on
// Windows the two collide for an instant and must be retried, not fail.
func TestRecordReadsSurviveConcurrentReplace(t *testing.T) {
	h, err := NewHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.NewRun("p", "fake", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := range 300 {
			run.Session = strings.Repeat("x", i%7)
			if err := run.Save(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 300 {
		if _, err := h.LoadRun(run.ID); err != nil {
			t.Fatalf("read during replace: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("replace during read: %v", err)
	}
}
