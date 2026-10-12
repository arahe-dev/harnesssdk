// Package harnesssdk lets an agent in one harness run agents in another
// harness as subagents, through each harness's own headless CLI (claude -p,
// pi -p, codex exec, opencode run). There are no adapters or protocols in
// between: one process starts the CLI, feeds it a prompt, and reads its
// answer.
//
// Nothing about a particular harness is compiled in. A definition (JSON)
// says how to call one CLI; a profile (Markdown with a header) names an
// agent built on it: model, tools, system prompt and execution policy.
// HertaSDK is the execution contract: each profile is a Herta operation
// with a declared effect, retry table and per-attempt timeout. Budgets,
// serialize keys and max_running are arbitrated across every harness
// process on the machine.
//
// The harness command is the main interface; see Guide. This package is
// the same engine for Go programs:
//
//	func main() {
//		if code, ok := harnesssdk.Subcommand(); ok {
//			os.Exit(code)
//		}
//		c, err := harnesssdk.Open(harnesssdk.Options{})
//		...
//		res, err := c.Run(ctx, harnesssdk.Request{Profile: "pr-review", Prompt: "Review the diff."})
//	}
//
// # Compatibility
//
// From v1.5.0 this package follows semantic versioning: within v1, nothing
// exported here is removed or changes meaning, and the data folder stays
// readable. Struct types may gain fields, so use keyed literals; a type
// that can be compared with == stays comparable. Every file harness writes
// for itself carries a format number,
// and a harness refuses a format newer than it reads rather than misread
// it. Definitions and profiles written for v1 keep working. The
// implementation lives in internal packages and may change in any release.
package harnesssdk
