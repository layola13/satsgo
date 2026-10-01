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
	_ = remote
}
