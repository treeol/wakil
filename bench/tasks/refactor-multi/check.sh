#!/bin/sh
# check.sh for refactor-multi: builds a clean copy, the old name is gone
# from non-test code (comments excluded), the new name is defined and used
# by every caller, and the hidden behaviour tests pass (agent-written
# tests are dropped, go.mod restored, imports restricted) — see
# bench/lib/gocheck.sh (which also defines the exit status: 0 pass, 1
# fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_prepare "$tdir" "errors fmt math strconv testing"
gocheck_go build ./...

# The old name must be gone from every non-test Go source file (a comment
# mentioning it does not fail; dead code does, via build/hidden tests).
if grep -rn 'OldAdd' --include='*.go' . | grep -v '^[^:]*:[0-9]*:[[:space:]]*//' >/dev/null 2>&1; then
    echo "old name still present in code:"
    grep -rn 'OldAdd' --include='*.go' . | grep -v '^[^:]*:[0-9]*:[[:space:]]*//'
    gocheck_fail "the old name is still present in code"
fi

# The new name is defined, and every caller package uses it.
grep -q 'func NewSum' shared/*.go || gocheck_fail "shared.NewSum is not defined"
for p in p01 p02 p03 p04 p05 p06 p07 p08; do
    grep -rq 'shared\.NewSum' "$p" || gocheck_fail "$p does not call shared.NewSum"
done

gocheck_run ./shared/ TestNewSum
for p in p01 p02 p03 p04 p05 p06 p07 p08; do
    gocheck_run "./$p/" TestTotalHidden
done
