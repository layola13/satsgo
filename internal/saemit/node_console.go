// Node console surfaces: error/time/timeEnd/clear over sa_plugin_node.
// console.log stays on the sa_std print path (untouched); the rest have no
// sa_std backend, so they project to the node plugin. Multi-arg error
// folds through the same render+space+newline shape as log, then crosses
// in one slice. time/timeEnd pair natively in the plugin (missing label
// panics via the status check); timers stay refused (async, Phase 2).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

func isConsoleMethod(fn *ast.Node, name string) bool {
	if fn.Kind != ast.KindPropertyAccessExpression {
		return false
	}
	pa := fn.AsPropertyAccessExpression()
	return pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "console" && pa.Name().Text() == name
}

// lowerConsoleError renders operands like log (spaces + trailing newline),
// folds them into one slice, and writes it via the node plugin.
// Returns tVoid; a nonzero plugin status panics (loud).
func (e *emitter) lowerConsoleError(args []string, types []saType, pos *ast.Node) (string, saType) {
	proj, ok := projectionByTS("console.error")
	if !ok {
		e.refuse(pos, "console.error is not a projected std surface (see StdProjectionTable)")
		return "0", tUnknown
	}
	acc := ""
	first := true
	lit := func(s string) string { return e.lowerStringLiteral(s) }
	var space string
	if len(args) > 1 {
		space = lit(" ")
	}
	for i, a := range args {
		vt := tI32
		if i < len(types) {
			vt = types[i]
		}
		seg, ok := e.renderInterpValue(a, vt, pos)
		if !ok {
			return "0", tUnknown
		}
		if first {
			acc = seg
			first = false
			continue
		}
		acc = e.concatSlices(acc, space)
		acc = e.concatSlices(acc, seg)
	}
	if first {
		acc = lit("")
	}
	acc = e.concatSlices(acc, lit("\n"))
	v, t := e.emitProjCall(proj, []string{acc}, pos)
	if e.refused {
		return "0", tUnknown
	}
	return v, t
}

// lowerConsoleTime lowers time(label?) / timeEnd(label?) / clear().
// Missing labels default to "default" like Node; timeEnd returns f64
// millis (missing timer panics via the status check).
func (e *emitter) lowerConsoleTime(method string, args []string, pos *ast.Node) (string, saType) {
	var operands []string
	if method == "clear" {
		if len(args) != 0 {
			e.refuse(pos, "console.clear takes no arguments")
			return "0", tUnknown
		}
		operands = nil
	} else {
		if len(args) > 1 {
			e.refuse(pos, "console.%s takes at most 1 argument", method)
			return "0", tUnknown
		}
		label := e.lowerStringLiteral("default")
		if len(args) == 1 {
			label = args[0]
		}
		operands = []string{label}
	}
	key := "console." + method
	proj, ok := projectionByTS(key)
	if !ok {
		e.refuse(pos, "%s is not a projected std surface (see StdProjectionTable)", key)
		return "0", tUnknown
	}
	v, t := e.emitProjCall(proj, operands, pos)
	if e.refused {
		return "0", tUnknown
	}
	return v, t
}
