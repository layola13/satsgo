// Node Buffer surfaces over sa_plugin_node: byteLength (string to u64)
// and concat (array-literal of strings to one slice).
//
// concat takes an array literal ONLY: the toolchain's array model stores
// 4-byte i32 slots that cannot round-trip slices, so dynamic arrays are
// unrepresentable (loud refusal); literal elements lower directly and
// pack through the same argv shape as path.join. Element kinds are
// restricted to identifiers and string literals (side-effect free, so
// lowering at the call site evaluates once).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// lowerBufferByteLength lowers Buffer.byteLength(s) (strings only) to the
// node byte_length primitive (u64 out, status-checked).
func (e *emitter) lowerBufferByteLength(args []string, types []saType, pos *ast.Node) (string, saType) {
	if len(args) != 1 {
		e.refuse(pos, "Buffer.byteLength takes exactly 1 argument")
		return "0", tUnknown
	}
	if len(types) > 0 && types[0] != tString {
		e.refuse(pos, "Buffer.byteLength takes a string")
		return "0", tUnknown
	}
	proj, ok := projectionByTS("Buffer.byteLength")
	if !ok {
		e.refuse(pos, "Buffer.byteLength is not a projected std surface (see StdProjectionTable)")
		return "0", tUnknown
	}
	v, t := e.emitProjCall(proj, args, pos)
	if e.refused {
		return "0", tUnknown
	}
	return v, t
}

// lowerBufferConcat lowers Buffer.concat([a, b, ...]) with a literal
// element list (identifiers and string literals only). Dynamic arrays,
// spreads and non-string elements refuse loudly.
func (e *emitter) lowerBufferConcat(argNodes *ast.ElementList, pos *ast.Node) (string, saType) {
	if argNodes == nil || len(argNodes.Nodes) != 1 {
		e.refuse(pos, "Buffer.concat takes exactly 1 argument (an array literal)")
		return "0", tUnknown
	}
	list := argNodes.Nodes[0]
	if list.Kind != ast.KindArrayLiteralExpression {
		e.refuse(pos, "Buffer.concat takes an array literal (dynamic arrays cannot hold slices)")
		return "0", tUnknown
	}
	parts := []string{}
	for _, el := range list.AsArrayLiteralExpression().Elements.Nodes {
		switch el.Kind {
		case ast.KindIdentifier, ast.KindStringLiteral, ast.KindNoSubstitutionTemplateLiteral:
			v, t := e.lowerExpr(el)
			if e.refused {
				return "0", tUnknown
			}
			if t != tString {
				e.refuse(el, "Buffer.concat elements must be strings")
				return "0", tUnknown
			}
			parts = append(parts, v)
		default:
			e.refuse(el, "Buffer.concat elements must be identifiers or string literals")
			return "0", tUnknown
		}
	}
	proj, ok := projectionByTS("Buffer.concat")
	if !ok {
		e.refuse(pos, "Buffer.concat is not a projected std surface (see StdProjectionTable)")
		return "0", tUnknown
	}
	// The argv branch packs pre-lowered parts directly.
	v, t := e.emitProjCall(proj, parts, pos)
	if e.refused {
		return "0", tUnknown
	}
	return v, t
}
