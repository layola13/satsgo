// typeof guards: `typeof v === "undefined"` (any side order, ==/===/!=/!==)
// lowers to a null check on v. The subset maps null/undefined to 0, so
// this is exact (no checker query needed; other typeof shapes keep their
// existing fold-or-refuse path). Non-identifier operands refuse loudly.
//
// `typeof X === "<kind>"` for other kinds folds to a constant below
// (lowerTypeofConstFold) when X's kind is statically known.
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

// scalarTypeofKind maps scalar annotations to their JS typeof string:
// real TS keywords and dialect scalar names alike. It answers from
// syntax alone, covering exactly the shapes the checker goes blind on
// under NoLib (dialect names resolve to error/any, so typeofKind yields
// false). Strings are excluded (strVars owns them), as are user type
// names, unions and void (checker/linker paths stay authoritative).
func scalarTypeofKind(tn *ast.Node) (string, bool) {
	if tn == nil {
		return "", false
	}
	switch tn.Kind {
	case ast.KindNumberKeyword:
		return "number", true
	case ast.KindBooleanKeyword:
		return "boolean", true
	case ast.KindBigIntKeyword:
		return "bigint", true
	case ast.KindTypeReference:
		switch tn.AsTypeReferenceNode().TypeName.Text() {
		case "number", "i32", "i64", "u64", "f32", "f64":
			return "number", true
		case "boolean":
			return "boolean", true
		case "bigint":
			return "bigint", true
		}
	}
	return "", false
}

// splitTypeofCompare finds a (typeof X, "string") comparison pair in
// either operand order for ==/===/!=/!==. Reports the typeof node, the
// literal text and the negation polarity. Shared by the undefined guard,
// the constant fold and the env-probe fold so the pair shape cannot drift.
func splitTypeofCompare(n *ast.Node) (typeOp *ast.Node, lit string, neg bool, ok bool) {
	if n == nil || n.Kind != ast.KindBinaryExpression {
		return nil, "", false, false
	}
	bin := n.AsBinaryExpression()
	switch bin.OperatorToken.Kind {
	case ast.KindEqualsEqualsToken, ast.KindEqualsEqualsEqualsToken:
		neg = false
	case ast.KindExclamationEqualsToken, ast.KindExclamationEqualsEqualsToken:
		neg = true
	default:
		return nil, "", false, false
	}
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
		l, good := stringLiteralText(other)
		if !good {
			continue
		}
		return side, l, neg, true
	}
	return nil, "", false, false
}

// lowerTypeofGuard handles typeof-against-"undefined" comparisons.
// Reports (value, type, handled); unhandled shapes return false so normal
// lowering (and its diagnostics) apply.
func (e *emitter) lowerTypeofGuard(n *ast.Node) (string, saType, bool) {
	typeOp, lit, neg, ok := splitTypeofCompare(n)
	if !ok || lit != "undefined" {
		return "", tUnknown, false
	}
	inner := typeOp.AsTypeOfExpression().Expression
	if inner == nil || inner.Kind != ast.KindIdentifier {
		e.refuse(n, "typeof guard needs a plain identifier")
		return "0", tUnknown, true
	}
	target := inner
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

// envProbeArm folds `typeof G === "undefined" ? A : B` (any equality,
// either order) to the taken arm when G is a binder-invisible plain
// identifier: SA has no ambient globals, so an undeclared probe name is
// definitionally undefined (planck's `typeof ASSERT === "undefined" ?
// false : ASSERT` idiom). Declared names, non-identifier operands and
// non-ternary shapes return !ok and the normal lowering (with its loud
// diagnostics) applies. The untaken arm is never lowered, so its
// possibly-undeclared value reference cannot trap.
func (e *emitter) envProbeArm(n *ast.Node) (*ast.Node, bool) {
	if n.Kind != ast.KindConditionalExpression {
		return nil, false
	}
	typeOp, lit, neg, ok := splitTypeofCompare(n.AsConditionalExpression().Condition)
	if !ok || lit != "undefined" {
		return nil, false
	}
	inner := typeOp.AsTypeOfExpression().Expression
	if inner == nil || inner.Kind != ast.KindIdentifier {
		return nil, false
	}
	// Binder authority required: in syntax-only fallback everything
	// looks undeclared, and folding there would invent facts.
	if e.tcx == nil || e.tcx.declaredAt(inner) {
		return nil, false
	}
	ce := n.AsConditionalExpression()
	if !neg {
		return ce.WhenTrue, true
	}
	return ce.WhenFalse, true
}

// probeFoldedInit applies the env-probe fold to a declarator initializer
// (module const/let slots examine the AST directly and never reach the
// ternary lowering). Returns init unchanged when no probe applies.
func (e *emitter) probeFoldedInit(init *ast.Node) *ast.Node {
	if init != nil && init.Kind == ast.KindConditionalExpression {
		if arm, ok := e.envProbeArm(init); ok {
			return arm
		}
	}
	return init
}

// lowerTypeofConstFold folds `typeof X === "<kind>"` (any side order,
// ==/===/!=/!==) to a constant immediate when X's static kind is known:
// literals by syntax (mirroring lowerTypeof's literal table exactly,
// including its null -> "undefined" dialect mapping), identifiers via
// the checker (typeofKind, the same source lowerTypeof consults, so the
// verdict can never disagree with the unfolded path). Unknown kinds
// return false so the normal lowering (and its loud diagnostics) applies.
// "undefined" pairs stay on the null-check path above and never reach
// here. The fold replaces two 16-byte string allocs plus an index_of
// call with one constant compare; the verdict materialises into a temp
// (`eq/ne 1, 1`, both `sa check`-clean) because br takes registers,
// never immediates (`br 1 -> ...` traps UnknownRegister).
func (e *emitter) lowerTypeofConstFold(n *ast.Node) (string, saType, bool) {
	typeOp, lit, neg, ok := splitTypeofCompare(n)
	if !ok {
		return "", tUnknown, false
	}
	if lit == "undefined" {
		return "", tUnknown, false
	}
	inner := typeOp.AsTypeOfExpression().Expression
	if inner == nil {
		return "", tUnknown, false
	}
	kind, known := "", false
	switch inner.Kind {
	case ast.KindNumericLiteral:
		kind, known = "number", true
	case ast.KindStringLiteral, ast.KindNoSubstitutionTemplateLiteral:
		kind, known = "string", true
	case ast.KindTrueKeyword, ast.KindFalseKeyword:
		kind, known = "boolean", true
	case ast.KindNullKeyword, ast.KindUndefinedKeyword:
		// Dialect mapping mirrors lowerTypeof (subset null/undefined
		// share 0); NOT JS ("object" for null).
		kind, known = "undefined", true
	case ast.KindArrowFunction, ast.KindFunctionExpression:
		kind, known = "function", true
	case ast.KindArrayLiteralExpression, ast.KindObjectLiteralExpression:
		kind, known = "object", true
	case ast.KindIdentifier:
		// Annotation-derived kinds first (panic-free, declaration
		// truth); checker second. Both agree by construction on real
		// TS types; dialect names only resolve via the former.
		if k, ok := e.kindVars[inner.Text()]; ok {
			kind, known = k, true
		} else if k, ok := e.tcx.typeofKind(inner); ok {
			kind, known = k, true
		}
	}
	if !known {
		return "", tUnknown, false
	}
	t := e.freshTmp()
	if (kind == lit) != neg {
		e.emit("%s = eq 1, 1", t)
	} else {
		e.emit("%s = ne 1, 1", t)
	}
	return t, tI32, true
}
