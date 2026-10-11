package harnesssdk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// A project keeps its own definitions and profiles in a .harness folder at
// or above the working folder; they take precedence over the global ones.
// Because a definition is a command line and a profile picks tools and
// flags, a project file is used only after it has been trusted: its SHA-256
// is recorded in <home>/trust.json, and any change needs trusting again. A
// file that harness itself writes into a project is trusted as it is saved.

// projectDir returns the .harness folder of the project containing cwd, if
// any. A data folder (this one, or any with a runs folder, such as the
// default ~/.harness when HARNESS_HOME points elsewhere) is never a project.
func (h *Home) projectDir(cwd string) (string, bool) {
	if cwd == "" {
		return "", false
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	for {
		cand := filepath.Join(dir, ".harness")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() && !samePath(cand, h.Dir) && !isDataFolder(cand) {
			return cand, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// FindProject returns the .harness folder of the project containing cwd.
func (h *Home) FindProject(cwd string) (string, bool) { return h.projectDir(cwd) }

// ProjectDir is where "set --project" writes for cwd: the existing project
// folder, or a new .harness folder in cwd.
func (h *Home) ProjectDir(cwd string) (string, error) {
	if dir, ok := h.projectDir(cwd); ok {
		return dir, nil
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, ".harness"), nil
}

// isDataFolder reports whether dir is a harness data folder: only those
// hold runs.
func isDataFolder(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "runs"))
	return err == nil && fi.IsDir()
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func trustKey(projectDir string) string {
	k := filepath.Clean(projectDir)
	if runtime.GOOS == "windows" {
		k = strings.ToLower(k)
	}
	return k
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type trustStore map[string]map[string]string // project dir → relative path → sha256

func (h *Home) trustPath() string { return filepath.Join(h.Dir, "trust.json") }

func (h *Home) readTrust() (trustStore, error) {
	data, err := readShared(h.trustPath())
	if errors.Is(err, fs.ErrNotExist) {
		return trustStore{}, nil
	}
	if err != nil {
		return nil, err
	}
	t := trustStore{}
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("trust.json: %w", err)
	}
	return t, nil
}

// updateTrust changes the trust store under a lock.
func (h *Home) updateTrust(change func(trustStore)) error {
	unlock, err := lockFileWait(filepath.Join(h.Dir, "trust.lock"), 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	t, err := h.readTrust()
	if err != nil {
		return err
	}
	change(t)
	data, _ := json.MarshalIndent(t, "", "  ")
	return writeFileAtomic(h.trustPath(), append(data, '\n'))
}

// checkTrust refuses a project file whose content has not been trusted.
func (h *Home) checkTrust(projectDir, rel string, data []byte) error {
	t, err := h.readTrust()
	if err != nil {
		return err
	}
	switch got := t[trustKey(projectDir)][filepath.ToSlash(rel)]; got {
	case digest(data):
		return nil
	case "":
		return fmt.Errorf("project file %s is not trusted; review it, then run: harness trust --approve --cwd %s",
			filepath.Join(projectDir, rel), filepath.Dir(projectDir))
	default:
		return fmt.Errorf("project file %s changed since it was trusted; review it, then run: harness trust --approve --cwd %s",
			filepath.Join(projectDir, rel), filepath.Dir(projectDir))
	}
}

func (h *Home) trustFile(projectDir, rel string, data []byte) error {
	return h.updateTrust(func(t trustStore) {
		k := trustKey(projectDir)
		if t[k] == nil {
			t[k] = map[string]string{}
		}
		t[k][filepath.ToSlash(rel)] = digest(data)
	})
}

// TrustEntry is one project file and whether its current content is trusted.
type TrustEntry struct {
	Path   string // relative to the project's .harness folder
	State  string // trusted, new, changed
	Digest string
}

// TrustStatus lists the project's definitions and profiles.
func (h *Home) TrustStatus(projectDir string) ([]TrustEntry, error) {
	t, err := h.readTrust()
	if err != nil {
		return nil, err
	}
	known := t[trustKey(projectDir)]
	var out []TrustEntry
	for _, kind := range []string{"harnesses", "profiles"} {
		entries, err := os.ReadDir(filepath.Join(projectDir, kind))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !(strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".md")) {
				continue
			}
			rel := kind + "/" + e.Name()
			data, err := os.ReadFile(filepath.Join(projectDir, kind, e.Name()))
			if err != nil {
				return nil, err
			}
			d, state := digest(data), "trusted"
			switch known[rel] {
			case d:
			case "":
				state = "new"
			default:
				state = "changed"
			}
			out = append(out, TrustEntry{Path: rel, State: state, Digest: d})
		}
	}
	slices.SortFunc(out, func(a, b TrustEntry) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// ApproveProject trusts the current content of every project file.
func (h *Home) ApproveProject(projectDir string) ([]TrustEntry, error) {
	entries, err := h.TrustStatus(projectDir)
	if err != nil {
		return nil, err
	}
	err = h.updateTrust(func(t trustStore) {
		k := trustKey(projectDir)
		t[k] = map[string]string{}
		for _, e := range entries {
			t[k][e.Path] = e.Digest
		}
	})
	return entries, err
}
