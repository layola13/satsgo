// Package saemit is the tsgo → SA bridge.
//
// Pipeline: tsgo (full TypeScript 7 frontend: parser + binder + checker)
//
//	→ subset-TS gate ("ts": only the SA-lowerable subset passes, everything
//	else is a located refusal, never silent bad codegen)
//	→ SA-ASM emitter (".sai") + sci/sa project scaffold.
//
// Design authority:
//   - Emission rules mirror sa_plugin_ts/src/lowerer.zig (br with two targets,
//     jmp for break/continue, panic for throw, `-> T` on value-returning
//     functions, explicit `load`/`store` byte offsets, `!` releases, lazy
//     labels, header `@const`/`@import`).
//   - Standard library policy (hard constraint): every TS std call projects
//     onto a symbol that literally exists in sci/sa_std (*.sai/*.sa/*.sal).
//     The frontend simulates NOTHING. If no sa_std symbol exists, lowering
//     refuses loudly with a located diagnostic (same philosophy as the .wit
//     refusal in sa_plugin_ts).
//
// The projection table lives in stdlib.go. The project scaffold (sa.mod +
// src/main.ts + src/main.sai + README + build.sh) lives in project.go.
package saemit

import (
	"fmt"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tspath"
)

// Diagnostic is a located refusal or warning produced by the subset gate.
type Diagnostic struct {
	File string
	Line int
	Col  int
	Msg  string
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", d.File, d.Line, d.Col, d.Msg)
}

// Result is the outcome of lowering one TS source file.
type Result struct {
	// SAI is the generated SA-ASM text (sci `sa build` input).
	SAI string
	// SubsetTS is the gated subset-TS (the middle "ts" step artifact).
	SubsetTS string
	// Diagnostics are refusals/warnings; a non-empty Refused flag means the
	// .sai is partial and must NOT be assembled.
	Diagnostics []Diagnostic
	Refused     bool
}

// Lower parses + lowers a single TypeScript source to SA-ASM.
//
// It uses the production tsgo parser (not a hand-rolled lexer), so all TS
// syntax is accepted at parse level; the subset gate then decides what is
// lowerable. Type-directed decisions (Phase 1) use explicit annotations +
// literal shapes; Phase 2 will thread checker types through for full
// `number`/`string` inference.
func Lower(fileName, sourceText string) Result {
	abs := fileName
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	opts := ast.SourceFileParseOptions{
		FileName: abs,
		Path:     tspath.ToPath(abs, "/", true),
	}
	sf := parser.ParseSourceFile(opts, sourceText, core.ScriptKindTS)
	e := &emitter{file: fileName, src: sourceText, lines: lineOffsets(sourceText)}
	e.lowerSourceFile(sf)
	return Result{
		SAI:         e.finish(),
		SubsetTS:    sourceText,
		Diagnostics: e.diags,
		Refused:     e.refused,
	}
}

func lineOffsets(src string) []int {
	offs := []int{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			offs = append(offs, i+1)
		}
	}
	return offs
}

func (e *emitter) posLineCol(pos int) (int, int) {
	line := 1
	lineStart := 0
	for i, off := range e.lines {
		if off > pos {
			break
		}
		line = i + 1
		lineStart = off
	}
	return line, pos - lineStart + 1
}

func (e *emitter) refuse(n *ast.Node, format string, args ...any) {
	line, col := e.posLineCol(n.Pos())
	e.diags = append(e.diags, Diagnostic{File: e.file, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)})
	e.refused = true
}

func (e *emitter) warn(n *ast.Node, format string, args ...any) {
	line, col := e.posLineCol(n.Pos())
	e.diags = append(e.diags, Diagnostic{File: e.file, Line: line, Col: col, Msg: "warning: " + fmt.Sprintf(format, args...)})
}

// ---------------------------------------------------------------------------
// SA type model
// ---------------------------------------------------------------------------

// saType is the lowered scalar/container type.
type saType string

const (
	tI32     saType = "i32"
	tI64     saType = "i64"
	tU64     saType = "u64"
	tF64     saType = "f64"
	tBool    saType = "i32" // booleans render as 0/1 (sa_plugin_ts rule)
	tString  saType = "ptr" // 16-byte {ptr,len} slice handle
	tArray   saType = "ptr" // 16-byte {ptr,len} slice header
	tVoid    saType = "void"
	tUnknown saType = "i32"
)

// annotationType renders a TS type annotation to an saType.
// Explicit SA-style names (i32/i64/u64/f64/ptr) are authoritative; core TS
// keywords map deterministically; `number` without annotation info defaults
// to i32 for integer literals (Phase 2: checker type will decide).
func annotationType(tn *ast.Node) saType {
	if tn == nil {
		return tUnknown
	}
	switch tn.Kind {
	case ast.KindNumberKeyword:
		return tI32
	case ast.KindStringKeyword:
		return tString
	case ast.KindBooleanKeyword:
		return tBool
	case ast.KindVoidKeyword:
		return tVoid
	case ast.KindBigIntKeyword:
		return tI64
	case ast.KindTypeReference:
		name := tn.AsTypeReferenceNode().TypeName.Text()
		switch name {
		case "i32":
			return tI32
		case "i64":
			return tI64
		case "u64":
			return tU64
		case "f64", "f32":
			return tF64
		case "string":
			return tString
		case "boolean":
			return tBool
		case "void":
			return tVoid
		case "ptr":
			return tArray
		default:
			// User types (interfaces, classes, generics) lower as
			// ptr-sized handles (mirrors saTypeOf fallback).
			return tArray
		}
	case ast.KindArrayType:
		return tArray
	default:
		return tUnknown
	}
}

// jumpTarget pairs a jump label with the scope depth that owns it, so
// break/continue release exactly the abandoned scopes (never enclosing
// ones, keeping merge states in agreement).
type jumpTarget struct {
	label string
	depth int
}

// layout is one interface's static memory layout: fields in declaration
// order with byte offsets; widths follow sci/sa_std scalar sizes, mirroring
// sa_plugin_ts getTypeSizeAndAlign (i32/u32/number/boolean=4, all handles=8).
type layout struct {
	name    string
	fields  []string
	types   map[string]string // field -> SA type name (saTypeOf shape)
	ftypes  map[string]string // field -> raw TS type name (for nested layouts)
	offsets map[string]int
	size    int
}

// saNameOfType mirrors sa_plugin_ts saTypeOf: SA scalars pass through,
// number/boolean lower as i32, everything else (string, slices, structs)
// is a ptr-sized handle.
func saNameOfType(tn *ast.Node) string {
	if tn == nil {
		return "i32"
	}
	switch tn.Kind {
	case ast.KindNumberKeyword, ast.KindBooleanKeyword:
		return "i32"
	case ast.KindStringKeyword:
		return "ptr"
	case ast.KindVoidKeyword:
		return "void"
	case ast.KindBigIntKeyword:
		return "i64"
	case ast.KindTypeReference:
		name := tn.AsTypeReferenceNode().TypeName.Text()
		switch name {
		case "i32", "u32", "i64", "u64", "f64", "f32", "i8", "u8", "i16", "u16", "bool", "void", "ptr":
			return name
		case "number", "boolean":
			return "i32"
		case "string":
			return "ptr"
		default:
			return "ptr"
		}
	case ast.KindArrayType:
		return "ptr"
	default:
		return "ptr"
	}
}

// widthOf mirrors sa_plugin_ts getTypeSizeAndAlign.
func widthOf(saname string) (size, align int) {
	switch saname {
	case "i32", "u32", "number", "boolean":
		return 4, 4
	case "u8", "i8":
		return 1, 1
	case "u16", "i16":
		return 2, 2
	default:
		return 8, 8
	}
}

func alignTo(off, align int) int {
	if align <= 1 {
		return off
	}
	if r := off % align; r != 0 {
		return off + (align - r)
	}
	return off
}

func fieldWidth(t saType) int {
	if t == tI32 {
		return 4
	}
	return 8
}

// ---------------------------------------------------------------------------
// Emitter
// ---------------------------------------------------------------------------

type emitter struct {
	file    string
	src     string
	lines   []int
	header  strings.Builder // @const / @import preamble
	body    strings.Builder
	diags   []Diagnostic
	refused bool

	tmp     int
	lbl     int
	imports map[string]bool

	// scope tracking for `!` releases
	scopes  []map[string]*binding // bindings per scope (innermost last)
	owned   []string              // emission-order names for reverse release
	breaks  []jumpTarget          // break target stack (label + scope depth)
	conts   []jumpTarget          // continue target stack (label + scope depth)
	inFunc  bool
	retType saType
	// layouts records interface static byte-offset layouts
	// (LayoutTable: TypeID -> {size, field->offset}).
	layouts map[string]*layout
	// varLayouts maps SSA variable names to their struct layout.
	varLayouts map[string]*layout
	// funcSigs pre-registers every top-level function signature so forward
	// calls resolve and void callees emit bare `call` (mirrors the
	// pre-registration rule for async forward calls).
	funcSigs map[string]saType
	// enums maps EnumName -> member -> ordinal (auto-numbered variants).
	enums map[string]map[string]int64
	// importedFrom maps a local value name to its module ("fs"/"net") for
	// `import { readFile } from "fs"` style calls.
	importedFrom map[string]string
	// strVars tracks string slice bindings; arrVars tracks array slice
	// bindings; arrElems records array element SA names (method dispatch
	// resolves the receiver kind like the scope lookup does).
	strVars  map[string]bool
	arrVars  map[string]bool
	arrElems map[string]string
	// terminated tracks SA-ASM well-formedness: the current block already
	// ends in a terminator (jmp/br/ret/panic), so emitting another
	// instruction would produce unreachable code. Mirrors the
	// block_terminated rule in sa_plugin_ts/src/lowerer.zig.
	terminated bool
}

func (e *emitter) freshTmp() string {
	e.tmp++
	return fmt.Sprintf("t_%d", e.tmp)
}

func (e *emitter) freshLabel(prefix string) string {
	e.lbl++
	return fmt.Sprintf("L_%s_%d", prefix, e.lbl)
}

func (e *emitter) needImport(path string) {
	if e.imports == nil {
		e.imports = map[string]bool{}
	}
	if !e.imports[path] {
		e.imports[path] = true
		fmt.Fprintf(&e.header, "@import \"%s\"\n", path)
	}
}

func (e *emitter) emit(format string, args ...any) {
	fmt.Fprintf(&e.body, "    "+format+"\n", args...)
}

func (e *emitter) emitRaw(format string, args ...any) {
	fmt.Fprintf(&e.body, format+"\n", args...)
}

func (e *emitter) finish() string {
	return e.header.String() + e.body.String()
}

// ---------------------------------------------------------------------------
// Top level
// ---------------------------------------------------------------------------

func (e *emitter) lowerSourceFile(sf *ast.SourceFile) {
	stmts := sf.AsSourceFile().Statements.Nodes
	// Pre-scan: register function signatures (forward calls resolve; void
	// callees known before first use), interfaces and enums.
	e.funcSigs = map[string]saType{}
	for _, st := range stmts {
		if st.Kind == ast.KindFunctionDeclaration && st.Name() != nil &&
			st.Name().Kind == ast.KindIdentifier {
			ret := tVoid
			if fd := st.AsFunctionDeclaration(); fd.Type != nil {
				ret = annotationType(fd.Type)
				if ret == tUnknown {
					ret = tI32
				}
			}
			e.funcSigs[st.Name().Text()] = ret
		}
		if st.Kind == ast.KindInterfaceDeclaration {
			e.recordLayout(st)
		}
		if st.Kind == ast.KindEnumDeclaration {
			e.recordEnum(st)
		}
	}
	for _, st := range stmts {
		e.lowerStatement(st, true)
	}
}

func (e *emitter) lowerStatement(st *ast.Node, topLevel bool) {
	switch st.Kind {
	case ast.KindFunctionDeclaration:
		e.lowerFunction(st)
	case ast.KindVariableStatement:
		if topLevel {
			e.refuse(st, "top-level variable statements are not lowerable; move state into function scope")
			return
		}
		e.lowerVarStatement(st)
	case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
		e.lowerTypeDecl(st)
	case ast.KindImportDeclaration, ast.KindExportDeclaration, ast.KindExportAssignment:
		e.lowerModuleDecl(st)
	default:
		e.lowerBlockStatement(st)
	}
}

// lowerBlockStatement handles statements valid inside function bodies.
func (e *emitter) lowerBlockStatement(st *ast.Node) {
	// Nothing may follow a terminator: suppress dead code.
	if e.terminated {
		return
	}
	switch st.Kind {
	case ast.KindVariableStatement:
		e.lowerVarStatement(st)
	case ast.KindIfStatement:
		e.lowerIf(st)
	case ast.KindWhileStatement:
		e.lowerWhile(st)
	case ast.KindForStatement:
		e.lowerFor(st)
	case ast.KindForOfStatement:
		e.lowerForOf(st)
	case ast.KindSwitchStatement:
		e.lowerSwitch(st)
	case ast.KindReturnStatement:
		e.lowerReturn(st)
	case ast.KindExpressionStatement:
		e.lowerExprStatement(st)
	case ast.KindBlock:
		e.pushScope()
		for _, s := range st.Statements() {
			e.lowerBlockStatement(s)
		}
		e.popScope()
	case ast.KindBreakStatement:
		if len(e.breaks) == 0 {
			e.refuse(st, "break outside loop/switch is not lowerable")
			return
		}
		tgt := e.breaks[len(e.breaks)-1]
		e.releaseForJump(tgt.depth)
		e.emit("jmp %s", tgt.label)
		e.terminated = true
	case ast.KindContinueStatement:
		if len(e.conts) == 0 {
			e.refuse(st, "continue outside loop is not lowerable")
			return
		}
		tgt := e.conts[len(e.conts)-1]
		e.releaseForJump(tgt.depth)
		e.emit("jmp %s", tgt.label)
		e.terminated = true
	case ast.KindThrowStatement:
		// SA-ASM has no exception edges: throw lowers to panic (abort).
		e.emit("panic")
		e.terminated = true
	case ast.KindEmptyStatement:
		// no-op
	default:
		e.refuse(st, "statement %s is not in the SA-lowerable subset", st.Kind.String())
	}
}

