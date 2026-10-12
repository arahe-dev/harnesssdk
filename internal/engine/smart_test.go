package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// usageDef reads the "usage" fake's events: the answer is the last part of
// the final message, and every step's tokens and cost are summed.
func usageDef(f *fixture) {
	f.def("fakeusage", Definition{
		Args: [][]string{{"{mode}"}, {"{*opts}"}},
		Output: OutputRule{Format: "jsonl",
			Answer: &Selector{Where: map[string]string{"type": "done"}, Field: "content.-1.text"},
			Metrics: map[string]*Selector{
				"input_tokens": {Where: map[string]string{"type": "step"}, Field: "usage.input+usage.cache", Pick: "sum"},
				"cost_usd":     {Where: map[string]string{"type": "step"}, Field: "cost", Pick: "sum"},
				"turns":        {Where: map[string]string{"type": "step"}, Pick: "count"},
			}},
		Failures: []FailureRule{{Match: "ECONNRESET", Outcome: "transient"}},
	})
}

func TestMetricsSumOverAttempts(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	state := filepath.Join(t.TempDir(), "state")
	f.profile("u", "harness: fakeusage\neffect: idempotent\nretry: transient x2\nmode: usage\nmodel: m-1\nopts: state="+state)
	rep := f.start(context.Background(), "u", "hi")[0]
	if rep.Err != nil || rep.Output != "answer: hi" || rep.Attempts != 2 {
		t.Fatalf("%+v", rep)
	}
	// One step in the failed attempt, two in the one that answered.
	want := map[string]float64{"input_tokens": 360, "cost_usd": 0.75, "turns": 3}
	run, _ := f.home.LoadRun(rep.Run.ID)
	rec := run.Turns[0]
	for k, v := range want {
		if rec.Metrics[k] != v || rep.Metrics[k] != v {
			t.Errorf("%s: record %v, report %v, want %v", k, rec.Metrics[k], rep.Metrics[k], v)
		}
	}
	if rec.Model != "m-1" || rep.Model != "m-1" {
		t.Errorf("model %q / %q", rec.Model, rep.Model)
	}
}

