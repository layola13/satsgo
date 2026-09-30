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

func TestLowerNamespaceRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"mutable state", "namespace M {\n export let x = 1;\n}\nfunction main(): i32 { return 0; }\n"},
		{"merging", "namespace M {\n export const a = 1;\n}\nnamespace M {\n export const b = 2;\n}\nfunction main(): i32 { return 0; }\n"},
		{"import equals", "namespace N {\n export const x = 1;\n}\nimport y = N.x;\nfunction main(): i32 { return 0; }\n"},
		{"unknown member", "namespace N {\n export const x = 1;\n}\nfunction main(): i32 {\n return N.y;\n}\n"},
		{"function nested", "function f(): i32 {\n namespace N {\n export const x = 1;\n }\n return 0;\n}\nfunction main(): i32 { return f(); }\n"},
	}
	for _, c := range cases {
		if r := Lower("nsr.ts", c.src); !r.Refused {
			t.Errorf("%s: expected refusal, got:\n%s", c.name, r.SAI)
		}
	}
}
