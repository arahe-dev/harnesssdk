package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Agents that start agents can spend without anyone watching. Spending
// limits cap what turns cost over the last 24 hours, from the cost_usd
// metric the CLIs report: config spend_limit_usd for the whole machine,
// and a profile's spend_limit_usd for that profile. They are checked when
// runs are created and again before every attempt, so a fan-out stops once
// the limit is reached; turns already running finish. A CLI that reports
// no cost (Codex) is not limited by them.
//
// What turns spent is kept in a ledger, <home>/spend, with one entry per
// attempt, <run>-<turn>-<attempt>-<random>.json, written whole (the random
// part keeps a run id used again, after its run is gone, from writing over
// the entries of the first). The entry is made,
// "running", before the attempt runs, and rewritten with what the attempt
// cost before the attempt's admission is released; a turn under a limit
// whose entry cannot be made does not run, and one whose cost cannot be
// written stays admitted, trying again, until it is written or the turn is
// stopped. Spending sums the entries in the window, by when each attempt
// ended. An entry still running counts nothing while its turn runs; one
// whose turn is over (its process died, or it was stopped before its cost
// could be written) has an unknown cost, and so does an entry that cannot
// be read: turns under a limit it falls under are refused until it leaves
// the window, or someone removes it; an entry is never removed because its
// cost could not be written. Turns recorded before v2 count by their run
// records, until v2 saves the record: their costs then move to the ledger
// first (<run>-<created>-<turn>-v1.json). Entries go once they are two
// windows old.

const spendWindow = 24 * time.Hour

// spendEntry is one attempt's entry in the ledger.
type spendEntry struct {
	Run     string    `json:"run"`
	Turn    int       `json:"turn"`
	Attempt int       `json:"attempt"`
	Profile string    `json:"profile"`
	Created time.Time `json:"created"` // the run's, which tells it from a later run with its id
	At      time.Time `json:"at"`      // when the attempt started, or, once its cost is in, ended
	USD     *float64  `json:"usd,omitempty"`
	Running bool      `json:"running,omitempty"`
	path    string    // where it is written
}

// valid reports whether e is an entry spending can count: one that names
// its run (and when the run was created), turn, attempt (0 for a cost moved
// from a v1 record), profile and time, and, once it is not running, its
// cost: a number of dollars.
func (e *spendEntry) valid() bool {
	cost := e.Running || e.USD != nil && *e.USD >= 0 && !math.IsInf(*e.USD, 0)
	return validName(e.Run) && !e.Created.IsZero() && e.Turn >= 1 && (e.Attempt >= 1 || e.Attempt == 0 && !e.Running) &&
		validName(e.Profile) && !e.At.IsZero() && cost
}

func dollars(usd float64) *float64 { return &usd }

func (h *Home) spendDir() string { return spendDirOf(h.Dir) }

func spendDirOf(home string) string { return filepath.Join(home, "spend") }

// spendPath is where e is written: a new name for a new entry.
func (h *Home) spendPath(e *spendEntry) string {
	if e.path == "" {
		e.path = filepath.Join(h.spendDir(), fmt.Sprintf("%s-%d-%d-%s.json", e.Run, e.Turn, e.Attempt, randHex(8)))
	}
	return e.path
}

// testHookSpend, when set, runs as an entry is opened or closed ("open",
// "close"); an error fails that write.
var testHookSpend func(stage string) error

func (h *Home) writeSpend(stage string, e *spendEntry) error {
	if testHookSpend != nil {
		if err := testHookSpend(stage); err != nil {
			return err
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.spendDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(h.spendPath(e), append(data, '\n'))
}

// openSpend makes the running entry of turn t's next attempt. Entries are
// numbered as they are made, so none is ever written over by another.
func (h *Home) openSpend(t *Turn) error {
	e := &spendEntry{Run: t.Run.ID, Created: t.Run.Created, Turn: t.N, Attempt: t.opened + 1, Profile: t.Profile.Name, At: now().UTC(), Running: true}
	if err := h.writeSpend("open", e); err != nil {
		return err
	}
	t.entry, t.opened = e, t.opened+1
	return nil
}

// closeSpend writes what turn t's attempt cost (since the cost last
// written) into its entry.
func (h *Home) closeSpend(t *Turn) error {
	if t.entry == nil {
		return nil
	}
	cost := finiteMetrics(t.metrics)["cost_usd"]
	e := *t.entry
	e.At, e.USD, e.Running = now().UTC(), dollars(cost-t.spendSaved), false
	if err := h.writeSpend("close", &e); err != nil {
		return err
	}
	t.spendSaved, t.entry = cost, nil
	return nil
}

// limited reports whether a spending limit applies to turns of p.
func limited(cfg Config, p *Profile) bool { return cfg.SpendLimitUSD > 0 || p.SpendLimit > 0 }

// checkSpend refuses a turn of p as throttled once p, or the machine, has
// spent its limit, or may have: while what an attempt under that limit
// cost is unknown.
func (h *Home) checkSpend(p *Profile) error {
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if !limited(cfg, p) {
		return nil
	}
	s, err := h.spending(now().Add(-spendWindow), p.Name)
	if err != nil {
		return err
	}
	unknown := func(what string) error {
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"%s, so turns under a spending limit are refused until it is 24 hours old, or until you remove it", what))
	}
	switch {
	case cfg.SpendLimitUSD > 0 && s.unknownAll != "":
		return unknown(s.unknownAll)
	case cfg.SpendLimitUSD > 0 && s.all >= float64(cfg.SpendLimitUSD):
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"turns on this machine have spent $%.2f in the last 24 hours, and spend_limit_usd is $%d; raise it with: harness config set spend_limit_usd <dollars>", s.all, cfg.SpendLimitUSD))
	case p.SpendLimit > 0 && s.unknownMine != "":
		return unknown(s.unknownMine)
	case p.SpendLimit > 0 && s.mine >= p.SpendLimit:
		return hertasdk.Fail(hertasdk.Throttled, fmt.Errorf(
			"profile %s has spent $%.2f in the last 24 hours, and its spend_limit_usd is $%.2f", p.Name, s.mine, p.SpendLimit))
	}
	return nil
}

