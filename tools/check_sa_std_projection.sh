#!/usr/bin/env bash
# CI check: every Symbol in StdProjectionTable must exist literally in
# $SCI_ROOT/sa_std (*.sai / *.sa / *.sal). The tsgo frontend simulates no
# runtime behavior; this script is the machine-enforced half of that policy
# (the other half is loud refusal at lowering time).
#
# Usage: SCI_ROOT=/content/sa_all/sci tools/check_sa_std_projection.sh
set -uo pipefail

SCI_ROOT="${SCI_ROOT:-}"
if [[ -z "$SCI_ROOT" ]]; then
  echo "error: set SCI_ROOT to the sci checkout (compiler-shipped sa_std)" >&2
  exit 2
fi
STD="$SCI_ROOT/sa_std"
if [[ ! -d "$STD" ]]; then
  echo "error: $STD not found" >&2
  exit 2
fi

# TS surface -> sa_std symbol (must mirror StdProjectionTable in
# internal/saemit/stdlib.go; keep the two lists in sync).
symbols=(
  sa_print_bytes
  sa_fmt_i64_into sa_fmt_f64_into
  sa_string_concat sa_string_from_char_code sa_string_from_code_point
  sa_string_index_of sa_string_last_index_of
  sa_string_starts_with sa_string_ends_with
  sa_string_to_lower_ascii sa_string_to_upper_ascii
  sa_string_repeat sa_string_pad_start sa_string_pad_end sa_string_replace
  sa_parse_float sa_string_code_point_at
  sa_math_sin sa_math_cos sa_math_tan sa_math_asin sa_math_acos sa_math_atan
  sa_math_atan2 sa_math_sinh sa_math_cosh sa_math_tanh sa_math_exp sa_math_expm1
  sa_math_log sa_math_log1p sa_math_log2 sa_math_cbrt sa_math_hypot sa_math_fround
  sa_btree_map_new
  sa_fs_read_file sa_fs_write_file sa_fs_file_open sa_fs_file_create
  sa_fs_file_close sa_fs_file_read sa_fs_file_write
  sa_fs_remove_file sa_fs_make_dir
  sa_net_tcp_connect sa_net_tcp_listener_bind sa_net_tcp_listener_accept
  sa_net_tcp_stream_read sa_net_tcp_stream_write sa_net_tcp_stream_close
)

fail=0
for sym in "${symbols[@]}"; do
  if grep -rq --include='*.sai' --include='*.sa' --include='*.sal' \
      -e "@extern $sym(" -e "@export $sym(" "$STD"; then
    echo "ok: $sym"
  else
    echo "MISSING: $sym (no @extern/@export in \$SCI_ROOT/sa_std)"
    fail=$((fail + 1))
  fi
done

if [[ "$fail" -ne 0 ]]; then
  echo "FAIL: $fail symbol(s) missing from sci/sa_std" >&2
  exit 1
fi
echo "PASS: all ${#symbols[@]} symbols resolve in sci/sa_std"
