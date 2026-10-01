package saemit

import (
	"strings"
	"testing"
)

func TestLowerTSXStatic(t *testing.T) {
	src := "function Greet() {\n  return <div className=\"g\">\n    <h1>Hello</h1>\n    <br />\n  </div>;\n}\n"
	res := LowerTSX("g.tsx", src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	for _, want := range []string{
		"<Component name=\"Greet\">",
		"<state>",
		"<div class=\"g\">",
		"<h1>",
		"Hello",
		"<br />",
		"</Component>",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
	}
	// Dangerous tags refuse (the consumer rejects them).
	script := "function C() {\n  return <div>\n    <script />\n  </div>;\n}\n"
	if r := LowerTSX("sc.tsx", script); !r.Refused {
		t.Fatalf("expected tag refusal, got:\n%s", r.SAX)
	}
	// Dangerous attributes refuse.
	attr := "function C() {\n  return <div dangerouslySetInnerHTML=\"x\">x</div>;\n}\n"
	if r := LowerTSX("at.tsx", attr); !r.Refused {
		t.Fatalf("expected attribute refusal, got:\n%s", r.SAX)
	}
}

func TestLowerTSXDynamicRefuses(t *testing.T) {
	hook := "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={() => setN(n + 1)}>x</button>;\n}\n"
	res := LowerTSX("c.tsx", hook)
	if !res.Refused {
		t.Fatalf("expected hooks refusal, got:\n%s", res.SAX)
	}
	handler := "function C() {\n  return <button onClick={() => 1}>x</button>;\n}\n"
	res = LowerTSX("c2.tsx", handler)
	if !res.Refused {
		t.Fatalf("expected handler refusal, got:\n%s", res.SAX)
	}
	custom := "function C() {\n  return <Badge>x</Badge>;\n}\n"
	res = LowerTSX("c3.tsx", custom)
	if !res.Refused {
		t.Fatalf("expected composition refusal, got:\n%s", res.SAX)
	}
}

// Bare same-file composition: <Badge /> references link by name to the
// sibling <Component> (consumer user-component nodes); props, children
// and unknown names refuse loudly (later slices).
func TestLowerTSXComposition(t *testing.T) {
	src := "function Badge() {\n  return <span>hi</span>;\n}\nfunction App() {\n  return <div>\n    <Badge />\n  </div>;\n}\n"
	res := LowerTSX("app.tsx", src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	for _, want := range []string{
		"<Component name=\"Badge\">",
		"<Component name=\"App\">",
		"<Badge />",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
	}
	if bad := checkSAXContract(res.SAX); len(bad) > 0 {
		t.Errorf("contract violations: %v\n%s", bad, res.SAX)
	}
	// Use-before-def resolves the same way (emission is textual).
	fwd := "function App() {\n  return <div>\n    <Badge />\n  </div>;\n}\nfunction Badge() {\n  return <span>hi</span>;\n}\n"
	if r := LowerTSX("fwd.tsx", fwd); r.Refused {
		msgs := []string{}
		for _, d := range r.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("forward reference must lower:\n%s", strings.Join(msgs, "\n"))
	}
	cases := []struct {
		name string
		src  string
	}{
		{"props", "function Badge() {\n  return <span>hi</span>;\n}\nfunction App() {\n  return <div>\n    <Badge text=\"x\" />\n  </div>;\n}\n"},
		{"children", "function Badge() {\n  return <span>hi</span>;\n}\nfunction App() {\n  return <div>\n    <Badge>x</Badge>\n  </div>;\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := LowerTSX(tc.name+".tsx", tc.src); !r.Refused {
				t.Fatalf("expected refusal, got:\n%s", r.SAX)
			}
		})
	}
}
// Click handlers lower setX(literal) arrows to @onClick_n SA blocks
// (consumer: onClick={^name}, normalized to onclick). Everything else
// (named refs, params, non-setter calls, computed args, other events)
// refuses loudly.
func TestLowerTSXClickHandler(t *testing.T) {
	src := "function Counter() {\n  const [count, setCount] = useState(0);\n  return <section>\n    <h1>{count}</h1>\n    <button onClick={() => setCount(5)}>+1</button>\n  </section>;\n}\n"
	res := LowerTSX("h.tsx", src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	for _, want := range []string{
		"onClick={^onClick_1}",
		"@onClick_1:",
		"L_ENTRY:",
		"store state+Counter_count, 5 as i64",
		"call @render()",
		"ret",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
	}
	if bad := checkSAXContract(res.SAX); len(bad) > 0 {
		t.Errorf("contract violations: %v\n%s", bad, res.SAX)
	}
	cases := []struct {
		name string
		src  string
	}{
		{"named ref", "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={setN}>x</button>;\n}\n"},
		{"params", "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={(e) => setN(1)}>x</button>;\n}\n"},
		{"non-setter", "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={() => console.log(1)}>x</button>;\n}\n"},
		{"computed", "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={() => setN(n + 1)}>x</button>;\n}\n"},
		{"other event", "function C() {\n  const [n, setN] = useState(0);\n  return <input onChange={() => setN(1)}>x</input>;\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := LowerTSX(tc.name+".tsx", tc.src); !r.Refused {
				t.Fatalf("expected refusal, got:\n%s", r.SAX)
			}
		})
	}
}
