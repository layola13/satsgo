package saemit

// TSX → direct airlock SA calls (road 2, first cut): static JSX trees
// lower to sax_dom_* calls that build the DOM at runtime, bypassing the
// .sax intermediate. Only static elements, static text and string-literal
// attributes are in the slice; params, interpolation, hooks, handlers,
// components, spreads and fragments refuse loudly for later slices.
//
// Consumer note: the airlock externs resolve at `sa react build` time
// (same standing as dom_proj.go); `@extern` declarations are emitted so
// `sa check` verifies the unit standalone. Dynamic values need a state
// system the direct path does not have yet (later slices).
import (
	"fmt"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tspath"
)

// TSXDirectResult is one direct-SA lowering outcome.
type TSXDirectResult struct {
	SAI         string
	Refused     bool
	Diagnostics []Diagnostic
}

// LowerTSXDirect lowers static tsx components to direct airlock calls.
func LowerTSXDirect(fileName, sourceText string) TSXDirectResult {
	abs := fileName
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	opts := ast.SourceFileParseOptions{FileName: abs, Path: tspath.ToPath(abs, "/", true)}
	sf := parser.ParseSourceFile(opts, sourceText, core.ScriptKindTSX)
	x := &tsxDirect{file: fileName, src: sourceText, lines: lineOffsets(sourceText)}
	x.e = &emitter{file: fileName, src: sourceText, lines: x.lines}
	x.lowerSourceFile(sf)
	return TSXDirectResult{SAI: x.e.finish(), Refused: x.e.refused, Diagnostics: x.e.diags}
}

type tsxDirect struct {
	file  string
	src   string
	lines []int
	e     *emitter
	used  map[string]bool
	// params holds string prop names (builder parameters, ptr slices);
	// strConsts maps useState vars with string-literal initials to their
	// text (initial-render constants); intConsts maps useState vars with
	// integer/boolean literal initials to their text (rendered through
	// renderInterpValue: sext + @sa_fmt_i64_into, so 0/1 stay "0"/"1");
	// setters marks setter names for loud refusal on use.
	params    map[string]bool
	strConsts map[string]string
	intConsts map[string]string
	setters   map[string]bool
	// handlers accumulates onClick handlers (emitted as @export fns
	// after the builder); binds pairs element handles with handlers
	// for bind_event emission once the root handle is known; constRefs
	// dedupes starred string consts (bind_event takes *name refs).
	handlers   []directHandler
	binds      []directBind
	handlerSeq int
	constRefs  map[string]string
}

// directHandler is one onClick handler: an @export function taking the
// component root handle as ctx (airlock calls exports[name](ctx)). No
// ffi wrapper: there is no react ctx struct to unpack and handler bodies
// cannot read events in this slice.
type directHandler struct {
	funcName string
	ctxName  string
	arrow    *ast.Node
	pos      *ast.Node
}

type directBind struct {
	elem    string
	handler int
}

// airlockExtern declares the used airlock imports with the exact runtime
// arities (sa_plugin_react airlock_gen.zig): handles are i64, (ptr,len)
// string pairs are (ptr, u64); setters return nothing.
var airlockExtern = map[string]string{
	"sax_dom_create":       "@extern sax_dom_create(tag_ptr: ptr, tag_len: u64) -> i64",
	"sax_dom_create_text":  "@extern sax_dom_create_text(text_ptr: ptr, text_len: u64) -> i64",
	"sax_dom_append_child": "@extern sax_dom_append_child(parent_h: i64, child_h: i64)",
	"sax_dom_set_attr":     "@extern sax_dom_set_attr(node_h: i64, key_ptr: ptr, key_len: u64, val_ptr: ptr, val_len: u64)",
	// bind_event uses the canonical starred/i64 form verbatim (react
	// build output): capability prefixes are enforced, temps do not
	// satisfy starred params.
	"sax_dom_bind_event": "@extern sax_dom_bind_event(node_h: i64, *evt_ptr: ptr, evt_len: i64, *handler_ptr: ptr, handler_len: i64, ctx: ptr) -> void",
	// set_text/remove_attr take temp args (like the other direct calls),
	// so their decls stay unstarred; bind_event above is the only
	// starred-const call shape (probed: mixing styles verifies clean).
	"sax_dom_set_text":   "@extern sax_dom_set_text(node_h: i64, text_ptr: ptr, text_len: u64)",
	"sax_dom_remove_attr": "@extern sax_dom_remove_attr(node_h: i64, key_ptr: ptr, key_len: u64)",
}

