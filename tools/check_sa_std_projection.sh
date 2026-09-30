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
# NOTE: Math trig/exp/log/hypot/fround are deliberately ABSENT here and in
# the table: both frontends refuse them loudly per the reference lowerer
# (stdlib.go documents why projecting them would assemble the wrong
# dialect). Never re-add sa_math_* without an emitter that emits them.
symbols=(
  sa_print_bytes
  sa_fmt_i64_into sa_fmt_f64_into
  sa_string_concat sa_string_from_char_code sa_string_from_code_point
  sa_string_index_of sa_string_last_index_of
  sa_string_starts_with sa_string_ends_with
  sa_string_to_lower_ascii sa_string_to_upper_ascii
  sa_string_repeat sa_string_pad_start sa_string_pad_end sa_string_replace
  sa_parse_float sa_string_code_point_at
  sa_btree_map_new
  sa_btree_map_insert sa_btree_map_get sa_btree_map_contains_key
  sa_btree_map_remove sa_btree_map_len sa_btree_map_clear
  sa_btree_map_keys_set sa_btree_map_values_vec sa_btree_map_iter_vec
  sa_btree_set_new sa_btree_set_insert sa_btree_set_contains
  sa_btree_set_remove sa_btree_set_len sa_btree_set_clear
  sa_fs_read_file sa_fs_write_file sa_fs_file_open sa_fs_file_create
  sa_fs_file_close sa_fs_file_read sa_fs_file_write
  sa_fs_remove_file sa_fs_make_dir
  sa_net_tcp_connect sa_net_tcp_listener_bind sa_net_tcp_listener_accept
  sa_net_tcp_stream_read sa_net_tcp_stream_write sa_net_tcp_stream_close
  sa_time_unix_ms
  sa_time_iso_from_unix_ms
  sa_time_parse_iso
  sa_time_get_full_year sa_time_get_month sa_time_get_date
  sa_time_get_hours sa_time_get_minutes sa_time_get_seconds
  sa_time_get_milliseconds sa_time_get_day
  sa_time_set_field
  sa_time_format_utc
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

# ---- plugin backends -------------------------------------------------------
# Entries with Backend: "node"/"deno"/"bun" verify against the plugin's own
# exported-symbols contract (exact match in all_exported_symbols.txt where
# present, else @extern in the plugin .sai).
# Usage: SA_PLUGINS_ROOT=/content/sa_all tools/check_sa_std_projection.sh
node_symbols=(
  sa_node_plugin_os_platform
  sa_node_plugin_os_arch
  sa_node_plugin_os_homedir
  sa_node_plugin_os_tmpdir
  sa_node_plugin_os_hostname
  sa_node_plugin_os_release
  sa_node_plugin_os_type
  sa_node_plugin_os_endianness
  sa_node_plugin_os_machine
  sa_node_plugin_os_cpus
  sa_node_plugin_os_version
  sa_node_plugin_os_user_info
  sa_node_plugin_os_network_interfaces
  sa_node_plugin_process_cwd
  sa_node_plugin_crypto_random_uuid
  sa_node_plugin_path_normalize
  sa_node_plugin_path_dirname
  sa_node_plugin_path_extname
  sa_node_plugin_path_join
  sa_node_plugin_path_resolve
  sa_node_plugin_crypto_random_bytes
  sa_node_plugin_crypto_hash
  sa_node_plugin_crypto_hmac
  sa_node_plugin_querystring_escape
  sa_node_plugin_querystring_unescape
  sa_node_plugin_querystring_parse
  sa_node_plugin_querystring_stringify
  sa_node_plugin_url_parse
  sa_node_plugin_url_format
  sa_node_plugin_url_resolve
  sa_node_plugin_util_strip_vt_control_characters
  sa_node_plugin_console_error
  sa_node_plugin_console_time
  sa_node_plugin_console_time_end
  sa_node_plugin_console_clear
  sa_node_plugin_buffer_byte_length
  sa_node_plugin_buffer_concat
)

if [[ ${#node_symbols[@]} -gt 0 ]]; then
  PLUGINS_ROOT="${SA_PLUGINS_ROOT:-}"
  if [[ -z "$PLUGINS_ROOT" ]]; then
    echo "SKIP: node backend check needs SA_PLUGINS_ROOT (sa_plugin_node checkout)" >&2
  else
    NODE="$PLUGINS_ROOT/sa_plugin_node"
    nfail=0
    for sym in "${node_symbols[@]}"; do
      if [[ -f "$NODE/all_exported_symbols.txt" ]] && grep -qx "$sym" "$NODE/all_exported_symbols.txt"; then
        echo "ok: $sym (node)"
      elif grep -rq --include='*.sai' -e "@extern $sym(" "$NODE"; then
        echo "ok: $sym (node .sai)"
      else
        echo "MISSING: $sym (no export in sa_plugin_node)"
        nfail=$((nfail + 1))
      fi
    done
    if [[ "$nfail" -ne 0 ]]; then
      echo "FAIL: $nfail node symbol(s) missing" >&2
      exit 1
    fi
    echo "PASS: all ${#node_symbols[@]} node symbols resolve"
  fi
fi

# Deno backend: no exported-symbols list exists upstream, so deno.sai is
# the contract (Zig source impls verified by hand for each listed symbol).
deno_symbols=(
  sa_deno_plugin_hostname
  sa_deno_plugin_os_release
  sa_deno_plugin_read_text_file
  sa_deno_plugin_write_text_file
  sa_deno_plugin_env_get
  sa_deno_plugin_env_set
  sa_deno_plugin_env_delete
)

if [[ ${#deno_symbols[@]} -gt 0 ]]; then
  PLUGINS_ROOT="${SA_PLUGINS_ROOT:-}"
  if [[ -z "$PLUGINS_ROOT" ]]; then
    echo "SKIP: deno backend check needs SA_PLUGINS_ROOT (sa_plugin_deno checkout)" >&2
  else
    DENO="$PLUGINS_ROOT/sa_plugin_deno"
    dfail=0
    for sym in "${deno_symbols[@]}"; do
      if grep -rq --include='*.sai' -e "@extern $sym(" "$DENO"; then
        echo "ok: $sym (deno)"
      else
        echo "MISSING: $sym (no extern in sa_plugin_deno)"
        dfail=$((dfail + 1))
      fi
    done
    if [[ "$dfail" -ne 0 ]]; then
      echo "FAIL: $dfail deno symbol(s) missing" >&2
      exit 1
    fi
    echo "PASS: all ${#deno_symbols[@]} deno symbols resolve"
  fi
fi
