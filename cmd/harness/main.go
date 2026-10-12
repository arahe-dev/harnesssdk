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

	"github.com/arahe-dev/hertasdk"

	sdk "github.com/arahe-dev/harnesssdk/v2"
	harness "github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

const usage = `harness runs agents in other harnesses (Pi, Claude Code, Codex, ...) as subagents.

  harness guide                            the manual: read this first
  harness run <profile> [prompt | -]       run an agent and wait; --task --file --cwd --set --kind --json --dry-run
  harness start <profile> [prompt | -]     start an agent in the background (same flags); prints run ids
  harness compare <p1>,<p2>[,...] [prompt] run one task on several profiles side by side; --file --cwd --set --kind --json
  harness stats [--profile p] [--kind k]   how each profile's turns ended and what they cost; --days --json
  harness send <run-id> [prompt | -]       follow up in the same session; --file --set --json --dry-run --detach
  harness wait <run-id> [--timeout 5m]     wait for a run's turn to finish, then print it (exit 7: still running)
  harness result <run-id>                  print a run's last turn without waiting
  harness stop <run-id>                    stop a run's turn
  harness runs | show <run-id> | status    past runs; budgets and keys in use
  harness profile set|show|list|rm         agents (Markdown with a header); --project for this project
  harness def set|show|list|rm             harness definitions (JSON); --project for this project
  harness trust [--approve]                review, then trust, this project's .harness files
  harness budget set|list|rm               capacity limits shared by every harness process
  harness config [set <key> <n>]           limits: nesting, running turns, retention, repeated failures
  harness gc [--days n] [--dry-run]        remove finished runs older than retention_days
  harness home | skill | version
`

// errUsage marks a usage or configuration error (exit 2).
type errUsage struct{ error }

func usageErr(format string, a ...any) error { return errUsage{fmt.Errorf(format, a...)} }

func main() {
	if code, ok := harness.Subcommand(); ok {
		os.Exit(code)
	}
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
			code = exitFor(err)
		}
	}
	return code
}

// exitFor is the exit code of a command that failed before any turn ran:
// the outcome's code when the error carries one (a busy run is throttled),
// otherwise a usage error.
func exitFor(err error) int {
	if _, ok := errors.AsType[*hertasdk.Failure](err); ok {
		return harness.ExitCode(harness.OutcomeName(err))
	}
	return harness.ExitUsage
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
		fmt.Fprint(c.stdout, sdk.Guide)
		return 0, nil
	case "skill":
		fmt.Fprint(c.stdout, sdk.Skill)
		return 0, nil
	case "version":
		fmt.Fprintln(c.stdout, "harness", sdk.Version)
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
	case "config":
		return 0, c.config(args)
	case "gc":
		return 0, c.gc(args)
	case "stats":
		return 0, c.stats(args)
	case "compare":
		return c.compare(args)
	case "trust":
		return 0, c.trust(args)
	case "run":
		return c.runCmd(args, false)
	case "start":
		return c.runCmd(args, true)
	case "send":
		return c.send(args)
	case "wait":
		return c.wait(args, false)
	case "result":
		return c.wait(args, true)
	case "stop":
		return c.stop(args)
	case "runs":
		return 0, c.runs(args)
	case "show":
		return 0, c.show(args)
	case "status":
		return 0, c.status()
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

// folder resolves --cwd: an existing folder, absolute; default the current.
func folder(dir string) (string, error) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", usageErr("--cwd %s is not a folder", abs)
	}
	return abs, nil
}

// scopeFlags are the --project and --cwd flags of def and profile.
type scopeFlags struct {
	project bool
	cwd     string
}

func (s *scopeFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&s.project, "project", false, "write to the project's .harness folder instead of the global one")
	fs.StringVar(&s.cwd, "cwd", "", "folder whose project to use (default: current)")
}

// projectDir is the folder to write to: a project's .harness, or "" for
// global.
func (c *cli) projectDir(s scopeFlags) (string, string, error) {
	cwd, err := folder(s.cwd)
	if err != nil {
		return "", "", err
	}
	if !s.project {
		return "", cwd, nil
	}
	dir, err := c.home.ProjectDir(cwd)
	return dir, cwd, err
}

