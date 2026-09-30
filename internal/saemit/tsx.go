// TSX → SAX: React-style function components to sa_plugin_react sources.
//
// Slice 1 (static templates): a component whose returned JSX contains only
// static tags, text and string attributes lowers to a .sax Component with an
// empty state block. Anything dynamic (hooks, handlers, expressions,
// custom components, spreads) refuses loudly with a located diagnostic.
//
// Reference target: sa_plugin_react demos/*.sax (Component/state/template
// + @handler SA blocks); dynamic slices arrive later per todo/04_tsx.md.
package saemit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tspath"
)

// TSXResult is one .sax lowering outcome.
type TSXResult struct {
	SAX         string
	Refused     bool
	Diagnostics []Diagnostic
}

// LowerTSX lowers static tsx components to .sax text.
func LowerTSX(fileName, sourceText string) TSXResult {
	abs := fileName
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	opts := ast.SourceFileParseOptions{FileName: abs, Path: tspath.ToPath(abs, "/", true)}
	sf := parser.ParseSourceFile(opts, sourceText, core.ScriptKindTSX)
	x := &tsxEmitter{file: fileName, src: sourceText, lines: lineOffsets(sourceText)}
	x.lowerSourceFile(sf)
	return TSXResult{SAX: x.out.String(), Refused: x.refused, Diagnostics: x.diags}
}

type tsxEmitter struct {
	file    string
	src     string
	lines   []int
	out     strings.Builder
	diags   []Diagnostic
	refused bool
	// stateVars maps state name -> SAX literal; stateSetters marks
	// setter names (any use refuses: setters have no render shape).
	stateVars    map[string]string
	stateSetters map[string]bool
	// comp is the current component name (state+Comp_field stores);
	// setterOf inverts setter -> state var; mountStores accumulates
	// @onMount store lines; mountEmpty records an empty mount effect.
	comp        string
	setterOf    map[string]string
	mountStores []string
	mountEmpty  bool
}

func (x *tsxEmitter) refuse(n *ast.Node, format string, args ...any) {
	line, col := 1, 1
	if n != nil {
		pos := n.Pos()
		line = 1
		lineStart := 0
		for i, off := range x.lines {
			if off > pos {
				break
			}
			line = i + 1
			lineStart = off
		}
		col = pos - lineStart + 1
	}
	x.diags = append(x.diags, Diagnostic{File: x.file, Line: line, Col: col, Msg: fmt.Sprintf(format, args...)})
	x.refused = true
}

func (x *tsxEmitter) lowerSourceFile(sf *ast.SourceFile) {
	for _, st := range sf.AsSourceFile().Statements.Nodes {
		if st.Kind != ast.KindFunctionDeclaration {
			continue
		}
		if st.Name() == nil || st.Name().Kind != ast.KindIdentifier {
			continue
		}
		x.lowerComponent(st.Name().Text(), st)
		if x.refused {
			return
		}
	}
}

func (x *tsxEmitter) lowerComponent(name string, fn *ast.Node) {
	body := fn.BodyData().Body
	if body == nil {
		x.refuse(fn, "component %s has no body", name)
		return
	}
	// Leading statements must be useState declarations or mount
	// useEffect calls; the tail is a single return of JSX. Anything
	// else (other hooks, handlers as statements, arbitrary code)
	// refuses loudly.
	stmts := body.Statements()
	if len(stmts) == 0 {
		x.refuse(fn, "component %s must be a single return of JSX", name)
		return
	}
	x.comp = name
	x.stateVars = map[string]string{}
	x.stateSetters = map[string]bool{}
	x.setterOf = map[string]string{}
	x.mountStores = nil
	x.mountEmpty = false
	for _, st := range stmts[:len(stmts)-1] {
		if st.Kind == ast.KindVariableStatement {
			if !x.lowerUseState(st) {
				return
			}
			continue
		}
		if !x.lowerUseEffect(st) {
			return
		}
	}
	ret := stmts[len(stmts)-1]
	if ret.Kind != ast.KindReturnStatement {
		x.refuse(fn, "component %s must end with a single return of JSX", name)
		return
	}
	rs := ret.AsReturnStatement()
	if rs.Expression == nil {
		x.refuse(ret, "component %s returns nothing", name)
		return
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<Component name=\"%s\">\n", name))
	b.WriteString("  <state>\n")
	names := make([]string, 0, len(x.stateVars))
	for n := range x.stateVars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString("    " + n + " = " + x.stateVars[n] + "\n")
	}
	b.WriteString("  </state>\n\n")
	if !x.lowerJSXNode(rs.Expression, &b, "  ") {
		return
	}
	b.WriteString("\n")
	if len(x.mountStores) > 0 || x.mountEmpty {
		// @onMount runs once at mount (counter-demo handler shape);
		// an empty effect still emits the block with a bare ret.
		b.WriteString("  @onMount:\n  L_ENTRY:\n")
		for _, s := range x.mountStores {
			b.WriteString("    " + s + "\n")
		}
		if len(x.mountStores) > 0 {
			b.WriteString("    call @render()\n")
		}
		b.WriteString("    ret\n")
	}
	b.WriteString("</Component>\n")
	x.out.WriteString(b.String())
}

