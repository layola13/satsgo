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
		"<div className=\"g\">",
		"<h1>",
		"Hello",
		"<br />",
		"</Component>",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
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
