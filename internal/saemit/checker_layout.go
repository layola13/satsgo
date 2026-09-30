// Checker-driven struct layouts: when the syntax-recorded varLayouts map
// misses (inferred structs such as factory-call results), fall back to
// tsgo checker types — interface/class names resolve into e.layouts,
// anonymous shapes resolve through matchLayout by data-field set.
// Nil-safe without a context; unions take the first named non-nullish
// constituent (mirrors the annotation rule); anything exotic yields nil.
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/checker"
)

// typeAtNode queries the checker type at a node (nil without a context;
// checker panics on out-of-subset shapes become unknown, never fatal).
func (t *typeCtx) typeAtNode(n *ast.Node) (ty *checker.Type) {
	if t == nil || t.check == nil || n == nil {
		return nil
	}
	func() {
		defer func() {
			_ = recover()
		}()
		ty = t.check.GetTypeAtLocation(n)
	}()
	return ty
}

// apparentBase unwraps to the layout-bearing constituent: unions take the
// first non-nullish member (annotation rule), intersections take the
// first member; anything else passes through.
func apparentBase(t *typeCtx, ty *checker.Type) *checker.Type {
	if t == nil || t.check == nil || ty == nil {
		return ty
	}
	var out *checker.Type
	func() {
		defer func() {
			_ = recover()
		}()
		if ty.Flags()&checker.TypeFlagsUnionOrIntersection != 0 {
			for _, c := range ty.Types() {
				if isNullish(c.Flags()) {
					continue
				}
				out = c
				break
			}
			if out == nil {
				out = ty
			}
			return
		}
		out = t.check.GetApparentType(ty)
		if out == nil {
			out = ty
		}
	}()
	if out == nil {
		return ty
	}
	return out
}

// layoutTypeName resolves the apparent interface/class/alias name at a
// node ("" when anonymous or unknown). Callers try e.layouts[name] and,
// for aliases, the alias target name.
func (t *typeCtx) layoutTypeName(n *ast.Node) (name, alias string) {
	ty := apparentBase(t, t.typeAtNode(n))
	if ty == nil {
		return "", ""
	}
	var sym *ast.Symbol
	var al *checker.TypeAlias
	func() {
		defer func() {
			_ = recover()
		}()
		sym = ty.Symbol()
		al = ty.Alias()
	}()
	if sym != nil && sym.Name != "" && sym.Name != "__type" {
		name = sym.Name
	}
	if al != nil {
		func() {
			defer func() {
				_ = recover()
			}()
			// TypeAlias carries the alias declaration name via its symbol.
			if as := al.Symbol(); as != nil && as.Name != "" {
				alias = as.Name
			}
		}()
	}
	return name, alias
}

// layoutDataFields lists data-field names of an anonymous object shape at
// a node (methods and accessors excluded; ok=false when not an object or
// on any checker failure). Field order is checker order; matchLayout is
// order-insensitive.
func (t *typeCtx) layoutDataFields(n *ast.Node) ([]string, bool) {
	ty := apparentBase(t, t.typeAtNode(n))
	if ty == nil {
		return nil, false
	}
	var props []*ast.Symbol
	ok := false
	func() {
		defer func() {
			_ = recover()
		}()
		props = t.check.GetPropertiesOfType(ty)
		ok = true
	}()
	if !ok || len(props) == 0 || len(props) > 64 {
		return nil, false
	}
	fields := make([]string, 0, len(props))
	for _, p := range props {
		if p == nil || p.Name == "" {
			return nil, false
		}
		if p.Flags&ast.SymbolFlagsProperty == 0 {
			continue
		}
		if p.Flags&(ast.SymbolFlagsMethod|ast.SymbolFlagsFunction|ast.SymbolFlagsGetAccessor|ast.SymbolFlagsSetAccessor) != 0 {
			continue
		}
		fields = append(fields, p.Name)
	}
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// recordTypeAlias records `type X = {a: T, ...}` object aliases as layouts,
// mirroring recordLayout (planck TransformValue/RotValue/Vec2Value shape).
// Non-literal targets (unions, primitives, references) record nothing;
// method signatures declare no field, same as interfaces.
func (e *emitter) recordTypeAlias(st *ast.Node) {
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	tgt := st.AsTypeAliasDeclaration().Type
	if tgt == nil || tgt.Kind != ast.KindTypeLiteral {
		return
	}
	if name == "<anon>" {
		return
	}
	name = e.nsDefName(name)
	l := &layout{name: name, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}, fdefs: map[string]*ast.Node{}}
	for _, tp := range st.TypeParameters() {
		if nm := tp.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
			l.tparams = append(l.tparams, nm.Text())
		}
	}
	off := 0
	for _, m := range tgt.AsTypeLiteralNode().Members.Nodes {
		if m.Kind != ast.KindPropertySignature {
			continue
		}
		fname, ok := bindingNameText(m)
		if !ok {
			continue
		}
		ftn := m.AsPropertySignatureDeclaration().Type
		saname := saNameOfType(ftn)
		size, align := widthOf(saname)
		off = alignTo(off, align)
		l.fields = append(l.fields, fname)
		l.types[fname] = saname
		l.ftypes[fname] = rawTypeName(ftn)
		l.fdefs[fname] = ftn
		l.offsets[fname] = off
		off += size
	}
	if len(l.fields) == 0 {
		return
	}
	l.size = off
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[name] = l
}

