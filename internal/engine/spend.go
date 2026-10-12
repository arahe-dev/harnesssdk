package engine

import (
	"encoding/json"
	"errors"
	"fmt"
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

const spendWindow = 24 * time.Hour

// checkSpend refuses a turn of p as throttled once p, or the machine, has
// spent its limit.
func (h *Home) checkSpend(p *Profile) error {
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if cfg.SpendLimitUSD == 0 && p.SpendLimit == 0 {
		return nil
	}
	all, mine, err := h.spending(now().Add(-spendWindow), p.Name)
	if err != nil {
		return err
	}
	switch {
	case cfg.SpendLimitUSD > 0 && all >= float64(cfg.SpendLimitUSD):
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"turns on this machine have spent $%.2f in the last 24 hours, and spend_limit_usd is $%d; raise it with: harness config set spend_limit_usd <dollars>", all, cfg.SpendLimitUSD))
	case p.SpendLimit > 0 && mine >= p.SpendLimit:
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"profile %s has spent $%.2f in the last 24 hours, and its spend_limit_usd is $%.2f", p.Name, mine, p.SpendLimit))
	}
	return nil
}

// spending sums what attempts cost since then: by every profile, and by
// profile. Each attempt counts by when it spent, not when its run was
// created or its turn ended, so a follow-up today on yesterday's run
// counts, a turn still running counts its finished attempts, and an
// attempt from before the window does not count because a later one is in
// it. Only run folders changed since then are read, and the spending
// recorded apart (see recordApart).
func (h *Home) spending(since time.Time, profile string) (all, mine float64, err error) {
	apart, err := os.ReadDir(h.spendDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, 0, err
	}
	for _, e := range apart {
		if info, err := e.Info(); err != nil || info.ModTime().Before(since) {
			continue
		}
		var a spendApart
		data, err := readShared(filepath.Join(h.spendDir(), e.Name()))
		if err != nil || json.Unmarshal(data, &a) != nil || a.At.Before(since) {
			continue
		}
		all += a.USD
		if a.Profile == profile {
			mine += a.USD
		}
	}
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), stagePrefix) {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().Before(since) {
			continue
		}
		run, err := h.LoadRun(e.Name())
		if err != nil {
			continue
		}
		for _, t := range run.Turns {
			var c float64
			if len(t.Spend) > 0 {
				for _, s := range t.Spend {
					if !s.At.Before(since) {
						c += s.USD
					}
				}
			} else if t.SpentApart == 0 && !t.Ended.Before(since) {
				c = t.Metrics["cost_usd"] // recorded before v2: by when it ended
			}
			all += c
			if run.Profile == profile {
				mine += c
			}
		}
	}
	return all, mine, nil
}

// A cost that cannot go into its run record (the record's lock is held too
// long, or the record cannot be replaced) is recorded apart, in a file of
// its own in <home>/spend, which needs no lock: it counts all the same.
// Such files go once they are past the window.

// spendApart is one cost recorded apart from its run.
type spendApart struct {
	Run     string    `json:"run"`
	Turn    int       `json:"turn"`
	Profile string    `json:"profile"`
	At      time.Time `json:"at"`
	USD     float64   `json:"usd"`
}

func (h *Home) spendDir() string { return filepath.Join(h.Dir, "spend") }

// recordApart records the turn's cost not yet on record, if any, apart
// from its run record.
func (h *Home) recordApart(t *Turn) error {
	if testHookSaveApart != nil {
		if err := testHookSaveApart(); err != nil {
			return err
		}
	}
	cost := finiteMetrics(t.metrics)["cost_usd"]
	d := cost - t.spendSaved
	if d == 0 {
		return nil
	}
	data, err := json.Marshal(spendApart{Run: t.Run.ID, Turn: t.N, Profile: t.Profile.Name, At: now().UTC(), USD: d})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.spendDir(), 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%d-%s.json", t.Run.ID, t.N, randHex(4))
	if err := writeFileAtomic(filepath.Join(h.spendDir(), name), append(data, '\n')); err != nil {
		return err
	}
	t.spendSaved, t.apart = cost, t.apart+d
	return nil
}

// pruneSpend removes costs recorded apart that are past the window.
func (h *Home) pruneSpend() {
	entries, _ := os.ReadDir(h.spendDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 2*spendWindow {
			os.Remove(filepath.Join(h.spendDir(), e.Name()))
		}
	}
}
