package harnesssdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newGateHome(t *testing.T) *Home {
	t.Helper()
	h, err := NewHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fastGate(h *Home, lineage ...string) *Gate {
	g := h.Gate(lineage)
	g.poll = 10 * time.Millisecond
	return g
}

func TestGateFIFO(t *testing.T) {
	g := fastGate(newGateHome(t))
	c := Claim{Name: "b", Units: 1, Capacity: 1}
	a, err := g.Acquire(context.Background(), []Claim{c}, false, "a")
	if err != nil {
		t.Fatal(err)
	}
	granted := make(chan string, 2)
	for _, label := range []string{"b", "c"} {
		go func() {
			tk, err := g.Acquire(context.Background(), []Claim{c}, false, label)
			if err != nil {
				t.Error(err)
				return
			}
			granted <- label
			time.Sleep(30 * time.Millisecond)
			tk.Release()
		}()
		time.Sleep(50 * time.Millisecond) // b queues before c
	}
	select {
	case l := <-granted:
		t.Fatalf("%s was granted while a held the only unit", l)
	case <-time.After(100 * time.Millisecond):
	}
	a.Release()
	if first, second := <-granted, <-granted; first != "b" || second != "c" {
		t.Fatalf("granted %s then %s, want FIFO b then c", first, second)
	}
}

func TestGateRejectCancelAndDuplicates(t *testing.T) {
	g := fastGate(newGateHome(t))
	c := Claim{Name: "b", Units: 1, Capacity: 1}
	holder, err := g.Acquire(context.Background(), []Claim{c}, false, "holder")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := g.Acquire(context.Background(), []Claim{c}, true, "rejecter"); !errors.Is(err, errBusy) {
		t.Fatalf("reject: got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, []Claim{c}, false, "waiter"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: got %v", err)
	}
	st, _ := readGate(g.ledger("b"))
	if len(st.Queue) != 0 || len(st.Held) != 1 {
		t.Fatalf("a refused or cancelled request must leave the ledger: %+v", st)
	}
	if _, err := g.Acquire(context.Background(), []Claim{c, c}, false, "dup"); err == nil {
		t.Fatal("a name claimed twice must be refused, not wait forever")
	}
}

// A request for several names waits holding none of them.
func TestGateAllOrNothing(t *testing.T) {
	h := newGateHome(t)
	g := fastGate(h)
	z, err := g.Acquire(context.Background(), []Claim{{Name: "z", Units: 1, Capacity: 1}}, false, "z-holder")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Acquire(ctx, []Claim{{Name: "a", Units: 1, Capacity: 1}, {Name: "z", Units: 1, Capacity: 1}}, false, "both")
	}()
	time.Sleep(100 * time.Millisecond)
	st, _ := readGate(g.ledger("a"))
	if len(st.Held) != 0 {
		t.Fatalf("a waiting request holds a: %+v", st)
	}
	cancel()
	<-done
	z.Release()
}

// A ticket whose owner died (its lock file is not held) is dropped.
func TestGateReclaimsDeadTickets(t *testing.T) {
	g := fastGate(newGateHome(t))
	dir := g.ledger("b")
	os.MkdirAll(dir, 0o700)
	os.MkdirAll(filepath.Join(g.dir, "live"), 0o700)
	dead := gateState{Held: []gateEntry{{ID: "deadbeef", Units: 1, PID: 999999, Label: "crashed"}}}
	data, _ := json.Marshal(dead)
	os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600)
	os.WriteFile(g.liveFile("deadbeef"), nil, 0o600) // not locked: its owner is gone
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tk, err := g.Acquire(ctx, []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "next")
	if err != nil {
		t.Fatalf("the dead holder's unit was not reclaimed: %v", err)
	}
	tk.Release()
}

func TestGateNestedSameBudget(t *testing.T) {
	h := newGateHome(t)
	parent, err := fastGate(h).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Release()
	if _, err := fastGate(h, parent.ID).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "child"); !errors.Is(err, ErrWouldDeadlock) {
		t.Fatalf("capacity 1 held by the parent: got %v, want ErrWouldDeadlock", err)
	}
}

// The review's three-run cycle: capacities a=1, z=2; the parent holds z=1;
// an unrelated run wants a=1,z=2; the parent's child wants a=1,z=1. With
// all-or-nothing grants the unrelated run holds nothing while it waits, and
// the nested child does not queue behind it.
func TestGateNestedAcrossBudgets(t *testing.T) {
	h := newGateHome(t)
	a := Claim{Name: "a", Units: 1, Capacity: 1}
	parent, err := fastGate(h).Acquire(context.Background(), []Claim{{Name: "z", Units: 1, Capacity: 2}}, false, "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Release()
	octx, ocancel := context.WithCancel(context.Background())
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		fastGate(h).Acquire(octx, []Claim{a, {Name: "z", Units: 2, Capacity: 2}}, false, "unrelated")
	}()
	defer func() { ocancel(); <-otherDone }() // its lock file must close before cleanup
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	child, err := fastGate(h, parent.ID).Acquire(ctx, []Claim{a, {Name: "z", Units: 1, Capacity: 2}}, false, "child")
	if err != nil {
		t.Fatalf("the nested child deadlocked: %v", err)
	}
	child.Release()
}

func TestGateStatus(t *testing.T) {
	h := newGateHome(t)
	h.SetBudget("pi", 2)
	tk, err := h.Gate(nil).Acquire(context.Background(), []Claim{{Name: "pi", Units: 1, Capacity: 2}}, false, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer tk.Release()
	gates, err := h.GateStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Used != 1 || gates[0].Capacity != 2 || gates[0].Held[0] != "run-1" {
		t.Fatalf("%+v", gates)
	}
}
