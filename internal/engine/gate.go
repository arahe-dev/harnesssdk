package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The gate arbitrates budgets and serialize keys across every harness
// process on the machine, so a limit holds however many parents start runs
// at once. Each name is a ledger in <home>/gate/<name>/: a state file,
// changed only under a short lock, that lists granted tickets and a FIFO
// queue. Its rules are those of a Herta budget (CONTRACT §4.1): a request is
// granted when it fits and nothing is queued ahead of it, so a heavy request
// never starves.
//
// A request for several names is granted all at once or not at all: a
// waiting request holds nothing, so no cycle of waiters can form. The owner
// of a ticket keeps <home>/gate/live/<id>.live locked while it waits or
// holds; when its last holder dies the operating system drops that lock,
// and the next reader of a ledger removes the dead ticket. On Unix the agent
// CLI inherits the lock, so a ticket outlives a crashed worker for as long
// as the agent it admitted is still running.
//
// Nested runs (an agent that runs harness itself) carry their ancestors'
// ticket ids in HARNESS_LINEAGE. Their ancestors are blocked waiting for
// them, so a nested request skips the queues, and fails at once if a budget
// could only fit it after its ancestors finish.

// ErrWouldDeadlock means a nested run asked for more of a budget than its
// own ancestors leave free: it could only be granted after they finish, and
// they are waiting for it.
var ErrWouldDeadlock = errors.New("would wait forever on its own ancestors")

// Claim is units of one gate name with the capacity it is checked against.
type Claim struct {
	Name     string
	Units    int64
	Capacity int64
}

// keyClaim is the gate name for a serialize key: one holder at a time.
func keyClaim(key string) Claim {
	sum := sha256.Sum256([]byte(key))
	return Claim{Name: "key-" + hex.EncodeToString(sum[:8]), Units: 1, Capacity: 1}
}

type gateEntry struct {
	ID    string    `json:"id"`
	Units int64     `json:"units"`
	PID   int       `json:"pid"`
	Label string    `json:"label,omitempty"`
	Since time.Time `json:"since"`
}

type gateState struct {
	Format int         `json:"format"`
	Held   []gateEntry `json:"held"`
	Queue  []gateEntry `json:"queue"`
}

// Gate is the machine-wide arbiter in one data folder.
type Gate struct {
	dir     string
	lineage map[string]bool
	poll    time.Duration
}

// Gate returns the arbiter. lineage lists the ticket ids held by the
// harness runs this process was started from.
func (h *Home) Gate(lineage []string) *Gate {
	g := &Gate{dir: filepath.Join(h.Dir, "gate"), lineage: map[string]bool{}, poll: 200 * time.Millisecond}
	for _, id := range lineage {
		if id != "" {
			g.lineage[id] = true
		}
	}
	return g
}

// Ticket is a granted gate request.
type Ticket struct {
	ID string
	// Live is the ticket's locked liveness file. On Unix, pass it to the
	// agent CLI so the ticket stays held while the agent runs.
	Live    *os.File
	release func()
}

// Release returns the ticket's units.
func (t *Ticket) Release() {
	if t != nil && t.release != nil {
		t.release()
		t.release = nil
	}
}

func (g *Gate) ledger(name string) string { return filepath.Join(g.dir, name) }
func (g *Gate) liveFile(id string) string { return filepath.Join(g.dir, "live", id+".live") }

// Acquire takes every claim at once, or waits holding none of them. With
// reject it does not wait: a request that is not granted at once fails with
// errBusy. No claims is an immediate empty ticket.
func (g *Gate) Acquire(ctx context.Context, claims []Claim, reject bool, label string) (*Ticket, error) {
	if len(claims) == 0 {
		return &Ticket{}, nil
	}
	claims = slices.Clone(claims)
	slices.SortFunc(claims, func(a, b Claim) int { return strings.Compare(a.Name, b.Name) })
	for i, c := range claims {
		if i > 0 && claims[i-1].Name == c.Name {
			return nil, fmt.Errorf("gate: %s is claimed twice", c.Name)
		}
		if c.Units < 1 || c.Units > c.Capacity {
			return nil, fmt.Errorf("gate: %s: %d units requested, capacity is %d", c.Name, c.Units, c.Capacity)
		}
	}
	if err := os.MkdirAll(filepath.Join(g.dir, "live"), 0o700); err != nil {
		return nil, err
	}
	// The "t" keeps an id from ever reading as a number where a CLI parses
	// it as TOML or JSON (a definition can pass {lineage} on).
	id := "t" + randHex(8)
	live, err := lockOpen(g.liveFile(id))
	if err != nil {
		return nil, err
	}
	me := gateEntry{ID: id, PID: os.Getpid(), Label: label, Since: time.Now().UTC()}
	drop := func() {
		g.update(claims, func(sts []*gateState) {
			for _, st := range sts {
				st.remove(id)
			}
		})
	}
	leave := func() {
		drop()
		live.Close()
		os.Remove(g.liveFile(id))
	}
	for queued := false; ; {
		granted, verdict, err := g.try(claims, me, !queued, reject)
		switch {
		case errors.Is(err, errBusy) && !reject:
			// A ledger stayed locked by others all through this pass:
			// contention is part of waiting, never a failure. The request
			// is queued by the first pass that gets through.
		case err != nil:
			leave()
			return nil, err
		case granted:
			return &Ticket{ID: id, Live: live, release: leave}, nil
		case verdict != nil:
			leave()
			return nil, verdict
		default:
			queued = true
		}
		select {
		case <-ctx.Done():
			leave()
			return nil, ctx.Err()
		case <-time.After(g.poll):
		}
	}
}

