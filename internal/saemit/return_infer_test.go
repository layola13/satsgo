package saemit

// Tests for typecheck.go inferredReturnType (todo/02#6 fifth knife:
// the "declare `-> T`" gate upgrades from a syntax-Kind refusal to a
// checker-typed verdict).
import (
	"strings"
	"testing"
)

func TestInferredReturnScalar(t *testing.T) {
	// Unannotated scalar returns lower with the checker-inferred type,
	// spelling the signature exactly like its keyword annotation would
	// (booleans render as 0/1 i32, the sa_plugin_ts rule).
	// (Params use real TS types: dialect names like `i32` are checker-blind
	// under NoLib, so inference needs honest `number` inputs.)
	cases := []struct {
		name string
		src  string
		sig  string
	}{
		{"i32", "function add(a: number, b: number) { return a + b; }\nfunction main(): i32 {\n  return add(1, 2);\n}\n", "@add(a: i32, b: i32) -> i32:"},
		{"string", "function greet(n: number) { return \"hi\"; }\nfunction main(): i32 {\n  return 0;\n}\n", "@greet(n: i32) -> ptr:"},
		{"bool", "function isPos(n: number) { return n > 0; }\nfunction main(): i32 {\n  return 0;\n}\n", "@isPos(n: i32) -> i32:"},
		{"arrow", "const f = () => { return 1; };\nfunction main(): i32 {\n  return f();\n}\n", "-> i32"},
	}
	for _, c := range cases {
		res := mustLower(t, "ret_"+c.name+".ts", c.src)
		if !strings.Contains(res.SAI, c.sig) {
			t.Errorf("%s: missing %q in output:\n%s", c.name, c.sig, res.SAI)
		}
	}
	// Void functions keep void (no signature arrow).
	res := mustLower(t, "ret_void.ts", "function f(n: i32) { n = n + 1; }\nfunction main(): i32 {\n  return 0;\n}\n")
	if strings.Contains(res.SAI, "@f(n: i32) ->") {
		t.Errorf("void function must not gain a return arrow:\n%s", res.SAI)
	}
}

func TestInferredReturnRefuses(t *testing.T) {
	// any/union/object returns stay loud: the checker cannot name a
	// concrete scalar, so the legacy refusal applies verbatim.
	cases := []struct {
		name string
		src  string
	}{
		{"any", "function f(x: any) { return x; }\nfunction main(): i32 {\n  return 0;\n}\n"},
		{"union", "function f(n: i32) { if (n > 0) { return \"s\"; }\n  return 0;\n}\nfunction main(): i32 {\n  return 0;\n}\n"},
	}
	for _, c := range cases {
		r := Lower("ret_"+c.name+".ts", c.src)
		if !r.Refused {
			t.Fatalf("%s: expected refusal, got:\n%s", c.name, r.SAI)
		}
		found := false
		for _, d := range r.Diagnostics {
			if strings.Contains(d.Msg, "declares no return type") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: refusal must keep the legacy message, got:\n%s", c.name, diagText(r))
		}
	}
}

func TestInferredReturnProgram(t *testing.T) {
	// Definition and call sites agree across files: the seeded
	// signature carries the inferred scalar, so the call binds it.
	files := map[string]string{
		"main.ts": "import { add } from \"./util\";\nfunction main(): i32 {\n  return add(20, 22);\n}\n",
		"util.ts": "export function add(a: number, b: number) { return a + b; }\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "@util__add(a: i32, b: i32) -> i32:") {
		t.Errorf("missing inferred cross-file signature:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "= call @util__add(20, 22)") {
		t.Errorf("call must bind the inferred value, got:\n%s", res.SAI)
	}
	// Re-exported inferred returns refuse loudly: the propagated copy
	// snapshotted void, so callers would drop the value silently.
	reexp := map[string]string{
		"main.ts": "import { add } from \"./idx\";\nfunction main(): i32 {\n  return add(1, 2);\n}\n",
		"idx.ts":  "export { add } from \"./util\";\n",
		"util.ts": "export function add(a: number, b: number) { return a + b; }\n",
	}
	r := LowerProgram("main.ts", reexp)
	if !r.Refused {
		t.Fatalf("expected re-export refusal, got:\n%s", r.SAI)
	}
	found := false
	for _, d := range r.Diagnostics {
		if strings.Contains(d, "needs an explicit `-> T`") {
			found = true
		}
	}
	if !found {
		t.Errorf("re-export refusal must name the annotation fix, got:\n%s", strings.Join(r.Diagnostics, "\n"))
	}
}
