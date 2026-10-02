package saemit

// Tests for tryLowerForMacro (todo stage-4 macro adoption, first cut:
// canonical C-for through upstream control.sal FOR_INIT/FOR_CHECK/
// FOR_NEXT; everything else keeps the exact legacy shape).
import (
	"strings"
	"testing"
)

func TestForMacroCanonical(t *testing.T) {
	src := "function main(): i32 {\n  let total: i32 = 0;\n  for (let i: i32 = 0; i < 5; i++) { total = total + i; }\n  return total;\n}\n"
	res := mustLower(t, "formacro.ts", src)
	for _, want := range []string{
		"@import \"sa_std/control.sal\"",
		"EXPAND FOR_INIT i, 0",
		"EXPAND FOR_CHECK i, 5,",
		"EXPAND FOR_NEXT i, 1,",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "slt") {
		t.Errorf("canonical loop must not emit a hand-rolled compare:\n%s", res.SAI)
	}
	// break/continue keep working through the macro shape.
	flow := "function main(): i32 {\n  let total: i32 = 0;\n  for (let i = 0; i < 10; i += 2) {\n    if (i == 4) { continue; }\n    if (i == 8) { break; }\n    total = total + i;\n  }\n  return total;\n}\n"
	res = mustLower(t, "formacro_flow.ts", flow)
	for _, want := range []string{
		"EXPAND FOR_INIT i, 0",
		"EXPAND FOR_CHECK i, 10,",
		"EXPAND FOR_NEXT i, 2,",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestForMacroLegacyShapes(t *testing.T) {
	// Non-canonical shapes keep the exact legacy shape (no EXPAND):
	// identifier bound (signedness), <=, decrement, call bound.
	cases := []struct {
		name string
		src  string
	}{
		{"identbound", "function f(n: i32): i32 {\n  let s: i32 = 0;\n  for (let i = 0; i < n; i++) { s = s + i; }\n  return s;\n}\nfunction main(): i32 {\n  return f(3);\n}\n"},
		{"lte", "function main(): i32 {\n  let s: i32 = 0;\n  for (let i = 0; i <= 3; i++) { s = s + i; }\n  return s;\n}\n"},
		{"dec", "function main(): i32 {\n  let s: i32 = 0;\n  for (let i = 3; i > 0; i--) { s = s + i; }\n  return s;\n}\n"},
	}
	for _, c := range cases {
		res := mustLower(t, "forleg_"+c.name+".ts", c.src)
		if strings.Contains(res.SAI, "EXPAND FOR_") {
			t.Errorf("%s: non-canonical loop must stay legacy, got:\n%s", c.name, res.SAI)
		}
		if strings.Contains(res.SAI, "control.sal") {
			t.Errorf("%s: legacy loop must not import control.sal, got:\n%s", c.name, res.SAI)
		}
	}
}