// ---------------------------------------------------------------------------
// Functions
// ---------------------------------------------------------------------------

func (e *emitter) lowerFunction(fn *ast.Node) {
	name := "<anon>"
	if fn.Name() != nil {
		if fn.Name().Kind == ast.KindIdentifier {
			name = fn.Name().Text()
		} else {
			e.refuse(fn, "computed function names are not in the SA-lowerable subset")
			return
		}
	}
	params := fn.Parameters()
	sig := []string{}
	e.pushScope()
	savedRet := e.retType
	savedInFunc := e.inFunc
	e.inFunc = true
	// Missing annotation means void (mirrors the `-> T` rule: a
	// value-returning function must declare it). The pre-scan agrees.
	e.retType = tVoid
	if fd := fn.AsFunctionDeclaration(); fd.Type != nil {
		e.retType = annotationType(fd.Type)
		if e.retType == tUnknown {
			e.retType = tI32
		}
	}
	for _, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			e.refuse(p.AsNode(), "destructured parameters are not in the SA-lowerable subset")
			continue
		}
		ptype := tI32
		if pd := p.AsParameterDeclaration(); pd.Type != nil {
			ptype = annotationType(pd.Type)
			e.trackBinding(pname, pd.Type, nil, tUnknown)
		}
		sig = append(sig, fmt.Sprintf("%s: %s", pname, ptype))
		e.declareOwned(pname)
	}
	// Body scope sits above the parameter scope, so reads of parameters
	// inside the body snapshot (copies) instead of moving them.
	e.pushScope()
	ret := ""
	if e.retType != tVoid {
		ret = fmt.Sprintf(" -> %s", e.retType)
	}
	e.emitRaw("@%s(%s)%s:", name, strings.Join(sig, ", "), ret)
	e.terminated = false
	body := fn.BodyData().Body
	if body == nil {
		e.refuse(fn, "function %s has no body (overload signatures are not lowerable)", name)
	} else {
		for _, s := range body.Statements() {
			e.lowerBlockStatement(s)
		}
	}
	// Implicit fallthrough only when the body does not already terminate:
	// nothing may follow a terminator.
	if !e.terminated {
		e.releaseAllOwned()
		if e.retType == tVoid {
			e.emit("return")
		} else {
			e.emit("return 0")
		}
	}
	e.terminated = false
	e.popScope() // body scope
	e.popScope() // parameter scope
	e.retType = savedRet
	e.inFunc = savedInFunc
}

// ---------------------------------------------------------------------------
// Variables
// ---------------------------------------------------------------------------

func (e *emitter) lowerVarStatement(st *ast.Node) {
	e.lowerVarDeclList(st.AsVariableStatement().DeclarationList)
}

// lowerVarDeclList lowers a VariableDeclarationList node (shared by
// VariableStatement and C-style for initializers, which hold the list
// directly rather than wrapped in a statement).
func (e *emitter) lowerVarDeclList(list *ast.Node) {
	dl := list.AsVariableDeclarationList()
	for _, d := range dl.Declarations.Nodes {
		name, ok := bindingNameText(d)
		if !ok {
			e.refuse(d, "destructuring declarations are not in the SA-lowerable subset")
			continue
		}
		atype := annotationType(d.AsVariableDeclaration().Type)
		init := d.Initializer()
		if init == nil {
			e.refuse(d, "declaration of %s without initializer is not lowerable", name)
			continue
		}
		val, vtype := e.lowerExpr(init)
		if atype == tUnknown {
			atype = vtype
		}
		_ = atype
		e.assign(name, val, operandKind(val, init), vtype, d)
		e.trackBinding(name, d.AsVariableDeclaration().Type, init, vtype)
	}
}

// trackBinding records receiver-kind information for method dispatch:
// struct layouts, string/array bindings and array element SA names.
func (e *emitter) trackBinding(name string, annot, init *ast.Node, vtype saType) {
	if e.varLayouts == nil {
		e.varLayouts = map[string]*layout{}
	}
	if e.strVars == nil {
		e.strVars = map[string]bool{}
	}
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	// struct-typed bindings remember their layout for field access.
	if annot != nil && annot.Kind == ast.KindTypeReference {
		if l, ok := e.layouts[annot.AsTypeReferenceNode().TypeName.Text()]; ok {
			e.varLayouts[name] = l
		}
	}
	// object literals self-report their layout via matchLayout.
	if init != nil && init.Kind == ast.KindObjectLiteralExpression {
		if l := e.layoutOfLiteral(init); l != nil {
			e.varLayouts[name] = l
		}
	}
	if annot != nil && annot.Kind == ast.KindArrayType {
		e.arrVars[name] = true
		e.arrElems[name] = saNameOfType(annot.AsArrayTypeNode().ElementType)
		return
	}
	// string bindings: explicit annotation or string-valued initializer.
	if (annot != nil && annotationType(annot) == tString) || vtype == tString {
		e.strVars[name] = true
		return
	}
	if vtype == tArray {
		e.arrVars[name] = true
		if _, ok := e.arrElems[name]; !ok {
			e.arrElems[name] = "i32"
		}
	}
}

// ---------------------------------------------------------------------------
// Control flow (br always carries BOTH targets; break/continue are jmp)
// ---------------------------------------------------------------------------

