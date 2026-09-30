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

// Annotation-only imports keep a layouts-only edge: the target joins
// prescan maps and the checker set (field accesses resolve), but never
// fuses cycles, links code, or binds values.
func TestLowerProgramTypeEdgeLayouts(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { P } from \"./m\";\nfunction f(v: P): i32 {\n  return v.x + v.y;\n}\nfunction main(): i32 {\n  return 0;\n}\n",
		"m.ts":    "export interface P { x: i32; y: i32 }\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "as i32") {
		t.Errorf("want field loads through the shared layout, got:\n%s", res.SAI)
	}
	for _, d := range res.Diagnostics {
		if strings.Contains(d, "not exported") || strings.Contains(d, "not lowerable") {
			t.Errorf("type-edge import must lower cleanly, got: %s", d)
		}
	}
	// Mutual annotation-only imports resolve layouts both ways without
	// fusing a cycle.
	cycle := map[string]string{
		"main.ts": "import { run } from \"./a\";\nfunction main(): i32 { return run(); }\n",
		"a.ts":    "import { Thing } from \"./b\";\nexport function run(): i32 { return 1; }\nexport interface Box { t: Thing; }\n",
		"b.ts":    "import { Tool } from \"./a\";\nexport function make(): i32 { return 2; }\nexport interface Kit { t: Tool; }\n",
	}
	mustLowerProgram(t, "main.ts", cycle)
	// Explicit `import type` stays fully erased (no layouts either).
	explicit := map[string]string{
		"main.ts": "import type { P } from \"./m\";\nfunction f(v: P): i32 {\n  return v.x;\n}\nfunction main(): i32 {\n  return 0;\n}\n",
		"m.ts":    "export interface P { x: i32; y: i32 }\n",
	}
	if r := LowerProgram("main.ts", explicit); !r.Refused {
		t.Fatalf("expected explicit-import-type refusal, got:\n%s", r.SAI)
	}
}

// Cross-file heritage resolves through type edges: the base joins the
// shared maps via the fixpoint prescan even when no value edge exists.
func TestLowerProgramTypeEdgeHeritage(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { C } from \"./c\";\nfunction f(v: C): i32 {\n  return v.x + v.y;\n}\nfunction main(): i32 {\n  return 0;\n}\n",
		"c.ts":    "import { B } from \"./b\";\nexport class C extends B {\n  y: i32 = 0;\n  constructor() {\n    super();\n    this.y = 1;\n  }\n}\n",
		"b.ts":    "export class B {\n  x: i32 = 0;\n  constructor() {\n  }\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	for _, want := range []string{"as i32"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in linked output:\n%s", want, res.SAI)
		}
	}
}
