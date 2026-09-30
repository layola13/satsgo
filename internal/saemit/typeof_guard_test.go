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
	// Truly unknown names stay "unknown global" (binder agrees).
	unk := "function main(): i32 {\n  if (typeof zzz === \"number\") {\n    return 1;\n  }\n  return 0;\n}\n"
	r := Lower("g4.ts", unk)
	if !r.Refused {
		t.Fatalf("expected unknown-global refusal, got:\n%s", r.SAI)
	}
	found := false
	for _, d := range r.Diagnostics {
		if strings.Contains(d.Error(), "unknown global") {
			found = true
		}
	}
	if !found {
		t.Errorf("want unknown-global diagnostic, got %v", r.Diagnostics)
	}
	// Binder-visible imports are declared (not unknown globals), even
	// though no scope binding records them.
	files := map[string]string{
		"main.ts": "import { y } from \"./u\";\nfunction main(): i32 {\n  if (typeof y === \"number\") {\n    return y;\n  }\n  return 0;\n}\n",
		"u.ts":    "export const y: i32 = 1;\n",
	}
	rp := LowerProgram("main.ts", files)
	if !rp.Refused {
		t.Fatalf("expected not-known refusal, got:\n%s", rp.SAI)
	}
	known := false
	for _, d := range rp.Diagnostics {
		if strings.Contains(d, "not statically known") {
			known = true
		}
		if strings.Contains(d, "unknown global") {
			t.Errorf("binder-visible import must not be unknown: %v", rp.Diagnostics)
		}
	}
	if !known {
		t.Errorf("want not-known diagnostic, got %v", rp.Diagnostics)
	}
}