func (e *emitter) lowerIf(st *ast.Node) {
	is := st.AsIfStatement()
	cond, _ := e.lowerExpr(is.Expression)
	thenL := e.freshLabel("then")
	elseL := e.freshLabel("else")
	endL := e.freshLabel("endif")
	if is.ElseStatement != nil {
		e.emit("br %s -> %s, %s", cond, thenL, elseL)
	} else {
		e.emit("br %s -> %s, %s", cond, thenL, endL)
	}
	e.emitRaw("%s:", thenL)
	e.pushScope()
	e.terminated = false
	e.lowerBranchBody(is.ThenStatement)
	e.releaseScope()
	e.popScope()
	thenTerm := e.terminated
	if !thenTerm {
		e.emit("jmp %s", endL)
	}
	elseTerm := false
	if is.ElseStatement != nil {
		e.emitRaw("%s:", elseL)
		e.pushScope()
		e.terminated = false
		e.lowerBranchBody(is.ElseStatement)
		e.releaseScope()
		e.popScope()
		elseTerm = e.terminated
		if !elseTerm {
			e.emit("jmp %s", endL)
		}
	}
	if is.ElseStatement != nil && thenTerm && elseTerm {
		// Both arms terminate: no fallthrough block exists; the br targets
		// are complete on their own. (No-else always falls through on the
		// false path, so the merge label is still required there.)
		e.terminated = true
		return
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
}

func (e *emitter) lowerBranchBody(s *ast.Node) {
	if s.Kind == ast.KindBlock {
		for _, x := range s.Statements() {
			e.lowerBlockStatement(x)
		}
		return
	}
	e.lowerBlockStatement(s)
}

func (e *emitter) lowerWhile(st *ast.Node) {
	ws := st.AsWhileStatement()
	topL := e.freshLabel("while_top")
	bodyL := e.freshLabel("while_body")
	endL := e.freshLabel("while_end")
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.conts = append(e.conts, jumpTarget{topL, len(e.scopes)})
	e.emitRaw("%s:", topL)
	cond, _ := e.lowerExpr(ws.Expression)
	e.emit("br %s -> %s, %s", cond, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	e.terminated = false
	e.lowerBranchBody(ws.Statement)
	e.releaseScope()
	e.popScope()
	bodyTerm := e.terminated
	if !bodyTerm {
		e.emit("jmp %s", topL)
	}
	e.emitRaw("%s:", endL)
	// A loop may exit normally: the merge is always reachable.
	e.terminated = false
	e.breaks = e.breaks[:len(e.breaks)-1]
	e.conts = e.conts[:len(e.conts)-1]
}

func (e *emitter) lowerFor(st *ast.Node) {
	fs := st.AsForStatement()
	e.pushScope()
	if fs.Initializer != nil {
		switch fs.Initializer.Kind {
		case ast.KindVariableStatement:
			e.lowerVarStatement(fs.Initializer)
		case ast.KindVariableDeclarationList:
			e.lowerVarDeclList(fs.Initializer)
		default:
			e.lowerExpr(fs.Initializer)
		}
	}
	topL := e.freshLabel("for_top")
	bodyL := e.freshLabel("for_body")
	endL := e.freshLabel("for_end")
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.conts = append(e.conts, jumpTarget{topL, len(e.scopes)})
	e.emitRaw("%s:", topL)
	if fs.Condition != nil {
		cond, _ := e.lowerExpr(fs.Condition)
		e.emit("br %s -> %s, %s", cond, bodyL, endL)
	} else {
		e.emit("jmp %s", bodyL)
	}
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	e.terminated = false
	e.lowerBranchBody(fs.Statement)
	// increment runs past the loop body (execution order, not parse order)
	if !e.terminated && fs.Incrementor != nil {
		e.lowerExpr(fs.Incrementor)
	}
	e.releaseScope()
	e.popScope()
	bodyTerm := e.terminated
	if !bodyTerm {
		e.emit("jmp %s", topL)
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.releaseScope()
	e.popScope()
	e.breaks = e.breaks[:len(e.breaks)-1]
	e.conts = e.conts[:len(e.conts)-1]
}

func (e *emitter) lowerForOf(st *ast.Node) {
	// Desugar: for (const x of arr) → indexed walk over the 16-byte slice.
	fo := st.AsForInOrOfStatement()
	arrVal, _ := e.lowerExpr(fo.Expression)
	idx := e.freshTmp()
	e.emit("%s = 0", idx)
	topL := e.freshLabel("forof_top")
	bodyL := e.freshLabel("forof_body")
	endL := e.freshLabel("forof_end")
	lenT := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", lenT, arrVal)
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.conts = append(e.conts, jumpTarget{topL, len(e.scopes)})
	e.emitRaw("%s:", topL)
	cT := e.freshTmp()
	e.emit("%s = slt %s, %s", cT, idx, lenT)
	e.emit("br %s -> %s, %s", cT, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	// bind element: elemT = base[idx]
	baseT := e.freshTmp()
	offT := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", baseT, arrVal)
	e.emit("%s = mul %s, 4", offT, idx)
	elemPtr := e.freshTmp()
	e.emit("%s = add %s, %s", elemPtr, baseT, offT)
	elemT := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", elemT, elemPtr)
	binding := foBindingName(fo)
	e.assign(binding, elemT, "temp", tI32, st)
	e.terminated = false
	e.lowerBranchBody(fo.Statement)
	if !e.terminated {
		incT := e.freshTmp()
		e.emit("%s = add %s, 1", incT, idx)
		e.emit("%s = %s", idx, incT)
	}
	e.releaseScope()
	e.popScope()
	if !e.terminated {
		e.emit("jmp %s", topL)
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.breaks = e.breaks[:len(e.breaks)-1]
	e.conts = e.conts[:len(e.conts)-1]
}

// lowerSwitch emits the guarded case-test chain, mirroring sa_plugin_ts
// parseSwitch: each case gets a test label (eq scrutinee/case -> body/next
// test) and a body label; bodies run sequentially so fallthrough is natural;
// break targets the end label via the breaks stack; an unmatched scrutinee
// lands on the default body or exits.
func (e *emitter) lowerSwitch(st *ast.Node) {
	sw := st.AsSwitchStatement()
	disc, _ := e.lowerExpr(sw.Expression)
	endL := e.freshLabel("endswitch")
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	clauses := sw.CaseBlock.AsCaseBlock().Clauses.Nodes
	// Split cases from default; default runs at the fallthrough point.
	type casePart struct {
		node *ast.Node
	}
	parts := []casePart{}
	var defaultNode *ast.Node
	for _, cl := range clauses {
		switch cl.Kind {
		case ast.KindCaseClause:
			parts = append(parts, casePart{node: cl})
		case ast.KindDefaultClause:
			if defaultNode != nil {
				e.refuse(cl, "multiple default clauses are not lowerable")
				continue
			}
			defaultNode = cl
		default:
			e.refuse(cl, "switch clause %s is not lowerable", cl.Kind.String())
		}
	}
	// Re-emit with stable chained labels: test_i -> body_i / test_{i+1}.
	testLabels := make([]string, len(parts)+1)
	bodyLabels := make([]string, len(parts))
	for i := range parts {
		testLabels[i] = e.freshLabel("case_t")
		bodyLabels[i] = e.freshLabel("case_b")
	}
	testLabels[len(parts)] = e.freshLabel("case_default")
	for i, p := range parts {
		e.emitRaw("%s:", testLabels[i])
		expr := p.node.AsCaseOrDefaultClause().Expression
		val, _ := e.lowerExpr(expr)
		cmp := e.freshTmp()
		e.emit("%s = eq %s, %s", cmp, disc, val)
		e.emit("br %s -> %s, %s", cmp, bodyLabels[i], testLabels[i+1])
		e.emitRaw("%s:", bodyLabels[i])
		e.pushScope()
		e.terminated = false
		for _, s := range p.node.AsCaseOrDefaultClause().Statements.Nodes {
			e.lowerBlockStatement(s)
		}
		e.releaseScope()
		e.popScope()
		if !e.terminated {
			e.emit("jmp %s", endL)
		}
	}
	e.emitRaw("%s:", testLabels[len(parts)])
	if defaultNode != nil {
		e.pushScope()
		e.terminated = false
		for _, s := range defaultNode.AsCaseOrDefaultClause().Statements.Nodes {
			e.lowerBlockStatement(s)
		}
		e.releaseScope()
		e.popScope()
		if !e.terminated {
			e.emit("jmp %s", endL)
		}
	} else {
		e.emit("jmp %s", endL)
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.breaks = e.breaks[:len(e.breaks)-1]
}

func (e *emitter) lowerReturn(st *ast.Node) {
	rs := st.AsReturnStatement()
	if rs.Expression != nil {
		if e.retType == tVoid {
			e.refuse(st, "function returns a value but declares no return type (a value-returning function needs `-> T`)")
			return
		}
		v, _ := e.lowerExpr(rs.Expression)
		e.releaseAllOwnedExcept(v)
		e.emit("return %s", v)
	} else {
		if e.retType != tVoid {
			e.refuse(st, "bare return in a value-returning function is not lowerable (return an explicit value)")
			return
		}
		e.releaseAllOwned()
		e.emit("return")
	}
	e.terminated = true
}

func (e *emitter) lowerExprStatement(st *ast.Node) {
	ex := st.AsExpressionStatement().Expression
	e.lowerExpr(ex)
}

// ---------------------------------------------------------------------------
// Expressions → value operands (temps or immediates)
// ---------------------------------------------------------------------------

func (e *emitter) lowerExpr(n *ast.Node) (string, saType) {
	switch n.Kind {
	case ast.KindNumericLiteral:
		text := n.Text()
		if isFloatLiteral(text) {
			return text, tF64
		}
		return text, tI32
	case ast.KindStringLiteral, ast.KindNoSubstitutionTemplateLiteral:
		return e.lowerStringLiteral(n.Text()), tString
	case ast.KindTrueKeyword:
		return "1", tBool
	case ast.KindFalseKeyword:
		return "0", tBool
	case ast.KindIdentifier:
		return n.Text(), tI32
	case ast.KindBinaryExpression:
		return e.lowerBinary(n)
	case ast.KindPrefixUnaryExpression:
		return e.lowerPrefixUnary(n)
	case ast.KindPostfixUnaryExpression:
		return e.lowerPostfixUnary(n)
	case ast.KindObjectLiteralExpression:
		return e.lowerObjectLiteral(n)
	case ast.KindCallExpression:
		return e.lowerCall(n)
	case ast.KindPropertyAccessExpression:
		return e.lowerPropertyAccess(n)
	case ast.KindElementAccessExpression:
		return e.lowerElementAccess(n)
	case ast.KindArrayLiteralExpression:
		return e.lowerArrayLiteral(n)
	case ast.KindNewExpression:
		return e.lowerNew(n)
	case ast.KindParenthesizedExpression:
		return e.lowerExpr(n.Expression())
	case ast.KindTemplateExpression:
		return e.lowerTemplate(n)
	case ast.KindConditionalExpression:
		return e.lowerTernary(n)
	case ast.KindAsExpression, ast.KindSatisfiesExpression, ast.KindNonNullExpression:
		return e.lowerExpr(n.Expression())
	default:
		e.refuse(n, "expression %s is not in the SA-lowerable subset", n.Kind.String())
		return "0", tUnknown
	}
}

func isFloatLiteral(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] == '.' || text[i] == 'e' || text[i] == 'E' {
			return true
		}
	}
	return false
}

// lowerStringLiteral materializes a real slice: @const utf8 + 16-byte header.
func (e *emitter) lowerStringLiteral(text string) string {
	e.tmp++
	cname := fmt.Sprintf("str_const_%d", e.tmp)
	escaped := strings.ReplaceAll(text, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	fmt.Fprintf(&e.header, "@const %s = utf8:\"%s\\0\"\n", cname, escaped)
	h := e.freshTmp()
	e.emit("%s = alloc 16", h)
	e.emit("store %s + 0, &%s as ptr", h, cname)
	e.emit("store %s + 8, %d as u64", h, len(text))
	e.declareOwned(h)
	return h
}

func (e *emitter) lowerBinary(n *ast.Node) (string, saType) {
	bin := n.AsBinaryExpression()
	op := bin.OperatorToken.Kind
	// assignment folds to register copy (plain `s = "..."` is NOT valid SA).
	if op == ast.KindEqualsToken {
		rhs, rtype := e.lowerExpr(bin.Right)
		if bin.Left.Kind == ast.KindIdentifier {
			e.assign(bin.Left.Text(), rhs, operandKind(rhs, bin.Right), rtype, n)
			return bin.Left.Text(), tI32
		}
		if bin.Left.Kind == ast.KindElementAccessExpression {
			e.lowerElementStore(bin.Left, rhs)
			return rhs, tI32
		}
		if bin.Left.Kind == ast.KindPropertyAccessExpression {
			if e.lowerFieldStore(bin.Left, rhs) {
				return rhs, tI32
			}
		}
		e.refuse(n, "assignment target is not lowerable")
		return "0", tUnknown
	}
	if isCompoundAssign(op) {
		e.lowerCompoundAssign(bin, op)
		return "0", tI32
	}
	l, lt := e.lowerExpr(bin.Left)
	r, rt := e.lowerExpr(bin.Right)
	floats := lt == tF64 || rt == tF64
	t := e.freshTmp()
	switch op {
	case ast.KindPlusToken:
		if floats {
			e.emit("%s = fadd %s, %s", t, l, r)
		} else {
			e.emit("%s = add %s, %s", t, l, r)
		}
	case ast.KindMinusToken:
		if floats {
			e.emit("%s = fsub %s, %s", t, l, r)
		} else {
			e.emit("%s = sub %s, %s", t, l, r)
		}
	case ast.KindAsteriskToken:
		if floats {
			e.emit("%s = fmul %s, %s", t, l, r)
		} else {
			e.emit("%s = mul %s, %s", t, l, r)
		}
	case ast.KindSlashToken:
		if floats {
			e.emit("%s = fdiv %s, %s", t, l, r)
		} else {
			e.emit("%s = div %s, %s", t, l, r)
		}
	case ast.KindPercentToken:
		if floats {
			e.refuse(n, "float %% lowers to no SA-ASM instruction (there is no frem); refuse loudly")
			return "0", tUnknown
		}
		e.emit("%s = srem %s, %s", t, l, r)
	case ast.KindAsteriskAsteriskToken:
		if floats {
			e.refuse(n, "** on floats is not lowerable (integer pow loop only)")
			return "0", tUnknown
		}
		return e.lowerPowLoop(l, r), tI32
	case ast.KindLessThanLessThanToken:
		e.emit("%s = shl %s, %s", t, l, r)
	case ast.KindGreaterThanGreaterThanToken:
		e.emit("%s = ashr %s, %s", t, l, r)
	case ast.KindGreaterThanGreaterThanGreaterThanToken:
		e.emit("%s = lshr %s, %s", t, l, r)
	case ast.KindAmpersandToken:
		e.emit("%s = and %s, %s", t, l, r)
	case ast.KindBarToken:
		e.emit("%s = or %s, %s", t, l, r)
	case ast.KindCaretToken:
		e.emit("%s = xor %s, %s", t, l, r)
	case ast.KindEqualsEqualsToken, ast.KindEqualsEqualsEqualsToken:
		if floats {
			e.emit("%s = fcmp_eq %s, %s", t, l, r)
		} else {
			e.emit("%s = eq %s, %s", t, l, r)
		}
	case ast.KindExclamationEqualsToken, ast.KindExclamationEqualsEqualsToken:
		if floats {
			e.emit("%s = fcmp_ne %s, %s", t, l, r)
		} else {
			e.emit("%s = ne %s, %s", t, l, r)
		}
	case ast.KindLessThanToken:
		if floats {
			e.emit("%s = fcmp_lt %s, %s", t, l, r)
		} else {
			e.emit("%s = slt %s, %s", t, l, r)
		}
	case ast.KindLessThanEqualsToken:
		if floats {
			e.emit("%s = fcmp_le %s, %s", t, l, r)
		} else {
			e.emit("%s = sle %s, %s", t, l, r)
		}
	case ast.KindGreaterThanToken:
		if floats {
			e.emit("%s = fcmp_gt %s, %s", t, l, r)
		} else {
			e.emit("%s = sgt %s, %s", t, l, r)
		}
	case ast.KindGreaterThanEqualsToken:
		if floats {
			e.emit("%s = fcmp_ge %s, %s", t, l, r)
		} else {
			e.emit("%s = sge %s, %s", t, l, r)
		}
	case ast.KindAmpersandAmpersandToken:
		e.emit("%s = and %s, %s", t, l, r)
	case ast.KindBarBarToken:
		e.emit("%s = or %s, %s", t, l, r)
	default:
		e.refuse(n, "binary operator %s is not in the SA-lowerable subset", op.String())
		return "0", tUnknown
	}
	rt2 := tI32
	if floats && isComparison(op) {
		rt2 = tBool
	} else if floats {
		rt2 = tF64
	}
	_ = rt2
	if floats && !isComparison(op) {
		return t, tF64
	}
	return t, tI32
}

// lowerPowLoop mirrors the sa_plugin_ts integer pow loop:
// r=1; ctr=expo; top: cc=sgt ctr,0; br body/end; body: r*=base; ctr--; jmp top.
func (e *emitter) lowerPowLoop(base, expo string) string {
	res := e.freshTmp()
	e.emit("%s = 1", res)
	topL := e.freshLabel("pow_top")
	bodyL := e.freshLabel("pow_body")
	endL := e.freshLabel("pow_end")
	ctr := e.freshTmp()
	e.emit("%s = add %s, 0", ctr, expo)
	e.emitRaw("%s:", topL)
	cc := e.freshTmp()
	e.emit("%s = sgt %s, 0", cc, ctr)
	e.emit("br %s -> %s, %s", cc, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	nr := e.freshTmp()
	e.emit("%s = mul %s, %s", nr, res, base)
	e.emit("%s = %s", res, nr)
	nc := e.freshTmp()
	e.emit("%s = sub %s, 1", nc, ctr)
	e.emit("%s = %s", ctr, nc)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return res
}

func isComparison(op ast.Kind) bool {
	switch op {
	case ast.KindEqualsEqualsToken, ast.KindEqualsEqualsEqualsToken,
		ast.KindExclamationEqualsToken, ast.KindExclamationEqualsEqualsToken,
		ast.KindLessThanToken, ast.KindLessThanEqualsToken,
		ast.KindGreaterThanToken, ast.KindGreaterThanEqualsToken:
		return true
	}
	return false
}

func isCompoundAssign(op ast.Kind) bool {
	switch op {
	case ast.KindPlusEqualsToken, ast.KindMinusEqualsToken,
		ast.KindAsteriskEqualsToken, ast.KindSlashEqualsToken,
		ast.KindPercentEqualsToken, ast.KindLessThanLessThanEqualsToken,
		ast.KindGreaterThanGreaterThanEqualsToken,
		ast.KindGreaterThanGreaterThanGreaterThanEqualsToken,
		ast.KindAmpersandEqualsToken, ast.KindBarEqualsToken,
		ast.KindCaretEqualsToken:
		return true
	}
	return false
}

func (e *emitter) lowerCompoundAssign(bin *ast.BinaryExpression, op ast.Kind) {
	base := map[ast.Kind]ast.Kind{
		ast.KindPlusEqualsToken:                              ast.KindPlusToken,
		ast.KindMinusEqualsToken:                             ast.KindMinusToken,
		ast.KindAsteriskEqualsToken:                          ast.KindAsteriskToken,
		ast.KindSlashEqualsToken:                             ast.KindSlashToken,
		ast.KindPercentEqualsToken:                           ast.KindPercentToken,
		ast.KindLessThanLessThanEqualsToken:                  ast.KindLessThanLessThanToken,
		ast.KindGreaterThanGreaterThanEqualsToken:            ast.KindGreaterThanGreaterThanToken,
		ast.KindGreaterThanGreaterThanGreaterThanEqualsToken: ast.KindGreaterThanGreaterThanGreaterThanToken,
		ast.KindAmpersandEqualsToken:                         ast.KindAmpersandToken,
		ast.KindBarEqualsToken:                               ast.KindBarToken,
		ast.KindCaretEqualsToken:                             ast.KindCaretToken,
	}[op]
	l, _ := e.lowerExpr(bin.Left)
	r, _ := e.lowerExpr(bin.Right)
	t := e.freshTmp()
	switch base {
	case ast.KindPlusToken:
		e.emit("%s = add %s, %s", t, l, r)
	case ast.KindMinusToken:
		e.emit("%s = sub %s, %s", t, l, r)
	case ast.KindAsteriskToken:
		e.emit("%s = mul %s, %s", t, l, r)
	case ast.KindSlashToken:
		e.emit("%s = div %s, %s", t, l, r)
	case ast.KindPercentToken:
		e.emit("%s = srem %s, %s", t, l, r)
	case ast.KindLessThanLessThanToken:
		e.emit("%s = shl %s, %s", t, l, r)
	case ast.KindGreaterThanGreaterThanToken:
		e.emit("%s = ashr %s, %s", t, l, r)
	case ast.KindGreaterThanGreaterThanGreaterThanToken:
		e.emit("%s = lshr %s, %s", t, l, r)
	case ast.KindAmpersandToken:
		e.emit("%s = and %s, %s", t, l, r)
	case ast.KindBarToken:
		e.emit("%s = or %s, %s", t, l, r)
	case ast.KindCaretToken:
		e.emit("%s = xor %s, %s", t, l, r)
	}
	if bin.Left.Kind == ast.KindIdentifier {
		e.assign(bin.Left.Text(), t, "temp", tI32, bin.Left)
	} else {
		e.refuse(bin.Left, "compound assignment target is not lowerable")
	}
}

func (e *emitter) lowerPrefixUnary(n *ast.Node) (string, saType) {
	un := n.AsPrefixUnaryExpression()
	op := un.Operator
	// ++i / --i desugar to i = i ± 1 returning the new value.
	if op == ast.KindPlusPlusToken || op == ast.KindMinusMinusToken {
		return e.lowerIncDec(un.Operand, op == ast.KindPlusPlusToken, false, n)
	}
	arg, at := e.lowerExpr(un.Operand)
	t := e.freshTmp()
	switch op {
	case ast.KindMinusToken:
		if at == tF64 {
			e.emit("%s = fneg %s", t, arg)
			return t, tF64
		}
		e.emit("%s = sub 0, %s", t, arg)
		return t, tI32
	case ast.KindExclamationToken:
		e.emit("%s = eq %s, 0", t, arg)
		return t, tBool
	default:
		e.refuse(n, "prefix operator %s is not in the SA-lowerable subset", op.String())
		return "0", tUnknown
	}
}

// lowerPostfixUnary lowers i++ / i-- returning the OLD value (JS semantics:
// old = i; i = i ± 1).
func (e *emitter) lowerPostfixUnary(n *ast.Node) (string, saType) {
	un := n.AsPostfixUnaryExpression()
	if un.Operator != ast.KindPlusPlusToken && un.Operator != ast.KindMinusMinusToken {
		e.refuse(n, "postfix operator %s is not in the SA-lowerable subset", un.Operator.String())
		return "0", tUnknown
	}
	return e.lowerIncDec(un.Operand, un.Operator == ast.KindPlusPlusToken, true, n)
}

func (e *emitter) lowerIncDec(operand *ast.Node, up, postfix bool, pos *ast.Node) (string, saType) {
	op := "add"
	if !up {
		op = "sub"
	}
	// Single-segment member update obj.f++ / index update a[i]++: load at
	// the static offset / indexed slot, add/sub, store back (mirrors
	// sa_plugin_ts member-update and arr[i]++/-- write-back).
	if operand.Kind == ast.KindPropertyAccessExpression {
		pa := operand.AsPropertyAccessExpression()
		if pa.Expression.Kind == ast.KindIdentifier {
			if l := e.layoutOfVar(pa.Expression.Text()); l != nil {
				if off, ok := l.offsets[pa.Name().Text()]; ok {
					saname := l.types[pa.Name().Text()]
					base, _ := e.lowerExpr(pa.Expression)
					cur := e.freshTmp()
					nxt := e.freshTmp()
					e.emit("%s = load %s + %d as %s", cur, base, off, saname)
					e.emit("%s = %s %s, 1", nxt, op, cur)
					e.emit("store %s + %d, %s as %s", base, off, nxt, saname)
					if postfix {
						return cur, tI32
					}
					return nxt, tI32
				}
			}
		}
		e.refuse(pos, "++/-- member target has no recorded layout")
		return "0", tUnknown
	}
	if operand.Kind == ast.KindElementAccessExpression {
		ea := operand.AsElementAccessExpression()
		base, _ := e.lowerExpr(ea.Expression)
		idx, _ := e.lowerExpr(ea.ArgumentExpression)
		baseT := e.freshTmp()
		offT := e.freshTmp()
		ptrT := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", baseT, base)
		e.emit("%s = mul %s, 4", offT, idx)
		e.emit("%s = add %s, %s", ptrT, baseT, offT)
		cur := e.freshTmp()
		nxt := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", cur, ptrT)
		e.emit("%s = %s %s, 1", nxt, op, cur)
		e.emit("store %s + 0, %s as i32", ptrT, nxt)
		if postfix {
			return cur, tI32
		}
		return nxt, tI32
	}
	if operand.Kind != ast.KindIdentifier {
		e.refuse(pos, "++/-- target is not lowerable (scalars only)")
		return "0", tUnknown
	}
	name := operand.Text()
	one := "1"
	if postfix {
		// Snapshot old (postfix delivers pre-increment value), compute new,
		// rebind with old-value release (reference postfix shape).
		old := e.freshTmp()
		nw := e.freshTmp()
		e.assign(old, name, "named", tI32, pos)
		e.emit("%s = %s %s, %s", nw, op, name, one)
		e.assign(name, nw, "temp", tI32, pos)
		return old, tI32
	}
	nw := e.freshTmp()
	e.emit("%s = %s %s, %s", nw, op, name, one)
	e.assign(name, nw, "temp", tI32, pos)
	return nw, tI32
}

// ---------------------------------------------------------------------------
// Calls: user functions + stdlib projection (sci/sa_std reuse, never simulate)
// ---------------------------------------------------------------------------

func (e *emitter) lowerCall(n *ast.Node) (string, saType) {
	call := n.AsCallExpression()
	args := []string{}
	for _, a := range call.Arguments.Nodes {
		v, _ := e.lowerExpr(a)
		args = append(args, v)
	}
	// console.log(...) → @sa_print_bytes via sa_std/io/print.sai
	if isConsoleLog(call.Expression) {
		return e.lowerConsoleLog(args), tVoid
	}
	// Math.* inline idioms (reference math_surface; trig etc. refuse).
	if name, ok := mathMethod(call.Expression); ok {
		if v, t, ok := e.lowerMathCall(name, args, call.Arguments, n); ok {
			return v, t
		}
		e.refuse(n, "Math.%s is not supported", name)
		return "0", tUnknown
	}
	// String.fromCharCode → sa_std/string.sai (policy: primitives live in sci)
	if isStringFromCharCode(call.Expression) {
		e.needImport("sa_std/string.sai")
		t := e.freshTmp()
		e.emit("%s = call @sa_string_from_char_code(%s)", t, strings.Join(args, ", "))
		e.ownTemp(t)
		return t, tString
	}
	// new Map() is a NewExpression, not a call; plain identifier calls are user fns.
	if call.Expression.Kind == ast.KindIdentifier {
		fname := call.Expression.Text()
		if proj, ok := globalFnProjection(fname); ok {
			e.needImport(proj.Module)
			t := e.freshTmp()
			e.emit("%s = call @%s(%s)", t, proj.Symbol, strings.Join(args, ", "))
			e.ownTemp(t)
			return t, proj.Ret
		}
		// Bare alloc(N) is the primitive instruction passthrough, result
		// discarded or bound (mirrors the sa_plugin_ts alloc shape).
		if fname == "alloc" && len(args) == 1 {
			t := e.freshTmp()
			e.emit("%s = alloc %s", t, args[0])
			e.declareOwned(t)
			return t, tArray
		}
		// Named std imports: readFile(...) with `import { readFile } from "fs"`.
		if mod, ok := e.importedFrom[fname]; ok {
			if proj, ok := projectionByTS(mod + "." + fname); ok {
				return e.emitProjCall(proj, args)
			}
			e.refuse(n, "%s.%s is not a projected std surface (see StdProjectionTable)", mod, fname)
			return "0", tUnknown
		}
		if ret, ok := e.funcSigs[fname]; ok {
			if ret == tVoid {
				e.emit("call @%s(%s)", fname, strings.Join(args, ", "))
				return "0", tVoid
			}
			t := e.freshTmp()
			e.emit("%s = call @%s(%s)", t, fname, strings.Join(args, ", "))
			e.ownTemp(t)
			return t, ret
		}
		e.refuse(n, "call to unknown function %s (declare it before use)", fname)
		return "0", tUnknown
	}
	// Method calls: Math.* inline idioms and string-method projections
	// (sci/sa_std reuse), mirroring sa_plugin_ts lib surface tables.
	if call.Expression.Kind == ast.KindPropertyAccessExpression {
		if v, t, ok := e.lowerMethodCall(call.Expression, args, n); ok {
			return v, t
		}
	}
	e.refuse(n, "call target is not in the SA-lowerable subset (functions are not first-class values)")
	return "0", tUnknown
}

// lowerMethodCall dispatches property calls. It returns ok=false when the
// method is not a projected surface (caller refuses loudly).
func (e *emitter) lowerMethodCall(fn *ast.Node, args []string, pos *ast.Node) (string, saType, bool) {
	pa := fn.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindIdentifier {
		return "", tUnknown, false
	}
	recv := pa.Expression.Text()
	method := pa.Name().Text()
	// Math is handled at the call site (needs argument nodes for spread).
	// Receiver-kind dispatch (mirrors the scope lookup): string bindings
	// route to the string surface, array bindings to array methods.
	if e.strVars[recv] {
		if v, t, ok := e.lowerStringMethod(recv, method, args, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
	}
	if v, t, ok := e.lowerArrayMethod(recv, method, args, pos); ok {
		return v, t, true
	}
	if e.arrVars[recv] {
		return "", tUnknown, false
	}
	if v, t, ok := e.lowerStringMethod(recv, method, args, pos); ok {
		return v, t, true
	}
	return "", tUnknown, false
}

// lowerMathCall dispatches Math.* per the projection table: @inline shapes,
// @const folds (property position only; calling a const refuses), unknown
// methods refuse (trig etc. are MathNotSupported in the reference).
func (e *emitter) lowerMathCall(method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	proj, ok := projectionByTS("Math." + method)
	if !ok {
		return "", tUnknown, false
	}
	if strings.HasPrefix(proj.Symbol, "@const:") {
		e.refuse(pos, "Math.%s is a constant, not a function", method)
		return "0", tUnknown, true
	}
	switch method {
	case "abs", "pow", "floor", "ceil", "round", "trunc":
		if v, t, ok := e.lowerMathInline(method, args, pos); ok {
			return v, t, true
		}
		return "", tUnknown, true
	case "min", "max":
		return e.lowerMathMinMax(method, args, argNodes, pos)
	}
	return "", tUnknown, false
}

// lowerMathMinMax mirrors the reference: spread reduces a slice (init
// INT_MIN/INT_MAX + take/skip loop), two scalars fold via join slot.
func (e *emitter) lowerMathMinMax(method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	isMax := method == "max"
	if argNodes != nil {
		for _, a := range argNodes.Nodes {
			if a.Kind == ast.KindSpreadElement {
				return e.lowerMathSpreadMinMax(isMax, args, a, pos)
			}
		}
	}
	if len(args) != 2 {
		e.refuse(pos, "Math.%s takes two scalars or one spread slice", method)
		return "0", tUnknown, true
	}
	// Pairwise fold via join slot (ternary shape).
	cmp := e.freshTmp()
	if isMax {
		e.emit("%s = sgt %s, %s", cmp, args[0], args[1])
	} else {
		e.emit("%s = slt %s, %s", cmp, args[0], args[1])
	}
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	tL := e.freshLabel("mm_t")
	fL := e.freshLabel("mm_f")
	endL := e.freshLabel("mm_end")
	e.emit("br %s -> %s, %s", cmp, tL, fL)
	e.emitRaw("%s:", tL)
	e.emit("store %s + 0, %s as ptr", slot, args[0])
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", fL)
	e.emit("store %s + 0, %s as ptr", slot, args[1])
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	out := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", out, slot)
	e.releaseIfOwnedTemp(slot)
	return out, tI32, true
}

func (e *emitter) lowerMathSpreadMinMax(isMax bool, args []string, spread *ast.Node, pos *ast.Node) (string, saType, bool) {
	arr := ""
	if se := spread.AsSpreadElement(); se.Expression != nil {
		v, _ := e.lowerExpr(se.Expression)
		arr = v
	} else {
		e.refuse(pos, "spread min/max needs a slice operand")
		return "0", tUnknown, true
	}
	_ = args
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, arr)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, arr)
	best := e.freshTmp()
	if isMax {
		e.emit("%s = -2147483648", best)
	} else {
		e.emit("%s = 2147483647", best)
	}
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("mm_top")
	bodyL := e.freshLabel("mm_body")
	endL := e.freshLabel("mm_end")
	takeL := e.freshLabel("mm_take")
	skipL := e.freshLabel("mm_skip")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	off := e.freshTmp()
	e.emit("%s = mul %s, 4", off, i)
	addr := e.freshTmp()
	e.emit("%s = add %s, %s", addr, data, off)
	elem := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", elem, addr)
	cmp := e.freshTmp()
	if isMax {
		e.emit("%s = sgt %s, %s", cmp, elem, best)
	} else {
		e.emit("%s = slt %s, %s", cmp, elem, best)
	}
	e.emit("br %s -> %s, %s", cmp, takeL, skipL)
	e.emitRaw("%s:", takeL)
	nb := e.freshTmp()
	e.emit("%s = add %s, 0", nb, elem)
	e.emit("%s = %s", best, nb)
	e.emit("jmp %s", skipL)
	e.emitRaw("%s:", skipL)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return best, tI32, true
}

// lowerMathInline mirrors the sa_plugin_ts Math shapes: abs via branch join,
// pow via integer loop, floor/ceil/round/trunc as integer identity or float
// conversion (fptosi with sign adjust).
func (e *emitter) lowerMathInline(method string, args []string, pos *ast.Node) (string, saType, bool) {
	switch method {
	case "abs":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		v := args[0]
		slot := e.freshTmp()
		e.emit("%s = alloc 8", slot)
		e.ownTemp(slot)
		c := e.freshTmp()
		tL := e.freshLabel("abs_t")
		fL := e.freshLabel("abs_f")
		endL := e.freshLabel("abs_end")
		e.emit("%s = sge %s, 0", c, v)
		e.emit("br %s -> %s, %s", c, tL, fL)
		e.emitRaw("%s:", tL)
		e.emit("store %s + 0, %s as ptr", slot, v)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", fL)
		nv := e.freshTmp()
		e.emit("%s = sub 0, %s", nv, v)
		e.emit("store %s + 0, %s as ptr", slot, nv)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", endL)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", out, slot)
		e.releaseIfOwnedTemp(slot)
		return out, tI32, true
	case "pow":
		if len(args) != 2 {
			return "", tUnknown, false
		}
		base, expo := args[0], args[1]
		res := e.freshTmp()
		e.emit("%s = 1", res)
		topL := e.freshLabel("pow_top")
		bodyL := e.freshLabel("pow_body")
		endL := e.freshLabel("pow_end")
		ctr := e.freshTmp()
		e.emit("%s = add %s, 0", ctr, expo)
		e.emitRaw("%s:", topL)
		cc := e.freshTmp()
		e.emit("%s = sgt %s, 0", cc, ctr)
		e.emit("br %s -> %s, %s", cc, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		nr := e.freshTmp()
		e.emit("%s = mul %s, %s", nr, res, base)
		e.emit("%s = %s", res, nr)
		nc := e.freshTmp()
		e.emit("%s = sub %s, 1", nc, ctr)
		e.emit("%s = %s", ctr, nc)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return res, tI32, true
	case "floor", "ceil", "round", "trunc":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		return e.lowerMathRounding(method, args[0]), tI32, true
	}
	return "", tUnknown, false
}

// lowerMathRounding: integer operands are identity; float operands convert
// (floor = trunc + adjust-down when negative; ceil(x) = -floor(-x);
// round(x) = floor(x+0.5); trunc = fptosi).
func (e *emitter) lowerMathRounding(method, v string) string {
	if !isFloatOperand(v) {
		out := e.freshTmp()
		e.emit("%s = add %s, 0", out, v)
		return out
	}
	farg := v
	negateOut := false
	if method == "ceil" {
		fn := e.freshTmp()
		e.emit("%s = fneg %s", fn, v)
		farg = fn
		negateOut = true
	} else if method == "round" {
		fh := e.freshTmp()
		e.emit("%s = fadd %s, 0.5", fh, v)
		farg = fh
	} else if method == "trunc" {
		ft := e.freshTmp()
		e.emit("%s = fptosi %s", ft, v)
		return ft
	}
	// floor path: t = fptosi farg; if farg<0 and fractional, t -= 1.
	t := e.freshTmp()
	e.emit("%s = fptosi %s", t, farg)
	isNeg := e.freshTmp()
	e.emit("%s = fcmp_lt %s, 0.0", isNeg, farg)
	back := e.freshTmp()
	e.emit("%s = sitofp %s", back, t)
	isFrac := e.freshTmp()
	e.emit("%s = fcmp_ne %s, %s", isFrac, farg, back)
	need := e.freshTmp()
	e.emit("%s = and %s, %s", need, isNeg, isFrac)
	adjL := e.freshLabel("fl_adj")
	endL := e.freshLabel("fl_end")
	e.emit("br %s -> %s, %s", need, adjL, endL)
	e.emitRaw("%s:", adjL)
	dec := e.freshTmp()
	e.emit("%s = sub %s, 1", dec, t)
	e.emit("%s = %s", t, dec)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	if negateOut {
		out := e.freshTmp()
		e.emit("%s = sub 0, %s", out, t)
		return out
	}
	return t
}

// isFloatOperand is the Phase-1 float heuristic: immediates with a fraction
// marker or temps known to carry f64. (Phase 2 uses checker types.)
func isFloatOperand(v string) bool {
	return isFloatLiteral(v)
}

// lowerArrayPush is the grow-copy push: allocate (len+1)*esz, copy old
// elements, append v, swap the header. Returns the new length.
func (e *emitter) lowerArrayPush(arr, val, elem string, esz int) string {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, arr)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, arr)
	nlen := e.freshTmp()
	e.emit("%s = add %s, 1", nlen, ln)
	nbytes := e.freshTmp()
	e.emit("%s = mul %s, %d", nbytes, nlen, esz)
	ndata := e.freshTmp()
	allocOk := e.freshLabel("push_alloc")
	allocEmpty := e.freshLabel("push_empty")
	allocDone := e.freshLabel("push_done")
	isempty := e.freshTmp()
	e.emit("%s = eq %s, 0", isempty, nbytes)
	e.emit("br %s -> %s, %s", isempty, allocEmpty, allocOk)
	e.emitRaw("%s:", allocEmpty)
	e.emit("%s = alloc 4", ndata)
	e.emit("jmp %s", allocDone)
	e.emitRaw("%s:", allocOk)
	e.emit("%s = alloc %s", ndata, nbytes)
	e.emitRaw("%s:", allocDone)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	copyL := e.freshLabel("push_copy")
	bodyL := e.freshLabel("push_body")
	endL := e.freshLabel("push_end")
	e.emitRaw("%s:", copyL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	soff := e.freshTmp()
	e.emit("%s = mul %s, %d", soff, i, esz)
	saddr := e.freshTmp()
	e.emit("%s = add %s, %s", saddr, data, soff)
	tmp := e.freshTmp()
	e.emit("%s = load %s + 0 as %s", tmp, saddr, elem)
	daddr := e.freshTmp()
	e.emit("%s = add %s, %s", daddr, ndata, soff)
	e.emit("store %s + 0, %s as %s", daddr, tmp, elem)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", copyL)
	e.emitRaw("%s:", endL)
	voff := e.freshTmp()
	e.emit("%s = mul %s, %d", voff, ln, esz)
	vaddr := e.freshTmp()
	e.emit("%s = add %s, %s", vaddr, ndata, voff)
	e.emit("store %s + 0, %s as %s", vaddr, val, elem)
	e.emit("store %s + 0, %s as ptr", arr, ndata)
	e.emit("store %s + 8, %s as u64", arr, nlen)
	e.emit("!%s", ndata)
	return nlen
}

// lowerInsertionSort is the in-place numeric insertion sort.
func (e *emitter) lowerInsertionSort(arr string) {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, arr)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, arr)
	topL := e.freshLabel("sort_top")
	bodyL := e.freshLabel("sort_body")
	endL := e.freshLabel("sort_end")
	i := e.freshTmp()
	e.emit("%s = 1", i)
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	ioff := e.freshTmp()
	e.emit("%s = mul %s, 4", ioff, i)
	iaddr := e.freshTmp()
	e.emit("%s = add %s, %s", iaddr, data, ioff)
	key := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", key, iaddr)
	j := e.freshTmp()
	e.emit("%s = sub %s, 1", j, i)
	inTop := e.freshLabel("sort_in_top")
	inChk := e.freshLabel("sort_in_chk")
	inBody := e.freshLabel("sort_in_body")
	inEnd := e.freshLabel("sort_in_end")
	e.emitRaw("%s:", inTop)
	c1 := e.freshTmp()
	e.emit("%s = sge %s, 0", c1, j)
	e.emit("br %s -> %s, %s", c1, inChk, inEnd)
	e.emitRaw("%s:", inChk)
	joff := e.freshTmp()
	e.emit("%s = mul %s, 4", joff, j)
	jaddr := e.freshTmp()
	e.emit("%s = add %s, %s", jaddr, data, joff)
	aj := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", aj, jaddr)
	c2 := e.freshTmp()
	e.emit("%s = sgt %s, %s", c2, aj, key)
	e.emit("br %s -> %s, %s", c2, inBody, inEnd)
	e.emitRaw("%s:", inBody)
	j1 := e.freshTmp()
	e.emit("%s = add %s, 1", j1, j)
	j1off := e.freshTmp()
	e.emit("%s = mul %s, 4", j1off, j1)
	j1addr := e.freshTmp()
	e.emit("%s = add %s, %s", j1addr, data, j1off)
	e.emit("store %s + 0, %s as i32", j1addr, aj)
	jm := e.freshTmp()
	e.emit("%s = sub %s, 1", jm, j)
	e.emit("%s = %s", j, jm)
	e.emit("jmp %s", inTop)
	e.emitRaw("%s:", inEnd)
	k1 := e.freshTmp()
	e.emit("%s = add %s, 1", k1, j)
	k1off := e.freshTmp()
	e.emit("%s = mul %s, 4", k1off, k1)
	k1addr := e.freshTmp()
	e.emit("%s = add %s, %s", k1addr, data, k1off)
	e.emit("store %s + 0, %s as i32", k1addr, key)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
}

// expandSlice expands a {ptr,len} slice handle into its pointer/length pair.
func (e *emitter) expandSlice(h string) (string, string) {
	p := e.freshTmp()
	l := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", p, h)
	e.emit("%s = load %s + 8 as u64", l, h)
	return p, l
}

// lowerArrayMethod mirrors the sa_plugin_ts array idioms: grow-copy push,
// pop (last + shrink), shift (drop 0 + slide), unshift (push-then-rotate),
// fill (per-element stores) and no-arg numeric insertion sort.
func (e *emitter) lowerArrayMethod(recv, method string, args []string, pos *ast.Node) (string, saType, bool) {
	elem := e.arrElems[recv]
	if elem == "" {
		elem = "i32"
	}
	esz, _ := widthOf(elem)
	switch method {
	case "push":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		return e.lowerArrayPush(recv, args[0], elem, esz), tI32, true
	case "pop":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		ln := e.freshTmp()
		e.emit("%s = load %s + 8 as u64", ln, recv)
		last := e.freshTmp()
		e.emit("%s = sub %s, 1", last, ln)
		data := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", data, recv)
		off := e.freshTmp()
		e.emit("%s = mul %s, %d", off, last, esz)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, data, off)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as %s", out, addr, elem)
		e.emit("store %s + 8, %s as u64", recv, last)
		return out, tI32, true
	case "shift":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		ln := e.freshTmp()
		e.emit("%s = load %s + 8 as u64", ln, recv)
		data := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", data, recv)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as %s", out, data, elem)
		nlen := e.freshTmp()
		e.emit("%s = sub %s, 1", nlen, ln)
		topL := e.freshLabel("sh_top")
		bodyL := e.freshLabel("sh_body")
		endL := e.freshLabel("sh_end")
		i := e.freshTmp()
		e.emit("%s = 0", i)
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, nlen)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		src := e.freshTmp()
		e.emit("%s = add %s, 1", src, i)
		soff := e.freshTmp()
		e.emit("%s = mul %s, %d", soff, src, esz)
		saddr := e.freshTmp()
		e.emit("%s = add %s, %s", saddr, data, soff)
		tmp := e.freshTmp()
		e.emit("%s = load %s + 0 as %s", tmp, saddr, elem)
		doff := e.freshTmp()
		e.emit("%s = mul %s, %d", doff, i, esz)
		daddr := e.freshTmp()
		e.emit("%s = add %s, %s", daddr, data, doff)
		e.emit("store %s + 0, %s as %s", daddr, tmp, elem)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		e.emit("store %s + 8, %s as u64", recv, nlen)
		return out, tI32, true
	case "unshift":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		nlen := e.lowerArrayPush(recv, args[0], elem, esz)
		data := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", data, recv)
		topL := e.freshLabel("unsh_top")
		bodyL := e.freshLabel("unsh_body")
		endL := e.freshLabel("unsh_end")
		i := e.freshTmp()
		e.emit("%s = sub %s, 1", i, nlen)
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = sgt %s, 0", c, i)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		prev := e.freshTmp()
		e.emit("%s = sub %s, 1", prev, i)
		soff := e.freshTmp()
		e.emit("%s = mul %s, %d", soff, prev, esz)
		saddr := e.freshTmp()
		e.emit("%s = add %s, %s", saddr, data, soff)
		tmp := e.freshTmp()
		e.emit("%s = load %s + 0 as %s", tmp, saddr, elem)
		doff := e.freshTmp()
		e.emit("%s = mul %s, %d", doff, i, esz)
		daddr := e.freshTmp()
		e.emit("%s = add %s, %s", daddr, data, doff)
		e.emit("store %s + 0, %s as %s", daddr, tmp, elem)
		e.emit("%s = %s", i, prev)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		e.emit("store %s + 0, %s as %s", data, args[0], elem)
		return nlen, tI32, true
	case "fill":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ln := e.freshTmp()
		e.emit("%s = load %s + 8 as u64", ln, recv)
		data := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", data, recv)
		topL := e.freshLabel("fill_top")
		bodyL := e.freshLabel("fill_body")
		endL := e.freshLabel("fill_end")
		i := e.freshTmp()
		e.emit("%s = 0", i)
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		off := e.freshTmp()
		e.emit("%s = mul %s, %d", off, i, esz)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, data, off)
		e.emit("store %s + 0, %s as %s", addr, args[0], elem)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return recv, tArray, true
	case "sort":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		if elem != "i32" && elem != "u32" {
			e.refuse(pos, "Array.sort without a comparator only lowers for numeric arrays")
			return "0", tUnknown, false
		}
		e.lowerInsertionSort(recv)
		return recv, tArray, true
	}
	return "", tUnknown, false
}

