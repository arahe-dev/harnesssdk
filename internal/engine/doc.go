// Package engine is HarnessSDK's implementation. It is internal: its API
// may change in any release. The stable API is the root package,
// github.com/arahe-dev/harnesssdk/v2.
//
// Layout:
//
//	template.go   argument templates: {name}, {name?text}, {*name}
//	definition.go harness definitions: args, output (text, json, jsonl), failure rules
//	profile.go    profiles and their policy fields
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
//	config.go     file format versions; config.json limits
//	gc.go         retention: removing old runs
//	lock*.go      cross-process file locks
//	share*.go     reading records while another process replaces them
package engine
