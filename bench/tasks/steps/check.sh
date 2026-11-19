#!/bin/sh
# check.sh for steps: all six artifacts (with a trailing newline allowed)
# hold the stated content.
set -eu
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
exit $fail