// spent is what turns spent in a window: on the machine, and by one
// profile, with the first entry of each whose cost is unknown.
type spent struct {
	all, mine               float64
	unknownAll, unknownMine string
}

// spending sums what attempts cost since then, by when each ended: so a
// follow-up today on yesterday's run counts, a turn still running counts
// its finished attempts, and an attempt from before the window does not
// count because a later one is in it.
func (h *Home) spending(since time.Time, profile string) (s spent, err error) {
	unknown := func(mine bool, what string) {
		if s.unknownAll == "" {
			s.unknownAll = what
		}
		if mine && s.unknownMine == "" {
			s.unknownMine = what
		}
	}
	add := func(p string, usd float64) {
		s.all += usd
		if p == profile {
			s.mine += usd
		}
	}
	// The ledger is read before the v1 records and again after them: a v1
	// cost moved to the ledger while the records were read (the record
	// upgraded or removed since) is counted from the ledger then, and one
	// the record counted (byRecord) is not counted again.
	seen, byRecord := map[string]bool{}, map[string]bool{}
	if err := h.readLedger(since, profile, seen, byRecord, add, unknown); err != nil {
		return s, err
	}
	if testHookSpending != nil {
		testHookSpending()
	}
	// Turns recorded before v2 count by their run records, by when they
	// ended, unless the ledger counted them; only run folders changed since
	// then are read.
	entries, err := os.ReadDir(filepath.Join(h.Dir, "runs"))
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), stagePrefix) {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().Before(since) {
			continue
		}
		run, err := h.LoadRun(e.Name())
		if err != nil || run.Format >= runFormat {
			continue
		}
		for _, t := range run.Turns {
			name := filepath.Base(v1SpendPath(h.Dir, run, t.N))
			if t.Ended.Before(since) || seen[name] {
				continue
			}
			byRecord[name] = true
			add(run.Profile, t.Metrics["cost_usd"])
		}
	}
	return s, h.readLedger(since, profile, seen, byRecord, add, unknown)
}

// testHookSpending, when set, runs between spending's first reading of
// the ledger and its reading of the v1 records.
var testHookSpending func()

// readLedger counts the ledger's entries not seen yet, nor counted by
// their v1 records.
func (h *Home) readLedger(since time.Time, profile string, seen, byRecord map[string]bool, add func(string, float64), unknown func(bool, string)) error {
	files, err := os.ReadDir(h.spendDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, f := range files {
		name := f.Name()
		if seen[name] || byRecord[name] || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue // counted already, or an entry being written
		}
		seen[name] = true
		path := filepath.Join(h.spendDir(), name)
		info, err := f.Info()
		if err == nil && info.ModTime().Before(since) {
			continue // last written before the window
		}
		var e spendEntry
		var data []byte
		if err == nil {
			data, err = readShared(path)
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if err == nil {
			err = json.Unmarshal(data, &e)
		}
		if err == nil && !e.valid() {
			err = errors.New("it is not a valid entry")
		}
		if err != nil {
			unknown(true, fmt.Sprintf("the spending entry %s cannot be read (%v)", path, err))
			continue
		}
		switch {
		case e.At.Before(since):
		case e.Running && h.turnRunning(&e):
			// Its cost is not known yet; turns already running finish.
		case e.Running:
			unknown(e.Profile == profile, fmt.Sprintf("what run %s turn %d attempt %d cost is unknown (it ended before its cost was written to %s)", e.Run, e.Turn, e.Attempt, path))
		default:
			add(e.Profile, *e.USD)
		}
	}
	return nil
}

// turnRunning reports whether the turn of entry e is executing: in the
// run e names, not a later run with its id.
func (h *Home) turnRunning(e *spendEntry) bool {
	run, err := h.LoadRun(e.Run)
	if err != nil || !run.Created.Equal(e.Created) || e.Turn < 1 || len(run.Turns) != e.Turn || run.Turns[e.Turn-1].Outcome != "running" {
		return false
	}
	return run.executing()
}

// v1SpendPath names a v1 turn's cost in the ledger: by its run's id and
// creation time, so a later run with the id has names of its own.
func v1SpendPath(home string, r *Run, turn int) string {
	return filepath.Join(spendDirOf(home), fmt.Sprintf("%s-%d-%d-v1.json", r.ID, r.Created.UnixNano(), turn))
}

// moveV1Spend writes the costs of a v1 record's recent turns into the
// ledger of the data folder home, under names of their own, so writing
// them again changes nothing.
func moveV1Spend(home string, r *Run) error {
	for _, t := range r.Turns {
		cost := t.Metrics["cost_usd"]
		if t.Ended.IsZero() || time.Since(t.Ended) > 2*spendWindow || !(cost > 0) || math.IsInf(cost, 0) {
			continue
		}
		e := &spendEntry{Run: r.ID, Created: r.Created, Turn: t.N, Profile: r.Profile, At: t.Ended, USD: dollars(cost)}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(spendDirOf(home), 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(v1SpendPath(home, r, t.N), append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// pruneSpend removes ledger entries two windows old.
func (h *Home) pruneSpend() {
	entries, _ := os.ReadDir(h.spendDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 2*spendWindow {
			os.Remove(filepath.Join(h.spendDir(), e.Name()))
		}
	}
}