func (x *tsxDirect) useExtern(sym string) {
	if x.used == nil {
		x.used = map[string]bool{}
	}
	if x.used[sym] {
		return
	}
	x.used[sym] = true
	fmt.Fprintf(&x.e.header, "%s\n", airlockExtern[sym])
}

// strConstRef emits a shared utf8 const and returns its starred name
// plus byte length (bind_event takes *name refs, not temps).
func (x *tsxDirect) strConstRef(text string) (string, int) {
	if x.constRefs == nil {
		x.constRefs = map[string]string{}
	}
	if n, ok := x.constRefs[text]; ok {
		return n, len(text)
	}
	e := x.e
	e.tmp++
	name := fmt.Sprintf("str_const_%d", e.tmp)
	escaped := strings.ReplaceAll(text, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	escaped = strings.ReplaceAll(escaped, "\n", "\\n")
	escaped = strings.ReplaceAll(escaped, "\r", "\\r")
	escaped = strings.ReplaceAll(escaped, "\t", "\\t")
	fmt.Fprintf(&e.header, "@const %s = utf8:\"%s\\0\"\n", name, escaped)
	x.constRefs[text] = name
	return name, len(text)
}

func (x *tsxDirect) refuse(n *ast.Node, format string, args ...any) {
	x.e.refuse(n, "tsx-direct: "+format, args...)
}

func (x *tsxDirect) lowerSourceFile(sf *ast.SourceFile) {
	for _, st := range sf.AsSourceFile().Statements.Nodes {
		if st.Kind != ast.KindFunctionDeclaration {
			x.refuse(st, "only function components lower (got %s)", st.Kind.String())
			return
		}
		if st.Name() == nil || st.Name().Kind != ast.KindIdentifier {
			x.refuse(st, "component needs an identifier name")
			return
		}
		if !x.lowerComponent(st.Name().Text(), st) {
			return
		}
	}
}

// lowerComponent lowers one component to a @render_Name builder.
// Leading statements must be string useState declarations; the tail is
// a single return of JSX. Props come from a destructured first parameter
// with an inline object type whose fields are all strings.
func (x *tsxDirect) lowerComponent(name string, fn *ast.Node) bool {
	x.params = map[string]bool{}
	x.strConsts = map[string]string{}
	x.intConsts = map[string]string{}
	x.setters = map[string]bool{}
	x.handlers = nil
	x.binds = nil
	x.handlerSeq = 0
	psig := ""
	if len(fn.Parameters()) > 0 {
		var ok bool
		psig, ok = x.collectDirectProps(fn)
		if !ok {
			return false
		}
	}
	body := fn.BodyData().Body
	if body == nil {
		x.refuse(fn, "component %s has no body", name)
		return false
	}
	stmts := body.Statements()
	if len(stmts) == 0 {
		x.refuse(fn, "component %s must be a single return of JSX", name)
		return false
	}
	for _, st := range stmts[:len(stmts)-1] {
		if !x.lowerDirectUseState(st) {
			return false
		}
	}
	rs := stmts[len(stmts)-1]
	if rs.Kind != ast.KindReturnStatement {
		x.refuse(fn, "component %s must end with a single return of JSX", name)
		return false
	}
	ret := rs.AsReturnStatement()
	if ret.Expression == nil {
		x.refuse(rs, "component %s returns nothing", name)
		return false
	}
	x.e.emitRaw("@render_%s(%s) -> i64:", name, psig)
	x.e.emitRaw("L_ENTRY:")
	x.e.pushScope()
	x.e.terminated = false
	// String params are callee-owned (same convention as normal
	// functions: the end-of-body release drops them).
	for pname := range x.params {
		x.e.declareOwned(pname)
	}
	root, ok := x.lowerNode(ret.Expression)
	if !ok {
		return false
	}
	// Event bindings go out once the root handle is known (ctx=root
	// convention: handlers receive the component root).
	for _, b := range x.binds {
		ecName, ecLen := x.strConstRef("click")
		hcName, hcLen := x.strConstRef(x.handlers[b.handler].funcName)
		x.e.emit("call @sax_dom_bind_event(%s, *%s, %d, *%s, %d, %s)", b.elem, ecName, ecLen, hcName, hcLen, root)
		x.useExtern("sax_dom_bind_event")
	}
	x.e.releaseAllOwnedExcept(root)
	x.e.emit("return %s", root)
	x.e.terminated = true
	x.e.popScope()
	if !x.emitHandlers() {
		return false
	}
	return true
}

// emitHandlers lowers recorded onClick handlers as @export functions
// taking the component root as ctx (the airlock calls
// exports[name](ctx)). No ffi wrapper: there is no react ctx struct to
// unpack and handler bodies cannot read events in this slice.
func (x *tsxDirect) emitHandlers() bool {
	e := x.e
	for _, hd := range x.handlers {
		e.emitRaw("@export %s(%s: i64):", hd.funcName, hd.ctxName)
		e.emitRaw("L_ENTRY:")
		e.pushScope()
		e.terminated = false
		e.declareOwned(hd.ctxName)
		if e.domVars == nil {
			e.domVars = map[string]bool{}
		}
		e.domVars[hd.ctxName] = true
		// Body statements lower below; empty bodies were refused at
		// collection time.
		if !x.lowerHandlerBody(hd) {
			return false
		}
		if !e.terminated {
			e.releaseAllOwned()
			e.emit("return")
			e.terminated = true
		}
		e.popScope()
	}
	return true
}

// collectDirectProps reads a destructured first parameter with an inline
// object type whose fields are all strings (`function C({ n }: { n:
// string })`), returning the SA parameter list (`n: ptr, ...`). Mirrors
// the .sax collectProps shape rules; non-string fields refuse (integers
// need an int→string primitive the direct path does not have yet).
func (x *tsxDirect) collectDirectProps(fn *ast.Node) (string, bool) {
	fail := func(pos *ast.Node, format string, args ...any) (string, bool) {
		x.refuse(pos, format, args...)
		return "", false
	}
	params := fn.Parameters()
	if len(params) > 1 {
		return fail(params[1], "components take at most one parameter (the props object)")
	}
	pd := params[0].AsParameterDeclaration()
	nm := pd.Name()
	if nm == nil || nm.Kind != ast.KindObjectBindingPattern {
		return fail(params[0], "component parameters must be a destructured props object ({ ... }: { ... })")
	}
	ty := pd.Type
	if ty == nil || ty.Kind != ast.KindTypeLiteral {
		return fail(params[0], "props need an inline object type ({ ... }: { n: string })")
	}
	tyOf := map[string]*ast.Node{}
	for _, m := range ty.AsTypeLiteralNode().Members.Nodes {
		if m.Kind != ast.KindPropertySignature {
			return fail(m, "props type holds only plain fields")
		}
		fname, ok := bindingNameText(m)
		if !ok {
			return fail(m, "props type holds only plain fields")
		}
		tyOf[fname] = m.AsPropertySignatureDeclaration().Type
	}
	var sig []string
	seen := map[string]bool{}
	for _, el := range nm.AsNode().AsBindingPattern().Elements.Nodes {
		if el.Kind != ast.KindBindingElement {
			return fail(el, "props pattern holds only plain names")
		}
		pname, ok := bindingIdentText(el)
		if !ok {
			return fail(el, "props pattern holds only plain shorthand names")
		}
		if seen[pname] {
			return fail(el, "duplicate prop %s", pname)
		}
		seen[pname] = true
		ftn, ok := tyOf[pname]
		if !ok {
			return fail(el, "prop %s is missing from the props type", pname)
		}
		if ftn == nil || ftn.Kind != ast.KindStringKeyword {
			return fail(el, "prop %s must be a string in the direct slice (integers need int→string)", pname)
		}
		x.params[pname] = true
		sig = append(sig, pname+": ptr")
	}
	for fname := range tyOf {
		if !seen[fname] {
			return fail(params[0], "props type field %s is not destructured", fname)
		}
	}
	return strings.Join(sig, ", "), true
}

// lowerDirectUseState records `const [x, setX] = useState("lit")` string
// initials as render-time constants (no updates exist in the direct
// path: any setter use refuses). Other initializers refuse loudly.
func (x *tsxDirect) lowerDirectUseState(st *ast.Node) bool {
	if st.Kind != ast.KindVariableStatement {
		x.refuse(st, "component statements before return must be string useState declarations")
		return false
	}
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		x.refuse(st, "component statements before return must be string useState declarations")
		return false
	}
	d := dl.Declarations.Nodes[0]
	nm := d.Name()
	if nm == nil || nm.Kind != ast.KindArrayBindingPattern {
		x.refuse(st, "component state must be const [x, setX] = useState(\"lit\")")
		return false
	}
	els := nm.AsNode().AsBindingPattern().Elements.Nodes
	if len(els) != 2 {
		x.refuse(st, "component state must be const [x, setX] = useState(\"lit\")")
		return false
	}
	sv, ok1 := bindingIdentText(els[0])
	ss, ok2 := bindingIdentText(els[1])
	if !ok1 || !ok2 {
		x.refuse(st, "component state must be const [x, setX] = useState(\"lit\")")
		return false
	}
	init := d.Initializer()
	if init == nil || init.Kind != ast.KindCallExpression {
		x.refuse(st, "component state must be const [x, setX] = useState(\"lit\")")
		return false
	}
	ce := init.AsCallExpression()
	if ce.Expression.Kind != ast.KindIdentifier || ce.Expression.Text() != "useState" {
		x.refuse(st, "component state must be const [x, setX] = useState(\"lit\")")
		return false
	}
	if ce.Arguments == nil || len(ce.Arguments.Nodes) != 1 {
		x.refuse(st, "useState takes exactly 1 string literal argument")
		return false
	}
	arg := ce.Arguments.Nodes[0]
	// String initials fold to literals; integer/boolean literals render
	// through renderInterpValue (sext + @sa_fmt_i64_into, so true/false
	// stay "1"/"0"); floats need ftoa precision policy (later slice).
	if arg.Kind == ast.KindStringLiteral {
		s, ok := stringLiteralText(arg)
		if !ok {
			x.refuse(arg, "useState string initializer is not lowerable")
			return false
		}
		if _, dup := x.strConsts[sv]; dup {
			x.refuse(st, "duplicate state variable %s", sv)
			return false
		}
		if _, dup := x.intConsts[sv]; dup {
			x.refuse(st, "duplicate state variable %s", sv)
			return false
		}
		x.strConsts[sv] = s
		x.setters[ss] = true
		return true
	}
	if arg.Kind == ast.KindNumericLiteral && !isFloatLiteral(arg.Text()) ||
		arg.Kind == ast.KindTrueKeyword || arg.Kind == ast.KindFalseKeyword {
		t := "1"
		if arg.Kind == ast.KindNumericLiteral {
			t = arg.Text()
		} else if arg.Kind == ast.KindFalseKeyword {
			t = "0"
		}
		if _, dup := x.intConsts[sv]; dup {
			x.refuse(st, "duplicate state variable %s", sv)
			return false
		}
		if _, dup := x.strConsts[sv]; dup {
			x.refuse(st, "duplicate state variable %s", sv)
			return false
		}
		x.intConsts[sv] = t
		x.setters[ss] = true
		return true
	}
	x.refuse(arg, "only string/integer/boolean useState initializers render in the direct slice (floats need ftoa policy)")
	return false
}

