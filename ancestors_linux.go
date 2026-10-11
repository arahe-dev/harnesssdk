//go:build linux

package harnesssdk

import (
	"os"
	"strconv"
	"strings"
)

// ancestors returns the process ids above pid, nearest first, at most max,
// read from /proc.
func ancestors(pid, max int) []int {
	var out []int
	for cur := pid; len(out) < max; {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(cur) + "/stat")
		if err != nil {
			break
		}
		// The command name is in parentheses and may contain spaces; the
		// fields after it are "state ppid ...".
		s := string(data)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 2 {
			break
		}
		p, err := strconv.Atoi(f[1])
		if err != nil || p <= 1 {
			break
		}
		out = append(out, p)
		cur = p
	}
	return out
}

// processStart is a process's start time in clock ticks since boot (field
// 22 of /proc/<pid>/stat), or 0 if it is gone.
func processStart(pid int) int64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return 0
	}
	t, _ := strconv.ParseInt(f[19], 10, 64)
	return t
}
