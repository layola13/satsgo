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
	// Classes carrying unread getters lower; reads refuse precisely.
	cls := "class C {\n  v: i32;\n  get g(): i32 { return this.v; }\n}\nfunction main(): i32 {\n  const c = new C();\n  return 1;\n}\n"
	res := mustLower(t, "ac1.ts", cls)
	_ = res
	// Getter reads refuse with the accessor diagnostic (not generic).
	rd := "class C {\n  v: i32;\n  get g(): i32 { return this.v; }\n}\nfunction main(): i32 {\n  const c = new C();\n  return c.g;\n}\n"
	r := Lower("ac2.ts", rd)
	if !r.Refused {
		t.Fatalf("expected getter refusal, got:\n%s", r.SAI)
	}
	hit := false
	for _, d := range r.Diagnostics {
		if strings.Contains(d.Error(), "getter") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("want getter diagnostic, got %v", r.Diagnostics)
	}
	// Setter writes refuse precisely.
	wr := "class C {\n  v: i32;\n  set s(x: i32) { }\n}\nfunction main(): i32 {\n  const c = new C();\n  c.s = 1;\n  return 1;\n}\n"
	r = Lower("ac3.ts", wr)
	if !r.Refused {
		t.Fatalf("expected setter refusal, got:\n%s", r.SAI)
	}
	hit = false
	for _, d := range r.Diagnostics {
		if strings.Contains(d.Error(), "setter") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("want setter diagnostic, got %v", r.Diagnostics)
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
		"main.ts": "import { C } from \"./shapes\";\nfunction main(): string {\n  return C.TYPE;\n}\n",
		"shapes.ts": "export class C {\n  static TYPE = \"circle\" as const;\n}\n",
	}
	res2 := mustLowerProgram(t, "main.ts", xf)
	if !strings.Contains(res2.SAI, "circle") {
		t.Errorf("missing cross-file static fold:\n%s", res2.SAI)
	}
	// Heritage classes publish statics and lower (single extends flattens;
	// empty subclasses inherit everything; see class_heritage.go).
	her := map[string]string{
		"main.ts": "import { CircleShape } from \"./shapes\";\nfunction main(): string {\n  return CircleShape.TYPE;\n}\n",
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
