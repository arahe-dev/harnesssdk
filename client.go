package harnesssdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// Client runs agents through one data folder. Its methods are safe for
// concurrent use, and it shares budgets, keys and limits with every other
// harness process on the machine.
type Client struct {
	home   *engine.Home
	runner *engine.Runner
}

// Options configure Open.
type Options struct {
	// Home is the data folder. Empty means $HARNESS_HOME, or ~/.harness:
	// the folder the harness command uses.
	Home string
}

// Open opens the data folder, creating it if needed. A program that opens
// a Client must call Subcommand first in main.
func Open(o Options) (*Client, error) {
	var h *engine.Home
	var err error
	if o.Home == "" {
		h, err = engine.OpenHome()
	} else {
		h, err = engine.NewHome(o.Home)
	}
	if err != nil {
		return nil, err
	}
	r, err := engine.NewRunner(h)
	if err != nil {
		return nil, err
	}
	return &Client{home: h, runner: r}, nil
}

// Close stops admitting turns, withdraws turns still waiting at the gate,
// and waits for running ones to end, until ctx is done. Background runs
// (Start, SendStart) are separate processes and keep going.
func (c *Client) Close(ctx context.Context) error { return c.runner.Shutdown(ctx) }

// Home returns the data folder.
func (c *Client) Home() string { return c.home.Dir }

// Request is a new run: one agent with its first prompt.
type Request struct {
	Profile string
	Prompt  string
	// Cwd is the agent's working folder, which also selects the project
	// whose .harness folder is searched first. Empty means the current
	// folder.
	Cwd string
	// Vars sets profile placeholders for this run only.
	Vars map[string]string
	// Kind labels the run for Stats, such as "review".
	Kind string
	// Check is a command line run as is in the agent's folder once it
	// answers; the turn succeeds only if it exits 0. It replaces the
	// profile's check.
	Check string
	// Worktree runs the agent in a git worktree of its own, on branch
	// harness/<run>, which keeps its work.
	Worktree bool
}

func (r Request) options(vars engine.Vars) engine.TurnOptions {
	return engine.TurnOptions{Extra: vars, Kind: r.Kind, Check: r.Check, Worktree: r.Worktree}
}

// FollowUp is the next turn of a run, in the same session.
type FollowUp struct {
	Prompt string
	Vars   map[string]string
	Check  string // replaces the profile's check for this turn
}

// Run runs one agent and waits for its answer. The error is non-nil
// whenever the turn did not succeed; the Result is non-nil once the run
// exists, and says how it ended.
func (c *Client) Run(ctx context.Context, req Request) (*Result, error) {
	cwd, vars, err := req.check()
	if err != nil {
		return nil, err
	}
	reps, err := c.runner.Start(ctx, req.Profile, []string{req.Prompt}, cwd, req.options(vars), nil)
	if err != nil {
		return nil, err
	}
	return finished(reps[0])
}

// Compare runs req.Prompt on each profile side by side (req.Profile is
// ignored) and returns their results in order. The error is non-nil if no
// run could be created; each result says how its turn ended.
func (c *Client) Compare(ctx context.Context, profiles []string, req Request) ([]*Result, error) {
	cwd, vars, err := req.check()
	if err != nil {
		return nil, err
	}
	reps, err := c.runner.Compare(ctx, profiles, req.Prompt, cwd, req.options(vars), nil)
	if err != nil {
		return nil, err
	}
	out := make([]*Result, len(reps))
	for i, rep := range reps {
		out[i] = resultOf(rep)
	}
	return out, nil
}

// Start starts one agent in the background and returns its run id. The
// run continues if this program exits.
func (c *Client) Start(req Request) (string, error) {
	cwd, vars, err := req.check()
	if err != nil {
		return "", err
	}
	runs, err := c.runner.Prepare(req.Profile, []string{req.Prompt}, cwd, req.options(vars))
	if err != nil {
		return "", err
	}
	return runs[0].ID, c.runner.Detach(runs[0].ID, 1)
}

// Send runs a follow-up turn in a run's session and waits for its answer.
func (c *Client) Send(ctx context.Context, run string, f FollowUp) (*Result, error) {
	if err := checkSend(f.Prompt, f.Vars); err != nil {
		return nil, err
	}
	rep, err := c.runner.Send(ctx, run, f.Prompt, engine.TurnOptions{Extra: f.Vars, Check: f.Check})
	if err != nil {
		return nil, err
	}
	return finished(rep)
}

