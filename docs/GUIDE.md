# harness: agent guide

`harness` runs agents that live in other harnesses (Pi, Claude Code, Codex,
OpenCode, ...) as your subagents. It calls each harness's own headless CLI
directly, waits for the answer (or lets you collect it later), and prints it.
Nothing about any harness is built in: you describe a harness once as JSON
(a **definition**) and an agent built on it as Markdown (a **profile**).
Read this guide, then use the commands below.

## Workflow

1. Look at what exists: `harness profile list`, `harness def list`,
   `harness budget list`. If the profile you need exists, skip to step 6.
2. If the harness has no definition, read its CLI help (for example
   `pi --help`, `claude --help`) and write one: `harness def set <name> --file -`
   with the JSON on stdin. Prefer the CLI's documented one-shot mode.
3. If the profile should share a capacity limit, create the budget:
   `harness budget set <name> <capacity>`.
4. Write the profile: `harness profile set <name> --file -` with the Markdown
   on stdin. Saving checks it against the definition, the budgets and the
   execution policy, and lists any argument group that would be left out.
5. Check the exact command without spending tokens:
   `harness run <profile> --dry-run "test prompt"`.
6. Run it.
   - Short task: `harness run <profile> "task"` waits and prints the answer.
   - Long task, or your shell tool has a timeout: `harness start <profile> "task"`
     prints a run id at once; the agent keeps working in the background.
     Collect it with `harness wait <run-id> --timeout 5m` (repeat while it
     exits 7) or check with `harness result <run-id>`.
7. Follow up in the same harness session: `harness send <run-id> "more"`
   (add `--detach` to run the turn in the background).

## Harness definition (`harness def set <name>`)

```json
{
  "description": "one line: what this is",
  "command": "pi",
  "args": [["-p"], ["--model", "{model}"], ["{no_mcp?--no-mcp}"], ["{*extra_args}"]],
  "stdin": "prompt",
  "session": "mint",
  "output": {"format": "text"},
  "failures": [{"match": "(?i)rate.?limit|\\b429\\b", "outcome": "throttled"}]
}
```

| Field | Meaning |
|---|---|
| `command` | Executable, found on PATH. |
| `args` | Argument **groups**. A group is passed only if every placeholder in it has a value, so `["--model", "{model}"]` disappears when the profile sets no model. |
| `stdin` | `"prompt"` (default) writes the prompt to the CLI's stdin; `"system+prompt"` writes the profile body first as an `<instructions>` block, for a CLI with no system-prompt option; `"none"` gives it no input. |
| `env` | Extra environment variables; values may use placeholders. |
| `session` | `"mint"`: harness picks a UUID per session and passes it through `{session}` / `{new_session}` / `{resume_session}`. Omit it if the CLI cannot take a caller-chosen id, and capture the id from the output instead. |
| `output` | Where the answer is; see below. |
| `failures` | Regex rules over a failed attempt's own stderr and stdout, first match wins. Outcomes: `transient` (the run did not take effect: safe to repeat), `throttled`, `uncertain` (it may have done work), `permanent`. Unmatched failures are permanent. |

Unknown fields are rejected, so typos fail at `def set`.

### Output

| `format` | Answer | Also |
|---|---|---|
| `"text"` | all of stdout | |
| `"json"` | `field`: dotted path in one JSON document | `error_field` (truthy marks failure), `session_field` (captures the session id) |
| `"jsonl"` | `answer`: a selector over one JSON event per line | `session`, `error` selectors |

A **selector** is `{"where": {"type": "text"}, "field": "part.text", "pick": "join"}`:
events whose dotted paths equal the `where` values, the value at `field`, and
`pick` `"last"` (default), `"first"` or `"join"` (all, joined with `join`,
default empty). A path may index arrays, and `-1` is the last element
(`message.content.-1.text`). Lines that are not JSON are skipped. Any event
matching the `error` selector (with a non-empty `field`, if one is given)
marks the run failed. Example, for `opencode run --format json`:

```json
"output": {
  "format": "jsonl",
  "answer": {"where": {"type": "text"}, "field": "part.text", "pick": "join"},
  "session": {"field": "sessionID", "pick": "first"},
  "error": {"where": {"type": "error"}}
}
```

### Metrics

`output.metrics` records numbers with every turn, for `harness stats`,
`harness compare` and `--json`: tokens, cost, model calls, tool calls,
whatever the CLI reports. Each is a selector (in `json` output, over the
one document). Two picks are for metrics only: `"sum"` adds up every match,
`"count"` counts matches (no `field` needed). A `field` may add several paths
with `+`. Every attempt's metrics count, a failed one's too.

