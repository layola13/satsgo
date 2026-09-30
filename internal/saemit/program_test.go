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

func TestLowerProgramUnresolvedDeps(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { x } from \"lodash-es\";\nimport { y } from \"./u\";\nfunction main(): i32 { return y(1); }\n",
		"u.ts":    "export function y(a: i32): i32 { return a; }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected refusal on bare import, got:\n%s", res.SAI)
	}
	if len(res.Unresolved) != 1 || res.Unresolved[0] != "lodash-es" {
		t.Errorf("missing unresolved dep aggregate: %v", res.Unresolved)
	}
}

func TestLowerProgramDtsPairing(t *testing.T) {
	files := map[string]string{
		"main.ts":   "import { add } from \"./util.js\";\nfunction main(): i32 {\n  return add(20, 22);\n}\n",
		"util.js":   "export function add(a, b) {\n  return a + b;\n}\n",
		"util.d.ts": "export function add(a: i32, b: i32): i32;\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "call @util__add(20, 22)") {
		t.Errorf("missing paired call:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "@util__add(a: i32, b: i32) -> i32:") {
		t.Errorf("missing paired signature:\n%s", res.SAI)
	}
}

func TestLowerProgramReExport(t *testing.T) {
	named := map[string]string{
		"main.ts": "import { add } from \"./idx\";\nfunction main(): i32 {\n  return add(1, 2);\n}\n",
		"idx.ts":  "export { add } from \"./util\";\n",
		"util.ts": "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", named)
	if !strings.Contains(res.SAI, "call @util__add(1, 2)") {
		t.Errorf("missing through-re-export call:\n%s", res.SAI)
	}
	star := map[string]string{
		"main.ts": "import { add, sub } from \"./idx\";\nfunction main(): i32 {\n  return add(1, 2) + sub(5, 1);\n}\n",
		"idx.ts":  "export * from \"./util\";\n",
		"util.ts": "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\nexport function sub(a: i32, b: i32): i32 {\n  return a - b;\n}\n",
	}
	res = mustLowerProgram(t, "main.ts", star)
	for _, want := range []string{"call @util__add(1, 2)", "call @util__sub(5, 1)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	cyc := map[string]string{
		"main.ts": "import { a } from \"./x\";\nfunction main(): i32 {\n  return a();\n}\n",
		"x.ts":    "export { a } from \"./y\";\n",
		"y.ts":    "export { a } from \"./x\";\n",
	}
	r := LowerProgram("main.ts", cyc)
	if !r.Refused {
		t.Fatalf("expected re-export cycle refusal, got:\n%s", r.SAI)
	}
}

func TestLowerProgramCycleRefuses(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { a } from \"./b\";\nfunction main(): i32 { return a(); }\n",
		"b.ts":    "import { main } from \"./main\";\nexport function a(): i32 { return main(); }\n",
	}
	res := LowerProgram("main.ts", files)
	if !res.Refused {
		t.Fatalf("expected cycle refusal, got:\n%s", res.SAI)
	}
	if !strings.Contains(strings.Join(res.Diagnostics, "\n"), "import cycle") {
		t.Errorf("missing cycle chain:\n%s", strings.Join(res.Diagnostics, "\n"))
	}
}

func TestLowerProgramDefaultObject(t *testing.T) {
	// `export default {a, b: c}` binds per-member; D.m() routes.
	files := map[string]string{
		"main.ts":  "import T from \"./timer\";\nfunction main(): i32 { return T.now() + T.diff(3); }\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport function diff(t: i32): i32 { return t - 1; }\nexport default { now, diff };\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	for _, want := range []string{"call @timer__now()", "call @timer__diff(3)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Renamed member routes to the local.
	ren := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 { return T.tick(); }\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport default { tick: now };\n",
	}
	res = mustLowerProgram(t, "main.ts", ren)
	if !strings.Contains(res.SAI, "call @timer__now()") {
		t.Errorf("missing renamed call:\n%s", res.SAI)
	}
	// Unknown member refuses loudly.
	bad := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 { return T.bogus(); }\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport default { now };\n",
	}
	if r := LowerProgram("main.ts", bad); !r.Refused {
		t.Fatalf("expected unknown-member refusal, got:\n%s", r.SAI)
	}
	// Method/spread values refuse at export scan (timer linked via use).
	meth := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 { return T.m(); }\n",
		"timer.ts": "export default { m() { return 1; } };\n",
	}
	if r := LowerProgram("main.ts", meth); !r.Refused {
		t.Fatalf("expected method-value refusal, got:\n%s", r.SAI)
	}
}

