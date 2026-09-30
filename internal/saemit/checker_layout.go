// Checker-driven struct layouts: when the syntax-recorded varLayouts map
// misses (inferred structs such as factory-call results), fall back to
// tsgo checker types — interface/class names resolve into e.layouts,
// anonymous shapes resolve through matchLayout by data-field set.
// Nil-safe without a context; unions take the first named non-nullish
// constituent (mirrors the annotation rule); anything exotic yields nil.
package saemit

import (
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

// layoutOfNode resolves a struct layout for a base register, consulting
// the checker when the syntax-recorded map misses: recorded layouts win,
// then checker interface/class/alias names, then anonymous field sets via
// matchLayout. Nil-safe; never refuses (callers keep their diagnostics).
func (e *emitter) layoutOfNode(base string, n *ast.Node) *layout {
	if l := e.layoutOfVar(base); l != nil {
		return l
	}
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
	if fields, ok := e.tcx.layoutDataFields(n); ok {
		if l := e.matchLayout(fields); l != nil {
			return l
		}
	}
	return nil
}
