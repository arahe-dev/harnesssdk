# Changelog

## v2.0.0-next.1 (2026-10-12)

- **`spend_unknown`: you choose how a cost harness could not record counts.**
  v2.0.0-next refused turns under a spending limit for 24 hours after such
  a cost (an agent's harness process died mid-turn, or the ledger could not
  be written), and nothing told the user how to get out of it. Now it is a
  setting, `harness config set spend_unknown <warn|refuse|dollars>`, which a
  profile can override with its own `spend_unknown`:
  - `warn`, the default, counts the cost as $0, and the turn's
    `harness: hint:` says so and how to change it;
  - a number counts each such cost as that many dollars;
  - `refuse` keeps the old behaviour.

  The policy governs the ledger's strict paths too: only under `refuse`
  does a turn under a limit not run when its spending cannot be recorded,
  or stay admitted while its cost cannot be written. Refusals name the
  setting.
- `harness config set` takes a value per key (`spend_unknown` is not a
  number); the Go API adds `Config.SpendUnknown` and
  `Client.SetConfigValue`.
- **Review.** The Pi review agent's first review found 5 medium issues, all
  in what happens when the spending ledger cannot be written. They were
  fixed in one change, against stated guarantees: every attempt's cost
  counts once (its own if written, else as `spend_unknown` says); an
  attempt's ledger handling follows the rule it was admitted by; an
  unknown cost is always shown. Concretely:
  - an attempt whose entry could not be made still has its cost written
    once the ledger can be;
  - a cost is given to its own attempt's entry, so a later entry never
    counts it again;
  - a retry counts its earlier attempts whose costs are not written;
  - ledger failures under a limit put a hint on the turn.

  Unreadable ledger or run folders now count as unknown instead of failing
  the turn. Its second review found 3 medium issues, also fixed with
  regression tests:
  - an unreadable ledger folder counts as one unknown cost per check;
  - a turn's own unwritten entries are left out of the ledger's reading,
    so each counts once even when its file cannot be read either;
  - they leave the window like any other entry.

## v2.0.0-next (2026-10-12)

The v2 candidate: verified outcomes, isolated working copies, spending and
waiting limits, and retries Herta 1.0-next cannot express yet. The module
path is now `github.com/arahe-dev/harnesssdk/v2`; v1.7.0 stays the stable v1.