// interpSlice resolves a whole-node {ident} interpolation to a string
// slice handle: string params pass through (ptr to {ptr,len}), string
// useState literals fold, integer/boolean useState literals render
// through renderInterpValue (sext + @sa_fmt_i64_into). Anything else
// refuses loudly (setters have no render shape; floats need ftoa).
func (x *tsxDirect) interpSlice(ident string, n *ast.Node) (string, bool) {
	e := x.e
	if x.params[ident] {
		return ident, true
	}
	if s, ok := x.strConsts[ident]; ok {
		return e.lowerStringLiteral(s), true
	}
	if t, ok := x.intConsts[ident]; ok {
		e.needImport("sa_std/fmt.sai")
		v, ok := e.renderInterpValue(t, tI32, n)
		if !ok {
			return "", false
		}
		return v, true
	}
	if x.setters[ident] {
		x.refuse(n, "setter %s has no render shape in the direct slice", ident)
		return "", false
	}
	x.refuse(n, "dynamic interpolation {%s} is not in the direct slice (string props and string/integer useState only)", ident)
	return "", false
}

// interpText builds a text handle for {ident}.
func (x *tsxDirect) interpText(ident string, n *ast.Node) (string, bool) {
	e := x.e
	h, ok := x.interpSlice(ident, n)
	if !ok {
		return "", false
	}
	tp, tl := e.expandSlice(h)
	t := e.freshTmp()
	e.emit("%s = call @sax_dom_create_text(%s, %s)", t, tp, tl)
	e.ownTemp(t)
	if e.domTemps == nil {
		e.domTemps = map[string]bool{}
	}
	e.domTemps[t] = true
	x.useExtern("sax_dom_create_text")
	return t, true
}

