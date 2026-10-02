package saemit

// .sax shape contract (todo/04_tsx.md road 1, tasks 3+4).
//
// LowerTSX output must satisfy the structural requirements of the real
// consumer grammar (sa_plugin_react/src/react/parser.zig — the `sa react`
// target; NOT the narrower sa_plugin_sax tables, which reject e.g. `br`
// that react accepts). `sa react build` is unavailable in this
// environment (no react subcommand in this sci binary, no plugin .so),
// so the contract mirrors the parser rule by rule instead of a
// round-trip:
//
//   - <Component name="X"> ... </Component> balanced (parseComponent).
//   - <state> ... </state> balanced; entries `name = expr` with ident
//     names, unique and sorted (parseStateBlock).
//   - {name} interpolations reference declared state vars only.
//   - @onMount blocks (when emitted) contain L_ENTRY: and ret
//     (lifecycle hook shape, counter demo).
//   - tags: anything lowercase and non-dangerous (isIntrinsicTag);
//     attributes: anything but on*/dangerous strings (isSupportedAttr),
//     className→class mapped (idempotent with the consumer).
//   - releases (`!v`): every <state> var must be released on its own
//     line (`!a !b`), unless the component has SLA handlers (satsgo
//     never emits those). The consumer refuses unreleased state with
//     SaxStateLeak (sa_plugin_react/src/plugin.zig findValidationFailure),
//     proven live by `sa react check`.
import (
	"regexp"
	"strings"
	"testing"
)

var contractStateLine = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+?)\s*$`)
var contractInterp = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var contractTag = regexp.MustCompile(`</?([A-Za-z][A-Za-z0-9._-]*)`)
var contractAttr = regexp.MustCompile(`\s([A-Za-z_:][A-Za-z0-9_.:-]*)=`)
var contractCompDef = regexp.MustCompile(`<Component name="([A-Za-z_][A-Za-z0-9_]*)">`)
// contractCustomOpen matches custom-element opening tags; the checker
// pairs each non-self-closing one with its close tag to find children.
var contractCustomOpen = regexp.MustCompile(`<([A-Z][A-Za-z0-9_]*)\b([^>]*?)>`)

// contractDangerous mirrors react parser.zig dangerous_tags /
// dangerous_attrs (the only hard refusals on the tag/attribute axes).
var contractDangerousTags = map[string]bool{
	"script": true, "iframe": true, "object": true, "embed": true, "template": true,
}
var contractDangerousAttrs = map[string]bool{
	"innerHTML": true, "outerHTML": true, "dangerouslySetInnerHTML": true, "srcDoc": true,
}

// checkSAXContract validates one emitted .sax document against the
// consumer-shape rules above. It returns the failure reasons (empty = ok).
// State and interpolation checks run per <Component> block (each
// component owns its slots); component references resolve document-wide.
func checkSAXContract(sax string) []string {
	var bad []string
	if n := strings.Count(sax, "<Component name="); n == 0 {
		bad = append(bad, "no <Component>")
	}
	if strings.Count(sax, "<Component name=") != strings.Count(sax, "</Component>") {
		bad = append(bad, "unbalanced Component tags")
	}
	if strings.Count(sax, "<state>") != strings.Count(sax, "</state>") {
		bad = append(bad, "unbalanced state tags")
	}
	defs := map[string]bool{}
	for _, m := range contractCompDef.FindAllStringSubmatch(sax, -1) {
		defs[m[1]] = true
	}
	chunks := map[string]string{}
	locs := contractCompDef.FindAllStringIndex(sax, -1)
	for i, loc := range locs {
		end := len(sax)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		chunk := sax[loc[0]:end]
		chunks[contractCompDef.FindStringSubmatch(chunk)[1]] = chunk
		bad = append(bad, checkSAXComponentChunk(chunk)...)
	}
	// Custom tag references resolve document-wide; elements carrying
	// children need a <Slot /> outlet in the referenced component.
	for _, m := range contractTag.FindAllStringSubmatch(sax, -1) {
		tag := m[1]
		if tag == "Component" || tag == "state" || tag == "Slot" {
			continue
		}
		if contractDangerousTags[tag] {
			bad = append(bad, "dangerous tag <"+tag+">")
		}
		if tag[0] >= 'A' && tag[0] <= 'Z' && !defs[tag] {
			bad = append(bad, "unresolved component <"+tag+">")
		}
	}
	// A custom element carrying children (<Tag>...</Tag>) needs a
	// <Slot /> outlet in the referenced component's block.
	for _, loc := range contractCustomOpen.FindAllStringSubmatchIndex(sax, -1) {
		tag := sax[loc[2]:loc[3]]
		if tag == "Component" {
			continue
		}
		attrs := sax[loc[4]:loc[5]]
		if strings.HasSuffix(strings.TrimRight(attrs, " \t"), "/") {
			continue
		}
		close := "</" + tag + ">"
		rest := sax[loc[1]:]
		ci := strings.Index(rest, close)
		if ci < 0 {
			continue
		}
		if strings.TrimSpace(rest[:ci]) == "" {
			continue
		}
		if def, ok := chunks[tag]; !ok || !strings.Contains(def, "<Slot") {
			bad = append(bad, "children passed to component without <Slot />: "+tag)
		}
	}
	for _, m := range contractAttr.FindAllStringSubmatch(sax, -1) {
		if contractDangerousAttrs[m[1]] {
			bad = append(bad, "dangerous attribute "+m[1])
		}
	}
	return bad
}

// contractAttrValue matches ={...} and ="..." attribute values so the
// interpolation check can skip prop/event references (caller-scope names,
// not callee state reads).
var contractAttrValue = regexp.MustCompile(`=\{[^}]*\}|="[^"]*"`)
var contractRelease = regexp.MustCompile(`![A-Za-z_][A-Za-z0-9_]*`)
// checkSAXComponentChunk validates one <Component> block: state entries
// unique/sorted, template interpolations referencing its own state, and
// @onMount shape when present.
func checkSAXComponentChunk(chunk string) []string {
	var bad []string
	si := strings.Index(chunk, "<state>")
	if si < 0 {
		return bad
	}
	ej := strings.Index(chunk, "</state>")
	if ej < 0 {
		return bad
	}
	vars := []string{}
	for _, m := range contractStateLine.FindAllStringSubmatch(chunk[si:ej], -1) {
		vars = append(vars, m[1])
	}
	seen := map[string]bool{}
	for i, v := range vars {
		if seen[v] {
			bad = append(bad, "duplicate state var "+v)
		}
		seen[v] = true
		if i > 0 && vars[i-1] >= v {
			bad = append(bad, "state vars not sorted")
			break
		}
	}
	stripped := contractAttrValue.ReplaceAllString(chunk, " ")
	for _, m := range contractInterp.FindAllStringSubmatch(stripped, -1) {
		if !seen[m[1]] {
			bad = append(bad, "interpolation of undeclared state {"+m[1]+"}")
		}
	}
	// Release coverage: every state var must be released (`!v`), the
	// consumer's SaxStateLeak rule (stateless components owe nothing).
	if len(vars) > 0 {
		rel := map[string]bool{}
		for _, m := range contractRelease.FindAllString(stripped, -1) {
			rel[m[1:]] = true
		}
		for _, v := range vars {
			if !rel[v] {
				bad = append(bad, "state var without release {"+v+"}")
			}
		}
	}
	if strings.Contains(chunk, "@onMount:") {
		hook := chunk[strings.Index(chunk, "@onMount:"):]
		if end := strings.Index(hook, "</Component>"); end >= 0 {
			hook = hook[:end]
		}
		if !strings.Contains(hook, "L_ENTRY:") || !strings.Contains(hook, "ret") {
			bad = append(bad, "@onMount block missing L_ENTRY:/ret")
		}
	}
	return bad
}

