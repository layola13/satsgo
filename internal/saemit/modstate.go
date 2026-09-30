// Top-level mutable module state (`let`/`var` at file scope and namespace
// `export let`, one mechanism).
//
// SA-ASM has no globals and `@const` cells are readonly under AOT (the LLVM
// backend marks them constant), so mutable cells live in the sci slot
// registry behind value-semantics externs (sa_std/modstate.sai):
// `sa_modstate_get_u64(key)` returns opaque bits, `sa_modstate_set_u64`
// reports a status the emitter checks loudly. No pointers cross the
// boundary, so use sites hold no registry memory: no borrows, no releases,
// no exit-leak facets (pointer-crossing shapes were probed and rejected:
// shared borrows refuse stores, mut borrows lock their root).
//
// Keys are FNV-1a-64 over a domain string plus the file prefix plus the
// qualified name, so program-mode files cannot share cells by accident.
// Zero initializers (and uninitialized `let x: T`) need no init code: the
// registry zero-fills. Nonzero scalar literals init lazily once via a
// second flag slot, emitted as a use-site branch (mirrors the `??`
// join shape); every access (read or write) runs the ensure prologue so a
// write-first program cannot be clobbered by a later lazy init.
//
// Scope: i32/i64/u64/f64 scalars (bool folds to i32) plus string state,
// single-file and same-file program. Strings ride dual u64 slots
// (ptr+len) with literal-only stores (@const data is immortal; computed
// strings have no module-lifetime story and refuse loudly). Arrays/objects,
// effectful initializers, and cross-file variable access refuse loudly.
// A name assigned anywhere in the file never folds into constVals (the old
// fold-then-rebind shape miscompiled silently: `sa=1` vs `node=2` on a
// counter, check-clean).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// modStrW is the slot-width sentinel for strings (dual ptr+len slots;
// reads materialize a 16-byte header, mirroring lowerStringLiteral).
const modStrW = "str"

// modState is one lowered module variable: qualified name, scalar width,
// value-slot key (ptr slot for strings), and init-flag key (0 when the
// zero fast path applies; strings always flag).
type modState struct {
	qual   string
	w      string // "i32" | "i64" | "u64" | "f64" | modStrW
	isBool bool   // boolean-typed (renders as i32 0/1; typeof says boolean)
	key    uint64
	key2   uint64 // len slot (strings only, 0 for scalars)
	flag   uint64
	init   string // init immediate text ("" when zero fast path)
	initW  saType // init literal width (for widening the init store)
}

// modWidthOf normalizes a slot width to the SA store/load name.
func modWidthOf(t saType) string {
	switch t {
	case tI64:
		return "i64"
	case tU64:
		return "u64"
	case tF64:
		return "f64"
	default:
		return "i32"
	}
}

// fnv1a64 hashes the slot-key domain string (FNV-1a 64, same key family
// convention as the sarust thread_local! DefPath hashes).
func fnv1a64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// modKeyMask keeps slot keys in signed-63-bit range: the interpreter
// parses call immediates as i64, so full-range u64 keys overflow half the
// time (error.Overflow at run). 63 bits keep FNV-64 collision resistance
// for all practical purposes; the Zig registry takes any u64.
const modKeyMask = uint64(0x7FFFFFFFFFFFFFFF)

// modKeyOf derives the value/flag slot keys for one qualified variable.
// Distinct domain strings keep value and flag cells apart without XOR hacks.
func (e *emitter) modKeyOf(qual string) (uint64, uint64) {
	base := "satsgo modstate v1\x00" + e.prefix + "\x00" + qual
	return fnv1a64("val\x00"+base) & modKeyMask, fnv1a64("flag\x00"+base) & modKeyMask
}

// modStrKeyOf derives the ptr/len/flag slot keys for one qualified string
// variable (separate domains; never collide with scalar cells).
func (e *emitter) modStrKeyOf(qual string) (ptr, ln, flag uint64) {
	base := "satsgo modstate v1\x00" + e.prefix + "\x00" + qual
	return fnv1a64("strptr\x00"+base) & modKeyMask, fnv1a64("strlen\x00"+base) & modKeyMask, fnv1a64("strflag\x00"+base) & modKeyMask
}

