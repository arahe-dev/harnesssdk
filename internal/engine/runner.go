package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Turn is one prompt sent to a run's harness. Herta executes it as one
// call: admission against the profile's budgets, then attempts.
type Turn struct {
	Run     *Run
	N       int
	Prompt  string
	Resume  bool
	Profile *Profile
	Def     *Definition
	Extra   Vars // per-invocation values (--set)

	home        string     // the data folder, for {harness_home}
	lineage     []string   // gate tickets held for this turn and its ancestors
	inherit     []*os.File // locks the agent CLI inherits (Unix)
	attempts    int
	argv        []string
	exitCode    int
	lastSession string
	stderrTail  string
}

// Result is a turn's answer.
type Result struct {
	Output  string
	Session string
}

// Report is how a turn ended, for printing.
type Report struct {
	Run        *Run
	Turn       int
	Output     string
	Outcome    string
	Err        error
	Attempts   int
	Duration   time.Duration
	StderrTail string
}

// Environment harness sets for every CLI it starts, so a harness command
// run by that agent shares the data folder and knows its ancestors.
const (
	EnvHome    = "HARNESS_HOME"
	EnvRun     = "HARNESS_RUN"
	EnvLineage = "HARNESS_LINEAGE"
)

// lostAfter is how long a queued turn may wait for its worker to start
// before it is reported as lost.
const lostAfter = 2 * time.Minute

// Runner executes turns: Herta applies each profile's contract, and the
// gate arbitrates budgets, serialize keys and max_running across every
// harness process on the machine.
type Runner struct {
	home *Home
	rt   *hertasdk.Runtime
	// kin is this process's parent run and lineage, found on first use:
	// only a new run or an executed turn needs them, so wait, result and
	// stop never pay for discovery.
	kin func() Parentage

	mu  sync.Mutex
	ops map[string]*hertasdk.Operation[*Turn, *Result]

	// Lifecycle: closing ends when Shutdown begins; active counts Execute
	// calls, so Shutdown can withdraw turns waiting at the gate and drain the
	// rest, as Herta does for its own waiters.
	closingCtx context.Context
	close      context.CancelFunc
	lifecycle  sync.Mutex
	active     sync.WaitGroup
}

// enter admits one Execute unless Shutdown has begun.
func (r *Runner) enter() bool {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if r.closingCtx.Err() != nil {
		return false
	}
	r.active.Add(1)
	return true
}

// Herta owns each turn's execution contract: its effect, retry table and
// per-attempt timeout, and the drain on shutdown. Budgets and serialize keys
// are arbitrated once, machine-wide, by the gate, so the runtime holds no
// budget; Herta needs one resource to exist, and no operation claims it.
func newRuntime() *hertasdk.Runtime {
	rt, err := hertasdk.NewRuntime(hertasdk.ResourceSpec{Name: "turns", Capacity: 1})
	if err != nil {
		panic(err) // a fixed, valid spec
	}
	return rt
}

// NewRunner builds a runner for this process: if an agent that harness
// started is running it, its parent run and lineage are found from the
// environment or, when a harness stripped that, from process ancestry.
func NewRunner(h *Home) (*Runner, error) {
	r := newRunner(h)
	r.kin = sync.OnceValue(h.DiscoverParentage)
	return r, nil
}

// NewRunnerFor builds a runner with an explicit parent run and lineage, for
// a detached worker that does not inherit its starter's environment.
func NewRunnerFor(h *Home, parent string, lineage []string) (*Runner, error) {
	r := newRunner(h)
	r.kin = func() Parentage { return Parentage{Parent: parent, Lineage: lineage} }
	return r, nil
}

func newRunner(h *Home) *Runner {
	closingCtx, closeFn := context.WithCancel(context.Background())
	return &Runner{
		home: h, rt: newRuntime(),
		ops:        map[string]*hertasdk.Operation[*Turn, *Result]{},
		closingCtx: closingCtx, close: closeFn,
	}
}

