package engine

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Which harness and model suits a task depends on the model, the kind of
// task and the budget, so the choice is best made from evidence: Stats
// sums up how each profile's turns ended and what they cost, from the run
// records.

// StatsFilter selects the turns Stats counts.
type StatsFilter struct {
	Profile string
	Kind    string
	Since   time.Time // runs started from then on
}

// Stats is how one profile did on one model, for one kind of run.
type Stats struct {
	Profile  string                 `json:"profile"`
	Model    string                 `json:"model,omitempty"`
	Kind     string                 `json:"kind,omitempty"`
	Turns    int                    `json:"turns"`
	Outcomes map[string]int         `json:"outcomes"`
	Attempts int                    `json:"attempts"`
	Median   time.Duration          `json:"median_ns"`
	P90      time.Duration          `json:"p90_ns"`
	Metrics  map[string]MetricStats `json:"metrics,omitempty"`
}

// MetricStats is a metric's total over the turns that reported it.
type MetricStats struct {
	Sum   float64 `json:"sum"`
	Turns int     `json:"turns"`
}

// Mean is the metric per reporting turn.
func (m MetricStats) Mean() float64 {
	if m.Turns == 0 {
		return 0
	}
	return m.Sum / float64(m.Turns)
}

// Stats groups the finished turns of matching runs by profile, model and
// kind, most used first.
func (h *Home) Stats(f StatsFilter) ([]Stats, error) {
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return nil, err
	}
	groups := map[[3]string]*Stats{}
	durations := map[[3]string][]time.Duration{}
	for _, e := range entries {
		id := e.Name()
		// Ids start with their creation time: runs clearly older are not
		// read; the record's own time decides the rest.
		if created, ok := idTime(id); !e.IsDir() || !ok || created.Add(idSlack).Before(f.Since) {
			continue
		}
		run, err := h.LoadRun(id)
		if err != nil || run.Created.Before(f.Since) || f.Profile != "" && run.Profile != f.Profile || f.Kind != "" && run.Kind != f.Kind {
			continue
		}
		for _, t := range run.Turns {
			if !t.finished() || t.Ended.Before(f.Since) {
				continue
			}
			key := [3]string{run.Profile, t.Model, run.Kind}
			g := groups[key]
			if g == nil {
				g = &Stats{Profile: run.Profile, Model: t.Model, Kind: run.Kind, Outcomes: map[string]int{}}
				groups[key] = g
			}
			g.Turns++
			g.Outcomes[t.Outcome]++
			g.Attempts += t.Attempts
			if !t.Started.IsZero() {
				durations[key] = append(durations[key], t.Ended.Sub(t.Started))
			}
			for name, v := range t.Metrics {
				if g.Metrics == nil {
					g.Metrics = map[string]MetricStats{}
				}
				m := g.Metrics[name]
				m.Sum, m.Turns = m.Sum+v, m.Turns+1
				g.Metrics[name] = m
			}
		}
	}
	out := make([]Stats, 0, len(groups))
	for key, g := range groups {
		if ds := durations[key]; len(ds) > 0 {
			slices.Sort(ds)
			g.Median, g.P90 = ds[len(ds)/2], ds[min(len(ds)-1, len(ds)*9/10)]
		}
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b Stats) int {
		return cmp.Or(cmp.Compare(b.Turns, a.Turns), cmp.Compare(a.Profile, b.Profile), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Kind, b.Kind))
	})
	return out, nil
}
