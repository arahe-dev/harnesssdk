# HarnessSDK

**Run agents that live in other harnesses as your subagents, through their
own headless CLIs, with HertaSDK as the execution policy.**

A Claude Code agent can hand a review to a Pi agent on GPT-6.1 Sol, a Codex
agent can delegate to Claude, and so on, without leaving its own harness.
Delegation is one shell command, `harness run <profile> "task"` (or
`harness start` for long tasks), so the parent keeps its own loop, tools,
prompts and context. The child runs in its own harness's native one-shot mode
(`pi -p`, `claude -p`, `opencode run`, ...).

> **Status: v1.1.0.** Used on Windows 11 with Pi 1.0.4, Claude Code 2.1.220
> and OpenCode 1.18.35. The test suite passes in CI on Ubuntu and Windows; no
> real harness CLI has been run on Linux or macOS yet. See
> [Limits](#limits) and the [CHANGELOG](CHANGELOG.md).

## One layer, nothing hardcoded

- **No adapters or protocols.** No ACP, no RPC modes, no MCP server. One
  process starts the harness CLI, feeds it the prompt, reads the answer.
- **No harness-specific code.** How to call a CLI is a JSON **definition**;
  an agent built on it is a Markdown **profile**. Agents create both by
  running `harness` commands, after reading the CLI's own `--help`.
- **HertaSDK owns execution policy.** Each profile is a Herta operation with a
  declared effect, budgets, admission, per-attempt timeout and retry table,
  checked when the profile is saved: a profile that could replay a
  half-finished file-editing run is rejected before it ever runs. Budgets and
  serialize keys hold across every harness process on the machine.

## Example: a Pi GPT-6.1 Sol reviewer

You tell your agent: *"launch a pi 6.1 sol review agent on this diff"*. With
the `harness` skill installed, it reads `harness guide` and runs:

```sh
harness def set pi --file - <<'EOF'
{
  "description": "Pi coding agent, one-shot print mode (pi -p)",
  "command": "pi",
  "args": [["-p"], ["--model", "{model}"], ["--thinking", "{thinking}"],
           ["--tools", "{tools}"], ["{no_mcp?--no-mcp}"],
           ["--append-system-prompt", "{system_file}"], ["--session-id", "{session}"]],
  "session": "mint",
  "output": {"format": "text"},
  "failures": [{"match": "(?i)\\b429\\b|rate.?limit|too many requests", "outcome": "throttled"}]
}
EOF
harness budget set pi 2
harness profile set review --file - <<'EOF'
---
harness: pi
description: Read-only code reviewer on GPT-6.1 Sol
model: openai-codex/gpt-6.1-sol
thinking: high
tools: read,grep,find,ls
no_mcp: true
effect: idempotent
retry: transient x2
timeout: 20m
budget: pi=1
---
You are a senior code reviewer. Find correctness bugs, security issues and
missing tests. Rank findings by severity with file:line. No style nits.
EOF
git diff main > /tmp/change.diff
harness start review "Review this change." --file /tmp/change.diff   # prints a run id
harness wait <run-id> --timeout 5m                                   # exit 7: still running
harness send <run-id> "Which finding would you fix first, and how?"
```

The second time, it is just the last three lines.

## Install

```sh
go install github.com/arahe-dev/harnesssdk/cmd/harness@latest   # or: go install ./cmd/harness
mkdir -p ~/.claude/skills/harness ~/.agents/skills/harness
harness skill > ~/.claude/skills/harness/SKILL.md                # Claude Code
harness skill > ~/.agents/skills/harness/SKILL.md                # Codex, Pi
```

`harness guide` is the full manual: definition and profile formats,
placeholders, output rules, policy keys, exit codes. Data lives in
`~/.harness` (`HARNESS_HOME` overrides it).

## Concepts

| | |
|---|---|
| **Definition** (`harnesses/<name>.json`) | Command, argument groups with placeholders (`{model}`, `{flag?--x}`, `{*extra}`), prompt on stdin, where the answer is (all of stdout, a JSON field, or selectors over a JSONL event stream), session handling, failure rules (regex → transient / throttled / uncertain / permanent). |
| **Profile** (`profiles/<name>.md`) | Header: harness, placeholder values, and the Herta policy (`effect`, `retry`, `timeout`, `budget`, `admission`, `serialize`). Body: the system prompt. |
| **Project** (`.harness/` in a repository) | Definitions and profiles for one project, taking precedence there. Used only once trusted; any change needs trusting again. |
| **Budget** (`budgets.json`) | A named capacity, e.g. `pi: 2`, enforced across every harness process through a FIFO ledger in the data folder. Nested runs never deadlock on their ancestors. |
| **Run** (`runs/<id>/`) | One delegated agent: its session, every turn's prompt and answer, raw stdout/stderr. `run` waits; `start` returns at once and a background worker finishes the turn; `wait`, `result`, `stop` and `send` work on it from any later command. |

How a run ends maps to Herta outcomes and exit codes: 0 success, 3 permanent,
4 transient, 5 throttled, 6 uncertain, 7 still running, 130 cancelled (2 for
usage errors). A timeout kills the whole process tree (a Job Object on
Windows, falling back to a parent-pid walk; a process group on Unix) and is
`uncertain`: the agent may already have done work.

## Limits

- **No real harness CLI has run on Linux or macOS yet.** The full test suite,
  including process-group kill and SIGTERM handling, passes in CI on Ubuntu.
- **Budgets are per machine**, not across machines: HertaSDK, and harness on
  top of it, stay local by design.
- **Pi on some networks** ends long runs with `fetch failed`; the turn is
  `uncertain`, and `harness send` resumes the same session.
- On Windows, background workers are started outside the starting command's
  Job Object (by breaking away, or through WMI when the job forbids that), so
  they survive a shell tool that kills its job; this needs PowerShell.
- On Windows, if the agent cannot be put in a Job Object, its process tree is
  tracked by parent process ids instead: best effort, since a process that
  starts a child and exits within 200 ms can hide that child. The attempt's
  log says when this happens.
- A process that leaves its process group or job on purpose (a daemon that
  calls `setsid`) is outside the tree and is not stopped with it.
- Programs that embed the Go package must call `harnesssdk.Subcommand()`
  first in `main`: harness starts its own executable as a detached worker and,
  on Unix, as the guard of each agent's process tree.

## What HarnessSDK is not

Not an orchestrator or planner (the parent agent plans), not a wire protocol,
not a daemon (a background worker lives for one turn), not a workflow engine,
not a sandbox: a child agent runs with the permissions its harness gives it.

## Development

```sh
go test ./...          # end-to-end tests run a fake harness: no tokens spent
go test -race ./...
```

Depends only on the Go standard library and
[HertaSDK](https://github.com/arahe-dev/hertasdk) v1.0.0-next.

## License

[Apache-2.0](LICENSE)
