// typeof guards: `typeof v === "undefined"` (any side order, ==/===/!=/!==)
// lowers to a null check on v. The subset maps null/undefined to 0, so
// this is exact (no checker query needed; other typeof shapes keep their
// existing fold-or-refuse path). Non-identifier operands refuse loudly.
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// lowerTypeofGuard handles typeof-against-"undefined" comparisons.
// Reports (value, type, handled); unhandled shapes return false so normal
// lowering (and its diagnostics) apply.
func (e *emitter) lowerTypeofGuard(n *ast.Node) (string, saType, bool) {
	bin := n.AsBinaryExpression()
	op := bin.OperatorToken.Kind
	neg := false
	switch op {
	case ast.KindEqualsEqualsToken, ast.KindEqualsEqualsEqualsToken:
		neg = false
	case ast.KindExclamationEqualsToken, ast.KindExclamationEqualsEqualsToken:
		neg = true
	default:
		return "", tUnknown, false
	}
	// Find the (typeof v, "undefined") pair in either order.
	var target *ast.Node
	for _, side := range []*ast.Node{bin.Left, bin.Right} {
		other := bin.Right
		if side == bin.Right {
			other = bin.Left
		}
		if side.Kind != ast.KindTypeOfExpression {
			continue
		}
		if other.Kind != ast.KindStringLiteral {
			continue
		}
		lit, ok := stringLiteralText(other)
		if !ok || lit != "undefined" {
			continue
		}
		inner := side.AsTypeOfExpression().Expression
		if inner == nil || inner.Kind != ast.KindIdentifier {
			e.refuse(n, "typeof guard needs a plain identifier")
			return "0", tUnknown, true
		}
		target = inner
		break
	}
	if target == nil {
		return "", tUnknown, false
	}
	v, _ := e.lowerExpr(target)
	if e.refused {
		return "0", tUnknown, true
	}
	t := e.freshTmp()
	if neg {
		e.emit("%s = ne %s, 0", t, v)
	} else {
		e.emit("%s = eq %s, 0", t, v)
	}
	return t, tI32, true
}
