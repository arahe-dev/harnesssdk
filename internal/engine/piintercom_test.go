//go:build !windows

package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBroker speaks the part of pi-intercom's broker protocol the bridge
// uses: register, list, send (routed to the target as a message, with a
// delivered receipt) and unregister.
type fakeBroker struct {
	ln       net.Listener
	mu       sync.Mutex
	sessions map[string]*brokerConn
}

type brokerConn struct {
	c    net.Conn
	info map[string]any
	wmu  sync.Mutex
}

func (bc *brokerConn) write(msg any) {
	bc.wmu.Lock()
	defer bc.wmu.Unlock()
	writeFrame(bc.c, msg)
}

func startFakeBroker(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.MkdirAll(filepath.Join(dir, "intercom"), 0o700)
	ln, err := net.Listen("unix", filepath.Join(dir, "intercom", "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{ln: ln, sessions: map[string]*brokerConn{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(&brokerConn{c: c})
		}
	}()
	return dir
}

func (b *fakeBroker) serve(bc *brokerConn) {
	defer bc.c.Close()
	id := ""
	for {
		msg, err := readFrame(bc.c)
		if err != nil {
			b.mu.Lock()
			delete(b.sessions, id)
			b.mu.Unlock()
			return
		}
		switch msg["type"] {
		case "register":
			id, _ = msg["sessionId"].(string)
			if id == "" {
				id = "s-" + randHex(4)
			}
			info, _ := msg["session"].(map[string]any)
			info["id"] = id
			bc.info = info
			b.mu.Lock()
			b.sessions[id] = bc
			b.mu.Unlock()
			bc.write(map[string]any{"type": "registered", "sessionId": id})
		case "list":
			b.mu.Lock()
			var ss []any
			for _, s := range b.sessions {
				ss = append(ss, s.info)
			}
			b.mu.Unlock()
			bc.write(map[string]any{"type": "sessions", "requestId": msg["requestId"], "sessions": ss})
		case "send":
			to, _ := msg["to"].(string)
			m, _ := msg["message"].(map[string]any)
			b.mu.Lock()
			var target *brokerConn
			for sid, s := range b.sessions {
				if sid == to || s.info["name"] == to {
					target = s
				}
			}
			b.mu.Unlock()
			if target == nil {
				bc.write(map[string]any{"type": "delivery_failed", "messageId": m["id"], "reason": "Session not found"})
				continue
			}
			target.write(map[string]any{"type": "message", "from": bc.info, "message": m})
			bc.write(map[string]any{"type": "delivered", "messageId": m["id"]})
		case "unregister":
			return
		}
	}
}

// fakePiChild registers with the broker as a Pi session would, asks
// harness-<run> a question and waits for the reply, then waits for one
// more message, and answers with both.
func fakePiChild(prompt []byte) int {
	path, err := piSocketPath()
	if err != nil {
		return 1
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	cwd, _ := os.Getwd()
	now := float64(time.Now().UnixMilli())
	writeFrame(c, map[string]any{"type": "register", "session": map[string]any{"name": "pi-child", "cwd": cwd, "model": "m", "pid": os.Getpid(), "startedAt": now, "lastActivity": now}})
	bridge := "harness-" + os.Getenv(EnvRun)
	var reply, steer string
	asked := false
	deadline := time.Now().Add(20 * time.Second)
	c.SetReadDeadline(deadline)
	for reply == "" || steer == "" {
		if !asked {
			// Wait for the bridge to be there, then ask it.
			writeFrame(c, map[string]any{"type": "list", "requestId": "r"})
		}
		msg, err := readFrame(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pichild:", err)
			return 1
		}
		switch msg["type"] {
		case "sessions":
			ss, _ := msg["sessions"].([]any)
			for _, s := range ss {
				if m, _ := s.(map[string]any); m["name"] == bridge && !asked {
					asked = true
					writeFrame(c, map[string]any{"type": "send", "to": bridge, "message": map[string]any{
						"id": "ask-1", "timestamp": now, "expectsReply": true, "content": map[string]any{"text": "which API version?"}}})
				}
			}
			if !asked {
				time.Sleep(100 * time.Millisecond)
			}
		case "message":
			m, _ := msg["message"].(map[string]any)
			content, _ := m["content"].(map[string]any)
			text, _ := content["text"].(string)
			if m["replyTo"] == "ask-1" {
				reply = text
			} else {
				steer = text
			}
		}
	}
	fmt.Printf("reply=%s; steer=%s\n", reply, steer)
	return 0
}

// A Pi agent with pi-intercom asks through the broker and is answered with
// harness reply; a message steered to it arrives mid-turn, and is not
// repeated at the next turn.
func TestPiIntercomBridge(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", startFakeBroker(t))
	f := newFixture(t, nil)
	f.profile("pi", "harness: fake\neffect: idempotent\nbridge: pi-intercom\nmode: pichild")
	runs, err := f.r.Prepare("pi", []string{"go"}, f.cwd, TurnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := runs[0].ID
	var rep Report
	var wg sync.WaitGroup
	wg.Go(func() { rep = f.r.Execute(context.Background(), id, 1) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, finished, alerts, err := f.r.WaitAttention(ctx, id)
	if err != nil || finished || len(alerts) != 1 || alerts[0].Text != "which API version?" {
		t.Fatalf("finished=%v alerts=%+v err=%v", finished, alerts, err)
	}
	if _, err := f.home.Reply(id, "", "v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.home.Steer(id, "also keep v1 working"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if rep.Err != nil || !strings.Contains(rep.Output, "reply=v2") || !strings.Contains(rep.Output, "steer=Message from the agent that started you: also keep v1 working") {
		t.Fatalf("report: %q %v", rep.Output, rep.Err)
	}
	evs, _, _ := f.home.Events(id, 0)
	if !hasEvent(evs, EvBridge, BridgePiIntercom) || !hasEvent(evs, EvDelivery, BridgePiIntercom) {
		t.Fatalf("events: %v", kinds(evs))
	}
	run, _ := f.home.LoadRun(id)
	if msgs, _, _ := unread(run); len(msgs) != 0 {
		t.Fatalf("a message delivered live is still unread: %+v", msgs)
	}
}

// Without a broker the bridge says so once, and the turn runs as usual.
func TestPiIntercomNoBroker(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	f := newFixture(t, nil)
	f.profile("pi", "harness: fake\neffect: pure\nbridge: pi-intercom\nmode: echo")
	rep := f.start(context.Background(), "pi", "hi")[0]
	if rep.Err != nil || rep.Output != "echo: hi" {
		t.Fatalf("%+v", rep)
	}
	evs, _, _ := f.home.Events(rep.Run.ID, 0)
	if !hasEvent(evs, EvBridge, BridgePiIntercom) {
		t.Fatalf("events: %v", kinds(evs))
	}
	if _, err := ParseProfile("x", []byte("---\nharness: h\neffect: pure\nbridge: slack\n---\n")); err == nil {
		t.Fatal("an unknown bridge was accepted")
	}
}