// SendStart queues a follow-up turn and runs it in the background. Wait
// or Result reads its answer.
func (c *Client) SendStart(run string, f FollowUp) error {
	if err := checkSend(f.Prompt, f.Vars); err != nil {
		return err
	}
	n, err := c.runner.PrepareSend(run, f.Prompt, engine.TurnOptions{Extra: f.Vars, Check: f.Check})
	if err != nil {
		return err
	}
	return c.runner.Detach(run, n)
}

func checkSend(prompt string, vars map[string]string) error {
	if prompt == "" {
		return errors.New("empty prompt")
	}
	return checkVars(vars)
}

// ErrRunning is returned with the Result of a turn that has not finished.
var ErrRunning = errors.New("the turn has not finished")

// Wait waits for a run's last turn to end and returns how it ended. If ctx
// ends first, it returns the turn as it is with ErrRunning.
func (c *Client) Wait(ctx context.Context, run string) (*Result, error) {
	rep, done, err := c.runner.Wait(ctx, run)
	if err != nil {
		return nil, err
	}
	if !done {
		res := resultOf(rep)
		res.Outcome = Outcome(rep.Run.Turns[rep.Turn-1].Outcome)
		return res, ErrRunning
	}
	return finished(rep)
}

// Result returns a run's last turn without waiting: ErrRunning if it has
// not finished.
func (c *Client) Result(run string) (*Result, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return c.Wait(ctx, run)
}

// Stop asks a run's unfinished turn to stop; Wait returns once it has.
func (c *Client) Stop(run string) error {
	_, err := c.runner.Stop(run)
	return err
}

// Runs lists runs, newest first; limit ≤ 0 lists all.
func (c *Client) Runs(limit int) ([]Run, error) {
	runs, err := c.home.Runs(limit)
	if err != nil {
		return nil, err
	}
	out := make([]Run, len(runs))
	for i, r := range runs {
		out[i] = runOf(r)
	}
	return out, nil
}

// Scope says where SetDefinition and SetProfile write. The zero Scope is
// the data folder, which every run sees.
type Scope struct {
	// Project, when set, is a folder inside a project: the file is written
	// to that project's .harness folder (created in this folder if there is
	// none above it) and trusted as written. Runs in the project prefer it.
	Project string
}

func (c *Client) projectDir(s Scope) (string, error) {
	if s.Project == "" {
		return "", nil
	}
	return c.home.ProjectDir(s.Project)
}

// SetDefinition validates and stores a harness definition (JSON): how to
// call one harness's headless CLI.
func (c *Client) SetDefinition(name string, definition []byte, s Scope) error {
	dir, err := c.projectDir(s)
	if err != nil {
		return err
	}
	_, err = c.home.SaveDefinition(name, definition, dir)
	return err
}

// SetProfile validates and stores a profile (Markdown with a header): an
// agent on a defined harness, with its model, tools, system prompt and
// execution policy. Its harness must be defined first.
func (c *Client) SetProfile(name string, profile []byte, s Scope) error {
	p, err := engine.ParseProfile(name, profile)
	if err != nil {
		return err
	}
	dir, err := c.projectDir(s)
	if err != nil {
		return err
	}
	cwd := s.Project
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return c.home.SaveProfile(p, dir, cwd)
}

// SetBudget sets a named capacity shared by every harness process on the
// machine; profiles claim units of it. Capacity 0 removes it.
func (c *Client) SetBudget(name string, capacity int) error {
	return c.home.SetBudget(name, int64(capacity))
}

// Config is the machine-wide limits in the data folder.
type Config struct {
	// MaxDepth is how deep runs may nest; 0 allows no nesting.
	MaxDepth int
	// MaxRunning caps the turns running at once on the machine; 0 means
	// no cap. A top-level turn waits for a slot; a nested one fails as
	// Throttled.
	MaxRunning int
	// RetentionDays is how long finished runs are kept; 0 keeps them.
	RetentionDays int
	// RepeatWarn and RepeatRefuse are how many times in a row the same
	// request may fail before the caller is warned (Result.Hint), and
	// before it is refused; 0 turns either off.
	RepeatWarn, RepeatRefuse int
	// MaxWaitMinutes bounds a turn's wait for its budgets unless its
	// profile sets max_wait; MaxQueue is how many turns may wait for one;
	// SpendLimitUSD caps the machine's reported spend over 24 hours. 0
	// means no limit.
	MaxWaitMinutes, MaxQueue, SpendLimitUSD int
}

