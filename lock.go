package harnesssdk

import (
	"errors"
	"time"
)

// errBusy is returned by lockFile when another process holds the lock.
var errBusy = errors.New("locked by another process")

// lockFileWait retries lockFile until it succeeds or timeout passes. It is
// for short critical sections, such as rewriting a small config file.
func lockFileWait(path string, timeout time.Duration) (unlock func(), err error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, err := lockFile(path)
		if err == nil || !errors.Is(err, errBusy) || time.Now().After(deadline) {
			return unlock, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
