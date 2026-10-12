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
| `create.lock` | taking and removing creation locks (never removed); waits for it end with the caller |
| `spend/<run>-<turn>-<attempt>-<random>.json` | the spending ledger: one attempt's entry, replaced whole (no lock) |
| `created/<creation>` | a committed creation (no lock; made once) |
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

`max_wait` bounds a wait, each lock wait and shared-file retry included,
and a grant that arrives after it is given back; a request whose wait ran
out, or whose caller cancelled, leaves at once (it withdraws from ledgers it
can lock without waiting, and the others drop its dead ticket); `max_queue` refuses a request that would join a
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
   turn backing off holds no budget, key or slot. Spending is a ledger of
   its own (below).
5. **Finished**: the outcome is counted for the repeat guard before the last
   ticket is released (so the next identical turn sees it), a worktree's
   changes are committed, and the record is saved.

## Creating runs

A run is prepared in `runs/.new-<id>`, with its record and first turn, and
renamed into `runs/<id>` whole, so `runs/` never holds a half-made run. The
creator holds `runs/.new-<id>.lock` from before it makes the folder until
the creation is committed or rolled back, and rechecks under that lock that
the id is free (a creator publishes before it releases its lock). Creation
locks are taken, and their files removed, only while holding `create.lock`,
briefly, so a lock can never be split.

A creation is all or nothing: `start` with several tasks, or `compare`
(which validates every profile first). Each run is marked `creating`, with
the creation's id, in its staged folder, so it is published marked, and a
run whose mark cannot be written is never published. Once every run is
published, the creator commits: it makes one file, `created/<creation>`, a
single decision that a crash cannot leave half made. A marked run executes
only if its creation committed; if the commit cannot be made, every run is
rolled back. After the commit the marks, then the commit file, are removed
as clutter: `gc` removes a mark left behind, and a commit file that no mark
named in a scan that read every mark, once it is an hour older than the
scan's start (the runs of an older commit were published before the scan
began). `gc` leaves a run of an uncommitted creation alone while its
creation lock is held; once it is free, the creator is gone and can no
longer commit, so the creation was abandoned (a crash, or a rollback that
could not finish), and `gc` removes the run as its rollback would have,
branch included, whatever its age. Only what is known is acted on: a mark
or commit file that cannot be read leaves the run alone (and `Execute`
refuses it), a run that ever started a turn is never treated as abandoned,
and a rollback removes a worktree only when git finds it clean (`status
--porcelain --ignored --untracked-files=all --ignore-submodules=none`) and
it holds no repository of its own (an initialized submodule), and a folder
that is not a worktree (no `.git`) only when it is empty: anything it
cannot inspect is kept.

A `--worktree` run gets its worktree and branch while staged. Each name is
claimed by an operation that fails if the name is taken (`mkdir` of the
folder, then `git branch`, a ref transaction) and recorded in the staged run
once claimed, so an aborted creation, or `gc` finding a dead creator's
staging folder, removes only what this creation made; a crash between a
claim and its record leaves a name behind, never removes another's. A
rollback deletes the branch only while it points at the commit it was made
from (`git update-ref -d <ref> <base>`).

## The spending ledger

Spending limits count `spend/<run>-<turn>-<attempt>-<random>.json`, one
entry per attempt, each written whole and only by its attempt's process
(the random part keeps a run id used again, after its run is gone, from
writing over the first run's entries). The entry is made, `running`, before
the attempt runs. Once the attempt is over, its cost is written into the
entry before its ticket is released, so the next turn admitted counts it.
An entry is never removed because its cost could not be written, limit or
not. With `spend_unknown` `refuse` (below), a turn under a limit whose
entry cannot be made does not run, and one whose cost cannot be written
stays admitted, trying again (1 s, doubling to 30 s), until it is written or
the turn is stopped; a stopped one lets go of its execution lock before its
ticket. Otherwise the turn goes on, and its cost is unknown.

Spending sums the entries in the window. A `running` entry counts nothing
while its turn executes (its run's execution lock is held, its record says
the turn is running, and the run is the one the entry names: same id and
creation time): turns already running finish, as before. Once
that turn is over, a `running` entry's cost is unknown, and so is an entry
that cannot be read, or that is not a valid entry (one naming its run and
its creation time, turn, attempt, profile and time, and, once closed, a
cost of zero or more dollars);
files being written, `.tmp-*`, are not entries. How an unknown cost
counts is the user's choice, `spend_unknown`, in `config.json` or the
profile of the turn being checked (the profile's wins), for every limit
over that turn: `warn` (the default) counts it as $0 and puts a hint on the
turn; a number counts it as that many dollars each; `refuse` refuses turns
under a limit it falls under until it is out of the window, or its entry is
removed. Turns recorded before v2 count by their run
records until v2 saves the record, or `gc` removes it, either of which
first moves their costs into the ledger (`<run>-<created>-<turn>-v1.json`,
the same name each time for one run, and another for a later run with its
id); a turn whose cost is there counts only there. Spending
reads the ledger before the v1 records and again after them, so a cost
moved while it reads is counted once, from wherever it was when read. A
run record in the window that cannot be read may be a v1 one, so its cost
is unknown; a folder whose record is gone was collected, its cost moved.
Entries go once they are two windows old.

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

`gc` removes runs last active before its cutoff, and abandoned creations
(above), under their execution and state locks, record first. It leaves
alone a run that is executing, or whose first turn still waits for its
worker, and a staging folder or marked run whose creator is alive. It stops
when its caller cancels; what it was removing is kept. A folder without a record (an interrupted removal) goes once it is
untouched since the cutoff. A collected worktree run loses its worktree, after anything left in it is
committed to its branch, which stays; a worktree whose work cannot be
committed is kept, with its run.

## What harness does not do

It does not sandbox agents (their harness's permissions apply), plan or
chain work (the parent agent does), run a daemon, or coordinate across
machines: every limit is per data folder, on one machine.
