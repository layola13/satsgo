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

func TestLowerRefusesClass(t *testing.T) {
	src := "class C { x: i32 = 0; }\nfunction main(): i32 { return 0; }\n"
	res := Lower("cls.ts", src)
	if !res.Refused {
		t.Fatalf("expected refusal for class, got:\n%s", res.SAI)
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
