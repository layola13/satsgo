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

func TestLowerTSXMountEffect(t *testing.T) {
	src := "function Counter() {\n  const [count, setCount] = useState(0);\n  useEffect(() => { setCount(5); }, []);\n  return <section>\n    <h1>{count}</h1>\n  </section>;\n}\n"
	res := LowerTSX("m.tsx", src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	for _, want := range []string{
		"count = 0",
		"@onMount:",
		"store state+Counter_count, 5 as i64",
		"call @render()",
	} {
		if !strings.Contains(res.SAX, want) {
			t.Errorf("missing %q:\n%s", want, res.SAX)
		}
	}
	// Empty mount effect emits a bare block.
	empty := "function C() {\n  const [n, setN] = useState(0);\n  useEffect(() => {}, []);\n  return <div>{n}</div>;\n}\n"
	res = LowerTSX("e2.tsx", empty)
	if res.Refused {
		t.Fatalf("empty mount effect must lower")
	}
	if !strings.Contains(res.SAX, "@onMount:") {
		t.Errorf("missing onMount block:\n%s", res.SAX)
	}
	// Non-empty deps refuse.
	deps := "function C() {\n  const [n, setN] = useState(0);\n  useEffect(() => {}, [n]);\n  return <div>{n}</div>;\n}\n"
	if r := LowerTSX("d.tsx", deps); !r.Refused {
		t.Fatalf("expected deps refusal, got:\n%s", r.SAX)
	}
	// Cleanup returns refuse.
	cl := "function C() {\n  const [n, setN] = useState(0);\n  useEffect(() => { return () => {}; }, []);\n  return <div>{n}</div>;\n}\n"
	if r := LowerTSX("c.tsx", cl); !r.Refused {
		t.Fatalf("expected cleanup refusal, got:\n%s", r.SAX)
	}
	// Other hooks refuse.
	mm := "function C() {\n  const m = useMemo(() => 1, []);\n  return <div />;\n}\n"
	if r := LowerTSX("m2.tsx", mm); !r.Refused {
		t.Fatalf("expected other-hook refusal, got:\n%s", r.SAX)
	}
	// Computed setter arguments refuse.
	ca := "function C() {\n  const [n, setN] = useState(0);\n  useEffect(() => { setN(n + 1); }, []);\n  return <div>{n}</div>;\n}\n"
	if r := LowerTSX("a.tsx", ca); !r.Refused {
		t.Fatalf("expected computed-arg refusal, got:\n%s", r.SAX)
	}
	// console.* bodies refuse deliberately: .sax handlers have no print
	// imports (no @const/@import in any demo), so emitting print_bytes
	// would assemble the wrong dialect (stdlib.go principle).
	co := "function C() {\n  const [n, setN] = useState(0);\n  useEffect(() => { console.log(\"hi\"); }, []);\n  return <div>{n}</div>;\n}\n"
	if r := LowerTSX("o.tsx", co); !r.Refused {
		t.Fatalf("expected console-body refusal, got:\n%s", r.SAX)
	}
}
