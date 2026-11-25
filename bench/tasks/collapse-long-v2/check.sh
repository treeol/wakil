#!/bin/sh
# check.sh for collapse-long-v2: report.json holds the expected per-category
# sums (parsed JSON comparison: key order and whitespace do not matter,
# extra keys fail). Exit status: see bench/lib/gocheck.sh (0 pass, 1
# fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_init "$tdir"
command -v python3 >/dev/null 2>&1 || gocheck_error "python3 is not installed"
test -f report.json || gocheck_fail "report.json missing"
rc=0
python3 - <<'PYEOF' || rc=$?
import json, sys
expected = {"alpha":1324,"beta":1195,"delta":1350,"epsilon":1262,"gamma":1159,"zeta":1074}
try:
    got = json.load(open("report.json"))
except (OSError, ValueError) as e:
    print("report.json is not valid JSON:", e); sys.exit(3)
if got != expected:
    print("mismatch: got", got); print("want", expected); sys.exit(3)
print("sums correct")
PYEOF
case "$rc" in
    0) ;;
    3) gocheck_fail "report.json does not hold the expected sums" ;;
    *) gocheck_error "python3 ended with status $rc" ;;
esac
