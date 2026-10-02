package saemit

// Tests for ternary SELECT macro adoption (upstream control.sal):
// integer arms lower through EXPAND SELECT; f64/string arms keep the
// slot join (SELECT has no fadd/ptr copy).
import (
	"strings"
	"testing"
)

func TestTernarySelectMacro(t *testing.T) {
	src := "function pick(c: i32, a: i32, b: i32): i32 {\n  return c ? a : b;\n}\nfunction main(): i32 {\n  return pick(1, 10, 20) + pick(0, 1, 2);\n}\n"
	res := mustLower(t, "ternsel.ts", src)
	for _, want := range []string{
		"@import \"sa_std/control.sal\"",
		"EXPAND SELECT ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "alloc 8") {
		t.Errorf("integer ternary must not take the slot join:\n%s", res.SAI)
	}
	// f64 and string arms stay on the slot join (no EXPAND).
	legacy := []struct {
		name string
		src  string
	}{
		{"f64", "function pick(c: i32): f64 {\n  return c ? 1.5 : 2.5;\n}\nfunction main(): i32 {\n  return 0;\n}\n"},
		{"str", "function pick(c: i32, a: string, b: string): string {\n  return c ? a : b;\n}\nfunction main(): i32 {\n  return 0;\n}\n"},
	}
	for _, c := range legacy {
		res := mustLower(t, "ternleg_"+c.name+".ts", c.src)
		if strings.Contains(res.SAI, "EXPAND SELECT") {
			t.Errorf("%s: non-integer ternary must keep the slot join, got:\n%s", c.name, res.SAI)
		}
	}
}
