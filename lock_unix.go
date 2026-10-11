//go:build unix

package harnesssdk

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path without waiting. The operating
// system releases it if the process dies.
func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errBusy
	}
	return func() { f.Close() }, nil
}
