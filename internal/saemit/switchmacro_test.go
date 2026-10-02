package saemit

// Tests for switch SWITCH_2/3 macro adoption (upstream control.sal):
// 2- and 3-arm switches dispatch through EXPAND; 1/4+ arms, missing
// defaults excluded only by arm count, and refusal shapes stay loud.
import (
	"strings"
	"testing"
)

func TestSwitchMacroDispatch(t *testing.T) {
	two := "function f(x: i32): i32 {\n  switch (x) {\n    case 1:\n      return 10;\n    case 2:\n      return 20;\n    default:\n      return 0;\n  }\n}\nfunction main(): i32 {\n  return f(1) + f(2) + f(9);\n}\n"
	res := mustLower(t, "sw2.ts", two)
	for _, want := range []string{
		"@import \"sa_std/control.sal\"",
		"EXPAND SWITCH_2 ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	three := "function f(x: i32): i32 {\n  switch (x) {\n    case 1:\n      return 10;\n    case 2:\n      return 20;\n    case 3:\n      return 30;\n    default:\n      return 0;\n  }\n}\nfunction main(): i32 {\n  return f(3);\n}\n"
	res = mustLower(t, "sw3.ts", three)
	if !strings.Contains(res.SAI, "EXPAND SWITCH_3 ") {
		t.Errorf("missing SWITCH_3:\n%s", res.SAI)
	}
	// No default: unmatched falls to end through the macro default arm.
	nodefault := "function f(x: i32): i32 {\n  let r: i32 = 7;\n  switch (x) {\n    case 1:\n      r = 10;\n      break;\n    case 2:\n      r = 20;\n      break;\n  }\n  return r;\n}\nfunction main(): i32 {\n  return f(1) + f(9);\n}\n"
	res = mustLower(t, "swnodefault.ts", nodefault)
	if !strings.Contains(res.SAI, "EXPAND SWITCH_2 ") {
		t.Errorf("missing SWITCH_2 without default:\n%s", res.SAI)
	}
	// 4 arms stay on the legacy chain (no EXPAND).
	four := "function f(x: i32): i32 {\n  switch (x) {\n    case 1:\n      return 1;\n    case 2:\n      return 2;\n    case 3:\n      return 3;\n    case 4:\n      return 4;\n    default:\n      return 0;\n  }\n}\nfunction main(): i32 {\n  return f(4);\n}\n"
	res = mustLower(t, "sw4.ts", four)
	if strings.Contains(res.SAI, "EXPAND SWITCH_") {
		t.Errorf("4-arm switch must keep the legacy chain, got:\n%s", res.SAI)
	}
	// Multiple defaults still refuse loudly.
	dup := "function f(x: i32): i32 {\n  switch (x) {\n    case 1:\n      return 1;\n    default:\n      return 0;\n    default:\n      return 2;\n  }\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	r := Lower("swdup.ts", dup)
	if !r.Refused {
		t.Fatalf("expected multiple-default refusal, got:\n%s", r.SAI)
	}
	if got := diagText(r); !strings.Contains(got, "multiple default clauses are not lowerable") {
		t.Errorf("missing multiple-default diagnostic, got:\n%s", got)
	}
}
