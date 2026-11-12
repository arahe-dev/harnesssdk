# HarnessSDK

**Run agents that live in other harnesses as your subagents, through their
own headless CLIs, with HertaSDK as the execution policy.**

A Claude Code agent can hand a review to a Pi agent on GPT-6.1 Sol, a Codex
agent can delegate to Claude, and so on, without leaving its own harness.
Delegation is one shell command, `harness run <profile> "task"` (or
`harness start` for long tasks), so the parent keeps its own loop, tools,
prompts and context. The child runs in its own harness's native one-shot mode
(`pi -p`, `claude -p`, `opencode run`, ...).

> **Status: v2.0.0-next.2**, the v2 candidate (module `github.com/arahe-dev/harnesssdk/v2`;
> v1.7.0 is the stable v1). Used on Windows 11 with Pi 1.0.4, Claude Code 2.1.220,
> OpenCode 1.18.35, Codex 0.162.1 and Hermes Agent 0.20.5, including a Codex agent that starts a Pi
> agent through harness, and on Linux with Claude Code 2.1.322 (streaming
> events, questions between agents and people, handover). The test suite
> passes in CI on Ubuntu and Windows; no real harness CLI has been run on
> macOS yet. See [Limits](#limits) and the [CHANGELOG](CHANGELOG.md).

## One layer, nothing hardcoded

- **No adapters or protocols.** No ACP, no RPC modes, no MCP server. One
  process starts the harness CLI, feeds it the prompt, reads the answer.
- **No harness-specific code.** How to call a CLI is a JSON **definition**;
  an agent built on it is a Markdown **profile**. Agents create both by
  running `harness` commands, after reading the CLI's own `--help`.
- **HertaSDK owns the execution contract.** Each profile is a Herta operation
  with a declared effect and retry table, checked when the profile is saved:
  a profile that could replay a half-finished file-editing run is rejected
  before it ever runs. Budgets and serialize keys are arbitrated once, across
  every harness process on the machine, with Herta's budget rules. Where Herta
  cannot yet express something an agent needs (backoff, retrying throttled
  turns, a deadline that can be extended), harness does it beside Herta,
  under the same rules.

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
harness wait <run-id> --timeout 5m                                   # exit 7: still running, and what it is doing
harness send <run-id> "Which finding would you fix first, and how?"
```

The second time, it is just the last three lines.

## Install

```sh
go install github.com/arahe-dev/harnesssdk/v2/cmd/harness@latest   # or: go install ./cmd/harness
mkdir -p ~/.claude/skills/harness ~/.agents/skills/harness
harness skill > ~/.claude/skills/harness/SKILL.md                # Claude Code
harness skill > ~/.agents/skills/harness/SKILL.md                # Codex, Pi
```

`harness guide` is the full manual: definition and profile formats,
placeholders, output rules, policy keys, exit codes. Data lives in
`~/.harness` (`HARNESS_HOME` overrides it).

## Go API

The root package is the same engine for Go programs, with the same data
folder, profiles and machine-wide limits as the command:

```go
func main() {
	if code, ok := harnesssdk.Subcommand(); ok { // background workers and process guards
		os.Exit(code)
	}
	c, err := harnesssdk.Open(harnesssdk.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close(context.Background())
	res, err := c.Run(ctx, harnesssdk.Request{Profile: "pr-review", Prompt: "Review the diff on this branch."})
	if err != nil {
		log.Fatal(err) // res, unless nil (no run was created), says how the turn ended
	}
	fmt.Println(res.Output)
}
```

`Run`, `Start`, `Send`, `SendStart`, `Wait`, `Result`, `Stop`, `Status`, `Events`, `WaitEvents`, `Runs`,
`Compare`, `Stats`, `SetDefinition`, `SetProfile`, `SetBudget`, `Config`,
`SetConfig`, `GC` and `CheckDefinition` mirror the commands; import it as
`github.com/arahe-dev/harnesssdk/v2`. v2.0.0-next is a pre-release: its API
may still change before v2.0.0. v1 (v1.7.0) follows semantic versioning:
nothing exported is removed or changes meaning within v1. Every file harness
writes for itself carries a format number; a harness refuses a newer format
rather than misread it, and reads files from earlier releases as they are.

## Concepts

| | |
|---|---|
| **Definition** (`harnesses/<name>.json`) | Command, argument groups with placeholders (`{model}`, `{flag?--x}`, `{*extra}`), prompt on stdin, where the answer is (all of stdout, a JSON field, or selectors over a JSONL event stream), session handling, failure rules (regex → transient / throttled / uncertain / permanent). |
| **Profile** (`profiles/<name>.md`) | Header: harness, placeholder values, and the Herta policy (`effect`, `retry`, `timeout`, `budget`, `admission`, `serialize`). Body: the system prompt. |
| **Project** (`.harness/` in a repository) | Definitions and profiles for one project, taking precedence there. Used only once trusted; any change needs trusting again. |
| **Budget** (`budgets.json`) | A named capacity, e.g. `pi: 2`, enforced across every harness process through a FIFO ledger in the data folder. Nested runs (an agent that runs harness itself) know their parent and never deadlock on their ancestors, even under a harness that strips its agents' environment. |
| **Config** (`config.json`) | Machine-wide limits: `max_depth` (3) for how deep runs nest, `max_running` (16) for turns running at once, `retention_days` (30) before finished runs are removed, `repeat_warn` / `repeat_refuse` (3 / 5) for a request that keeps failing the same way, and optional `max_wait_minutes`, `max_queue` and `spend_limit_usd` (dollars per 24 hours, as the CLIs report it), with `spend_unknown` choosing how a cost harness could not record counts (`warn`, `refuse` or dollars). `harness config` lists and sets them. |
| **Checks and worktrees** | A profile's `check` (or `--check`) is a command, such as `go test ./...`, that must pass in the agent's folder for the turn to succeed, so success means the work is right. `--worktree` gives a run its own git worktree and branch, so agents that edit files run side by side and `compare` can set them against each other. |
| **Metrics** (`output.metrics` in a definition) | Numbers read from the CLI's own output with every turn: tokens, cost, model calls, tool calls. `harness stats` sums them up per profile and model (and per `--kind` of task) with success rates and times; `harness compare a,b "task"` runs one task on several profiles side by side. The guide's section on choosing a harness and model turns a 2026 study of harness design into starting points to check this way. |
| **Run** (`runs/<id>/`) | One delegated agent: its session, every turn's prompt and answer, raw stdout/stderr. `run` waits; `start` returns at once and a background worker finishes the turn; `wait`, `result`, `stop` and `send` work on it from any later command. |
| **Time and context** | A profile's `timeout` is per attempt and can be extended while it runs, by the caller or the agent itself (`harness extend <run-id> 30m`, `harness extend --self 30m`); `timeout_soon` warns first and wakes a waiting `wait`. An agent whose CLI streams events is stopped after `idle` (15m by default) without output. Either way the turn is **parked**: its session is kept, and a stuck agent is not started over. When a turn ends with the agent's context past `handover_at` (80%), its hint says so, and `harness handover <run-id>` continues the work in a fresh session from the agent's own handover notes. |
| **Talking with agents** | A run's agent is told, in its system prompt, how to reach whoever started it: `harness note` posts progress, `harness ask` asks and waits for the answer (and `wait` wakes on it; `harness reply` answers), `harness inbox` reads what was posted to it with `harness steer`. What it has not read leads its next turn; `steer --now` stops it and continues the session with the message. All of it is in the run's event log. |
| **Many agents and people** | Agents in different harnesses, and people, `harness join --as <alias>` and get a mailbox; rooms (`room:<name>`) are shared ones. `harness inbox --as <me> --wait` gathers, once each, what others posted to you or your rooms and what your runs (started `--as` you) need from you. `harness ask --to` waits for an answer from anyone; `harness claim` says, advisorily, what you are about to edit; `harness peers` shows who is here; `harness watch` follows it all live for a person. No daemon: presence is using harness. |
| **Bridges** | `bridge: pi-intercom` carries messages to a Pi agent mid-turn through pi-intercom, and its intercom messages back into the run's log. |
| **Events and status** (`runs/<id>/events.jsonl`) | How a caller follows a run while it works, without reading the files its agent edits. The log records each turn's lifecycle (queued, admitted, attempts, retries, check, finished) and, through a definition's `output.progress` rules, the agent's tool calls and messages as the CLI streams them. `harness status <run-id>` sums it up with how full the agent's context is (`context_tokens` against `context_window`), the files changed and what to run next; `harness events --since <cursor> --wait 2m` wakes on what happens next; `wait --timeout` prints the status instead of nothing. |

How a run ends maps to Herta outcomes and exit codes: 0 success, 3 permanent,
4 transient, 5 throttled, 6 uncertain, 7 still running, 130 cancelled (2 for
usage errors). A timeout kills the whole process tree (a Job Object on
Windows, falling back to a parent-pid walk; a process group on Unix) and is
`uncertain`: the agent may already have done work.

## Limits

- **No real harness CLI has run on macOS yet**, and on Linux only Claude
  Code. The full test suite, including process-group kill and SIGTERM
  handling, passes in CI on Ubuntu.
- **The pi-intercom bridge** is tested against a broker speaking
  pi-intercom 0.18's protocol, not yet against Pi itself, and does not
  support Windows yet.
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
- Programs that use the Go API must call `harnesssdk.Subcommand()` first in
  `main`: harness starts its own executable as a detached worker and, on
  Unix, as the guard of each agent's process tree.

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