func TestMetricsFromOneDocument(t *testing.T) {
	d, err := ParseDefinition("claude", []byte(`{"command": "claude", "output": {"format": "json", "field": "result",
		"metrics": {"input_tokens": {"field": "usage.input_tokens+usage.cache_read_input_tokens"}, "cost_usd": {"field": "total_cost_usd"},
		"missing": {"field": "nope"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	x, err := d.extract(strings.NewReader(`{"result":"ok","total_cost_usd":0.016,"usage":{"input_tokens":10,"cache_read_input_tokens":7852}}`))
	if err != nil || x.Answer != "ok" || x.Metrics["input_tokens"] != 7862 || x.Metrics["cost_usd"] != 0.016 {
		t.Fatalf("%+v %v", x, err)
	}
	if _, ok := x.Metrics["missing"]; ok {
		t.Error("a metric the output lacks was recorded")
	}
	// Metrics are found even when the answer is not.
	x, err = d.extract(strings.NewReader(`{"total_cost_usd":0.5}`))
	if err == nil || x.Metrics["cost_usd"] != 0.5 {
		t.Fatalf("%+v %v", x, err)
	}
	for name, out := range map[string]string{
		"text output":   `{"format":"text","metrics":{"x":{"field":"a"}}}`,
		"no field":      `{"format":"json","field":"r","metrics":{"x":{}}}`,
		"join":          `{"format":"json","field":"r","metrics":{"x":{"field":"a","pick":"join","join":","}}}`,
		"bad name":      `{"format":"json","field":"r","metrics":{"Cost":{"field":"a"}}}`,
		"sum on answer": `{"format":"jsonl","answer":{"field":"a","pick":"sum"}}`,
	} {
		if _, err := ParseDefinition("x", []byte(`{"command":"x","output":`+out+`}`)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRepeatGuard(t *testing.T) {
	f := newFixture(t, nil)
	f.profile("bad", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=boom")
	f.home.SetConfig("repeat_warn", 2)
	f.home.SetConfig("repeat_refuse", 3)
	for i := 1; i <= 3; i++ {
		rep := f.start(context.Background(), "bad", "same")[0]
		if rep.Outcome != "permanent" {
			t.Fatalf("attempt %d: %s", i, rep.Outcome)
		}
		if hinted := rep.Hint != ""; hinted != (i >= 2) {
			t.Fatalf("attempt %d: hint %q", i, rep.Hint)
		}
		if run, _ := f.home.LoadRun(rep.Run.ID); run.Turns[0].Hint != rep.Hint {
			t.Fatal("hint not recorded for wait and result")
		}
	}
	_, err := f.r.Start(context.Background(), "bad", []string{"same"}, f.cwd, TurnOptions{}, nil)
	if !errors.Is(err, ErrRepeated) || OutcomeName(err) != "permanent" {
		t.Fatalf("fourth identical request: %v", err)
	}
	// A different prompt is a different request.
	if rep := f.start(context.Background(), "bad", "different")[0]; rep.Hint != "" {
		t.Fatalf("different request hinted: %q", rep.Hint)
	}
	// A success clears the count.
	f.profile("ok", "harness: fake\neffect: pure\nmode: echo")
	fp := runFingerprint(t, f, "ok", "x")
	for i := range 2 {
		f.home.noteRepeat(fp, fmt.Sprint("r/", i), "permanent", now())
	}
	if h := f.home.noteRepeat(fp, "r/9", "throttled", now()); h != "" || len(f.home.failures(fp)) != 2 {
		t.Fatal("a throttled turn counted")
	}
	f.start(context.Background(), "ok", "x")
	if n := len(f.home.failures(fp)); n != 0 {
		t.Fatalf("count after success: %d", n)
	}
	// Turned off.
	f.home.SetConfig("repeat_refuse", 0)
	if _, err := f.r.Start(context.Background(), "bad", []string{"same"}, f.cwd, TurnOptions{}, nil); err != nil {
		t.Fatalf("with repeat_refuse 0: %v", err)
	}
}

func TestCompare(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("a", "harness: fake\neffect: pure\nmode: echo")
	f.profile("b", "harness: fakeusage\neffect: pure\nmode: usage\nmodel: m-b")
	reps, err := f.r.Compare(context.Background(), []string{"a", "b"}, "q", f.cwd, TurnOptions{Kind: "eval"}, nil)
	if err != nil || len(reps) != 2 {
		t.Fatal(err)
	}
	if reps[0].Run.Profile != "a" || !strings.Contains(reps[0].Output, "q") || reps[1].Run.Profile != "b" || reps[1].Output != "answer: q" {
		t.Fatalf("%+v", reps)
	}
	if reps[1].Metrics["turns"] != 2 || reps[0].Run.Kind != "eval" {
		t.Fatalf("metrics %v, kind %q", reps[1].Metrics, reps[0].Run.Kind)
	}
	before, _ := f.home.Runs(0)
	for _, bad := range [][]string{{"a"}, {"a", "a"}, {"a", "missing"}} {
		if _, err := f.r.Compare(context.Background(), bad, "q", f.cwd, TurnOptions{}, nil); err == nil {
			t.Errorf("compare %v accepted", bad)
		}
	}
	if after, _ := f.home.Runs(0); len(after) != len(before) {
		t.Fatal("a refused compare created runs")
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t, nil)
	usageDef(f)
	f.profile("b", "harness: fakeusage\neffect: pure\nmode: usage\nmodel: m-b")
	f.profile("bad", "harness: fake\neffect: pure\nmode: fail\nopts: code=3 msg=boom")
	f.start(context.Background(), "b", "1", "2")
	if _, err := f.r.Start(context.Background(), "b", []string{"3"}, f.cwd, TurnOptions{Kind: "review"}, nil); err != nil {
		t.Fatal(err)
	}
	f.start(context.Background(), "bad", "x")
	st, err := f.home.Stats(StatsFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil || len(st) != 3 {
		t.Fatalf("%+v %v", st, err)
	}
	b := st[0]
	if b.Profile != "b" || b.Model != "m-b" || b.Kind != "" || b.Turns != 2 || b.Outcomes["success"] != 2 ||
		b.Metrics["cost_usd"].Sum != 1 || b.Metrics["cost_usd"].Mean() != 0.5 || b.Metrics["turns"].Turns != 2 {
		t.Fatalf("%+v", b)
	}
	only, _ := f.home.Stats(StatsFilter{Kind: "review"})
	if len(only) != 1 || only[0].Turns != 1 || only[0].Kind != "review" {
		t.Fatalf("by kind: %+v", only)
	}
	bad, _ := f.home.Stats(StatsFilter{Profile: "bad"})
	if len(bad) != 1 || bad[0].Outcomes["permanent"] != 1 || bad[0].Metrics != nil {
		t.Fatalf("bad: %+v", bad)
	}
	if none, _ := f.home.Stats(StatsFilter{Since: time.Now().Add(time.Hour)}); len(none) != 0 {
		t.Fatalf("future: %+v", none)
	}
}
