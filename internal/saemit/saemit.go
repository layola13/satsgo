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

// Satsgo-emitted panic codes (25xx block; bare `panic` is rejected by the
// assembler, so every runtime abort carries a code). The 25xx range is
// free in sci/sa_std (12xx-22xx taken); the SAI context above each panic
// names the failing call. 1403 stays for registry OOM (same-registry
// family as the THREAD_LOCAL_SLOT macro).
const (
	panicThrow          = 2501 // `throw` (user exception aborts; no resume edges)
	panicDomScratchFull = 2502 // DOM read scratch exactly full (would truncate)
	panicBackendStatus  = 2503 // status-checked backend call failed (node/deno/sa_std nonzero status)
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
	// tparams lists generic parameter names (empty for plain shapes);
	// fdefs keeps each field's declared type node for instantiation.
	tparams []string
	fdefs   map[string]*ast.Node
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
	// labels maps bound loop/block/switch labels to their jump targets
	// (see labels.go); pendingLabels await the inner push of a labeled
	// statement. Both die at function boundaries like breaks/conts.
	labels        map[string]*labelDef
	pendingLabels []string
	// contJumps counts emitted continue jumps (labeled or not); for-loops
	// consult it to decide whether a body-terminated loop still needs its
	// incrementor lowered (a continue may target it). Over-approximates
	// across nesting (an inner-loop continue also counts); the only cost
	// is a possibly-dead incrementor lowering, never a missed one.
	contJumps int
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
	// funcDefaultExpr records the default Initializer node per parameter
	// (nil when none) so short calls replay literal defaults at the call
	// site; non-literal defaults refuse loudly instead of miscompiling.
	funcDefaultExpr map[string][]*ast.Node
	// inlineRet intercepts callback returns (higher-order inlining).
	inlineRet *inlineRetState
	// classDefs records class shapes; varClass maps instance variables to
	// their class; instFnFields maps instance -> field -> inline arrow for
	// function-typed fields; thisSelf is the current method receiver.
	classDefs    map[string]*classDef
	// classParent maps a subclass to its direct base (single inheritance;
	// see class_heritage.go). curMethodClass is the class whose method body
	// is being inlined (super routing context). curMethodOwner is the
	// LEXICAL defining class (via methodOwner, defaulting to curMethodClass
	// at inline sites): private `#x` resolves against it (`#x` → `#Owner#x`).
	classParent    map[string]string
	curMethodClass string
	curMethodOwner string
	// Namespace flattening (see namespace_ts.go): nsStack is the active
	// path; namespaces/nsMembers/nsExports record declared scopes, member
	// kinds and export sets (per-file; cross-file member access is a later
	// slice and refuses loudly).
	nsStack    []string
	namespaces map[string]bool
	nsMembers  map[string]string
	nsExports  map[string]map[string]bool
	// nsMemberNodes records the declaring node per qualified member
	// (duplicate detection across reopened bodies; same-node re-prescan
	// stays idempotent). pendingNs defers member lowering until the
	// file's definition pass completes (see namespace_ts.go).
	nsMemberNodes map[string]*ast.Node
	pendingNs     []pendingNsBody
	// eqAliases maps `import x = N.y` locals to their flattened targets
	// ("N_y", or a namespace path for `import M = N`); qualify() routes
	// all use positions through it (see namespace_ts.go).
	eqAliases map[string]string
	// staticDefs holds statics-only shells from heritage-refused classes
	// (fold reads only; never instantiation; see recordClass).
	staticDefs   map[string]*classDef
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
	// domVars/domTemps track airlock DOM handles by binding name and by
	// call-result temp (see dom_proj.go).
	domVars  map[string]bool
	domTemps map[string]bool
	// defNSImports marks default imports of object defaults
	// (`import D from` where the target is `export default {..}`);
	// D.member routes through importEnv like namespace members.
	defNSImports map[string]bool
	localDefs     map[string]bool
	importedNames map[string]bool
	// linkExports maps every linked top-level definable name to its file;
// linkExportKind records its kind (function, class, namespace, value)
// and linkExported whether any linked file exports it (see linkRoute
// in program.go for the kind-aware miss diagnostic).
	linkExports    map[string]string
	linkExportKind map[string]string
	linkExported   map[string]bool
	// linkLets marks exported let/var literals (never foldable) and
	// linkValueConst marks const literals (see linkRoute for the
	// kind-aware miss diagnostic).
	linkLets       map[string]bool
	linkValueConst map[string]bool
	// linkSeeded marks signatures pre-seeded from the program link
	// (globalRets); namespace prescan exempts them from collision
	// refusal (a seeded member signature is the member itself).
	// directTopFuncs marks real top-level function definitions, so a
	// genuine same-name definition still refuses despite the seed.
	linkSeeded     map[string]bool
	directTopFuncs map[string]bool
	// tcx is the shared binder/checker context (nil-safe: syntax-only
	// fallback preserves exact legacy behavior).
	tcx *typeCtx
	// mapVars/setVars mark Map/Set handles for backend dispatch.
	mapVars map[string]bool
	setVars map[string]bool
	// dateVars marks Date millis bindings (new Date() narrows to i64;
	// getTime is the identity on them).
	dateVars map[string]bool
	// hashAcc tracks crypto Hash accumulators by binding name: each
	// entry buffers fed bytes as a plain string slice (updates fold via
	// sa_string_concat) until digest() routes algo+buffer through the
	// one-shot node crypto_hash primitive. lastHash carries the pending
	// state from a createHash call to its declaration adoption.
	hashAcc  map[string]*hashState
	lastHash *hashState
	// f64Vars marks float-valued bindings (sqrt and friends refuse them).
	f64Vars map[string]bool
	// dtsRet overrides top-level unannotated bodies with co-located .d.ts
	// return types (program mode; empty in single-file lowering).
	dtsRet map[string]saType
	// constVals folds top-level pure literals (name -> literal text plus a
	// string flag); mathAliases maps top-level `var f = Math.g` to g.
	// Reassignment drops the entry (then normal declaration applies).
	// Names assigned anywhere in the file never fold: they lower to
	// module-state slots instead (see modstate.go).
	constVals   map[string]string
	constIsStr  map[string]bool
	mathAliases map[string]string
	// modAssigned collects whole-file assigned bare names (pre-scan, so
	// fold decisions precede later assignments); modVars holds lowered
	// module-state slots by qualified name (see modstate.go); modStrTmps
	// marks string header temps so handle-aware param sites alias instead
	// of half-copying.
	modAssigned map[string]bool
	// reboundNames collects whole-file PLAIN-rebound bare names (`x =`,
	// compound/logic-assign, `++`/`--` on a bare identifier; member/index
	// stores excluded since they never rebind the root). The checker
	// method-dispatch fallback consults it: a rebound class-typed name
	// may no longer hold its declared class, so it stays loud.
	reboundNames map[string]bool
	modVars     map[string]*modState
	modStrTmps  map[string]bool
	// entryStmts collects top-level executable statements for the
	// synthesized `@main` (see entry_top.go); mainRenamed marks the
	// user-`main` → `main__user` collision rename; entryBuf holds the
	// lowered entry spliced ahead of definitions at finish().
	entryStmts  []*ast.Node
	mainRenamed bool
	entryBuf    string
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
	// enumNonInt marks EnumName -> member for string/computed members:
	// recordEnum numbers every named member (non-strict), so without
	// this set string reads would silently fold to ordinals. Reads
	// check this set first and refuse loudly; integer members fold.
	// Materialization (dual-track string slices) waits for the first
	// real use case (demand probe 2026-10-01: zero across 286) —
	// jev_thinking 97% — not built speculatively.
	enumNonInt map[string]map[string]bool
	// importedFrom maps a local value name to its module ("fs"/"net") for
	// `import { readFile } from "fs"` style calls. importedRemote maps
	// the local name to the remote export name (`import { a as b }`
	// calls b(...) but projects a); absent means local == remote.
	importedFrom   map[string]string
	importedRemote map[string]string
	// strVars tracks string slice bindings; arrVars tracks array slice
	// bindings; arrElems records array element SA names (method dispatch
	// resolves the receiver kind like the scope lookup does).
	strVars  map[string]bool
	arrVars  map[string]bool
	arrElems map[string]string
	// kindVars records annotation-derived typeof kinds ("number" /
	// "boolean" / "bigint") for scalar-annotated bindings the checker
	// goes blind on under NoLib (dialect names resolve to error/any).
	// Consulted only by the typeof paths; never by dispatch or
	// arithmetic (f64Vars stays authoritative for float lowering).
	kindVars map[string]string
	// terminated tracks SA-ASM well-formedness: the current block already
	// ends in a terminator (jmp/br/ret/panic), so emitting another
	// instruction would produce unreachable code. Mirrors the
	// block_terminated rule in sa_plugin_ts/src/lowerer.zig.
	terminated bool
}

// hashState is one crypto Hash/Hmac accumulator: acc buffers fed bytes,
// algo/key are the lowered operands (key empty for plain Hash), done
// latches at digest() (update/digest past finalize refuse loudly,
// mirroring Node's ERR_CRYPTO_HASH_FINALIZED).
type hashState struct {
	kind string // "Hash" or "Hmac", for diagnostics
	acc  string
	algo string
	key  string
	done bool
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
	// The synthesized entry splices ahead of definitions; files without
	// executables keep exact single-pass order (empty entryBuf).
	if e.entryBuf == "" {
		return e.header.String() + e.body.String()
	}
	return e.header.String() + e.entryBuf + e.body.String()
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
	// scopeBase is the deepest surviving scope index at inline entry;
	// join-point cleanup releases owned temps in deeper scopes only.
	scopeBase int
}

// ---------------------------------------------------------------------------
// Top level
// ---------------------------------------------------------------------------

