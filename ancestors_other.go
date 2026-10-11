//go:build unix && !linux

package harnesssdk

import (
	"os/exec"
	"strconv"
	"strings"
)

// ancestors returns the process ids above pid, nearest first, at most max,
// asking ps one level at a time.
func ancestors(pid, max int) []int {
	var out []int
	for cur := pid; len(out) < max; {
		b, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(cur)).Output()
		if err != nil {
			break
		}
		p, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || p <= 1 {
			break
		}
		out = append(out, p)
		cur = p
	}
	return out
}

// processStart is unknown here: 0 skips the creation-time check.
func processStart(int) int64 { return 0 }