// lowerUseEffect lowers mount-only `useEffect(fn, [])`: fn takes no
// params, declares no cleanup (no return value), and its body holds only
// setter calls with integer/boolean literal arguments (stored straight
// into state slots). console.* bodies refuse deliberately: .sax handlers
// have no print imports (no @const/@import in any demo), so emitting
// print_bytes would assemble the wrong dialect. Non-empty deps, missing
// deps, cleanup returns and every other hook refuse loudly.
func (x *tsxEmitter) lowerUseEffect(st *ast.Node) bool {
	isHook := false
	if st.Kind == ast.KindExpressionStatement {
		if es := st.AsExpressionStatement(); es.Expression != nil && es.Expression.Kind == ast.KindCallExpression {
			if ce := es.Expression.AsCallExpression(); ce.Expression.Kind == ast.KindIdentifier {
				callee := ce.Expression.Text()
				isHook = callee == "useEffect" || (strings.HasPrefix(callee, "use") && len(callee) > 3 && callee[3] >= 'A' && callee[3] <= 'Z')
			}
		}
	}
	if !isHook {
		x.refuse(st, "component statements before return must be useState/useEffect declarations")
		return false
	}
	ce := st.AsExpressionStatement().Expression.AsCallExpression()
	if ce.Expression.Text() != "useEffect" {
		x.refuse(st, "hook %s is not in the subset (only mount useEffect)", ce.Expression.Text())
		return false
	}
	if ce.Arguments == nil || len(ce.Arguments.Nodes) != 2 {
		x.refuse(st, "useEffect needs (fn, []) exactly (missing deps run every render)")
		return false
	}
	deps := ce.Arguments.Nodes[1]
	if deps.Kind != ast.KindArrayLiteralExpression || len(deps.AsArrayLiteralExpression().Elements.Nodes) != 0 {
		x.refuse(st, "only mount useEffect with [] deps is in the subset")
		return false
	}
	fn := ce.Arguments.Nodes[0]
	if fn.Kind != ast.KindArrowFunction && fn.Kind != ast.KindFunctionExpression {
		x.refuse(st, "useEffect callback must be an inline function")
		return false
	}
	if len(fn.Parameters()) != 0 {
		x.refuse(st, "useEffect callback takes no parameters")
		return false
	}
	fbody := fn.BodyData().Body
	if fbody == nil || fbody.Kind != ast.KindBlock {
		x.refuse(st, "useEffect callback must have a block body")
		return false
	}
	for _, s := range fbody.Statements() {
		if s.Kind == ast.KindReturnStatement {
			x.refuse(s, "useEffect cleanup returns are not in the subset")
			return false
		}
		if s.Kind != ast.KindExpressionStatement {
			x.refuse(s, "mount effect bodies hold only setter(literal) calls")
			return false
		}
		es := s.AsExpressionStatement()
		if es.Expression == nil || es.Expression.Kind != ast.KindCallExpression {
			x.refuse(s, "mount effect bodies hold only setter(literal) calls")
			return false
		}
		sc := es.Expression.AsCallExpression()
		if sc.Expression.Kind != ast.KindIdentifier || !x.stateSetters[sc.Expression.Text()] {
			x.refuse(s, "mount effect bodies hold only setter(literal) calls")
			return false
		}
		if sc.Arguments == nil || len(sc.Arguments.Nodes) != 1 {
			x.refuse(s, "setter calls take exactly 1 argument")
			return false
		}
		arg := sc.Arguments.Nodes[0]
		var val, ty string
		switch arg.Kind {
		case ast.KindNumericLiteral:
			if strings.ContainsAny(arg.Text(), ".eE") {
				x.refuse(arg, "setter float arguments are not in the subset")
				return false
			}
			val, ty = arg.Text(), "i64"
		case ast.KindTrueKeyword:
			val, ty = "1", "i1"
		case ast.KindFalseKeyword:
			val, ty = "0", "i1"
		default:
			x.refuse(arg, "setter arguments must be integer/boolean literals")
			return false
		}
		// Invert the setter: the matching state var is the one whose
		// setter name was recorded alongside it.
		field := x.setterOf[sc.Expression.Text()]
		if field == "" {
			x.refuse(s, "setter %s has no state slot", sc.Expression.Text())
			return false
		}
		x.mountStores = append(x.mountStores, fmt.Sprintf("store state+%s_%s, %s as %s", x.comp, field, val, ty))
	}
	x.mountEmpty = true
	return true
}

