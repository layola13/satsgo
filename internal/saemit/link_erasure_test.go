package saemit

// Tests for link_erasure.go (usage-based import erasure). The feature code
// lives in its own module; these tests move with it (project rule: new
// features ship implementation + tests as one module).
import (
	"strings"
	"testing"
)

func TestLowerProgramTypeOnlyEdgeSkipped(t *testing.T) {
	// A value cycle through a type-only edge is erasable: b's use of
	// main is `import type`, so no runtime cycle exists (planck
	// Shape->Body shape).
	files := map[string]string{
		"main.ts": "import { a } from \"./b\";\nfunction main(): i32 { return a(); }\n",
		"b.ts":    "import type { m } from \"./main\";\nexport function a(): i32 { return 1; }\n",
	}
	res := LowerProgram("main.ts", files)
	if res.Refused {
		t.Fatalf("type-only edge must not fuse a cycle, got:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
	if !strings.Contains(res.SAI, "call @b__a()") {
		t.Errorf("missing linked call:\n%s", res.SAI)
	}
	// export type re-exports erase the same way.
	re := map[string]string{
		"main.ts": "import { a } from \"./b\";\nfunction main(): i32 { return a(); }\n",
		"b.ts":    "export type { m } from \"./main\";\nexport function a(): i32 { return 1; }\n",
	}
	res = LowerProgram("main.ts", re)
	if res.Refused {
		t.Fatalf("export-type edge must not fuse a cycle, got:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
}

func TestLowerProgramUsageErasedEdge(t *testing.T) {
	// Value imports used only in type positions carry no runtime edge
	// (planck Shape<->Distance shape): the cycle evaporates.
	files := map[string]string{
		"main.ts": "import { run } from \"./a\";\nfunction main(): i32 { return run(); }\n",
		"a.ts":    "import { Thing } from \"./b\";\nexport function run(): i32 { return 1; }\nexport interface Box { t: Thing; }\n",
		"b.ts":    "import { Tool } from \"./a\";\nexport function make(): i32 { return 2; }\nexport interface Kit { t: Tool; }\n",
	}
	res := LowerProgram("main.ts", files)
	if res.Refused {
		t.Fatalf("type-only-used imports must not fuse a cycle, got:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
	if !strings.Contains(res.SAI, "call @a__run()") {
		t.Errorf("missing linked call:\n%s", res.SAI)
	}
	// A real value use keeps the cycle refusal loud.
	live := map[string]string{
		"main.ts": "import { run } from \"./a\";\nfunction main(): i32 { return run(); }\n",
		"a.ts":    "import { make } from \"./b\";\nexport function run(): i32 { return make(); }\n",
		"b.ts":    "import { run } from \"./a\";\nexport function make(): i32 { return run(); }\n",
	}
	r := LowerProgram("main.ts", live)
	if !r.Refused {
		t.Fatalf("expected value-use cycle refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(strings.Join(r.Diagnostics, "\n"), "import cycle") {
		t.Errorf("missing cycle chain:\n%s", strings.Join(r.Diagnostics, "\n"))
	}
}
