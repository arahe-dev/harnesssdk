// Package harnesssdk lets an agent in one harness run agents in another
// harness as subagents, through each harness's own headless CLI (claude -p,
// pi -p, codex exec, opencode run). There are no adapters or protocols in
// between: one process starts the CLI, feeds it a prompt, and reads its
// answer.
//
// Nothing about a particular harness is compiled in. A Definition (JSON)
// says how to call one CLI; a Profile (Markdown with a header) names an
// agent built on it: model, tools, system prompt and execution policy. An
// agent creates both by running the harness command.
//
// The execution contract is HertaSDK: each profile becomes a Herta
// operation with a declared Effect, retry table and per-attempt timeout,
// validated when the profile is saved. Budgets and serialize keys are
// arbitrated once, across every harness process on the machine, by the
// gate, which follows Herta's budget rules.
//
// harness starts its own executable as a detached worker and, on Unix, as
// the guard of each agent's process tree; a program embedding Runner must
// call Subcommand first in main.
//
// Layout:
//
//	template.go   argument templates: {name}, {name?text}, {*name}
//	definition.go harness definitions: args, output (text, json, jsonl), failure rules
//	profile.go    profiles and their Herta policy fields
//	home.go       the data directory (~/.harness) and lookups
//	project.go    per-project .harness folders and trust
//	gate.go       machine-wide FIFO ledgers for budgets and serialize keys
//	lineage.go    nested runs: parent and lineage from env or process ancestry
//	ancestors*.go process ancestry per OS
//	active.go     the index of running turns and their agents
//	record.go     run records (runs/<id>/)
//	runner.go     turns: prepare, execute under the gate and Herta, wait, stop
//	worker.go     detached workers (harness start, send --detach)
//	detach*.go    starting a worker that outlives its starter
//	process*.go   one CLI invocation; process-tree containment (job object;
//	              on Unix a guard leading the process group)
//	lock*.go      cross-process file locks
//	share*.go     reading records while another process replaces them
//	guide.go      the embedded agent guide and skill
package harnesssdk
