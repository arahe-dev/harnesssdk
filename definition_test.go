package harnesssdk

import (
	"slices"
	"testing"

	"github.com/arahe-dev/hertasdk"
)

func TestParseDefinition(t *testing.T) {
	d, err := ParseDefinition("pi", []byte(`{
		"command": "pi",
		"args": [["-p"], ["--model", "{model}"], ["--thinking", "{thinking}"], ["{no_mcp?--no-mcp}"]],
		"session": "mint",
		"output": {"format": "text"},
		"failures": [{"match": "(?i)rate limit", "outcome": "throttled"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	argv, dropped, err := d.Render(Vars{"model": "m", "no_mcp": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pi", "-p", "--model", "m", "--no-mcp"}; !slices.Equal(argv, want) {
		t.Fatalf("argv %v, want %v", argv, want)
	}
	if len(dropped) != 1 || dropped[0].Missing != "thinking" {
		t.Fatalf("dropped %v", dropped)
	}
	if o := d.classify("Error: Rate Limit exceeded"); o != hertasdk.Throttled {
		t.Fatalf("classify: %v", o)
	}
	if o := d.classify("segfault"); o != hertasdk.Permanent {
		t.Fatalf("unmatched should be permanent, got %v", o)
	}
}

func TestParseDefinitionErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":   `{"command":"x","args":[],"output":{"format":"text"},"typo":1}`,
		"no command":      `{"args":[],"output":{"format":"text"}}`,
		"empty group":     `{"command":"x","args":[[]],"output":{"format":"text"}}`,
		"bad template":    `{"command":"x","args":[["{oops"]],"output":{"format":"text"}}`,
		"bad stdin":       `{"command":"x","stdin":"file","output":{"format":"text"}}`,
		"bad session":     `{"command":"x","session":"auto","output":{"format":"text"}}`,
		"bad format":      `{"command":"x","output":{"format":"xml"}}`,
		"json no field":   `{"command":"x","output":{"format":"json"}}`,
		"text with field": `{"command":"x","output":{"format":"text","field":"a"}}`,
		"bad outcome":     `{"command":"x","output":{"format":"text"},"failures":[{"match":"a","outcome":"fatal"}]}`,
		"bad regex":       `{"command":"x","output":{"format":"text"},"failures":[{"match":"(","outcome":"permanent"}]}`,
		"splat env":       `{"command":"x","env":{"A":"{*b}"},"output":{"format":"text"}}`,
	}
	for name, text := range cases {
		if _, err := ParseDefinition("x", []byte(text)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ParseDefinition("Bad Name", []byte(`{"command":"x","output":{"format":"text"}}`)); err == nil {
		t.Error("bad name: want error")
	}
}

func TestExtractJSON(t *testing.T) {
	d := &Definition{Output: OutputRule{Format: "json", Field: "result", ErrorField: "is_error", SessionField: "meta.session"}}
	ans, failed, session, err := d.extract([]byte(`{"result":"done","is_error":false,"meta":{"session":"s-1"}}`))
	if err != nil || ans != "done" || failed || session != "s-1" {
		t.Fatalf("got %q %v %q %v", ans, failed, session, err)
	}
	_, failed, _, _ = d.extract([]byte(`{"result":"x","is_error":true}`))
	if !failed {
		t.Fatal("error_field true should mark failure")
	}
	if _, _, _, err := d.extract([]byte(`not json`)); err == nil {
		t.Fatal("want error for non-JSON output")
	}
	if _, _, _, err := d.extract([]byte(`{"other":1}`)); err == nil {
		t.Fatal("want error for a missing field")
	}
	d.Output.Field = "items.1"
	if ans, _, _, _ := d.extract([]byte(`{"items":["a","b"]}`)); ans != "b" {
		t.Fatalf("array index: %q", ans)
	}
}

// The event stream of opencode run --format json, as captured from
// OpenCode 1.18.35 on 2026-10-11.
const opencodeEvents = `{"type":"step_start","timestamp":1791724338031,"sessionID":"ses_ed4e8557fffeMwuZjsS6XPzGJk","part":{"type":"step-start"}}
{"type":"text","timestamp":1791724338108,"sessionID":"ses_ed4e8557fffeMwuZjsS6XPzGJk","part":{"type":"text","text":"O"}}
not json: a banner line
{"type":"text","timestamp":1791724338109,"sessionID":"ses_ed4e8557fffeMwuZjsS6XPzGJk","part":{"type":"text","text":"K"}}
{"type":"step_finish","timestamp":1791724338517,"sessionID":"ses_ed4e8557fffeMwuZjsS6XPzGJk","part":{"reason":"stop","type":"step-finish"}}
`

func TestExtractJSONL(t *testing.T) {
	d, err := ParseDefinition("opencode", []byte(`{
		"command": "opencode",
		"args": [["run"], ["--format", "json"]],
		"output": {
			"format": "jsonl",
			"answer": {"where": {"type": "text"}, "field": "part.text", "pick": "join"},
			"session": {"where": {"type": "step_start"}, "field": "sessionID", "pick": "first"},
			"error": {"where": {"type": "error"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	ans, failed, session, err := d.extract([]byte(opencodeEvents))
	if err != nil || ans != "OK" || failed || session != "ses_ed4e8557fffeMwuZjsS6XPzGJk" {
		t.Fatalf("got %q %v %q %v", ans, failed, session, err)
	}
	_, failed, _, _ = d.extract([]byte(opencodeEvents + `{"type":"error","error":{"message":"boom"}}` + "\n"))
	if !failed {
		t.Fatal("an error event should mark failure")
	}
	if _, _, _, err := d.extract([]byte(`{"type":"step_start"}`)); err == nil {
		t.Fatal("no answer event should be an error")
	}
	d.Output.Answer.Pick = "last"
	if ans, _, _, _ := d.extract([]byte(opencodeEvents)); ans != "K" {
		t.Fatalf("pick last: %q", ans)
	}
}

func TestJSONLValidation(t *testing.T) {
	for name, out := range map[string]string{
		"no answer":          `{"format":"jsonl"}`,
		"answer no field":    `{"format":"jsonl","answer":{"where":{"type":"text"}}}`,
		"json field":         `{"format":"jsonl","field":"x","answer":{"field":"a"}}`,
		"bad pick":           `{"format":"jsonl","answer":{"field":"a","pick":"middle"}}`,
		"join without pick":  `{"format":"jsonl","answer":{"field":"a","join":","}}`,
		"selector with json": `{"format":"json","field":"a","answer":{"field":"a"}}`,
	} {
		if _, err := ParseDefinition("x", []byte(`{"command":"x","output":`+out+`}`)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