// depth is how deeply a run started by this process nests: one level
// below its parent run. It may not exceed config max_depth.
func (r *Runner) depth() (int, error) {
	parent := r.kin().Parent
	if parent == "" {
		return 0, nil
	}
	depth := 1
	if pr, err := r.home.LoadRun(parent); err == nil {
		depth = pr.Depth + 1
	}
	cfg, err := r.home.Config()
	if err != nil {
		return 0, err
	}
	if depth > cfg.MaxDepth {
		return 0, hertasdk.Fail(hertasdk.Permanent, fmt.Errorf("%w: this run would nest %d deep under run %s, and max_depth is %d; do the work here, or raise it with: harness config set max_depth %d",
			ErrTooDeep, depth, parent, cfg.MaxDepth, depth))
	}
	return depth, nil
}

// ErrTooDeep refuses a run nested deeper than config max_depth.
var ErrTooDeep = errors.New("runs nest too deep")

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Shutdown stops admitting turns, withdraws turns still waiting at the gate
// (they end as stopped), and waits for running ones to drain. It returns
// ctx.Err() if ctx ends first; draining continues either way.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.lifecycle.Lock()
	r.close()
	r.lifecycle.Unlock()
	drained := make(chan struct{})
	go func() {
		r.active.Wait()
		close(drained)
	}()
	herr := r.rt.Shutdown(ctx)
	select {
	case <-drained:
		return herr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func policyFor(p *Profile) hertasdk.Policy[*Turn] {
	return hertasdk.Policy[*Turn]{Effect: p.Effect, Timeout: p.Timeout, Retry: p.Retry}
}

// checkPolicy validates a profile's contract: Herta checks its effect and
// retry table (an unsafe retry fails here, not under load), and every budget
// it claims must exist, once, and fit.
func checkPolicy(p *Profile, budgets map[string]int64) error {
	noop := func(context.Context, *Turn) (*Result, error) { return nil, nil }
	if _, err := hertasdk.NewOperation(newRuntime(), p.Name, noop, policyFor(p)); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
	}
	seen := map[string]bool{}
	for _, req := range p.Budget {
		c, ok := budgets[req.Name]
		switch {
		case seen[req.Name]:
			return fmt.Errorf("profile %s: budget %q is listed twice", p.Name, req.Name)
		case !ok:
			return fmt.Errorf("profile %s: budget %q is not defined; set it with: harness budget set %s <capacity>", p.Name, req.Name, req.Name)
		case req.Units > c:
			return fmt.Errorf("profile %s: needs %d units of budget %s, whose capacity is %d", p.Name, req.Units, req.Name, c)
		}
		seen[req.Name] = true
	}
	return nil
}

// op returns the Herta operation for this exact version of the profile:
// the cache key is the whole profile file, so an edited profile never runs
// under the policy of an older one.
func (r *Runner) op(p *Profile) (*hertasdk.Operation[*Turn, *Result], error) {
	key := p.Name + "\x00" + string(p.Bytes())
	r.mu.Lock()
	defer r.mu.Unlock()
	if op, ok := r.ops[key]; ok {
		return op, nil
	}
	op, err := hertasdk.NewOperation(r.rt, p.Name, r.attempt, policyFor(p))
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	r.ops[key] = op
	return op, nil
}

// vars are the values every attempt of the turn shares. Built-ins always
// come from harness: an Extra value can never set one.
func (t *Turn) vars() Vars {
	v := Vars{}
	for k, x := range t.Profile.Vars {
		v[k] = x
	}
	for k, x := range t.Extra {
		if CheckVar(k) == nil {
			v[k] = x
		}
	}
	v["prompt"] = t.Prompt
	v["prompt_file"] = t.Run.Path(fmt.Sprintf("turn-%d.prompt.md", t.N))
	if t.Profile.System != "" {
		v["system_file"] = t.Run.Path("system.md")
	}
	v["cwd"], v["run_id"], v["run_dir"] = t.Run.Cwd, t.Run.ID, t.Run.dir
	v["harness_home"] = t.home
	v["lineage"] = strings.Join(t.lineage, ",")
	return v
}

