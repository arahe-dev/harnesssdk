package engine

import (
	"errors"
	"os"
	"time"
)

// errBusy is returned when another process holds the lock.
var errBusy = errors.New("locked by another process")

// lockFile takes an exclusive lock on path without waiting.
func lockFile(path string) (unlock func(), err error) {
	f, err := lockOpen(path)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}

// lockWaitOpen retries lockOpen until it succeeds or timeout passes.
func lockWaitOpen(path string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := lockOpen(path)
		if err == nil || !errors.Is(err, errBusy) || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockFileWait retries lockFile until it succeeds or timeout passes. It is
// for short critical sections, such as rewriting a small file.
func lockFileWait(path string, timeout time.Duration) (unlock func(), err error) {
	f, err := lockWaitOpen(path, timeout)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}