// lowerClickAttr records an onClick={(ctx) => ...} handler for bind_event
// emission (ctx=root convention). The arrow takes exactly one identifier
// param (bound to the component root at bind time); its body is a block
// with at least one statement, or a single call expression treated as
// one statement. Reports whether the attribute was consumed.
func (x *tsxDirect) lowerClickAttr(h string, a *ast.Node, at *ast.JsxAttribute) bool {
	if at.Initializer == nil || at.Initializer.Kind != ast.KindJsxExpression {
		x.refuse(a, "onClick needs an inline arrow in the handlers slice")
		return false
	}
	ex := at.Initializer.AsJsxExpression().Expression
	if ex == nil || ex.Kind != ast.KindArrowFunction {
		x.refuse(a, "onClick needs an inline arrow in the handlers slice")
		return false
	}
	params := ex.Parameters()
	if len(params) != 1 {
		x.refuse(a, "onClick arrow takes exactly its ctx param in the handlers slice")
		return false
	}
	ctxName, ok := bindingNameText(params[0].AsNode())
	if !ok {
		x.refuse(a, "onClick ctx param must be a plain identifier")
		return false
	}
	body := ex.AsArrowFunction().Body
	if body == nil {
		x.refuse(a, "onClick arrow has no body")
		return false
	}
	var stmts []*ast.Node
	if body.Kind != ast.KindBlock {
		// Expression bodies map onto one handler statement (calls and
		// ctx.textContent writes only; see lowerHandlerBody).
		if body.Kind != ast.KindCallExpression && body.Kind != ast.KindBinaryExpression {
			x.refuse(a, "onClick body must be a block of ctx DOM statements")
			return false
		}
		stmts = []*ast.Node{body}
	} else {
		stmts = body.Statements()
	}
	if len(stmts) == 0 {
		x.refuse(a, "empty onClick handler has no direct shape")
		return false
	}
	x.handlerSeq++
	x.handlers = append(x.handlers, directHandler{
		funcName: fmt.Sprintf("onClick_%d", x.handlerSeq),
		ctxName:  ctxName,
		arrow:    ex,
		pos:      a,
	})
	x.binds = append(x.binds, directBind{elem: h, handler: len(x.handlers) - 1})
	return true
}

