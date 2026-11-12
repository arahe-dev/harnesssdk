package engine

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Status is a run's last turn as it is now: where it is, what its agent is
// doing, what it has changed, and what the caller can do next. It is read
// from the run's record, event log, output files and git, and changes
// nothing.
type Status struct {
	Run     *Run
	Turn    int
	Outcome string // the record's: queued, running, or how the turn ended
	// Phase is where the turn is: queued, admitting (waiting for its
	// budgets, keys or a running slot), running, backing off (between
	// attempts), checking, finishing, or its outcome once it has ended.
	Phase       string
	Attempt     int
	MaxAttempts int
	Started     time.Time     // the turn
	AttemptAt   time.Time     // the current attempt
	Timeout     time.Duration // per attempt; 0 for none
	// Deadline is when the current attempt is stopped: its start plus
	// Timeout, or later if extended; zero if it has no timeout.
	Deadline time.Time
	Extended bool          // harness extend moved the deadline
	Idle     time.Duration // the attempt stops after this long with no output; 0 for none
	// HandoverAt is how full the context may get before handing over
	// is suggested; 0 never.
	HandoverAt float64
	// LastOutput is when the agent last wrote to its stdout or stderr.
	// With Live, its CLI streams events as it works, so a long silence
	// means it is thinking, waiting on a tool, or stuck; without, the CLI
	// prints only at the end.
	LastOutput time.Time
	Live       bool
	Metrics    map[string]float64 // the turn's so far, the current attempt included
	// ContextTokens is what the agent's context held at its last model
	// call, and ContextWindow its model's window: the profile's
	// context_window if it sets one, else the CLI's (metrics
	// context_tokens and context_window, this turn's or the latest turn's
	// that reported them). 0 when unknown.
	ContextTokens, ContextWindow float64
	Activity                     []Event  // the turn's most recent activity, oldest first
	Said                         string   // the last thing the agent said
	Changes                      *Changes // nil outside git
	Hint                         string
	Error                        string
	Cursor                       int64 // the event log's end: events --since this shows what comes next
}

// Changes are the files changed where a run's agent works.
type Changes struct {
	// Scope is "branch" for a worktree run (exactly its work since it
	// started) or "folder" (everything changed there since its last
	// commit, by anyone).
	Scope          string
	Files          []string // changed, then new, paths
	Changed, New   int
	Added, Deleted int // lines, in changed files
	Err            string
}

const statusActivity = 8

// Status describes a run's last turn.
func (r *Runner) Status(ctx context.Context, id string) (*Status, error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return nil, err
	}
	if len(run.Turns) > 0 && !lastFinished(run) && !run.executing() {
		// A turn whose process died is recorded as lost, as wait does.
		wctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := r.WaitTurn(wctx, id, len(run.Turns)); err == nil {
			if again, err := r.home.LoadRun(id); err == nil {
				run = again
			}
		}
	}
	st := &Status{Run: run}
	if len(run.Turns) == 0 {
		st.Phase, st.Outcome = "new", "new"
		return st, nil
	}
	rec := run.Turns[len(run.Turns)-1]
	st.Turn, st.Outcome, st.Phase = rec.N, rec.Outcome, rec.Outcome
	st.Attempt, st.Started, st.Hint, st.Error = rec.Attempts, rec.Started, rec.Hint, rec.Error
	st.Metrics = rec.Metrics
	p, d, perr := r.load(run.Profile, run.Cwd)
	if perr == nil {
		st.Timeout, st.MaxAttempts = p.Timeout, max(p.Retry.MaxAttempts, 1)
		st.Live = d.Output.Format == "jsonl"
		st.Idle, st.HandoverAt = idleFor(p, d), handoverAt(p)
	}

	evs, cursor, _ := tailEvents(run.dir, 256<<10)
	st.Cursor = cursor
	last := ""
	for _, ev := range evs {
		if ev.Turn != rec.N {
			continue
		}
		if ev.lifecycle() {
			last = ev.Kind
			if ev.Kind == EvAttempt {
				st.Attempt, st.AttemptAt = ev.Attempt, ev.Time
			}
			continue
		}
		if ev.Kind == EvSay {
			st.Said = ev.Text
		}
		st.Activity = append(st.Activity, ev)
	}
	if len(st.Activity) > statusActivity {
		st.Activity = st.Activity[len(st.Activity)-statusActivity:]
	}
	if !rec.finished() {
		switch last {
		case EvStarted:
			st.Phase = "admitting"
		case EvAttempt:
			st.Phase = "running"
		case EvRetry:
			st.Phase = "backing off"
		case EvCheck:
			st.Phase = "checking"
		case EvFinished:
			st.Phase = "finishing"
		}
		if rec.Outcome == "running" && last == "" {
			st.Phase = "admitting"
		}
	}
	until, _ := readExtension(run, rec.N)
	if !st.AttemptAt.IsZero() && st.Timeout > 0 {
		st.Deadline = st.AttemptAt.Add(st.Timeout)
		if until.After(st.Deadline) {
			st.Deadline, st.Extended = until, true
		}
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if fi, err := os.Stat(run.Path(fmt.Sprintf("turn-%d.%s", rec.N, name))); err == nil && fi.ModTime().After(st.LastOutput) {
			st.LastOutput = fi.ModTime()
		}
	}
	// The current attempt's metrics are in its stdout until it ends and
	// they are added to the record.
	if st.Phase == "running" && perr == nil && len(d.Output.Metrics) > 0 {
		if x, _ := extractFile(d, run.Path(fmt.Sprintf("turn-%d.stdout.log", rec.N))); len(x.Metrics) > 0 {
			sum := map[string]float64{}
			for k, v := range rec.Metrics {
				sum[k] = v
			}
			for k, v := range x.Metrics {
				if gauge(k) {
					sum[k] = v
				} else {
					sum[k] += v
				}
			}
			st.Metrics = finiteMetrics(sum)
		}
	}
	st.ContextTokens = st.Metrics["context_tokens"]
	if perr == nil && p.ContextWindow > 0 {
		// Said outright, it wins: a CLI that reports several models (an
		// agent's subagents) may not say which window is the agent's.
		st.ContextWindow = float64(p.ContextWindow)
	} else {
		st.ContextWindow = st.Metrics["context_window"]
	}
	for i := len(run.Turns) - 1; i >= 0 && (st.ContextTokens == 0 || st.ContextWindow == 0); i-- {
		m := run.Turns[i].Metrics
		if st.ContextTokens == 0 {
			st.ContextTokens = m["context_tokens"] // a resumed session starts where it was
		}
		if st.ContextWindow == 0 {
			st.ContextWindow = m["context_window"]
		}
	}
	st.Changes = changesOf(ctx, run)
	return st, nil
}

