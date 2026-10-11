//go:build windows

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Without a Job Object the tree is found by parent process ids: a timeout
// still kills the grandchild, and a CLI's leftovers die when it exits.
func TestJobFallback(t *testing.T) {
	forceNoJob = true
	t.Cleanup(func() { forceNoJob = false })
	f := newFixture(t, nil)

	pidfile := filepath.Join(t.TempDir(), "pid")
	f.profile("hang", "harness: fake\neffect: non-idempotent\ntimeout: 2s\nmode: tree\nopts: pidfile="+pidfile)
	rep := f.start(context.Background(), "hang", "x")[0]
	if rep.Outcome != "uncertain" {
		t.Fatalf("%+v", rep)
	}
	data, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(string(data))
	waitDead(t, pid)
	log, _ := os.ReadFile(rep.Run.Path("turn-1.stderr.log"))
	if !strings.Contains(string(log), "no job object") {
		t.Fatalf("the attempt log should record the fallback:\n%s", log)
	}

	// Review finding 3: a grandchild whose intermediate parent exited is
	// still found, because the tree was tracked while it ran.
	chain := filepath.Join(t.TempDir(), "pid")
	f.profile("chain", "harness: fake\neffect: non-idempotent\ntimeout: 3s\nmode: chain\nopts: pidfile="+chain)
	if rep := f.start(context.Background(), "chain", "x")[0]; rep.Outcome != "uncertain" {
		t.Fatalf("%+v", rep)
	}
	data, _ = os.ReadFile(chain)
	pid, _ = strconv.Atoi(string(data))
	waitDead(t, pid)

	orphan := filepath.Join(t.TempDir(), "pid")
	f.profile("orphan", "harness: fake\neffect: pure\nmode: orphan\nopts: pidfile="+orphan)
	if rep := f.start(context.Background(), "orphan", "x")[0]; rep.Err != nil {
		t.Fatal(rep.Err)
	}
	data, _ = os.ReadFile(orphan)
	pid, _ = strconv.Atoi(string(data))
	waitDead(t, pid)
}