// lowerHandlerBody lowers one recorded handler body: each statement must
// be a DOM write on the ctx param (setAttribute / textContent= /
// removeAttribute). setX and every other shape refuse loudly (no state
// slots exist in the direct path).
func (x *tsxDirect) lowerHandlerBody(hd directHandler) bool {
	body := hd.arrow.AsArrowFunction().Body
	var stmts []*ast.Node
	if body.Kind == ast.KindBlock {
		stmts = body.Statements()
	} else {
		stmts = []*ast.Node{body}
	}
	for _, st := range stmts {
		// Bare expression bodies (arrow expression form) lower as one
		// statement of the same shape.
		if st.Kind == ast.KindCallExpression {
			if !x.lowerHandlerCall(hd, st) {
				return false
			}
			continue
		}
		if st.Kind == ast.KindBinaryExpression {
			if !x.lowerHandlerAssign(hd, st) {
				return false
			}
			continue
		}
		if st.Kind != ast.KindExpressionStatement {
			x.refuse(st, "only ctx DOM statements lower in direct handlers")
			return false
		}
		es := st.AsExpressionStatement().Expression
		switch es.Kind {
		case ast.KindCallExpression:
			if !x.lowerHandlerCall(hd, es) {
				return false
			}
		case ast.KindBinaryExpression:
			if !x.lowerHandlerAssign(hd, es) {
				return false
			}
		default:
			x.refuse(st, "only ctx DOM statements lower in direct handlers")
			return false
		}
	}
	return true
}

