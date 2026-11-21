#!/bin/sh
# check.sh for steps: all six artifacts (with a trailing newline allowed)
# hold the stated content. Exit status: see bench/lib/gocheck.sh (0 pass, 1
# fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_init "$tdir"
fail=0
expect() {
    f=$1; want=$2
    if [ ! -f "$f" ]; then
        echo "$f: missing"; fail=1; return
    fi
    got=$(cat "$f")
    if [ "$got" != "$want" ]; then
        echo "$f: got '$got', want '$want'"; fail=1
    fi
}
expect step1.txt 1
expect step2.txt final
expect step3.txt 11
expect step4.txt six
expect step5.txt 100
expect step6.txt last
[ "$fail" -eq 0 ] || gocheck_fail "an artifact is missing or wrong"
