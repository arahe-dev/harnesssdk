//go:build unix

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	harness "github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// SIGTERM to the harness process cancels the run like Ctrl-C: the agent's
// whole process tree dies and the run is recorded as cancelled.
func TestSIGTERMCancelsRun(t *testing.T) {
	home := t.TempDir()
	exe, _ := os.Executable()
	pidfile := filepath.Join(t.TempDir(), "pid")
	def, _ := json.Marshal(map[string]any{
		"command": exe,
		"args":    [][]string{{"tree"}, {"{pidfile}"}},
		"env":     map[string]string{"HARNESS_TEST_FAKE": "1"},
		"output":  map[string]string{"format": "text"},
	})
	t.Setenv("HARNESS_HOME", home)
	if code, _, errs := harnessCLI(t, string(def), "def", "set", "fake", "--file", "-"); code != 0 {
		t.Fatal(errs)
	}
	profile := "---\nharness: fake\neffect: pure\npidfile: " + pidfile + "\n---\n"
	if code, _, errs := harnessCLI(t, profile, "profile", "set", "p", "--file", "-"); code != 0 {
		t.Fatal(errs)
	}

	cmd := exec.Command(exe, "run", "p", "hi")
	cmd.Env = append(os.Environ(), "HARNESS_TEST_CLI=1", "HARNESS_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(pidfile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the agent never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != harness.ExitCancelled {
		t.Fatalf("exit: %v, want %d", err, harness.ExitCancelled)
	}
	data, _ := os.ReadFile(pidfile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived SIGTERM", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, out, _ := harnessCLI(t, "", "runs"); code != 0 || !strings.Contains(out, "cancelled") {
		t.Fatalf("runs:\n%s", out)
	}
}
