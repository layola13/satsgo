// Module namespace objects: `export default {a, b: c}` member recording,
// `export default nsAlias` passthrough, and `D.m()` member routing.
//
// This file owns the whole feature; saemit.go and program.go keep only thin
// call sites (project rule: new features live in their own module, core
// files keep hooks). No runtime object ever materialises: members lower to
// direct qualified calls, anything dynamic refuses loudly.
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
)

// collectDefObject records `export default {a, b: c}` member -> local
// pairs into exp.defNS ("" when clean, else the refusal message).
// Shorthand and identifier-valued properties lower to direct qualified
// calls; methods, spreads, accessors and computed/non-identifier values
// refuse loudly (namespace objects carry no runtime shape).
func collectDefObject(obj *ast.Node, exp *fileExports) string {
	for _, prop := range obj.AsObjectLiteralExpression().Properties.Nodes {
		switch prop.Kind {
		case ast.KindShorthandPropertyAssignment:
			if nm := prop.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				if exp.defNS == nil {
					exp.defNS = map[string]string{}
				}
				exp.defNS[nm.Text()] = nm.Text()
				continue
			}
			return "default-export object keys must be identifiers"
		case ast.KindPropertyAssignment:
			pa := prop.AsPropertyAssignment()
			if pa.Name() == nil || pa.Name().Kind != ast.KindIdentifier {
				return "default-export object keys must be identifiers"
			}
			if pa.Initializer == nil || pa.Initializer.Kind != ast.KindIdentifier {
				return "default-export object values must be local names"
			}
			if exp.defNS == nil {
				exp.defNS = map[string]string{}
			}
			exp.defNS[pa.Name().Text()] = pa.Initializer.Text()
			continue
		default:
			return "default-export object supports only shorthand and identifier-valued properties"
		}
	}
	if len(exp.defNS) == 0 {
		return "default-export object is empty"
	}
	return ""
}

// recordNsAlias notes `import * as A from "./y"` (relative, resolved) so a
// later `export default A` can passthrough the target's export surface.
// Non-namespace and unresolvable forms record nothing.
func recordNsAlias(st *ast.Node, tgt string, nsAliasOf map[string]map[string]string, p string) {
	cl := st.AsImportDeclaration().ImportClause
	if cl == nil {
		return
	}
	nb := cl.AsImportClause().NamedBindings
	if nb == nil || nb.Kind != ast.KindNamespaceImport {
		return
	}
	if nsAliasOf[p] == nil {
		nsAliasOf[p] = map[string]string{}
	}
	nsAliasOf[p][nb.AsNamespaceImport().Name().Text()] = tgt
}

// adoptDefNSAlias handles `export default A` where A is a namespace-import
// alias: the target file's export surface becomes this file's default
// members (recorded as a file key; expanded at links time when re-exports
// have resolved). Reports whether it handled the name.
func adoptDefNSAlias(exp *fileExports, name string, nsAliasOf map[string]map[string]string, p string) bool {
	tgt, ok := nsAliasOf[p][name]
	if !ok {
		return false
	}
	exp.defNSFrom = tgt
	return true
}

// expandDefNSFrom materialises defNSFrom into member -> member pairs over
// the target's resolved export surface (ESM `import *` excludes default),
// seeding reexpQualified so member calls resolve to the true qualified
// names (through renames). Runs at links construction, after
// resolveReExports.
func expandDefNSFrom(expOf map[string]*fileExports, prefixOf map[string]string, p string) {
	exp := expOf[p]
	if exp.defNSFrom == "" || len(exp.defNS) > 0 {
		return
	}
	tgt, ok := expOf[exp.defNSFrom]
	if !ok {
		return
	}
	if exp.reexpQualified == nil {
		exp.reexpQualified = map[string]string{}
	}
	for name, q := range tgt.reexpQualified {
		if name == "default" {
			continue
		}
		if exp.defNS == nil {
			exp.defNS = map[string]string{}
		}
		exp.defNS[name] = name
		if _, ok := exp.reexpQualified[name]; !ok {
			exp.reexpQualified[name] = q
		}
	}
	// Non-re-exported direct definitions may be absent from the target's
	// reexpQualified; fall back to prefix + name for those.
	for name := range exp.defNS {
		if _, ok := exp.reexpQualified[name]; !ok {
			exp.reexpQualified[name] = prefixOf[exp.defNSFrom] + name
		}
	}
}

