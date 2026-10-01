// TypeScript namespaces (`namespace N { ... }` / `module N { ... }`) and
// ambient module blocks (`declare module "x" { ... }`, `declare global`).
//
// Two shapes, two treatments:
//
//   - Ambient blocks declare no runtime code (signatures and types only;
//     the real-world corpus case is planck's `declare module "./Joint"`
//     call-signature augmentations). They erase wholesale: the link graph
//     already ignores them (no export scan case, no value uses), so only
//     the lowering gate needs the same erasure.
//   - Runtime namespaces flatten to qualified file-scope definitions
//     (`N.f` -> `N_f`, nested `A.B.x` -> `A_B_x`). Members record through
//     the existing recorders with the namespace prefix active (one-line
//     nsDefName hooks); bare references inside resolve qualified-first via
//     qualify(); dotted `N.x` accesses route through the member table with
//     export checks. `implements`-style type members erase naturally.
//
// Sequenced gaps (loud, never silent): mutable namespace state (`export
// let` with effectful inits shares the module-state problem with top-level
// vars), namespace merging (reopening N), import-equals aliases, and
// cross-file member access (same-file and single-file are complete).
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
)

// Namespace member kinds recorded in nsMembers (qualified name -> kind).
const (
	nsKindFunc      = "func"
	nsKindArrow     = "arrow"
	nsKindConst     = "const"
	nsKindLet       = "let"
	nsKindClass     = "class"
	nsKindEnum      = "enum"
	nsKindInterface = "interface"
	nsKindType      = "type"
)

// nsPath joins the active namespace stack ("A_B" or "" at top level).
func (e *emitter) nsPath() string {
	return strings.Join(e.nsStack, "_")
}

// nsDefName qualifies a member definition name with the active namespace
// path (identity outside namespaces, so recorder hooks are no-ops there).
func (e *emitter) nsDefName(raw string) string {
	if p := e.nsPath(); p != "" {
		return p + "_" + raw
	}
	return raw
}

// qualify resolves a bare reference qualified-first: the innermost
// namespace prefix wins, then outer prefixes, then import-equals aliases
// (`import x = N.y` records x → N_y), then the bare name (TS shadowing;
// identity outside namespaces and on misses). All callers are use
// positions (reads, calls, lookups); definitions register via nsDefName.
func (e *emitter) qualify(name string) string {
	for i := len(e.nsStack); i > 0; i-- {
		q := strings.Join(e.nsStack[:i], "_") + "_" + name
		if _, ok := e.nsMembers[q]; ok {
			return q
		}
	}
	if q, ok := e.eqAliases[name]; ok {
		return q
	}
	return name
}

// dottedBaseName folds an `A.B.C` identifier chain to its flattened
// namespace path ("A_B_C"). Reports false on any non-identifier segment.
func dottedBaseName(n *ast.Node) (string, bool) {
	segs := []string{}
	cur := n
	for cur.Kind == ast.KindPropertyAccessExpression {
		pa := cur.AsPropertyAccessExpression()
		if pa.Name() == nil || pa.Name().Kind != ast.KindIdentifier {
			return "", false
		}
		segs = append([]string{pa.Name().Text()}, segs...)
		cur = pa.Expression
	}
	if cur.Kind != ast.KindIdentifier {
		return "", false
	}
	segs = append([]string{cur.Text()}, segs...)
	return strings.Join(segs, "_"), true
}

// isAmbientModule reports type-only module blocks: `declare`-modified,
// string-named (`declare module "./x"`), or `declare global`. All erase.
func isAmbientModule(st *ast.Node) bool {
	if hasModifier(st, ast.KindDeclareKeyword) {
		return true
	}
	md := st.AsModuleDeclaration()
	if nm := md.Name(); nm != nil && nm.Kind == ast.KindStringLiteral {
		return true
	}
	return false
}

// moduleMemberStmts unwraps a module body to its member statements:
// a ModuleBlock's list, or the single nested ModuleDeclaration for the
// `namespace A.B {}` sugar. Reports nil for missing bodies.
func moduleMemberStmts(st *ast.Node) []*ast.Node {
	md := st.AsModuleDeclaration()
	if md.Body == nil {
		return nil
	}
	body := md.Body
	if body.Kind == ast.KindModuleBlock {
		return body.AsModuleBlock().Statements.Nodes
	}
	if body.Kind == ast.KindModuleDeclaration {
		return []*ast.Node{body}
	}
	return nil
}

// moduleDeclName reads the namespace name (identifier only; string-named
// modules are ambient and never reach runtime lowering).
func moduleDeclName(st *ast.Node) (string, bool) {
	if nm := st.AsModuleDeclaration().Name(); nm != nil && nm.Kind == ast.KindIdentifier {
		return nm.Text(), true
	}
	return "", false
}

