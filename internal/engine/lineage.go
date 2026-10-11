package engine

import (
	"os"
	"time"
)

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

// ancestorRun returns the running turn's run whose agent is an ancestor of
// this process. Each turn's agent process is its own, so turns executed side
// by side by one harness process are told apart. Only a process executing a
// single turn identifies a run by itself. A turn's agent is recorded a
// moment after it starts, so when an ancestor executes several turns and
// none of their agents is ours yet, the lookup is retried briefly; with no
// executing ancestor, the answer is immediate.
func (h *Home) ancestorRun() string {
	markers := h.activeMarkers()
	if len(markers) == 0 {
		return ""
	}
	up := ancestors(os.Getpid(), 64)
	for try := 0; ; try++ {
		agents := map[int]activeMarker{}
		executors := map[int][]string{}
		for _, m := range markers {
			if m.AgentPID > 0 {
				agents[m.AgentPID] = m
			}
			executors[m.PID] = append(executors[m.PID], m.Run)
		}
		// This process may itself be a turn's agent (a definition can run
		// harness directly); it is never the executor of its own agent.
		for _, pid := range append([]int{os.Getpid()}, up...) {
			m, ok := agents[pid]
			// A matching creation time proves the id was not reused.
			if ok && (m.AgentStart == 0 || processStart(pid) == m.AgentStart) {
				return m.Run
			}
		}
		pending := false
		for _, pid := range up {
			switch runs := executors[pid]; {
			case len(runs) == 1:
				return runs[0]
			case len(runs) > 1:
				pending = true
			}
		}
		if !pending || try == 2 {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
		markers = h.activeMarkers()
	}
}

// lineageOf is the lineage recorded by a run's running turn.
func (h *Home) lineageOf(id string) []string {
	r, err := h.LoadRun(id)
	if err != nil || len(r.Turns) == 0 {
		return nil
	}
	return r.Turns[len(r.Turns)-1].Lineage
}