// lowerUseState lowers `const [x, setX] = useState(lit)` into a state slot.
// Only numeric/boolean literals lower (buffers for strings belong to a
// later slice); the setter name is recorded for refusal on use.
func (x *tsxEmitter) lowerUseState(st *ast.Node) bool {
	if st.Kind != ast.KindVariableStatement {
		x.refuse(st, "component statements before return must be useState declarations")
		return false
	}
	dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
	if len(dl.Declarations.Nodes) != 1 {
		x.refuse(st, "component statements before return must be useState declarations")
		return false
	}
	d := dl.Declarations.Nodes[0]
	nm := d.Name()
	if nm == nil || nm.Kind != ast.KindArrayBindingPattern {
		x.refuse(st, "component state must be const [x, setX] = useState(lit)")
		return false
	}
	els := nm.AsNode().AsBindingPattern().Elements.Nodes
	if len(els) != 2 {
		x.refuse(st, "component state must be const [x, setX] = useState(lit)")
		return false
	}
	sv, ok1 := bindingIdentText(els[0])
	ss, ok2 := bindingIdentText(els[1])
	if !ok1 || !ok2 {
		x.refuse(st, "component state must be const [x, setX] = useState(lit)")
		return false
	}
	init := d.Initializer()
	if init == nil || init.Kind != ast.KindCallExpression {
		x.refuse(st, "component state must be const [x, setX] = useState(lit)")
		return false
	}
	ce := init.AsCallExpression()
	if ce.Expression.Kind != ast.KindIdentifier || ce.Expression.Text() != "useState" {
		x.refuse(st, "component state must be const [x, setX] = useState(lit)")
		return false
	}
	if ce.Arguments == nil || len(ce.Arguments.Nodes) != 1 {
		x.refuse(st, "useState takes exactly 1 literal argument")
		return false
	}
	lit := ce.Arguments.Nodes[0]
	var text string
	switch lit.Kind {
	case ast.KindNumericLiteral:
		// Integer slots lower as i64 (counter-demo shape); floats have
		// no slot type yet and refuse loudly rather than mistruncate.
		if strings.ContainsAny(lit.Text(), ".eE") {
			x.refuse(lit, "useState float initializers are not in the subset (integer/boolean only)")
			return false
		}
		text = lit.Text()
	case ast.KindTrueKeyword:
		text = "1 as i1"
	case ast.KindFalseKeyword:
		text = "0 as i1"
	default:
		x.refuse(lit, "useState initializers must be integer/boolean literals (strings need the buffer slice)")
		return false
	}
	if _, dup := x.stateVars[sv]; dup {
		x.refuse(st, "duplicate state variable %s", sv)
		return false
	}
	x.stateVars[sv] = text
	x.stateSetters[ss] = true
	x.setterOf[ss] = sv
	return true
}

// bindingIdentText reads a plain identifier out of a binding element.
func bindingIdentText(el *ast.Node) (string, bool) {
	be := el.AsBindingElement()
	if be.PropertyName != nil {
		return "", false
	}
	nm := be.Name()
	if nm == nil || nm.Kind != ast.KindIdentifier {
		return "", false
	}
	return nm.Text(), true
}

// lowerJSXNode renders static JSX into the template builder.
func (x *tsxEmitter) lowerJSXNode(n *ast.Node, b *strings.Builder, indent string) bool {
	switch n.Kind {
	case ast.KindJsxElement:
		el := n.AsJsxElement()
		tag, ok := x.jsxTagName(el.OpeningElement)
		if !ok {
			return false
		}
		b.WriteString(indent + "<" + tag)
		if !x.lowerJSXAttrs(el.OpeningElement, b) {
			return false
		}
		b.WriteString(">\n")
		for _, ch := range el.Children.Nodes {
			if !x.lowerJSXChild(ch, b, indent+"  ") {
				return false
			}
		}
		b.WriteString(indent + "</" + tag + ">\n")
		return true
	case ast.KindJsxSelfClosingElement:
		tag, ok := x.jsxTagName(n)
		if !ok {
			return false
		}
		b.WriteString(indent + "<" + tag)
		if !x.lowerJSXAttrs(n, b) {
			return false
		}
		b.WriteString(" />\n")
		return true
	case ast.KindJsxFragment:
		fr := n.AsJsxFragment()
		for _, ch := range fr.Children.Nodes {
			if !x.lowerJSXChild(ch, b, indent) {
				return false
			}
		}
		return true
	default:
		x.refuse(n, "component root must be JSX (got %s)", n.Kind.String())
		return false
	}
}

