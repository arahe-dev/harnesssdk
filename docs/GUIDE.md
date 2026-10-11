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
default empty). Lines that are not JSON are skipped. Any event matching the
`error` selector (with a non-empty `field`, if one is given) marks the run
failed. Example, for `opencode run --format json`:

```json
"output": {
  "format": "jsonl",
  "answer": {"where": {"type": "text"}, "field": "part.text", "pick": "join"},
  "session": {"field": "sessionID", "pick": "first"},
  "error": {"where": {"type": "error"}}
}
```

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
harness run <profile> [prompt words | -] [--task T]... [--file F]... [--cwd D] [--set k=v]... [--json] [--dry-run]
harness start <profile> ...           same flags; returns run ids at once
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
- `--json` prints `{run, turn, profile, harness, outcome, exit_code, attempts, session, duration_ms, output, error}`
  (an array for several tasks).
- Every run is kept in `<home>/runs/<run-id>/`: `run.json`, each turn's
  prompt, answer (`turn-N.out.md`), raw stdout/stderr, and `worker.log` for
  background turns.
- A turn's duration includes any time spent waiting for its budget.

| Exit code | Outcome |
|---:|---|
| 0 | success |
| 2 | bad command line, profile or definition |
| 3 | permanent |
| 4 | transient |
| 5 | throttled (also: the run is busy with another turn) |
| 6 | uncertain |
| 7 | still running (`wait --timeout`, `result`) |
| 130 | cancelled (Ctrl-C, SIGTERM, `harness stop`) |

A failed first turn keeps its session: `harness send` can continue it. A
background turn whose worker died is reported as `uncertain` by `wait`.

## Verified examples

Definitions known to work, with the CLI version they were checked against.
Start from one, then check the flags against the installed CLI's `--help`.

**Pi** (1.0.4), print mode, answer on stdout:

```json
{"command": "pi",
 "args": [["-p"], ["--model", "{model}"], ["--thinking", "{thinking}"], ["--tools", "{tools}"],
          ["{no_mcp?--no-mcp}"], ["--append-system-prompt", "{system_file}"], ["--session-id", "{session}"]],
 "session": "mint", "output": {"format": "text"},
 "failures": [{"match": "(?i)\\b429\\b|rate.?limit|too many requests", "outcome": "throttled"},
              {"match": "(?i)ECONNRESET|ETIMEDOUT|fetch failed", "outcome": "uncertain"}]}
```

`--tools` keeps MCP tools; a read-only profile also needs `no_mcp: true`.

**Claude Code** (2.1.220), one JSON result:

```json
{"command": "claude",
 "args": [["-p"], ["--output-format", "json"], ["--model", "{model}"], ["--tools", "{tools}"],
          ["--append-system-prompt-file", "{system_file}"],
          ["--session-id", "{new_session}"], ["--resume", "{resume_session}"]],
 "session": "mint",
 "output": {"format": "json", "field": "result", "error_field": "is_error", "session_field": "session_id"},
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
   "error": {"where": {"type": "turn.failed"}, "field": "error.message"}},
 "failures": [{"match": "(?i)rate.?limit|\\b429\\b|usage limit", "outcome": "throttled"}]}
```

A Codex agent that runs harness needs `sandbox: danger-full-access` (harness
writes to its data folder, and the agents it starts need the network).

**OpenCode** (1.18.35), JSONL events, session captured from the stream; no
system-prompt option, so the profile body goes at the top of the prompt:

```json
{"command": "opencode",
 "args": [["run"], ["--format", "json"], ["--model", "{model}"], ["--session", "{resume_session}"]],
 "stdin": "system+prompt",
 "output": {"format": "jsonl",
   "answer": {"where": {"type": "text"}, "field": "part.text", "pick": "join"},
   "session": {"field": "sessionID", "pick": "first"},
   "error": {"where": {"type": "error"}}}}
```

## Other commands

`harness def show|list|rm`, `harness profile show|list|rm` (with `--project`
and `--cwd`), `harness budget set|list|rm`, `harness trust [--approve]`,
`harness home` (data folder; `HARNESS_HOME` overrides it), `harness skill`
(the skill file for parent harnesses), `harness version`.