// lowerNamespace lowers `namespace N { ... }` / `module N { ... }`.
// Ambient blocks erase. Runtime bodies lower in two passes (signature
// pre-scan for forward calls, then bodies) with the namespace prefix
// active. Function-nested namespaces refuse (declarations hoist to file
// scope in this model).
func (e *emitter) lowerNamespace(st *ast.Node, topLevel bool) {
	if isAmbientModule(st) {
		return
	}
	if !topLevel && len(e.nsStack) == 0 {
		e.refuse(st, "namespaces inside function bodies are not lowerable (hoist the namespace to file scope)")
		return
	}
	name, ok := moduleDeclName(st)
	if !ok {
		e.refuse(st, "string-named modules are ambient-only (add declare, or use a namespace)")
		return
	}
	members := moduleMemberStmts(st)
	full := name
	if p := e.nsPath(); p != "" {
		full = p + "_" + name
	}
	// Reopening merges: the scope registers once, every body prescans
	// (duplicate members refuse), and member lowering drains after the
	// whole file prescans — so cross-body forward references resolve.
	if e.namespaces == nil {
		e.namespaces = map[string]bool{}
	}
	e.namespaces[full] = true
	if e.nsExports == nil {
		e.nsExports = map[string]map[string]bool{}
	}
	if e.nsExports[full] == nil {
		e.nsExports[full] = map[string]bool{}
	}
	e.nsStack = append(e.nsStack, name)
	// Members prescanned file-wide (see prescanNamespaces); only queue
	// here. If an earlier statement already refused, still queue
	// (accumulate-style: the drain stops at the first refusal inside).
	e.pendingNs = append(e.pendingNs, pendingNsBody{
		path:    append([]string{}, e.nsStack...),
		members: members,
	})
	e.nsStack = e.nsStack[:len(e.nsStack)-1]
}

// prescanNamespaces registers every top-level namespace scope and its
// members before any lowering (nested scopes recurse inside nsPreScan).
// Ambient blocks and string-named modules skip, mirroring lowerNamespace.
func (e *emitter) prescanNamespaces(stmts []*ast.Node) {
	for _, st := range stmts {
		if st.Kind != ast.KindModuleDeclaration || isAmbientModule(st) {
			continue
		}
		name, ok := moduleDeclName(st)
		if !ok {
			continue
		}
		if e.namespaces == nil {
			e.namespaces = map[string]bool{}
		}
		e.namespaces[name] = true
		if e.nsExports == nil {
			e.nsExports = map[string]map[string]bool{}
		}
		if e.nsExports[name] == nil {
			e.nsExports[name] = map[string]bool{}
		}
		saved := e.nsStack
		e.nsStack = append([]string{}, name)
		e.nsPreScan(moduleMemberStmts(st))
		e.nsStack = saved
	}
}

// pendingNsBody is one namespace body's deferred member lowering: the
// path active at prescan plus its members, drained after the file's
// definition pass (see lowerPendingNamespaces).
type pendingNsBody struct {
	path    []string
	members []*ast.Node
}

// lowerPendingNamespaces drains deferred namespace member lowering in
// source order (emission order is irrelevant: @labels resolve file-wide).
// Each body lowers under its prescan path; a refusal stops the drain and
// drops the rest (their diagnostics would cascade). Re-entrant: members
// that declare nested namespaces append during the drain and are picked
// up by the same loop.
func (e *emitter) lowerPendingNamespaces() {
	if len(e.pendingNs) == 0 {
		return
	}
	saved := e.nsStack
	for len(e.pendingNs) > 0 {
		b := e.pendingNs[0]
		e.pendingNs = e.pendingNs[1:]
		e.nsStack = append([]string{}, b.path...)
		for _, m := range b.members {
			e.lowerNamespaceMember(m)
			if e.refused {
				break
			}
		}
		if e.refused {
			e.pendingNs = nil
			break
		}
	}
	e.nsStack = saved
}

// memberExported reports an `export`-modified member (namespace-private
// members lower too, but outside access refuses).
func memberExported(m *ast.Node) bool {
	return hasModifier(m, ast.KindExportKeyword)
}

// nsPreScan registers member signatures qualified (forward calls resolve;
// mirrors the lowerSourceFile pre-scan) and records member kinds for
// qualify()/dotted dispatch. Collisions with existing definitions refuse.
func (e *emitter) nsPreScan(members []*ast.Node) {
	for _, m := range members {
		// Nested namespaces pre-scan under their extended path.
		if m.Kind == ast.KindModuleDeclaration && !isAmbientModule(m) {
			nm, ok := moduleDeclName(m)
			if !ok {
				continue
			}
			sub := append(append([]string{}, e.nsStack...), nm)
			saved := e.nsStack
			e.nsStack = sub
			subPath := e.nsPath()
			// Register the nested scope name at prescan (member bodies
			// lower deferred; routing needs the name before any lowering).
			if e.namespaces == nil {
				e.namespaces = map[string]bool{}
			}
			e.namespaces[subPath] = true
			if e.nsExports == nil {
				e.nsExports = map[string]map[string]bool{}
			}
			if e.nsExports[subPath] == nil {
				e.nsExports[subPath] = map[string]bool{}
			}
			e.nsPreScan(moduleMemberStmts(m))
			e.nsStack = saved
			if e.refused {
				return
			}
			continue
		}
		raw := ""
		kind := ""
		switch m.Kind {
		case ast.KindFunctionDeclaration:
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			raw = m.Name().Text()
			kind = nsKindFunc
		case ast.KindVariableStatement:
			dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			// Mutable multi-declarator lets register per declarator
			// (each scalar non-arrow declarator is an independent
			// slot, same as file-scope preRegisterModStates). Any
			// exotic declarator skips the whole statement so the
			// drain refuses loudly as before.
			if m.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsConst == 0 {
				var letNames []string
				for _, d := range dl.Declarations.Nodes {
					nm, ok := bindingNameText(d)
					if !ok {
						letNames = nil
						break
					}
					if init := d.Initializer(); init != nil && init.Kind == ast.KindArrowFunction {
						letNames = nil
						break
					}
					letNames = append(letNames, nm)
				}
				if len(letNames) == 0 {
					continue
				}
				for _, nm := range letNames {
					e.nsRegisterOne(m, nm, nsKindLet)
					if e.refused {
						return
					}
				}
				continue
			}
			// Only `const` members reach here (mutable members return
			// through the per-declarator path above). Multi-declarator
			// consts register per declarator (arrow declarators keep
			// arrow kind for call routing; literal ones fold below).
			// Any declarator without a plain name skips the whole
			// statement so the drain refuses loudly as before.
			type nsConstPart struct {
				name string
				kind string
			}
			var parts []nsConstPart
			for _, d := range dl.Declarations.Nodes {
				nm, ok := bindingNameText(d)
				if !ok {
					parts = nil
					break
				}
				dkind := nsKindConst
				if init := d.Initializer(); init != nil && init.Kind == ast.KindArrowFunction {
					dkind = nsKindArrow
				}
				parts = append(parts, nsConstPart{name: nm, kind: dkind})
			}
			if len(parts) == 0 {
				continue
			}
			for _, p := range parts {
				if !e.nsRegisterOne(m, p.name, p.kind) {
					return
				}
			}
			continue
		case ast.KindClassDeclaration:
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			raw = m.Name().Text()
			kind = nsKindClass
		case ast.KindEnumDeclaration:
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			raw = m.Name().Text()
			kind = nsKindEnum
		case ast.KindInterfaceDeclaration:
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			raw = m.Name().Text()
			kind = nsKindInterface
		case ast.KindTypeAliasDeclaration:
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			raw = m.Name().Text()
			kind = nsKindType
		default:
			continue
		}
		if !e.nsRegisterOne(m, raw, kind) {
			return
		}
	}
}

