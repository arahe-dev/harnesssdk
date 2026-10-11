package harnesssdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// Runner executes turns under one Herta runtime built from the budgets, so
// every turn started by this process shares the same capacities.
type Runner struct {
	home *Home
	rt   *hertasdk.Runtime

	mu  sync.Mutex
	ops map[string]*hertasdk.Operation[*Turn, *Result]
}

// unclaimed keeps the runtime valid when no budget is defined: Herta needs
// at least one resource. It is not a valid user name, so it cannot collide.
const unclaimed = "_unclaimed"

func runtimeFor(budgets map[string]int64) (*hertasdk.Runtime, error) {
	specs := []hertasdk.ResourceSpec{{Name: unclaimed, Capacity: 1}}
	for name, c := range budgets {
		specs = append(specs, hertasdk.ResourceSpec{Name: name, Capacity: c})
	}
	return hertasdk.NewRuntime(specs...)
}

// NewRunner builds the runtime from the stored budgets.
func NewRunner(h *Home) (*Runner, error) {
	budgets, err := h.Budgets()
	if err != nil {
		return nil, err
	}
	rt, err := runtimeFor(budgets)
	if err != nil {
		return nil, err
	}
	return &Runner{home: h, rt: rt, ops: map[string]*hertasdk.Operation[*Turn, *Result]{}}, nil
}

// Shutdown stops admitting turns and waits for running ones to drain.
func (r *Runner) Shutdown(ctx context.Context) error { return r.rt.Shutdown(ctx) }

func policyFor(p *Profile) hertasdk.Policy[*Turn] {
	pol := hertasdk.Policy[*Turn]{
		Effect:    p.Effect,
		Resources: p.Budget,
		Admission: p.Admission,
		Timeout:   p.Timeout,
		Retry:     p.Retry,
	}
	if tmpl := p.Serialize; tmpl != "" {
		pol.SerializeKey = func(t *Turn) string {
			k, _ := expandString(tmpl, t.vars())
			return k
		}
	}
	return pol
}

// checkPolicy builds the profile's Herta operation against the budgets, so
// an unknown budget or an unsafe retry contract fails when it is saved.
func checkPolicy(p *Profile, budgets map[string]int64) error {
	rt, err := runtimeFor(budgets)
	if err != nil {
		return err
	}
	noop := func(context.Context, *Turn) (*Result, error) { return nil, nil }
	if _, err := hertasdk.NewOperation(rt, p.Name, noop, policyFor(p)); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
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
// come from harness: an Extra value can never set or clear one.
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
		if stdin, err = os.Open(v["prompt_file"]); err != nil {
			return nil, hertasdk.Fail(hertasdk.Permanent, err)
		}
		defer stdin.Close()
	}

	code, warnings, runErr := runProcess(ctx, procSpec{
		Argv: argv, Dir: t.Run.Cwd, Env: env,
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	t.exitCode = code
	out, _ := os.ReadFile(stdoutPath)
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
	answer, failed, session, xerr := t.Def.extract(out)
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
	evidence := t.stderrTail + "\n" + string(lastBytes(out, 8<<10))
	return nil, hertasdk.Fail(t.Def.classify(evidence), cause)
}

// Start runs each task as a new run of the profile. Tasks run concurrently
// and Herta admits them against the profile's budgets. started, if set, is
// called with each run before any of them executes.
func (r *Runner) Start(ctx context.Context, profile string, tasks []string, cwd string, extra Vars, started func(*Run)) ([]Report, error) {
	p, d, err := r.load(profile)
	if err != nil {
		return nil, err
	}
	runs := make([]*Run, len(tasks))
	for i := range tasks {
		if runs[i], err = r.home.NewRun(p.Name, d.Name, cwd); err != nil {
			return nil, err
		}
		if started != nil {
			started(runs[i])
		}
	}
	reports := make([]Report, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Go(func() { reports[i] = r.turn(ctx, runs[i], p, d, task, false, extra) })
	}
	wg.Wait()
	return reports, nil
}

// Send continues a run with one more turn in the same harness session,
// using the run's current profile and harness definition.
func (r *Runner) Send(ctx context.Context, id, prompt string, extra Vars) (Report, error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return Report{}, err
	}
	p, d, err := r.load(run.Profile)
	if err != nil {
		return Report{}, err
	}
	if p.Harness != run.Harness {
		return Report{}, fmt.Errorf("run %s was started on harness %s, but profile %s now uses %s; a session cannot move between harnesses, so start a new run",
			id, run.Harness, p.Name, p.Harness)
	}
	return r.turn(ctx, run, p, d, prompt, true, extra), nil
}