// lowerHandlerAssign lowers `ctx.textContent = "lit"` writes through the
// shared text setter. Anything else refuses loudly.
func (x *tsxDirect) lowerHandlerAssign(hd directHandler, es *ast.Node) bool {
	e := x.e
	bin := es.AsBinaryExpression()
	if bin.OperatorToken.Kind != ast.KindEqualsToken {
		x.refuse(es, "only ctx DOM statements lower in direct handlers")
		return false
	}
	if bin.Left == nil || bin.Left.Kind != ast.KindPropertyAccessExpression {
		x.refuse(es, "only ctx DOM statements lower in direct handlers")
		return false
	}
	pa := bin.Left.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindIdentifier || pa.Expression.Text() != hd.ctxName {
		x.refuse(es, "only ctx DOM statements lower in direct handlers")
		return false
	}
	if pa.Name().Text() != "textContent" {
		x.refuse(es, "only ctx.textContent writes lower in direct handlers")
		return false
	}
	if bin.Right == nil || bin.Right.Kind != ast.KindStringLiteral {
		x.refuse(es, "ctx.textContent needs a string literal in the direct slice")
		return false
	}
	s, ok := stringLiteralText(bin.Right)
	if !ok {
		x.refuse(es, "ctx.textContent value is not lowerable")
		return false
	}
	vs := e.lowerStringLiteral(s)
	// Route through the shared text setter (validates + emits).
	if !e.lowerDomStore(hd.ctxName, "textContent", vs, tString, es) {
		return false
	}
	x.useExtern("sax_dom_set_text")
	return true
}

// lowerHandlerCall lowers one ctx method call statement
// (ctx.setAttribute / ctx.removeAttribute with string literals).
func (x *tsxDirect) lowerHandlerCall(hd directHandler, call *ast.Node) bool {
	e := x.e
	ce := call.AsCallExpression()
	if ce.Expression.Kind != ast.KindPropertyAccessExpression {
		x.refuse(call, "only ctx DOM statements lower in direct handlers")
		return false
	}
	pa := ce.Expression.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindIdentifier || pa.Expression.Text() != hd.ctxName {
		x.refuse(call, "only ctx DOM statements lower in direct handlers")
		return false
	}
	method := pa.Name().Text()
	args := []string{}
	if ce.Arguments != nil {
		for _, a := range ce.Arguments.Nodes {
			if a.Kind != ast.KindStringLiteral {
				x.refuse(a, "handler DOM calls take string literals in the direct slice")
				return false
			}
			s, ok := stringLiteralText(a)
			if !ok {
				x.refuse(a, "handler DOM argument is not lowerable")
				return false
			}
			args = append(args, e.lowerStringLiteral(s))
		}
	}
	types := make([]saType, len(args))
	for i := range args {
		types[i] = tString
	}
	switch method {
	case "setAttribute", "removeAttribute":
		if _, _, claimed := e.lowerDomMethod(hd.ctxName, method, args, types, call); !claimed {
			x.refuse(call, "handler DOM call %s rejected", method)
			return false
		}
		x.useExtern("sax_dom_set_attr")
		if method == "removeAttribute" {
			x.useExtern("sax_dom_remove_attr")
		}
		return true
	default:
		x.refuse(call, "handler DOM call %s is not in the direct slice (setAttribute/removeAttribute/textContent only)", method)
		return false
	}
}

