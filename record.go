package harnesssdk

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
	ID      string       `json:"id"`
	Profile string       `json:"profile"`
	Harness string       `json:"harness"`
	Cwd     string       `json:"cwd"`
	Session string       `json:"session,omitempty"`
	Created time.Time    `json:"created"`
	Turns   []TurnRecord `json:"turns"`

	dir string
}

// TurnRecord is how one turn went.
type TurnRecord struct {
	N        int       `json:"n"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Outcome  string    `json:"outcome"` // running, success, transient, ...
	Attempts int       `json:"attempts"`
	ExitCode int       `json:"exit_code"`
	Error    string    `json:"error,omitempty"`
	Command  []string  `json:"command,omitempty"`
}

// NewRun creates a run directory with a fresh, time-ordered id.
func (h *Home) NewRun(profile, harness, cwd string) (*Run, error) {
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
		r := &Run{ID: id, Profile: profile, Harness: harness, Cwd: cwd, Created: time.Now().UTC(), dir: dir}
		return r, r.Save()
	}
	return nil, errors.New("could not allocate a run id")
}

// LoadRun reads a run record.
func (h *Home) LoadRun(id string) (*Run, error) {
	if !validName(id) {
		return nil, fmt.Errorf("run id %q is not valid", id)
	}
	dir := filepath.Join(h.Dir, "runs", id)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
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