// sessionVars fills the session placeholders for one attempt. A new turn on
// a minted session gets a fresh id per attempt, so a retry never resumes
// the half-finished conversation of the attempt before it.
func (t *Turn) sessionVars(v Vars) {
	switch {
	case t.Resume && t.Run.Session != "":
		v["session"], v["resume_session"] = t.Run.Session, t.Run.Session
	case !t.Resume && t.Def.Session == "mint":
		id := newUUID()
		v["session"], v["new_session"] = id, id
	}
}

// attempt is the Herta handler: one run of the harness CLI.
func (r *Runner) attempt(ctx context.Context, t *Turn) (*Result, error) {
	t.attempts++
	v := t.vars()
	t.sessionVars(v)
	t.lastSession = v["session"]
	argv, _, err := t.Def.Render(v)
	if err != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, err)
	}
	env, err := t.Def.environ(v)
	if err != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, err)
	}
	env = append(env, EnvHome+"="+r.home.Dir, EnvRun+"="+t.Run.ID, EnvLineage+"="+strings.Join(t.lineage, ","))
	t.argv = argv

	stdoutPath := t.Run.Path(fmt.Sprintf("turn-%d.stdout.log", t.N))
	stderrPath := t.Run.Path(fmt.Sprintf("turn-%d.stderr.log", t.N))
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, err)
	}
	defer stderr.Close()
	fmt.Fprintf(stderr, "== attempt %d: %s\n", t.attempts, strings.Join(argv, " "))
	// Only what the CLI writes in this attempt is evidence: not the trace
	// line above, and not an earlier attempt's output.
	mark, err := stderr.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, err)
	}

	var stdin *os.File
	if t.Def.Stdin != "none" {
		input := v["prompt_file"]
		if t.Def.Stdin == "system+prompt" && t.Profile.System != "" {
			input = t.Run.Path(fmt.Sprintf("turn-%d.input.md", t.N))
			text := "<instructions>\n" + t.Profile.System + "\n</instructions>\n\n" + t.Prompt
			if err := os.WriteFile(input, []byte(text), 0o600); err != nil {
				return nil, hertasdk.Fail(hertasdk.Permanent, err)
			}
		}
		if stdin, err = os.Open(input); err != nil {
			return nil, hertasdk.Fail(hertasdk.Permanent, err)
		}
		defer stdin.Close()
	}

	code, warnings, runErr := runProcess(ctx, procSpec{
		Argv: argv, Dir: t.Run.Cwd, Env: env,
		Stdin: stdin, Stdout: stdout, Stderr: stderr, Inherit: t.inherit,
		OnStart: func(pid int) {
			// Record this attempt's agent, so a nested run can find this
			// turn even when its sibling turns share this process.
			r.home.writeActive(activeMarker{Run: t.Run.ID, Turn: t.N, PID: os.Getpid(), AgentPID: pid, AgentStart: processStart(pid)})
		},
	})
	t.exitCode = code
	t.stderrTail = readTail(stderrPath, mark, 8<<10)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "== harness warning: %s\n", w)
	}

	// The attempt's own deadline or the caller ending: return the context
	// error untagged, so Herta reads an expired deadline as Uncertain and a
	// cancelled caller as cancellation.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s stopped after %d attempt(s): %w", t.Def.Name, t.attempts, err)
	}
	if runErr != nil {
		return nil, hertasdk.Fail(hertasdk.Permanent, fmt.Errorf("start %s: %w", t.Def.Name, runErr))
	}
	answer, failed, session, xerr := extractFile(t.Def, stdoutPath)
	if code == 0 && xerr == nil && !failed {
		if session == "" {
			session = v["session"]
		}
		return &Result{Output: answer, Session: session}, nil
	}
	if session != "" {
		t.lastSession = session
	}
	var cause error
	switch {
	case code != 0:
		cause = fmt.Errorf("%s exited with status %d%s", t.Def.Name, code, lastLine(t.stderrTail))
	case xerr != nil:
		cause = fmt.Errorf("%s: %w", t.Def.Name, xerr)
	default:
		cause = fmt.Errorf("%s reported an error: %s", t.Def.Name, firstLine(answer))
	}
	evidence := t.stderrTail + "\n" + readTail(stdoutPath, 0, 8<<10)
	return nil, hertasdk.Fail(t.Def.classify(evidence), cause)
}

