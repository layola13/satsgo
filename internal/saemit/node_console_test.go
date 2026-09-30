package saemit

// Tests for node_console.go (console error/time/timeEnd/clear projection).
import (
	"strings"
	"testing"
)

func TestLowerNodeConsole(t *testing.T) {
	src := "function main(): i32 {\n  console.error(\"boom\", 7);\n  console.time(\"t\");\n  console.time();\n  const ms: f64 = console.timeEnd(\"t\");\n  console.clear();\n  return 1;\n}\n"
	res := mustLower(t, "c1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_console_error",
		"call @sa_node_plugin_console_time(",
		"call @sa_node_plugin_console_time_end",
		"call @sa_node_plugin_console_clear()",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// timeEnd with two args refuses loudly.
	bad := "function main(): i32 {\n  console.timeEnd(\"a\", \"b\");\n  return 1;\n}\n"
	if r := Lower("c2.ts", bad); !r.Refused {
		t.Fatalf("expected timeEnd-arity refusal, got:\n%s", r.SAI)
	}
	// clear with an argument refuses loudly.
	badClear := "function main(): i32 {\n  console.clear(\"x\");\n  return 1;\n}\n"
	if r := Lower("c3.ts", badClear); !r.Refused {
		t.Fatalf("expected clear-arity refusal, got:\n%s", r.SAI)
	}
	// Unknown console methods still refuse (no silent sink).
	badM := "function main(): i32 {\n  console.table(\"x\");\n  return 1;\n}\n"
	if r := Lower("c4.ts", badM); !r.Refused {
		t.Fatalf("expected unknown-method refusal, got:\n%s", r.SAI)
	}
}
