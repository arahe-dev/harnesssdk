package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A bridge to pi-intercom, for a Pi agent that has that extension: while
// an attempt runs, the worker joins the intercom broker as a session named
// harness-<run>, finds the agent's own session, and carries messages both
// ways, so they arrive mid-turn rather than at the next one:
//
//   - a message posted to the run (harness steer, harness post run:<id>)
//     is sent to the agent; pi-intercom puts it in at its next safe point.
//     Once the broker confirms it, the run's inbox cursor moves past it, so
//     the next turn does not repeat it;
//   - what the agent sends harness-<run> becomes a note in the run's log,
//     or an ask if it expects a reply; harness reply answers it through
//     the broker.
//
// The protocol (pi-intercom 0.18, protocol version 1, read from its
// broker/framing.ts, protocol.ts and paths.ts): a Unix socket at
// $PI_CODING_AGENT_DIR/intercom/broker.sock (default ~/.pi/agent), each
// frame a 4-byte big-endian length and a UTF-8 JSON payload of at most
// 1 MiB. Without a broker the bridge notes so once, and messages wait for
// the next turn as they do without it. Windows (a named pipe) is not
// supported yet.

const (
	BridgePiIntercom = "pi-intercom"
	piMaxFrame       = 1 << 20
	// EvBridge says what the bridge did, or why it could not.
	EvBridge = "bridge"
)

// piSocketPath is the broker's socket.
func piSocketPath() (string, error) {
	if runtime.GOOS == "windows" {
		return "", errors.New("the pi-intercom bridge does not support Windows' named pipes yet")
	}
	dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".pi", "agent")
	}
	return filepath.Join(dir, "intercom", "broker.sock"), nil
}

func writeFrame(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > piMaxFrame {
		return fmt.Errorf("intercom frame of %d bytes is over the limit", len(data))
	}
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	_, err = w.Write(frame)
	return err
}

func readFrame(r io.Reader) (map[string]any, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > piMaxFrame {
		return nil, fmt.Errorf("intercom frame of %d bytes is over the limit", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var msg map[string]any
	return msg, json.Unmarshal(data, &msg)
}

// piSession is the part of the broker's SessionInfo the bridge uses.
type piSession struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Cwd       string  `json:"cwd"`
	PID       float64 `json:"pid"`
	StartedAt float64 `json:"startedAt"`
}

func sessionOf(v any) piSession {
	var s piSession
	data, _ := json.Marshal(v)
	json.Unmarshal(data, &s)
	return s
}

// piBridge carries one attempt's messages.
type piBridge struct {
	run      *Run
	turn     int
	session  string // the agent's harness session id, which Pi may also use
	dir      string
	started  time.Time
	agentPID chan int

	conn   net.Conn
	wmu    sync.Mutex
	self   string
	target piSession
	// asks maps the run's ask ids to the agent's intercom message ids;
	// sent maps the intercom ids of our messages to their end offset in
	// the log.
	mu    sync.Mutex
	asks  map[string]string
	sent  map[string]int64
	lists map[string]chan []piSession
}

// ids are our own session id and the agent's, once known.
func (b *piBridge) ids() (self, target string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.self, b.target.ID
}

func (b *piBridge) send(msg any) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return writeFrame(b.conn, msg)
}

// startPiBridge runs the bridge until ctx ends. agentPID receives the
// agent's process id once it starts.
func startPiBridge(ctx context.Context, t *Turn, agentPID chan int) (stop func()) {
	path, err := piSocketPath()
	if err != nil {
		t.Run.note(Event{Turn: t.N, Kind: EvBridge, Name: BridgePiIntercom, Text: err.Error() + "; messages wait for the next turn"})
		return func() {}
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		t.Run.note(Event{Turn: t.N, Kind: EvBridge, Name: BridgePiIntercom, Text: "no pi-intercom broker at " + path + "; messages wait for the next turn"})
		return func() {}
	}
	b := &piBridge{run: t.Run, turn: t.N, session: t.lastSession, dir: t.Run.AgentDir(), started: time.Now(), agentPID: agentPID,
		conn: conn, asks: map[string]string{}, sent: map[string]int64{}, lists: map[string]chan []piSession{}}
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.Close()
		if err := b.serve(bctx); err != nil && bctx.Err() == nil {
			t.Run.note(Event{Turn: t.N, Kind: EvBridge, Name: BridgePiIntercom, Text: "stopped: " + err.Error()})
		}
	}()
	return func() {
		b.send(map[string]any{"type": "unregister"})
		cancel()
		conn.Close()
		<-done
	}
}

func nowMS() float64 { return float64(time.Now().UnixMilli()) }

func (b *piBridge) serve(ctx context.Context) error {
	if err := b.send(map[string]any{"type": "register", "session": map[string]any{
		"name": "harness-" + b.run.ID, "cwd": b.dir, "model": "harness", "pid": os.Getpid(),
		"startedAt": nowMS(), "lastActivity": nowMS(), "status": "bridge",
	}}); err != nil {
		return err
	}
	frames := make(chan map[string]any)
	readErr := make(chan error, 1)
	go func() {
		for {
			msg, err := readFrame(b.conn)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	// The log is read from now: what was posted before the attempt is in
	// its prompt already, or in the inbox.
	cursor := logEnd(filepath.Join(b.run.dir, eventsFile))
	pid := 0
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	lastList := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case p := <-b.agentPID:
			pid = p
		case msg := <-frames:
			if err := b.handle(msg); err != nil {
				return err
			}
		case <-tick.C:
			self, target := b.ids()
			if self == "" {
				continue // not registered yet
			}
			if target == "" {
				if time.Since(lastList) > time.Second {
					lastList = time.Now()
					go b.find(ctx, pid)
				}
				continue
			}
			evs, ends, next, err := readEventsEnds(filepath.Join(b.run.dir, eventsFile), cursor)
			if err != nil {
				return err
			}
			cursor = next
			for i, e := range evs {
				b.deliver(e, ends[i])
			}
		}
	}
}

