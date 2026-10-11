//go:build unix

package harnesssdk

// transientShare is always false: Unix renames and reads do not collide.
func transientShare(error) bool { return false }
