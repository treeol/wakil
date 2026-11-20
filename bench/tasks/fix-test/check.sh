#!/bin/sh
# check.sh for fix-test: runs the hidden test against a clean copy of the
# package (agent-written tests are dropped, go.mod restored, imports of the
# package restricted) — see bench/lib/gocheck.sh.
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_prepare "$tdir" "bytes cmp errors fmt maps math regexp slices sort strconv strings unicode unicode/utf8"
gocheck_run ./wordcount/ TestCountCaseInsensitive
