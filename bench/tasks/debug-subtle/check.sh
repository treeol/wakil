#!/bin/sh
# check.sh for debug-subtle: the hidden tests pass, and the fix is minimal
# (at most 6 changed lines total, counted on gofmt-normalized files; the
# reference fix is 5). Exit status: see bench/lib/gocheck.sh (0 pass,
# 1 fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_prepare "$tdir" "reflect testing"

# The hidden tests judge the fix (they call Take and windowTake directly).
gocheck_run ./window/ TestTakeCases TestHelperCases

# Minimal-fix rule: diff the agent tree against the pristine repo tree.
# Both sides are gofmt-normalized first, so formatting churn does not
# count; both sides have all *_test.go removed by gocheck_prepare (the
# agent tree) and here (the pristine copy). New/deleted .go files count
# in full. Counted with diff -U0 on the whole trees: the reference fix
# is 5 changed lines (a 4-line loop replaced by 1), so the budget of 6
# covers the fix plus a one-line guard elsewhere. A bloated fix (extra
# helper, rewritten package) exceeds it. N = 6.
_pristine="$GC_TMP/pristine"
mkdir "$_pristine"
cp -R "$GC_TDIR/repo/." "$_pristine/"
# Both sides hold no tests: the agent tree lost its *_test.go in
# gocheck_prepare, and gocheck_run then copied the task's hidden tests
# into it — they are the check's own files, not the agent's changes.
find "$GC_TMP/w" -type f -name '*_test.go' -delete
find "$_pristine" -type f -name '*_test.go' -delete
find "$_pristine" -type f -name '*.go' | sort | while IFS= read -r _f; do
    gofmt -w "$_f" 2>/dev/null || true
done
find "$GC_TMP/w" -type f -name '*.go' | sort | while IFS= read -r _f; do
    gofmt -w "$_f" 2>/dev/null || true
done
# Union of .go paths in both trees.
{ find "$_pristine" -type f -name '*.go'; find "$GC_TMP/w" -type f -name '*.go'; } | sort -u | while IFS= read -r _f; do
    printf '%s\n' "${_f#"$_pristine"/}"
done | sort -u > "$GC_TMP/paths.txt"
# The loop above strips the wrong prefix for agent-tree paths; rebuild
# the union by relative name properly.
: > "$GC_TMP/paths.txt"
find "$_pristine" -type f -name '*.go' | while IFS= read -r _f; do printf '%s\n' "${_f#"$_pristine"/}"; done >> "$GC_TMP/paths.txt"
find "$GC_TMP/w" -type f -name '*.go' | while IFS= read -r _f; do printf '%s\n' "${_f#"$GC_TMP/w"/}"; done >> "$GC_TMP/paths.txt"
sort -u "$GC_TMP/paths.txt" -o "$GC_TMP/paths.txt"
_changed=0
while IFS= read -r _rel; do
    _o="$_pristine/$_rel"
    _n="$GC_TMP/w/$_rel"
    if [ ! -f "$_o" ] || [ ! -f "$_n" ]; then
        # A new or deleted file counts in full (lines present on one side).
        for _side in "$_o" "$_n"; do
            [ ! -f "$_side" ] || _changed=$((_changed + $(wc -l < "$_side")))
        done
        continue
    fi
    cmp -s "$_o" "$_n" && continue
    _c=$(diff -U0 "$_o" "$_n" 2>/dev/null | awk '
/^@@/{next}
/^---/{next}
/^\+\+\+/{next}
/^\+/{a++; next}
/^-/{r++; next}
END{print a+r+0}')
    _changed=$((_changed + _c))
done < "$GC_TMP/paths.txt"
if [ "$_changed" -gt 6 ]; then
    gocheck_fail "fix is not minimal: $_changed changed lines (at most 6 allowed)"
fi
echo "check: minimal fix ($_changed changed lines)"
