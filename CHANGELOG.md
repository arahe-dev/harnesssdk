# Changelog

## v1.7.0 (2026-10-12)

Fixes for the findings of a Pi review agent (`review`, GPT-6.1 Sol): 13 in
its review of v1.6.0, and 5 more in its review of the first version of
this release. Every code fix has a regression test. **v1.6.0 is
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
  background turn whose worker died counts, once, however often it is
  observed (its end is saved even when the follow-up that found it is
  refused); "in a row within an hour" is now a true sliding window, not an
  inactivity timeout; one lock guards every record, so cleanup can no
  longer remove a lock in use.
- **compare** checks every profile's policy and budgets before creating
  any run, and removes the runs it created if creating a later one fails
  at any stage. A run whose first save fails no longer leaves its folder.
  An invalid policy is now refused before any run is created, by `run` and
  `start` too.
- **gc** never removes a run folder that is being created: a folder without
  a record is removed only if untouched since the cutoff.
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
