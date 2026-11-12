package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Many agents, in different harnesses, and people, working on one machine.
//
// Anything with an address has a mailbox, an event log like a run's:
//   - run:<id>     a run (its own log; phase-3 messages already go there)
//   - <alias>      a party: an agent session or a person that joined with
//                  harness join --as <alias>; parties/<alias>.jsonl
//   - room:<name>  a room: a log every member reads; rooms/<name>.jsonl
//
// A party is present while it uses harness (each command it runs with
// --as marks it seen), and away after partyAway without: there is no
// daemon to hold a session open. Runs a party starts are its own (their
// Owner), so its inbox gathers, in one place, its mailbox, its rooms, and
// what its runs need from it (questions, warnings, endings).
//
// Claims are advisory leases on paths: an agent says what it is about to
// edit, and an overlapping claim by someone else is refused, naming the
// holder. Nothing stops a write; it stops two agents from not knowing.

const (
	partyFormat = 1
	partyAway   = 30 * time.Minute
	// HarnessAs names the party a command speaks for when --as is not
	// given.
	EnvAs = "HARNESS_AS"
)

// Event kinds of parties and rooms.
const (
	EvJoined = "joined" // a party joined a room; from: the party
	EvLeft   = "left"
)

// Party is an agent session or a person that joined.
type Party struct {
	Format int       `json:"format"`
	Alias  string    `json:"alias"`
	Kind   string    `json:"kind"` // agent or human
	Cwd    string    `json:"cwd,omitempty"`
	Note   string    `json:"note,omitempty"` // what it is working on
	Rooms  []string  `json:"rooms,omitempty"`
	Joined time.Time `json:"joined"`
	Seen   time.Time `json:"seen"`
	// Cursors: how far it has read its mailbox, each room, and each of its
	// runs' logs.
	Cursors map[string]int64 `json:"cursors,omitempty"`
}

// Away reports whether the party has not used harness for partyAway.
func (p *Party) Away() bool { return time.Since(p.Seen) > partyAway }

func (h *Home) partyPath(alias, ext string) string {
	return filepath.Join(h.Dir, "parties", alias+ext)
}

func (h *Home) roomPath(name string) string { return filepath.Join(h.Dir, "rooms", name+".jsonl") }

// LoadParty reads a party.
func (h *Home) LoadParty(alias string) (*Party, error) {
	if !validName(alias) {
		return nil, fmt.Errorf("alias %q: use lower-case letters, digits, '.', '_' and '-'", alias)
	}
	data, err := readShared(h.partyPath(alias, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no party %q; join first with: harness join --as %s", alias, alias)
	}
	if err != nil {
		return nil, err
	}
	var p Party
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("party %s: %w", alias, err)
	}
	if err := checkFormatUpTo("party "+alias, p.Format, partyFormat); err != nil {
		return nil, err
	}
	return &p, nil
}

