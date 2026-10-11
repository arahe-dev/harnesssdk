// Command harness runs agents in other harnesses as subagents. Run
// "harness guide" for the manual.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	harness "github.com/arahe-dev/harnesssdk"
)

const usage = `harness runs agents in other harnesses (Pi, Claude Code, Codex, ...) as subagents.

  harness guide                          the manual: read this first
  harness run <profile> [prompt | -]     run an agent; flags: --task --file --cwd --set --json --dry-run
  harness send <run-id> [prompt | -]     follow up in the same session; flags: --file --set --json --dry-run
  harness runs | show <run-id>           past runs
  harness profile set|show|list|rm       agents (Markdown with a header)
  harness def set|show|list|rm           harness definitions (JSON)
  harness budget set|list|rm             shared capacity limits
  harness home | skill | version
`

// errUsage marks a usage or configuration error (exit 2).
type errUsage struct{ error }

func usageErr(format string, a ...any) error { return errUsage{fmt.Errorf(format, a...)} }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return harness.ExitUsage
	}
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr}
	code, err := c.dispatch(args[0], args[1:])
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		if code == 0 {
			code = harness.ExitUsage
		}
	}
	return code
}

type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	home           *harness.Home
}

func (c *cli) dispatch(cmd string, args []string) (int, error) {
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, usage)
		return 0, nil
	case "guide":
		fmt.Fprint(c.stdout, harness.Guide)
		return 0, nil
	case "skill":
		fmt.Fprint(c.stdout, harness.Skill)
		return 0, nil
	case "version":
		fmt.Fprintln(c.stdout, "harness", harness.Version)
		return 0, nil
	}
	h, err := harness.OpenHome()
	if err != nil {
		return harness.ExitUsage, err
	}
	c.home = h
	switch cmd {
	case "home":
		fmt.Fprintln(c.stdout, h.Dir)
		return 0, nil
	case "def":
		return 0, c.def(args)
	case "profile":
		return 0, c.profile(args)
	case "budget":
		return 0, c.budget(args)
	case "run":
		return c.runCmd(args)
	case "send":
		return c.send(args)
	case "runs":
		return 0, c.runs(args)
	case "show":
		return 0, c.show(args)
	}
	return harness.ExitUsage, usageErr("unknown command %q; see: harness help", cmd)
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// parse lets flags and positional arguments interleave; "--" ends flags.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (c *cli) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	return fs
}

// read returns a file's contents, or stdin for "-".
func (c *cli) read(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(c.stdin)
	}
	return os.ReadFile(path)
}

