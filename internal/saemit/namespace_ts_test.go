// Tests for namespace_ts.go (ambient erasure + runtime flattening).
package saemit

import (
	"strings"
	"testing"
)

func TestLowerDeclareModuleErased(t *testing.T) {
	// planck's joint augmentation shape: ambient call signatures carry
	// no runtime code and must erase (never refuse).
	src := `class DistanceJoint {
  v: i32 = 0;
}
declare module "./DistanceJoint" {
  function DistanceJoint(def: i32): i32;
  function DistanceJoint(a: i32, b: i32): i32;
}
function main(): i32 {
  return 0;
}
`
	res := Lower("decl.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerAmbientFormsErased(t *testing.T) {
	src := `declare global {
  const G: i32;
}
declare namespace Tools {
  function helper(x: i32): i32;
}
export as namespace MyLib;
function main(): i32 {
  return 0;
}
`
	res := Lower("amb.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerNamespaceBasic(t *testing.T) {
	src := `namespace Math2 {
  export const K = 3;
  export function add(a: i32, b: i32): i32 {
    return a + b + K;
  }
}
function main(): i32 {
  return Math2.add(20, 22);
}
`
	res := Lower("ns.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	for _, want := range []string{"call @Math2_add(", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestLowerNamespaceNested(t *testing.T) {
	src := `namespace A {
  export const Base = 10;
  export namespace B {
    export function get(): i32 {
      return Base + 1;
    }
  }
}
function main(): i32 {
  return A.B.get();
}
`
	res := Lower("nest.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	if !strings.Contains(res.SAI, "call @A_B_get(") {
		t.Errorf("missing nested call:\n%s", res.SAI)
	}
}

func TestLowerNamespaceClassEnum(t *testing.T) {
	src := `namespace Shapes {
  export enum Kind {
    Circle = 0,
    Square = 1,
  }
  export class Box {
    v: i32 = 0;
    constructor(n: i32) { this.v = n; }
    get(): i32 { return this.v; }
  }
  export function kindName(k: i32): i32 {
    return k;
  }
}
function main(): i32 {
  const b = new Shapes.Box(Shapes.Kind.Square);
  return b.get();
}
`
	res := Lower("shape.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return:\n%s", res.SAI)
	}
}

func TestLowerNamespaceClassInMethodSibling(t *testing.T) {
	// Class methods resolve bare namespace siblings qualified.
	src := `namespace N {
  export const K = 2;
  export class C {
    get(): i32 { return K; }
  }
}
function main(): i32 {
  const c = new N.C();
  return c.get();
}
`
	res := Lower("sib.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	if !strings.Contains(res.SAI, ", 2 as i32") {
		t.Errorf("want folded sibling const:\n%s", res.SAI)
	}
}

func TestLowerNamespacePrivacy(t *testing.T) {
	src := `namespace P {
  const Hidden = 1;
  export const Shown = 2;
  export function get(): i32 {
    return Hidden + Shown;
  }
}
function main(): i32 {
  return P.get();
}
`
	res := Lower("priv.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	bad := `namespace P {
  const Hidden = 1;
}
function main(): i32 {
  return P.Hidden;
}
`
	if r := Lower("priv2.ts", bad); !r.Refused {
		t.Fatalf("expected privacy refusal, got:\n%s", r.SAI)
	}
}

func TestLowerNamespaceMultiDeclaratorLet(t *testing.T) {
	// Multi-declarator mutable members lower per declarator (each scalar
	// non-arrow declarator is an independent module-state slot).
	src := `namespace M {
  export let a = 1, b = 2;
  export function sum(): i32 {
    return a + b;
  }
  export function bump(): i32 {
    a += 10;
    b += 20;
    return a + b;
  }
}
function main(): i32 {
  return M.sum() + M.bump();
}
`
	res := mustLower(t, "multilet.ts", src)
	for _, want := range []string{`@import "sa_std/modstate.sai"`, "call @M_sum()", "call @M_bump()"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Cross-body forward reads resolve through prescan recording.
	cross := `namespace M {
  export function sum(): i32 {
    return a + b;
  }
}
namespace M {
  export let a = 1, b = 2;
}
function main(): i32 {
  return M.sum();
}
`
	res = mustLower(t, "multiletcross.ts", cross)
	if !strings.Contains(res.SAI, "call @M_sum()") {
		t.Errorf("missing cross-body call:\n%s", res.SAI)
	}
	// Mixed exotic declarators still refuse loudly.
	bad := `namespace M {
  export let a = 1, f = () => 1;
}
function main(): i32 {
  return 0;
}
`
	if r := Lower("multiletbad.ts", bad); !r.Refused {
		t.Fatalf("expected refusal, got:\n%s", r.SAI)
	}
	// String multi-declarators share the same slot path (one slot per
	// declarator, string width dispatched at the store).
	strs := `namespace M {
  export let s1 = "a", s2 = "b";
  export function get(i: i32): string {
    if (i == 0) {
      return s1;
    }
    return s2;
  }
}
function main(): i32 {
  const s = M.get(0);
  if (s == "a") {
    return 7;
  }
  return 0;
}
`
	res = mustLower(t, "multiletstr.ts", strs)
	if !strings.Contains(res.SAI, "call @M_get(") {
		t.Errorf("missing string multi-let call:\n%s", res.SAI)
	}
	// `var` shares the non-const declarator path (same slot mechanics).
	vsrc := `namespace M {
  export var a = 1, b = 2;
  export function sum(): i32 {
    return a + b;
  }
}
function main(): i32 {
  return M.sum();
}
`
	res = mustLower(t, "multivar.ts", vsrc)
	if !strings.Contains(res.SAI, "call @M_sum()") {
		t.Errorf("missing var multi-let call:\n%s", res.SAI)
	}
	// Uninitialized declarators register bare slots; later stores fill them.
	nsrc := `namespace M {
  export let a: i32, b: i32;
  export function init(): i32 {
    a = 1;
    b = 2;
    return a + b;
  }
}
function main(): i32 {
  return M.init();
}
`
	res = mustLower(t, "multinoinit.ts", nsrc)
	if !strings.Contains(res.SAI, "call @M_init()") {
		t.Errorf("missing uninitialized multi-let call:\n%s", res.SAI)
	}
	// Mixed initialized/uninitialized declarators in one statement.
	xsrc := `namespace M {
  export let a = 1, b: i32;
  export function f(): i32 {
    b = 2;
    return a + b;
  }
}
function main(): i32 {
  return M.f();
}
`
	res = mustLower(t, "multimix.ts", xsrc)
	if !strings.Contains(res.SAI, "call @M_f()") {
		t.Errorf("missing mixed multi-let call:\n%s", res.SAI)
	}
	// Nested namespaces qualify per declarator under the extended path.
	nsrc2 := `namespace A {
  export namespace B {
    export let x = 1, y = 2;
    export function sum(): i32 {
      return x + y;
    }
  }
}
function main(): i32 {
  return A.B.sum();
}
`
	res = mustLower(t, "multinest.ts", nsrc2)
	if !strings.Contains(res.SAI, "call @A_B_sum()") {
		t.Errorf("missing nested multi-let call:\n%s", res.SAI)
	}
}

func TestLowerNamespaceRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		// "mutable state" turned positive: namespace `export let` lowers
		// to module-state slots (see TestModStateNamespaceLet).
		// "merging" turned positive: reopened bodies merge (see
		// TestLowerNamespaceReopen).
		// "import equals" turned positive: aliases route through
		// qualify (see TestImportEquals).
		{"unknown member", "namespace N {\n export const x = 1;\n}\nfunction main(): i32 {\n return N.y;\n}\n"},
		{"function nested", "function f(): i32 {\n namespace N {\n export const x = 1;\n }\n return 0;\n}\nfunction main(): i32 { return f(); }\n"},
	}
	for _, c := range cases {
		if r := Lower("nsr.ts", c.src); !r.Refused {
			t.Errorf("%s: expected refusal, got:\n%s", c.name, r.SAI)
		}
	}
}

// Reopening merges bodies: members accumulate, cross-body forward
// references resolve, duplicates refuse.
func TestLowerNamespaceReopen(t *testing.T) {
	src := `namespace M {
  export const a = 1;
  export function f(): i32 {
    return a + g();
  }
}
namespace M {
  export const b = 2;
  export function g(): i32 {
    return b * 10;
  }
}
function main(): i32 {
  return M.f() + M.g();
}
`
	res := mustLower(t, "reopen.ts", src)
	for _, want := range []string{"call @M_f()", "call @M_g()", "@M_f() -> i32:", "@M_g() -> i32:"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Cross-body value reads resolve through prescan recording (const
	// folds and let slots register before any member lowers).
	cross := `namespace M {
  export function f(): i32 {
    return K + counter;
  }
}
namespace M {
  export const K = 3;
  export let counter = 4;
  export function bump(): i32 {
    counter = counter + 1;
    return counter;
  }
}
function main(): i32 {
  M.bump();
  return M.f();
}
`
	res = mustLower(t, "reopencross.ts", cross)
	for _, want := range []string{
		`@import "sa_std/modstate.sai"`,
		"call @M_f()",
		"call @M_bump()",
		"add 3, ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Duplicates refuse whether split across bodies or within one.
	dups := []struct {
		name string
		src  string
	}{
		{"across bodies", "namespace M {\n export const a = 1;\n}\nnamespace M {\n export const a = 2;\n}\nfunction main(): i32 { return 0; }\n"},
		{"within body", "namespace M {\n export const a = 1;\n export function a(): i32 { return 1; }\n}\nfunction main(): i32 { return 0; }\n"},
		{"nested reopen", "namespace A {\n export namespace B {\n export function x(): i32 { return 1; }\n }\n}\nnamespace A {\n export namespace B {\n export function y(): i32 { return 2; }\n }\n}\nfunction main(): i32 { return A.B.x() + A.B.y(); }\n"},
	}
	for _, tc := range dups {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "nested reopen" {
				res := mustLower(t, "nestedreopen.ts", tc.src)
				for _, want := range []string{"call @A_B_x()", "call @A_B_y()"} {
					if !strings.Contains(res.SAI, want) {
						t.Errorf("missing %q in output:\n%s", want, res.SAI)
					}
				}
				return
			}
			res := Lower("refuse.ts", tc.src)
			if !res.Refused {
				t.Fatalf("expected refusal, lowered:\n%s", res.SAI)
			}
			if !strings.Contains(diagText(res), "already declared") {
				t.Errorf("missing duplicate diagnostic:\n%s", diagText(res))
			}
		})
	}
}

// Import-equals aliases route through qualify (member aliases) and the
// namespace receiver branches (namespace aliases); exotic and cross-file
// targets refuse loudly.
func TestImportEquals(t *testing.T) {
	src := `namespace N {
  export const K = 3;
  export let c = 4;
  export function add(a: i32, b: i32): i32 {
    return a + b;
  }
  export class C {
    v: i32 = 0;
    constructor(n: i32) {
      this.v = n;
    }
  }
  export enum E {
    A = 1,
  }
}
import k = N.K;
import cc = N.c;
import add = N.add;
import C = N.C;
import E = N.E;
function main(): i32 {
  cc = cc + 1;
  const c = new C(10);
  return k + cc + add(1, 2) + c.v + E.A;
}
`
	res := mustLower(t, "eqalias.ts", src)
	for _, want := range []string{"call @N_add(1, 2)", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Namespace alias rewrites receivers for calls and reads.
	nsal := `namespace N {
  export const K = 5;
  export function f(): i32 {
    return K;
  }
}
import M = N;
function main(): i32 {
  return M.f() + M.K;
}
`
	res = mustLower(t, "eqns.ts", nsal)
	for _, want := range []string{"call @N_f()", ", 5"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Alias-before-namespace order works (file-wide prescan).
	fwd := `import k = N.K;
namespace N {
  export const K = 7;
}
function main(): i32 {
  return k;
}
`
	res = mustLower(t, "eqfwd.ts", fwd)
	if !strings.Contains(res.SAI, "return 7") {
		t.Errorf("missing forwarded fold in output:\n%s", res.SAI)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unknown member", "namespace N {\n export const x = 1;\n}\nimport y = N.z;\nfunction main(): i32 {\n return 0;\n}\n", "has no member"},
		{"private member", "namespace P {\n const Hidden = 1;\n}\nimport h = P.Hidden;\nfunction main(): i32 {\n return 0;\n}\n", "is not exported"},
		{"require form", "import x = require(\"m\");\nfunction main(): i32 {\n return 0;\n}\n", "require(...) is not lowerable"},
		{"duplicate alias", "namespace N {\n export const x = 1;\n}\nimport y = N.x;\nimport y = N.x;\nfunction main(): i32 {\n return 0;\n}\n", "already declared"},
		{"colliding alias", "namespace N {\n export const x = 1;\n}\nfunction y(): i32 {\n return 0;\n}\nimport y = N.x;\nfunction main(): i32 {\n return y();\n}\n", "collides with an existing definition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Lower("refuse.ts", tc.src)
			if !res.Refused {
				t.Fatalf("expected refusal, lowered:\n%s", res.SAI)
			}
			if !strings.Contains(diagText(res), tc.want) {
				t.Errorf("missing %q in diagnostics:\n%s", tc.want, diagText(res))
			}
		})
	}
}