// nsRegisterOne registers one prescanned member (qualified name, kind,
// value and type recording). It reports false when prescan must abort
// (a loud refuse was emitted); true means registered or idempotently
// skipped (same-node re-prescan). Multi-declarator lets call it once per
// declarator with the shared statement node.
func (e *emitter) nsRegisterOne(m *ast.Node, raw, kind string) bool {
	q := e.nsDefName(raw)
	// Prescans repeat (file-wide pass plus per-body passes): same-node
	// re-registration skips wholesale (slot registration refuses
	// duplicates, so replaying it would false-refuse). A DIFFERENT
	// node under an occupied member name is a duplicate (reopening or
	// not, TS rejects it); fresh collisions with outside definitions
	// refuse as before.
	if prev, ok := e.nsMemberNodes[q]; ok {
		if prev != m {
			e.refuse(m, "namespace member %s is already declared (duplicates are not lowerable)", q)
			return false
		}
		return true
	}
	if e.nsNameTaken(q) && !(e.linkSeeded[q] && !e.directTopFuncs[q]) {
		e.refuse(m, "namespace member %s collides with an existing definition", q)
		return false
	}
	if e.nsMembers == nil {
		e.nsMembers = map[string]string{}
	}
	if e.nsMemberNodes == nil {
		e.nsMemberNodes = map[string]*ast.Node{}
	}
	e.nsMembers[q] = kind
	e.nsMemberNodes[q] = m
	if memberExported(m) {
		e.nsExports[e.nsPath()][raw] = true
	}
	if kind == nsKindFunc {
		e.registerNsFuncSig(m, q)
	}
	// Value recording lives in prescan (not drain): consts fold and
	// lets register slots now, so cross-body member reads and later
	// top-level code resolve. Both are pure recording (literal inits
	// only); the drain replays idempotently (see lowerNamespaceMember).
	if kind == nsKindConst {
		e.foldNsConstMember(m, q)
	}
	if kind == nsKindLet {
		e.registerNsLetMember(m, q)
	}
	// Type-space recording lives in prescan (not drain): layouts,
	// enums and classes resolve for code lowered before the drain
	// (e.g. a top-level main calling N.f, or N.f itself). Same-node
	// re-prescan overwrites identically; drain skips re-recording.
	switch kind {
	case nsKindClass:
		if _, ok := e.classDefs[q]; !ok {
			e.recordClass(m)
			if e.refused {
				return false
			}
		}
	case nsKindInterface, nsKindEnum, nsKindType:
		e.lowerTypeDecl(m)
		if e.refused {
			return false
		}
	}
	return true
}

// foldNsConstMember folds one literal const member (mirrors the literal
// arms of tryTopLevelConst, including pure backticks; arrow members stay
// callees for the drain). It folds the declarator whose qualified name
// matches q, so multi-declarator consts fold per member. Recording-only:
// no code emits, so prescan order never matters.
func (e *emitter) foldNsConstMember(m *ast.Node, q string) {
	dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	var d *ast.Node
	for _, cand := range dl.Declarations.Nodes {
		nm, ok := bindingNameText(cand)
		if !ok {
			continue
		}
		if e.nsDefName(nm) != q {
			continue
		}
		d = cand
		break
	}
	if d == nil {
		return
	}
	init := d.Initializer()
	if init == nil {
		return
	}
	switch init.Kind {
	case ast.KindNumericLiteral, ast.KindTrueKeyword, ast.KindFalseKeyword:
		if e.constVals == nil {
			e.constVals = map[string]string{}
		}
		if init.Kind == ast.KindTrueKeyword {
			e.constVals[q] = "1"
		} else if init.Kind == ast.KindFalseKeyword {
			e.constVals[q] = "0"
		} else {
			e.constVals[q] = init.Text()
		}
	case ast.KindStringLiteral:
		s, ok := stringLiteralText(init)
		if !ok {
			return
		}
		if e.constVals == nil {
			e.constVals = map[string]string{}
		}
		if e.constIsStr == nil {
			e.constIsStr = map[string]bool{}
		}
		e.constVals[q] = s
		e.constIsStr[q] = true
	case ast.KindNoSubstitutionTemplateLiteral:
		// Pure backtick consts fold like string literals (same cooked
		// n.Text() the value-position lowering feeds to
		// lowerStringLiteral; escapes preserved).
		if e.constVals == nil {
			e.constVals = map[string]string{}
		}
		if e.constIsStr == nil {
			e.constIsStr = map[string]bool{}
		}
		e.constVals[q] = init.Text()
		e.constIsStr[q] = true
	case ast.KindPropertyAccessExpression:
		pa := init.AsPropertyAccessExpression()
		if pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "Math" {
			if _, ok := projectionByTS("Math." + pa.Name().Text()); ok {
				if e.mathAliases == nil {
					e.mathAliases = map[string]string{}
				}
				e.mathAliases[q] = pa.Name().Text()
			}
		}
	}
}

