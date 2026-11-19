#!/bin/sh
# check.sh for add-flag: builds the command and runs it with three inputs.
set -eu
go build -o .grep-bin ./cmd/grep

input='alpha
beta gamma
gamma beta
delta'

# 1: without -n, matching lines only.
out=$(printf '%s\n' "$input" | ./.grep-bin beta)
test "$out" = 'beta gamma
gamma beta' || { echo "no-flag: got $out"; exit 1; }

# 2: with -n, line numbers prefix matches.
out=$(printf '%s\n' "$input" | ./.grep-bin -n beta)
test "$out" = '2:beta gamma
3:gamma beta' || { echo "with -n: got $out"; exit 1; }

# 3: unknown flag is an error.
if printf '%s\n' "$input" | ./.grep-bin -x beta >/dev/null 2>&1; then
    echo "unknown flag: expected exit 3"; exit 1
fi

# Also: flag after the search string still works (defined behaviour).
out=$(printf '%s\n' "$input" | ./.grep-bin beta -n)
test "$out" = '2:beta gamma
3:gamma beta' || { echo "flag after string: got $out"; exit 1; }

rm -f ./.grep-bin