// lowerStringMethod projects string methods onto sa_std/string.sai.
func (e *emitter) lowerStringMethod(recv, method string, args []string, pos *ast.Node) (string, saType, bool) {
	e.needImport("sa_std/string.sai")
	bp, bl := e.expandSlice(recv)
	call1 := func(sym string, extra ...string) (string, saType) {
		all := append([]string{bp, bl}, extra...)
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, sym, strings.Join(all, ", "))
		return t, tI32
	}
	callStr := func(sym string, extra ...string) (string, saType) {
		all := append([]string{bp, bl}, extra...)
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, sym, strings.Join(all, ", "))
		e.declareOwned(t)
		return t, tString
	}
	switch method {
	case "charCodeAt":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		t := e.freshTmp()
		e.emit("%s = call @sa_string_code_point_at(%s, %s, %s)", t, bp, bl, args[0])
		return t, tI32, true
	case "indexOf", "lastIndexOf":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		np, nl := e.expandSlice(args[0])
		from := "0"
		if len(args) > 1 {
			from = args[1]
		}
		sym := "sa_string_index_of"
		if method == "lastIndexOf" {
			sym = "sa_string_last_index_of"
		}
		v, t := call1(sym, np, nl, from)
		return v, t, true
	case "startsWith", "endsWith":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		np, nl := e.expandSlice(args[0])
		sym := "sa_string_starts_with"
		if method == "endsWith" {
			sym = "sa_string_ends_with"
		}
		v, t := call1(sym, np, nl)
		return v, t, true
	case "toLowerCase":
		v, t := callStr("sa_string_to_lower_ascii")
		return v, t, true
	case "toUpperCase":
		v, t := callStr("sa_string_to_upper_ascii")
		return v, t, true
	case "repeat":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		v, t := callStr("sa_string_repeat", args[0])
		return v, t, true
	case "padStart", "padEnd":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		padArg := e.lowerStringLiteral(" ")
		if len(args) > 1 {
			padArg = args[1]
		}
		pp, pl := e.expandSlice(padArg)
		sym := "sa_string_pad_start"
		if method == "padEnd" {
			sym = "sa_string_pad_end"
		}
		v, t := callStr(sym, args[0], pp, pl)
		return v, t, true
	case "replace":
		if len(args) != 3 && len(args) != 2 {
			return "", tUnknown, false
		}
		np, nl := e.expandSlice(args[0])
		rp, rl := e.expandSlice(args[1])
		all := "0"
		if len(args) == 3 {
			all = args[2]
		}
		v, t := callStr("sa_string_replace", np, nl, rp, rl, all)
		return v, t, true
	}
	return "", tUnknown, false
}