func (r *Runner) load(profile, cwd string) (*Profile, *Definition, error) {
	p, err := r.home.Profile(profile, cwd)
	if err != nil {
		return nil, nil, err
	}
	d, err := r.home.Definition(p.Harness, cwd)
	if err != nil {
		return nil, nil, err
	}
	return p, d, nil
}

// request is what a queued turn needs besides its prompt.
type request struct {
	Resume bool `json:"resume"`
	Extra  Vars `json:"extra,omitempty"`
}

// queue appends a queued turn with its prompt and request. The caller owns
// the run: a new run, or one under its state lock.
func queue(run *Run, prompt string, req request) (int, error) {
	n := len(run.Turns) + 1
	if err := os.WriteFile(run.Path(fmt.Sprintf("turn-%d.prompt.md", n)), []byte(prompt), 0o600); err != nil {
		return 0, err
	}
	data, _ := json.Marshal(req)
	if err := os.WriteFile(run.Path(fmt.Sprintf("turn-%d.request.json", n)), data, 0o600); err != nil {
		return 0, err
	}
	run.Turns = append(run.Turns, TurnRecord{N: n, Queued: time.Now().UTC(), Outcome: "queued"})
	return n, nil
}

// Prepare records one new run per task, each with its first turn queued,
// after checking that the profile and its harness load from cwd.
func (r *Runner) Prepare(profile string, tasks []string, cwd string, extra Vars) ([]*Run, error) {
	p, d, err := r.load(profile, cwd)
	if err != nil {
		return nil, err
	}
	depth, err := r.depth()
	if err != nil {
		return nil, err
	}
	r.home.AutoGC()
	var runs []*Run
	for _, task := range tasks {
		run, err := r.home.NewRun(p.Name, d.Name, cwd, r.kin().Parent)
		if err != nil {
			return nil, err
		}
		run.Depth = depth
		if _, err := queue(run, task, request{Extra: extra}); err != nil {
			return nil, err
		}
		if err := run.Save(); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// PrepareSend queues the next turn of a run, in the same harness session.
// A run whose last turn has not finished refuses it as throttled.
func (r *Runner) PrepareSend(id, prompt string, extra Vars) (int, error) {
	var n int
	_, err := r.home.updateRun(id, func(run *Run) error {
		p, _, err := r.load(run.Profile, run.Cwd)
		if err != nil {
			return err
		}
		if p.Harness != run.Harness {
			return fmt.Errorf("run %s was started on harness %s, but profile %s now uses %s; a session cannot move between harnesses, so start a new run",
				id, run.Harness, p.Name, p.Harness)
		}
		if k := len(run.Turns); k > 0 && !run.Turns[k-1].finished() {
			if !r.reapLost(run) {
				return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf("run %s is busy with turn %d (%s): %w", id, k, run.Turns[k-1].Outcome, errBusy))
			}
		}
		if run.Session == "" {
			return fmt.Errorf("run %s has no session to resume: harness %s neither mints nor captures one", id, run.Harness)
		}
		n, err = queue(run, prompt, request{Resume: true, Extra: extra})
		return err
	})
	return n, err
}

// reapLost marks the run's last turn lost if no process is executing it: a
// running turn whose worker died (the agent may have done work, so the
// outcome is uncertain), or a queued turn whose worker never started. The
// caller holds the state lock. It reports whether the turn was reaped.
func (r *Runner) reapLost(run *Run) bool {
	rec := &run.Turns[len(run.Turns)-1]
	if run.executing() {
		return false
	}
	switch {
	case rec.Outcome == "running":
		rec.Outcome, rec.Error = "uncertain", "the process executing this turn exited without finishing it"
	case rec.Outcome == "queued" && time.Since(rec.Queued) > lostAfter:
		rec.Outcome, rec.Error = "transient", "no worker started this turn"
		// The environment handed to the worker that never came holds the
		// caller's secrets.
		os.Remove(run.Path(fmt.Sprintf("turn-%d.env", rec.N)))
	default:
		return false
	}
	rec.Ended = time.Now().UTC()
	return true
}

// Execute runs queued turn n of a run: it is what harness run does in
// process and what a detached worker does. The execution lock is held for
// the whole turn, so waiters can tell a live turn from a lost one.
func (r *Runner) Execute(ctx context.Context, id string, n int) Report {
	rep := Report{Turn: n}
	fail := func(err error) Report {
		rep.Err, rep.Outcome = err, OutcomeName(err)
		return rep
	}
	if !r.enter() {
		return fail(hertasdk.ErrRuntimeStopping)
	}
	defer r.active.Done()
	run, err := r.home.LoadRun(id)
	if err != nil {
		return fail(err)
	}
	rep.Run = run
	execLock, err := lockWaitOpen(run.Path("exec.lock"), 30*time.Second)
	if err != nil {
		return fail(fmt.Errorf("run %s: another process is executing it: %w", id, err))
	}
	defer execLock.Close()
	start := time.Now()
	run, err = r.home.updateRun(id, func(run *Run) error {
		if n != len(run.Turns) || run.Turns[n-1].Outcome != "queued" {
			state := "missing"
			if n <= len(run.Turns) {
				state = run.Turns[n-1].Outcome
			}
			return fmt.Errorf("turn %d of run %s is not queued (it is %s)", n, id, state)
		}
		rec := &run.Turns[n-1]
		rec.Outcome, rec.Started, rec.PID = "running", start.UTC(), os.Getpid()
		return nil
	})
	if err != nil {
		return fail(err)
	}
	rep.Run = run

	if err := r.home.writeActive(activeMarker{Run: run.ID, Turn: n, PID: os.Getpid()}); err != nil {
		return fail(err)
	}
	defer r.home.clearActive(run.ID)

	res, t, err := r.execute(ctx, run, n, execLock)
	run, ferr := r.finish(run.ID, n, t, res, err)
	if run != nil {
		rep.Run = run
	}
	if ferr != nil && err == nil {
		err = ferr
	}
	rep.Outcome, rep.Err = OutcomeName(err), err
	rep.Duration = time.Since(start)
	if t != nil {
		rep.Attempts, rep.StderrTail = t.attempts, t.stderrTail
	}
	if res != nil && err == nil {
		rep.Output = res.Output
	}
	return rep
}

// execute loads the turn's profile and request, passes the gate, and runs
// the Herta operation. A stop file for the turn cancels it; Shutdown
// withdraws it while it waits at the gate.
func (r *Runner) execute(ctx context.Context, run *Run, n int, execLock *os.File) (*Result, *Turn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go watchStop(ctx, run.Path(fmt.Sprintf("stop-%d", n)), cancel)

	var req request
	data, err := os.ReadFile(run.Path(fmt.Sprintf("turn-%d.request.json", n)))
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, nil, fmt.Errorf("turn %d request: %w", n, err)
	}
	prompt, err := os.ReadFile(run.Path(fmt.Sprintf("turn-%d.prompt.md", n)))
	if err != nil {
		return nil, nil, err
	}
	p, d, err := r.load(run.Profile, run.Cwd)
	if err != nil {
		return nil, nil, err
	}
	if d.Name != run.Harness {
		return nil, nil, fmt.Errorf("run %s was started on harness %s, but profile %s now uses %s", run.ID, run.Harness, p.Name, d.Name)
	}
	if p.System != "" {
		if err := os.WriteFile(run.Path("system.md"), []byte(p.System+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
	}
	t := &Turn{Run: run, N: n, Prompt: string(prompt), Resume: req.Resume, Profile: p, Def: d, Extra: req.Extra, home: r.home.Dir}

	// Validate the policy before waiting anywhere: a project file reaches
	// here without passing through SaveProfile.
	budgets, err := r.home.Budgets()
	if err != nil {
		return nil, t, err
	}
	if err := checkPolicy(p, budgets); err != nil {
		return nil, t, err
	}
	op, err := r.op(p)
	if err != nil {
		return nil, t, err
	}
	cfg, err := r.home.Config()
	if err != nil {
		return nil, t, err
	}
	claims := claimsFor(p, budgets, t)
	if cfg.MaxRunning > 0 {
		// A nested run never waits for a slot: its ancestors hold slots
		// while they wait for it, so waiting could last forever.
		nested := len(r.kin().Lineage) > 0
		claims = append(claims, Claim{Name: runningGate, Units: 1, Capacity: int64(cfg.MaxRunning), NoWait: nested})
	}
	gctx, gcancel := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(r.closingCtx, gcancel)
	ticket, err := r.home.Gate(r.kin().Lineage).Acquire(gctx, claims, p.Admission == hertasdk.Reject, run.ID)
	stopWatch()
	gcancel()
	switch {
	case errors.Is(err, errNoWait):
		return nil, t, hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"all %d turns max_running allows are running, and a nested run does not wait for one; raise it with: harness config set max_running <n>", cfg.MaxRunning))
	case errors.Is(err, errBusy):
		return nil, t, hertasdk.Fail(hertasdk.Throttled, fmt.Errorf("profile %s: its budget or key is in use and admission is reject", p.Name))
	case errors.Is(err, ErrWouldDeadlock):
		return nil, t, hertasdk.Fail(hertasdk.Permanent, err)
	case err != nil && r.closingCtx.Err() != nil && ctx.Err() == nil:
		return nil, t, hertasdk.ErrRuntimeStopping
	case err != nil:
		return nil, t, err
	}
	defer ticket.Release()
	t.lineage = slices.Clone(r.kin().Lineage)
	if ticket.ID != "" {
		t.lineage = append(t.lineage, ticket.ID)
	}
	// Record the lineage, so a nested run that only finds this turn through
	// process ancestry still learns which tickets its ancestors hold.
	if _, err := r.home.updateRun(run.ID, func(run *Run) error {
		run.Turns[n-1].Lineage = t.lineage
		return nil
	}); err != nil {
		return nil, t, err
	}
	// On Unix the agent inherits these locks: while any process of its tree
	// runs, the turn counts as executing and its budget stays held, even if
	// this process dies.
	t.inherit = []*os.File{execLock}
	if ticket.Live != nil {
		t.inherit = append(t.inherit, ticket.Live)
	}
	res, err := op.Do(ctx, t)
	return res, t, err
}