// assignedNames walks whole files collecting bare names on the left of
// `=`, compound assignments, and `++`/`--`. Over-approximating (shadowed
// assigns count) is sound: it only routes more names to slots, and scope
// lookup still prefers locals at use sites. for-of/in bind a fresh
// "forof_elem" (or declare via assignLocal), so they never assign module
// bindings and stay out of the scan.
func assignedNames(stmts []*ast.Node) map[string]bool {
	out := map[string]bool{}
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case ast.KindBinaryExpression:
			bin := n.AsBinaryExpression()
			op := bin.OperatorToken.Kind
			if op == ast.KindEqualsToken || isCompoundAssign(op) {
				if bin.Left.Kind == ast.KindIdentifier {
					out[bin.Left.Text()] = true
				}
			}
		case ast.KindPrefixUnaryExpression:
			un := n.AsPrefixUnaryExpression()
			if (un.Operator == ast.KindPlusPlusToken || un.Operator == ast.KindMinusMinusToken) &&
				un.Operand.Kind == ast.KindIdentifier {
				out[un.Operand.Text()] = true
			}
		case ast.KindPostfixUnaryExpression:
			un := n.AsPostfixUnaryExpression()
			if (un.Operator == ast.KindPlusPlusToken || un.Operator == ast.KindMinusMinusToken) &&
				un.Operand.Kind == ast.KindIdentifier {
				out[un.Operand.Text()] = true
			}
		}
		for ch := range n.IterChildren() {
			if pureTypeKinds[ch.Kind] {
				continue
			}
			walk(ch)
		}
	}
	for _, st := range stmts {
		walk(st)
	}
	return out
}

// modStateOf resolves a bare-or-qualified name to its slot (nil when the
// name is not module state). Namespace bodies qualify first, matching
// reads; identity everywhere else.
func (e *emitter) modStateOf(name string) *modState {
	if e.modVars == nil {
		return nil
	}
	if ms := e.modVars[name]; ms != nil {
		return ms
	}
	if q := e.qualify(name); q != name {
		return e.modVars[q]
	}
	return nil
}

// modInitOf classifies one declarator initializer: scalar/string literal
// (plus unary-minus numerics) or absent (zero fast path). Exotic shapes
// report ok=false; the caller refuses loudly with the reason. String text
// mirrors the fold paths exactly (stringLiteralText / Text(), fed to
// lowerStringLiteral identically), so @const bytes stay consistent.
func modInitOf(init *ast.Node) (imm string, w saType, noInit, ok bool) {
	if init == nil {
		return "", tI32, true, true
	}
	switch init.Kind {
	case ast.KindNumericLiteral:
		if isFloatLiteral(init.Text()) {
			return init.Text(), tF64, false, true
		}
		return init.Text(), tI32, false, true
	case ast.KindTrueKeyword:
		return "1", tBool, false, true
	case ast.KindFalseKeyword:
		return "0", tBool, false, true
	case ast.KindStringLiteral:
		if s, ok := stringLiteralText(init); ok {
			return s, tString, false, true
		}
		return "", tUnknown, false, false
	case ast.KindNoSubstitutionTemplateLiteral:
		return init.Text(), tString, false, true
	case ast.KindPrefixUnaryExpression:
		un := init.AsPrefixUnaryExpression()
		if un.Operator == ast.KindMinusToken && un.Operand.Kind == ast.KindNumericLiteral {
			if isFloatLiteral(un.Operand.Text()) {
				return "-" + un.Operand.Text(), tF64, false, true
			}
			return "-" + un.Operand.Text(), tI32, false, true
		}
		return "", tUnknown, false, false
	default:
		return "", tUnknown, false, false
	}
}

