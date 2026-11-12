package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Every run keeps an event log, runs/<id>/events.jsonl: one JSON event per
// line, appended, never rewritten. It is how a caller follows a run while
// it works, instead of reading the files its agent edits.
//
// Lifecycle events say where a turn is: queued, started (its process took
// it and it is waiting for admission), attempt, retry, check, stop
// (requested), finished and lost. Activity events say what the agent is
// doing: tool, file, say and step, read from the CLI's own event stream by
// the definition's output.progress rules while the attempt runs.
//
// Each event is one write to a file opened for appending, so concurrent
// writers (the process executing the turn, and one asking it to stop)
// never interleave within a line. A reader's cursor is a byte offset: it
// reads complete lines from there, and the offset after the last one is
// its next cursor. An event that cannot be written is lost, never fatal:
// the run record, not the log, is what a turn's outcome rests on.

const (
	eventFormat  = 1
	eventsFile   = "events.jsonl"
	maxEventText = 300 // runes; a longer text is cut, with "…"
)

// Event kinds.
const (
	EvQueued   = "queued"   // a turn was queued; text: its prompt's first line
	EvStarted  = "started"  // a process took the turn; it waits for admission
	EvAttempt  = "attempt"  // an attempt began; text: its timeout
	EvRetry    = "retry"    // an attempt failed and will be retried; name: outcome
	EvCheck    = "check"    // the agent answered; its check runs; text: the command
	EvStop     = "stop"     // a stop was asked for
	EvFinished = "finished" // the turn ended; name: outcome; text: error or answer
	EvLost     = "lost"     // the turn's process died; name: outcome

	EvTool = "tool" // the agent used a tool; name: the tool; text: detail
	EvFile = "file" // the agent changed a file; text: its path
	EvSay  = "say"  // the agent said something; text: what
	EvStep = "step" // the agent finished a step (a model call); text: detail
)

// Event is one entry in a run's event log.
type Event struct {
	Format  int       `json:"format"`
	Time    time.Time `json:"time"`
	Turn    int       `json:"turn"`
	Attempt int       `json:"attempt,omitempty"`
	Kind    string    `json:"kind"`
	// From is who posted a message (a run, run:<id>, or a party's alias);
	// To, in a room or a party's mailbox, who it is for.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

// lifecycle reports whether the event moves a turn from one phase to the
// next; the others (activity, messages, the clock) happen within a phase.
func (e Event) lifecycle() bool {
	switch e.Kind {
	case EvQueued, EvStarted, EvAttempt, EvRetry, EvCheck, EvFinished, EvLost:
		return true
	}
	return false
}

// note appends an event to the run's log. It never fails the caller.
func (r *Run) note(ev Event) {
	noteAt(r.dir, ev)
}

func noteAt(dir string, ev Event) { appendEvent(filepath.Join(dir, eventsFile), ev) }

// appendEvent appends an event to the log at path.
func appendEvent(path string, ev Event) {
	ev.Format = eventFormat
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	ev.Text = clip(ev.Text, maxEventText)
	ev.Name = clip(ev.Name, 80)
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write(append(line, '\n')) // one write: appends never interleave
	f.Close()
}

// clip shortens s to n runes on one line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// maxEventRead bounds how much of the log one read returns; the cursor
// says where to go on.
const maxEventRead = 1 << 20

// readEvents reads the complete events after byte offset since, and the
// offset to read from next. A line that is not an event, or is from a newer
// harness, is skipped. A cursor past the end (the run was replaced) reads
// from the start.
func readEvents(dir string, since int64) ([]Event, int64, error) {
	return readEventsAt(filepath.Join(dir, eventsFile), since)
}

// readEventsAt is readEvents for the log at path.
func readEventsAt(path string, since int64) ([]Event, int64, error) {
	evs, _, next, err := readEventsEnds(path, since)
	return evs, next, err
}

// readEventsEnds is readEventsAt that also returns, for each event, the
// offset just after its line.
func readEventsEnds(path string, since int64) ([]Event, []int64, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, since, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, since, err
	}
	if since < 0 || since > fi.Size() {
		since = 0
	}
	data := make([]byte, min(fi.Size()-since, maxEventRead))
	n, err := f.ReadAt(data, since)
	if err != nil && err != io.EOF {
		return nil, nil, since, err
	}
	data = data[:n]
	end := bytes.LastIndexByte(data, '\n') + 1 // complete lines only
	var evs []Event
	var ends []int64
	off := since
	for line := range bytes.SplitSeq(data[:end], []byte{'\n'}) {
		off += int64(len(line)) + 1
		var ev Event
		if len(line) == 0 || json.Unmarshal(line, &ev) != nil || ev.Format > eventFormat || ev.Kind == "" {
			continue
		}
		evs, ends = append(evs, ev), append(ends, off)
	}
	return evs, ends, since + int64(end), nil
}

