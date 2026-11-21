#!/bin/sh
# check.sh for add-flag: builds the command and runs it with three inputs.
# Exit status: see bench/lib/gocheck.sh (0 pass, 1 fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_init "$tdir"
gocheck_env

# The binary is built outside the work folder (no artifact is left in it).
GC_BIN="$GC_TMP/grep-bin"
gocheck_go build -o "$GC_BIN" ./cmd/grep

input='alpha
beta gamma
gamma beta
delta'
printf '%s\n' "$input" > "$GC_TMP/input.txt"

# 1: without -n, matching lines only.
gocheck_bin "$GC_TMP/input.txt" beta
test "$GC_BIN_OUT" = 'beta gamma
gamma beta' || gocheck_fail "no-flag: got $GC_BIN_OUT"

# 2: with -n, line numbers prefix matches.
gocheck_bin "$GC_TMP/input.txt" -n beta
test "$GC_BIN_OUT" = '2:beta gamma
3:gamma beta' || gocheck_fail "with -n: got $GC_BIN_OUT"

# 3: unknown flag is an error.
gocheck_bin "$GC_TMP/input.txt" -x beta
if [ "$GC_BIN_RC" -eq 0 ]; then
    gocheck_fail "unknown flag: expected a non-zero exit"
fi

# Also: flag after the search string still works (defined behaviour).
gocheck_bin "$GC_TMP/input.txt" beta -n
test "$GC_BIN_OUT" = '2:beta gamma
3:gamma beta' || gocheck_fail "flag after string: got $GC_BIN_OUT"
