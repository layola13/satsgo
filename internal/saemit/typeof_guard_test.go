package saemit

// Tests for typeof_guard.go (typeof-against-"undefined" guards).
import (
	"strings"
	"testing"
)

func TestLowerTypeofGuard(t *testing.T) {
	// planck Math.mod shape: optional param guarded before defaulting.
	src := "function mod(num: i32, min?: i32): i32 {\n  if (typeof min === \"undefined\") {\n    min = 0;\n  }\n  return num + min;\n}\nfunction main(): i32 {\n  return mod(5, 3);\n}\n"
	res := mustLower(t, "g1.ts", src)
	if !strings.Contains(res.SAI, "eq min, 0") {
		t.Errorf("missing null check:\n%s", res.SAI)
	}
	// Negation and swapped order lower the same way.
	neg := "function f(x?: i32): i32 {\n  if (\"undefined\" !== typeof x) {\n    return x;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res = mustLower(t, "g2.ts", neg)
	if !strings.Contains(res.SAI, "ne x, 0") {
		t.Errorf("missing negated check:\n%s", res.SAI)
	}
	// Non-identifier operands refuse loudly.
	bad := "function main(): i32 {\n  const o = { a: 1 };\n  if (typeof o.a === \"undefined\") {\n    return 1;\n  }\n  return 0;\n}\n"
	if r := Lower("g3.ts", bad); !r.Refused {
		t.Fatalf("expected non-identifier refusal, got:\n%s", r.SAI)
	}
}
