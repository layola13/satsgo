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
		{"params", "function C({ n }: { n: i32 }) {\n  return <div />;\n}\n", "takes params"},
		{"interp", "function C() {\n  return <div>{count}</div>;\n}\n", "not in the direct slice"},
		{"hook", "function C() {\n  const [n, setN] = useState(0);\n  return <div />;\n}\n", "single return of JSX"},
		{"handler", "function C() {\n  return <button onClick={() => {}}>x</button>;\n}\n", "handlers slice"},
		{"custom", "function C() {\n  return <Widget />;\n}\n", "composition slice"},
		{"spread", "function C() {\n  return <div {...{}} />;\n}\n", "spread attributes"},
		{"danger", "function C() {\n  return <script />;\n}\n", "dangerous"},
		{"nonstring", "function C() {\n  return <div tabIndex={1} />;\n}\n", "non-string attribute"},
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
