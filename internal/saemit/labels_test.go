package saemit

import (
	"strings"
	"testing"
)

// Labeled break/continue route through the label table (loops, switch
// and blocks; anything else refuses loudly).
func TestLabels(t *testing.T) {
	src := `function main(): i32 {
  let s = 0;
  outer: for (let i = 0; i < 3; i++) {
    for (let j = 0; j < 3; j++) {
      if (j == 1) {
        continue outer;
      }
      if (i == 2 && j == 2) {
        break outer;
      }
      s = s + 1;
    }
  }
  return s;
}
`
	res := mustLower(t, "lbl.ts", src)
	for _, want := range []string{"L_for_end_", "L_for_top_"} {
		if !strings.Contains(res.SAI, want) {
			t.Errorf("missing %q in output:\n%s", want, res.SAI)
		}
	}
	blk := `function main(): i32 {
  let s = 0;
  blk: {
    s = 1;
    if (s == 1) {
      break blk;
    }
    s = 2;
  }
  return s;
}
`
	res = mustLower(t, "lblblk.ts", blk)
	if !strings.Contains(res.SAI, "L_lbl_end_") {
		t.Errorf("missing block end label in output:\n%s", res.SAI)
	}
	sw := `function pick(n: i32): i32 {
  let r = 0;
  sw: switch (n) {
    case 1:
      r = 10;
      break sw;
    default:
      r = 20;
  }
  return r;
}
function main(): i32 {
  return pick(1);
}
`
	res = mustLower(t, "lblsw.ts", sw)
	if !strings.Contains(res.SAI, "L_endswitch_") {
		t.Errorf("missing switch end label in output:\n%s", res.SAI)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"undef break", "function main(): i32 {\n  break nope;\n  return 0;\n}\n", "break to undefined label"},
		{"undef continue", "function main(): i32 {\n  continue nope;\n  return 0;\n}\n", "continue to undefined label"},
		{"continue block", "function main(): i32 {\n  blk: {\n    continue blk;\n  }\n  return 0;\n}\n", "continue to non-loop label"},
		{"dup label", "function main(): i32 {\n  a: for (let i = 0; i < 1; i++) {\n    a: for (let j = 0; j < 1; j++) {\n    }\n  }\n  return 0;\n}\n", "duplicate label"},
		{"reuse ok", "function main(): i32 {\n  a: for (let i = 0; i < 1; i++) {\n    break a;\n  }\n  a: for (let j = 0; j < 1; j++) {\n    break a;\n  }\n  return 0;\n}\n", "@@SEQUENTIAL-REUSE-LOWERS@@"},
		{"label expr", "function main(): i32 {\n  let x = 0;\n  lbl: x = 1;\n  return x;\n}\n", "labeled KindExpressionStatement is not lowerable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Lower("refuse.ts", tc.src)
			// Sequential label reuse is legal JS and must lower.
			if tc.want == "@@SEQUENTIAL-REUSE-LOWERS@@" {
				if res.Refused {
					t.Fatalf("sequential reuse refused:\n%s", diagText(res))
				}
				return
			}
			if !res.Refused {
				t.Fatalf("expected refusal, lowered:\n%s", res.SAI)
			}
			if !strings.Contains(diagText(res), tc.want) {
				t.Errorf("missing %q in diagnostics:\n%s", tc.want, diagText(res))
			}
		})
	}
}
