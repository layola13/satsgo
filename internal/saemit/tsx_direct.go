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
}

// airlockExtern declares the used airlock imports with the exact runtime
// arities (sa_plugin_react airlock_gen.zig): handles are i64, (ptr,len)
// string pairs are (ptr, u64); setters return nothing.
var airlockExtern = map[string]string{
	"sax_dom_create":       "@extern sax_dom_create(tag_ptr: ptr, tag_len: u64) -> i64",
	"sax_dom_create_text":  "@extern sax_dom_create_text(text_ptr: ptr, text_len: u64) -> i64",
	"sax_dom_append_child": "@extern sax_dom_append_child(parent_h: i64, child_h: i64)",
	"sax_dom_set_attr":     "@extern sax_dom_set_attr(node_h: i64, key_ptr: ptr, key_len: u64, val_ptr: ptr, val_len: u64)",
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
func (x *tsxDirect) lowerComponent(name string, fn *ast.Node) bool {
	if len(fn.Parameters()) > 0 {
		x.refuse(fn, "component %s takes params (static slice: no props yet)", name)
		return false
	}
	body := fn.BodyData().Body
	if body == nil {
		x.refuse(fn, "component %s has no body", name)
		return false
	}
	stmts := body.Statements()
	if len(stmts) != 1 || stmts[0].Kind != ast.KindReturnStatement {
		x.refuse(fn, "component %s must be a single return of JSX", name)
		return false
	}
	rs := stmts[0].AsReturnStatement()
	if rs.Expression == nil {
		x.refuse(stmts[0], "component %s returns nothing", name)
		return false
	}
	x.e.emitRaw("@render_%s() -> i64:", name)
	x.e.emitRaw("L_ENTRY:")
	x.e.pushScope()
	x.e.terminated = false
	root, ok := x.lowerNode(rs.Expression)
	if !ok {
		return false
	}
	x.e.releaseAllOwnedExcept(root)
	x.e.emit("return %s", root)
	x.e.terminated = true
	return true
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