// updateParty changes a party under its lock; create makes it if missing.
func (h *Home) updateParty(alias string, create bool, change func(*Party) error) (*Party, error) {
	if !validName(alias) {
		return nil, fmt.Errorf("alias %q: use lower-case letters, digits, '.', '_' and '-'", alias)
	}
	if err := os.MkdirAll(filepath.Join(h.Dir, "parties"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFileWait(h.partyPath(alias, ".lock"), 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := h.LoadParty(alias)
	if err != nil {
		if !create {
			return nil, err
		}
		p = &Party{Alias: alias, Joined: time.Now().UTC()}
	}
	if err := change(p); err != nil {
		return p, err
	}
	p.Format, p.Seen = partyFormat, time.Now().UTC()
	data, _ := json.MarshalIndent(p, "", "  ")
	return p, writeFileAtomic(h.partyPath(alias, ".json"), append(data, '\n'))
}

// Join makes, or refreshes, a party, and adds it to rooms.
func (h *Home) Join(alias, kind, cwd, note string, rooms []string) (*Party, error) {
	if kind != "agent" && kind != "human" {
		return nil, fmt.Errorf("kind must be agent or human, got %q", kind)
	}
	for _, r := range rooms {
		if !validName(r) {
			return nil, fmt.Errorf("room %q: use lower-case letters, digits, '.', '_' and '-'", r)
		}
	}
	var added []string
	p, err := h.updateParty(alias, true, func(p *Party) error {
		p.Kind, p.Cwd = kind, cwd
		if note != "" {
			p.Note = note
		}
		for _, r := range rooms {
			if !slices.Contains(p.Rooms, r) {
				p.Rooms = append(p.Rooms, r)
				added = append(added, r)
				if p.Cursors == nil {
					p.Cursors = map[string]int64{}
				}
				p.Cursors["room:"+r] = logEnd(h.roomPath(r)) // a newcomer reads from now
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(h.Dir, "rooms"), 0o700); err != nil {
		return nil, err
	}
	for _, r := range added {
		appendEvent(h.roomPath(r), Event{Kind: EvJoined, From: alias, To: "room:" + r, Text: p.Note})
	}
	return p, nil
}

// Leave removes a party, with its claims; its mailbox stays for gc.
func (h *Home) Leave(alias string) error {
	p, err := h.LoadParty(alias)
	if err != nil {
		return err
	}
	for _, r := range p.Rooms {
		appendEvent(h.roomPath(r), Event{Kind: EvLeft, From: alias, To: "room:" + r})
	}
	h.Release(alias, nil)
	return os.Remove(h.partyPath(alias, ".json"))
}

// Parties lists the parties, present ones first.
func (h *Home) Parties() ([]*Party, error) {
	entries, err := os.ReadDir(filepath.Join(h.Dir, "parties"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Party
	for _, e := range entries {
		if alias, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if p, err := h.LoadParty(alias); err == nil {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b *Party) int { return b.Seen.Compare(a.Seen) })
	return out, nil
}

// Speaker is who a command speaks for: a party (--as, or HARNESS_AS), or
// the run the calling process is inside; "" if neither. A party is marked
// seen.
func (h *Home) Speaker(as string) (string, error) {
	if as == "" {
		as = os.Getenv(EnvAs)
	}
	if as != "" {
		if _, err := h.updateParty(as, false, func(*Party) error { return nil }); err != nil {
			return "", err
		}
		return as, nil
	}
	if p := h.DiscoverParentage(); p.Parent != "" {
		return "run:" + p.Parent, nil
	}
	return "", nil
}

// mailbox is the log of an address, which must exist.
func (h *Home) mailbox(addr string) (string, error) {
	switch {
	case strings.HasPrefix(addr, "run:"):
		run, err := h.LoadRun(strings.TrimPrefix(addr, "run:"))
		if err != nil {
			return "", err
		}
		return filepath.Join(run.dir, eventsFile), nil
	case strings.HasPrefix(addr, "room:"):
		name := strings.TrimPrefix(addr, "room:")
		if !validName(name) {
			return "", fmt.Errorf("room %q is not a valid name", name)
		}
		if err := os.MkdirAll(filepath.Join(h.Dir, "rooms"), 0o700); err != nil {
			return "", err
		}
		return h.roomPath(name), nil
	}
	if _, err := h.LoadParty(addr); err != nil {
		return "", err
	}
	return h.partyPath(addr, ".jsonl"), nil
}

// Post puts a message in an address's mailbox and returns its id. A
// message to a run is one its agent reads with inbox, or at its next turn.
func (h *Home) Post(from, to, text string) (string, error) {
	path, err := h.mailbox(to)
	if err != nil {
		return "", err
	}
	ev := Event{Kind: EvMessage, Name: newMessageID("m"), From: or(from, "caller"), To: to, Text: text}
	if strings.HasPrefix(to, "run:") {
		run, _ := h.LoadRun(strings.TrimPrefix(to, "run:"))
		ev.Turn = len(run.Turns)
	}
	appendEvent(path, ev)
	return ev.Name, nil
}

// AskTo puts a question in an address's mailbox and waits, until ctx
// ends, for the reply in the asker's own: a party's mailbox, or its run's
// log. answered is false if none came; the question is then closed.
func (r *Runner) AskTo(ctx context.Context, from, to, question string) (id, answer string, answered bool, err error) {
	if from == "" {
		return "", "", false, errors.New("say who is asking: --as <alias> (join first), or ask from inside a run")
	}
	target, err := r.home.mailbox(to)
	if err != nil {
		return "", "", false, err
	}
	own, err := r.home.mailbox(from)
	if err != nil {
		return "", "", false, err
	}
	if strings.HasPrefix(from, "run:") {
		run, _ := r.home.LoadRun(strings.TrimPrefix(from, "run:"))
		if asking, err := lockOpen(run.Path("asking.lock")); err == nil {
			defer asking.Close() // a run waiting for a peer is not idle either
		}
	}
	cursor := logEnd(own)
	id = newMessageID("q")
	appendEvent(target, Event{Kind: EvAsk, Name: id, From: from, To: to, Text: question})
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		var evs []Event
		if evs, cursor, err = readEventsAt(own, cursor); err != nil {
			return id, "", false, err
		}
		for _, e := range evs {
			if e.Kind == EvReply && e.Name == id {
				return id, e.Text, true, nil
			}
		}
		select {
		case <-ctx.Done():
			appendEvent(target, Event{Kind: EvExpired, Name: id, From: from, To: to, Text: "no answer in time"})
			return id, "", false, nil
		case <-tick.C:
		}
	}
}

// ReplyTo answers a question someone asked a party (in its mailbox) or a
// room it is in: the reply goes to the asker's mailbox.
func (h *Home) ReplyTo(me, id, answer string) (string, error) {
	p, err := h.LoadParty(me)
	if err != nil {
		return "", err
	}
	sources := []string{h.partyPath(me, ".jsonl")}
	for _, room := range p.Rooms {
		sources = append(sources, h.roomPath(room))
	}
	for _, src := range sources {
		evs, _, err := readEventsAt(src, 0)
		if err != nil {
			continue
		}
		for _, a := range openAsks(evs) {
			if a.Name != id {
				continue
			}
			own, err := h.mailbox(a.From)
			if err != nil {
				return "", fmt.Errorf("the asker, %s, cannot be reached: %w", a.From, err)
			}
			appendEvent(own, Event{Kind: EvReply, Name: id, From: me, To: a.From, Text: answer})
			appendEvent(src, Event{Kind: EvReply, Name: id, From: me, To: a.From, Text: answer}) // closes it here too
			return a.From, nil
		}
	}
	return "", fmt.Errorf("no open question %s for %s (it was answered, or its wait ended)", id, me)
}

// logEnd is the size of a log: a cursor at its end.
func logEnd(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// InboxItem is one thing in a party's inbox, and where it came from: its
// mailbox ("me"), room:<name>, or run:<id> (one of its runs).
type InboxItem struct {
	Source string
	Event
}

// ownedRuns are the runs a party started, newest first.
func (h *Home) ownedRuns(alias string) []*Run {
	runs, _ := h.Runs(200)
	var out []*Run
	for _, r := range runs {
		if r.Owner == alias {
			out = append(out, r)
		}
	}
	return out
}

// forParty reports whether a run event needs the party that owns the run.
func forParty(e Event) bool {
	switch e.Kind {
	case EvAsk, EvTimeoutSoon, EvFinished, EvLost, EvNote:
		return true
	}
	return false
}

// PartyInbox gathers what is new for a party: its mailbox, its rooms (what
// others posted), and what its runs need from it. Unless peek, it is
// marked read.
func (h *Home) PartyInbox(alias string, peek bool) ([]InboxItem, error) {
	p, err := h.LoadParty(alias)
	if err != nil {
		return nil, err
	}
	cursors := map[string]int64{}
	for k, v := range p.Cursors {
		cursors[k] = v
	}
	var items []InboxItem
	read := func(key, path string, keep func(Event) bool) {
		start, seen := cursors[key]
		if !seen && strings.HasPrefix(key, "run:") {
			start = 0 // a run's whole story is the owner's
		}
		evs, next, err := readEventsAt(path, start)
		if err != nil {
			return
		}
		cursors[key] = next
		for _, e := range evs {
			if keep(e) {
				items = append(items, InboxItem{Source: key, Event: e})
			}
		}
	}
	read("me", h.partyPath(alias, ".jsonl"), func(Event) bool { return true })
	for _, room := range p.Rooms {
		read("room:"+room, h.roomPath(room), func(e Event) bool { return e.From != alias })
	}
	for _, run := range h.ownedRuns(alias) {
		key := "run:" + run.ID
		if _, known := cursors[key]; !known && lastFinished(run) && time.Since(run.Created) > partyAway {
			continue // an old run, done before this party looked
		}
		read(key, filepath.Join(run.dir, eventsFile), forParty)
	}
	// Oldest first. Each source keeps its own order; items from different
	// sources with the same time (one clock tick: coarse on Windows) come in
	// the order the sources are read.
	slices.SortStableFunc(items, func(a, b InboxItem) int { return a.Time.Compare(b.Time) })
	if !peek {
		_, err = h.updateParty(alias, false, func(p *Party) error {
			p.Cursors = cursors
			return nil
		})
	}
	return items, err
}

// WaitPartyInbox is PartyInbox, waiting until there is something or ctx
// ends.
func (h *Home) WaitPartyInbox(ctx context.Context, alias string, peek bool) ([]InboxItem, error) {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		items, err := h.PartyInbox(alias, peek)
		if err != nil || len(items) > 0 {
			return items, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-tick.C:
		}
	}
}

// Claims: advisory leases on paths.

const claimsFormat = 1

// PathClaim is a lease on a path (a file, or a folder and all under it).
type PathClaim struct {
	Path   string    `json:"path"`
	Holder string    `json:"holder"` // a party's alias, or run:<id>
	Until  time.Time `json:"until"`
	Note   string    `json:"note,omitempty"`
}

type claimsFile struct {
	Format int         `json:"format"`
	Claims []PathClaim `json:"claims"`
}

// ErrClaimed refuses a claim that overlaps someone else's.
var ErrClaimed = errors.New("claimed by someone else")

func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(a, strings.TrimSuffix(b, sep)+sep) || strings.HasPrefix(b, strings.TrimSuffix(a, sep)+sep)
}

// live reports whether a claim's holder still holds it: not expired, and
// its run still going or its party present.
func (h *Home) live(c PathClaim) bool {
	if time.Now().After(c.Until) {
		return false
	}
	if id, ok := strings.CutPrefix(c.Holder, "run:"); ok {
		run, err := h.LoadRun(id)
		return err == nil && !lastFinished(run)
	}
	p, err := h.LoadParty(c.Holder)
	return err == nil && !p.Away()
}

// changeClaims reads the claims under their lock, keeps the live ones,
// applies change, and saves them.
func (h *Home) changeClaims(change func([]PathClaim) ([]PathClaim, error)) ([]PathClaim, error) {
	unlock, err := lockFileWait(filepath.Join(h.Dir, "claims.lock"), 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var cf claimsFile
	if data, err := readShared(filepath.Join(h.Dir, "claims.json")); err == nil {
		if err := json.Unmarshal(data, &cf); err != nil {
			return nil, fmt.Errorf("claims.json: %w", err)
		}
		if err := checkFormatUpTo("claims", cf.Format, claimsFormat); err != nil {
			return nil, err
		}
	}
	live := slices.DeleteFunc(cf.Claims, func(c PathClaim) bool { return !h.live(c) })
	out, err := change(live)
	if err != nil {
		return live, err
	}
	data, _ := json.MarshalIndent(claimsFile{Format: claimsFormat, Claims: out}, "", "  ")
	return out, writeFileAtomic(filepath.Join(h.Dir, "claims.json"), append(data, '\n'))
}

// ClaimPaths claims absolute paths for holder for ttl. A path overlapping
// someone else's live claim refuses the whole request, naming them; the
// holder's own claims on the paths are renewed.
func (h *Home) ClaimPaths(holder string, paths []string, ttl time.Duration, note string) error {
	if holder == "" {
		return errors.New("say who claims: --as <alias> (join first), or claim from inside a run")
	}
	_, err := h.changeClaims(func(cs []PathClaim) ([]PathClaim, error) {
		var conflicts []string
		for _, p := range paths {
			for _, c := range cs {
				if c.Holder != holder && overlaps(p, c.Path) {
					conflicts = append(conflicts, fmt.Sprintf("%s is claimed by %s until %s (%s)", c.Path, c.Holder, c.Until.Local().Format("15:04"), or(c.Note, "no note")))
				}
			}
		}
		if len(conflicts) > 0 {
			return nil, hertasdk.Fail(hertasdk.Throttled, fmt.Errorf("%w: %s; ask them, or work elsewhere", ErrClaimed, strings.Join(conflicts, "; ")))
		}
		until := time.Now().Add(ttl).UTC()
		for _, p := range paths {
			cs = slices.DeleteFunc(cs, func(c PathClaim) bool { return c.Holder == holder && c.Path == p })
			cs = append(cs, PathClaim{Path: p, Holder: holder, Until: until, Note: note})
		}
		return cs, nil
	})
	return err
}

// Release drops holder's claims on paths, or all its claims if paths is
// empty.
func (h *Home) Release(holder string, paths []string) error {
	_, err := h.changeClaims(func(cs []PathClaim) ([]PathClaim, error) {
		return slices.DeleteFunc(cs, func(c PathClaim) bool {
			return c.Holder == holder && (len(paths) == 0 || slices.Contains(paths, c.Path))
		}), nil
	})
	return err
}

// Claims lists the live claims.
func (h *Home) Claims() ([]PathClaim, error) {
	return h.changeClaims(func(cs []PathClaim) ([]PathClaim, error) { return cs, nil })
}

// Watcher follows, from when it starts, every active run's log and every
// room (or one room), for a person watching.
type Watcher struct {
	h       *Home
	room    string
	cursors map[string]int64
}

// NewWatcher starts following.
func NewWatcher(h *Home, room string) *Watcher {
	w := &Watcher{h: h, room: room, cursors: map[string]int64{}}
	w.Poll() // what came before is not news
	return w
}

// Poll returns what is new since the last poll, oldest first.
func (w *Watcher) Poll() []InboxItem {
	sources := map[string]string{}
	if w.room != "" {
		sources["room:"+w.room] = w.h.roomPath(w.room)
	} else {
		if entries, err := os.ReadDir(filepath.Join(w.h.Dir, "rooms")); err == nil {
			for _, e := range entries {
				if name, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok {
					sources["room:"+name] = w.h.roomPath(name)
				}
			}
		}
		runs, _ := w.h.Runs(200)
		for _, r := range runs {
			_, known := w.cursors["run:"+r.ID]
			if known || !lastFinished(r) {
				sources["run:"+r.ID] = filepath.Join(r.dir, eventsFile)
			}
		}
	}
	first := len(w.cursors) == 0
	var items []InboxItem
	for key, path := range sources {
		start, known := w.cursors[key]
		if !known && first {
			start = logEnd(path)
		}
		evs, next, err := readEventsAt(path, start)
		if err != nil {
			continue
		}
		w.cursors[key] = next
		for _, e := range evs {
			items = append(items, InboxItem{Source: key, Event: e})
		}
	}
	if first {
		w.cursors[""] = 0 // started
		return nil
	}
	slices.SortStableFunc(items, func(a, b InboxItem) int { return a.Time.Compare(b.Time) })
	return items
}
