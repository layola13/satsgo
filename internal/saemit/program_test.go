package saemit

import (
	"strings"
	"testing"
)

func mustLowerProgram(t *testing.T, entry string, files map[string]string) ProgramResult {
	t.Helper()
	res := LowerProgram(entry, files)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
	return res
}

func TestLowerProgramCrossFile(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { add } from \"./util\";\nimport * as m from \"./math\";\nfunction main(): i32 {\n  return add(20, 22) + m.square(5);\n}\n",
		"util.ts": "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\n",
		"math.ts": "export function square(x: i32): i32 {\n  return x * x;\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	for _, want := range []string{"@util__add", "@math__square", "@main", "call @util__add(20, 22)", "call @math__square(5)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in linked output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "jz") {
		t.Errorf("forbidden jz emitted:\n%s", res.SAI)
	}
}

func TestLowerProgramCycleRefuses(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { a } from \"./b\";\nfunction main(): i32 { return a(); }\n",
		"b.ts":    "import { main } from \"./main\";\nexport function a(): i32 { return 1; }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected cycle refusal, got:\n%s", res.SAI)
	}
	if !strings.Contains(strings.Join(res.Diagnostics, "\n"), "import cycle") {
		t.Errorf("missing cycle chain:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
}

func TestLowerProgramImportFirst(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { sub } from \"./util\";\nfunction main(): i32 { return add(1, 2) + sub(5, 1); }\n",
		"util.ts": "export function add(a: i32, b: i32): i32 { return a + b; }\nexport function sub(a: i32, b: i32): i32 { return a - b; }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected import-first refusal, got:\n%s", res.SAI)
	}
	if !strings.Contains(strings.Join(res.Diagnostics, "\n"), "import it first") {
		t.Errorf("missing import-first hint:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
}

func TestLowerProgramDefaultExportRefuses(t *testing.T) {
	files := map[string]string{
		"main.ts": "import d from \"./util\";\nfunction main(): i32 { return d(1); }\n",
		"util.ts": "export default function d(x: i32): i32 { return x; }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected default-import refusal, got:\n%s", res.SAI)
	}
}

func TestLowerProgramMissingExportRefuses(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { nope } from \"./util\";\nfunction main(): i32 { return nope(); }\n",
		"util.ts": "export function yup(): i32 { return 1; }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected missing-export refusal, got:\n%s", res.SAI)
	}
}