- **Retries with backoff.** Herta 1.0-next keeps v0.2.0's limits: no backoff,
  and `throttled` cannot be retried. Harness now owns the retry loop (Herta
  runs one attempt per call and still validates effects and turns a timeout
  into `uncertain`): a profile's `backoff` (default 2s) doubles per retry up
  to 5 minutes with jitter, and waits at least what the CLI asked for (a
  definition's `retry_after` regex). `throttled` may be listed in `retry`,
  except for non-idempotent profiles. **Between attempts a turn holds no
  budget, key or slot.**
- **Verified outcomes.** A profile's `check` (or `--check`) is a command line
  run as written in the agent's folder once it answers; the turn succeeds
  only if it exits 0. A failed check is permanent and keeps the answer and
  session. `stats` counts checks passed.
- **Worktrees.** `--worktree` (or a profile's `worktree: true`) runs the
  agent in a git worktree on branch `harness/<run>`; each turn's work is
  committed there and summarized. `compare --worktree` compares agents that
  edit files. `gc` removes the worktree and keeps the branch.
- **Limits.** Config `max_wait_minutes` and profile `max_wait` bound a wait at
  the gate; `max_queue` refuses a turn that would queue behind that many;
  config and profile `spend_limit_usd` cap the reported cost over 24 hours.
- **`harness def check <name> [--profile p]`** finds the CLI and its version,
  and with a profile runs a live turn and checks its answer and metrics.
- **Usage reports.** `output.report` reads metrics and the session id from a
  JSON file the CLI writes to `{report_file}`.
- **New definitions.** Hermes Agent 0.20.5, verified live (answer, resume,
  metrics). Factory Droid and Kimi Code as unverified starting points (the
  CLIs were not signed in).
- **Go API (v2).** `Request.Check` and `Worktree`; `Send` and `SendStart` take
  a `FollowUp`; `Result` and `Turn` gain check, branch and changes fields;
  `Config` gains the new limits; `Stats` counts checks; `CheckDefinition`;
  `GC` takes a context.
  The engine's `TurnOptions` replaces its growing argument lists.
- **docs/DESIGN.md** explains every lock and protocol in one place.
- **Formats.** Run records and `config.json` are format 2: they hold what
  v1 would misread or drop (a worktree run's folder, the new limits), so
  v1.7 refuses them ("upgrade harness") instead of acting on them. v2 reads
  format 1. Other files are unchanged and shared.

### Review

The Pi review agent (`review`, GPT-6.1 Sol) reviewed this release before it
was merged. Its first review found 2 high, 11 medium and 2 low issues, all
fixed with regression tests:
- `gc` could delete a worktree whose work was never committed: it now
  commits the work first and keeps any worktree it cannot commit; a failed
  turn commit is reported.
- v1.7 could act on v2 records (above).
- A spending refusal could loop as a retry; only an attempt that ran is
  retried.
- Spending counted only finished turns of runs created in the window: each
  attempt's cost is now recorded before its budget is released, and counted
  by when it was spent.
- Checks now run with the run's identity (`HARNESS_*`), its locks, and as
  its recorded process.
- `max_wait` held only between gate passes: lock waits are now bounded by
  it, and a grant after it is given back.
- Worktrees: what is about to be made is recorded first, so a failed
  `git worktree add` or a creator that died is undone (by its rollback or by
  `gc`); git is addressed from the run's folder or the repository's own git
  folder; every git command is bounded and contained like an agent; an
  untracked starting folder is created in the worktree; paths keep leading
  spaces.
- `def check` no longer accepts an echoed prompt, reports declared metrics
  that did not come out, and runs the version command with its process
  tree bounded.
- Each attempt has its own `{report_file}`.

Its second review found 3 high, 5 medium and 1 low issues, also fixed with
regression tests: worktrees claim only free branch and folder names, and a
rollback never deletes what it did not make; gc keeps a worktree whose HEAD
left its branch; harness's commits run no hook at all (an empty hooks
folder), and gc's removal is never forced, so a change it did not commit is
kept; each attempt's cost is a timestamped entry, so the rolling window
counts only what was spent in it, and a turn whose spending cannot be
recorded is not retried; every ledger lock waits only what remains of
`max_wait`, and an expired turn leaves without waiting for locks; a rollback
that cannot finish keeps its staged record for gc to finish; git honors
cancellation and shutdown.

Its third review found 2 high and 5 medium issues, also fixed with
regression tests:
- Worktree names are claimed by operations that fail when a name is taken
  (making the folder, then `git branch`), and recorded only once claimed,
  so two data folders using one repository can never remove each other's
  branch; a rollback deletes a branch only while it still points where it
  was made.
- Each commit gets a new private hooks folder (one in a fixed place could
  be planted with hooks).
- An attempt whose cost could not be recorded keeps its admission until the
  turn's record holds it, so no turn is admitted against a total that
  leaves it out.
- Gate lock waits and shared-file retries stop when the wait ends, and a
  pass that ends that way leaves at once.
- A `compare` whose rollback cannot finish leaves its runs marked: they
  never execute, and `gc` finishes the rollback, whatever their age.
- `Start` creates worktrees under its caller's context, and `harness stop`
  also stops a turn's final commit.
- The Pi example's `error` selector is back, reading only the final
  `turn_end`: in JSON mode Pi exits 0 when its last model call failed.

Its fourth review found 5 medium issues, also fixed with regression tests:
- A turn whose cost cannot be recorded stays admitted, saving its record
  again, until the cost is on record or the turn is stopped.
- A creation is a transaction: runs are published marked `creating` and
  unmarked only once all of them exist, so a run never executes, and is
  never kept as a normal run, unless its creation finished; gc finishes an
  abandoned creation once its creator is gone. (This replaces the
  `abandoned` mark of the third round.)
- The worktree lock and the daily automatic `gc` honor the caller's
  cancellation.
- A stop request is acted on once, by the turn or its final commit,
  whichever sees it first, so one that arrives in between is not lost.
- A rollback does not delete a branch that another worktree has checked out.

Its fifth review found 4 medium issues, also fixed with regression tests:
- A cost that cannot go into its run record is recorded apart, in
  `<home>/spend`, and counts at once, so a turn stopped while its record
  cannot be saved leaves its cost on record.
- A creation commits by one decision, a file in `<home>/created`, instead of
  removing its runs' marks one by one, so a crash part way cannot leave some
  runs created and others not.
- Waits for `create.lock` end with the caller, in creation and in `gc`.
- A stop watch ends between takes of a stop request, so a request is never
  taken by a watch that has ended.

Its sixth review found 2 high and 3 medium issues, also fixed with
regression tests:
- A creation mark or commit file that cannot be read leaves its run alone
  (gc used to read it as "not committed"), a run that ever ran is never
  rolled back, and a rollback keeps a worktree that holds changes.
- Commit files are pruned only when an hour older than the start of a gc
  scan that read every mark, so a long scan cannot prune one still needed.
- Spending is one ledger, `<home>/spend`, with an entry per attempt made
  before it runs and given its cost before its admission is released. It
  replaces the per-turn `spend` records and the files written apart in the
  fifth round. A cost that never reached the ledger is unknown and, like an
  unreadable entry, refuses turns under a limit until it leaves the 24-hour
  window or is removed: spending limits fail closed.

Its seventh review found 1 high and 5 medium issues, also fixed with
regression tests:
- A rollback removes a worktree only when git finds it clean, or never made
  it one; one it cannot inspect is kept.
- A v1 record's turn costs move into the ledger before v2 first saves the
  record, so a follow-up on an old run still counts them.
- Ledger entries have names of their own, so a run id used again cannot
  write over earlier costs, and an entry whose cost could not be written is
  kept even when no limit applies.
- A run counts as having run only when a turn started, so an uncommitted
  creation whose queued turn was ended as lost is still rolled back.
- Ledger entries are validated (run, turn, attempt, profile, time, cost);
  an invalid one is unknown, and a turn number of 0 no longer panics.

Its eighth review found 2 high and 4 medium issues, also fixed with
regression tests:
- A rollback asks git for every untracked and ignored file whatever the
  repository's configuration, and removes a folder that is not a worktree
  only when it is empty.
- A v1 turn's cost counts once, in the ledger once it is there, and moves
  there before `gc` removes its record too.
- A ledger entry names its run's creation time, so a later run with the
  same id cannot make an earlier unknown entry look like it is running.
- A closed entry without a cost, or with a null one, is not valid.

Its ninth review found 1 high and 1 medium issue, also fixed with
regression tests: a rollback also shows submodule changes whatever the
configuration, and keeps a worktree that holds a submodule or another
repository; and spending reads the ledger again after the v1 records, so a
cost moved to the ledger while it reads counts once.

Its tenth review found 2 medium issues, also fixed with regression tests:
moved v1 costs are named by their run's creation time too, so two v1 runs
with one id keep both costs; and the search for submodules stops when its
caller cancels.

Its eleventh review found 1 medium issue, also fixed with a regression
test: a run record that cannot be read, or that a later harness wrote, may
hold a cost, so its cost is unknown and turns under a limit are refused.

- **Pi definition fix.** v1.6's Pi example had an `error` selector that
  marked a run failed when any model call errored, even one Pi recovered
  from (the review agent's own long run hit it). An `error` selector can now
  use `"pick": "last"`: only the last event its `where` matches decides.
  The Pi example uses it on `turn_end`.

Not done: the Codex `workspace-write` sandbox (with the data folder writable
and network on) could not be verified, because Codex's Windows sandbox fails
to start any command on the machine used; the guide describes it as
unverified, and the full-access Codex example stays.

## v1.7.0 (2026-10-12)

Fixes for the findings of a Pi review agent (`review`, GPT-6.1 Sol): 13 in
its review of v1.6.0, and 12 more across its six reviews of this release's
pull request (5, 2, 2, 1, 1 and 1), until it passed. Every code fix has a
regression test. **v1.6.0 is
retracted** (`go.mod`), since it broke code written for v1.5.

- **API compatibility restored.** v1.6.0 added map fields to `Result` and
  `Turn`, which made them impossible to compare with `==` or use as map
  keys, breaking code written for v1.5. `Result.Metrics` and
  `Turn.Metrics` are now a comparable `Metrics` value (with `Get`, `Map`,
  `String` and JSON support; equal numbers, `-0` and `0` included, compare
  equal). v1.6.0 is retracted for this; code that used its map fields calls
  `Map()`. `gorelease` against v1.5.0 reports only additions. A test
  compiles v1.5-style uses of every comparable type, and the compatibility
  notes now state Go's rules: struct types may gain fields (use keyed
  literals), and comparable types stay comparable. `Stats` gains
  `Reported`, how many turns reported each metric.
- **Metrics.** The extraction pre-filter no longer skips a line whose
  `where` value contains a character JSON escapes briefly (tab, newline,
  carriage return, backspace, form feed); a metric that reads as NaN or
  infinity, or whose sum overflows, is dropped instead of making the turn's
  record impossible to save (which left the turn "running").
- **Repeat guard.** A request is identified by its profile and definition
  as they are when the turn executes (editing a refused profile makes a new
  request, also for turns already queued); turns queued before the limit
  was reached are refused, checked again once admitted past their budgets,
  and a turn's outcome is counted before it releases its budget, so the
  next identical turn sees it; a refusal is not counted as a failure; a
  background turn whose worker died counts. Each turn is counted once ever:
  it is marked in its own record before it is counted, so observing its
  end again (after a lost final save, a success or the window cleared the
  failures) never counts it again; a lost turn's end is saved even when
  the follow-up that found it is refused, and the turn counted is the one
  that was found lost, even if another was queued after it meanwhile.
  Outcomes are ordered by when their turns ended: a failure counted late
  does not undo a success that came after it. "In a row within an hour" is now
  a true sliding window, not an
  inactivity timeout; one lock guards every record, so cleanup can no
  longer remove a lock in use.
- **compare** checks every profile's policy and budgets before creating
  any run, and removes the runs it created if creating a later one fails
  at any stage. A run whose first save fails no longer leaves its folder.
  An invalid policy is now refused before any run is created, by `run` and
  `start` too.
- **gc** can no longer remove a run being created, whatever its cutoff:
  a run is prepared under a staging name, with its record and first turn,
  and renamed into `runs/` whole, while its creator holds a lock beside
  it; gc removes a staging folder only once that lock is free (its creator
  is gone), however old the folder. Creation locks are taken, and their
  files removed, only under one permanent coordination lock, so on Unix a
  lock can never be split between a file and the one recreated under its
  name. A run whose turn still waits for its
  worker is left alone until the turn counts as lost. A folder without a
  record (an interrupted removal) is removed only if untouched since the
  cutoff. Run ids that collide with a published run or a creation in
  progress are skipped, never replaced.
- **stats** no longer misses runs when the time zone differs from when they
  were created (run ids spell local time); `COST/OK` is shown only when
  every turn of the group reported its cost.
- **Guide:** `compare` is for read-only profiles; agents that edit files are
  compared with `run --kind` in separate checkouts and `stats`.

## v1.6.0 (2026-10-12)

Choosing a harness and model from evidence, after *An Empirical Study of
Harness Design for Coding Agents* (Fan et al., arXiv:2609.20804). The study
found that no harness setup is best everywhere: which tools, planning and
context handling help depends on the model, the task type and the budget.
HarnessSDK's job is that choice, so this release measures it.

- **Metrics.** A definition's `output.metrics` reads numbers from the CLI's
  own output with every turn: tokens, cost, model calls, tool calls. Two
  new picks, `sum` and `count`; a field may add paths with `+`; array
  indexes may count from the end (`-1`). Every attempt counts, failed ones
  too. Turns record their metrics and model; `--json`, `show` and the
  status line print them. harness keeps no prices: cost is recorded only
  where the CLI reports it.
- **`harness stats`** sums up finished turns per profile, model and kind:
  success rate, failures by outcome, median and p90 time, attempts, tokens
  and cost per turn, and cost per success. `--kind <label>` on `run` and
  `start` labels runs for it.
- **`harness compare a,b,... "task"`** runs one task on several profiles at
  once and sets their answers, outcomes, time, tokens and cost side by
  side. Every profile is checked before any run is created.
- **Repeat guard.** When the same request (profile, prompt, inputs and
  caller) fails `repeat_warn` (3) times in a row within an hour, harness
  adds a hint to change approach; after `repeat_refuse` (5) it refuses the
  request (exit 3). A success clears the count; throttled and cancelled
  turns do not count. This is the study's stuck detection, applied to whole
  runs, which cost far more than a repeated tool call.
- **Guide:** a section on choosing a harness, model and settings (tools,
  planning, context) with the study's numbers and caveats, and how to check
  them with `compare` and `stats`. The verified definitions now record
  metrics; Pi's uses `--mode json`, which reports usage and cost.
- **Go API:** `Client.Compare`, `Client.Stats`, `Request.Kind`, and
  `Result.Model`, `Result.Metrics`, `Result.Hint`, `Result.Error`
  (additions only).

### Verified (Windows 11)

- `go test ./...`, `-race`, `go vet` and staticcheck for Windows, Linux and
  macOS; the guide's example definitions are parsed by a test.
- Live, with the new definitions: `compare` of Claude Haiku, Pi
  (`gpt-6.1-sol`, JSON mode) and OpenCode on one task, all three answering
  with tokens and cost; a Pi run that used a tool (`tool_calls: 1`,
  `turns: 2`); a Codex agent that ran a nested Pi agent, with Codex token
  counts and the nested run at depth 1; `harness stats` over these runs; and
  the repeat guard on a missing CLI (hint from the 3rd failure, refusal of
  the 6th, a different prompt still allowed).

## v1.5.0 (2026-10-12)

A stable Go API, machine-wide limits, retention, versioned files, and the
five issues from the v1.2 product audit.

- **Stable Go API.** The root package `harnesssdk` is now a small client
  over the same engine and data folder as the command: `Open`, then `Run`,
  `Start`, `Send`, `SendStart`, `Wait`, `Result`, `Stop`, `Runs`,
  `SetDefinition`, `SetProfile`, `SetBudget`, `Config`, `SetConfig`, `GC`,
  with its own `Result`, `Run`, `Turn` and `Outcome` types. It follows
  semantic versioning from this release; the implementation moved to
  `internal/engine` and may change in any release.
- **Versioned files.** Run records, budgets, trust, gate ledgers, the active
  index, definitions and the new `config.json` carry a format number. A
  harness refuses a newer format with "upgrade harness" rather than misread
  it; files from earlier releases read as format 1 and are upgraded when
  next written.
- **Limits** (`harness config`, `harness config set <key> <n>`):
  `max_depth` (3) refuses runs nested deeper (exit 3, with the command to
  raise it); `max_running` (16) caps turns running at once on the machine
  through the gate. A top-level turn waits for a slot; a nested one fails at
  once as throttled, since its ancestors hold slots while they wait for it.
  Runs record their `depth`.
- **Retention.** Finished runs, with their prompts and answers, are removed
  `retention_days` (30) after their last turn: by `harness gc [--days n]
  [--dry-run]`, and by harness itself at most once a day as runs start. A
  run that is executing, or was sent a turn since the cutoff, is kept.
- **No discovery where it is not needed.** `wait`, `result` and `stop` no
  longer look for a parent run, and discovery retries only while an
  executing ancestor's agent is not yet recorded. `harness result` while a
  run is active: 1.1 s before, 0.04 s now.
- **The gate waits through contention.** A ledger kept locked by other
  processes for more than two seconds was a permanent failure; it is now
  part of waiting. A pass that changes nothing no longer rewrites the
  ledger, and `harness status` reads ledgers without locking them.

### Optimizations

Measured with the benchmarks in `internal/engine/bench_test.go` (Windows
11, Go 1.26):

| | before | after |
|---|---|---|
| Extract the answer from a 5 MB Codex JSONL stream | 62 ms, 25 MB, 597k allocs | 10 ms, 68 KB, 61 allocs |
| Walk process ancestry (Windows) | 10 ms | 19 µs |
| A waiting turn, per second | 5 locked ledger passes (340 µs each) | 10 stats (22 µs) + 1 pass |
| Grant noticed after a release | ≤ 200 ms | ≤ 100 ms |
| `wait`, per poll | parse the record every 300 ms | stat every 100 ms, parse on change |
| Check one active turn | 170 µs, 23 KB | 90 µs, 0.8 KB |

JSONL extraction now reads one event at a time, so memory no longer grows
with the stream, and skips decoding lines whose text cannot match a
selector (a value spelled with a `\u` escape is still decoded).

### Removed

- Budgets and keys are arbitrated once, by the gate; the per-process Herta
  budget that duplicated it is gone.
- `Home.FindProject`'s private twin, and an unused return value.

### Verified (Windows 11)

- Unit and end-to-end tests, `-race`, `go vet` and staticcheck on Windows,
  Linux and macOS targets.
- Live: Claude Haiku (`run`, and `start` then `wait`); a Codex 0.162 agent
  on `gpt-6.1-sol` that ran a nested Pi agent through harness (recorded with
  `parent` and `depth: 1`); three Pi reviewers on a `pi=2` budget ran two
  at a time, the third starting as a slot freed; a simulated run nested
  below `max_depth` was refused with exit 3.

## v1.2.0 (2026-10-12)

Nested runs work under any harness, and Codex works end to end.

- **Nested runs find their parent without help.** A harness can strip the
  environment of its agent's commands (Codex with
  `shell_environment_policy.inherit = "core"` removes `HARNESS_RUN`), which
  left a nested run without its parent or its ancestors' tickets. When the
  variables are missing, harness now walks up its process ancestors to the
  harness process executing a running turn, and reads that turn's recorded
  lineage. `harness status`, run inside an agent, says which run it belongs to
  and whether that came from the environment or from ancestry.
- **`{lineage}` and `{harness_home}` placeholders**, so a definition can hand
  harness's variables to a CLI explicitly (the Codex example does).
- **Turn records keep their lineage**; gate ticket ids start with `t`, so
  passing one through a TOML or JSON setting never turns it into a number.
- **Windows: reads no longer fail while a record is being replaced.** A
  `harness wait` polling a run while its worker updated it could fail with a
  sharing violation; reads and replacing renames of records now retry through
  it. A stress test fails without the fix.
- **`def set` refuses control characters in failure patterns**, the usual
  result of writing `\b` (a backspace in JSON) for a regex word boundary;
  such a rule could never match. Three definitions written during v1.1
  testing had this defect and silently missed HTTP 429.
- **`"stdin": "system+prompt"`** sends the profile body as an
  `<instructions>` block ahead of the prompt, for CLIs with no system-prompt
  option (Codex has none that adds to its instructions from a file; OpenCode
  has none). Both now get their profile's instructions.
- **The guide has verified example definitions** for Pi, Claude Code, Codex
  and OpenCode.

### Review

The Pi `gpt-6.1-sol` reviewer found three defects, fixed before release with
regression tests: (1) turns executed side by side by one harness process
share that process, so discovery could hand a nested run a sibling's parent
and tickets; each turn now records its own agent's root process (with its
creation time) in an index of active turns, and discovery matches that; (2)
discovery searched only the 500 newest runs, so an older long-running parent
could be missed; it now reads only the active index; (3) the Codex and
OpenCode examples dropped the profile body; see `system+prompt` above.
Checking (1) also showed that a process that is itself a turn's agent (a
definition can run harness directly) must count as nested in that turn.