func (x *tsxEmitter) lowerJSXChild(n *ast.Node, b *strings.Builder, indent string) bool {
	switch n.Kind {
	case ast.KindJsxElement, ast.KindJsxSelfClosingElement, ast.KindJsxFragment:
		return x.lowerJSXNode(n, b, indent)
	case ast.KindJsxText:
		t := n.AsJsxText().Text
		if strings.TrimSpace(t) == "" {
			return true
		}
		for _, line := range strings.Split(t, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				b.WriteString(indent + s + "\n")
			}
		}
		return true
	case ast.KindJsxExpression:
		// `{stateVar}` interpolates a state slot (counter shape);
		// setters and computed expressions refuse loudly.
		ex := n.AsJsxExpression().Expression
		if ex != nil && ex.Kind == ast.KindIdentifier {
			if _, ok := x.stateVars[ex.Text()]; ok {
				b.WriteString(indent + "{" + ex.Text() + "}\n")
				return true
			}
			if x.stateSetters[ex.Text()] {
				x.refuse(n, "setter %s has no render shape", ex.Text())
				return false
			}
		}
		x.refuse(n, "dynamic JSX expression is not in the state slice (only {stateVar})")
		return false
	default:
		x.refuse(n, "dynamic JSX child %s is not in the static slice (see todo/04_tsx.md)", n.Kind.String())
		return false
	}
}

func (x *tsxEmitter) lowerJSXAttrs(open *ast.Node, b *strings.Builder) bool {
	var attrs *ast.Node
	switch open.Kind {
	case ast.KindJsxOpeningElement:
		attrs = open.AsJsxOpeningElement().Attributes
	case ast.KindJsxSelfClosingElement:
		attrs = open.AsJsxSelfClosingElement().Attributes
	default:
		return true
	}
	if attrs == nil {
		return true
	}
	ok := true
	attrs.ForEachChild(func(a *ast.Node) bool {
		if !ok {
			return true
		}
		if a.Kind == ast.KindJsxSpreadAttribute {
			x.refuse(a, "spread attributes are not in the static slice")
			ok = false
			return true
		}
		if a.Kind != ast.KindJsxAttribute {
			return false
		}
		at := a.AsJsxAttribute()
		aname := at.Name().Text()
		if len(aname) > 2 && aname[0] == 'o' && aname[1] == 'n' && 'A' <= aname[2] && aname[2] <= 'Z' {
			x.refuse(a, "event handler %s needs the handlers slice", aname)
			ok = false
			return true
		}
		if at.Initializer == nil {
			b.WriteString(" " + aname)
			return false
		}
		init := at.Initializer
		if init.Kind == ast.KindStringLiteral {
			if s, ok2 := stringLiteralText(init); ok2 {
				b.WriteString(" " + aname + "=\"" + s + "\"")
				return false
			}
		}
		x.refuse(a, "non-string attribute %s is not in the static slice", aname)
		ok = false
		return true
	})
	return ok
}

// jsxTagName resolves lowercase intrinsic tags; uppercase (custom)
// components refuse (composition is a later slice).
func (x *tsxEmitter) jsxTagName(open *ast.Node) (string, bool) {
	var tag *ast.Node
	switch open.Kind {
	case ast.KindJsxOpeningElement:
		tag = open.AsJsxOpeningElement().TagName
	case ast.KindJsxSelfClosingElement:
		tag = open.AsJsxSelfClosingElement().TagName
	default:
		x.refuse(open, "bad JSX opening element")
		return "", false
	}
	if tag.Kind != ast.KindIdentifier {
		x.refuse(tag, "namespaced/qualified tags are not in the static slice")
		return "", false
	}
	name := tag.Text()
	if name == "" {
		x.refuse(tag, "empty tag name")
		return "", false
	}
	if 'A' <= name[0] && name[0] <= 'Z' {
		x.refuse(tag, "custom component <%s> needs the composition slice", name)
		return "", false
	}
	return name, true
}