func isConsoleLog(fn *ast.Node) bool {
	if fn.Kind != ast.KindPropertyAccessExpression {
		return false
	}
	pa := fn.AsPropertyAccessExpression()
	return pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "console" && pa.Name().Text() == "log"
}

// lowerConsoleLog normalizes each operand to a text slice then joins with a
// space + trailing newline, mirroring sa_plugin_ts emitPrintln shape.
func (e *emitter) lowerConsoleLog(args []string) string {
	e.needImport("sa_std/io/print.sai")
	for _, a := range args {
		// Slice fields load into temps; the pointer carries `&`
		// (copied from sa_plugin_sla emitPrintln via the reference).
		bp, bl := e.expandSlice(a)
		e.emit("call @sa_print_bytes(&%s, %s)", bp, bl)
		e.releaseIfOwnedTemp(bp)
		e.releaseIfOwnedTemp(bl)
	}
	return "0"
}

func (e *emitter) lowerPropertyAccess(n *ast.Node) (string, saType) {
	pa := n.AsPropertyAccessExpression()
	// Enum.Member folds to its ordinal as a value.
	if pa.Expression.Kind == ast.KindIdentifier {
		if members, ok := e.enums[pa.Expression.Text()]; ok {
			if ord, ok := members[pa.Name().Text()]; ok {
				return fmt.Sprintf("%d", ord), tI32
			}
			e.refuse(n, "unknown enum member %s.%s", pa.Expression.Text(), pa.Name().Text())
			return "0", tUnknown
		}
		// Math.PI/E and Number.* integer constants fold to immediates.
		if v, ok := mathConstFold(pa.Expression.Text(), pa.Name().Text()); ok {
			return v, tI32
		}
	}
	// s.length aliases the string slice len field.
	if pa.Name().Text() == "length" {
		base, _ := e.lowerExpr(pa.Expression)
		t := e.freshTmp()
		e.emit("%s = load %s + 8 as u64", t, base)
		return t, tI32
	}
	// Interface field read, including nested chains (o.inner.a): each
	// segment lowers to a static-offset load, mirroring sa_plugin_ts
	// member chains.
	if v, t, ok := e.lowerMemberChain(n); ok {
		return v, t
	}
	// String/Array method projections handled at call sites; bare property
	// reads other than .length are refused.
	e.refuse(n, "property access .%s is not in the SA-lowerable subset", pa.Name().Text())
	return "0", tUnknown
}

