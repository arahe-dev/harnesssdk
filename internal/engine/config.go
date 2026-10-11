package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"time"
)

// formatVersion is the version of every file harness writes for itself:
// run records, budgets, trust, gate ledgers, the active index and config.
// A reader refuses a newer version rather than misread it; files written
// before versions existed read as version 1.
const formatVersion = 1

func checkFormat(what string, v int) error {
	if v > formatVersion {
		return fmt.Errorf("%s was written by a newer harness (format %d; this one reads up to %d): upgrade harness", what, v, formatVersion)
	}
	return nil
}

// readVersioned reads a file written by writeVersioned, {"format": N,
// key: payload}, or, as written before versions existed, the payload
// alone. A missing file leaves payload as it is.
func readVersioned(path, key string, payload any) error {
	data, err := readShared(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var v int
	inner, hasKey := top[key]
	if raw, ok := top["format"]; ok && hasKey && json.Unmarshal(raw, &v) == nil && len(inner) > 0 && inner[0] == '{' {
		if err := checkFormat(name, v); err != nil {
			return err
		}
		data = inner
	}
	if err := json.Unmarshal(data, payload); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// writeVersioned writes {"format": formatVersion, key: payload}.
func writeVersioned(path, key string, payload any) error {
	data, err := json.MarshalIndent(map[string]any{"format": formatVersion, key: payload}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// Config is <home>/config.json: limits for every run on this machine.
type Config struct {
	Format int `json:"format"`
	// MaxDepth is how deep runs may nest: a run started by an agent of
	// another run is one level deeper than it. 0 allows no nesting.
	MaxDepth int `json:"max_depth"`
	// MaxRunning caps the turns executing at once on this machine, across
	// every profile; 0 means no cap. A top-level turn waits for a slot; a
	// nested one fails as throttled instead, as its ancestors hold slots.
	MaxRunning int `json:"max_running"`
	// RetentionDays is how long finished runs are kept; 0 keeps them.
	RetentionDays int `json:"retention_days"`
}

// ConfigKeys are the settable keys, in display order.
var ConfigKeys = []string{"max_depth", "max_running", "retention_days"}

// DefaultConfig is used for any key config.json does not set.
func DefaultConfig() Config {
	return Config{Format: formatVersion, MaxDepth: 3, MaxRunning: 16, RetentionDays: 30}
}

func (h *Home) configPath() string { return filepath.Join(h.Dir, "config.json") }

// Config reads the limits, defaulting every key the file does not set.
func (h *Home) Config() (Config, error) {
	c := DefaultConfig()
	data, err := readShared(h.configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if err := checkFormat("config.json", c.Format); err != nil {
		return c, err
	}
	if c.MaxDepth < 0 || c.MaxRunning < 0 || c.RetentionDays < 0 {
		return c, errors.New("config.json: limits must be ≥ 0")
	}
	return c, nil
}

// SetConfig sets one key.
func (h *Home) SetConfig(key string, value int) error {
	if !slices.Contains(ConfigKeys, key) {
		return fmt.Errorf("unknown config key %q (%v)", key, ConfigKeys)
	}
	if value < 0 {
		return fmt.Errorf("%s must be ≥ 0", key)
	}
	lk, err := lockWaitOpen(filepath.Join(h.Dir, "config.lock"), 5*time.Second)
	if err != nil {
		return err
	}
	defer lk.Close()
	c, err := h.Config()
	if err != nil {
		return err
	}
	switch key {
	case "max_depth":
		c.MaxDepth = value
	case "max_running":
		c.MaxRunning = value
	case "retention_days":
		c.RetentionDays = value
	}
	c.Format = formatVersion
	data, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(h.configPath(), append(data, '\n'))
}
