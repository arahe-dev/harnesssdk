//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

var procNtQueryInformationProcess = syscall.NewLazyDLL("ntdll.dll").NewProc("NtQueryInformationProcess")

// processBasicInformation is PROCESS_BASIC_INFORMATION; each field is
// pointer-sized or padded to it.
type processBasicInformation struct {
	ExitStatus      uintptr
	PebBaseAddress  uintptr
	AffinityMask    uintptr
	BasePriority    uintptr
	UniqueProcessID uintptr
	ParentProcessID uintptr
}

// ancestors returns the process ids above pid, nearest first, at most max.
// It asks each process for its parent, rather than snapshotting every
// process on the machine. Windows keeps a process's parent id after the
// parent exits and may reuse it, so a recorded parent born after its child
// is not followed.
func ancestors(pid, max int) []int {
	var out []int
	cur := uint32(pid)
	parent, curBirth, ok := processInfo(cur)
	for ok && len(out) < max && parent != 0 && parent != cur {
		next, b, alive := processInfo(parent)
		if !alive || (curBirth != 0 && b > curBirth) {
			break // gone, or a reused id
		}
		out = append(out, int(parent))
		cur, curBirth, parent = parent, b, next
	}
	return out
}

// processInfo returns a process's parent id and creation time.
func processInfo(pid uint32) (parent uint32, birth int64, ok bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0, 0, false
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, 0, false
	}
	var pbi processBasicInformation
	var n uint32
	if st, _, _ := procNtQueryInformationProcess.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi), uintptr(unsafe.Pointer(&n))); st != 0 {
		return 0, created.Nanoseconds(), true
	}
	return uint32(pbi.ParentProcessID), created.Nanoseconds(), true
}

// processStart is a process's creation time, or 0 if it is gone.
func processStart(pid int) int64 {
	_, b, _ := processInfo(uint32(pid))
	return b
}
