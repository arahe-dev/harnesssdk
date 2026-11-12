package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// A run and the agent that started it talk through the run's event log,
// which is also its mailbox: durable, ordered, and read with the cursors
// every reader already keeps.
//
// The run's agent (inside the run, found as harness finds a nested run's
// parent) posts with note (how it is doing) and ask (a question it cannot
// go on without), which blocks its shell until a reply, or its wait ends.
// The caller answers with reply, and posts with steer; the agent reads what
// was posted to it with inbox, and whatever it has not read when its turn
// ends is put at the top of its next turn's prompt. steer --now stops the
// turn and continues the session with the message at once.
//
// While an ask waits, the asking process holds runs/<id>/asking.lock, and
// the attempt's idle clock counts that as activity: an agent waiting for an
// answer is not stuck.

// Event kinds of the intercom.
const (
	EvNote     = "note"     // the agent's progress; text
	EvAsk      = "ask"      // the agent asks; name: its id; text: the question
	EvReply    = "reply"    // an answer; name: the ask's id; text
	EvExpired  = "expired"  // an ask whose wait ended unanswered; name: its id
	EvMessage  = "message"  // posted to the agent; name: its id; text
	EvDelivery = "delivery" // messages put into a turn's prompt; text: their ids
)

const defaultAskWait = 10 * time.Minute

// intercomNote tells an agent how to reach whoever started it.
func intercomNote(run *Run) string {
	return fmt.Sprintf(`You are running as harness run %s, started by another agent, which may follow your progress. Your shell has the harness command:
- harness note "<one line>": tell it how you are doing at a milestone.
- harness ask "<question>": ask it, and wait (up to 10 minutes) for its answer; only when you cannot go on without a decision.
- harness inbox: messages it sent you; check it between steps of a long task.
- harness extend --self 30m --reason "<why>": before something that will take longer than your time left, or a long silence.`, run.ID)
}

// systemFor is the system prompt of a turn: the profile's, and the
// intercom note unless the profile turns it off.
func systemFor(p *Profile, run *Run) string {
	if !p.intercom() {
		return p.System
	}
	return strings.TrimSpace(p.System + "\n\n" + intercomNote(run))
}

func newMessageID(prefix string) string { return prefix + "-" + randHex(3) }

// inRun is the run the calling process is inside, as for a nested run.
func (h *Home) inRun() (*Run, error) {
	p := h.DiscoverParentage()
	if p.Parent == "" {
		return nil, errors.New("this process is not running inside a harness run (only an agent that harness started can do this)")
	}
	return h.LoadRun(p.Parent)
}

// currentTurn is the run's last turn, which must be unfinished.
func currentTurn(run *Run) (int, error) {
	n := len(run.Turns)
	if n == 0 || run.Turns[n-1].finished() {
		return 0, fmt.Errorf("run %s has no turn running", run.ID)
	}
	return n, nil
}

// Note posts a line of progress from the run the caller is inside.
func (h *Home) Note(text string) (*Run, error) {
	run, err := h.inRun()
	if err != nil {
		return nil, err
	}
	n, err := currentTurn(run)
	if err != nil {
		return nil, err
	}
	run.note(Event{Turn: n, Kind: EvNote, Text: text})
	return run, nil
}

// Ask posts a question from the run the caller is inside and waits for its
// reply until ctx ends; answered is false then, and the ask is marked
// expired.
func (r *Runner) Ask(ctx context.Context, question string) (run *Run, id, answer string, answered bool, err error) {
	if run, err = r.home.inRun(); err != nil {
		return nil, "", "", false, err
	}
	n, err := currentTurn(run)
	if err != nil {
		return run, "", "", false, err
	}
	asking, err := lockOpen(run.Path("asking.lock"))
	if err == nil {
		defer asking.Close() // another ask may hold it: either keeps the clock awake
	}
	_, cursor, err := tailEvents(run.dir, 0)
	if err != nil {
		return run, "", "", false, err
	}
	id = newMessageID("q")
	run.note(Event{Turn: n, Kind: EvAsk, Name: id, Text: question})
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		var evs []Event
		if evs, cursor, err = readEvents(run.dir, cursor); err != nil {
			return run, id, "", false, err
		}
		for _, e := range evs {
			if e.Kind == EvReply && e.Name == id {
				return run, id, e.Text, true, nil
			}
		}
		select {
		case <-ctx.Done():
			run.note(Event{Turn: n, Kind: EvExpired, Name: id, Text: "no answer in time"})
			return run, id, "", false, nil
		case <-tick.C:
		}
	}
}

// openAsks are the run's asks with no reply and not expired, oldest first.
func openAsks(evs []Event) []Event {
	var open []Event
	for _, e := range evs {
		switch e.Kind {
		case EvAsk:
			open = append(open, e)
		case EvReply, EvExpired:
			for i, a := range open {
				if a.Name == e.Name {
					open = append(open[:i], open[i+1:]...)
					break
				}
			}
		}
	}
	return open
}

