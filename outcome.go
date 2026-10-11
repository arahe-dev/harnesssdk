package harnesssdk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/arahe-dev/hertasdk"
)

// Exit codes for a run, so a parent agent can branch without parsing.
const (
	ExitSuccess   = 0
	ExitUsage     = 2 // bad command line, profile or definition
	ExitPermanent = 3
	ExitTransient = 4
	ExitThrottled = 5
	ExitUncertain = 6
	ExitRunning   = 7 // harness wait or result: the turn has not finished
	ExitCancelled = 130
)

// parseOutcome reads an outcome name used in failure rules and retry lists.
func parseOutcome(s string) (hertasdk.Outcome, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "transient":
		return hertasdk.Transient, nil
	case "permanent":
		return hertasdk.Permanent, nil
	case "throttled":
		return hertasdk.Throttled, nil
	case "uncertain":
		return hertasdk.Uncertain, nil
	}
	return 0, fmt.Errorf("unknown outcome %q (transient, permanent, throttled, uncertain)", s)
}

// OutcomeName names how a run ended: success, cancelled, stopped, or the
// Herta outcome of its error.
func OutcomeName(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, hertasdk.ErrRuntimeStopping):
		return "stopped"
	}
	return strings.ToLower(hertasdk.OutcomeOf(err).String())
}

// ExitCode maps an outcome name to the process exit code.
func ExitCode(outcome string) int {
	switch outcome {
	case "success":
		return ExitSuccess
	case "transient":
		return ExitTransient
	case "throttled":
		return ExitThrottled
	case "uncertain":
		return ExitUncertain
	case "cancelled", "stopped":
		return ExitCancelled
	}
	return ExitPermanent
}
