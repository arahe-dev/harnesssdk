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
	cmd := exec.Command(path, s.Argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	if s.Stdin != nil {
		cmd.Stdin = s.Stdin
	}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr

	t := newTree(cmd)
	if err := cmd.Start(); err != nil {
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
