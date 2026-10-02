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

func TestLowerClassStaticCall(t *testing.T) {
	src := "class B {\n  static add(a: i32, b: i32): i32 {\n    return a + b;\n  }\n}\nfunction main(): i32 {\n  return B.add(20, 22);\n}\n"
	res := mustLower(t, "st1.ts", src)
	if strings.Contains(res.SAI, "call @B") {
		t.Errorf("static call must inline, not call:\n%s", res.SAI)
	}
	arity := "class B {\n  static add(a: i32, b: i32): i32 {\n    return a + b;\n  }\n}\nfunction main(): i32 {\n  return B.add(1);\n}\n"
	if r := Lower("st2.ts", arity); !r.Refused {
		t.Fatalf("expected static arity refusal, got:\n%s", r.SAI)
	}
	inh := "class Base {\n  static one(): i32 {\n    return 1;\n  }\n}\nclass Sub extends Base {\n}\nfunction main(): i32 {\n  return Sub.one();\n}\n"
	res = mustLower(t, "st3.ts", inh)
	if !strings.Contains(res.SAI, "L_m_end") {
		t.Errorf("missing inherited static inline join:\n%s", res.SAI)
	}
	if strings.Contains(res.SAI, "call @") {
		t.Errorf("inherited static call must inline, not call:\n%s", res.SAI)
	}
	unk := "class B {\n  static add(a: i32): i32 {\n    return a;\n  }\n}\nfunction main(): i32 {\n  return B.nope(1);\n}\n"
	if r := Lower("st4.ts", unk); !r.Refused {
		t.Fatalf("expected unknown-static refusal, got:\n%s", r.SAI)
	}
	thisUse := "class B {\n  v: i32 = 0;\n  static f(): i32 {\n    return this.v;\n  }\n}\nfunction main(): i32 {\n  return B.f();\n}\n"
	if r := Lower("st5.ts", thisUse); !r.Refused {
		t.Fatalf("expected this-in-static refusal, got:\n%s", r.SAI)
	}
}

func TestLowerClassAccessor(t *testing.T) {
	src := "class C {\n  _v: i32 = 0;\n  get v(): i32 {\n    return this._v;\n  }\n  set v(n: i32) {\n    this._v = n;\n  }\n}\nfunction main(): i32 {\n  const c = new C();\n  c.v = 41;\n  return c.v;\n}\n"
	res := mustLower(t, "ac1.ts", src)
	if !strings.Contains(res.SAI, "L_m_end") {
		t.Errorf("missing inlined accessor body:\n%s", res.SAI)
	}
	sg := "class C {\n  static _w: i32 = 7;\n  static get w(): i32 {\n    return 8;\n  }\n  static set w(n: i32) {\n  }\n}\nfunction main(): i32 {\n  C.w = 1;\n  return C.w;\n}\n"
	res = mustLower(t, "ac2.ts", sg)
	if !strings.Contains(res.SAI, "L_m_end") {
		t.Errorf("missing inlined static accessor body:\n%s", res.SAI)
	}
	bareInst := "class C {\n  _v: i32 = 0;\n  get v(): i32 {\n    return this._v;\n  }\n}\nfunction main(): i32 {\n  return C.v;\n}\n"
	if r := Lower("ac3.ts", bareInst); !r.Refused {
		t.Fatalf("expected bare-class instance-getter refusal, got:\n%s", r.SAI)
	}
	bodiless := "class C {\n  m(x: string): i32;\n  m(x: number): i32;\n}\nfunction main(): i32 {\n  const c = new C();\n  return c.m(1);\n}\n"
	if r := Lower("ac4.ts", bodiless); !r.Refused {
		t.Fatalf("expected bodiless-method refusal, got:\n%s", r.SAI)
	}
}

