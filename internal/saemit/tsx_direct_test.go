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
		{"hook", "function C() {\n  const x = 1;\n  return <div />;\n}\n", "useState"},
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
	// Integer/boolean useState initials render through @sa_fmt_i64_into
	// (sext first, so true/false stay "1"/"0").
	num := "function C() {\n  const [c, setC] = useState(0);\n  const [b, setB] = useState(true);\n  return <div>{c}{b}</div>;\n}\n"
	res = mustLowerDirect(t, "num.tsx", num)
	for _, want := range []string{
		"@import \"sa_std/fmt.sai\"",
		"sext ",
		"call @sa_fmt_i64_into(",
		"call @sax_dom_create_text(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Float initials render through @sa_fmt_f64_into at precision 6
	// (the template-interpolation policy).
	fl := "function C() {\n  const [f, setF] = useState(1.5);\n  return <div>{f}</div>;\n}\n"
	res = mustLowerDirect(t, "fl.tsx", fl)
	for _, want := range []string{
		"call @sa_fmt_f64_into(",
		"call @sax_dom_create_text(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestTSXDirectClick(t *testing.T) {
	// onClick with a ctx param lowers to bind_event (ctx=root) plus an
	// @export handler function; bodies take ctx DOM writes only.
	src := "function C() {\n  return <div>\n    <button onClick={(root) => root.setAttribute(\"id\", \"hit\")}>go</button>\n  </div>;\n}\n"
	res := mustLowerDirect(t, "click.tsx", src)
	for _, want := range []string{
		"@export onClick_1(root: i64):",
		"call @sax_dom_bind_event(",
		"*",
		"call @sax_dom_set_attr(",
		"sax_dom_bind_event(node_h: i64, *evt_ptr: ptr",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// textContent write shape.
	txt := "function C() {\n  return <div>\n    <button onClick={(root) => root.textContent = \"done\"}>go</button>\n  </div>;\n}\n"
	res = mustLowerDirect(t, "clicktxt.tsx", txt)
	if !strings.Contains(res.SAI, "call @sax_dom_set_text(") {
		t.Errorf("missing set_text handler:\n%s", res.SAI)
	}
	// appendChild with a nested createElement lowers through the main
	// expression pipeline; non-handle args refuse loudly.
	app := "function C() {\n  return <div>\n    <button onClick={(root) => root.appendChild(document.createElement(\"span\"))}>go</button>\n  </div>;\n}\n"
	res = mustLowerDirect(t, "clickapp.tsx", app)
	for _, want := range []string{
		"call @sax_dom_create(",
		"call @sax_dom_append_child(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"noparam", "function C() {\n  return <button onClick={() => {}}>x</button>;\n}\n", "exactly its ctx param"},
		{"setx", "function C() {\n  const [n, setN] = useState(0);\n  return <button onClick={(r) => setN(1)}>x</button>;\n}\n", "only ctx DOM statements"},
		{"namedref", "function C() {\n  return <button onClick={go}>x</button>;\n}\n", "inline arrow"},
		{"otherhandler", "function C() {\n  return <button onChange={(r) => r.setAttribute(\"a\", \"b\")}>x</button>;\n}\n", "handlers slice"},
		{"nonhandle", "function C() {\n  return <button onClick={(r) => r.appendChild(7)}>x</button>;\n}\n", "takes a DOM node handle"},
	}
	for _, c := range cases {
		r := LowerTSXDirect("clk_"+c.name+".tsx", c.src)
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

func TestTSXDirectCompose(t *testing.T) {
	// Same-file composition: parent calls the child builder with props
	// passed positionally by callee order (exact-match); the child root
	// appends like any node.
	src := "function Badge({ label }: { label: string }) {\n  return <span>{label}</span>;\n}\nfunction Card({ title }: { title: string }) {\n  return <section>\n    <Badge label={title} />\n    <Badge label=\"hi\" />\n  </section>;\n}\n"
	res := mustLowerDirect(t, "compose.tsx", src)
	for _, want := range []string{
		"@render_Badge(label: ptr) -> i64:",
		"@render_Card(title: ptr) -> i64:",
		"call @render_Badge(",
		"call @sax_dom_append_child(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing", "function B({ a }: { a: string }) {\n  return <span>{a}</span>;\n}\nfunction C() {\n  return <div>\n    <B />\n  </div>;\n}\n", "is missing from <B>"},
		{"unknownprop", "function B({ a }: { a: string }) {\n  return <span>{a}</span>;\n}\nfunction C() {\n  return <div>\n    <B a=\"x\" b=\"y\" />\n  </div>;\n}\n", "unknown prop b"},
		{"children", "function B() {\n  return <span>x</span>;\n}\nfunction C() {\n  return <div>\n    <B>kid</B>\n  </div>;\n}\n", "no <Slot /> outlet"},
		{"unknowncomp", "function C() {\n  return <div>\n    <Widget />\n  </div>;\n}\n", "needs the composition slice"},
		{"handlerprop", "function B({ a }: { a: string }) {\n  return <span>{a}</span>;\n}\nfunction C() {\n  return <div>\n    <B a=\"x\" onClick={(r) => r.setAttribute(\"i\", \"v\")} />\n  </div>;\n}\n", "not a string prop"},
	}
	for _, c := range cases {
		r := LowerTSXDirect("cmp_"+c.name+".tsx", c.src)
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

func TestTSXDirectSlot(t *testing.T) {
	// <Slot /> outlets: callee takes trailing slot_N params fed
	// positionally by the caller's kids (exact-N, in order).
	src := "function Box() {\n  return <section>\n    <Slot />\n  </section>;\n}\nfunction Card() {\n  return <div>\n    <Box><b>hi</b></Box>\n  </div>;\n}\n"
	res := mustLowerDirect(t, "slot.tsx", src)
	for _, want := range []string{
		"@render_Box(slot_1: i64) -> i64:",
		"call @render_Box(",
		"call @sax_dom_append_child(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Two outlets take two kids positionally.
	two := "function Layout() {\n  return <section>\n    <header>\n      <Slot />\n    </header>\n    <footer>\n      <Slot />\n    </footer>\n  </section>;\n}\nfunction Page() {\n  return <div>\n    <Layout><b>top</b><i>bot</i></Layout>\n  </div>;\n}\n"
	res = mustLowerDirect(t, "slot2.tsx", two)
	for _, want := range []string{
		"@render_Layout(slot_1: i64, slot_2: i64) -> i64:",
		"call @render_Layout(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"twokids", "function B() {\n  return <section>\n    <Slot />\n  </section>;\n}\nfunction C() {\n  return <div>\n    <B>a<b />b</B>\n  </div>;\n}\n", "exactly 1 children"},
		{"nooutlet", "function B() {\n  return <span>x</span>;\n}\nfunction C() {\n  return <div>\n    <B>kid</B>\n  </div>;\n}\n", "no <Slot /> outlet"},
		{"onekidoftwo", "function B() {\n  return <section>\n    <Slot />\n    <Slot />\n  </section>;\n}\nfunction C() {\n  return <div>\n    <B>x</B>\n  </div>;\n}\n", "exactly 2 children"},
		{"slotroot", "function B() {\n  return <Slot />;\n}\n", "cannot be the render root"},
		{"slotattrs", "function B() {\n  return <section>\n    <Slot id=\"x\" />\n  </section>;\n}\nfunction C() {\n  return <div>\n    <B>y</B>\n  </div>;\n}\n", "bare <Slot /> only"},
	}
	for _, c := range cases {
		r := LowerTSXDirect("slot_"+c.name+".tsx", c.src)
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
