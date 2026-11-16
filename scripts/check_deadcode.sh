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
# stderr is kept out of the report: `go run` writes "go: downloading ..."
# lines to stderr on a cold module cache, and the loop below treated
# those as unreachable entries that must be allow-listed. Only stdout is
# the analyzer report. A real analyzer failure (non-zero exit) is also
# fatal — without the `|| rc=$?` guard, `set -e` aborts on it before
# the exit-code branch below can print a useful message, so the
# deadcode_rc branch was unreachable; use the guarded form.
errfile=$(mktemp "${TMPDIR:-/tmp}/deadcode-err.XXXXXXXX")
trap 'rm -f "$report" "$errfile"' EXIT
deadcode_rc=0
go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./... >"$report" 2>"$errfile" || deadcode_rc=$?

if [ "$deadcode_rc" -ne 0 ]; then
	echo "check_deadcode: deadcode analyzer failed (exit $deadcode_rc):" >&2
	cat "$report" "$errfile" >&2
	exit 1
fi

# A clean report has no lines matching the "path:symbol" pattern.
# Match only lines containing "unreachable func:" so non-report lines
# (e.g. a future go-run note on stdout) are not treated as entries.
if ! grep -q 'unreachable func:' "$report"; then
	echo "check_deadcode: deadcode clean, nothing to allow"
	exit 0
fi

fail=0
while IFS= read -r line; do
	case "$line" in
		"" | \#*) continue ;;
	esac
	case "$line" in
		*"unreachable func:"*) ;;
		*) continue ;;
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
