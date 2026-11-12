package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// An attempt's clock: its timeout, its idle limit, and extensions.
//
// Herta's per-attempt timeout is a fixed deadline, set when the attempt
// begins, so nothing could give a long task more time. Harness runs the
// clock itself instead, as it runs retries: a watch cancels the attempt
// when its deadline passes, or when an agent whose CLI streams events has
// written nothing for its idle limit. Either way the outcome is uncertain,
// as a Herta timeout's is: the agent may already have done work.
//
// `harness extend` moves the deadline of a turn's attempts to at least a
// given time, from any process (the parent, or the agent itself), through
// runs/<id>/extend-<turn>; it also counts as activity, so an agent about to
// think for a long time can say so. A deadline's last fifth, or an idle
// limit's last quarter, is announced once with a timeout_soon event, which
// wakes a waiting parent so it can extend in time.
//
// A turn the idle limit stopped is parked, not retried: a fresh attempt
// would start the work again, and the stuck one's session may still be
// worth resuming. Its hint says how.

const (
	defaultIdle = 15 * time.Minute
	clockTick   = 200 * time.Millisecond
)

// Event kinds of the clock.
const (
	EvTimeoutSoon = "timeout_soon" // name: timeout or idle; text: how long is left and how to extend
	EvExtend      = "extend"       // text: the new deadline, and why
)

var (
	// ErrTimedOut ends an attempt that ran past its deadline.
	ErrTimedOut = errors.New("the attempt ran past its timeout")
	// ErrIdle ends an attempt whose agent wrote nothing for its idle limit.
	ErrIdle = errors.New("the agent wrote nothing for its idle limit")
)

// idleFor is the idle limit of a turn: the profile's, or the default for a
// CLI that streams events; none for one that prints only at the end, whose
// silence means nothing.
func idleFor(p *Profile, d *Definition) time.Duration {
	if p.IdleSet {
		return p.Idle
	}
	if d.Output.Format == "jsonl" {
		return defaultIdle
	}
	return 0
}

// extension is a turn's extend file.
type extension struct {
	Format int       `json:"format"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

func extendPath(run *Run, n int) string { return run.Path(fmt.Sprintf("extend-%d", n)) }

// readExtension is the turn's extension and when it was last made; zero if
// none.
func readExtension(run *Run, n int) (until, at time.Time) {
	path := extendPath(run, n)
	data, err := readShared(path)
	if err != nil {
		return
	}
	var e extension
	if json.Unmarshal(data, &e) != nil || e.Format > formatVersion {
		return
	}
	if fi, err := os.Stat(path); err == nil {
		at = fi.ModTime()
	}
	return e.Until, at
}

// Extend gives a run's unfinished turn until at least d from now, and
// returns the new deadline. by says who asked, for the event.
func (r *Runner) Extend(id string, d time.Duration, reason, by string) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, errors.New("extend by a positive duration, such as 30m")
	}
	var until time.Time
	_, err := r.home.updateRun(id, func(run *Run) error {
		n := len(run.Turns)
		if n == 0 || run.Turns[n-1].finished() {
			return fmt.Errorf("run %s has no unfinished turn to extend", id)
		}
		until = time.Now().Add(d).UTC().Truncate(time.Second)
		if prev, _ := readExtension(run, n); prev.After(until) {
			until = prev
		}
		data, _ := json.Marshal(extension{Format: formatVersion, Until: until, Reason: reason})
		if err := writeFileAtomic(extendPath(run, n), data); err != nil {
			return err
		}
		text := "until " + until.Local().Format("15:04:05")
		if reason != "" {
			text += ": " + reason
		}
		run.note(Event{Turn: n, Kind: EvExtend, Name: by, Text: text})
		return errNoSave
	})
	if errors.Is(err, errNoSave) {
		err = nil
	}
	return until, err
}

// errNoSave ends an updateRun change that wrote nothing to the record.
var errNoSave = errors.New("no change")

// watchClock cancels the attempt with ErrTimedOut or ErrIdle when its time
// is up, until stop is called.
func (t *Turn) watchClock(ctx context.Context, cancel context.CancelCauseFunc, outputs ...string) (stop func()) {
	timeout, idle := t.Profile.Timeout, t.idle
	if timeout <= 0 && idle <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(finished)
		tick := time.NewTicker(clockTick)
		defer tick.Stop()
		var warnedDeadline time.Time
		warnedIdle := false
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			now := time.Now()
			until, extendedAt := readExtension(t.Run, t.N)
			if timeout > 0 {
				deadline := start.Add(timeout)
				if until.After(deadline) {
					deadline = until
				}
				left := deadline.Sub(now)
				if left <= 0 {
					cancel(fmt.Errorf("%w (%s, extended to %s)", ErrTimedOut, timeout, deadline.Local().Format("15:04:05")))
					return
				}
				if left <= timeout/5 && !deadline.Equal(warnedDeadline) {
					warnedDeadline = deadline
					t.Run.note(Event{Turn: t.N, Attempt: t.attempts, Kind: EvTimeoutSoon, Name: "timeout",
						Text: fmt.Sprintf("%s left; give it more with: harness extend %s 30m", left.Round(time.Second), t.Run.ID)})
				}
			}
			if idle > 0 {
				last := start
				for _, p := range outputs {
					if fi, err := os.Stat(p); err == nil && fi.ModTime().After(last) {
						last = fi.ModTime()
					}
				}
				if extendedAt.After(last) {
					last = extendedAt
				}
				quiet := now.Sub(last)
				if quiet >= idle {
					cancel(fmt.Errorf("%w (%s)", ErrIdle, idle))
					return
				}
				if quiet >= idle*3/4 && !warnedIdle {
					warnedIdle = true
					t.Run.note(Event{Turn: t.N, Attempt: t.attempts, Kind: EvTimeoutSoon, Name: "idle",
						Text: fmt.Sprintf("quiet for %s of its %s idle limit; if it is still working: harness extend %s 30m", quiet.Round(time.Second), idle, t.Run.ID)})
				} else if quiet < idle/2 {
					warnedIdle = false
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

// clockFailure is the outcome of an attempt its clock stopped, if it did.
func clockFailure(actx context.Context) error {
	cause := context.Cause(actx)
	if errors.Is(cause, ErrTimedOut) || errors.Is(cause, ErrIdle) {
		return hertasdk.Fail(hertasdk.Uncertain, cause)
	}
	return nil
}

// parked is the hint of a turn its clock stopped.
func parkedHint(run *Run, err error) string {
	if !errors.Is(err, ErrTimedOut) && !errors.Is(err, ErrIdle) {
		return ""
	}
	return fmt.Sprintf("parked: the session is kept, and the agent may have done part of the work; see what it did with: harness status %s, then continue it with: harness send %s \"<what to do next>\"", run.ID, run.ID)
}