// registerNsLetMember registers one mutable scalar member slot (pure
// recording; declarations emit no code). It locates the declarator whose
// qualified name matches q, so multi-declarator lets register per member.
// Refusals (exotic inits) surface here at prescan with the same diagnostics
// as the drain path.
func (e *emitter) registerNsLetMember(m *ast.Node, q string) {
	dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	for _, d := range dl.Declarations.Nodes {
		nm, ok := bindingNameText(d)
		if !ok {
			continue
		}
		if e.nsDefName(nm) != q {
			continue
		}
		if init := d.Initializer(); init != nil && init.Kind == ast.KindArrowFunction {
			return
		}
		e.registerModDeclarator(d, q)
		return
	}
}

// nsNameTaken reports qualified-name collisions against every definition
// table (duplicate @labels would collide at assembly).
func (e *emitter) nsNameTaken(q string) bool {
	if _, ok := e.funcSigs[q]; ok {
		return true
	}
	if _, ok := e.arrowAliases[q]; ok {
		return true
	}
	if _, ok := e.constVals[q]; ok {
		return true
	}
	if _, ok := e.modVars[q]; ok {
		return true
	}
	if _, ok := e.classDefs[q]; ok {
		return true
	}
	if _, ok := e.staticDefs[q]; ok {
		return true
	}
	if _, ok := e.enums[q]; ok {
		return true
	}
	if _, ok := e.layouts[q]; ok {
		return true
	}
	if e.namespaces[q] {
		return true
	}
	return false
}

// registerNsFuncSig pre-registers one qualified function signature
// (mirrors the lowerSourceFile pre-scan: ret, arity, defaults, rest).
func (e *emitter) registerNsFuncSig(m *ast.Node, q string) {
	ret := tVoid
	if fd := m.AsFunctionDeclaration(); fd.Type != nil {
		ret = annotationType(fd.Type)
		if ret == tUnknown {
			ret = tI32
		}
	}
	if e.funcSigs == nil {
		e.funcSigs = map[string]saType{}
	}
	// Program links pre-seed cross-file signatures; never clobber.
	if _, ok := e.funcSigs[q]; !ok {
		e.funcSigs[q] = ret
	}
	params := m.Parameters()
	if e.funcParams == nil {
		e.funcParams = map[string]int{}
	}
	if _, ok := e.funcParams[q]; !ok {
		e.funcParams[q] = len(params)
	}
	defs := make([]bool, len(params))
	for i, p := range params {
		if pd := p.AsParameterDeclaration(); pd.Initializer != nil {
			defs[i] = true
		}
	}
	if e.funcDefaults == nil {
		e.funcDefaults = map[string][]bool{}
	}
	if _, ok := e.funcDefaults[q]; !ok {
		e.funcDefaults[q] = defs
	}
	if len(params) > 0 {
		if pd := params[len(params)-1].AsParameterDeclaration(); pd.DotDotDotToken != nil {
			if e.funcHasRest == nil {
				e.funcHasRest = map[string]bool{}
			}
			e.funcHasRest[q] = true
		}
	}
}

