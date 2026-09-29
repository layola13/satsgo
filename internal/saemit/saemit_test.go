package saemit

import (
	"os"
	"strings"
	"testing"
)

func mustLower(t *testing.T, name, src string) Result {
	t.Helper()
	res := Lower(name, src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
	return res
}

func diagText(res Result) string {
	var b strings.Builder
	for _, d := range res.Diagnostics {
		b.WriteString(d.Error() + "\n")
	}
	return b.String()
}

func TestLowerFactorial(t *testing.T) {
	src := `function fact(n: i32): i32 {
  if (n <= 1) { return 1; }
  return n * fact(n - 1);
}
function main(): i32 {
  return fact(5);
}
`
	res := mustLower(t, "fact.ts", src)
	for _, want := range []string{
		"@fact(n: i32) -> i32:",
		"@main() -> i32:",
		"br ", " -> ",
		"call @fact(",
		"return ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "jz") {
		t.Errorf("forbidden jz emitted:\n%s", res.SAI)
	}
}

func TestLowerWhileBreakContinue(t *testing.T) {
	src := `function main(): i32 {
  let i: i32 = 0;
  let s: i32 = 0;
  while (i < 10) {
    i = i + 1;
    if (i == 3) { continue; }
    if (i == 8) { break; }
    s = s + i;
  }
  return s;
}
`
	res := mustLower(t, "loop.ts", src)
	for _, want := range []string{"jmp ", "br ", "slt ", "return s"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestLowerStdProjection(t *testing.T) {
	src := `function main(): i32 {
  console.log("hi");
  const x: i32 = Math.abs(0 - 7);
  return x;
}
`
	res := mustLower(t, "std.ts", src)
	for _, want := range []string{
		`@import "sa_std/io/print.sai"`,
		"call @sa_print_bytes(&",
		"sge ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestLowerShadowingNoDoubleRelease(t *testing.T) {
	src := `function main(): i32 {
  let x: i32 = 1;
  if (x > 0) {
    let x: i32 = 2;
    return x;
  }
  return x;
}
`
	res := mustLower(t, "shadow.ts", src)
	if strings.Count(res.SAI, "!x") != 1 {
		t.Errorf("expected exactly one !x release, got:\n%s", res.SAI)
	}
}

func TestLowerClassMethodInline(t *testing.T) {
	src := `interface Item {
  key: number;
  val: number;
}
class Box<T> {
  pick: (e: T) => number;
  constructor(k: (e: T) => number) {
    this.pick = k;
  }
  get(e: T): number {
    return this.pick(e);
  }
}
function main(): i32 {
  const b = new Box((e: Item) => e.key);
  const it: Item = { key: 0, val: 0 };
  it.key = 7;
  it.val = 5;
  return b.get(it) * 10 + it.val;
}
`
	res := mustLower(t, "box.ts", src)
	for _, want := range []string{"alloc", "store", "load", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "jz") {
		t.Errorf("forbidden jz emitted:\n%s", res.SAI)
	}
}

func TestLowerOptionalChainGuard(t *testing.T) {
	src := `interface Box {
  v: i32;
}
function get(b: Box | null): i32 {
  const x = b?.v;
  return x;
}
function main(): i32 {
  return 1;
}
`
	res := mustLower(t, "opt.ts", src)
	// Checker-driven null guard: null base yields 0 via the join.
	for _, want := range []string{"eq b, 0", "L_prop_null", "L_prop_ok", "load b + 0 as i32"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestLowerOptionalNonNullDirect(t *testing.T) {
	src := `function plus(a: i32, b: i32): i32 {
  return a + b;
}
function main(): i32 {
  return plus?.(3, 4);
}
`
	res := mustLower(t, "opt2.ts", src)
	// Non-nullable callee keeps the direct call (no guard labels).
	if strings.Contains(res.SAI, "L_call_null") {
		t.Errorf("unexpected guard for non-nullable callee:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "call @plus(3, 4)") {
		t.Errorf("missing direct call:\n%s", res.SAI)
	}
}

func TestLowerDestructuring(t *testing.T) {
	arr := "function main(): i32 {\n  const p: number[] = [3, 4];\n  const [a, b] = p;\n  return a * 10 + b;\n}\n"
	res := mustLower(t, "ds.ts", arr)
	if !strings.Contains(res.SAI, "return") {
		t.Errorf("missing return:\n%s", res.SAI)
	}
	obj := "interface Pt {\n  x: i32;\n  y: i32;\n}\nfunction main(): i32 {\n  const pt: Pt = { x: 3, y: 4 };\n  const { x, y } = pt;\n  return x + y;\n}\n"
	res = mustLower(t, "ds2.ts", obj)
	for _, want := range []string{"load pt + 0 as i32", "load pt + 4 as i32"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	forof := "function main(): i32 {\n  const pairs: number[][] = [[1, 2], [3, 4]];\n  let s: i32 = 0;\n  for (const [a, b] of pairs) { s = s + a + b; }\n  return s;\n}\n"
	res = mustLower(t, "ds3.ts", forof)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
	}
}

func TestLowerRefusesClassExtends(t *testing.T) {
	src := "class B { x: i32 = 0; }\nclass C extends B {}\nfunction main(): i32 { return 0; }\n"
	res := Lower("cls.ts", src)
	if !res.Refused {
		t.Fatalf("expected refusal for extends, got:\n%s", res.SAI)
	}
}

func TestProjectionTableSymbolsDocumented(t *testing.T) {
	// Every table entry must name its sa_std contract module; CI checks each
	// Symbol against $SCI_ROOT/sa_std (tools/check_sa_std_projection.sh).
	for _, p := range StdProjectionTable {
		if p.TS == "" || p.Symbol == "" {
			t.Errorf("incomplete projection entry: %+v", p)
		}
		if p.Symbol == "@inline" || strings.HasPrefix(p.Symbol, "@const:") {
			continue // emitter-side idioms, no sa_std contract
		}
		if p.Module == "" {
			t.Errorf("missing module for projected symbol: %+v", p)
		}
		if !strings.HasPrefix(p.Module, "sa_std/") {
			t.Errorf("module must live under sa_std/: %+v", p)
		}
	}
}

func TestScaffoldLayout(t *testing.T) {
	dir := t.TempDir()
	results, err := Scaffold(dir, ScaffoldOptions{
		ModuleName: "demo_sa",
		Sources:    map[string]string{"main": "function main(): i32 { return 42; }\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results["main"].Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(results["main"]))
	}
	for _, f := range []string{"sa.mod", "src/main.ts", "src/main.sai", "subset-report.txt", "README.md", "build.sh"} {
		if _, err := os.Stat(dir + "/" + f); err != nil {
			t.Errorf("missing scaffold file %s: %v", f, err)
		}
	}
}
