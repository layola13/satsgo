#!/usr/bin/env bash
# Differential gate: tsgo-lowered .sai vs Node oracle (method mirrors
# sa_plugin_ts/tools/verify_demos.sh buckets).
#
# Usage:
#   DEMO_DIR=/content/sa_all/sa_plugin_ts/demos \
#   SA_BIN=/content/sa_all/sci/zig-out/bin/sa \
#   NODE_BIN=/tools/node/bin/node \
#   STRIP=/content/sa_all/sa_plugin_ts/tools/strip_ts.py \
#   tools/verify_demos_diff.sh [sai-dir]
#
# sai-dir defaults to a fresh sweep (requires go + this repo on PATH).
# Buckets: verified / failed / no-oracle (strip or node rejects the source)
# / timeout (needs live peer/input). fs/net demos need live fixtures and
# land in timeout/failed environmentally, not as lowering bugs.
set -uo pipefail
SA_BIN="${SA_BIN:-/content/sa_all/sci/zig-out/bin/sa}"
NODE_BIN="${NODE_BIN:-/tools/node/bin/node}"
DEMO_DIR="${DEMO_DIR:-/content/sa_all/sa_plugin_ts/demos}"
STRIP="${STRIP:-/content/sa_all/sa_plugin_ts/tools/strip_ts.py}"
SAI_DIR="${1:-/tmp/opencode/sweep}"

if [[ ! -d "$SAI_DIR" ]] || [[ -z "$(ls "$SAI_DIR"/*.sai 2>/dev/null)" ]]; then
  echo "error: no .sai files in $SAI_DIR (pass a sweep dir or generate one)" >&2
  exit 2
fi
WORK="$(mktemp -d)"
pass=0; fail=0; skipped=0; upstream=0; failed_names=()
for sai in "$SAI_DIR"/*.sai; do
  dir="$(basename "$sai" .sai)"
  src="$DEMO_DIR/$dir/main.ts"
  [ -f "$src" ] || continue
  if ! "$SA_BIN" build-exe "$sai" -o "$WORK/case.exe" > "$WORK/build.out" 2>&1; then
    echo "FAIL $dir (assemble)"
    grep -o 'error\[[A-Za-z]*\][^,]*' "$WORK/build.out" | head -1 | sed 's/^/    /'
    fail=$((fail+1)); failed_names+=("$dir"); continue
  fi
  oracle=""
  if python3 "$STRIP" "$src" "$WORK/case.mjs" 2>/dev/null; then
    oracle="$("$NODE_BIN" "$WORK/case.mjs" 2>/dev/null)"
  else
    skipped=$((skipped+1)); continue
  fi
  timeout 10 "$WORK/case.exe" > "$WORK/run.out" 2>&1
  rc=$?
  if [ "$rc" = 124 ]; then upstream=$((upstream+1)); continue; fi
  if [ "$rc" = 139 ] || [ "$rc" = 134 ]; then
    echo "FAIL $dir (crash $rc)"; fail=$((fail+1)); failed_names+=("$dir"); continue
  fi
  if [[ "$oracle" =~ ^-?[0-9]+$ ]]; then
    want=$(( (oracle % 256 + 256) % 256 ))
    if [ "$rc" != "$want" ]; then
      echo "FAIL $dir (sa=$rc node=$oracle want=$want)"
      fail=$((fail+1)); failed_names+=("$dir")
    else pass=$((pass+1)); fi
  else
    cat "$WORK/run.out" > "$WORK/want.out"; printf '0' >> "$WORK/want.out"
    if cmp -s "$WORK/want.out" <(printf '%s' "$oracle"); then pass=$((pass+1)); else
      echo "FAIL $dir (output)"; fail=$((fail+1)); failed_names+=("$dir")
    fi
  fi
done
total=$((pass+fail+skipped+upstream))
echo "demos: $total verified: $pass no-oracle: $skipped timeout: $upstream failed: $fail"
[ ${#failed_names[@]} -gt 0 ] && printf 'failing: %s\n' "${failed_names[*]}"
rm -rf "$WORK"
[[ $fail -eq 0 ]]
