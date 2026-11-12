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

func (c *cli) extend(args []string) (int, error) {
	fs := c.flags("extend")
	self := fs.Bool("self", false, "extend the run this agent is running in (for an agent that harness started)")
	reason := fs.String("reason", "", "why, for the run's event log")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	id, by := "", "caller"
	switch {
	case *self && len(pos) == 1:
		p := c.home.DiscoverParentage()
		if p.Parent == "" {
			return harness.ExitUsage, usageErr("--self: this process is not running inside a harness run")
		}
		id, by = p.Parent, "self"
	case !*self && len(pos) == 2:
		id = pos[0]
		pos = pos[1:]
	default:
		return harness.ExitUsage, usageErr("extend needs <run-id> <duration>, or --self <duration>")
	}
	d, err := time.ParseDuration(pos[0])
	if err != nil {
		return harness.ExitUsage, usageErr("duration %q: use a form such as 30m or 1h30m", pos[0])
	}
	until, err := r.Extend(id, d, *reason, by)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stdout, "%s may run until %s (%s from now)\n", id, until.Local().Format("15:04:05"), time.Until(until).Round(time.Second))
	return 0, nil
}

func (c *cli) handover(args []string) (int, error) {
	fs := c.flags("handover")
	next := fs.String("next", "", "what the new run should do next (default: continue the work)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) != 1 {
		return harness.ExitUsage, usageErr("handover needs <run-id>")
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(c.stderr, "harness: asking %s for handover notes\n", pos[0])
	nr, rep, err := r.Handover(ctx, pos[0], *next)
	c.drain(r)
	if err != nil {
		if rep.Run != nil {
			c.status1(rep)
		}
		return exitFor(err), err
	}
	fmt.Fprintf(c.stderr, "harness: %s handed over to %s, started in the background; its notes are turn %d of %s\n", pos[0], nr.ID, rep.Turn, pos[0])
	if *asJSON {
		data, _ := json.MarshalIndent(struct {
			Run   string `json:"run"`
			From  string `json:"from"`
			Notes string `json:"notes"`
		}{nr.ID, pos[0], rep.Output}, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
	} else {
		fmt.Fprintln(c.stdout, nr.ID)
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
	if st.Phase == "running" && !st.Deadline.IsZero() {
		left := max(time.Until(st.Deadline), 0).Round(time.Second)
		if st.Extended {
			phase += fmt.Sprintf(" · %s left (extended to %s)", left, st.Deadline.Local().Format("15:04:05"))
		} else {
			phase += fmt.Sprintf(" · %s left of its %s timeout", left, st.Timeout)
		}
	}
	fmt.Fprintf(w, "phase:    %s\n", phase)
	if st.Phase == "running" && !st.LastOutput.IsZero() {
		quiet := time.Since(st.LastOutput).Round(time.Second)
		if st.Live {
			limit := ""
			if st.Idle > 0 {
				limit = fmt.Sprintf("; stopped after %s quiet", st.Idle)
			}
			fmt.Fprintf(w, "output:   last %s ago (its CLI streams events as it works%s)\n", quiet, limit)
		} else {
			fmt.Fprintf(w, "output:   its CLI prints only when it is done; follow its files below\n")
		}
	}
	if s := spend(st.Metrics); s != "" {
		fmt.Fprintf(w, "spent:    %s\n", strings.TrimPrefix(s, " · "))
	}
	if line := contextLine(st); line != "" {
		fmt.Fprintf(w, "context:  %s\n", line)
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
	for i, a := range st.Asks {
		label := "          "
		if i == 0 {
			label = "asks:     "
		}
		fmt.Fprintf(w, "%s%s (waiting for your answer): %s\n", label, a.Name, a.Text)
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

// contextLine is how full the agent's context is, as far as it is known.
func contextLine(st *harness.Status) string {
	used := map[string]float64{"t": st.ContextTokens}
	switch share, ok := st.ContextUsed(); {
	case ok:
		return fmt.Sprintf("%.0f%% (%s of %s tokens)", share*100, tokens(used, "t"), tokens(map[string]float64{"t": st.ContextWindow}, "t"))
	case st.ContextTokens > 0:
		return tokens(used, "t") + " tokens (window unknown: set context_window in the profile)"
	}
	return ""
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
	Deadline    *time.Time         `json:"deadline,omitempty"`
	QuietMS     *int64             `json:"quiet_ms,omitempty"`
	IdleMS      int64              `json:"idle_ms,omitempty"`
	Live        bool               `json:"live"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	// Context: tokens in the agent's context, its model's window, and the
	// share used, when known.
	ContextTokens float64         `json:"context_tokens,omitempty"`
	ContextWindow float64         `json:"context_window,omitempty"`
	ContextUsed   *float64        `json:"context_used,omitempty"`
	Activity      []harness.Event `json:"activity,omitempty"`
	Said          string          `json:"said,omitempty"`
	Asks          []harness.Event `json:"asks,omitempty"`
	Changes       *jsonChanges    `json:"changes,omitempty"`
	Error         string          `json:"error,omitempty"`
	Hint          string          `json:"hint,omitempty"`
	Cursor        int64           `json:"cursor"`
	Next          []string        `json:"next,omitempty"`
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
		Metrics: st.Metrics, Activity: st.Activity, Said: st.Said, Asks: st.Asks, Error: st.Error, Hint: st.Hint, Cursor: st.Cursor, Next: st.Next()}
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
	j.IdleMS = st.Idle.Milliseconds()
	if st.Phase == "running" {
		if !st.Deadline.IsZero() {
			left := max(time.Until(st.Deadline), 0).Milliseconds()
			j.TimeLeftMS = &left
			j.Deadline = &st.Deadline
		}
		if !st.LastOutput.IsZero() {
			quiet := time.Since(st.LastOutput).Milliseconds()
			j.QuietMS = &quiet
		}
	}
	j.ContextTokens, j.ContextWindow = st.ContextTokens, st.ContextWindow
	if share, ok := st.ContextUsed(); ok {
		share = float64(int(share*1000+0.5)) / 1000
		j.ContextUsed = &share
	}
	if ch := st.Changes; ch != nil {
		j.Changes = &jsonChanges{Scope: ch.Scope, Summary: ch.Summary(), Files: ch.Files}
	}
	return j
}

// Messages between a run's agent and its caller.

func (c *cli) noteCmd(args []string) (int, error) {
	if len(args) == 0 {
		return harness.ExitUsage, usageErr("note needs a line of text")
	}
	run, err := c.home.Note(strings.Join(args, " "))
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: noted in run %s\n", run.ID)
	return 0, nil
}

func (c *cli) ask(args []string) (int, error) {
	fs := c.flags("ask")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the answer (exit 7 if none came)")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("ask needs a question")
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	run, id, answer, answered, err := r.Ask(ctx, strings.Join(pos, " "))
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if !answered {
		fmt.Fprintf(c.stderr, "harness: no answer to %s within %s; go on with your best judgement and say what you assumed, or check later with: harness inbox\n", id, *wait)
		return harness.ExitRunning, nil
	}
	fmt.Fprintln(c.stdout, answer)
	fmt.Fprintf(c.stderr, "harness: answer to %s in run %s\n", id, run.ID)
	return 0, nil
}

func (c *cli) reply(args []string) (int, error) {
	if len(args) < 2 {
		return harness.ExitUsage, usageErr("reply needs <run-id> [question-id] \"answer\"")
	}
	id, qid, answer := args[0], "", strings.Join(args[1:], " ")
	if len(args) > 2 && strings.HasPrefix(args[1], "q-") && !strings.Contains(args[1], " ") {
		qid, answer = args[1], strings.Join(args[2:], " ")
	}
	qid, err := c.home.Reply(id, qid, answer)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: answered %s in run %s\n", qid, id)
	return 0, nil
}

func (c *cli) steer(args []string) (int, error) {
	fs := c.flags("steer")
	now := fs.Bool("now", false, "stop the running turn and continue the session with the message at once")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) < 2 {
		return harness.ExitUsage, usageErr("steer needs <run-id> \"message\"")
	}
	id, text := pos[0], strings.Join(pos[1:], " ")
	if !*now {
		mid, err := c.home.Steer(id, text)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		fmt.Fprintf(c.stderr, "harness: posted %s to run %s: its agent reads it with harness inbox, or at the top of its next turn\n", mid, id)
		return 0, nil
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	n, err := r.SteerNow(context.Background(), id, text)
	if err != nil {
		return exitFor(err), err
	}
	fmt.Fprintf(c.stderr, "harness: %s continues with the message as turn %d, in the background; follow it with: harness wait %s\n", id, n, id)
	return 0, nil
}

func (c *cli) inbox(args []string) (int, error) {
	fs := c.flags("inbox")
	peek := fs.Bool("peek", false, "show the messages without marking them read")
	if _, err := parse(fs, args); err != nil {
		return harness.ExitUsage, err
	}
	run, msgs, err := c.home.Inbox(*peek)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	for _, m := range msgs {
		fmt.Fprintf(c.stdout, "%s  %s\n", m.Time.Local().Format("15:04:05"), m.Text)
	}
	if len(msgs) == 0 {
		fmt.Fprintf(c.stderr, "harness: no new messages for run %s\n", run.ID)
	}
	return 0, nil
}