### Verified (Windows 11)

- Codex 0.162.1 (the latest release) on `gpt-6.1-sol` (the top model in the
  account's Codex model list): a Codex agent started with `harness start`
  created a Pi profile, ran it through harness, and the Pi run recorded the
  Codex run as its parent and inherited its gate ticket. `harness send`
  resumed the Codex session. With the variables deliberately not passed,
  `harness status` inside Codex found its run through process ancestry.
- Codex and OpenCode both answered from instructions present only in the
  profile body, delivered with `system+prompt`.
- Tests: nested discovery with and without the environment, sibling turns in
  one process, discovery among 600 newer runs, the new placeholders, ancestry,
  concurrent record replacement, `system+prompt`, and the pattern check;
  race-clean.

## v1.1.0 (2026-10-11)

Removes every v0.1.0 limit that code could remove.

- **Budgets and serialize keys hold across every harness process** on the
  machine: a FIFO ledger per name in `<home>/gate/`, with Herta's budget
  rules (granted when it fits and nothing is queued ahead), reject admission,
  all-or-nothing grants for requests that name several budgets, and crash
  safety (each ticket's owner holds a lock file; a dead owner's units are
  reclaimed by the next reader). `harness status` shows holders and waiters.
- **Nested runs cannot deadlock on their ancestors.** Runs pass
  `HARNESS_RUN`, `HARNESS_LINEAGE` and `HARNESS_HOME` to the agents they start;
  a nested run skips the queue for a budget its ancestors hold, and fails at
  once if it could only fit after they finish. Runs record their parent.
- **Detached runs:** `harness start` and `send --detach` return at once while a
  background worker executes the turn; `wait [--timeout]`, `result` and `stop`
  work from any later command (exit 7: still running). On Windows the worker
  breaks away from the starting command's Job Object, or is started through
  WMI's `Win32_Process.Create` when the job forbids breakaway, so a shell tool
  that kills its job does not kill the worker. A turn whose worker died is
  reported as `uncertain`, a turn whose worker never started as `transient`.
- **Per-project profiles and definitions** in a `.harness` folder, taking
  precedence in that project, with hash-pinned trust: a project file is used
  only after `harness trust --approve`, and any change needs trusting again.
  Files written with `--project` are trusted as they are saved. A data folder
  is never mistaken for a project.
- **JSONL output rules:** `answer`, `session` and `error` selectors (`where`,
  `field`, `pick`) for CLIs that stream events, such as `opencode run --format json`.
- **Windows without a Job Object:** the process tree is tracked by parent
  process ids while the CLI runs, holding a handle to each process found, so
  cancelling kills it and a CLI's leftovers die when it exits (best effort:
  see the review notes below).
