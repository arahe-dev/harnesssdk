//go:build unix

package harnesssdk

import "syscall"

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