// claimsFor are the gate claims of a turn: its budgets, at their current
// capacities (checkPolicy has verified them), and its serialize key.
func claimsFor(p *Profile, budgets map[string]int64, t *Turn) []Claim {
	var claims []Claim
	for _, req := range p.Budget {
		claims = append(claims, Claim{Name: req.Name, Units: req.Units, Capacity: budgets[req.Name]})
	}
	if p.Serialize != "" {
		if k, _ := expandString(p.Serialize, t.vars()); k != "" {
			claims = append(claims, keyClaim(k))
		}
	}
	return claims
}

// finish records a turn's outcome. The answer is saved before the record,
// so the record never claims an answer that is not on disk.
func (r *Runner) finish(id string, n int, t *Turn, res *Result, err error) (*Run, error) {
	var saveErr error
	run, uerr := r.home.updateRun(id, func(run *Run) error {
		switch {
		case res != nil:
			run.Session = res.Session
			if werr := os.WriteFile(run.Path(fmt.Sprintf("turn-%d.out.md", n)), []byte(res.Output+"\n"), 0o600); werr != nil {
				saveErr = fmt.Errorf("the agent answered, but saving the answer failed: %w", werr)
			}
		case t != nil && !t.Resume && t.lastSession != "":
			// Keep the failed turn's session so it can be resumed with send.
			run.Session = t.lastSession
		}
		if err == nil {
			err = saveErr
		}
		rec := &run.Turns[n-1]
		rec.Ended = time.Now().UTC()
		rec.Outcome = OutcomeName(err)
		rec.Error = ""
		if err != nil {
			rec.Error = err.Error()
		}
		if t != nil {
			rec.Attempts, rec.ExitCode, rec.Command = t.attempts, t.exitCode, t.argv
		}
		return nil // always save: the record must leave "running"
	})
	if uerr != nil {
		return run, fmt.Errorf("the turn finished, but saving the run record failed (it may still show running): %w", uerr)
	}
	return run, saveErr
}