- Exit code 7 (still running); `run`/`send` prepare a turn and then execute it,
  so the same turn can run in process or in a worker.

### Verified (Windows 11)

- Tests: race-clean; new tests for the gate (FIFO, reject, cancel, dead
  tickets, nesting), three detached workers sharing a budget of 1 across
  processes, stop, lost-worker detection, project trust, JSONL against a real
  OpenCode event stream, leftover-process cleanup, and the Job Object
  fallback. A Unix-only test sends SIGTERM to the CLI and checks the tree dies;
  it runs in CI on Ubuntu.
- Live: `harness start` on a Claude Haiku profile returned in 0.8 s from a
  shell that forbids Job Object breakaway; the WMI-started worker finished the
  turn and `harness wait` collected it from a later command. Two detached
  Claude runs with a budget of 1 ran one after the other, with `harness status`
  showing one holding and one waiting. An OpenCode profile defined only in a
  project answered through the JSONL rules and resumed its captured session.
  An untrusted project profile was refused.

### Review

The Pi `gpt-6.1-sol` reviewer, started with `harness start` and collected with
`harness wait`, reviewed this release and found eight defects, all fixed
before release, each with a regression test checked to fail when its fix is
reverted (pid reuse excepted: it cannot be triggered on demand):

1. A nested run could deadlock across two budgets through an unrelated
   waiter holding one of them. The gate now grants a request's claims all at
   once or holds nothing, and nested runs skip the queues.
