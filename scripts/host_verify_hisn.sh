#!/bin/sh
# host_verify_hisn.sh — the hisn version helpers of scripts/host_verify.sh.
# Sourced, not run. host_verify_hisn_test.sh covers it with stub binaries.
#
# It never installs anything and never runs a binary beyond `--version`.

# version_ge: return 0 (true) if version $1 >= $2, comparing numerically
# per component (major, minor, patch). Ignores any suffix after the patch
# number. No string comparison — only integer -lt/-gt/-eq. The sed keeps
# only [0-9.] characters after stripping an optional "hisn " prefix, so
# "0.6.0-dev1" → "0.6.0" and "0.10.0" → "0.10.0".
version_ge() {
  a=$(printf '%s' "$1" | sed 's/^hisn //; s/[^0-9.].*$//')
  b=$(printf '%s' "$2" | sed 's/^hisn //; s/[^0-9.].*$//')
  a1=$(printf '%s' "$a" | cut -d. -f1); a2=$(printf '%s' "$a" | cut -d. -f2); a3=$(printf '%s' "$a" | cut -d. -f3)
  b1=$(printf '%s' "$b" | cut -d. -f1); b2=$(printf '%s' "$b" | cut -d. -f2); b3=$(printf '%s' "$b" | cut -d. -f3)
  a1=${a1:-0}; a2=${a2:-0}; a3=${a3:-0}
  b1=${b1:-0}; b2=${b2:-0}; b3=${b3:-0}
  if [ "$a1" -gt "$b1" ]; then return 0; fi
  if [ "$a1" -lt "$b1" ]; then return 1; fi
  if [ "$a2" -gt "$b2" ]; then return 0; fi
  if [ "$a2" -lt "$b2" ]; then return 1; fi
  if [ "$a3" -ge "$b3" ]; then return 0; fi
  return 1
}

# hv_hisn_floor_line <found> <required> <path>: print nothing when found >=
# required, else one line naming both versions, the binary and where to get
# the required release. The release URL is the hisn project's own tag, not
# Wakil's install.sh: that script is pinned to the hisn of the latest Wakil
# release, which may be below the floor, and it would not displace an older
# hisn earlier on PATH.
hv_hisn_floor_line() {
  if version_ge "$1" "$2"; then
    return 0
  fi
  printf 'hisn %s at %s is below the required %s; get hisn %s from https://github.com/treeol/hisn/releases/tag/v%s (the asset for your architecture and SHA256SUMS), put it first on PATH or set WAKIL_HISN_BIN\n' \
    "$1" "$3" "$2" "$2" "$2"
}

# hv_hisn_floor_check <required>: check both binaries a later step can use —
# WAKIL_HISN_BIN when it is set (the go-test legs) and `hisn` on PATH (the
# real-hisn step always takes PATH). Prints one hv_hisn_floor_line per binary
# below the floor. Returns 1 when one is below it, 77 when there is no hisn
# to check, 0 otherwise. A version string that does not parse is not a floor
# failure: the real-hisn step reports it.
hv_hisn_floor_check() {
  hv_req=$1
  hv_bins=""
  if [ -n "${WAKIL_HISN_BIN:-}" ] && [ -x "$WAKIL_HISN_BIN" ]; then
    hv_bins="$WAKIL_HISN_BIN"
  fi
  hv_path_bin=$(command -v hisn 2>/dev/null || true)
  if [ -n "$hv_path_bin" ] && [ "$hv_path_bin" != "$hv_bins" ]; then
    hv_bins="$hv_bins $hv_path_bin"
  fi
  if [ -z "${hv_bins# }" ]; then
    return 77
  fi
  hv_rc=0
  for hv_bin in $hv_bins; do
    hv_out=$("$hv_bin" --version 2>&1 </dev/null | head -1) || hv_out=""
    hv_found=$(printf '%s' "$hv_out" | sed -n 's/^hisn \([0-9][0-9.]*\).*/\1/p')
    if [ -z "$hv_found" ]; then
      continue
    fi
    if ! version_ge "$hv_found" "$hv_req"; then
      hv_hisn_floor_line "$hv_found" "$hv_req" "$hv_bin"
      hv_rc=1
    fi
  done
  return "$hv_rc"
}