```json
"metrics": {
  "input_tokens": {"where": {"type": "step_finish"}, "field": "part.tokens.input+part.tokens.cache.read", "pick": "sum"},
  "cost_usd": {"where": {"type": "step_finish"}, "field": "part.cost", "pick": "sum"},
  "turns": {"where": {"type": "step_finish"}, "pick": "count"}
}
```

Use these names, so profiles on different harnesses compare:
`input_tokens` (every prompt token, cached ones included), `cached_tokens`,
`output_tokens`, `cost_usd` (only as the CLI reports it; harness keeps no
prices), `turns` (model calls) and `tool_calls`. Text output has no metrics:
prefer a CLI's JSON mode (the examples below all use one).

### Placeholders

| Form | Expands to |
|---|---|
| `{name}` | the value of `name` |
| `{name?text}` | `text` if `name` is truthy (anything but empty, false, 0, no, off) |
| `{*name}` | the value split on whitespace into several arguments (whole element only) |
| `{{`, `}}` | literal braces |

Values come from the profile header, from `--set key=value`, and from these
built-ins: `{prompt}`, `{prompt_file}`, `{system_file}` (the profile body,
when present), `{session}`, `{new_session}` (first turn only),
`{resume_session}` (follow-ups only), `{cwd}`, `{run_id}`, `{run_dir}`,
`{harness_home}` (the data folder) and `{lineage}` (the gate tickets of this
turn and its ancestors). The last three exist so a definition can hand
harness's environment (`HARNESS_RUN`, `HARNESS_HOME`, `HARNESS_LINEAGE`) to a
CLI that strips it; see the Codex example.

In JSON, a regex word boundary is `\\b`: a plain `\b` is a backspace, and
`def set` refuses it.

**Keep user text out of arguments.** Send the prompt on stdin (the default)
or as `{prompt_file}`, and the system prompt as `{system_file}`. On Windows,
npm-installed CLIs are `.cmd` shims, and `cmd.exe` re-parses arguments:
quotes, `&`, `%` and `|` inside a prompt passed as `{prompt}` can break or
inject.

## Profile (`harness profile set <name>`)

```markdown
---
harness: pi
description: Read-only code reviewer
model: openai-codex/gpt-6.1-sol
thinking: high
tools: read,grep,find,ls
no_mcp: true
effect: idempotent
retry: transient x2
timeout: 20m
budget: pi=1
---
You are a senior code reviewer. ...
```

The body is the system prompt (`{system_file}`). Reserved header keys set the
execution policy (HertaSDK); every other key is a placeholder value.

| Key | Values | Meaning |
|---|---|---|
| `harness` | definition name | required |
| `effect` | `pure`, `idempotent`, `non-idempotent` | required: what repeating a run does. Use `idempotent` only if the agent cannot change anything (read-only tools); anything that can edit files or run commands is `non-idempotent`. |
| `retry` | `transient x2`, `transient,uncertain x3`, `none` | which outcomes are retried, total attempts. `uncertain` is refused for `non-idempotent` profiles: a run that may have edited files is never replayed blind. |
| `timeout` | `20m`, `1h`, `none` | per attempt; on expiry the whole process tree is killed and the outcome is `uncertain` |
| `budget` | `pi=1, gpu=4` | units claimed from named budgets while the turn runs |
| `admission` | `wait` (default), `reject` | when a budget or key is taken: queue, or fail at once as `throttled` |
| `serialize` | template, e.g. `{cwd}` | turns with the same key run one at a time |
| `description` | text | shown in `profile list` |

Update single keys without rewriting: `harness profile set <name> thinking=xhigh`
(an empty value removes the key).

## Choosing a harness, model and settings

No harness setup is best for every model and task. A controlled study of
coding-agent harnesses (Fan et al., *An Empirical Study of Harness Design
for Coding Agents*, arXiv:2609.20804, 2026) varied one component at a time
across four models and two benchmarks. Its findings, as starting points:

- **Tools.** Structured file tools (read, edit, grep) helped a weaker model
  a lot (+15 points of success). The strongest model did better with bash
  alone, and cost 30–53% less. For one model the answer flipped with the
  task: bash alone did better on command-line tasks, the structured tools
  on repository bug fixes. So: for a
  strong model on shell-heavy work, try a bash-only tool set; for a weaker
  model, or careful multi-file edits, give it the harness's file tools.
