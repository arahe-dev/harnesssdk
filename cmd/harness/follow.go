package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	harness "github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// Following a run while it works: status (where it is, what it is doing,
// what it changed) and events (its event log, from a cursor).

func (c *cli) statusCmd(args []string) (int, error) {
	fs := c.flags("status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return 0, c.status()
	}
	if len(pos) != 1 {
		return harness.ExitUsage, usageErr("status takes at most one <run-id>")
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	st, err := r.Status(context.Background(), pos[0])
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if *asJSON {
		data, _ := json.MarshalIndent(statusJSON(st), "", "  ")
		fmt.Fprintln(c.stdout, string(data))
	} else {
		printStatus(c.stdout, st)
	}
	return 0, nil
}

func (c *cli) events(args []string) (int, error) {
	fs := c.flags("events")
	since := fs.Int64("since", 0, "the cursor to read from (0: the start); each read prints the next one")
	wait := fs.Duration("wait", 0, "wait this long for a new event, or for the turn to end (exit 7: nothing new yet)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) != 1 {
		return harness.ExitUsage, usageErr("events needs <run-id>")
	}
	id := pos[0]
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx := context.Background()
	if *wait <= 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel() // read what is there
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *wait)
		defer cancel()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	evs, next, done, err := r.WaitEvents(ctx, id, *since)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if *asJSON {
		out := struct {
			Events   []harness.Event `json:"events"`
			Cursor   int64           `json:"cursor"`
			Finished bool            `json:"finished"`
		}{evs, next, done}
		if out.Events == nil {
			out.Events = []harness.Event{}
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
	} else {
		for _, ev := range evs {
			fmt.Fprintln(c.stdout, eventLine(ev))
		}
		state := "still going"
		if done {
			state = "its last turn has finished: harness result " + id
		}
		fmt.Fprintf(c.stderr, "harness: cursor %d; %s; next: harness events %s --since %d --wait 2m\n", next, state, id, next)
	}
	if len(evs) == 0 && !done && *wait > 0 {
		return harness.ExitRunning, nil
	}
	return 0, nil
}

// eventLine is one event on a line: time, turn, kind, name, text.
func eventLine(ev harness.Event) string {
	parts := []string{ev.Time.Local().Format("15:04:05"), fmt.Sprintf("turn %d", ev.Turn), ev.Kind}
	if ev.Attempt > 1 && ev.Kind == harness.EvAttempt {
		parts[2] = fmt.Sprintf("attempt %d", ev.Attempt)
	}
	if ev.Name != "" {
		parts = append(parts, ev.Name)
	}
	if ev.Text != "" {
		parts = append(parts, ev.Text)
	}
	return strings.Join(parts, "  ")
}

// printStatus writes a run's status for an agent or a person to read.
func printStatus(w io.Writer, st *harness.Status) {
	run := st.Run
	model := ""
	if st.Turn > 0 {
		if m := run.Turns[st.Turn-1].Model; m != "" {
			model = " (" + m + ")"
		}
	}
	fmt.Fprintf(w, "run:      %s · profile %s on %s%s · turn %d\n", run.ID, run.Profile, run.Harness, model, st.Turn)
	if st.Turn == 0 {
		return
	}
	phase := st.Phase
	if st.Attempt > 0 && st.MaxAttempts > 0 {
		phase += fmt.Sprintf(" · attempt %d of %d", st.Attempt, st.MaxAttempts)
	}
	if !st.Started.IsZero() {
		end := run.Turns[st.Turn-1].Ended
		if end.IsZero() {
			end = time.Now()
		}
		phase += " · " + end.Sub(st.Started).Round(time.Second).String()
	}
	if st.Phase == "running" && !st.AttemptAt.IsZero() && st.Timeout > 0 {
		left := st.Timeout - time.Since(st.AttemptAt)
		phase += fmt.Sprintf(" · %s left of its %s timeout", max(left, 0).Round(time.Second), st.Timeout)
	}
	fmt.Fprintf(w, "phase:    %s\n", phase)
	if st.Phase == "running" && !st.LastOutput.IsZero() {
		quiet := time.Since(st.LastOutput).Round(time.Second)
		if st.Live {
			fmt.Fprintf(w, "output:   last %s ago (its CLI streams events as it works)\n", quiet)
		} else {
			fmt.Fprintf(w, "output:   its CLI prints only when it is done; follow its files below\n")
		}
	}
	if s := spend(st.Metrics); s != "" {
		fmt.Fprintf(w, "spent:    %s\n", strings.TrimPrefix(s, " · "))
	}
	if len(st.Activity) > 0 {
		for i, ev := range st.Activity {
			label := "          "
			if i == 0 {
				label = "activity: "
			}
			fmt.Fprintf(w, "%s%s\n", label, eventLine(ev))
		}
	}
	if st.Said != "" {
		fmt.Fprintf(w, "said:     %s\n", st.Said)
	}
	if ch := st.Changes; ch != nil {
		line := ch.Scope + ": " + ch.Summary()
		if ch.Scope == "folder" {
			line += " (since the last commit, by anyone)"
		}
		if len(ch.Files) > 0 {
			files := ch.Files
			more := ""
			if len(files) > 6 {
				files, more = files[:6], fmt.Sprintf(", +%d more", len(ch.Files)-6)
			}
			line += ": " + strings.Join(files, ", ") + more
		}
		fmt.Fprintf(w, "changes:  %s\n", line)
	}
	if st.Error != "" {
		fmt.Fprintf(w, "error:    %s\n", st.Error)
	}
	if st.Hint != "" {
		fmt.Fprintf(w, "hint:     %s\n", st.Hint)
	}
	for i, n := range st.Next() {
		label := "          "
		if i == 0 {
			label = "next:     "
		}
		fmt.Fprintf(w, "%s%s\n", label, n)
	}
}

type jsonStatus struct {
	Run         string             `json:"run"`
	Profile     string             `json:"profile"`
	Harness     string             `json:"harness"`
	Model       string             `json:"model,omitempty"`
	Turn        int                `json:"turn"`
	Outcome     string             `json:"outcome"`
	Phase       string             `json:"phase"`
	Attempt     int                `json:"attempt,omitempty"`
	MaxAttempts int                `json:"max_attempts,omitempty"`
	ElapsedMS   int64              `json:"elapsed_ms,omitempty"`
	TimeoutMS   int64              `json:"timeout_ms,omitempty"`
	TimeLeftMS  *int64             `json:"time_left_ms,omitempty"`
	QuietMS     *int64             `json:"quiet_ms,omitempty"`
	Live        bool               `json:"live"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	Activity    []harness.Event    `json:"activity,omitempty"`
	Said        string             `json:"said,omitempty"`
	Changes     *jsonChanges       `json:"changes,omitempty"`
	Error       string             `json:"error,omitempty"`
	Hint        string             `json:"hint,omitempty"`
	Cursor      int64              `json:"cursor"`
	Next        []string           `json:"next,omitempty"`
}

type jsonChanges struct {
	Scope   string   `json:"scope"`
	Summary string   `json:"summary"`
	Files   []string `json:"files,omitempty"`
}

func statusJSON(st *harness.Status) jsonStatus {
	run := st.Run
	j := jsonStatus{Run: run.ID, Profile: run.Profile, Harness: run.Harness, Turn: st.Turn, Outcome: st.Outcome, Phase: st.Phase,
		Attempt: st.Attempt, MaxAttempts: st.MaxAttempts, TimeoutMS: st.Timeout.Milliseconds(), Live: st.Live,
		Metrics: st.Metrics, Activity: st.Activity, Said: st.Said, Error: st.Error, Hint: st.Hint, Cursor: st.Cursor, Next: st.Next()}
	if st.Turn > 0 {
		rec := run.Turns[st.Turn-1]
		j.Model = rec.Model
		if !st.Started.IsZero() {
			end := rec.Ended
			if end.IsZero() {
				end = time.Now()
			}
			j.ElapsedMS = end.Sub(st.Started).Milliseconds()
		}
	}
	if st.Phase == "running" {
		if !st.AttemptAt.IsZero() && st.Timeout > 0 {
			left := max(st.Timeout-time.Since(st.AttemptAt), 0).Milliseconds()
			j.TimeLeftMS = &left
		}
		if !st.LastOutput.IsZero() {
			quiet := time.Since(st.LastOutput).Milliseconds()
			j.QuietMS = &quiet
		}
	}
	if ch := st.Changes; ch != nil {
		j.Changes = &jsonChanges{Scope: ch.Scope, Summary: ch.Summary(), Files: ch.Files}
	}
	return j
}
