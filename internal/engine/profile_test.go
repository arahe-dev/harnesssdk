package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arahe-dev/hertasdk"
)

const reviewProfile = `---
harness: pi
description: Read-only reviewer   # trailing comment
model: "openai-codex/gpt-6.1-sol"
thinking: high
# a comment line
effect: idempotent
retry: transient x3
timeout: 20m
budget: pi=1, gpu=2
serialize: "{cwd}"
---
You are a reviewer.
`

func TestParseProfile(t *testing.T) {
	p, err := ParseProfile("review", []byte(reviewProfile))
	if err != nil {
		t.Fatal(err)
	}
	if p.Harness != "pi" || p.Description != "Read-only reviewer" || p.Effect != hertasdk.Idempotent {
		t.Fatalf("header: %+v", p)
	}
	if p.Vars["model"] != "openai-codex/gpt-6.1-sol" || p.Vars["thinking"] != "high" {
		t.Fatalf("vars: %v", p.Vars)
	}
	if p.Timeout != 20*time.Minute || p.Retry.MaxAttempts != 3 || !p.Retry.On[hertasdk.Transient] {
		t.Fatalf("policy: %v %+v", p.Timeout, p.Retry)
	}
	if len(p.Budget) != 2 || p.Budget[1] != (hertasdk.Requirement{Name: "gpu", Units: 2}) {
		t.Fatalf("budget: %v", p.Budget)
	}
	if p.Serialize != "{cwd}" || p.System != "You are a reviewer." {
		t.Fatalf("serialize %q system %q", p.Serialize, p.System)
	}
}

func TestParseProfileCRLFAndEmptyBody(t *testing.T) {
	p, err := ParseProfile("x", []byte("---\r\nharness: pi\r\neffect: pure\r\n---\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.System != "" || p.Harness != "pi" {
		t.Fatalf("%+v", p)
	}
}

func TestParseProfileErrors(t *testing.T) {
	cases := map[string]string{
		"no header":      "harness: pi\n",
		"unclosed":       "---\nharness: pi\n",
		"no harness":     "---\neffect: pure\n---\n",
		"no effect":      "---\nharness: pi\n---\n",
		"bad effect":     "---\nharness: pi\neffect: sometimes\n---\n",
		"builtin key":    "---\nharness: pi\neffect: pure\nprompt: x\n---\n",
		"duplicate":      "---\nharness: pi\nharness: pi\neffect: pure\n---\n",
		"bad retry":      "---\nharness: pi\neffect: pure\nretry: transient y2\n---\n",
		"bad budget":     "---\nharness: pi\neffect: pure\nbudget: pi=0\n---\n",
		"bad timeout":    "---\nharness: pi\neffect: pure\ntimeout: soon\n---\n",
		"not key value":  "---\nharness pi\n---\n",
		"bad serialize":  "---\nharness: pi\neffect: pure\nserialize: {oops\n---\n",
		"bad admission":  "---\nharness: pi\neffect: pure\nadmission: maybe\n---\n",
		"bad outcome":    "---\nharness: pi\neffect: pure\nretry: flaky x2\n---\n",
		"bad header key": "---\nharness: pi\neffect: pure\nBad Key: 1\n---\n",
	}
	for name, text := range cases {
		if _, err := ParseProfile("x", []byte(text)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestProfileSet(t *testing.T) {
	p, err := ParseProfile("review", []byte(reviewProfile))
	if err != nil {
		t.Fatal(err)
	}
	q, err := p.Set([]string{"thinking=xhigh", "tools=read,grep", "description="})
	if err != nil {
		t.Fatal(err)
	}
	if q.Vars["thinking"] != "xhigh" || q.Vars["tools"] != "read,grep" || q.Description != "" {
		t.Fatalf("%+v %v", q, q.Vars)
	}
	text := string(q.Bytes())
	if !strings.Contains(text, "# a comment line") || !strings.HasSuffix(text, "You are a reviewer.\n") {
		t.Fatalf("Set lost content:\n%s", text)
	}
	if _, err := p.Set([]string{"novalue"}); err == nil {
		t.Fatal("want error for a pair without =")
	}
}

func TestCheckPolicyRejectsUnsafeRetry(t *testing.T) {
	p, err := ParseProfile("writer", []byte("---\nharness: pi\neffect: non-idempotent\nretry: uncertain x2\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPolicy(p, nil); !errors.Is(err, hertasdk.ErrUnsafeRetry) {
		t.Fatalf("got %v, want ErrUnsafeRetry", err)
	}
	p, _ = ParseProfile("reader", []byte("---\nharness: pi\neffect: idempotent\nretry: uncertain x2\nbudget: pi\n---\n"))
	if err := checkPolicy(p, map[string]int64{"pi": 2}); err != nil {
		t.Fatalf("idempotent uncertain retry should be allowed: %v", err)
	}
	if err := checkPolicy(p, nil); err == nil {
		t.Fatal("unknown budget should be rejected")
	}
}
