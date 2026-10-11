//go:build unix

package engine

import (
	"os/exec"
	"syscall"
)

// detach starts exe in a new session, with no terminal and its standard
// streams on /dev/null, so neither the end of the starting command nor a
// signal to its process group reaches it.
func detach(exe string, args []string, dir string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