- **Planning.** An explicit plan raised the weakest model's success by 11.6
  points. For strong models it mostly cut cost (about 30%), at a small
  accuracy loss. Turn on a harness's plan mode or todo tool for weaker or
  cheaper models; leave it off for strong ones unless the task is long.
- **Context.** Context management mattered a great deal with a 32k window
  and little at 128k. Every harness here compacts its own context; what is
  yours to manage is the task: give an agent with a small window a smaller
  task, and keep follow-ups in one session with `send` only while it fits.

The study used open models in its own harness, not these CLIs, so treat
these as hypotheses for your setup and check them with your own runs:

1. Give runs a label: `--kind review`, `--kind refactor`.
2. Try candidates on the same task: `harness compare a,b,c "task"` prints
   each answer, then outcome, time, tokens and cost side by side.
3. Look at the record: `harness stats` (or `--kind review`) lists, per
   profile and model, success rate, failures by outcome, median and p90
   time, attempts, tokens and cost per turn, and cost per success.

Prefer the profile with the best success and cost for that kind of task,
and say why when you report it.

## Budgets and keys hold across every harness process

`budget` and `serialize` are enforced machine-wide: every `harness` process
(yours, another agent's, a background worker) queues on the same ledger in the
data folder, first come first served, with Herta's rules (a request is granted
when it fits and nothing is queued ahead of it). A crashed process's units are
reclaimed automatically. `harness status` shows what is held and queued.

**Nested runs.** An agent that harness started can run harness itself. Its
runs know their parent run and their ancestors' tickets: they skip the queue
for a budget an ancestor holds, and fail at once, rather than wait forever, if
the budget cannot fit them until those ancestors finish. harness passes
`HARNESS_RUN`, `HARNESS_LINEAGE` and `HARNESS_HOME` to every agent; when a
harness strips them from its agent's commands, the nested run walks up its
process ancestors to the run executing it instead, so this works with no
setup. `harness status`, run inside an agent, prints the run it belongs to
and how it was found.

## Machine limits

`harness config` lists the limits for every run on the machine;
`harness config set <key> <n>` changes one.

- `max_depth` (default 3): how deep runs may nest. A run started by an agent
  that harness launched is one level below that agent's run; a run that
  would go deeper than `max_depth` is refused (exit 3) with the command to
  raise it. Do the work yourself rather than nest further.
- `max_running` (default 16, 0 for no cap): turns running at once, across
  every profile. A top-level turn waits for a slot like any budget. A nested
  turn does not wait (its ancestors hold slots while they wait for it): when
  every slot is taken it fails at once as `throttled` (exit 5).
- `retention_days` (default 30, 0 to keep everything): finished runs, with
  their prompts and answers, are removed this many days after their last
  turn. harness does this at most once a day as runs start;
  `harness gc [--days n] [--dry-run]` does it now.
- `repeat_warn` (default 3) and `repeat_refuse` (default 5), 0 to turn either
  off: when the same request fails that many times in a row, counting the
  failures of the last hour, harness adds a `hint` telling you to change
  approach, and then refuses it (exit 3), including turns already queued.
  A request is the profile and its definition as they are now (editing
  either makes a new request), the folder, the prompt, the `--set` values
  and the parent run; for `send`, the run instead of the parent. A success
  clears the failures; throttled and cancelled turns do not count; a
  background turn whose worker died does. Do not loop on a failing request:
  read the error, change the prompt, the profile or the plan.

## Per-project profiles

A project can keep definitions and profiles in a `.harness` folder at or
above its working folder (`.harness/harnesses/*.json`, `.harness/profiles/*.md`);
they take precedence over the global ones there. Write them with
`--project`: `harness profile set <name> --project --file -`.

A project file is used only once it is **trusted**: a definition is a command
line and a profile picks tools and flags, so a cloned repository must not be
able to run or escalate anything silently. Files harness writes are trusted as
they are saved. Anything else, and any change, is refused until you read it
and run `harness trust --approve` (`harness trust` lists the state of each
file).

## Running

```
harness run <profile> [prompt words | -] [--task T]... [--file F]... [--cwd D] [--set k=v]... [--kind K] [--json] [--dry-run]
harness start <profile> ...           same flags; returns run ids at once
harness compare <p1>,<p2>[,...] [prompt words | -] [--file F]... [--cwd D] [--set k=v]... [--kind K] [--json]
harness stats [--profile P] [--kind K] [--days N] [--json]
harness send <run-id> [prompt words | -] [--file F]... [--set k=v]... [--json] [--dry-run] [--detach]
harness wait <run-id> [--timeout D] [--json]     harness result <run-id> [--json]
harness stop <run-id>                 harness runs [--limit N]     harness show <run-id>     harness status
```

