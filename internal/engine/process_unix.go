//go:build unix

package engine

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// On Unix the CLI runs under a guard: this binary again (GuardCommand),
// leading a new process group, with the CLI as its child in the same group.
// The guard holds the turn's inherited locks and watches a pipe whose write
// end only the harness process holds. If the harness process dies (even by
// SIGKILL), the pipe reaches EOF and the guard kills the whole group, so a
// crashed worker never leaves its agent's tree running while its budget is
// handed to someone else.

type tree struct {
	cmd   *exec.Cmd
	watch *os.File // write end of the guard's pipe; closing it ends the tree
	read  *os.File // read end, closed here once the guard has it
}

// newProcess wraps the CLI in the guard. extra are files the guard holds
// for the tree's lifetime.
func setCmdLine(*exec.Cmd, string) {}

// shellArgv runs a command line with /bin/sh.
func shellArgv(line string) (argv []string, cmdLine string) {
	return []string{"/bin/sh", "-c", line}, ""
}

func newProcess(path string, args []string, extra []*os.File) (*exec.Cmd, *tree, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(self, append([]string{GuardCommand, "--", path}, args...)...)
	cmd.ExtraFiles = append([]*os.File{r}, extra...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, &tree{cmd: cmd, watch: w, read: r}, nil
}

func (t *tree) started() (warning string, err error) {
	t.read.Close() // the guard has the read end now
	return "", nil
}

func (t *tree) kill() {
	if t.cmd.Process != nil {
		syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// close ends anything the CLI left running in its group.
func (t *tree) close() {
	t.read.Close()
	t.watch.Close()
	t.kill()
}

// guard is the body of the guard process: args are "--", the CLI's path
// and its arguments; fd 3 is the watch pipe. It returns the CLI's exit
// status (128+signal if a signal ended it).
func guard(args []string) int {
	if len(args) < 2 || args[0] != "--" {
		return 127
	}
	watch := os.NewFile(3, "watch")
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Stderr.WriteString("harness guard: " + err.Error() + "\n")
		return 127
	}
	go func() {
		io.Copy(io.Discard, watch) // returns when the harness process is gone
		syscall.Kill(0, syscall.SIGKILL)
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 127
	}
	return 0
}
