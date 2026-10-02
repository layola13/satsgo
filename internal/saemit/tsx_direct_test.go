package saemit

// Tests for tsx_direct.go (road 2 first cut: static JSX to direct
// airlock calls, bypassing .sax).
import (
	"strings"
	"testing"
)

func mustLowerDirect(t *testing.T, name, src string) TSXDirectResult {
	t.Helper()
	res := LowerTSXDirect(name, src)
	if res.Refused {
		msgs := []string{}
		for _, d := range res.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("unexpected refusal:\n%s", strings.Join(msgs, "\n"))
	}
	return res
}

func TestTSXDirectStatic(t *testing.T) {
	src := "function Card() {\n  return <section className=\"card\">\n    <h1>Hi</h1>\n    <br />\n  </section>;\n}\n"
	res := mustLowerDirect(t, "card.tsx", src)
	for _, want := range []string{
		"@extern sax_dom_create(tag_ptr: ptr, tag_len: u64) -> i64",
		"@extern sax_dom_create_text(text_ptr: ptr, text_len: u64) -> i64",
		"@extern sax_dom_append_child(parent_h: i64, child_h: i64)",
		"@extern sax_dom_set_attr(node_h: i64, key_ptr: ptr, key_len: u64, val_ptr: ptr, val_len: u64)",
		"@render_Card() -> i64:",
		"call @sax_dom_create(",
		"call @sax_dom_create_text(",
		"call @sax_dom_append_child(",
		"call @sax_dom_set_attr(",
		"return ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestTSXDirectRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"params", "function C({ a }: { a: string }, { b }: { b: string }) {\n  return <div />;\n}\n", "at most one parameter"},
		{"nonstrprop", "function C({ n }: { n: i32 }) {\n  return <div />;\n}\n", "must be a string in the direct slice"},
		{"interp", "function C() {\n  return <div>{count}</div>;\n}\n", "not in the direct slice"},
		{"hook", "function C() {\n  const [n, setN] = useState(0);\n  return <div />;\n}\n", "integers need int→string"},
		{"effect", "function C() {\n  useEffect(() => {}, []);\n  return <div />;\n}\n", "must be string useState declarations"},
		{"handler", "function C() {\n  return <button onClick={() => {}}>x</button>;\n}\n", "handlers slice"},
		{"custom", "function C() {\n  return <Widget />;\n}\n", "composition slice"},
		{"spread", "function C() {\n  return <div {...{}} />;\n}\n", "spread attributes"},
		{"danger", "function C() {\n  return <script />;\n}\n", "dangerous"},
		{"bareattr", "function C() {\n  return <input disabled />;\n}\n", "bare attribute"},
	}
	for _, c := range cases {
		r := LowerTSXDirect("d_"+c.name+".tsx", c.src)
		if !r.Refused {
			t.Errorf("%s: expected refusal, got:\n%s", c.name, r.SAI)
			continue
		}
		found := false
		for _, d := range r.Diagnostics {
			if strings.Contains(d.Error(), c.want) {
				found = true
			}
		}
		if !found {
			msgs := []string{}
			for _, d := range r.Diagnostics {
				msgs = append(msgs, d.Error())
			}
			t.Errorf("%s: missing %q, got:\n%s", c.name, c.want, strings.Join(msgs, "\n"))
		}
	}
}

func TestTSXDirectInterp(t *testing.T) {
	// String props read at runtime (param slice loads); string useState
	// initials fold to literals; setter use stays loud.
	src := "function Hi({ name }: { name: string }) {\n  const [tag, setTag] = useState(\"hi\");\n  return <section id={name}>\n    <h1>{name}</h1>\n    <p>{tag}</p>\n  </section>;\n}\n"
	res := mustLowerDirect(t, "hi.tsx", src)
	for _, want := range []string{
		"@render_Hi(name: ptr) -> i64:",
		"load name + 0 as ptr",
		"load name + 8 as u64",
		"call @sax_dom_create_text(",
		"call @sax_dom_set_attr(",
		"!name",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Setter use refuses (no render shape for updates).
	bad := "function Hi() {\n  const [tag, setTag] = useState(\"hi\");\n  return <p>{setTag}</p>;\n}\n"
	r := LowerTSXDirect("bad.tsx", bad)
	if !r.Refused {
		t.Fatalf("expected setter-use refusal, got:\n%s", r.SAI)
	}
	// Integer interpolation refuses (no int→string primitive yet).
	noint := "function C() {\n  const [c, setC] = useState(0);\n  return <div>{c}</div>;\n}\n"
	r = LowerTSXDirect("noint.tsx", noint)
	if !r.Refused {
		t.Fatalf("expected integer-interp refusal, got:\n%s", r.SAI)
	}
}
