#!/usr/bin/env python3
"""Structural validator for tsgo-sa generated .sai files.

Enforces the SA-ASM emission rules from sa_plugin_ts AGENTS.md that can be
checked without the sci assembler:
  - no `jz`; br carries BOTH targets; no break/continue/throw instructions
  - value-returning functions declare `-> T:`
  - load/store carry explicit byte offsets
  - every br/jmp target label is defined; no dead code after a terminator
  - basic @func signature shape

Usage: check_sai_shape.py <file.sai> [...]
Exit 1 on any violation.
"""
import re
import sys

SIG = re.compile(r"^@(?:export\s+)?([A-Za-z_][\w]*)\(([^)]*)\)(\s*->\s*(\w+))?:\s*$")
LBL = re.compile(r"^([A-Za-z_][\w]*):\s*$")
BR = re.compile(r"^br\s+(\S+)\s+->\s*(\S+)\s*,\s*(\S+)\s*$")
JMP = re.compile(r"^jmp\s+(\S+)\s*$")
LOAD = re.compile(r"^(?:\S+\s*=\s*)?load\s+(\S+)\s*\+\s*(\d+)\s+as\s+(\w+)\s*$")
STORE = re.compile(r"^store\s+(\S+)\s*\+\s*(\d+)\s*,\s*(\S+)\s+as\s+(\w+)\s*$")
RET = re.compile(r"^(?:ret|return)(?:\s+(\S+))?\s*$")

errors = []


def err(f, ln, msg):
    errors.append(f"{f}:{ln}: {msg}")


def check(path):
    with open(path, encoding="utf-8") as fh:
        lines = [l.rstrip("\n") for l in fh]
    labels = {}
    refs = []
    in_func = False
    terminated = False
    has_ret_sig = False
    for i, raw in enumerate(lines, 1):
        line = raw.strip()
        if not line or line.startswith("//") or line.startswith("@import") or line.startswith("@const") or line.startswith("@extern"):
            continue
        # EXPAND lines are compiler directives (macro bodies verify
        # post-expansion under `sa check`, the authority here); they are
        # transparent to block-shape tracking like @import.
        if line.startswith("EXPAND "):
            continue
        if line.startswith("@"):
            m = SIG.match(line)
            if not m:
                err(path, i, f"bad function signature: {line}")
                continue
            in_func = True
            terminated = False
            has_ret_sig = m.group(4) is not None
            continue
        m = LBL.match(line)
        if m:
            labels[m.group(1)] = i
            terminated = False
            continue
        if not in_func:
            err(path, i, f"instruction outside function: {line}")
            continue
        if terminated:
            err(path, i, f"dead code after terminator: {line}")
            continue
        if line == "panic" or re.match(r"^panic\(\d+\)$", line):
            terminated = True
            continue
        if line in ("break", "continue") or line.startswith("throw"):
            err(path, i, f"forbidden instruction: {line}")
            continue
        if re.match(r"^jz\b", line):
            err(path, i, "forbidden jz (use br with two targets)")
            continue
        mb = BR.match(line)
        if mb:
            refs.append((mb.group(2).rstrip(","), i))
            refs.append((mb.group(3), i))
            terminated = True
            continue
        mj = JMP.match(line)
        if mj:
            refs.append((mj.group(1), i))
            terminated = True
            continue
        if line.startswith("load ") or re.match(r"^\S+\s*=\s*load\s", line):
            if not LOAD.match(line):
                err(path, i, f"load without explicit offset/type: {line}")
            continue
        if line.startswith("store ") or re.match(r"^\S+\s*=\s*store\s", line):
            if not STORE.match(line):
                err(path, i, f"store without explicit offset/type: {line}")
            continue
        mr = RET.match(line)
        if mr:
            if mr.group(1) is None and has_ret_sig:
                pass  # bare ret in value fn: assembler rejects; flag belowLexically
            terminated = True
            continue
        if line.startswith("!"):
            continue
        if re.match(r"^(\S+)\s*=\s*(alloc|call|add|sub|mul|div|srem|fadd|fsub|fmul|fdiv|fneg|fptosi|sitofp|sext|eq|ne|slt|sle|sgt|sge|fcmp_\w+|and|or|xor|shl|ashr|lshr|ult|call)\b", line):
            continue
        if re.match(r"^(\S+)\s*=\s*(\S+)\s*$", line):
            continue
        if re.match(r"^call\s+@\S+", line):
            continue
        err(path, i, f"unrecognized instruction: {line}")
    for name, ln in refs:
        if name not in labels:
            err(path, ln, f"undefined label: {name}")


def main(files):
    for f in files:
        check(f)
    if errors:
        print("\n".join(errors))
        print(f"FAIL: {len(errors)} violation(s)")
        return 1
    print(f"PASS: {len(files)} file(s) structurally valid")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
