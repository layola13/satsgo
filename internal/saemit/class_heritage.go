// Single-inheritance lowering for `class C extends B` (+ `implements` erasure).
//
// Design (mirrors TS runtime semantics on top of the existing static-layout
// model; no vtables exist in SA-ASM):
//
//   - The child layout is the base layout with the child's own fields
//     appended (parent offsets preserved, so inherited code keeps working).
//   - Methods/getters/setters/statics are inherited by copy; the child's own
//     members override by name (TS override semantics for direct calls).
//   - A missing child ctor inherits the base ctor node (default derived
//     constructor forwards to super).
//   - `super(...)` in the child ctor delegates to the base ctor wiring on
//     the same instance handle.
//   - `super.m(...)` / `super.f` inside methods route to the base method /
//     the flattened field (same offsets, so field reads are `this`-shaped).
//   - `implements ...` is type-only and erased (no runtime effect, never a
//     refusal on its own; unknown names are still fine — the checker owns
//     type errors).
//   - `abstract` classes record and refuse `new` precisely (TS fidelity).
//
// Loud refusals (never silent): dynamic/non-identifier bases
// (`extends mixin()` / `extends A.B`), unknown bases, inheritance cycles,
// `super` outside a heritage method/ctor, and multiple extends entries.
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// heritageInfo is one parsed heritage clause set: the single-extends base
// ("" when absent) plus whether any extends clause was present.
type heritageInfo struct {
	base       string
	hasExtends bool
}

// parseHeritage reads `extends`/`implements` off a class declaration.
// Implements lists are erased (type-only). Extends must be a single plain
// identifier; anything dynamic refuses loudly. Reports ok=false after
// refusing.
func (e *emitter) parseHeritage(cd *ast.ClassDeclaration, st *ast.Node) (heritageInfo, bool) {
	var hi heritageInfo
	if cd.HeritageClauses == nil || len(cd.HeritageClauses.Nodes) == 0 {
		return hi, true
	}
	for _, h := range cd.HeritageClauses.Nodes {
		hc := h.AsHeritageClause()
		if hc.Token == ast.KindExtendsKeyword {
			if hi.hasExtends || len(hc.Types.Nodes) != 1 {
				e.refuse(st, "class extends needs exactly one base class")
				return hi, false
			}
			el := hc.Types.Nodes[0]
			base := ""
			if el.Kind == ast.KindExpressionWithTypeArguments {
				expr := el.AsExpressionWithTypeArguments().Expression
				if expr != nil {
					if expr.Kind == ast.KindIdentifier {
						base = expr.Text()
					} else if expr.Kind == ast.KindPropertyAccessExpression {
						// Qualified bases (`extends N.B`) flatten to the
						// namespace path (see namespace_ts.go).
						if q, ok := dottedBaseName(expr); ok {
							base = q
						}
					}
				}
			} else if el.Kind == ast.KindIdentifier {
				base = el.Text()
			}
			if base == "" {
				e.refuse(st, "class extends needs a plain base class name (mixins are not lowerable)")
				return hi, false
			}
			hi.base = base
			hi.hasExtends = true
			continue
		}
		// Implements (and any future non-extends clause): type-only, erased.
	}
	return hi, true
}

// inheritClass seeds def/layout/statics from the base class and records the
// parent link for cycle checks and super routing. The caller appends the
// child's own members afterwards (own wins). Reports false after refusing.
func (e *emitter) inheritClass(name, base string, def *classDef, l *layout, st *ast.Node) bool {
	bdef, ok := e.classDefs[base]
	if !ok || bdef.layout == nil {
		e.refuse(st, "class %s extends unknown base %s (declare the base class first)", name, base)
		return false
	}
	// Cycle check: walk parent links from the base; reaching name (or a
	// missing link end) decides.
	for p := base; p != ""; {
		if p == name {
			e.refuse(st, "class %s has an inheritance cycle through %s", name, base)
			return false
		}
		p = e.classParent[p]
	}
	// Layout: base fields keep offsets; child fields append below.
	l.fields = append(append([]string{}, bdef.layout.fields...), l.fields...)
	if l.types == nil {
		l.types = map[string]string{}
	}
	if l.ftypes == nil {
		l.ftypes = map[string]string{}
	}
	if l.offsets == nil {
		l.offsets = map[string]int{}
	}
	for _, f := range bdef.layout.fields {
		l.types[f] = bdef.layout.types[f]
		if t, ok := bdef.layout.ftypes[f]; ok {
			l.ftypes[f] = t
		}
		l.offsets[f] = bdef.layout.offsets[f]
	}
	// Methods/getters/setters/statics inherit by copy (child overrides).
	// methodOwner follows the node: inherited members keep the base as
	// their lexical owner for private resolution (mirrors ctorOwner).
	if def.methodOwner == nil {
		def.methodOwner = map[string]string{}
	}
	if def.methods == nil {
		def.methods = map[string]*ast.Node{}
	}
	for k, v := range bdef.methods {
		if _, ok := def.methods[k]; !ok {
			def.methods[k] = v
			if _, ok := def.methodOwner[k]; !ok {
				owner := base
				if o, ok := bdef.methodOwner[k]; ok {
					owner = o
				}
				def.methodOwner[k] = owner
			}
		}
	}
	if def.getters == nil {
		def.getters = map[string]*ast.Node{}
	}
	for k, v := range bdef.getters {
		if _, ok := def.getters[k]; !ok {
			def.getters[k] = v
		}
	}
	if def.setters == nil {
		def.setters = map[string]*ast.Node{}
	}
	for k, v := range bdef.setters {
		if _, ok := def.setters[k]; !ok {
			def.setters[k] = v
		}
	}
	if def.statics == nil {
		def.statics = map[string]staticVal{}
	}
	for k, v := range bdef.statics {
		if _, ok := def.statics[k]; !ok {
			def.statics[k] = v
		}
	}
	// Default derived constructor: inherit the base ctor node when the
	// child declares none (set by the caller when def.ctor == nil).
	if e.classParent == nil {
		e.classParent = map[string]string{}
	}
	e.classParent[name] = base
	_ = st
	return true
}

