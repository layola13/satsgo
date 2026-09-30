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
	"strings"

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
	if _, ok := domCreateSym[pa.Name().Text()]; !ok {
		return
	}
	if e.domVars == nil {
		e.domVars = map[string]bool{}
	}
	e.domVars[name] = true
}

// domCreateSym maps document factory methods to airlock externs.
var domCreateSym = map[string]string{
	"createElement":  "sax_dom_create",
	"createTextNode": "sax_dom_create_text",
}

// lowerDocumentCreate lowers document.createElement(tag) and
// document.createTextNode(text) to sax_dom_* calls (string args only).
// The result temp is handle-tracked.
func (e *emitter) lowerDocumentCreate(method string, args []string, types []saType, pos *ast.Node) (string, saType) {
	sym, ok := domCreateSym[method]
	if !ok {
		e.refuse(pos, "document.%s is not projected yet", method)
		return "0", tUnknown
	}
	if len(args) != 1 {
		e.refuse(pos, "document.%s takes exactly 1 argument", method)
		return "0", tUnknown
	}
	if len(types) > 0 && types[0] != tString {
		e.refuse(pos, "document.%s takes a string", method)
		return "0", tUnknown
	}
	tp, tl := e.expandSlice(args[0])
	t := e.freshTmp()
	e.emit("%s = call @%s(%s, %s)", t, sym, tp, tl)
	e.ownTemp(t)
	if e.domTemps == nil {
		e.domTemps = map[string]bool{}
	}
	e.domTemps[t] = true
	return t, tI64
}

// domTextField maps writable text properties to airlock externs.
var domTextField = map[string]string{
	"textContent": "sax_dom_set_text",
	"innerHTML":   "sax_dom_set_inner_html",
}

// lowerDomStore lowers el.textContent = s / el.innerHTML = s (string RHS
// only) to the airlock setters. Reports claimed: a dom base never falls
// through (unknown shapes refuse loudly here, not generically).
func (e *emitter) lowerDomStore(base, field, rhs string, rtype saType, pos *ast.Node) bool {
	sym, ok := domTextField[field]
	if !ok {
		e.refuse(pos, "DOM.%s is not writable yet (textContent/innerHTML only)", field)
		return true
	}
	if rtype != tString {
		e.refuse(pos, "DOM.%s takes a string", field)
		return true
	}
	rp, rl := e.expandSlice(rhs)
	e.emit("call @%s(%s, %s, %s)", sym, base, rp, rl)
	return true
}

// domReadCap is the scratch size for textContent reads (get_text has no
// size query; exact-full reads panic rather than silently truncate).
const domReadCap = 4096

// domRead emits a caller-scratch read: alloc, call sym(base, extra...,
// buf, cap), full-buffer panics, wrap {buf, n} slice. The slice aliases
// the scratch, so both stay owned to scope exit.
func (e *emitter) domRead(sym, base string, extra []string, pos *ast.Node) (string, saType) {
	_ = pos
	buf := e.freshTmp()
	e.emit("%s = alloc %d", buf, domReadCap)
	e.declareOwned(buf)
	callArgs := append(append([]string{base}, extra...), buf, "4096")
	n := e.freshTmp()
	e.emit("%s = call @%s(%s)", n, sym, strings.Join(callArgs, ", "))
	e.ownTemp(n)
	fullL := e.freshLabel("dom_full")
	okL := e.freshLabel("dom_ok")
	full := e.freshTmp()
	e.emit("%s = eq %s, %d", full, n, domReadCap)
	e.emit("br %s -> %s, %s", full, fullL, okL)
	e.emitRaw("%s:", fullL)
	e.emit("panic(%d)", panicDomScratchFull)
	e.terminated = true
	e.emitRaw("%s:", okL)
	e.terminated = false
	out := e.freshTmp()
	e.emit("%s = alloc 16", out)
	e.emit("store %s + 0, %s as ptr", out, buf)
	e.emit("store %s + 8, %s as u64", out, n)
	e.declareOwned(out)
	return out, tString
}

// domAttrKey maps readable attribute sugar to airlock keys.
var domAttrKey = map[string]string{
	"className": "class",
	"id":        "id",
}

// lowerDomLoad reads el.textContent (native) and className/id sugar
// (via get_attr). innerHTML reads, length and everything else refuse
// loudly (a .length load on a handle would be garbage).
func (e *emitter) lowerDomLoad(base, field string, pos *ast.Node) (string, saType, bool) {
	if field == "textContent" {
		v, t := e.domRead("sax_dom_get_text", base, nil, pos)
		return v, t, true
	}
	if key, ok := domAttrKey[field]; ok {
		ks := e.lowerStringLiteral(key)
		kp, kl := e.expandSlice(ks)
		v, t := e.domRead("sax_dom_get_attr", base, []string{kp, kl}, pos)
		return v, t, true
	}
	e.refuse(pos, "DOM.%s is not readable yet (textContent/className/id only)", field)
	return "0", tUnknown, true
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
	case "getAttribute":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		if len(types) > 0 && types[0] != tString {
			e.refuse(pos, "getAttribute takes a string key")
			return "0", tUnknown, true
		}
		kp, kl := e.expandSlice(args[0])
		v, t := e.domRead("sax_dom_get_attr", recv, []string{kp, kl}, pos)
		return v, t, true
	case "removeAttribute":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		if len(types) > 0 && types[0] != tString {
			e.refuse(pos, "removeAttribute takes a string key")
			return "0", tUnknown, true
		}
		kp, kl := e.expandSlice(args[0])
		e.emit("call @sax_dom_remove_attr(%s, %s, %s)", recv, kp, kl)
		return "0", tVoid, true
	default:
		e.refuse(pos, "DOM.%s is not projected yet (see todo/04_tsx.md road 2)", method)
		return "0", tUnknown, true
	}
}
