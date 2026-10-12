package harnesssdk

import (
	"encoding/json"
	"testing"
	"time"
)

// Code written against v1.5.0 must keep compiling. These are the uses of
// v1.5 types that adding a field can break: comparing values and using
// them as map keys. (Unkeyed struct literals are not covered: types may
// gain fields.)
var (
	_ = Result{} == Result{}
	_ = Turn{} == Turn{}
	_ = Config{} == Config{}
	_ = Options{} == Options{}
	_ = Scope{} == Scope{}
	_ = map[Result]bool{}
	_ = map[Turn]bool{}
	_ = Outcome("") == Success
)

// v1.5 callers set and read these fields by name.
func TestV15Fields(t *testing.T) {
	r := Result{Run: "r", Turn: 1, Profile: "p", Harness: "h", Session: "s", Outcome: Success,
		Output: "o", Attempts: 1, Duration: time.Second, StderrTail: "e"}
	tr := Turn{N: 1, Outcome: Success, Attempts: 1, ExitCode: 0, Queued: time.Time{}, Started: time.Time{}, Ended: time.Time{}, Error: ""}
	run := Run{ID: "r", Profile: "p", Harness: "h", Cwd: ".", Parent: "", Depth: 0, Session: "s", Created: time.Time{}, Turns: []Turn{tr}}
	cfg := Config{MaxDepth: 1, MaxRunning: 1, RetentionDays: 1}
	req := Request{Profile: "p", Prompt: "q", Cwd: ".", Vars: map[string]string{}}
	_, _, _, _, _ = r, tr, run, cfg, req
}

func TestMetricsValue(t *testing.T) {
	a := metricsOf(map[string]float64{"cost_usd": 0.0159, "input_tokens": 7839})
	b := metricsOf(map[string]float64{"input_tokens": 7839, "cost_usd": 0.0159})
	if a != b || a == (Metrics{}) {
		t.Fatal("equal metrics compare unequal, or empty")
	}
	if v, ok := a.Get("cost_usd"); !ok || v != 0.0159 {
		t.Fatalf("Get: %v %v", v, ok)
	}
	if _, ok := a.Get("missing"); ok {
		t.Fatal("missing metric found")
	}
	if m := a.Map(); len(m) != 2 || m["input_tokens"] != 7839 {
		t.Fatalf("Map: %v", m)
	}
	if a.String() != "cost_usd=0.0159 input_tokens=7839" {
		t.Fatalf("String: %q", a.String())
	}
	data, err := json.Marshal(Result{Metrics: a})
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(data, &back); err != nil || back.Metrics != a {
		t.Fatalf("JSON round trip: %s %v", data, err)
	}
	if metricsOf(map[string]float64{"a=b": 1, "c\nd": 2}) != (Metrics{}) {
		t.Fatal("names that break the encoding were kept")
	}
}

func TestMetricsSignedZero(t *testing.T) {
	var a, b Metrics
	if err := json.Unmarshal([]byte(`{"cost_usd":-0}`), &a); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(`{"cost_usd":0}`), &b)
	if a != b {
		t.Fatalf("%q != %q", a.String(), b.String())
	}
}