// lowerMemberChain lowers a.b.c... to nested static-offset loads. It returns
// false when the base has no recorded layout.
func (e *emitter) lowerMemberChain(n *ast.Node) (string, saType, bool) {
	segs := []string{}
	cur := n
	for cur.Kind == ast.KindPropertyAccessExpression {
		pa := cur.AsPropertyAccessExpression()
		segs = append([]string{pa.Name().Text()}, segs...)
		cur = pa.Expression
	}
	if cur.Kind != ast.KindIdentifier {
		return "", tUnknown, false
	}
	segs = append([]string{cur.Text()}, segs...)
	l := e.layoutOfVar(segs[0])
	if l == nil {
		return "", tUnknown, false
	}
	base, _ := e.lowerExpr(cur)
	curOp := base
	for i := 1; i < len(segs); i++ {
		off, ok := l.offsets[segs[i]]
		if !ok {
			return "", tUnknown, false
		}
		saname := l.types[segs[i]]
		t := e.freshTmp()
		e.emit("%s = load %s + %d as %s", t, curOp, off, saname)
		curOp = t
		if i+1 < len(segs) {
			// Intermediate segment: descend into the nested layout.
			nl, ok := e.layouts[l.ftypes[segs[i]]]
			if !ok {
				return "", tUnknown, false
			}
			l = nl
		}
	}
	return curOp, tI32, true
}

// lowerFieldStore lowers obj.field = v (and nested chains) through the
// static layout.
func (e *emitter) lowerFieldStore(target *ast.Node, rhs string) bool {
	segs := []string{}
	cur := target
	for cur.Kind == ast.KindPropertyAccessExpression {
		pa := cur.AsPropertyAccessExpression()
		segs = append([]string{pa.Name().Text()}, segs...)
		cur = pa.Expression
	}
	if cur.Kind != ast.KindIdentifier {
		return false
	}
	segs = append([]string{cur.Text()}, segs...)
	l := e.layoutOfVar(segs[0])
	if l == nil {
		return false
	}
	base, _ := e.lowerExpr(cur)
	curOp := base
	for i := 1; i+1 < len(segs); i++ {
		off, ok := l.offsets[segs[i]]
		if !ok {
			return false
		}
		t := e.freshTmp()
		e.emit("%s = load %s + %d as ptr", t, curOp, off)
		curOp = t
		nl, ok := e.layouts[l.ftypes[segs[i]]]
		if !ok {
			return false
		}
		l = nl
	}
	last := segs[len(segs)-1]
	off, ok := l.offsets[last]
	if !ok {
		return false
	}
	e.emit("store %s + %d, %s as %s", curOp, off, rhs, l.types[last])
	return true
}

func (e *emitter) lowerElementAccess(n *ast.Node) (string, saType) {
	ea := n.AsElementAccessExpression()
	base, _ := e.lowerExpr(ea.Expression)
	idx, _ := e.lowerExpr(ea.ArgumentExpression)
	// Bounds-checked slice load (mirrors emitCheckedIndexLoad); the
	// optional form a?.[i] additionally guards a null base.
	optional := ea.QuestionDotToken != nil
	return e.lowerCheckedIndex(base, idx, optional), tI32
}

func (e *emitter) lowerElementStore(target *ast.Node, rhs string) {
	ea := target.AsElementAccessExpression()
	base, _ := e.lowerExpr(ea.Expression)
	idx, _ := e.lowerExpr(ea.ArgumentExpression)
	baseT := e.freshTmp()
	offT := e.freshTmp()
	ptrT := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", baseT, base)
	e.emit("%s = mul %s, 4", offT, idx)
	e.emit("%s = add %s, %s", ptrT, baseT, offT)
	e.emit("store %s + 0, %s as i32", ptrT, rhs)
}

// lowerTernary mirrors sa_plugin_ts parseTernary: branch+slot join. Each
// arm stores into an alloc'd slot; the merge loads with the arm SA type
// (arms must agree; mixed int/float refuses loudly).
func (e *emitter) lowerTernary(n *ast.Node) (string, saType) {
	ce := n.AsConditionalExpression()
	cond, _ := e.lowerExpr(ce.Condition)
	tv, tt := e.lowerExpr(ce.WhenTrue)
	fv, ft := e.lowerExpr(ce.WhenFalse)
	// Mixed int/float arms promote the integer side via sitofp so the join
	// stays single-typed (type-directed join).
	if tt != ft {
		if tt == tI32 && ft == tF64 {
			c := e.freshTmp()
			e.emit("%s = sitofp %s", c, tv)
			tv, tt = c, tF64
		} else if tt == tF64 && ft == tI32 {
			c := e.freshTmp()
			e.emit("%s = sitofp %s", c, fv)
			fv, ft = c, tF64
		} else {
			e.refuse(n, "ternary arms disagree (%s vs %s); mixed-type joins are not lowerable", tt, ft)
			return "0", tUnknown
		}
	}
	saname := "i32"
	if tt == tF64 {
		saname = "f64"
	} else if tt == tString || tt == tArray {
		saname = "ptr"
	}
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	tL := e.freshLabel("tern_t")
	fL := e.freshLabel("tern_f")
	endL := e.freshLabel("tern_end")
	e.emit("br %s -> %s, %s", cond, tL, fL)
	e.emitRaw("%s:", tL)
	e.emit("store %s + 0, %s as %s", slot, tv, saname)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", fL)
	e.emit("store %s + 0, %s as %s", slot, fv, saname)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	res := e.freshTmp()
	e.emit("%s = load %s + 0 as %s", res, slot, saname)
	e.releaseIfOwnedTemp(slot)
	return res, tt
}

// lowerCheckedIndex mirrors sa_plugin_ts emitCheckedIndexLoad: an
// out-of-bounds index yields 0 (JS undefined maps to 0). The optional form
// a?.[i] additionally guards a null base.
func (e *emitter) lowerCheckedIndex(base, idx string, optional bool) string {
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	endL := e.freshLabel("idx_end")
	oobL := e.freshLabel("idx_oob")
	loadL := e.freshLabel("idx_ok")
	if optional {
		nullL := e.freshLabel("idx_null")
		chkL := e.freshLabel("idx_chk")
		isnull := e.freshTmp()
		e.emit("%s = eq %s, 0", isnull, base)
		e.emit("br %s -> %s, %s", isnull, nullL, chkL)
		e.emitRaw("%s:", nullL)
		e.emit("store %s + 0, 0 as ptr", slot)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", chkL)
	}
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, base)
	ok := e.freshTmp()
	e.emit("%s = ult %s, %s", ok, idx, ln)
	e.emit("br %s -> %s, %s", ok, loadL, oobL)
	e.emitRaw("%s:", loadL)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, base)
	off := e.freshTmp()
	e.emit("%s = mul %s, 4", off, idx)
	addr := e.freshTmp()
	e.emit("%s = add %s, %s", addr, data, off)
	v := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", v, addr)
	e.emit("store %s + 0, %s as ptr", slot, v)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", oobL)
	e.emit("store %s + 0, 0 as ptr", slot)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	dest := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", dest, slot)
	e.releaseIfOwnedTemp(slot)
	return dest
}

