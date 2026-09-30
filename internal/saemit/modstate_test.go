package saemit

import (
	"strings"
	"testing"
)

// Top-level mutable module state lowers to the sci slot registry
// (sa_std/modstate.sai value externs); assignments never fold.
// See modstate.go.
func TestModStateCounter(t *testing.T) {
	src := `let counter: i32 = 0;
function bump(): i32 {
  counter = counter + 1;
  return counter;
}
function main(): i32 {
  bump();
  return bump();
}
`
	res := mustLower(t, "counter.ts", src)
	for _, want := range []string{
		`@import "sa_std/modstate.sai"`,
		"call @sa_modstate_get_u64(",
		"call @sa_modstate_set_u64(",
		"trunc ",
		" as i32",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// The old fold-then-rebind shape emitted a register copy for the
	// module name (silently wrong across calls); it must be gone.
	if strings.Contains(res.SAI, "counter = t_") || strings.Contains(res.SAI, "\ncounter = ") {
		t.Errorf("register assignment to module state leaked through:\n%s", res.SAI)
	}
}

func TestModStateZeroInitHasNoEnsureBranch(t *testing.T) {
	src := `let counter: i32 = 0;
function main(): i32 {
  counter = 41;
  return counter;
}
`
	res := mustLower(t, "zeroinit.ts", src)
	if strings.Contains(res.SAI, "ms_init") || strings.Contains(res.SAI, "ms_done") {
		t.Errorf("zero-init var must skip the lazy-ensure branch:\n%s", res.SAI)
	}
}

func TestModStateNonZeroInitLaziesOnce(t *testing.T) {
	src := `let step = 5;
function main(): i32 {
  step = step + 1;
  return step;
}
`
	res := mustLower(t, "lazinit.ts", src)
	for _, want := range []string{
		"ms_init", "ms_done",
		"call @sa_modstate_set_u64(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateIncDec(t *testing.T) {
	src := `let n: i32 = 10;
function main(): i32 {
  n++;
  ++n;
  return n;
}
`
	res := mustLower(t, "incdec.ts", src)
	for _, want := range []string{
		"call @sa_modstate_get_u64(",
		"= add ",
		"call @sa_modstate_set_u64(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateUnassignedStillFolds(t *testing.T) {
	src := `let K = 42;
function main(): i32 {
  return K + 1;
}
`
	res := mustLower(t, "fold.ts", src)
	if strings.Contains(res.SAI, "sa_modstate") {
		t.Errorf("unassigned top-level let must keep the const fold:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "add 42, 1") {
		t.Errorf("missing folded add in output:\n%s", res.SAI)
	}
}

func TestModStateShadowedLocalStaysLocal(t *testing.T) {
	src := `let x = 0;
function f(x: i32): i32 {
  x = x + 1;
  return x;
}
function main(): i32 {
  x = 5;
  return f(x);
}
`
	res := mustLower(t, "shadow.ts", src)
	// The module slot exists (main stores through it) ...
	if !strings.Contains(res.SAI, "call @sa_modstate_set_u64(") {
		t.Errorf("missing module slot store in output:\n%s", res.SAI)
	}
	// ... but f's parameter assignment stays a register copy.
	if !strings.Contains(res.SAI, "x = ") {
		t.Errorf("shadowed parameter lost its register assignment:\n%s", res.SAI)
	}
}

func TestModStateRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		// "string state" turned positive: literal string stores lower to
		// dual slots (see TestModStateStringBasic).
		{"effectful init", "function g(): i32 { return 1; }\nlet x = g();\nfunction main(): i32 {\n x = 2;\n return x;\n}\n", "move state into function scope"},
		{"annotation mismatch", "let x: i32 = 1.5;\nfunction main(): i32 {\n x = 2;\n return x;\n}\n", "does not match its annotation"},
		{"redefinition", "let x = 0;\nlet x = 1;\nfunction main(): i32 {\n x = 2;\n return x;\n}\n", "already declared"},
		{"const reassign", "const K = 1;\nfunction main(): i32 {\n K = 2;\n return K;\n}\n", "cannot reassign const"},
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

func TestModStateNamespaceLet(t *testing.T) {
	src := `namespace N {
  export let x = 0;
  export function inc(): i32 {
    x = x + 1;
    return x;
  }
}
function main(): i32 {
  N.x = 41;
  return N.inc();
}
`
	res := mustLower(t, "nslet.ts", src)
	for _, want := range []string{
		`@import "sa_std/modstate.sai"`,
		"call @sa_modstate_get_u64(",
		"call @sa_modstate_set_u64(",
		"call @N_inc()",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateNamespacePrivacy(t *testing.T) {
	src := `namespace N {
  let hidden = 0;
  export function get(): i32 {
    return hidden;
  }
}
function main(): i32 {
  N.hidden = 1;
  return N.get();
}
`
	res := Lower("nspriv.ts", src)
	if !res.Refused {
		t.Fatalf("expected privacy refusal, lowered:\n%s", res.SAI)
	}
	if !strings.Contains(diagText(res), "not exported") {
		t.Errorf("missing privacy diagnostic:\n%s", diagText(res))
	}
}

func TestModStateI64AndF64Widths(t *testing.T) {
	src := `let big: i64 = 0;
let ratio = 1.5;
function main(): i32 {
  big = 7;
  ratio = 2.5;
  return 0;
}
`
	res := mustLower(t, "widths.ts", src)
	for _, want := range []string{
		"sext ",
		" as u64",
		" as f64",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

// String module state rides dual slots (ptr+len) with literal-only
// stores (@const data is immortal); reads materialize headers.
func TestModStateStringBasic(t *testing.T) {
	src := `let mode = "auto";
function main(): i32 {
  mode = "fast";
  return mode.length;
}
`
	res := mustLower(t, "strmode.ts", src)
	for _, want := range []string{
		`@import "sa_std/modstate.sai"`,
		"call @sa_modstate_get_u64(",
		"call @sa_modstate_set_u64(",
		"alloc 16",
		"load ",
		" + 8 as u64",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "\nmode = ") {
		t.Errorf("register assignment to module string leaked through:\n%s", res.SAI)
	}
}

func TestModStateStringMethod(t *testing.T) {
	src := `let mode = "auto";
function shout(): i32 {
  return mode.toUpperCase().length;
}
function main(): i32 {
  mode = "fast";
  return shout();
}
`
	res := mustLower(t, "strmethod.ts", src)
	for _, want := range []string{
		"call @sa_string_to_upper_ascii(",
		"call @sa_modstate_get_u64(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateStringConstInit(t *testing.T) {
	src := `const D = "-";
let sep = "-";
function use(d: string): i32 {
  return d.length;
}
function main(): i32 {
  sep = ",";
  return use(D) + sep.length;
}
`
	res := mustLower(t, "strconst.ts", src)
	if !strings.Contains(res.SAI, "call @sa_modstate_set_u64(") {
		t.Errorf("missing slot stores in output:\n%s", res.SAI)
	}
	// Identifier inits (`let sep = D`) keep baseline refusal: no
	// constant propagation through the fold yet (sequenced gap).
	bad := Lower("strconst2.ts", "const D = \"-\";\nlet sep = D;\nfunction main(): i32 {\n sep = \",\";\n return sep.length;\n}\n")
	if !bad.Refused {
		t.Fatalf("expected refusal for identifier init, lowered:\n%s", bad.SAI)
	}
}

func TestModStateStringRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"computed store", "let s = \"a\";\nfunction main(): i32 {\n s = s + \"x\";\n return 0;\n}\n", "string literals and string constants only"},
		{"call store", "function g(): string { return \"z\"; }\nlet s = \"a\";\nfunction main(): i32 {\n s = g();\n return 0;\n}\n", "string literals and string constants only"},
		{"incdec", "let s = \"a\";\nfunction main(): i32 {\n s++;\n return 0;\n}\n", "++/-- on string module state"},
		{"array element", "let s = \"a\";\nfunction main(): i32 {\n const a = [s];\n s = \"b\";\n return 0;\n}\n", "string array elements"},
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

func TestModStateStringNamespace(t *testing.T) {
	src := `namespace N {
  export let tag = "a";
  export function len(): i32 {
    return tag.length;
  }
}
function main(): i32 {
  N.tag = "bb";
  return N.len();
}
`
	res := mustLower(t, "nsstr.ts", src)
	for _, want := range []string{
		"call @sa_modstate_get_u64(",
		"call @sa_modstate_set_u64(",
		"call @N_len()",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateStringUnassignedFolds(t *testing.T) {
	src := `let K = "hi";
function main(): i32 {
  return K.length;
}
`
	res := mustLower(t, "strfold.ts", src)
	if strings.Contains(res.SAI, "sa_modstate") {
		t.Errorf("unassigned string let must keep the const fold:\n%s", res.SAI)
	}
}

// Slot keys must fit signed 63 bits: the interpreter parses call
// immediates as i64, so full-range u64 keys overflow (error.Overflow).
func TestModStateKeysFitSigned(t *testing.T) {
	src := `let counter: i32 = 0;
let step = 5;
let mode = "auto";
namespace N {
  export let x = 1;
  export let s = "a";
}
function main(): i32 {
  counter = counter + step;
  mode = "fast";
  N.x = N.x + 1;
  N.s = "b";
  return counter + N.x + mode.length + N.s.length;
}
`
	res := mustLower(t, "keys.ts", src)
	found := 0
	for _, line := range strings.Split(res.SAI, "\n") {
		for _, prefix := range []string{"sa_modstate_get_u64(", "sa_modstate_set_u64("} {
			rest := line
			for {
				j := strings.Index(rest, prefix)
				if j < 0 {
					break
				}
				num := rest[j+len(prefix):]
				k := 0
				for k < len(num) && num[k] >= '0' && num[k] <= '9' {
					k++
				}
				if k == 0 {
					t.Fatalf("unparseable slot key in line: %s", line)
				}
				var v uint64
				for _, c := range []byte(num[:k]) {
					v = v*10 + uint64(c-'0')
				}
				if v >= 1<<63 {
					t.Errorf("slot key %d overflows i64 (line: %s)", v, line)
				}
				found++
				rest = rest[j+len(prefix)+k:]
			}
		}
	}
	if found == 0 {
		t.Fatalf("no slot keys found in output:\n%s", res.SAI)
	}
}

// Object module state rides per-field scalar slots (no heap persists);
// reads materialize headers, writes persist per field.
func TestModStateObjectBasic(t *testing.T) {
	src := `interface P {
  x: i32;
  y: i32;
}
let o: P = { x: 1, y: 2 };
function bump(): i32 {
  o.x = o.x + 10;
  o.y++;
  return o.x + o.y;
}
function main(): i32 {
  bump();
  return bump();
}
`
	res := mustLower(t, "objstate.ts", src)
	for _, want := range []string{
		`@import "sa_std/modstate.sai"`,
		"call @sa_modstate_get_u64(",
		"call @sa_modstate_set_u64(",
		"alloc 8",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestModStateObjectWholeStore(t *testing.T) {
	src := `interface P {
  x: i32;
  y: i32;
}
let o: P = { x: 0, y: 0 };
function main(): i32 {
  o = { x: 3, y: 4 };
  return o.x + o.y;
}
`
	res := mustLower(t, "objwhole.ts", src)
	if !strings.Contains(res.SAI, "call @sa_modstate_set_u64(") {
		t.Errorf("missing slot stores in output:\n%s", res.SAI)
	}
}

func TestModStateObjectRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"string field", "interface P {\n  x: i32;\n  s: string;\n}\nlet o: P = { x: 1, s: \"a\" };\nfunction main(): i32 {\n o.x = 2;\n return o.x;\n}\n", "holds scalars only"},
		{"computed store", "interface P {\n  x: i32;\n  y: i32;\n}\nlet o: P = { x: 1, y: 2 };\nfunction mk(): P {\n  return { x: 9, y: 9 };\n}\nfunction main(): i32 {\n o = mk();\n return o.x;\n}\n", "stores object literals only"},
		{"unknown field", "interface P {\n  x: i32;\n}\nlet o: P = { x: 1 };\nfunction main(): i32 {\n o.z = 2;\n return o.x;\n}\n", "has no field z"},
		{"extra key", "interface P {\n  x: i32;\n}\nlet o: P = { x: 1 };\nfunction main(): i32 {\n o = { x: 1, z: 2 };\n return o.x;\n}\n", "is not in the layout"},
		{"deep write", "interface P {\n  x: i32;\n  y: i32;\n}\nlet o: P = { x: 1, y: 2 };\nfunction main(): i32 {\n o.x.y = 1;\n return o.x;\n}\n", "member depth is not lowerable"},
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

func TestModStateObjectNamespace(t *testing.T) {
	src := `namespace N {
  export let o: P = { x: 1, y: 2 };
}
interface P {
  x: i32;
  y: i32;
}
function main(): i32 {
  N.o = { x: 5, y: 6 };
  return N.o.x + N.o.y;
}
`
	res := mustLower(t, "nsobj.ts", src)
	for _, want := range []string{
		"call @sa_modstate_set_u64(",
		"call @sa_modstate_get_u64(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

// Scope bindings shadow module objects: a scalar parameter named like
// the module object must miss (never read the slot layout).
func TestModStateObjectShadowing(t *testing.T) {
	src := `interface P {
  x: i32;
  y: i32;
}
let o: P = { x: 1, y: 2 };
function get(o: i32): i32 {
  return o + 10;
}
function main(): i32 {
  o.x = 100;
  return get(5) + o.x;
}
`
	res := mustLower(t, "objshadow.ts", src)
	// The parameter reads the register (never the slot layout).
	if !strings.Contains(res.SAI, "add o, 10") {
		t.Errorf("shadowed param misrouted in output:\n%s", res.SAI)
	}
	// A member write through a shadowed name refuses (scalars have no
	// fields) instead of storing to the module slot.
	bad := Lower("refuse.ts", "interface P {\n  x: i32;\n  y: i32;\n}\nlet o: P = { x: 1, y: 2 };\nfunction set(o: i32): i32 {\n  o.x = 9;\n  return o;\n}\nfunction main(): i32 {\n  return set(1);\n}\n")
	if !bad.Refused {
		t.Fatalf("expected shadowed member-write refusal, lowered:\n%s", bad.SAI)
	}
}
