# harness: agent guide

`harness` runs agents that live in other harnesses (Pi, Claude Code, Codex,
OpenCode, ...) as your subagents. It calls each harness's own headless CLI
directly, waits for the answer, and prints it. Nothing about any harness is
built in: you describe a harness once as JSON (a **definition**) and an agent
built on it as Markdown (a **profile**). Read this guide, then use the
commands below.

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
6. Run it: `harness run <profile> "task"`. The answer is printed on stdout;
   a status line goes to stderr.
7. Follow up in the same harness session: `harness send <run-id> "more"`.

Runs can take many minutes. If your shell tool has a timeout, run `harness`
as a background command when you can, and wait for it to finish.

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
| `stdin` | `"prompt"` (default) writes the prompt to the CLI's stdin; `"none"` gives it no input. |
| `env` | Extra environment variables; values may use placeholders. |
| `session` | `"mint"`: harness picks a UUID per session and passes it through `{session}` / `{new_session}` / `{resume_session}`. Omit if the CLI cannot take a caller-chosen id. |
| `output` | `{"format": "text"}`: the answer is all of stdout. `{"format": "json", "field": "result", "error_field": "is_error", "session_field": "session_id"}`: stdout is one JSON document; `field` is a dotted path to the answer, `error_field` marks failure when truthy, `session_field` captures the session id. |
| `failures` | Regex rules over a failed run's stderr and stdout, first match wins. Outcomes: `transient` (the run did not take effect: safe to repeat), `throttled`, `uncertain` (it may have done work), `permanent`. Unmatched failures are permanent. |

Unknown fields are rejected, so typos fail at `def set`.

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
`{resume_session}` (follow-ups only), `{cwd}`, `{run_id}`, `{run_dir}`.

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
| `budget` | `pi=1, gpu=4` | units claimed from named budgets while the run executes |
| `admission` | `wait` (default), `reject` | when the budget is full: queue, or fail at once as `throttled` |
| `serialize` | template, e.g. `{cwd}` | runs with the same key run one at a time |
| `description` | text | shown in `profile list` |

Update single keys without rewriting: `harness profile set <name> thinking=xhigh`
(an empty value removes the key).

## Running

```
harness run <profile> [prompt words | -] [--task T]... [--file F]... [--cwd D] [--set k=v]... [--json] [--dry-run]
harness send <run-id> [prompt words | -] [--file F]... [--set k=v]... [--json] [--dry-run]
harness runs [--limit N]        harness show <run-id>
```

- The prompt is the words after the profile, or stdin with `-`. Each
  `--file` is appended to it as a fenced block. Put `--` before a prompt
  that starts with `-`.
- Each `--task` starts its own run; tasks run concurrently, admitted against
  the profile's budgets.
- `--cwd` sets the agent's working folder (default: the current one).
- `--json` prints `{run, turn, profile, harness, outcome, exit_code, attempts, session, duration_ms, output, error}`
  (an array for several tasks).
- Every run is kept in `<home>/runs/<run-id>/`: `run.json`, each turn's
  prompt, answer (`turn-N.out.md`) and raw stdout/stderr.

| Exit code | Outcome |
|---:|---|
| 0 | success |
| 2 | bad command line, profile or definition |
| 3 | permanent |
| 4 | transient |
| 5 | throttled |
| 6 | uncertain |
| 130 | cancelled |

A run that is busy with another turn refuses a second one as `throttled`.
A failed first turn keeps its session: `harness send` can continue it.

## Other commands

`harness def show|list|rm`, `harness profile show|list|rm`,
`harness budget set|list|rm`, `harness home` (data folder; `HARNESS_HOME`
overrides it), `harness skill` (the skill file for parent harnesses),
`harness version`.
