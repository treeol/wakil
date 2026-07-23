#!/bin/sh
# check_coverage.sh — per-package coverage floors for damage-critical packages.
#
# Reads `go test -cover` output (the "ok <pkg> <time> coverage: X% of
# statements" lines) from stdin or a file, and fails if any gated package
# dropped below its floor. Global coverage is deliberately NOT gated — the
# floors exist to ratchet the packages where a regression can cause damage
# (gating, execution, streaming), not to chase a vanity number.
#
# Floors are set 1pt below the measured values at introduction
# (test-hardening branch, 2026-07-19): agent 68.3, tools 45.5, exec 38.5,
# proxy 80.2. Raise them (never lower) as coverage improves.
#
# Updated 2026-07-23: tools jumped to 60.3% (google_test.go), exec
# ratcheted to 53.6%, agent to 68.3%. Ratcheted to current−1pt.
#
# Usage:
#   go test -count=1 -cover ./... 2>&1 | scripts/check_coverage.sh
#   go test -count=1 -cover ./... > cover.txt 2>&1; scripts/check_coverage.sh cover.txt
set -eu

input="${1:-/dev/stdin}"

# fpoint10 converts "80.2" to fixed-point tenths (802) using shell only.
fpoint10() {
	int_part="${1%%.*}"
	frac_part="${1#*.}"
	[ "$frac_part" = "$1" ] && frac_part="0"
	# take first digit of the fraction only
	frac_part="${frac_part%"${frac_part#?}"}"
	echo $((int_part * 10 + frac_part))
}

fail=0
check() {
	pkg="$1"
	floor="$2"
	# Match the "ok ... coverage: X% of statements" line for this package.
	pct=$(grep -E "^ok\s+github.com/treeol/wakil/${pkg}\s.*coverage: [0-9.]+%" "$input" \
		| grep -oE "coverage: [0-9.]+%" | grep -oE "[0-9.]+" | head -1)
	if [ -z "$pct" ]; then
		echo "check_coverage: no coverage line found for ${pkg} — did 'go test -cover ./...' run?"
		fail=1
		return
	fi
	# Compare as fixed-point integers (x10) using pure POSIX shell — no bc/awk.
	actual=$(fpoint10 "$pct")
	want=$(fpoint10 "$floor")
	if [ "$actual" -lt "$want" ]; then
		echo "check_coverage: FAIL ${pkg} = ${pct}% (floor ${floor}%)"
		fail=1
	else
		echo "check_coverage: ok   ${pkg} = ${pct}% (floor ${floor}%)"
	fi
}

check "internal/agent" "68.3"
check "internal/tools" "59.3"
# exec floor: Docker-dependent tests (live_docker_test.go) require the
# wakil-dev image. CI pulls it from Docker Hub (see ci.yml "pull sandbox
# image" step); without it, these tests skip and coverage drops. The
# floor was ratcheted at 2026-07-23 when the image became available in CI.
check "internal/exec" "53.6"
check "internal/proxy" "79.2"

if [ "$fail" -ne 0 ]; then
	echo "check_coverage: coverage floor violated — see above"
	exit 1
fi
