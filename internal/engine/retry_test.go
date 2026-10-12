package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arahe-dev/hertasdk"
)

func TestBackoffDelay(t *testing.T) {
	for range 100 {
		if d := backoffDelay(time.Second, 1, 0); d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("first retry waited %s, want 0.5s to 1s", d)
		}
		if d := backoffDelay(time.Second, 3, 0); d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("third retry waited %s, want 2s to 4s", d)
		}
		if d := backoffDelay(time.Second, 40, 0); d > maxBackoff {
			t.Fatalf("waited %s, above %s", d, maxBackoff)
		}
	}
	if d := backoffDelay(time.Second, 1, 10*time.Minute); d != 10*time.Minute {
		t.Fatalf("retry-after not honored: %s", d)
	}
	if d := backoffDelay(time.Second, 1, 3*time.Hour); d != maxRetryAfter {
		t.Fatalf("retry-after not capped: %s", d)
	}
	if d := backoffDelay(0, 1, 0); d < defaultBackoff/2 || d > defaultBackoff {
		t.Fatalf("default backoff: %s", d)
	}
}

func TestRetryAfterIn(t *testing.T) {
	d, err := ParseDefinition("x", []byte(`{"command":"x","output":{"format":"text"},"retry_after":"(?i)retry.after:?\\s*([0-9.]+(?:[hms][0-9hms.]*)?)"}`))
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]time.Duration{
		"429 too many requests; retry after 30":  30 * time.Second,
		"Retry-After: 1.5":                       1500 * time.Millisecond,
		"error: rate limited, retry after 1m30s": 90 * time.Second,
		"no hint here":                           0,
	} {
		if got := d.retryAfterIn(text); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
	for _, bad := range []string{`"retry_after":"no group"`, `"retry_after":"(a)(b)"`, `"retry_after":"(["`} {
		if _, err := ParseDefinition("x", []byte(`{"command":"x","output":{"format":"text"},`+bad+`}`)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// A throttled attempt is retried after at least the wait its CLI asked
// for, by a profile that may repeat it; a non-idempotent one may not.
func TestThrottledRetryHonorsRetryAfter(t *testing.T) {
	f := newFixture(t, nil)
	f.def("fakera", Definition{
		Args:       [][]string{{"{mode}"}, {"{*opts}"}},
		Output:     OutputRule{Format: "text"},
		Failures:   []FailureRule{{Match: "(?i)rate limit", Outcome: "throttled"}},
		RetryAfter: `retry after (\d+) seconds`,
	})
	state := filepath.Join(t.TempDir(), "state")
	f.profile("ra", "harness: fakera\neffect: idempotent\nretry: throttled x2\nbackoff: 10ms\nmode: ratelimited\nopts: state="+state)
	start := time.Now()
	rep := f.start(context.Background(), "ra", "x")[0]
	if rep.Err != nil || rep.Output != "ok after waiting" || rep.Attempts != 2 {
		t.Fatalf("%+v", rep)
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("retried after %s; the CLI asked for 1s", d)
	}
	log, _ := os.ReadFile(rep.Run.Path("turn-1.stderr.log"))
	if !strings.Contains(string(log), "throttled; retrying in") {
		t.Fatalf("backoff not logged:\n%s", log)
	}
	p, err := ParseProfile("unsafe", []byte("---\nharness: fakera\neffect: non-idempotent\nretry: throttled x2\n---\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.home.SaveProfile(p, "", f.cwd); !errors.Is(err, hertasdk.ErrUnsafeRetry) {
		t.Fatalf("non-idempotent throttled retry saved: %v", err)
	}
}

// While a turn backs off between attempts it holds no budget: another
// turn runs in the meantime.
func TestBackoffReleasesBudgets(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	state := filepath.Join(t.TempDir(), "state")
	f.profile("flaky", "harness: fake\neffect: idempotent\nbudget: b=1\nretry: transient x2\nbackoff: 3s\nmode: flaky\nopts: state="+state)
	f.saveProfile("quick", "harness: fake\neffect: pure\nbudget: b=1\nmode: echo")
	done := make(chan Report)
	go func() { done <- f.start(context.Background(), "flaky", "x")[0] }()
	// Wait for the first attempt to fail and the backoff to begin.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(state); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first attempt never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if rep := f.start(context.Background(), "quick", "y")[0]; rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Fatalf("the other turn waited %s: the backing-off turn held the budget", d)
	}
	select {
	case rep := <-done:
		t.Fatalf("the flaky turn finished before its backoff: %+v", rep)
	default:
	}
	if rep := <-done; rep.Err != nil || rep.Attempts != 2 {
		t.Fatalf("%+v", rep)
	}
}

// A turn waits at most max_wait for its budgets, and does not queue behind
// max_queue others.
func TestMaxWaitAndMaxQueue(t *testing.T) {
	f := newFixture(t, map[string]int64{"b": 1})
	f.profile("w", "harness: fake\neffect: pure\nbudget: b=1\nmax_wait: 300ms\nmode: echo")
	held, err := f.home.Gate(nil).Acquire(context.Background(), []Claim{{Name: "b", Units: 1, Capacity: 1}}, false, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	start := time.Now()
	rep := f.start(context.Background(), "w", "x")[0]
	if rep.Outcome != "throttled" || !strings.Contains(rep.Err.Error(), "max_wait") || time.Since(start) > 3*time.Second {
		t.Fatalf("%s %v after %s", rep.Outcome, rep.Err, time.Since(start))
	}
	// One turn waits; with max_queue 1 a second is refused at once.
	f.home.SetConfig("max_queue", 1)
	f.profile("q", "harness: fake\neffect: pure\nbudget: b=1\nmode: echo")
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan Report)
	go func() { waiting <- f.start(ctx, "q", "first")[0] }()
	time.Sleep(300 * time.Millisecond)
	start = time.Now()
	rep = f.start(context.Background(), "q", "second")[0]
	if rep.Outcome != "throttled" || !strings.Contains(rep.Err.Error(), "waiting already") || time.Since(start) > 3*time.Second {
		t.Fatalf("%s %v", rep.Outcome, rep.Err)
	}
	cancel()
	<-waiting
}
