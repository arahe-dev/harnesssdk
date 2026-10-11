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
// Execution policy is HertaSDK: each profile becomes a Herta operation with
// a declared Effect, budgets, admission, per-attempt timeout and retry
// table, validated when the profile is saved. Fan-out within one process is
// arbitrated by one Herta runtime.
//
// Layout:
//
//	template.go   argument templates: {name}, {name?text}, {*name}
//	definition.go harness definitions: args, output, failure rules
//	profile.go    profiles and their Herta policy fields
//	home.go       the data directory (~/.harness)
//	record.go     run records (runs/<id>/)
//	runner.go     turns as Herta operations; start, send, dry run
//	process*.go   one CLI invocation; process-tree kill (job object, group)
//	lock*.go      cross-process run locks
//	guide.go      the embedded agent guide and skill
package harnesssdk
