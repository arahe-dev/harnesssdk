package engine

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Detach starts a worker process that executes turn n of a run and returns
// at once. The worker outlives the command that started it, so a parent
// agent whose shell tool times out can still collect the answer later with
// harness wait. Its log is the run's worker.log.
func (r *Runner) Detach(id string, n int) error {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// A worker started through WMI does not inherit this environment (API
	// keys exported in the parent's shell, PATH additions): hand it over in
	// a private file that the worker reads and deletes, never on a command
	// line.
	envPath := run.Path(fmt.Sprintf("turn-%d.env", n))
	env, _ := json.Marshal(os.Environ())
	if err := os.WriteFile(envPath, env, 0o600); err != nil {
		return err
	}
	args := []string{WorkerCommand,
		"--home", r.home.Dir, "--run", id, "--turn", strconv.Itoa(n),
		"--parent", r.parent, "--lineage", strings.Join(r.lineage, ","),
	}
	if err := detach(exe, args, run.Cwd); err != nil {
		os.Remove(envPath) // it holds the caller's secrets: never leave it behind
		return fmt.Errorf("start the worker for run %s: %w", id, err)
	}
	return nil
}

// WorkerCommand is the hidden first argument that makes the harness binary
// (or a test binary) act as a detached worker; see Worker.
const WorkerCommand = "__worker"

// Worker is the body of a detached worker process: it executes one queued
// turn and returns the exit code for its outcome. args follow WorkerCommand.
// The worker does not rely on its environment, which may not be its
// starter's: everything it needs is in args and the run directory.
func Worker(args []string) int {
	fs := flag.NewFlagSet(WorkerCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "")
	id := fs.String("run", "", "")
	n := fs.Int("turn", 0, "")
	parent := fs.String("parent", "", "")
	lineage := fs.String("lineage", "", "")
	if err := fs.Parse(args); err != nil || *home == "" || *id == "" || *n < 1 {
		fmt.Fprintln(os.Stderr, "harness worker: bad arguments")
		return ExitUsage
	}
	h, err := NewHome(*home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness worker:", err)
		return ExitUsage
	}
	logw := io.Writer(os.Stderr)
	if run, err := h.LoadRun(*id); err == nil {
		if f, err := os.OpenFile(run.Path("worker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			defer f.Close()
			logw = f
		}
		envPath := run.Path(fmt.Sprintf("turn-%d.env", *n))
		if data, err := os.ReadFile(envPath); err == nil {
			if err := os.Remove(envPath); err != nil {
				fmt.Fprintf(logw, "harness worker: could not delete %s, which holds the caller's environment: %v\n", envPath, err)
			}
			var env []string
			if json.Unmarshal(data, &env) == nil {
				os.Clearenv()
				for _, kv := range env {
					if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
						os.Setenv(k, v)
					}
				}
			}
		}
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(logw, time.Now().Format(time.RFC3339)+" pid %d: "+format+"\n", append([]any{os.Getpid()}, a...)...)
	}
	r, err := NewRunnerFor(h, *parent, splitList(*lineage))
	if err != nil {
		logf("%v", err)
		return ExitUsage
	}
	logf("executing turn %d", *n)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := r.Execute(ctx, *id, *n)
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Shutdown(sctx)
	if rep.Err != nil {
		logf("turn %d: %s: %v", *n, rep.Outcome, rep.Err)
	} else {
		logf("turn %d: %s", *n, rep.Outcome)
	}
	return ExitCode(rep.Outcome)
}
