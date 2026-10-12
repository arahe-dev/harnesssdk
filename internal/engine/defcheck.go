package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// CLIs change under their definitions: a new version renames a flag or
// reshapes its events, and the first sign is a real task failing. harness
// def check looks first: the CLI is on PATH and reports its version, and
// with a profile, a tiny turn answers and its metrics are found.

// DefCheck is what CheckDefinition found.
type DefCheck struct {
	Definition *Definition
	Path       string // where the CLI was found
	Version    string // the first line it printed for its version
	Live       *Report
	Answered   bool     // the live turn answered as asked
	Metrics    []string // the metrics the live turn reported
	Missing    []string // the metrics the definition declares that it did not
}

// answeredAsAsked: the answer is the requested reply, give or take
// surrounding space, quotes and a final period; an answer that only
// contains it (an echo of the prompt) is not.
func answeredAsAsked(output string) bool {
	return strings.Trim(strings.TrimSpace(output), "`'\". ") == checkReply
}

// checkReply is what the live turn is asked to answer.
const checkReply = "harness-check-ok"

// CheckDefinition checks definition name as a run in cwd would load it.
// With a profile, it also runs one live turn on it (this spends tokens).
func (r *Runner) CheckDefinition(ctx context.Context, name, cwd, profile string) (*DefCheck, error) {
	d, err := r.home.Definition(name, cwd)
	if err != nil {
		return nil, err
	}
	c := &DefCheck{Definition: d}
	if c.Path, err = exec.LookPath(d.Command); err != nil {
		return c, fmt.Errorf("harness %s: %s is not on PATH: %w", name, d.Command, err)
	}
	args := d.Version
	if len(args) == 0 {
		args = []string{"--version"}
	}
	env, err := d.environ(Vars{})
	if err != nil {
		return c, err
	}
	// Run as agents are: its whole process tree ends at the deadline, and
	// output goes to a file, so a child holding it open cannot stall us.
	out, err := os.CreateTemp("", "harness-version-*")
	if err != nil {
		return c, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, _, err := runProcess(vctx, procSpec{Argv: append([]string{c.Path}, args...), Env: env, Stdout: out, Stderr: out})
	text, _ := readAll(out)
	switch {
	case err != nil:
		return c, fmt.Errorf("harness %s: %s %s: %w", name, d.Command, strings.Join(args, " "), err)
	case vctx.Err() != nil:
		return c, fmt.Errorf("harness %s: %s %s did not finish within 30s", name, d.Command, strings.Join(args, " "))
	case code != 0:
		return c, fmt.Errorf("harness %s: %s %s exited with status %d: %s", name, d.Command, strings.Join(args, " "), code, firstLine(text))
	}
	c.Version = firstLine(text)
	if profile == "" {
		return c, nil
	}
	p, err := r.home.Profile(profile, cwd)
	if err != nil {
		return c, err
	}
	if p.Harness != name {
		return c, fmt.Errorf("profile %s runs on harness %s, not %s", profile, p.Harness, name)
	}
	reps, err := r.Start(ctx, profile, []string{"Reply with exactly: " + checkReply}, cwd, TurnOptions{Kind: "check"}, nil)
	if err != nil {
		return c, err
	}
	c.Live = &reps[0]
	c.Answered = c.Live.Err == nil && answeredAsAsked(c.Live.Output)
	for k := range c.Live.Metrics {
		c.Metrics = append(c.Metrics, k)
	}
	// Every metric the definition declares should have come out: one that
	// did not is a CLI that changed its output, and a spending limit that
	// no longer sees its costs.
	declared := maps.Keys(d.Output.Metrics)
	if d.Output.Report != nil {
		declared = func(yield func(string) bool) {
			for k := range d.Output.Metrics {
				if !yield(k) {
					return
				}
			}
			for k := range d.Output.Report.Metrics {
				if !yield(k) {
					return
				}
			}
		}
	}
	for k := range declared {
		if _, ok := c.Live.Metrics[k]; !ok {
			c.Missing = append(c.Missing, k)
		}
	}
	slices.Sort(c.Missing)
	return c, nil
}
