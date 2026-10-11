# Changelog

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

- Unix builds and vets but has not been run; CI runs the suite on Ubuntu and
  Windows.
- If Windows refuses a Job Object (a parent job can forbid it), cancelling
  falls back to `taskkill /T`, and processes the CLI leaves running when it
  exits are not cleaned up. The attempt's stderr log records a warning.
- Not yet covered by tests: real SIGTERM delivery, two `send`s racing for one
  run, and the Job Object fallback path.
