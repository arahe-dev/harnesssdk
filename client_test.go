package harnesssdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arahe-dev/harnesssdk/v2/internal/engine"
)

// The test binary is also the fake agent CLI (HARNESS_SDK_FAKE=1) and, as
// any program using Client must, runs harness's own subcommands.
func TestMain(m *testing.M) {
	if code, ok := Subcommand(); ok {
		os.Exit(code)
	}
	if os.Getenv("HARNESS_SDK_FAKE") == "1" {
		in, _ := io.ReadAll(os.Stdin)
		if os.Args[1] == "fail" {
			fmt.Fprintln(os.Stderr, "boom")
			os.Exit(1)
		}
		fmt.Printf("answer: %s", strings.TrimSpace(string(in)))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := Open(Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	exe, _ := os.Executable()
	def, _ := json.Marshal(map[string]any{
		"command": exe,
		"args":    [][]string{{"{mode}"}, {"--sid", "{new_session}"}, {"--resume", "{resume_session}"}},
		"env":     map[string]string{"HARNESS_SDK_FAKE": "1"},
		"session": "mint",
		"output":  map[string]string{"format": "text"},
	})
	if err := c.SetDefinition("fake", def, Scope{}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"echo", "fail"} {
		p := "---\nharness: fake\neffect: pure\nmode: " + mode + "\n---\nBe brief.\n"
		if err := c.SetProfile(mode, []byte(p), Scope{}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestRunAndSend(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	res, err := c.Run(ctx, Request{Profile: "echo", Prompt: "hello"})
	if err != nil || res.Outcome != Success || res.Output != "answer: hello" || res.Session == "" || res.Turn != 1 {
		t.Fatalf("run: %+v %v", res, err)
	}
	again, err := c.Send(ctx, res.Run, FollowUp{Prompt: "again"})
	if err != nil || again.Turn != 2 || again.Session != res.Session || again.Output != "answer: again" {
		t.Fatalf("send: %+v %v", again, err)
	}
	runs, err := c.Runs(0)
	if err != nil || len(runs) != 1 || len(runs[0].Turns) != 2 || runs[0].Turns[1].Outcome != Success || runs[0].Depth != 0 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
}

func TestFailedTurn(t *testing.T) {
	c := newClient(t)
	res, err := c.Run(context.Background(), Request{Profile: "fail", Prompt: "x"})
	if err == nil || res == nil || res.Outcome != Permanent || res.Outcome.ExitCode() != 3 || !strings.Contains(res.StderrTail, "boom") {
		t.Fatalf("failed turn: %+v %v", res, err)
	}
	if _, err := c.Run(context.Background(), Request{Profile: "missing", Prompt: "x"}); err == nil {
		t.Fatal("missing profile ran")
	}
	if _, err := c.Run(context.Background(), Request{Profile: "echo"}); err == nil {
		t.Fatal("empty prompt ran")
	}
	if _, err := c.Run(context.Background(), Request{Profile: "echo", Prompt: "x", Vars: map[string]string{"prompt": "y"}}); err == nil {
		t.Fatal("a built-in placeholder was set")
	}
}

func TestStartAndWait(t *testing.T) {
	c := newClient(t)
	id, err := c.Start(Request{Profile: "echo", Prompt: "later"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Wait(ctx, id)
	if err != nil || res.Output != "answer: later" {
		t.Fatalf("wait: %+v %v", res, err)
	}
	if err := c.SendStart(id, FollowUp{Prompt: "more"}); err != nil {
		t.Fatal(err)
	}
	res, err = c.Result(id)
	if err != nil && !errors.Is(err, ErrRunning) {
		t.Fatalf("result: %v", err)
	}
	if errors.Is(err, ErrRunning) && res.Outcome.Finished() {
		t.Fatalf("ErrRunning with outcome %s", res.Outcome)
	}
	if res, err = c.Wait(ctx, id); err != nil || res.Turn != 2 || res.Output != "answer: more" {
		t.Fatalf("second wait: %+v %v", res, err)
	}
}

func TestCompareAndStats(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	res, err := c.Compare(ctx, []string{"echo", "fail"}, Request{Prompt: "q", Kind: "eval"})
	if err != nil || len(res) != 2 {
		t.Fatal(err)
	}
	if res[0].Profile != "echo" || res[0].Outcome != Success || res[1].Outcome != Permanent || res[1].Error == "" {
		t.Fatalf("%+v %+v", res[0], res[1])
	}
	st, err := c.Stats(StatsFilter{Kind: "eval"})
	if err != nil || len(st) != 2 || st[0].Turns != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	runs, _ := c.Runs(0)
	if runs[0].Kind != "eval" {
		t.Fatalf("kind %q", runs[0].Kind)
	}
}

// Every complete definition in the guide is one def set accepts.
func TestGuideDefinitionsParse(t *testing.T) {
	n := 0
	for _, block := range strings.Split(Guide, "```json\n")[1:] {
		body, _, _ := strings.Cut(block, "```")
		if !strings.HasPrefix(body, `{"command"`) {
			continue
		}
		n++
		if _, err := engine.ParseDefinition("example", []byte(body)); err != nil {
			t.Errorf("guide example %d: %v\n%s", n, err, body)
		}
	}
	if n < 4 {
		t.Fatalf("found %d example definitions, want at least 4", n)
	}
}

func TestConfigAndGC(t *testing.T) {
	c := newClient(t)
	if err := c.SetConfig("max_depth", 0); err != nil {
		t.Fatal(err)
	}
	cfg, err := c.Config()
	if err != nil || cfg.MaxDepth != 0 || cfg.RetentionDays != 30 {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	res, err := c.Run(context.Background(), Request{Profile: "echo", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := c.GC(time.Hour, true); err != nil || len(ids) != 0 {
		t.Fatalf("gc of runs an hour old: %v %v", ids, err)
	}
	if ids, err := c.GC(0, true); err != nil || len(ids) != 1 || ids[0] != res.Run {
		t.Fatalf("gc of every finished run: %v %v", ids, err)
	}
}
