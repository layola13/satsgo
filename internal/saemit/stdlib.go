// Standard library projection table: TypeScript surface → sci/sa_std.
//
// HARD POLICY: every TS std call lowers to a `call @sa_*` symbol whose
// contract literally exists in sci/sa_std (*.sai / *.sa / *.sal). The tsgo
// frontend simulates NOTHING at runtime. If no sa_std symbol exists for a
// construct, lowering refuses loudly with a located diagnostic instead of
// emitting stub code (same philosophy as the .wit refusal).
//
// Each entry records the exact sa_std contract file so generated `.sai`
// outputs carry the matching `@import` and `sa build` resolves them.
// Symbol names below were verified against sci/sa_std:
//
//	fmt.sai, string.sai, math.sai, io.sai, io/print.sai, fs.sai, net.sai,
//	btree_map.sa (@export sa_btree_map_new).
package saemit

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
)

// StdProjection maps one TS std surface to one sa_std contract symbol.
type StdProjection struct {
	// TS is the TypeScript surface, e.g. "console.log", "Math.sin",
	// "String.fromCharCode", "fs.readFile", "net.tcpConnect", "new Map".
	TS string
	// Module is the contract file imported by generated code,
	// e.g. "sa_std/io/print.sai". Plugin backends name their .sai
	// (e.g. "node.sai", resolved via plugin share dirs at sa build time).
	Module string
	// Backend selects the runtime owner: "" (default) is compiler-shipped
	// sci/sa_std; "node"/"deno"/"bun" are the same-named SA plugins'
	// native surfaces (see tools/check_sa_std_projection.sh: each backend
	// is verified against its own exported-symbols contract).
	Backend string
	// Symbol is the SA callee, e.g. "sa_print_bytes".
	Symbol string
	// Ret is the SA return type of the call.
	Ret saType
	// StrArgs lists 0-based argument indices that are string slices and
	// expand to a (ptr, len) pair with `&` on the pointer when the
	// contract declares it (mirrors emitStdlibCall string_args).
	StrArgs []int
	// AmpArgs lists 0-based argument indices passed with the `&`
	// capability prefix without expansion.
	AmpArgs []int
	// Extra appends fixed callee-arity parameters (e.g. "1048576", "0",
	// or "&buf, 4096" for scratch-buffer out-params).
	Extra string
	// Fallible marks the u64!-returning trio whose handle materialises
	// via scratch + field-0 load (mirrors isFallibleHandle).
	Fallible bool
	// NodeOut marks node-plugin u32-status out-param calls whose shape is
	// (status, outs...): "string" wraps (&ptr,&len) outs into a slice.
	// Nonzero status panics (loud; no silent error zeros).
	NodeOut string
	// Note documents arity/shape adaptation (e.g. string arg expansion).
	Note string
}

// GlobalFnProjection is a projection for a bare global call target.
type GlobalFnProjection = StdProjection