- The prompt is the words after the profile, or stdin with `-`. Each
  `--file` is appended to it as a fenced block. Put `--` before a prompt
  that starts with `-`.
- Each `--task` starts its own run; tasks run concurrently, admitted against
  the profile's budgets.
- `--cwd` sets the agent's working folder (default: the current one) and the
  project whose profiles apply.
- `--kind` labels runs (lower-case, such as `review`) so `harness stats` can
  compare profiles per kind of task.
- `compare` runs the same task on each profile at once, prints each answer,
  then a table of outcome, time, attempts, tokens and cost. Every agent in a
  compare works in the same folder at the same time, so compare read-only
  profiles. To compare agents that edit files, give each its own checkout
  and use `harness run --kind <label> --cwd <checkout>` per profile instead;
  `harness stats --kind <label>` then compares them.
- `--json` prints `{run, turn, profile, harness, model, kind, outcome, exit_code, attempts, session, duration_ms, metrics, output, error, hint}`
  (an array for several tasks, and for `compare`).
- A `hint` (also printed as `harness: hint: ...`) is advice to act on, such
  as the repeat guard's.
- Every run is kept in `<home>/runs/<run-id>/` for `retention_days` after
  its last turn: `run.json`, each turn's prompt, answer (`turn-N.out.md`),
  raw stdout/stderr, and `worker.log` for background turns.
- A turn's duration includes any time spent waiting for its budget.

| Exit code | Outcome |
|---:|---|
| 0 | success |
| 2 | bad command line, profile or definition |
| 3 | permanent (also: nested deeper than `max_depth`; a request refused by the repeat guard) |
| 4 | transient |
| 5 | throttled (also: the run is busy with another turn; a nested turn found every `max_running` slot taken) |
| 6 | uncertain |
| 7 | still running (`wait --timeout`, `result`) |
| 130 | cancelled (Ctrl-C, SIGTERM, `harness stop`) |

A failed first turn keeps its session: `harness send` can continue it. A
background turn whose worker died is reported as `uncertain` by `wait`.

## Verified examples

Definitions known to work, with the CLI version they were checked against.
Start from one, then check the flags against the installed CLI's `--help`.

**Pi** (1.0.4), print mode with JSON events (`--mode json`). Each model
call ends with a `turn_end` event carrying its usage and cost; the answer is
the last part of the final message:

```json
{"command": "pi",
 "args": [["-p"], ["--mode", "json"], ["--model", "{model}"], ["--thinking", "{thinking}"], ["--tools", "{tools}"],
          ["{no_mcp?--no-mcp}"], ["--append-system-prompt", "{system_file}"], ["--session-id", "{session}"]],
 "session": "mint",
 "output": {"format": "jsonl",
   "answer": {"where": {"type": "turn_end"}, "field": "message.content.-1.text"},
   "error": {"where": {"type": "turn_end", "message.stopReason": "error"}},
   "metrics": {
     "input_tokens": {"where": {"type": "turn_end"}, "field": "message.usage.input+message.usage.cacheRead+message.usage.cacheWrite", "pick": "sum"},
     "cached_tokens": {"where": {"type": "turn_end"}, "field": "message.usage.cacheRead", "pick": "sum"},
     "output_tokens": {"where": {"type": "turn_end"}, "field": "message.usage.output", "pick": "sum"},
     "cost_usd": {"where": {"type": "turn_end"}, "field": "message.usage.cost.total", "pick": "sum"},
     "turns": {"where": {"type": "turn_end"}, "pick": "count"},
     "tool_calls": {"where": {"type": "tool_execution_end"}, "pick": "count"}}},
 "failures": [{"match": "(?i)\\b429\\b|rate.?limit|too many requests", "outcome": "throttled"},
              {"match": "(?i)ECONNRESET|ETIMEDOUT|fetch failed", "outcome": "uncertain"}]}
```

`--tools` keeps MCP tools; a read-only profile also needs `no_mcp: true`.
Without `--mode json`, Pi prints only the answer (`"output": {"format": "text"}`),
and reports no tokens or cost.

**Claude Code** (2.1.220), one JSON result:

