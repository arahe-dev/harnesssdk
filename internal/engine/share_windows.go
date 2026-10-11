//go:build windows

package engine

import (
	"errors"
	"syscall"
)

const errorSharingViolation = syscall.Errno(32)

// transientShare reports a sharing violation, or the access denial a
// rename meets while the target is open or being deleted.
func transientShare(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