2. A dying worker released its budget while its agent kept running. On Unix
   the CLI now runs under a guard (the harness binary again, leading the
   agent's process group and holding the turn's locks) that kills the whole
   group when the harness process dies, however it dies; on Windows the job's
   kill-on-close ends the tree.
3. The Windows fallback missed a grandchild whose intermediate parent had
   exited; descendants are now tracked while the CLI runs.
4. The fallback could kill a process that reused a descendant's pid; a child
   must be born between its parent and the snapshot, and the kill re-checks
   the creation time on the handle it terminates.
5. A trusted project profile with an invalid policy could wait at the gate
   forever; the policy is validated before waiting, and duplicate claims are
   refused.
6. `wait` and `stop` could switch to a later turn; they now pin theirs.
7. A worker started through WMI lost the caller's environment (API keys,
   PATH additions); it is handed over in a private file the worker deletes.
8. `Shutdown` did not withdraw turns waiting at the gate; it now does, and
   drains the rest.

A second pass over the fixes found four gaps, also fixed: the first fix for
(2) used Linux's parent-death signal, which reaches only the CLI and not its
children (replaced by the guard above); the Windows fallback now holds a
handle to every process it tracks, which stops Windows from reusing their ids,
so a dead tracked parent can be walked safely; a 200 ms tracking interval can
still miss a helper that starts a child and exits within it, so the fallback
is documented as best effort; and the environment file handed to a worker,
which holds the caller's secrets, is now removed when detaching fails or when
a turn whose worker never started is reaped.

