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

func TestLowerMapSet(t *testing.T) {
	mapSrc := "function main(): i32 {\n  const m = new Map();\n  m.set(1, 100);\n  m.set(\"k\", 7);\n  const v = m.get(1);\n  const h = m.has(2);\n  const d = m.delete(2);\n  return v + h + d;\n}\n"
	res := mustLower(t, "m.ts", mapSrc)
	for _, want := range []string{
		"call @sa_btree_map_insert",
		"call @sa_btree_map_get",
		"call @sa_btree_map_contains_key",
		"call @sa_btree_map_remove",
		"@import \"sa_std/btree_map.sa\"",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	setSrc := "function main(): i32 {\n  const s = new Set();\n  s.add(7);\n  return s.has(7) + s.size();\n}\n"
	res = mustLower(t, "s.ts", setSrc)
	for _, want := range []string{
		"call @sa_btree_set_insert",
		"call @sa_btree_set_contains",
		"call @sa_btree_set_len",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerArity(t *testing.T) {
	many := "function f(a: i32): i32 {\n  return a;\n}\nfunction main(): i32 {\n  return f(1, 2);\n}\n"
	res := Lower("ar.ts", many)
	if !res.Refused {
		t.Fatalf("expected too-many refusal, got:\n%s", res.SAI)
	}
	if !strings.Contains(diagText(res), "too many arguments") {
		t.Errorf("missing arity message:\n%s", diagText(res))
	}
	few := "function f(a: i32, b: i32): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res = Lower("ar2.ts", few)
	if !res.Refused {
		t.Fatalf("expected too-few refusal, got:\n%s", res.SAI)
	}
	def := "function f(a: i32, b: i32 = 5): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	res = mustLower(t, "ar3.ts", def)
	if !strings.Contains(res.SAI, "call @f(1)") {
		t.Errorf("missing short call:\n%s", res.SAI)
	}
}

func TestLowerMathExtra(t *testing.T) {
	src := "function main(): i32 {\n  const a = Math.sqrt(16);\n  const b = Math.log10(100);\n  const c = Math.random();\n  return a + b + c;\n}\n"
	res := mustLower(t, "mx.ts", src)
	for _, want := range []string{"L_sqrt_top", "L_l10_top", "1103515245", "32767"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	fl := "function main(): i32 {\n  const x: number = 2.5;\n  return Math.sqrt(x);\n}\n"
	r2 := Lower("mx2.ts", fl)
	if !r2.Refused {
		t.Fatalf("expected float-sqrt refusal, got:\n%s", r2.SAI)
	}
}

func TestLowerFindLastFrom(t *testing.T) {
	fl := "function main(): i32 {\n  const a: number[] = [1, 2, 3, 2, 1];\n  return a.findLast((x) => x == 2) * 10 + a.findLastIndex((x) => x == 2);\n}\n"
	res := mustLower(t, "fl.ts", fl)
	if !strings.Contains(res.SAI, "L_fl_top") {
		t.Errorf("missing findLast loop:\n%s", res.SAI)
	}
	afm := "function main(): i32 {\n  const a: number[] = [1, 2, 3];\n  const b = Array.from(a, (x) => x * 2);\n  return b[2];\n}\n"
	res = mustLower(t, "afm.ts", afm)
	if !strings.Contains(res.SAI, "L_mp_top") {
		t.Errorf("missing inlined map loop:\n%s", res.SAI)
	}
}

func TestLowerNodeOs(t *testing.T) {
	src := "import { platform, arch } from \"os\";\nfunction main(): i32 {\n  const p = platform();\n  return p.length;\n}\n"
	res := mustLower(t, "n.ts", src)
	for _, want := range []string{
		"@import \"node.sai\"",
		"call @sa_node_plugin_os_platform",
		"panic",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	nodeArch := "import { arch } from \"node:os\";\nfunction main(): i32 {\n  return arch().length;\n}\n"
	res = mustLower(t, "n2.ts", nodeArch)
	if !strings.Contains(res.SAI, "call @sa_node_plugin_os_arch") {
		t.Errorf("missing node:os arch call:\n%s", res.SAI)
	}
	nodeOs := "import { homedir, tmpdir, hostname } from \"os\";\nfunction main(): i32 {\n  return homedir().length + tmpdir().length + hostname().length;\n}\n"
	res = mustLower(t, "n3.ts", nodeOs)
	for _, want := range []string{
		"call @sa_node_plugin_os_homedir",
		"call @sa_node_plugin_os_tmpdir",
		"call @sa_node_plugin_os_hostname",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	nodeGlobals := "function main(): i32 {\n  const c = process.cwd();\n  const u = crypto.randomUUID();\n  return c.length + u.length;\n}\n"
	res = mustLower(t, "n4.ts", nodeGlobals)
	for _, want := range []string{
		"call @sa_node_plugin_process_cwd",
		"call @sa_node_plugin_crypto_random_uuid",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerTopLevelConst(t *testing.T) {
	src := "var K = 42;\nvar S = \"hi\";\nvar nativeMax = Math.max;\nfunction main(): i32 {\n  return K + S.length + nativeMax(3, 8);\n}\n"
	res := mustLower(t, "tc.ts", src)
	for _, want := range []string{"add 42,", "@const str_const_"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	bad := "var r = require(\"x\");\nfunction main(): i32 {\n  return 1;\n}\n"
	r := Lower("tc2.ts", bad)
	if !r.Refused {
		t.Fatalf("expected effectful top-level refusal, got:\n%s", r.SAI)
	}
}

func TestLowerNullArray(t *testing.T) {
	src := "function f(x: i32 | null): i32 {\n  if (x == null) {\n    return 0;\n  }\n  return x;\n}\nfunction main(): i32 {\n  const a = Array(3);\n  const b = Array(1, 2);\n  const u = undefined;\n  return f(null) + a.length + b[1] + u;\n}\n"
	res := mustLower(t, "nv.ts", src)
	for _, want := range []string{"eq x, 0", "call @f(0)"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerFromCharCodeUnwrap(t *testing.T) {
	src := "function main(): i32 {\n  const s = String.fromCharCode(65);\n  return s.length;\n}\n"
	res := mustLower(t, "fcc.ts", src)
	// Buffer-handle protocol: data/len unwrap, never a direct slice read.
	for _, want := range []string{
		"call @sa_string_from_char_code",
		"call @sa_fmt_buffer_data",
		"call @sa_fmt_buffer_len",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerLengthFieldPriority(t *testing.T) {
	src := "interface Header {\n  kind: i32;\n  length: i32;\n}\nfunction main(): i32 {\n  const h: Header = { kind: 1, length: 64 };\n  return h.length;\n}\n"
	res := mustLower(t, "hdr.ts", src)
	if !strings.Contains(res.SAI, "load h + 4 as i32") {
		t.Errorf("struct length field must win over slice-len alias:\n%s", res.SAI)
	}
}

func TestLowerTypeofStringEq(t *testing.T) {
	src := "function fn(a: i32): i32 {\n  return a;\n}\nfunction main(): i32 {\n  const s: string = \"hi\";\n  let r: i32 = 0;\n  if (typeof fn == \"function\") { r = r + 1; }\n  if (typeof s == \"string\") { r = r + 2; }\n  if (s == \"hi\") { r = r + 4; }\n  if (s != \"yo\") { r = r + 8; }\n  return r;\n}\n"
	res := mustLower(t, "te.ts", src)
	// Content equality via indexOf (never bare address compare).
	if !strings.Contains(res.SAI, "call @sa_string_index_of") {
		t.Errorf("missing content equality:\n%s", res.SAI)
	}
	unk := "function main(): i32 {\n  if (typeof self == \"object\") { return 1; }\n  return 0;\n}\n"
	r := Lower("te2.ts", unk)
	if !r.Refused {
		t.Fatalf("expected unknown-global refusal, got:\n%s", r.SAI)
	}
}

func TestLowerFsReadUnwrap(t *testing.T) {
	src := "import { readFile } from \"fs\";\nfunction main(): i32 {\n  const d: string = readFile(\"/tmp/x.txt\");\n  return d.length;\n}\n"
	res := mustLower(t, "fs.ts", src)
	// Buffer-handle protocol: payload at +8, then data/len unwrap.
	out := res.SAI
	for _, want := range []string{
		"call @sa_fs_read_buffer_data",
		"call @sa_fs_read_buffer_len",
		"+ 8 as u64",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestLowerInDelete(t *testing.T) {
	src := "interface Cfg {\n  path: string;\n  retries: i32;\n}\nfunction main(): i32 {\n  const c: Cfg = { path: `x`, retries: 2 };\n  let r: i32 = 0;\n  if (\"retries\" in c) { r = r + 1; }\n  if (\"nope\" in c) { r = r + 10; }\n  return r;\n}\n"
	res := mustLower(t, "in.ts", src)
	if strings.Count(res.SAI, "br ") != 2 {
		t.Errorf("expected folded branches:\n%s", res.SAI)
	}
	del := "interface Cfg {\n  path: string;\n}\nfunction main(): i32 {\n  const c: Cfg = { path: `x` };\n  delete c.path;\n  return 0;\n}\n"
	r := Lower("del.ts", del)
	if !r.Refused {
		t.Fatalf("expected delete refusal, got:\n%s", r.SAI)
	}
}

func TestLowerCallDesugar(t *testing.T) {
	src := "function add(self: i32, x: i32): i32 {\n  return self + x;\n}\nfunction main(): i32 {\n  return add.call(20, 22);\n}\n"
	res := mustLower(t, "cc.ts", src)
	if !strings.Contains(res.SAI, "call @add(20, 22)") {
		t.Errorf("missing desugared call:\n%s", res.SAI)
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
			if p.Backend == "" {
				t.Errorf("module must live under sa_std/ or declare a plugin Backend: %+v", p)
			}
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
