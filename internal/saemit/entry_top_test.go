package saemit

import (
	"strings"
	"testing"
)

// Entry synthesis collects top-level executables into a generated @main
// (definitions first, two-pass); files without executables emit nothing
// extra. See entry_top.go.
func TestEntryBareScript(t *testing.T) {
	src := "console.log(\"hi\");\n"
	res := mustLower(t, "bare.ts", src)
	for _, want := range []string{
		"@main() -> i32:",
		"call @sa_print_bytes(",
		"return 0",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "main__user") {
		t.Errorf("spurious rename in output:\n%s", res.SAI)
	}
}

func TestEntryMainCallRoutesRenamed(t *testing.T) {
	src := `function main(): i32 {
  return 41;
}
main();
`
	res := mustLower(t, "callmain.ts", src)
	for _, want := range []string{
		"@main() -> i32:",
		"call @main__user()",
		"@main__user() -> i32:",
		"return 0",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Count(res.SAI, "@main(") != 1 {
		t.Errorf("expected exactly one @main definition:\n%s", res.SAI)
	}
}

func TestEntryStateBeforeCall(t *testing.T) {
	src := `let counter: i32 = 0;
function bump(): i32 {
  counter = counter + 1;
  return counter;
}
counter = 41;
function main(): i32 {
  return bump();
}
main();
`
	res := mustLower(t, "statefirst.ts", src)
	for _, want := range []string{
		"call @sa_modstate_set_u64(",
		"call @main__user()",
		"@main__user() -> i32:",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
}

func TestEntryRecursionStaysRenamed(t *testing.T) {
	src := `function fact(n: i32): i32 {
  if (n <= 1) { return 1; }
  return n * fact(n - 1);
}
function main(): i32 {
  return fact(5);
}
main();
`
	res := mustLower(t, "rec.ts", src)
	if !strings.Contains(res.SAI, "call @fact(") {
		t.Errorf("missing recursive call:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "call @main__user()") {
		t.Errorf("missing renamed entry call:\n%s", res.SAI)
	}
}

func TestEntryNoExecutablesUnchanged(t *testing.T) {
	src := `function main(): i32 {
  return 42;
}
`
	res := mustLower(t, "plain.ts", src)
	if strings.Contains(res.SAI, "main__user") || strings.Contains(res.SAI, "return 0") {
		t.Errorf("entry synthesis leaked into definition-only file:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "@main() -> i32:") {
		t.Errorf("missing user @main:\n%s", res.SAI)
	}
}

func TestEntryRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"rename collision", "function main__user(): i32 {\n return 1;\n}\nfunction main(): i32 {\n return 2;\n}\nmain();\n", "collides with existing definition main__user"},
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

// D2 program parity: the entry file synthesizes through the shared path
// (prefix ""), non-entry top executables refuse loudly.
func TestEntryProgramEntrySynthesizes(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { get } from \"./util\";\nfunction main(): i32 {\n  return get();\n}\nmain();\n",
		"util.ts": "export function get(): i32 {\n  return 7;\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	for _, want := range []string{"@main() -> i32:", "call @main__user()", "@main__user() -> i32:", "call @util__get()"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in linked output:\n%s", want, res.SAI)
		}
	}
}

func TestEntryProgramNonEntryRefuses(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { get } from \"./util\";\nfunction main(): i32 {\n  return get();\n}\n",
		"util.ts": "export function get(): i32 {\n  return 7;\n}\nconsole.log(\"loaded\");\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected refusal for non-entry top executables, got:\n%s", res.SAI)
	}
	found := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d, "non-entry program files") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing non-entry diagnostic: %v", res.Diagnostics)
	}
}

func TestEntryProgramNoExecUnchanged(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { get } from \"./util\";\nfunction main(): i32 {\n  return get();\n}\n",
		"util.ts": "export function get(): i32 {\n  return 7;\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if strings.Contains(res.SAI, "main__user") {
		t.Errorf("spurious rename in executable-free program:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "@main() -> i32:") {
		t.Errorf("missing user @main:\n%s", res.SAI)
	}
}
