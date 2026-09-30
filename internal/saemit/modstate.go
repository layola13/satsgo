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
// Scope: i32/i64/u64/f64 scalars (bool folds to i32), single-file and
// same-file program. Strings/arrays/objects (pointer ownership has no
// module-lifetime story), effectful initializers, and cross-file variable
// access refuse loudly. A name assigned anywhere in the file never folds
// into constVals (the old fold-then-rebind shape miscompiled silently:
// `sa=1` vs `node=2` on a counter, check-clean).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// modState is one lowered module variable: qualified name, scalar width,
// value-slot key, and init-flag key (0 when the zero fast path applies).
type modState struct {
	qual   string
	w      string // "i32" | "i64" | "u64" | "f64"
	isBool bool   // boolean-typed (renders as i32 0/1; typeof says boolean)
	key    uint64
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

// modKeyOf derives the value/flag slot keys for one qualified variable.
// Distinct domain strings keep value and flag cells apart without XOR hacks.
func (e *emitter) modKeyOf(qual string) (uint64, uint64) {
	base := "satsgo modstate v1\x00" + e.prefix + "\x00" + qual
	return fnv1a64("val\x00" + base), fnv1a64("flag\x00" + base)
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

// modInitOf classifies one declarator initializer: scalar literal (plus
// unary-minus numerics) or absent (zero fast path). Strings and exotic
// shapes report ok=false; the caller refuses loudly with the reason.
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
func modIsZero(imm string, w saType) bool {
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

// registerModState records one qualified scalar variable and returns its
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
	width := modWidthOf(w)
	key, flag := e.modKeyOf(qual)
	ms := &modState{qual: qual, w: width, isBool: w == tBool, key: key}
	if !noInit && !modIsZero(init, w) {
		ms.flag = flag
		ms.init = init
		// The init store widens the literal itself (never raw: negative
		// int and f64 immediates are not verbatim u64).
		ms.initW = iw
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
		if d.Initializer() != nil && d.Initializer().Kind == ast.KindStringLiteral {
			e.refuse(d, "string module state %s is not lowerable yet (scalar i32/i64/u64/f64 only)", qual)
		} else {
			e.refuse(d, "module state %s needs a scalar literal initializer", qual)
		}
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
		// same way); anything outside scalar widths refuses.
		if at == tString {
			e.refuse(d, "string module state %s is not lowerable yet (scalar i32/i64/u64/f64 only)", qual)
			return
		} else if at == tVoid {
			e.refuse(d, "module state %s needs a scalar literal initializer", qual)
			return
		} else if at != tI32 && at != tBool && at != tI64 && at != tU64 && at != tF64 {
			e.refuse(d, "module state %s needs a scalar type annotation (i32/i64/u64/f64/boolean)", qual)
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
// literals need boolean (same i32 width).
func modInitFitsSlot(at, iw saType, imm string) bool {
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
		if _, _, _, ok := modInitOf(init); !ok && init.Kind != ast.KindStringLiteral {
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
	default:
		return tI32
	}
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
	// The init value widens like any store (never verbatim: negative
	// int and f64 immediates are not verbatim u64).
	if wv, ok := e.modWiden(ms, ms.init, "imm", ms.initW, pos); ok {
		e.emitModSetRaw(ms.key, wv)
	}
	e.emitModSetRaw(ms.flag, "1")
	e.emit("jmp %s", doneL)
	e.emitRaw("%s:", doneL)
	e.terminated = false
	e.releaseIfOwnedTemp(f)
}

// modWiden normalizes one value operand to u64 bits for the set call:
// same-width ints retag via trunc, i32 widens via sext, ints enter f64 via
// sitofp, f64 bits spill through scratch. Anything else refuses loudly
// (handle copies never reach here: strings/arrays cannot register).
func (e *emitter) modWiden(ms *modState, src, srcKind string, srcType saType, pos *ast.Node) (string, bool) {
	_ = srcKind
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
// (trunc narrows, scratch round-trips f64 bits, u64 snapshots).
func (e *emitter) emitModLoad(ms *modState, pos *ast.Node) (string, saType) {
	e.emitModEnsure(ms, pos)
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
// store back. Postfix delivers the old value (JS semantics).
func (e *emitter) emitModIncDec(ms *modState, up, postfix bool, pos *ast.Node) (string, saType) {
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
