package engine

import (
	"slices"
	"testing"
)

func TestExpandGroup(t *testing.T) {
	v := Vars{"model": "gpt-6.1-sol", "on": "true", "off": "no", "extra": "  --a  --b ", "empty": ""}
	cases := []struct {
		group   []string
		want    []string
		ok      bool
		missing string
	}{
		{[]string{"-p"}, []string{"-p"}, true, ""},
		{[]string{"--model", "{model}"}, []string{"--model", "gpt-6.1-sol"}, true, ""},
		{[]string{"--model={model}"}, []string{"--model=gpt-6.1-sol"}, true, ""},
		{[]string{"--thinking", "{thinking}"}, nil, false, "thinking"},
		{[]string{"--x", "{empty}"}, nil, false, "empty"},
		{[]string{"{on?--no-mcp}"}, []string{"--no-mcp"}, true, ""},
		{[]string{"{off?--no-mcp}"}, nil, false, "off"},
		{[]string{"{*extra}"}, []string{"--a", "--b"}, true, ""},
		{[]string{"{*empty}"}, nil, false, "empty"},
		{[]string{"{{literal}}"}, []string{"{literal}"}, true, ""},
	}
	for _, c := range cases {
		got, ok, missing, err := expandGroup(c.group, v)
		if err != nil {
			t.Fatalf("%v: %v", c.group, err)
		}
		if ok != c.ok || missing != c.missing || !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v ok=%v missing=%q, want %v ok=%v missing=%q", c.group, got, ok, missing, c.want, c.ok, c.missing)
		}
	}
}

func TestParseTemplateErrors(t *testing.T) {
	for _, s := range []string{"{unclosed", "close}", "{Bad}", "{}", "x{*splat}", "{*a?b}"} {
		if _, err := parseTemplate(s); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}

func TestExpandString(t *testing.T) {
	got, err := expandString("{cwd}/{missing}{flag?:on}", Vars{"cwd": "/w", "flag": "yes"})
	if err != nil || got != "/w/:on" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"pi", "pr-review", "gpt.6_1", "a1"} {
		if !validName(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "Pi", "-x", "_x", "a b", "a/b", "_unclaimed"} {
		if validName(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}
