#!/bin/sh
# check_deadcode.sh — keep unreachable code from growing back.
#
# Runs deadcode (golang.org/x/tools v0.52.0) with test files as roots and
# fails when it reports a function that is neither reachable nor allow-listed
# in scripts/deadcode_allow.txt. The allow file holds test-support helpers,
# intended seams and API surfaces that are documented as deliberately
# unreachable from today's call graph.
#
# Usage:
#   scripts/check_deadcode.sh
#   (run from repo root)
set -eu

allow="scripts/deadcode_allow.txt"

if [ ! -f "$allow" ]; then
	echo "check_deadcode: $allow not found (run from repo root)" >&2
	exit 1
fi

report=$(mktemp "${TMPDIR:-/tmp}/deadcode-report.XXXXXXXX")
trap 'rm -f "$report"' EXIT

# deadcode exits 0 on success, non-zero on analyzer failure. We must
# distinguish "analyzer ran and found nothing" from "analyzer crashed".
# A real report has lines like "file.go:LINE:COL: unreachable func: Name".
go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./... >"$report" 2>&1
deadcode_rc=$?

if [ "$deadcode_rc" -ne 0 ]; then
	echo "check_deadcode: deadcode analyzer failed (exit $deadcode_rc):" >&2
	cat "$report" >&2
	exit 1
fi

# A clean report has no lines matching the "path:symbol" pattern.
if ! grep -Eq '^[^ ]+\.go:[0-9]+:[0-9]+: .* ' "$report"; then
	echo "check_deadcode: deadcode clean, nothing to allow"
	exit 0
fi

fail=0
while IFS= read -r line; do
	case "$line" in
		"" | \#*) continue ;;
	esac
	# deadcode prints one entry per line: "file.go:LINE:COL: unreachable func: Name".
	# Match on the "package.Symbol" suffix (after "unreachable func: ") so
	# a line-number shift between allow-list generation and CI does not cause drift.
	symbol=${line##*unreachable func: }
	symbol=${symbol%% *}
	# symbol is like "pkg.Func". The allow file stores "pkg.Func — reason".
	allow_symbol=$(awk -F' — ' '{print $1}' "$allow" | grep -Fx "$symbol" || true)
	if [ -z "$allow_symbol" ]; then
		echo "check_deadcode: new unreachable entry not in $allow: $line" >&2
		fail=1
	fi
done <"$report"

if [ "$fail" -ne 0 ]; then
	echo "check_deadcode: add the entry to $allow with a reason, or remove the code" >&2
	exit 1
fi

echo "check_deadcode: only allow-listed unreachable entries remain"