func (c *cli) def(args []string) error {
	if len(args) == 0 {
		return usageErr("def needs set, show, list or rm")
	}
	fs := c.flags("def " + args[0])
	file := fs.String("file", "", "definition JSON file, or - for stdin")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		names, err := c.home.List("harnesses")
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		for _, n := range names {
			d, err := c.home.Definition(n)
			if err != nil {
				fmt.Fprintf(w, "%s\t(invalid: %v)\n", n, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", n, d.Command, d.Description)
		}
		return w.Flush()
	case "set", "show", "rm":
	default:
		return usageErr("unknown def command %q", args[0])
	}
	if len(pos) != 1 {
		return usageErr("def %s needs one harness name", args[0])
	}
	name := pos[0]
	switch args[0] {
	case "set":
		if *file == "" {
			return usageErr("def set needs --file <path> or --file - (JSON on stdin)")
		}
		data, err := c.read(*file)
		if err != nil {
			return err
		}
		if _, err := c.home.SaveDefinition(name, data); err != nil {
			return errUsage{err}
		}
		fmt.Fprintf(c.stdout, "saved harness %s\n", name)
		return nil
	case "show":
		d, err := c.home.Definition(name)
		if err != nil {
			return errUsage{err}
		}
		data, _ := json.MarshalIndent(d, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		return nil
	}
	return c.home.Remove("harnesses", name)
}

func (c *cli) profile(args []string) error {
	if len(args) == 0 {
		return usageErr("profile needs set, show, list or rm")
	}
	fs := c.flags("profile " + args[0])
	file := fs.String("file", "", "profile Markdown file, or - for stdin")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		names, err := c.home.List("profiles")
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		for _, n := range names {
			p, err := c.home.Profile(n)
			if err != nil {
				fmt.Fprintf(w, "%s\t(invalid: %v)\n", n, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, p.Harness, p.Vars["model"], p.Description)
		}
		return w.Flush()
	case "set", "show", "rm":
	default:
		return usageErr("unknown profile command %q", args[0])
	}
	if len(pos) == 0 {
		return usageErr("profile %s needs a profile name", args[0])
	}
	name, pairs := pos[0], pos[1:]
	switch args[0] {
	case "show":
		p, err := c.home.Profile(name)
		if err != nil {
			return errUsage{err}
		}
		c.stdout.Write(p.Bytes())
		return nil
	case "rm":
		return c.home.Remove("profiles", name)
	}
	var p *harness.Profile
	switch {
	case *file != "":
		data, err := c.read(*file)
		if err != nil {
			return err
		}
		if p, err = harness.ParseProfile(name, data); err != nil {
			return errUsage{err}
		}
		if len(pairs) > 0 {
			if p, err = p.Set(pairs); err != nil {
				return errUsage{err}
			}
		}
	case len(pairs) > 0:
		cur, err := c.home.Profile(name)
		if err != nil {
			return errUsage{err}
		}
		if p, err = cur.Set(pairs); err != nil {
			return errUsage{err}
		}
	default:
		return usageErr("profile set needs --file <path|-> or key=value pairs")
	}
	if err := c.home.SaveProfile(p); err != nil {
		return errUsage{err}
	}
	fmt.Fprintf(c.stdout, "saved profile %s (harness %s)\n", name, p.Harness)
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	if plan, err := r.DryRun(name, "<prompt>", cwd, nil); err == nil {
		c.printPlan(plan)
	}
	return nil
}

func (c *cli) budget(args []string) error {
	if len(args) == 0 {
		return usageErr("budget needs set, list or rm")
	}
	switch args[0] {
	case "list":
		b, err := c.home.Budgets()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		for _, name := range slices.Sorted(maps.Keys(b)) {
			fmt.Fprintf(w, "%s\t%d\n", name, b[name])
		}
		return w.Flush()
	case "set":
		if len(args) != 3 {
			return usageErr("budget set needs <name> <capacity>")
		}
		n, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || n < 1 {
			return usageErr("capacity must be a positive integer")
		}
		return c.home.SetBudget(args[1], n)
	case "rm":
		if len(args) != 2 {
			return usageErr("budget rm needs <name>")
		}
		return c.home.SetBudget(args[1], 0)
	}
	return usageErr("unknown budget command %q", args[0])
}

// turnFlags are shared by run and send.
type turnFlags struct {
	files, sets multi
	asJSON, dry bool
}

func (tf *turnFlags) register(fs *flag.FlagSet) {
	fs.Var(&tf.files, "file", "append a file to the prompt (repeatable)")
	fs.Var(&tf.sets, "set", "key=value placeholder value for this run (repeatable)")
	fs.BoolVar(&tf.asJSON, "json", false, "print a JSON result")
	fs.BoolVar(&tf.dry, "dry-run", false, "print the command without running it")
}

func (tf *turnFlags) extra() (harness.Vars, error) {
	v := harness.Vars{}
	for _, kv := range tf.sets {
		k, val, ok := strings.Cut(kv, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok {
			return nil, usageErr("--set %q is not key=value", kv)
		}
		if err := harness.CheckVar(k); err != nil {
			return nil, usageErr("--set: %v", err)
		}
		v[k] = val
	}
	return v, nil
}

// prompt joins the prompt words (or stdin for "-") with the attached files.
func (c *cli) prompt(words []string, files []string) (string, error) {
	text := strings.Join(words, " ")
	if len(words) == 1 && words[0] == "-" {
		data, err := io.ReadAll(c.stdin)
		if err != nil {
			return "", err
		}
		text = string(data)
	}
	return attach(text, files)
}

func attach(text string, files []string) (string, error) {
	var b strings.Builder
	b.WriteString(text)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fence := "```"
		for strings.Contains(string(data), fence) {
			fence += "`"
		}
		fmt.Fprintf(&b, "\n\n%s\n%s %s\n%s\n%s\n", filepath.Base(f), fence, f, strings.TrimRight(string(data), "\n"), fence)
	}
	return b.String(), nil
}

func (c *cli) runCmd(args []string) (int, error) {
	fs := c.flags("run")
	var tf turnFlags
	tf.register(fs)
	var tasks multi
	fs.Var(&tasks, "task", "start a separate run for this task (repeatable)")
	cwd := fs.String("cwd", "", "working folder for the agent (default: current)")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("run needs a profile name")
	}
	profile, words := pos[0], pos[1:]
	if len(tasks) > 0 && len(words) > 0 {
		return harness.ExitUsage, usageErr("give either prompt words or --task, not both")
	}
	var prompts []string
	if len(tasks) > 0 {
		for _, t := range tasks {
			p, err := attach(t, tf.files)
			if err != nil {
				return harness.ExitUsage, err
			}
			prompts = append(prompts, p)
		}
	} else {
		p, err := c.prompt(words, tf.files)
		if err != nil {
			return harness.ExitUsage, err
		}
		prompts = []string{p}
	}
	for _, p := range prompts {
		if strings.TrimSpace(p) == "" {
			return harness.ExitUsage, usageErr("empty prompt")
		}
	}
	extra, err := tf.extra()
	if err != nil {
		return harness.ExitUsage, err
	}
	dir := *cwd
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return harness.ExitUsage, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return harness.ExitUsage, usageErr("--cwd %s is not a folder", dir)
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	if tf.dry {
		plan, err := r.DryRun(profile, prompts[0], dir, extra)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		c.printPlan(plan)
		return 0, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reports, err := r.Start(ctx, profile, prompts, dir, extra, func(run *harness.Run) {
		fmt.Fprintf(c.stderr, "harness: %s started (%s → %s, %s)\n", run.ID, run.Profile, run.Harness, run.Cwd)
	})
	c.drain(r)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	return c.report(reports, tf.asJSON, len(tasks) > 0), nil
}

func (c *cli) send(args []string) (int, error) {
	fs := c.flags("send")
	var tf turnFlags
	tf.register(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) < 2 {
		return harness.ExitUsage, usageErr("send needs <run-id> and a prompt")
	}
	prompt, err := c.prompt(pos[1:], tf.files)
	if err != nil {
		return harness.ExitUsage, err
	}
	extra, err := tf.extra()
	if err != nil {
		return harness.ExitUsage, err
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	if tf.dry {
		plan, err := r.DryRunSend(pos[0], prompt, extra)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		c.printPlan(plan)
		return 0, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := r.Send(ctx, pos[0], prompt, extra)
	c.drain(r)
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	return c.report([]harness.Report{rep}, tf.asJSON, false), nil
}

func (c *cli) drain(r *harness.Runner) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Shutdown(ctx)
}

type jsonReport struct {
	Run        string `json:"run"`
	Turn       int    `json:"turn"`
	Profile    string `json:"profile"`
	Harness    string `json:"harness"`
	Outcome    string `json:"outcome"`
	ExitCode   int    `json:"exit_code"`
	Attempts   int    `json:"attempts"`
	Session    string `json:"session,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
}

// report prints the answers and returns the most severe exit code.
func (c *cli) report(reps []harness.Report, asJSON, many bool) int {
	code := 0
	var js []jsonReport
	for _, rep := range reps {
		ec := harness.ExitCode(rep.Outcome)
		code = worse(code, ec)
		if asJSON {
			j := jsonReport{Turn: rep.Turn, Outcome: rep.Outcome, ExitCode: ec, Attempts: rep.Attempts,
				DurationMS: rep.Duration.Milliseconds(), Output: rep.Output}
			if rep.Run != nil {
				j.Run, j.Profile, j.Harness, j.Session = rep.Run.ID, rep.Run.Profile, rep.Run.Harness, rep.Run.Session
			}
			if rep.Err != nil {
				j.Error = rep.Err.Error()
			}
			js = append(js, j)
			continue
		}
		c.status(rep)
		if many {
			fmt.Fprintf(c.stdout, "=== %s · %s\n", rep.Run.ID, rep.Outcome)
		}
		if rep.Output != "" {
			fmt.Fprintln(c.stdout, rep.Output)
		}
	}
	if asJSON {
		var data []byte
		if len(js) == 1 && !many {
			data, _ = json.MarshalIndent(js[0], "", "  ")
		} else {
			data, _ = json.MarshalIndent(js, "", "  ")
		}
		fmt.Fprintln(c.stdout, string(data))
	}
	return code
}

func (c *cli) status(rep harness.Report) {
	id := "?"
	if rep.Run != nil {
		id = rep.Run.ID
	}
	fmt.Fprintf(c.stderr, "harness: %s %s · turn %d · %d attempt(s) · %s\n",
		id, rep.Outcome, rep.Turn, rep.Attempts, rep.Duration.Round(time.Second))
	if rep.Err == nil {
		return
	}
	fmt.Fprintf(c.stderr, "harness: %v\n", rep.Err)
	if tail := lastLines(rep.StderrTail, 10); tail != "" {
		fmt.Fprintf(c.stderr, "harness: last stderr lines:\n%s\n", tail)
	}
	if rep.Run != nil {
		fmt.Fprintf(c.stderr, "harness: details: harness show %s\n", rep.Run.ID)
	}
}

func (c *cli) printPlan(p *harness.Plan) {
	quoted := make([]string, len(p.Argv))
	for i, a := range p.Argv {
		quoted[i] = a
		if a == "" || strings.ContainsAny(a, " \t\"'") {
			quoted[i] = strconv.Quote(a)
		}
	}
	fmt.Fprintf(c.stdout, "command: %s\n", strings.Join(quoted, " "))
	if p.Stdin {
		fmt.Fprintln(c.stdout, "stdin:   the prompt")
	}
	for _, e := range p.Env {
		fmt.Fprintf(c.stdout, "env:     %s\n", e)
	}
	for _, d := range p.Dropped {
		fmt.Fprintf(c.stdout, "left out: %v ({%s} has no value)\n", d.Group, d.Missing)
	}
}

func (c *cli) runs(args []string) error {
	fs := c.flags("runs")
	limit := fs.Int("limit", 20, "how many runs to list, newest first")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	runs, err := c.home.Runs(*limit)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	for _, r := range runs {
		last := "-"
		if n := len(r.Turns); n > 0 {
			last = r.Turns[n-1].Outcome
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d turn(s)\t%s\t%s\n", r.ID, r.Profile, last, len(r.Turns), r.Created.Local().Format("2006-01-02 15:04"), r.Cwd)
	}
	return w.Flush()
}

func (c *cli) show(args []string) error {
	if len(args) != 1 {
		return usageErr("show needs <run-id>")
	}
	r, err := c.home.LoadRun(args[0])
	if err != nil {
		return errUsage{err}
	}
	fmt.Fprintf(c.stdout, "run:     %s\nprofile: %s (harness %s)\ncwd:     %s\nsession: %s\ndir:     %s\n",
		r.ID, r.Profile, r.Harness, r.Cwd, r.Session, r.Dir())
	for _, t := range r.Turns {
		dur := "-"
		if !t.Ended.IsZero() {
			dur = t.Ended.Sub(t.Started).Round(time.Second).String()
		}
		fmt.Fprintf(c.stdout, "turn %d:  %s · %d attempt(s) · exit %d · %s\n", t.N, t.Outcome, t.Attempts, t.ExitCode, dur)
		if t.Error != "" {
			fmt.Fprintf(c.stdout, "         %s\n", t.Error)
		}
	}
	if n := len(r.Turns); n > 0 {
		if out, err := r.Output(n); err == nil {
			fmt.Fprintf(c.stdout, "\n%s", out)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "== attempt") {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

func worse(a, b int) int {
	rank := map[int]int{
		harness.ExitSuccess: 0, harness.ExitTransient: 1, harness.ExitThrottled: 2,
		harness.ExitUncertain: 3, harness.ExitPermanent: 4, harness.ExitUsage: 5, harness.ExitCancelled: 6,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
