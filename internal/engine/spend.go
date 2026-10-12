package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Agents that start agents can spend without anyone watching. Spending
// limits cap what turns cost over the last 24 hours, from the cost_usd
// metric the CLIs report: config spend_limit_usd for the whole machine,
// and a profile's spend_limit_usd for that profile. They are checked when
// runs are created and again before every attempt, so a fan-out stops once
// the limit is reached; turns already running finish. A CLI that reports
// no cost (Codex) is not limited by them.
//
// What turns spent is kept in a ledger, <home>/spend, with one entry per
// attempt, <run>-<turn>-<attempt>.json, written whole. The entry is made,
// "running", before the attempt runs, and rewritten with what the attempt
// cost before the attempt's admission is released; a turn under a limit
// whose entry cannot be made does not run, and one whose cost cannot be
// written stays admitted, trying again, until it is written or the turn is
// stopped. Spending sums the entries in the window, by when each attempt
// ended. An entry still running counts nothing while its turn runs; one
// whose turn is over (its process died, or it was stopped before its cost
// could be written) has an unknown cost, and so does an entry that cannot
// be read: turns under a limit it falls under are refused until it leaves
// the window, or someone removes it. Turns recorded before v2 count by
// their run records. Entries go once they are two windows old.

const spendWindow = 24 * time.Hour

// spendEntry is one attempt's entry in the ledger.
type spendEntry struct {
	Run     string    `json:"run"`
	Turn    int       `json:"turn"`
	Attempt int       `json:"attempt"`
	Profile string    `json:"profile"`
	At      time.Time `json:"at"` // when the attempt started, or, once its cost is in, ended
	USD     float64   `json:"usd"`
	Running bool      `json:"running,omitempty"`
}

func (h *Home) spendDir() string { return filepath.Join(h.Dir, "spend") }

func (h *Home) spendPath(e *spendEntry) string {
	return filepath.Join(h.spendDir(), fmt.Sprintf("%s-%d-%d.json", e.Run, e.Turn, e.Attempt))
}

// testHookSpend, when set, runs as an entry is opened or closed ("open",
// "close"); an error fails that write.
var testHookSpend func(stage string) error

func (h *Home) writeSpend(stage string, e *spendEntry) error {
	if testHookSpend != nil {
		if err := testHookSpend(stage); err != nil {
			return err
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.spendDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(h.spendPath(e), append(data, '\n'))
}

// openSpend makes the running entry of turn t's next attempt. Entries are
// numbered as they are made, so none is ever written over by another.
func (h *Home) openSpend(t *Turn) error {
	e := &spendEntry{Run: t.Run.ID, Turn: t.N, Attempt: t.opened + 1, Profile: t.Profile.Name, At: now().UTC(), Running: true}
	if err := h.writeSpend("open", e); err != nil {
		return err
	}
	t.entry, t.opened = e, t.opened+1
	return nil
}

// closeSpend writes what turn t's attempt cost (since the cost last
// written) into its entry.
func (h *Home) closeSpend(t *Turn) error {
	if t.entry == nil {
		return nil
	}
	cost := finiteMetrics(t.metrics)["cost_usd"]
	e := *t.entry
	e.At, e.USD, e.Running = now().UTC(), cost-t.spendSaved, false
	if err := h.writeSpend("close", &e); err != nil {
		return err
	}
	t.spendSaved, t.entry = cost, nil
	return nil
}

// limited reports whether a spending limit applies to turns of p.
func limited(cfg Config, p *Profile) bool { return cfg.SpendLimitUSD > 0 || p.SpendLimit > 0 }

// checkSpend refuses a turn of p as throttled once p, or the machine, has
// spent its limit, or may have: while what an attempt under that limit
// cost is unknown.
func (h *Home) checkSpend(p *Profile) error {
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if !limited(cfg, p) {
		return nil
	}
	s, err := h.spending(now().Add(-spendWindow), p.Name)
	if err != nil {
		return err
	}
	unknown := func(what string) error {
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"%s, so turns under a spending limit are refused until it is 24 hours old, or until you remove it", what))
	}
	switch {
	case cfg.SpendLimitUSD > 0 && s.unknownAll != "":
		return unknown(s.unknownAll)
	case cfg.SpendLimitUSD > 0 && s.all >= float64(cfg.SpendLimitUSD):
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"turns on this machine have spent $%.2f in the last 24 hours, and spend_limit_usd is $%d; raise it with: harness config set spend_limit_usd <dollars>", s.all, cfg.SpendLimitUSD))
	case p.SpendLimit > 0 && s.unknownMine != "":
		return unknown(s.unknownMine)
	case p.SpendLimit > 0 && s.mine >= p.SpendLimit:
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"profile %s has spent $%.2f in the last 24 hours, and its spend_limit_usd is $%.2f", p.Name, s.mine, p.SpendLimit))
	}
	return nil
}

// spent is what turns spent in a window: on the machine, and by one
// profile, with the first entry of each whose cost is unknown.
type spent struct {
	all, mine               float64
	unknownAll, unknownMine string
}

// spending sums what attempts cost since then, by when each ended: so a
// follow-up today on yesterday's run counts, a turn still running counts
// its finished attempts, and an attempt from before the window does not
// count because a later one is in it.
func (h *Home) spending(since time.Time, profile string) (s spent, err error) {
	unknown := func(mine bool, what string) {
		if s.unknownAll == "" {
			s.unknownAll = what
		}
		if mine && s.unknownMine == "" {
			s.unknownMine = what
		}
	}
	files, err := os.ReadDir(h.spendDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue // an entry being written
		}
		path := filepath.Join(h.spendDir(), name)
		info, err := f.Info()
		if err == nil && info.ModTime().Before(since) {
			continue // last written before the window
		}
		var e spendEntry
		var data []byte
		if err == nil {
			data, err = readShared(path)
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if err == nil {
			err = json.Unmarshal(data, &e)
		}
		if err != nil {
			unknown(true, fmt.Sprintf("the spending entry %s cannot be read (%v)", path, err))
			continue
		}
		switch {
		case e.At.Before(since):
		case e.Running && h.turnRunning(e.Run, e.Turn):
			// Its cost is not known yet; turns already running finish.
		case e.Running:
			unknown(e.Profile == profile, fmt.Sprintf("what run %s turn %d attempt %d cost is unknown (it ended before its cost was written to %s)", e.Run, e.Turn, e.Attempt, path))
		default:
			s.all += e.USD
			if e.Profile == profile {
				s.mine += e.USD
			}
		}
	}
	// Turns recorded before v2 count by their run records, by when they
	// ended; only run folders changed since then are read.
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), stagePrefix) {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().Before(since) {
			continue
		}
		run, err := h.LoadRun(e.Name())
		if err != nil || run.Format >= runFormat {
			continue
		}
		for _, t := range run.Turns {
			if !t.Ended.Before(since) {
				c := t.Metrics["cost_usd"]
				s.all += c
				if run.Profile == profile {
					s.mine += c
				}
			}
		}
	}
	return s, nil
}

// turnRunning reports whether turn n of run id is executing.
func (h *Home) turnRunning(id string, n int) bool {
	run, err := h.LoadRun(id)
	if err != nil || len(run.Turns) != n || run.Turns[n-1].Outcome != "running" {
		return false
	}
	return run.executing()
}

// pruneSpend removes ledger entries two windows old.
func (h *Home) pruneSpend() {
	entries, _ := os.ReadDir(h.spendDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 2*spendWindow {
			os.Remove(filepath.Join(h.spendDir(), e.Name()))
		}
	}
}
