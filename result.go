package harnesssdk

import (
	"errors"
	"time"

	"github.com/arahe-dev/harnesssdk/internal/engine"
)

// Outcome is how a turn ended, or that it has not.
type Outcome string

// Outcomes. The four failures are Herta's: Transient (worth retrying),
// Throttled (retry later), Uncertain (it may have had its effect; check
// before retrying), Permanent (retrying will not help).
const (
	Success   Outcome = "success"
	Transient Outcome = "transient"
	Throttled Outcome = "throttled"
	Uncertain Outcome = "uncertain"
	Permanent Outcome = "permanent"
	Cancelled Outcome = "cancelled" // its context ended, or it was stopped
	Stopped   Outcome = "stopped"   // its process shut down before it ran
	Queued    Outcome = "queued"
	Running   Outcome = "running"
)

// Finished reports whether the turn has ended.
func (o Outcome) Finished() bool { return o != Queued && o != Running }

// ExitCode is the harness command's exit code for the outcome.
func (o Outcome) ExitCode() int {
	if !o.Finished() {
		return engine.ExitRunning
	}
	return engine.ExitCode(string(o))
}

// Result is one turn of a run.
type Result struct {
	Run      string
	Turn     int
	Profile  string
	Harness  string
	Session  string // the harness's session id, once known
	Outcome  Outcome
	Output   string // the agent's answer
	Attempts int
	Duration time.Duration
	// StderrTail is the end of the CLI's stderr when the turn failed.
	StderrTail string
	Model      string  // the profile's model placeholder, if any
	Metrics    Metrics // the definition's metrics, such as tokens and cost
	// Hint is advice for the caller, such as to stop repeating a request
	// that keeps failing.
	Hint string
	// Error says why the turn did not succeed; Run, Send and Wait also
	// return it as their error.
	Error string
}

func resultOf(rep engine.Report) *Result {
	res := &Result{Turn: rep.Turn, Outcome: Outcome(rep.Outcome), Output: rep.Output,
		Attempts: rep.Attempts, Duration: rep.Duration, StderrTail: rep.StderrTail,
		Model: rep.Model, Metrics: metricsOf(rep.Metrics), Hint: rep.Hint}
	if rep.Err != nil {
		res.Error = rep.Err.Error()
	}
	if rep.Run != nil {
		res.Run, res.Profile, res.Harness, res.Session = rep.Run.ID, rep.Run.Profile, rep.Run.Harness, rep.Run.Session
	}
	return res
}

// finished returns a finished turn's result, with its error unless it
// succeeded.
func finished(rep engine.Report) (*Result, error) {
	res := resultOf(rep)
	if rep.Run == nil { // it never started: there is no run to describe
		if rep.Err == nil {
			return nil, errors.New("the turn did not start")
		}
		return nil, rep.Err
	}
	if res.Outcome != Success && rep.Err == nil {
		return res, errors.New(string(res.Outcome))
	}
	return res, rep.Err
}

// Run is a run's record.
type Run struct {
	ID      string
	Profile string
	Harness string
	Cwd     string
	Parent  string // the run whose agent started this one, if any
	Depth   int    // 0 for a run started outside any run
	Kind    string // the label given when it started
	Session string
	Created time.Time
	Turns   []Turn
}

// Turn is the record of one turn.
type Turn struct {
	N        int
	Outcome  Outcome
	Attempts int
	ExitCode int // the CLI's exit code
	Queued   time.Time
	Started  time.Time
	Ended    time.Time
	Error    string
	Model    string
	Metrics  Metrics
	Hint     string
}

func runOf(r *engine.Run) Run {
	out := Run{ID: r.ID, Profile: r.Profile, Harness: r.Harness, Cwd: r.Cwd, Parent: r.Parent,
		Depth: r.Depth, Kind: r.Kind, Session: r.Session, Created: r.Created}
	for _, t := range r.Turns {
		out.Turns = append(out.Turns, Turn{N: t.N, Outcome: Outcome(t.Outcome), Attempts: t.Attempts,
			ExitCode: t.ExitCode, Queued: t.Queued, Started: t.Started, Ended: t.Ended, Error: t.Error,
			Model: t.Model, Metrics: metricsOf(t.Metrics), Hint: t.Hint})
	}
	return out
}
