package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// An agent that keeps sending the same request after it fails is usually
// stuck: each repeat is a whole agent run, spent for the same failure. The
// repeat guard keeps each request's consecutive failures from the last
// hour in <home>/repeats/, warns the caller once there are config
// repeat_warn of them and refuses the request at repeat_refuse. A success
// clears them; a throttled or cancelled turn does not count, since waiting
// or stopping is not the request's fault. A request is everything that
// shapes it: its caller, the profile and definition as they are now (so an
// edited profile is a new request), the folder, the prompt and the per-run
// values.

const repeatWindow = time.Hour

// ErrRepeated refuses a request that has failed repeat_refuse times in a
// row within the last hour.
var ErrRepeated = errors.New("this exact request keeps failing")

// now is the clock; tests move it.
var now = time.Now

type repeatRecord struct {
	Format   int         `json:"format"`
	Failures []time.Time `json:"failures"` // consecutive, oldest first
}

// fingerprint identifies a request.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// requestFingerprint is the fingerprint of a new run ("run", by its parent
// run) or a follow-up ("send", by its run).
func requestFingerprint(kind, caller string, p *Profile, d *Definition, cwd, prompt string, extra Vars) string {
	def, _ := json.Marshal(d)
	return fingerprint(kind, caller, p.Name, digest(p.Bytes()), digest(def), cwd, prompt, varsKey(extra))
}

// varsKey is a stable rendering of per-run placeholder values.
func varsKey(v Vars) string {
	var b strings.Builder
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range v {
			if !yield(k) {
				return
			}
		}
	}) {
		b.WriteString(k + "=" + v[k] + "\x00")
	}
	return b.String()
}

func (h *Home) repeatDir() string { return filepath.Join(h.Dir, "repeats") }

func (h *Home) repeatPath(fp string) string { return filepath.Join(h.repeatDir(), fp+".json") }

// One lock guards every record: updates are rare and short, and a lock
// file that is never removed cannot be removed while it is held.
func (h *Home) repeatLock() string { return filepath.Join(h.Dir, "repeats.lock") }

// failures are a request's consecutive failures within the window.
func (h *Home) failures(fp string) []time.Time {
	var rec repeatRecord
	data, err := readShared(h.repeatPath(fp))
	if err != nil || json.Unmarshal(data, &rec) != nil || checkFormat("repeat record", rec.Format) != nil {
		return nil // missing, or from an older harness that counted differently: start over
	}
	cutoff := now().Add(-repeatWindow)
	return slices.DeleteFunc(rec.Failures, func(t time.Time) bool { return t.Before(cutoff) })
}

// checkRepeat refuses a request that has failed repeat_refuse times in a
// row within the window. It is checked when a turn is queued and again
// when it is executed, so turns queued before the limit was reached are
// refused too.
func (h *Home) checkRepeat(fp string) error {
	if fp == "" {
		return nil
	}
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if n := len(h.failures(fp)); cfg.RepeatRefuse > 0 && n >= cfg.RepeatRefuse {
		return hertasdk.Fail(hertasdk.Permanent, fmt.Errorf(
			"%w: it failed %d times in a row within the last hour; change the prompt, the profile or the approach (or raise the limit: harness config set repeat_refuse <n>)",
			ErrRepeated, n))
	}
	return nil
}

// noteRepeat counts a finished turn of a request and returns a hint for the
// caller once the request has failed repeat_warn times in a row.
func (h *Home) noteRepeat(fp, outcome string) string {
	cfg, err := h.Config()
	if fp == "" || err != nil || cfg.RepeatWarn == 0 && cfg.RepeatRefuse == 0 {
		return ""
	}
	switch outcome {
	case "throttled", "cancelled", "stopped":
		return ""
	}
	if err := os.MkdirAll(h.repeatDir(), 0o700); err != nil {
		return ""
	}
	unlock, err := lockFileWait(h.repeatLock(), 5*time.Second)
	if err != nil {
		return ""
	}
	defer unlock()
	if outcome == "success" {
		os.Remove(h.repeatPath(fp))
		return ""
	}
	fails := append(h.failures(fp), now().UTC())
	data, _ := json.Marshal(repeatRecord{Format: formatVersion, Failures: fails})
	if writeFileAtomic(h.repeatPath(fp), data) != nil || cfg.RepeatWarn == 0 || len(fails) < cfg.RepeatWarn {
		return ""
	}
	limit := "it will not be refused"
	if cfg.RepeatRefuse > 0 {
		limit = fmt.Sprintf("harness refuses it at %d", cfg.RepeatRefuse)
	}
	return fmt.Sprintf("this exact request (same profile, prompt and inputs) has failed %d times in a row within the last hour; change the prompt, the profile or the approach instead of repeating it (%s)",
		len(fails), limit)
}

// pruneRepeats removes records whose failures have all aged out, under the
// lock every update takes; a busy lock leaves them for the next pass.
func (h *Home) pruneRepeats() {
	entries, err := os.ReadDir(h.repeatDir())
	if err != nil {
		return
	}
	unlock, err := lockFile(h.repeatLock())
	if err != nil {
		return
	}
	defer unlock()
	for _, e := range entries {
		name := e.Name()
		if fp, ok := strings.CutSuffix(name, ".json"); ok && len(h.failures(fp)) == 0 {
			os.Remove(filepath.Join(h.repeatDir(), name))
		}
		// v1.6 kept a lock file per record; none is taken any more.
		if info, err := e.Info(); err == nil && strings.HasSuffix(name, ".lock") && now().Sub(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(h.repeatDir(), name))
		}
	}
}