// Reply answers a run's ask: the one with id, or its only open one.
func (h *Home) Reply(runID, id, answer string) (string, error) {
	run, err := h.LoadRun(runID)
	if err != nil {
		return "", err
	}
	evs, _, err := readEvents(run.dir, 0)
	if err != nil {
		return "", err
	}
	open := openAsks(evs)
	if id == "" {
		switch len(open) {
		case 0:
			return "", fmt.Errorf("run %s has no open question", runID)
		case 1:
			id = open[0].Name
		default:
			var ids []string
			for _, a := range open {
				ids = append(ids, a.Name)
			}
			return "", fmt.Errorf("run %s has %d open questions (%s); say which: harness reply %s <id> \"...\"", runID, len(open), strings.Join(ids, ", "), runID)
		}
	}
	for _, a := range open {
		if a.Name == id {
			run.note(Event{Turn: a.Turn, Kind: EvReply, Name: id, Text: answer})
			return id, nil
		}
	}
	return "", fmt.Errorf("run %s has no open question %s (it was answered, or its wait ended)", runID, id)
}

// Steer posts a message to a run's agent. It returns the message's id.
func (h *Home) Steer(runID, text string) (string, error) {
	run, err := h.LoadRun(runID)
	if err != nil {
		return "", err
	}
	id := newMessageID("m")
	run.note(Event{Turn: len(run.Turns), Kind: EvMessage, Name: id, Text: text})
	return id, nil
}

// SteerNow stops a run's turn, if one is running, and continues its
// session with the message (and any others it has not read) as a new
// turn, which a detached worker runs. It returns the new turn.
func (r *Runner) SteerNow(ctx context.Context, runID, text string) (int, error) {
	if _, err := r.home.Steer(runID, text); err != nil {
		return 0, err
	}
	run, err := r.home.LoadRun(runID)
	if err != nil {
		return 0, err
	}
	if n := len(run.Turns); n > 0 && !run.Turns[n-1].finished() {
		if _, err := r.Stop(runID); err != nil {
			return 0, err
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, done, err := r.WaitTurn(wctx, runID, n); err != nil {
			return 0, err
		} else if !done {
			return 0, fmt.Errorf("run %s turn %d has not stopped yet; the message waits for its next turn", runID, n)
		}
	}
	n, err := r.PrepareSend(runID, "Read the messages above, then carry on.", TurnOptions{})
	if err != nil {
		return 0, err
	}
	return n, r.Detach(runID, n)
}

// inboxPath keeps the offset in the run's log up to which its agent has
// read the messages posted to it.
func inboxPath(run *Run) string { return run.Path("inbox.cursor") }

func readInboxCursor(run *Run) int64 {
	data, err := os.ReadFile(inboxPath(run))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}

// unread are the messages posted to the run's agent after its inbox
// cursor, and the log's end.
func unread(run *Run) ([]Event, int64, error) {
	evs, next, err := readEvents(run.dir, readInboxCursor(run))
	var msgs []Event
	for _, e := range evs {
		if e.Kind == EvMessage {
			msgs = append(msgs, e)
		}
	}
	return msgs, next, err
}

// Inbox returns the messages posted to the run the caller is inside that
// it has not read, and marks them read unless peek.
func (h *Home) Inbox(peek bool) (*Run, []Event, error) {
	run, err := h.inRun()
	if err != nil {
		return nil, nil, err
	}
	msgs, next, err := unread(run)
	if err == nil && !peek {
		err = writeFileAtomic(inboxPath(run), []byte(strconv.FormatInt(next, 10)))
	}
	return run, msgs, err
}

// withUnread puts the messages the run's agent has not read at the top of
// a turn's prompt, marks them read, and logs their delivery. The caller
// holds the run's state lock.
func withUnread(run *Run, n int, prompt string) string {
	msgs, next, err := unread(run)
	if err != nil || len(msgs) == 0 {
		return prompt
	}
	if writeFileAtomic(inboxPath(run), []byte(strconv.FormatInt(next, 10))) != nil {
		return prompt
	}
	var b strings.Builder
	var ids []string
	b.WriteString("Messages from the agent that started you, sent while you worked:\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "- %s\n", m.Text)
		ids = append(ids, m.Name)
	}
	run.note(Event{Turn: n, Kind: EvDelivery, Text: strings.Join(ids, ", ")})
	return b.String() + "\n" + prompt
}

// asking reports whether an agent of the run is waiting for an answer.
func asking(run *Run) bool { return lockHeld(run.Path("asking.lock")) }

// lockHeld reports whether a process holds a lock file.
func lockHeld(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	unlock, err := lockFile(path)
	if err != nil {
		return errors.Is(err, errBusy)
	}
	unlock()
	return false
}