func TestLowerDefaultReplay(t *testing.T) {
	src := "function g(a: i32, b: i32 = 2): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  return g(1) + g(10, 20);\n}\n"
	res := mustLower(t, "dr1.ts", src)
	if !strings.Contains(res.SAI, "call @g(1, 2)") {
		t.Errorf("missing replayed default call:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "call @g(10, 20)") {
		t.Errorf("missing full call:\n%s", res.SAI)
	}
	nonlit := "const d: i32 = 2;\nfunction g(a: i32, b: i32 = d): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  return g(1);\n}\n"
	if r := Lower("dr2.ts", nonlit); !r.Refused {
		t.Fatalf("expected non-literal-default refusal, got:\n%s", r.SAI)
	}
	nodef := "function f(a: i32, b: i32): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  return f(1);\n}\n"
	if r := Lower("dr3.ts", nodef); !r.Refused {
		t.Fatalf("expected too-few refusal, got:\n%s", r.SAI)
	}
	str := "function h(a: string, b: string = \"hi\"): i32 {\n  return a.length + b.length;\n}\nfunction main(): i32 {\n  return h(\"ab\");\n}\n"
	res = mustLower(t, "dr4.ts", str)
	if !strings.Contains(res.SAI, "call @h(") {
		t.Errorf("missing padded string call:\n%s", res.SAI)
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
	if !strings.Contains(res.SAI, "call @f(1, 5)") {
		t.Errorf("missing replayed default call:\n%s", res.SAI)
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

func TestMathAliasValue(t *testing.T) {
	// Alias value-uses resolve through the projection table exactly
	// like direct Math uses: const folds to the literal, inline targets
	// refuse loudly. Before, `2 * math_PI` emitted `mul 2, math_PI`
	// (undeclared register, silent check-trap).
	alias := "const math_PI = Math.PI;\nfunction main(): i32 {\n  return 2 * math_PI;\n}\n"
	direct := "function main(): i32 {\n  return 2 * Math.PI;\n}\n"
	ra := mustLower(t, "mav1.ts", alias)
	rd := mustLower(t, "mav2.ts", direct)
	if ra.SAI != rd.SAI {
		t.Errorf("alias and direct must lower identically:\n--- alias:\n%s\n--- direct:\n%s", ra.SAI, rd.SAI)
	}
	if !strings.Contains(ra.SAI, "mul 2, 3") {
		t.Errorf("want folded mul 2, 3, got:\n%s", ra.SAI)
	}
	// Function projections have no first-class value (call them).
	fn := "const math_abs = Math.abs;\nfunction main(): i32 {\n  const f = math_abs;\n  return f(1);\n}\n"
	r := Lower("mav3.ts", fn)
	if !r.Refused {
		t.Fatalf("expected function-value refusal, got:\n%s", r.SAI)
	}
	if !strings.Contains(diagText(r), "as a value is not lowerable") {
		t.Errorf("missing value diagnostic: %v", r.Diagnostics)
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
	nodeOsBatch2 := "import { release, type, endianness, machine, cpus } from \"os\";\nfunction main(): i32 {\n  return release().length + type().length + endianness().length + machine().length + cpus().length;\n}\n"
	res = mustLower(t, "n3b.ts", nodeOsBatch2)
	for _, want := range []string{
		"call @sa_node_plugin_os_release",
		"call @sa_node_plugin_os_type",
		"call @sa_node_plugin_os_endianness",
		"call @sa_node_plugin_os_machine",
		"call @sa_node_plugin_os_cpus",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	nodeOsBatch3 := "import { version, userInfo, networkInterfaces } from \"os\";\nfunction main(): i32 {\n  return version().length + userInfo().length + networkInterfaces().length;\n}\n"
	res = mustLower(t, "n3c.ts", nodeOsBatch3)
	for _, want := range []string{
		"call @sa_node_plugin_os_version",
		"call @sa_node_plugin_os_user_info",
		"call @sa_node_plugin_os_network_interfaces",
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

func TestLowerNodePath(t *testing.T) {
	src := "import { normalize, dirname, extname } from \"path\";\nfunction main(): i32 {\n  const n: string = normalize(\"/a//b/../c\");\n  const d: string = dirname(\"/a/b/c.txt\");\n  const e: string = extname(\"c.txt\");\n  return n.length + d.length + e.length;\n}\n"
	res := mustLower(t, "p1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_path_normalize",
		"call @sa_node_plugin_path_dirname",
		"call @sa_node_plugin_path_extname",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	bad := "import { normalize } from \"path\";\nfunction main(): i32 {\n  const n: string = normalize();\n  return n.length;\n}\n"
	r := Lower("p2.ts", bad)
	if !r.Refused {
		t.Fatalf("expected arity refusal, got:\n%s", r.SAI)
	}
	argv := "import { join, resolve } from \"path\";\nfunction main(): i32 {\n  const j: string = join(\"a\", \"b\", \"c\");\n  const r: string = resolve(\"/x\");\n  const e: string = join();\n  return j.length + r.length + e.length;\n}\n"
	res = mustLower(t, "p3.ts", argv)
	for _, want := range []string{
		"call @sa_node_plugin_path_join",
		"call @sa_node_plugin_path_resolve",
		"alloc 48",
		"store ",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	base := "import { basename } from \"path\";\nfunction main(): i32 {\n  const b: string = basename(\"/a/b/c.txt\", \".txt\");\n  return b.length;\n}\n"
	res = mustLower(t, "p4.ts", base)
	if !strings.Contains(res.SAI, "call @sa_node_plugin_path_basename") {
		t.Errorf("missing basename call:\n%s", res.SAI)
	}
	baseArity := "import { basename } from \"path\";\nfunction main(): i32 {\n  const b: string = basename(\"/a/b/c.txt\");\n  return b.length;\n}\n"
	r = Lower("p5.ts", baseArity)
	if !r.Refused {
		t.Fatalf("expected basename 1-arg refusal (ext required), got:\n%s", r.SAI)
	}
	abs := "import { isAbsolute } from \"path\";\nfunction main(): i32 {\n  const b: boolean = isAbsolute(\"/a/b\");\n  if (b) { return 1; }\n  return 0;\n}\n"
	res = mustLower(t, "p6.ts", abs)
	for _, want := range []string{
		"call @sa_node_plugin_path_is_absolute",
		"load ",
		" as i32",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	absArity := "import { isAbsolute } from \"path\";\nfunction main(): i32 {\n  const b: boolean = isAbsolute();\n  return 0;\n}\n"
	r = Lower("p7.ts", absArity)
	if !r.Refused {
		t.Fatalf("expected isAbsolute arity refusal, got:\n%s", r.SAI)
	}
	exists := "import { existsSync } from \"fs\";\nfunction main(): i32 {\n  const b: boolean = existsSync(\"/tmp/x\");\n  if (b) { return 1; }\n  return 0;\n}\n"
	res = mustLower(t, "p8.ts", exists)
	for _, want := range []string{
		"call @sa_node_plugin_fs_exists",
		" as i32",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	existsArity := "import { existsSync } from \"fs\";\nfunction main(): i32 {\n  const b: boolean = existsSync();\n  return 0;\n}\n"
	r = Lower("p9.ts", existsArity)
	if !r.Refused {
		t.Fatalf("expected existsSync arity refusal, got:\n%s", r.SAI)
	}
}

func TestLowerOverloadErasure(t *testing.T) {
	src := "function add(x: string): i32;\nfunction add(x: number): i32;\nfunction add(x: any): i32 {\n  return 1;\n}\nfunction main(): i32 {\n  return add(1);\n}\n"
	res := mustLower(t, "ov1.ts", src)
	if !strings.Contains(res.SAI, "call @add(1)") {
		t.Errorf("missing implementation call:\n%s", res.SAI)
	}
	if strings.Count(res.SAI, "@add(x:") != 1 {
		t.Errorf("overload signatures must erase to one definition:\n%s", res.SAI)
	}
	lone := "function lone(x: string): i32;\nfunction main(): i32 {\n  return lone(1);\n}\n"
	r := Lower("ov2.ts", lone)
	if !r.Refused {
		t.Fatalf("expected lone-signature refusal, got:\n%s", r.SAI)
	}
}

func TestLowerUninitDecl(t *testing.T) {
	src := "function main(): i32 {\n  let x: number;\n  x = 5;\n  return x;\n}\n"
	res := mustLower(t, "u1.ts", src)
	if !strings.Contains(res.SAI, "x = 0") {
		t.Errorf("missing zero placeholder:\n%s", res.SAI)
	}
	if !strings.Contains(res.SAI, "x = 5") {
		t.Errorf("missing rebind:\n%s", res.SAI)
	}
	str := "function main(): i32 {\n  let s: string;\n  s = \"hi\";\n  return s.length;\n}\n"
	res = mustLower(t, "u2.ts", str)
	if !strings.Contains(res.SAI, "s = 0") {
		t.Errorf("missing null placeholder:\n%s", res.SAI)
	}
	bad := "function main(): i32 {\n  const c: number;\n  return c;\n}\n"
	r := Lower("u3.ts", bad)
	if !r.Refused {
		t.Fatalf("expected const-no-init refusal, got:\n%s", r.SAI)
	}
}

func TestLowerDestructuredParams(t *testing.T) {
	src := "interface Point {\n  x: i32;\n  y: i32;\n}\nfunction dsum({x, y}: Point): i32 {\n  return x + y;\n}\nfunction asum([a, b]: i32[]): i32 {\n  return a + b;\n}\nfunction main(): i32 {\n  const pt: Point = { x: 3, y: 4 };\n  const arr: i32[] = [10, 20];\n  return dsum(pt) + asum(arr);\n}\n"
	res := mustLower(t, "dp1.ts", src)
	for _, want := range []string{
		"@dsum(__darg: ptr)",
		"@asum(__darg: ptr)",
		"call @dsum(",
		"call @asum(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	arrow := "interface Point {\n  x: i32;\n  y: i32;\n}\nconst f = ({x, y}: Point): i32 => {\n  return x + y;\n};\nfunction main(): i32 {\n  const pt: Point = { x: 5, y: 6 };\n  return f(pt);\n}\n"
	res = mustLower(t, "dp2.ts", arrow)
	if !strings.Contains(res.SAI, "call @f(") {
		t.Errorf("missing arrow call:\n%s", res.SAI)
	}
	rest := "function f({x, ...r}: any): i32 {\n  return x;\n}\nfunction main(): i32 {\n  return 0;\n}\n"
	r := Lower("dp3.ts", rest)
	if !r.Refused {
		t.Fatalf("expected rest-pattern refusal, got:\n%s", r.SAI)
	}
}

func TestLowerNodePunycode(t *testing.T) {
	src := "import { encode, decode } from \"punycode\";\nfunction main(): i32 {\n  const e: string = encode(\"münchen\");\n  const d: string = decode(\"mnchen-3ya\");\n  return e.length + d.length;\n}\n"
	res := mustLower(t, "pc1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_punycode_encode",
		"call @sa_node_plugin_punycode_decode",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	prefixed := "import { encode } from \"node:punycode\";\nfunction main(): i32 {\n  const e: string = encode(\"münchen\");\n  return e.length;\n}\n"
	res = mustLower(t, "pc2.ts", prefixed)
	if !strings.Contains(res.SAI, "call @sa_node_plugin_punycode_encode") {
		t.Errorf("missing node:punycode encode call:\n%s", res.SAI)
	}
	bad := "import { encode } from \"punycode\";\nfunction main(): i32 {\n  const e: string = encode();\n  return e.length;\n}\n"
	r := Lower("pc3.ts", bad)
	if !r.Refused {
		t.Fatalf("expected punycode arity refusal, got:\n%s", r.SAI)
	}
}

func TestLowerNodeCrypto(t *testing.T) {
	src := "import { randomBytes } from \"crypto\";\nfunction main(): i32 {\n  const b = randomBytes(16);\n  return b.length;\n}\n"
	res := mustLower(t, "c1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_crypto_random_bytes",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	bad := "import { randomBytes } from \"crypto\";\nfunction main(): i32 {\n  const b = randomBytes();\n  return b.length;\n}\n"
	r := Lower("c2.ts", bad)
	if !r.Refused {
		t.Fatalf("expected arity refusal, got:\n%s", r.SAI)
	}
}

func TestLowerNodeHash(t *testing.T) {
	src := "import { createHash } from \"crypto\";\nfunction main(): i32 {\n  const h = createHash(\"sha256\");\n  h.update(\"abc\");\n  h.update(\"def\");\n  const d: string = h.digest();\n  const g = createHash(\"sha256\");\n  g.update(\"abc\");\n  const x: string = g.digest(\"hex\");\n  return d.length + x.length;\n}\n"
	res := mustLower(t, "h1.ts", src)
	for _, want := range []string{
		"call @sa_string_concat",
		"call @sa_node_plugin_crypto_hash",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	badEnc := "import { createHash } from \"crypto\";\nfunction main(): i32 {\n  const h = createHash(\"sha256\");\n  const d: string = h.digest(\"base64\");\n  return d.length;\n}\n"
	if r := Lower("h2.ts", badEnc); !r.Refused {
		t.Fatalf("expected base64 refusal, got:\n%s", r.SAI)
	}
	finalized := "import { createHash } from \"crypto\";\nfunction main(): i32 {\n  const h = createHash(\"sha256\");\n  const a: string = h.digest();\n  const b: string = h.digest();\n  return a.length + b.length;\n}\n"
	if r := Lower("h3.ts", finalized); !r.Refused {
		t.Fatalf("expected double-digest refusal, got:\n%s", r.SAI)
	}
	updArity := "import { createHash } from \"crypto\";\nfunction main(): i32 {\n  const h = createHash(\"sha256\");\n  h.update();\n  return 0;\n}\n"
	if r := Lower("h4.ts", updArity); !r.Refused {
		t.Fatalf("expected update-arity refusal, got:\n%s", r.SAI)
	}
	mac := "import { createHmac } from \"crypto\";\nfunction main(): i32 {\n  const m = createHmac(\"sha256\", \"key\");\n  m.update(\"abc\");\n  const d: string = m.digest(\"hex\");\n  return d.length;\n}\n"
	res = mustLower(t, "m1.ts", mac)
	for _, want := range []string{
		"call @sa_string_concat",
		"call @sa_node_plugin_crypto_hmac",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	macArity := "import { createHmac } from \"crypto\";\nfunction main(): i32 {\n  const m = createHmac(\"sha256\");\n  return 0;\n}\n"
	if r := Lower("m2.ts", macArity); !r.Refused {
		t.Fatalf("expected createHmac-arity refusal, got:\n%s", r.SAI)
	}
	alias := "import { createHash as ch, createHmac as cm } from \"crypto\";\nimport { join as pjoin } from \"path\";\nfunction main(): i32 {\n  const h = ch(\"sha256\");\n  h.update(\"a\");\n  const d: string = h.digest();\n  const m = cm(\"sha256\", \"k\");\n  m.update(\"b\");\n  const e: string = m.digest();\n  const j: string = pjoin(\"x\", \"y\");\n  return d.length + e.length + j.length;\n}\n"
	res = mustLower(t, "m3.ts", alias)
	for _, want := range []string{
		"call @sa_node_plugin_crypto_hash",
		"call @sa_node_plugin_crypto_hmac",
		"call @sa_node_plugin_path_join",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerNodeURL(t *testing.T) {
	src := "import { escape, unescape, parse, stringify } from \"querystring\";\nimport { parse as uparse, format, resolve } from \"url\";\nfunction main(): i32 {\n  const a: string = escape(\"a b\");\n  const b: string = unescape(\"a%20b\");\n  const c: string = parse(\"x=1\");\n  const d: string = stringify(\"{}\");\n  const e: string = uparse(\"http://h/p\");\n  const f: string = format(\"{}\");\n  const g: string = resolve(\"http://h/a\", \"b\");\n  return a.length + b.length + c.length + d.length + e.length + f.length + g.length;\n}\n"
	res := mustLower(t, "u1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_querystring_escape",
		"call @sa_node_plugin_querystring_unescape",
		"call @sa_node_plugin_querystring_parse",
		"call @sa_node_plugin_querystring_stringify",
		"call @sa_node_plugin_url_parse",
		"call @sa_node_plugin_url_format",
		"call @sa_node_plugin_url_resolve",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
}

func TestLowerNodeUtil(t *testing.T) {
	src := "import { stripVTControlCharacters } from \"util\";\nfunction main(): i32 {\n  const s: string = stripVTControlCharacters(\"\\u001b[31mhi\\u001b[0m\");\n  return s.length;\n}\n"
	res := mustLower(t, "t1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_util_strip_vt_control_characters",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Deferred util surfaces refuse loudly (no JSON encoder / bool-out
	// kind / missing plugin symbol yet).
	for _, src := range []string{
		"import { format } from \"util\";\nfunction main(): i32 {\n  const s: string = format(\"%d\", 1);\n  return s.length;\n}\n",
		"import { inspect } from \"util\";\nfunction main(): i32 {\n  const s: string = inspect(\"x\");\n  return s.length;\n}\n",
		"import { isDeepStrictEqual } from \"util\";\nfunction main(): i32 {\n  return isDeepStrictEqual(\"a\", \"a\");\n}\n",
	} {
		if r := Lower("t2.ts", src); !r.Refused {
			t.Fatalf("expected refusal, got:\n%s", r.SAI)
		}
	}
}

func TestLowerDate(t *testing.T) {
	src := "function main(): i64 {\n  const t: i64 = Date.now();\n  const d = new Date();\n  return t + d.getTime();\n}\n"
	res := mustLower(t, "d1.ts", src)
	for _, want := range []string{
		"call @sa_time_unix_ms",
		`@import "sa_std/time.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	for _, src := range []string{
		"function main(): i64 {\n  const d = new Date(\"2024-01-01\");\n  return d.getTime();\n}\n",
	} {
		if r := Lower("d2.ts", src); !r.Refused {
			t.Fatalf("expected refusal, got:\n%s", r.SAI)
		}
	}
	iso := "function main(): string {\n  const d = new Date();\n  return d.toISOString();\n}\n"
	res = mustLower(t, "d3.ts", iso)
	if !strings.Contains(res.SAI, "call @sa_time_iso_from_unix_ms") {
		t.Errorf("missing iso call:\n%s", res.SAI)
	}
	isoArg := "function main(): string {\n  const d = new Date();\n  return d.toISOString(\"x\");\n}\n"
	if r := Lower("d4.ts", isoArg); !r.Refused {
		t.Fatalf("expected iso-arity refusal, got:\n%s", r.SAI)
	}
	prs := "function main(): i64 {\n  return Date.parse(\"2024-01-01\");\n}\n"
	res = mustLower(t, "d5.ts", prs)
	for _, want := range []string{
		"call @sa_time_parse_iso",
		`@import "sa_std/time.sai"`,
		"panic",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	prsArity := "function main(): i64 {\n  return Date.parse();\n}\n"
	if r := Lower("d6.ts", prsArity); !r.Refused {
		t.Fatalf("expected parse-arity refusal, got:\n%s", r.SAI)
	}
	getters := "function main(): i64 {\n  const d = new Date();\n  return d.getFullYear() + d.getMonth() + d.getDate() + d.getHours() + d.getMinutes() + d.getSeconds() + d.getMilliseconds() + d.getDay() + d.getTimezoneOffset();\n}\n"
	res = mustLower(t, "d7.ts", getters)
	for _, want := range []string{
		"call @sa_time_get_full_year",
		"call @sa_time_get_month",
		"call @sa_time_get_date",
		"call @sa_time_get_hours",
		"call @sa_time_get_minutes",
		"call @sa_time_get_seconds",
		"call @sa_time_get_milliseconds",
		"call @sa_time_get_day",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	setters := "function main(): i64 {\n  const d = new Date();\n  const t: i64 = d.setFullYear(2025);\n  d.setMonth(0);\n  d.setDate(1);\n  d.setHours(0);\n  d.setMinutes(0);\n  d.setSeconds(0);\n  d.setMilliseconds(0);\n  return t + d.getTime();\n}\n"
	res = mustLower(t, "d8.ts", setters)
	for _, want := range []string{
		"call @sa_time_set_field",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	setArity := "function main(): i64 {\n  const d = new Date();\n  d.setFullYear();\n  return d.getTime();\n}\n"
	if r := Lower("d9.ts", setArity); !r.Refused {
		t.Fatalf("expected setter-arity refusal, got:\n%s", r.SAI)
	}
	strs := "function main(): i64 {\n  const d = new Date();\n  const a: string = d.toString();\n  const b: string = d.toDateString();\n  const c: string = d.toTimeString();\n  const e: string = d.toUTCString();\n  return a.length + b.length + c.length + e.length;\n}\n"
	res = mustLower(t, "d10.ts", strs)
	for _, want := range []string{
		"call @sa_time_format_utc",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	if c := strings.Count(res.SAI, "call @sa_time_format_utc"); c != 4 {
		t.Errorf("want 4 format calls, got %d:\n%s", c, res.SAI)
	}
	loc := "function main(): string {\n  const d = new Date();\n  return d.toLocaleString();\n}\n"
	if r := Lower("d11.ts", loc); !r.Refused {
		t.Fatalf("expected locale refusal, got:\n%s", r.SAI)
	}
	// toLocale* refuse with the Date-specific diagnostic (not the generic
	// first-class tail).
	for _, m := range []string{"toLocaleString", "toLocaleDateString", "toLocaleTimeString"} {
		src := "function main(): string {\n  const d = new Date();\n  return d." + m + "();\n}\n"
		r := Lower("d12.ts", src)
		if !r.Refused {
			t.Fatalf("expected %s refusal, got:\n%s", m, r.SAI)
		}
		hit := false
		for _, dg := range r.Diagnostics {
			if strings.Contains(dg.Error(), "Date."+m) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("want Date-specific diagnostic for %s, got %v", m, r.Diagnostics)
		}
	}
}

func TestLowerDeno(t *testing.T) {
	src := "function main(): i32 {\n  const h: string = Deno.hostname();\n  const r: string = Deno.osRelease();\n  return h.length + r.length;\n}\n"
	res := mustLower(t, "dn1.ts", src)
	for _, want := range []string{
		"call @sa_deno_plugin_hostname",
		"call @sa_deno_plugin_os_release",
		`@import "deno.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Unknown Deno members refuse loudly (no silent sink).
	bad := "function main(): i32 {\n  const x = Deno.serveHttp();\n  return 1;\n}\n"
	if r := Lower("dn2.ts", bad); !r.Refused {
		t.Fatalf("expected unknown-member refusal, got:\n%s", r.SAI)
	}
	files := "function main(): i32 {\n  const t: string = Deno.readTextFile(\"/tmp/a.txt\");\n  Deno.writeTextFile(\"/tmp/b.txt\", t);\n  return t.length;\n}\n"
	res = mustLower(t, "dn4.ts", files)
	for _, want := range []string{
		"call @sa_deno_plugin_read_text_file",
		"call @sa_deno_plugin_write_text_file",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	envOk := "function main(): i32 {\n  const v: string = Deno.env.get(\"HOME\");\n  Deno.env.set(\"X\", \"1\");\n  Deno.env.delete(\"X\");\n  return v.length;\n}\n"
	res = mustLower(t, "dn5.ts", envOk)
	for _, want := range []string{
		"call @sa_deno_plugin_env_get",
		"call @sa_deno_plugin_env_set",
		"call @sa_deno_plugin_env_delete",
		"panic",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	other := "function main(): i32 {\n  const x = Deno.foo.bar();\n  return 1;\n}\n"
	if r := Lower("dn6.ts", other); !r.Refused {
		t.Fatalf("expected ns refusal, got:\n%s", r.SAI)
	}
	fs := "function main(): i32 {\n  const c: string = Deno.cwd();\n  Deno.chdir(\"/tmp\");\n  Deno.mkdir(\"/tmp/d\");\n  Deno.remove(\"/tmp/d\");\n  const e: string = btoa(\"hi\");\n  const d: string = atob(e);\n  return c.length + d.length;\n}\n"
	res = mustLower(t, "dn7.ts", fs)
	for _, want := range []string{
		"call @sa_deno_plugin_cwd",
		"call @sa_deno_plugin_chdir",
		"call @sa_deno_plugin_mkdir",
		"call @sa_deno_plugin_remove",
		"call @sa_deno_plugin_btoa",
		"call @sa_deno_plugin_atob",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// mkdir takes exactly one path (options objects refuse).
	opts := "function main(): i32 {\n  Deno.mkdir(\"/tmp/a\", \"/tmp/b\");\n  return 1;\n}\n"
	if r := Lower("dn8.ts", opts); !r.Refused {
		t.Fatalf("expected mkdir-arity refusal, got:\n%s", r.SAI)
	}
	// version/build objects stay refused: no direct TS spelling maps to
	// the *_json strings without inventing an object shape (documented;
	// member reads like Deno.version.deno need object materialisation).
	for _, src := range []string{
		"function main(): string {\n  return Deno.version;\n}\n",
		"function main(): string {\n  return Deno.version.deno;\n}\n",
		"function main(): string {\n  return Deno.build.os;\n}\n",
	} {
		if r := Lower("dn7.ts", src); !r.Refused {
			t.Fatalf("expected version refusal, got:\n%s", r.SAI)
		}
	}
}

func TestLowerArrowCaptureSharedWalk(t *testing.T) {
	// todo/02#5: captures come from the shared usage walk, so the type
	// name Box never becomes a capture; only the outer value does.
	src := "interface Box { v: i32 }\nfunction main(): i32 {\n  const base = 10;\n  const f = (x: Box): i32 => x.v + base;\n  return f({ v: 1 });\n}\n"
	res := mustLower(t, "a1.ts", src)
	if !strings.Contains(res.SAI, "base: i32") {
		t.Errorf("missing base capture param:\n%s", res.SAI)
	}
	if strings.Contains(res.SAI, "Box: i32") {
		t.Errorf("type name must not be captured:\n%s", res.SAI)
	}
}

func TestLowerAccessorRefuse(t *testing.T) {
	// Classes carrying unread getters lower; reads inline the body.
	cls := "class C {\n  v: i32;\n  get g(): i32 { return this.v; }\n}\nfunction main(): i32 {\n  const c = new C();\n  return 1;\n}\n"
	res := mustLower(t, "ac1.ts", cls)
	_ = res
	// Getter reads inline (was: precise refusal).
	rd := "class C {\n  v: i32;\n  get g(): i32 { return this.v; }\n}\nfunction main(): i32 {\n  const c = new C();\n  return c.g;\n}\n"
	res = mustLower(t, "ac2.ts", rd)
	if !strings.Contains(res.SAI, "L_m_end") {
		t.Errorf("missing inlined getter body:\n%s", res.SAI)
	}
	// Setter writes inline (was: precise refusal).
	wr := "class C {\n  v: i32;\n  set s(x: i32) { this.v = x; }\n}\nfunction main(): i32 {\n  const c = new C();\n  c.s = 1;\n  return 1;\n}\n"
	res = mustLower(t, "ac3.ts", wr)
	if !strings.Contains(res.SAI, "L_m_end") {
		t.Errorf("missing inlined setter body:\n%s", res.SAI)
	}
}

func TestLowerStaticFold(t *testing.T) {
	// Static literal members fold at reads (class name and instances).
	src := "class C {\n  static TYPE = \"circle\" as const;\n  static N = 7;\n  v: i32;\n}\nfunction main(): i32 {\n  const c = new C();\n  const t: string = C.TYPE;\n  const n: i32 = c.N;\n  return t.length + n + c.v;\n}\n"
	res := mustLower(t, "s1.ts", src)
	if !strings.Contains(res.SAI, "circle") {
		t.Errorf("missing folded string:\n%s", res.SAI)
	}
	// Instance layout skips folded statics (v stays at offset 0).
	if !strings.Contains(res.SAI, "+ 0 as i32") {
		t.Errorf("want instance field at +0:\n%s", res.SAI)
	}
	// Non-literal statics keep the legacy path byte-for-byte (wasteful
	// but sound: all accesses share the same layout).
	legacy := "class D {\n  static cfg = { a: 1 };\n  v: i32;\n}\nfunction main(): i32 {\n  const d = new D();\n  return d.v;\n}\n"
	res = mustLower(t, "s2.ts", legacy)
	if !strings.Contains(res.SAI, "+ 8 as i32") {
		t.Errorf("want legacy instance load:\n%s", res.SAI)
	}
	// Cross-file statics fold without heritage in the way.
	xf := map[string]string{
		"main.ts":   "import { C } from \"./shapes\";\nfunction main(): string {\n  return C.TYPE;\n}\n",
		"shapes.ts": "export class C {\n  static TYPE = \"circle\" as const;\n}\n",
	}
	res2 := mustLowerProgram(t, "main.ts", xf)
	if !strings.Contains(res2.SAI, "circle") {
		t.Errorf("missing cross-file static fold:\n%s", res2.SAI)
	}
	// Heritage classes publish statics and lower (single extends flattens;
	// empty subclasses inherit everything; see class_heritage.go).
	her := map[string]string{
		"main.ts":   "import { CircleShape } from \"./shapes\";\nfunction main(): string {\n  return CircleShape.TYPE;\n}\n",
		"shapes.ts": "class Shape {\n}\nexport class CircleShape extends Shape {\n  static TYPE = \"circle\" as const;\n}\n",
	}
	r := LowerProgram("main.ts", her)
	if r.Refused {
		t.Fatalf("unexpected refusal:\n%v", r.Diagnostics)
	}
	if !strings.Contains(r.SAI, "circle") {
		t.Errorf("missing heritage static fold:\n%s", r.SAI)
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

func TestLowerClassExtendsEmptyAllowed(t *testing.T) {
	// Empty subclasses flatten (base fields/methods inherit; see
	// class_heritage.go). Still-refused heritage shapes stay locked in
	// TestLowerHeritageRefusals.
	src := "class B { x: i32 = 0; }\nclass C extends B {}\nfunction main(): i32 { return 0; }\n"
	res := Lower("cls.ts", src)
	if res.Refused {
		t.Fatalf("unexpected refusal:\n%s", diagText(res))
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

// String enum members refuse loudly at read sites (they previously
// folded to ordinals silently: S.A ("a") lowered to return 0). Integer
// members of mixed enums keep folding; all-integer enums are untouched.
func TestStringEnumReadRefuses(t *testing.T) {
	str := "enum S {\n  A = \"a\",\n  B = \"b\",\n}\nfunction main(): string {\n  return S.A;\n}\n"
	r := Lower("str.ts", str)
	if !r.Refused {
		t.Fatalf("expected string-enum refusal, got:\n%s", r.SAI)
	}
	if got := diagText(r); !strings.Contains(got, "string enum member S.A is not lowerable") {
		t.Errorf("missing string-enum diagnostic:\n%s", got)
	}
	mix := "enum M {\n  A = 0,\n  B = \"b\",\n}\nfunction main(): i32 {\n  return M.A;\n}\n"
	res := mustLower(t, "mix.ts", mix)
	if !strings.Contains(res.SAI, "return 0") {
		t.Errorf("integer member must still fold, got:\n%s", res.SAI)
	}
	mixBad := "enum M {\n  A = 0,\n  B = \"b\",\n}\nfunction main(): string {\n  return M.B;\n}\n"
	r2 := Lower("mixbad.ts", mixBad)
	if !r2.Refused {
		t.Fatalf("expected mixed string-member refusal, got:\n%s", r2.SAI)
	}
	if got := diagText(r2); !strings.Contains(got, "string enum member M.B is not lowerable") {
		t.Errorf("missing mixed string-enum diagnostic:\n%s", got)
	}
}

// Coded panics: bare `panic` is rejected by the assembler, so every
// runtime abort carries a code (25xx satsgo block; 1403 registry OOM).
func TestCodedPanics(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"throw", "function main(): void {\n  throw \"boom\";\n}\n", "panic(2501)"},
		{"dom scratch full", "function main(): string {\n  const el = document.createElement(\"div\");\n  return el.textContent;\n}\n", "panic(2502)"},
		{"date parse status", "function main(): i64 {\n  return Date.parse(\"2024-01-01\");\n}\n", "panic(2503)"},
		{"node status", "import { platform } from \"os\";\nfunction main(): i32 {\n  const p = platform();\n  return p.length;\n}\n", "panic(2503)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mustLower(t, "panic.ts", tc.src)
			if !strings.Contains(res.SAI, tc.want) {
				t.Errorf("missing %q in output:\n%s", tc.want, res.SAI)
			}
			if strings.Contains(res.SAI, "\n    panic\n") {
				t.Errorf("bare panic emitted:\n%s", res.SAI)
			}
		})
	}
}

// Decorators and `using` used to lower silently (dropping definition-time
// effects and disposal); both refuse loudly now.
// Async honesty: pure-value await chains unwrap (demos 263/264 stay
// lowered, incl. cross-fn nested await of values); genuine suspensions
// refuse at the inner call so unwrap never masks them. Locks the SLA
// boundary (TS Promise != SLA future<T>; SLA 314/315 nested-suspension
// gap has no silent counterpart here).
// Try precision: a throw nested inside a function declared in the try body
// fires on call, not while the body runs, so the try still lowers (catch
// dead, finally runs); a single direct `throw <i32>` transfers to catch
// (value bound to the catch param); other throwing shapes still refuse.
// SLA parity: neither frontend has exception edges (SLA has no try at all).
func TestTryNestedFunctionThrow(t *testing.T) {
	ok := "function main(): i32 {\n  try {\n    const bomb = (): void => {\n      throw \"boom\";\n    };\n  } catch (e) {\n  } finally {\n  }\n  return 7;\n}\n"
	res := mustLower(t, "try_nested.ts", ok)
	if !strings.Contains(res.SAI, "return 7") && !strings.Contains(res.SAI, "7") {
		t.Errorf("try with nested-fn throw should lower the body, got:\n%s", res.SAI)
	}
	bad := "function main(): i32 {\n  try {\n    throw \"boom\";\n  } catch (e) {\n    return 1;\n  }\n  return 7;\n}\n"
	r := Lower("try_direct.ts", bad)
	if !r.Refused {
		t.Fatalf("expected string-throw-in-try refusal, got:\n%s", r.SAI)
	}
	if got := diagText(r); !strings.Contains(got, "throw value type is not lowerable (catch params carry i32 only)") {
		t.Errorf("missing try/throw diagnostic:\n%s", got)
	}
	nestedThrow := "function main(): i32 {\n  try {\n    if (1) {\n      throw 1;\n    }\n  } catch (e) {\n    return e;\n  }\n  return 7;\n}\n"
	r = Lower("try_nested_throw.ts", nestedThrow)
	if !r.Refused {
		t.Fatalf("expected nested-throw-in-try refusal, got:\n%s", r.SAI)
	}
	if got := diagText(r); !strings.Contains(got, "throw inside try is not lowerable (catch cannot resume after panic)") {
		t.Errorf("missing nested try/throw diagnostic:\n%s", got)
	}
}

func TestLowerTryThrowCatch(t *testing.T) {
	withParam := "function main(): i32 {\n  try {\n    throw 41;\n  } catch (e) {\n    return e;\n  }\n}\n"
	res := mustLower(t, "try_catch.ts", withParam)
	if !strings.Contains(res.SAI, "e = 41") || !strings.Contains(res.SAI, "return e") {
		t.Errorf("throw should bind the catch param, got:\n%s", res.SAI)
	}
	bareCatch := "function main(): i32 {\n  let x: i32 = 0;\n  try {\n    throw 7;\n  } catch {\n    x = 9;\n  }\n  return x;\n}\n"
	res = mustLower(t, "try_bare.ts", bareCatch)
	if !strings.Contains(res.SAI, "x = 9") {
		t.Errorf("bare catch should run the handler, got:\n%s", res.SAI)
	}
	withFinally := "function main(): i32 {\n  try {\n    throw 1;\n  } catch (e) {\n    return e + 1;\n  } finally {\n  }\n  return 0;\n}\n"
	res = mustLower(t, "try_finally.ts", withFinally)
	if !strings.Contains(res.SAI, "return") {
		t.Errorf("catch+finally should lower, got:\n%s", res.SAI)
	}
	noCatch := "function main(): i32 {\n  try {\n    throw 1;\n  } finally {\n  }\n  return 0;\n}\n"
	res = mustLower(t, "try_nocatch.ts", noCatch)
	if !strings.Contains(res.SAI, "panic(2501)") {
		t.Errorf("try/finally throw should panic after finally, got:\n%s", res.SAI)
	}
	identThrow := "function main(): i32 {\n  const x: i32 = 1;\n  try {\n    throw x;\n  } catch (e) {\n    return e;\n  }\n  return 0;\n}\n"
	res = mustLower(t, "try_ident.ts", identThrow)
	if !strings.Contains(res.SAI, "return e") {
		t.Errorf("identifier throw should bind the catch param, got:\n%s", res.SAI)
	}
	prefix := "function log(x: i32): void {\n}\nfunction main(): i32 {\n  let x: i32 = 0;\n  try {\n    log(1);\n    x = 5;\n    const k: i32 = 40;\n    throw k + 2;\n  } catch (e) {\n    return x + e;\n  }\n  return 0;\n}\n"
	res = mustLower(t, "try_prefix.ts", prefix)
	if !strings.Contains(res.SAI, "return") {
		t.Errorf("prefix throw should lower prefix then catch, got:\n%s", res.SAI)
	}
	leakPrefix := "function main(): i32 {\n  try {\n    let t: i32 = 1;\n    throw 2;\n  } catch (e) {\n    return t;\n  }\n  return 0;\n}\n"
	if r := Lower("try_leak.ts", leakPrefix); !r.Refused {
		t.Fatalf("expected try-local leak refusal, got:\n%s", r.SAI)
	}
	multiThrow := "function main(): i32 {\n  try {\n    throw 1;\n    throw 2;\n  } catch (e) {\n    return e;\n  }\n  return 0;\n}\n"
	res = mustLower(t, "try_multi.ts", multiThrow)
	if !strings.Contains(res.SAI, "e = 1") {
		t.Errorf("first throw should win, got:\n%s", res.SAI)
	}
}

func TestAsyncSyncUnwrapHonest(t *testing.T) {
	single := "async function fetch(): i32 {\n  return 41;\n}\nasync function main(): i32 {\n  const v = await fetch();\n  return v + 1;\n}\n"
	res := mustLower(t, "async.ts", single)
	if !strings.Contains(res.SAI, "call @fetch()") {
		t.Errorf("await should unwrap to a direct call, got:\n%s", res.SAI)
	}
	nested := "async function fetch(): i32 {\n  return 41;\n}\nasync function wrap(): i32 {\n  const v = await fetch();\n  return v + 1;\n}\nasync function main(): i32 {\n  const v = await wrap();\n  return v;\n}\n"
	res2 := mustLower(t, "async_nested.ts", nested)
	if !strings.Contains(res2.SAI, "call @wrap()") || !strings.Contains(res2.SAI, "call @fetch()") {
		t.Errorf("nested value-await should lower both calls, got:\n%s", res2.SAI)
	}
	timer := "async function main(): i32 {\n  const v = await setTimeout(1);\n  return v;\n}\n"
	r := Lower("async_timer.ts", timer)
	if !r.Refused {
		t.Fatalf("expected timer-await refusal, got:\n%s", r.SAI)
	}
	if got := diagText(r); !strings.Contains(got, "needs an event loop with callback dispatch (async timers are Phase 2)") {
		t.Errorf("missing Phase-2 timer diagnostic:\n%s", got)
	}
}

func TestLoudDecoratorUsing(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"class decorator", "@sealed\nclass C {\n}\nfunction main(): i32 {\n  return 0;\n}\n", "class decorators are not lowerable"},
		{"member decorator", "class C {\n  @m v: i32 = 1;\n}\nfunction main(): i32 {\n  return 0;\n}\n", "member decorators are not lowerable"},
		{"using local", "function main(): i32 {\n  using x = 1;\n  return x;\n}\n", "using declarations are not lowerable"},
		{"await using local", "function main(): i32 {\n  await using y = 2;\n  return y;\n}\n", "using declarations are not lowerable"},
		{"using top level", "using K = 42;\nfunction main(): i32 {\n  return 0;\n}\n", "using declarations are not lowerable"},
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

// Logical compound assignment short-circuits: the RHS lowers only on
// the assign arm, and both arms join through a slot (mirrors `??`).
func TestLogicAssign(t *testing.T) {
	src := `function bump(): i32 {
  return 10;
}
function main(): i32 {
  let a = 0;
  let b = 2;
  let c = 0;
  a ||= bump();
  b &&= bump();
  c ??= bump();
  return a * 100 + b * 10 + c;
}
`
	res := mustLower(t, "logas.ts", src)
	for _, want := range []string{
		"logas_assign", "logas_skip", "logas_end",
		"call @bump()",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// One call per operator: RHS lowers once, on the assign arm only.
	if n := strings.Count(res.SAI, "call @bump()"); n != 3 {
		t.Errorf("expected 3 bump calls, got %d:\n%s", n, res.SAI)
	}
	// String truthiness tests length, not the header pointer.
	str := `function main(): i32 {
  let s = "";
  s ||= "d";
  return s.length;
}
`
	res = mustLower(t, "logas_str.ts", str)
	if !strings.Contains(res.SAI, "+ 8 as u64") {
		t.Errorf("missing length test in string ||= output:\n%s", res.SAI)
	}
	// Module slots route through the registry on both arms.
	mod := `let m: i32 = 0;
function main(): i32 {
  m ||= 7;
  m &&= 3;
  return m;
}
`
	res = mustLower(t, "logas_mod.ts", mod)
	for _, want := range []string{
		"call @sa_modstate_set_u64(",
		"call @sa_modstate_get_u64(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Bad targets refuse loudly.
	bad := Lower("refuse.ts", "function main(): i32 {\n  const o = { x: 1 };\n  o.x ||= 2;\n  return 0;\n}\n")
	if !bad.Refused {
		t.Fatalf("expected refusal for non-slot member target, lowered:\n%s", bad.SAI)
	}
}

// Angle assertions erase exactly like `as` (type-only, no runtime).
func TestAngleAssertionErasure(t *testing.T) {
	src := `interface P {
  x: i32;
}
function main(): i32 {
  const a = <number>41;
  const o = <P>{ x: 1 };
  const s = { x: 2 } satisfies P;
  return a + o.x + s.x;
}
`
	res := mustLower(t, "angle.ts", src)
	for _, want := range []string{"a = 41", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Chained assertions fold through statics as well.
	st := `class C {
  static K = <number>7;
}
function main(): i32 {
  return C.K;
}
`
	res = mustLower(t, "angle2.ts", st)
	if !strings.Contains(res.SAI, "return 7") {
		t.Errorf("missing folded static in output:\n%s", res.SAI)
	}
}

// Object spread copies layout-guided fields in source order (later props
// override); literal computed keys fold; shorthand reads the binding.
func TestObjectSpread(t *testing.T) {
	src := `interface P {
  x: i32;
  y: i32;
}
function main(): i32 {
  const o: P = { x: 1, y: 2 };
  const p: P = { ...o, y: 20 };
  const q: P = { x: 100, ...o };
  return p.x * 1000 + p.y + q.x;
}
`
	res := mustLower(t, "spread.ts", src)
	for _, want := range []string{"load ", " as i32", "store "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Literal computed keys and shorthand fold to static names.
	lit := `interface P {
  x: i32;
  y: i32;
}
function main(): i32 {
  const y = 7;
  const o: P = { ["x"]: 1, y };
  return o.x + o.y;
}
`
	res = mustLower(t, "compkey.ts", lit)
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in output:\n%s", res.SAI)
	}
	// Dynamic keys, layout-less spreads and mismatched targets refuse.
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"dynamic key", "interface P {\n  x: i32;\n}\nfunction main(): i32 {\n  const k = \"x\";\n  const o: P = { [k]: 1 };\n  return o.x;\n}\n", "computed property names must be literals"},
		{"spread unknown", "interface P {\n  x: i32;\n  y: i32;\n}\nfunction g(o: any): P {\n  return { ...o, y: 1 };\n}\nfunction main(): i32 {\n  return 0;\n}\n", "spread source has no recorded interface layout"},
		{"spread mismatch", "interface P {\n  x: i32;\n  y: i32;\n}\ninterface Q {\n  x: i32;\n  z: i32;\n}\nfunction main(): i32 {\n  const o: Q = { x: 1, z: 2 };\n  const p: P = { ...o, y: 3 };\n  return p.x;\n}\n", "matches no recorded interface layout"},
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

// Class and function expressions share the declaration paths (recorded
// under the bound name, or the class's own name with an alias).
func TestExprForms(t *testing.T) {
	src := `function main(): i32 {
  const C = class {
    v: i32 = 0;
    constructor(n: i32) {
      this.v = n;
    }
    get(): i32 {
      return this.v + 1;
    }
  };
  const c = new C(3);
  const f = function (n: i32): i32 {
    return n * 2;
  };
  return c.get() + f(20);
}
`
	res := mustLower(t, "expr.ts", src)
	// Instance methods inline at the call site (no vtables); local
	// function expressions share the @__arrow_N alias path.
	for _, want := range []string{"call @__arrow_", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	named := `const D = class E {
  static K = 5;
  v: i32 = 0;
  constructor(n: i32) {
    this.v = n;
  }
};
function main(): i32 {
  const d = new D(1);
  return d.v + D.K;
}
`
	res = mustLower(t, "exprnamed.ts", named)
	for _, want := range []string{"return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	// Known gap (sloppy scope): the inner name leaks to the enclosing
	// scope instead of refusing. Values stay correct (same class); only
	// error-visibility differs from node (ReferenceError). Locked here
	// so a future scoping pass can flip it to a refusal deliberately.
	innerUse := `const D = class E {
  static K = 5;
};
function main(): i32 {
  return E.K;
}
`
	res = mustLower(t, "exprinner.ts", innerUse)
	if !strings.Contains(res.SAI, "return 5") {
		t.Errorf("missing folded inner static in output:\n%s", res.SAI)
	}
	top := `const C = class {
  v: i32 = 4;
};
function main(): i32 {
  const c = new C();
  return c.v;
}
`
	res = mustLower(t, "exprtop.ts", top)
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in output:\n%s", res.SAI)
	}
	topfn := `const f = function (n: i32): i32 {
  return n + 1;
};
function main(): i32 {
  return f(41);
}
`
	res = mustLower(t, "exprtopfn.ts", topfn)
	if !strings.Contains(res.SAI, "call @f(41)") {
		t.Errorf("missing top-level fn call in output:\n%s", res.SAI)
	}
	// Expression heritage shares the declaration machinery
	// (parseHeritage/inheritClass): a declared base links, an unknown
	// base stays loud through the shared unknown-base diagnostic.
	ok := `class B {
  x: i32 = 0;
  constructor(n: i32) {
    this.x = n;
  }
}
const C = class extends B {
  y: i32 = 0;
  constructor(n: i32, m: i32) {
    super(n);
    this.y = m;
  }
};
function main(): i32 {
  const c = new C(3, 4);
  return c.x + c.y;
}
`
	res = mustLower(t, "exprherit.ts", ok)
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in expression-heritage output:\n%s", res.SAI)
	}
	bad := Lower("refuse.ts", "function main(): i32 {\n  const C = class extends Object {\n  };\n  return 0;\n}\n")
	if !bad.Refused {
		t.Fatalf("expected heritage refusal, lowered:\n%s", bad.SAI)
	}
	if !strings.Contains(diagText(bad), "extends unknown base Object") {
		t.Errorf("missing heritage diagnostic:\n%s", diagText(bad))
	}
}

// Private fields mangle by owner (`#x` → `#Owner#x`): own-method access
// lowers, shadowing owners keep distinct slots, outsiders refuse loudly.
func TestPrivateFields(t *testing.T) {
	src := `class C {
  #x: i32 = 0;
  constructor(n: i32) {
    this.#x = n;
  }
  get(): i32 {
    return this.#x;
  }
  add(o: C): i32 {
    return this.#x + o.#x;
  }
}
function main(): i32 {
  const c = new C(5);
  return c.get() + c.add(c);
}
`
	res := mustLower(t, "priv.ts", src)
	// Mangled keys live in the layout table (numeric offsets in SAI);
	// assert the lowered shape instead: ctor store plus two method loads.
	for _, want := range []string{"store t_1 + 0, 5 as i32", "load c + 0 as i32", "return "} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	if strings.Contains(res.SAI, "computed field names") {
		t.Errorf("stale refusal leaked:\n%s", res.SAI)
	}
	// Brand checks fold statically against the owner.
	brand := `class C {
  #x: i32 = 0;
  has(o: C): i32 {
    if (#x in o) {
      return 1;
    }
    return 0;
  }
}
function main(): i32 {
  const c = new C();
  return c.has(c);
}
`
	res = mustLower(t, "privbrand.ts", brand)
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in output:\n%s", res.SAI)
	}
	// Private statics fold behind the owner (read from an instance
	// method via the class name; static methods stay unsupported).
	stat := `class C {
  static #K = 7;
  v: i32 = 0;
  getK(): i32 {
    return C.#K + this.v;
  }
}
function main(): i32 {
  const c = new C();
  return c.getK();
}
`
	res = mustLower(t, "privstatic.ts", stat)
	if !strings.Contains(res.SAI, "add 7, ") {
		t.Errorf("missing folded private static in output:\n%s", res.SAI)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"outside read", "class C {\n  #x: i32 = 0;\n}\nfunction main(): i32 {\n  const c = new C();\n  return c.#x;\n}\n", "not accessible outside a class method"},
		{"subclass read", "class B {\n  #x: i32 = 0;\n  get(): i32 {\n    return this.#x;\n  }\n}\nclass S extends B {\n  probe(): i32 {\n    return this.#x;\n  }\n}\nfunction main(): i32 {\n  const s = new S();\n  return s.probe();\n}\n", "not declared in class S"},
		{"super read", "class B {\n  #x: i32 = 0;\n  get(): i32 {\n    return this.#x;\n  }\n}\nclass S extends B {\n  probe(): i32 {\n    return super.#x;\n  }\n}\nfunction main(): i32 {\n  const s = new S();\n  return s.probe();\n}\n", "not accessible via super"},
		{"outside static", "class C {\n  static #K = 7;\n}\nfunction main(): i32 {\n  return C.#K;\n}\n", "not accessible outside a class method"},
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

// `String.raw` cooks nothing (raw parts, rendered substitutions); other
// tags refuse loudly (tag functions have no first-class value, so even a
// materialized strings array could not dispatch them).
func TestTaggedTemplate(t *testing.T) {
	src := `function main(): i32 {
  const a = String.raw` + "`a\\nb${41}c`" + `;
  return a.length;
}
`
	res := mustLower(t, "raw.ts", src)
	for _, want := range []string{
		`@import "sa_std/string.sai"`,
		"call @sa_string_concat(",
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	plain := `function main(): i32 {
  const b = String.raw` + "`hi`" + `;
  return b.length;
}
`
	res = mustLower(t, "rawplain.ts", plain)
	if !strings.Contains(res.SAI, "return ") {
		t.Errorf("missing return in output:\n%s", res.SAI)
	}
	bad := Lower("refuse.ts", "function tag(s: any): string {\n  return \"t\";\n}\nfunction main(): i32 {\n  const s = tag`hi`;\n  return s.length;\n}\n")
	if !bad.Refused {
		t.Fatalf("expected tag refusal, lowered:\n%s", bad.SAI)
	}
	if !strings.Contains(diagText(bad), "tagged templates are not lowerable (tag functions have no first-class value; String.raw is the only supported tag)") {
		t.Errorf("missing tag diagnostic:\n%s", diagText(bad))
	}
}

// String.raw never cooks: escapes stay literal (backslash-n is two
// chars), including no-substitution literals (source-sliced; the parser
// leaves their RawText empty).
func TestTaggedRawEscapes(t *testing.T) {
	src := "function main(): i32 {\n  const a = String.raw`a\\nb`;\n  return a.length;\n}\n"
	res := mustLower(t, "rawesc.ts", src)
	if !strings.Contains(res.SAI, `utf8:"a\\nb\0"`) {
		t.Errorf("raw escape cooked in output:\n%s", res.SAI)
	}
	// Cooked templates keep cooking (control shape, not raw).
	cooked := "function main(): i32 {\n  const c = `a\\nb`;\n  return c.length;\n}\n"
	res = mustLower(t, "cooked.ts", cooked)
	if strings.Contains(res.SAI, `utf8:"a\\nb\0"`) {
		t.Errorf("cooked template left raw in output:\n%s", res.SAI)
	}
}

// rawNoSubText slices exact source bytes (positions are byte-exact even
// past multibyte prefixes); cooked text can never stand in (every escape
// changes length, including invisible unicode escapes).
func TestRawNoSubSlicing(t *testing.T) {
	src := "const e = \"caf\u00e9\";\nfunction main(): i32 {\n  const u = String.raw`a\\tb`;\n  return u.length;\n}\n"
	res := mustLower(t, "rawuni.ts", src)
	if !strings.Contains(res.SAI, `utf8:"a\\tb\0"`) {
		t.Errorf("raw slice wrong past multibyte prefix:\n%s", res.SAI)
	}
}