func TestLowerProgramDefaultPassthrough(t *testing.T) {
	// `import * as ns` + `export default ns` exposes the target surface
	// through the default (planck main.ts shape).
	files := map[string]string{
		"lib.ts":   "export function add(a: i32, b: i32): i32 { return a + b; }\n",
		"mid.ts":   "import * as lib from \"./lib\";\nexport default lib;\n",
		"entry.ts": "import * as lib from \"./lib\";\nimport D from \"./mid\";\nfunction main(): i32 { return lib.add(1, 2) + D.add(3, 4); }\n",
	}
	res := mustLowerProgram(t, "entry.ts", files)
	for _, want := range []string{"call @lib__add(1, 2)", "call @lib__add(3, 4)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerProgramNsDestructure(t *testing.T) {
	// `const {a, b: c} = NS` binds members straight to qualified callees.
	files := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 {\n  const { now, diff: delta } = T;\n  return now() + delta(3);\n}\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport function diff(t: i32): i32 { return t - 1; }\nexport default { now, diff };\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	for _, want := range []string{"call @timer__now()", "call @timer__diff(3)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Namespace imports destructure the same way.
	ns := map[string]string{
		"main.ts": "import * as u from \"./util\";\nfunction main(): i32 {\n  const { add } = u;\n  return add(1, 2);\n}\n",
		"util.ts": "export function add(a: i32, b: i32): i32 { return a + b; }\n",
	}
	res = mustLowerProgram(t, "main.ts", ns)
	if !strings.Contains(res.SAI, "call @util__add(1, 2)") {
		t.Errorf("missing ns call:\n%s", res.SAI)
	}
	// Unknown members refuse loudly.
	bad := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 {\n  const { bogus } = T;\n  return 1;\n}\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport default { now };\n",
	}
	if r := LowerProgram("main.ts", bad); !r.Refused {
		t.Fatalf("expected unknown-member refusal, got:\n%s", r.SAI)
	}
	// Defaults refuse loudly.
	def := map[string]string{
		"main.ts": "import T from \"./timer\";\nfunction main(): i32 {\n  const { now = 1 } = T;\n  return 1;\n}\n",
		"timer.ts": "export function now(): i32 { return 7; }\nexport default { now };\n",
	}
	if r := LowerProgram("main.ts", def); !r.Refused {
		t.Fatalf("expected default-value refusal, got:\n%s", r.SAI)
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

func TestLowerProgramDefaultImport(t *testing.T) {
	files := map[string]string{
		"main.ts": "import d from \"./util\";\nfunction main(): i32 { return d(1); }\n",
		"util.ts": "export default function d(x: i32): i32 { return x; }\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "call @util__d(1)") {
		t.Errorf("missing default-import call:\n%s", res.SAI)
	}
	assign := map[string]string{
		"main.ts": "import d from \"./util\";\nfunction main(): i32 { return d(1); }\n",
		"util.ts": "function d(x: i32): i32 { return x; }\nexport default d;\n",
	}
	res = mustLowerProgram(t, "main.ts", assign)
	if !strings.Contains(res.SAI, "call @util__d(1)") {
		t.Errorf("missing export-assignment default call:\n%s", res.SAI)
	}
	none := map[string]string{
		"main.ts": "import d from \"./util\";\nfunction main(): i32 { return d(1); }\n",
		"util.ts": "export function e(x: i32): i32 { return x; }\n",
	}
	r := LowerProgram("main.ts", none)
	if !r.Refused {
		t.Fatalf("expected no-default refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(strings.Join(r.Diagnostics, "\n"), "no default export") {
		t.Errorf("missing no-default message:\n%s", strings.Join(r.Diagnostics, "\n"))
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

// Star/named re-exports bind no local: same-file calls refuse loudly
// (previously emitted the own prefix, failing only at `sa check`).
func TestLowerProgramReexpSameFileRefuses(t *testing.T) {
	star := map[string]string{
		"main.ts": "import { callAdd } from \"./mid\";\nfunction main(): i32 {\n  return callAdd();\n}\n",
		"mid.ts":  "export * from \"./lib\";\nexport function callAdd(): i32 {\n  return add(1, 2);\n}\n",
		"lib.ts":  "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\n",
	}
	r := LowerProgram("main.ts", star)
	if !r.Refused {
		t.Fatalf("expected star same-file refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(strings.Join(r.Diagnostics, "\n"), "import it first") {
		t.Errorf("missing import-it-first diagnostic: %v", r.Diagnostics)
	}
	named := map[string]string{
		"main.ts": "import { callY } from \"./mid\";\nfunction main(): i32 {\n  return callY();\n}\n",
		"mid.ts":  "export { y } from \"./u\";\nexport function callY(): i32 {\n  return y(1);\n}\n",
		"u.ts":    "export function y(a: i32): i32 {\n  return a;\n}\n",
	}
	r = LowerProgram("main.ts", named)
	if !r.Refused {
		t.Fatalf("expected named-reexport same-file refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(strings.Join(r.Diagnostics, "\n"), "import it first") {
		t.Errorf("missing import-it-first diagnostic: %v", r.Diagnostics)
	}
}

// Cross-file misses route to the defining file (kind-aware): importable
// kinds point at the import, the rest name their gap honestly.
func TestLinkRouteMisses(t *testing.T) {
	lib := "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\nexport const K = 7;\nexport class C {\n  v: i32 = 0;\n  constructor(n: i32) {\n    this.v = n;\n  }\n}\nnamespace N {\n  export function f(): i32 {\n    return 1;\n  }\n}\n"
	cases := []struct {
		name string
		main string
		want string
	}{
		{"call", "function main(): i32 {\n  return add(1, 2);\n}\n", "add is defined in lib.ts; import it first"},
		{"const read", "function main(): i32 {\n  return K;\n}\n", "K is defined in lib.ts, but cross-file value imports are not lowerable yet"},
		{"new", "function main(): i32 {\n  const c = new C(3);\n  return c.v;\n}\n", "C is defined in lib.ts; import it first"},
		{"extends", "class D extends C {\n}\nfunction main(): i32 {\n  return 0;\n}\n", "C is defined in lib.ts; import it first"},
		{"ns call", "function main(): i32 {\n  return N.f();\n}\n", "N is a namespace defined in lib.ts; cross-file namespace member access is not lowerable yet"},
		{"private const", "function main(): i32 {\n  return h();\n}\n", "h is defined in lib.ts but not exported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"main.ts": tc.main, "lib.ts": lib}
			if tc.name == "private const" {
				files["lib.ts"] = "function h(): i32 {\n  return 1;\n}\n"
			}
			res := LowerProgram("main.ts", files)
			if !res.Refused {
				t.Fatalf("expected refusal, got:\n%s", res.SAI)
			}
			got := strings.Join(res.Diagnostics, "\n")
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing %q in diagnostics:\n%s", tc.want, got)
			}
		})
	}
	// Importable kinds turn positive end to end (check-clean).
	okFiles := map[string]string{
		"main.ts": "import { add } from \"./lib\";\nimport { C } from \"./lib\";\nfunction main(): i32 {\n  const c = new C(3);\n  return add(1, 2) + c.v;\n}\n",
		"lib.ts":  lib,
	}
	res := mustLowerProgram(t, "main.ts", okFiles)
	// Calls route qualified; `new` inlines the cross-file layout.
	for _, want := range []string{"call @lib__add(1, 2)", "alloc 4"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in linked output:\n%s", want, res.SAI)
		}
	}
}

// Importing from a file with namespace functions lowers (no
// self-collision, no duplicated diagnostics).
func TestLinkNsFileImport(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { add } from \"./lib\";\nfunction main(): i32 {\n  return add(1, 2);\n}\n",
		"lib.ts":  "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\nnamespace N {\n  export function f(): i32 {\n    return 1;\n  }\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "call @lib__add(1, 2)") {
		t.Errorf("missing qualified call in linked output:\n%s", res.SAI)
	}
	for _, d := range res.Diagnostics {
		if strings.Contains(d, "collides with an existing definition") {
			t.Errorf("spurious self-collision diagnostic: %s", d)
		}
	}
	// A genuine same-name definition still refuses (not the seed).
	bad := map[string]string{
		"main.ts": "import { add } from \"./lib\";\nfunction main(): i32 {\n  return add(1, 2);\n}\n",
		"lib.ts":  "export function add(a: i32, b: i32): i32 {\n  return a + b;\n}\nnamespace N {\n  export function f(): i32 {\n    return 1;\n  }\n}\nfunction N_f(): i32 {\n  return 2;\n}\n",
	}
	r := LowerProgram("main.ts", bad)
	if !r.Refused {
		t.Fatalf("expected genuine-collision refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(strings.Join(r.Diagnostics, "\n"), "collides with an existing definition") {
		t.Errorf("missing collision diagnostic: %v", r.Diagnostics)
	}
}

// A local binding shadows an import of the same name: the emitter scope
// stack (not binder visibility, which sees the import) is authoritative
// for lowering-time routing, so the param wins loudly-clean. Locks the
// todo/02#7 boundary: local-shadow checks must not migrate to declaredAt
// (which answers source-scope, a different question).
func TestLowerProgramLocalShadowsImport(t *testing.T) {
	files := map[string]string{
		"main.ts": "import { f } from \"./u\";\nfunction g(f: i32): i32 {\n  return f + 1;\n}\nfunction main(): i32 {\n  return g(41);\n}\n",
		"u.ts":    "export function f(): i32 {\n  return 7;\n}\n",
	}
	res := mustLowerProgram(t, "main.ts", files)
	if !strings.Contains(res.SAI, "add f, 1") {
		t.Errorf("local param must win over the import, got:\n%s", res.SAI)
	}
	if strings.Contains(res.SAI, "call @u__f") {
		t.Errorf("shadowed import must not be called, got:\n%s", res.SAI)
	}
}
