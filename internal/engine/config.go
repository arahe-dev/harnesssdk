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

// Run records and config.json are format 2 since v2.0: they hold what
// v1 would misread or drop on saving (a worktree run's folder, the new
// limits), so v1 refuses them. v2 still reads format 1. Files whose meaning
// did not change stay format 1, shared by both.
const (
	runFormat    = 2
	configFormat = 2
)

func checkFormat(what string, v int) error { return checkFormatUpTo(what, v, formatVersion) }

func checkFormatUpTo(what string, v, max int) error {
	if v > max {
		return fmt.Errorf("%s was written by a newer harness (format %d; this one reads up to %d): upgrade harness", what, v, max)
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
	// RepeatWarn and RepeatRefuse are how many times in a row the same
	// request may fail before the caller is warned, and before it is
	// refused; 0 turns either off.
	RepeatWarn   int `json:"repeat_warn"`
	RepeatRefuse int `json:"repeat_refuse"`
	// MaxWaitMinutes bounds how long a turn waits at the gate unless its
	// profile sets max_wait; 0 means as long as it takes.
	MaxWaitMinutes int `json:"max_wait_minutes"`
	// MaxQueue is how many turns may wait for one budget, key or slot; a
	// turn that would wait behind more fails at once as throttled. 0
	// means no limit.
	MaxQueue int `json:"max_queue"`
	// SpendLimitUSD caps what turns on this machine cost over 24 hours, in
	// whole dollars, as the CLIs report it; 0 means no limit.
	SpendLimitUSD int `json:"spend_limit_usd"`
}

// ConfigKeys are the settable keys, in display order.
var ConfigKeys = []string{"max_depth", "max_running", "retention_days", "repeat_warn", "repeat_refuse", "max_wait_minutes", "max_queue", "spend_limit_usd"}

// DefaultConfig is used for any key config.json does not set.
func DefaultConfig() Config {
	return Config{Format: configFormat, MaxDepth: 3, MaxRunning: 16, RetentionDays: 30, RepeatWarn: 3, RepeatRefuse: 5}
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
	if err := checkFormatUpTo("config.json", c.Format, configFormat); err != nil {
		return c, err
	}
	if c.MaxDepth < 0 || c.MaxRunning < 0 || c.RetentionDays < 0 || c.RepeatWarn < 0 || c.RepeatRefuse < 0 || c.MaxWaitMinutes < 0 || c.MaxQueue < 0 || c.SpendLimitUSD < 0 {
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
	case "repeat_warn":
		c.RepeatWarn = value
	case "repeat_refuse":
		c.RepeatRefuse = value
	case "max_wait_minutes":
		c.MaxWaitMinutes = value
	case "max_queue":
		c.MaxQueue = value
	case "spend_limit_usd":
		c.SpendLimitUSD = value
	}
	c.Format = configFormat
	data, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(h.configPath(), append(data, '\n'))
}
