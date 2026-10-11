package harnesssdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Home is the data directory: $HARNESS_HOME, or ~/.harness.
//
//	harnesses/<name>.json   harness definitions
//	profiles/<name>.md      profiles
//	budgets.json            named capacities, {"pi": 2}
//	runs/<id>/              one directory per run
type Home struct {
	Dir string
}

// OpenHome finds and creates the data directory.
func OpenHome() (*Home, error) {
	dir := os.Getenv("HARNESS_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".harness")
	}
	return NewHome(dir)
}

// NewHome uses dir as the data directory, creating it if needed. The path
// is made absolute, because file placeholders are handed to CLIs that run in
// other folders. Directories are private: runs hold prompts and answers.
func NewHome(dir string) (*Home, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"harnesses", "profiles", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	// MkdirAll leaves an existing folder's mode alone; tighten it, since a
	// private top folder keeps everything under it private.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Home{Dir: dir}, nil
}

func (h *Home) defPath(name string) string {
	return filepath.Join(h.Dir, "harnesses", name+".json")
}

func (h *Home) profilePath(name string) string {
	return filepath.Join(h.Dir, "profiles", name+".md")
}

func (h *Home) budgetsPath() string { return filepath.Join(h.Dir, "budgets.json") }

// Definition loads a harness definition.
func (h *Home) Definition(name string) (*Definition, error) {
	if !validName(name) {
		return nil, fmt.Errorf("harness name %q is not valid", name)
	}
	data, err := os.ReadFile(h.defPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no harness %q; define it with: harness def set %s --file <path|->", name, name)
	}
	if err != nil {
		return nil, err
	}
	return ParseDefinition(name, data)
}

// SaveDefinition validates and stores a definition.
func (h *Home) SaveDefinition(name string, data []byte) (*Definition, error) {
	d, err := ParseDefinition(name, data)
	if err != nil {
		return nil, err
	}
	return d, writeFileAtomic(h.defPath(name), data)
}

// Profile loads a profile.
func (h *Home) Profile(name string) (*Profile, error) {
	if !validName(name) {
		return nil, fmt.Errorf("profile name %q is not valid", name)
	}
	data, err := os.ReadFile(h.profilePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no profile %q; create it with: harness profile set %s --file <path|->", name, name)
	}
	if err != nil {
		return nil, err
	}
	return ParseProfile(name, data)
}

// SaveProfile stores a profile after checking it against its harness and
// the budgets: the Herta policy is built here, so an unsafe contract is
// rejected when the profile is saved, not when it runs.
func (h *Home) SaveProfile(p *Profile) error {
	if _, err := h.Definition(p.Harness); err != nil {
		return err
	}
	budgets, err := h.Budgets()
	if err != nil {
		return err
	}
	if err := checkPolicy(p, budgets); err != nil {
		return err
	}
	return writeFileAtomic(h.profilePath(p.Name), p.Bytes())
}

// Budgets loads the named capacities; a missing file means none.
func (h *Home) Budgets() (map[string]int64, error) {
	data, err := os.ReadFile(h.budgetsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	b := map[string]int64{}
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("budgets.json: %w", err)
	}
	for name, c := range b {
		if !validName(name) || c < 1 {
			return nil, fmt.Errorf("budgets.json: %q needs a valid name and a capacity ≥ 1", name)
		}
	}
	return b, nil
}

// SetBudget sets one capacity; capacity 0 removes it.
func (h *Home) SetBudget(name string, capacity int64) error {
	if !validName(name) {
		return fmt.Errorf("budget name %q is not valid", name)
	}
	if capacity < 0 {
		return fmt.Errorf("capacity must be ≥ 0")
	}
	// Read, change and replace under one lock, so two concurrent updates
	// cannot drop each other's change.
	unlock, err := lockFileWait(filepath.Join(h.Dir, "budgets.lock"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("budgets are being changed by another process: %w", err)
	}
	defer unlock()
	b, err := h.Budgets()
	if err != nil {
		return err
	}
	if capacity == 0 {
		delete(b, name)
	} else {
		b[name] = capacity
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	return writeFileAtomic(h.budgetsPath(), append(data, '\n'))
}

// List returns the names stored in a kind's directory ("harnesses" or
// "profiles").
func (h *Home) List(kind string) ([]string, error) {
	ext := map[string]string{"harnesses": ".json", "profiles": ".md"}[kind]
	entries, err := os.ReadDir(filepath.Join(h.Dir, kind))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ext); ok && !e.IsDir() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Remove deletes a stored definition or profile.
func (h *Home) Remove(kind, name string) error {
	path := map[string]string{"harnesses": h.defPath(name), "profiles": h.profilePath(name)}[kind]
	if path == "" || !validName(name) {
		return fmt.Errorf("cannot remove %s %q", kind, name)
	}
	return os.Remove(path)
}

// writeFileAtomic writes through a temporary file and a rename, so a
// reader never sees half a file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
