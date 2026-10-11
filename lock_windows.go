//go:build windows

package harnesssdk

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

// lockFile takes an exclusive lock on path without waiting. The operating
// system releases it if the process dies.
func lockFile(path string) (unlock func(), err error) {
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
	return func() { f.Close() }, nil
}
