package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// codexStream is a 5 MB Codex event stream: one session event, 20,000
// command results and the answer.
func codexStream() []byte {
	var b bytes.Buffer
	b.WriteString(`{"type":"thread.started","thread_id":"th-1"}` + "\n")
	for i := range 20000 {
		fmt.Fprintf(&b, `{"type":"item.completed","item":{"id":"item_%d","type":"command_execution","command":"rg -n foo",`+
			`"aggregated_output":"src/a.go:12: foo bar baz qux quux corge grault garply waldo fred plugh xyzzy thud lorem ipsum dolor sit amet consectetur","exit_code":0}}`+"\n", i)
	}
	b.WriteString(`{"type":"item.completed","item":{"id":"item_z","type":"agent_message","text":"final answer"}}` + "\n")
	b.WriteString(`{"type":"turn.completed","usage":{"input_tokens":1}}` + "\n")
	return b.Bytes()
}

func BenchmarkExtractJSONL(b *testing.B) {
	d := &Definition{Output: OutputRule{Format: "jsonl",
		Answer:  &Selector{Where: map[string]string{"type": "item.completed", "item.type": "agent_message"}, Field: "item.text"},
		Session: &Selector{Where: map[string]string{"type": "thread.started"}, Field: "thread_id"},
		Error:   &Selector{Where: map[string]string{"type": "turn.failed"}},
	}}
	out := codexStream()
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	for b.Loop() {
		ans, _, sess, err := d.extract(bytes.NewReader(out))
		if err != nil || ans != "final answer" || sess != "th-1" {
			b.Fatal(ans, sess, err)
		}
	}
}

func BenchmarkAncestors(b *testing.B) {
	for b.Loop() {
		ancestors(os.Getpid(), 64)
	}
}

// A waiter's pass over a ledger that two live tickets hold.
func BenchmarkGatePass(b *testing.B) {
	g := newGateHome(b).Gate(nil)
	c := []Claim{{Name: "pi", Units: 1, Capacity: 2}}
	for range 2 {
		tk, err := g.Acquire(b.Context(), c, false, "x")
		if err != nil {
			b.Fatal(err)
		}
		defer tk.Release()
	}
	for b.Loop() {
		g.update(c, func([]*gateState) {})
	}
}

// What a waiter does between passes.
func BenchmarkFileStamp(b *testing.B) {
	path := filepath.Join(b.TempDir(), "state.json")
	os.WriteFile(path, []byte("{}"), 0o600)
	for b.Loop() {
		fileStamp(path)
	}
}
