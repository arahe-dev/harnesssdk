//go:build unix

package harnesssdk

import (
	"os/exec"
	"syscall"
)

// On Unix the process tree is a process group led by the CLI.

type tree struct{ cmd *exec.Cmd }

func newTree(cmd *exec.Cmd) *tree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &tree{cmd: cmd}
}

func (t *tree) started() (warning string, err error) { return "", nil }

func (t *tree) kill() { syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL) }

// close ends anything the CLI left running in its group.
func (t *tree) close() { syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL) }
