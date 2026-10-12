package engine

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// A turn's exit status says the agent finished, not that its work is
// right. A check, such as "go test ./...", says that: it runs in the
// agent's working folder once the agent has answered, and the turn
// succeeds only if it exits 0. A failed check is a permanent failure that
// keeps the agent's answer and session, so the caller can send the
// failure back to the same agent.

const defaultCheckTimeout = 10 * time.Minute

// runCheck runs the command line as is, through the platform shell: it is
// written by the profile's author or the caller, and no placeholder or
// prompt text is put into it.
func (r *Runner) runCheck(ctx context.Context, t *Turn, line string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	t.check = line
	logPath := t.Run.Path(fmt.Sprintf("turn-%d.check.log", t.N))
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return hertasdk.Fail(hertasdk.Permanent, err)
	}
	defer out.Close()
	fmt.Fprintf(out, "== check: %s\n", line)
	mark, _ := out.Seek(0, 1)
	argv, cmdLine := shellArgv(line)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, _, err := runProcess(cctx, procSpec{Argv: argv, CmdLine: cmdLine, Dir: t.Run.AgentDir(), Stdout: out, Stderr: out})
	switch {
	case err != nil:
		t.checked = "failed"
		return hertasdk.Fail(hertasdk.Permanent, fmt.Errorf("check %q could not start: %w", line, err))
	case ctx.Err() != nil:
		return ctx.Err()
	case cctx.Err() != nil:
		t.checked = "failed"
		return hertasdk.Fail(hertasdk.Permanent, fmt.Errorf("check %q did not finish within %s", line, timeout))
	case code != 0:
		t.checked = "failed"
		return hertasdk.Fail(hertasdk.Permanent, fmt.Errorf("check %q failed with status %d%s; its output: %s",
			line, code, lastLine(readTail(logPath, mark, 4<<10)), logPath))
	}
	t.checked = "passed"
	return nil
}