// tailEvents reads the events of the last part of the log, at most back
// bytes, and the log's end as a cursor.
func tailEvents(dir string, back int64) ([]Event, int64, error) {
	fi, err := os.Stat(filepath.Join(dir, eventsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	from := max(0, fi.Size()-back)
	evs, next, err := readEvents(dir, from)
	if from > 0 && len(evs) > 0 {
		evs = evs[1:] // the first line may have been cut
	}
	return evs, next, err
}

// Events reads a run's events after cursor since, and the next cursor.
func (h *Home) Events(id string, since int64) ([]Event, int64, error) {
	run, err := h.LoadRun(id)
	if err != nil {
		return nil, since, err
	}
	return readEvents(run.dir, since)
}

// WaitEvents is Events, waiting until there is at least one new event, the
// run's last turn has finished, or ctx ends. done says the last turn has
// finished; a turn whose process died is recorded as lost first.
func (r *Runner) WaitEvents(ctx context.Context, id string, since int64) (evs []Event, next int64, done bool, err error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		run, err := r.home.LoadRun(id)
		if err != nil {
			return nil, since, false, err
		}
		if evs, next, err = readEvents(run.dir, since); err != nil || len(evs) > 0 {
			return evs, next, lastFinished(run), err
		}
		if lastFinished(run) {
			return nil, next, true, nil
		}
		if !run.executing() {
			// A turn whose process died: WaitTurn records it as lost, and
			// that adds an event.
			wctx, cancel := context.WithCancel(ctx)
			cancel()
			if _, _, err := r.WaitTurn(wctx, id, len(run.Turns)); err != nil {
				return nil, since, false, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, next, false, nil
		case <-tick.C:
		}
	}
}

// needsAttention reports whether an event should wake a caller waiting for
// the turn to end: something it can still act on.
func needsAttention(e Event) bool { return e.Kind == EvTimeoutSoon || e.Kind == EvAsk }

// WaitAttention is WaitTurn for the run's last turn that also returns,
// unfinished, as soon as an event that needs the caller's attention is
// logged (such as timeout_soon), with those events.
func (r *Runner) WaitAttention(ctx context.Context, id string) (rep Report, finished bool, alerts []Event, err error) {
	run, err := r.home.LoadRun(id)
	if err != nil {
		return Report{}, false, nil, err
	}
	n := len(run.Turns)
	_, cursor, err := tailEvents(run.dir, 0)
	if err != nil {
		return Report{}, false, nil, err
	}
	now, cancel := context.WithCancel(ctx)
	cancel()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if rep, finished, err = r.WaitTurn(now, id, n); err != nil || finished {
			return rep, finished, nil, err
		}
		var evs []Event
		if evs, cursor, err = readEvents(run.dir, cursor); err != nil {
			return rep, false, nil, err
		}
		for _, e := range evs {
			if e.Turn == n && needsAttention(e) {
				alerts = append(alerts, e)
			}
		}
		if len(alerts) > 0 {
			return rep, false, alerts, nil
		}
		select {
		case <-ctx.Done():
			return rep, false, nil, nil
		case <-tick.C:
		}
	}
}

func lastFinished(run *Run) bool {
	n := len(run.Turns)
	return n == 0 || run.Turns[n-1].finished()
}

// ProgressRule picks what an agent is doing out of the CLI's JSONL events
// as they are written: each event matching Where becomes an Event of the
// rule's Kind. Field is the tool's name (tool), the file's path (file), or
// the text (say, step); Detail is a tool's or step's detail, the first of
// its "|"-separated paths that has a value.
type ProgressRule struct {
	Kind   string            `json:"kind"`
	Where  map[string]string `json:"where"`
	Field  string            `json:"field,omitempty"`
	Detail string            `json:"detail,omitempty"`
}

func (p *ProgressRule) validate() error {
	switch p.Kind {
	case EvTool, EvFile, EvSay, EvStep:
	default:
		return fmt.Errorf("kind must be tool, file, say or step, got %q", p.Kind)
	}
	if len(p.Where) == 0 {
		return errors.New("where is required: a rule matching every event would report every line")
	}
	if p.Field == "" && p.Kind != EvStep {
		return errors.New("field is required, except for kind step")
	}
	return nil
}

// event is what the rule makes of a matching CLI event, if anything.
func (p *ProgressRule) event(ev any) (Event, bool) {
	field := ""
	if p.Field != "" {
		v, ok := lookup(ev, p.Field)
		if !ok || scalar(v) == "" {
			return Event{}, false
		}
		field = scalar(v)
	}
	detail := ""
	for path := range strings.SplitSeq(p.Detail, "|") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		if v, ok := lookup(ev, path); ok && scalar(v) != "" {
			detail = scalar(v)
			break
		}
	}
	switch p.Kind {
	case EvTool:
		return Event{Kind: EvTool, Name: field, Text: detail}, true
	case EvStep:
		return Event{Kind: EvStep, Text: strings.TrimSpace(field + " " + detail)}, true
	}
	return Event{Kind: p.Kind, Text: field}, true
}