// lowerNode lowers one JSX node to a DOM handle temp.
func (x *tsxDirect) lowerNode(n *ast.Node) (string, bool) {
	switch n.Kind {
	case ast.KindJsxElement, ast.KindJsxSelfClosingElement:
		return x.lowerElement(n)
	default:
		x.refuse(n, "dynamic JSX node %s is not in the direct slice (static elements only)", n.Kind.String())
		return "", false
	}
}

func (x *tsxDirect) lowerElement(n *ast.Node) (string, bool) {
	var open *ast.Node
	var children []*ast.Node
	switch n.Kind {
	case ast.KindJsxElement:
		el := n.AsJsxElement()
		open = el.OpeningElement
		if el.Children != nil {
			children = el.Children.Nodes
		}
	case ast.KindJsxSelfClosingElement:
		open = n
	default:
		x.refuse(n, "bad JSX element")
		return "", false
	}
	var tagNode *ast.Node
	var attrs *ast.Node
	if open.Kind == ast.KindJsxOpeningElement {
		tagNode = open.AsJsxOpeningElement().TagName
		attrs = open.AsJsxOpeningElement().Attributes
	} else {
		tagNode = open.AsJsxSelfClosingElement().TagName
		attrs = open.AsJsxSelfClosingElement().Attributes
	}
	if tagNode.Kind != ast.KindIdentifier {
		x.refuse(tagNode, "namespaced/qualified tags are not in the direct slice")
		return "", false
	}
	tag := tagNode.Text()
	if tag == "" || (tag[0] >= 'A' && tag[0] <= 'Z') {
		x.refuse(tagNode, "custom component <%s> needs the composition slice", tag)
		return "", false
	}
	if reactDangerousTags[tag] {
		x.refuse(tagNode, "tag <%s> is dangerous and not in the direct surface", tag)
		return "", false
	}
	e := x.e
	tagSym := e.lowerStringLiteral(tag)
	h, _ := e.lowerDocumentCreate("createElement", []string{tagSym}, []saType{tString}, n)
	if e.refused {
		return "", false
	}
	if e.domVars == nil {
		e.domVars = map[string]bool{}
	}
	e.domVars[h] = true
	x.useExtern("sax_dom_create")
	if attrs != nil {
		done := true
		attrs.ForEachChild(func(a *ast.Node) bool {
			if !done {
				return true
			}
			if a.Kind == ast.KindJsxSpreadAttribute {
				x.refuse(a, "spread attributes are not in the direct slice")
				done = false
				return true
			}
			if a.Kind != ast.KindJsxAttribute {
				return false
			}
			at := a.AsJsxAttribute()
			aname := at.Name().Text()
			// onClick lowers to a bind_event call (ctx=root convention);
			// all other onXxx need later slices.
			if aname == "onClick" {
				if x.lowerClickAttr(h, a, at) {
					return false
				}
				done = false
				return true
			}
			if len(aname) > 2 && aname[0] == 'o' && aname[1] == 'n' && 'A' <= aname[2] && aname[2] <= 'Z' {
				x.refuse(a, "event handler %s needs the handlers slice", aname)
				done = false
				return true
			}
			if aname == "className" {
				aname = "class"
			}
			if reactDangerousAttrs[aname] {
				x.refuse(a, "attribute %s is dangerous and not in the direct surface", aname)
				done = false
				return true
			}
			if at.Initializer == nil {
				x.refuse(a, "bare attribute %s needs a string value in the direct slice", aname)
				done = false
				return true
			}
			// Whole-value {ident} interpolates (params read at runtime,
			// useState literals fold, integers render); other shapes
			// refuse loudly.
			if at.Initializer.Kind == ast.KindJsxExpression {
				ex := at.Initializer.AsJsxExpression().Expression
				if ex == nil || ex.Kind != ast.KindIdentifier {
					x.refuse(a, "dynamic attribute %s is not in the direct slice (whole-value {ident} only)", aname)
					done = false
					return true
				}
				vh, ok := x.interpSlice(ex.Text(), a)
				if !ok {
					done = false
					return true
				}
				ks := e.lowerStringLiteral(aname)
				kp, kl := e.expandSlice(ks)
				vp, vl := e.expandSlice(vh)
				e.emit("call @sax_dom_set_attr(%s, %s, %s, %s, %s)", h, kp, kl, vp, vl)
				x.useExtern("sax_dom_set_attr")
				return false
			}
			if at.Initializer.Kind != ast.KindStringLiteral {
				x.refuse(a, "non-string attribute %s is not in the direct slice", aname)
				done = false
				return true
			}
			s, ok := stringLiteralText(at.Initializer)
			if !ok {
				x.refuse(a, "attribute %s value is not lowerable", aname)
				done = false
				return true
			}
			ks := e.lowerStringLiteral(aname)
			vs := e.lowerStringLiteral(s)
			if _, _, claimed := e.lowerDomMethod(h, "setAttribute", []string{ks, vs}, []saType{tString, tString}, a); !claimed {
				x.refuse(a, "setAttribute rejected for %s", aname)
				done = false
				return true
			}
			x.useExtern("sax_dom_set_attr")
			return false
		})
		if !done {
			return "", false
		}
	}
	for _, c := range children {
		var ch string
		switch c.Kind {
		case ast.KindJsxElement, ast.KindJsxSelfClosingElement:
			var ok bool
			ch, ok = x.lowerNode(c)
			if !ok {
				return "", false
			}
		case ast.KindJsxText:
			t := c.AsJsxText().Text
			if strings.TrimSpace(t) == "" {
				continue
			}
			for _, line := range strings.Split(t, "\n") {
				s := strings.TrimSpace(line)
				if s == "" {
					continue
				}
				ts := e.lowerStringLiteral(s)
				th, _ := e.lowerDocumentCreate("createTextNode", []string{ts}, []saType{tString}, c)
				if e.refused {
					return "", false
				}
				x.useExtern("sax_dom_create_text")
				if _, _, claimed := e.lowerDomMethod(h, "appendChild", []string{th}, nil, c); !claimed {
					x.refuse(c, "appendChild rejected text node")
					return "", false
				}
				x.useExtern("sax_dom_append_child")
			}
			continue
		case ast.KindJsxExpression:
			// Whole-node {ident} interpolation (params read at runtime,
			// useState literals fold); anything else refuses loudly.
			ex := c.AsJsxExpression().Expression
			if ex == nil || ex.Kind != ast.KindIdentifier {
				x.refuse(c, "dynamic JSX expression is not in the direct slice (whole-node {ident} only)")
				return "", false
			}
			th, ok := x.interpText(ex.Text(), c)
			if !ok {
				return "", false
			}
			if _, _, claimed := e.lowerDomMethod(h, "appendChild", []string{th}, nil, c); !claimed {
				x.refuse(c, "appendChild rejected interpolation node")
				return "", false
			}
			x.useExtern("sax_dom_append_child")
			continue
		default:
			x.refuse(c, "dynamic JSX child %s is not in the direct slice (static only)", c.Kind.String())
			return "", false
		}
		if _, _, claimed := e.lowerDomMethod(h, "appendChild", []string{ch}, nil, c); !claimed {
			x.refuse(c, "appendChild rejected child node")
			return "", false
		}
		x.useExtern("sax_dom_append_child")
	}
	return h, true
}