// StdProjectionTable is the authoritative TS → sa_std map. The emitter
// consults it at every call site; tools/check_sa_std_projection.sh verifies
// each Symbol against $SCI_ROOT/sa_std in CI.
var StdProjectionTable = []StdProjection{
	// ---- console -------------------------------------------------------
	{TS: "console.log", Module: "sa_std/io/print.sai", Symbol: "sa_print_bytes", Ret: tVoid,
		Note: "each operand normalises to a text slice, joined with space + trailing newline (emitPrintln shape)"},

	// ---- template interpolation ----------------------------------------
	{TS: "interp<i32>", Module: "sa_std/fmt.sai", Symbol: "sa_fmt_i64_into", Ret: tI32,
		Note: "integer operands go through sext + sa_fmt_i64_into; booleans render as 0/1"},
	{TS: "interp<f64>", Module: "sa_std/fmt.sai", Symbol: "sa_fmt_f64_into", Ret: tI32,
		Note: "precision 6, per sa_plugin_ts rule"},
	{TS: "concat<string>", Module: "sa_std/string.sai", Symbol: "sa_string_concat", Ret: tString,
		Note: "chunks join chunk-by-chunk into a fresh {ptr,len} slice"},

	// ---- String ----------------------------------------------------------
	{TS: "String.fromCharCode", Module: "sa_std/string.sai", Symbol: "sa_string_from_char_code", Ret: tString,
		Note: "shared sci primitive; never simulated in the TS frontend"},
	{TS: "String.fromCodePoint", Module: "sa_std/string.sai", Symbol: "sa_string_from_code_point", Ret: tString, Note: ""},
	{TS: "s.indexOf", Module: "sa_std/string.sai", Symbol: "sa_string_index_of", Ret: tI32, Note: ""},
	{TS: "s.lastIndexOf", Module: "sa_std/string.sai", Symbol: "sa_string_last_index_of", Ret: tI32, Note: ""},
	{TS: "s.startsWith", Module: "sa_std/string.sai", Symbol: "sa_string_starts_with", Ret: tI32, Note: ""},
	{TS: "s.endsWith", Module: "sa_std/string.sai", Symbol: "sa_string_ends_with", Ret: tI32, Note: ""},
	{TS: "s.toLowerCase", Module: "sa_std/string.sai", Symbol: "sa_string_to_lower_ascii", Ret: tString,
		Note: "ascii fold; full Unicode lower is refused"},
	{TS: "s.toUpperCase", Module: "sa_std/string.sai", Symbol: "sa_string_to_upper_ascii", Ret: tString, Note: ""},
	{TS: "s.repeat", Module: "sa_std/string.sai", Symbol: "sa_string_repeat", Ret: tString, Note: ""},
	{TS: "s.padStart", Module: "sa_std/string.sai", Symbol: "sa_string_pad_start", Ret: tString, Note: ""},
	{TS: "s.padEnd", Module: "sa_std/string.sai", Symbol: "sa_string_pad_end", Ret: tString, Note: ""},
	{TS: "s.replace", Module: "sa_std/string.sai", Symbol: "sa_string_replace", Ret: tString, Note: ""},
	{TS: "Number.parseFloat", Module: "sa_std/string.sai", Symbol: "sa_parse_float", Ret: tF64, Note: ""},

	// ---- Math: ONLY the reference-supported surface (math_surface table).
	// Trig/exp/log/hypot/fround have .sai externs but the reference
	// lowerer refuses them (MathNotSupported); projecting them would
	// assemble the wrong dialect, so they refuse loudly here too.
	// abs/pow/floor/ceil/round/trunc lower inline (branch/loop/convert
	// idioms, no import); min/max fold pairwise (spread reduces a slice);
	// sqrt/log10/random/PI/E/Number consts documented below.
	{TS: "Math.floor", Module: "", Symbol: "@inline", Ret: tI32, Note: "integer identity; floats via fptosi convert"},
	{TS: "Math.ceil", Module: "", Symbol: "@inline", Ret: tI32, Note: "ceil(x) = -floor(-x)"},
	{TS: "Math.round", Module: "", Symbol: "@inline", Ret: tI32, Note: "round(x) = floor(x+0.5)"},
	{TS: "Math.trunc", Module: "", Symbol: "@inline", Ret: tI32, Note: "trunc(x) = fptosi(x)"},
	{TS: "Math.abs", Module: "", Symbol: "@inline", Ret: tI32, Note: "branch join"},
	{TS: "Math.pow", Module: "", Symbol: "@inline", Ret: tI32, Note: "integer multiply loop"},
	{TS: "Math.min", Module: "", Symbol: "@inline", Ret: tI32, Note: "pairwise fold; spread reduces a slice"},
	{TS: "Math.max", Module: "", Symbol: "@inline", Ret: tI32, Note: "pairwise fold; spread reduces a slice"},
	{TS: "Math.sqrt", Module: "", Symbol: "@inline", Ret: tI32, Note: "integer binary search (floats refuse)"},
	{TS: "Math.log10", Module: "", Symbol: "@inline", Ret: tI32, Note: "digit-count loop"},
	{TS: "Math.random", Module: "", Symbol: "@inline", Ret: tI32, Note: "deterministic LCG in [0, 32767]"},
	{TS: "Math.PI", Module: "", Symbol: "@const:3", Ret: tI32, Note: "folds to 3 (integer subset)"},
	{TS: "Math.E", Module: "", Symbol: "@const:2", Ret: tI32, Note: "folds to 2 (integer subset)"},
	{TS: "Number.MAX_VALUE", Module: "", Symbol: "@const:2147483647", Ret: tI32, Note: ""},
	{TS: "Number.MAX_SAFE_INTEGER", Module: "", Symbol: "@const:2147483647", Ret: tI32, Note: ""},
	{TS: "Number.MIN_SAFE_INTEGER", Module: "", Symbol: "@const:-2147483648", Ret: tI32, Note: ""},

	// ---- collections: new Map() → real btree backend --------------------
	{TS: "new Map", Module: "sa_std/btree_map.sa", Symbol: "sa_btree_map_new", Ret: tArray,
		Note: "@export sa_btree_map_new() -> ^ptr in btree_map.sa"},

	// ---- fs (import {...} from "fs"): string args expand ptr+len ---------
	{TS: "fs.readFile", Module: "sa_std/fs.sai", Symbol: "sa_fs_read_file", Ret: tU64,
		StrArgs: []int{0}, Extra: "1048576", Note: "path expands to &ptr+len; returns owned buffer handle"},
	{TS: "fs.writeFile", Module: "sa_std/fs.sai", Symbol: "sa_fs_write_file", Ret: tI32,
		StrArgs: []int{0, 1}, Note: "path+data expand to ptr+len pairs"},
	{TS: "fs.open", Module: "sa_std/fs.sai", Symbol: "sa_fs_file_open", Ret: tI32,
		StrArgs: []int{0}, Extra: "0", Note: "path expands to &ptr+len; flags=0"},
	{TS: "fs.create", Module: "sa_std/fs.sai", Symbol: "sa_fs_file_create", Ret: tI32,
		StrArgs: []int{0}, Note: "path expands to &ptr+len"},
	{TS: "fs.close", Module: "sa_std/fs.sai", Symbol: "sa_fs_file_close", Ret: tI32, Note: ""},
	{TS: "fs.read", Module: "sa_std/fs.sai", Symbol: "sa_fs_file_read", Ret: tI32,
		Extra: "&buf, 4096",
		Note:  "fd-level read; emitter supplies a 4096-byte scratch buffer since the TS site passes only the handle"},
	{TS: "fs.write", Module: "sa_std/fs.sai", Symbol: "sa_fs_file_write", Ret: tI32,
		Extra: "&buf, 0",
		Note:  "fd-level write; emitter supplies a scratch buffer"},
	{TS: "fs.remove", Module: "sa_std/fs.sai", Symbol: "sa_fs_remove_file", Ret: tI32,
		StrArgs: []int{0}, Note: "path expands to &ptr+len"},
	{TS: "fs.mkdir", Module: "sa_std/fs.sai", Symbol: "sa_fs_make_dir", Ret: tI32,
		StrArgs: []int{0}, Note: "path expands to &ptr+len"},

	// ---- net (import {...} from "net") ------------------------------------
	{TS: "net.tcpConnect", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_connect", Ret: tU64,
		StrArgs: []int{0}, Extra: "0", Fallible: true, Note: "host expands to &ptr+len; port=0; u64! via scratch+field-0"},
	{TS: "net.tcpListen", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_listener_bind", Ret: tU64,
		StrArgs: []int{0}, Extra: "0", Fallible: true, Note: "host expands to &ptr+len; port=0; u64! via scratch+field-0"},
	{TS: "net.tcpAccept", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_listener_accept", Ret: tU64,
		Fallible: true, Note: "u64! via scratch+field-0"},
	{TS: "net.tcpRead", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_stream_read", Ret: tU64,
		Extra: "&buf, 0",
		Note:  "emitter supplies a scratch buffer; fixed callee arity"},
	{TS: "net.tcpWrite", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_stream_write", Ret: tI32,
		Extra: "&buf, 0",
		Note:  "emitter supplies a scratch buffer"},
	{TS: "net.tcpClose", Module: "sa_std/net.sai", Symbol: "sa_net_tcp_stream_close", Ret: tI32, Note: ""},

	// ---- async/await → sa_std ready-future (Phase 2; refused in Phase 1) --
	{TS: "async/await", Module: "sa_std/async.sla", Symbol: "(ready-future handle)", Ret: tArray,
		Note: "Phase 2: async fn returns 16-byte {state,value}; await unwraps; async main driven by sync @main"},

	// ---- node plugin backend (pilot): native os surfaces ---------------
	// Convention per node.sai: u32 status + &out slots; nonzero panics.
	{TS: "os.platform", Module: "node.sai", Backend: "node", Symbol: "sa_node_plugin_os_platform", Ret: tString,
		NodeOut: "string", Note: "zero-arg string out-param; status-checked"},
	{TS: "os.arch", Module: "node.sai", Backend: "node", Symbol: "sa_node_plugin_os_arch", Ret: tString,
		NodeOut: "string", Note: "zero-arg string out-param; status-checked"},
}