// lowerNew mirrors the sa_plugin_ts new-expressions: new Map() lowers to
// the real sa_std/btree_map.sa backend; new Array(n) with an integer
// length lowers to the 16-byte {ptr,len} slice header plus a zeroed buffer.
func (e *emitter) lowerNew(n *ast.Node) (string, saType) {
	nw := n.AsNewExpression()
	if nw.Expression.Kind == ast.KindIdentifier {
		name := nw.Expression.Text()
		if name == "Map" {
			e.needImport("sa_std/btree_map.sa")
			t := e.freshTmp()
			e.emit("%s = call @sa_btree_map_new()", t)
			e.declareOwned(t)
			return t, tArray
		}
		if name == "Array" && nw.Arguments != nil && len(nw.Arguments.Nodes) == 1 {
			// Element buffer is length*4 bytes (i32 stride); the header
			// records the element count (mirrors the new Array tests).
			lenOp, _ := e.lowerExpr(nw.Arguments.Nodes[0])
			bytes := e.freshTmp()
			e.emit("%s = mul %s, 4", bytes, lenOp)
			h := e.freshTmp()
			buf := e.freshTmp()
			e.emit("%s = alloc 16", h)
			e.emit("%s = alloc %s", buf, bytes)
			e.emit("store %s + 0, %s as ptr", h, buf)
			e.emit("store %s + 8, %s as u64", h, lenOp)
			e.emit("!%s", buf)
			e.declareOwned(h)
			return h, tArray
		}
	}
	e.refuse(n, "new expressions other than new Map() / new Array(n) are not lowerable")
	return "0", tUnknown
}

func (e *emitter) lowerArrayLiteral(n *ast.Node) (string, saType) {
	al := n.AsArrayLiteralExpression()
	elems := []string{}
	for _, el := range al.Elements.Nodes {
		v, _ := e.lowerExpr(el)
		elems = append(elems, v)
	}
	h := e.freshTmp()
	buf := e.freshTmp()
	e.emit("%s = alloc 16", h)
	e.emit("%s = alloc %d", buf, len(elems)*4)
	for i, v := range elems {
		p := e.freshTmp()
		e.emit("%s = add %s, %d", p, buf, i*4)
		e.emit("store %s + 0, %s as i32", p, v)
	}
	e.emit("store %s + 0, %s as ptr", h, buf)
	e.emit("store %s + 8, %d as u64", h, len(elems))
	e.emit("!%s", buf)
	e.declareOwned(h)
	return h, tArray
}

func (e *emitter) lowerTemplate(n *ast.Node) (string, saType) {
	// Interpolated templates join chunk-by-chunk (mirrors
	// parseInterpolatedTemplate + concatSlices + renderInterpValue).
	tp := n.AsTemplateExpression()
	e.needImport("sa_std/string.sai")
	e.needImport("sa_std/fmt.sai")
	head := tp.Head.Text()
	acc := e.lowerStringLiteral(head)
	for _, sp := range tp.TemplateSpans.Nodes {
		span := sp.AsTemplateSpan()
		v, vt := e.lowerExpr(span.Expression)
		part, ok := e.renderInterpValue(v, vt, span.Expression)
		if !ok {
			return "0", tUnknown
		}
		acc = e.concatSlices(acc, part)
		tail := span.Literal.Text()
		if tail != "" {
			tailH := e.lowerStringLiteral(tail)
			acc = e.concatSlices(acc, tailH)
			e.releaseIfOwnedTemp(tailH)
		}
	}
	return acc, tString
}

// renderInterpValue renders one interpolation operand to a string slice
// (mirrors renderInterpValue/renderFloatInterpValue): strings pass through,
// integers go through sext + @sa_fmt_i64_into, floats through
// @sa_fmt_f64_into (precision 6), into a scratch buffer borrowed by a fresh
// slice. Anything else refuses loudly.
func (e *emitter) renderInterpValue(v string, vt saType, pos *ast.Node) (string, bool) {
	if vt == tString {
		return v, true
	}
	if vt == tF64 {
		numbuf := e.freshTmp()
		e.emit("%s = alloc 64", numbuf)
		e.declareOwned(numbuf)
		numlen := e.freshTmp()
		e.emit("%s = alloc 8", numlen)
		e.declareOwned(numlen)
		rc := e.freshTmp()
		e.emit("%s = call @sa_fmt_f64_into(%s, 6, %s, 64, &%s)", rc, v, numbuf, numlen)
		e.ownTemp(rc)
		nlen := e.freshTmp()
		e.emit("%s = load %s + 0 as u64", nlen, numlen)
		vslice := e.freshTmp()
		e.emit("%s = alloc 16", vslice)
		e.emit("store %s + 0, %s as ptr", vslice, numbuf)
		e.emit("store %s + 8, %s as u64", vslice, nlen)
		e.declareOwned(vslice)
		e.releaseIfOwnedTemp(rc)
		e.releaseIfOwnedTemp(nlen)
		return vslice, true
	}
	if vt == tI32 || vt == tI64 || vt == tU64 || vt == tBool {
		wide := e.freshTmp()
		e.emit("%s = sext %s as i64", wide, v)
		numbuf := e.freshTmp()
		e.emit("%s = alloc 64", numbuf)
		e.declareOwned(numbuf)
		numlen := e.freshTmp()
		e.emit("%s = alloc 8", numlen)
		e.declareOwned(numlen)
		rc := e.freshTmp()
		e.emit("%s = call @sa_fmt_i64_into(%s, 10, %s, 64, &%s)", rc, wide, numbuf, numlen)
		e.ownTemp(rc)
		nlen := e.freshTmp()
		e.emit("%s = load %s + 0 as u64", nlen, numlen)
		vslice := e.freshTmp()
		e.emit("%s = alloc 16", vslice)
		e.emit("store %s + 0, %s as ptr", vslice, numbuf)
		e.emit("store %s + 8, %s as u64", vslice, nlen)
		e.declareOwned(vslice)
		e.releaseIfOwnedTemp(rc)
		e.releaseIfOwnedTemp(nlen)
		return vslice, true
	}
	e.refuse(pos, "cannot interpolate operand: only integers, floats and strings lower to text")
	return "0", false
}

// concatSlices joins two string slices (mirrors concatSlices):
// @sa_string_concat yields a buffer handle read back via
// @sa_fmt_buffer_data/len into a fresh slice; intermediates release.
func (e *emitter) concatSlices(left, right string) string {
	lptr, llen := e.expandSlice(left)
	rptr, rlen := e.expandSlice(right)
	obuf := e.freshTmp()
	e.emit("%s = call @sa_string_concat(%s, %s, %s, %s)", obuf, lptr, llen, rptr, rlen)
	e.ownTemp(obuf)
	optr := e.freshTmp()
	e.emit("%s = call @sa_fmt_buffer_data(%s)", optr, obuf)
	e.ownTemp(optr)
	olen := e.freshTmp()
	e.emit("%s = call @sa_fmt_buffer_len(%s)", olen, obuf)
	e.ownTemp(olen)
	out := e.freshTmp()
	e.emit("%s = alloc 16", out)
	e.emit("store %s + 0, %s as ptr", out, optr)
	e.emit("store %s + 8, %s as u64", out, olen)
	e.declareOwned(out)
	for _, t := range []string{lptr, llen, rptr, rlen, optr, olen, obuf} {
		e.releaseIfOwnedTemp(t)
	}
	// The consumed input chunks die here (single-path join, no merge).
	e.releaseIfOwnedTemp(left)
	e.releaseIfOwnedTemp(right)
	return out
}

// rawTypeName renders a type annotation to its source-level name for
// nested-layout resolution (e.g. field `inner: Inner`).
func rawTypeName(tn *ast.Node) string {
	if tn == nil {
		return "i32"
	}
	if tn.Kind == ast.KindTypeReference {
		return tn.AsTypeReferenceNode().TypeName.Text()
	}
	return saNameOfType(tn)
}

// layoutOfLiteral matches an object literal against recorded interfaces by
// field-name set.
func (e *emitter) layoutOfLiteral(n *ast.Node) *layout {
	ol := n.AsObjectLiteralExpression()
	names := []string{}
	for _, p := range ol.Properties.Nodes {
		if p.Kind != ast.KindPropertyAssignment {
			return nil
		}
		fname, ok := bindingNameText(p)
		if !ok {
			return nil
		}
		names = append(names, fname)
	}
	return e.matchLayout(names)
}

// lowerObjectLiteral materializes a struct, mirroring sa_plugin_ts:
// dest = alloc size, zero-init every field, then store each entry at its
// static offset with the field's SA type.
func (e *emitter) lowerObjectLiteral(n *ast.Node) (string, saType) {
	l := e.layoutOfLiteral(n)
	if l == nil {
		e.refuse(n, "object literal matches no recorded interface layout (declare the interface first)")
		return "0", tUnknown
	}
	ol := n.AsObjectLiteralExpression()
	h := e.freshTmp()
	e.emit("%s = alloc %d", h, l.size)
	for _, f := range l.fields {
		e.emit("store %s + %d, 0 as %s", h, l.offsets[f], l.types[f])
	}
	for _, p := range ol.Properties.Nodes {
		pa := p.AsPropertyAssignment()
		fname, _ := bindingNameText(p)
		v, _ := e.lowerExpr(pa.Initializer)
		e.emit("store %s + %d, %s as %s", h, l.offsets[fname], v, l.types[fname])
	}
	e.declareOwned(h)
	if e.varLayouts == nil {
		e.varLayouts = map[string]*layout{}
	}
	e.varLayouts[h] = l
	return h, tArray
}

// layoutOfVar resolves the struct layout for a base register (variable name
// or struct handle temp).
func (e *emitter) layoutOfVar(base string) *layout {
	if e.varLayouts == nil {
		return nil
	}
	return e.varLayouts[base]
}

// ---------------------------------------------------------------------------
// Type declarations: recorded, no code emitted (static layout)
// ---------------------------------------------------------------------------

func (e *emitter) lowerTypeDecl(st *ast.Node) {
	// Interfaces / aliases / enums: layout recorded, no code emitted.
	if st.Kind == ast.KindInterfaceDeclaration {
		e.recordLayout(st)
	}
	if st.Kind == ast.KindEnumDeclaration {
		e.recordEnum(st)
	}
}

// recordEnum records auto-numbered variants (explicit =N honored), mirroring
// sa_plugin_ts EnumDef. Enum.Member folds to its ordinal at use sites.
func (e *emitter) recordEnum(st *ast.Node) {
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	m := map[string]int64{}
	var next int64
	for _, mem := range st.AsEnumDeclaration().Members.Nodes {
		mname, ok := bindingNameText(mem)
		if !ok {
			continue
		}
		if init := mem.AsEnumMember().Initializer; init != nil &&
			init.Kind == ast.KindNumericLiteral && !isFloatLiteral(init.Text()) {
			var v int64
			fmt.Sscanf(init.Text(), "%d", &v)
			next = v
		}
		m[mname] = next
		next++
	}
	if e.enums == nil {
		e.enums = map[string]map[string]int64{}
	}
	e.enums[name] = m
}

// recordLayout builds the static byte-offset table for an interface.
func (e *emitter) recordLayout(st *ast.Node) {
	decl := st.AsInterfaceDeclaration()
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	_ = decl
	l := &layout{name: name, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}}
	off := 0
	for _, m := range st.AsInterfaceDeclaration().Members.Nodes {
		if m.Kind != ast.KindPropertySignature {
			// Method signatures declare no layout field (mirrors parseInterface).
			continue
		}
		fname, ok := bindingNameText(m)
		if !ok {
			continue
		}
		saname := saNameOfType(m.AsPropertySignatureDeclaration().Type)
		size, align := widthOf(saname)
		off = alignTo(off, align)
		l.fields = append(l.fields, fname)
		l.types[fname] = saname
		l.ftypes[fname] = rawTypeName(m.AsPropertySignatureDeclaration().Type)
		l.offsets[fname] = off
		off += size
	}
	l.size = off
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[name] = l
}

// matchLayout finds a recorded interface whose field set exactly matches the
// literal's property names (order-insensitive).
func (e *emitter) matchLayout(names []string) *layout {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, l := range e.layouts {
		if len(l.fields) != len(names) {
			continue
		}
		ok := true
		for _, f := range l.fields {
			if !set[f] {
				ok = false
				break
			}
		}
		if ok {
			return l
		}
	}
	return nil
}

func (e *emitter) lowerModuleDecl(st *ast.Node) {
	switch st.Kind {
	case ast.KindImportDeclaration:
		e.lowerImport(st)
	default:
		// export lists / export default: no codegen
	}
}

func (e *emitter) lowerImport(st *ast.Node) {
	imp := st.AsImportDeclaration()
	mod, ok := stringLiteralText(imp.ModuleSpecifier)
	if !ok {
		e.refuse(st, "non-literal module specifiers are not lowerable")
		return
	}
	if mod == "fs" || mod == "net" || mod == "path" || mod == "os" {
		// Record named imports so bare calls (readFile(...)) resolve via
		// the projection table at call sites.
		e.recordNamedImports(imp, mod)
		return
	}
	if strings.HasSuffix(mod, ".wasm") {
		e.refuse(st, ".wasm import %s needs arity-matched @extern at first call site with the real module at link time (linking needs the real module)", mod)
		return
	}
	if strings.HasSuffix(mod, ".wit") {
		e.refuse(st, ".wit import %s refused: the assembler accepts no @wit_import directive", mod)
		return
	}
	// Local .ts modules: multi-file linking is Phase 2.
	e.warn(st, "local module import %s recorded; single-file lowering continues (multi-file link is Phase 2)", mod)
}