func (c *cli) def(args []string) error {
	if len(args) == 0 {
		return usageErr("def needs set, show, list or rm")
	}
	fs := c.flags("def " + args[0])
	file := fs.String("file", "", "definition JSON file, or - for stdin")
	var sc scopeFlags
	sc.register(fs)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	proj, cwd, err := c.projectDir(sc)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		items, err := c.home.List("harnesses", cwd)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		for _, it := range items {
			d, err := c.home.Definition(it.Name, cwd)
			if err != nil {
				fmt.Fprintf(w, "%s\t%s\t(%v)\n", it.Name, it.Scope, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.Name, it.Scope, d.Command, d.Description)
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
		if _, err := c.home.SaveDefinition(name, data, proj); err != nil {
			return errUsage{err}
		}
		fmt.Fprintf(c.stdout, "saved harness %s (%s)\n", name, scopeOf(proj))
		return nil
	case "show":
		d, err := c.home.Definition(name, cwd)
		if err != nil {
			return errUsage{err}
		}
		data, _ := json.MarshalIndent(d, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		fmt.Fprintf(c.stderr, "harness: %s definition %s\n", d.Scope, d.Path)
		return nil
	}
	return c.home.Remove("harnesses", name, proj)
}

func scopeOf(projectDir string) string {
	if projectDir == "" {
		return "global"
	}
	return "project " + projectDir
}

func (c *cli) profile(args []string) error {
	if len(args) == 0 {
		return usageErr("profile needs set, show, list or rm")
	}
	fs := c.flags("profile " + args[0])
	file := fs.String("file", "", "profile Markdown file, or - for stdin")
	var sc scopeFlags
	sc.register(fs)
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	proj, cwd, err := c.projectDir(sc)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		items, err := c.home.List("profiles", cwd)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		for _, it := range items {
			p, err := c.home.Profile(it.Name, cwd)
			if err != nil {
				fmt.Fprintf(w, "%s\t%s\t(%v)\n", it.Name, it.Scope, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", it.Name, it.Scope, p.Harness, p.Vars["model"], p.Description)
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
		p, err := c.home.Profile(name, cwd)
		if err != nil {
			return errUsage{err}
		}
		c.stdout.Write(p.Bytes())
		fmt.Fprintf(c.stderr, "harness: %s profile %s\n", p.Scope, p.Path)
		return nil
	case "rm":
		return c.home.Remove("profiles", name, proj)
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
		cur, err := c.home.Profile(name, cwd)
		if err != nil {
			return errUsage{err}
		}
		if cur.Scope == "project" && !sc.project {
			// Edit the profile where it was found.
			proj = filepath.Dir(filepath.Dir(cur.Path))
		}
		if p, err = cur.Set(pairs); err != nil {
			return errUsage{err}
		}
	default:
		return usageErr("profile set needs --file <path|-> or key=value pairs")
	}
	if err := c.home.SaveProfile(p, proj, cwd); err != nil {
		return errUsage{err}
	}
	fmt.Fprintf(c.stdout, "saved profile %s (harness %s, %s)\n", name, p.Harness, scopeOf(proj))
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return err
	}
	if plan, err := r.DryRun(name, "<prompt>", cwd, nil); err == nil {
		c.printPlan(plan)
	}
	return nil
}

func (c *cli) trust(args []string) error {
	fs := c.flags("trust")
	approve := fs.Bool("approve", false, "trust the current content of every project file")
	cwdFlag := fs.String("cwd", "", "folder whose project to trust (default: current)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cwd, err := folder(*cwdFlag)
	if err != nil {
		return err
	}
	proj, ok := c.home.FindProject(cwd)
	if !ok {
		return usageErr("no project .harness folder at or above %s", cwd)
	}
	var entries []harness.TrustEntry
	if *approve {
		entries, err = c.home.ApproveProject(proj)
	} else {
		entries, err = c.home.TrustStatus(proj)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "project %s\n", proj)
	pending := 0
	for _, e := range entries {
		state := e.State
		if *approve {
			state = "trusted"
		}
		if state != "trusted" {
			pending++
		}
		fmt.Fprintf(c.stdout, "  %-8s %s\n", state, e.Path)
	}
	if pending > 0 {
		fmt.Fprintf(c.stdout, "%d file(s) are not trusted. Read them (a definition is a command line; a profile picks tools and flags), then run: harness trust --approve\n", pending)
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

func (c *cli) config(args []string) error {
	switch {
	case len(args) == 0 || len(args) == 1 && args[0] == "list":
		cfg, err := c.home.Config()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "max_depth\t%d\thow deep runs may nest (0: no nesting)\n", cfg.MaxDepth)
		fmt.Fprintf(w, "max_running\t%d\tturns running at once on this machine (0: no cap)\n", cfg.MaxRunning)
		fmt.Fprintf(w, "retention_days\t%d\tdays finished runs are kept (0: forever)\n", cfg.RetentionDays)
		fmt.Fprintf(w, "repeat_warn\t%d\twarn after the same request fails this many times in a row (0: never)\n", cfg.RepeatWarn)
		fmt.Fprintf(w, "repeat_refuse\t%d\trefuse it after this many (0: never)\n", cfg.RepeatRefuse)
		return w.Flush()
	case args[0] == "set" && len(args) == 3:
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return usageErr("%s must be a whole number", args[1])
		}
		if err := c.home.SetConfig(args[1], n); err != nil {
			return errUsage{err}
		}
		return nil
	}
	return usageErr("config takes no arguments, list, or set <key> <n>")
}

func (c *cli) gc(args []string) error {
	fs := c.flags("gc")
	days := fs.Int("days", -1, "remove runs last active this many days ago or more (default: retention_days)")
	dry := fs.Bool("dry-run", false, "list what would be removed")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("gc takes no arguments")
	}
	if *days < 0 {
		cfg, err := c.home.Config()
		if err != nil {
			return err
		}
		if cfg.RetentionDays == 0 {
			return usageErr("retention_days is 0 (keep runs forever); pass --days n")
		}
		*days = cfg.RetentionDays
	}
	rep, err := c.home.GC(time.Now().AddDate(0, 0, -*days), *dry)
	verb := "removed"
	if *dry {
		verb = "would remove"
	}
	for _, id := range rep.Runs {
		fmt.Fprintln(c.stdout, id)
	}
	fmt.Fprintf(c.stderr, "harness: %s %d run(s), %s, last active %d+ days ago\n", verb, len(rep.Runs), size(rep.Bytes), *days)
	return err
}

func size(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", b)
}

// turnFlags are shared by run, start and send.
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

// runCmd is run (wait for the answers) and start (detach and return).
func (c *cli) runCmd(args []string, detach bool) (int, error) {
	name := map[bool]string{false: "run", true: "start"}[detach]
	fs := c.flags(name)
	var tf turnFlags
	tf.register(fs)
	var tasks multi
	fs.Var(&tasks, "task", "start a separate run for this task (repeatable)")
	cwdFlag := fs.String("cwd", "", "working folder for the agent (default: current)")
	kind := fs.String("kind", "", "label the run for harness stats, such as review")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("%s needs a profile name", name)
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
	dir, err := folder(*cwdFlag)
	if err != nil {
		return harness.ExitUsage, err
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
	if detach {
		runs, err := r.Prepare(profile, prompts, dir, extra, *kind)
		if err != nil {
			return exitFor(err), err
		}
		for _, run := range runs {
			if err := r.Detach(run.ID, 1); err != nil {
				return harness.ExitUsage, err
			}
			fmt.Fprintf(c.stderr, "harness: %s started in the background (%s → %s, %s)\n", run.ID, run.Profile, run.Harness, run.Cwd)
		}
		c.printStarted(runs, 1, tf.asJSON)
		return 0, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reports, err := r.Start(ctx, profile, prompts, dir, extra, *kind, func(run *harness.Run) {
		fmt.Fprintf(c.stderr, "harness: %s started (%s → %s, %s)\n", run.ID, run.Profile, run.Harness, run.Cwd)
	})
	c.drain(r)
	if err != nil {
		return exitFor(err), err
	}
	return c.report(reports, tf.asJSON, len(tasks) > 0), nil
}

func (c *cli) printStarted(runs []*harness.Run, turn int, asJSON bool) {
	if !asJSON {
		for _, run := range runs {
			fmt.Fprintln(c.stdout, run.ID)
		}
		return
	}
	type started struct {
		Run     string `json:"run"`
		Turn    int    `json:"turn"`
		Profile string `json:"profile"`
		Harness string `json:"harness"`
		Outcome string `json:"outcome"`
	}
	var out []started
	for _, run := range runs {
		out = append(out, started{run.ID, turn, run.Profile, run.Harness, "queued"})
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(c.stdout, string(data))
}

func (c *cli) send(args []string) (int, error) {
	fs := c.flags("send")
	var tf turnFlags
	tf.register(fs)
	detach := fs.Bool("detach", false, "run the turn in the background and return its run id")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) < 2 {
		return harness.ExitUsage, usageErr("send needs <run-id> and a prompt")
	}
	id := pos[0]
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
		plan, err := r.DryRunSend(id, prompt, extra)
		if err != nil {
			return harness.ExitUsage, errUsage{err}
		}
		c.printPlan(plan)
		return 0, nil
	}
	n, err := r.PrepareSend(id, prompt, extra)
	if err != nil {
		return exitFor(err), err
	}
	if *detach {
		if err := r.Detach(id, n); err != nil {
			return harness.ExitUsage, err
		}
		run, err := c.home.LoadRun(id)
		if err != nil {
			return harness.ExitUsage, err
		}
		fmt.Fprintf(c.stderr, "harness: %s turn %d started in the background\n", id, n)
		c.printStarted([]*harness.Run{run}, n, tf.asJSON)
		return 0, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := r.Execute(ctx, id, n)
	c.drain(r)
	return c.report([]harness.Report{rep}, tf.asJSON, false), nil
}

// wait is wait (block until the turn ends or --timeout) and result (now).
func (c *cli) wait(args []string, now bool) (int, error) {
	name := map[bool]string{false: "wait", true: "result"}[now]
	fs := c.flags(name)
	asJSON := fs.Bool("json", false, "print a JSON result")
	var timeout time.Duration
	if !now {
		fs.DurationVar(&timeout, "timeout", 0, "give up after this long, leaving the run going (0: wait until it ends)")
	}
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if len(pos) != 1 {
		return harness.ExitUsage, usageErr("%s needs <run-id>", name)
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	switch {
	case now:
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	case timeout > 0:
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, finished, err := r.Wait(ctx, pos[0])
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	if !finished {
		rep.Outcome = rep.Run.Turns[rep.Turn-1].Outcome
		if *asJSON {
			c.report([]harness.Report{rep}, true, false)
		} else {
			fmt.Fprintf(c.stderr, "harness: %s turn %d is still %s; wait again with: harness wait %s\n", rep.Run.ID, rep.Turn, rep.Outcome, rep.Run.ID)
		}
		return harness.ExitRunning, nil
	}
	return c.report([]harness.Report{rep}, *asJSON, false), nil
}

func (c *cli) stop(args []string) (int, error) {
	if len(args) != 1 {
		return harness.ExitUsage, usageErr("stop needs <run-id>")
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	n, err := r.Stop(args[0])
	if err != nil {
		return harness.ExitUsage, errUsage{err}
	}
	fmt.Fprintf(c.stderr, "harness: asked %s turn %d to stop\n", args[0], n)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, finished, err := r.WaitTurn(ctx, args[0], n)
	if err != nil {
		return harness.ExitUsage, err
	}
	if !finished {
		fmt.Fprintf(c.stderr, "harness: %s has not stopped yet; check with: harness wait %s\n", args[0], args[0])
		return harness.ExitRunning, nil
	}
	return c.report([]harness.Report{rep}, false, false), nil
}

func (c *cli) drain(r *harness.Runner) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Shutdown(ctx)
}

type jsonReport struct {
	Run        string             `json:"run"`
	Turn       int                `json:"turn"`
	Profile    string             `json:"profile"`
	Harness    string             `json:"harness"`
	Model      string             `json:"model,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Outcome    string             `json:"outcome"`
	ExitCode   int                `json:"exit_code"`
	Attempts   int                `json:"attempts"`
	Session    string             `json:"session,omitempty"`
	DurationMS int64              `json:"duration_ms"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Output     string             `json:"output"`
	Error      string             `json:"error,omitempty"`
	Hint       string             `json:"hint,omitempty"`
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
				DurationMS: rep.Duration.Milliseconds(), Output: rep.Output, Model: rep.Model, Metrics: rep.Metrics, Hint: rep.Hint}
			if rep.Run != nil {
				j.Run, j.Profile, j.Harness, j.Session, j.Kind = rep.Run.ID, rep.Run.Profile, rep.Run.Harness, rep.Run.Session, rep.Run.Kind
			}
			if rep.Err != nil {
				j.Error = rep.Err.Error()
			}
			js = append(js, j)
			continue
		}
		c.status1(rep)
		if many {
			fmt.Fprintf(c.stdout, "=== %s · %s · %s\n", runID(rep), runProfile(rep), rep.Outcome)
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

func (c *cli) status1(rep harness.Report) {
	id := "?"
	if rep.Run != nil {
		id = rep.Run.ID
	}
	fmt.Fprintf(c.stderr, "harness: %s %s · turn %d · %d attempt(s) · %s%s\n",
		id, rep.Outcome, rep.Turn, rep.Attempts, rep.Duration.Round(time.Second), spend(rep.Metrics))
	if rep.Hint != "" {
		fmt.Fprintf(c.stderr, "harness: hint: %s\n", rep.Hint)
	}
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
	if p.Profile != nil && p.Def != nil {
		fmt.Fprintf(c.stdout, "profile: %s (%s)\nharness: %s (%s)\n", p.Profile.Path, p.Profile.Scope, p.Def.Path, p.Def.Scope)
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
	if r.Parent != "" {
		fmt.Fprintf(c.stdout, "parent:  %s (depth %d)\n", r.Parent, r.Depth)
	}
	if r.Kind != "" {
		fmt.Fprintf(c.stdout, "kind:    %s\n", r.Kind)
	}
	for _, t := range r.Turns {
		dur := "-"
		if !t.Ended.IsZero() && !t.Started.IsZero() {
			dur = t.Ended.Sub(t.Started).Round(time.Second).String()
		}
		model := ""
		if t.Model != "" {
			model = " · " + t.Model
		}
		fmt.Fprintf(c.stdout, "turn %d:  %s · %d attempt(s) · exit %d · %s%s%s\n", t.N, t.Outcome, t.Attempts, t.ExitCode, dur, model, spend(t.Metrics))
		if t.Error != "" {
			fmt.Fprintf(c.stdout, "         %s\n", t.Error)
		}
		if t.Hint != "" {
			fmt.Fprintf(c.stdout, "         hint: %s\n", t.Hint)
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

func (c *cli) status() error {
	if p := c.home.DiscoverParentage(); p.Parent != "" {
		fmt.Fprintf(c.stdout, "this process runs inside run %s (found through %s; lineage %s)\n\n", p.Parent, p.Found, dash(p.Lineage))
	}
	gates, err := c.home.GateStatus()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "BUDGET\tIN USE\tHELD BY\tWAITING")
	for _, g := range gates {
		fmt.Fprintf(w, "%s\t%d/%d\t%s\t%s\n", g.Name, g.Used, g.Capacity, dash(g.Held), dash(g.Queued))
	}
	w.Flush()
	runs, err := c.home.Runs(200)
	if err != nil {
		return err
	}
	var active []*harness.Run
	for _, r := range runs {
		if n := len(r.Turns); n > 0 && (r.Turns[n-1].Outcome == "queued" || r.Turns[n-1].Outcome == "running") {
			active = append(active, r)
		}
	}
	fmt.Fprintf(c.stdout, "\n%d active run(s)\n", len(active))
	for _, r := range active {
		t := r.Turns[len(r.Turns)-1]
		fmt.Fprintf(c.stdout, "  %s  %s  turn %d %s  %s\n", r.ID, r.Profile, t.N, t.Outcome, r.Cwd)
	}
	return nil
}

// compare runs one task on several profiles and sets their outcomes and
// metrics side by side.
func (c *cli) compare(args []string) (int, error) {
	fs := c.flags("compare")
	var tf turnFlags
	tf.register(fs)
	cwdFlag := fs.String("cwd", "", "working folder for the agents (default: current)")
	kind := fs.String("kind", "", "label the runs for harness stats, such as review")
	pos, err := parse(fs, args)
	if err != nil {
		return harness.ExitUsage, err
	}
	if tf.dry {
		return harness.ExitUsage, usageErr("compare has no --dry-run; check each profile with: harness run <profile> --dry-run")
	}
	if len(pos) == 0 {
		return harness.ExitUsage, usageErr("compare needs profiles, separated by commas: harness compare a,b \"task\"")
	}
	var profiles []string
	for p := range strings.SplitSeq(pos[0], ",") {
		if p = strings.TrimSpace(p); p != "" {
			profiles = append(profiles, p)
		}
	}
	prompt, err := c.prompt(pos[1:], tf.files)
	if err != nil {
		return harness.ExitUsage, err
	}
	if strings.TrimSpace(prompt) == "" {
		return harness.ExitUsage, usageErr("empty prompt")
	}
	extra, err := tf.extra()
	if err != nil {
		return harness.ExitUsage, err
	}
	dir, err := folder(*cwdFlag)
	if err != nil {
		return harness.ExitUsage, err
	}
	r, err := harness.NewRunner(c.home)
	if err != nil {
		return harness.ExitUsage, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reps, err := r.Compare(ctx, profiles, prompt, dir, extra, *kind, func(run *harness.Run) {
		fmt.Fprintf(c.stderr, "harness: %s started (%s → %s, %s)\n", run.ID, run.Profile, run.Harness, run.Cwd)
	})
	c.drain(r)
	if err != nil {
		return exitFor(err), err
	}
	code := c.report(reps, tf.asJSON, true)
	if !tf.asJSON {
		fmt.Fprintln(c.stdout)
		w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PROFILE\tMODEL\tOUTCOME\tTIME\tTRIES\tIN\tOUT\tCOST")
		for _, rep := range reps {
			m := rep.Metrics
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", runProfile(rep), or(rep.Model, "-"), rep.Outcome,
				rep.Duration.Round(time.Second), rep.Attempts, tokens(m, "input_tokens"), tokens(m, "output_tokens"), money(m, "cost_usd"))
		}
		w.Flush()
	}
	return code, nil
}

// stats prints how each profile's turns ended and what they cost.
func (c *cli) stats(args []string) error {
	fs := c.flags("stats")
	profile := fs.String("profile", "", "only this profile")
	kind := fs.String("kind", "", "only runs with this label")
	days := fs.Int("days", 30, "runs started in the last n days")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("stats takes no arguments")
	}
	st, err := c.home.Stats(harness.StatsFilter{Profile: *profile, Kind: *kind, Since: time.Now().AddDate(0, 0, -*days)})
	if err != nil {
		return err
	}
	if *asJSON {
		if st == nil {
			st = []harness.Stats{}
		}
		data, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		return nil
	}
	if len(st) == 0 {
		fmt.Fprintf(c.stderr, "harness: no finished turns in the last %d days\n", *days)
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tMODEL\tKIND\tTURNS\tOK\tP50\tP90\tTRIES\tIN\tOUT\tCOST\tCOST/OK\tFAILED")
	for _, s := range st {
		ok := s.Outcomes["success"]
		mean := map[string]float64{}
		for k, m := range s.Metrics {
			mean[k] = m.Mean()
		}
		var failed []string
		for _, o := range slices.Sorted(maps.Keys(s.Outcomes)) {
			if o != "success" {
				failed = append(failed, fmt.Sprintf("%s %d", o, s.Outcomes[o]))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d%%\t%s\t%s\t%.1f\t%s\t%s\t%s\t%s\t%s\n",
			s.Profile, or(s.Model, "-"), or(s.Kind, "-"), s.Turns, 100*ok/s.Turns,
			s.Median.Round(time.Second), s.P90.Round(time.Second), float64(s.Attempts)/float64(s.Turns),
			tokens(mean, "input_tokens"), tokens(mean, "output_tokens"), money(mean, "cost_usd"), costPerSuccess(s), dash(failed))
	}
	fmt.Fprintln(w, "\nIN, OUT and COST are means per turn, over the turns whose harness reports them;")
	fmt.Fprintln(w, "COST/OK is shown only when every turn reported its cost.")
	return w.Flush()
}

// costPerSuccess is what the group spent per successful turn. A turn that
// did not report its cost would count as free, so a group with any such
// turn has no figure.
func costPerSuccess(s harness.Stats) string {
	m, has := s.Metrics["cost_usd"]
	ok := s.Outcomes["success"]
	if !has || ok == 0 || m.Turns < s.Turns {
		return "-"
	}
	return fmt.Sprintf("$%.4f", m.Sum/float64(ok))
}

// spend renders a turn's tokens and cost, when its harness reports them.
func spend(m map[string]float64) string {
	var parts []string
	if _, ok := m["input_tokens"]; ok {
		parts = append(parts, tokens(m, "input_tokens")+" in")
	}
	if _, ok := m["output_tokens"]; ok {
		parts = append(parts, tokens(m, "output_tokens")+" out")
	}
	if _, ok := m["cost_usd"]; ok {
		parts = append(parts, money(m, "cost_usd"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

func tokens(m map[string]float64, key string) string {
	v, ok := m[key]
	switch {
	case !ok:
		return "-"
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func money(m map[string]float64, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprintf("$%.4f", v)
	}
	return "-"
}

// runID and runProfile name a report's run, which a turn that failed
// before its record was read does not have.
func runID(rep harness.Report) string {
	if rep.Run == nil {
		return "?"
	}
	return rep.Run.ID
}

func runProfile(rep harness.Report) string {
	if rep.Run == nil {
		return "?"
	}
	return rep.Run.Profile
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func dash(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "== ") {
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
