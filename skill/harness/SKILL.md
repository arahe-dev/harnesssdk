---
name: harness
description: Launch, set up and follow up agents that run in other CLI harnesses (Pi, Claude Code, Codex, OpenCode) as subagents, using reusable profiles, through the `harness` command. Use when the user asks to launch, run, set up or configure an agent, reviewer or subagent in another harness or on another model.
---

# harness

`harness` runs agents in other harnesses through their own headless CLIs and
returns their answers. Agents are named **profiles**; how to call each
harness's CLI is a **definition**. Both are data you create with the command.

Before your first use in a session, run `harness guide` and follow it.

Most requests are one of these:

- **Run an existing agent:** `harness profile list`, then
  `harness run <profile> "task"` (add `--file <path>` for context such as a
  diff). For anything that may take minutes, use `harness start <profile> "task"`
  instead: it returns a run id at once; collect the answer with
  `harness wait <run-id> --timeout 5m` (exit 7 means still running: wait again).
- **Set up a new agent:** read the target CLI's `--help`, write the definition
  if it is missing (`harness def set <name> --file -`), then the profile
  (`harness profile set <name> --file -`, add `--project` for one repository),
  check it with `harness run <profile> --dry-run "test"`, then run it.
- **Follow a running agent:** `harness status <run-id>` says where it is,
  what it is doing and what it has changed; `harness events <run-id> --since <cursor> --wait 2m`
  wakes on what it does next. `harness wait --timeout` prints the status
  when it times out. Use these rather than reading the files it edits.
- **Time and context:** `harness extend <run-id> 30m` gives a running turn
  more time (an agent harness started can run `harness extend --self 30m`).
  A turn stopped by its timeout or idle limit is parked: check it with
  `harness status`, then `harness send`. If a hint says the context is
  nearly full, `harness handover <run-id> --next "<task>"` continues in a
  fresh session.
- **Talk to a running agent:** when `wait` returns early with a question
  (`attention: ... ask q-...`), answer it with `harness reply <run-id> "answer"`;
  redirect an agent with `harness steer <run-id> "message"` (`--now` to stop
  it and continue with the message at once).
- **Work with other agents and people on this machine:** `harness join --as <me> --room <team>`,
  then `harness start <profile> --as <me> "task"` so your runs report to you,
  and loop on `harness inbox --as <me> --wait 5m` (it says how to answer each
  item). `harness peers` shows who is here; `harness post <alias|run:id|room:name> "..."`
  messages them; `harness claim <path> --as <me>` before editing shared files.
- **Follow up:** `harness send <run-id> "more"` continues the same session.
- **Choose between agents:** `harness stats --kind <label>` shows each
  profile's success rate, time, tokens and cost so far; `harness compare a,b "task"`
  runs the same task on several profiles side by side. Label runs with
  `--kind review` (or similar) so the stats separate kinds of task.
- **See what is running:** `harness status`.

If harness prints `harness: hint: ...`, act on it: it means, for example,
that the same request keeps failing and should not be repeated as it is.

Report the agent's answer to the user, and say which profile and run id
produced it.
