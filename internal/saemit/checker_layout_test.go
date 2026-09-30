package saemit

// Tests for checker_layout.go (checker-driven struct layouts).
import (
	"strings"
	"testing"
)

func TestCheckerLayoutInferred(t *testing.T) {
	// v has no annotation and no literal init: only the checker's
	// return-type inference reveals P's layout (refused before).
	src := "interface P { x: i32; y: i32 }\nfunction mk(): P { return { x: 1, y: 2 }; }\nfunction main(): i32 {\n  const v = mk();\n  return v.x + v.y;\n}\n"
	res := mustLower(t, "l1.ts", src)
	if !strings.Contains(res.SAI, "as i32") {
		t.Errorf("missing struct field loads:\n%s", res.SAI)
	}
	// Anonymous factory shape resolves through matchLayout by field set.
	anon := "interface Q { a: i32; b: i32 }\nfunction mkq(): { a: i32; b: i32 } { return { a: 3, b: 4 }; }\nfunction main(): i32 {\n  const v = mkq();\n  return v.a + v.b;\n}\n"
	res = mustLower(t, "l2.ts", anon)
	if !strings.Contains(res.SAI, "as i32") {
		t.Errorf("missing anonymous field loads:\n%s", res.SAI)
	}
	// Unknown shapes still refuse loudly (no silent fallback).
	bad := "function main(): i32 {\n  const v = mkMissing();\n  return v.x;\n}\n"
	if r := Lower("l3.ts", bad); !r.Refused {
		t.Fatalf("expected unknown-shape refusal, got:\n%s", r.SAI)
	}
	// Monomorphization: Box<i32> and Box<string> get distinct field
	// widths (value at +4 as i32 vs +8 as ptr).
	mono := "interface Box<T> { tag: i32; value: T; }\nfunction getI(b: Box<i32>): i32 { return b.value; }\nfunction getS(b: Box<string>): string { return b.value; }\nfunction main(): i32 {\n  return 0;\n}\n"
	res = mustLower(t, "l5.ts", mono)
	if !strings.Contains(res.SAI, "+ 4 as i32") {
		t.Errorf("missing i32 field load:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "+ 8 as ptr") {
		t.Errorf("missing string field load:\n%s", res.SAI)
	}
	// Type-alias object shapes record layouts, including nested descent
	// through alias-typed fields (planck TransformValue/RotValue shape).
	nested := "export type RotValue = { c: i32; s: i32 };\nexport type TransformValue = { p: P; q: RotValue };\ninterface P { x: i32; y: i32 }\nfunction f(t: TransformValue): i32 {\n  return t.q.c + t.p.x;\n}\nfunction main(): i32 {\n  return 1;\n}\n"
	res = mustLower(t, "l4.ts", nested)
	if c := strings.Count(res.SAI, "as i32"); c < 2 {
		t.Errorf("want nested loads, got:\n%s", res.SAI)
	}
	// Self-recursive generics terminate via the shell cache and route
	// through the canonical key (no lazy-instantiation refusal).
	rec := "interface List<T> { head: T; tail: List<T>; }\nfunction sum(l: List<i32>): i32 { return l.head + l.tail.head; }\nfunction main(): i32 {\n  return 0;\n}\n"
	res = mustLower(t, "l6.ts", rec)
	if c := strings.Count(res.SAI, "as i32"); c < 2 {
		t.Errorf("want recursive loads, got:\n%s", res.SAI)
	}
}
