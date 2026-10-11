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
// repeat guard counts consecutive failures of each request (the same
// profile, prompt, inputs and caller) in <home>/repeats/, warns the caller
// after config repeat_warn of them and refuses the request after
// repeat_refuse. A success clears the count; a throttled or cancelled turn
// does not count, since waiting or stopping is not the request's fault.
// Counts older than repeatWindow start over.

const repeatWindow = time.Hour

// ErrRepeated refuses a request that has failed repeat_refuse times in a
// row.
var ErrRepeated = errors.New("this exact request keeps failing")

type repeatRecord struct {
	Format   int       `json:"format"`
	Failures int       `json:"failures"`
	Last     time.Time `json:"last"`
}

// fingerprint identifies a request.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
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

func (h *Home) repeatPath(fp string) string { return filepath.Join(h.Dir, "repeats", fp+".json") }

func (h *Home) readRepeat(fp string) repeatRecord {
	var rec repeatRecord
	data, err := readShared(h.repeatPath(fp))
	if err != nil || json.Unmarshal(data, &rec) != nil || checkFormat("repeat count", rec.Format) != nil || time.Since(rec.Last) > repeatWindow {
		return repeatRecord{}
	}
	return rec
}

// checkRepeat refuses a request that has failed repeat_refuse times in a
// row within the window.
func (h *Home) checkRepeat(fp string) error {
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if n := h.readRepeat(fp).Failures; cfg.RepeatRefuse > 0 && n >= cfg.RepeatRefuse {
		return hertasdk.Fail(hertasdk.Permanent, fmt.Errorf(
			"%w: it failed %d times in a row in the last hour; change the prompt, the profile or the approach (or raise the limit: harness config set repeat_refuse <n>)",
			ErrRepeated, n))
	}
	return nil
}

// noteRepeat counts a finished turn of a request and returns a hint for the
// caller once the request has failed repeat_warn times in a row.
func (h *Home) noteRepeat(fp, outcome string) string {
	cfg, err := h.Config()
	if err != nil || cfg.RepeatWarn == 0 && cfg.RepeatRefuse == 0 {
		return ""
	}
	if err := os.MkdirAll(filepath.Join(h.Dir, "repeats"), 0o700); err != nil {
		return ""
	}
	unlock, err := lockFileWait(filepath.Join(h.Dir, "repeats", fp+".lock"), 5*time.Second)
	if err != nil {
		return ""
	}
	defer unlock()
	rec := h.readRepeat(fp)
	switch outcome {
	case "success":
		os.Remove(h.repeatPath(fp))
		return ""
	case "throttled", "cancelled", "stopped":
		return ""
	}
	rec.Format, rec.Failures, rec.Last = formatVersion, rec.Failures+1, time.Now().UTC()
	data, _ := json.Marshal(rec)
	if writeFileAtomic(h.repeatPath(fp), data) != nil || cfg.RepeatWarn == 0 || rec.Failures < cfg.RepeatWarn {
		return ""
	}
	limit := "it will not be refused"
	if cfg.RepeatRefuse > 0 {
		limit = fmt.Sprintf("harness refuses it after %d", cfg.RepeatRefuse)
	}
	return fmt.Sprintf("this exact request (same profile, prompt and inputs) has failed %d times in a row; change the prompt, the profile or the approach instead of repeating it (%s)",
		rec.Failures, limit)
}

// pruneRepeats removes counts that have run out, with their locks.
func (h *Home) pruneRepeats() {
	dir := filepath.Join(h.Dir, "repeats")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
