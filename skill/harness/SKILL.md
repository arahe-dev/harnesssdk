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
  diff). Runs can take minutes: run the command in the background if your
  shell tool has a timeout.
- **Set up a new agent:** read the target CLI's `--help`, write the definition
  if it is missing (`harness def set <name> --file -`), then the profile
  (`harness profile set <name> --file -`), check it with
  `harness run <profile> --dry-run "test"`, then run it.
- **Follow up:** `harness send <run-id> "more"` continues the same session.

Report the agent's answer to the user, and say which profile and run id
produced it.