```json
{"command": "claude",
 "args": [["-p"], ["--output-format", "json"], ["--model", "{model}"], ["--tools", "{tools}"],
          ["--append-system-prompt-file", "{system_file}"],
          ["--session-id", "{new_session}"], ["--resume", "{resume_session}"]],
 "session": "mint",
 "output": {"format": "json", "field": "result", "error_field": "is_error", "session_field": "session_id",
   "metrics": {
     "input_tokens": {"field": "usage.input_tokens+usage.cache_read_input_tokens+usage.cache_creation_input_tokens"},
     "cached_tokens": {"field": "usage.cache_read_input_tokens"},
     "output_tokens": {"field": "usage.output_tokens"},
     "cost_usd": {"field": "total_cost_usd"},
     "turns": {"field": "num_turns"}}},
 "failures": [{"match": "(?i)rate.?limit|overloaded|\\b429\\b", "outcome": "throttled"}]}
```

**Codex** (0.162.1), JSONL events. Codex may strip its commands'
environment (`shell_environment_policy`), so harness's variables are set
through it; `exec resume` continues a session. Codex has no option that adds
to its instructions from a file, so the profile body goes at the top of the
prompt:

```json
{"command": "codex",
 "args": [["exec"], ["-m", "{model}"], ["-c", "model_reasoning_effort={reasoning}"], ["-s", "{sandbox}"],
          ["-c", "shell_environment_policy.set.HARNESS_RUN={run_id}"],
          ["-c", "shell_environment_policy.set.HARNESS_LINEAGE={lineage}"],
          ["-c", "shell_environment_policy.set.HARNESS_HOME={harness_home}"],
          ["--skip-git-repo-check"], ["--json"], ["resume", "{resume_session}"], ["-"]],
 "stdin": "system+prompt",
 "output": {"format": "jsonl",
   "answer": {"where": {"type": "item.completed", "item.type": "agent_message"}, "field": "item.text"},
   "session": {"where": {"type": "thread.started"}, "field": "thread_id", "pick": "first"},
   "error": {"where": {"type": "turn.failed"}, "field": "error.message"},
   "metrics": {
     "input_tokens": {"where": {"type": "turn.completed"}, "field": "usage.input_tokens", "pick": "sum"},
     "cached_tokens": {"where": {"type": "turn.completed"}, "field": "usage.cached_input_tokens", "pick": "sum"},
     "output_tokens": {"where": {"type": "turn.completed"}, "field": "usage.output_tokens", "pick": "sum"},
     "tool_calls": {"where": {"type": "item.completed", "item.type": "command_execution"}, "pick": "count"}}},
 "failures": [{"match": "(?i)rate.?limit|\\b429\\b|usage limit", "outcome": "throttled"}]}
```

Codex reports tokens but no cost. A Codex agent that runs harness needs
`sandbox: danger-full-access` (harness writes to its data folder, and the
agents it starts need the network).

**OpenCode** (1.18.35), JSONL events, session captured from the stream; no
system-prompt option, so the profile body goes at the top of the prompt:

```json
{"command": "opencode",
 "args": [["run"], ["--format", "json"], ["--model", "{model}"], ["--session", "{resume_session}"]],
 "stdin": "system+prompt",
 "output": {"format": "jsonl",
   "answer": {"where": {"type": "text"}, "field": "part.text", "pick": "join"},
   "session": {"field": "sessionID", "pick": "first"},
   "error": {"where": {"type": "error"}},
   "metrics": {
     "input_tokens": {"where": {"type": "step_finish"}, "field": "part.tokens.input+part.tokens.cache.read+part.tokens.cache.write", "pick": "sum"},
     "cached_tokens": {"where": {"type": "step_finish"}, "field": "part.tokens.cache.read", "pick": "sum"},
     "output_tokens": {"where": {"type": "step_finish"}, "field": "part.tokens.output", "pick": "sum"},
     "cost_usd": {"where": {"type": "step_finish"}, "field": "part.cost", "pick": "sum"},
     "turns": {"where": {"type": "step_finish"}, "pick": "count"}}}}
```

## Other commands

`harness def show|list|rm`, `harness profile show|list|rm` (with `--project`
and `--cwd`), `harness budget set|list|rm`, `harness config [set <key> <n>]`,
`harness gc [--days n] [--dry-run]`, `harness stats`, `harness compare`,
`harness trust [--approve]`,
`harness home` (data folder; `HARNESS_HOME` overrides it), `harness skill`
(the skill file for parent harnesses), `harness version`.
