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
	// Checker-known kinds fold (number param; dialect i32 annotations
	// are Any under NoLib, so only real TS types resolve).
	fold := "function f(x: number): string {\n  return typeof x;\n}\nfunction main(): i32 {\n  const s: string = f(1);\n  return s.length;\n}\n"
	res = mustLower(t, "g5.ts", fold)
	if !strings.Contains(res.SAI, "number") {
		t.Errorf("missing folded kind:\n%s", res.SAI)
	}
	// any-typed values refuse (no silent kind).
	anyv := "function f(x: any): string {\n  return typeof x;\n}\nfunction main(): i32 {\n  return 1;\n}\n"
	if r := Lower("g6.ts", anyv); !r.Refused {
		t.Fatalf("expected any refusal, got:\n%s", r.SAI)
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

func TestLowerTypeofConstFold(t *testing.T) {
	// Checker-known kind folds: two string allocs + index_of collapse
	// to one constant compare materialised into a temp (br takes
	// registers, never immediates).
	src := "function f(x: number): i32 {\n  if (typeof x === \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res := mustLower(t, "k1.ts", src)
	if !strings.Contains(res.SAI, "= eq 1, 1") {
		t.Errorf("want folded eq 1, 1, got:\n%s", res.SAI)
	}
	if strings.Contains(res.SAI, "index_of") {
		t.Errorf("fold must drop the string compare:\n%s", res.SAI)
	}
	// Mismatch folds to ne; negation flips; either side order works.
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"mismatch", "function f(x: number): i32 {\n  if (typeof x === \"string\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= ne 1, 1"},
		{"negHit", "function f(x: number): i32 {\n  if (typeof x !== \"string\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= eq 1, 1"},
		{"negMiss", "function f(x: number): i32 {\n  if (typeof x !== \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= ne 1, 1"},
		{"swapped", "function f(x: number): i32 {\n  if (\"number\" === typeof x) {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= eq 1, 1"},
		{"loose", "function f(x: number): i32 {\n  if (typeof x == \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= eq 1, 1"},
		{"litHit", "function main(): i32 {\n  if (typeof 1 === \"number\") {\n    return 1;\n  }\n  return 0;\n}\n", "= eq 1, 1"},
		{"litMiss", "function main(): i32 {\n  if (typeof \"a\" === \"number\") {\n    return 1;\n  }\n  return 0;\n}\n", "= ne 1, 1"},
	}
	for _, c := range cases {
		res := mustLower(t, c.name+".ts", c.src)
		if !strings.Contains(res.SAI, c.want) {
			t.Errorf("%s: want %s, got:\n%s", c.name, c.want, res.SAI)
		}
		if strings.Contains(res.SAI, "index_of") {
			t.Errorf("%s: fold must drop the string compare:\n%s", c.name, res.SAI)
		}
	}
	// "undefined" + non-identifier stays loud (guard path refuses);
	// the fold never claims undefined pairs.
	nlit := "function main(): i32 {\n  if (typeof null === \"undefined\") {\n    return 1;\n  }\n  return 0;\n}\n"
	if r := Lower("knlit.ts", nlit); !r.Refused {
		t.Fatalf("expected non-identifier undefined refusal, got:\n%s", r.SAI)
	}
	// Value position also folds (no branch involved).
	val := "function f(x: number): i32 {\n  const b = typeof x === \"number\";\n  if (b) {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res = mustLower(t, "kval.ts", val)
	if !strings.Contains(res.SAI, "= eq 1, 1") {
		t.Errorf("want value fold eq 1, 1, got:\n%s", res.SAI)
	}
	// Unknown kinds keep the loud path (any refuses, no silent 0/1).
	anyv := "function f(x: any): i32 {\n  if (typeof x === \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	if r := Lower("kany.ts", anyv); !r.Refused {
		t.Fatalf("expected any refusal, got:\n%s", r.SAI)
	}
	// "undefined" pairs stay on the null-check path, never the fold.
	undef := "function f(x?: i32): i32 {\n  if (typeof x === \"undefined\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res = mustLower(t, "kundef.ts", undef)
	if !strings.Contains(res.SAI, "eq x, 0") {
		t.Errorf("undefined must keep the null check, got:\n%s", res.SAI)
	}
}

func TestTypeofScalarAnnotation(t *testing.T) {
	// Dialect-annotated bindings (checker-blind under NoLib) fold from
	// their declared annotation. Previously `typeof y === "number"`
	// with y: i32 refused "not statically known"; now it folds.
	src := "function f(y: i32): i32 {\n  if (typeof y === \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res := mustLower(t, "s1.ts", src)
	if !strings.Contains(res.SAI, "= eq 1, 1") {
		t.Errorf("want annotation fold eq 1, 1, got:\n%s", res.SAI)
	}
	if strings.Contains(res.SAI, "index_of") {
		t.Errorf("fold must drop the string compare:\n%s", res.SAI)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"i32miss", "function f(y: i32): i32 {\n  if (typeof y === \"string\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n", "= ne 1, 1"},
		{"boolHit", "function f(b: boolean): i32 {\n  if (typeof b === \"boolean\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(true);\n}\n", "= eq 1, 1"},
		{"u64Hit", "function f(n: u64): i32 {\n  if (typeof n === \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(2);\n}\n", "= eq 1, 1"},
		{"bigintHit", "function f(v: bigint): i32 {\n  if (typeof v === \"bigint\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(3);\n}\n", "= eq 1, 1"},
		{"ifaceObj", "interface P { x: i32 }\nfunction f(p: P): i32 {\n  if (typeof p === \"object\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return 0;\n}\n", "= eq 1, 1"},
		{"localConst", "function main(): i32 {\n  const n: i64 = 5;\n  if (typeof n === \"number\") {\n    return 1;\n  }\n  return 0;\n}\n", "= eq 1, 1"},
	}
	for _, c := range cases {
		res := mustLower(t, c.name+".ts", c.src)
		if !strings.Contains(res.SAI, c.want) {
			t.Errorf("%s: want %s, got:\n%s", c.name, c.want, res.SAI)
		}
		if strings.Contains(res.SAI, "index_of") {
			t.Errorf("%s: fold must drop the string compare:\n%s", c.name, res.SAI)
		}
	}
	// Bare typeof also resolves from the annotation (same source).
	bare := "function f(y: i32): string {\n  return typeof y;\n}\nfunction main(): i32 {\n  const s: string = f(1);\n  return s.length;\n}\n"
	res = mustLower(t, "sbare.ts", bare)
	if !strings.Contains(res.SAI, "number") {
		t.Errorf("want annotated kind slice, got:\n%s", res.SAI)
	}
	// Unannotated bindings stay loud (no annotation to consult,
	// checker blind without a declaration type).
	unann := "function f(y): i32 {\n  if (typeof y === \"number\") {\n    return 1;\n  }\n  return 0;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	if r := Lower("sunann.ts", unann); !r.Refused {
		t.Fatalf("expected unannotated refusal, got:\n%s", r.SAI)
	}
}