// modIsZero reports numeric/bool zero initializers (any spelling the
// classifier accepts): the registry zero-fills, so no flag or branch.
// Strings never take the fast path (even "" materializes explicitly: a
// null ptr must never reach an extern).
func modIsZero(imm string, w saType) bool {
	if w == tString {
		return false
	}
	if w == tF64 {
		f := imm
		if len(f) > 0 && f[0] == '-' {
			f = f[1:]
		}
		return f == "0" || f == "0.0"
	}
	i := imm
	if len(i) > 0 && i[0] == '-' {
		i = i[1:]
	}
	return i == "0"
}

// registerModState records one qualified module variable and returns its
// state. Collisions with existing definitions refuse (same rule as
// namespace merging: the program is invalid TS, and sharing the name
// would resolve uses ambiguously).
func (e *emitter) registerModState(qual string, w, iw saType, init string, noInit bool, pos *ast.Node) (*modState, bool) {
	// Redefinition first (specific message wins over the generic
	// collision below, e.g. two `let x` in one file).
	if e.modVars != nil {
		if _, ok := e.modVars[qual]; ok {
			e.refuse(pos, "module variable %s is already declared (redefinition is not lowerable)", qual)
			return nil, false
		}
	}
	if e.nsNameTaken(qual) {
		e.refuse(pos, "module variable %s collides with an existing definition", qual)
		return nil, false
	}
	ms := &modState{qual: qual, w: modWidthOf(w), isBool: w == tBool}
	if w == tString {
		// Strings ride dual slots (ptr+len) with their own key family;
		// the flag always applies, even for "" (see modIsZero).
		ms.w = modStrW
		ms.key, ms.key2, ms.flag = e.modStrKeyOf(qual)
		ms.init = init
		ms.initW = iw
	} else {
		key, flag := e.modKeyOf(qual)
		ms.key = key
		if !noInit && !modIsZero(init, w) {
			ms.flag = flag
			ms.init = init
			// The init store widens the literal itself (never raw: negative
			// int and f64 immediates are not verbatim u64).
			ms.initW = iw
		}
	}
	if e.modVars == nil {
		e.modVars = map[string]*modState{}
	}
	e.modVars[qual] = ms
	e.needImport("sa_std/modstate.sai")
	return ms, true
}

// registerModDeclarator classifies one declarator and records its slot.
// Shared by top-level pre-registration and namespace member lowering.
func (e *emitter) registerModDeclarator(d *ast.Node, qual string) {
	imm, iw, noInit, ok := modInitOf(d.Initializer())
	if !ok {
		e.refuse(d, "module state %s needs a scalar or string literal initializer", qual)
		return
	}
	// An explicit annotation pins the slot width. NOTE: tUnknown shares
	// the "i32" spelling (house default-int), so test the node for nil
	// instead of comparing against tUnknown.
	w := iw
	if tn := d.AsVariableDeclaration().Type; tn != nil {
		at := annotationType(tn)
		// Widths share spellings (tString == tArray == ptr, tUnknown ==
		// tI32), so compare with ifs, not switch cases. An exotic
		// annotation collapsing to "i32" stays i32 (locals infer the
		// same way); anything outside scalar/string widths refuses.
		if at == tVoid {
			e.refuse(d, "module state %s needs a scalar or string literal initializer", qual)
			return
		} else if at != tI32 && at != tBool && at != tI64 && at != tU64 && at != tF64 && at != tString {
			e.refuse(d, "module state %s needs a scalar or string type annotation (i32/i64/u64/f64/boolean/string)", qual)
			return
		} else if !noInit && !modInitFitsSlot(at, iw, imm) {
			e.refuse(d, "module state %s initializer does not match its annotation", qual)
			return
		}
		w = at
	}
	_, _ = e.registerModState(qual, w, iw, imm, noInit, d)
}

// modInitFitsSlot reports whether a literal initializer fits an annotated
// slot: int literals fill any int width (zero fills f64 too, via the
// zero fast path which emits no init); float literals need f64; bool
// literals need boolean (same i32 width); string literals need string.
func modInitFitsSlot(at, iw saType, imm string) bool {
	if at == tString || iw == tString {
		return at == tString && iw == tString
	}
	aw, lw := modWidthOf(at), modWidthOf(iw)
	if aw == lw {
		return true
	}
	if lw == "i32" && (aw == "i64" || aw == "u64") {
		return iw == tI32
	}
	if aw == "f64" && lw == "i32" {
		return modIsZero(imm, iw)
	}
	return false
}