// watchStop cancels when a stop file appears.
func watchStop(ctx context.Context, path string, cancel context.CancelFunc) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Start runs each task as a new run of the profile, in this process. Tasks
// run concurrently, admitted against the profile's budgets. started, if
// set, is called with each run before any of them executes.
func (r *Runner) Start(ctx context.Context, profile string, tasks []string, cwd string, extra Vars, started func(*Run)) ([]Report, error) {
	runs, err := r.Prepare(profile, tasks, cwd, extra)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if started != nil {
			started(run)
		}
	}
	reports := make([]Report, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Go(func() { reports[i] = r.Execute(ctx, run.ID, 1) })
	}
	wg.Wait()
	return reports, nil
}

// Send continues a run with one more turn in the same harness session, in
// this process.
func (r *Runner) Send(ctx context.Context, id, prompt string, extra Vars) (Report, error) {
	n, err := r.PrepareSend(id, prompt, extra)
	if err != nil {
		return Report{}, err
	}
	return r.Execute(ctx, id, n), nil
}

// Stop asks a run's unfinished turn to stop and returns its number. The
// process executing it notices within a quarter second.
func (r *Runner) Stop(id string) (int, error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return 0, err
	}
	n := len(run.Turns)
	if n == 0 || run.Turns[n-1].finished() {
		return n, fmt.Errorf("run %s has no unfinished turn", id)
	}
	return n, os.WriteFile(run.Path(fmt.Sprintf("stop-%d", n)), nil, 0o600)
}

