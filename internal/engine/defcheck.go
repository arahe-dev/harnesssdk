package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	env, err := d.environ(Vars{})
	if err != nil {
		return c, err
	}
	cmd := exec.CommandContext(vctx, c.Path, args...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return c, fmt.Errorf("harness %s: %s %s: %w: %s", name, d.Command, strings.Join(args, " "), err, firstLine(out.String()))
	}
	c.Version = firstLine(out.String())
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
	c.Answered = c.Live.Err == nil && strings.Contains(c.Live.Output, checkReply)
	for k := range c.Live.Metrics {
		c.Metrics = append(c.Metrics, k)
	}
	return c, nil
}