// lowerNamespaceMember lowers one namespace body member with the prefix
// active. Body-less functions are overload signatures (the implementation
// carries the body); declare-marked members are ambient and skipped.
func (e *emitter) lowerNamespaceMember(m *ast.Node) {
	if hasModifier(m, ast.KindDeclareKeyword) {
		return
	}
	switch m.Kind {
	case ast.KindFunctionDeclaration:
		if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
			e.refuse(m, "computed function names are not in the SA-lowerable subset")
			return
		}
		if m.BodyData().Body == nil {
			// Overload signature: the implementation lowers the body.
			// A lone signature with no implementation never defines its
			// qualified name, so uses refuse as unknown functions.
			return
		}
		e.lowerFunction(m)
	case ast.KindVariableStatement:
		// Mutable scalar members lower to module-state slots (same
		// mechanism as file-scope `let`; see modstate.go). `const`
		// members keep the fold/arrow path below. Registration happens
		// here (member order, like const folds): reads from earlier
		// members refuse loudly at check time, same as consts today.
		if m.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsConst == 0 {
			dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			// Multi-declarator lets lower per declarator (each scalar
			// non-arrow declarator is an independent slot). A statement
			// is only eligible when EVERY declarator qualifies; otherwise
			// the single-shape refuses below name the real gap (splitting
			// still fixes multi-declarator statements).
			eligible := len(dl.Declarations.Nodes) > 0
			for _, d := range dl.Declarations.Nodes {
				if _, ok := bindingNameText(d); !ok {
					eligible = false
					break
				}
				if init := d.Initializer(); init != nil && init.Kind == ast.KindArrowFunction {
					eligible = false
					break
				}
			}
			if !eligible {
				if len(dl.Declarations.Nodes) != 1 {
					e.refuse(m, "multi-declarator mutable namespace state is not lowerable (split into single declarations)")
					return
				}
				d := dl.Declarations.Nodes[0]
				if _, ok := bindingNameText(d); !ok {
					e.refuse(m, "destructured mutable namespace state is not lowerable (scalar let only)")
					return
				}
				e.refuse(m, "arrow mutable namespace state is not lowerable (use const for callees)")
				return
			}
			// Already registered at prescan (cross-body reads resolve);
			// the drain replays only if prescan was bypassed. A present
			// slot is always ours: prescan refused genuine collisions.
			for _, d := range dl.Declarations.Nodes {
				nm, _ := bindingNameText(d)
				if _, ok := e.modVars[e.nsDefName(nm)]; ok {
					continue
				}
				// Declarations emit no code; use sites call the registry.
				e.registerModDeclarator(d, e.nsDefName(nm))
			}
			return
		}
		// Arrow consts emit callees; pure literals fold (both qualified
		// via the active prefix inside the shared helpers). Anything
		// else refuses loudly below.
		if e.tryTopLevelArrow(m) {
			return
		}
		if e.tryTopLevelConst(m) {
			return
		}
		e.refuse(m, "namespace const initializers must be pure literals or arrows")
	case ast.KindClassDeclaration:
		// Recorded at prescan (layouts resolve before the drain);
		// re-record only if something else claimed the name first
		// (then the prescan collision rule already refused). Nameless
		// members skipped prescan and record here as before.
		needsRecord := true
		if m.Name() != nil && m.Name().Kind == ast.KindIdentifier {
			if _, ok := e.classDefs[e.nsDefName(m.Name().Text())]; ok {
				needsRecord = false
			}
		}
		if needsRecord {
			e.recordClass(m)
		}
	case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
		// Layouts/enums record at prescan; the drain replays
		// idempotently for passes that bypass prescan.
		e.lowerTypeDecl(m)
	case ast.KindModuleDeclaration:
		if isAmbientModule(m) {
			return
		}
		e.lowerNamespace(m, true)
	default:
		e.refuse(m, "namespace member %s is not lowerable", m.Kind.String())
	}
}

