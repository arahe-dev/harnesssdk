package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// When an agent's context fills up, its work is better continued by a
// fresh session than squeezed into what is left. A turn that ends with its
// context past the profile's handover_at (80% by default) says so in its
// hint, and Handover does it: it asks the agent for handover notes, in its
// own session, and starts a new run of the same profile, in the same
// folder, with those notes and the next task. The new run records the run
// it came from, and the old run's log names the new one.

const defaultHandoverAt = 0.8

// EvHandover is noted on a run handed over; text: the new run.
const EvHandover = "handover"

// handoverPrompt asks an agent for the notes a fresh session needs.
const handoverPrompt = `Your context is nearly full, so a fresh session will continue this work. It will see nothing but what you write now. Write handover notes for it: the goal; what is done (with file paths); what is left, in order; decisions made and why; anything that failed or must be avoided; and how to check the work. Be complete but brief. Do not do any more of the work now.`

// contextShare is how full a turn's context is, 0 to 1, if known: the
// turn's last context_tokens against the profile's context_window, or the
// window the CLI reported in this turn or an earlier one.
func contextShare(run *Run, p *Profile, metrics map[string]float64) (float64, bool) {
	tokens := metrics["context_tokens"]
	window := float64(p.ContextWindow)
	if window == 0 {
		window = metrics["context_window"]
	}
	for i := len(run.Turns) - 1; i >= 0 && window == 0; i-- {
		window = run.Turns[i].Metrics["context_window"]
	}
	if tokens <= 0 || window <= 0 {
		return 0, false
	}
	return tokens / window, true
}

func handoverAt(p *Profile) float64 {
	if p.HandoverSet {
		return p.HandoverAt
	}
	return defaultHandoverAt
}

// handoverHint is the hint of a turn that ended with its context past the
// profile's handover_at.
func handoverHint(run *Run, t *Turn) string {
	if t == nil || t.Profile == nil {
		return ""
	}
	at := handoverAt(t.Profile)
	share, ok := contextShare(run, t.Profile, t.metrics)
	if !ok || at <= 0 || share < at {
		return ""
	}
	return fmt.Sprintf("the agent's context is %.0f%% full; continue in a fresh session with: harness handover %s --next \"<the next task>\"", share*100, run.ID)
}

// Handover asks a run's agent for handover notes in its session, then
// starts a new run of its profile, in its folder, with the notes and next
// (or "continue the work"). It returns the new run, executed by a detached
// worker, and the notes turn's report.
func (r *Runner) Handover(ctx context.Context, id, next string) (*Run, Report, error) {
	old, err := r.home.LoadRun(id)
	if err != nil {
		return nil, Report{}, err
	}
	rep, err := r.Send(ctx, id, handoverPrompt, TurnOptions{})
	if err != nil {
		return nil, rep, err
	}
	if rep.Err != nil || strings.TrimSpace(rep.Output) == "" {
		return nil, rep, fmt.Errorf("run %s did not write handover notes (%s): %w", id, rep.Outcome, errors.Join(rep.Err, errors.New("no new run was started")))
	}
	if next == "" {
		next = "Continue the work."
	}
	prompt := fmt.Sprintf("You are continuing work started in harness run %s, whose context filled up. Its handover notes:\n\n%s\n\nNext: %s", id, strings.TrimSpace(rep.Output), next)
	runs, err := r.Prepare(old.Profile, []string{prompt}, old.AgentDir(), TurnOptions{Kind: old.Kind})
	if err != nil {
		return nil, rep, err
	}
	nr := runs[0]
	if saved, err := r.home.updateRun(nr.ID, func(run *Run) error {
		run.From = id
		return nil
	}); err == nil {
		nr = saved
	}
	old.note(Event{Turn: rep.Turn, Kind: EvHandover, Name: nr.ID, Text: "handed over to run " + nr.ID})
	if err := r.Detach(nr.ID, 1); err != nil {
		return nr, rep, err
	}
	return nr, rep, nil
}
