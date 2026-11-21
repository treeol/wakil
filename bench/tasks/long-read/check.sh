#!/bin/sh
# check.sh for long-read: result.json matches the expected per-category sums.
# Exit status: see bench/lib/gocheck.sh (0 pass, 1 fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_init "$tdir"
command -v python3 >/dev/null 2>&1 || gocheck_error "python3 is not installed"
test -f result.json || gocheck_fail "result.json missing"
# The script exits 0 (match), 1 (mismatch or unreadable JSON) or crashes; only
# the first two are judgements of the agent's file.
rc=0
python3 - <<'PYEOF' || rc=$?
import json, sys
expected = {"alpha":157,"beta":146,"delta":123,"epsilon":191,"gamma":218,"zeta":153}
try:
    got = json.load(open("result.json"))
except (OSError, ValueError) as e:
    print("result.json is not valid JSON:", e); sys.exit(1)
if got != expected:
    print("mismatch: got", got); print("want", expected); sys.exit(1)
print("sums correct")
PYEOF
case "$rc" in
    0) ;;
    1) gocheck_fail "result.json does not hold the expected sums" ;;
    *) gocheck_error "python3 ended with status $rc" ;;
esac
