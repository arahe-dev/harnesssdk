//go:build unix

package harnesssdk

import (
	"os"
	"syscall"
)

// lockOpen takes an exclusive lock on path without waiting and returns the
// locked file; closing it releases the lock. The lock belongs to the open
// file, so a child that inherits the file keeps it held: the agent CLI
// inherits its turn's locks, and they stay held while any process of its
// tree is alive. The operating system releases it when the last holder dies.
func lockOpen(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errBusy
	}
	return f, nil
}