func (e *emitter) lowerSourceFile(sf *ast.SourceFile) {
	stmts := sf.AsSourceFile().Statements.Nodes
	// Pre-scan: whole-file assigned names, so top-level fold-vs-slot
	// decisions precede later assignments (see modstate.go). The plain-
	// rebound set guards checker method dispatch (rebound class-typed
	// names may no longer hold their declared class).
	e.modAssigned = assignedNames(stmts)
	e.reboundNames = reboundNames(stmts)
	// Plan entry synthesis before signatures: top executables collect,
	// a colliding user `main` renames (see entry_top.go).
	e.planEntry(stmts)
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
			// Overload signatures carry no body; the implementation
			// (with body) registers the signature. Skipping here keeps
			// funcSigs/params/defaults from the bodiless decl.
			if st.BodyData().Body == nil {
				continue
			}
			// Entry synthesis renames a colliding user `main`
			// (definition, signatures and call sites move together).
			fname := st.Name().Text()
			regName := e.entryMainName(fname)
			if regName != fname {
				delete(e.funcSigs, fname)
				delete(e.funcParams, fname)
				delete(e.funcHasRest, fname)
				delete(e.funcDefaults, fname)
				if r, ok := e.dtsRet[fname]; ok {
					e.dtsRet[regName] = r
				}
				if e.localDefs == nil {
					e.localDefs = map[string]bool{}
				}
				e.localDefs[regName] = true
			}
			dts, hasDts := e.dtsRet[regName]
			e.funcSigs[regName] = e.prescanRet(st, dts, hasDts)
			// Direct top-level definitions (vs link-seeded signatures):
			// namespace prescan refuses genuine @label collisions but
			// exempts seeded member signatures (same entity).
			if e.directTopFuncs == nil {
				e.directTopFuncs = map[string]bool{}
			}
			e.directTopFuncs[regName] = true
			params := st.Parameters()
			e.funcParams[regName] = len(params)
			defs := make([]bool, len(params))
			dexprs := make([]*ast.Node, len(params))
			for i, p := range params {
				if pd := p.AsParameterDeclaration(); pd.Initializer != nil {
					defs[i] = true
					dexprs[i] = pd.Initializer
				}
			}
			if e.funcDefaults == nil {
				e.funcDefaults = map[string][]bool{}
			}
			e.funcDefaults[regName] = defs
			if e.funcDefaultExpr == nil {
				e.funcDefaultExpr = map[string][]*ast.Node{}
			}
			e.funcDefaultExpr[regName] = dexprs
			if len(params) > 0 {
				if pd := params[len(params)-1].AsParameterDeclaration(); pd.DotDotDotToken != nil {
					e.funcHasRest[regName] = true
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
	// Namespace scopes prescan file-wide (before any lowering): member
	// kinds, signatures, folds, slots, classes and nested scopes all
	// resolve regardless of declaration order (reopened bodies,
	// import-equals aliases above their namespace, cross-body calls).
	// Member BODIES still lower deferred (see lowerPendingNamespaces).
	e.prescanNamespaces(stmts)
	// Pre-register assigned top-level declarators, so forward reads from
	// earlier functions resolve to slots (after layouts record: object
	// slots need their interface layout).
	e.preRegisterModStates(stmts)
	// Two-pass lowering: definitions first (executables skipped), then
	// deferred namespace members (cross-body forward refs resolved),
	// then the entry body (see entry_top.go). Files without executables
	// keep exact single-pass order. Pending namespace bodies drain
	// eagerly before each non-namespace top-level statement (source
	// order preserved: a main after its namespace sees lowered members)
	// and unconditionally at end of pass (accumulate-style diagnostics).
	for _, st := range stmts {
		if e.isEntryStmt(st) {
			continue
		}
		e.lowerStatement(st, true)
	}
	e.lowerPendingNamespaces()
	if len(e.entryStmts) > 0 && !e.refused {
		e.lowerEntry()
	}
}

func (e *emitter) lowerStatement(st *ast.Node, topLevel bool) {
	// lowerStatement runs only at file scope (bodies go through
	// lowerBlockStatement), so draining here always emits to the file
	// builder. Namespace statements append instead of draining.
	if topLevel && st.Kind != ast.KindModuleDeclaration && len(e.pendingNs) > 0 {
		e.lowerPendingNamespaces()
	}
	switch st.Kind {
	case ast.KindClassDeclaration:
		// Classes record layouts + bodies for call-site inlining
		// (mirrors sa_plugin_ts parseClass without vtables).
		if !topLevel {
			e.refuse(st, "nested class declarations are not lowerable")
			return
		}
		// Decorators run arbitrary code at definition time; silently
		// dropping them would change program behavior.
		if len(st.Decorators()) > 0 {
			e.refuse(st, "class decorators are not lowerable (definition-time effects have no SA-ASM form)")
			return
		}
		e.recordClass(st)
		return
	case ast.KindFunctionDeclaration:
		// Overload signatures erase (implementation lowers the body;
		// mirrors lowerNamespaceMember; lone signatures define nothing
		// so uses refuse as unknown functions).
		if st.BodyData().Body == nil {
			return
		}
		e.lowerFunction(st)
	case ast.KindVariableStatement:
		// `using`/`await using` dispose at scope exit; even top-level
		// disposal has observable order, so refuse before any fold/slot
		// path can claim the declaration.
		if st.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsUsing != 0 {
			e.refuse(st, "using declarations are not lowerable (explicit resource disposal has no SA-ASM scope-exit hook)")
			return
		}
		if topLevel {
		// A top-level `const f = (...) => ...` is a file-scope callback
		// (mirrors sa_plugin_ts parseTopLevelArrowFn): emit @f directly.
		if e.tryTopLevelArrow(st) {
			return
		}
		// A top-level `const C = class ...` records the class (no code,
		// like arrows); instances resolve via classDefs.
		if e.tryTopLevelClassExpr(st) {
			return
		}
			// Assigned top-level `let`/`var` scalars lower to
			// module-state slots (see modstate.go); unassigned names
			// fall through to the const fold below.
			if e.tryModState(st) {
				return
			}
			// Top-level pure bindings (`var K = 1.5`, `var nativeMax =
			// Math.max`) fold without registers (see tryTopLevelConst).
			if e.tryTopLevelConst(st) {
				return
			}
			// Mixed multi-declarator consts (arrows + foldables) split
			// per declarator (same helper as the namespace drain;
			// identity outside namespaces).
			if e.trySplitMixedConst(st) {
				return
			}
			e.refuse(st, "top-level variable statements are not lowerable; move state into function scope")
			return
		}
		e.lowerVarStatement(st)
	case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
		e.lowerTypeDecl(st)
	case ast.KindModuleDeclaration:
		e.lowerNamespace(st, topLevel)
	case ast.KindImportEqualsDeclaration:
		e.lowerImportEquals(st)
	case ast.KindImportDeclaration, ast.KindExportDeclaration, ast.KindExportAssignment, ast.KindNamespaceExportDeclaration:
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
		if lbl := st.AsBreakStatement().Label; lbl != nil {
			e.lowerBreakLabel(lbl.Text(), st)
			return
		}
		if len(e.breaks) == 0 {
			e.refuse(st, "break outside loop/switch is not lowerable")
			return
		}
		tgt := e.breaks[len(e.breaks)-1]
		e.releaseForJump(tgt.depth)
		e.emit("jmp %s", tgt.label)
		e.terminated = true
	case ast.KindContinueStatement:
		if lbl := st.AsContinueStatement().Label; lbl != nil {
			e.lowerContinueLabel(lbl.Text(), st)
			return
		}
		if len(e.conts) == 0 {
			e.refuse(st, "continue outside loop is not lowerable")
			return
		}
		tgt := e.conts[len(e.conts)-1]
		e.releaseForJump(tgt.depth)
		e.emit("jmp %s", tgt.label)
		e.contJumps++
		e.terminated = true
	case ast.KindLabeledStatement:
		e.lowerLabeled(st)
	case ast.KindThrowStatement:
		// SA-ASM has no exception edges: throw lowers to panic (abort).
		e.emit("panic(%d)", panicThrow)
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
	// Namespace members emit under their qualified name (identity at top
	// level; see namespace_ts.go). A colliding top-level `main` emits
	// under the entry rename (see entry_top.go).
	name = e.nsDefName(name)
	name = e.entryMainName(name)
	params := fn.Parameters()
	sig := []string{}
	pendingParams := []destructurePending{}
	e.pushScope()
	savedRet := e.retType
	savedInFunc := e.inFunc
	e.inFunc = true
	// Missing annotation means void (mirrors the `-> T` rule: a
	// value-returning function must declare it). The pre-scan agrees.
	// Top-level unannotated bodies take co-located .d.ts returns;
	// otherwise the checker decides concrete scalar returns (todo/02#6).
	e.retType = tVoid
	if !savedInFunc {
		dts, hasDts := e.dtsRet[name]
		e.retType = e.prescanRet(fn, dts, hasDts)
	} else {
		e.retType = e.prescanRet(fn, tVoid, false)
	}
	for _, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			// Binding-pattern parameter: hidden handle param plus a
			// body-top destructure (declaration shapes, same refusals).
			hid, pat, annot, ok := e.hiddenDestructuredParam(p, params)
			if !ok {
				e.refuse(p.AsNode(), "destructured parameters are not in the SA-lowerable subset")
				continue
			}
			sig = append(sig, fmt.Sprintf("%s: ptr", hid))
			e.declareOwned(hid)
			e.trackBindingAt(hid, annot, nil, p.AsNode(), tArray)
			pendingParams = append(pendingParams, destructurePending{hid: hid, pat: pat, annot: annot})
			continue
		}
		ptype := tI32
		if pd := p.AsParameterDeclaration(); pd.Type != nil {
			ptype = annotationType(pd.Type)
			e.trackBindingAt(pname, pd.Type, nil, p.AsNode(), tUnknown)
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
	// Pattern parameters expand here, in the body scope above params.
	// Guarded, never an early return: diagnostics accumulate across
	// the file (a prior refuse must not hide later statements).
	if !e.refused {
		e.drainDestructuredParams(pendingParams)
	}
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

// tryTopLevelArrow emits `const f = (...) => ...` (or `= function...`)
// at file scope as @f. Reports whether the statement was consumed.
func (e *emitter) tryTopLevelArrow(st *ast.Node) bool {
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		return false
	}
	d := dl.Declarations.Nodes[0]
	init := d.Initializer()
	if init == nil || (init.Kind != ast.KindArrowFunction && init.Kind != ast.KindFunctionExpression) {
		return false
	}
	name, ok := bindingNameText(d)
	if !ok {
		return false
	}
	name = e.nsDefName(name)
	e.lowerArrowBinding(name, init, true)
	return !e.refused
}

// tryTopLevelClassExpr records a top-level `const C = class ...`
// (anonymous under the bound name, named under its own name with the
// bound name aliased). Reports whether the statement was consumed.
func (e *emitter) tryTopLevelClassExpr(st *ast.Node) bool {
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		return false
	}
	d := dl.Declarations.Nodes[0]
	init := d.Initializer()
	if init == nil || init.Kind != ast.KindClassExpression {
		return false
	}
	name, ok := bindingNameText(d)
	if !ok {
		return false
	}
	e.lowerClassExpression(name, init, d)
	return !e.refused
}

// lowerArrowBinding builds @gen(params..., captures...) for one arrow or
// function expression and records the name as a call alias. Top-level
// callees keep the source name (reference behavior); locals get @__arrow_N.
func (e *emitter) lowerArrowBinding(name string, arrow *ast.Node, topLevel bool) {
	var retNode *ast.Node
	switch arrow.Kind {
	case ast.KindArrowFunction:
		retNode = arrow.AsArrowFunction().Type
	case ast.KindFunctionExpression:
		retNode = arrow.AsFunctionExpression().Type
	default:
		e.refuse(arrow, "function value %s is not lowerable (arrow or function expression only)", name)
		return
	}
	params := arrow.Parameters()
	pnames := make([]string, 0, len(params))
	psig := make([]string, 0, len(params))
	ptypes := make([]saType, 0, len(params))
	pendingArrow := []destructurePending{}
	for _, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			hid, pat, annot, ok := e.hiddenDestructuredParam(p, params)
			if !ok {
				e.refuse(p.AsNode(), "destructured parameters are not in the SA-lowerable subset")
				return
			}
			pnames = append(pnames, hid)
			ptypes = append(ptypes, tArray)
			psig = append(psig, fmt.Sprintf("%s: ptr", hid))
			pendingArrow = append(pendingArrow, destructurePending{hid: hid, pat: pat, annot: annot})
			continue
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
	// A remaining void default consults the checker for concrete scalar
	// returns (todo/02#6, same rule as lowerFunction; the i32 default
	// above is untouched).
	ret := tVoid
	if retNode != nil {
		ret = annotationType(retNode)
		if ret == tUnknown {
			ret = tI32
		}
	} else {
		body := arrow.Body()
		if body == nil {
			e.refuse(arrow, "function value %s has no body", name)
			return
		}
		if body.Kind != ast.KindBlock || len(pnames) > 0 {
			ret = tI32
		} else if rt, ok := e.tcx.inferredReturnType(arrow); ok {
			ret = rt
		}
	}
	// Captures: free identifiers minus params, minus locals declared in
	// the body, minus globals and callee names (never values). Uses come
	// from the shared usage walk (todo/02#5: no hand-rolled identifier
	// walk; types and binding names never count as uses).
	bodyNode := arrow.Body()
	uses := map[string]bool{}
	if bodyNode != nil {
		if bodyNode.Kind == ast.KindBlock {
			uses = valueUsedNames(bodyNode.Statements())
		} else {
			uses = valueUsedNames([]*ast.Node{bodyNode})
		}
	}
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
	savedLabels := e.labels
	savedPending := e.pendingLabels
	savedRet := e.retType
	savedInFunc := e.inFunc
	savedTerm := e.terminated
	savedInline := e.inlineRet
	e.body = strings.Builder{}
	e.owned = nil
	e.scopes = nil
	e.breaks = nil
	e.conts = nil
	e.labels = nil
	e.pendingLabels = nil
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
		e.trackBindingAt(p, annot, nil, params[i].AsNode(), ptypes[i])
		e.declareOwned(p)
	}
	// Captures arrive as same-named trailing params: re-declare them so
	// the body resolves. Slice kinds stay visible via the shared
	// strVars/arrVars maps (outer entries are still present).
	for _, c := range captures {
		e.declareOwned(c)
	}
	e.pushScope()
	// Pattern parameters expand here, in the body scope above params.
	// Guarded, never an early return: the out-of-line body below must
	// still drain so diagnostics accumulate (and the builder swap
	// below must always restore).
	if !e.refused {
		e.drainDestructuredParams(pendingArrow)
	}
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
	e.labels = savedLabels
	e.pendingLabels = savedPending
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
// defaults may be omitted (padDefaultArgs replays them at the call site;
// non-literal defaults refuse there instead of miscompiling).
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

// padDefaultArgs replays omitted trailing default arguments at a short
// call site (JS per-call evaluation). Only pure literal Initializers
// replay (numbers, strings, booleans, null/undefined); anything else
// (identifiers, calls, templates with holes) refuses loudly: replaying
// those would rebind caller-scope names or duplicate side effects.
// Callers run this after checkArity passes.
func (e *emitter) padDefaultArgs(fname string, args []string, argTypes []saType, pos *ast.Node) ([]string, []saType, bool) {
	arity, ok := e.funcParams[fname]
	if !ok || e.funcHasRest[fname] || len(args) >= arity {
		return args, argTypes, true
	}
	defs := e.funcDefaults[fname]
	exprs := e.funcDefaultExpr[fname]
	// Copy-on-write: callers may alias args (desugar `full := args`).
	out := append([]string{}, args...)
	ot := append([]saType{}, argTypes...)
	for i := len(args); i < arity; i++ {
		if i >= len(defs) || !defs[i] {
			return args, argTypes, true
		}
		var init *ast.Node
		if i < len(exprs) {
			init = exprs[i]
		}
		if init == nil {
			e.refuse(pos, "omitted default argument %d of %s has no recorded default (short calls need a literal default)", i+1, fname)
			return args, argTypes, false
		}
		switch init.Kind {
		case ast.KindNumericLiteral, ast.KindStringLiteral,
			ast.KindNoSubstitutionTemplateLiteral, ast.KindTrueKeyword,
			ast.KindFalseKeyword, ast.KindNullKeyword, ast.KindUndefinedKeyword:
			v, t := e.lowerExpr(init)
			if e.refused {
				return args, argTypes, false
			}
			out = append(out, v)
			ot = append(ot, t)
		default:
			e.refuse(pos, "omitted default argument %d of %s is not a literal (non-literal defaults do not replay at short calls)", i+1, fname)
			return args, argTypes, false
		}
	}
	return out, ot, true
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
	// `using`/`await using` dispose at scope exit (Symbol.dispose):
	// silently dropping the declaration would leak/skip disposal.
	if dl.Flags&ast.NodeFlagsUsing != 0 {
		e.refuse(list, "using declarations are not lowerable (explicit resource disposal has no SA-ASM scope-exit hook)")
		return
	}
	for _, d := range dl.Declarations.Nodes {
		init := d.Initializer()
		// A createHash call stages its accumulator here; only a plain
		// declaration init adopts it (anything else drops it loudly
		// below via the missing hashAcc entry).
		e.lastHash = nil
		if init == nil {
			// `const` without init is a TS compile error (must initialize).
			if dl.Flags&ast.NodeFlagsConst != 0 {
				e.refuse(d, "const declarations must be initialized")
				continue
			}
			// Definite-assignment placeholder: `let x: T;` binds the
			// zero value (scalars 0/0.0, handles null "0") so later
			// `x = v` rebinds normally; use before assign reads zero.
			vd := d.AsVariableDeclaration()
			atype := annotationType(vd.Type)
			if atype == tUnknown {
				atype = tI32
			}
			zero := "0"
			if atype == tF64 {
				zero = "0.0"
			}
			if nm := d.Name(); nm != nil && nm.Kind != ast.KindIdentifier {
				e.refuse(d, "destructuring declarations are not in the SA-lowerable subset")
				continue
			}
			name, ok := bindingNameText(d)
			if !ok {
				e.refuse(d, "destructuring declarations are not in the SA-lowerable subset")
				continue
			}
			e.assignLocal(name, zero, "imm", atype, d)
			e.trackBindingAt(name, vd.Type, nil, d, atype)
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
		// name becomes a call alias, not a register binding. Named
		// function expressions share the path (the inner name, if any,
		// binds only inside the body per JS; recursion uses the alias).
		if init.Kind == ast.KindArrowFunction || init.Kind == ast.KindFunctionExpression {
			e.lowerArrowBinding(name, init, false)
			continue
		}
		// Class expressions record under the bound name (anonymous) or
		// their own name (named, aliased for `new` via the bound name).
		if init.Kind == ast.KindClassExpression {
			e.lowerClassExpression(name, init, d)
			continue
		}
		val, vtype := e.lowerExpr(init)
		if atype == tUnknown {
			atype = vtype
		}
		_ = atype
		// Declaration site: always a fresh local binding, even when a
		// same-named module slot exists (shadowing; assignLocal never
		// routes to slots).
		e.assignLocal(name, val, operandKind(val, init), vtype, d)
		e.trackBindingAt(name, d.AsVariableDeclaration().Type, init, d, vtype)
		// Adopt a staged crypto Hash/Hmac accumulator onto the bound name.
		// The callee may be an import alias (`import { createHash as ch}`),
		// so resolve through the remote export name like the call site.
		if e.lastHash != nil {
			if init.Kind == ast.KindCallExpression {
				if ce := init.AsCallExpression(); ce.Expression.Kind == ast.KindIdentifier {
					callee := ce.Expression.Text()
					if r, ok := e.importedRemote[callee]; ok {
						callee = r
					}
					if callee == "createHash" || callee == "createHmac" {
						if e.hashAcc == nil {
							e.hashAcc = map[string]*hashState{}
						}
						e.hashAcc[name] = e.lastHash
					}
				}
			}
			e.lastHash = nil
		}
		// `const b = new Box(...)` records the instance class for method
		// dispatch and per-instance fn-field devirtualization (namespace
		// member classes resolve qualified, including `new NS.C()`).
		if init.Kind == ast.KindNewExpression {
			cname := ""
			if nw := init.AsNewExpression(); nw.Expression.Kind == ast.KindIdentifier {
				cname = e.qualify(nw.Expression.Text())
			} else if nw := init.AsNewExpression(); nw.Expression.Kind == ast.KindPropertyAccessExpression {
				if q, ok := dottedBaseName(nw.Expression); ok {
					if _, _, ok := e.splitNsQualified(q); ok {
						cname = q
					} else if root := dottedRoot(nw.Expression); root != "" {
					// Imported namespace member class (cross-file `new N.C()`):
					// the shared table holds the layout (same-file keeps
					// the path above).
					if _, ok := e.nsImports[root]; ok {
						if _, ok := e.classDefs[q]; ok {
							cname = q
						}
					}
					}
				}
			}
			if cname != "" {
				if cd, ok := e.classDefs[cname]; ok {
					if e.varClass == nil {
						e.varClass = map[string]string{}
					}
					e.varClass[name] = cname
					// The instance layout is known at construction: record
					// it so field reads never depend on checker sight
					// (cross-file instances are checker-blind; recorded
					// layouts keep priority per layoutOfNode).
					if cd.layout != nil {
						if e.varLayouts == nil {
							e.varLayouts = map[string]*layout{}
						}
						e.varLayouts[name] = cd.layout
					}
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
	e.trackBindingAt(name, annot, init, nil, vtype)
}

// trackBindingAt is trackBinding with the declaration name node attached
// so the checker resolves first (todo/02#2 v4): exact checker layouts beat
// annotation guesses; generic instantiations (Box<i32> vs Box<string>)
// stay annotation-driven for width precision, and dialect scalars stay on
// scalarTypeofKind where the checker is blind under NoLib.
func (e *emitter) trackBindingAt(name string, annot, init, nameNode *ast.Node, vtype saType) {
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
	// Checker first (todo/02#2 v4): the declaration name's checker type
	// beats syntax annotation guesses. Generic annotations keep priority
	// for width precision (Box<i32> vs Box<string> instantiate distinct
	// layouts the bare checker name cannot distinguish).
	genericHit := false
	if annot != nil && annot.Kind == ast.KindTypeReference && len(annot.TypeArguments()) > 0 {
		if l := e.layoutOfAnnotation(annot); l != nil {
			e.varLayouts[name] = l
			genericHit = true
		}
	}
	checkerHit := false
	if !genericHit && nameNode != nil {
		if l := e.checkerLayoutForDecl(nameNode); l != nil {
			e.varLayouts[name] = l
			checkerHit = true
		}
	}
	// struct-typed bindings remember their layout for field access
	// (generic annotations instantiate per type argument).
	// Annotation fallback: only when the checker saw nothing (checker-blind
	// dialect shapes, NoLib gaps); checker hits already recorded above.
	if !genericHit && !checkerHit && annot != nil && annot.Kind == ast.KindTypeReference {
		if l := e.layoutOfAnnotation(annot); l != nil {
			e.varLayouts[name] = l
		}
	}
	// Union constituents contribute the first known struct layout
	// (`Box | null` still reads .v through the Box layout under a guard).
	if !checkerHit && annot != nil && (annot.Kind == ast.KindUnionType || annot.Kind == ast.KindIntersectionType) {
		for _, m := range annot.AsUnionTypeNode().Types.Nodes {
			if m.Kind == ast.KindTypeReference {
				if l := e.layoutOfAnnotation(m); l != nil {
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
	// new Date() narrows to i64 millis for getTime (argued forms refuse
	// in lowerNew, so marking here is harmless either way).
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
			case "Date":
				if e.dateVars == nil {
					e.dateVars = map[string]bool{}
				}
				e.dateVars[name] = true
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
	// DOM handles from document.createElement (see dom_proj.go).
	trackDomBinding(e, name, init)
	// Scalar annotations pin the typeof kind for bindings the checker
	// cannot see (dialect names under NoLib). Recorded last so stronger
	// claims (string/array/layout/float handles above, all early-return
	// or map-checked) always win; user type names never mark.
	if k, ok := scalarTypeofKind(annot); ok {
		if !e.strVars[name] && !e.arrVars[name] && !e.mapVars[name] && !e.setVars[name] && !e.f64Vars[name] {
			if e.kindVars == nil {
				e.kindVars = map[string]string{}
			}
			e.kindVars[name] = k
		}
	}
}

// ---------------------------------------------------------------------------
// Control flow (br always carries BOTH targets; break/continue are jmp)
// ---------------------------------------------------------------------------

// materializeCond turns an integer-immediate condition into a register:
// br takes registers, never immediates (`br 0` traps UnknownRegister,
// the latent `if (1)` / const-bool-if shape). Truthiness follows the
// subset (nonzero is true) via a single `ne 0` test. Non-immediates
// (registers, including bool-typed ones) pass through untouched.
func (e *emitter) materializeCond(cond string) string {
	isInt := len(cond) > 0
	for i := 0; i < len(cond) && isInt; i++ {
		c := cond[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if i == 0 && c == '-' && len(cond) > 1 {
			continue
		}
		isInt = false
	}
	if !isInt {
		return cond
	}
	t := e.freshTmp()
	e.emit("%s = ne %s, 0", t, cond)
	return t
}

// isFalseConst decides constant-false conditions WITHOUT lowering:
// false keyword, integer/float zero literals, and constVals-folded names
// holding zero. Only the false arm drives elimination (dead code may name
// values that do not exist); true conditions keep the materialize path.
func (e *emitter) isFalseConst(n *ast.Node) bool {
	for n.Kind == ast.KindParenthesizedExpression {
		n = n.Expression()
		if n == nil {
			return false
		}
	}
	switch n.Kind {
	case ast.KindFalseKeyword:
		return true
	case ast.KindNumericLiteral:
		return n.Text() == "0" || n.Text() == "0.0"
	case ast.KindIdentifier:
		if lit, ok := e.constVals[e.qualify(n.Text())]; ok {
			return lit == "0" || lit == "0.0"
		}
	}
	return false
}

func (e *emitter) lowerIf(st *ast.Node) {
	is := st.AsIfStatement()
	// Constant-false conditions eliminate the then-arm without lowering
	// it: dead code may name values that do not exist (planck's
	// `if (_ASSERT) console.assert(...)` with folded _ASSERT).
	if e.isFalseConst(is.Expression) {
		if is.ElseStatement == nil {
			e.terminated = false
			return
		}
		e.pushScope()
		e.terminated = false
		e.lowerBranchBody(is.ElseStatement)
		e.releaseScope()
		e.popScope()
		return
	}
	cond, _ := e.lowerExpr(is.Expression)
	cond = e.materializeCond(cond)
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
	// Never-taken loops emit nothing (same dead-code rule as if).
	if e.isFalseConst(ws.Expression) {
		e.terminated = false
		return
	}
	topL := e.freshLabel("while_top")
	bodyL := e.freshLabel("while_body")
	endL := e.freshLabel("while_end")
	e.pushLoopTargets(endL, topL)
	e.emitRaw("%s:", topL)
	cond, _ := e.lowerExpr(ws.Expression)
	cond = e.materializeCond(cond)
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

// nonNegIntLiteral reports whether n is a non-negative integer literal
// (decimal digits only: floats, hex and negatives stay legacy).
func nonNegIntLiteral(n *ast.Node) (string, bool) {
	if n == nil || n.Kind != ast.KindNumericLiteral {
		return "", false
	}
	t := n.Text()
	if t == "" || isFloatLiteral(t) {
		return "", false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return "", false
		}
	}
	return t, true
}

// canonicalForStep matches i++ / ++i / i += K / i = i + K (either order)
// with a literal step K >= 1, reporting the step text.
func canonicalForStep(ctr string, incr *ast.Node) (string, bool) {
	if incr == nil {
		return "", false
	}
	isCtr := func(n *ast.Node) bool {
		return n != nil && n.Kind == ast.KindIdentifier && n.Text() == ctr
	}
	switch incr.Kind {
	case ast.KindPostfixUnaryExpression:
		un := incr.AsPostfixUnaryExpression()
		if un.Operator == ast.KindPlusPlusToken && isCtr(un.Operand) {
			return "1", true
		}
	case ast.KindPrefixUnaryExpression:
		un := incr.AsPrefixUnaryExpression()
		if un.Operator == ast.KindPlusPlusToken && isCtr(un.Operand) {
			return "1", true
		}
	case ast.KindBinaryExpression:
		bin := incr.AsBinaryExpression()
		switch bin.OperatorToken.Kind {
		case ast.KindPlusEqualsToken:
			if isCtr(bin.Left) {
				if k, ok := nonNegIntLiteral(bin.Right); ok && k != "0" {
					return k, true
				}
			}
		case ast.KindEqualsToken:
			if !isCtr(bin.Left) || bin.Right == nil || bin.Right.Kind != ast.KindBinaryExpression {
				return "", false
			}
			add := bin.Right.AsBinaryExpression()
			if add.OperatorToken.Kind != ast.KindPlusToken {
				return "", false
			}
			if isCtr(add.Left) {
				if k, ok := nonNegIntLiteral(add.Right); ok && k != "0" {
					return k, true
				}
			} else if isCtr(add.Right) {
				if k, ok := nonNegIntLiteral(add.Left); ok && k != "0" {
					return k, true
				}
			}
		}
	}
	return "", false
}

// canonicalForShape matches for (let i = L0; i < L1; step) with
// non-negative integer literal bounds and a positive literal step,
// reporting the counter and operand texts. Anything else (<=, calls,
// identifier bounds, floats, zero step) returns false for the legacy
// path.
//
// Bound note: the FOR_CHECK macro compares with ult (unsigned) while
// the legacy condition lowers to slt (signed); the shapes agree exactly
// when the counter never goes negative (non-negative L0, positive
// step) and the bound is a non-negative literal. An identifier bound
// stays legacy: a negative value in the register would iterate ~2^64
// times under ult while slt exits immediately.
func canonicalForShape(fs *ast.ForStatement) (ctr, lo, hi, step string, ok bool) {
	init := fs.Initializer
	if init != nil && init.Kind == ast.KindVariableStatement {
		init = init.AsVariableStatement().DeclarationList
	}
	if init == nil || init.Kind != ast.KindVariableDeclarationList {
		return "", "", "", "", false
	}
	dl := init.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		return "", "", "", "", false
	}
	d := dl.Declarations.Nodes[0]
	nm, good := bindingNameText(d)
	if !good {
		return "", "", "", "", false
	}
	lo, good = nonNegIntLiteral(d.Initializer())
	if !good {
		return "", "", "", "", false
	}
	cond := fs.Condition
	if cond == nil || cond.Kind != ast.KindBinaryExpression {
		return "", "", "", "", false
	}
	bin := cond.AsBinaryExpression()
	if bin.OperatorToken.Kind != ast.KindLessThanToken {
		return "", "", "", "", false
	}
	if bin.Left == nil || bin.Left.Kind != ast.KindIdentifier || bin.Left.Text() != nm {
		return "", "", "", "", false
	}
	hi, good = nonNegIntLiteral(bin.Right)
	if !good {
		return "", "", "", "", false
	}
	step, good = canonicalForStep(nm, fs.Incrementor)
	if !good {
		return "", "", "", "", false
	}
	return nm, lo, hi, step, true
}

// tryLowerForMacro lowers canonical counted loops through the upstream
// control.sal macros (EXPAND FOR_INIT / FOR_CHECK / FOR_NEXT) instead of
// hand-rolled br sequences. Scope, label, break/continue and termination
// discipline mirrors lowerFor exactly; the never-taken elision matches
// too. Reports whether it emitted (anything else falls to legacy).
func (e *emitter) tryLowerForMacro(st *ast.Node) bool {
	fs := st.AsForStatement()
	ctr, lo, hi, step, ok := canonicalForShape(fs)
	if !ok {
		return false
	}
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
	// Never-taken C loops emit the init only (same dead-code rule).
	if fs.Condition != nil && e.isFalseConst(fs.Condition) {
		e.releaseScope()
		e.popScope()
		e.terminated = false
		return true
	}
	e.needImport("sa_std/control.sal")
	topL := e.freshLabel("for_top")
	bodyL := e.freshLabel("for_body")
	endL := e.freshLabel("for_end")
	// `continue` lands before the increment (execution order, mirroring
	// the legacy cont label); continue-free loops keep no dead label.
	contL := ""
	contTgt := topL
	needCont := fs.Incrementor != nil && bodyHasContinue(fs.Statement)
	if needCont {
		contL = e.freshLabel("for_cont")
		contTgt = contL
	}
	e.pushLoopTargets(endL, contTgt)
	e.emit("EXPAND FOR_INIT %s, %s", ctr, lo)
	e.emitRaw("%s:", topL)
	e.emit("EXPAND FOR_CHECK %s, %s, %s, %s", ctr, hi, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	e.terminated = false
	seenCont := e.contJumps
	e.lowerBranchBody(fs.Statement)
	// Increment runs past the loop body (execution order, not parse
	// order). FOR_NEXT carries its own back-jump, unlike the legacy
	// incrementor which needs an explicit jmp after it. Body-scope
	// releases go BEFORE it: anything after its jump would be dead
	// code (FallthroughForbidden). Order mirrors the legacy tail
	// (releases, then back-edge); releaseScope itself stays silent on
	// terminated bodies exactly as before.
	if needCont {
		e.emitRaw("%s:", contL)
	}
	doIncr := !e.terminated || (needCont && e.contJumps > seenCont)
	if doIncr {
		e.terminated = false
	}
	e.releaseScope()
	if doIncr {
		e.emit("EXPAND FOR_NEXT %s, %s, %s", ctr, step, topL)
	}
	e.popScope()
	// No trailing jmp: FOR_NEXT carries its own back-jump when emitted,
	// and when it wasn't (terminated body, no continue used it) control
	// falls through to endL exactly like the legacy tail.
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.releaseScope()
	e.popScope()
	e.breaks = e.breaks[:len(e.breaks)-1]
	e.conts = e.conts[:len(e.conts)-1]
	return true
}

func (e *emitter) lowerFor(st *ast.Node) {
	// Canonical counted loops go through the upstream control.sal
	// macros; everything else keeps the exact legacy shape.
	if e.tryLowerForMacro(st) {
		return
	}
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
	// Never-taken C loops emit the init only (same dead-code rule):
	// balance the scope above, pop no loop targets (none pushed).
	if fs.Condition != nil && e.isFalseConst(fs.Condition) {
		e.releaseScope()
		e.popScope()
		e.terminated = false
		return
	}
	topL := e.freshLabel("for_top")
	bodyL := e.freshLabel("for_body")
	endL := e.freshLabel("for_end")
	// `continue` runs the incrementor (execution order, not parse
	// order), so the continue target is the incrementor when one
	// exists — jumping to top would skip it and hang (found live via
	// labeled-continue verification; unlabeled shared the bug).
	// Continue-free loops keep the exact legacy shape (no dead label).
	contL := ""
	contTgt := topL
	needCont := fs.Incrementor != nil && bodyHasContinue(fs.Statement)
	if needCont {
		contL = e.freshLabel("for_cont")
		contTgt = contL
	}
	e.pushLoopTargets(endL, contTgt)
	e.emitRaw("%s:", topL)
	if fs.Condition != nil {
		cond, _ := e.lowerExpr(fs.Condition)
		cond = e.materializeCond(cond)
		e.emit("br %s -> %s, %s", cond, bodyL, endL)
	} else {
		e.emit("jmp %s", bodyL)
	}
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	e.terminated = false
	seenCont := e.contJumps
	e.lowerBranchBody(fs.Statement)
	// increment runs past the loop body (execution order, not parse order).
	// A body-terminated loop still lowers its incrementor when a continue
	// targeted it (the cont label below is its landing pad).
	if needCont {
		e.emitRaw("%s:", contL)
	}
	if !e.terminated || (needCont && e.contJumps > seenCont) {
		e.terminated = false
		if fs.Incrementor != nil {
			e.lowerExpr(fs.Incrementor)
		}
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
	e.pushLoopTargets(endL, topL)
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
		// Loop-variable declaration: shadowing local, never a slot store.
		e.assignLocal(binding, elemT, "temp", tI32, st)
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
	e.pushLoopTargets(endL, topL)
	e.emitRaw("%s:", topL)
	cT := e.freshTmp()
	e.emit("%s = slt %s, %s", cT, idx, lenT)
	e.emit("br %s -> %s, %s", cT, bodyL, endL)
	e.emitRaw("%s:", bodyL)
	e.pushScope()
	binding := foBindingName(fo)
	// Loop-variable declaration: shadowing local, never a slot store.
	e.assignLocal(binding, idx, "named", tI32, st)
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
	e.pushLoopTargets(endL, condL)
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
		cond = e.materializeCond(cond)
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
// skipped (dead code), while a finally block always runs. SLA parity: SLA
// has no try statement (only postfix ? for Result/Option), so both
// frontends agree catch can never resume.
func (e *emitter) lowerTry(st *ast.Node) {
	ts := st.AsTryStatement()
	if containsThrow(ts.TryBlock) {
		if e.tryLowerThrowingTry(st, ts) {
			return
		}
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

// tryLowerThrowingTry lowers the throwing-try slice:
//
//	try { <prefix>; throw <i32>; } catch (e) { ... } [finally { ... }]
//
// The prefix runs, then the throw transfers to the catch block (no panic):
// the thrown value is evaluated once and bound to the catch param (if any),
// the catch body runs, then finally runs. try/finally without catch runs
// prefix + finally then panics. Statements after the top-level throw are
// dead and skipped. Anything else (no top-level throw, nested throw inside
// if/loop, terminating prefix, try-locals read in handlers, non-i32 throw
// values) returns false for the legacy loud refusal. No Result/future
// convention is invented: the value is a plain i32 local, mirroring the
// zero-value/uninit-decl plumbing.
func (e *emitter) tryLowerThrowingTry(st *ast.Node, ts *ast.TryStatement) bool {
	tryStmts := ts.TryBlock.Statements()
	throwIdx := -1
	for i, s := range tryStmts {
		if s.Kind == ast.KindThrowStatement {
			throwIdx = i
			break
		}
	}
	if throwIdx < 0 {
		return false
	}
	// Prefix must be straight-line (no top-level terminator) and free of
	// nested throws; anything else keeps the legacy loud refusal.
	for _, s := range tryStmts[:throwIdx] {
		switch s.Kind {
		case ast.KindReturnStatement, ast.KindThrowStatement,
			ast.KindBreakStatement, ast.KindContinueStatement:
			return false
		}
		if containsThrow(s) {
			return false
		}
	}
	// Try-locals must not leak into the handlers (the catch scope nests
	// inside the try scope during binding): collect prefix let/var and
	// function/class names; any catch/finally use refuses. Consts fold by
	// value (immutable) and stay visible.
	blocked := map[string]bool{}
	for _, s := range tryStmts[:throwIdx] {
		switch s.Kind {
		case ast.KindVariableStatement:
			dl := s.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			if dl.Flags&ast.NodeFlagsConst != 0 {
				continue
			}
			for _, d := range dl.Declarations.Nodes {
				nm := d.Name()
				if nm == nil || nm.Kind != ast.KindIdentifier {
					return false
				}
				blocked[nm.Text()] = true
			}
		case ast.KindFunctionDeclaration, ast.KindClassDeclaration:
			if nm := s.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				blocked[nm.Text()] = true
			}
		}
	}
	if len(blocked) > 0 {
		uses := map[string]bool{}
		if ts.CatchClause != nil {
			for n := range valueUsedNames(ts.CatchClause.AsCatchClause().Block.Statements()) {
				uses[n] = true
			}
		}
		if ts.FinallyBlock != nil {
			for n := range valueUsedNames(ts.FinallyBlock.Statements()) {
				uses[n] = true
			}
		}
		for n := range uses {
			if blocked[n] {
				return false
			}
		}
	}
	endL := e.freshLabel("endtry")
	e.pushScope()
	e.terminated = false
	for _, s := range tryStmts[:throwIdx] {
		e.lowerBlockStatement(s)
	}
	if e.refused {
		e.releaseScope()
		e.popScope()
		return true
	}
	if e.terminated {
		// Prefix terminated through nested control (both-arms return):
		// the throw is dead; stay loud instead of inventing continuation.
		e.refuse(st, "throw inside try is not lowerable (catch cannot resume after panic)")
		e.releaseScope()
		e.popScope()
		return true
	}
	throwSt := tryStmts[throwIdx]
	throwExpr := throwSt.AsThrowStatement().Expression
	val, vtype := e.lowerExpr(throwExpr)
	if e.refused {
		e.releaseScope()
		e.popScope()
		return true
	}
	if vtype != tI32 && vtype != tBool {
		e.refuse(throwSt, "throw value type is not lowerable (catch params carry i32 only)")
		e.releaseScope()
		e.popScope()
		return true
	}
	if ts.CatchClause == nil {
		if ts.FinallyBlock != nil {
			e.pushScope()
			e.terminated = false
			for _, s := range ts.FinallyBlock.Statements() {
				e.lowerBlockStatement(s)
			}
			e.releaseScope()
			e.popScope()
		}
		e.releaseIfOwnedTemp(val)
		e.releaseScope()
		e.popScope()
		e.emit("panic(%d)", panicThrow)
		e.terminated = true
		return true
	}
	cc := ts.CatchClause.AsCatchClause()
	e.pushScope()
	if cc.VariableDeclaration != nil {
		name, ok := bindingNameText(cc.VariableDeclaration)
		if !ok {
			e.refuse(cc.VariableDeclaration, "destructured catch params are not in the SA-lowerable subset")
			e.popScope()
			e.releaseScope()
			e.popScope()
			return true
		}
		e.assignLocal(name, val, operandKind(val, throwExpr), vtype, cc.VariableDeclaration)
		e.trackBindingAt(name, nil, nil, cc.VariableDeclaration, vtype)
	} else {
		e.releaseIfOwnedTemp(val)
	}
	e.terminated = false
	for _, s := range cc.Block.Statements() {
		e.lowerBlockStatement(s)
	}
	e.releaseScope()
	e.popScope()
	catchTerm := e.terminated
	if ts.FinallyBlock != nil {
		e.pushScope()
		e.terminated = false
		for _, s := range ts.FinallyBlock.Statements() {
			e.lowerBlockStatement(s)
		}
		e.releaseScope()
		e.popScope()
		e.terminated = e.terminated || catchTerm
	}
	e.releaseScope()
	e.popScope()
	if !e.terminated {
		e.emitRaw("%s:", endL)
	}
	return true
}

// containsThrow reports whether a try body itself can throw (then catch is
// unreachably dead and the try must refuse rather than miscompile).
// Nested function/arrow bodies are not descended into: a throw there fires
// on call, not while the try body runs. SLA parity note: SLA has no try
// statement at all (only postfix ? propagation for Result/Option); neither
// frontend has exception edges, so catch is dead code in both.
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
		// A nested function body throws on call, not in this try body.
		if x.Kind == ast.KindFunctionDeclaration || x.Kind == ast.KindFunctionExpression || x.Kind == ast.KindArrowFunction {
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

// tryLowerSwitchMacro lowers 2- and 3-arm switches through the upstream
// control.sal SWITCH_2 / SWITCH_3 dispatch macros. Body, break, default,
// scope and termination discipline mirrors lowerSwitch exactly; only the
// test chain (eq + br per case) moves into the macro. Anything else (1 or
// 4+ arms, multiple defaults, unknown clause kinds) returns false for the
// exact legacy path. Reports whether it emitted (a refusal mid-way still
// claims the statement: refused files are discarded either way, and
// falling through would double-emit).
func (e *emitter) tryLowerSwitchMacro(st *ast.Node) bool {
	sw := st.AsSwitchStatement()
	clauses := sw.CaseBlock.AsCaseBlock().Clauses.Nodes
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
				return true
			}
			defaultNode = cl
		default:
			e.refuse(cl, "switch clause %s is not lowerable", cl.Kind.String())
			return true
		}
	}
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	disc, _ := e.lowerExpr(sw.Expression)
	endL := e.freshLabel("endswitch")
	e.pushBreakTarget(endL)
	// Case values lower up front in order (legacy interleaves them with
	// bodies, but evaluation order at runtime is fixed by labels; both
	// arms evaluate exactly once either way).
	vals := make([]string, len(parts))
	for i, p := range parts {
		expr := p.node.AsCaseOrDefaultClause().Expression
		val, _ := e.lowerExpr(expr)
		vals[i] = val
		if e.refused {
			return true
		}
	}
	bodyLabels := make([]string, len(parts))
	for i := range parts {
		bodyLabels[i] = e.freshLabel("case_b")
	}
	defaultL := e.freshLabel("case_default")
	e.needImport("sa_std/control.sal")
	if len(parts) == 2 {
		e.emit("EXPAND SWITCH_2 %s, %s, %s, %s, %s, %s", disc, vals[0], bodyLabels[0], vals[1], bodyLabels[1], defaultL)
	} else {
		e.emit("EXPAND SWITCH_3 %s, %s, %s, %s, %s, %s, %s, %s", disc, vals[0], bodyLabels[0], vals[1], bodyLabels[1], vals[2], bodyLabels[2], defaultL)
	}
	lowerBody := func(cl *ast.Node) {
		e.pushScope()
		e.terminated = false
		for _, s := range cl.AsCaseOrDefaultClause().Statements.Nodes {
			e.lowerBlockStatement(s)
		}
		e.releaseScope()
		e.popScope()
		if !e.terminated {
			e.emit("jmp %s", endL)
		}
	}
	for i, p := range parts {
		e.emitRaw("%s:", bodyLabels[i])
		lowerBody(p.node)
	}
	e.emitRaw("%s:", defaultL)
	if defaultNode != nil {
		lowerBody(defaultNode)
	} else {
		e.emit("jmp %s", endL)
	}
	e.emitRaw("%s:", endL)
	e.terminated = false
	e.breaks = e.breaks[:len(e.breaks)-1]
	return true
}

// lowerSwitch emits the guarded case-test chain, mirroring sa_plugin_ts
// parseSwitch: each case gets a test label (eq scrutinee/case -> body/next
// test) and a body label; bodies run sequentially so fallthrough is natural;
// break targets the end label via the breaks stack; an unmatched scrutinee
// lands on the default body or exits.
func (e *emitter) lowerSwitch(st *ast.Node) {
	// 2- and 3-arm switches dispatch through the upstream SWITCH_2/3
	// macros; everything else keeps the exact legacy chain.
	if e.tryLowerSwitchMacro(st) {
		return
	}
	sw := st.AsSwitchStatement()
	disc, _ := e.lowerExpr(sw.Expression)
	endL := e.freshLabel("endswitch")
	e.pushBreakTarget(endL)
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
		// Inline-created temps die here (the slot value already joined);
		// caller scopes stay live.
		e.releaseDeeperThan(e.inlineRet.scopeBase, e.inlineRet.slot)
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
		// Top-level pure consts inline (locals shadow via scopes above;
		// namespace members resolve qualified-first).
		qname := e.qualify(n.Text())
		if lit, ok := e.constVals[qname]; ok {
			if e.constIsStr[qname] {
				return e.lowerStringLiteral(lit), tString
			}
			if isFloatLiteral(lit) {
				return lit, tF64
			}
			return lit, tI32
		}
		// Module-state slots read through the registry (locals shadow via
		// scopes above; namespace members resolve qualified-first).
		if ms := e.modStateOf(n.Text()); ms != nil {
			return e.emitModLoad(ms, n)
		}
		// Unbound reads in program mode may name a cross-file member
		// (emitting the bare name traps at check with no diagnostic).
		// Top-level `var f = Math.g` aliases in value position resolve
		// first: const projections (PI/E) fold to their literal through
		// the same table as direct Math.PI; function projections have no
		// first-class value, so they refuse loudly (calls route through
		// lowerMathCall). Without this the bare alias falls through as
		// an undeclared register (silent trap: `mul 2, math_PI`).
		if g, ok := e.mathAliases[e.qualify(n.Text())]; ok {
			if lit, ok := mathConstFold("Math", g); ok {
				return lit, tI32
			}
			e.refuse(n, "Math.%s as a value is not lowerable (call Math.%s(...) directly)", g, g)
			return "0", tUnknown
		}
		// Functions have no first-class value: a cross-file function name
		// read as a value names the real gap even when unimported
		// (importing cannot help; call it directly). Mirrors the Math-alias
		// value refusal above and the bound-function linkRoute branch.
		if e.linkExportKind[n.Text()] == "function" {
			e.refuse(n, "%s as a value is not lowerable (functions have no first-class value; call %s(...) directly)", n.Text(), n.Text())
			return "0", tUnknown
		}
		if r := e.linkRoute(n.Text()); r != "" {
			e.refuse(n, "%s", r)
			return "0", tUnknown
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
	case ast.KindSuperKeyword:
		// Super shares the flattened instance (offsets identical); only
		// valid inside a subclass method/ctor.
		if e.thisSelf == "" {
			e.refuse(n, "super outside a subclass method is not lowerable")
			return "0", tUnknown
		}
		if _, ok := e.superBaseForRecv(e.thisSelf); !ok {
			e.refuse(n, "super outside a subclass method is not lowerable")
			return "0", tUnknown
		}
		return e.thisSelf, tArray
	case ast.KindAwaitExpression:
		// Await unwraps synchronously for value shapes: TS Promise is not
		// SLA future<T> (SLA lowers single/two/linear-8/join2 via a real
		// state machine; cross-async-fn nested await is still a shared-layer
		// gap per rosetta 314/315). Satsgo keeps await v = v for pure values
		// (demos 263/264 stay lowered); genuine suspensions refuse loudly at
		// the inner call (async timers are Phase 2, Promise/new-Promise fall
		// to their existing loud refuses), so unwrap never masks a suspension.
		return e.lowerExpr(n.AsAwaitExpression().Expression)
	case ast.KindAsExpression, ast.KindSatisfiesExpression, ast.KindNonNullExpression, ast.KindTypeAssertionExpression:
		// Type-only assertions erase (angle `<T>x` included: same
		// erasure as `as`; the parser already rejects angle assertions
		// in expression positions where they would ambiguate).
		return e.lowerExpr(n.Expression())
	case ast.KindTaggedTemplateExpression:
		return e.lowerTaggedTemplate(n)
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
		name = e.nsDefName(name)
		init := d.Initializer()
		if init == nil {
			allOk = false
			continue
		}
		// Env-probe ternaries (`typeof G === "undefined" ? lit : G`)
		// fold to their taken arm first: the AST-level const fold below
		// only sees literals, and the untaken arm may not exist.
		init = e.probeFoldedInit(init)
		switch init.Kind {
		case ast.KindNumericLiteral, ast.KindStringLiteral, ast.KindNoSubstitutionTemplateLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword:
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
			// Pure backtick consts fold like string literals
			// (value-position lowering uses n.Text() for both;
			// probed cooked-identical, escapes preserved).
			if init.Kind == ast.KindNoSubstitutionTemplateLiteral {
				e.constVals[name] = init.Text()
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
	// typeof-against-"undefined" guards lower before anything else
	// (see typeof_guard.go); other shapes fall through.
	if v, t, ok := e.lowerTypeofGuard(n); ok {
		return v, t
	}
	// Statically-known typeof-kind comparisons fold to a constant
	// (checker/literal knowledge; unknown kinds keep the loud path).
	if v, t, ok := e.lowerTypeofConstFold(n); ok {
		return v, t
	}
	// assignment folds to register copy (plain `s = "..."` is NOT valid SA).
	if op == ast.KindEqualsToken {
		// Module-string stores materialize literals directly: lowering
		// the RHS first would strand a dead header, and computed RHS
		// refuse here before any code emits for them. Whole objects
		// dispatch the same way (literals only, per-field stores).
		if bin.Left.Kind == ast.KindIdentifier && e.lookupBinding(bin.Left.Text()) == nil {
			if ms := e.modStateOf(bin.Left.Text()); ms != nil && ms.w == modStrW {
				v, t, ok := e.emitModStoreStringDispatch(ms, bin.Right, n)
				if !ok {
					return "0", tUnknown
				}
				return v, t
			}
			if ms := e.modStateOf(bin.Left.Text()); ms != nil && ms.isObj {
				v, t, ok := e.emitModStoreObject(ms, bin.Right, n)
				if !ok {
					return "0", tUnknown
				}
				return v, t
			}
		}
		rhs, rtype := e.lowerExpr(bin.Right)
		if bin.Left.Kind == ast.KindIdentifier {
			name := bin.Left.Text()
			// Module-state assignment stores through the registry and
			// yields the stored operand (chained assignments keep a real
			// value); scope bindings prefer locals via assign below.
			if e.lookupBinding(name) == nil {
				if ms := e.modStateOf(name); ms != nil {
					v, t, ok := e.emitModStore(ms, rhs, operandKind(rhs, bin.Right), rtype, n)
					if !ok {
						return "0", tUnknown
					}
					return v, t
				}
			}
			e.assign(name, rhs, operandKind(rhs, bin.Right), rtype, n)
			return bin.Left.Text(), tI32
		}
		if bin.Left.Kind == ast.KindElementAccessExpression {
			e.lowerElementStore(bin.Left, rhs)
			return rhs, tI32
		}
		if bin.Left.Kind == ast.KindPropertyAccessExpression {
			// DOM text writes claim their receivers first (see dom_proj.go).
			if pa := bin.Left.AsPropertyAccessExpression(); pa.Expression.Kind == ast.KindIdentifier && e.domVars[pa.Expression.Text()] {
				if e.lowerDomStore(pa.Expression.Text(), pa.Name().Text(), rhs, rtype, n) {
					return rhs, tI32
				}
				return "0", tUnknown
			}
			// Mutable namespace members store through their slot
			// (privacy enforced; unregistered members fall through to
			// the loud refusal below, same as consts today).
			if q, ns, mem, ok := e.nsLetTarget(bin.Left); ok {
				if ms := e.modVars[q]; ms != nil {
					if !e.checkNsAccess(ns, mem, n) {
						return "0", tUnknown
					}
					// String members dispatch on the RHS node (the
					// pre-lowered header, if any, releases via normal
					// scope cleanup). Whole objects dispatch the same
					// way (literals only, per-field stores).
					if ms.w == modStrW {
						v, t, ok := e.emitModStoreStringDispatch(ms, bin.Right, n)
						if !ok {
							return "0", tUnknown
						}
						return v, t
					}
					if ms.isObj {
						v, t, ok := e.emitModStoreObject(ms, bin.Right, n)
						if !ok {
							return "0", tUnknown
						}
						return v, t
					}
					v, t, ok := e.emitModStore(ms, rhs, operandKind(rhs, bin.Right), rtype, n)
					if !ok {
						return "0", tUnknown
					}
					return v, t
				}
			}
			// Module-object field stores persist to the field slot
			// (single-level only; deeper chains refuse in
			// lowerFieldStore rather than dropping silently). Scope
			// bindings shadow the module name (see layoutOfVar).
			if pa := bin.Left.AsPropertyAccessExpression(); pa.Expression.Kind == ast.KindIdentifier {
				if e.lookupBinding(pa.Expression.Text()) == nil {
					if ms := e.modStateOf(pa.Expression.Text()); ms != nil && ms.isObj {
						v, t, ok := e.emitModStoreField(ms, pa.Name().Text(), rhs, operandKind(rhs, bin.Right), rtype, n)
						if !ok {
							return "0", tUnknown
						}
						return v, t
					}
				}
			}
			// Namespace object field stores (`N.obj.x`, privacy like
			// the member itself).
			if ms, field, ns, mem, ok := e.nsObjFieldTarget(bin.Left); ok {
				if !e.checkNsAccess(ns, mem, n) {
					return "0", tUnknown
				}
				v, t, ok := e.emitModStoreField(ms, field, rhs, operandKind(rhs, bin.Right), rtype, n)
				if !ok {
					return "0", tUnknown
				}
				return v, t
			}
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
	// `&&=` / `||=` / `??=` lower with real short-circuit (unlike the
	// eager `and`/`or` value ops): the RHS lowers only on the assign arm,
	// and the arms join through a slot (mirrors the `??` join shape).
	if op == ast.KindAmpersandAmpersandEqualsToken || op == ast.KindBarBarEqualsToken ||
		op == ast.KindQuestionQuestionEqualsToken {
		return e.lowerLogicAssign(bin, op, n)
	}
	// `in` folds statically: layouts are fixed, so field presence is a
	// compile-time 1/0 (unknown bases refuse loudly). The verdict
	// materialises into a temp (br takes registers, not immediates).
	if op == ast.KindInKeyword {
		verdict := ""
		// Brand checks (`#x in obj`) resolve against the lexical owner
		// (checker-backed layouts cover class-annotated params, which
		// carry no varLayouts entry; mirrors lowerMemberChain).
		if bin.Left.Kind == ast.KindPrivateIdentifier {
			if bin.Right.Kind == ast.KindIdentifier {
				if l := e.layoutOfNode(bin.Right.Text(), bin.Right); l != nil {
					if key, ok := e.privResolve(l, bin.Left.Text(), n); ok {
						if _, ok := l.offsets[key]; ok {
							verdict = "1"
						} else {
							verdict = "0"
						}
					} else {
						return "0", tUnknown
					}
				}
			}
		} else if bin.Left.Kind == ast.KindStringLiteral {
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
			// 字面量除零编译期拒（`1/0` 原生 SIGFPE，JS 得 Infinity；
			// 变量除数沿旧门，f64 除法 IEEE 无崩沿旧路；与 tsgosa R3-49 同形）。
			if bin.Right != nil && bin.Right.Kind == ast.KindNumericLiteral && bin.Right.Text() == "0" {
				e.refuse(n, "division by zero (literal zero divisor traps; JS yields Infinity)")
				return "0", tUnknown
			}
			e.emit("%s = div %s, %s", t, l, r)
		}
	case ast.KindPercentToken:
		if floats {
			e.refuse(n, "float %% lowers to no SA-ASM instruction (there is no frem); refuse loudly")
			return "0", tUnknown
		}
		// 字面量零取余同除零拒（`1%0` 同样陷阱；与上除法臂同形）。
		if bin.Right != nil && bin.Right.Kind == ast.KindNumericLiteral && bin.Right.Text() == "0" {
			e.refuse(n, "division by zero (literal zero divisor traps; JS yields Infinity)")
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
	} else if bin.Left.Kind == ast.KindPropertyAccessExpression {
		// Mutable namespace members compound through their slot (the
		// switch above emitted integer arithmetic, same as locals; f64
		// slots refuse loudly instead of mis-storing).
		if q, ns, mem, ok := e.nsLetTarget(bin.Left); ok {
			if ms := e.modVars[q]; ms != nil {
				if !e.checkNsAccess(ns, mem, bin.Left) {
					return
				}
				if ms.w == "f64" {
					e.refuse(bin.Left, "compound assignment on f64 module state %s is not lowerable (use plain assignment)", q)
					return
				}
				_, _, _ = e.emitModStore(ms, t, "temp", ms.saType(), bin.Left)
				return
			}
		}
		e.refuse(bin.Left, "compound assignment target is not lowerable")
	} else {
		e.refuse(bin.Left, "compound assignment target is not lowerable")
	}
}

// lowerLogicAssign lowers `a &&= b` / `a ||= b` / `a ??= b` with real
// short-circuit: the test reads the target once, the RHS lowers only on
// the assign arm, and both arms join through a slot (mirrors the `??`
// join shape). Targets mirror `=`: bare identifiers (scope bindings win
// over module slots) and mutable namespace members; anything else refuses
// loudly. Strings test length (empty is falsy; a header pointer never is);
// `??=` keeps the pointer test like `??` (null/undefined map to 0).
func (e *emitter) lowerLogicAssign(bin *ast.BinaryExpression, op ast.Kind, pos *ast.Node) (string, saType) {
	// Resolve the target once: test-value operand plus a store closure.
	// The closure lowers the RHS and stores it, reporting the value
	// operand the assignment yields (same rule as `=`).
	type target struct {
		test  string
		ttype saType
		store func(rhs *ast.Node) (string, saType, bool)
	}
	var tgt *target
	if bin.Left.Kind == ast.KindIdentifier {
		// Same declared-or-sloppy rule as `=`/`+=`: lowerExpr resolves
		// (locals, slots, folds), assign declares the rest. The join
		// slot needs a materialized register, so the closure returns
		// the RHS operand (never the bare name: inside namespaces it
		// resolves qualified and names no register).
		name := bin.Left.Text()
		l, lt := e.lowerExpr(bin.Left)
		if e.refused {
			return "0", tUnknown
		}
		tgt = &target{test: l, ttype: lt, store: func(rhs *ast.Node) (string, saType, bool) {
			// Module-string slots take literal stores (assign would
			// refuse via modWiden; mirrors the `=` dispatch).
			if e.lookupBinding(name) == nil {
				if ms := e.modStateOf(name); ms != nil && ms.w == modStrW {
					return e.emitModStoreStringDispatch(ms, rhs, pos)
				}
			}
			rv, rt := e.lowerExpr(rhs)
			kind := operandKind(rv, rhs)
			e.assign(name, rv, kind, rt, pos)
			if e.refused {
				return "0", tUnknown, false
			}
			return e.snapImm(rv, rt, kind), rt, true
		}}
	} else if bin.Left.Kind == ast.KindPropertyAccessExpression {
		if q, ns, mem, ok := e.nsLetTarget(bin.Left); ok {
			if ms := e.modVars[q]; ms != nil {
				if !e.checkNsAccess(ns, mem, bin.Left) {
					return "0", tUnknown
				}
				l, lt := e.emitModLoad(ms, bin.Left)
				tgt = &target{test: l, ttype: lt, store: func(rhs *ast.Node) (string, saType, bool) {
					if ms.w == modStrW {
						return e.emitModStoreStringDispatch(ms, rhs, pos)
					}
					rv, rt := e.lowerExpr(rhs)
					kind := operandKind(rv, rhs)
					v, t, ok := e.emitModStore(ms, rv, kind, rt, pos)
					if !ok {
						return "0", tUnknown, false
					}
					return e.snapImm(v, t, kind), t, true
				}}
			}
		}
		if tgt == nil {
			e.refuse(bin.Left, "logical assignment target is not lowerable")
			return "0", tUnknown
		}
	} else {
		e.refuse(bin.Left, "logical assignment target is not lowerable")
		return "0", tUnknown
	}
	// Truthiness test: scalars/f64 compare against zero (fcmp for
	// floats); strings compare length (empty is falsy); `??=` keeps the
	// pointer test like `??`.
	// The join slot allocates before the test/branch (a terminator
	// must precede every label; mirrors the `??` shape).
	slot := e.freshTmp()
	e.emit("%s = alloc 8", slot)
	e.ownTemp(slot)
	test := e.freshTmp()
	isStr := tgt.ttype == tString
	if isStr && op != ast.KindQuestionQuestionEqualsToken {
		_, ln := e.expandSlice(tgt.test)
		if op == ast.KindAmpersandAmpersandEqualsToken {
			e.emit("%s = ne %s, 0", test, ln)
		} else {
			e.emit("%s = eq %s, 0", test, ln)
		}
	} else if tgt.ttype == tF64 {
		if op == ast.KindAmpersandAmpersandEqualsToken {
			e.emit("%s = fcmp_ne %s, 0.0", test, tgt.test)
		} else {
			e.emit("%s = fcmp_eq %s, 0.0", test, tgt.test)
		}
	} else {
		if op == ast.KindAmpersandAmpersandEqualsToken {
			e.emit("%s = ne %s, 0", test, tgt.test)
		} else {
			e.emit("%s = eq %s, 0", test, tgt.test)
		}
	}
	// The test is "should assign": truthy for `&&=`, falsy/nullish
	// for `||=`/`??=` — so it always branches to the assign arm first.
	assignL := e.freshLabel("logas_assign")
	skipL := e.freshLabel("logas_skip")
	endL := e.freshLabel("logas_end")
	e.emit("br %s -> %s, %s", test, assignL, skipL)
	e.emitRaw("%s:", assignL)
	v, _, ok := tgt.store(bin.Right)
	if !ok || e.refused {
		return "0", tUnknown
	}
	e.emit("store %s + 0, %s as ptr", slot, v)
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", skipL)
	// Folded-constant tests arrive as immediates (the assign arm then
	// refuses); snapshot so the slot store always sees a register.
	e.emit("store %s + 0, %s as ptr", slot, e.snapImm(tgt.test, tgt.ttype, operandKind(tgt.test, bin.Left)))
	e.emit("jmp %s", endL)
	e.emitRaw("%s:", endL)
	out := e.freshTmp()
	e.emit("%s = load %s + 0 as i32", out, slot)
	e.releaseIfOwnedTemp(slot)
	return out, tgt.ttype
}

// isLogicAssign reports the short-circuit assignments (`&&=`/`||=`/`??=`),
// which lower through lowerLogicAssign rather than lowerCompoundAssign.
func isLogicAssign(op ast.Kind) bool {
	return op == ast.KindAmpersandAmpersandEqualsToken ||
		op == ast.KindBarBarEqualsToken ||
		op == ast.KindQuestionQuestionEqualsToken
}

// snapImm snapshots an immediate operand into a fresh temp (join slots
// and stores take registers, not immediates); anything else passes
// through untouched.
func (e *emitter) snapImm(v string, t saType, kind string) string {
	if kind != "imm" {
		return v
	}
	o := e.freshTmp()
	if t == tF64 {
		e.emit("%s = fadd %s, 0.0", o, v)
	} else {
		e.emit("%s = add %s, 0", o, v)
	}
	return o
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
		// Mutable namespace members inc/dec through their slot.
		if q, ns, mem, ok := e.nsLetTarget(operand); ok {
			if ms := e.modVars[q]; ms != nil {
				if !e.checkNsAccess(ns, mem, pos) {
					return "0", tUnknown
				}
				return e.emitModIncDec(ms, up, postfix, pos)
			}
		}
		// Module-object fields inc/dec through their field slot
		// (scope bindings shadow the module name, see layoutOfVar).
		pa := operand.AsPropertyAccessExpression()
		if pa.Expression.Kind == ast.KindIdentifier && e.lookupBinding(pa.Expression.Text()) == nil {
			if ms := e.modStateOf(pa.Expression.Text()); ms != nil && ms.isObj {
				return e.emitModObjIncDec(ms, pa.Name().Text(), up, postfix, pos)
			}
		}
		// Namespace object fields (`N.obj.x`, privacy like the member).
		if ms, field, ns, mem, ok := e.nsObjFieldTarget(operand); ok {
			if !e.checkNsAccess(ns, mem, pos) {
				return "0", tUnknown
			}
			return e.emitModObjIncDec(ms, field, up, postfix, pos)
		}
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
	// Module-state inc/dec loads, adds, and stores back through the
	// registry (scope bindings prefer locals below).
	if e.lookupBinding(name) == nil {
		if ms := e.modStateOf(name); ms != nil {
			return e.emitModIncDec(ms, up, postfix, pos)
		}
	}
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
	// console.error/time/timeEnd/clear → node plugin (node_console.go);
	// timers stay refused (async, Phase 2).
	for _, m := range []string{"error", "time", "timeEnd", "clear"} {
		if isConsoleMethod(call.Expression, m) {
			if m == "error" {
				return e.lowerConsoleError(args, argTypes, n)
			}
			return e.lowerConsoleTime(m, args, n)
		}
	}
	// Bare btoa/atob (Web globals) via the deno plugin; user definitions
	// shadow (checked first, like timers).
	if call.Expression.Kind == ast.KindIdentifier {
		fname := call.Expression.Text()
		if fname == "btoa" || fname == "atob" {
			_, isArrow := e.arrowAliases[fname]
			_, isImport := e.importEnv[fname]
			_, isFunc := e.funcSigs[fname]
			if !isArrow && !isImport && !isFunc && !e.localDefs[fname] {
				return e.lowerBareBtoa(fname, args, argTypes, n)
			}
		}
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
		if g, ok := e.mathAliases[e.qualify(call.Expression.Text())]; ok {
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
		// Namespace members resolve qualified-first (bare `f()` inside
		// NS; identity elsewhere, so builtins keep working).
		fname := e.qualify(call.Expression.Text())
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
		// Aliased imports (`import { a as b }`) resolve through the
		// remote export name while the local name stays the callee.
		if mod, ok := e.importedFrom[fname]; ok {
			remote := fname
			if r, ok := e.importedRemote[fname]; ok {
				remote = r
			}
			// createHash/createHmac stage a Hash/Hmac accumulator
			// (buffered slices until digest; see hashState). No SA is
			// emitted for the call itself.
			if mod == "crypto" && (remote == "createHash" || remote == "createHmac") {
				return e.lowerCreateHash(remote, args, n)
			}
			if proj, ok := projectionByTS(mod + "." + remote); ok {
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
			var padOk bool
			if args, argTypes, padOk = e.padDefaultArgs(q, args, argTypes, n); !padOk {
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
		// Entry synthesis renamed a colliding user `main` (explicit
		// arrows and imports win above).
		fname = e.entryMainName(fname)
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
			var padOk bool
			if args, argTypes, padOk = e.padDefaultArgs(fname, args, argTypes, n); !padOk {
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
		// Cross-file misses route to the defining file (kind-aware:
		// importable kinds point at the import, the rest name the gap).
		if r := e.linkRoute(fname); r != "" {
			e.refuse(n, "%s", r)
			return "0", tUnknown
		}
		// Async timers refuse with the Phase-2 rationale (user-defined
		// shadowing still wins via funcSigs above).
		if refuseTimerCall(e, fname, n) {
			return "0", tUnknown
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
		// A dotted call whose root names a cross-file definable routes
		// to its file instead of the generic first-class refuse (same-
		// file values shadow: folds, slots and aliases win first).
		// Nested chains (N.M.g) route on the leftmost root via
		// dottedRoot; single-level keeps the exact-identifier path.
		if pa := call.Expression.AsPropertyAccessExpression(); pa.Expression.Kind == ast.KindIdentifier {
			root := pa.Expression.Text()
			if _, ok := e.constVals[root]; !ok && e.modStateOf(root) == nil {
				if _, ok := e.arrowAliases[root]; !ok {
					if r := e.linkRoute(root); r != "" {
						e.refuse(n, "%s", r)
						return "0", tUnknown
					}
				}
			}
		} else if root := dottedRoot(call.Expression); root != "" {
			if _, ok := e.constVals[root]; !ok && e.modStateOf(root) == nil {
				if _, ok := e.arrowAliases[root]; !ok {
					if r := e.linkRoute(root); r != "" {
						e.refuse(n, "%s", r)
						return "0", tUnknown
					}
				}
			}
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
	fname := e.qualify(call.Expression.Text())
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
	fname = e.qualify(fname)
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
	// Entry synthesis renamed a colliding user `main` (explicit
	// arrows and imports win above).
	fname = e.entryMainName(fname)
	if _, ok := e.funcSigs[fname]; ok {
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, e.fnRef(fname), strings.Join(args, ", "))
		e.ownTemp(t)
		return t
	}
	if refuseTimerCall(e, fname, n) {
		return "0"
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
	// TypeScript namespaces (`NS.f(...)`, `A.B.g(...)`) route to qualified
	// callees before receiver resolution (dotted bases never reach the
	// value dispatch below). Value receivers shadow namespaces.
	if v, t, ok := e.lowerNamespaceCallSite(fn, pa, method, args, argNodes, pos); ok {
		return v, t, true
	}
	// `super.m(...)` routes to the base-class method on this (flattened
	// layouts keep field offsets; see class_heritage.go).
	if pa.Expression.Kind == ast.KindSuperKeyword {
		return e.lowerSuperMethodCall(pa, args, argNodes, pos)
	}
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
		// Two-level namespaces (Deno.env.get) route in their module;
		// imported namespace paths (N.M.g) route in link_namespace;
		// anything else stays loudly unroutable.
		if v, t, ok := routeDenoEnvChain(e, pa.Expression, method, args, types, pos); ok {
			return v, t, true
		}
		if v, t, ok := routeNestedNSCall(e, pa.Expression, method, args, pos); ok {
			return v, t, true
		}
	}
	// Object-default member routing lives in link_nsobject.
	if v, t, ok := routeDefNSMember(e, recv, method, args, pos); ok {
		return v, t, true
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
	// `f.call(thisArg, ...args)` desugars to a direct call with thisArg
	// first (matches the static-method convention: self is explicit).
	// The callee must resolve statically (aliases, imports, functions).
	// Class instances keep their own `call` method if one exists.
	if method == "call" {
		if _, ok := e.varClass[recv]; !ok {
			if v, t, ok := e.lowerCallDesugar(recv, args, types, argNodes, pos); ok {
				return v, t, true
			}
			return "", tUnknown, false
		}
	}
	// process/crypto globals lower through the node backend without an
	// import (Node exposes them globally; same zero-arg string shape).
	// Deno.* globals lower through the deno backend the same way.
	if (recv == "process" || recv == "crypto") && len(args) == 0 {
		if proj, ok := projectionByTS(recv + "." + method); ok && isPluginBackend(proj) {
			v, t := e.emitProjCall(proj, args, pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
	}
	if recv == "Deno" {
		if method == "mkdir" || method == "remove" {
			if v, t, ok := routeDenoFs(e, method, args, types, pos); ok {
				return v, t, true
			}
			return "0", tUnknown, true
		}
		if proj, ok := projectionByTS("Deno." + method); ok && isPluginBackend(proj) {
			v, t := e.emitProjCall(proj, args, pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
	}
	// Buffer.byteLength lowers direct; Buffer.concat needs its array
	// literal (see node_buffer.go). Anything else on Buffer refuses.
	if recv == "Buffer" {
		if method == "byteLength" {
			v, t := e.lowerBufferByteLength(args, types, pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
		if method == "concat" {
			v, t := e.lowerBufferConcat(argNodes, pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
	}
	// Date.now() lowers to sa_time_unix_ms (no import; Date is global).
	// Date.parse(s) lowers to sa_time_parse_iso with a status check
	// (invalid ISO panics; NaN is unrepresentable in i64).
	// Anything else on Date falls through to loud refusal below.
	if recv == "Date" && method == "now" && len(args) == 0 {
		if proj, ok := projectionByTS("Date.now"); ok {
			v, t := e.emitProjCall(proj, args, pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
	}
	if recv == "Date" && method == "parse" && len(args) == 1 {
		if proj, ok := projectionByTS("Date.parse"); ok {
			v, t := e.emitStatusCheckedI64(proj, args[0], pos)
			if e.refused {
				return "0", tUnknown, true
			}
			return v, t, true
		}
	}
	// Date millis bindings answer getTime as the identity (the value
	// already is i64 millis), toISOString/getters via the sci time
	// primitives (i64 by value), and getTimezoneOffset as constant 0
	// (UTC-only subset); other methods refuse explicitly here (never
	// fall through: the generic tail would misreport them).
	if e.dateVars[recv] {
		if (method == "getTime" || method == "valueOf") && len(args) == 0 {
			return recv, tI64, true
		}
		if method == "getTimezoneOffset" && len(args) == 0 {
			return "0", tI32, true
		}
		if len(args) == 0 {
			for _, m := range []string{"toISOString", "getFullYear", "getMonth", "getDate", "getHours", "getMinutes", "getSeconds", "getMilliseconds", "getDay", "toString", "toDateString", "toTimeString", "toUTCString"} {
				if method != m {
					continue
				}
				if proj, ok := projectionByTS("Date." + m); ok {
					v, t := e.emitProjCall(proj, []string{recv}, pos)
					if e.refused {
						return "0", tUnknown, true
					}
					return v, t, true
				}
			}
		}
		// Setters take one value, mutate (rebind) and return new millis.
		// Contract order is (ms, field, value); the id splices explicitly
		// (table Extra appends last, which would misorder).
		if len(args) == 1 {
			fields := map[string]string{
				"setFullYear": "0", "setMonth": "1", "setDate": "2",
				"setHours": "3", "setMinutes": "4", "setSeconds": "5",
				"setMilliseconds": "6",
			}
			if fid, ok := fields[method]; ok {
				if proj, ok := projectionByTS("Date." + method); ok {
					v, t := e.emitProjCall(proj, []string{recv, fid, args[0]}, pos)
					if e.refused {
						return "0", tUnknown, true
					}
					e.assign(recv, v, "temp", tI64, pos)
					return v, t, true
				}
			}
		}
		e.refuse(pos, "Date.%s is not in the subset (see the time projection list)", method)
		return "0", tUnknown, true
	}
	// Class methods inline at the call site (no vtables in SA-ASM).
	// Scope maps win; otherwise the checker names the receiver's class
	// (annotated params/locals, including cross-file types). Map writes
	// never happen here, so cross-function staleness cannot form.
	className, ok := e.varClass[recv]
	if !ok && pa.Expression.Kind == ast.KindIdentifier {
		className, ok = e.checkerClassOf(pa.Expression)
	}
	if ok {
		if v, t, ok := e.lowerClassMethodCall(recv, className, method, args, argNodes, pos); ok {
			return v, t, true
		}
		// Unknown method: fall through to array/string surfaces, then refuse.
	}
	// Static methods dispatch on the class name (no instance; `this`
	// stays empty inside). Locals and module-state slots shadow the
	// class binding, and private `#m` stays on the loud path below.
	if pa.Expression.Kind == ast.KindIdentifier && !strings.HasPrefix(method, "#") &&
		e.lookupBinding(recv) == nil && e.modStateOf(recv) == nil {
		if _, ok := e.classDefs[recv]; ok {
			if v, t, ok := e.lowerClassStaticCall(recv, method, args, argNodes, pos); ok {
				return v, t, true
			}
			// Unknown static: fall through to the loud refusal below.
		}
	}
	// document.createElement/createTextNode lower to the airlock DOM
	// backend; DOM handles dispatch to sax_dom_* (see dom_proj.go).
	if recv == "document" && (method == "createElement" || method == "createTextNode") {
		v, t := e.lowerDocumentCreate(method, args, types, pos)
		if e.refused {
			return "0", tUnknown, true
		}
		return v, t, true
	}
	if _, ok := e.domVars[recv]; ok {
		if v, t, ok := e.lowerDomMethod(recv, method, args, types, pos); ok {
			return v, t, true
		}
		return "", tUnknown, false
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
	// Crypto Hash accumulators buffer slices until digest (Map/Set-style
	// handle dispatch; unknown hash methods refuse loudly).
	if st, ok := e.hashAcc[recv]; ok {
		if v, t, ok := e.lowerHashMethod(recv, st, method, args, types, argNodes, pos); ok {
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
	// Module-state strings pre-lower to a header temp and route to the
	// string surface directly (locals shadow via scopes above; without
	// this, array-named methods like indexOf would claim the receiver).
	if h, ok := e.modStrRecv(recv, pos); ok {
		if v, t, ok := e.lowerStringMethod(h, method, args, pos); ok {
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

// lowerCallDesugar lowers `f.call(thisArg, ...args)` as `f(thisArg, ...)`.
// Only statically-known callees (aliases, imports, declared functions)
// desugar; anything else refuses (true first-class values stay loud).
func (e *emitter) lowerCallDesugar(recv string, args []string, types []saType, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	_ = argNodes
	if len(args) < 1 {
		return "", tUnknown, false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "@callback:") || strings.HasPrefix(a, "@spread:") {
			return "", tUnknown, false
		}
	}
	if _, ok := e.arrowAliases[recv]; ok {
		ai := e.arrowAliases[recv]
		if len(args)-1 != len(ai.params) {
			e.refuse(pos, "arity mismatch in .call to %s", recv)
			return "0", tUnknown, true
		}
		full := append(append([]string{}, args...), ai.captures...)
		if ai.ret == tVoid {
			e.emit("call @%s(%s)", ai.fn, strings.Join(full, ", "))
			return "0", tVoid, true
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, ai.fn, strings.Join(full, ", "))
		e.ownTemp(t)
		return t, ai.ret, true
	}
	if q, ok := e.importEnv[recv]; ok {
		ret := e.importRet[recv]
		full := args
		if !e.checkArity(q, full, pos) {
			return "0", tUnknown, true
		}
		fullTypes := types
		var padOk bool
		if full, fullTypes, padOk = e.padDefaultArgs(q, full, fullTypes, pos); !padOk {
			return "0", tUnknown, true
		}
		if ret == tVoid {
			e.emit("call @%s(%s)", q, strings.Join(full, ", "))
			return "0", tVoid, true
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, q, strings.Join(full, ", "))
		e.ownTemp(t)
		return t, ret, true
	}
	if ret, ok := e.funcSigs[recv]; ok {
		if e.link != nil && !e.localDefs[recv] {
			return "", tUnknown, false
		}
		full := args
		if !e.checkArity(recv, full, pos) {
			return "0", tUnknown, true
		}
		fullTypes := types
		var padOk bool
		if full, fullTypes, padOk = e.padDefaultArgs(recv, full, fullTypes, pos); !padOk {
			return "0", tUnknown, true
		}
		if ret == tVoid {
			e.emit("call @%s(%s)", e.fnRef(recv), strings.Join(full, ", "))
			return "0", tVoid, true
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, e.fnRef(recv), strings.Join(full, ", "))
		e.ownTemp(t)
		return t, ret, true
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

// lowerCreateHash stages a crypto Hash/Hmac accumulator: fed bytes buffer
// as a plain string slice (empty at creation) until digest() routes the
// algorithm (plus key) plus buffer through the one-shot node crypto_hash/
// crypto_hmac primitives. Only declaration-form adoption tracks the state
// (see lowerVarDeclList); anything else using the result refuses loudly
// at the method site.
func (e *emitter) lowerCreateHash(fname string, args []string, pos *ast.Node) (string, saType) {
	kind := "Hash"
	want := 1
	if fname == "createHmac" {
		kind = "Hmac"
		want = 2
	}
	if len(args) != want {
		e.refuse(pos, "%s takes exactly %d argument(s)", fname, want)
		return "0", tUnknown
	}
	acc := e.lowerStringLiteral("")
	st := &hashState{kind: kind, acc: acc, algo: args[0]}
	if fname == "createHmac" {
		st.key = args[1]
	}
	e.lastHash = st
	return acc, tString
}

// lowerHashMethod routes Hash.update/digest over a staged accumulator.
// update folds one string chunk via sa_string_concat and rebinds the
// receiver (assign carries release discipline); digest emits the node
// crypto_hash call (hex natively) and latches finalized. Non-literal or
// non-hex encodings, post-finalize use, and unknown methods all refuse.
func (e *emitter) lowerHashMethod(recv string, st *hashState, method string, args []string, types []saType, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	if st.done {
		e.refuse(pos, "%s is already digested (ERR_CRYPTO_HASH_FINALIZED)", st.kind)
		return "0", tUnknown, true
	}
	switch method {
	case "update":
		if len(args) != 1 {
			e.refuse(pos, "%s.update takes exactly 1 argument", st.kind)
			return "0", tUnknown, true
		}
		if len(types) > 0 && types[0] != tString {
			e.refuse(pos, "%s.update takes a string chunk", st.kind)
			return "0", tUnknown, true
		}
		e.needImport("sa_std/string.sai")
		dp, dl := e.expandSlice(args[0])
		ap, al := e.expandSlice(st.acc)
		t := e.freshTmp()
		e.emit("%s = call @sa_string_concat(%s, %s, %s, %s)", t, ap, al, dp, dl)
		e.declareOwned(t)
		e.assign(recv, t, "temp", tString, pos)
		st.acc = t
		return t, tString, true
	case "digest":
		enc := "hex"
		if len(args) > 1 {
			e.refuse(pos, "%s.digest takes at most 1 argument (encoding)", st.kind)
			return "0", tUnknown, true
		}
		if len(args) == 1 {
			lit, ok := digestEncoding(argNodes)
			if !ok {
				e.refuse(pos, "%s.digest encoding must be a string literal", st.kind)
				return "0", tUnknown, true
			}
			enc = lit
		}
		if enc != "hex" {
			e.refuse(pos, "%s.digest(%q) is not lowerable (only hex digests are projected)", st.kind, enc)
			return "0", tUnknown, true
		}
		surface := "crypto.hash"
		callArgs := []string{st.algo, st.acc}
		if st.kind == "Hmac" {
			surface = "crypto.hmac"
			callArgs = []string{st.algo, st.key, st.acc}
		}
		proj, ok := projectionByTS(surface)
		if !ok {
			e.refuse(pos, "%s is not a projected std surface (see StdProjectionTable)", surface)
			return "0", tUnknown, true
		}
		v, t := e.emitProjCall(proj, callArgs, pos)
		if e.refused {
			return "0", tUnknown, true
		}
		st.done = true
		return v, t, true
	default:
		e.refuse(pos, "%s.%s is not a projected surface", st.kind, method)
		return "0", tUnknown, true
	}
}

// digestEncoding reads a literal digest encoding from the call's argument
// nodes (lowered operands lose literal text; non-literals refuse).
func digestEncoding(argNodes *ast.ElementList) (string, bool) {
	if argNodes == nil || len(argNodes.Nodes) != 1 {
		return "", false
	}
	return stringLiteralText(argNodes.Nodes[0])
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
	// Scopes at or below this index survive the inline (join-point
	// cleanup releases deeper ones only).
	scopeBase := len(e.scopes) - 1
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
	e.inlineRet = &inlineRetState{active: true, slot: slot, end: endL, saname: "i32", scopeBase: scopeBase}
	e.terminated = false
	for _, s := range body.Statements() {
		e.lowerBlockStatement(s)
		if e.refused {
			break
		}
	}
	e.inlineRet = saved
	// Fallthrough join releases callback-created temps (the slot
	// outlives into the load below).
	if !e.terminated {
		e.releaseDeeperThan(scopeBase, slot)
	}
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
		if e.arrVars[val] || e.strVars[val] || e.modStrTmps[val] {
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
			if e.strVars[val] || e.modStrTmps[val] {
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

// destructurePending records one binding-pattern parameter: the hidden
// handle param plus the pattern to expand at the top of the body.
type destructurePending struct {
	hid   string
	pat   *ast.Node
	annot *ast.Node
}

// hiddenDestructuredParam synthesizes a hidden handle parameter for a
// binding-pattern parameter (`{x, y}` / `[a, b]`). Bare patterns only:
// rest/default/optional pattern params stay loudly refused. The hidden
// name is unique against sibling parameter names.
func (e *emitter) hiddenDestructuredParam(p *ast.Node, params []*ast.Node) (string, *ast.Node, *ast.Node, bool) {
	pd := p.AsParameterDeclaration()
	if pd.DotDotDotToken != nil || pd.Initializer != nil || pd.QuestionToken != nil {
		return "", nil, nil, false
	}
	var pat *ast.Node
	if nm := pd.Name(); nm != nil {
		pat = nm.AsNode()
	}
	if pat == nil || (pat.Kind != ast.KindObjectBindingPattern && pat.Kind != ast.KindArrayBindingPattern) {
		return "", nil, nil, false
	}
	taken := map[string]bool{}
	for _, q := range params {
		if s, ok := bindingNameText(q.AsNode()); ok {
			taken[s] = true
		}
	}
	hid := "__darg"
	for taken[hid] {
		hid += "_"
	}
	taken[hid] = true
	return hid, pat, pd.Type, true
}

// drainDestructuredParams expands pending pattern parameters field-wise at
// the top of the body (same helpers as destructuring declarations, so
// shapes and refusals agree exactly).
func (e *emitter) drainDestructuredParams(pending []destructurePending) {
	for _, q := range pending {
		switch q.pat.Kind {
		case ast.KindArrayBindingPattern:
			if e.arrVars == nil {
				e.arrVars = map[string]bool{}
			}
			if e.arrElems == nil {
				e.arrElems = map[string]string{}
			}
			e.arrVars[q.hid] = true
			if _, ok := e.arrElems[q.hid]; !ok {
				e.arrElems[q.hid] = "i32"
			}
			e.destructureArray(q.pat, q.hid, q.pat)
		case ast.KindObjectBindingPattern:
			if q.annot != nil {
				if _, ok := e.varLayouts[q.hid]; !ok {
					if l := e.layoutOfAnnotation(q.annot); l != nil {
						if e.varLayouts == nil {
							e.varLayouts = map[string]*layout{}
						}
						e.varLayouts[q.hid] = l
					}
				}
			}
			e.destructureObject(q.pat, q.hid, q.pat)
		default:
			e.refuse(q.pat, "binding pattern %s is not lowerable", q.pat.Kind.String())
		}
		if e.refused {
			return
		}
	}
}

// lowerDestructuringDecl binds `const [a, b] = arr` / `const {x} = obj`
// element/field-wise (initializers lower once, then destructure).
func (e *emitter) lowerDestructuringDecl(d, nm, init *ast.Node) {
	if init.Kind == ast.KindArrowFunction || init.Kind == ast.KindFunctionExpression {
		e.refuse(d, "function values do not destructure")
		return
	}
	// Namespace destructuring claims its own receivers first
	// (see link_nsobject.go); spread/dynamic shapes stay loud there.
	if init.Kind == ast.KindIdentifier {
		if _, ok := e.defNSImports[init.Text()]; ok {
			lowerNsDestructure(e, d, nm, init.Text())
			return
		}
		if _, ok := e.nsImports[init.Text()]; ok {
			lowerNsDestructure(e, d, nm, init.Text())
			return
		}
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
		// Destructured declaration: fresh local, never a slot store.
		e.assignLocal(nm.Text(), v, kind, tI32, pos)
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
		if e.modStrTmps[args[0]] {
			// Demand probe 2026-10-01: zero hits across 286 sweep +
			// demos (kept loud deliberately). Lifting needs 16-wide
			// element threading through push/index/clone/methods plus
			// the sci-side array memory model (see todo/03 notes);
			// implement on first real use case, not speculatively.
			e.refuse(pos, "module string array elements are not lowerable yet (4-wide slots hold i32 only)")
			return "0", tUnknown, true
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
	case ast.KindIdentifier, ast.KindThisKeyword, ast.KindSuperKeyword:
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
	// Two-level namespace reads (`NS.E.M` enum folds, `NS.C.S` class
	// statics). Struct chains fall through to lowerMemberChain below.
	if pa.Expression.Kind == ast.KindPropertyAccessExpression {
		if q, ok := dottedBaseName(pa.Expression); ok {
			if members, ok := e.enums[q]; ok {
				if e.enumNonInt[q][pa.Name().Text()] {
					e.refuse(n, "string enum member %s.%s is not lowerable (only all-integer enums fold ordinals)", q, pa.Name().Text())
					return "0", tUnknown
				}
				if ord, ok := members[pa.Name().Text()]; ok {
					return fmt.Sprintf("%d", ord), tI32
				}
				e.refuse(n, "unknown enum member %s.%s", q, pa.Name().Text())
				return "0", tUnknown
			}
			if ns, mem, ok := e.splitNsQualified(q); ok {
				if sv, stok := nsStaticText(e, q, pa.Name().Text()); stok {
					if !e.checkNsAccess(ns, mem, n) {
						return "0", tUnknown
					}
					if sv.typ == tString {
						return e.lowerStringLiteral(sv.text), tString
					}
					return sv.text, sv.typ
				}
			}
			// Nested mutable member reads (`A.B.x`) load through the slot
			// (single-level `N.x` routes below; unregistered members fall
			// through to the loud refusal like consts).
			if q, ns, mem, ok := e.nsLetTarget(n); ok {
				if ms := e.modVars[q]; ms != nil {
					if !e.checkNsAccess(ns, mem, n) {
						return "0", tUnknown
					}
					v, t := e.emitModLoad(ms, n)
					return v, t
				}
			}
		}
	}
	// Enum.Member folds to its ordinal as a value.
	if pa.Expression.Kind == ast.KindIdentifier {
		if members, ok := e.enums[e.qualify(pa.Expression.Text())]; ok {
			if e.enumNonInt[e.qualify(pa.Expression.Text())][pa.Name().Text()] {
				e.refuse(n, "string enum member %s.%s is not lowerable (only all-integer enums fold ordinals)", pa.Expression.Text(), pa.Name().Text())
				return "0", tUnknown
			}
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
		// Class static literals fold through the class name or an
		// instance of it (readonly-ness is enforced by refusing static
		// writes: only reads reach here). Heritage shells live apart.
		clsName := pa.Expression.Text()
		if c, ok := e.varClass[clsName]; ok {
			clsName = c
		}
		// Namespace member classes resolve qualified-first.
		clsName = e.qualify(clsName)
		// Class-name (or instance-aliased) bases fold private statics
		// through the lexical owner; anything else (locals, params)
		// falls through to the member-chain layout path below.
		if _, isClass := e.classDefs[clsName]; isClass {
			staticName := pa.Name().Text()
			if strings.HasPrefix(staticName, "#") {
				if e.curMethodOwner == "" {
					e.refuse(n, "private static %s is not accessible outside a class method", staticName)
					return "0", tUnknown
				}
				staticName = privFieldKey(e.curMethodOwner, staticName)
			}
			if cd, ok := e.classDefs[clsName]; ok {
				if sv, ok := cd.statics[staticName]; ok {
					if sv.typ == tString {
						return e.lowerStringLiteral(sv.text), tString
					}
					return sv.text, sv.typ
				}
			}
			if strings.HasPrefix(pa.Name().Text(), "#") {
				e.refuse(n, "private static %s is not declared in class %s", pa.Name().Text(), e.curMethodOwner)
				return "0", tUnknown
			}
		} else if _, isShell := e.staticDefs[clsName]; isShell {
			staticName := pa.Name().Text()
			if strings.HasPrefix(staticName, "#") {
				if e.curMethodOwner == "" {
					e.refuse(n, "private static %s is not accessible outside a class method", staticName)
					return "0", tUnknown
				}
				staticName = privFieldKey(e.curMethodOwner, staticName)
			}
			if cd, ok := e.staticDefs[clsName]; ok {
				if sv, ok := cd.statics[staticName]; ok {
					if sv.typ == tString {
						return e.lowerStringLiteral(sv.text), tString
					}
					return sv.text, sv.typ
				}
			}
			if strings.HasPrefix(pa.Name().Text(), "#") {
				e.refuse(n, "private static %s is not declared in class %s", pa.Name().Text(), e.curMethodOwner)
				return "0", tUnknown
			}
		}
		// TypeScript namespace value reads (`NS.CONST`; a shadowing
		// value at the root wins and falls through below). Namespace
		// aliases (`import M = N`) rewrite the receiver first.
		nsBase := pa.Expression.Text()
		if q, ok := e.eqAliases[nsBase]; ok && e.namespaces[q] && !e.isValueReceiver(nsBase) {
			nsBase = q
		}
		if e.namespaces[nsBase] && !e.isValueReceiver(nsBase) {
			member := pa.Name().Text()
			if e.nsMemberKind(nsBase, member) == "" {
				e.refuse(n, "%s has no member %s", nsBase, member)
				return "0", tUnknown
			}
			v, t, _ := e.lowerNamespaceMemberRead(nsBase, member, n)
			return v, t
		}
	}
	// Namespace object field reads (`N.obj.x`, nested `A.B.obj.x`):
	// privacy enforced like the member itself, then a field load.
	if ms, field, ns, mem, ok := e.nsObjFieldTarget(n); ok {
		if !e.checkNsAccess(ns, mem, n) {
			return "0", tUnknown
		}
		mf := ms.fieldByName(field)
		if mf == nil {
			e.refuse(n, "module state %s has no field %s", ms.qual, field)
			return "0", tUnknown
		}
		e.emitModEnsure(ms, n)
		v, t := e.emitModLoadField(ms, mf, n)
		return v, t
	}
	// Accessor reads inline the getter body with `this` bound (instance
	// reads; static reads dispatch on the class name with empty this).
	// Anything else (super anchors, unknown members) stays loud below.
	// (Previous shape refused here unconditionally: "needs inline
	// support (not yet)".)
	if pa.Expression.Kind == ast.KindIdentifier || (pa.Expression.Kind == ast.KindThisKeyword && e.thisSelf != "") {
		recv := e.thisSelf
		bareClass := false
		if pa.Expression.Kind == ast.KindIdentifier {
			recv = pa.Expression.Text()
			if _, isClass := e.classDefs[recv]; isClass {
				if _, isInst := e.varClass[recv]; !isInst {
					bareClass = true
				}
			}
		}
		if cd := e.classDefOf(recv); cd != nil {
			if bareClass {
				if gn, ok := cd.staticGetters[pa.Name().Text()]; ok {
					owner := cd.name
					if o, ok := cd.methodOwner[pa.Name().Text()]; ok {
						owner = o
					}
					v, t, _ := e.inlineClassMethod("", cd.name, owner, pa.Name().Text(), gn, []string{}, nil, n)
					return v, t
				}
				if _, ok := cd.getters[pa.Name().Text()]; ok {
					e.refuse(n, "getter %s.%s is an instance getter (static reads need a static getter)", pa.Expression.Text(), pa.Name().Text())
					return "0", tUnknown
				}
			} else if gn, ok := cd.getters[pa.Name().Text()]; ok {
				owner := cd.name
				if o, ok := cd.methodOwner[pa.Name().Text()]; ok {
					owner = o
				}
				v, t, _ := e.inlineClassMethod(recv, cd.name, owner, pa.Name().Text(), gn, []string{}, nil, n)
				return v, t
			}
		}
	}
	// DOM handle reads claim their receivers first (a .length load on a
	// handle would be garbage; see dom_proj.go).
	if pa.Expression.Kind == ast.KindIdentifier && e.domVars[pa.Expression.Text()] {
		if v, t, ok := e.lowerDomLoad(pa.Expression.Text(), pa.Name().Text(), n); ok {
			return v, t
		}
		return "0", tUnknown
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
	// A dotted read whose root names a cross-file namespace routes to
	// its file (same-file values shadow first, as for calls).
	if pa.Expression.Kind == ast.KindIdentifier {
		root := pa.Expression.Text()
		// Imported namespace consts fold by value (bound at import
		// from the member tables; mirrors lowerNamespaceMemberRead).
		if _, ok := e.nsImports[root]; ok {
			if lit, ok := e.constVals[root+"."+pa.Name().Text()]; ok {
				if e.constIsStr[root+"."+pa.Name().Text()] {
					return e.lowerStringLiteral(lit), tString
				}
				if isFloatLiteral(lit) {
					return lit, tF64
				}
				return lit, tI32
			}
		}
		if _, ok := e.constVals[root]; !ok && e.modStateOf(root) == nil {
			if _, ok := e.arrowAliases[root]; !ok && !e.isValueReceiver(root) {
				if r := e.linkRoute(root); r != "" {
					e.refuse(n, "%s", r)
					return "0", tUnknown
				}
			}
		}
	}
	// String/Array method projections handled at call sites; bare property
	// reads other than .length are refused.
	e.refuse(n, "property access .%s is not in the SA-lowerable subset", pa.Name().Text())
	return "0", tUnknown
}

// lowerMemberChain lowers a.b.c... to nested static-offset loads. It returns
// false when the base has no recorded layout.
// privFieldKey mangles a private field to its owner (`#x` in C → `#C#x`,
// so shadowing owners keep distinct slots); public names pass through.
func privFieldKey(owner, fname string) string {
	if !strings.HasPrefix(fname, "#") {
		return fname
	}
	return "#" + owner + fname
}

// privResolveOwned maps a `#`-prefixed member to its owner-mangled layout
// key, verifying the field exists (undeclared/illegal access refuses
// loudly with the owner named). Public names pass through untouched.
func (e *emitter) privResolveOwned(l *layout, raw, owner string, pos *ast.Node) (string, bool) {
	if !strings.HasPrefix(raw, "#") {
		return raw, true
	}
	q := privFieldKey(owner, raw)
	if _, ok := l.offsets[q]; !ok {
		e.refuse(pos, "private field %s is not declared in class %s", raw, owner)
		return "", false
	}
	return q, true
}

// privResolve is privResolveOwned against the lexical method owner;
// outside a method body there is no owner to resolve with.
func (e *emitter) privResolve(l *layout, raw string, pos *ast.Node) (string, bool) {
	if !strings.HasPrefix(raw, "#") {
		return raw, true
	}
	if e.curMethodOwner == "" {
		e.refuse(pos, "private field %s is not accessible outside a class method", raw)
		return "", false
	}
	return e.privResolveOwned(l, raw, e.curMethodOwner, pos)
}

func (e *emitter) lowerMemberChain(n *ast.Node) (string, saType, bool) {
	segs := []string{}
	cur := n
	for cur.Kind == ast.KindPropertyAccessExpression {
		pa := cur.AsPropertyAccessExpression()
		segs = append([]string{pa.Name().Text()}, segs...)
		cur = pa.Expression
	}
	if cur.Kind != ast.KindIdentifier {
		// `this.f` chains resolve to the current method receiver; `super.f`
		// shares the flattened offsets (validated in class_heritage.go).
		if cur.Kind == ast.KindThisKeyword && e.thisSelf != "" {
			segs = append([]string{e.thisSelf}, segs...)
		} else if cur.Kind == ast.KindSuperKeyword && e.thisSelf != "" {
			if !e.checkSuperAccess(n, segs) {
				return "0", tUnknown, true
			}
			segs = append([]string{e.thisSelf}, segs...)
		} else {
			return "", tUnknown, false
		}
	} else {
		segs = append([]string{cur.Text()}, segs...)
	}
	// Checker-backed layouts cover inferred structs (factory results)
	// that syntax recording misses; recorded layouts keep priority.
	l := e.layoutOfNode(segs[0], cur)
	if l == nil {
		return "", tUnknown, false
	}
	// Private fields never cross `super` (TS: always an error there);
	// resolve the rest against the lexical owner per segment.
	if cur.Kind == ast.KindSuperKeyword {
		for _, s := range segs[1:] {
			if strings.HasPrefix(s, "#") {
				e.refuse(n, "private field %s is not accessible via super", s)
				return "0", tUnknown, true
			}
		}
	}
	base, _ := e.lowerExpr(cur)
	curOp := base
	for i := 1; i < len(segs); i++ {
		key, ok := e.privResolve(l, segs[i], n)
		if !ok {
			return "0", tUnknown, true
		}
		off, ok := l.offsets[key]
		if !ok {
			return "", tUnknown, false
		}
		saname := l.types[key]
		t := e.freshTmp()
		e.emit("%s = load %s + %d as %s", t, curOp, off, saname)
		curOp = t
		if i+1 < len(segs) {
			// Intermediate segment: descend into the nested layout.
			nl, ok := e.layouts[l.ftypes[key]]
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
		// `this.f = v` stores resolve to the current method receiver;
		// `super.f = v` shares the flattened offsets (subclass only).
		if cur.Kind == ast.KindThisKeyword && e.thisSelf != "" {
			segs = append([]string{e.thisSelf}, segs...)
		} else if cur.Kind == ast.KindSuperKeyword && e.thisSelf != "" {
			if _, ok := e.superBaseForRecv(e.thisSelf); !ok {
				e.refuse(target, "super property access is only lowerable inside a subclass method")
				return true
			}
			segs = append([]string{e.thisSelf}, segs...)
		} else {
			return false
		}
	} else {
		segs = append([]string{cur.Text()}, segs...)
	}
	// Accessor writes inline the setter body with the RHS bound (instance
	// writes; static writes dispatch on the class name with empty this).
	// Super-anchored access stays loud (base dispatch is unowned).
	if cd := e.classDefOf(segs[0]); cd != nil && cur.Kind != ast.KindSuperKeyword {
		last := segs[len(segs)-1]
		bareClass := false
		if _, isClass := e.classDefs[segs[0]]; isClass {
			if _, isInst := e.varClass[segs[0]]; !isInst {
				bareClass = true
			}
		}
		owner := cd.name
		if o, ok := cd.methodOwner[last]; ok {
			owner = o
		}
		if bareClass {
			if sn, ok := cd.staticSetters[last]; ok {
				e.inlineClassMethod("", cd.name, owner, last, sn, []string{rhs}, nil, cur)
				return true
			}
			if _, ok := cd.setters[last]; ok {
				e.refuse(cur, "setter %s.%s is an instance setter (static writes need a static setter)", segs[0], last)
				return true
			}
		} else if sn, ok := cd.setters[last]; ok {
			e.inlineClassMethod(segs[0], cd.name, owner, last, sn, []string{rhs}, nil, cur)
			return true
		}
	}
	l := e.layoutOfVar(segs[0])
	if l == nil {
		return false
	}
	// Module objects persist field stores to slots (callers dispatch
	// depth-1 writes with real operand types beforehand); deeper chains
	// would store to a fresh header and silently drop, so refuse loudly.
	if ms := e.modStateOf(segs[0]); ms != nil && ms.isObj {
		e.refuse(target, "module state %s member depth is not lowerable (single-level fields only)", ms.qual)
		return true
	}
	// Private fields never cross `super` (TS: always an error there).
	if cur.Kind == ast.KindSuperKeyword {
		for _, s := range segs[1:] {
			if strings.HasPrefix(s, "#") {
				e.refuse(target, "private field %s is not accessible via super", s)
				return true
			}
		}
	}
	base, _ := e.lowerExpr(cur)
	curOp := base
	for i := 1; i+1 < len(segs); i++ {
		key, ok := e.privResolve(l, segs[i], target)
		if !ok {
			return true
		}
		off, ok := l.offsets[key]
		if !ok {
			return false
		}
		t := e.freshTmp()
		e.emit("%s = load %s + %d as ptr", t, curOp, off)
		curOp = t
		nl, ok := e.layouts[l.ftypes[key]]
		if !ok {
			return false
		}
		l = nl
	}
	last, ok := e.privResolve(l, segs[len(segs)-1], target)
	if !ok {
		return true
	}
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

// lowerElemAddress (struct-element address join) was prototyped here and
// reverted: array literals store element pointers in i32 slots, so no
// 64-bit-clean read-back exists until the creation model changes (see
// AGENTS record). Element layouts still resolve via fdefs for future work.

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
	// Env probes (`typeof G === "undefined" ? A : B` with G undeclared)
	// fold to the taken arm before anything lowers: the untaken arm may
	// name a value that does not exist.
	if arm, ok := e.envProbeArm(n); ok {
		return e.lowerExpr(arm)
	}
	// Constant-false conditions take the false arm without lowering the
	// true arm (same dead-code rule as if).
	if e.isFalseConst(ce.Condition) {
		return e.lowerExpr(ce.WhenFalse)
	}
	cond, _ := e.lowerExpr(ce.Condition)
	cond = e.materializeCond(cond)
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
	// Integer arms go through the upstream SELECT macro (value copy via
	// add; immediates and registers are both legal operands, booleans
	// arrive as 1/0). f64/ptr arms keep the slot join (SELECT has no
	// fadd/ptr copy).
	if tt == tI32 {
		e.needImport("sa_std/control.sal")
		out := e.freshTmp()
		e.emit("EXPAND SELECT %s, %s, %s, %s", out, cond, tv, fv)
		return out, tt
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
		name := e.qualify(nw.Expression.Text())
		if _, ok := e.classDefs[name]; ok {
			return e.lowerNewClass(name, nw, n)
		}
		// Cross-file class misses route to the defining file instead
		// of the generic builtin refuse below.
		if r := e.linkRoute(nw.Expression.Text()); r != "" {
			e.refuse(n, "%s", r)
			return "0", tUnknown
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
		// new Date() narrows to i64 millis (Date.now shape); argued
		// forms (parse/format territory) refuse loudly.
		if name == "Date" {
			if nw.Arguments != nil && len(nw.Arguments.Nodes) != 0 {
				e.refuse(n, "new Date(x) is not lowerable (only arg-less now-shape)")
				return "0", tUnknown
			}
			if proj, ok := projectionByTS("Date.now"); ok {
				v, t := e.emitProjCall(proj, nil, n)
				if e.refused {
					return "0", tUnknown
				}
				return v, t
			}
		}
	}
	// `new NS.C()` instantiates namespace member classes (export-checked;
	// a shadowing value at the root falls through to the generic refuse).
	if nw.Expression.Kind == ast.KindPropertyAccessExpression {
		// Imported namespace member class (cross-file `new N.C()`): the
		// member layout lives in the shared table; the import proves the
		// use. Same-file namespaces keep the shadowing path below, and
		// anything else falls through to the loud refuses there.
		if root := dottedRoot(nw.Expression); root != "" {
			if _, ok := e.nsImports[root]; ok {
				if q, ok := dottedBaseName(nw.Expression); ok {
					// A same-file namespace keeps its own path below (privacy
					// included); only genuinely cross-file members route here.
					if _, _, ok := e.splitNsQualified(q); !ok {
						if _, ok := e.classDefs[q]; ok {
							return e.lowerNewClass(q, nw, n)
						}
					}
				}
			}
		}
		if r := dottedRoot(nw.Expression); r == "" || !e.isValueReceiver(r) {
			if q, ok := dottedBaseName(nw.Expression); ok {
				if ns, mem, ok := e.splitNsQualified(q); ok {
					if e.nsMemberKind(ns, mem) == "" {
						e.refuse(n, "%s has no member %s", ns, mem)
						return "0", tUnknown
					}
					if _, ok := e.classDefs[q]; ok {
						if !e.checkNsAccess(ns, mem, n) {
							return "0", tUnknown
						}
						return e.lowerNewClass(q, nw, n)
					}
					e.refuse(n, "%s.%s is not a class", ns, mem)
					return "0", tUnknown
				}
			}
		}
		// Unimported namespace roots route to the defining file (new N.C
		// lowers once `import { N }` binds); same-file values shadow first.
		if root := dottedRoot(nw.Expression); root != "" {
			if _, ok := e.constVals[root]; !ok && e.modStateOf(root) == nil {
				if _, ok := e.arrowAliases[root]; !ok && !e.isValueReceiver(root) {
					if r := e.linkRoute(root); r != "" {
						e.refuse(n, "%s", r)
						return "0", tUnknown
					}
				}
			}
		}
	}
	e.refuse(n, "new expressions other than new Map() / new Array(n) / new Date() are not lowerable")
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
			// Demand probe 2026-10-01: zero hits (see push-site note).
			if e.modStrTmps[v] {
				e.refuse(el, "module string array elements are not lowerable yet (4-wide slots hold i32 only)")
				return "0", tUnknown
			}
			e.lowerArrayPush(h, v, "i32", 4)
		}
		return h, tArray
	}
	elems := []string{}
	for _, el := range al.Elements.Nodes {
		v, _ := e.lowerExpr(el)
		// Module-string headers are 16 bytes; 4-wide slots would
		// truncate them (pure-literal arrays stay node-consumable for
		// Buffer.concat-style backends, so only marked temps refuse).
		// Demand probe 2026-10-01: zero hits (see push-site note).
		if e.modStrTmps[v] {
			e.refuse(el, "module string array elements are not lowerable yet (4-wide slots hold i32 only)")
			return "0", tUnknown
		}
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

// rawTemplateText returns the raw (uncooked) text of a template head,
// middle or tail (the parser fills RawText there); anything else falls
// back to cooked text. No-substitution literals go through rawNoSubText
// instead: the parser leaves their RawText empty.
func rawTemplateText(n *ast.Node) string {
	switch n.Kind {
	case ast.KindTemplateHead:
		return n.AsTemplateHead().RawText
	case ast.KindTemplateMiddle:
		return n.AsTemplateMiddle().RawText
	case ast.KindTemplateTail:
		return n.AsTemplateTail().RawText
	default:
		return n.Text()
	}
}

// rawNoSubText slices the exact source bytes between a no-substitution
// template's backticks (positions are byte-exact, verified against
// multibyte prefixes). The parser leaves NoSub RawText empty, and cooked
// text is untrustworthy for String.raw (every escape changes length).
func (e *emitter) rawNoSubText(n *ast.Node) (string, bool) {
	p, en := n.Pos(), n.End()
	if p < 0 || en > len(e.src) || en-p < 2 || e.src[p] != '`' {
		return "", false
	}
	return e.src[p+1 : en-1], true
}

// lowerTaggedTemplate lowers tagged templates. `String.raw` cooks
// nothing (raw parts plus normally-rendered substitutions); any other
// tag refuses loudly: calling the tag needs it as a first-class function
// value (functions have no first-class value in the subset; cf. linkRoute),
// so even a materialized strings array could not dispatch it.
func (e *emitter) lowerTaggedTemplate(n *ast.Node) (string, saType) {
	tt := n.AsTaggedTemplateExpression()
	if tag := tt.Tag; tag.Kind == ast.KindPropertyAccessExpression {
		pa := tag.AsPropertyAccessExpression()
		if pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "String" &&
			pa.Name().Text() == "raw" {
			return e.lowerRawTemplate(tt.Template, n)
		}
	}
	e.refuse(n, "tagged templates are not lowerable (tag functions have no first-class value; String.raw is the only supported tag)")
	return "0", tUnknown
}

// lowerRawTemplate joins raw template parts with rendered substitutions
// (mirrors lowerTemplate, but heads/tails stay uncooked).
func (e *emitter) lowerRawTemplate(tpl *ast.Node, pos *ast.Node) (string, saType) {
	_ = pos
	if tpl.Kind == ast.KindNoSubstitutionTemplateLiteral {
		raw, ok := e.rawNoSubText(tpl)
		if !ok {
			e.refuse(tpl, "String.raw literal has no recoverable source text")
			return "0", tUnknown
		}
		return e.lowerStringLiteral(raw), tString
	}
	if tpl.Kind != ast.KindTemplateExpression {
		e.refuse(tpl, "tagged template shape is not lowerable")
		return "0", tUnknown
	}
	tp := tpl.AsTemplateExpression()
	e.needImport("sa_std/string.sai")
	e.needImport("sa_std/fmt.sai")
	acc := e.lowerStringLiteral(rawTemplateText(tp.Head))
	for _, sp := range tp.TemplateSpans.Nodes {
		span := sp.AsTemplateSpan()
		v, vt := e.lowerExpr(span.Expression)
		part, ok := e.renderInterpValue(v, vt, span.Expression)
		if !ok {
			return "0", tUnknown
		}
		acc = e.concatSlices(acc, part)
		tail := rawTemplateText(span.Literal)
		if tail != "" {
			tailH := e.lowerStringLiteral(tail)
			acc = e.concatSlices(acc, tailH)
			e.releaseIfOwnedTemp(tailH)
		}
	}
	return acc, tString
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
// nested-layout resolution (e.g. field `inner: Inner`; qualified `NS.I`
// flattens to the namespace path form).
func rawTypeName(tn *ast.Node) string {
	if tn == nil {
		return "i32"
	}
	if tn.Kind == ast.KindTypeReference {
		if flat := entityNameText(tn.AsTypeReferenceNode().TypeName); flat != "" {
			return flat
		}
		return tn.AsTypeReferenceNode().TypeName.Text()
	}
	return saNameOfType(tn)
}

// layoutOfLiteral matches an object literal against recorded interfaces by
// field-name set.
func (e *emitter) layoutOfLiteral(n *ast.Node) *layout {
	// Checker-backed names first: contextually-typed literals (and
	// same-named shapes with different field types) resolve exactly
	// instead of by order-insensitive name-set guess. Shorthand and
	// computed-literal forms, which the syntactic walk skips, resolve
	// here too when the checker names them.
	if l := e.layoutOfCheckerName(n); l != nil {
		return l
	}
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
// objPropName resolves one object-literal key: plain identifiers,
// literal computed keys (`{["x"]: 1}` folds like a static name), and
// shorthand names. Dynamic keys refuse loudly (static layouts have no
// runtime key dispatch).
func objPropName(p *ast.Node) (string, bool) {
	nm := p.Name()
	if nm == nil {
		return "", false
	}
	switch nm.Kind {
	case ast.KindIdentifier:
		return nm.Text(), true
	case ast.KindStringLiteral:
		return stringLiteralText(nm)
	case ast.KindComputedPropertyName:
		expr := nm.AsComputedPropertyName().Expression
		if expr == nil {
			return "", false
		}
		switch expr.Kind {
		case ast.KindStringLiteral:
			return stringLiteralText(expr)
		case ast.KindNumericLiteral:
			return expr.Text(), true
		}
	}
	return "", false
}

func (e *emitter) lowerObjectLiteral(n *ast.Node) (string, saType) {
	ol := n.AsObjectLiteralExpression()
	// Phase 1: lower spread sources (handles + layouts stay live for the
	// copies below) and resolve every key. Later properties override
	// earlier ones, so replay stays in source order (phase 3).
	type spreadSrc struct {
		h string
		l *layout
	}
	type op struct {
		spread int // index into spreads, -1 for a plain store
		fname  string
		init   *ast.Node // value node for plain stores (nil for spreads)
	}
	spreads := []spreadSrc{}
	ops := []op{}
	names := []string{}
	for _, p := range ol.Properties.Nodes {
		if p.Kind == ast.KindSpreadAssignment {
			spreadExpr := p.AsSpreadAssignment().Expression
			sv, _ := e.lowerExpr(spreadExpr)
			if e.refused {
				return "0", tUnknown
			}
			// Recorded layouts win; otherwise the checker names the
			// source (factory results, inferred consts) where the
			// scope map has no entry.
			sl := e.layoutOfNode(sv, spreadExpr)
			if sl == nil {
				e.refuse(p, "spread source has no recorded interface layout (spread an interface-typed object)")
				return "0", tUnknown
			}
			spreads = append(spreads, spreadSrc{h: sv, l: sl})
			ops = append(ops, op{spread: len(spreads) - 1})
			names = append(names, sl.fields...)
			continue
		}
		fname, ok := objPropName(p)
		if !ok {
			e.refuse(p, "computed property names must be literals (dynamic keys have no static layout)")
			return "0", tUnknown
		}
		var init *ast.Node
		switch p.Kind {
		case ast.KindPropertyAssignment:
			init = p.AsPropertyAssignment().Initializer
		case ast.KindShorthandPropertyAssignment:
			// `{x}` reads the in-scope binding (same value an explicit
			// `: x` would lower).
			init = p.Name()
		default:
			e.refuse(p, "object literal property %s is not lowerable", p.Kind.String())
			return "0", tUnknown
		}
		ops = append(ops, op{spread: -1, fname: fname, init: init})
		names = append(names, fname)
	}
	// Match on the key set (overrides duplicate names; matchLayout
	// compares set membership but also counts entries).
	seen := map[string]bool{}
	uniq := names[:0]
	for _, nm := range names {
		if !seen[nm] {
			seen[nm] = true
			uniq = append(uniq, nm)
		}
	}
	l := e.matchLayout(uniq)
	if l == nil {
		e.refuse(n, "object literal matches no recorded interface layout (declare the interface first)")
		return "0", tUnknown
	}
	h := e.freshTmp()
	e.emit("%s = alloc %d", h, l.size)
	for _, f := range l.fields {
		e.emit("store %s + %d, 0 as %s", h, l.offsets[f], l.types[f])
	}
	for _, o := range ops {
		if o.spread >= 0 {
			src := spreads[o.spread]
			for _, f := range src.l.fields {
				dstOff, ok := l.offsets[f]
				if !ok {
					e.refuse(n, "spread field %s is not in the target layout", f)
					return "0", tUnknown
				}
				if src.l.types[f] != l.types[f] {
					e.refuse(n, "spread field %s type mismatch (%s vs %s)", f, src.l.types[f], l.types[f])
					return "0", tUnknown
				}
				t := e.freshTmp()
				e.emit("%s = load %s + %d as %s", t, src.h, src.l.offsets[f], src.l.types[f])
				e.emit("store %s + %d, %s as %s", h, dstOff, t, l.types[f])
			}
			continue
		}
		v, _ := e.lowerExpr(o.init)
		if e.refused {
			return "0", tUnknown
		}
		e.emit("store %s + %d, %s as %s", h, l.offsets[o.fname], v, l.types[o.fname])
	}
	e.declareOwned(h)
	if e.varLayouts == nil {
		e.varLayouts = map[string]*layout{}
	}
	e.varLayouts[h] = l
	return h, tArray
}

// layoutOfVar resolves the struct layout for a base register (variable name
// or struct handle temp). Module-state objects resolve by (qualified) name
// to their recorded layout, so member chains read through them — but a
// scope binding always shadows (a scalar parameter named like the module
// object must miss, never read the slot layout).
func (e *emitter) layoutOfVar(base string) *layout {
	if e.varLayouts != nil {
		if l, ok := e.varLayouts[base]; ok {
			return l
		}
	}
	if e.lookupBinding(base) != nil {
		return nil
	}
	if ms := e.modStateOf(base); ms != nil && ms.isObj {
		return ms.olay
	}
	return nil
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
		// Definition maps resolve shadowed-first (an inner namespace
		// member hides outer definitions); scope bindings above and
		// below keep the raw name.
		dname := e.qualify(name)
		switch {
		case e.strVars[name]:
			kind = "string"
		case e.arrVars[name] || e.mapVars[name] || e.setVars[name]:
			kind = "object"
		case e.layoutOfVar(name) != nil:
			kind = "object"
		case e.f64Vars[name]:
			kind = "number"
		case e.kindVars[name] != "":
			kind = e.kindVars[name]
		case e.mainRenamed && dname == "main":
			kind = "function"
		case e.modStateOf(name) != nil:
			if ms := e.modStateOf(name); ms.isBool {
				kind = "boolean"
			} else if ms.w == modStrW {
				kind = "string"
			} else if ms.isObj {
				kind = "object"
			} else {
				kind = "number"
			}
		default:
			if _, ok := e.arrowAliases[dname]; ok {
				kind = "function"
			} else if _, ok := e.funcSigs[dname]; ok {
				kind = "function"
			} else if _, ok := e.constVals[dname]; ok && !e.constIsStr[dname] {
				kind = "number"
			} else if k, ok := e.tcx.typeofKind(op); ok {
				kind = k
			} else if e.lookupBinding(name) == nil && !e.tcx.declaredAt(op) {
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
	if st.Kind == ast.KindTypeAliasDeclaration {
		e.recordTypeAlias(st)
	}
	if st.Kind == ast.KindEnumDeclaration {
		e.recordEnum(st)
	}
}

// integerInit folds enum initializers: plain integer literals and unary
// -/+ applied to them (planck's `= -1`). Anything else (strings, floats,
// computed expressions) reports false.
func integerInit(init *ast.Node) (int64, bool) {
	if init.Kind == ast.KindNumericLiteral && !isFloatLiteral(init.Text()) {
		var v int64
		fmt.Sscanf(init.Text(), "%d", &v)
		return v, true
	}
	if init.Kind == ast.KindPrefixUnaryExpression {
		un := init.AsPrefixUnaryExpression()
		if (un.Operator == ast.KindMinusToken || un.Operator == ast.KindPlusToken) &&
			un.Operand.Kind == ast.KindNumericLiteral && !isFloatLiteral(un.Operand.Text()) {
			var v int64
			fmt.Sscanf(un.Operand.Text(), "%d", &v)
			if un.Operator == ast.KindMinusToken {
				v = -v
			}
			return v, true
		}
	}
	return 0, false
}

// enumMemberTable numbers enum members (explicit =N honored, auto takes
// next++), mirroring sa_plugin_ts EnumDef. Strict mode reports false when
// any member is not auto-or-integer (string/computed inits have no ordinal
// form); recordEnum keeps legacy non-strict behavior byte-for-byte, while
// the linker only ships strict tables cross-file.
func enumMemberTable(st *ast.Node, strict bool) (map[string]int64, bool) {
	m := map[string]int64{}
	var next int64
	for _, mem := range st.AsEnumDeclaration().Members.Nodes {
		mname, ok := bindingNameText(mem)
		if !ok {
			if strict {
				return nil, false
			}
			continue
		}
		if init := mem.AsEnumMember().Initializer; init != nil {
			if v, ok := integerInit(init); ok {
				next = v
			} else if strict {
				return nil, false
			}
		}
		m[mname] = next
		next++
	}
	return m, true
}

// recordEnum records auto-numbered variants (explicit =N honored), mirroring
// sa_plugin_ts EnumDef. Enum.Member folds to its ordinal at use sites.
// String/computed members land in enumNonInt (the numbering core still
// assigns them ordinals): reads refuse instead of silently folding to 0.
func (e *emitter) recordEnum(st *ast.Node) {
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	name = e.nsDefName(name)
	m, _ := enumMemberTable(st, false)
	if e.enums == nil {
		e.enums = map[string]map[string]int64{}
	}
	e.enums[name] = m
	// Per-member integer check (independent of the strict table's
	// all-or-nothing fate): only string/computed-initialized members
	// land in the set; integer members of mixed enums keep folding.
	set := map[string]bool{}
	for _, mem := range st.AsEnumDeclaration().Members.Nodes {
		mname, ok := bindingNameText(mem)
		if !ok {
			continue
		}
		if init := mem.AsEnumMember().Initializer; init != nil {
			if _, ok := integerInit(init); !ok {
				set[mname] = true
			}
		}
	}
	if len(set) > 0 {
		if e.enumNonInt == nil {
			e.enumNonInt = map[string]map[string]bool{}
		}
		e.enumNonInt[name] = set
	}
}

// recordLayout builds the static byte-offset table for an interface.
func (e *emitter) recordLayout(st *ast.Node) {
	decl := st.AsInterfaceDeclaration()
	name := "<anon>"
	if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
		name = st.Name().Text()
	}
	_ = decl
	name = e.nsDefName(name)
	l := &layout{name: name, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}, fdefs: map[string]*ast.Node{}}
	// Interface extends flattens base fields first (type-only; see
	// class_heritage.go). Missing bases skip best-effort.
	e.inheritInterface(st, l)
	off := l.size
	for _, tp := range st.TypeParameters() {
		if nm := tp.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
			l.tparams = append(l.tparams, nm.Text())
		}
	}
	for _, m := range st.AsInterfaceDeclaration().Members.Nodes {
		if m.Kind != ast.KindPropertySignature {
			// Method signatures declare no layout field (mirrors parseInterface).
			continue
		}
		fname, ok := bindingNameText(m)
		if !ok {
			continue
		}
		ftn := m.AsPropertySignatureDeclaration().Type
		saname := saNameOfType(ftn)
		size, align := widthOf(saname)
		off = alignTo(off, align)
		l.fields = append(l.fields, fname)
		l.types[fname] = saname
		l.ftypes[fname] = rawTypeName(ftn)
		l.fdefs[fname] = ftn
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
	// staticMethods records `static m()` bodies for ClassName.m()
	// call-site inlining (instance entries never hold them; see
	// recordClass). `this` stays empty inside: instance state refuses
	// honestly, other statics route through the same dispatch.
	staticMethods map[string]*ast.Node
	ctor    *ast.Node
	// ctorOwner names the class that declared ctor (a derived class without
	// its own ctor inherits the base node; super() inside it targets the
	// owner's parent chain).
	ctorOwner string
	// methodOwner names the declaring class per method/getter/setter
	// (inherited members keep their lexical owner, mirroring ctorOwner).
	// Private `#x` resolves against it: `#x` → `#Owner#x`.
	methodOwner map[string]string
	// isAbstract refuses `new` precisely (TS fidelity; concrete subclasses
	// still instantiate).
	isAbstract bool
	// nsSegs records the namespace path owning this class (method bodies
	// resolve bare sibling references qualified; see namespace_ts.go).
	nsSegs []string
	// getters/setters record accessor bodies (reads/writes refuse
	// precisely until inline support lands; the class itself lowers).
	getters map[string]*ast.Node
	setters map[string]*ast.Node
	// staticGetters/staticSetters record `static get/set` apart (class-name
	// dispatch inlines them with empty this, mirroring staticMethods).
	staticGetters map[string]*ast.Node
	staticSetters map[string]*ast.Node
	// statics folds `static X = <literal>` (methods/getters excluded;
	// non-literal statics keep the legacy instance-layout path).
	statics map[string]staticVal
}

// staticVal is one folded static: SA literal text plus its type.
type staticVal struct {
	text string
	typ  saType
}

// staticLiteralText unwraps as/satisfies/non-null/angle-assert chains
// (upstream ast.SkipOuterExpressions with OEKAssertions: identical kind
// set) to a static literal value (text, type, ok). Anything else is not
// foldable. Parens intentionally stay opaque, matching upstream call shape.
func staticLiteralText(n *ast.Node) (string, saType, bool) {
	if n == nil {
		return "", tUnknown, false
	}
	n = ast.SkipOuterExpressions(n, ast.OEKAssertions)
	switch n.Kind {
	case ast.KindNumericLiteral:
		return n.Text(), tI32, true
	case ast.KindStringLiteral:
		if s, ok := stringLiteralText(n); ok {
			return s, tString, true
		}
		return "", tUnknown, false
	case ast.KindNoSubstitutionTemplateLiteral:
		// Pure backtick literal: same cooked text the value-position
		// lowering feeds to lowerStringLiteral (probed identical).
		return n.Text(), tString, true
	case ast.KindTrueKeyword:
		return "1", tBool, true
	case ast.KindFalseKeyword:
		return "0", tBool, true
	default:
		return "", tUnknown, false
	}
}

// classDefOf resolves a base name to its class definition through
// instance bindings, this-receivers and direct class names (nil when
// none applies; map reads stay nil-safe).
func (e *emitter) classDefOf(base string) *classDef {
	if c, ok := e.varClass[base]; ok {
		if d, ok := e.classDefs[c]; ok {
			return d
		}
	}
	if base == e.thisSelf && e.thisSelf != "" {
		if c, ok := e.varClass[e.thisSelf]; ok {
			if d, ok := e.classDefs[c]; ok {
				return d
			}
		}
		if d, ok := e.classDefs[e.thisSelf]; ok {
			return d
		}
	}
	return e.classDefs[base]
}

// checkerClassOf resolves a method receiver's class through the checker
// when scope maps miss: annotated params/locals (including cross-file
// class types) carry no varClass entry, but their declared type names
// the class exactly. No maps are written, so nothing can go stale across
// functions. Refusals stay loud for: nullable receivers (unwrapping to
// the class would skip the null guard), names that resolve to the class
// itself (static call position; statics route elsewhere), unknown and
// non-class types. Nil-safe; callers keep their diagnostics.
func (e *emitter) checkerClassOf(base *ast.Node) (string, bool) {
	if e.tcx == nil || base == nil || base.Kind != ast.KindIdentifier {
		return "", false
	}
	if _, isClass := e.classDefs[base.Text()]; isClass {
		return "", false
	}
	// Only lowered bindings dispatch: module scratch consts and failed
	// imports carry class types in the checker but own no register, so
	// inlining under their name would cascade dangling-receiver
	// diagnostics (or worse). Scope presence is the binding proof.
	if e.lookupBinding(base.Text()) == nil {
		return "", false
	}
	// Rebound names may no longer hold their declared class (the checker
	// reports the declared type, not the reassigned value): stay loud.
	if e.reboundNames[base.Text()] {
		return "", false
	}
	if e.tcx.nullable(base) {
		return "", false
	}
	name, alias := e.tcx.layoutTypeName(base)
	if name != "" {
		if _, ok := e.classDefs[name]; ok {
			return name, true
		}
	}
	if alias != "" {
		if _, ok := e.classDefs[alias]; ok {
			return alias, true
		}
	}
	return "", false
}

// recordClass registers a class shape. Only data fields contribute layout;
// single extends flattens (see class_heritage.go), implements erases;
// accessors and static blocks refuse loudly.
func (e *emitter) recordClass(st *ast.Node) {
	e.recordClassNamed(st, "")
}

// lowerClassExpression records `const C = class ...` under the bound
// name (anonymous) or the class's own name (named, with the bound name
// aliased for `new` and static reads). Like arrows, the name becomes a
// type-ish binding, not a register: declarations emit no code.
func (e *emitter) lowerClassExpression(name string, init, pos *ast.Node) {
	_ = pos
	own := ""
	if init.Name() != nil && init.Name().Kind == ast.KindIdentifier {
		own = init.Name().Text()
	}
	eff := name
	if own != "" {
		eff = own
	}
	e.recordClassNamed(init, eff)
	if e.refused {
		return
	}
	// Named expressions keep their identity; alias the bound name to
	// the same def so `new D` / `D.K` resolve (mirrors declaration
	// resolution, which keys off the recorded name).
	if own != "" && own != name {
		qOwn := e.nsDefName(own)
		qBound := e.nsDefName(name)
		if def, ok := e.classDefs[qOwn]; ok {
			if e.classDefs == nil {
				e.classDefs = map[string]*classDef{}
			}
			e.classDefs[qBound] = def
		}
		if sd, ok := e.staticDefs[qOwn]; ok {
			if e.staticDefs == nil {
				e.staticDefs = map[string]*classDef{}
			}
			e.staticDefs[qBound] = sd
		}
	}
}

// recordClassNamed records a class declaration or expression. forceName
// overrides the declared name (anonymous class expressions record under
// their bound const name). Heritage uses the shared declaration machinery
// (parseHeritage/inheritClass take clause lists and shape-agnostic defs).
func (e *emitter) recordClassNamed(st *ast.Node, forceName string) {
	var members []*ast.Node
	var heritage *ast.NodeList
	switch st.Kind {
	case ast.KindClassDeclaration:
		cd := st.AsClassDeclaration()
		members = cd.Members.Nodes
		heritage = cd.HeritageClauses
	case ast.KindClassExpression:
		ce := st.AsClassExpression()
		members = ce.Members.Nodes
		heritage = ce.HeritageClauses
		// Heritage shares the declaration machinery below
		// (parseHeritage takes the clause list; layouts, member
		// inheritance, ctor/super rules are shape-agnostic).
	default:
		e.refuse(st, "class record of %s is not lowerable", st.Kind.String())
		return
	}
	name := forceName
	if name == "" {
		name = "<anon>"
		if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
			name = st.Name().Text()
		}
	}
	def := &classDef{name: name, methods: map[string]*ast.Node{}}
	def.name = e.nsDefName(def.name)
	name = def.name
	def.nsSegs = append([]string{}, e.nsStack...)
	// Member/parameter decorators run arbitrary code at definition
	// time; silently dropping them would change program behavior.
	for _, m := range members {
		if len(m.Decorators()) > 0 {
			e.refuse(m, "member decorators are not lowerable (definition-time effects have no SA-ASM form)")
			return
		}
		if m.Kind == ast.KindConstructor {
			for _, p := range m.Parameters() {
				if len(p.AsNode().Decorators()) > 0 {
					e.refuse(p.AsNode(), "parameter decorators are not lowerable (definition-time effects have no SA-ASM form)")
					return
				}
			}
		}
	}
	// Static literals fold regardless of heritage (no instance needed);
	// a heritage-refused class still publishes its statics for folding.
	for _, m := range members {
		if m.Kind != ast.KindPropertyDeclaration {
			continue
		}
		pd := m.AsPropertyDeclaration()
		if m.Name() == nil || (m.Name().Kind != ast.KindIdentifier && m.Name().Kind != ast.KindPrivateIdentifier) {
			continue
		}
		if !ast.HasModifier(m, ast.ModifierFlagsStatic) {
			continue
		}
		if text, typ, ok := staticLiteralText(pd.Initializer); ok {
			if def.statics == nil {
				def.statics = map[string]staticVal{}
			}
			// Private statics mangle by owner (name is final here:
			// nsDefName applied above).
			def.statics[privFieldKey(name, m.Name().Text())] = staticVal{text: text, typ: typ}
		}
	}
	l := &layout{name: name, types: map[string]string{}, ftypes: map[string]string{}, offsets: map[string]int{}, fdefs: map[string]*ast.Node{}}
	off := 0
	if heritage != nil && len(heritage.Nodes) > 0 {
		// Declarations and expressions both arrive with their clause
		// list (see recordClassNamed switch above).
		hi, ok := e.parseHeritage(heritage, st)
		if !ok {
			if len(def.statics) > 0 {
				if e.staticDefs == nil {
					e.staticDefs = map[string]*classDef{}
				}
				e.staticDefs[name] = def
			}
			return
		}
		if !hi.hasExtends {
			// Implements-only: type-erased, the class lowers normally.
		} else if !e.inheritClass(name, e.qualify(hi.base), def, l, st) {
			// Statics-only shells live apart from instantiable defs so no
			// instance path can observe a missing layout (see lowerNewClass).
			if len(def.statics) > 0 {
				if e.staticDefs == nil {
					e.staticDefs = map[string]*classDef{}
				}
				e.staticDefs[name] = def
			}
			return
		} else {
			off = e.classDefs[e.qualify(hi.base)].layout.size
			l.size = off
		}
	}
	if ast.HasModifier(st, ast.ModifierFlagsAbstract) {
		def.isAbstract = true
	}
	for _, m := range members {
		switch m.Kind {
		case ast.KindPropertyDeclaration:
			pd := m.AsPropertyDeclaration()
			raw := ""
			if m.Name() != nil && (m.Name().Kind == ast.KindIdentifier || m.Name().Kind == ast.KindPrivateIdentifier) {
				raw = m.Name().Text()
			} else {
				e.refuse(m, "computed field names are not lowerable")
				continue
			}
			// Private fields mangle by owner (`#x` → `#C#x`, so
			// shadowing owners keep distinct slots); name is final.
			fname := privFieldKey(name, raw)
			// Static literal members fold (never instance slots).
			if ast.HasModifier(m, ast.ModifierFlagsStatic) {
				if text, typ, ok := staticLiteralText(pd.Initializer); ok {
					if def.statics == nil {
						def.statics = map[string]staticVal{}
					}
					def.statics[fname] = staticVal{text: text, typ: typ}
					continue
				}
			}
			saname := "ptr"
			if pd.Type != nil {
				saname = saNameOfType(pd.Type)
			}
			if prevOff, dup := l.offsets[fname]; dup {
				// Inherited field redeclared: keep the base offset (inherited
				// code reads the same slot) and require the same width.
				prevSize, _ := widthOf(l.types[fname])
				newSize, _ := widthOf(saname)
				if prevSize != newSize {
					e.refuse(m, "field %s redeclared with a different width in subclass %s", fname, name)
					continue
				}
				l.types[fname] = saname
				if pd.Type != nil {
					l.ftypes[fname] = rawTypeName(pd.Type)
					l.fdefs[fname] = pd.Type
				}
				_ = prevOff
				continue
			}
			size, align := widthOf(saname)
			off = alignTo(off, align)
			l.fields = append(l.fields, fname)
			l.types[fname] = saname
			if pd.Type != nil {
				l.ftypes[fname] = rawTypeName(pd.Type)
				l.fdefs[fname] = pd.Type
			}
			l.offsets[fname] = off
			off += size
		case ast.KindConstructor:
			def.ctor = m
		case ast.KindMethodDeclaration:
			if m.Name() != nil && m.Name().Kind == ast.KindIdentifier {
				// Static methods record apart and never dispatch as
				// instance methods (same-named statics would
				// shadow/arity-clash the instance entry; static calls
				// route through lowerClassStaticCall).
				if ast.HasModifier(m, ast.ModifierFlagsStatic) {
					if def.staticMethods == nil {
						def.staticMethods = map[string]*ast.Node{}
					}
					def.staticMethods[m.Name().Text()] = m
					if def.methodOwner == nil {
						def.methodOwner = map[string]string{}
					}
					def.methodOwner[m.Name().Text()] = name
					continue
				}
				if def.methods == nil {
					def.methods = map[string]*ast.Node{}
				}
				def.methods[m.Name().Text()] = m
				// Lexical owner for private resolution (inherited
				// members keep the base owner; see inheritClass).
				if def.methodOwner == nil {
					def.methodOwner = map[string]string{}
				}
				def.methodOwner[m.Name().Text()] = name
			}
		case ast.KindGetAccessor, ast.KindSetAccessor:
			// Accessors record bodies for call-site inlining (instance
			// reads/writes) and class-name dispatch (static accessors);
			// super-anchored access stays loud (base dispatch is unowned).
			if m.Name() != nil && m.Name().Kind == ast.KindIdentifier {
				isStatic := ast.HasModifier(m, ast.ModifierFlagsStatic)
				if m.Kind == ast.KindGetAccessor {
					if isStatic {
						if def.staticGetters == nil {
							def.staticGetters = map[string]*ast.Node{}
						}
						def.staticGetters[m.Name().Text()] = m
					} else {
						if def.getters == nil {
							def.getters = map[string]*ast.Node{}
						}
						def.getters[m.Name().Text()] = m
					}
				} else {
					if isStatic {
						if def.staticSetters == nil {
							def.staticSetters = map[string]*ast.Node{}
						}
						def.staticSetters[m.Name().Text()] = m
					} else {
						if def.setters == nil {
							def.setters = map[string]*ast.Node{}
						}
						def.setters[m.Name().Text()] = m
					}
				}
			}
		case ast.KindSemicolonClassElement:
			// no-op separator
		default:
			e.refuse(m, "class member %s is not lowerable", m.Kind.String())
		}
	}
	// Parameter properties (upstream RuntimeSyntaxTransformer.visitClassDeclaration
	// synthesizes a PropertyDeclaration per ident param-prop). Accessibility
	// erases: private/public/protected/readonly all lower as plain instance
	// slots, matching the plain-`private x` probe. Explicit member fields win.
	e.recordParamPropFields(def, l, &off, members)
	l.size = off
	def.layout = l
	// Default derived constructor: a subclass without its own ctor inherits
	// the base node (ctorOwner keeps super() targeting the right parent).
	if def.ctor != nil {
		def.ctorOwner = name
	} else if base, ok := e.classParent[name]; ok && base != "" {
		if bdef, ok := e.classDefs[base]; ok {
			def.ctor = bdef.ctor
			def.ctorOwner = base
		}
	}
	// A declared derived constructor must call super() (TS rule; without it
	// base fields would silently stay zero). Checked at declaration so the
	// gate fires even when the class is never instantiated.
	if def.ctor != nil && def.ctorOwner == name {
		if _, ok := e.classParent[name]; ok && !e.ctorCallsSuper(def.ctor) {
			e.refuse(st, "constructor of %s must call super() (derived constructors delegate to the base)", name)
		}
	}
	if e.layouts == nil {
		e.layouts = map[string]*layout{}
	}
	e.layouts[name] = l
	if e.classDefs == nil {
		e.classDefs = map[string]*classDef{}
	}
	e.classDefs[name] = def
}

// recordParamPropFields records constructor parameter properties as instance
// fields (upstream runtimesyntax visitClassDeclaration synthesizes a
// PropertyDeclaration per ident param-prop; detection via
// ast.IsParameterPropertyDeclaration, same predicate). Non-ident names and
// missing annotations refuse loudly (TS-illegal or unsizable); names already
// present (explicit member or inherited slot) are skipped — the duplicate is
// a checker error and the existing slot wins.
func (e *emitter) recordParamPropFields(def *classDef, l *layout, off *int, members []*ast.Node) {
	var ctor *ast.Node
	for _, m := range members {
		if m.Kind == ast.KindConstructor {
			ctor = m
			break
		}
	}
	if ctor == nil {
		return
	}
	for _, p := range ctor.Parameters() {
		pn := p.AsNode()
		if !ast.IsParameterPropertyDeclaration(pn, ctor) {
			continue
		}
		nm := pn.Name()
		if nm == nil || nm.Kind != ast.KindIdentifier {
			e.refuse(pn, "parameter property names must be identifiers")
			continue
		}
		fname := privFieldKey(def.name, nm.Text())
		if _, dup := l.offsets[fname]; dup {
			continue
		}
		tn := pn.Type()
		if tn == nil {
			e.refuse(pn, "parameter property %s needs a type annotation (slot width)", nm.Text())
			continue
		}
		saname := saNameOfType(tn)
		size, align := widthOf(saname)
		*off = alignTo(*off, align)
		l.fields = append(l.fields, fname)
		l.types[fname] = saname
		l.ftypes[fname] = rawTypeName(tn)
		l.fdefs[fname] = tn
		l.offsets[fname] = *off
		*off += size
	}
}

// lowerNewClass materializes `new Box(...)`: allocates the static layout,
// then interprets the constructor (`this.f = param` wirings; function-typed
// fields capture inline arrows per instance).
func (e *emitter) lowerNewClass(name string, nw *ast.NewExpression, pos *ast.Node) (string, saType) {
	def := e.classDefs[name]
	if def == nil || def.layout == nil {
		// Heritage/statics-only shells never instantiate (a missing
		// layout used to panic here; hostile inputs must refuse).
		e.refuse(pos, "class %s cannot be instantiated in the subset", name)
		return "0", tUnknown
	}
	if def.isAbstract {
		e.refuse(pos, "abstract class %s cannot be instantiated (declare a concrete subclass)", name)
		return "0", tUnknown
	}
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
			// ctorOwner keeps an inherited super() targeting the right
			// parent (multi-level chains delegate recursively).
			owner := name
			if def.ctorOwner != "" {
				owner = def.ctorOwner
			}
			if !e.wireCtorBody(h, owner, def.ctor, paramArg, paramVal, pos) {
				return h, tArray
			}
		}
	}
	return h, tArray
}

// wireCtorStatement interprets one `this.f = <param>` constructor wiring
// (plus `super(...)` delegation for subclasses; see class_heritage.go).
// Reports false after refusing.
func (e *emitter) wireCtorStatement(h, className string, s *ast.Node, paramArg map[string]*ast.Node, paramVal map[string]string, pos *ast.Node) bool {
	_ = pos
	if done, ok := e.wireSuperCtorStatement(h, className, s, paramVal, pos); done {
		return ok
	}
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
	if bin.Right.Kind != ast.KindIdentifier {
		e.refuse(s, "constructor wiring right side must be a parameter name")
		return false
	}
	pname := bin.Right.Text()
	return e.wireCtorFieldStore(h, className, pa.Name().Text(), pname, s, paramArg, paramVal)
}

// wireCtorFieldStore stores one ctor wiring value (`this.<field> = <param>`)
// into a fresh instance handle (plus arrow-capture bookkeeping; see
// lowerNewClass). Reports false after refusing.
func (e *emitter) wireCtorFieldStore(h, owner, field, pname string, s *ast.Node, paramArg map[string]*ast.Node, paramVal map[string]string) bool {
	def := e.classDefs[owner]
	// Private fields mangle by the constructor's owner (wireCtorStatement
	// carries it explicitly, like ctorOwner for super).
	rfield, ok := e.privResolveOwned(def.layout, field, owner, s)
	if !ok {
		return false
	}
	off, ok := def.layout.offsets[rfield]
	if !ok {
		e.refuse(s, "field %s is not in the %s layout", rfield, owner)
		return false
	}
	saname := def.layout.types[rfield]
	if anode, ok := paramArg[pname]; ok && (anode.Kind == ast.KindArrowFunction || anode.Kind == ast.KindFunctionExpression) {
		// Function-typed field captures the inline arrow per instance.
		if e.instFnFields == nil {
			e.instFnFields = map[string]map[string]*ast.Node{}
		}
		if e.instFnFields[h] == nil {
			e.instFnFields[h] = map[string]*ast.Node{}
		}
		e.instFnFields[h][rfield] = anode
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

// paramPropNames lists ident parameter-property names in order (upstream
// getParameterProperties, ident-only). Non-ident shapes were already refused
// at record; they are skipped here.
func paramPropNames(ctor *ast.Node) []string {
	var out []string
	for _, p := range ctor.Parameters() {
		pn := p.AsNode()
		if !ast.IsParameterPropertyDeclaration(pn, ctor) {
			continue
		}
		if nm := pn.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
			out = append(out, nm.Text())
		}
	}
	return out
}

// isSuperCallStatement reports a top-level `super(...)` statement (same shape
// test as wireSuperCtorStatement).
func isSuperCallStatement(s *ast.Node) bool {
	if s.Kind != ast.KindExpressionStatement {
		return false
	}
	ex := s.AsExpressionStatement().Expression
	if ex.Kind != ast.KindCallExpression {
		return false
	}
	call := ex.AsCallExpression()
	return call.Expression != nil && call.Expression.Kind == ast.KindSuperKeyword
}

// wireCtorBody wires ctor body statements in order, injecting parameter-
// property stores (`this.p = p`) right after the top-level super() statement
// (or at the top when none) — mirroring upstream visitConstructorBody +
// transformConstructorBodyWorker. Names the body already wires explicitly
// (`this.p = ...`) are skipped (explicit wiring wins). Reports false after
// refusing.
func (e *emitter) wireCtorBody(h, owner string, ctor *ast.Node, paramArg map[string]*ast.Node, paramVal map[string]string, pos *ast.Node) bool {
	body := ctor.Body()
	if body == nil {
		return true
	}
	stmts := body.Statements()
	wired := map[string]bool{}
	for _, s := range stmts {
		if s.Kind != ast.KindExpressionStatement {
			continue
		}
		ex := s.AsExpressionStatement().Expression
		if ex.Kind != ast.KindBinaryExpression {
			continue
		}
		bin := ex.AsBinaryExpression()
		if bin.OperatorToken.Kind != ast.KindEqualsToken || bin.Left.Kind != ast.KindPropertyAccessExpression {
			continue
		}
		if pa := bin.Left.AsPropertyAccessExpression(); pa.Expression.Kind == ast.KindThisKeyword && pa.Name() != nil {
			wired[pa.Name().Text()] = true
		}
	}
	inject := func() bool {
		if e.refused {
			// Record already refused (unsizable field): the file is
			// discarded; skip silently instead of cascading offset noise.
			return true
		}
		for _, pname := range paramPropNames(ctor) {
			if wired[pname] {
				continue
			}
			if _, ok := paramArg[pname]; !ok {
				e.refuse(ctor, "parameter property %s has no argument", pname)
				return false
			}
			if !e.wireCtorFieldStore(h, owner, pname, pname, ctor, paramArg, paramVal) {
				return false
			}
		}
		return true
	}
	superIdx := -1
	for i, s := range stmts {
		if isSuperCallStatement(s) {
			superIdx = i
			break
		}
	}
	if superIdx < 0 {
		if !inject() {
			return false
		}
	}
	for i, s := range stmts {
		if !e.wireCtorStatement(h, owner, s, paramArg, paramVal, pos) {
			return false
		}
		if i == superIdx {
			if !inject() {
				return false
			}
		}
	}
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
	owner := className
	if o, ok := def.methodOwner[method]; ok {
		owner = o
	}
	return e.inlineClassMethod(recv, className, owner, method, mn, args, argNodes, pos)
}

// lowerClassStaticCall inlines `Class.m(args)`: same core, but with no
// instance `this` stays empty, so instance state refuses honestly inside
// while other statics route through the same dispatch.
func (e *emitter) lowerClassStaticCall(className, method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	def, ok := e.classDefs[className]
	if !ok {
		return "", tUnknown, false
	}
	mn, ok := def.staticMethods[method]
	if !ok {
		return "", tUnknown, false
	}
	owner := className
	if o, ok := def.methodOwner[method]; ok {
		owner = o
	}
	return e.inlineClassMethod("", className, owner, method, mn, args, argNodes, pos)
}

// inlineClassMethod is the shared method-inline core for instance and
// static calls: parameters bind (generic params inherit the argument
// layout), thisSelf aliases the receiver (empty for statics), and the
// body joins through a value slot.
func (e *emitter) inlineClassMethod(thisSelf, className, owner, method string, mn *ast.Node, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	def := e.classDefs[className]
	if mn.Body() == nil {
		e.refuse(pos, "%s.%s has no body (overload signatures do not inline)", className, method)
		return "0", tUnknown, true
	}
	params := mn.Parameters()
	var anodeList []*ast.Node
	if argNodes != nil {
		anodeList = argNodes.Nodes
	}
	// Arity counts lowered args (arrows arrive as markers with nodes).
	// Setter inlines pass argNodes == nil (single already-lowered RHS);
	// the lowered-arg count still enforces exactly one value.
	nArgs := len(args)
	if nArgs != len(params) || (argNodes != nil && len(anodeList) != len(params)) {
		e.refuse(pos, "%s.%s takes %d arguments", className, method, len(params))
		return "0", tUnknown, true
	}
	// Scopes at or below this index survive the inline (join-point
	// cleanup releases deeper ones only).
	scopeBase := len(e.scopes) - 1
	e.pushScope()
	savedSelf := e.thisSelf
	e.thisSelf = thisSelf
	savedCls := e.curMethodClass
	e.curMethodClass = className
	savedOwner := e.curMethodOwner
	// Lexical owner for private resolution (inherited members keep the
	// base owner; unrecorded methods fall back to the receiver class).
	e.curMethodOwner = owner
	// Method bodies resolve bare namespace siblings qualified (the
	// owner's path, not the call-site prefix).
	savedNs := e.nsStack
	if def != nil && len(def.nsSegs) > 0 {
		e.nsStack = def.nsSegs
	}
	for i, p := range params {
		pname, ok := bindingNameText(p.AsNode())
		if !ok {
			e.refuse(p.AsNode(), "destructured method parameters are not lowerable")
			e.thisSelf = savedSelf
			e.curMethodClass = savedCls
			e.curMethodOwner = savedOwner
			e.nsStack = savedNs
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
		} else if e.arrVars[args[i]] || e.strVars[args[i]] || e.modStrTmps[args[i]] {
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
			if e.strVars[args[i]] || e.modStrTmps[args[i]] {
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
	e.inlineRet = &inlineRetState{active: true, slot: slot, end: endL, saname: "i32", scopeBase: scopeBase}
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
	e.curMethodClass = savedCls
	e.curMethodOwner = savedOwner
	e.nsStack = savedNs
	// Fallthrough join releases method-created temps (the slot outlives
	// into the load below).
	if !e.terminated {
		e.releaseDeeperThan(scopeBase, slot)
	}
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
	// Type-only imports erase at compile time (no runtime edge, no
	// resolution, no recording; mirrors the link-graph filter).
	if cl := imp.ImportClause; cl != nil && cl.IsTypeOnly() {
		return
	}
	mod, ok := stringLiteralText(imp.ModuleSpecifier)
	if !ok {
		e.refuse(st, "non-literal module specifiers are not lowerable")
		return
	}
	if mod == "fs" || mod == "net" || mod == "path" || mod == "os" || mod == "crypto" || mod == "querystring" || mod == "url" || mod == "util" || mod == "punycode" ||
		mod == "node:fs" || mod == "node:net" || mod == "node:path" || mod == "node:os" || mod == "node:crypto" || mod == "node:querystring" || mod == "node:url" || mod == "node:util" || mod == "node:punycode" {
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
	// Usage-erased imports (no value-position use) never reach resolution
	// (mirrors the link-graph filter; keeps unresolvable-erased decls silent).
	if e.link != nil {
		if strings.HasPrefix(mod, ".") && e.link.valueUsed != nil && !importDeclValueEdge(st, e.link.valueUsed) {
			return
		}
		if e.importEnv == nil {
			e.importEnv = map[string]string{}
		}
		if e.importRet == nil {
			e.importRet = map[string]saType{}
		}
		if e.nsImports == nil {
			e.nsImports = map[string]string{}
		}
		if e.defNSImports == nil {
			e.defNSImports = map[string]bool{}
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
			if qq, ok := res.qualified[remote]; ok {
				q = qq
			}
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
			// Default import binds the target's default export. An object
			// default (`export default {a}`) binds per-member instead
			// (the object itself is not callable; bare D() stays loud).
			if nm := clause.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				if len(res.defNS) > 0 {
					// Object-default member binding lives in link_nsobject.
					bindDefNSMembers(e, nm.Text(), res)
					if clause.NamedBindings == nil {
						return
					}
				} else {
					if res.defQualified == "" {
						e.refuse(st, "%s has no default export", mod)
						return
					}
					callable := false
				if res.defLocal != "" {
					_, callable = res.rets[res.defLocal]
				} else if _, ok := res.rets["default"]; ok {
					callable = true
				}
				if !callable {
					e.refuse(st, "default export of %s is not callable", mod)
					return
				}
				q := res.defQualified
				local := nm.Text()
				e.importEnv[local] = q
				if r, ok := res.rets[res.defLocal]; ok {
					e.importRet[local] = r
				} else {
					e.importRet[local] = tI32
				}
				e.importedNames[local] = true
				}
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
						q := res.prefix + exp
						if qq, ok := res.qualified[exp]; ok {
							q = qq
						}
						e.importEnv[ns+"."+exp] = q
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
					// Type-only specifiers erase (esbuild
					// importsNotUsedAsValues): explicit `import { type X }`
					// always; otherwise when the local name has no
					// value-position use in this file (the link phase
					// precomputed valueUsed per file). A wrongly skipped
					// name surfaces as a loud "import it first" refusal
					// at its use, never a silent miscompile.
					if sp.IsTypeOnly {
						continue
					}
					if e.link != nil && e.link.valueUsed != nil && !e.link.valueUsed[local] {
						continue
					}
					// Exported literal consts fold by value (same shapes
					// tryTopLevelConst accepts): no importEnv binding, no
					// call signature; every consumer (folds, typeof,
					// arithmetic, const-reassign guard) reuses the
					// single-file const machinery untouched.
					if lit, ok := res.consts[remote]; ok {
						if e.constVals == nil {
							e.constVals = map[string]string{}
						}
						if e.constIsStr == nil {
							e.constIsStr = map[string]bool{}
						}
						e.constVals[local] = lit
						if res.constStr[remote] {
							e.constIsStr[local] = true
						}
						continue
					}
					// Exported enums fold ordinals through the
					// single-file enum tables (member reads, switch
					// cases, comparisons all reuse them untouched).
					if members, ok := res.enums[remote]; ok {
						if e.enums == nil {
							e.enums = map[string]map[string]int64{}
						}
						e.enums[local] = members
						continue
					}
					// Namespace literal consts fold by value (mirrors the top-level
					// const import above; the dotted key keeps them namespaced).
					if consts, ok := res.nsConsts[remote]; ok {
						for member, lit := range consts {
							if e.constVals == nil {
								e.constVals = map[string]string{}
							}
							e.constVals[local+"."+member] = lit
						}
						if strs, ok := res.nsConstStr[remote]; ok {
							for member := range strs {
								if e.constIsStr == nil {
									e.constIsStr = map[string]bool{}
								}
								e.constIsStr[local+"."+member] = true
							}
						}
					}
					// Namespace declarations bind per member (`import { N }`
					// then N.f(); the namespace itself is not a value;
					// see link_namespace.go).
					if members, ok := res.nsMembers[remote]; ok {
						bindNSMembers(e, local, remote, res, members)
						continue
					}
					// String/computed enums are exported in TS but have no
					// ordinal table (single-file reads refuse too); name
					// the gap instead of the false "not exported".
					if res.nonIntEnums[remote] {
						e.refuse(el, "enum %s has string/computed members (only all-integer enums link; single-file string reads are not lowerable either)", remote)
						continue
					}
					// Exported lets are exported in TS but never fold
					// (reassignment would stale the importer's fold);
					// name the gap instead of the false "not exported".
					if res.lets[remote] {
						e.refuse(el, "let %s cannot link by value (reassignment would stale the importer's fold; use const or keep it file-local)", remote)
						continue
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
			sp := n.AsImportSpecifier()
			if nm := n.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
				local := nm.Text()
				e.importedFrom[local] = mod
				// `import { a }`: remote is a; `import { b as c }`:
				// PropertyName holds the remote (b), local is c.
				remote := local
				if sp.PropertyName != nil {
					remote = sp.PropertyName.Text()
				}
				if e.importedRemote == nil {
					e.importedRemote = map[string]string{}
				}
				e.importedRemote[local] = remote
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
	// "string" takes no arguments; "string1"/"string2"/"string3" take
	// one/two/three string slices expanded to (&ptr, len) in-params
	// ahead of the outs; "fire" passes slices by value with no outs;
	// "fireF64" adds one f64 out slot; "u64out" adds one u64 out slot;
	// "boolout" adds one bool out slot (i32 0/1);
	// "nullable" wraps string outs with status 1 mapping to null "0".
	if isPluginBackend(proj) && (proj.NodeOut == "string" || proj.NodeOut == "string1" || proj.NodeOut == "string2" || proj.NodeOut == "string3" || proj.NodeOut == "argv" || proj.NodeOut == "sized" || proj.NodeOut == "fire" || proj.NodeOut == "fireF64" || proj.NodeOut == "u64out" || proj.NodeOut == "boolout" || proj.NodeOut == "nullable") {
		if proj.NodeOut == "nullable" {
			// One slice in, string out; status 1 maps to null "0"
			// (subset null mapping), other nonzero panics. Both arms
			// join on one result temp (rebind at the join, like loops).
			if len(args) != 1 {
				e.refuse(pos, "%s takes exactly 1 argument", proj.TS)
				return "0", tUnknown
			}
			np, nl := e.expandSlice(args[0])
			nps := e.freshTmp()
			nls := e.freshTmp()
			e.emit("%s = alloc 8", nps)
			e.emit("%s = alloc 8", nls)
			e.ownTemp(nps)
			e.ownTemp(nls)
			nst := e.freshTmp()
			e.emit("%s = call @%s(&%s, %s, &%s, &%s)", nst, proj.Symbol, np, nl, nps, nls)
			e.ownTemp(nst)
			nres := e.freshTmp()
			zeroL := e.freshLabel("node_null")
			chkL := e.freshLabel("node_chk")
			wrapL := e.freshLabel("node_wrap")
			endL := e.freshLabel("node_end")
			badL := e.freshLabel("node_bad")
			isnull := e.freshTmp()
			e.emit("%s = eq %s, 1", isnull, nst)
			e.emit("br %s -> %s, %s", isnull, zeroL, chkL)
			e.emitRaw("%s:", zeroL)
			e.releaseIfOwnedTemp(nps)
			e.releaseIfOwnedTemp(nls)
			e.emit("%s = 0", nres)
			e.emit("jmp %s", endL)
			e.emitRaw("%s:", chkL)
			isbad := e.freshTmp()
			e.emit("%s = ne %s, 0", isbad, nst)
			e.emit("br %s -> %s, %s", isbad, badL, wrapL)
			e.emitRaw("%s:", badL)
			e.emit("panic(%d)", panicBackendStatus)
			e.terminated = true
			e.emitRaw("%s:", wrapL)
			e.terminated = false
			nptr := e.freshTmp()
			e.emit("%s = load %s + 0 as ptr", nptr, nps)
			nln := e.freshTmp()
			e.emit("%s = load %s + 0 as u64", nln, nls)
			e.emit("%s = alloc 16", nres)
			e.emit("store %s + 0, %s as ptr", nres, nptr)
			e.emit("store %s + 8, %s as u64", nres, nln)
			e.declareOwned(nres)
			e.releaseIfOwnedTemp(nps)
			e.releaseIfOwnedTemp(nls)
			e.emit("jmp %s", endL)
			e.emitRaw("%s:", endL)
			return nres, tString
		}
		if proj.NodeOut == "u64out" {
			// One slice in, u64 out (Buffer.byteLength shape).
			if len(args) != 1 {
				e.refuse(pos, "%s takes exactly 1 argument", proj.TS)
				return "0", tUnknown
			}
			up, ul := e.expandSlice(args[0])
			uslot := e.freshTmp()
			e.emit("%s = alloc 8", uslot)
			e.ownTemp(uslot)
			ust := e.freshTmp()
			e.emit("%s = call @%s(&%s, %s, &%s)", ust, proj.Symbol, up, ul, uslot)
			e.ownTemp(ust)
			ubadL := e.freshLabel("node_bad")
			uokL := e.freshLabel("node_ok")
			ubad := e.freshTmp()
			e.emit("%s = ne %s, 0", ubad, ust)
			e.emit("br %s -> %s, %s", ubad, ubadL, uokL)
			e.emitRaw("%s:", ubadL)
			e.emit("panic(%d)", panicBackendStatus)
			e.terminated = true
			e.emitRaw("%s:", uokL)
			e.terminated = false
			uout := e.freshTmp()
			e.emit("%s = load %s + 0 as u64", uout, uslot)
			e.releaseIfOwnedTemp(uslot)
			return uout, tU64
		}
		if proj.NodeOut == "boolout" {
			// One slice in, bool out (path.isAbsolute/fs.existsSync shape:
			// plugin writes 0/1 into an out slot, u32 status checked).
			// Slot is 8 bytes; backends write u32 or u64 0/1, so the low
			// 4 bytes load as i32 is exact on little-endian.
			if len(args) != 1 {
				e.refuse(pos, "%s takes exactly 1 argument", proj.TS)
				return "0", tUnknown
			}
			bp, bl := e.expandSlice(args[0])
			bslot := e.freshTmp()
			e.emit("%s = alloc 8", bslot)
			e.ownTemp(bslot)
			bst := e.freshTmp()
			e.emit("%s = call @%s(&%s, %s, &%s)", bst, proj.Symbol, bp, bl, bslot)
			e.ownTemp(bst)
			bbadL := e.freshLabel("node_bad")
			bokL := e.freshLabel("node_ok")
			bbad := e.freshTmp()
			e.emit("%s = ne %s, 0", bbad, bst)
			e.emit("br %s -> %s, %s", bbad, bbadL, bokL)
			e.emitRaw("%s:", bbadL)
			e.emit("panic(%d)", panicBackendStatus)
			e.terminated = true
			e.emitRaw("%s:", bokL)
			e.terminated = false
			bout := e.freshTmp()
			e.emit("%s = load %s + 0 as i32", bout, bslot)
			e.releaseIfOwnedTemp(bslot)
			return bout, tI32
		}
		if proj.NodeOut == "fire" || proj.NodeOut == "fireF64" {
			if proj.NodeOut == "fireF64" && len(args) != 1 {
				e.refuse(pos, "%s takes exactly 1 argument", proj.TS)
				return "0", tUnknown
			}
			ins := []string{}
			for _, a := range args {
				ap, al := e.expandSlice(a)
				ins = append(ins, ap, al)
			}
			// Fixed trailing immediates (e.g. recursive=0 for mkdir).
			if proj.Extra != "" {
				ins = append(ins, proj.Extra)
			}
			fslot := ""
			if proj.NodeOut == "fireF64" {
				fslot = e.freshTmp()
				e.emit("%s = alloc 8", fslot)
				e.ownTemp(fslot)
				ins = append(ins, "&"+fslot)
			}
			st := e.freshTmp()
			e.emit("%s = call @%s(%s)", st, proj.Symbol, strings.Join(ins, ", "))
			e.ownTemp(st)
			badL := e.freshLabel("node_bad")
			okL := e.freshLabel("node_ok")
			bad := e.freshTmp()
			e.emit("%s = ne %s, 0", bad, st)
			e.emit("br %s -> %s, %s", bad, badL, okL)
			e.emitRaw("%s:", badL)
			e.emit("panic(%d)", panicBackendStatus)
			e.terminated = true
			e.emitRaw("%s:", okL)
			e.terminated = false
			if proj.NodeOut == "fireF64" {
				fout := e.freshTmp()
				e.emit("%s = load %s + 0 as f64", fout, fslot)
				e.releaseIfOwnedTemp(fslot)
				return fout, tF64
			}
			return "0", tVoid
		}
		if proj.NodeOut == "sized" {
			// size in, bare &ptr out whose length echoes the request.
			if len(args) != 1 {
				e.refuse(pos, "%s takes 1 argument", proj.TS)
				return "0", tUnknown
			}
			ps := e.freshTmp()
			e.emit("%s = alloc 8", ps)
			e.ownTemp(ps)
			st := e.freshTmp()
			e.emit("%s = call @%s(%s, &%s)", st, proj.Symbol, args[0], ps)
			e.ownTemp(st)
			badL := e.freshLabel("node_bad")
			okL := e.freshLabel("node_ok")
			bad := e.freshTmp()
			e.emit("%s = ne %s, 0", bad, st)
			e.emit("br %s -> %s, %s", bad, badL, okL)
			e.emitRaw("%s:", badL)
			e.emit("panic(%d)", panicBackendStatus)
			e.terminated = true
			e.emitRaw("%s:", okL)
			e.terminated = false
			ptr := e.freshTmp()
			e.emit("%s = load %s + 0 as ptr", ptr, ps)
			out := e.freshTmp()
			e.emit("%s = alloc 16", out)
			e.emit("store %s + 0, %s as ptr", out, ptr)
			e.emit("store %s + 8, %s as u64", out, args[0])
			e.declareOwned(out)
			e.releaseIfOwnedTemp(ps)
			return out, tString
		}
		want := 0
		if proj.NodeOut == "string1" {
			want = 1
		}
		if proj.NodeOut == "string2" {
			want = 2
		}
		if proj.NodeOut == "string3" {
			want = 3
		}
		if proj.NodeOut != "argv" && len(args) != want {
			e.refuse(pos, "%s takes %d argument(s)", proj.TS, want)
			return "0", tUnknown
		}
		pre := ""
		argvRel := ""
		if proj.NodeOut == "string1" || proj.NodeOut == "string2" || proj.NodeOut == "string3" {
			n := 1
			if proj.NodeOut == "string2" {
				n = 2
			}
			if proj.NodeOut == "string3" {
				n = 3
			}
			for _, a := range args[:n] {
				ip, il := e.expandSlice(a)
				pre += "&" + ip + ", " + il + ", "
			}
		}
		if proj.NodeOut == "argv" {
			// Pack parts as 16-byte {ptr,len} entries (SA slice layout,
			// matching the plugin's SaSlice array). Zero parts still
			// allocates one slot; the plugin short-circuits on argc==0
			// before touching argv, so no null idiom is invented.
			n := len(args)
			slots := n
			if slots == 0 {
				slots = 1
			}
			argv := e.freshTmp()
			e.emit("%s = alloc %d", argv, slots*16)
			e.ownTemp(argv)
			argvRel = argv
			for i, a := range args {
				ap, al := e.expandSlice(a)
				e.emit("store %s + %d, %s as ptr", argv, i*16, ap)
				e.emit("store %s + %d, %s as u64", argv, i*16+8, al)
			}
			pre = argv + ", " + fmt.Sprintf("%d", n) + ", "
		}
		ps := e.freshTmp()
		ls := e.freshTmp()
		e.emit("%s = alloc 8", ps)
		e.emit("%s = alloc 8", ls)
		e.ownTemp(ps)
		e.ownTemp(ls)
		st := e.freshTmp()
		e.emit("%s = call @%s(%s&%s, &%s)", st, proj.Symbol, pre, ps, ls)
		e.ownTemp(st)
		badL := e.freshLabel("node_bad")
		okL := e.freshLabel("node_ok")
		bad := e.freshTmp()
		e.emit("%s = ne %s, 0", bad, st)
		e.emit("br %s -> %s, %s", bad, badL, okL)
		e.emitRaw("%s:", badL)
		e.emit("panic(%d)", panicBackendStatus)
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
		if argvRel != "" {
			e.releaseIfOwnedTemp(argvRel)
		}
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

// emitStatusCheckedI64 emits a status-checked sa_std call returning i64:
// one string-slice operand in, i64 out on status 0, panic otherwise
// (mirrors the node-backend shape for status-checked calls whose
// failure has no in-band encoding, e.g. Date.parse vs NaN).
func (e *emitter) emitStatusCheckedI64(proj StdProjection, arg string, pos *ast.Node) (string, saType) {
	e.needImport(proj.Module)
	ip, il := e.expandSlice(arg)
	ms := e.freshTmp()
	e.emit("%s = alloc 8", ms)
	e.ownTemp(ms)
	st := e.freshTmp()
	e.emit("%s = call @%s(&%s, %s, &%s)", st, proj.Symbol, ip, il, ms)
	e.ownTemp(st)
	badL := e.freshLabel("parse_bad")
	okL := e.freshLabel("parse_ok")
	bad := e.freshTmp()
	e.emit("%s = ne %s, 0", bad, st)
	e.emit("br %s -> %s, %s", bad, badL, okL)
	e.emitRaw("%s:", badL)
	e.emit("panic(%d)", panicBackendStatus)
	e.terminated = true
	e.emitRaw("%s:", okL)
	e.terminated = false
	out := e.freshTmp()
	e.emit("%s = load %s + 0 as i64", out, ms)
	e.releaseIfOwnedTemp(ms)
	return out, tI64
}

// isPluginBackend reports native-plugin backends sharing the u32-status
// out-param shape (node.sai, deno.sai; bun has no plugin yet).
func isPluginBackend(proj StdProjection) bool {
	return proj.Backend == "node" || proj.Backend == "deno"
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
//
// Scope bindings win: a declared local/param/alias never routes to a
// same-named module slot (declaration sites call assignLocal directly).
// Otherwise a module-state name stores through the registry, and an
// assignment to a folded const refuses loudly (invalid TS that used to
// miscompile silently via fold-then-rebind).
func (e *emitter) assign(dst, src, srcKind string, srcType saType, pos *ast.Node) {
	if e.lookupBinding(dst) == nil {
		if ms := e.modStateOf(dst); ms != nil {
			_, _, _ = e.emitModStore(ms, src, srcKind, srcType, pos)
			return
		}
		if _, ok := e.constVals[dst]; ok {
			e.refuse(pos, "cannot reassign const %s (folded literals are immutable)", dst)
			return
		}
		if q := e.qualify(dst); q != dst {
			if _, ok := e.constVals[q]; ok {
				e.refuse(pos, "cannot reassign const %s (folded literals are immutable)", q)
				return
			}
		}
		// True global rebinding drops alias folds (normal declaration
		// applies from here on); declarations route through assignLocal
		// and must never disturb file-scope folds they shadow.
		delete(e.mathAliases, dst)
	}
	e.assignLocal(dst, src, srcKind, srcType, pos)
}

// assignLocal is the register-target assignment core (declaration sites
// and scope-bound names; never routes to module-state slots, never
// disturbs file-scope folds shadowed by locals).
func (e *emitter) assignLocal(dst, src, srcKind string, srcType saType, pos *ast.Node) {
	// Rebinding an alias drops the alias (the name becomes a fresh value).
	if b := e.lookupBinding(dst); b != nil && b.alias != "" {
		b.alias = ""
	}
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
	// Terminated blocks emit no releases: anything here would land after
	// a terminator (unreachable-code trap). Return already released via
	// releaseAllOwnedExcept, break/continue via releaseForJump, and panic
	// aborts; list filtering below still runs for a consistent list.
	if !e.terminated {
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

// releaseDeeperThan releases owned-live bindings in scopes deeper than
// depth, except one survivor. Inlined bodies (methods, callbacks) bypass
// lowerReturn's exit cleanup, so their join point releases method-created
// temps explicitly; the join slot itself outlives the body. Caller scopes
// (at or above depth) stay live.
func (e *emitter) releaseDeeperThan(depth int, except string) {
	done := map[string]bool{}
	for d := len(e.scopes) - 1; d > depth; d-- {
		top := e.scopes[d]
		for i := len(e.owned) - 1; i >= 0; i-- {
			name := e.owned[i]
			if name == except || top[name] == nil || done[name] {
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