// progressTail follows an attempt's stdout while the CLI writes it, and
// notes an event for each line a progress rule matches.
type progressTail struct {
	run       *Run
	path      string
	turn, att int
	rules     []ProgressRule
	sels      []*collector // each rule's where, for matching raw lines cheaply
	off       int64
	rest      []byte // a line not finished yet
	stop      chan struct{}
	done      sync.WaitGroup
}

// followProgress starts following path for the definition's progress
// rules; nil when it has none.
func followProgress(d *Definition, run *Run, path string, turn, attempt int) *progressTail {
	if d.Output.Format != "jsonl" || len(d.Output.Progress) == 0 {
		return nil
	}
	t := &progressTail{run: run, path: path, turn: turn, att: attempt, rules: d.Output.Progress, stop: make(chan struct{})}
	for _, rule := range t.rules {
		t.sels = append(t.sels, newCollector(&Selector{Where: rule.Where}))
	}
	t.done.Add(1)
	go func() {
		defer t.done.Done()
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				t.poll(true)
				return
			case <-tick.C:
				t.poll(false)
			}
		}
	}()
	return t
}

// end reads what is left once the CLI has exited, and stops.
func (t *progressTail) end() {
	if t == nil {
		return
	}
	close(t.stop)
	t.done.Wait()
}

// poll reads what was written since the last poll; final also takes a last
// line with no newline.
func (t *progressTail) poll(final bool) {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return
	}
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		chunk, err := br.ReadSlice('\n')
		t.off += int64(len(chunk))
		if len(t.rest)+len(chunk) > 4<<20 { // a runaway line: skip it
			t.rest, chunk = nil, nil
		}
		t.rest = append(t.rest, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil || (final && err == io.EOF && len(t.rest) > 0) {
			t.line(t.rest)
			t.rest = t.rest[:0]
		}
		if err != nil {
			return
		}
	}
}

func (t *progressTail) line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev any
	decoded := false
	for i, rule := range t.rules {
		if !t.sels[i].mayMatch(line) {
			continue
		}
		if !decoded {
			if json.Unmarshal(line, &ev) != nil {
				return
			}
			decoded = true
		}
		if !(&Selector{Where: rule.Where}).matches(ev) {
			continue
		}
		if e, ok := rule.event(ev); ok {
			e.Turn, e.Attempt = t.turn, t.att
			t.run.note(e)
		}
	}
}