Programs that embed `Runner` must route harness's hidden subcommands (the
detached worker and the Unix guard) by calling `harnesssdk.Subcommand()` first
in `main`.

### Fixed during development

- Project discovery treated the default `~/.harness` as a project for any
  folder under the home directory when `HARNESS_HOME` pointed elsewhere; a
  test run wrote a profile into the real data folder (removed). Data folders
  are now recognised by their `runs` folder.

## v0.1.0 (2026-10-11)

First version. One `harness` CLI that runs agents in other harnesses through
their own headless CLIs, with HertaSDK v1.0.0-next as execution policy.

- Harness **definitions** (JSON): command, argument groups with placeholders
  (`{name}`, `{name?text}`, `{*name}`), prompt on stdin, answer from all of
  stdout or one JSON field, minted or captured session ids, failure rules.
  Unknown fields are rejected.
- **Profiles** (Markdown with a header): placeholder values, system prompt, and
  the Herta policy (`effect`, `retry`, `timeout`, `budget`, `admission`,
  `serialize`), checked against the definition and budgets when saved.
- `run` (with `--task` fan-out, `--file`, `--cwd`, `--set`, `--json`,
  `--dry-run`), `send` to continue a run's harness session, `runs`, `show`.
- Each attempt runs the CLI in its own Job Object (Windows) or process group
  (Unix): a timeout or cancel kills the whole tree, and leftovers are killed
  when the CLI exits. A timeout is `uncertain`.