// Wait blocks until the run's last turn finishes or ctx ends, and reports
// it. finished is false if ctx ended first. A turn whose process died is
// reported, and recorded, as lost.
func (r *Runner) Wait(ctx context.Context, id string) (rep Report, finished bool, err error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return Report{}, false, err
	}
	return r.WaitTurn(ctx, id, len(run.Turns))
}

// WaitTurn is Wait for turn n: a turn queued after it does not change what
// is waited for.
func (r *Runner) WaitTurn(ctx context.Context, id string, n int) (rep Report, finished bool, err error) {
	// The record is read when it changes, and every few seconds to notice a
	// turn whose process died without recording an end.
	record := filepath.Join(r.home.Dir, "runs", id, "run.json")
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var run *Run
	var stamp string
	var read time.Time
	for {
		if s := fileStamp(record); run == nil || s != stamp || time.Since(read) > 3*time.Second {
			stamp, read = s, time.Now()
			if run, err = r.home.LoadRun(id); err != nil {
				return Report{}, false, err
			}
			if n < 1 || n > len(run.Turns) {
				return Report{}, false, fmt.Errorf("run %s has no turn %d", id, n)
			}
			// Only the last turn can be unfinished: a turn is queued only
			// after the one before it finished.
			if !run.Turns[n-1].finished() && !run.executing() {
				if run, err = r.home.updateRun(id, func(run *Run) error {
					if !run.Turns[len(run.Turns)-1].finished() {
						r.reapLost(run)
					}
					return nil
				}); err != nil {
					return Report{}, false, err
				}
			}
			if run.Turns[n-1].finished() {
				return reportOf(run, n), true, nil
			}
		}
		select {
		case <-ctx.Done():
			return reportOf(run, n), false, nil
		case <-tick.C:
		}
	}
}

