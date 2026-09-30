package saemit

// Tests for dom_proj.go (airlock DOM projection, road 2 task 7 slice).
import (
	"strings"
	"testing"
)

func TestLowerDOM(t *testing.T) {
	src := "function main(): i32 {\n  const el = document.createElement(\"div\");\n  el.setAttribute(\"class\", \"box\");\n  const root = document.createElement(\"section\");\n  root.appendChild(el);\n  return 1;\n}\n"
	res := mustLower(t, "d1.ts", src)
	for _, want := range []string{
		"call @sax_dom_create",
		"call @sax_dom_set_attr",
		"call @sax_dom_append_child",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Inline temporaries route as children.
	nest := "function main(): i32 {\n  const root = document.createElement(\"div\");\n  root.appendChild(document.createElement(\"span\"));\n  return 1;\n}\n"
	if r := Lower("d2.ts", nest); r.Refused {
		msgs := []string{}
		for _, d := range r.Diagnostics {
			msgs = append(msgs, d.Error())
		}
		t.Fatalf("nested create must lower:\n%s", strings.Join(msgs, "\n"))
	}
	// Non-string tags refuse loudly.
	tag := "function main(): i32 {\n  const el = document.createElement(7);\n  return 1;\n}\n"
	if r := Lower("d3.ts", tag); !r.Refused {
		t.Fatalf("expected tag-type refusal, got:\n%s", r.SAI)
	}
	// Non-handle children refuse loudly (an i64 is not a node).
	kid := "function main(): i32 {\n  const el = document.createElement(\"div\");\n  el.appendChild(3);\n  return 1;\n}\n"
	if r := Lower("d4.ts", kid); !r.Refused {
		t.Fatalf("expected handle refusal, got:\n%s", r.SAI)
	}
	// Unknown DOM methods refuse for later slices.
	q := "function main(): i32 {\n  const el = document.createElement(\"div\");\n  el.querySelector(\".x\");\n  return 1;\n}\n"
	if r := Lower("d5.ts", q); !r.Refused {
		t.Fatalf("expected unknown-method refusal, got:\n%s", r.SAI)
	}
}