// find lists the broker's sessions and picks the agent's: its session id,
// its process, or one started in its folder since the attempt began.
func (b *piBridge) find(ctx context.Context, pid int) {
	id := "l-" + randHex(4)
	ch := make(chan []piSession, 1)
	b.mu.Lock()
	b.lists[id] = ch
	b.mu.Unlock()
	if b.send(map[string]any{"type": "list", "requestId": id}) != nil {
		return
	}
	var sessions []piSession
	select {
	case sessions = <-ch:
	case <-time.After(5 * time.Second):
		return
	case <-ctx.Done():
		return
	}
	self, _ := b.ids()
	var pick *piSession
	for i, s := range sessions {
		switch {
		case s.ID == self:
		case b.session != "" && s.ID == b.session, pid != 0 && int(s.PID) == pid:
			pick = &sessions[i]
		case pick == nil && s.Cwd == b.dir && s.StartedAt >= float64(b.started.UnixMilli())-1000:
			pick = &sessions[i]
		}
	}
	if pick == nil {
		return
	}
	b.mu.Lock()
	first := b.target.ID == ""
	b.target = *pick
	b.mu.Unlock()
	if first {
		b.run.note(Event{Turn: b.turn, Kind: EvBridge, Name: BridgePiIntercom, Text: "connected to the agent's intercom session " + or(pick.Name, pick.ID)})
	}
}

// deliver sends a run event the agent should get now: a message posted to
// it, or the answer to its intercom ask. end is the event's end offset.
func (b *piBridge) deliver(e Event, end int64) {
	b.mu.Lock()
	target := b.target.ID
	askID, isAnswer := b.asks[e.Name]
	b.mu.Unlock()
	msg := map[string]any{"id": "harness-" + randHex(8), "timestamp": nowMS()}
	switch {
	case e.Kind == EvMessage:
		msg["content"] = map[string]any{"text": "Message from " + or(e.From, "the agent that started you") + ": " + e.Text}
	case e.Kind == EvReply && isAnswer:
		msg["replyTo"] = askID
		msg["content"] = map[string]any{"text": e.Text}
	default:
		return
	}
	b.mu.Lock()
	if e.Kind == EvMessage {
		b.sent[msg["id"].(string)] = end
	}
	b.mu.Unlock()
	b.send(map[string]any{"type": "send", "to": target, "message": msg})
}

func (b *piBridge) handle(msg map[string]any) error {
	switch msg["type"] {
	case "registered":
		b.mu.Lock()
		b.self, _ = msg["sessionId"].(string)
		b.mu.Unlock()
	case "error":
		return fmt.Errorf("broker: %v", msg["error"])
	case "sessions":
		id, _ := msg["requestId"].(string)
		raw, _ := msg["sessions"].([]any)
		var ss []piSession
		for _, r := range raw {
			ss = append(ss, sessionOf(r))
		}
		b.mu.Lock()
		ch := b.lists[id]
		delete(b.lists, id)
		b.mu.Unlock()
		if ch != nil {
			ch <- ss
		}
	case "delivered":
		id, _ := msg["messageId"].(string)
		b.mu.Lock()
		end, ok := b.sent[id]
		delete(b.sent, id)
		b.mu.Unlock()
		if ok && end > readInboxCursor(b.run) {
			// Delivered live: the next turn need not repeat it.
			writeFileAtomic(inboxPath(b.run), []byte(strconv.FormatInt(end, 10)))
			b.run.note(Event{Turn: b.turn, Kind: EvDelivery, Name: BridgePiIntercom, Text: "delivered live"})
		}
	case "delivery_failed":
		id, _ := msg["messageId"].(string)
		b.mu.Lock()
		_, ours := b.sent[id]
		delete(b.sent, id)
		b.mu.Unlock()
		if ours {
			b.run.note(Event{Turn: b.turn, Kind: EvBridge, Name: BridgePiIntercom, Text: fmt.Sprintf("not delivered live (%v); it waits for the next turn", msg["reason"])})
		}
	case "message":
		from := sessionOf(msg["from"])
		var m struct {
			ID           string `json:"id"`
			ExpectsReply bool   `json:"expectsReply"`
			Content      struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		data, _ := json.Marshal(msg["message"])
		json.Unmarshal(data, &m)
		who := "pi:" + or(from.Name, from.ID)
		if _, target := b.ids(); from.ID == target {
			who = ""
		}
		if m.ExpectsReply {
			qid := "q-" + randHex(3)
			b.mu.Lock()
			b.asks[qid] = m.ID
			b.mu.Unlock()
			b.run.note(Event{Turn: b.turn, Kind: EvAsk, Name: qid, From: who, Text: m.Content.Text})
		} else {
			b.run.note(Event{Turn: b.turn, Kind: EvNote, From: who, Text: m.Content.Text})
		}
	}
	return nil
}
