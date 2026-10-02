// Type context: tsgo binder/checker over the linked file set.
//
// Built once per Lower/LowerProgram (sub-millisecond for small programs
// with NoLib), shared by every file emitter. Nil-safe: every query reports
// "unknown" without a context, preserving exact single-file behavior.
package saemit

import (
	"context"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/checker"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/vfs/vfstest"
)

type typeCtx struct {
	prog  *compiler.Program
	check *checker.Checker
	done  func()
}

// newTypeCtx builds a bound + checked program over files (slash-path keys).
// Returns nil without error when the host cannot be built (callers fall
// back to syntax-only lowering).
func newTypeCtx(files map[string]string) *typeCtx {
	m := map[string]string{}
	names := []string{}
	for k, v := range files {
		abs := "/" + strings.TrimPrefix(k, "/")
		m[abs] = v
		names = append(names, `"`+abs+`"`)
	}
	m["/tsconfig.json"] = `{"compilerOptions": {"noLib": true, "strictNullChecks": true, "module": "commonjs", "target": "esnext"}, "files": [` + strings.Join(names, ",") + `]}`
	fs := bundled.WrapFS(vfstest.FromMap(m, false))
	host := compiler.NewCompilerHost("/", fs, bundled.LibPath(), nil, nil, nil)
	parsed, errs := tsoptions.GetParsedCommandLineOfConfigFile("/tsconfig.json", &core.CompilerOptions{}, nil, host, nil)
	if len(errs) != 0 {
		return nil
	}
	p := compiler.NewProgram(compiler.ProgramOptions{Config: parsed, Host: host})
	p.BindSourceFiles()
	c, done := p.GetTypeChecker(context.Background())
	return &typeCtx{prog: p, check: c, done: done}
}

func (t *typeCtx) close() {
	if t != nil && t.done != nil {
		t.done()
	}
}

// nullable reports whether the static type includes null or undefined
// (drives `?.` guards; unknown without a context).
func (t *typeCtx) nullable(n *ast.Node) bool {
	if t == nil || t.check == nil || n == nil {
		return false
	}
	var ty *checker.Type
	func() {
		defer func() {
			// The checker may fail on out-of-subset shapes (no lib,
			// dynamic idioms); treat as unknown, never fatal.
			_ = recover()
		}()
		ty = t.check.GetTypeAtLocation(n)
	}()
	if ty == nil {
		return false
	}
	if ty.Flags()&checker.TypeFlagsUnionOrIntersection != 0 {
		for _, c := range ty.Types() {
			if isNullish(c.Flags()) {
				return true
			}
		}
		return false
	}
	return isNullish(ty.Flags())
}

// inferredReturnType maps a function declaration/expression's checker
// return type to an SA type when it is a concrete scalar (todo/02#6 fifth
// knife: the "declare `-> T`" gate upgrades from a syntax-Kind refusal to
// a checker-typed verdict). number/string/boolean (literals included)
// mirror annotationType exactly (`number` reads i32, the integer
// discipline); void/undefined read void (the default); any/unknown,
// disagreeing unions/intersections and all exotic shapes yield false so
// the legacy loud refusal applies unchanged.
// Nil-safe: no context (or any checker failure) reports unknown.
func (t *typeCtx) inferredReturnType(fn *ast.Node) (saType, bool) {
	if t == nil || t.check == nil || fn == nil {
		return tUnknown, false
	}
	var out saType
	ok := false
	func() {
		defer func() {
			_ = recover()
		}()
		ty := t.check.GetTypeAtLocation(fn)
		if ty == nil {
			return
		}
		sigs := t.check.GetSignaturesOfType(ty, checker.SignatureKindCall)
		if len(sigs) == 0 {
			return
		}
		rt := t.check.GetReturnTypeOfSignature(sigs[0])
		if rt == nil {
			return
		}
		// Unions fold only when every member agrees on one scalar
		// (the checker spells `boolean` as `true|false`; the same rule
		// typeofKind uses). `string|undefined` and friends disagree and
		// stay loud.
		var flats []*checker.Type
		if rt.Flags()&checker.TypeFlagsUnionOrIntersection != 0 {
			flats = rt.Types()
		} else {
			flats = []*checker.Type{rt}
		}
		if len(flats) == 0 {
			return
		}
		// NOTE: tUnknown spells "i32", so unanimity needs its own
		// boolean sentinel rather than a got==tUnknown comparison.
		got := tUnknown
		have := false
		for _, m := range flats {
			s, good := scalarReturnKind(m)
			if !good {
				return
			}
			if !have {
				got, have = s, true
			} else if got != s {
				return
			}
		}
		out, ok = got, have
	}()
	if !ok {
		return tUnknown, false
	}
	return out, true
}

// prescanRet resolves one function declaration's SA return signature
// with the fixed priority: explicit annotation > co-located .d.ts >
// checker-inferred concrete scalar > void (todo/02#6). Every signature
// table (single-file prescan, namespace members, program links) must use
// it so call sites agree with the emitted definition; a bare
// annotation-only table against an inferred definition drops call values
// silently. Nil-tcx safe (scratch prescans keep legacy void).
func (e *emitter) prescanRet(fn *ast.Node, dts saType, hasDts bool) saType {
	if fd := fn.AsFunctionDeclaration(); fd.Type != nil {
		if ret := annotationType(fd.Type); ret != tUnknown {
			return ret
		}
		return tI32
	}
	if hasDts {
		return dts
	}
	if rt, ok := e.tcx.inferredReturnType(fn); ok {
		return rt
	}
	return tVoid
}

// scalarReturnKind maps one checker type to its SA scalar (mirror of
// annotationType's keyword table: `number` reads i32). any/unknown and
// all exotic shapes fail.
func scalarReturnKind(ty *checker.Type) (saType, bool) {
	f := ty.Flags()
	switch {
	case f&checker.TypeFlagsAnyOrUnknown != 0:
		return tUnknown, false
	case f&checker.TypeFlagsStringLike != 0:
		return tString, true
	case f&checker.TypeFlagsNumberLike != 0:
		return tI32, true
	case f&checker.TypeFlagsBooleanLike != 0:
		return tBool, true
	case f&checker.TypeFlagsVoid != 0 || f&checker.TypeFlagsUndefined != 0:
		return tVoid, true
	}
	return tUnknown, false
}

func isNullish(f checker.TypeFlags) bool {
	return f&checker.TypeFlagsNullable != 0 || f&checker.TypeFlagsUndefined != 0
}

// declaredAt reports whether the binder resolves a name reference at its
// location (any meaning: values, types, imports). It answers scope
// VISIBILITY, not shape: custom scope maps stay authoritative for
// ownership and aliasing, which the binder cannot see (todo/02#7
// boundary). Unknown without a context.
func (t *typeCtx) declaredAt(n *ast.Node) bool {
	if t == nil || t.check == nil || n == nil {
		return false
	}
	found := false
	func() {
		defer func() {
			_ = recover()
		}()
		if sym := t.check.GetSymbolAtLocation(n); sym != nil {
			found = true
		}
	}()
	return found
}