// modClaim reports whether one declarator belongs to the slot mechanism:
// named, assigned somewhere in the file, and initialized with nil or a
// scalar/string literal (arrows stay callees; effectful and exotic inits
// keep their existing fold/refuse paths untouched).
func (e *emitter) modClaim(d *ast.Node) (string, bool) {
	name, ok := bindingNameText(d)
	if !ok {
		return "", false
	}
	if !e.modAssigned[name] {
		return "", false
	}
	if init := d.Initializer(); init != nil {
		if init.Kind == ast.KindArrowFunction {
			return "", false
		}
		if _, _, _, ok := modInitOf(init); !ok {
			return "", false
		}
	}
	return name, true
}

// preRegisterModStates records every claimed top-level scalar declarator
// before any statement lowers, so forward reads from earlier functions
// resolve to slots (declarations emit no code; use sites call the
// registry). Unclaimed names stay out: folds and existing refusals own them.
func (e *emitter) preRegisterModStates(stmts []*ast.Node) {
	for _, st := range stmts {
		if st.Kind != ast.KindVariableStatement {
			continue
		}
		// Const declarations fold (reassignment refuses loudly at the
		// assign choke point); slots are for `let`/`var` only.
		if st.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsConst != 0 {
			continue
		}
		dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
		for _, d := range dl.Declarations.Nodes {
			name, ok := e.modClaim(d)
			if !ok {
				continue
			}
			e.registerModDeclarator(d, e.nsDefName(name))
		}
	}
}

// tryModState consumes top-level `let`/`var` statements with slot-bound
// declarators (pre-registered above; declarations emit no code). Reports
// false otherwise, so the caller falls through to the const fold. Arrow
// bindings stay out: callees win even when the name is assigned.
func (e *emitter) tryModState(st *ast.Node) bool {
	// Const declarations fold (see preRegisterModStates); slots are for
	// `let`/`var` only.
	if st.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsConst != 0 {
		return false
	}
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	for _, d := range dl.Declarations.Nodes {
		if _, ok := e.modClaim(d); ok {
			return true
		}
	}
	return false
}

// saTypeOfSlot maps the slot width back to its SA type.
func (ms *modState) saType() saType {
	switch ms.w {
	case "i64":
		return tI64
	case "u64":
		return tU64
	case "f64":
		return tF64
	case modStrW:
		return tString
	default:
		return tI32
	}
}

// modStrRecv pre-lowers an unshadowed module-string receiver to a header
// temp for method dispatch (locals shadow via scopes above; namespace
// members resolve qualified-first through modStateOf).
func (e *emitter) modStrRecv(recv string, pos *ast.Node) (string, bool) {
	if e.lookupBinding(recv) != nil {
		return "", false
	}
	ms := e.modStateOf(recv)
	if ms == nil || ms.w != modStrW {
		return "", false
	}
	h, _ := e.emitModLoad(ms, pos)
	return h, true
}

// emitModSetRaw stores one u64-valued operand to a raw slot key with the
// house status check (nonzero panics, mirroring emitStatusCheckedI64).
func (e *emitter) emitModSetRaw(key uint64, val string) {
	st := e.freshTmp()
	e.emit("%s = call @sa_modstate_set_u64(%d, %s)", st, key, val)
	e.ownTemp(st)
	badL := e.freshLabel("ms_bad")
	okL := e.freshLabel("ms_ok")
	bad := e.freshTmp()
	e.emit("%s = ne %s, 0", bad, st)
	e.emit("br %s -> %s, %s", bad, badL, okL)
	e.emitRaw("%s:", badL)
	// Registry OOM: same code as the THREAD_LOCAL_SLOT macro (1403,
	// same registry, same failure family).
	e.emit("panic(1403)")
	e.terminated = true
	e.emitRaw("%s:", okL)
	e.terminated = false
	e.releaseIfOwnedTemp(st)
}

