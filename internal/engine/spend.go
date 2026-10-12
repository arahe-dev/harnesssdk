package engine

import (
	"fmt"
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
	st, err := h.Stats(StatsFilter{Since: now().Add(-spendWindow)})
	if err != nil {
		return err
	}
	var all, mine float64
	for _, s := range st {
		c := s.Metrics["cost_usd"].Sum
		all += c
		if s.Profile == p.Name {
			mine += c
		}
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
