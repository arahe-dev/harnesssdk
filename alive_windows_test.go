//go:build windows

package harnesssdk

import "syscall"

func processAlive(pid int) bool {
	const synchronize, queryLimited = 0x00100000, 0x1000
	h, err := syscall.OpenProcess(synchronize|queryLimited, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	r, _ := syscall.WaitForSingleObject(h, 0)
	return r == syscall.WAIT_TIMEOUT
}