// monoKey renders the canonical instantiation key for a type node:
// `Box<i32,string>` for generic references (args recurse), else the raw
// type name. Bare names and non-references key by themselves.
func monoKey(tn *ast.Node) string {
	if tn == nil {
		return "i32"
	}
	if tn.Kind != ast.KindTypeReference {
		return rawTypeName(tn)
	}
	name := tn.AsTypeReferenceNode().TypeName.Text()
	args := tn.TypeArguments()
	if len(args) == 0 {
		return name
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, monoKey(a))
	}
	key := name + "<"
	for i, p := range parts {
		if i > 0 {
			key += ","
		}
		key += p
	}
	return key + ">"
}

// instantiateLayout monomorphizes a recorded generic interface over its
// type arguments (field widths recomputed from substituted args; nested
// generic fields recurse). Results cache under the canonical key; unknown
// templates and arity mismatches fall back to the bare layout (legacy).
func (e *emitter) instantiateLayout(name string, args []*ast.Node) *layout {
	tmpl, ok := e.layouts[name]
	if !ok || len(tmpl.tparams) == 0 || len(args) != len(tmpl.tparams) {
		return e.layouts[name]
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, monoKey(a))
	}
	cacheKey := name + "<" + strings.Join(parts, ",") + ">"
	if l, ok := e.layouts[cacheKey]; ok {
		return l
	}
	pmap := map[string]*ast.Node{}
	for i, p := range tmpl.tparams {
		pmap[p] = args[i]
	}
	l := &layout{name: cacheKey, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}, fdefs: map[string]*ast.Node{}}
	// Shell-first: recursive instantiations hitting the cache mid-fill
	// resolve to this shell (fields complete synchronously below, before
	// any member access can read them).
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[cacheKey] = l
	off := 0
	for _, fname := range tmpl.fields {
		saname, fkey := e.substFieldType(tmpl.fdefs[fname], pmap)
		size, align := widthOf(saname)
		off = alignTo(off, align)
		l.fields = append(l.fields, fname)
		l.types[fname] = saname
		l.ftypes[fname] = fkey
		l.fdefs[fname] = tmpl.fdefs[fname]
		l.offsets[fname] = off
		off += size
	}
	l.size = off
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[cacheKey] = l
	return l
}

