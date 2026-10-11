package harnesssdk

import "os"

// A run started by an agent that harness launched is nested: it knows its
// parent run and the gate tickets held by its ancestors (its lineage), so
// it never deadlocks on them and records where it came from. harness hands
// both to every agent in HARNESS_RUN and HARNESS_LINEAGE. Some harnesses
// strip the environment of the commands their agents run (Codex with
// shell_environment_policy.inherit = "core", for instance), so when the
// variables are missing the nested run finds its parent itself: it walks up
// its own process ancestors to the harness process executing a running
// turn, and reads that turn's recorded lineage.

// Parentage is how a harness process is related to the runs above it.
type Parentage struct {
	Parent  string   // the run whose agent started this process, if any
	Lineage []string // gate tickets held by that run and its ancestors
	Found   string   // "env", "ancestry" or "" (not nested)
}

// DiscoverParentage resolves this process's parent run and lineage: from
// the environment if harness's variables survived, otherwise from process
// ancestry. A known parent without a lineage takes its recorded one.
func (h *Home) DiscoverParentage() Parentage {
	p := Parentage{Parent: os.Getenv(EnvRun), Lineage: splitList(os.Getenv(EnvLineage))}
	if p.Parent != "" {
		p.Found = "env"
	} else if id := h.ancestorRun(); id != "" {
		p.Parent, p.Found = id, "ancestry"
	}
	if p.Parent != "" && len(p.Lineage) == 0 {
		p.Lineage = h.lineageOf(p.Parent)
	}
	return p
}

// ancestorRun returns the running run whose executing process is an
// ancestor of this one.
func (h *Home) ancestorRun() string {
	up := ancestors(os.Getpid(), 64)
	if len(up) == 0 {
		return ""
	}
	runs, err := h.Runs(500)
	if err != nil {
		return ""
	}
	byPID := map[int]*Run{}
	for _, r := range runs {
		if n := len(r.Turns); n > 0 && r.Turns[n-1].Outcome == "running" && r.Turns[n-1].PID > 0 {
			byPID[r.Turns[n-1].PID] = r
		}
	}
	for _, pid := range up {
		// A recorded pid could have been reused; a running turn whose
		// execution lock is held still has its process.
		if r, ok := byPID[pid]; ok && r.executing() {
			return r.ID
		}
	}
	return ""
}

// lineageOf is the lineage recorded by a run's running turn.
func (h *Home) lineageOf(id string) []string {
	r, err := h.LoadRun(id)
	if err != nil || len(r.Turns) == 0 {
		return nil
	}
	return r.Turns[len(r.Turns)-1].Lineage
}
