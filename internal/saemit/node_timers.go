// Async timers: setTimeout/clearTimeout/setInterval/clearInterval/
// setImmediate/queueMicrotask need an event loop with callback dispatch
// (Phase 2; see todo/03_npm.md), so they refuse loudly with a dedicated
// diagnostic instead of the generic unknown-function message. The node
// timers_sleep primitive is blocking and has no sync JS spelling
// (Atomics.wait needs futex condition semantics, not just sleep), so it
// stays unprojected too. SA-native code sleeps via sa_time_sleep_ms.
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// timerNames are the async timer globals (bare calls only; method forms
// on other receivers fall through to their own surfaces).
var timerNames = map[string]bool{
	"setTimeout":     true,
	"clearTimeout":   true,
	"setInterval":    true,
	"clearInterval":  true,
	"setImmediate":   true,
	"queueMicrotask": true,
}

// refuseTimerCall rejects async timer globals with the Phase-2 rationale.
// Reports handled (always true when it claims the callee).
func refuseTimerCall(e *emitter, fname string, n *ast.Node) bool {
	if !timerNames[fname] {
		return false
	}
	e.refuse(n, "%s needs an event loop with callback dispatch (async timers are Phase 2)", fname)
	return true
}