// mathMethod resolves Math.<name> property-access callees to the table
// method name. Inline/const entries are handled by the emitter, not by
// direct sa_std calls.
func mathMethod(fn *ast.Node) (string, bool) {
	if fn.Kind != ast.KindPropertyAccessExpression {
		return "", false
	}
	pa := fn.AsPropertyAccessExpression()
	if pa.Expression.Kind != ast.KindIdentifier || pa.Expression.Text() != "Math" {
		return "", false
	}
	return pa.Name().Text(), true
}

// mathConstFold folds Math.PI/E and Number.* integer constants.
func mathConstFold(recv, member string) (string, bool) {
	for _, p := range StdProjectionTable {
		if p.TS != recv+"."+member {
			continue
		}
		if strings.HasPrefix(p.Symbol, "@const:") {
			return strings.TrimPrefix(p.Symbol, "@const:"), true
		}
	}
	return "", false
}

func isStringFromCharCode(fn *ast.Node) bool {
	if fn.Kind != ast.KindPropertyAccessExpression {
		return false
	}
	pa := fn.AsPropertyAccessExpression()
	return pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "String" &&
		pa.Name().Text() == "fromCharCode"
}

// isNumberIsInteger matches Number.isInteger(x).
func isNumberIsInteger(fn *ast.Node) bool {
	if fn.Kind != ast.KindPropertyAccessExpression {
		return false
	}
	pa := fn.AsPropertyAccessExpression()
	return pa.Expression.Kind == ast.KindIdentifier && pa.Expression.Text() == "Number" &&
		pa.Name().Text() == "isInteger"
}

// globalFnProjection resolves bare global calls (none projected yet;
// user functions take this path).
func globalFnProjection(fname string) (StdProjection, bool) {
	return StdProjection{}, false
}
