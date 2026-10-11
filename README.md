# HarnessSDK

**Run agents that live in other harnesses as your subagents, through their
own headless CLIs, with HertaSDK as the execution policy.**

A Claude Code agent can hand a review to a Pi agent on GPT-6.1 Sol, a Codex
agent can delegate to Claude, and so on, without leaving its own harness.
Delegation is one shell command, `harness run <profile> "task"`, so the parent
keeps its own loop, tools, prompts and context. The child runs in its own
harness's native one-shot mode (`pi -p`, `claude -p`, `codex exec`, ...).

> **Status: v0.1.0.** Built and tested on Windows 11 with Pi 1.0.4 and
> Claude Code 2.1.220; Unix support compiles and is vetted but has not been
> run on Linux or macOS yet. See [What v0 does not do](#what-v0-does-not-do).

## One layer, nothing hardcoded

- **No adapters or protocols.** No ACP, no RPC modes, no MCP server. One
  process starts the harness CLI, feeds it the prompt, reads the answer.
- **No harness-specific code.** How to call a CLI is a JSON **definition**;
  an agent built on it is a Markdown **profile**. Agents create both by
  running `harness` commands, after reading the CLI's own `--help`.
- **HertaSDK owns execution policy.** Each profile is a Herta operation with a
  declared effect, budgets, admission, per-attempt timeout and retry table,
  checked when the profile is saved: a profile that could replay a
  half-finished file-editing run is rejected before it ever runs.

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
harness run review "Review this change." --file /tmp/change.diff
harness send <run-id> "Which finding would you fix first, and how?"
```

The second time, it is just the last two lines.

## Install

```sh
go install github.com/arahe-dev/harnesssdk/cmd/harness@latest   # or: go install ./cmd/harness
mkdir -p ~/.claude/skills/harness ~/.agents/skills/harness
harness skill > ~/.claude/skills/harness/SKILL.md                # Claude Code
harness skill > ~/.agents/skills/harness/SKILL.md                # Codex, Pi
```

`harness guide` is the full manual: definition and profile formats,
placeholders, policy keys, exit codes. Data lives in `~/.harness`
(`HARNESS_HOME` overrides it).

## Concepts

| | |
|---|---|
| **Definition** (`harnesses/<name>.json`) | Command, argument groups with placeholders (`{model}`, `{flag?--x}`, `{*extra}`), prompt on stdin, where the answer is (all of stdout, or a JSON field), session handling, failure rules (regex → transient / throttled / uncertain / permanent). |
| **Profile** (`profiles/<name>.md`) | Header: harness, placeholder values, and the Herta policy (`effect`, `retry`, `timeout`, `budget`, `admission`, `serialize`). Body: the system prompt. |
| **Budget** (`budgets.json`) | A named capacity, e.g. `pi: 2`. Turns in one `harness` process are admitted against it by one Herta runtime. |
| **Run** (`runs/<id>/`) | One delegated agent: its session, every turn's prompt and answer, raw stdout/stderr. `harness send` continues it in the same harness session. |

How a run ends maps to Herta outcomes and exit codes: 0 success, 3 permanent,
4 transient, 5 throttled, 6 uncertain, 130 cancelled (2 for usage errors).
A timeout kills the whole process tree (a Job Object on Windows, a process
group on Unix) and is `uncertain`: the agent may already have done work.

## What v0 does not do

- **Budgets hold within one `harness` process**, e.g. one `run` with several
  `--task`s. Separate concurrent invocations do not yet share a limit
  (planned: OS lock-file slots, plus run lineage to avoid nested deadlock).
- **No detached runs.** `harness run` blocks until the agent finishes. In
  Claude Code, run it as a background command; parents whose shell tools
  time out need a future `start`/`wait`.
- **Global profiles only**; per-project profiles and a trust step for
  project-defined commands are planned.
- **No JSONL output rules yet**, so CLIs that only stream events (OpenCode
  `--format json`, `codex exec --json`) need a text or single-JSON mode.

## What HarnessSDK is not

Not an orchestrator or planner (the parent agent plans), not a wire protocol,
not a daemon, not a workflow engine, not a sandbox: a child agent runs with
the permissions its harness gives it.

## Development

```sh
go test ./...          # end-to-end tests run a fake harness: no tokens spent
go test -race ./...
```

Depends only on the Go standard library and
[HertaSDK](https://github.com/arahe-dev/hertasdk) v1.0.0-next.

## License

[Apache-2.0](LICENSE)
