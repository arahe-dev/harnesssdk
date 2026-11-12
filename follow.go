package harnesssdk

import (
	"context"
	"time"

	"github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// Event is one entry in a run's event log. Lifecycle events say where a
// turn is: queued, started (waiting for admission), attempt, retry, check,
// stop, finished and lost. Activity events say what its agent is doing:
// tool (Name is the tool, Text its detail), file (Text is the path), say
// (Text is what it said) and step; they come from the definition's
// output.progress rules.
type Event struct {
	Time    time.Time
	Turn    int
	Attempt int
	Kind    string
	Name    string
	Text    string
	From    string // who posted a message: a party's alias, or run:<id>
}

func eventsOf(evs []engine.Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = Event{Time: e.Time, Turn: e.Turn, Attempt: e.Attempt, Kind: e.Kind, Name: e.Name, Text: e.Text, From: e.From}
	}
	return out
}

// Events returns a run's events after cursor since (0 for the start), and
// the cursor to read from next.
func (c *Client) Events(run string, since int64) ([]Event, int64, error) {
	evs, next, err := c.home.Events(run, since)
	return eventsOf(evs), next, err
}

// WaitEvents is Events, waiting until there is a new event, the run's last
// turn finishes (done), or ctx ends.
func (c *Client) WaitEvents(ctx context.Context, run string, since int64) (evs []Event, next int64, done bool, err error) {
	e, next, done, err := c.runner.WaitEvents(ctx, run, since)
	return eventsOf(e), next, done, err
}

// Status is a run's last turn as it is now.
type Status struct {
	Run     string
	Turn    int
	Outcome Outcome
	// Phase is where the turn is: queued, admitting (waiting for its
	// budgets, keys or a running slot), running, backing off, checking,
	// finishing, or its outcome once it has ended.
	Phase       string
	Attempt     int
	MaxAttempts int
	Started     time.Time
	AttemptAt   time.Time     // when the current attempt began
	Timeout     time.Duration // per attempt; 0 for none
	Deadline    time.Time     // when the current attempt stops, extensions included; zero for none
	Idle        time.Duration // the attempt stops after this long with no output; 0 for none
	// LastOutput is when the agent last wrote output. Live says its CLI
	// streams events as it works; otherwise it prints only at the end.
	LastOutput time.Time
	Live       bool
	Metrics    Metrics
	// ContextTokens is what the agent's context held at its last model
	// call and ContextWindow its model's window, from the CLI's metrics
	// (context_tokens, context_window) or the profile's context_window; 0
	// when unknown. ContextUsed is their ratio, 0 to 1, or -1 if unknown.
	ContextTokens, ContextWindow int64
	ContextUsed                  float64
	Activity                     []Event // the turn's most recent activity, oldest first
	Said                         string  // the last thing the agent said
	// Changes are the files changed where the agent works: Scope "branch"
	// for a worktree run, "folder" for everything changed there since its
	// last commit, by anyone. Nil outside git.
	Changes *Changes
	Hint    string
	Error   string
	Cursor  int64    // pass to Events to read what happens next
	Next    []string // harness commands the caller can run now
}

// Changes are the files changed where a run's agent works.
type Changes struct {
	Scope          string
	Summary        string
	Files          []string
	Changed, New   int
	Added, Deleted int
}

// Status describes a run's last turn: where it is, what its agent is
// doing, and what it has changed.
func (c *Client) Status(ctx context.Context, run string) (*Status, error) {
	st, err := c.runner.Status(ctx, run)
	if err != nil {
		return nil, err
	}
	out := &Status{Run: st.Run.ID, Turn: st.Turn, Outcome: Outcome(st.Outcome), Phase: st.Phase, Attempt: st.Attempt,
		MaxAttempts: st.MaxAttempts, Started: st.Started, AttemptAt: st.AttemptAt, Timeout: st.Timeout, Deadline: st.Deadline, Idle: st.Idle,
		LastOutput: st.LastOutput, Live: st.Live, Metrics: metricsOf(st.Metrics), Activity: eventsOf(st.Activity),
		Said: st.Said, Hint: st.Hint, Error: st.Error, Cursor: st.Cursor, Next: st.Next()}
	out.ContextTokens, out.ContextWindow, out.ContextUsed = int64(st.ContextTokens), int64(st.ContextWindow), -1
	if share, ok := st.ContextUsed(); ok {
		out.ContextUsed = share
	}
	if ch := st.Changes; ch != nil {
		out.Changes = &Changes{Scope: ch.Scope, Summary: ch.Summary(), Files: ch.Files, Changed: ch.Changed, New: ch.New,
			Added: ch.Added, Deleted: ch.Deleted}
	}
	return out, nil
}

// Extend gives a run's unfinished turn until at least d from now (its
// attempts' timeout is moved, and its idle clock reset), and returns the
// new deadline. reason is noted in the run's event log.
func (c *Client) Extend(run string, d time.Duration, reason string) (time.Time, error) {
	return c.runner.Extend(run, d, reason, "caller")
}

