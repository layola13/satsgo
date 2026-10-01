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
//   - releases (`!`) are absent: integer/boolean state holds no buffers,
//     so no cleanup lines are owed (string state refuses elsewhere).
import (
	"regexp"
	"strings"
	"testing"
)

var contractStateLine = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+?)\s*$`)
var contractInterp = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var contractTag = regexp.MustCompile(`</?([A-Za-z][A-Za-z0-9._-]*)`)
var contractAttr = regexp.MustCompile(`\s([A-Za-z_:][A-Za-z0-9_.:-]*)=`)

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
	// State block: unique, sorted, ident-shaped names.
	if si := strings.Index(sax, "<state>"); si >= 0 {
		ej := strings.Index(sax, "</state>")
		vars := []string{}
		for _, m := range contractStateLine.FindAllStringSubmatch(sax[si:ej], -1) {
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
		// Interpolations must reference declared state.
		for _, m := range contractInterp.FindAllStringSubmatch(sax, -1) {
			if !seen[m[1]] {
				bad = append(bad, "interpolation of undeclared state {"+m[1]+"}")
			}
		}
	}
	// onMount shape.
	if strings.Contains(sax, "@onMount:") {
		hook := sax[strings.Index(sax, "@onMount:"):]
		if end := strings.Index(hook, "</Component>"); end >= 0 {
			hook = hook[:end]
		}
		if !strings.Contains(hook, "L_ENTRY:") || !strings.Contains(hook, "ret") {
			bad = append(bad, "@onMount block missing L_ENTRY:/ret")
		}
	}
	// Tag/attribute gates mirror the consumer: only dangerous tags
	// and dangerous attributes are hard refusals (lowercase tags and
	// aria-/data- attributes pass; events arrive as onClick={^name}).
	for _, m := range contractTag.FindAllStringSubmatch(sax, -1) {
		tag := m[1]
		if tag == "Component" || tag == "state" {
			continue
		}
		if contractDangerousTags[tag] {
			bad = append(bad, "dangerous tag <"+tag+">")
		}
		if tag[0] >= 'A' && tag[0] <= 'Z' {
			bad = append(bad, "custom component <"+tag+"> needs composition")
		}
	}
	for _, m := range contractAttr.FindAllStringSubmatch(sax, -1) {
		if contractDangerousAttrs[m[1]] {
			bad = append(bad, "dangerous attribute "+m[1])
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
}
