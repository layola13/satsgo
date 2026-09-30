package saemit

// Tests for node_buffer.go (Buffer.byteLength/concat projection).
import (
	"strings"
	"testing"
)

func TestLowerNodeBuffer(t *testing.T) {
	src := "function main(): i64 {\n  const n: u64 = Buffer.byteLength(\"hello\");\n  const b: string = Buffer.concat([\"ab\", \"cd\"]);\n  return 1;\n}\n"
	res := mustLower(t, "b1.ts", src)
	for _, want := range []string{
		"call @sa_node_plugin_buffer_byte_length",
		"call @sa_node_plugin_buffer_concat",
		`@import "node.sai"`,
	} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q:\n%s", want, res.SAI)
		}
	}
	// Non-string byteLength refuses loudly.
	num := "function main(): i64 {\n  return Buffer.byteLength(7);\n}\n"
	if r := Lower("b2.ts", num); !r.Refused {
		t.Fatalf("expected tag-type refusal, got:\n%s", r.SAI)
	}
	// Dynamic arrays cannot hold slices: refuse loudly.
	arrVar := "function main(): string {\n  const xs = [\"a\", \"b\"];\n  return Buffer.concat(xs);\n}\n"
	if r := Lower("b3.ts", arrVar); !r.Refused {
		t.Fatalf("expected dynamic-array refusal, got:\n%s", r.SAI)
	}
	// Non-string elements refuse loudly.
	mix := "function main(): string {\n  return Buffer.concat([\"a\", 1]);\n}\n"
	if r := Lower("b4.ts", mix); !r.Refused {
		t.Fatalf("expected element-type refusal, got:\n%s", r.SAI)
	}
}
