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
//	gate/<name>/            machine-wide ledgers for budgets and keys
//	trust.json              trusted project files
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

func (h *Home) budgetsPath() string { return filepath.Join(h.Dir, "budgets.json") }

var kindExt = map[string]string{"harnesses": ".json", "profiles": ".md"}

// Item is a stored definition or profile and where it came from.
type Item struct {
	Name  string
	Scope string // project or global
	Path  string
}

// locate finds a definition or profile for a run in cwd: a project file
// first, which must be trusted, then the global one.
func (h *Home) locate(kind, name, cwd string) (Item, []byte, error) {
	file := name + kindExt[kind]
	if proj, ok := h.projectDir(cwd); ok {
		path := filepath.Join(proj, kind, file)
		data, err := readShared(path)
		if err == nil {
			if err := h.checkTrust(proj, kind+"/"+file, data); err != nil {
				return Item{}, nil, err
			}
			return Item{Name: name, Scope: "project", Path: path}, data, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Item{}, nil, err
		}
	}
	path := filepath.Join(h.Dir, kind, file)
	data, err := readShared(path)
	if err != nil {
		return Item{}, nil, err
	}
	return Item{Name: name, Scope: "global", Path: path}, data, nil
}

// Definition loads the harness definition a run in cwd would use.
func (h *Home) Definition(name, cwd string) (*Definition, error) {
	if !validName(name) {
		return nil, fmt.Errorf("harness name %q is not valid", name)
	}
	it, data, err := h.locate("harnesses", name, cwd)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no harness %q; define it with: harness def set %s --file <path|->", name, name)
	}
	if err != nil {
		return nil, err
	}
	d, err := ParseDefinition(name, data)
	if err != nil {
		return nil, err
	}
	d.Scope, d.Path = it.Scope, it.Path
	return d, nil
}

// SaveDefinition validates and stores a definition, globally or in a
// project's .harness folder (which also trusts the new content).
func (h *Home) SaveDefinition(name string, data []byte, projectDir string) (*Definition, error) {
	d, err := ParseDefinition(name, data)
	if err != nil {
		return nil, err
	}
	return d, h.store("harnesses", name, data, projectDir)
}

// Profile loads the profile a run in cwd would use.
func (h *Home) Profile(name, cwd string) (*Profile, error) {
	if !validName(name) {
		return nil, fmt.Errorf("profile name %q is not valid", name)
	}
	it, data, err := h.locate("profiles", name, cwd)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no profile %q; create it with: harness profile set %s --file <path|->", name, name)
	}
	if err != nil {
		return nil, err
	}
	p, err := ParseProfile(name, data)
	if err != nil {
		return nil, err
	}
	p.Scope, p.Path = it.Scope, it.Path
	return p, nil
}

// SaveProfile stores a profile after checking it against the definition a
// run in cwd would use and the budgets: the Herta policy is built here, so
// an unsafe contract is rejected when the profile is saved, not when it
// runs.
func (h *Home) SaveProfile(p *Profile, projectDir, cwd string) error {
	if _, err := h.Definition(p.Harness, cwd); err != nil {
		return err
	}
	budgets, err := h.Budgets()
	if err != nil {
		return err
	}
	if err := checkPolicy(p, budgets); err != nil {
		return err
	}
	return h.store("profiles", p.Name, p.Bytes(), projectDir)
}

func (h *Home) store(kind, name string, data []byte, projectDir string) error {
	file := name + kindExt[kind]
	if projectDir == "" {
		return writeFileAtomic(filepath.Join(h.Dir, kind, file), data)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, kind), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(projectDir, kind, file), data); err != nil {
		return err
	}
	return h.trustFile(projectDir, kind+"/"+file, data)
}

// Budgets loads the named capacities; a missing file means none.
func (h *Home) Budgets() (map[string]int64, error) {
	data, err := readShared(h.budgetsPath())
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

// List returns the definitions or profiles a run in cwd can see: the
// project's, then the global ones they do not shadow.
func (h *Home) List(kind, cwd string) ([]Item, error) {
	var items []Item
	seen := map[string]bool{}
	scan := func(dir, scope string) error {
		entries, err := os.ReadDir(filepath.Join(dir, kind))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			n, ok := strings.CutSuffix(e.Name(), kindExt[kind])
			if !ok || e.IsDir() || seen[n] {
				continue
			}
			seen[n] = true
			items = append(items, Item{Name: n, Scope: scope, Path: filepath.Join(dir, kind, e.Name())})
		}
		return nil
	}
	if proj, ok := h.projectDir(cwd); ok {
		if err := scan(proj, "project"); err != nil {
			return nil, err
		}
	}
	if err := scan(h.Dir, "global"); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// Remove deletes a definition or profile, globally or from a project.
func (h *Home) Remove(kind, name, projectDir string) error {
	ext, ok := kindExt[kind]
	if !ok || !validName(name) {
		return fmt.Errorf("cannot remove %s %q", kind, name)
	}
	dir := h.Dir
	if projectDir != "" {
		dir = projectDir
	}
	return os.Remove(filepath.Join(dir, kind, name+ext))
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
	if err := renameShared(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
