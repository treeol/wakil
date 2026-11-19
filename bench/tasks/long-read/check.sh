#!/bin/sh
# check.sh for long-read: result.json matches the expected per-category sums.
set -eu
test -f result.json || { echo "result.json missing"; exit 1; }
python3 - <<'PYEOF'
import json, sys
expected = {"alpha":157,"beta":146,"delta":123,"epsilon":191,"gamma":218,"zeta":153}
got = json.load(open("result.json"))
if got != expected:
    print("mismatch: got", got); print("want", expected); sys.exit(1)
print("sums correct")
PYEOF