// Config reads the limits.
func (c *Client) Config() (Config, error) {
	cfg, err := c.home.Config()
	return Config{MaxDepth: cfg.MaxDepth, MaxRunning: cfg.MaxRunning, RetentionDays: cfg.RetentionDays,
		RepeatWarn: cfg.RepeatWarn, RepeatRefuse: cfg.RepeatRefuse,
		MaxWaitMinutes: cfg.MaxWaitMinutes, MaxQueue: cfg.MaxQueue, SpendLimitUSD: cfg.SpendLimitUSD}, err
}

// SetConfig sets one limit: max_depth, max_running, retention_days,
// repeat_warn, repeat_refuse, max_wait_minutes, max_queue or
// spend_limit_usd.
func (c *Client) SetConfig(key string, value int) error { return c.home.SetConfig(key, value) }

// GC removes runs last active before olderThan ago that are not running,
// and returns their ids. With dryRun it only lists them.
func (c *Client) GC(olderThan time.Duration, dryRun bool) ([]string, error) {
	rep, err := c.home.GC(time.Now().Add(-olderThan), dryRun)
	return rep.Runs, err
}

// StatsFilter selects the turns Stats counts. The zero filter counts every
// kept run.
type StatsFilter struct {
	Profile string
	Kind    string
	Since   time.Time
}

// Stats is how one profile did on one model, for one kind of run.
type Stats struct {
	Profile  string
	Model    string
	Kind     string
	Turns    int
	Outcomes map[Outcome]int
	Attempts int
	Checks   int // turns that ran a check
	Passed   int // and passed it
	Median   time.Duration
	P90      time.Duration
	// Metrics are each metric's mean over the turns that reported it, and
	// Reported how many turns did: a mean over fewer than Turns covers only
	// some of them.
	Metrics  map[string]float64
	Reported map[string]int
}

// Stats sums up finished turns by profile, model and kind, most used
// first: how they ended, how long they took, and their metrics.
func (c *Client) Stats(f StatsFilter) ([]Stats, error) {
	st, err := c.home.Stats(engine.StatsFilter{Profile: f.Profile, Kind: f.Kind, Since: f.Since})
	if err != nil {
		return nil, err
	}
	out := make([]Stats, len(st))
	for i, s := range st {
		o := Stats{Profile: s.Profile, Model: s.Model, Kind: s.Kind, Turns: s.Turns, Attempts: s.Attempts, Checks: s.Checks, Passed: s.Passed,
			Median: s.Median, P90: s.P90, Outcomes: map[Outcome]int{}}
		for k, n := range s.Outcomes {
			o.Outcomes[Outcome(k)] = n
		}
		for k, m := range s.Metrics {
			if o.Metrics == nil {
				o.Metrics, o.Reported = map[string]float64{}, map[string]int{}
			}
			o.Metrics[k], o.Reported[k] = m.Mean(), m.Turns
		}
		out[i] = o
	}
	return out, nil
}

// DefCheck is what CheckDefinition found.
type DefCheck struct {
	Path     string  // where the CLI was found
	Version  string  // the first line of its version
	Live     *Result // the live turn, when a profile was given
	Answered bool    // it answered as asked
	Metrics  []string
	Missing  []string // declared metrics the live turn did not report
}

// CheckDefinition checks that a definition's CLI is on PATH and reports
// its version; with a profile on that harness, it also runs one short
// live turn (spending tokens) and checks the answer and metrics come out.
func (c *Client) CheckDefinition(ctx context.Context, name, cwd, profile string) (*DefCheck, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	dc, err := c.runner.CheckDefinition(ctx, name, cwd, profile)
	if dc == nil {
		return nil, err
	}
	out := &DefCheck{Path: dc.Path, Version: dc.Version, Answered: dc.Answered, Metrics: dc.Metrics, Missing: dc.Missing}
	if dc.Live != nil {
		out.Live = resultOf(*dc.Live)
	}
	return out, err
}

func (r Request) check() (string, engine.Vars, error) {
	if r.Prompt == "" {
		return "", nil, errors.New("empty prompt")
	}
	if err := checkVars(r.Vars); err != nil {
		return "", nil, err
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("cwd %s is not a folder", abs)
	}
	return abs, r.Vars, nil
}

func checkVars(vars map[string]string) error {
	for k := range vars {
		if err := engine.CheckVar(k); err != nil {
			return err
		}
	}
	return nil
}

// Subcommand runs the hidden subcommands harness starts its own executable
// with: the detached worker of a background run and, on Unix, the guard of
// each agent's process tree. A program that opens a Client must call it
// first in main and exit with its code when it reports handled:
//
//	if code, ok := harnesssdk.Subcommand(); ok {
//		os.Exit(code)
//	}
func Subcommand() (code int, handled bool) { return engine.Subcommand() }
