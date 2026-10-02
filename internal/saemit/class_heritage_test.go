// Tests for class_heritage.go (single-extends flattening + super routing +
// implements/interface-extends erasure).
package saemit

import (
	"strings"
	"testing"
)

func TestLowerClassExtendsBasic(t *testing.T) {
	src := `class Animal {
  legs: i32 = 0;
  constructor(n: i32) { this.legs = n; }
  describe(): i32 { return this.legs; }
}
class Dog extends Animal {
  tags: i32 = 0;
  constructor(n: i32, x: i32) { super(n); this.tags = x; }
  total(): i32 { return this.describe(); }
}
function main(): i32 {
  const d = new Dog(4, 1);
  return d.total();
}
`
	res := Lower("dog.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	// Base field + own field both materialize; inherited method inlines.
	for _, want := range []string{"store ", "load ", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestLowerClassExtendsOverrideAndSuperMethod(t *testing.T) {
	src := `class Base {
  v: i32 = 0;
  constructor(n: i32) { this.v = n; }
  get(): i32 { return this.v; }
}
class Sub extends Base {
  constructor(n: i32) { super(n); }
  get(): i32 { return super.get(); }
}
function main(): i32 {
  const s = new Sub(7);
  return s.get();
}
`
	res := Lower("sub.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in output:\n%s", res.SAI)
	}
}

func TestLowerClassInheritsCtor(t *testing.T) {
	// No declared ctor: the base node wires (default derived constructor).
	src := `class Base {
  v: i32 = 0;
  constructor(n: i32) { this.v = n; }
  get(): i32 { return this.v; }
}
class Kid extends Base {
}
function main(): i32 {
  const k = new Kid(3);
  return k.get();
}
`
	res := Lower("kid.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerClassImplementsErased(t *testing.T) {
	src := `interface Named {
  name: i32;
}
class Point implements Named {
  name: i32 = 0;
  constructor(n: i32) { this.name = n; }
}
function main(): i32 {
  const p = new Point(5);
  return p.name;
}
`
	res := Lower("pt.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerInterfaceExtendsFlattens(t *testing.T) {
	src := `interface J {
  a: i32;
}
interface I extends J {
  b: i32;
}
function sum(o: I): i32 {
  return o.a;
}
function main(): i32 {
  return sum({a: 1, b: 2});
}
`
	res := Lower("iface.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerAbstractRefusesNew(t *testing.T) {
	src := `abstract class Shape {
  v: i32 = 0;
}
class Circle extends Shape {
  constructor(n: i32) { super(n); }
}
function main(): i32 {
  const c = new Circle(1);
  return c.v;
}
`
	res := Lower("abs.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	bad := `abstract class Shape {
  v: i32 = 0;
}
function main(): i32 {
  const s = new Shape();
  return 0;
}
`
	if r := Lower("abs2.ts", bad); !r.Refused {
		t.Fatalf("expected refusal for new on abstract class, got:\n%s", r.SAI)
	}
}

func TestLowerHeritageRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"unknown base", "class C extends Missing {}\nfunction main(): i32 { return 0; }\n"},
		{"self cycle", "class A extends A {}\nfunction main(): i32 { return 0; }\n"},
		{"dynamic base", "class C extends (Object as any) {}\nfunction main(): i32 { return 0; }\n"},
		{"missing super", "class B {\n v: i32 = 0;\n constructor(n: i32) { this.v = n; }\n}\nclass C extends B {\n constructor(n: i32) { this.v = n; }\n}\nfunction main(): i32 { return 0; }\n"},
		{"super outside method", "class B {\n v: i32 = 0;\n}\nfunction f(): i32 { return super.v; }\nfunction main(): i32 { return f(); }\n"},
	}
	for _, c := range cases {
		if r := Lower("h.ts", c.src); !r.Refused {
			t.Errorf("%s: expected refusal, got:\n%s", c.name, r.SAI)
		}
	}
}

// Method calls on class-annotated params/locals resolve through the
// checker (no scope maps are written, so cross-function staleness cannot
// form). Rebound, nullable and static receivers stay loud.
func TestCheckerClassDispatch(t *testing.T) {
	cls := `class C {
  v: i32 = 0;
  constructor(n: i32) { this.v = n; }
  set(n: i32): void { this.v = n; }
  get(): i32 { return this.v + 1; }
}
`
	// Annotated param dispatches (was: first-class refusal).
	param := cls + "function f(c: C): i32 {\n  return c.get();\n}\nfunction main(): i32 {\n  const c = new C(3);\n  return f(c);\n}\n"
	res := mustLower(t, "cd1.ts", param)
	if !strings.Contains(res.SAI, "L_m_end_") {
		t.Errorf("want inlined method body, got:\n%s", res.SAI)
	}
	// Annotated local dispatches.
	local := cls + "function main(): i32 {\n  const c: C = new C(3);\n  return c.get();\n}\n"
	res = mustLower(t, "cd2.ts", local)
	if !strings.Contains(res.SAI, "L_m_end_") {
		t.Errorf("want inlined method body, got:\n%s", res.SAI)
	}
	// Field stores never rebind the root: still dispatch.
	store := cls + "function f(c: C): i32 {\n  c.set(9);\n  return c.get();\n}\nfunction main(): i32 {\n  const c = new C(0);\n  return f(c);\n}\n"
	res = mustLower(t, "cd3.ts", store)
	if !strings.Contains(res.SAI, "L_m_end_") {
		t.Errorf("want inlined method body after field store, got:\n%s", res.SAI)
	}
	// Plain rebinds may no longer hold the class: stay loud.
	rebind := cls + "function f(c: C): i32 {\n  c = 5;\n  return c.get();\n}\nfunction main(): i32 {\n  const c = new C(3);\n  return f(c);\n}\n"
	if r := Lower("cd4.ts", rebind); !r.Refused {
		t.Fatalf("expected rebind refusal, got:\n%s", r.SAI)
	}
	// Nullable receivers skip the null guard nowhere: stay loud.
	nul := cls + "function f(c: C | null): i32 {\n  return c.get();\n}\nfunction main(): i32 {\n  const c = new C(3);\n  return f(c);\n}\n"
	if r := Lower("cd5.ts", nul); !r.Refused {
		t.Fatalf("expected nullable refusal, got:\n%s", r.SAI)
	}
	// Static call position dispatches as a static (never as an instance).
	staticSrc := "class C {\n  v: i32 = 0;\n  constructor(n: i32) { this.v = n; }\n  static create(n: i32): i32 { return n * 2; }\n  get(): i32 { return this.v + 1; }\n}\nfunction main(): i32 {\n  return C.create(21);\n}\n"
	res = mustLower(t, "cd6.ts", staticSrc)
	if !strings.Contains(res.SAI, "L_m_end_") {
		t.Errorf("want inlined static body, got:\n%s", res.SAI)
	}
	// Cross-function reuse without annotation stays loud (stateless:
	// nothing leaks from the annotated sibling).
	sib := cls + "function f(c: C): i32 {\n  return c.get();\n}\nfunction g(c): i32 {\n  return c.get();\n}\nfunction main(): i32 {\n  const c = new C(3);\n  return f(c) + g(c);\n}\n"
	r := Lower("cd7.ts", sib)
	if !r.Refused {
		t.Fatalf("expected unannotated-sibling refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(diagText(r), "first-class") {
		t.Errorf("want first-class diagnostic, got: %v", r.Diagnostics)
	}
}
