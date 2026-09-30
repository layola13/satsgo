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