// fileStamp identifies a file's current version: its modification time
// and size, or "" if it is missing.
func fileStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
}

// reportOf builds a report from a run's record of turn n.
func reportOf(run *Run, n int) Report {
	rec := run.Turns[n-1]
	rep := Report{Run: run, Turn: n, Outcome: rec.Outcome, Attempts: rec.Attempts}
	if rec.Error != "" {
		rep.Err = errors.New(rec.Error)
	}
	if !rec.Started.IsZero() {
		end := rec.Ended
		if end.IsZero() {
			end = time.Now()
		}
		rep.Duration = end.Sub(rec.Started)
	}
	if out, err := run.Output(n); err == nil {
		rep.Output = strings.TrimRight(out, "\n")
	}
	if rec.finished() && rec.Outcome != "success" {
		rep.StderrTail = readTail(run.Path(fmt.Sprintf("turn-%d.stderr.log", n)), 0, 8<<10)
	}
	return rep
}

// Plan renders what a turn would run, without running it.
type Plan struct {
	Argv    []string
	Dropped []Dropped
	Env     []string
	Stdin   bool
	Profile *Profile
	Def     *Definition
}

// DryRun renders the command a new run of the profile would execute.
func (r *Runner) DryRun(profile, prompt, cwd string, extra Vars) (*Plan, error) {
	p, d, err := r.load(profile, cwd)
	if err != nil {
		return nil, err
	}
	run := &Run{ID: "<run-id>", Cwd: cwd, dir: "<run-dir>"}
	return plan(&Turn{Run: run, N: 1, Prompt: prompt, Profile: p, Def: d, Extra: extra, home: r.home.Dir})
}

// DryRunSend renders the command a follow-up turn of a run would execute.
func (r *Runner) DryRunSend(id, prompt string, extra Vars) (*Plan, error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return nil, err
	}
	p, d, err := r.load(run.Profile, run.Cwd)
	if err != nil {
		return nil, err
	}
	return plan(&Turn{Run: run, N: len(run.Turns) + 1, Prompt: prompt, Resume: true, Profile: p, Def: d, Extra: extra, home: r.home.Dir})
}

func plan(t *Turn) (*Plan, error) {
	d := t.Def
	v := t.vars()
	t.sessionVars(v)
	argv, dropped, err := d.Render(v)
	if err != nil {
		return nil, err
	}
	env, err := d.environ(v)
	if err != nil {
		return nil, err
	}
	return &Plan{Argv: argv, Dropped: dropped, Env: env, Stdin: d.Stdin != "none", Profile: t.Profile, Def: d}, nil
}

// readTail returns at most the last n bytes of path written after offset
// from.
func readTail(path string, from, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(from, fi.Size()-n)
	if start >= fi.Size() {
		return ""
	}
	b := make([]byte, fi.Size()-start)
	m, _ := f.ReadAt(b, start)
	return string(b[:m])
}

func extractFile(d *Definition, path string) (answer string, failed bool, session string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, "", err
	}
	defer f.Close()
	return d.extract(f)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "== ") {
			return ": " + l
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
