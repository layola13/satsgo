// Deno namespace chains: Deno.env.get/set/delete route through the
// deno plugin (env object has no value of its own; only these methods
// project). Reads/writes of Deno.env itself, toObject, and has() refuse
// loudly (no contract / needs object materialisation).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// routeDenoFs lowers Deno.mkdir/remove with exactly one path (options
// objects are out of subset; recursive defaults to 0 via table Extra).
// Reports (value, type, handled); other methods return handled=false.
func routeDenoFs(e *emitter, method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
	if method != "mkdir" && method != "remove" {
		return "", tUnknown, false
	}
	if len(args) != 1 {
		e.refuse(pos, "Deno.%s takes exactly 1 path (options objects are out of subset)", method)
		return "0", tUnknown, true
	}
	if len(types) > 0 && types[0] != tString {
		e.refuse(pos, "Deno.%s takes a string path", method)
		return "0", tUnknown, true
	}
	key := "Deno." + method
	proj, ok := projectionByTS(key)
	if !ok {
		e.refuse(pos, "%s is not a projected std surface (see StdProjectionTable)", key)
		return "0", tUnknown, true
	}
	v, t := e.emitProjCall(proj, args, pos)
	if e.refused {
		return "0", tUnknown, true
	}
	return v, t, true
}

// lowerBareBtoa lowers bare btoa/atob calls (Web globals) with user
// shadowing preserved (checked by the caller before routing here).
func (e *emitter) lowerBareBtoa(fname string, args []string, types []saType, pos *ast.Node) (string, saType) {
	if len(args) != 1 {
		e.refuse(pos, "%s takes exactly 1 argument", fname)
		return "0", tUnknown
	}
	if len(types) > 0 && types[0] != tString {
		e.refuse(pos, "%s takes a string", fname)
		return "0", tUnknown
	}
	proj, ok := projectionByTS(fname)
	if !ok {
		e.refuse(pos, "%s is not a projected std surface (see StdProjectionTable)", fname)
		return "0", tUnknown
	}
	v, t := e.emitProjCall(proj, args, pos)
	if e.refused {
		return "0", tUnknown
	}
	return v, t
}

// routeDenoEnvChain lowers Deno.env.<m>(...) for get/set/delete.
// Reports (value, type, handled); non-env chains return handled=false so
// the standard refusal path applies.
func routeDenoEnvChain(e *emitter, recv *ast.Node, method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
	if recv.Kind != ast.KindPropertyAccessExpression {
		return "", tUnknown, false
	}
	rpa := recv.AsPropertyAccessExpression()
	if rpa.Expression.Kind != ast.KindIdentifier || rpa.Expression.Text() != "Deno" {
		return "", tUnknown, false
	}
	if rpa.Name().Text() != "env" {
		e.refuse(pos, "Deno.%s namespaces are not projected (env methods only)", rpa.Name().Text())
		return "0", tUnknown, true
	}
	want := 0
	var key string
	switch method {
	case "get":
		key, want = "Deno.env.get", 1
	case "set":
		key, want = "Deno.env.set", 2
	case "delete":
		key, want = "Deno.env.delete", 1
	default:
		e.refuse(pos, "Deno.env.%s is not projected (get/set/delete only)", method)
		return "0", tUnknown, true
	}
	if len(args) != want {
		e.refuse(pos, "Deno.env.%s takes exactly %d argument(s)", method, want)
		return "0", tUnknown, true
	}
	for i := range args {
		if i < len(types) && types[i] != tString {
			e.refuse(pos, "Deno.env.%s takes strings", method)
			return "0", tUnknown, true
		}
	}
	proj, ok := projectionByTS(key)
	if !ok {
		e.refuse(pos, "%s is not a projected std surface (see StdProjectionTable)", key)
		return "0", tUnknown, true
	}
	v, t := e.emitProjCall(proj, args, pos)
	if e.refused {
		return "0", tUnknown, true
	}
	return v, t, true
}
