#!/bin/sh
# check.sh for rename: builds a clean copy, the old name is gone, the new name
# is used by every caller, and the hidden behaviour test passes (agent-written
# tests are dropped, go.mod restored, imports restricted) — see
# bench/lib/gocheck.sh (which also defines the exit status: 0 pass, 1 fail, 99
# error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_prepare "$tdir" "errors fmt math strconv"
gocheck_go build ./...

# The old name must be gone from every Go source file.
if grep -rn 'OldName' --include='*.go' . >/dev/null 2>&1; then
    echo "old name still present:"; grep -rn 'OldName' --include='*.go' .
    gocheck_fail "the old name is still present"
fi

# The new name is defined, and every caller package uses it.
grep -q 'func NewName' shared/*.go || gocheck_fail "shared.NewName is not defined"
for p in alpha beta gamma delta epsilon zeta; do
    grep -rq 'shared\.NewName' "$p" || gocheck_fail "$p does not call shared.NewName"
done

gocheck_run ./behaviour/ TestBehaviour
