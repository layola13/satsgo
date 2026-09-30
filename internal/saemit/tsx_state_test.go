package saemit

// Tests for the useState slice (todo/04_tsx.md road 2, task 5).
import (
	"strings"
	"testing"
)

func TestLowerTSXUseState(t *testing.T) {
	src := "function Counter() {\n  const [count, setCount] = useState(0);\n  return <section>\n    <h1>{count}</h1>\n  </section>;\n}\n"
	res := LowerTSX("c.tsx", src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	for _, want := range []string{
		"<Component name=\"Counter\">",
		"count = 0",
		"{count}",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
	}
	// String initializers refuse (buffer slice later).
	str := "function C() {\n  const [s, setS] = useState(\"hi\");\n  return <div>{s}</div>;\n}\n"
	if r := LowerTSX("s.tsx", str); !r.Refused {
		t.Fatalf("expected string-init refusal, got:\n%s", r.SAX)
	}
	// Non-useState statements refuse.
	code := "function C() {\n  const x = 1;\n  return <div />;\n}\n"
	if r := LowerTSX("x.tsx", code); !r.Refused {
		t.Fatalf("expected non-hook refusal, got:\n%s", r.SAX)
	}
	// Setter interpolation refuses.
	setv := "function C() {\n  const [n, setN] = useState(0);\n  return <div>{setN}</div>;\n}\n"
	if r := LowerTSX("v.tsx", setv); !r.Refused {
		t.Fatalf("expected setter-use refusal, got:\n%s", r.SAX)
	}
	// Computed expressions still refuse.
	cmp := "function C() {\n  const [n, setN] = useState(0);\n  return <div>{n + 1}</div>;\n}\n"
	if r := LowerTSX("e.tsx", cmp); !r.Refused {
		t.Fatalf("expected computed refusal, got:\n%s", r.SAX)
	}
}
