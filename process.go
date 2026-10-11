package harnesssdk

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// procSpec is one harness CLI invocation. The standard streams are files,
// so no copying goroutine waits on a pipe that a leftover grandchild holds.
type procSpec struct {
	Argv   []string
	Dir    string
	Env    []string // added to the inherited environment
	Stdin  *os.File // nil: no input
	Stdout *os.File
	Stderr *os.File
	// Inherit are files kept open for as long as the CLI's process tree runs
	// (Unix: the guard holds them; Windows ties the tree to this process
	// with a job object instead).
	Inherit []*os.File
}

// runProcess runs the CLI to completion and returns its exit status. A
// nonzero status is data, not an error; err reports only a failure to
// start. When ctx ends, the whole process tree is killed, and leftovers are
// killed once the CLI itself exits. warnings say when that containment was
// weaker than usual.
func runProcess(ctx context.Context, s procSpec) (exitCode int, warnings []string, err error) {
	path, err := exec.LookPath(s.Argv[0])
	if err != nil {
		return -1, nil, err
	}
	cmd, t, err := newProcess(path, s.Argv[1:], s.Inherit)
	if err != nil {
		return -1, nil, err
	}
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	if s.Stdin != nil {
		cmd.Stdin = s.Stdin
	}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	if err := cmd.Start(); err != nil {
		t.close()
		return -1, nil, err
	}
	warning, err := t.started()
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.close()
		return -1, nil, err
	}
	if warning != "" {
		warnings = append(warnings, warning)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			t.kill()
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	t.close()
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		err = nil
	}
	return exitCode, warnings, err
}

// GuardCommand is the hidden first argument that makes the harness binary
// act as the guard of one CLI process tree (Unix).
const GuardCommand = "__guard"

// Subcommand runs a hidden harness subcommand, a detached worker or a
// process guard, if os.Args asks for one. A program that embeds Runner must
// call it first thing in main, because harness starts its own executable
// for both:
//
//	if code, ok := harnesssdk.Subcommand(); ok {
//		os.Exit(code)
//	}
func Subcommand() (code int, handled bool) {
	if len(os.Args) < 2 {
		return 0, false
	}
	switch os.Args[1] {
	case WorkerCommand:
		return Worker(os.Args[2:]), true
	case GuardCommand:
		return guard(os.Args[2:]), true
	}
	return 0, false
}