// recordNamedImports remembers `import { a, b } from "fs"` bindings so
// bare identifier calls resolve through the std projection table.
func (e *emitter) recordNamedImports(imp *ast.ImportDeclaration, mod string) {
	if imp.ImportClause == nil {
		return
	}
	if e.importedFrom == nil {
		e.importedFrom = map[string]string{}
	}
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		if n.Kind == ast.KindImportSpecifier {
			if nm := n.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				e.importedFrom[nm.Text()] = mod
			}
			return
		}
		for ch := range n.IterChildren() {
			walk(ch)
		}
	}
	walk(imp.ImportClause)
}

// emitProjCall emits one projected std call per emitStdlibCall: string args
// expand to (`&`ptr, len) pairs, Extra supplies fixed-arity parameters
// (`&buf` materialises a 4096-byte scratch), and the fallible trio calls
// into scratch with field-0 load. Results are owned temps.
func (e *emitter) emitProjCall(proj StdProjection, args []string) (string, saType) {
	e.needImport(proj.Module)
	isStr := func(i int) bool {
		for _, s := range proj.StrArgs {
			if s == i {
				return true
			}
		}
		return false
	}
	out := []string{}
	for i, a := range args {
		if isStr(i) {
			p, l := e.expandSlice(a)
			out = append(out, "&"+p, l)
		} else {
			amp := false
			for _, s := range proj.AmpArgs {
				if s == i {
					amp = true
				}
			}
			if amp {
				out = append(out, "&"+a)
			} else {
				out = append(out, a)
			}
		}
	}
	if proj.Extra != "" {
		if strings.Contains(proj.Extra, "&buf") {
			sb := e.freshTmp()
			e.emit("%s = alloc 4096", sb)
			e.declareOwned(sb)
			rest := strings.Trim(strings.Replace(proj.Extra, "&buf", "", 1), " ,")
			out = append(out, "&"+sb)
			if rest != "" {
				out = append(out, rest)
			}
		} else {
			out = append(out, proj.Extra)
		}
	}
	if proj.Fallible {
		sc := e.freshTmp()
		e.emit("%s = call @%s(%s)", sc, proj.Symbol, strings.Join(out, ", "))
		dest := e.freshTmp()
		e.emit("%s = load %s + 0 as i64", dest, sc)
		e.ownTemp(dest)
		return dest, proj.Ret
	}
	dest := e.freshTmp()
	e.emit("%s = call @%s(%s)", dest, proj.Symbol, strings.Join(out, ", "))
	e.ownTemp(dest)
	return dest, proj.Ret
}

// projectionByTS finds a projection table entry by its TS surface name.
func projectionByTS(ts string) (StdProjection, bool) {
	for _, p := range StdProjectionTable {
		if p.TS == ts {
			return p, true
		}
	}
	return StdProjection{}, false
}

// ---------------------------------------------------------------------------
// Scope / ownership (`!` releases)
//
// Ownership model v3 (machine-verified against `sa build`, mirroring
// sa_plugin_ts scope.zig + lowerer shapes):
//   - Owned (must release/move/return): parameters, call results, alloc
//     results, and named string/array/struct handles.
//   - Non-owned (free to abandon; `!` on them is still legal): immediates,
//     arithmetic/comparison/load/snapshot pure temps, plain scalar bindings.
//   - `dst = srcReg`: src temp owned-live moves (consumed); src named always
//     snapshots (`add src, 0`, never moves, so sibling paths agree at merges
//     and later reads stay valid); src immediate assigns plain.
//   - Rebinding a live register always releases the old value first.
//   - Releases run innermost-scope-first in reverse declaration order;
//     `return v` releases everything except v; keyword is `return`.
// ---------------------------------------------------------------------------

type binding struct {
	heap     bool // owned: needs release/move/return
	consumed bool // moved-from
	released bool // `!` already emitted
}

func (e *emitter) pushScope() {
	e.scopes = append(e.scopes, map[string]*binding{})
}

func (e *emitter) popScope() {
	if len(e.scopes) > 0 {
		e.scopes = e.scopes[:len(e.scopes)-1]
	}
}

func (e *emitter) lookupBinding(name string) *binding {
	for i := len(e.scopes) - 1; i >= 0; i-- {
		if b, ok := e.scopes[i][name]; ok {
			return b
		}
	}
	return nil
}

// declareOwned records an owned binding (params, handles, call/alloc temps).
func (e *emitter) declareOwned(name string) {
	if len(e.scopes) == 0 {
		e.pushScope()
	}
	top := e.scopes[len(e.scopes)-1]
	if _, ok := top[name]; !ok {
		top[name] = &binding{heap: true}
		e.owned = append(e.owned, name)
	} else {
		top[name].heap = true
	}
}

// declarePlain records a non-owned scalar binding.
func (e *emitter) declarePlain(name string) {
	if len(e.scopes) == 0 {
		e.pushScope()
	}
	top := e.scopes[len(e.scopes)-1]
	if _, ok := top[name]; !ok {
		top[name] = &binding{heap: false}
		e.owned = append(e.owned, name)
	}
}

// setHeap adjusts a binding's owned-ness after assignment.
func (e *emitter) setHeap(name string, heap bool) {
	if b := e.lookupBinding(name); b != nil {
		b.heap = heap
	}
}

// isOwnedTemp reports whether a temp register is owned-live.
func (e *emitter) isOwnedTemp(t string) bool {
	b := e.lookupBinding(t)
	return b != nil && b.heap && !b.consumed && !b.released
}

// consume marks an owned source as moved-from.
func (e *emitter) consume(name string) {
	if b := e.lookupBinding(name); b != nil {
		if b.heap && !b.released {
			b.consumed = true
		}
	}
}

// rebindRelease kills a live NAMED register before rebinding (always legal,
// mirrors the `!r` / `!best` shapes). Compiler temps (t_N) are loop-carried
// SSA values rebound plainly (machine-verified legal, matches the pow loop).
func (e *emitter) rebindRelease(dst string) {
	if isTempName(dst) {
		return
	}
	if b := e.lookupBinding(dst); b != nil && !b.consumed && !b.released {
		e.emit("!%s", dst)
		b.released = true
	}
}

// assign lowers `dst = src` with move/snapshot discipline.
// srcKind is "imm" (immediate), "temp" (fresh SSA temp) or "named".
// srcType guides the snapshot op for named sources (f64 uses fadd).
// pos supplies diagnostic context for handle-copy refusals.
func (e *emitter) assign(dst, src, srcKind string, srcType saType, pos *ast.Node) {
	fresh := e.lookupBinding(dst) == nil
	if srcKind == "named" {
		if _, ok := e.handleNamed(src); ok {
			e.refuse(pos, "handle copies need an explicit clone (pass the handle directly)")
			return
		}
		op := "add"
		zero := "0"
		if srcType == tF64 {
			op = "fadd"
			zero = "0.0"
		}
		if fresh {
			// Fresh destination: direct snapshot (logIt shape).
			e.emit("%s = %s %s, %s", dst, op, src, zero)
			if !isTempName(dst) {
				e.declarePlain(dst)
			}
			return
		}
		// Live destination: double-temp snapshot + rebind (t5 shape).
		c1 := e.freshTmp()
		c2 := e.freshTmp()
		e.emit("%s = %s %s, %s", c1, op, src, zero)
		e.emit("%s = %s %s, %s", c2, op, c1, zero)
		e.rebindRelease(dst)
		e.emit("%s = %s", dst, c2)
		e.setHeap(dst, false)
		return
	}
	if !fresh {
		e.rebindRelease(dst)
	}
	e.emit("%s = %s", dst, src)
	if srcKind == "temp" {
		// Binding from a temp register takes ownership, even when the
		// temp itself is pure (009 shape: `c = t_1` must release `!c`).
		if e.isOwnedTemp(src) {
			e.consume(src)
		}
		if !fresh {
			e.setHeap(dst, true)
		} else if !isTempName(dst) {
			e.declareOwned(dst)
		}
		return
	}
	if fresh && !isTempName(dst) {
		e.declarePlain(dst)
	} else if !fresh {
		e.setHeap(dst, false)
	}
}

// isTempName reports compiler temps (t_N). User bindings with such names
// are treated as pure temps (documented collision caveat).
func isTempName(s string) bool {
	if len(s) < 3 || s[0] != 't' || s[1] != '_' {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// operandKind classifies a lowered operand for assign().
func operandKind(op string, n *ast.Node) string {
	if isTempName(op) {
		return "temp"
	}
	if isIntImm(op) {
		return "imm"
	}
	switch n.Kind {
	case ast.KindNumericLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword:
		return "imm"
	}
	return "named"
}

func isIntImm(op string) bool {
	if op == "" {
		return false
	}
	i := 0
	if op[0] == '-' {
		i = 1
	}
	if i >= len(op) {
		return false
	}
	for ; i < len(op); i++ {
		if op[i] < '0' || op[i] > '9' {
			return false
		}
	}
	return true
}

// ownTemp marks a call/alloc result temp as owned.
func (e *emitter) ownTemp(t string) {
	e.declareOwned(t)
}

// releaseIfOwnedTemp releases an owned temp (never named bindings, which
// may still be live later).
func (e *emitter) releaseIfOwnedTemp(t string) {
	if isTempName(t) {
		if b := e.lookupBinding(t); b != nil && b.heap && !b.consumed && !b.released {
			e.emit("!%s", t)
			b.released = true
		}
	}
}

// handleNamed reports whether a named source holds a heap handle.
func (e *emitter) handleNamed(src string) (string, bool) {
	if e.strVars[src] || e.arrVars[src] {
		return "handle", true
	}
	if l := e.layoutOfVar(src); l != nil {
		return "handle", true
	}
	return "", false
}

func (e *emitter) releaseScope() {
	if len(e.scopes) == 0 {
		return
	}
	top := e.scopes[len(e.scopes)-1]
	done := map[string]bool{}
	for i := len(e.owned) - 1; i >= 0; i-- {
		name := e.owned[i]
		if top[name] == nil || done[name] {
			continue
		}
		done[name] = true
		if b := top[name]; b.heap && !b.consumed && !b.released {
			e.emit("!%s", name)
			b.released = true
		}
	}
	kept := e.owned[:0]
	for _, name := range e.owned {
		if top[name] == nil {
			kept = append(kept, name)
		}
	}
	e.owned = kept
}

func (e *emitter) releaseAllOwnedExcept(except string) {
	done := map[string]bool{}
	for i := len(e.owned) - 1; i >= 0; i-- {
		name := e.owned[i]
		if name == except || done[name] {
			continue
		}
		done[name] = true
		if b := e.lookupBinding(name); b != nil && b.heap && !b.consumed && !b.released {
			e.emit("!%s", name)
			b.released = true
		}
	}
	e.owned = nil
	for _, s := range e.scopes {
		for k := range s {
			delete(s, k)
		}
	}
}

func (e *emitter) releaseAllOwned() {
	e.releaseAllOwnedExcept("")
}

// releaseForJump releases scopes deeper than the jump target's depth.
// callDepth is the scope depth at the loop/switch that owns the target:
// only abandoned arm/block scopes release; enclosing scopes stay live so
// merge states agree.
func (e *emitter) releaseForJump(callDepth int) {
	done := map[string]bool{}
	for d := len(e.scopes) - 1; d > callDepth; d-- {
		top := e.scopes[d]
		for i := len(e.owned) - 1; i >= 0; i-- {
			name := e.owned[i]
			if top[name] == nil || done[name] {
				continue
			}
			done[name] = true
			if b := top[name]; b.heap && !b.consumed && !b.released {
				e.emit("!%s", name)
				b.released = true
			}
		}
	}
}

// releaseTemp releases a pure-value SSA temp.
//
// Phase 1 policy: pure-value temps (arithmetic/comparison results) are
// non-owned and need no release; heap handles (strings, arrays, params)
// travel through the scope machinery. Kept for Phase 2 heap-temp work.
func (e *emitter) releaseTemp(t string) {
	if strings.HasPrefix(t, "t_") {
		e.emit("!%s", t)
	}
}

// ---------------------------------------------------------------------------
// Small AST shape helpers
// ---------------------------------------------------------------------------

func foBindingName(fo *ast.ForInOrOfStatement) string {
	init := fo.Initializer
	if init != nil && init.Kind == ast.KindVariableDeclarationList {
		if decls := init.AsVariableDeclarationList().Declarations.Nodes; len(decls) > 0 {
			if name, ok := bindingNameText(decls[0]); ok {
				return name
			}
		}
	}
	return "forof_elem"
}

// bindingNameText extracts a plain identifier binding name; patterns refuse.
func bindingNameText(n *ast.Node) (string, bool) {
	name := n.Name()
	if name == nil || name.Kind != ast.KindIdentifier {
		return "", false
	}
	return name.Text(), true
}

// stringLiteralText extracts a string literal's value without panicking.
func stringLiteralText(n *ast.Node) (string, bool) {
	if n == nil || n.Kind != ast.KindStringLiteral {
		return "", false
	}
	return n.Text(), true
}