// emitModEnsure runs the lazy-init-once prologue: flag 0 takes the init
// arm (value store plus flag set, both status-checked raw stores: the flag
// is unset by definition here, so no nested ensure), then joins. The zero
// fast path (flag == 0) emits nothing.
func (e *emitter) emitModEnsure(ms *modState, pos *ast.Node) {
	if ms.flag == 0 {
		return
	}
	f := e.freshTmp()
	e.emit("%s = call @sa_modstate_get_u64(%d)", f, ms.flag)
	e.ownTemp(f)
	c := e.freshTmp()
	doneL := e.freshLabel("ms_done")
	initL := e.freshLabel("ms_init")
	e.emit("%s = ne %s, 0", c, f)
	e.emit("br %s -> %s, %s", c, doneL, initL)
	e.emitRaw("%s:", initL)
	if ms.w == modStrW {
		// String inits materialize the literal (@const-immortal data)
		// into both slots; the header releases after the stores.
		e.emitModInitString(ms, pos)
	} else if wv, ok := e.modWiden(ms, ms.init, "imm", ms.initW, pos); ok {
		// The init value widens like any store (never verbatim: negative
		// int and f64 immediates are not verbatim u64).
		e.emitModSetRaw(ms.key, wv)
	}
	e.emitModSetRaw(ms.flag, "1")
	e.emit("jmp %s", doneL)
	e.emitRaw("%s:", doneL)
	e.terminated = false
	e.releaseIfOwnedTemp(f)
}

// emitModInitString materializes a string literal init into both slots.
// The @const bytes are process-immortal, so the header releases after
// the stores with no lifetime hazard (computed strings never reach here:
// only literal/response text flows through registration).
func (e *emitter) emitModInitString(ms *modState, pos *ast.Node) {
	_ = pos
	h := e.lowerStringLiteral(ms.init)
	pv := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", pv, h)
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, h)
	pu := e.freshTmp()
	e.emit("%s = trunc %s as u64", pu, pv)
	e.emitModSetRaw(ms.key, pu)
	e.emitModSetRaw(ms.key2, ln)
	e.releaseIfOwnedTemp(h)
}

// modStringText extracts literal response text for string stores: string
// literals and untagged templates mirror the fold paths; identifiers must
// name folded string consts (their @const data is immortal like literals).
func (e *emitter) modStringText(n *ast.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Kind {
	case ast.KindStringLiteral:
		return stringLiteralText(n)
	case ast.KindNoSubstitutionTemplateLiteral:
		return n.Text(), true
	case ast.KindIdentifier:
		if q := e.qualify(n.Text()); q != "" {
			if lit, ok := e.constVals[q]; ok && e.constIsStr[q] {
				return lit, true
			}
		}
		return "", false
	default:
		return "", false
	}
}

// emitModStoreString lowers one literal string assignment to a slot:
// ensure, materialize, dual set. The header transfers to the caller as
// the assignment value (chained stores keep a real slice); computed RHS
// never reaches here (the dispatch refuses first).
func (e *emitter) emitModStoreString(ms *modState, text string, pos *ast.Node) (string, saType, bool) {
	e.emitModEnsure(ms, pos)
	h := e.lowerStringLiteral(text)
	pv := e.freshTmp()
	e.emit("%s = load %s + 0 as ptr", pv, h)
	ln := e.freshTmp()
	e.emit("%s = load %s + 8 as u64", ln, h)
	pu := e.freshTmp()
	e.emit("%s = trunc %s as u64", pu, pv)
	e.emitModSetRaw(ms.key, pu)
	e.emitModSetRaw(ms.key2, ln)
	e.modStrMark(h)
	return h, tString, true
}

// emitModStoreStringDispatch routes one string-slot assignment: literal
// and folded-const RHS store directly; anything computed refuses loudly
// (its buffer has no module lifetime: storing the bits would dangle).
func (e *emitter) emitModStoreStringDispatch(ms *modState, rhs *ast.Node, pos *ast.Node) (string, saType, bool) {
	if text, ok := e.modStringText(rhs); ok {
		return e.emitModStoreString(ms, text, pos)
	}
	e.refuse(pos, "module state %s stores string literals and string constants only (computed strings are not lowerable yet)", ms.qual)
	return "0", tUnknown, false
}

