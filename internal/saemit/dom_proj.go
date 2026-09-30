// DOM projection: document.createElement / handle.appendChild /
// handle.setAttribute over the react airlock externs (sax_dom_*).
//
// Handles are i64 tracked in domVars (names) and domTemps (call-result
// temps); anything else passed as a node refuses loudly (an arbitrary
// i64 is not a handle). Extern declarations are NOT emitted: like
// sax_get_time, airlock imports resolve at `sa react build` time.
// query/remove/text/attrs beyond setAttribute belong to later slices;
// timers stay refused (async, Phase 2).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// trackDomBinding marks `const el = document.createElement(..)` bindings
// for method dispatch (thin hook called from trackBinding).
func trackDomBinding(e *emitter, name string, init *ast.Node) {
	if init == nil || init.Kind != ast.KindCallExpression {
		return
	}
	ce := init.AsCallExpression()
	if ce.Expression.Kind != ast.KindPropertyAccessExpression {
		return
	}
	pa := ce.Expression.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindIdentifier || pa.Expression.Text() != "document" {
		return
	}
	if pa.Name().Text() != "createElement" {
		return
	}
	if e.domVars == nil {
		e.domVars = map[string]bool{}
	}
	e.domVars[name] = true
}

// lowerDocumentCreate lowers document.createElement(tag) to a sax_dom_create
// call (tag must be a string). The result temp is handle-tracked.
func (e *emitter) lowerDocumentCreate(args []string, types []saType, pos *ast.Node) (string, saType) {
	if len(args) != 1 {
		e.refuse(pos, "document.createElement takes exactly 1 argument")
		return "0", tUnknown
	}
	if len(types) > 0 && types[0] != tString {
		e.refuse(pos, "document.createElement takes a string tag")
		return "0", tUnknown
	}
	tp, tl := e.expandSlice(args[0])
	t := e.freshTmp()
	e.emit("%s = call @sax_dom_create(%s, %s)", t, tp, tl)
	e.ownTemp(t)
	if e.domTemps == nil {
		e.domTemps = map[string]bool{}
	}
	e.domTemps[t] = true
	return t, tI64
}

// lowerDomMethod routes handle.appendChild / handle.setAttribute. Only
// tracked handles lower (names or call-result temps); unknown methods
// (querySelector and friends) refuse loudly for later slices.
func (e *emitter) lowerDomMethod(recv, method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
	if !e.domVars[recv] {
		return "", tUnknown, false
	}
	switch method {
	case "appendChild":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		if !e.domVars[args[0]] && !e.domTemps[args[0]] {
			e.refuse(pos, "appendChild takes a DOM node handle")
			return "0", tUnknown, true
		}
		e.emit("call @sax_dom_append_child(%s, %s)", recv, args[0])
		return "0", tVoid, true
	case "setAttribute":
		if len(args) != 2 {
			return "", tUnknown, false
		}
		for i, a := range args {
			_ = a
			if i < len(types) && types[i] != tString {
				e.refuse(pos, "setAttribute takes string key and value")
				return "0", tUnknown, true
			}
		}
		kp, kl := e.expandSlice(args[0])
		vp, vl := e.expandSlice(args[1])
		e.emit("call @sax_dom_set_attr(%s, %s, %s, %s, %s)", recv, kp, kl, vp, vl)
		return "0", tVoid, true
	default:
		e.refuse(pos, "DOM.%s is not projected yet (see todo/04_tsx.md road 2)", method)
		return "0", tUnknown, true
	}
}
