//go:build windows

package engine

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	procLockFileEx = kernel32.NewProc("LockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

// lockOpen takes an exclusive lock on path without waiting and returns the
// locked file; closing it releases the lock. The operating system releases
// it if the process dies. (A Windows lock belongs to the locking process, so
// it is not inherited; the job object ties the agent's life to its worker
// instead.)
func lockOpen(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r, _, _ := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		f.Close()
		return nil, errBusy
	}
	return f, nil
}
