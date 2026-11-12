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
}

func eventsOf(evs []engine.Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = Event{Time: e.Time, Turn: e.Turn, Attempt: e.Attempt, Kind: e.Kind, Name: e.Name, Text: e.Text}
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
	// LastOutput is when the agent last wrote output. Live says its CLI
	// streams events as it works; otherwise it prints only at the end.
	LastOutput time.Time
	Live       bool
	Metrics    Metrics
	Activity   []Event // the turn's most recent activity, oldest first
	Said       string  // the last thing the agent said
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
		MaxAttempts: st.MaxAttempts, Started: st.Started, AttemptAt: st.AttemptAt, Timeout: st.Timeout,
		LastOutput: st.LastOutput, Live: st.Live, Metrics: metricsOf(st.Metrics), Activity: eventsOf(st.Activity),
		Said: st.Said, Hint: st.Hint, Error: st.Error, Cursor: st.Cursor, Next: st.Next()}
	if ch := st.Changes; ch != nil {
		out.Changes = &Changes{Scope: ch.Scope, Summary: ch.Summary(), Files: ch.Files, Changed: ch.Changed, New: ch.New,
			Added: ch.Added, Deleted: ch.Deleted}
	}
	return out, nil
}