// bindDefNSMembers binds `import D from` (object-default target) per member
// (the object itself is not callable; bare D() stays loud). Mirrors the
// namespace-import binding shape.
func bindDefNSMembers(e *emitter, local string, res *modResolution) {
	for member, tgt := range res.defNS {
		q := res.prefix + tgt
		if qq, ok := res.qualified[tgt]; ok {
			q = qq
		}
		e.importEnv[local+"."+member] = q
		if r, ok := res.rets[tgt]; ok {
			e.importRet[local+"."+member] = r
		} else {
			e.importRet[local+"."+member] = tI32
		}
		e.importedNames[local+"."+member] = true
	}
	e.defNSImports[local] = true
}

// lowerNsDestructure lowers `const {a, b: c} = NS` for namespace imports
// (default-object or `import * as`): each member binds straight to its
// qualified callee (dotted importEnv keys can only come from namespace
// machinery, so presence is the existence proof). Initializers, nesting,
// spreads and computed keys refuse loudly (p3 covers one static level).
func lowerNsDestructure(e *emitter, d, nm *ast.Node, ns string) {
	if nm.Kind != ast.KindObjectBindingPattern {
		e.refuse(d, "namespace destructuring needs an object pattern")
		return
	}
	for _, el := range nm.AsBindingPattern().Elements.Nodes {
		if el.Kind != ast.KindBindingElement {
			e.refuse(el, "namespace destructuring needs plain elements")
			return
		}
		be := el.AsBindingElement()
		if be.Initializer != nil {
			e.refuse(el, "namespace destructuring defaults are not in the subset")
			return
		}
		// Key: shorthand (no PropertyName) or identifier/string key.
		key := ""
		if be.PropertyName == nil {
			nname := be.Name()
			if nname == nil || nname.Kind != ast.KindIdentifier {
				e.refuse(el, "namespace destructuring needs plain names")
				return
			}
			key = nname.Text()
		} else {
			pn := be.PropertyName.AsNode()
			if pn.Kind != ast.KindIdentifier && pn.Kind != ast.KindStringLiteral {
				e.refuse(el, "namespace destructuring keys must be identifiers or strings")
				return
			}
			if pn.Kind == ast.KindStringLiteral {
				s, ok := stringLiteralText(pn)
				if !ok {
					e.refuse(el, "namespace destructuring keys must be identifiers or strings")
					return
				}
				key = s
			} else {
				key = pn.Text()
			}
		}
		ln := be.Name()
		if ln == nil || ln.Kind != ast.KindIdentifier {
			e.refuse(el, "namespace destructuring needs plain local names (no nesting)")
			return
		}
		q, ok := e.importEnv[ns+"."+key]
		if !ok {
			e.refuse(el, "%s is not exported by %s", key, ns)
			return
		}
		local := ln.Text()
		e.importEnv[local] = q
		if r, ok := e.importRet[ns+"."+key]; ok {
			e.importRet[local] = r
		} else {
			e.importRet[local] = tI32
		}
		e.importedNames[local] = true
	}
}

// routeDefNSMember lowers `D.m(..)` for default-object imports through
// importEnv (export must exist; checked at link time). Reports handled;
// unknown members refuse loudly like namespaces.
func routeDefNSMember(e *emitter, recv, method string, args []string, pos *ast.Node) (string, saType, bool) {
	if _, ok := e.defNSImports[recv]; !ok {
		return "", tUnknown, false
	}
	q, ok := e.importEnv[recv+"."+method]
	if !ok {
		e.refuse(pos, "%s.%s is not exported by its module", recv, method)
		return "0", tUnknown, true
	}
	ret := e.importRet[recv+"."+method]
	if ret == tVoid {
		e.emit("call @%s(%s)", q, strings.Join(args, ", "))
		return "0", tVoid, true
	}
	t := e.freshTmp()
	e.emit("%s = call @%s(%s)", t, q, strings.Join(args, ", "))
	e.ownTemp(t)
	return t, ret, true
}