// superBaseForRecv resolves the base class for a `super.m()` / `super.f`
// use inside the method currently being inlined (recv is e.thisSelf).
func (e *emitter) superBaseForRecv(recv string) (string, bool) {
	cls, ok := e.varClass[recv]
	if !ok {
		cls = e.curMethodClass
	}
	if cls == "" {
		return "", false
	}
	base, ok := e.classParent[cls]
	if !ok || base == "" {
		return "", false
	}
	return base, true
}

// lowerSuperMethodCall inlines `super.m(args)` as the base-class method with
// the current receiver. Reports handled=true when the call was a super form
// (lowered or refused); handled=false means "not a super call".
func (e *emitter) lowerSuperMethodCall(pa *ast.PropertyAccessExpression, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	if pa.Expression == nil || pa.Expression.Kind != ast.KindSuperKeyword {
		return "", tUnknown, false
	}
	if e.thisSelf == "" || e.curMethodClass == "" {
		e.refuse(pos, "super calls are only lowerable inside a subclass method")
		return "0", tUnknown, true
	}
	base, ok := e.superBaseForRecv(e.thisSelf)
	if !ok {
		e.refuse(pos, "super calls are only lowerable inside a subclass method")
		return "0", tUnknown, true
	}
	return e.lowerClassMethodCall(e.thisSelf, base, pa.Name().Text(), args, argNodes, pos)
}

// checkSuperAccess validates `super.f` (read path): the current method must
// belong to a subclass, and accessor fields keep their precise diagnostic
// (accessors occupy no layout slot; a silent offset load would be garbage).
// Reports false after refusing.
func (e *emitter) checkSuperAccess(n *ast.Node, segs []string) bool {
	if e.thisSelf == "" {
		e.refuse(n, "super property access is only lowerable inside a subclass method")
		return false
	}
	if _, ok := e.superBaseForRecv(e.thisSelf); !ok {
		e.refuse(n, "super property access is only lowerable inside a subclass method")
		return false
	}
	if len(segs) > 0 {
		if cd := e.classDefOf(e.thisSelf); cd != nil {
			if _, ok := cd.getters[segs[0]]; ok {
				e.refuse(n, "getter super.%s needs inline support (not yet)", segs[0])
				return false
			}
		}
	}
	return true
}

// ctorCallsSuper reports whether a constructor body contains a `super(...)`
// call. Nested function/class boundaries own their super (illegal there);
// arrows inherit the outer binding, so the walk descends into them.
func (e *emitter) ctorCallsSuper(ctor *ast.Node) bool {
	body := ctor.Body()
	if body == nil {
		return false
	}
	found := false
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		if found || n == nil {
			return
		}
		switch n.Kind {
		case ast.KindFunctionDeclaration, ast.KindFunctionExpression,
			ast.KindClassDeclaration, ast.KindClassExpression:
			return
		case ast.KindCallExpression:
			call := n.AsCallExpression()
			if call.Expression != nil && call.Expression.Kind == ast.KindSuperKeyword {
				found = true
				return
			}
		}
		n.ForEachChild(func(c *ast.Node) bool {
			walk(c)
			return false
		})
	}
	for _, s := range body.Statements() {
		walk(s)
		if found {
			break
		}
	}
	return found
}

