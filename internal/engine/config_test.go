package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyFilesStillRead(t *testing.T) {
	h := newGateHome(t)
	// budgets.json and trust.json as v1.2 wrote them: the payload alone.
	if err := os.WriteFile(h.budgetsPath(), []byte(`{"pi": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.trustPath(), []byte(`{"/p/.harness": {"profiles/a.md": "abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := h.Budgets()
	if err != nil || b["pi"] != 2 {
		t.Fatalf("legacy budgets: %v %v", b, err)
	}
	tr, err := h.readTrust()
	if err != nil || tr["/p/.harness"]["profiles/a.md"] != "abc" {
		t.Fatalf("legacy trust: %v %v", tr, err)
	}
	// A budget named "format" in a legacy file is still a budget.
	if err := os.WriteFile(h.budgetsPath(), []byte(`{"format": 3, "budgets": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := h.Budgets(); err != nil || b["format"] != 3 || b["budgets"] != 2 {
		t.Fatalf("legacy budgets named format: %v %v", b, err)
	}
	// Rewriting upgrades the file and keeps its content.
	if err := h.SetBudget("pi", 4); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(h.budgetsPath())
	if !strings.Contains(string(data), `"format": 1`) {
		t.Fatalf("budgets not versioned: %s", data)
	}
	if b, err := h.Budgets(); err != nil || b["pi"] != 4 || b["format"] != 3 {
		t.Fatalf("after upgrade: %v %v", b, err)
	}
}

func TestNewerFormatsAreRefused(t *testing.T) {
	h := newGateHome(t)
	for _, c := range []struct {
		path, data string
		read       func() error
	}{
		{h.budgetsPath(), `{"format": 9, "budgets": {"pi": 1}}`, func() error { _, err := h.Budgets(); return err }},
		{h.trustPath(), `{"format": 9, "projects": {}}`, func() error { _, err := h.readTrust(); return err }},
		{h.configPath(), `{"format": 9}`, func() error { _, err := h.Config(); return err }},
	} {
		if err := os.WriteFile(c.path, []byte(c.data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.read(); err == nil || !strings.Contains(err.Error(), "newer harness") {
			t.Errorf("%s: err = %v, want a newer-harness refusal", filepath.Base(c.path), err)
		}
		os.Remove(c.path)
	}
	if _, err := ParseDefinition("x", []byte(`{"format": 9, "command": "x", "output": {"format": "text"}}`)); err == nil || !strings.Contains(err.Error(), "newer harness") {
		t.Errorf("definition: err = %v", err)
	}
	dir := filepath.Join(h.Dir, "runs", "r1")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"format": 9, "id": "r1"}`), 0o600)
	if _, err := h.LoadRun("r1"); err == nil || !strings.Contains(err.Error(), "newer harness") {
		t.Errorf("run: err = %v", err)
	}
}

func TestConfig(t *testing.T) {
	h := newGateHome(t)
	if c, err := h.Config(); err != nil || c != DefaultConfig() {
		t.Fatalf("default config: %+v %v", c, err)
	}
	if err := h.SetConfig("max_depth", 5); err != nil {
		t.Fatal(err)
	}
	if err := h.SetConfig("nope", 1); err == nil {
		t.Fatal("unknown key accepted")
	}
	if err := h.SetConfig("max_running", -1); err == nil {
		t.Fatal("negative value accepted")
	}
	c, err := h.Config()
	if err != nil || c.MaxDepth != 5 || c.MaxRunning != DefaultConfig().MaxRunning {
		t.Fatalf("config: %+v %v", c, err)
	}
}

func TestGateWritesOnlyChanges(t *testing.T) {
	h := newGateHome(t)
	g := fastGate(h)
	c := Claim{Name: "b", Units: 1, Capacity: 1}
	a, err := g.Acquire(context.Background(), []Claim{c}, false, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := g.Acquire(ctx, []Claim{c}, false, "b")
		done <- err
	}()
	state := filepath.Join(g.ledger("b"), "state.json")
	time.Sleep(50 * time.Millisecond) // b is queued
	before, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // ~10 polls that change nothing
	after, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a waiter that changed nothing rewrote the ledger")
	}
	cancel()
	<-done
}

func TestGateContentionIsWaiting(t *testing.T) {
	h := newGateHome(t)
	g := fastGate(h)
	c := Claim{Name: "b", Units: 1, Capacity: 1}
	os.MkdirAll(g.ledger("b"), 0o700)
	// Another process sits on the ledger's lock longer than one pass waits.
	lk, err := lockOpen(filepath.Join(g.ledger("b"), "state.lock"))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error)
	go func() {
		tk, err := g.Acquire(context.Background(), []Claim{c}, false, "x")
		if err == nil {
			tk.Release()
		}
		got <- err
	}()
	time.Sleep(2500 * time.Millisecond)
	lk.Close()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("contention failed the request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not granted after the lock was released")
	}
	// Without waiting, contention is a refusal.
	lk, _ = lockOpen(filepath.Join(g.ledger("b"), "state.lock"))
	defer lk.Close()
	if _, err := g.Acquire(context.Background(), []Claim{c}, true, "y"); err == nil {
		t.Fatal("reject request granted through a held lock")
	}
}
