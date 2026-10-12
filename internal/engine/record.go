package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Run is the record of one delegated agent: its profile, working folder,
// harness session and turns. It lives in runs/<id>/ with the prompts,
// answers and raw output of every turn.
type Run struct {
	Format  int          `json:"format"`
	ID      string       `json:"id"`
	Profile string       `json:"profile"`
	Harness string       `json:"harness"`
	Cwd     string       `json:"cwd"`
	Parent  string       `json:"parent,omitempty"` // the harness run whose agent started this one
	Depth   int          `json:"depth,omitempty"`  // nesting below the top-level run: 0 at top
	Kind    string       `json:"kind,omitempty"`   // a label for grouping stats, such as review
	Session string       `json:"session,omitempty"`
	Created time.Time    `json:"created"`
	Turns   []TurnRecord `json:"turns"`

	dir string
}

// TurnRecord is how one turn went.
type TurnRecord struct {
	N        int       `json:"n"`
	Queued   time.Time `json:"queued,omitzero"`
	Started  time.Time `json:"started,omitzero"`
	Ended    time.Time `json:"ended,omitzero"`
	Outcome  string    `json:"outcome"` // queued, running, success, transient, ...
	Attempts int       `json:"attempts"`
	ExitCode int       `json:"exit_code"`
	PID      int       `json:"pid,omitempty"`     // the process executing the turn
	Lineage  []string  `json:"lineage,omitempty"` // gate tickets of this turn and its ancestors
	Error    string    `json:"error,omitempty"`
	Command  []string  `json:"command,omitempty"`
	Model    string    `json:"model,omitempty"`  // the model placeholder's value
	Hint     string    `json:"hint,omitempty"`   // advice for the caller, such as the repeat guard's
	Repeat   string    `json:"repeat,omitempty"` // the fingerprint of the request as it executed
	// Metrics are the definition's metrics, summed over attempts.
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

// finished reports whether the turn has a final outcome.
func (t TurnRecord) finished() bool { return t.Outcome != "queued" && t.Outcome != "running" }

// NewRun creates a run directory with a fresh, time-ordered id.
func (h *Home) NewRun(profile, harness, cwd, parent string) (*Run, error) {
	for range 8 {
		id := time.Now().Format("060102-150405") + "-" + randHex(2)
		dir := filepath.Join(h.Dir, "runs", id)
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r := &Run{Format: formatVersion, ID: id, Profile: profile, Harness: harness, Cwd: cwd, Parent: parent, Created: time.Now().UTC(), dir: dir}
		if err := createStage("record"); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		if err := r.Save(); err != nil {
			os.RemoveAll(dir) // never leave a folder without its record
			return nil, err
		}
		return r, nil
	}
	return nil, errors.New("could not allocate a run id")
}

// idSlack bounds how far a run id's time can be from when the run was
// created: ids are local wall-clock time without a zone, and the zone can
// differ between the writer and a later reader by up to 26 hours.
const idSlack = 26 * time.Hour

// idTime is the creation time a run id spells, read in this zone; it is
// within idSlack of the truth.
func idTime(id string) (time.Time, bool) {
	t, err := time.ParseInLocation("060102-150405", id[:min(len(id), 13)], time.Local)
	return t, err == nil
}

// LoadRun reads a run record.
func (h *Home) LoadRun(id string) (*Run, error) {
	if !validName(id) {
		return nil, fmt.Errorf("run id %q is not valid", id)
	}
	dir := filepath.Join(h.Dir, "runs", id)
	data, err := readShared(filepath.Join(dir, "run.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no run %q; list runs with: harness runs", id)
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	if err := checkFormat("run "+id, r.Format); err != nil {
		return nil, err
	}
	r.dir = dir
	return &r, nil
}

// Runs lists the newest runs first; limit ≤ 0 means all.
func (h *Home) Runs(limit int) ([]*Run, error) {
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	var runs []*Run
	for _, id := range ids {
		if limit > 0 && len(runs) == limit {
			break
		}
		if r, err := h.LoadRun(id); err == nil {
			runs = append(runs, r)
		}
	}
	return runs, nil
}

// updateRun changes a run record under its state lock, after rereading it,
// and saves it if change succeeds.
func (h *Home) updateRun(id string, change func(*Run) error) (*Run, error) {
	run, err := h.LoadRun(id)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFileWait(run.Path("state.lock"), 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	defer unlock()
	if run, err = h.LoadRun(id); err != nil {
		return nil, err
	}
	if err := change(run); err != nil {
		return run, err
	}
	return run, run.Save()
}

// executing reports whether a process holds the run's execution lock.
func (r *Run) executing() bool { return executingAt(r.dir) }

// executingAt is executing for a run folder, without reading its record.
func executingAt(dir string) bool {
	unlock, err := lockFile(filepath.Join(dir, "exec.lock"))
	if err != nil {
		return errors.Is(err, errBusy)
	}
	unlock()
	return false
}

// Save writes run.json.
func (r *Run) Save() error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.Path("run.json"), append(data, '\n'))
}

// Path names a file in the run directory.
func (r *Run) Path(name string) string { return filepath.Join(r.dir, name) }

// Dir is the run directory.
func (r *Run) Dir() string { return r.dir }

// Output returns the saved answer of turn n.
func (r *Run) Output(n int) (string, error) {
	data, err := os.ReadFile(r.Path(fmt.Sprintf("turn-%d.out.md", n)))
	return string(data), err
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUID returns a random version 4 UUID, which every harness that takes a
// caller-chosen session id accepts.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
