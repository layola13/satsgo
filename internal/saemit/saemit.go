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
	tcx := newTypeCtx(map[string]string{fileName: sourceText})
	if tcx != nil {
		defer tcx.close()
		// Lower the checker's own SourceFile so node identity matches
		// GetTypeAtLocation (positions are identical: same text).
		if psf := tcx.prog.GetSourceFile(abs); psf != nil {
			sf = psf
		}
	}
	e := &emitter{file: fileName, src: sourceText, lines: lineOffsets(sourceText), tcx: tcx}
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
	// pendingFuncs buffers out-of-line arrow-function bodies (mirrors the
	// callbacks buffer in sa_plugin_ts parseTopLevelArrowFn/parseArrowBody):
	// nested emission splices them into file scope at finish().
	pendingFuncs []string
	// arrowAliases maps a local/top-level value name bound to an arrow
	// function to its generated callee (direct calls rewrite to it).
	arrowAliases map[string]*arrowInfo
	arrowSeq     int
	// funcParams records top-level callee arity; funcHasRest marks a
	// trailing ...rest parameter (spread calls pack or expand per arity).
	funcParams  map[string]int
	funcHasRest map[string]bool
	// funcDefaults records per-parameter defaults (missing args are only
	// tolerable when every omitted parameter declares one).
	funcDefaults map[string][]bool
	// inlineRet intercepts callback returns (higher-order inlining).
	inlineRet *inlineRetState
	// classDefs records class shapes; varClass maps instance variables to
	// their class; instFnFields maps instance -> field -> inline arrow for
	// function-typed fields; thisSelf is the current method receiver.
	classDefs    map[string]*classDef
	varClass     map[string]string
	instFnFields map[string]map[string]*ast.Node
	thisSelf     string
	// Program linking (multi-file): prefix namespaces @-definitions of
	// non-entry files; importEnv maps an imported local name to its
	// qualified callee; nsImports maps `import * as ns` to the file key.
	// Nil link preserves exact single-file behavior.
	prefix        string
	link          *fileLink
	importEnv     map[string]string
	importRet     map[string]saType
	nsImports     map[string]string
	localDefs     map[string]bool
	importedNames map[string]bool
	// linkExports maps every linked top-level function name to its file
	// (for the "import it first" diagnostic).
	linkExports map[string]string
	// tcx is the shared binder/checker context (nil-safe: syntax-only
	// fallback preserves exact legacy behavior).
	tcx *typeCtx
	// mapVars/setVars mark Map/Set handles for backend dispatch.
	mapVars map[string]bool
	setVars map[string]bool
	// f64Vars marks float-valued bindings (sqrt and friends refuse them).
	f64Vars map[string]bool
	// dtsRet overrides top-level unannotated bodies with co-located .d.ts
	// return types (program mode; empty in single-file lowering).
	dtsRet map[string]saType
	// constVals folds top-level pure literals (name -> literal text plus a
	// string flag); mathAliases maps top-level `var f = Math.g` to g.
	// Reassignment drops the entry (then normal declaration applies).
	constVals   map[string]string
	constIsStr  map[string]bool
	mathAliases map[string]string
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

// fnDef renders a definition label with the file prefix (entry files use
// no prefix, so `@main` is preserved; prefix "" is exactly single-file).
func (e *emitter) fnDef(name string) string {
	return e.prefix + name
}

// fnRef renders a same-file call target with the file prefix.
func (e *emitter) fnRef(name string) string {
	return e.prefix + name
}

func (e *emitter) finish() string {
	// Out-of-line arrow bodies splice into file scope after the callers
	// (SA-ASM resolves @labels file-wide, so order is irrelevant).
	for _, fn := range e.pendingFuncs {
		e.body.WriteString(fn)
	}
	e.pendingFuncs = nil
	return e.header.String() + e.body.String()
}

// arrowInfo is one out-of-line arrow callee: the generated @name plus the
// captured outer variables appended as extra trailing arguments.
type arrowInfo struct {
	fn       string
	ret      saType
	captures []string
	params   []string
}

// inlineRetState intercepts return inside an inlined higher-order callback:
// the value lands in the method join slot and control jumps to the end.
type inlineRetState struct {
	active bool
	slot   string
	end    string
	saname string
}

// ---------------------------------------------------------------------------
// Top level
// ---------------------------------------------------------------------------

