# HarnessSDK design notes

How harness keeps its promises when many `harness` processes, background
workers and nested agents share one data folder. Each protocol below names
the invariant it keeps and the lock that keeps it. Code: `internal/engine`.

## Processes and locks

Every lock is an exclusive lock on a file, taken without waiting or with a
short bounded wait (`lockOpen`, `lockFileWait`): `flock` on Unix, `LockFileEx`
on Windows. A lock belongs to the open file; the operating system drops it
when the holder dies. **No lock file is ever removed while it can be
taken**, except creation locks, whose removal is itself coordinated (below):
on Unix, unlinking a locked file lets the next opener lock a new file under
the same name, which splits one lock into two.

| File | Guards |
|---|---|
| `runs/<id>/exec.lock` | the run is executing a turn (held for the whole turn; on Unix the agent inherits it) |
| `runs/<id>/state.lock` | read-modify-write of `run.json` (`updateRun`) |
| `gate/<name>/state.lock` | one gate ledger |
| `gate/live/<ticket>.live` | a gate ticket's owner is alive |
| `runs/.new-<id>.lock` | a run being created; removed only under `create.lock` |
| `create.lock` | taking and removing creation locks (never removed) |
| `repeats.lock` | every repeat-guard record (never removed) |
| `budgets.lock`, `config.lock`, `trust.lock` | those files |
| `worktrees.lock` | adding git worktrees, one at a time |
| `gc.lock` | one automatic collection at a time |

Files that other processes read are replaced whole (`writeFileAtomic`:
write a temporary file, rename over the old one), so a reader never sees half
a file. On Windows, readers and renames retry through sharing violations.

## Admission: the gate

Budgets, serialize keys and `max_running` are FIFO ledgers in
`gate/<name>/state.json`, with Herta's budget rules: a request is granted when
it fits and nothing is queued ahead of it. A request for several names is
granted all at once or not at all, so a waiting request holds nothing and no
cycle of waiters can form. A ticket's owner holds its `.live` file; the next
reader of a ledger drops tickets whose owner died.

Nested runs carry their ancestors' tickets (`HARNESS_LINEAGE`, or found
through process ancestry). They skip the queues for budgets their ancestors
hold, fail at once (`ErrWouldDeadlock`) when only their ancestors' release
would let them in, and never wait for a `max_running` slot.

`max_wait` bounds a wait; `max_queue` refuses a request that would join a
queue already that long. Waiters re-read a ledger only when its file changes,
or once a second to reclaim dead tickets.

## A turn

1. **Prepared**: the profile and definition load, the policy is validated,
   `max_depth`, the repeat guard and spending limits are checked, then the run
   is created (below) with its first turn queued.
2. **Executed** by `harness run`, or by a detached worker for `start`: the
   worker holds `exec.lock` for the whole turn.
3. **Each attempt** is admitted at the gate on its own, re-checks the repeat
   guard and spending limits, runs the CLI through Herta (one attempt: its
   effect checks, timeout as `uncertain`, classification), then, if it
   answered, the check.
4. **Retries** are harness's own, because Herta 1.0-next has no backoff and
   cannot retry `throttled`: a retryable failure releases its ticket, waits
   the backoff (at least the CLI's `retry_after`), and is admitted again. A
   turn backing off holds no budget, key or slot.
5. **Finished**: the outcome is counted for the repeat guard before the last
   ticket is released (so the next identical turn sees it), a worktree's
   changes are committed, and the record is saved.

## Creating runs

A run is prepared in `runs/.new-<id>`, with its record and first turn, and
renamed into `runs/<id>` whole, so `runs/` never holds a half-made run. The
creator holds `runs/.new-<id>.lock` from before it makes the folder until it
has published or removed it, and rechecks under that lock that the id is
free (a creator publishes before it releases its lock). Creation locks are
taken, and their files removed, only while holding `create.lock`, briefly, so
a lock can never be split. `compare` validates every profile first and
removes what it created if a later creation fails. A `--worktree` run gets its
worktree and branch while staged; an aborted creation removes both.

## The repeat guard

A request's fingerprint covers the profile and definition as they are when
the turn executes, the folder, the prompt, the `--set` values, the check, the
worktree flag, and the caller (parent run, or the run for `send`). Its record
in `repeats/<fp>.json` keeps consecutive failures from the last hour, each
with its turn's identity and end time, and the time of the last success.
Outcomes are ordered by when turns ended, so a failure counted late does not
undo a later success. A turn is counted once ever: it is marked `counted` in
its own record before the count, so observing its end again (a lost final
save, a reaped worker) never counts it twice; a crash between the two
undercounts, never overcounts.

## Collection

`gc` removes runs last active before its cutoff, under their execution and
state locks, record first. It leaves alone a run that is executing, or whose
first turn still waits for its worker, and a staging folder whose creator is
alive. A folder without a record (an interrupted removal) goes once it is
untouched since the cutoff. A collected worktree run loses its worktree; its
branch stays.

## What harness does not do

It does not sandbox agents (their harness's permissions apply), plan or
chain work (the parent agent does), run a daemon, or coordinate across
machines: every limit is per data folder, on one machine.