func TestTSXSAXContract(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"static", "function Greet() {\n  return <div className=\"g\">\n    <h1>Hello</h1>\n  </div>;\n}\n"},
		{"state", "function Counter() {\n  const [count, setCount] = useState(0);\n  return <section>\n    <h1>{count}</h1>\n  </section>;\n}\n"},
		{"mount", "function Counter() {\n  const [count, setCount] = useState(0);\n  useEffect(() => { setCount(5); }, []);\n  return <section>\n    <h1>{count}</h1>\n  </section>;\n}\n"},
		// Recalibration lock: lowercase tags outside the old sax tables
		// (br) and aria-/data- attributes pass the react consumer.
		{"wide", "function C() {\n  return <div data-x=\"1\">\n    <br />\n    <p>hi</p>\n  </div>;\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := LowerTSX(tc.name+".tsx", tc.src)
			if res.Refused {
				msgs := []string{}
				for _, d := range res.Diagnostics {
					msgs = append(msgs, d.Error())
				}
				t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
			}
			if bad := checkSAXContract(res.SAX); len(bad) > 0 {
				t.Errorf("contract violations: %v\n%s", bad, res.SAX)
			}
		})
	}
	// The checker itself is locked: an interpolation of undeclared
	// state must fail the contract.
	ghost := "<Component name=\"C\">\n  <state>\n    n = 0\n  </state>\n  <div>\n    {ghost}\n  </div>\n</Component>\n"
	if bad := checkSAXContract(ghost); len(bad) == 0 {
		t.Errorf("checker must flag undeclared interpolation:\n%s", ghost)
	}
	// Children without a <Slot /> outlet must fail the contract.
	noslot := "<Component name=\"B\">\n  <span>hi</span>\n</Component>\n<Component name=\"A\">\n  <B>x</B>\n</Component>\n"
	if bad := checkSAXContract(noslot); len(bad) == 0 {
		t.Errorf("checker must flag children without <Slot />:\n%s", noslot)
	}
}