// Handover continues a run whose context is full in a fresh session: it
// asks the agent for handover notes, then starts a new run of the same
// profile and folder, in the background, with the notes and next ("" to
// continue the work). It returns the new run's id and the notes.
func (c *Client) Handover(ctx context.Context, run, next string) (newRun, notes string, err error) {
	nr, rep, err := c.runner.Handover(ctx, run, next)
	if nr != nil {
		newRun = nr.ID
	}
	return newRun, rep.Output, err
}

// Reply answers a run's question (an agent waiting in harness ask): the
// one with questionID, or its only open one. It returns the question's id.
func (c *Client) Reply(run, questionID, answer string) (string, error) {
	return c.home.Reply(run, questionID, answer)
}

// Steer posts a message to a run's agent, which reads it with harness
// inbox, or at the top of its next turn. With now, a running turn is
// stopped and the session continues with the message at once, in the
// background.
func (c *Client) Steer(ctx context.Context, run, message string, now bool) error {
	if now {
		_, err := c.runner.SteerNow(ctx, run, message)
		return err
	}
	_, err := c.home.Steer(run, message)
	return err
}

// Party is an agent session or a person that joined, with a mailbox.
type Party struct {
	Alias, Kind, Cwd, Note string
	Rooms                  []string
	Joined, Seen           time.Time
	Away                   bool // no harness command for 30 minutes
}

// Join makes this program a party (kind "agent" or "human") with alias,
// in rooms. Runs started with Request.Owner set to the alias report to its
// Inbox.
func (c *Client) Join(alias, kind, cwd, note string, rooms []string) error {
	_, err := c.home.Join(alias, kind, cwd, note, rooms)
	return err
}

// Leave removes a party and its claims.
func (c *Client) Leave(alias string) error { return c.home.Leave(alias) }

// Parties lists the parties, most recently seen first.
func (c *Client) Parties() ([]Party, error) {
	ps, err := c.home.Parties()
	out := make([]Party, len(ps))
	for i, p := range ps {
		out[i] = Party{Alias: p.Alias, Kind: p.Kind, Cwd: p.Cwd, Note: p.Note, Rooms: p.Rooms, Joined: p.Joined, Seen: p.Seen, Away: p.Away()}
	}
	return out, err
}

// Post puts a message from a party (or "" for the caller) in a mailbox: a
// party's alias, run:<id> or room:<name>. It returns the message's id.
func (c *Client) Post(from, to, text string) (string, error) { return c.home.Post(from, to, text) }

// Ask puts a question from a party in a mailbox and waits, until ctx ends,
// for the answer; answered is false if none came.
func (c *Client) Ask(ctx context.Context, from, to, question string) (answer string, answered bool, err error) {
	_, answer, answered, err = c.runner.AskTo(ctx, from, to, question)
	return answer, answered, err
}

// Answer replies, as party me, to a question asked of it or of a room it
// is in, by the question's id.
func (c *Client) Answer(me, questionID, answer string) error {
	_, err := c.home.ReplyTo(me, questionID, answer)
	return err
}

// InboxItem is one thing in a party's inbox: Source is "me" (its mailbox),
// room:<name>, or run:<id> (one of its runs).
type InboxItem struct {
	Source string
	Event
}

// Inbox returns what is new for a party, waiting until there is something
// or ctx ends, and marks it read: its mailbox, its rooms, and what its runs
// need from it (questions, warnings, endings).
func (c *Client) Inbox(ctx context.Context, alias string) ([]InboxItem, error) {
	items, err := c.home.WaitPartyInbox(ctx, alias, false)
	out := make([]InboxItem, len(items))
	for i, it := range items {
		out[i] = InboxItem{Source: it.Source, Event: eventsOf([]engine.Event{it.Event})[0]}
	}
	return out, err
}

// Claim is an advisory lease on a path.
type Claim struct {
	Path, Holder, Note string
	Until              time.Time
}

// Claim leases absolute paths (files, or folders and all under them) for
// holder (a party's alias, or run:<id>) for ttl. A path overlapping
// someone else's live claim refuses the request, naming them.
func (c *Client) Claim(holder string, paths []string, ttl time.Duration, note string) error {
	return c.home.ClaimPaths(holder, paths, ttl, note)
}

// Release drops holder's claims on paths, or all of them.
func (c *Client) Release(holder string, paths []string) error { return c.home.Release(holder, paths) }

// Claims lists the live claims.
func (c *Client) Claims() ([]Claim, error) {
	cs, err := c.home.Claims()
	out := make([]Claim, len(cs))
	for i, cl := range cs {
		out[i] = Claim{Path: cl.Path, Holder: cl.Holder, Note: cl.Note, Until: cl.Until}
	}
	return out, err
}