- Run records in `~/.harness/runs/<id>/`; an OS file lock keeps two turns of
  one run from sharing a session.
- `harness guide` (agent manual) and `harness skill` (Agent Skills file) are
  embedded.

### Verified (Windows 11)

- Go tests with a fake harness: race-clean, 80.6% statement coverage. The
  tree-kill and review-fix regression tests were each checked to fail when
  the code they protect is reverted.
- A Claude Code 2.1.220 Haiku profile: a JSON answer, a captured session, and
  a resumed second turn that remembered the first.
- A Pi 1.0.4 `gpt-6.1-sol` reviewer set up only through `harness` commands,
  reviewing this release. Its first turn ended after 5m51s with Pi's
  `fetch failed`, which the definition classifies as `uncertain`, so it was
  not retried. `harness send` resumed the same Pi session, with its 34 tool
  results intact, and the review finished in 2m53s.
- That review reported 13 findings. One was refuted (npm `.cmd` shims do run:
  the review itself ran through `pi.cmd`); the other twelve were fixed before
  release: failure rules now see only the current attempt's own output,
  operations are cached per profile version, `send` reloads the run under its
  lock and refuses to switch harnesses, built-in placeholders cannot be set
  from outside, JSON failure documents keep their session, the data folder is
  absolute and private (0700/0600), budget updates are locked, SIGTERM
  cancels like Ctrl-C, a missing Job Object falls back to `taskkill /T` with a
  warning, and a run's record is finalized after its answer is saved.

- A second turn in the same reviewer session checked those fixes. It withdrew
  the refuted finding and raised four follow-ups, fixed here: only a lock
  actually held by another turn is reported as busy (`throttled`), the
  `taskkill` fallback has a 10-second deadline, an existing data folder's
  mode is tightened to 0700, and a failed run-record save says so.

### Known limits

- The full suite, including the process-group tree-kill test, passes in CI on
  Ubuntu and Windows with Go 1.26 and stable; no harness CLI has been run on
  Unix yet.
- If Windows refuses a Job Object (a parent job can forbid it), cancelling
  falls back to `taskkill /T`, and processes the CLI leaves running when it
  exits are not cleaned up. The attempt's stderr log records a warning.
- Not yet covered by tests: real SIGTERM delivery, two `send`s racing for one
  run, and the Job Object fallback path.