// inheritInterfaceLayout seeds an interface layout from its `extends` bases
// (type-only flattening; unknown or qualified bases are skipped best-effort
// since the checker owns type errors — own members always record).
func (e *emitter) inheritInterface(st *ast.Node, l *layout) {
	decl := st.AsInterfaceDeclaration()
	if decl.HeritageClauses == nil {
		return
	}
	for _, h := range decl.HeritageClauses.Nodes {
		hc := h.AsHeritageClause()
		if hc.Token != ast.KindExtendsKeyword {
			continue
		}
		for _, el := range hc.Types.Nodes {
			iname := ""
			switch el.Kind {
			case ast.KindExpressionWithTypeArguments:
				if ex := el.AsExpressionWithTypeArguments().Expression; ex != nil {
					if ex.Kind == ast.KindIdentifier {
						iname = ex.Text()
					} else if ex.Kind == ast.KindPropertyAccessExpression {
						iname, _ = dottedBaseName(ex)
					}
				}
			case ast.KindTypeReference:
				// Qualified names (`extends NS.J`) flatten to the path
				// form (see namespace_ts.go).
				iname = entityNameText(el.AsTypeReferenceNode().TypeName)
			case ast.KindIdentifier:
				iname = el.Text()
			}
			if iname == "" {
				continue
			}
			base, ok := e.layouts[iname]
			if !ok || base == nil {
				continue
			}
			for _, f := range base.fields {
				if _, dup := l.offsets[f]; dup {
					continue
				}
				size, align := widthOf(base.types[f])
				off := alignTo(l.size, align)
				l.fields = append(l.fields, f)
				l.types[f] = base.types[f]
				if t, ok := base.ftypes[f]; ok {
					l.ftypes[f] = t
				}
				if d, ok := base.fdefs[f]; ok {
					l.fdefs[f] = d
				}
				l.offsets[f] = off
				l.size = off + size
			}
		}
	}
}

// wireSuperCtorStatement delegates `super(a, b, ...)` in a child ctor to the
// base ctor wiring on the same instance handle. Super arguments are
// evaluated in the child ctor scope, so identifiers resolve through the
// outer call-site map (child params are invisible to the emitter scope);
// literals lower fresh; anything else refuses loudly. Reports (handled, ok):
// handled=false means "not a super() statement".
func (e *emitter) wireSuperCtorStatement(h, className string, s *ast.Node, outerVal map[string]string, pos *ast.Node) (bool, bool) {
	if s.Kind != ast.KindExpressionStatement {
		return false, true
	}
	ex := s.AsExpressionStatement().Expression
	if ex.Kind != ast.KindCallExpression {
		return false, true
	}
	call := ex.AsCallExpression()
	if call.Expression == nil || call.Expression.Kind != ast.KindSuperKeyword {
		return false, true
	}
	base, ok := e.classParent[className]
	if !ok || base == "" {
		e.refuse(s, "super() is only lowerable inside a subclass constructor")
		return true, false
	}
	bdef := e.classDefs[base]
	if bdef == nil || bdef.ctor == nil {
		// Base has no declared ctor: the implicit default forwards any
		// arguments up the chain (ultimately ignored). Evaluate each
		// argument in order for effect ordering, then discard.
		for _, a := range call.Arguments.Nodes {
			switch a.Kind {
			case ast.KindArrowFunction, ast.KindFunctionExpression:
			case ast.KindIdentifier:
				if _, ok := outerVal[a.Text()]; !ok {
					e.refuse(a, "super() arguments must be constructor parameters or literals")
					return true, false
				}
			case ast.KindNumericLiteral, ast.KindStringLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword, ast.KindNullKeyword, ast.KindUndefinedKeyword:
				if _, _ = e.lowerExpr(a); e.refused {
					return true, false
				}
			default:
				e.refuse(a, "super() arguments must be constructor parameters or literals")
				return true, false
			}
		}
		return true, true
	}
	params := bdef.ctor.Parameters()
	argNodes := call.Arguments.Nodes
	if len(argNodes) != len(params) {
		e.refuse(s, "super() takes %d arguments (%d given)", len(params), len(argNodes))
		return true, false
	}
	paramArg := map[string]*ast.Node{}
	paramVal := map[string]string{}
	for i, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			e.refuse(p.AsNode(), "destructured constructor parameters are not lowerable")
			return true, false
		}
		a := argNodes[i]
		paramArg[pname] = a
		switch a.Kind {
		case ast.KindArrowFunction, ast.KindFunctionExpression:
			// Adopted by wireCtorStatement via paramArg (no eager value).
		case ast.KindIdentifier:
			v, ok := outerVal[a.Text()]
			if !ok {
				e.refuse(a, "super() arguments must be constructor parameters or literals")
				return true, false
			}
			paramVal[pname] = v
		case ast.KindNumericLiteral, ast.KindStringLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword, ast.KindNullKeyword, ast.KindUndefinedKeyword:
			v, _ := e.lowerExpr(a)
			if e.refused {
				return true, false
			}
			paramVal[pname] = v
		default:
			e.refuse(a, "super() arguments must be constructor parameters or literals")
			return true, false
		}
	}
	body := bdef.ctor.Body()
	if body != nil {
		for _, bs := range body.Statements() {
			// Nested super() in the base ctor targets the grandparent;
			// recurse through the same path with the base as owner.
			if done, ok := e.wireSuperCtorStatement(h, base, bs, paramVal, pos); done {
				if !ok {
					return true, false
				}
				continue
			}
			if !e.wireCtorStatement(h, base, bs, paramArg, paramVal, pos) {
				return true, false
			}
		}
	}
	return true, true
}