// substFieldType resolves one field type under a param mapping to an SA
// name plus a nested-layout key. Bare parameters substitute the actual
// argument (instantiating generic args so the layout exists for descent);
// closed generic references instantiate under their canonical key;
// parameterised nesting (List<T> inside Box<T>) keeps the raw name and
// refuses loudly downstream (recursive generics need lazy instantiation).
func (e *emitter) substFieldType(ftn *ast.Node, pmap map[string]*ast.Node) (string, string) {
	if ftn != nil && ftn.Kind == ast.KindTypeReference {
		refName := ftn.AsTypeReferenceNode().TypeName.Text()
		if arg, ok := pmap[refName]; ok && arg != nil {
			if arg.Kind == ast.KindTypeReference {
				if sub := e.instantiateLayout(arg.AsTypeReferenceNode().TypeName.Text(), arg.TypeArguments()); sub != nil {
					return saNameOfType(arg), sub.name
				}
			}
			return saNameOfType(arg), monoKey(arg)
		}
		if fargs := ftn.TypeArguments(); len(fargs) > 0 {
			// Substitute params, then instantiate (shell-cached recursion
			// terminates: self/mutual references hit the in-progress shell
			// and use its key). Param-held generics and deep nesting stay
			// raw (loud downstream).
			sub := make([]*ast.Node, 0, len(fargs))
			closed := true
			for _, a := range fargs {
				if a.Kind == ast.KindTypeReference {
					if parg, ok := pmap[a.AsTypeReferenceNode().TypeName.Text()]; ok {
						if parg == nil || (parg.Kind == ast.KindTypeReference && len(parg.TypeArguments()) > 0) {
							closed = false
							break
						}
						sub = append(sub, parg)
						continue
					}
				}
				if typeHasParam(a, pmap) {
					closed = false
					break
				}
				sub = append(sub, a)
			}
			if closed {
				if l := e.instantiateLayout(refName, sub); l != nil {
					return "ptr", l.name
				}
			}
			return "ptr", refName
		}
	}
	return saNameOfType(ftn), rawTypeName(ftn)
}

// typeHasParam reports whether a type node mentions any mapped parameter.
func typeHasParam(tn *ast.Node, pmap map[string]*ast.Node) bool {
	if tn == nil {
		return false
	}
	if tn.Kind == ast.KindTypeReference {
		if _, ok := pmap[tn.AsTypeReferenceNode().TypeName.Text()]; ok {
			return true
		}
		for _, a := range tn.TypeArguments() {
			if typeHasParam(a, pmap) {
				return true
			}
		}
		return false
	}
	return false
}

// layoutOfAnnotation resolves a TypeReference annotation to its layout,
// instantiating generics (Box<i32> vs Box<string> get distinct widths);
// bare names keep the legacy direct lookup. Never refuses (nil on miss).
func (e *emitter) layoutOfAnnotation(tn *ast.Node) *layout {
	if tn == nil || tn.Kind != ast.KindTypeReference {
		return nil
	}
	name := tn.AsTypeReferenceNode().TypeName.Text()
	if args := tn.TypeArguments(); len(args) > 0 {
		return e.instantiateLayout(name, args)
	}
	return e.layouts[name]
}

// layoutOfCheckerName resolves a struct layout from the checker's
// apparent interface/class/alias name at a node ("", nil-safe).
// Shared by layoutOfNode (member bases) and layoutOfLiteral (literals):
// exact names beat order-insensitive field-set guesses.
func (e *emitter) layoutOfCheckerName(n *ast.Node) *layout {
	if e.tcx == nil || n == nil {
		return nil
	}
	if name, alias := e.tcx.layoutTypeName(n); name != "" || alias != "" {
		if l, ok := e.layouts[name]; ok && name != "" {
			return l
		}
		if l, ok := e.layouts[alias]; ok && alias != "" {
			return l
		}
		// A bare name with no recorded layout may still match by shape
		// when the checker exposes fields (partial program views).
	}
	return nil
}

// layoutOfNode resolves a struct layout for a base register, consulting
// the checker when the syntax-recorded map misses: recorded layouts win,
// then checker interface/class/alias names, then anonymous field sets via
// matchLayout. Nil-safe; never refuses (callers keep their diagnostics).
func (e *emitter) layoutOfNode(base string, n *ast.Node) *layout {
	if l := e.layoutOfVar(base); l != nil {
		return l
	}
	if l := e.layoutOfCheckerName(n); l != nil {
		return l
	}
	if e.tcx == nil || n == nil {
		return nil
	}
	if fields, ok := e.tcx.layoutDataFields(n); ok {
		if l := e.matchLayout(fields); l != nil {
			return l
		}
	}
	return nil
}
