// Top-level executable statements (entry synthesis).
//
// Demos define `function main` and the runtime calls the first `@main`,
// but bare top-level statements (`main();`, `console.log(...)`, loop/if
// scaffolding) have no home: emitted inline they land after function
// bodies (fallthrough trap) or before any label (invalid). Entry
// synthesis collects them, in source order, into a generated `@main`
// lowered after all definitions (two-pass); files without executables
// emit nothing extra (286 byte-identical by construction).
//
// Collision: a user `function main` plus executables renames the user
// definition to `main__user` (definition, signatures and call sites;
// imports and arrows win explicitly). The entry itself always returns 0
// (TS completion values are not exit codes); only explicit top-level
// calls run user code, matching TS evaluation order.
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
)

// mainEntryRename is the user-`main` collision rename when entry
// synthesis claims `@main`.
const mainEntryRename = "main__user"

// isEntryStmt reports top-level statements that execute at module load.
// Declarations (functions, classes, types, namespaces, variable
// statements of any shape, imports/exports) define without emitting
// runtime code; everything else runs.
func (e *emitter) isEntryStmt(st *ast.Node) bool {
	switch st.Kind {
	case ast.KindFunctionDeclaration,
		ast.KindClassDeclaration,
		ast.KindInterfaceDeclaration,
		ast.KindTypeAliasDeclaration,
		ast.KindEnumDeclaration,
		ast.KindImportDeclaration,
		ast.KindExportDeclaration,
		ast.KindExportAssignment,
		ast.KindNamespaceExportDeclaration,
		ast.KindImportEqualsDeclaration,
		ast.KindVariableStatement,
		ast.KindModuleDeclaration:
		return false
	default:
		return true
	}
}

// entryMainName maps a bare top-level `main` to its renamed definition
// when entry synthesis collides (identity otherwise, including inside
// namespaces where names qualify first).
func (e *emitter) entryMainName(name string) string {
	if name == "main" && e.mainRenamed {
		return mainEntryRename
	}
	return name
}

// planEntry splits top-level executables out of the definition stream.
// Non-entry program files refuse theirs loudly (cross-file init order is
// a sequenced gap); a user `main` colliding with synthesis renames.
func (e *emitter) planEntry(stmts []*ast.Node) {
	for _, st := range stmts {
		if e.isEntryStmt(st) {
			e.entryStmts = append(e.entryStmts, st)
		}
	}
	if len(e.entryStmts) == 0 {
		return
	}
	if e.link != nil && e.prefix != "" {
		e.refuse(e.entryStmts[0], "top-level executable statements are not lowerable in non-entry program files (move them into functions)")
		return
	}
	hasMain := false
	for _, st := range stmts {
		if st.Kind == ast.KindFunctionDeclaration && st.Name() != nil &&
			st.Name().Kind == ast.KindIdentifier && st.Name().Text() == "main" {
			hasMain = true
		}
		if st.Kind == ast.KindVariableStatement {
			dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			for _, d := range dl.Declarations.Nodes {
				if name, ok := bindingNameText(d); ok && name == mainEntryRename {
					e.refuse(d, "entry synthesis collides with existing definition main__user (rename it)")
					return
				}
			}
		}
		if st.Kind == ast.KindFunctionDeclaration && st.Name() != nil &&
			st.Name().Kind == ast.KindIdentifier && st.Name().Text() == mainEntryRename {
			e.refuse(st, "entry synthesis collides with existing definition main__user (rename it)")
			return
		}
	}
	if hasMain {
		e.mainRenamed = true
	}
}

// lowerEntry lowers collected top-level executables into a generated
// `@main() -> i32` (entry buffer spliced ahead of definitions at
// finish()). State mirrors out-of-line arrow emission: swapped body with
// saved scopes/ownership/terminators, own scope frame, i32 exit.
func (e *emitter) lowerEntry() {
	savedBody := e.body
	savedOwned := e.owned
	savedScopes := e.scopes
	savedBreaks := e.breaks
	savedConts := e.conts
	savedRet := e.retType
	savedInFunc := e.inFunc
	savedTerm := e.terminated
	e.body = strings.Builder{}
	e.owned = nil
	e.scopes = nil
	e.breaks = nil
	e.conts = nil
	e.retType = tI32
	e.inFunc = true
	e.emitRaw("@%s() -> i32:", e.fnDef("main"))
	e.terminated = false
	e.pushScope()
	for _, st := range e.entryStmts {
		e.lowerBlockStatement(st)
	}
	if !e.terminated {
		e.releaseAllOwned()
		e.emit("return 0")
	}
	e.terminated = false
	e.popScope()
	e.entryBuf = e.body.String()
	e.body = savedBody
	e.owned = savedOwned
	e.scopes = savedScopes
	e.breaks = savedBreaks
	e.conts = savedConts
	e.retType = savedRet
	e.inFunc = savedInFunc
	e.terminated = savedTerm
}
