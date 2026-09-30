// typeof guards: `typeof v === "undefined"` (any side order, ==/===/!=/!==)
// lowers to a null check on v. The subset maps null/undefined to 0, so
// this is exact (no checker query needed; other typeof shapes keep their
// existing fold-or-refuse path). Non-identifier operands refuse loudly.
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/checker"
)

// typeofKind folds a statically-known typeof operand to its JS result
// string via checker types (unions fold only when every member agrees;
// any/unknown/never and exotic shapes yield false). JS quirks honored:
// null -> "object", void/undefined -> "undefined", bigint/symbol kept.
func (t *typeCtx) typeofKind(n *ast.Node) (string, bool) {
	ty := t.typeAtNode(n)
	if ty == nil {
		return "", false
	}
	kind := ""
	ok := false
	func() {
		defer func() {
			_ = recover()
		}()
		var flats []*checker.Type
		if ty.Flags()&checker.TypeFlagsUnionOrIntersection != 0 {
			flats = ty.Types()
		} else {
			flats = []*checker.Type{ty}
		}
		for _, m := range flats {
			k, good := typeofKindSingle(t, m)
			if !good {
				return
			}
			if kind == "" {
				kind = k
			} else if kind != k {
				return
			}
		}
		ok = kind != ""
	}()
	if !ok {
		return "", false
	}
	return kind, true
}

func typeofKindSingle(t *typeCtx, ty *checker.Type) (string, bool) {
	f := ty.Flags()
	switch {
	case f&checker.TypeFlagsAnyOrUnknown != 0:
		return "", false
	case f&checker.TypeFlagsStringLike != 0:
		return "string", true
	case f&checker.TypeFlagsNumberLike != 0:
		return "number", true
	case f&checker.TypeFlagsBigIntLike != 0:
		return "bigint", true
	case f&checker.TypeFlagsBooleanLike != 0:
		return "boolean", true
	case f&checker.TypeFlagsESSymbolLike != 0:
		return "symbol", true
	case f&checker.TypeFlagsVoid != 0:
		return "undefined", true
	case f&checker.TypeFlagsUndefined != 0:
		return "undefined", true
	case f&checker.TypeFlagsNull != 0:
		return "object", true
	}
	if len(t.check.GetSignaturesOfType(ty, checker.SignatureKindCall)) > 0 {
		return "function", true
	}
	if f&checker.TypeFlagsObject != 0 {
		return "object", true
	}
	return "", false
}

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