func (e *emitter) lowerSourceFile(sf *ast.SourceFile) {
	stmts := sf.AsSourceFile().Statements.Nodes
	// Pre-scan: register function signatures (forward calls resolve; void
	// callees known before first use), interfaces and enums. Program links
	// pre-seed cross-file signatures, so only create when absent.
	if e.funcSigs == nil {
		e.funcSigs = map[string]saType{}
	}
	if e.funcParams == nil {
		e.funcParams = map[string]int{}
	}
	if e.funcHasRest == nil {
		e.funcHasRest = map[string]bool{}
	}
	if e.funcDefaults == nil {
		e.funcDefaults = map[string][]bool{}
	}
	for _, st := range stmts {
		if st.Kind == ast.KindFunctionDeclaration && st.Name() != nil &&
			st.Name().Kind == ast.KindIdentifier {
			ret := tVoid
			if fd := st.AsFunctionDeclaration(); fd.Type != nil {
				ret = annotationType(fd.Type)
				if ret == tUnknown {
					ret = tI32
				}
			} else if r, ok := e.dtsRet[st.Name().Text()]; ok {
				ret = r
			}
			e.funcSigs[st.Name().Text()] = ret
			params := st.Parameters()
			e.funcParams[st.Name().Text()] = len(params)
			defs := make([]bool, len(params))
			for i, p := range params {
				if pd := p.AsParameterDeclaration(); pd.Initializer != nil {
					defs[i] = true
				}
			}
			if e.funcDefaults == nil {
				e.funcDefaults = map[string][]bool{}
			}
			e.funcDefaults[st.Name().Text()] = defs
			if len(params) > 0 {
				if pd := params[len(params)-1].AsParameterDeclaration(); pd.DotDotDotToken != nil {
					e.funcHasRest[st.Name().Text()] = true
				}
			}
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
	case ast.KindClassDeclaration:
		// Classes record layouts + bodies for call-site inlining
		// (mirrors sa_plugin_ts parseClass without vtables).
		if !topLevel {
			e.refuse(st, "nested class declarations are not lowerable")
			return
		}
		e.recordClass(st)
		return
	case ast.KindFunctionDeclaration:
		e.lowerFunction(st)
	case ast.KindVariableStatement:
		if topLevel {
			// A top-level `const f = (...) => ...` is a file-scope callback
			// (mirrors sa_plugin_ts parseTopLevelArrowFn): emit @f directly.
			if e.tryTopLevelArrow(st) {
				return
			}
			// Top-level pure bindings (`var K = 1.5`, `var nativeMax =
			// Math.max`) fold without registers (see tryTopLevelConst).
			if e.tryTopLevelConst(st) {
				return
			}
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
	case ast.KindForInStatement:
		e.lowerForIn(st)
	case ast.KindDoStatement:
		e.lowerDoWhile(st)
	case ast.KindTryStatement:
		e.lowerTry(st)
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
	// Top-level unannotated bodies take co-located .d.ts returns.
	e.retType = tVoid
	if fd := fn.AsFunctionDeclaration(); fd.Type != nil {
		e.retType = annotationType(fd.Type)
		if e.retType == tUnknown {
			e.retType = tI32
		}
	} else if !savedInFunc {
		if r, ok := e.dtsRet[name]; ok {
			e.retType = r
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
		// ...rest collects the packed variadic slice (callers pack).
		if pd := p.AsParameterDeclaration(); pd.DotDotDotToken != nil {
			ptype = tArray
			if e.arrVars == nil {
				e.arrVars = map[string]bool{}
			}
			if e.arrElems == nil {
				e.arrElems = map[string]string{}
			}
			e.arrVars[pname] = true
			e.arrElems[pname] = "i32"
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
	e.emitRaw("@%s(%s)%s:", e.fnDef(name), strings.Join(sig, ", "), ret)
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
// Arrow functions → out-of-line callees (mirrors sa_plugin_ts
// parseTopLevelArrowFn + the callbacks buffer: the name becomes a call
// alias, captures append as trailing parameters, bodies splice into file
// scope). Direct calls lower exactly; first-class passing still refuses.
// ---------------------------------------------------------------------------

// tryTopLevelArrow emits `const f = (...) => ...` at file scope as @f.
// Reports whether the statement was consumed.
func (e *emitter) tryTopLevelArrow(st *ast.Node) bool {
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		return false
	}
	d := dl.Declarations.Nodes[0]
	init := d.Initializer()
	if init == nil || init.Kind != ast.KindArrowFunction {
		return false
	}
	name, ok := bindingNameText(d)
	if !ok {
		return false
	}
	e.lowerArrowBinding(name, init, true)
	return !e.refused
}

// lowerArrowBinding builds @gen(params..., captures...) for one arrow and
// records the name as a call alias. Top-level arrows keep the source name
// (reference behavior); locals get @__arrow_N.
func (e *emitter) lowerArrowBinding(name string, arrow *ast.Node, topLevel bool) {
	af := arrow.AsArrowFunction()
	params := arrow.Parameters()
	pnames := make([]string, 0, len(params))
	psig := make([]string, 0, len(params))
	ptypes := make([]saType, 0, len(params))
	for _, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			e.refuse(p.AsNode(), "destructured parameters are not in the SA-lowerable subset")
			return
		}
		pt := tI32
		if pd := p.AsParameterDeclaration(); pd.Type != nil {
			pt = annotationType(pd.Type)
			if pt == tUnknown {
				pt = tI32
			}
		}
		pnames = append(pnames, pname)
		ptypes = append(ptypes, pt)
		psig = append(psig, fmt.Sprintf("%s: %s", pname, pt))
	}
	// Return type: explicit annotation wins; otherwise an expression body
	// or any parameter means a value function (reference value_fn rule).
	ret := tVoid
	if af.Type != nil {
		ret = annotationType(af.Type)
		if ret == tUnknown {
			ret = tI32
		}
	} else {
		body := arrow.Body()
		if body == nil {
			e.refuse(arrow, "arrow function %s has no body", name)
			return
		}
		if body.Kind != ast.KindBlock || len(pnames) > 0 {
			ret = tI32
		}
	}
	// Captures: free identifiers minus params, minus locals declared in
	// the body, minus globals and callee names (never values).
	bodyNode := arrow.Body()
	uses := map[string]bool{}
	collectValueIdents(bodyNode, uses)
	decls := map[string]bool{name: true}
	for _, p := range pnames {
		decls[p] = true
	}
	collectDeclaredNames(bodyNode, decls)
	for fn := range e.funcSigs {
		decls[fn] = true
	}
	for _, g := range []string{"console", "Math", "String", "Number", "alloc", "structuredClone", "Array", "Map", "Set", "undefined", "null", "true", "false"} {
		decls[g] = true
	}
	captures := []string{}
	for id := range uses {
		if !decls[id] && e.lookupBinding(id) != nil {
			captures = append(captures, id)
		}
	}
	sortStrings(captures)
	gen := e.fnDef(name)
	if !topLevel {
		e.arrowSeq++
		// Locals carry the file prefix so linked files never collide.
		gen = fmt.Sprintf("%s__arrow_%d", e.prefix, e.arrowSeq)
	}
	if e.funcSigs == nil {
		e.funcSigs = map[string]saType{}
	}
	e.funcSigs[gen] = ret
	if e.arrowAliases == nil {
		e.arrowAliases = map[string]*arrowInfo{}
	}
	e.arrowAliases[name] = &arrowInfo{fn: gen, ret: ret, captures: captures, params: pnames}
	// Out-of-line emission with swapped builders and saved CFG state.
	savedBody := e.body
	savedOwned := e.owned
	savedScopes := e.scopes
	savedBreaks := e.breaks
	savedConts := e.conts
	savedRet := e.retType
	savedInFunc := e.inFunc
	savedTerm := e.terminated
	savedInline := e.inlineRet
	e.body = strings.Builder{}
	e.owned = nil
	e.scopes = nil
	e.breaks = nil
	e.conts = nil
	e.retType = ret
	e.inFunc = true
	e.terminated = false
	e.inlineRet = nil
	full := append(append([]string{}, psig...), captureSig(e, captures)...)
	retAnn := ""
	if ret != tVoid {
		retAnn = fmt.Sprintf(" -> %s", ret)
	}
	e.emitRaw("@%s(%s)%s:", gen, strings.Join(full, ", "), retAnn)
	e.pushScope()
	for i, p := range pnames {
		// Mirror lowerFunction: record layouts and slice kinds so field
		// access and method dispatch resolve inside the body.
		var annot *ast.Node
		if pd := params[i].AsParameterDeclaration(); pd.Type != nil {
			annot = pd.Type
		}
		e.trackBinding(p, annot, nil, ptypes[i])
		e.declareOwned(p)
	}
	// Captures arrive as same-named trailing params: re-declare them so
	// the body resolves. Slice kinds stay visible via the shared
	// strVars/arrVars maps (outer entries are still present).
	for _, c := range captures {
		e.declareOwned(c)
	}
	e.pushScope()
	bd := arrow.Body()
	if bd.Kind == ast.KindBlock {
		for _, s := range bd.Statements() {
			e.lowerBlockStatement(s)
		}
		if !e.terminated {
			e.releaseAllOwned()
			if ret == tVoid {
				e.emit("return")
			} else {
				e.emit("return 0")
			}
		}
	} else {
		v, _ := e.lowerExpr(bd)
		e.releaseAllOwnedExcept(v)
		e.emit("return %s", v)
	}
	e.terminated = false
	e.popScope()
	e.popScope()
	fnText := e.body.String()
	e.body = savedBody
	e.owned = savedOwned
	e.scopes = savedScopes
	e.breaks = savedBreaks
	e.conts = savedConts
	e.retType = savedRet
	e.inFunc = savedInFunc
	e.terminated = savedTerm
	e.inlineRet = savedInline
	e.pendingFuncs = append(e.pendingFuncs, fnText)
	// The alias name itself is a plain (non-owned) local binding.
	if !topLevel {
		e.declarePlain(name)
	}
}

// checkArity enforces call arity loudly: rest callees take any count;
// fixed callees take exactly arity, except trailing parameters with
// defaults may be omitted (their replay is a known gap: omitted values
// arrive as caller-passed zeros only when the caller pads; short calls
// otherwise refuse rather than miscompile).
func (e *emitter) checkArity(fname string, args []string, pos *ast.Node) bool {
	if e.funcHasRest[fname] {
		return true
	}
	arity, ok := e.funcParams[fname]
	if !ok {
		return true
	}
	if len(args) == arity {
		return true
	}
	if len(args) > arity {
		e.refuse(pos, "too many arguments in call to %s (%d given, %d expected)", fname, len(args), arity)
		return false
	}
	defs := e.funcDefaults[fname]
	for i := len(args); i < arity; i++ {
		if i >= len(defs) || !defs[i] {
			e.refuse(pos, "too few arguments in call to %s (%d given, %d expected)", fname, len(args), arity)
			return false
		}
	}
	return true
}

// captureSig renders captured outer names as trailing i32 params. Slice
// captures keep ptr width (both render as handle registers at call sites).
func captureSig(e *emitter, captures []string) []string {
	sig := make([]string, 0, len(captures))
	for _, c := range captures {
		t := "i32"
		if e.strVars[c] || e.arrVars[c] {
			t = "ptr"
		}
		sig = append(sig, fmt.Sprintf("%s: %s", c, t))
	}
	return sig
}

// collectValueIdents gathers identifier uses, skipping property names
// (`a.b` contributes `a`, not `b`).
func collectValueIdents(n *ast.Node, out map[string]bool) {
	if n == nil {
		return
	}
	if n.Kind == ast.KindPropertyAccessExpression {
		pa := n.AsPropertyAccessExpression()
		collectValueIdents(pa.Expression, out)
		return
	}
	if n.Kind == ast.KindIdentifier {
		out[n.Text()] = true
		return
	}
	n.ForEachChild(func(c *ast.Node) bool {
		collectValueIdents(c, out)
		return false
	})
}

// collectDeclaredNames gathers locally-declared names (vars, functions,
// classes, params of nested arrows) so they are not mistaken for captures.
func collectDeclaredNames(n *ast.Node, out map[string]bool) {
	if n == nil {
		return
	}
	switch n.Kind {
	case ast.KindVariableDeclaration:
		if name, ok := bindingNameText(n); ok {
			out[name] = true
		}
	case ast.KindFunctionDeclaration, ast.KindClassDeclaration, ast.KindInterfaceDeclaration, ast.KindEnumDeclaration:
		if n.Name() != nil && n.Name().Kind == ast.KindIdentifier {
			out[n.Name().Text()] = true
		}
	}
	n.ForEachChild(func(c *ast.Node) bool {
		collectDeclaredNames(c, out)
		return false
	})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
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
		init := d.Initializer()
		if init == nil {
			e.refuse(d, "declaration without initializer is not lowerable")
			continue
		}
		// Destructuring declarations bind element/field-wise (mirrors
		// sa_plugin_ts destructuring; holes skip, rest refuses loudly).
		if nm := d.Name(); nm != nil && nm.Kind != ast.KindIdentifier {
			e.lowerDestructuringDecl(d, nm, init)
			continue
		}
		name, ok := bindingNameText(d)
		if !ok {
			e.refuse(d, "destructuring declarations are not in the SA-lowerable subset")
			continue
		}
		atype := annotationType(d.AsVariableDeclaration().Type)
		// `let f = (x) => ...` desugars to an out-of-line callee; the
		// name becomes a call alias, not a register binding.
		if init.Kind == ast.KindArrowFunction {
			e.lowerArrowBinding(name, init, false)
			continue
		}
		val, vtype := e.lowerExpr(init)
		if atype == tUnknown {
			atype = vtype
		}
		_ = atype
		e.assign(name, val, operandKind(val, init), vtype, d)
		e.trackBinding(name, d.AsVariableDeclaration().Type, init, vtype)
		// `const b = new Box(...)` records the instance class for method
		// dispatch and per-instance fn-field devirtualization.
		if init.Kind == ast.KindNewExpression {
			if nw := init.AsNewExpression(); nw.Expression.Kind == ast.KindIdentifier {
				if _, ok := e.classDefs[nw.Expression.Text()]; ok {
					if e.varClass == nil {
						e.varClass = map[string]string{}
					}
					e.varClass[name] = nw.Expression.Text()
					// Retarget per-instance fn fields from the result temp
					// (lowerNewClass records under it) to the bound name.
					if fields, ok := e.instFnFields[val]; ok {
						if e.instFnFields == nil {
							e.instFnFields = map[string]map[string]*ast.Node{}
						}
						e.instFnFields[name] = fields
					}
				}
			}
		}
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
	// Union constituents contribute the first known struct layout
	// (`Box | null` still reads .v through the Box layout under a guard).
	if annot != nil && (annot.Kind == ast.KindUnionType || annot.Kind == ast.KindIntersectionType) {
		for _, m := range annot.AsUnionTypeNode().Types.Nodes {
			if m.Kind == ast.KindTypeReference {
				if l, ok := e.layouts[m.AsTypeReferenceNode().TypeName.Text()]; ok {
					e.varLayouts[name] = l
					break
				}
			}
		}
	}
	// object literals self-report their layout via matchLayout.
	if init != nil && init.Kind == ast.KindObjectLiteralExpression {
		if l := e.layoutOfLiteral(init); l != nil {
			e.varLayouts[name] = l
		}
	}
	// new Map()/new Set() handles remember their kind for method dispatch.
	if init != nil && init.Kind == ast.KindNewExpression {
		if nw := init.AsNewExpression(); nw.Expression.Kind == ast.KindIdentifier {
			switch nw.Expression.Text() {
			case "Map":
				if e.mapVars == nil {
					e.mapVars = map[string]bool{}
				}
				e.mapVars[name] = true
			case "Set":
				if e.setVars == nil {
					e.setVars = map[string]bool{}
				}
				e.setVars[name] = true
			}
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
	// float bindings: f64-valued initializer (literal or copied).
	if vtype == tF64 || (init != nil && init.Kind == ast.KindIdentifier && e.f64Vars[init.Text()]) {
		if e.f64Vars == nil {
			e.f64Vars = map[string]bool{}
		}
		e.f64Vars[name] = true
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
	// Pattern bindings (`for (const [a, b] of pairs)`) destructure the
	// element slice; object patterns need element layouts (loud gap).
	if pat := foBindingPattern(fo); pat != nil {
		if pat.Kind == ast.KindArrayBindingPattern {
			if e.arrVars == nil {
				e.arrVars = map[string]bool{}
			}
			if e.arrElems == nil {
				e.arrElems = map[string]string{}
			}
			e.arrVars[elemT] = true
			e.arrElems[elemT] = "i32"
			e.destructureArray(pat, elemT, st)
		} else {
			e.refuse(st, "object patterns in for-of need static element layouts")
		}
	} else {
		binding := foBindingName(fo)
		e.assign(binding, elemT, "temp", tI32, st)
	}
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

// lowerForIn desugars `for (const k in arr)` to an index loop over the
// 16-byte slice (mirrors sa_plugin_ts for-in: arrays/strings by index,
// objects refuse).
func (e *emitter) lowerForIn(st *ast.Node) {
	fo := st.AsForInOrOfStatement()
	arrVal, _ := e.lowerExpr(fo.Expression)
	idx := e.freshTmp()
	e.emit("%s = 0", idx)
	topL := e.freshLabel("forin_top")
	bodyL := e.freshLabel("forin_body")
	endL := e.freshLabel("forin_end")
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
	binding := foBindingName(fo)
	e.assign(binding, idx, "named", tI32, st)
	e.declarePlain(binding)
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

// lowerDoWhile mirrors sa_plugin_ts parseDoWhile: the body runs once, then
// the condition branches back to the top. continue lands on the condition.
func (e *emitter) lowerDoWhile(st *ast.Node) {
	ds := st.AsDoStatement()
	loopL := e.freshLabel("dowhile")
	condL := e.freshLabel("dowhile_cond")
	endL := e.freshLabel("enddowhile")
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.conts = append(e.conts, jumpTarget{condL, len(e.scopes)})
	e.emitRaw("%s:", loopL)
	e.pushScope()
	e.terminated = false
	e.lowerBranchBody(ds.Statement)
	e.releaseScope()
	e.popScope()
	if !e.terminated {
		e.emit("jmp %s", condL)
	}
	e.emitRaw("%s:", condL)
	cond, _ := e.lowerExpr(ds.Expression)
	if cond == "1" || cond == "true" {
		e.emit("jmp %s", loopL)
	} else if cond == "0" || cond == "false" {
		// Falls through to the end.
	} else {
		e.emit("br %s -> %s, %s", cond, loopL, endL)
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.breaks = e.breaks[:len(e.breaks)-1]
	e.conts = e.conts[:len(e.conts)-1]
}

// lowerTry mirrors sa_plugin_ts parseTryCatch: SA-ASM has no exception
// edges and throw lowers to panic (unresumable), so a try whose body can
// throw refuses loudly; otherwise the body runs and the catch handler is
// skipped (dead code), while a finally block always runs.
func (e *emitter) lowerTry(st *ast.Node) {
	ts := st.AsTryStatement()
	if containsThrow(ts.TryBlock) {
		e.refuse(st, "throw inside try is not lowerable (catch cannot resume after panic)")
		return
	}
	endL := e.freshLabel("endtry")
	e.pushScope()
	e.terminated = false
	for _, s := range ts.TryBlock.Statements() {
		e.lowerBlockStatement(s)
	}
	e.releaseScope()
	e.popScope()
	if !e.terminated {
		e.emit("jmp %s", endL)
	}
	// Catch handler is dead code: skipped without emission.
	if ts.FinallyBlock != nil {
		e.pushScope()
		e.terminated = false
		for _, s := range ts.FinallyBlock.Statements() {
			e.lowerBlockStatement(s)
		}
		e.releaseScope()
		e.popScope()
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
}

// containsThrow reports whether a block can throw (then catch is unreachably
// dead and the try must refuse rather than miscompile).
func containsThrow(n *ast.Node) bool {
	found := false
	var walk func(x *ast.Node)
	walk = func(x *ast.Node) {
		if x == nil || found {
			return
		}
		if x.Kind == ast.KindThrowStatement {
			found = true
			return
		}
		x.ForEachChild(func(c *ast.Node) bool {
			walk(c)
			return false
		})
	}
	walk(n)
	return found
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
	// Inside an inlined higher-order callback, return delivers the
	// callback value into the join slot and jumps to the method end.
	if e.inlineRet != nil && e.inlineRet.active {
		if rs.Expression != nil {
			v, _ := e.lowerExpr(rs.Expression)
			e.emit("store %s + 0, %s as %s", e.inlineRet.slot, v, e.inlineRet.saname)
		}
		e.emit("jmp %s", e.inlineRet.end)
		e.terminated = true
		return
	}
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
	case ast.KindNullKeyword, ast.KindUndefinedKeyword:
		// The subset maps null/undefined to 0 (checked-index OOB,
		// `== null` handles, missing values).
		return "0", tI32
	case ast.KindVoidExpression:
		// `void expr` evaluates to undefined → 0 (side effects keep
		// lowering through the operand).
		_, _ = e.lowerExpr(n.AsVoidExpression().Expression)
		return "0", tI32
	case ast.KindTypeOfExpression:
		return e.lowerTypeof(n)
	case ast.KindDeleteExpression:
		// Static layouts cannot drop fields; Map/Set use .delete().
		e.refuse(n, "delete operator is not lowerable (static layouts cannot drop fields; Maps/Sets use .delete())")
		return "0", tUnknown
	case ast.KindIdentifier:
		// Handle aliases resolve to the underlying register (inlined
		// method params never copy handles).
		if b := e.lookupBinding(n.Text()); b != nil {
			if b.alias != "" {
				target := b.alias
				for i := 0; i < 8; i++ {
					nb := e.lookupBinding(target)
					if nb == nil || nb.alias == "" {
						break
					}
					target = nb.alias
				}
				return target, tArray
			}
			// Bindings report their static kind (strings/arrays/floats
			// must not masquerade as i32: interpolation, map keys and
			// join typing depend on it).
			if e.strVars[n.Text()] {
				return n.Text(), tString
			}
			if e.arrVars[n.Text()] || e.mapVars[n.Text()] || e.setVars[n.Text()] {
				return n.Text(), tArray
			}
			if e.f64Vars[n.Text()] {
				return n.Text(), tF64
			}
			return n.Text(), tI32
		}
		// Top-level pure consts inline (locals shadow via scopes above).
		if lit, ok := e.constVals[n.Text()]; ok {
			if e.constIsStr[n.Text()] {
				return e.lowerStringLiteral(lit), tString
			}
			if isFloatLiteral(lit) {
				return lit, tF64
			}
			return lit, tI32
		}
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
	case ast.KindThisKeyword:
		// Method receiver alias (set while inlining class methods).
		if e.thisSelf == "" {
			e.refuse(n, "`this` outside a class method is not lowerable")
			return "0", tUnknown
		}
		return e.thisSelf, tArray
	case ast.KindAwaitExpression:
		// Await unwraps synchronously: the subset has no concurrent
		// runtime (async lowers to direct calls; sa_std async.sla
		// drivers are Phase 2), so await v ≡ v when v is a value.
		return e.lowerExpr(n.AsAwaitExpression().Expression)
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

// tryTopLevelConst folds top-level pure declarators: numeric/string
// literals become inline consts, `var f = Math.g` becomes a math alias.
// Reports whether every declarator folded (partial folds stay bound; the
// caller refuses when false).
func (e *emitter) tryTopLevelConst(st *ast.Node) bool {
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) == 0 {
		return false
	}
	allOk := true
	for _, d := range dl.Declarations.Nodes {
		name, ok := bindingNameText(d)
		if !ok {
			allOk = false
			continue
		}
		init := d.Initializer()
		if init == nil {
			allOk = false
			continue
		}
		switch init.Kind {
		case ast.KindNumericLiteral, ast.KindStringLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword:
			if e.constVals == nil {
				e.constVals = map[string]string{}
			}
			if e.constIsStr == nil {
				e.constIsStr = map[string]bool{}
			}
			if init.Kind == ast.KindStringLiteral {
				s, ok := stringLiteralText(init)
				if !ok {
					allOk = false
					continue
				}
				e.constVals[name] = s
				e.constIsStr[name] = true
				continue
			}
			if init.Kind == ast.KindTrueKeyword {
				e.constVals[name] = "1"
				continue
			}
			if init.Kind == ast.KindFalseKeyword {
				e.constVals[name] = "0"
				continue
			}
			e.constVals[name] = init.Text()
		case ast.KindPropertyAccessExpression:
			pa := init.AsPropertyAccessExpression()
			if pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "Math" {
				if _, ok := projectionByTS("Math." + pa.Name().Text()); ok {
					if e.mathAliases == nil {
						e.mathAliases = map[string]string{}
					}
					e.mathAliases[name] = pa.Name().Text()
					continue
				}
			}
			allOk = false
		default:
			allOk = false
		}
	}
	return allOk
}

// lowerStringLiteral materializes a real slice: @const utf8 + 16-byte header.
func (e *emitter) lowerStringLiteral(text string) string {
	e.tmp++
	cname := fmt.Sprintf("str_const_%d", e.tmp)
	escaped := strings.ReplaceAll(text, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	escaped = strings.ReplaceAll(escaped, "\n", "\\n")
	escaped = strings.ReplaceAll(escaped, "\r", "\\r")
	escaped = strings.ReplaceAll(escaped, "\t", "\\t")
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
	// `in` folds statically: layouts are fixed, so field presence is a
	// compile-time 1/0 (unknown bases refuse loudly). The verdict
	// materialises into a temp (br takes registers, not immediates).
	if op == ast.KindInKeyword {
		verdict := ""
		if bin.Left.Kind == ast.KindStringLiteral {
			if s, ok := stringLiteralText(bin.Left); ok {
				if bin.Right.Kind == ast.KindIdentifier {
					if l := e.layoutOfVar(bin.Right.Text()); l != nil {
						if _, ok := l.offsets[s]; ok {
							verdict = "1"
						} else {
							verdict = "0"
						}
					}
				}
			}
		}
		if verdict == "" {
			e.refuse(n, "in operator needs a literal key and a known-layout object")
			return "0", tUnknown
		}
		t := e.freshTmp()
		e.emit("%s = %s", t, verdict)
		return t, tBool
	}
	// `??` lowers as a nullish join-slot (mirrors sa_plugin_ts
	// parseNullishCoalesce): the subset maps null/undefined to 0, so a
	// nonzero left passes through, otherwise the right lowers. The right
	// lowers only in the fallback arm (lazy, single evaluation each).
	if op == ast.KindQuestionQuestionToken {
		l, lt := e.lowerExpr(bin.Left)
		slot := e.freshTmp()
		e.emit("%s = alloc 8", slot)
		e.ownTemp(slot)
		c := e.freshTmp()
		tL := e.freshLabel("null_t")
		fL := e.freshLabel("null_f")
		endL := e.freshLabel("null_end")
		e.emit("%s = ne %s, 0", c, l)
		e.emit("br %s -> %s, %s", c, tL, fL)
		e.emitRaw("%s:", tL)
		e.emit("store %s + 0, %s as ptr", slot, l)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", fL)
		r, _ := e.lowerExpr(bin.Right)
		e.emit("store %s + 0, %s as ptr", slot, r)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", endL)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", out, slot)
		e.releaseIfOwnedTemp(slot)
		return out, lt
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
		if lt == tString && rt == tString {
			// Content equality (address compare would lie): equal
			// lengths plus a zero-offset indexOf hit.
			e.emit("%s = %s", t, e.stringContentEq(l, r, false))
		} else if floats {
			e.emit("%s = fcmp_eq %s, %s", t, l, r)
		} else {
			e.emit("%s = eq %s, %s", t, l, r)
		}
	case ast.KindExclamationEqualsToken, ast.KindExclamationEqualsEqualsToken:
		if lt == tString && rt == tString {
			e.emit("%s = %s", t, e.stringContentEq(l, r, true))
		} else if floats {
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
	// `f?.()` with a statically nullable callee guards null (checker-
	// driven; the subset has no nullable function values otherwise, so
	// non-nullable callees keep the direct shape).
	if call.QuestionDotToken != nil && e.tcx != nil &&
		call.Expression.Kind == ast.KindIdentifier && e.tcx.nullable(call.Expression) {
		return e.lowerGuardedCall(n)
	}
	args := []string{}
	argTypes := []saType{}
	for _, a := range call.Arguments.Nodes {
		// Spread markers expand per-callee at dispatch (see
		// resolveSpreadCall); Math spread reduces a slice separately.
		if a.Kind == ast.KindSpreadElement {
			se := a.AsSpreadElement()
			sv, st := e.lowerExpr(se.Expression)
			args = append(args, "@spread:"+sv)
			argTypes = append(argTypes, st)
			continue
		}
		// Callbacks lower inline at the higher-order call site (see
		// lowerHigherOrder); pre-lowering them here would refuse.
		if a.Kind == ast.KindArrowFunction || a.Kind == ast.KindFunctionExpression {
			args = append(args, "@callback:")
			argTypes = append(argTypes, tI32)
			continue
		}
		v, t := e.lowerExpr(a)
		args = append(args, v)
		argTypes = append(argTypes, t)
	}
	// console.log(...) → @sa_print_bytes via sa_std/io/print.sai
	if isConsoleLog(call.Expression) {
		return e.lowerConsoleLog(args, argTypes, n), tVoid
	}
	// Math.* inline idioms (reference math_surface; trig etc. refuse).
	if name, ok := mathMethod(call.Expression); ok {
		if v, t, ok := e.lowerMathCall(name, args, argTypes, call.Arguments, n); ok {
			return v, t
		}
		e.refuse(n, "Math.%s is not supported", name)
		return "0", tUnknown
	}
	// Top-level `var f = Math.g` aliases dispatch as Math.g.
	if call.Expression.Kind == ast.KindIdentifier {
		if g, ok := e.mathAliases[call.Expression.Text()]; ok {
			if v, t, ok := e.lowerMathCall(g, args, argTypes, call.Arguments, n); ok {
				return v, t
			}
			e.refuse(n, "Math.%s is not supported", g)
			return "0", tUnknown
		}
	}
	// String.fromCharCode → sa_std/string.sai (policy: primitives live in sci).
	// The primitive returns a BUFFER handle (u64); unwrap via
	// sa_fmt_buffer_data/len exactly like concatSlices (reading the u64
	// as a slice header segfaults).
	if isStringFromCharCode(call.Expression) {
		e.needImport("sa_std/string.sai")
		e.needImport("sa_std/fmt.sai")
		hbuf := e.freshTmp()
		e.emit("%s = call @sa_string_from_char_code(%s)", hbuf, strings.Join(args, ", "))
		e.ownTemp(hbuf)
		hptr := e.freshTmp()
		e.emit("%s = call @sa_fmt_buffer_data(%s)", hptr, hbuf)
		e.ownTemp(hptr)
		hlen := e.freshTmp()
		e.emit("%s = call @sa_fmt_buffer_len(%s)", hlen, hbuf)
		e.ownTemp(hlen)
		t := e.freshTmp()
		e.emit("%s = alloc 16", t)
		e.emit("store %s + 0, %s as ptr", t, hptr)
		e.emit("store %s + 8, %s as u64", t, hlen)
		e.declareOwned(t)
		e.releaseIfOwnedTemp(hptr)
		e.releaseIfOwnedTemp(hlen)
		e.releaseIfOwnedTemp(hbuf)
		return t, tString
	}
	// Number.isInteger(x): i32 operands are trivially integral.
	if isNumberIsInteger(call.Expression) {
		if len(args) != 1 {
			e.refuse(n, "Number.isInteger takes one argument")
			return "0", tUnknown
		}
		if len(argTypes) > 0 && argTypes[0] == tF64 {
			e.refuse(n, "Number.isInteger on floats is not lowerable (i32 subset only)")
			return "0", tUnknown
		}
		return "1", tBool
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
		// Array(n) / Array(a, b, c): constructor-call form (JS semantics:
		// single length allocates, multiple elements literalize).
		if fname == "Array" {
			if len(args) == 1 {
				return e.newSizedArray(args[0]), tArray
			}
			h := e.newEmptyArray()
			for _, a := range args {
				if strings.HasPrefix(a, "@spread:") || strings.HasPrefix(a, "@callback:") {
					e.refuse(n, "Array(...) elements must be plain values")
					return "0", tUnknown
				}
				e.lowerArrayPush(h, a, "i32", 4)
			}
			return h, tArray
		}
		// String(x): strings pass through, scalars render via the shared
		// sa_fmt_*_into path (same primitives as template interpolation).
		if fname == "String" && len(args) == 1 {
			at := tI32
			if len(argTypes) > 0 {
				at = argTypes[0]
			}
			if at == tString {
				return args[0], tString
			}
			e.needImport("sa_std/fmt.sai")
			if v, ok := e.renderInterpValue(args[0], at, n); ok {
				return v, tString
			}
			return "0", tUnknown
		}
		// Number(s): numeric strings parse via sa_parse_float (sci primitive).
		if fname == "Number" && len(args) == 1 {
			at := tI32
			if len(argTypes) > 0 {
				at = argTypes[0]
			}
			if at == tString {
				e.needImport("sa_std/string.sai")
				bp, bl := e.expandSlice(args[0])
				t := e.freshTmp()
				e.emit("%s = call @sa_parse_float(%s, %s)", t, bp, bl)
				return t, tF64
			}
			return args[0], at
		}
		// parseInt(s)/parseFloat(s): decimal/lexical scan (sci/simulated
		// only via existing sa_parse_float for floats; integers scan inline
		// per the reference lowerParseInt).
		if fname == "parseInt" && len(args) == 1 {
			return e.lowerParseIntCall(args[0], n), tI32
		}
		if fname == "parseFloat" && len(args) == 1 {
			e.needImport("sa_std/string.sai")
			bp, bl := e.expandSlice(args[0])
			t := e.freshTmp()
			e.emit("%s = call @sa_parse_float(%s, %s)", t, bp, bl)
			e.ownTemp(t)
			return t, tF64
		}
		// structuredClone(v): deep copy by static shape (see lowerClone).
		if fname == "structuredClone" && len(args) == 1 {
			at := tI32
			if len(argTypes) > 0 {
				at = argTypes[0]
			}
			// Identifiers report tI32; recover handle-ness from the
			// binding maps (arrays/strings need element-wise copies).
			if e.arrVars[args[0]] {
				at = tArray
			} else if e.strVars[args[0]] {
				at = tString
			}
			return e.lowerDeepClone(args[0], at, n), at
		}
		// Named std imports: readFile(...) with `import { readFile } from "fs"`.
		if mod, ok := e.importedFrom[fname]; ok {
			if proj, ok := projectionByTS(mod + "." + fname); ok {
				v, t := e.emitProjCall(proj, args, n)
				// fs.readFile returns a BUFFER handle (u64!): unwrap via
				// read_buffer_data/len like from_char_code (a direct
				// slice read yields a garbage length).
				if proj.TS == "fs.readFile" && !e.refused {
					e.needImport("sa_std/fs.sai")
					return e.unwrapFsBuffer(v)
				}
				return v, t
			}
			e.refuse(n, "%s.%s is not a projected std surface (see StdProjectionTable)", mod, fname)
			return "0", tUnknown
		}
		// Arrow alias: `let f = (x) => ...; f(41)` rewrites to the
		// out-of-line callee with captured outer variables appended.
		if ai, ok := e.arrowAliases[fname]; ok {
			for _, a := range args {
				if strings.HasPrefix(a, "@callback:") {
					e.refuse(n, "spread arguments to arrow callees are not lowerable")
					return "0", tUnknown
				}
			}
			// Arrows declare no defaults: exact arity (captures append
			// internally and never count).
			if len(args) != len(ai.params) {
				e.refuse(n, "arity mismatch in call to %s (%d given, %d expected)", fname, len(args), len(ai.params))
				return "0", tUnknown
			}
			full := append(append([]string{}, args...), ai.captures...)
			if ai.ret == tVoid {
				e.emit("call @%s(%s)", ai.fn, strings.Join(full, ", "))
				return "0", tVoid
			}
			t := e.freshTmp()
			e.emit("%s = call @%s(%s)", t, ai.fn, strings.Join(full, ", "))
			e.ownTemp(t)
			return t, ai.ret
		}
		// Cross-file import: `import { add } from "./util"` resolves to
		// the qualified callee (link-time import environment).
		if q, ok := e.importEnv[fname]; ok {
			ret := e.importRet[fname]
			args, argTypes = e.resolveSpreadCall(q, args, argTypes, n)
			if e.refused {
				return "0", tUnknown
			}
			if !e.checkArity(q, args, n) {
				return "0", tUnknown
			}
			if ret == tVoid {
				e.emit("call @%s(%s)", q, strings.Join(args, ", "))
				return "0", tVoid
			}
			t := e.freshTmp()
			e.emit("%s = call @%s(%s)", t, q, strings.Join(args, ", "))
			e.ownTemp(t)
			return t, ret
		}
		if ret, ok := e.funcSigs[fname]; ok {
			// Defined in another linked file but not imported: ES
			// semantics require an explicit import (loud, not silent).
			if e.link != nil && !e.localDefs[fname] {
				e.refuse(n, "%s is defined in another file; import it first", fname)
				return "0", tUnknown
			}
			for _, a := range args {
				if strings.HasPrefix(a, "@callback:") {
					e.refuse(n, "function values are not first-class; callbacks inline only at higher-order array sites")
					return "0", tUnknown
				}
			}
			args, argTypes = e.resolveSpreadCall(fname, args, argTypes, n)
			if e.refused {
				return "0", tUnknown
			}
			if !e.checkArity(fname, args, n) {
				return "0", tUnknown
			}
			if ret == tVoid {
				e.emit("call @%s(%s)", e.fnRef(fname), strings.Join(args, ", "))
				return "0", tVoid
			}
			t := e.freshTmp()
			e.emit("%s = call @%s(%s)", t, e.fnRef(fname), strings.Join(args, ", "))
			e.ownTemp(t)
			return t, ret
		}
		if e.link != nil {
			if owner, ok := e.linkExports[fname]; ok {
				e.refuse(n, "%s is defined in %s; import it first", fname, owner)
				return "0", tUnknown
			}
		}
		e.refuse(n, "call to unknown function %s (declare it before use)", fname)
		return "0", tUnknown
	}
	// Method calls: Math.* inline idioms and string-method projections
	// (sci/sa_std reuse), mirroring sa_plugin_ts lib surface tables.
	if call.Expression.Kind == ast.KindPropertyAccessExpression {
		if v, t, ok := e.lowerMethodCall(call.Expression, args, argTypes, call.Arguments, n); ok {
			return v, t
		}
	}
	e.refuse(n, "call target is not in the SA-lowerable subset (functions are not first-class values)")
	return "0", tUnknown
}

// lowerGuardedCall emits the null-guarded direct call join for `f?.()`:
// null callee yields 0, otherwise the call runs (identifier callees only;
// member optionals route through the method/property guards).
func (e *emitter) lowerGuardedCall(n *ast.Node) (string, saType) {
	call := n.AsCallExpression()
	fname := call.Expression.Text()
	base := fname
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	endL := e.freshLabel("call_end")
	nullL := e.freshLabel("call_null")
	okL := e.freshLabel("call_ok")
	isnull := e.freshTmp()
	e.emit("%s = eq %s, 0", isnull, base)
	e.emit("br %s -> %s, %s", isnull, nullL, okL)
	e.emitRaw("%s:", nullL)
	e.emit("store %s + 0, 0 as ptr", slot)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", okL)
	// Re-parse as a direct call: build a detached direct-call lowering by
	// reusing the same argument nodes through the standard path. The
	// callee is an identifier, so re-lowering is pure.
	args := []string{}
	for _, a := range call.Arguments.Nodes {
		v, _ := e.lowerExpr(a)
		args = append(args, v)
	}
	v := e.lowerDirectCallee(fname, args, n)
	e.emit("store %s + 0, %s as ptr", slot, v)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	dest := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", dest, slot)
	e.releaseIfOwnedTemp(slot)
	return dest, tI32
}

// lowerDirectCallee lowers a same-scope identifier call without optional
// handling (shared by the guarded-call ok arm).
func (e *emitter) lowerDirectCallee(fname string, args []string, n *ast.Node) string {
	if ai, ok := e.arrowAliases[fname]; ok {
		full := append(append([]string{}, args...), ai.captures...)
		if ai.ret == tVoid {
			e.emit("call @%s(%s)", ai.fn, strings.Join(full, ", "))
			return "0"
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, ai.fn, strings.Join(full, ", "))
		e.ownTemp(t)
		return t
	}
	if q, ok := e.importEnv[fname]; ok {
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, q, strings.Join(args, ", "))
		e.ownTemp(t)
		return t
	}
	if _, ok := e.funcSigs[fname]; ok {
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, e.fnRef(fname), strings.Join(args, ", "))
		e.ownTemp(t)
		return t
	}
	e.refuse(n, "call to unknown function %s (declare it before use)", fname)
	return "0"
}

// unwrapFsBuffer wraps an fs buffer handle into a {ptr,len} slice: the
// u64! call value is a {status:i32, payload:u64} struct in memory; the
// payload at +8 is the buffer for data/len.
func (e *emitter) unwrapFsBuffer(buf string) (string, saType) {
	h := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", h, buf)
	e.releaseIfOwnedTemp(buf)
	bp := e.freshTmp()
	e.emit("%s = call @sa_fs_read_buffer_data(%s)", bp, h)
	e.ownTemp(bp)
	bl := e.freshTmp()
	e.emit("%s = call @sa_fs_read_buffer_len(%s)", bl, h)
	e.ownTemp(bl)
	out := e.freshTmp()
	e.emit("%s = alloc 16", out)
	e.emit("store %s + 0, %s as ptr", out, bp)
	e.emit("store %s + 8, %s as u64", out, bl)
	e.declareOwned(out)
	e.releaseIfOwnedTemp(bp)
	e.releaseIfOwnedTemp(bl)
	e.releaseIfOwnedTemp(buf)
	return out, tString
}

// lowerMethodCall dispatches property calls. It returns ok=false when the
// method is not a projected surface (caller refuses loudly).
func (e *emitter) lowerMethodCall(fn *ast.Node, args []string, types []saType, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	pa := fn.AsPropertyAccessExpression()
	method := pa.Name().Text()
	// Math is handled at the call site (needs argument nodes for spread).
	// `this.m(...)` aliases the current method receiver (Text() panics on
	// non-identifier expressions, so resolve the receiver first).
	recv := ""
	if pa.Expression.Kind == ast.KindThisKeyword {
		if e.thisSelf == "" {
			e.refuse(pos, "`this` outside a class method is not lowerable")
			return "0", tUnknown, true
		}
		recv = e.thisSelf
	} else if pa.Expression.Kind == ast.KindIdentifier {
		recv = pa.Expression.Text()
	} else {
		return "", tUnknown, false
	}
	// Namespace import: `import * as u` + `u.add(1)` calls the qualified
	// callee (export must exist; checked at link time).
	if fileKey, ok := e.nsImports[recv]; ok {
		_ = fileKey
		q, ok := e.importEnv[recv+"."+method]
		if !ok {
			e.refuse(pos, "%s.%s is not exported by its module", recv, method)
			return "0", tUnknown, true
		}
		ret := e.importRet[recv+"."+method]
		if ret == tVoid {
			e.emit("call @%s(%s)", q, strings.Join(args, ", "))
			return "0", tVoid, true
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, q, strings.Join(args, ", "))
		e.ownTemp(t)
		return t, ret, true
	}
	if pa.Expression.Kind == ast.KindThisKeyword {
		// Per-instance fn fields devirtualize (`this.pick(e)` replays the
		// captured arrow inline with caller-scope captures).
		if fields, ok := e.instFnFields[recv]; ok {
			if anode, ok := fields[method]; ok {
				if v, t, ok := e.inlineInstanceCallback(recv, anode, args, argNodes, pos); ok {
					return v, t, true
				}
				return "", tUnknown, false
			}
		}
	}
	// Class methods inline at the call site (no vtables in SA-ASM).
	if className, ok := e.varClass[recv]; ok {
		if v, t, ok := e.lowerClassMethodCall(recv, className, method, args, argNodes, pos); ok {
			return v, t, true
		}
		// Unknown method: fall through to array/string surfaces, then refuse.
	}
	// Map/Set handles dispatch to the sa_std btree backends (never
	// simulated; keys encode via mapKeySlice).
	if e.mapVars[recv] {
		if v, t, ok := e.lowerMapMethod(recv, method, args, types, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
	}
	if e.setVars[recv] {
		if v, t, ok := e.lowerSetMethod(recv, method, args, types, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
	}
	// Higher-order array sites inline the callback body (no function
	// pointers; captures resolve in the caller scope).
	if isHigherOrderMethod(method) {
		if v, t, ok := e.lowerHigherOrder(recv, method, args, argNodes, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
	}
	// Receiver-kind dispatch (mirrors the scope lookup): string bindings
	// route to the string surface, array bindings to array methods.
	if e.strVars[recv] {
		if v, t, ok := e.lowerStringMethod(recv, method, args, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
	}
	if v, t, ok := e.lowerArrayMethod(recv, method, args, types, argNodes, pos); ok {
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

// inlineInstanceCallback replays a per-instance captured arrow
// (`this.pick(e)`): parameters bind positionally to the lowered arguments.
func (e *emitter) inlineInstanceCallback(recv string, anode *ast.Node, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	_ = recv
	_ = argNodes
	vals := []string{}
	for _, a := range args {
		if strings.HasPrefix(a, "@callback:") || strings.HasPrefix(a, "@spread:") {
			e.refuse(pos, "callback/spread arguments to instance callbacks are not lowerable")
			return "0", tUnknown, false
		}
		vals = append(vals, a)
	}
	v, t := e.callbackValue(anode, vals, true, pos)
	if e.refused {
		return "0", tUnknown, false
	}
	return v, t, true
}

// mapKeySlice encodes a key operand as a {ptr,len} slice: strings pass
// through, integers box into a fresh 8-byte cell (reference shape).
func (e *emitter) mapKeySlice(key string, kt saType) string {
	if kt == tString {
		return key
	}
	cell := e.freshTmp()
	e.emit("%s = alloc 8", cell)
	e.emit("store %s + 0, %s as i32", cell, key)
	slice := e.freshTmp()
	e.emit("%s = alloc 16", slice)
	e.emit("store %s + 0, %s as ptr", slice, cell)
	e.emit("store %s + 8, 4 as u64", slice)
	e.declareOwned(slice)
	return slice
}

// lowerMapMethod projects Map methods onto sa_std/btree_map.sa.
func (e *emitter) lowerMapMethod(recv, method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
	e.needImport("sa_std/btree_map.sa")
	kt := tI32
	if len(types) > 0 {
		kt = types[0]
	}
	switch method {
	case "set":
		if len(args) != 2 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		e.emit("call @sa_btree_map_insert(&%s, &%s, %s)", recv, ks, args[1])
		return "0", tI32, true
	case "get":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_map_get(&%s, &%s)", t, recv, ks)
		e.ownTemp(t)
		return t, tI32, true
	case "has":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_map_contains_key(&%s, &%s)", t, recv, ks)
		e.ownTemp(t)
		return t, tBool, true
	case "delete":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		// Probe presence first so a stored 0 still reports true.
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_map_contains_key(&%s, &%s)", t, recv, ks)
		e.ownTemp(t)
		drop := e.freshTmp()
		e.emit("%s = call @sa_btree_map_remove(&%s, &%s)", drop, recv, ks)
		e.ownTemp(drop)
		return t, tBool, true
	case "clear":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		e.emit("call @sa_btree_map_clear(&%s)", recv)
		return "0", tI32, true
	case "size", "getSize":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_map_len(&%s)", t, recv)
		e.ownTemp(t)
		return t, tI32, true
	case "keys", "values", "entries":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		sym := "sa_btree_map_keys_set"
		if method == "values" {
			sym = "sa_btree_map_values_vec"
		} else if method == "entries" {
			sym = "sa_btree_map_iter_vec"
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(&%s)", t, sym, recv)
		e.declareOwned(t)
		return t, tArray, true
	default:
		e.refuse(pos, "Map.%s is not a projected surface", method)
		return "0", tUnknown, true
	}
}

// lowerSetMethod projects Set methods onto sa_std/btree_set.sa.
func (e *emitter) lowerSetMethod(recv, method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
	e.needImport("sa_std/btree_set.sa")
	kt := tI32
	if len(types) > 0 {
		kt = types[0]
	}
	switch method {
	case "add":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		e.emit("call @sa_btree_set_insert(&%s, &%s)", recv, ks)
		return "0", tI32, true
	case "has":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_set_contains(&%s, &%s)", t, recv, ks)
		e.ownTemp(t)
		return t, tBool, true
	case "delete":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		ks := e.mapKeySlice(args[0], kt)
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_set_contains(&%s, &%s)", t, recv, ks)
		e.ownTemp(t)
		drop := e.freshTmp()
		e.emit("%s = call @sa_btree_set_remove(&%s, &%s)", drop, recv, ks)
		e.ownTemp(drop)
		return t, tBool, true
	case "clear":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		e.emit("call @sa_btree_set_clear(&%s)", recv)
		return "0", tI32, true
	case "size":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		t := e.freshTmp()
		e.emit("%s = call @sa_btree_set_len(&%s)", t, recv)
		e.ownTemp(t)
		return t, tI32, true
	default:
		e.refuse(pos, "Set.%s is not a projected surface", method)
		return "0", tUnknown, true
	}
}

// isHigherOrderMethod reports array methods taking a callback (inlined at
// the call site; plain forms of sort/toSorted still lower directly).
func isHigherOrderMethod(method string) bool {
	switch method {
	case "forEach", "map", "filter", "find", "findIndex", "findLast", "findLastIndex",
		"some", "every", "reduce", "reduceRight", "sort", "toSorted":
		return true
	}
	return false
}

// lowerHigherOrder inlines arrow callbacks into loop shapes (no function
// pointers exist in SA-ASM; captures resolve in the caller scope, matching
// the desugared semantics of sa_plugin_ts callbacks).
func (e *emitter) lowerHigherOrder(recv, method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	elem := e.arrElems[recv]
	if elem == "" {
		elem = "i32"
	}
	// Fixed 4-byte slots: literals, push, checked-index and clone all
	// build/read 4-wide (nested handles truncate but round-trip); a
	// type-sized esz mis-strides those buffers (segfault). Struct field
	// layouts still use widthOf; only array slots are fixed.
	esz := 4
	// Locate the inline callback (arrows arrive as @callback: markers;
	// named function values cannot inline without a body).
	var cb *ast.Node
	cbIdx := -1
	if argNodes != nil {
		for i, a := range argNodes.Nodes {
			if a.Kind == ast.KindArrowFunction || a.Kind == ast.KindFunctionExpression {
				cb = a
				cbIdx = i
				break
			}
			if a.Kind == ast.KindIdentifier {
				if _, ok := e.arrowAliases[a.Text()]; ok {
					e.refuse(a, "pass the arrow inline at %s (named callbacks do not inline)", method)
					return "0", tUnknown, true
				}
			}
		}
	}
	// Plain sort/toSorted (no callback) lower directly.
	if cb == nil && (method == "sort" || method == "toSorted") {
		return e.lowerArrayMethod(recv, method, args, nil, argNodes, pos)
	}
	if cb == nil {
		e.refuse(pos, "%s needs an inline arrow callback", method)
		return "0", tUnknown, true
	}
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	loadElem := func(idx string) string {
		off := e.freshTmp()
		e.emit("%s = mul %s, %d", off, idx, esz)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, data, off)
		v := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", v, addr)
		return v
	}
	switch method {
	case "forEach":
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("fe_top")
		bodyL := e.freshLabel("fe_body")
		endL := e.freshLabel("fe_end")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		e.callbackValue(cb, []string{el, i}, false, pos)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return "0", tI32, true
	case "map":
		h := e.newEmptyArray()
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("mp_top")
		bodyL := e.freshLabel("mp_body")
		endL := e.freshLabel("mp_end")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		v, _ := e.callbackValue(cb, []string{el, i}, true, pos)
		e.lowerArrayPush(h, v, "i32", 4)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return h, tArray, true
	case "filter":
		h := e.newEmptyArray()
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("fi_top")
		bodyL := e.freshLabel("fi_body")
		endL := e.freshLabel("fi_end")
		takeL := e.freshLabel("fi_take")
		skipL := e.freshLabel("fi_skip")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		v, _ := e.callbackValue(cb, []string{el, i}, true, pos)
		e.emit("br %s -> %s, %s", v, takeL, skipL)
		e.emitRaw("%s:", takeL)
		e.lowerArrayPush(h, el, "i32", 4)
		e.emit("jmp %s", skipL)
		e.emitRaw("%s:", skipL)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return h, tArray, true
	case "find", "findIndex":
		slot := e.freshTmp()
		e.emit("%s = alloc 8", slot)
		e.ownTemp(slot)
		init := "-1"
		if method == "find" {
			init = "0"
		}
		e.emit("store %s + 0, %s as ptr", slot, init)
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("fd_top")
		bodyL := e.freshLabel("fd_body")
		endL := e.freshLabel("fd_end")
		hitL := e.freshLabel("fd_hit")
		nextL := e.freshLabel("fd_next")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		v, _ := e.callbackValue(cb, []string{el, i}, true, pos)
		e.emit("br %s -> %s, %s", v, hitL, nextL)
		e.emitRaw("%s:", hitL)
		if method == "find" {
			e.emit("store %s + 0, %s as ptr", slot, el)
		} else {
			e.emit("store %s + 0, %s as ptr", slot, i)
		}
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", nextL)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", out, slot)
		e.releaseIfOwnedTemp(slot)
		return out, tI32, true
	case "findLast", "findLastIndex":
		// Same scan without early exit: every hit overwrites the slot,
		// so the last match wins (reference reverse-scan semantics).
		slot := e.freshTmp()
		e.emit("%s = alloc 8", slot)
		e.ownTemp(slot)
		init := "-1"
		if method == "findLast" {
			init = "0"
		}
		e.emit("store %s + 0, %s as ptr", slot, init)
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("fl_top")
		bodyL := e.freshLabel("fl_body")
		endL := e.freshLabel("fl_end")
		hitL := e.freshLabel("fl_hit")
		nextL := e.freshLabel("fl_next")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		v, _ := e.callbackValue(cb, []string{el, i}, true, pos)
		e.emit("br %s -> %s, %s", v, hitL, nextL)
		e.emitRaw("%s:", hitL)
		if method == "findLast" {
			e.emit("store %s + 0, %s as ptr", slot, el)
		} else {
			e.emit("store %s + 0, %s as ptr", slot, i)
		}
		e.emit("jmp %s", nextL)
		e.emitRaw("%s:", nextL)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", out, slot)
		e.releaseIfOwnedTemp(slot)
		return out, tI32, true
	case "some", "every":
		slot := e.freshTmp()
		e.emit("%s = alloc 8", slot)
		e.ownTemp(slot)
		init, stop := "0", "1"
		if method == "every" {
			init, stop = "1", "0"
		}
		e.emit("store %s + 0, %s as ptr", slot, init)
		i := e.freshTmp()
		e.emit("%s = 0", i)
		topL := e.freshLabel("se_top")
		bodyL := e.freshLabel("se_body")
		endL := e.freshLabel("se_end")
		hitL := e.freshLabel("se_hit")
		nextL := e.freshLabel("se_next")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		v, _ := e.callbackValue(cb, []string{el, i}, true, pos)
		cmp := v
		if method == "every" {
			nv := e.freshTmp()
			e.emit("%s = eq %s, 0", nv, v)
			cmp = nv
		}
		e.emit("br %s -> %s, %s", cmp, hitL, nextL)
		e.emitRaw("%s:", hitL)
		e.emit("store %s + 0, %s as ptr", slot, stop)
		e.emit("jmp %s", endL)
		e.emitRaw("%s:", nextL)
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		out := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", out, slot)
		e.releaseIfOwnedTemp(slot)
		return out, tBool, true
	case "reduce", "reduceRight":
		right := method == "reduceRight"
		hasInit := cbIdx >= 0 && len(args) > cbIdx+1
		var initVal string
		if hasInit {
			initVal = args[cbIdx+1]
			// The init arg lowered before the callback marker; with a
			// single callback arg the init is args[1].
			if len(args) > 1 && args[1] != "@callback:" {
				initVal = args[1]
			}
		}
		acc := e.freshTmp()
		i := e.freshTmp()
		if hasInit {
			e.emit("%s = add %s, 0", acc, initVal)
			if !right {
				e.emit("%s = 0", i)
			} else {
				e.emit("%s = sub %s, 1", i, ln)
			}
		} else {
			// Seed from the edge element via the OOB-safe join (empty
			// arrays seed 0 and skip the loop; the subset maps
			// undefined to 0 instead of throwing).
			if !right {
				e.emit("%s = add %s, 0", acc, e.lowerCheckedIndex(recv, "0", false))
				e.emit("%s = 1", i)
			} else {
				last := e.freshTmp()
				e.emit("%s = sub %s, 1", last, ln)
				e.emit("%s = add %s, 0", acc, e.lowerCheckedIndex(recv, last, false))
				e.emit("%s = sub %s, 2", i, ln)
			}
		}
		topL := e.freshLabel("rd_top")
		bodyL := e.freshLabel("rd_body")
		endL := e.freshLabel("rd_end")
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		if !right {
			e.emit("%s = slt %s, %s", c, i, ln)
		} else {
			e.emit("%s = sge %s, 0", c, i)
		}
		e.emit("br %s -> %s, %s", c, bodyL, endL)
		e.emitRaw("%s:", bodyL)
		el := loadElem(i)
		// Callback params: (acc, cur[, idx]).
		v, _ := e.callbackValue(cb, []string{acc, el, i}, true, pos)
		e.emit("%s = %s", acc, v)
		step := e.freshTmp()
		if !right {
			e.emit("%s = add %s, 1", step, i)
		} else {
			e.emit("%s = sub %s, 1", step, i)
		}
		e.emit("%s = %s", i, step)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return acc, tI32, true
	case "sort":
		return e.lowerSortWithCmp(recv, elem, esz, argNodes, pos)
	case "toSorted":
		cp := e.lowerArraySlice(recv, elem, esz, "0", "", pos)
		return e.lowerSortWithCmp(cp, elem, esz, argNodes, pos)
	}
	return "", tUnknown, false
}

// callbackValue lowers one inline callback application: parameters bind to
// the given loop values (identifier or array-pattern), then the body lowers
// (expression bodies deliver the value; block bodies join through a slot
// with return interception).
func (e *emitter) callbackValue(cb *ast.Node, argVals []string, wantValue bool, pos *ast.Node) (string, saType) {
	params := cb.Parameters()
	if len(params) > len(argVals) {
		e.refuse(pos, "callback declares %d parameters but only %d values are provided", len(params), len(argVals))
		return "0", tUnknown
	}
	e.pushScope()
	for i, p := range params {
		e.bindCallbackParam(p.AsNode(), argVals[i], pos)
		if e.refused {
			e.popScope()
			return "0", tUnknown
		}
	}
	body := cb.Body()
	if body == nil {
		e.refuse(pos, "callback has no body")
		e.popScope()
		return "0", tUnknown
	}
	if body.Kind != ast.KindBlock {
		v, t := e.lowerExpr(body)
		e.popScope()
		return v, t
	}
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	e.emit("store %s + 0, 0 as ptr", slot)
	endL := e.freshLabel("cb_end")
	saved := e.inlineRet
	e.inlineRet = &inlineRetState{active: true, slot: slot, end: endL, saname: "i32"}
	e.terminated = false
	for _, s := range body.Statements() {
		e.lowerBlockStatement(s)
		if e.refused {
			break
		}
	}
	e.inlineRet = saved
	e.emitRaw("%s:", endL)
	e.terminated = false
	out := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", out, slot)
	e.releaseIfOwnedTemp(slot)
	e.popScope()
	_ = wantValue
	return out, tI32
}

// bindCallbackParam binds one callback parameter: plain identifiers
// snapshot scalars and alias handles (never copy), array patterns
// destructure by safe indexed loads.
func (e *emitter) bindCallbackParam(p *ast.Node, val string, pos *ast.Node) {
	name := p.Name()
	if name == nil {
		e.refuse(pos, "callback parameter has no binding name")
		return
	}
	if name.Kind == ast.KindIdentifier {
		if l := e.layoutOfVar(val); l != nil {			e.declareAlias(name.Text(), val)
			if e.varLayouts == nil {
				e.varLayouts = map[string]*layout{}
			}
			e.varLayouts[name.Text()] = l
			return
		}
		if e.arrVars[val] || e.strVars[val] {
			e.declareAlias(name.Text(), val)
			if e.arrVars[val] {
				if e.arrVars == nil {
					e.arrVars = map[string]bool{}
				}
				e.arrVars[name.Text()] = true
				if e.arrElems == nil {
					e.arrElems = map[string]string{}
				}
				e.arrElems[name.Text()] = e.arrElems[val]
			}
			if e.strVars[val] {
				if e.strVars == nil {
					e.strVars = map[string]bool{}
				}
				e.strVars[name.Text()] = true
			}
			return
		}
		// Scalars snapshot by copy, never move: a `p = tmp` move of a
		// loop-invariant temp breaks back-edge path states
		// (PhiStateConflict), so callbacks always copy their values.
		e.emit("%s = add %s, 0", name.Text(), val)
		e.declarePlain(name.Text())
		return
	}
	if name.Kind == ast.KindArrayBindingPattern {
		e.destructureArray(name, val, pos)
		return
	}
	if name.Kind == ast.KindObjectBindingPattern {
		e.destructureObject(name, val, pos)
		return
	}
	e.refuse(pos, "callback parameter shape is not lowerable")
}

// destructureArray binds [a, b, ...] from a slice via safe indexed loads
// (missing elements read 0, matching the undefined→0 subset rule).
func (e *emitter) destructureArray(pat *ast.Node, arr string, pos *ast.Node) {
	idx := 0
	for _, el := range pat.AsBindingPattern().Elements.Nodes {
		if el.Kind != ast.KindBindingElement {
			idx++
			continue
		}
		be := el.AsBindingElement()
		if be.DotDotDotToken != nil {
			e.refuse(pos, "rest elements in destructuring are not lowerable")
			return
		}
		nm := be.Name()
		if nm == nil {
			idx++
			continue
		}
		v := e.lowerCheckedIndex(arr, fmt.Sprintf("%d", idx), false)
		e.bindPatternName(nm, v, arr, idx, pos)
		if e.refused {
			return
		}
		idx++
	}
}

// lowerDestructuringDecl binds `const [a, b] = arr` / `const {x} = obj`
// element/field-wise (initializers lower once, then destructure).
func (e *emitter) lowerDestructuringDecl(d, nm, init *ast.Node) {
	if init.Kind == ast.KindArrowFunction || init.Kind == ast.KindFunctionExpression {
		e.refuse(d, "function values do not destructure")
		return
	}
	v, _ := e.lowerExpr(init)
	if e.refused {
		return
	}
	switch nm.Kind {
	case ast.KindArrayBindingPattern:
		if e.arrVars == nil {
			e.arrVars = map[string]bool{}
		}
		if e.arrElems == nil {
			e.arrElems = map[string]string{}
		}
		e.arrVars[v] = true
		if _, ok := e.arrElems[v]; !ok {
			e.arrElems[v] = "i32"
		}
		e.destructureArray(nm, v, d)
	case ast.KindObjectBindingPattern:
		if init.Kind == ast.KindObjectLiteralExpression {
			if l := e.layoutOfLiteral(init); l != nil {
				if e.varLayouts == nil {
					e.varLayouts = map[string]*layout{}
				}
				e.varLayouts[v] = l
			}
		}
		e.destructureObject(nm, v, d)
	default:
		e.refuse(d, "binding pattern %s is not lowerable", nm.Kind.String())
	}
}

// destructureObject binds {x, y: z} from a struct handle via static layout
// offsets (mirrors interface field loads).
func (e *emitter) destructureObject(pat *ast.Node, obj string, pos *ast.Node) {
	l := e.layoutOfVar(obj)
	for _, el := range pat.AsBindingPattern().Elements.Nodes {
		if el.Kind != ast.KindBindingElement {
			continue
		}
		be := el.AsBindingElement()
		field := ""
		if be.PropertyName != nil {
			pn := be.PropertyName.AsNode()
			switch pn.Kind {
			case ast.KindIdentifier:
				field = pn.Text()
			case ast.KindStringLiteral:
				field = pn.Text()
			default:
				e.refuse(pos, "computed destructuring keys are not lowerable")
				return
			}
		}
		nm := be.Name()
		if nm == nil {
			continue
		}
		if field == "" && nm.Kind == ast.KindIdentifier {
			field = nm.Text()
		}
		if l == nil {
			e.refuse(pos, "object destructuring needs a recorded struct layout for %s", obj)
			return
		}
		off, ok := l.offsets[field]
		if !ok {
			e.refuse(pos, "field %s is not in the %s layout", field, l.name)
			return
		}
		saname := l.types[field]
		v := e.freshTmp()
		e.emit("%s = load %s + %d as %s", v, obj, off, saname)
		e.bindPatternName(nm, v, obj, -1, pos)
		if e.refused {
			return
		}
	}
}

// bindPatternName binds one destructured name: identifiers snapshot,
// nested patterns recurse.
func (e *emitter) bindPatternName(nm *ast.Node, v, src string, idx int, pos *ast.Node) {
	_ = src
	_ = idx
	if nm.Kind == ast.KindIdentifier {
		kind := "named"
		if isTempName(v) {
			kind = "temp"
		}
		e.assign(nm.Text(), v, kind, tI32, pos)
		e.trackBinding(nm.Text(), nil, nil, tI32)
		return
	}
	if nm.Kind == ast.KindArrayBindingPattern {
		e.destructureArray(nm, v, pos)
		return
	}
	if nm.Kind == ast.KindObjectBindingPattern {
		e.destructureObject(nm, v, pos)
		return
	}
	e.refuse(pos, "nested destructuring shape is not lowerable")
}

// lowerSortWithCmp sorts via insertion sort driven by an inline comparator:
// cmp(a, b) > 0 shifts a right (reference order semantics).
func (e *emitter) lowerSortWithCmp(recv, elem string, esz int, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	var cb *ast.Node
	if argNodes != nil {
		for _, a := range argNodes.Nodes {
			if a.Kind == ast.KindArrowFunction || a.Kind == ast.KindFunctionExpression {
				cb = a
				break
			}
		}
	}
	if cb == nil {
		e.refuse(pos, "sort with a comparator needs an inline arrow")
		return "0", tUnknown, true
	}
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	loadAt := func(idx string) string {
		off := e.freshTmp()
		e.emit("%s = mul %s, %d", off, idx, esz)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, data, off)
		v := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", v, addr)
		return v
	}
	storeAt := func(idx, v string) {
		off := e.freshTmp()
		e.emit("%s = mul %s, %d", off, idx, esz)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, data, off)
		e.emit("store %s + 0, %s as i32", addr, v)
	}
	// cmpGt(x, y) lowers the comparator body with x/y bound, testing > 0.
	cmpGt := func(x, y string) string {
		v, _ := e.callbackValue(cb, []string{x, y}, true, pos)
		c := e.freshTmp()
		e.emit("%s = sgt %s, 0", c, v)
		return c
	}
	topL := e.freshLabel("cs_top")
	bodyL := e.freshLabel("cs_body")
	endL := e.freshLabel("cs_end")
	i := e.freshTmp()
	e.emit("%s = 1", i)
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	key := loadAt(i)
	j := e.freshTmp()
	e.emit("%s = sub %s, 1", j, i)
	inTop := e.freshLabel("cs_in_top")
	inChk := e.freshLabel("cs_in_chk")
	inBody := e.freshLabel("cs_in_body")
	inEnd := e.freshLabel("cs_in_end")
	e.emitRaw("%s:", inTop)
	c1 := e.freshTmp()
	e.emit("%s = sge %s, 0", c1, j)
	e.emit("br %s -> %s, %s", c1, inChk, inEnd)
	e.emitRaw("%s:", inChk)
	aj := loadAt(j)
	gt := cmpGt(aj, key)
	e.emit("br %s -> %s, %s", gt, inBody, inEnd)
	e.emitRaw("%s:", inBody)
	j1 := e.freshTmp()
	e.emit("%s = add %s, 1", j1, j)
	storeAt(j1, aj)
	jm := e.freshTmp()
	e.emit("%s = sub %s, 1", jm, j)
	e.emit("%s = %s", j, jm)
	e.emit("jmp %s", inTop)
	e.emitRaw("%s:", inEnd)
	k1 := e.freshTmp()
	e.emit("%s = add %s, 1", k1, j)
	storeAt(k1, key)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return recv, tArray, true
}
// lowerMathCall dispatches Math.* per the projection table: @inline shapes,
// @const folds (property position only; calling a const refuses), unknown
// methods refuse (trig etc. are MathNotSupported in the reference).
func (e *emitter) lowerMathCall(method string, args []string, types []saType, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	proj, ok := projectionByTS("Math." + method)
	if !ok {
		return "", tUnknown, false
	}
	if strings.HasPrefix(proj.Symbol, "@const:") {
		e.refuse(pos, "Math.%s is a constant, not a function", method)
		return "0", tUnknown, true
	}
	switch method {
	case "abs", "pow", "floor", "ceil", "round", "trunc", "sqrt", "log10", "random":
		if v, t, ok := e.lowerMathInline(method, args, types, pos); ok {
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
func (e *emitter) lowerMathInline(method string, args []string, types []saType, pos *ast.Node) (string, saType, bool) {
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
	case "sqrt":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		isFloat := len(types) > 0 && types[0] == tF64
		if !isFloat && e.f64Vars[args[0]] {
			isFloat = true
		}
		if isFloat {
			e.refuse(pos, "Math.sqrt on floats is not supported (integer subset only)")
			return "0", tUnknown, true
		}
		return e.lowerMathSqrt(args[0]), tI32, true
	case "log10":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		return e.lowerMathLog10(args[0]), tI32, true
	case "random":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		return e.lowerMathRandom(), tI32, true
	}
	return "", tUnknown, false
}

// lowerMathSqrt ports the reference integer binary search: acc tracks the
// best mid with mid <= x/mid over [1, x].
func (e *emitter) lowerMathSqrt(x string) string {
	fx := x
	if !isTempName(x) && e.lookupBinding(x) != nil {
		cp := e.freshTmp()
		e.emit("%s = add %s, 0", cp, x)
		fx = cp
	}
	acc := e.freshTmp()
	e.emit("%s = 0", acc)
	lo := e.freshTmp()
	e.emit("%s = 1", lo)
	hi := e.freshTmp()
	e.emit("%s = add %s, 0", hi, fx)
	topL := e.freshLabel("sqrt_top")
	bodyL := e.freshLabel("sqrt_body")
	takeL := e.freshLabel("sqrt_take")
	skipL := e.freshLabel("sqrt_skip")
	nextL := e.freshLabel("sqrt_next")
	endL := e.freshLabel("sqrt_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = sle %s, %s", c, lo, hi)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	d := e.freshTmp()
	e.emit("%s = sub %s, %s", d, hi, lo)
	h := e.freshTmp()
	e.emit("%s = div %s, 2", h, d)
	mid := e.freshTmp()
	e.emit("%s = add %s, %s", mid, lo, h)
	q := e.freshTmp()
	e.emit("%s = div %s, %s", q, fx, mid)
	ok := e.freshTmp()
	e.emit("%s = sle %s, %s", ok, mid, q)
	e.emit("br %s -> %s, %s", ok, takeL, skipL)
	e.emitRaw("%s:", takeL)
	e.emit("%s = %s", acc, mid)
	loN := e.freshTmp()
	e.emit("%s = add %s, 1", loN, mid)
	e.emit("%s = %s", lo, loN)
	e.emit("jmp %s", nextL)
	e.emitRaw("%s:", skipL)
	hiN := e.freshTmp()
	e.emit("%s = sub %s, 1", hiN, mid)
	e.emit("%s = %s", hi, hiN)
	e.emit("jmp %s", nextL)
	e.emitRaw("%s:", nextL)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return acc
}

// lowerMathLog10 ports the reference digit-count loop (integer log10).
func (e *emitter) lowerMathLog10(x string) string {
	lacc := e.freshTmp()
	e.emit("%s = 0", lacc)
	ltmp := e.freshTmp()
	e.emit("%s = add %s, 0", ltmp, x)
	topL := e.freshLabel("l10_top")
	bodyL := e.freshLabel("l10_body")
	endL := e.freshLabel("l10_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = sge %s, 10", c, ltmp)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	q := e.freshTmp()
	e.emit("%s = div %s, 10", q, ltmp)
	e.emit("%s = %s", ltmp, q)
	a := e.freshTmp()
	e.emit("%s = add %s, 1", a, lacc)
	e.emit("%s = %s", lacc, a)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return lacc
}

// lowerMathRandom ports the reference deterministic LCG in [0, 32767]
// (documented: not cryptographic, sequences differ from Node).
func (e *emitter) lowerMathRandom() string {
	const seed = "__ts_rand_seed"
	if e.lookupBinding(seed) == nil {
		e.emit("%s = 12345", seed)
		e.declarePlain(seed)
	}
	rs := e.freshTmp()
	e.emit("%s = mul %s, 1103515245", rs, seed)
	rs2 := e.freshTmp()
	e.emit("%s = add %s, 12345", rs2, rs)
	e.emit("%s = %s", seed, rs2)
	ro := e.freshTmp()
	e.emit("%s = ashr %s, 16", ro, seed)
	out := e.freshTmp()
	e.emit("%s = and %s, 32767", out, ro)
	return out
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
	e.emit("%s = load %s + 0 as i32", tmp, saddr)
	daddr := e.freshTmp()
	e.emit("%s = add %s, %s", daddr, ndata, soff)
	e.emit("store %s + 0, %s as i32", daddr, tmp)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", copyL)
	e.emitRaw("%s:", endL)
	voff := e.freshTmp()
	e.emit("%s = mul %s, %d", voff, ln, esz)
	vaddr := e.freshTmp()
	e.emit("%s = add %s, %s", vaddr, ndata, voff)
	e.emit("store %s + 0, %s as i32", vaddr, val)
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
func (e *emitter) lowerArrayMethod(recv, method string, args []string, types []saType, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	elem := e.arrElems[recv]
	if elem == "" {
		elem = "i32"
	}
	// Fixed 4-byte slots: literals, push, checked-index and clone all
	// build/read 4-wide (nested handles truncate but round-trip); a
	// type-sized esz mis-strides those buffers (segfault). Struct field
	// layouts still use widthOf; only array slots are fixed.
	esz := 4
	switch method {
	case "push":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		return e.lowerArrayPush(recv, args[0], "i32", 4), tI32, true
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
		e.emit("%s = load %s + 0 as i32", out, addr)
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
		e.emit("%s = load %s + 0 as i32", out, data)
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
		e.emit("%s = load %s + 0 as i32", tmp, saddr)
		doff := e.freshTmp()
		e.emit("%s = mul %s, %d", doff, i, esz)
		daddr := e.freshTmp()
		e.emit("%s = add %s, %s", daddr, data, doff)
		e.emit("store %s + 0, %s as i32", daddr, tmp)
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
		nlen := e.lowerArrayPush(recv, args[0], "i32", 4)
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
		e.emit("%s = load %s + 0 as i32", tmp, saddr)
		doff := e.freshTmp()
		e.emit("%s = mul %s, %d", doff, i, esz)
		daddr := e.freshTmp()
		e.emit("%s = add %s, %s", daddr, data, doff)
		e.emit("store %s + 0, %s as i32", daddr, tmp)
		e.emit("%s = %s", i, prev)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		e.emit("store %s + 0, %s as i32", data, args[0])
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
		e.emit("store %s + 0, %s as i32", addr, args[0])
		inext := e.freshTmp()
		e.emit("%s = add %s, 1", inext, i)
		e.emit("%s = %s", i, inext)
		e.emit("jmp %s", topL)
		e.emitRaw("%s:", endL)
		return recv, tArray, true
	case "sort":
		if len(args) != 0 {
			if len(args) == 1 && args[0] == "@callback:" {
				return e.lowerSortWithCmp(recv, elem, esz, argNodes, pos)
			}
			return "", tUnknown, false
		}
		if elem != "i32" && elem != "u32" {
			e.refuse(pos, "Array.sort without a comparator only lowers for numeric arrays")
			return "0", tUnknown, false
		}
		e.lowerInsertionSort(recv)
		return recv, tArray, true
	case "indexOf":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		from := "0"
		if len(args) > 1 {
			from = args[1]
		}
		return e.lowerArrayScan(recv, elem, esz, args[0], from, false, true, pos)
	case "lastIndexOf":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		from := ""
		if len(args) > 1 {
			from = args[1]
		}
		return e.lowerArrayScan(recv, elem, esz, args[0], from, true, true, pos)
	case "includes":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		return e.lowerArrayScan(recv, elem, esz, args[0], "0", false, false, pos)
	case "reverse":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		e.lowerArrayReverse(recv, elem, esz)
		return recv, tArray, true
	case "slice":
		start := "0"
		end := ""
		if len(args) > 0 {
			start = args[0]
		}
		if len(args) > 1 {
			end = args[1]
		}
		return e.lowerArraySlice(recv, elem, esz, start, end, pos), tArray, true
	case "at":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		return e.lowerArrayAt(recv, args[0], pos)
	case "join":
		sep := ""
		if len(args) > 0 {
			sep = args[0]
		}
		return e.lowerArrayJoin(recv, elem, sep, pos), tString, true
	case "copyWithin":
		if len(args) < 1 {
			return "", tUnknown, false
		}
		start, end := "0", ""
		if len(args) > 1 {
			start = args[1]
		}
		if len(args) > 2 {
			end = args[2]
		}
		e.lowerCopyWithin(recv, elem, esz, args[0], start, end)
		return recv, tArray, true
	case "toReversed":
		if len(args) != 0 {
			return "", tUnknown, false
		}
		return e.lowerToReversed(recv, elem, esz), tArray, true
	case "toSorted":
		if len(args) == 0 {
			if elem != "i32" && elem != "u32" {
				e.refuse(pos, "Array.toSorted without a comparator only lowers for numeric arrays")
				return "0", tUnknown, false
			}
			cp := e.lowerArraySlice(recv, elem, esz, "0", "", pos)
			e.lowerInsertionSort(cp)
			return cp, tArray, true
		}
		if len(args) == 1 && args[0] == "@callback:" {
			cp := e.lowerArraySlice(recv, elem, esz, "0", "", pos)
			return e.lowerSortWithCmp(cp, elem, esz, argNodes, pos)
		}
		return "", tUnknown, false
	case "with":
		if len(args) != 2 {
			return "", tUnknown, false
		}
		return e.lowerArrayWith(recv, elem, esz, args[0], args[1], pos), tArray, true
	case "toSpliced":
		return e.lowerToSpliced(recv, elem, esz, args, pos), tArray, true
	case "concat":
		return e.lowerArrayConcat(recv, elem, esz, args, types, pos), tArray, true
	}
	// Array.from shape (static call: recv is the Array constructor).
	if recv == "Array" && method == "from" {
		return e.lowerArrayFrom(args, argNodes, pos)
	}
	return "", tUnknown, false
}

// arrayClampLen normalizes an index against len: negatives count from the
// end, results clamp to [0, len] (reference clampToLen shape).
func (e *emitter) arrayClampLen(v, ln string) string {
	adj := e.freshTmp()
	out := e.freshTmp()
	nL := e.freshLabel("cx_neg")
	nN := e.freshLabel("cx_nneg")
	nE := e.freshLabel("cx_end")
	neg := e.freshTmp()
	e.emit("%s = slt %s, 0", neg, v)
	e.emit("br %s -> %s, %s", neg, nL, nN)
	e.emitRaw("%s:", nL)
	e.emit("%s = add %s, %s", adj, ln, v)
	e.emit("jmp %s", nE)
	e.emitRaw("%s:", nN)
	e.emit("%s = add %s, 0", adj, v)
	e.emit("jmp %s", nE)
	e.emitRaw("%s:", nE)
	lo := e.freshTmp()
	loT := e.freshLabel("cx_lot")
	loF := e.freshLabel("cx_lof")
	loE := e.freshLabel("cx_loe")
	e.emit("%s = slt %s, 0", lo, adj)
	e.emit("br %s -> %s, %s", lo, loT, loF)
	e.emitRaw("%s:", loT)
	e.emit("%s = 0", out)
	e.emit("jmp %s", loE)
	e.emitRaw("%s:", loF)
	hi := e.freshTmp()
	hiT := e.freshLabel("cx_hit")
	hiF := e.freshLabel("cx_hif")
	e.emit("%s = sgt %s, %s", hi, adj, ln)
	e.emit("br %s -> %s, %s", hi, hiT, hiF)
	e.emitRaw("%s:", hiT)
	e.emit("%s = add %s, 0", out, ln)
	e.emit("jmp %s", loE)
	e.emitRaw("%s:", hiF)
	e.emit("%s = add %s, 0", out, adj)
	e.emit("jmp %s", loE)
	e.emitRaw("%s:", loE)
	return out
}

// lowerArrayScan emits equality scans: index forms return the position or
// -1, includes forms return 1/0 (reference equality-scan shape).
func (e *emitter) lowerArrayScan(recv, elem string, esz int, want, from string, reverse, wantIndex bool, pos *ast.Node) (string, saType, bool) {
	_ = pos
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	start := e.freshTmp()
	if reverse && from == "" {
		e.emit("%s = sub %s, 1", start, ln)
	} else if from == "" {
		e.emit("%s = 0", start)
	} else {
		e.emit("%s = add %s, 0", start, e.arrayClampLen(from, ln))
	}
	res := e.freshTmp()
	if wantIndex {
		e.emit("%s = -1", res)
	} else {
		e.emit("%s = 0", res)
	}
	i := e.freshTmp()
	e.emit("%s = add %s, 0", i, start)
	topL := e.freshLabel("sc_top")
	bodyL := e.freshLabel("sc_body")
	nextL := e.freshLabel("sc_next")
	endL := e.freshLabel("sc_end")
	hitL := e.freshLabel("sc_hit")
	if !reverse {
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = slt %s, %s", c, i, ln)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
	} else {
		e.emitRaw("%s:", topL)
		c := e.freshTmp()
		e.emit("%s = sge %s, 0", c, i)
		e.emit("br %s -> %s, %s", c, bodyL, endL)
	}
	e.emitRaw("%s:", bodyL)
	off := e.freshTmp()
	e.emit("%s = mul %s, %d", off, i, esz)
	addr := e.freshTmp()
	e.emit("%s = add %s, %s", addr, data, off)
	cur := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", cur, addr)
	eq := e.freshTmp()
	e.emit("%s = eq %s, %s", eq, cur, want)
	e.emit("br %s -> %s, %s", eq, hitL, nextL)
	e.emitRaw("%s:", hitL)
	if wantIndex {
		e.emit("%s = add %s, 0", res, i)
	} else {
		e.emit("%s = 1", res)
	}
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", nextL)
	step := e.freshTmp()
	if !reverse {
		e.emit("%s = add %s, 1", step, i)
	} else {
		e.emit("%s = sub %s, 1", step, i)
	}
	e.emit("%s = %s", i, step)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	if wantIndex {
		return res, tI32, true
	}
	return res, tBool, true
}

// lowerArrayReverse swaps in place (reference shape), returning the array.
func (e *emitter) lowerArrayReverse(recv, elem string, esz int) {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	half := e.freshTmp()
	e.emit("%s = div %s, 2", half, ln)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("rv_top")
	bodyL := e.freshLabel("rv_body")
	endL := e.freshLabel("rv_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, half)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	j := e.freshTmp()
	e.emit("%s = sub %s, 1", j, ln)
	j2 := e.freshTmp()
	e.emit("%s = sub %s, %s", j2, j, i)
	ao := e.freshTmp()
	e.emit("%s = mul %s, %d", ao, i, esz)
	aa := e.freshTmp()
	e.emit("%s = add %s, %s", aa, data, ao)
	bo := e.freshTmp()
	e.emit("%s = mul %s, %d", bo, j2, esz)
	ba := e.freshTmp()
	e.emit("%s = add %s, %s", ba, data, bo)
	a := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", a, aa)
	b := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", b, ba)
	e.emit("store %s + 0, %s as i32", aa, b)
	e.emit("store %s + 0, %s as i32", ba, a)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
}

// lowerArraySlice copies [start, end) into a fresh array (clamped).
func (e *emitter) lowerArraySlice(recv, elem string, esz int, start, end string, pos *ast.Node) string {
	_ = pos
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	sdata := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", sdata, recv)
	s := e.arrayClampLen(start, ln)
	f := ln
	if end != "" {
		f = e.arrayClampLen(end, ln)
	}
	n := e.freshTmp()
	e.emit("%s = sub %s, %s", n, f, s)
	// Clamp negative ranges to 0 branch-free (n * (n >= 0)): definitions
	// stay on the single path (branch arms hide names from joins).
	nneg := e.freshTmp()
	e.emit("%s = slt %s, 0", nneg, n)
	one := e.freshTmp()
	e.emit("%s = 1", one)
	keep := e.freshTmp()
	e.emit("%s = sub %s, %s", keep, one, nneg)
	nn := e.freshTmp()
	e.emit("%s = mul %s, %s", nn, n, keep)
	n = nn
	// Single-path allocation: the buffer holds (n+1) slots, the header
	// keeps the exact length, the copy loop runs zero times when empty.
	n1 := e.freshTmp()
	e.emit("%s = add %s, 1", n1, n)
	nby := e.freshTmp()
	e.emit("%s = mul %s, %d", nby, n1, esz)
	ddata := e.freshTmp()
	e.emit("%s = alloc %s", ddata, nby)
	dh := e.freshTmp()
	e.emit("%s = alloc 16", dh)
	e.emit("store %s + 0, %s as ptr", dh, ddata)
	e.emit("store %s + 8, %s as u64", dh, n)
	e.emit("!%s", ddata)
	dloop := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", dloop, dh)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("sl_top")
	bodyL := e.freshLabel("sl_body")
	cendL := e.freshLabel("sl_cend")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, n)
	e.emit("br %s -> %s, %s", c, bodyL, cendL)
	e.emitRaw("%s:", bodyL)
	si := e.freshTmp()
	e.emit("%s = add %s, %s", si, s, i)
	so := e.freshTmp()
	e.emit("%s = mul %s, %d", so, si, esz)
	sa := e.freshTmp()
	e.emit("%s = add %s, %s", sa, sdata, so)
	cv := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", cv, sa)
	do := e.freshTmp()
	e.emit("%s = mul %s, %d", do, i, esz)
	da := e.freshTmp()
	e.emit("%s = add %s, %s", da, dloop, do)
	e.emit("store %s + 0, %s as i32", da, cv)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", cendL)
	// The header owns itself (no snapshot copy: copies orphan the alloc
	// temp as a leak).
	e.declareOwned(dh)
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[dh] = true
	e.arrElems[dh] = elem
	return dh
}

// lowerArrayAt normalizes negative indices branch-free
// (sel = idx + (idx<0 ? ln : 0); arm rebinds would orphan temps), then
// uses the checked-index join (OOB yields 0, matching the subset rule).
func (e *emitter) lowerArrayAt(recv, idx string, pos *ast.Node) (string, saType, bool) {
	_ = pos
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	isneg := e.freshTmp()
	e.emit("%s = slt %s, 0", isneg, idx)
	adj := e.freshTmp()
	e.emit("%s = mul %s, %s", adj, ln, isneg)
	sel := e.freshTmp()
	e.emit("%s = add %s, %s", sel, idx, adj)
	return e.lowerCheckedIndex(recv, sel, false), tI32, true
}

// lowerArrayJoin folds elements with the separator via string concat
// (elements render through the shared sa_fmt path; string elements pass).
func (e *emitter) lowerArrayJoin(recv, elem, sep string, pos *ast.Node) string {
	_ = pos
	e.needImport("sa_std/string.sai")
	e.needImport("sa_std/fmt.sai")
	sepslice := e.lowerStringLiteral(",")
	if sep != "" {
		sepslice = sep
	}
	acc := e.lowerStringLiteral("")
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("jn_top")
	bodyL := e.freshLabel("jn_body")
	endL := e.freshLabel("jn_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	// Separator before every element except the first.
	first := e.freshTmp()
	fl := e.freshLabel("jn_fl")
	fe := e.freshLabel("jn_fe")
	e.emit("%s = ne %s, 0", first, i)
	e.emit("br %s -> %s, %s", first, fl, fe)
	e.emitRaw("%s:", fl)
	acc = e.concatSlices(acc, sepslice)
	e.emit("jmp %s", fe)
	e.emitRaw("%s:", fe)
	off := e.freshTmp()
	e.emit("%s = mul %s, 4", off, i)
	addr := e.freshTmp()
	e.emit("%s = add %s, %s", addr, data, off)
	raw := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", raw, addr)
	part := raw
	if elem != "ptr" {
		var ok bool
		part, ok = e.renderInterpValue(raw, tI32, pos)
		if !ok {
			return acc
		}
	}
	acc = e.concatSlices(acc, part)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	return acc
}

// lowerCopyWithin ports the reference algorithm: clamp, count =
// min(end-start, len-target), no-op when non-positive, overlap-safe
// direction (forward when target < start, else backward).
func (e *emitter) lowerCopyWithin(recv, elem string, esz int, target, start, end string) {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, recv)
	t := e.arrayClampLen(target, ln)
	s := e.arrayClampLen(start, ln)
	f := ln
	if end != "" {
		f = e.arrayClampLen(end, ln)
	}
	span := e.freshTmp()
	e.emit("%s = sub %s, %s", span, f, s)
	room := e.freshTmp()
	e.emit("%s = sub %s, %s", room, ln, t)
	count := e.freshTmp()
	pick := e.freshTmp()
	minS := e.freshLabel("cw_minspan")
	minR := e.freshLabel("cw_minroom")
	cntL := e.freshLabel("cw_cnt")
	e.emit("%s = slt %s, %s", pick, span, room)
	e.emit("br %s -> %s, %s", pick, minS, minR)
	e.emitRaw("%s:", minS)
	e.emit("%s = add %s, 0", count, span)
	e.emit("jmp %s", cntL)
	e.emitRaw("%s:", minR)
	e.emit("%s = add %s, 0", count, room)
	e.emit("jmp %s", cntL)
	e.emitRaw("%s:", cntL)
	goT := e.freshTmp()
	runL := e.freshLabel("cw_run")
	skipL := e.freshLabel("cw_skip")
	e.emit("%s = sgt %s, 0", goT, count)
	e.emit("br %s -> %s, %s", goT, runL, skipL)
	e.emitRaw("%s:", runL)
	fwd := e.freshTmp()
	fInit := e.freshLabel("cw_finit")
	bTop := e.freshLabel("cw_btop")
	e.emit("%s = slt %s, %s", fwd, t, s)
	e.emit("br %s -> %s, %s", fwd, fInit, bTop)
	// Forward loop.
	e.emitRaw("%s:", fInit)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	fTop := e.freshLabel("cw_ftop")
	fBody := e.freshLabel("cw_fbody")
	fEnd := e.freshLabel("cw_fend")
	e.emit("jmp %s", fTop)
	e.emitRaw("%s:", fTop)
	fc := e.freshTmp()
	e.emit("%s = slt %s, %s", fc, i, count)
	e.emit("br %s -> %s, %s", fc, fBody, fEnd)
	e.emitRaw("%s:", fBody)
	e.lowerCopyWithinStep(data, esz, elem, t, s, i)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", fTop)
	e.emitRaw("%s:", fEnd)
	e.emit("jmp %s", skipL)
	// Backward loop.
	e.emitRaw("%s:", bTop)
	j := e.freshTmp()
	e.emit("%s = sub %s, 1", j, count)
	bCond := e.freshLabel("cw_bcond")
	bBody := e.freshLabel("cw_bbody")
	bEnd := e.freshLabel("cw_bend")
	e.emit("jmp %s", bCond)
	e.emitRaw("%s:", bCond)
	bc := e.freshTmp()
	e.emit("%s = sge %s, 0", bc, j)
	e.emit("br %s -> %s, %s", bc, bBody, bEnd)
	e.emitRaw("%s:", bBody)
	e.lowerCopyWithinStep(data, esz, elem, t, s, j)
	jnext := e.freshTmp()
	e.emit("%s = sub %s, 1", jnext, j)
	e.emit("%s = %s", j, jnext)
	e.emit("jmp %s", bCond)
	e.emitRaw("%s:", bEnd)
	e.emit("jmp %s", skipL)
	e.emitRaw("%s:", skipL)
}

// lowerCopyWithinStep copies src[s+k] to dst[t+k] for one offset k.
func (e *emitter) lowerCopyWithinStep(data string, esz int, elem, t, s, k string) {
	si := e.freshTmp()
	e.emit("%s = add %s, %s", si, s, k)
	so := e.freshTmp()
	e.emit("%s = mul %s, %d", so, si, esz)
	sa := e.freshTmp()
	e.emit("%s = add %s, %s", sa, data, so)
	cur := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", cur, sa)
	di := e.freshTmp()
	e.emit("%s = add %s, %s", di, t, k)
	dof := e.freshTmp()
	e.emit("%s = mul %s, %d", dof, di, esz)
	da := e.freshTmp()
	e.emit("%s = add %s, %s", da, data, dof)
	e.emit("store %s + 0, %s as i32", da, cur)
}

// lowerToReversed copies reversed into a fresh array.
func (e *emitter) lowerToReversed(recv, elem string, esz int) string {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	sdata := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", sdata, recv)
	// Single-path: (len+1) slots always (see lowerArraySlice).
	ln1 := e.freshTmp()
	e.emit("%s = add %s, 1", ln1, ln)
	nby := e.freshTmp()
	e.emit("%s = mul %s, %d", nby, ln1, esz)
	ddata := e.freshTmp()
	e.emit("%s = alloc %s", ddata, nby)
	dest := e.freshTmp()
	e.emit("%s = alloc 16", dest)
	e.emit("store %s + 0, %s as ptr", dest, ddata)
	e.emit("store %s + 8, %s as u64", dest, ln)
	e.emit("!%s", ddata)
	dloop := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", dloop, dest)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("tr_top")
	bodyL := e.freshLabel("tr_body")
	endL := e.freshLabel("tr_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	si := e.freshTmp()
	e.emit("%s = sub %s, 1", si, ln)
	si2 := e.freshTmp()
	e.emit("%s = sub %s, %s", si2, si, i)
	so := e.freshTmp()
	e.emit("%s = mul %s, %d", so, si2, esz)
	sa := e.freshTmp()
	e.emit("%s = add %s, %s", sa, sdata, so)
	cv := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", cv, sa)
	dof := e.freshTmp()
	e.emit("%s = mul %s, %d", dof, i, esz)
	da := e.freshTmp()
	e.emit("%s = add %s, %s", da, dloop, dof)
	e.emit("store %s + 0, %s as i32", da, cv)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	e.declareOwned(dest)
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[dest] = true
	e.arrElems[dest] = elem
	return dest
}

// lowerArrayWith copies, replacing index i (negative counts from end;
// out-of-range refuses loudly like a RangeError).
func (e *emitter) lowerArrayWith(recv, elem string, esz int, idx, val string, pos *ast.Node) string {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	isneg := e.freshTmp()
	e.emit("%s = slt %s, 0", isneg, idx)
	adj := e.freshTmp()
	e.emit("%s = mul %s, %s", adj, ln, isneg)
	norm := e.freshTmp()
	e.emit("%s = add %s, %s", norm, idx, adj)
	// One copy up front; the store runs only when 0 <= norm < len
	// (RangeError has no SA-ASM edge, so out-of-range passes the copy
	// through unchanged).
	cp := e.lowerArraySlice(recv, elem, esz, "0", "", pos)
	lo := e.freshTmp()
	hi := e.freshTmp()
	e.emit("%s = slt %s, 0", lo, norm)
	e.emit("%s = sge %s, %s", hi, norm, ln)
	bad := e.freshTmp()
	e.emit("%s = or %s, %s", bad, lo, hi)
	badL := e.freshLabel("w_bad")
	okL := e.freshLabel("w_ok")
	finL := e.freshLabel("w_fin")
	e.emit("br %s -> %s, %s", bad, badL, okL)
	e.emitRaw("%s:", badL)
	e.emit("jmp %s", finL)
	e.emitRaw("%s:", okL)
	cdata := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", cdata, cp)
	off := e.freshTmp()
	e.emit("%s = mul %s, %d", off, norm, esz)
	addr := e.freshTmp()
	e.emit("%s = add %s, %s", addr, cdata, off)
	e.emit("store %s + 0, %s as i32", addr, val)
	e.emit("jmp %s", finL)
	e.emitRaw("%s:", finL)
	return cp
}

// lowerToSpliced returns a fresh array with [start, start+delete) removed
// and items inserted (JS semantics with clamped bounds).
func (e *emitter) lowerToSpliced(recv, elem string, esz int, args []string, pos *ast.Node) string {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, recv)
	sdata := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", sdata, recv)
	start := "0"
	del := ln
	items := []string{}
	if len(args) > 0 {
		start = args[0]
	}
	if len(args) > 1 {
		del = args[1]
	}
	if len(args) > 2 {
		items = args[2:]
		for _, it := range items {
			if strings.HasPrefix(it, "@spread:") || strings.HasPrefix(it, "@callback:") {
				e.refuse(pos, "toSpliced items must be plain values")
				return recv
			}
		}
	}
	s := e.arrayClampLen(start, ln)
	// Clamp delete count to [0, maxdel] branch-free (arm rebinds orphan
	// temps; see lowerArrayAt).
	maxdel := e.freshTmp()
	e.emit("%s = sub %s, %s", maxdel, ln, s)
	d := e.freshTmp()
	e.emit("%s = add %s, 0", d, del)
	neg := e.freshTmp()
	e.emit("%s = slt %s, 0", neg, d)
	keepNeg := e.freshTmp()
	e.emit("%s = sub 1, %s", keepNeg, neg)
	d0 := e.freshTmp()
	e.emit("%s = mul %s, %s", d0, d, keepNeg)
	over := e.freshTmp()
	e.emit("%s = sgt %s, %s", over, d0, maxdel)
	gap := e.freshTmp()
	e.emit("%s = sub %s, %s", gap, maxdel, d0)
	fix := e.freshTmp()
	e.emit("%s = mul %s, %s", fix, gap, over)
	d1 := e.freshTmp()
	e.emit("%s = add %s, %s", d1, d0, fix)
	d = d1
	// newlen = len - d + nitems; copy head, items, tail.
	ni := fmt.Sprintf("%d", len(items))
	kept := e.freshTmp()
	e.emit("%s = sub %s, %s", kept, ln, d)
	nlen := e.freshTmp()
	e.emit("%s = add %s, %s", nlen, kept, ni)
	return e.spliceCopy(recv, elem, esz, sdata, ln, s, d, items, nlen)
}

// spliceCopy builds the toSpliced result: head [0,s), items, tail [s+d, len).
func (e *emitter) spliceCopy(recv, elem string, esz int, sdata, ln, s, d string, items []string, nlen string) string {
	_ = recv
	nlen1 := e.freshTmp()
	e.emit("%s = add %s, 1", nlen1, nlen)
	nby := e.freshTmp()
	e.emit("%s = mul %s, %d", nby, nlen1, esz)
	ddata := e.freshTmp()
	e.emit("%s = alloc %s", ddata, nby)
	dest := e.freshTmp()
	e.emit("%s = alloc 16", dest)
	e.emit("store %s + 0, %s as ptr", dest, ddata)
	e.emit("store %s + 8, %s as u64", dest, nlen)
	e.emit("!%s", ddata)
	dloop := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", dloop, dest)
	// Head [0, s).
	e.copyRange(sdata, dloop, esz, elem, "0", s, "0")
	// Items at s.
	for k, it := range items {
		di := e.freshTmp()
		e.emit("%s = add %s, %d", di, s, k)
		dof := e.freshTmp()
		e.emit("%s = mul %s, %d", dof, di, esz)
		da := e.freshTmp()
		e.emit("%s = add %s, %s", da, dloop, dof)
		e.emit("store %s + 0, %s as i32", da, it)
	}
	// Tail [s+d, len) at s+nitems.
	ni := fmt.Sprintf("%d", len(items))
	tailStart := e.freshTmp()
	e.emit("%s = add %s, %s", tailStart, s, d)
	dstOff := e.freshTmp()
	e.emit("%s = add %s, %s", dstOff, s, ni)
	e.copyRange(sdata, dloop, esz, elem, tailStart, ln, dstOff)
	e.declareOwned(dest)
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[dest] = true
	e.arrElems[dest] = elem
	_ = ln
	return dest
}

// copyRange copies src[s0, s1) to dst[d0, d0 + (s1-s0)).
func (e *emitter) copyRange(sdata, ddata string, esz int, elem, s0, s1, d0 string) {
	i := e.freshTmp()
	e.emit("%s = add %s, 0", i, s0)
	topL := e.freshLabel("cr_top")
	bodyL := e.freshLabel("cr_body")
	endL := e.freshLabel("cr_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, s1)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	rel := e.freshTmp()
	e.emit("%s = sub %s, %s", rel, i, s0)
	so := e.freshTmp()
	e.emit("%s = mul %s, %d", so, i, esz)
	sa := e.freshTmp()
	e.emit("%s = add %s, %s", sa, sdata, so)
	cv := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", cv, sa)
	di := e.freshTmp()
	e.emit("%s = add %s, %s", di, d0, rel)
	dof := e.freshTmp()
	e.emit("%s = mul %s, %d", dof, di, esz)
	da := e.freshTmp()
	e.emit("%s = add %s, %s", da, ddata, dof)
	e.emit("store %s + 0, %s as i32", da, cv)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
}

// lowerArrayConcat returns a fresh array joining the receiver with each
// argument (arrays append element-wise, scalars push).
func (e *emitter) lowerArrayConcat(recv, elem string, esz int, args []string, types []saType, pos *ast.Node) string {
	_ = pos
	h := e.newEmptyArray()
	e.appendSlice(h, recv)
	for i, a := range args {
		isArr := false
		if i < len(types) && types[i] == tArray {
			isArr = true
		}
		if e.arrVars[a] {
			isArr = true
		}
		if strings.HasPrefix(a, "@spread:") {
			e.appendSlice(h, strings.TrimPrefix(a, "@spread:"))
			continue
		}
		if strings.HasPrefix(a, "@callback:") {
			e.refuse(pos, "callbacks are not concat values")
			continue
		}
		if isArr {
			e.appendSlice(h, a)
			continue
		}
		e.lowerArrayPush(h, a, "i32", 4)
	}
	_ = elem
	_ = esz
	return h
}

// lowerArrayFrom lowers Array.from: object literals with length allocate
// zero arrays; slices clone (mappers inline through the map path).
func (e *emitter) lowerArrayFrom(args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	if len(args) < 1 {
		return "", tUnknown, false
	}
	hasMapper := len(args) > 1 && args[1] == "@callback:"
	base, _, _ := e.lowerArrayFromBase(args, argNodes, pos)
	if e.refused {
		return "0", tUnknown, true
	}
	if !hasMapper {
		return base, tArray, true
	}
	// Mapper form: clone, then inline map over it (indices carry the
	// length-shape for {length:} inputs, matching JS mapper protocol).
	return e.lowerHigherOrder(base, "map", args, argNodes, pos)
}

// lowerArrayFromBase materializes the unmapped source array.
func (e *emitter) lowerArrayFromBase(args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	if argNodes != nil && len(argNodes.Nodes) > 0 {
		if argNodes.Nodes[0].Kind == ast.KindObjectLiteralExpression {
			n := e.freshTmp()
			e.emit("%s = 0", n)
			for _, p := range argNodes.Nodes[0].AsObjectLiteralExpression().Properties.Nodes {
				if p.Kind == ast.KindPropertyAssignment {
					pa := p.AsPropertyAssignment()
					if pa.Name().Kind == ast.KindIdentifier && pa.Name().Text() == "length" {
						v, _ := e.lowerExpr(pa.Initializer)
						e.emit("%s = %s", n, v)
					}
				}
			}
			h := e.freshTmp()
			e.emit("%s = alloc 16", h)
			n1 := e.freshTmp()
			e.emit("%s = add %s, 1", n1, n)
			nby := e.freshTmp()
			e.emit("%s = mul %s, 4", nby, n1)
			buf := e.freshTmp()
			e.emit("%s = alloc %s", buf, nby)
			e.emit("store %s + 0, %s as ptr", h, buf)
			e.emit("store %s + 8, %s as u64", h, n)
			e.emit("!%s", buf)
			e.declareOwned(h)
			if e.arrVars == nil {
				e.arrVars = map[string]bool{}
			}
			if e.arrElems == nil {
				e.arrElems = map[string]string{}
			}
			e.arrVars[h] = true
			e.arrElems[h] = "i32"
			return h, tArray, true
		}
	}
	// Slice input clones element-wise.
	h := e.newEmptyArray()
	e.appendSlice(h, args[0])
	return h, tArray, true
}

// lowerStringMethod projects string methods onto sa_std/string.sai.
func (e *emitter) lowerStringMethod(recv, method string, args []string, pos *ast.Node) (string, saType, bool) {
	e.needImport("sa_std/string.sai")
	bp, bl := e.expandSlice(recv)
	call1 := func(sym string, extra ...string) (string, saType) {
		all := append([]string{bp, bl}, extra...)
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, sym, strings.Join(all, ", "))
		e.ownTemp(t)
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
		e.ownTemp(t)
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
	case "replaceAll":
		// Same sci primitive as replace with all=1 (contract carries it).
		if len(args) != 2 {
			return "", tUnknown, false
		}
		np, nl := e.expandSlice(args[0])
		rp, rl := e.expandSlice(args[1])
		v, t := callStr("sa_string_replace", np, nl, rp, rl, "1")
		return v, t, true
	case "includes":
		// Desugars over indexOf: present iff index != -1.
		if len(args) < 1 {
			return "", tUnknown, false
		}
		np, nl := e.expandSlice(args[0])
		from := "0"
		if len(args) > 1 {
			from = args[1]
		}
		idx, _ := call1("sa_string_index_of", np, nl, from)
		out := e.freshTmp()
		e.emit("%s = ne %s, -1", out, idx)
		return out, tBool, true
	case "charAt":
		// 1-byte slice at data+index (reference lowerStringCharAt shape).
		if len(args) != 1 {
			return "", tUnknown, false
		}
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, bp, args[0])
		out := e.freshTmp()
		e.emit("%s = alloc 16", out)
		e.emit("store %s + 0, %s as ptr", out, addr)
		e.emit("store %s + 8, 1 as u64", out)
		e.declareOwned(out)
		return out, tString, true
	case "at":
		// Negative indices count from the end (then same as charAt).
		if len(args) != 1 {
			return "", tUnknown, false
		}
		// Negative indices count from the end, branch-free
		// (sel = idx + (idx<0 ? len : 0); arm rebinds orphan temps).
		isneg := e.freshTmp()
		e.emit("%s = slt %s, 0", isneg, args[0])
		adj := e.freshTmp()
		e.emit("%s = mul %s, %s", adj, bl, isneg)
		sel := e.freshTmp()
		e.emit("%s = add %s, %s", sel, args[0], adj)
		addr := e.freshTmp()
		e.emit("%s = add %s, %s", addr, bp, sel)
		out := e.freshTmp()
		e.emit("%s = alloc 16", out)
		e.emit("store %s + 0, %s as ptr", out, addr)
		e.emit("store %s + 8, 1 as u64", out)
		e.declareOwned(out)
		return out, tString, true
	case "trim", "trimStart", "trimEnd":
		// Compose the sci trim primitives (ascii subset).
		start := e.freshTmp()
		e.emit("%s = call @sa_str_trim_ascii_start_index(%s, %s)", start, bp, bl)
		e.ownTemp(start)
		full := e.freshTmp()
		e.emit("%s = call @sa_str_trim_ascii_end_len(%s, %s)", full, bp, bl)
		e.ownTemp(full)
		s, l := start, full
		if method == "trimStart" {
			rest := e.freshTmp()
			e.emit("%s = sub %s, %s", rest, bl, start)
			l = rest
		} else if method == "trimEnd" {
			s = "0"
		} else {
			rest := e.freshTmp()
			e.emit("%s = sub %s, %s", rest, full, start)
			l = rest
		}
		nptr := e.freshTmp()
		if s == "0" {
			nptr = bp
		} else {
			e.emit("%s = add %s, %s", nptr, bp, s)
		}
		out := e.freshTmp()
		e.emit("%s = alloc 16", out)
		e.emit("store %s + 0, %s as ptr", out, nptr)
		e.emit("store %s + 8, %s as u64", out, l)
		e.declareOwned(out)
		return out, tString, true
	case "concat":
		// Fold sa_string_concat pairwise (sci primitive, never simulated).
		acc := recv
		for _, a := range args {
			np, nl := e.expandSlice(a)
			abp, abl := e.expandSlice(acc)
			t := e.freshTmp()
			e.emit("%s = call @sa_string_concat(%s, %s, %s, %s)", t, abp, abl, np, nl)
			e.declareOwned(t)
			acc = t
		}
		return acc, tString, true
	case "slice", "substring", "substr":
		// Clamped sub-slice sharing the data pointer (reference shape).
		if len(args) < 1 {
			return "", tUnknown, false
		}
		end := bl
		if len(args) > 1 {
			end = args[1]
		}
		if method == "substr" {
			// substr(start, length): end = start + length.
			nend := e.freshTmp()
			e.emit("%s = add %s, %s", nend, args[0], end)
			end = nend
		}
		s, l := e.clampRange(bp, bl, args[0], end, method == "substring")
		out := e.freshTmp()
		e.emit("%s = alloc 16", out)
		e.emit("store %s + 0, %s as ptr", out, s)
		e.emit("store %s + 8, %s as u64", out, l)
		e.declareOwned(out)
		return out, tString, true
	case "split":
		// Scan with indexOf, pushing each part (i32-slot array model).
		if len(args) < 1 {
			return "", tUnknown, false
		}
		return e.lowerStringSplit(recv, bp, bl, args[0], pos), tArray, true
	case "toString":
		return recv, tString, true
	case "codePointAt":
		if len(args) != 1 {
			return "", tUnknown, false
		}
		t := e.freshTmp()
		e.emit("%s = call @sa_string_code_point_at(%s, %s, %s)", t, bp, bl, args[0])
		e.ownTemp(t)
		return t, tI32, true
	}
	return "", tUnknown, false
}

// clampRange normalizes [start, end) against len: negatives count from the
// end, values clamp to [0, len]; substring additionally swaps inverted
// bounds and maps negatives to 0 (JS semantics).
func (e *emitter) clampRange(bp, bl, start, end string, substring bool) (string, string) {
	norm := func(v string, isEnd bool) string {
		neg := e.freshTmp()
		adj := e.freshTmp()
		out := e.freshTmp()
		nL := e.freshLabel("cl_neg")
		nN := e.freshLabel("cl_nneg")
		nE := e.freshLabel("cl_end")
		e.emit("%s = slt %s, 0", neg, v)
		e.emit("br %s -> %s, %s", neg, nL, nN)
		e.emitRaw("%s:", nL)
		if substring {
			e.emit("%s = 0", adj)
		} else {
			e.emit("%s = add %s, %s", adj, bl, v)
		}
		e.emit("jmp %s", nE)
		e.emitRaw("%s:", nN)
		e.emit("%s = add %s, 0", adj, v)
		e.emit("jmp %s", nE)
		e.emitRaw("%s:", nE)
		// Clamp adj to [0, bl].
		lo := e.freshTmp()
		loT := e.freshLabel("cl_lot")
		loF := e.freshLabel("cl_lof")
		loE := e.freshLabel("cl_loe")
		e.emit("%s = slt %s, 0", lo, adj)
		e.emit("br %s -> %s, %s", lo, loT, loF)
		e.emitRaw("%s:", loT)
		e.emit("%s = 0", out)
		e.emit("jmp %s", loE)
		e.emitRaw("%s:", loF)
		hi := e.freshTmp()
		hiT := e.freshLabel("cl_hit")
		hiF := e.freshLabel("cl_hif")
		e.emit("%s = sgt %s, %s", hi, adj, bl)
		e.emit("br %s -> %s, %s", hi, hiT, hiF)
		e.emitRaw("%s:", hiT)
		e.emit("%s = add %s, 0", out, bl)
		e.emit("jmp %s", loE)
		e.emitRaw("%s:", hiF)
		e.emit("%s = add %s, 0", out, adj)
		e.emit("jmp %s", loE)
		e.emitRaw("%s:", loE)
		_ = isEnd
		return out
	}
	s := norm(start, false)
	f := norm(end, true)
	if substring {
		// Swap when s > f.
		sw := e.freshTmp()
		c := e.freshTmp()
		tL := e.freshLabel("cl_swap")
		kL := e.freshLabel("cl_keep")
		dL := e.freshLabel("cl_done")
		e.emit("%s = sgt %s, %s", c, s, f)
		e.emit("br %s -> %s, %s", c, tL, kL)
		e.emitRaw("%s:", tL)
		e.emit("%s = add %s, 0", sw, s)
		e.emit("%s = %s", s, f)
		e.emit("%s = %s", f, sw)
		e.emit("jmp %s", dL)
		e.emitRaw("%s:", kL)
		e.emit("jmp %s", dL)
		e.emitRaw("%s:", dL)
	}
	nptr := e.freshTmp()
	e.emit("%s = add %s, %s", nptr, bp, s)
	nlen := e.freshTmp()
	e.emit("%s = sub %s, %s", nlen, f, s)
	return nptr, nlen
}

// lowerStringSplit scans with indexOf and pushes each part into a fresh
// i32-slot array (same element model as array literals).
func (e *emitter) lowerStringSplit(recv, bp, bl, sepArg string, pos *ast.Node) string {
	_ = recv
	_ = pos
	sp, sl := e.expandSlice(sepArg)
	h := e.freshTmp()
	e.emit("%s = alloc 16", h)
	e.emit("store %s + 0, 0 as ptr", h)
	e.emit("store %s + 8, 0 as u64", h)
	e.declareOwned(h)
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrElems[h] = "ptr"
	start := e.freshTmp()
	e.emit("%s = 0", start)
	topL := e.freshLabel("sp_top")
	bodyL := e.freshLabel("sp_body")
	endL := e.freshLabel("sp_end")
	e.emitRaw("%s:", topL)
	idx := e.freshTmp()
	e.emit("%s = call @sa_string_index_of(%s, %s, %s, %s, %s)", idx, bp, bl, sp, sl, start)
	e.ownTemp(idx)
	found := e.freshTmp()
	e.emit("%s = ne %s, -1", found, idx)
	e.emit("br %s -> %s, %s", found, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	// part = [start, idx): wrap and push.
	pp := e.freshTmp()
	e.emit("%s = add %s, %s", pp, bp, start)
	pl := e.freshTmp()
	e.emit("%s = sub %s, %s", pl, idx, start)
	part := e.freshTmp()
	e.emit("%s = alloc 16", part)
	e.emit("store %s + 0, %s as ptr", part, pp)
	e.emit("store %s + 8, %s as u64", part, pl)
	e.declareOwned(part)
	e.lowerArrayPush(h, part, "i32", 4)
	nstart := e.freshTmp()
	e.emit("%s = add %s, %s", nstart, idx, sl)
	e.emit("%s = %s", start, nstart)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	// Tail part [start, len).
	pp2 := e.freshTmp()
	e.emit("%s = add %s, %s", pp2, bp, start)
	pl2 := e.freshTmp()
	e.emit("%s = sub %s, %s", pl2, bl, start)
	tail := e.freshTmp()
	e.emit("%s = alloc 16", tail)
	e.emit("store %s + 0, %s as ptr", tail, pp2)
	e.emit("store %s + 8, %s as u64", tail, pl2)
	e.declareOwned(tail)
	e.lowerArrayPush(h, tail, "i32", 4)
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	e.arrVars[h] = true
	return h
}

// lowerParseIntCall ports the reference lowerParseInt decimal scan: stops
// at the first non-digit, a leading `-` negates (JS semantics).
func (e *emitter) lowerParseIntCall(s string, pos *ast.Node) string {
	_ = pos
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, s)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, s)
	acc := e.freshTmp()
	e.emit("%s = 0", acc)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	neg := e.freshTmp()
	e.emit("%s = 0", neg)
	signL := e.freshLabel("pi_sign")
	topL := e.freshLabel("pi_top")
	bodyL := e.freshLabel("pi_body")
	digL := e.freshLabel("pi_digit")
	nextL := e.freshLabel("pi_next")
	endL := e.freshLabel("pi_end")
	negL := e.freshLabel("pi_neg")
	doneL := e.freshLabel("pi_done")
	nonempty := e.freshTmp()
	e.emit("%s = ne %s, 0", nonempty, ln)
	e.emit("br %s -> %s, %s", nonempty, signL, topL)
	e.emitRaw("%s:", signL)
	b0a := e.freshTmp()
	e.emit("%s = add %s, 0", b0a, data)
	b0 := e.freshTmp()
	e.emit("%s = load %s + 0 as u8", b0, b0a)
	ism := e.freshTmp()
	e.emit("%s = eq %s, 45", ism, b0)
	e.emit("br %s -> %s, %s", ism, negL, topL)
	e.emitRaw("%s:", negL)
	e.emit("%s = 1", neg)
	i1 := e.freshTmp()
	e.emit("%s = add %s, 1", i1, i)
	e.emit("%s = %s", i, i1)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	off := e.freshTmp()
	e.emit("%s = add %s, %s", off, data, i)
	b := e.freshTmp()
	e.emit("%s = load %s + 0 as u8", b, off)
	d := e.freshTmp()
	e.emit("%s = sub %s, 48", d, b)
	ok := e.freshTmp()
	e.emit("%s = sle %s, 9", ok, d)
	// d in [0,9] iff byte was a digit (sle is signed, negatives fail).
	nn := e.freshTmp()
	e.emit("%s = sge %s, 0", nn, d)
	both := e.freshTmp()
	e.emit("%s = and %s, %s", both, ok, nn)
	e.emit("br %s -> %s, %s", both, digL, endL)
	e.emitRaw("%s:", digL)
	mul := e.freshTmp()
	e.emit("%s = mul %s, 10", mul, acc)
	nacc := e.freshTmp()
	e.emit("%s = add %s, %s", nacc, mul, d)
	e.emit("%s = %s", acc, nacc)
	e.emit("jmp %s", nextL)
	e.emitRaw("%s:", nextL)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	// Apply sign.
	isn := e.freshTmp()
	e.emit("%s = ne %s, 0", isn, neg)
	negB := e.freshLabel("pi_negb")
	doneB := e.freshLabel("pi_doneb")
	e.emit("br %s -> %s, %s", isn, negB, doneL)
	e.emitRaw("%s:", negB)
	nv := e.freshTmp()
	e.emit("%s = sub 0, %s", nv, acc)
	e.emit("%s = %s", acc, nv)
	e.emit("jmp %s", doneL)
	e.emitRaw("%s:", doneB)
	e.emit("jmp %s", doneL)
	e.emitRaw("%s:", doneL)
	return acc
}

// lowerDeepClone ports sa_plugin_ts lowerDeepClone: scalars snapshot,
// slices copy element-wise, recursing into nested slices (by the static
// arrElems record; unknown temps default to scalar elements).
func (e *emitter) lowerDeepClone(src string, st saType, pos *ast.Node) string {
	return e.lowerDeepCloneInner(src, st, pos, true)
}

// lowerDeepCloneInner ports sa_plugin_ts lowerDeepClone. takeOwn marks the
// result owned for function-exit release; recursive inners pass false
// (their headers move into the outer array, and exit-releasing a
// loop-scoped temp is UnknownRegister).
func (e *emitter) lowerDeepCloneInner(src string, st saType, pos *ast.Node, takeOwn bool) string {
	_ = pos
	if st != tArray {
		cp := e.freshTmp()
		e.emit("%s = add %s, 0", cp, src)
		return cp
	}
	elemIsSlice := e.arrElems[src] == "ptr"
	esz := 4
	saElem := "i32"
	if elemIsSlice {
		saElem = "ptr"
	}
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, src)
	sdata := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", sdata, src)
	dest := e.freshTmp()
	e.emit("%s = alloc 16", dest)
	ln1 := e.freshTmp()
	e.emit("%s = add %s, 1", ln1, ln)
	nby := e.freshTmp()
	e.emit("%s = mul %s, %d", nby, ln1, esz)
	ddata := e.freshTmp()
	e.emit("%s = alloc %s", ddata, nby)
	e.emit("store %s + 0, %s as ptr", dest, ddata)
	e.emit("store %s + 8, %s as u64", dest, ln)
	e.emit("!%s", ddata)
	dloop := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", dloop, dest)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("dc_top")
	bodyL := e.freshLabel("dc_body")
	endL := e.freshLabel("dc_end")
	e.emitRaw("%s:", topL)
	c := e.freshTmp()
	e.emit("%s = slt %s, %s", c, i, ln)
	e.emit("br %s -> %s, %s", c, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	so := e.freshTmp()
	e.emit("%s = mul %s, %d", so, i, esz)
	saddr := e.freshTmp()
	e.emit("%s = add %s, %s", saddr, sdata, so)
	daddr := e.freshTmp()
	e.emit("%s = add %s, %s", daddr, dloop, so)
	if elemIsSlice {
		inner := e.freshTmp()
		e.emit("%s = load %s + 0 as i32", inner, saddr)
		inew := e.lowerDeepCloneInner(inner, tArray, pos, false)
		// Nested elements default to scalars (two-level shapes cover
		// the demo corpus; deeper nests copy the inner slice headers).
		e.emit("store %s + 0, %s as i32", daddr, inew)
	} else {
		cv := e.freshTmp()
		e.emit("%s = load %s + 0 as %s", cv, saddr, saElem)
		e.emit("store %s + 0, %s as %s", daddr, cv, saElem)
	}
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
	if takeOwn {
		e.declareOwned(dest)
	}
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[dest] = true
	if elemIsSlice {
		e.arrElems[dest] = "ptr"
	} else {
		e.arrElems[dest] = "i32"
	}
	return dest
}

// newSizedArray materializes a zeroed i32 array header of lenOp elements.
func (e *emitter) newSizedArray(lenOp string) string {
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
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[h] = true
	e.arrElems[h] = "i32"
	return h
}

// resolveSpreadCall expands `@spread:` markers for a known callee
// (mirrors sa_plugin_ts spread-call handling):
//   - rest callees pack every argument (spreads appended element-wise)
//     into one fresh slice;
//   - fixed-arity callees expand one trailing spread by indexed loads
//     (OOB yields 0 via the checked-index join).
// Calls without markers pass through untouched.
func (e *emitter) resolveSpreadCall(fname string, args []string, types []saType, pos *ast.Node) ([]string, []saType) {
	// Rest callees always pack (spread markers or plain statics alike).
	if e.funcHasRest[fname] {
		h := e.newEmptyArray()
		for i, a := range args {
			_ = types[i]
			if strings.HasPrefix(a, "@spread:") {
				e.appendSlice(h, strings.TrimPrefix(a, "@spread:"))
				continue
			}
			if strings.HasPrefix(a, "@callback:") {
				e.refuse(pos, "callbacks are not rest values")
				return args, types
			}
			e.lowerArrayPush(h, a, "i32", 4)
		}
		return []string{h}, []saType{tArray}
	}
	hasSpread := false
	for _, a := range args {
		if strings.HasPrefix(a, "@spread:") {
			hasSpread = true
			break
		}
	}
	if !hasSpread {
		return args, types
	}
	arity, ok := e.funcParams[fname]
	if !ok {
		e.refuse(pos, "spread call to %s needs a known callee arity", fname)
		return args, types
	}
	nStatic := 0
	nSpread := 0
	for _, a := range args {
		if strings.HasPrefix(a, "@spread:") {
			nSpread++
		} else {
			nStatic++
		}
	}
	if nSpread != 1 || nStatic+1 != len(args) || args[len(args)-1][:8] != "@spread:" {
		// Only a single trailing spread is supported (all demo shapes).
		e.refuse(pos, "spread call to %s supports only one trailing spread", fname)
		return args, types
	}
	if nStatic > arity {
		e.refuse(pos, "too many arguments in call to %s", fname)
		return args, types
	}
	arr := strings.TrimPrefix(args[len(args)-1], "@spread:")
	out := append([]string{}, args[:nStatic]...)
	ot := append([]saType{}, types[:nStatic]...)
	for j := nStatic; j < arity; j++ {
		idx := fmt.Sprintf("%d", j-nStatic)
		out = append(out, e.lowerCheckedIndex(arr, idx, false))
		ot = append(ot, tI32)
	}
	return out, ot
}

// newEmptyArray materializes a zero-length i32 array header.
func (e *emitter) newEmptyArray() string {
	h := e.freshTmp()
	e.emit("%s = alloc 16", h)
	e.emit("store %s + 0, 0 as ptr", h)
	e.emit("store %s + 8, 0 as u64", h)
	e.declareOwned(h)
	if e.arrVars == nil {
		e.arrVars = map[string]bool{}
	}
	if e.arrElems == nil {
		e.arrElems = map[string]string{}
	}
	e.arrVars[h] = true
	e.arrElems[h] = "i32"
	return h
}

// appendSlice copies every 4-byte element of src into dst (push loop).
func (e *emitter) appendSlice(dst, src string) {
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, src)
	data := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", data, src)
	i := e.freshTmp()
	e.emit("%s = 0", i)
	topL := e.freshLabel("ap_top")
	bodyL := e.freshLabel("ap_body")
	endL := e.freshLabel("ap_end")
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
	e.lowerArrayPush(dst, elem, "i32", 4)
	inext := e.freshTmp()
	e.emit("%s = add %s, 1", inext, i)
	e.emit("%s = %s", i, inext)
	e.emit("jmp %s", topL)
	e.emitRaw("%s:", endL)
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
// lowerConsoleLog mirrors the reference: each operand normalises to a
// text slice (renderInterpValue), operands join with a space, then a
// trailing newline (JS console.log shape).
func (e *emitter) lowerConsoleLog(args []string, types []saType, pos *ast.Node) string {
	e.needImport("sa_std/io/print.sai")
	// Operands render through sa_fmt (numbers) and sa_string (concat
	// unwraps) even with no template literal in sight.
	e.needImport("sa_std/fmt.sai")
	e.needImport("sa_std/string.sai")
	for i, a := range args {
		if i > 0 {
			e.printConstText(" ")
		}
		vt := tI32
		if i < len(types) {
			vt = types[i]
		}
		seg, ok := e.renderInterpValue(a, vt, pos)
		if !ok {
			return "0"
		}
		bp, bl := e.expandSlice(seg)
		e.emit("call @sa_print_bytes(&%s, %s)", bp, bl)
		e.releaseIfOwnedTemp(bp)
		e.releaseIfOwnedTemp(bl)
	}
	e.printConstText("\n")
	return "0"
}

// printConstText materialises static text as a slice and prints it.
func (e *emitter) printConstText(text string) {
	seg := e.lowerStringLiteral(text)
	bp, bl := e.expandSlice(seg)
	e.emit("call @sa_print_bytes(&%s, %s)", bp, bl)
	e.releaseIfOwnedTemp(bp)
	e.releaseIfOwnedTemp(bl)
}

func (e *emitter) lowerPropertyAccess(n *ast.Node) (string, saType) {
	pa := n.AsPropertyAccessExpression()
	// `a?.b` with a statically nullable base guards null (checker-driven;
	// non-nullable receivers keep the direct shape, and syntax-only
	// lowering preserves the legacy downgrade).
	if pa.QuestionDotToken != nil && e.tcx != nil && e.tcx.nullable(pa.Expression) {
		if !isPureBase(pa.Expression) {
			e.refuse(n, "?. on an effectful base is not lowerable (bind it first)")
			return "0", tUnknown
		}
		return e.lowerGuardedProperty(n)
	}
	return e.lowerPropertyAccessInner(n)
}

// isPureBase reports side-effect-free member bases (safe to lower twice
// across a guard join).
func isPureBase(n *ast.Node) bool {
	switch n.Kind {
	case ast.KindIdentifier, ast.KindThisKeyword:
		return true
	case ast.KindPropertyAccessExpression:
		pa := n.AsPropertyAccessExpression()
		return pa.QuestionDotToken == nil && isPureBase(pa.Expression)
	}
	return false
}

// lowerGuardedProperty emits the null-guarded member join: null base yields
// 0, otherwise the direct member shape runs (mirrors lowerCheckedIndex).
func (e *emitter) lowerGuardedProperty(n *ast.Node) (string, saType) {
	pa := n.AsPropertyAccessExpression()
	base, _ := e.lowerExpr(pa.Expression)
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	endL := e.freshLabel("prop_end")
	nullL := e.freshLabel("prop_null")
	loadL := e.freshLabel("prop_ok")
	isnull := e.freshTmp()
	e.emit("%s = eq %s, 0", isnull, base)
	e.emit("br %s -> %s, %s", isnull, nullL, loadL)
	e.emitRaw("%s:", nullL)
	e.emit("store %s + 0, 0 as ptr", slot)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", loadL)
	v, t := e.lowerPropertyAccessInner(n)
	e.emit("store %s + 0, %s as ptr", slot, v)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	dest := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", dest, slot)
	e.releaseIfOwnedTemp(slot)
	return dest, t
}

func (e *emitter) lowerPropertyAccessInner(n *ast.Node) (string, saType) {
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
	// s.length aliases the string slice len field — but a struct field
	// literally named length wins (member chain first when a layout
	// provides it).
	if pa.Name().Text() == "length" {
		if pa.Expression.Kind == ast.KindIdentifier {
			if l := e.layoutOfVar(pa.Expression.Text()); l != nil {
				if _, ok := l.offsets["length"]; ok {
					if v, t, ok := e.lowerMemberChain(n); ok {
						return v, t
					}
				}
			}
		}
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
		// `this.f` chains resolve to the current method receiver.
		if cur.Kind == ast.KindThisKeyword && e.thisSelf != "" {
			segs = append([]string{e.thisSelf}, segs...)
		} else {
			return "", tUnknown, false
		}
	} else {
		segs = append([]string{cur.Text()}, segs...)
	}
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
		// `this.f = v` stores resolve to the current method receiver.
		if cur.Kind == ast.KindThisKeyword && e.thisSelf != "" {
			segs = append([]string{e.thisSelf}, segs...)
		} else {
			return false
		}
	} else {
		segs = append([]string{cur.Text()}, segs...)
	}
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
		if _, ok := e.classDefs[name]; ok {
			return e.lowerNewClass(name, nw, n)
		}
		if name == "Map" {
			e.needImport("sa_std/btree_map.sa")
			t := e.freshTmp()
			e.emit("%s = call @sa_btree_map_new()", t)
			e.declareOwned(t)
			if e.mapVars == nil {
				e.mapVars = map[string]bool{}
			}
			e.mapVars[t] = true
			return t, tArray
		}
		if name == "Set" {
			e.needImport("sa_std/btree_set.sa")
			t := e.freshTmp()
			e.emit("%s = call @sa_btree_set_new()", t)
			e.declareOwned(t)
			if e.setVars == nil {
				e.setVars = map[string]bool{}
			}
			e.setVars[t] = true
			return t, tArray
		}
		if name == "Array" && nw.Arguments != nil && len(nw.Arguments.Nodes) == 1 {
			// Element buffer is length*4 bytes (i32 stride); the header
			// records the element count (mirrors the new Array tests).
			lenOp, _ := e.lowerExpr(nw.Arguments.Nodes[0])
			return e.newSizedArray(lenOp), tArray
		}
	}
	e.refuse(n, "new expressions other than new Map() / new Array(n) are not lowerable")
	return "0", tUnknown
}

func (e *emitter) lowerArrayLiteral(n *ast.Node) (string, saType) {
	al := n.AsArrayLiteralExpression()
	// Spread literals (`[...a, 3]`) build element-wise via push loops
	// (mirrors sa_plugin_ts lowerSpreadLiteral concatenation).
	hasSpread := false
	for _, el := range al.Elements.Nodes {
		if el.Kind == ast.KindSpreadElement {
			hasSpread = true
			break
		}
	}
	if hasSpread {
		h := e.newEmptyArray()
		for _, el := range al.Elements.Nodes {
			if el.Kind == ast.KindSpreadElement {
				sv, _ := e.lowerExpr(el.AsSpreadElement().Expression)
				e.appendSlice(h, sv)
				continue
			}
			v, _ := e.lowerExpr(el)
			e.lowerArrayPush(h, v, "i32", 4)
		}
		return h, tArray
	}
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

// stringContentEq compares string contents: equal lengths plus a
// zero-offset indexOf hit (address compare would lie for distinct slices
// with equal bytes). negate flips the verdict.
func (e *emitter) stringContentEq(l, r string, negate bool) string {	e.needImport("sa_std/string.sai")
	lp, ll := e.expandSlice(l)
	rp, rl := e.expandSlice(r)
	idx := e.freshTmp()
	e.emit("%s = call @sa_string_index_of(%s, %s, %s, %s, 0)", idx, lp, ll, rp, rl)
	e.ownTemp(idx)
	at0 := e.freshTmp()
	e.emit("%s = eq %s, 0", at0, idx)
	samelen := e.freshTmp()
	e.emit("%s = eq %s, %s", samelen, ll, rl)
	both := e.freshTmp()
	e.emit("%s = and %s, %s", both, at0, samelen)
	e.releaseIfOwnedTemp(idx)
	out := e.freshTmp()
	if negate {
		e.emit("%s = eq %s, 0", out, both)
	} else {
		e.emit("%s = add %s, 0", out, both)
	}
	return out
}

// lowerTypeof folds statically-known typeof queries to string slices
// (JS operator semantics for the subset kinds; anything else refuses —
// environment probes like `typeof self` name unbound globals).
func (e *emitter) lowerTypeof(n *ast.Node) (string, saType) {
	op := n.AsTypeOfExpression().Expression
	kind := ""
	if op.Kind == ast.KindIdentifier {
		name := op.Text()
		switch {
		case e.strVars[name]:
			kind = "string"
		case e.arrVars[name] || e.mapVars[name] || e.setVars[name]:
			kind = "object"
		case e.layoutOfVar(name) != nil:
			kind = "object"
		case e.f64Vars[name]:
			kind = "number"
		default:
			if _, ok := e.arrowAliases[name]; ok {
				kind = "function"
			} else if _, ok := e.funcSigs[name]; ok {
				kind = "function"
			} else if _, ok := e.constVals[name]; ok && !e.constIsStr[name] {
				kind = "number"
			} else if e.lookupBinding(name) == nil {
				e.refuse(n, "typeof unknown global %s is not lowerable", name)
				return "0", tUnknown
			} else {
				e.refuse(n, "typeof %s is not statically known", name)
				return "0", tUnknown
			}
		}
	} else {
		switch op.Kind {
		case ast.KindNumericLiteral:
			kind = "number"
		case ast.KindStringLiteral, ast.KindNoSubstitutionTemplateLiteral:
			kind = "string"
		case ast.KindTrueKeyword, ast.KindFalseKeyword:
			kind = "boolean"
		case ast.KindNullKeyword, ast.KindUndefinedKeyword:
			kind = "undefined"
		case ast.KindArrowFunction, ast.KindFunctionExpression:
			kind = "function"
		case ast.KindArrayLiteralExpression, ast.KindObjectLiteralExpression:
			kind = "object"
		default:
			e.refuse(n, "typeof on computed values is not lowerable (bind it first)")
			return "0", tUnknown
		}
	}
	return e.lowerStringLiteral(kind), tString
}

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
// classDef is one recorded class: static layout plus constructor and
// method bodies for call-site inlining (no vtables exist in SA-ASM;
// function-typed fields devirtualize per instance via instFnFields).
type classDef struct {
	name    string
	layout  *layout
	methods map[string]*ast.Node
	ctor    *ast.Node
}

// recordClass registers a class shape. Only data fields contribute layout;
// extends/implements, accessors and static blocks refuse loudly.
func (e *emitter) recordClass(st *ast.Node) {
	cd := st.AsClassDeclaration()
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	if cd.HeritageClauses != nil && len(cd.HeritageClauses.Nodes) > 0 {
		e.refuse(st, "class %s with extends/implements is not lowerable", name)
		return
	}
	def := &classDef{name: name, methods: map[string]*ast.Node{}}
	l := &layout{name: name, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}}
	off := 0
	for _, m := range cd.Members.Nodes {
		switch m.Kind {
		case ast.KindPropertyDeclaration:
			pd := m.AsPropertyDeclaration()
			fname := ""
			if m.Name() != nil && m.Name().Kind == ast.KindIdentifier {
				fname = m.Name().Text()
			} else {
				e.refuse(m, "computed field names are not lowerable")
				continue
			}
			saname := "ptr"
			if pd.Type != nil {
				saname = saNameOfType(pd.Type)
			}
			size, align := widthOf(saname)
			off = alignTo(off, align)
			l.fields = append(l.fields, fname)
			l.types[fname] = saname
			if pd.Type != nil {
				l.ftypes[fname] = rawTypeName(pd.Type)
			}
			l.offsets[fname] = off
			off += size
		case ast.KindConstructor:
			def.ctor = m
		case ast.KindMethodDeclaration:
			if m.Name() != nil && m.Name().Kind == ast.KindIdentifier {
				def.methods[m.Name().Text()] = m
			}
		case ast.KindSemicolonClassElement:
			// no-op separator
		default:
			e.refuse(m, "class member %s is not lowerable", m.Kind.String())
		}
	}
	l.size = off
	def.layout = l
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[name] = l
	if e.classDefs == nil {
		e.classDefs = map[string]*classDef{}
	}
	e.classDefs[name] = def
}

// lowerNewClass materializes `new Box(...)`: allocates the static layout,
// then interprets the constructor (`this.f = param` wirings; function-typed
// fields capture inline arrows per instance).
func (e *emitter) lowerNewClass(name string, nw *ast.NewExpression, pos *ast.Node) (string, saType) {
	def := e.classDefs[name]
	l := def.layout
	h := e.freshTmp()
	if l.size == 0 {
		e.emit("%s = alloc 4", h)
	} else {
		e.emit("%s = alloc %d", h, l.size)
	}
	e.declareOwned(h)
	if e.varLayouts == nil {
		e.varLayouts = map[string]*layout{}
	}
	e.varLayouts[h] = l
	if def.ctor != nil {
		params := def.ctor.Parameters()
		var argNodes []*ast.Node
		if nw.Arguments != nil {
			argNodes = nw.Arguments.Nodes
		}
		if len(argNodes) != len(params) {
			e.refuse(pos, "new %s takes %d arguments (%d given)", name, len(params), len(argNodes))
			return h, tArray
		}
		// Map constructor params to their argument nodes for this wirings.
		paramArg := map[string]*ast.Node{}
		paramVal := map[string]string{}
		for i, p := range params {
			pname, ok := bindingNameText(p.AsNode())
			if !ok {
				e.refuse(p.AsNode(), "destructured constructor parameters are not lowerable")
				return h, tArray
			}
			a := argNodes[i]
			paramArg[pname] = a
			if a.Kind != ast.KindArrowFunction && a.Kind != ast.KindFunctionExpression {
				v, _ := e.lowerExpr(a)
				paramVal[pname] = v
			}
		}
		body := def.ctor.Body()
		if body != nil {
			for _, s := range body.Statements() {
				if !e.wireCtorStatement(h, name, s, paramArg, paramVal, pos) {
					return h, tArray
				}
			}
		}
	}
	return h, tArray
}

// wireCtorStatement interprets one `this.f = <param>` constructor wiring.
// Reports false after refusing.
func (e *emitter) wireCtorStatement(h, className string, s *ast.Node, paramArg map[string]*ast.Node, paramVal map[string]string, pos *ast.Node) bool {
	_ = pos
	if s.Kind != ast.KindExpressionStatement {
		e.refuse(s, "constructor of %s supports only this.f = param wirings", className)
		return false
	}
	ex := s.AsExpressionStatement().Expression
	if ex.Kind != ast.KindBinaryExpression {
		e.refuse(s, "constructor of %s supports only this.f = param wirings", className)
		return false
	}
	bin := ex.AsBinaryExpression()
	if bin.OperatorToken.Kind != ast.KindEqualsToken {
		e.refuse(s, "constructor of %s supports only this.f = param wirings", className)
		return false
	}
	if bin.Left.Kind != ast.KindPropertyAccessExpression {
		e.refuse(s, "constructor of %s supports only this.f = param wirings", className)
		return false
	}
	pa := bin.Left.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindThisKeyword {
		e.refuse(s, "constructor of %s supports only this.f = param wirings", className)
		return false
	}
	field := pa.Name().Text()
	def := e.classDefs[className]
	off, ok := def.layout.offsets[field]
	if !ok {
		e.refuse(s, "field %s is not in the %s layout", field, className)
		return false
	}
	saname := def.layout.types[field]
	if bin.Right.Kind != ast.KindIdentifier {
		e.refuse(s, "constructor wiring right side must be a parameter name")
		return false
	}
	pname := bin.Right.Text()
	if anode, ok := paramArg[pname]; ok && (anode.Kind == ast.KindArrowFunction || anode.Kind == ast.KindFunctionExpression) {
		// Function-typed field captures the inline arrow per instance.
		if e.instFnFields == nil {
			e.instFnFields = map[string]map[string]*ast.Node{}
		}
		if e.instFnFields[h] == nil {
			e.instFnFields[h] = map[string]*ast.Node{}
		}
		e.instFnFields[h][field] = anode
		zero := e.freshTmp()
		e.emit("%s = 0", zero)
		e.emit("store %s + %d, %s as %s", h, off, zero, saname)
		return true
	}
	v, ok := paramVal[pname]
	if !ok {
		e.refuse(s, "constructor parameter %s has no value", pname)
		return false
	}
	e.emit("store %s + %d, %s as %s", h, off, v, saname)
	return true
}

// lowerClassMethodCall inlines `inst.method(args)`: parameters bind (generic
// params inherit the argument layout), this aliases the instance, and the
// body joins through a value slot.
func (e *emitter) lowerClassMethodCall(recv, className, method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	def := e.classDefs[className]
	mn, ok := def.methods[method]
	if !ok {
		return "", tUnknown, false
	}
	params := mn.Parameters()
	var anodeList []*ast.Node
	if argNodes != nil {
		anodeList = argNodes.Nodes
	}
	// Arity counts lowered args (arrows arrive as markers with nodes).
	nArgs := len(args)
	if len(anodeList) != len(params) || nArgs != len(params) {
		e.refuse(pos, "%s.%s takes %d arguments", className, method, len(params))
		return "0", tUnknown, true
	}
	e.pushScope()
	savedSelf := e.thisSelf
	e.thisSelf = recv
	for i, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			e.refuse(p.AsNode(), "destructured method parameters are not lowerable")
			e.thisSelf = savedSelf
			e.popScope()
			return "0", tUnknown, true
		}
		// Handles alias (never copy); scalars snapshot. Generic params
		// inherit the argument struct layout (e.g. T <- Item).
		if l := e.layoutOfVar(args[i]); l != nil {
			e.declareAlias(pname, args[i])
			if e.varLayouts == nil {
				e.varLayouts = map[string]*layout{}
			}
			e.varLayouts[pname] = l
		} else if e.arrVars[args[i]] || e.strVars[args[i]] {
			e.declareAlias(pname, args[i])
			if e.arrVars[args[i]] {
				if e.arrVars == nil {
					e.arrVars = map[string]bool{}
				}
				e.arrVars[pname] = true
				if e.arrElems == nil {
					e.arrElems = map[string]string{}
				}
				e.arrElems[pname] = e.arrElems[args[i]]
			}
			if e.strVars[args[i]] {
				if e.strVars == nil {
					e.strVars = map[string]bool{}
				}
				e.strVars[pname] = true
			}
		} else {
			// Scalars snapshot by copy (same back-edge rule as
			// bindCallbackParam: never move caller temps into params).
			e.emit("%s = add %s, 0", pname, args[i])
			e.declarePlain(pname)
		}
	}
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	e.emit("store %s + 0, 0 as ptr", slot)
	endL := e.freshLabel("m_end")
	saved := e.inlineRet
	e.inlineRet = &inlineRetState{active: true, slot: slot, end: endL, saname: "i32"}
	e.terminated = false
	body := mn.Body()
	if body != nil {
		for _, s := range body.Statements() {
			e.lowerBlockStatement(s)
			if e.refused {
				break
			}
		}
	}
	e.inlineRet = saved
	e.thisSelf = savedSelf
	e.emitRaw("%s:", endL)
	e.terminated = false
	out := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", out, slot)
	e.releaseIfOwnedTemp(slot)
	e.popScope()
	_ = anodeList
	return out, tI32, true
}

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
	if mod == "fs" || mod == "net" || mod == "path" || mod == "os" ||
		mod == "node:fs" || mod == "node:net" || mod == "node:path" || mod == "node:os" {
		// Record named imports so bare calls (readFile(...)) resolve via
		// the projection table at call sites ("node:" maps to the same
		// backend table; node-plugin symbols carry Backend: "node").
		base := strings.TrimPrefix(mod, "node:")
		e.recordNamedImports(imp, base)
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
	// Linked program: the import graph pre-resolved every local module;
	// bind imported names to qualified callees (link-time environment).
	if e.link != nil {
		if e.importEnv == nil {
			e.importEnv = map[string]string{}
		}
		if e.importRet == nil {
			e.importRet = map[string]saType{}
		}
		if e.nsImports == nil {
			e.nsImports = map[string]string{}
		}
		if e.importedNames == nil {
			e.importedNames = map[string]bool{}
		}
		res, ok := e.link.resolved[mod]
		if !ok {
			e.refuse(st, "import %s is not resolvable (bare third-party imports are Phase 3; see todo/03_npm.md)", mod)
			return
		}
		bind := func(local, remote string) {
			q := res.prefix + remote
			e.importEnv[local] = q
			if r, ok := res.rets[remote]; ok {
				e.importRet[local] = r
			} else {
				e.importRet[local] = tI32
			}
			e.importedNames[local] = true
		}
		if imp.ImportClause != nil {
			clause := imp.ImportClause.AsImportClause()
			// Default import binds the target's default export.
			if nm := clause.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				if res.defLocal == "" {
					e.refuse(st, "%s has no default export", mod)
					return
				}
				if _, ok := res.rets[res.defLocal]; !ok {
					e.refuse(st, "default export of %s is not callable", mod)
					return
				}
				q := res.prefix + res.defLocal
				local := nm.Text()
				e.importEnv[local] = q
				if r, ok := res.rets[res.defLocal]; ok {
					e.importRet[local] = r
				} else {
					e.importRet[local] = tI32
				}
				e.importedNames[local] = true
				// `import d, { x } from`: fall through to named bindings.
				if clause.NamedBindings == nil {
					return
				}
			}
			nb := clause.NamedBindings
			if nb != nil {
				if nb.Kind == ast.KindNamespaceImport {
					ns := nb.AsNamespaceImport().Name().Text()
					e.nsImports[ns] = res.key
					for exp, r := range res.rets {
						e.importEnv[ns+"."+exp] = res.prefix + exp
						e.importRet[ns+"."+exp] = r
					}
					return
				}
				for _, el := range nb.AsNamedImports().Elements.Nodes {
					if el.Kind != ast.KindImportSpecifier {
						continue
					}
					// `import { a }` / `import { b as c }`: PropertyName
					// holds the remote name when aliased.
					remote, local := "", ""
					sp := el.AsImportSpecifier()
					if sp.PropertyName != nil {
						remote = sp.PropertyName.Text()
					}
					if n := el.Name(); n != nil {
						local = n.Text()
					}
					if remote == "" {
						remote = local
					}
					if _, ok := res.exports[remote]; !ok {
						e.refuse(el, "%s is not exported by %s", remote, mod)
						continue
					}
					bind(local, remote)
				}
				return
			}
		}
		// Side-effect import: module is linked; nothing to bind.
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
func (e *emitter) emitProjCall(proj StdProjection, args []string, pos *ast.Node) (string, saType) {
	e.needImport(proj.Module)
	// Node-plugin u32-status out-param shape: allocate out slots, call,
	// panic on nonzero status (loud), then wrap outs per NodeOut.
	if proj.Backend == "node" && proj.NodeOut == "string" {
		if len(args) != 0 {
			e.refuse(pos, "%s takes no arguments", proj.TS)
			return "0", tUnknown
		}
		ps := e.freshTmp()
		ls := e.freshTmp()
		e.emit("%s = alloc 8", ps)
		e.emit("%s = alloc 8", ls)
		e.ownTemp(ps)
		e.ownTemp(ls)
		st := e.freshTmp()
		e.emit("%s = call @%s(&%s, &%s)", st, proj.Symbol, ps, ls)
		e.ownTemp(st)
		badL := e.freshLabel("node_bad")
		okL := e.freshLabel("node_ok")
		bad := e.freshTmp()
		e.emit("%s = ne %s, 0", bad, st)
		e.emit("br %s -> %s, %s", bad, badL, okL)
		e.emitRaw("%s:", badL)
		e.emit("panic")
		e.terminated = true
		e.emitRaw("%s:", okL)
		e.terminated = false
		ptr := e.freshTmp()
		e.emit("%s = load %s + 0 as ptr", ptr, ps)
		ln := e.freshTmp()
		e.emit("%s = load %s + 0 as u64", ln, ls)
		out := e.freshTmp()
		e.emit("%s = alloc 16", out)
		e.emit("store %s + 0, %s as ptr", out, ptr)
		e.emit("store %s + 8, %s as u64", out, ln)
		e.declareOwned(out)
		e.releaseIfOwnedTemp(ps)
		e.releaseIfOwnedTemp(ls)
		return out, tString
	}
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
		e.emit("!%s", sc)
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
	heap     bool   // owned: needs release/move/return
	consumed bool   // moved-from
	released bool   // `!` already emitted
	alias    string // handle alias: reads resolve to this register (no copy)
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

// declareAlias binds a name to an existing handle register without
// copying (inlined struct/array/string params; the owner still releases).
func (e *emitter) declareAlias(name, target string) {
	if len(e.scopes) == 0 {
		e.pushScope()
	}
	top := e.scopes[len(e.scopes)-1]
	if _, ok := top[name]; !ok {
		top[name] = &binding{heap: false, alias: target}
		e.owned = append(e.owned, name)
	} else {
		top[name].alias = target
		top[name].heap = false
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

// markRebound flags a name as freshly bound: the new value is live and
// needs its own future release, even if the previous value was just
// released or consumed (mirrors sa_plugin_ts markRebound).
func (e *emitter) markRebound(name string) {
	if b := e.lookupBinding(name); b != nil {
		b.released = false
		b.consumed = false
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
	// Rebinding an alias drops the alias (the name becomes a fresh value).
	if b := e.lookupBinding(dst); b != nil && b.alias != "" {
		b.alias = ""
	}
	// Reassignment drops top-level const/alias folds (normal declaration
	// applies from here on).
	delete(e.constVals, dst)
	delete(e.constIsStr, dst)
	delete(e.mathAliases, dst)
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
		e.markRebound(dst)
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
			e.markRebound(dst)
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
	// Fresh backing array: kept aliases e.owned would corrupt the
	// reverse iteration it reads from.
	kept := make([]string, 0, len(e.owned))
	for i := len(e.owned) - 1; i >= 0; i-- {
		name := e.owned[i]
		if name == except || done[name] {
			kept = append(kept, name)
			continue
		}
		done[name] = true
		if b := e.lookupBinding(name); b != nil && b.heap && !b.consumed && !b.released {
			e.emit("!%s", name)
			b.released = true
		}
		// Bindings stay in their scopes (marked released): wiping them
		// orphans outer names after early returns (045/222), which then
		// redeclare as heap in inner scopes and break back-edge states.
		kept = append(kept, name)
	}
	e.owned = kept
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

// foBindingPattern returns the loop binding pattern when the declarator
// is not a plain identifier (e.g. `for (const [a, b] of pairs)`).
func foBindingPattern(fo *ast.ForInOrOfStatement) *ast.Node {
	init := fo.Initializer
	if init != nil && init.Kind == ast.KindVariableDeclarationList {
		if decls := init.AsVariableDeclarationList().Declarations.Nodes; len(decls) > 0 {
			if nm := decls[0].Name(); nm != nil && nm.Kind != ast.KindIdentifier {
				return nm
			}
		}
	}
	return nil
}

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