// modWiden normalizes one value operand to u64 bits for the set call:
// same-width ints retag via trunc, i32 widens via sext, ints enter f64 via
// sitofp, f64 bits spill through scratch. String slots refuse here: their
// stores go through emitModStoreString with literal text (a widened temp
// cannot prove @const-immortal data). Anything else refuses loudly
// (handle copies never reach here: arrays cannot register).
func (e *emitter) modWiden(ms *modState, src, srcKind string, srcType saType, pos *ast.Node) (string, bool) {
	_ = srcKind
	if ms.w == modStrW {
		e.refuse(pos, "module state %s stores string literals and string constants only (computed strings are not lowerable yet)", ms.qual)
		return "0", false
	}
	sw := modWidthOf(srcType)
	if sw == "u64" && ms.w == "u64" {
		return src, true
	}
	if sw == ms.w {
		// Same width retags through trunc, except f64 bits which spill
		// through scratch (trunc needs int-like sources).
		if ms.w == "f64" {
			return e.modF64ToU64(src)
		}
		if ms.w == "u64" {
			return src, true
		}
		t := e.freshTmp()
		e.emit("%s = trunc %s as u64", t, src)
		return t, true
	}
	if ms.w == "f64" {
		if sw == "i32" || sw == "i64" || sw == "u64" {
			t := e.freshTmp()
			e.emit("%s = sitofp %s as f64", t, src)
			u, ok := e.modF64ToU64(t)
			return u, ok
		}
		e.refuse(pos, "module state %s needs an f64 value", ms.qual)
		return "0", false
	}
	if srcType == tF64 {
		if ms.w == "i32" || ms.w == "i64" || ms.w == "u64" {
			t := e.freshTmp()
			e.emit("%s = fptosi %s as %s", t, src, ms.w)
			return t, true
		}
		e.refuse(pos, "module state %s needs an integer value", ms.qual)
		return "0", false
	}
	if (sw == "i32" || sw == "i64" || sw == "u64") && (ms.w == "i32" || ms.w == "i64" || ms.w == "u64") {
		t := e.freshTmp()
		if sw == "i32" {
			e.emit("%s = sext %s as u64", t, src)
		} else {
			e.emit("%s = trunc %s as u64", t, src)
		}
		return t, true
	}
	e.refuse(pos, "module state %s needs a %s value", ms.qual, ms.w)
	return "0", false
}

// modF64ToU64 spills f64 bits to scratch and reloads them as u64 (exact
// round-trip; mirrors the probe shape, scratch released after the load).
func (e *emitter) modF64ToU64(src string) (string, bool) {
	sc := e.freshTmp()
	e.emit("%s = alloc 8", sc)
	e.ownTemp(sc)
	e.emit("store %s + 0, %s as f64", sc, src)
	u := e.freshTmp()
	e.emit("%s = load %s + 0 as u64", u, sc)
	e.releaseIfOwnedTemp(sc)
	return u, true
}

// emitModLoad reads one slot to a fresh temp carrying the slot width
// (trunc narrows, scratch round-trips f64 bits, u64 snapshots, strings
// materialize a header from the dual slots).
func (e *emitter) emitModLoad(ms *modState, pos *ast.Node) (string, saType) {
	e.emitModEnsure(ms, pos)
	if ms.w == modStrW {
		return e.emitModLoadString(ms, pos)
	}
	t := e.freshTmp()
	e.emit("%s = call @sa_modstate_get_u64(%d)", t, ms.key)
	e.ownTemp(t)
	switch ms.w {
	case "i64":
		n := e.freshTmp()
		e.emit("%s = trunc %s as i64", n, t)
		e.releaseIfOwnedTemp(t)
		return n, tI64
	case "u64":
		n := e.freshTmp()
		e.emit("%s = add %s, 0", n, t)
		e.releaseIfOwnedTemp(t)
		return n, tU64
	case "f64":
		sc := e.freshTmp()
		e.emit("%s = alloc 8", sc)
		e.ownTemp(sc)
		e.emit("store %s + 0, %s as u64", sc, t)
		e.releaseIfOwnedTemp(t)
		n := e.freshTmp()
		e.emit("%s = load %s + 0 as f64", n, sc)
		e.releaseIfOwnedTemp(sc)
		return n, tF64
	default:
		n := e.freshTmp()
		e.emit("%s = trunc %s as i32", n, t)
		e.releaseIfOwnedTemp(t)
		return n, tI32
	}
}

