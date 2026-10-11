//go:build windows

package harnesssdk

import (
	"syscall"
	"unsafe"
)

// ancestors returns the process ids above pid, nearest first, at most max.
// Windows keeps a process's parent id after the parent exits and may reuse
// it, so a recorded parent born after its child is not followed.
func ancestors(pid, max int) []int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	parent := map[uint32]uint32{}
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		parent[pe.ProcessID] = pe.ParentProcessID
	}
	syscall.CloseHandle(snap)
	var out []int
	cur := uint32(pid)
	curBirth := birthOf(cur)
	for len(out) < max {
		p, ok := parent[cur]
		if !ok || p == 0 || p == cur {
			break
		}
		b := birthOf(p)
		if b == 0 || (curBirth != 0 && b > curBirth) {
			break // gone, or a reused id
		}
		out = append(out, int(p))
		cur, curBirth = p, b
	}
	return out
}

func birthOf(pid uint32) int64 {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return created.Nanoseconds()
}

// processStart is a process's creation time, or 0 if it is gone.
func processStart(pid int) int64 { return birthOf(uint32(pid)) }
