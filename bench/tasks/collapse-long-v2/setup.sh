#!/bin/sh
# setup.sh for collapse-long-v2: generate the 40 prose files (fixed seed)
# into the work copy's docs/. Runs with the work copy as the working
# directory, BEFORE the work folder's first commit. The generator and its
# seed live in gen/ next to this file, outside repo/, so they are never
# copied into the work folder and never appear in its git history. The
# answer key is written beside docs/ (never in it) for the self-test tie.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$here/../../lib/gocheck.sh"
gocheck_env
go run -C "$here/gen" . "$PWD/docs"