// emitModLoadString reads dual ptr+len slots into a fresh 16-byte header
// (caller-managed lifetime like any literal header; the slot bits stay
// put). The header temp registers in modStrTmps so handle-aware sites
// (class-inline/callback params) alias instead of half-copying.
func (e *emitter) emitModLoadString(ms *modState, pos *ast.Node) (string, saType) {
	_ = pos
	tp := e.freshTmp()
	e.emit("%s = call @sa_modstate_get_u64(%d)", tp, ms.key)
	e.ownTemp(tp)
	tl := e.freshTmp()
	e.emit("%s = call @sa_modstate_get_u64(%d)", tl, ms.key2)
	e.ownTemp(tl)
	h := e.freshTmp()
	e.emit("%s = alloc 16", h)
	e.ownTemp(h)
	e.emit("store %s + 0, %s as ptr", h, tp)
	e.emit("store %s + 8, %s as u64", h, tl)
	e.releaseIfOwnedTemp(tp)
	e.releaseIfOwnedTemp(tl)
	e.modStrMark(h)
	return h, tString
}

// modStrMark records a string header temp for handle-aware param sites.
func (e *emitter) modStrMark(h string) {
	if e.modStrTmps == nil {
		e.modStrTmps = map[string]bool{}
	}
	e.modStrTmps[h] = true
}

// emitModStore lowers one assignment to a slot: widen the value, ensure,
// status-checked set. Returns the stored value operand and slot type so
// chained assignments (`y = (x = 5)`) keep a real operand.
func (e *emitter) emitModStore(ms *modState, src, srcKind string, srcType saType, pos *ast.Node) (string, saType, bool) {
	wv, ok := e.modWiden(ms, src, srcKind, srcType, pos)
	if !ok {
		return "0", tUnknown, false
	}
	e.emitModEnsure(ms, pos)
	e.emitModSetRaw(ms.key, wv)
	var rt saType
	switch ms.w {
	case "i64":
		rt = tI64
	case "u64":
		rt = tU64
	case "f64":
		rt = tF64
	default:
		rt = tI32
	}
	// Immediates are free-floating; widened temps already carry the slot
	// width, so either flows on as the assignment value.
	if wv == src {
		return src, srcType, true
	}
	return wv, rt, true
}

// emitModIncDec lowers `x++`/`x--`/`++x`/`--x` on a slot: load, add/sub,
// store back. Postfix delivers the old value (JS semantics). Strings
// refuse: arithmetic on slice headers is meaningless.
func (e *emitter) emitModIncDec(ms *modState, up, postfix bool, pos *ast.Node) (string, saType) {
	if ms.w == modStrW {
		e.refuse(pos, "++/-- on string module state %s is not lowerable", ms.qual)
		return "0", tUnknown
	}
	cur, ct := e.emitModLoad(ms, pos)
	nw := e.freshTmp()
	one := "1"
	op := "add"
	if !up {
		op = "sub"
	}
	if ct == tF64 {
		op = "fadd"
		if !up {
			op = "fsub"
		}
		one = "1.0"
	}
	e.emit("%s = %s %s, %s", nw, op, cur, one)
	// The load already ensured init; the store re-ensures idempotently.
	// Same-width widening cannot refuse here, but propagate loudly anyway.
	if _, _, ok := e.emitModStore(ms, nw, "temp", ct, pos); !ok {
		return "0", tUnknown
	}
	if postfix {
		return cur, ct
	}
	return nw, ct
}