// changesOf summarizes what has changed where the run's agent works, or nil
// outside a git repository.
func changesOf(ctx context.Context, run *Run) *Changes {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dir := run.AgentDir()
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	if _, err := git(ctx, "-C", dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil
	}
	c := &Changes{Scope: "folder"}
	base := "HEAD"
	if run.Worktree != "" && run.Base != "" {
		c.Scope, base = "branch", run.Base
	}
	numstat, err := git(ctx, "-C", dir, "diff", "--numstat", "--no-renames", base, "--", ".")
	if err != nil {
		c.Err = err.Error()
		return c
	}
	for line := range strings.Lines(numstat) {
		f := strings.SplitN(strings.TrimRight(line, "\r\n"), "\t", 3)
		if len(f) != 3 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" for a binary file
		b, _ := strconv.Atoi(f[1])
		c.Added, c.Deleted, c.Changed = c.Added+a, c.Deleted+b, c.Changed+1
		c.Files = append(c.Files, f[2])
	}
	untracked, err := git(ctx, "-C", dir, "ls-files", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		c.Err = err.Error()
		return c
	}
	for line := range strings.Lines(untracked) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			c.New++
			c.Files = append(c.Files, line)
		}
	}
	return c
}

// Summary is the changes in a few words: "3 changed (+40 -2), 1 new".
func (c *Changes) Summary() string {
	if c == nil {
		return ""
	}
	if c.Err != "" {
		return "unknown (" + c.Err + ")"
	}
	if c.Changed == 0 && c.New == 0 {
		return "none"
	}
	var parts []string
	if c.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed (+%d -%d)", c.Changed, c.Added, c.Deleted))
	}
	if c.New > 0 {
		parts = append(parts, fmt.Sprintf("%d new", c.New))
	}
	return strings.Join(parts, ", ")
}

// ContextUsed is the share of the context window in use, 0 to 1, and
// whether it is known.
func (s *Status) ContextUsed() (float64, bool) {
	if s.ContextTokens <= 0 || s.ContextWindow <= 0 {
		return 0, false
	}
	return s.ContextTokens / s.ContextWindow, true
}

// Next is what the caller can do now, as commands.
func (s *Status) Next() []string {
	id := s.Run.ID
	switch s.Outcome {
	case "new":
		return nil
	case "queued", "running":
		return []string{
			"harness wait " + id + " --timeout 5m",
			fmt.Sprintf("harness events %s --since %d --wait 2m", id, s.Cursor),
			"harness extend " + id + " 30m",
			"harness stop " + id,
		}
	}
	next := []string{"harness result " + id}
	if s.Run.Session != "" {
		next = append(next, "harness send "+id+" \"<follow-up>\"")
		if share, ok := s.ContextUsed(); ok && s.HandoverAt > 0 && share >= s.HandoverAt {
			next = append(next, "harness handover "+id+" --next \"<the next task>\"")
		}
	}
	return next
}