func (r *Runner) load(profile string) (*Profile, *Definition, error) {
	p, err := r.home.Profile(profile)
	if err != nil {
		return nil, nil, err
	}
	d, err := r.home.Definition(p.Harness)
	if err != nil {
		return nil, nil, err
	}
	return p, d, nil
}

// turn records and executes one turn. The run's lock keeps two turns of the
// same run from driving one harness session at once, across processes.
func (r *Runner) turn(ctx context.Context, run *Run, p *Profile, d *Definition, prompt string, resume bool, extra Vars) Report {
	rep := Report{Run: run}
	fail := func(err error) Report {
		rep.Err, rep.Outcome = err, OutcomeName(err)
		return rep
	}
	unlock, err := lockFile(run.Path("lock"))
	switch {
	case errors.Is(err, errBusy):
		// Another turn holds the session: try again later, like a full budget.
		return fail(hertasdk.Fail(hertasdk.Throttled, fmt.Errorf("run %s is busy with another turn: %w", run.ID, err)))
	case err != nil:
		return fail(fmt.Errorf("lock run %s: %w", run.ID, err))
	}
	defer unlock()
	if resume {
		// Reread under the lock: a turn that finished while this one waited
		// to load has moved the record on.
		fresh, err := r.home.LoadRun(run.ID)
		if err != nil {
			return fail(err)
		}
		*run = *fresh
		if run.Session == "" {
			return fail(fmt.Errorf("run %s has no session to resume: harness %s neither mints nor captures one", run.ID, d.Name))
		}
	}

	n := len(run.Turns) + 1
	rep.Turn = n
	t := &Turn{Run: run, N: n, Prompt: prompt, Resume: resume, Profile: p, Def: d, Extra: extra}
	if err := os.WriteFile(t.vars()["prompt_file"], []byte(prompt), 0o600); err != nil {
		return fail(err)
	}
	if p.System != "" {
		if err := os.WriteFile(run.Path("system.md"), []byte(p.System+"\n"), 0o600); err != nil {
			return fail(err)
		}
	}
	start := time.Now()
	run.Turns = append(run.Turns, TurnRecord{N: n, Started: start.UTC(), Outcome: "running"})
	if err := run.Save(); err != nil {
		return fail(err)
	}

	op, err := r.op(p)
	var res *Result
	if err == nil {
		res, err = op.Do(ctx, t)
	}

	switch {
	case res != nil:
		run.Session = res.Session
		rep.Output = res.Output
		if werr := os.WriteFile(run.Path(fmt.Sprintf("turn-%d.out.md", n)), []byte(res.Output+"\n"), 0o600); werr != nil {
			err = fmt.Errorf("the agent answered, but saving the answer failed: %w", werr)
		}
	case !resume && t.lastSession != "":
		// Keep the failed turn's session so it can be resumed with send.
		run.Session = t.lastSession
	}
	// Record the outcome last, so the record and the report agree.
	rec := &run.Turns[n-1]
	rec.Ended = time.Now().UTC()
	rec.Outcome = OutcomeName(err)
	rec.Attempts, rec.ExitCode, rec.Command = t.attempts, t.exitCode, t.argv
	if err != nil {
		rec.Error = err.Error()
	}
	if serr := run.Save(); serr != nil && err == nil {
		err = fmt.Errorf("the turn finished, but saving the run record failed (it still shows running): %w", serr)
	}
	rep.Outcome, rep.Err = OutcomeName(err), err
	rep.Attempts, rep.Duration, rep.StderrTail = t.attempts, time.Since(start), t.stderrTail
	return rep
}

// Plan renders what a turn would run, without running it.
type Plan struct {
	Argv    []string
	Dropped []Dropped
	Env     []string
	Stdin   bool
}

// DryRun renders the command a new run of the profile would execute.
func (r *Runner) DryRun(profile, prompt, cwd string, extra Vars) (*Plan, error) {
	p, d, err := r.load(profile)
	if err != nil {
		return nil, err
	}
	run := &Run{ID: "<run-id>", Cwd: cwd, dir: "<run-dir>"}
	return plan(&Turn{Run: run, N: 1, Prompt: prompt, Profile: p, Def: d, Extra: extra})
}

// DryRunSend renders the command a follow-up turn of a run would execute.
func (r *Runner) DryRunSend(id, prompt string, extra Vars) (*Plan, error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return nil, err
	}
	p, d, err := r.load(run.Profile)
	if err != nil {
		return nil, err
	}
	return plan(&Turn{Run: run, N: len(run.Turns) + 1, Prompt: prompt, Resume: true, Profile: p, Def: d, Extra: extra})
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
	return &Plan{Argv: argv, Dropped: dropped, Env: env, Stdin: d.Stdin != "none"}, nil
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

func lastBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "== attempt") {
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
