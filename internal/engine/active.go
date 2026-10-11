package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The active index, <home>/active/<run-id>.json, lists turns that are
// running: the process executing each, and the root process of its current
// agent (the CLI, or its guard on Unix). Nested-run discovery reads only
// this index, however many runs the data folder holds, and matches a
// turn's own agent, which no other turn shares; several turns executed by
// one harness process share that process.

type activeMarker struct {
	Format     int    `json:"format"`
	Run        string `json:"run"`
	Turn       int    `json:"turn"`
	PID        int    `json:"pid"` // the process executing the turn
	AgentPID   int    `json:"agent_pid,omitempty"`
	AgentStart int64  `json:"agent_start,omitempty"` // creation time, where the OS reports one
}

func (h *Home) activeDir() string { return filepath.Join(h.Dir, "active") }

func (h *Home) activePath(run string) string {
	return filepath.Join(h.activeDir(), run+".json")
}

func (h *Home) writeActive(m activeMarker) error {
	if err := os.MkdirAll(h.activeDir(), 0o700); err != nil {
		return err
	}
	m.Format = formatVersion
	data, _ := json.Marshal(m)
	return writeFileAtomic(h.activePath(m.Run), data)
}

// clearActive removes a run's marker.
func (h *Home) clearActive(run string) { os.Remove(h.activePath(run)) }

// activeMarkers lists the markers of turns still executing, removing those
// whose process is gone.
func (h *Home) activeMarkers() []activeMarker {
	entries, err := os.ReadDir(h.activeDir())
	if err != nil {
		return nil
	}
	var out []activeMarker
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		data, err := readShared(filepath.Join(h.activeDir(), e.Name()))
		if err != nil {
			continue
		}
		var m activeMarker
		if json.Unmarshal(data, &m) != nil || checkFormat("active marker", m.Format) != nil {
			continue
		}
		if !validName(id) || !executingAt(filepath.Join(h.Dir, "runs", id)) {
			h.clearActive(id)
			continue
		}
		out = append(out, m)
	}
	return out
}
