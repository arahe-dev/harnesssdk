package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func itemKinds(items []InboxItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Source+"/"+it.Kind)
	}
	return out
}

// Parties join rooms, post to each other and to rooms, and read one inbox:
// what others posted, once, and never their own posts.
func TestPartiesAndRooms(t *testing.T) {
	f := newFixture(t, nil)
	h := f.home
	if _, err := h.Join("alice", "agent", f.cwd, "refactoring the gate", []string{"team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join("bob", "human", f.cwd, "", []string{"team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join("Bad Name", "agent", f.cwd, "", nil); err == nil {
		t.Fatal("joined with an invalid alias")
	}
	ps, err := h.Parties()
	if err != nil || len(ps) != 2 || ps[0].Away() {
		t.Fatalf("parties: %v %v", ps, err)
	}
	if who, err := h.Speaker("alice"); err != nil || who != "alice" {
		t.Fatalf("speaker: %q %v", who, err)
	}
	if _, err := h.Speaker("carol"); err == nil {
		t.Fatal("spoke as a party that never joined")
	}

	h.Post("alice", "room:team", "gate.go is mine for an hour")
	h.Post("alice", "bob", "can you review #12?")
	items, err := h.PartyInbox("bob", true)
	if err != nil || len(items) != 2 {
		t.Fatalf("bob peeks: %v %v", itemKinds(items), err)
	}
	items, _ = h.PartyInbox("bob", false)
	if got := itemKinds(items); !slices.Equal(got, []string{"room:team/message", "me/message"}) || items[1].From != "alice" {
		t.Fatalf("bob's inbox: %v", got)
	}
	if items, _ := h.PartyInbox("bob", false); len(items) != 0 {
		t.Fatalf("read twice: %v", itemKinds(items))
	}
	// Alice sees bob join after her, and nothing she posted herself.
	if items, _ := h.PartyInbox("alice", false); len(items) != 1 || items[0].Kind != EvJoined || items[0].From != "bob" {
		t.Fatalf("alice's inbox: %v", itemKinds(items))
	}
	// A newcomer reads a room from when it joined.
	h.Join("carol", "agent", f.cwd, "", []string{"team"})
	h.Post("bob", "room:team", "lunch")
	if items, _ := h.PartyInbox("carol", false); len(items) != 1 || items[0].Text != "lunch" {
		t.Fatalf("carol: %v", itemKinds(items))
	}
	if err := h.Leave("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Post("bob", "carol", "x"); err == nil {
		t.Fatal("posted to a party that left")
	}
}

// A party asks another and waits; the other answers from its inbox.
func TestAskAParty(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	f.home.Join("alice", "agent", f.cwd, "", nil)
	f.home.Join("bob", "human", f.cwd, "", nil)
	var wg sync.WaitGroup
	var answer string
	var answered bool
	wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, answer, answered, _ = f.r.AskTo(ctx, "alice", "bob", "ship it?")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, err := f.home.WaitPartyInbox(ctx, "bob", false)
	if err != nil || len(items) != 1 || items[0].Kind != EvAsk || items[0].From != "alice" {
		t.Fatalf("bob's inbox: %v %v", itemKinds(items), err)
	}
	if to, err := f.home.ReplyTo("bob", items[0].Name, "yes"); err != nil || to != "alice" {
		t.Fatalf("reply: %s %v", to, err)
	}
	wg.Wait()
	if !answered || answer != "yes" {
		t.Fatalf("alice got %q (%v)", answer, answered)
	}
	if _, err := f.home.ReplyTo("bob", items[0].Name, "again"); err == nil {
		t.Fatal("answered twice")
	}
}

// A run's agent asks a peer, not whoever started it, and gets its answer.
func TestRunAsksAPeer(t *testing.T) {
	f := newFixture(t, nil)
	f.home.Join("bob", "human", f.cwd, "", nil)
	f.profile("asker", "harness: fake\neffect: idempotent\nmode: ask\nopts: home="+f.home.Dir+" to=bob")
	runs, err := f.r.Prepare("asker", []string{"go"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	var wg sync.WaitGroup
	wg.Go(func() { rep = f.r.Execute(context.Background(), runs[0].ID, 1) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, err := f.home.WaitPartyInbox(ctx, "bob", false)
	if err != nil || len(items) != 1 || items[0].From != "run:"+runs[0].ID {
		t.Fatalf("bob's inbox: %+v %v", items, err)
	}
	f.home.ReplyTo("bob", items[0].Name, "v2")
	wg.Wait()
	if rep.Output != "answer: v2" {
		t.Fatalf("run: %q %v", rep.Output, rep.Err)
	}
}

// What a party's runs need from it reaches its inbox: their questions and
// endings.
func TestOwnerInbox(t *testing.T) {
	f := newFixture(t, nil)
	f.home.Join("alice", "agent", f.cwd, "", nil)
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	reps, err := f.r.Start(context.Background(), "echo", []string{"hi"}, f.cwd, TurnOptions{Owner: "alice"}, nil)
	if err != nil || reps[0].Run.Owner != "alice" {
		t.Fatalf("start: %v %+v", err, reps)
	}
	f.start(context.Background(), "echo", "not hers")
	items, _ := f.home.PartyInbox("alice", false)
	if len(items) != 1 || items[0].Source != "run:"+reps[0].Run.ID || items[0].Kind != EvFinished || items[0].Name != "success" {
		t.Fatalf("alice's inbox: %v", itemKinds(items))
	}
}

// Claims are advisory leases: an overlapping claim by someone else is
// refused, naming its holder, until it is released, its party leaves, or
// its run ends.
func TestClaims(t *testing.T) {
	f := newFixture(t, nil)
	h := f.home
	h.Join("alice", "agent", f.cwd, "", nil)
	h.Join("bob", "agent", f.cwd, "", nil)
	dir := filepath.Join(f.cwd, "internal")
	if err := h.ClaimPaths("alice", []string{dir}, time.Hour, "refactor"); err != nil {
		t.Fatal(err)
	}
	err := h.ClaimPaths("bob", []string{filepath.Join(dir, "gate.go")}, time.Hour, "")
	if !errors.Is(err, ErrClaimed) || !strings.Contains(err.Error(), "claimed by alice") || OutcomeName(err) != "throttled" {
		t.Fatalf("overlapping claim: %v", err)
	}
	if err := h.ClaimPaths("bob", []string{filepath.Join(f.cwd, "docs")}, time.Hour, ""); err != nil {
		t.Fatalf("separate claim: %v", err)
	}
	if err := h.ClaimPaths("alice", []string{dir}, 2*time.Hour, "longer"); err != nil {
		t.Fatalf("renewing one's own claim: %v", err)
	}
	if cs, _ := h.Claims(); len(cs) != 2 {
		t.Fatalf("claims: %+v", cs)
	}
	h.Leave("alice")
	if err := h.ClaimPaths("bob", []string{filepath.Join(dir, "gate.go")}, time.Hour, ""); err != nil {
		t.Fatalf("after alice left: %v", err)
	}
	// A run's claim ends with the run.
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	rep := f.start(context.Background(), "echo", "x")[0]
	h.changeClaims(func(cs []PathClaim) ([]PathClaim, error) {
		return append(cs, PathClaim{Path: filepath.Join(f.cwd, "x"), Holder: "run:" + rep.Run.ID, Until: time.Now().Add(time.Hour)}), nil
	})
	if err := h.ClaimPaths("bob", []string{filepath.Join(f.cwd, "x")}, time.Hour, ""); err != nil {
		t.Fatalf("a finished run's claim held: %v", err)
	}
	if err := h.Release("bob", nil); err != nil {
		t.Fatal(err)
	}
	if cs, _ := h.Claims(); len(cs) != 0 {
		t.Fatalf("after release: %+v", cs)
	}
}

// A watcher reports only what happens after it starts.
func TestWatcher(t *testing.T) {
	f := newFixture(t, nil)
	f.home.Join("alice", "agent", f.cwd, "", []string{"team"})
	f.home.Post("alice", "room:team", "before")
	w := NewWatcher(f.home, "")
	f.home.Post("alice", "room:team", "after")
	f.profile("echo", "harness: fake\neffect: pure\nmode: echo")
	runs, _ := f.r.Prepare("echo", []string{"x"}, f.cwd, TurnOptions{})
	w.Poll() // sees the new run
	f.r.Execute(context.Background(), runs[0].ID, 1)
	items := w.Poll()
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Kind+":"+it.Text)
	}
	joined := strings.Join(texts, " ")
	if strings.Contains(joined, "before") || !strings.Contains(joined, "finished:echo: x") {
		t.Fatalf("watcher: %v", texts)
	}
}