// splitNsQualified splits a flattened "A_B_x" into its longest declared
// namespace prefix plus the trailing member path.
func (e *emitter) splitNsQualified(q string) (ns, member string, ok bool) {
	best := ""
	for name := range e.namespaces {
		if strings.HasPrefix(q, name+"_") && len(name) > len(best) {
			best = name
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, strings.TrimPrefix(q, best+"_"), true
}

// nsStaticText folds class static literals (instantiable defs and heritage
// shells alike). Private statics mangle by the lexical owner; without one
// (or without a match) the caller falls through to its loud refusal.
func nsStaticText(e *emitter, cls, field string) (staticVal, bool) {
	if strings.HasPrefix(field, "#") {
		if e.curMethodOwner == "" {
			return staticVal{}, false
		}
		field = privFieldKey(e.curMethodOwner, field)
	}
	if cd, ok := e.classDefs[cls]; ok {
		if sv, ok := cd.statics[field]; ok {
			return sv, true
		}
	}
	if cd, ok := e.staticDefs[cls]; ok {
		if sv, ok := cd.statics[field]; ok {
			return sv, true
		}
	}
	return staticVal{}, false
}

// checkNsAccess enforces namespace privacy: members accessed from outside
// their namespace need `export`. Inside (or nested-inside) access is free.
func (e *emitter) checkNsAccess(ns, member string, pos *ast.Node) bool {
	cur := e.nsPath()
	if cur == ns || (cur != "" && strings.HasPrefix(cur, ns+"_")) {
		return true
	}
	if e.nsExports[ns][member] {
		return true
	}
	e.refuse(pos, "%s.%s is not exported by its namespace", ns, member)
	return false
}

// nsMemberKind resolves a dotted member to its recorded kind ("", when the
// base is not a namespace or the member is unknown).
func (e *emitter) nsMemberKind(ns, member string) string {
	if !e.namespaces[ns] {
		return ""
	}
	return e.nsMembers[ns+"_"+member]
}

// lowerImportEquals lowers `import x = N.y` to a file-scope alias
// (x → flattened N_y; `import M = N` aliases the namespace path).
// All use positions route through qualify() (member aliases) plus two
// namespace-receiver branches below; declarations emit no code. Type-only
// aliases erase. Cross-file targets (`import {N} from` then `N.y`) and
// `require()` forms refuse loudly (listed gaps: cross-file members,
// bare imports).
func (e *emitter) lowerImportEquals(st *ast.Node) {
	ed := st.AsImportEqualsDeclaration()
	if ed.IsTypeOnly {
		return
	}
	if st.Name() == nil || st.Name().Kind != ast.KindIdentifier {
		e.refuse(st, "import-equals needs a plain local name")
		return
	}
	name := st.Name().Text()
	if _, dup := e.eqAliases[name]; dup {
		e.refuse(st, "import %s is already declared (duplicates are not lowerable)", name)
		return
	}
	if e.nsNameTaken(name) {
		e.refuse(st, "import %s collides with an existing definition", name)
		return
	}
	mr := ed.ModuleReference
	if mr == nil {
		e.refuse(st, "import-equals needs a module reference")
		return
	}
	if mr.Kind == ast.KindExternalModuleReference {
		e.refuse(st, "import %s = require(...) is not lowerable (bare third-party imports are Phase 3)", name)
		return
	}
	// A bare identifier (`import M = N`) aliases the whole namespace;
	// longer paths go through entityNameText (`A.B.y` → `A_B_y`).
	dotted := ""
	if mr.Kind == ast.KindIdentifier {
		dotted = mr.Text()
	} else if mr.Kind == ast.KindQualifiedName {
		dotted = entityNameText(mr.AsNode())
	} else {
		e.refuse(st, "import-equals needs a qualified namespace path (N.y)")
		return
	}
	if dotted == "" {
		e.refuse(st, "import-equals needs a plain dotted path (computed segments are not lowerable)")
		return
	}
	// Whole-namespace alias (`import M = N`): the receiver branches
	// below rewrite M to N for calls and reads.
	if e.namespaces[dotted] {
		if e.eqAliases == nil {
			e.eqAliases = map[string]string{}
		}
		e.eqAliases[name] = dotted
		return
	}
	// Member alias (`import x = N.y`, nested `A.B.y` included): the
	// namespace must exist, the member must exist and be exported
	// (privacy enforced at the alias, like the use site).
	ns, member, ok := e.splitNsQualified(dotted)
	if !ok {
		if e.isCrossFileRoot(dotted) {
			e.refuse(st, "import %s = %s is not lowerable (cross-file member aliases: import the member's module directly)", name, dotted)
		} else {
			e.refuse(st, "import %s = %s names no declared namespace", name, dotted)
		}
		return
	}
	if e.nsMemberKind(ns, member) == "" {
		e.refuse(st, "%s has no member %s", ns, member)
		return
	}
	if exps, ok := e.nsExports[ns]; !ok || !exps[member] {
		e.refuse(st, "%s.%s is not exported by its namespace", ns, member)
		return
	}
	if e.eqAliases == nil {
		e.eqAliases = map[string]string{}
	}
	e.eqAliases[name] = ns + "_" + member
}

// isCrossFileRoot reports dotted paths whose root is an imported name
// (as opposed to a typo'd same-file namespace).
func (e *emitter) isCrossFileRoot(dotted string) bool {
	for name := range e.importedNames {
		if dotted == name || strings.HasPrefix(dotted, name+"_") {
			return true
		}
	}
	for ns := range e.nsImports {
		if dotted == ns || strings.HasPrefix(dotted, ns+"_") {
			return true
		}
	}
	for spec := range e.importEnv {
		if dotted == spec || strings.HasPrefix(dotted, spec+"_") {
			return true
		}
	}
	return false
}

// isValueReceiver reports runtime value bindings that shadow namespace
// interpretation at call sites (instances, slices, handles, locals).
func (e *emitter) isValueReceiver(name string) bool {
	if e.lookupBinding(name) != nil {
		return true
	}
	if _, ok := e.varClass[name]; ok {
		return true
	}
	if e.layoutOfVar(name) != nil {
		return true
	}
	if e.arrVars[name] || e.strVars[name] || e.mapVars[name] || e.setVars[name] || e.domVars[name] {
		return true
	}
	return false
}

// lowerNamespaceCallSite routes `NS.f(...)` and `A.B.g(...)` to qualified
// callees. Reports ok=false when the callee is not namespace-shaped (the
// caller falls through to the existing dispatch).
func (e *emitter) lowerNamespaceCallSite(fn *ast.Node, pa *ast.PropertyAccessExpression, method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	if full, ok := dottedBaseName(fn); ok {
		// A shadowing value at the root wins (locals shadow namespaces).
		if r := dottedRoot(fn); r != "" && e.isValueReceiver(r) {
			return "", tUnknown, false
		}
		if ns, mem, ok := e.splitNsQualified(full); ok {
			if e.nsMemberKind(ns, mem) == "" {
				e.refuse(pos, "%s has no member %s", ns, mem)
				return "0", tUnknown, true
			}
			return e.lowerNamespaceCall(ns, mem, args, argNodes, pos)
		}
	}
	if pa.Expression.Kind == ast.KindIdentifier {
		recv := pa.Expression.Text()
		// Namespace aliases (`import M = N`) rewrite the receiver;
		// a shadowing local value still wins (checked on the original).
		if q, ok := e.eqAliases[recv]; ok && e.namespaces[q] && !e.isValueReceiver(recv) {
			recv = q
		}
		if e.namespaces[recv] && !e.isValueReceiver(recv) {
			if e.nsMemberKind(recv, method) == "" {
				e.refuse(pos, "%s has no member %s", recv, method)
				return "0", tUnknown, true
			}
			return e.lowerNamespaceCall(recv, method, args, argNodes, pos)
		}
	}
	return "", tUnknown, false
}

// (functions carry no receiver; consts/classes/enums refuse as callees).
// dottedRoot returns the leftmost identifier of a dotted chain ("" when
// the root is this/super/computed).
func dottedRoot(n *ast.Node) string {
	cur := n
	for cur != nil && cur.Kind == ast.KindPropertyAccessExpression {
		cur = cur.AsPropertyAccessExpression().Expression
	}
	if cur != nil && cur.Kind == ast.KindIdentifier {
		return cur.Text()
	}
	return ""
}

// lowerNamespaceCall lowers `NS.f(args)` to a direct qualified call
// (functions carry no receiver; consts/classes/enums refuse as callees).
func (e *emitter) lowerNamespaceCall(ns, method string, args []string, argNodes *ast.ElementList, pos *ast.Node) (string, saType, bool) {
	kind := e.nsMemberKind(ns, method)
	if kind == "" {
		return "", tUnknown, false
	}
	if !e.checkNsAccess(ns, method, pos) {
		return "0", tUnknown, true
	}
	q := ns + "_" + method
	switch kind {
	case nsKindFunc, nsKindArrow:
		if !e.checkArity(q, args, pos) {
			return "0", tUnknown, true
		}
		ret := e.funcSigs[q]
		if ai, ok := e.arrowAliases[q]; ok {
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
		if ret == tVoid {
			e.emit("call @%s(%s)", e.fnRef(q), strings.Join(args, ", "))
			return "0", tVoid, true
		}
		t := e.freshTmp()
		e.emit("%s = call @%s(%s)", t, e.fnRef(q), strings.Join(args, ", "))
		e.ownTemp(t)
		return t, ret, true
	default:
		e.refuse(pos, "%s.%s is not callable", ns, method)
		return "0", tUnknown, true
	}
}

// lowerNamespaceMemberRead folds `NS.CONST` / `NS.E` value reads: pure
// consts inline, enums refuse as bare values (members fold per-access like
// top-level enums), classes/interfaces refuse as values.
func (e *emitter) lowerNamespaceMemberRead(ns, member string, n *ast.Node) (string, saType, bool) {
	kind := e.nsMemberKind(ns, member)
	if kind == "" {
		return "", tUnknown, false
	}
	if !e.checkNsAccess(ns, member, n) {
		return "0", tUnknown, true
	}
	q := ns + "_" + member
	if kind == nsKindConst {
		if lit, ok := e.constVals[q]; ok {
			if e.constIsStr[q] {
				return e.lowerStringLiteral(lit), tString, true
			}
			if isFloatLiteral(lit) {
				return lit, tF64, true
			}
			return lit, tI32, true
		}
		// Math aliases are callable, not readable (first-class values
		// refuse like top-level aliases at call sites).
	}
	// Mutable members read through their slot (nil when the declaration
	// was refused or sorts after this use: fall through to the loud
	// refusal below, same as consts today).
	if kind == nsKindLet {
		if ms := e.modVars[q]; ms != nil {
			v, t := e.emitModLoad(ms, n)
			return v, t, true
		}
	}
	e.refuse(n, "%s.%s is not a value (only const members read as values)", ns, member)
	return "0", tUnknown, true
}

// nsLetTarget resolves a dotted access to a mutable namespace member:
// qualified slot name plus its namespace/member split ("", "", "", false
// when the target is not a let member). Single `N.x` honors value
// shadowing; nested `A.B.x` resolves longest-namespace-first (mirrors
// call-site routing).
func (e *emitter) nsLetTarget(n *ast.Node) (q, ns, member string, ok bool) {
	if n.Kind != ast.KindPropertyAccessExpression {
		return "", "", "", false
	}
	pa := n.AsPropertyAccessExpression()
	if pa.Expression.Kind == ast.KindIdentifier {
		base := pa.Expression.Text()
		if e.namespaces[base] && !e.isValueReceiver(base) {
			if e.nsMemberKind(base, pa.Name().Text()) == nsKindLet {
				return base + "_" + pa.Name().Text(), base, pa.Name().Text(), true
			}
		}
	}
	if full, ok := dottedBaseName(n); ok {
		if ns, mem, ok := e.splitNsQualified(full); ok {
			if e.nsMemberKind(ns, mem) == nsKindLet {
				return ns + "_" + mem, ns, mem, true
			}
		}
	}
	return "", "", "", false
}

// nsObjFieldTarget resolves `N.obj.field` (and deeper `A.B.obj.field`)
// to its object slot plus field: peel the last segment, resolve the
// outer as a let member, require an object slot. Privacy enforced by
// the caller via checkNsAccess (same as the member itself).
func (e *emitter) nsObjFieldTarget(n *ast.Node) (ms *modState, field, ns, member string, ok bool) {
	if n.Kind != ast.KindPropertyAccessExpression {
		return nil, "", "", "", false
	}
	pa := n.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindPropertyAccessExpression {
		return nil, "", "", "", false
	}
	if pa.Name() == nil || pa.Name().Kind != ast.KindIdentifier {
		return nil, "", "", "", false
	}
	q, qns, qmem, found := e.nsLetTarget(pa.Expression)
	if !found {
		return nil, "", "", "", false
	}
	m := e.modVars[q]
	if m == nil || !m.isObj {
		return nil, "", "", "", false
	}
	return m, pa.Name().Text(), qns, qmem, true
}

// entityNameText flattens type-level entity names to the namespace path
// form (`NS.I` -> "NS_I", matching qualified layout keys). Unknown shapes
// yield "" (never panics on hostile input).
func entityNameText(n *ast.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == ast.KindIdentifier {
		return n.Text()
	}
	if n.Kind == ast.KindQualifiedName {
		q := n.AsQualifiedName()
		l := entityNameText(q.Left)
		r := ""
		if q.Right != nil && q.Right.Kind == ast.KindIdentifier {
			r = q.Right.Text()
		}
		if l == "" || r == "" {
			return ""
		}
		return l + "_" + r
	}
	return ""
}

// collectNsProgramSigs walks runtime namespace bodies for the program-link
// scratch pass, recording qualified function/arrow signatures into the
// shared per-file tables (same-file calls resolve; ambient blocks skip).
func collectNsProgramSigs(members []*ast.Node, prefix string, rets map[string]saType, arity map[string]int, rest map[string]bool, defs map[string][]bool) {
	q := func(raw string) string {
		if prefix == "" {
			return raw
		}
		return prefix + "_" + raw
	}
	for _, m := range members {
		if m.Kind == ast.KindModuleDeclaration {
			if isAmbientModule(m) {
				continue
			}
			nm, ok := moduleDeclName(m)
			if !ok {
				continue
			}
			sub := nm
			if prefix != "" {
				sub = prefix + "_" + nm
			}
			collectNsProgramSigs(moduleMemberStmts(m), sub, rets, arity, rest, defs)
			continue
		}
		if m.Kind == ast.KindFunctionDeclaration {
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			name := q(m.Name().Text())
			ret := tVoid
			if fd := m.AsFunctionDeclaration(); fd.Type != nil {
				ret = annotationType(fd.Type)
				if ret == tUnknown {
					ret = tI32
				}
			}
			rets[name] = ret
			params := m.Parameters()
			arity[name] = len(params)
			d := make([]bool, len(params))
			for i, p := range params {
				if pd := p.AsParameterDeclaration(); pd.Initializer != nil {
					d[i] = true
				}
			}
			defs[name] = d
			if len(params) > 0 {
				if pd := params[len(params)-1].AsParameterDeclaration(); pd.DotDotDotToken != nil {
					rest[name] = true
				}
			}
			continue
		}
		if m.Kind == ast.KindVariableStatement {
			dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			if len(dl.Declarations.Nodes) != 1 {
				continue
			}
			d := dl.Declarations.Nodes[0]
			nm, ok := bindingNameText(d)
			if !ok {
				continue
			}
			if init := d.Initializer(); init == nil || init.Kind != ast.KindArrowFunction {
				continue
			}
			name := q(nm)
			rets[name] = tI32
			arity[name] = len(d.Initializer().Parameters())
		}
	}
}

// collectNsMembers records namespace callable members for cross-file
// imports into exp.nsMembers: ns -> member -> flat local (N_f,
// mirroring the sig keys in collectNsProgramSigs). Only function
// declarations and single arrow consts; classes, enums, consts and
// values stay out so their cross-file uses keep loud diagnostics.
// Nested namespaces recurse under their full path key.
func collectNsMembers(members []*ast.Node, prefix string, exp *fileExports) {
	flat := func(raw string) string {
		if prefix == "" {
			return raw
		}
		return prefix + "_" + raw
	}
	if exp.nsMembers == nil {
		exp.nsMembers = map[string]map[string]string{}
	}
	if exp.nsMembers[prefix] == nil {
		exp.nsMembers[prefix] = map[string]string{}
	}
	for _, m := range members {
		if m.Kind == ast.KindModuleDeclaration {
			if isAmbientModule(m) {
				continue
			}
			nm, ok := moduleDeclName(m)
			if !ok {
				continue
			}
			sub := nm
			if prefix != "" {
				sub = prefix + "_" + nm
			}
			collectNsMembers(moduleMemberStmts(m), sub, exp)
			continue
		}
		if m.Kind == ast.KindFunctionDeclaration {
			if m.Name() == nil || m.Name().Kind != ast.KindIdentifier {
				continue
			}
			exp.nsMembers[prefix][m.Name().Text()] = flat(m.Name().Text())
			continue
		}
		if m.Kind == ast.KindVariableStatement {
			dl := m.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
			if len(dl.Declarations.Nodes) != 1 {
				continue
			}
			d := dl.Declarations.Nodes[0]
			nm, ok := bindingNameText(d)
			if !ok {
				continue
			}
			if init := d.Initializer(); init == nil || init.Kind != ast.KindArrowFunction {
				continue
			}
			exp.nsMembers[prefix][nm] = flat(nm)
		}
	}
}

// collectNsConsts records namespace literal const members for cross-file
// imports into exp.nsConsts (ns -> member -> fold text; nsConstStr marks
// strings). Literal arms mirror foldNsConstMember; lets, non-const
// declarators and computed inits stay out so their uses keep loud
// diagnostics. Nested namespaces recurse under their full path key.
func collectNsConsts(members []*ast.Node, prefix string, exp *fileExports) {
	for _, m := range members {
		if m.Kind == ast.KindModuleDeclaration {
			if isAmbientModule(m) {
				continue
			}
			nm, ok := moduleDeclName(m)
			if !ok {
				continue
			}
			sub := nm
			if prefix != "" {
				sub = prefix + "_" + nm
			}
			collectNsConsts(moduleMemberStmts(m), sub, exp)
			continue
		}
		if m.Kind != ast.KindVariableStatement {
			continue
		}
		vs := m.AsVariableStatement()
		if vs.DeclarationList.Flags&ast.NodeFlagsConst == 0 {
			continue
		}
		dl := vs.DeclarationList.AsVariableDeclarationList()
		if len(dl.Declarations.Nodes) != 1 {
			continue
		}
		d := dl.Declarations.Nodes[0]
		member, ok := bindingNameText(d)
		if !ok {
			continue
		}
		init := d.Initializer()
		if init == nil {
			continue
		}
		var lit string
		isStr := false
		switch init.Kind {
		case ast.KindNumericLiteral:
			lit = init.Text()
		case ast.KindTrueKeyword:
			lit = "1"
		case ast.KindFalseKeyword:
			lit = "0"
		case ast.KindStringLiteral:
			s, ok := stringLiteralText(init)
			if !ok {
				continue
			}
			lit = s
			isStr = true
		default:
			continue
		}
		if exp.nsConsts == nil {
			exp.nsConsts = map[string]map[string]string{}
		}
		if exp.nsConsts[prefix] == nil {
			exp.nsConsts[prefix] = map[string]string{}
		}
		exp.nsConsts[prefix][member] = lit
		if isStr {
			if exp.nsConstStr == nil {
				exp.nsConstStr = map[string]map[string]bool{}
			}
			if exp.nsConstStr[prefix] == nil {
				exp.nsConstStr[prefix] = map[string]bool{}
			}
			exp.nsConstStr[prefix][member] = true
		}
	}
}
