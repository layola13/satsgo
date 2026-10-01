// Cross-file `namespace N` imports (`import { N }` then N.f()).
//
// Design (mirrors the default-object member binding in link_nsobject.go):
// the namespace itself is not a value; each callable member binds to its
// qualified callee through importEnv (dotted keys), so the existing
// namespace-import call path (nsImports branch) routes N.f() with zero
// new routing code. Unknown members refuse loudly there; non-callable
// members (classes, enums, consts, values) never enter the member table
// (see collectNsMembers), so their uses keep today's loud diagnostics.
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
)

// bindNSMembers binds `import { N }` (namespace-declaration target) per
// member. The local namespace name marks nsImports so calls, destructuring
// and cross-file-root checks treat it like `import * as`.
func bindNSMembers(e *emitter, local, remote string, res *modResolution, members map[string]string) {
	for member, flat := range members {
		q := res.prefix + flat
		if qq, ok := res.qualified[flat]; ok {
			q = qq
		}
		e.importEnv[local+"."+member] = q
		if r, ok := res.rets[flat]; ok {
			e.importRet[local+"."+member] = r
		} else {
			e.importRet[local+"."+member] = tI32
		}
		e.importedNames[local+"."+member] = true
	}
	if e.nsImports == nil {
		e.nsImports = map[string]string{}
	}
	e.nsImports[local] = res.key
	// Nested namespaces bind under dotted locals as well (`N.M.g`
	// from ns path N_M member g); the nested call router below
	// resolves them. Keys outside this namespace stay untouched.
	if res != nil {
		for nsPath := range res.nsMembers {
			if nsPath == remote || !strings.HasPrefix(nsPath, remote+"_") {
				continue
			}
			dotted := local + "." + strings.ReplaceAll(nsPath[len(remote)+1:], "_", ".")
			for member, flat := range res.nsMembers[nsPath] {
				q := res.prefix + flat
				if qq, ok := res.qualified[flat]; ok {
					q = qq
				}
				e.importEnv[dotted+"."+member] = q
				if r, ok := res.rets[flat]; ok {
					e.importRet[dotted+"."+member] = r
				} else {
					e.importRet[dotted+"."+member] = tI32
				}
				e.importedNames[dotted+"."+member] = true
			}
		}
	}
}

// routeNestedNSCall lowers `N.M.g(..)` where N is an imported namespace
// (see bindNSMembers): the receiver spells a bound dotted path. Reports
// handled; an imported root with an unbound path refuses loudly, any
// other shape falls through untouched.
func routeNestedNSCall(e *emitter, recv *ast.Node, method string, args []string, pos *ast.Node) (string, saType, bool) {
	if recv == nil || recv.Kind != ast.KindPropertyAccessExpression {
		return "", tUnknown, false
	}
	pa := recv.AsPropertyAccessExpression()
	if pa.Expression == nil || pa.Expression.Kind != ast.KindIdentifier {
		return "", tUnknown, false
	}
	root := pa.Expression.Text()
	if _, ok := e.nsImports[root]; !ok {
		return "", tUnknown, false
	}
	dotted := root + "." + pa.Name().Text() + "." + method
	q, ok := e.importEnv[dotted]
	if !ok {
		e.refuse(pos, "%s is not exported by its module", dotted)
		return "0", tUnknown, true
	}
	ret := e.importRet[dotted]
	if ret == tVoid {
		e.emit("call @%s(%s)", q, strings.Join(args, ", "))
		return "0", tVoid, true
	}
	t := e.freshTmp()
	e.emit("%s = call @%s(%s)", t, q, strings.Join(args, ", "))
	e.ownTemp(t)
	return t, ret, true
}