// try makes one pass over the ledgers: it enqueues me on the first pass and
// grants me everything if every ledger allows it. verdict is a final
// refusal.
func (g *Gate) try(claims []Claim, me gateEntry, first, reject bool) (granted bool, verdict, err error) {
	nested := len(g.lineage) > 0
	err = g.update(claims, func(sts []*gateState) {
		ok := true
		for i, c := range claims {
			st := sts[i]
			if first {
				e := me
				e.Units = c.Units
				st.Queue = append(st.Queue, e)
			}
			var used, ancestors int64
			for _, e := range st.Held {
				used += e.Units
				if g.lineage[e.ID] {
					ancestors += e.Units
				}
			}
			if c.Units > c.Capacity-ancestors {
				verdict = fmt.Errorf("%s: needs %d of %d units but its ancestor runs hold %d: %w",
					c.Name, c.Units, c.Capacity, ancestors, ErrWouldDeadlock)
				return
			}
			head := len(st.Queue) > 0 && st.Queue[0].ID == me.ID
			if c.Capacity-used < c.Units || !(head || nested) {
				ok = false
			}
		}
		switch {
		case ok:
			for i, c := range claims {
				st := sts[i]
				st.remove(me.ID)
				e := me
				e.Units = c.Units
				st.Held = append(st.Held, e)
			}
			granted = true
		case reject:
			verdict = errBusy
		}
	})
	return granted, verdict, err
}

// update changes ledgers under their locks, taken in name order, after
// dropping dead tickets.
func (g *Gate) update(claims []Claim, change func([]*gateState)) error {
	var locks []*os.File
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Close()
		}
	}()
	sts := make([]*gateState, len(claims))
	before := make([][]byte, len(claims))
	for i, c := range claims {
		dir := g.ledger(c.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		lk, err := lockWaitOpen(filepath.Join(dir, "state.lock"), 2*time.Second)
		if err != nil {
			return fmt.Errorf("gate %s: %w", c.Name, err)
		}
		locks = append(locks, lk)
		st, err := readGate(dir)
		if err != nil {
			return err
		}
		before[i], _ = json.Marshal(st)
		st.prune(g)
		sts[i] = st
	}
	change(sts)
	// Write only what changed: waiters poll, and most passes change
	// nothing.
	for i, c := range claims {
		sts[i].Format = formatVersion
		after, _ := json.Marshal(sts[i])
		if string(after) == string(before[i]) {
			continue
		}
		if err := writeFileAtomic(filepath.Join(g.ledger(c.Name), "state.json"), append(after, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func readGate(dir string) (*gateState, error) {
	data, err := readShared(filepath.Join(dir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return &gateState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st gateState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("gate %s: %w", filepath.Base(dir), err)
	}
	if err := checkFormat("gate "+filepath.Base(dir), st.Format); err != nil {
		return nil, err
	}
	return &st, nil
}

// alive reports whether a ticket's owner, or an agent that inherited its
// lock, still holds it, and removes the liveness file of a dead one.
func (g *Gate) alive(id string) bool {
	unlock, err := lockFile(g.liveFile(id))
	if errors.Is(err, errBusy) {
		return true
	}
	if err == nil {
		unlock()
		os.Remove(g.liveFile(id))
	}
	return false
}

// held is alive without the cleanup, for readers.
func (g *Gate) held(id string) bool {
	if _, err := os.Stat(g.liveFile(id)); err != nil {
		return false
	}
	unlock, err := lockFile(g.liveFile(id))
	if err == nil {
		unlock()
	}
	return errors.Is(err, errBusy)
}

// prune drops dead tickets.
func (st *gateState) prune(g *Gate) {
	st.Held = slices.DeleteFunc(st.Held, func(e gateEntry) bool { return !g.alive(e.ID) })
	st.Queue = slices.DeleteFunc(st.Queue, func(e gateEntry) bool { return !g.alive(e.ID) })
}

func (st *gateState) remove(id string) {
	match := func(e gateEntry) bool { return e.ID == id }
	st.Held = slices.DeleteFunc(st.Held, match)
	st.Queue = slices.DeleteFunc(st.Queue, match)
}

// GateInfo is one ledger's current state, for harness status.
type GateInfo struct {
	Name     string
	Capacity int64
	Used     int64
	Held     []string // labels
	Queued   []string
}

// GateStatus lists every budget and every serialize key in use.
func (h *Home) GateStatus() ([]GateInfo, error) {
	budgets, err := h.Budgets()
	if err != nil {
		return nil, err
	}
	g := h.Gate(nil)
	names := map[string]bool{}
	for n := range budgets {
		names[n] = true
	}
	if entries, err := os.ReadDir(g.dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != "live" {
				names[e.Name()] = true
			}
		}
	}
	var out []GateInfo
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range names {
			if !yield(k) {
				return
			}
		}
	}) {
		info := GateInfo{Name: n, Capacity: budgets[n]}
		if strings.HasPrefix(n, "key-") {
			info.Capacity = 1
		}
		// Read without the lock: ledgers are replaced whole, and status
		// must not queue behind the waiters it reports on.
		st, err := readGate(g.ledger(n))
		if err != nil {
			return nil, err
		}
		st.Held = slices.DeleteFunc(st.Held, func(e gateEntry) bool { return !g.held(e.ID) })
		st.Queue = slices.DeleteFunc(st.Queue, func(e gateEntry) bool { return !g.held(e.ID) })
		if strings.HasPrefix(n, "key-") && len(st.Held)+len(st.Queue) == 0 {
			continue
		}
		for _, e := range st.Held {
			info.Used += e.Units
			info.Held = append(info.Held, e.Label)
		}
		for _, e := range st.Queue {
			info.Queued = append(info.Queued, e.Label)
		}
		out = append(out, info)
	}
	return out, nil
}
