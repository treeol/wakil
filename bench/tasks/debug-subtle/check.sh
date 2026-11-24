#!/bin/sh
# check.sh for debug-subtle: the hidden tests pass, and the fix is minimal
# (at most 4 changed lines outside the buggy helper windowTake, counted on
# gofmt-normalized files by old-file hunk coordinates). Exit status: see
# bench/lib/gocheck.sh (0 pass, 1 fail, 99 error).
set -eu
tdir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=../../lib/gocheck.sh
. "$tdir/../../lib/gocheck.sh"
gocheck_prepare "$tdir" "reflect testing"

# The hidden tests judge the fix (they call Take and windowTake directly).
gocheck_run ./window/ TestTakeCases TestHelperCases

# Minimal-fix rule: diff the agent tree against the pristine repo tree.
# Both sides are gofmt-normalized first, so formatting churn does not
# count. The buggy function (windowTake) is excised from both sides; the
# remaining diff may change at most 4 lines (added + removed, by old-file
# hunk coordinates). The agent tree here is $GC_TMP/w (already test-free).
_pristine="$GC_TMP/pristine"
mkdir "$_pristine"
cp -R "$GC_TDIR/repo/." "$_pristine/"
cp "$GC_TDIR/repo/go.mod" "$_pristine/go.mod" 2>/dev/null || true
# Normalize and excise in both trees.
for _d in "$_pristine" "$GC_TMP/w"; do
    find "$_d" -type f -name '*.go' | sort | while IFS= read -r _f; do
        gofmt -w "$_f" 2>/dev/null || true
    done
    # Excise windowTake: from "^func windowTake" to the next "^}" inclusive.
    find "$_d" -type f -name '*.go' | sort | while IFS= read -r _f; do
        if grep -q '^func windowTake' "$_f" 2>/dev/null; then
            awk '/^func windowTake/{skip=1} skip && /^\}/{print "}"; skip=0; next} !skip{print}' "$_f" > "$_f.tmp"
            mv "$_f.tmp" "$_f"
        fi
    done
done
# Count changed lines outside the function (old-file coordinates). A
# deleted or new .go file counts in full.
_changed=0
_old="$_pristine/window/window.go"
_new="$GC_TMP/w/window/window.go"
if [ -f "$_old" ] && [ -f "$_new" ]; then
    _changed=$(diff -U0 "$_old" "$_new" 2>/dev/null | awk '
/^@@/{next}
/^---/{next}
/^\+\+\+/{next}
/^\+/{a++; next}
/^-/{r++; next}
END{print a+r+0}')
fi
# Any other .go file that differs (or is new/deleted) counts in full.
find "$_pristine" -type f -name '*.go' | sort | while IFS= read -r _f; do
    _rel=${_f#"$_pristine"/}
    _n="$GC_TMP/w/$_rel"
    if [ "$_rel" != window/window.go ]; then
        if [ ! -f "$_n" ] || ! cmp -s "$_f" "$_n"; then
            echo "$_rel differs (counts as changed)"
        fi
    fi
done > "$GC_TMP/changed.txt" 2>/dev/null || true
if [ -s "$GC_TMP/changed.txt" ]; then
    while IFS= read -r _line; do
        _rel=${_line% differs*}
        if [ -f "$_pristine/$_rel" ]; then
            _changed=$((_changed + $(wc -l < "$_pristine/$_rel")))
        fi
        if [ -f "$GC_TMP/w/$_rel" ]; then
            _changed=$((_changed + $(wc -l < "$GC_TMP/w/$_rel")))
        fi
    done < "$GC_TMP/changed.txt"
fi
find "$GC_TMP/w" -type f -name '*.go' | sort | while IFS= read -r _f; do
    _rel=${_f#"$GC_TMP/w"/}
    if [ ! -f "$_pristine/$_rel" ]; then
        echo "$_rel new"
    fi
done > "$GC_TMP/newfiles.txt" 2>/dev/null || true
if [ -s "$GC_TMP/newfiles.txt" ]; then
    while IFS= read -r _line; do
        _rel=${_line% new}
        _changed=$((_changed + $(wc -l < "$GC_TMP/w/$_rel")))
    done < "$GC_TMP/newfiles.txt"
fi
if [ "$_changed" -gt 4 ]; then
    gocheck_fail "fix is not minimal: $_changed changed lines outside windowTake (at most 4 allowed)"
fi
echo "check: minimal fix ($_changed changed lines outside windowTake)"
