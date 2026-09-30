package saemit

// Tests for node_timers.go (async timer refusal locking).
import (
	"strings"
	"testing"
)

func TestLowerTimersRefuse(t *testing.T) {
	for _, src := range []string{
		"function main(): i32 {\n  setTimeout(\"x\", 1);\n  return 1;\n}\n",
		"function main(): i32 {\n  setInterval(\"x\", 1);\n  return 1;\n}\n",
		"function main(): i32 {\n  queueMicrotask(\"x\");\n  return 1;\n}\n",
	} {
		r := Lower("t1.ts", src)
		if !r.Refused {
			t.Fatalf("expected timer refusal, got:\n%s", r.SAI)
		}
		found := false
		for _, d := range r.Diagnostics {
			if strings.Contains(d.Error(), "event loop") {
				found = true
			}
		}
		if !found {
			t.Errorf("want event-loop diagnostic, got %v", r.Diagnostics)
		}
	}
	// User shadowing still wins over the timer rationale.
	shadow := "function setTimeout(x: i32): i32 {\n  return x;\n}\nfunction main(): i32 {\n  return setTimeout(1);\n}\n"
	res := mustLower(t, "t2.ts", shadow)
	if !strings.Contains(res.SAI, "call @main__setTimeout") && !strings.Contains(res.SAI, "call @setTimeout") {
		t.Errorf("user-defined setTimeout must lower:\n%s", res.SAI)
	}
}
