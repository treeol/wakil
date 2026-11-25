#!/bin/sh
# check.sh for debug-subtle: the hidden tests pass, and the fix is minimal
# under the fair rule: changed lines are counted (gofmt-normalized, hidden
# tests removed first) only OUTSIDE the windowTake window (the function plus
# its contiguous doc-comment block above); inside the window any size is
# allowed. Outside: at most 2 changed lines. In total: at most 30 changed
# lines (stops a whole-file rewrite). The reference fix is 5 changed lines,
# all inside. Agent-written *_test.go files are excluded from the diff
# entirely (they are deleted by gocheck_prepare before the diff). Exit
# status: see bench/lib/gocheck.sh (0 pass, 1 fail, 99 error).
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
# in full as outside. Removed lines classify by their OLD-file line
# number; added lines by their NEW-file line number (inside iff inside
# that side's windowTake window range). A hunk spanning the boundary is
# split per-line. An insertion immediately after the closing brace is
# outside; a contiguous comment above func windowTake is inside.
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
# WindowTake window range per file: awk prints START END for the extended
# window (contiguous // comment lines directly above ^func windowTake,
# through the matching ^} line). One range per file; files without the
# function get range 0 0 (every line outside).
window_range() {
    # $1 = file path; prints "start end" (1-based, inclusive) or "0 0".
    awk '
    /^func windowTake\(/ { fstart = NR; next }
    /^\/\// && fstart == 0 { start = NR; last = NR; next }
    /^}/ && fstart > 0 { printf "%d %d\n", (last > 0 ? last : fstart), NR; found = 1; exit }
    END { if (!found) print "0 0" }
    ' "$1"
}
# Union of .go paths in both trees.
: > "$GC_TMP/paths.txt"
find "$_pristine" -type f -name '*.go' | while IFS= read -r _f; do printf '%s\n' "${_f#"$_pristine"/}"; done >> "$GC_TMP/paths.txt"
find "$GC_TMP/w" -type f -name '*.go' | while IFS= read -r _f; do printf '%s\n' "${_f#"$GC_TMP/w"/}"; done >> "$GC_TMP/paths.txt"
sort -u "$GC_TMP/paths.txt" -o "$GC_TMP/paths.txt"
_changed=0
_outside=0
while IFS= read -r _rel; do
    _o="$_pristine/$_rel"
    _n="$GC_TMP/w/$_rel"
    if [ ! -f "$_o" ] || [ ! -f "$_n" ]; then
        # A new or deleted file counts in full, all as outside.
        for _side in "$_o" "$_n"; do
            [ ! -f "$_side" ] || _nlines=$(wc -l < "$_side") || true
            [ ! -f "$_side" ] || { _changed=$((_changed + _nlines)); _outside=$((_outside + _nlines)); }
        done
        continue
    fi
    cmp -s "$_o" "$_n" && continue
    _or=$(window_range "$_o")
    _nr=$(window_range "$_n")
    _os=${_or%% *}; _oe=${_or##* }
    _ns=${_nr%% *}; _ne=${_nr##* }
    _counts=$(diff -U0 "$_o" "$_n" 2>/dev/null | awk -v os="$_os" -v oe="$_oe" -v ns="$_ns" -v ne="$_ne" '
    /^@@ / {
        # Hunk header: @@ -a,b +c,d @@ — only -a and +c matter.
        line = $0
        sub(/^@@ -/, "", line)
        old_pos = line + 0
        sub(/.*\+/, "", line)
        sub(/,.*/, "", line)
        new_pos = line + 0
        next
    }
    /^---/ { next }
    /^\+\+\+/ { next }
    /^\+/ { a++; if (ns == 0 || new_pos < ns || new_pos > ne) out_a++; new_pos++; next }
    /^-/ { r++; if (os == 0 || old_pos < os || old_pos > oe) out_r++; old_pos++; next }
    END { print (a+r), (out_a+out_r) }')
    _c=${_counts%% *}; _co=${_counts##* }
    _changed=$((_changed + _c))
    _outside=$((_outside + _co))
done < "$GC_TMP/paths.txt"
if [ "$_outside" -gt 2 ]; then
    gocheck_fail "fix touches $_outside lines outside windowTake (at most 2 allowed)"
fi
if [ "$_changed" -gt 30 ]; then
    gocheck_fail "fix is not minimal: $_changed changed lines (at most 30 allowed)"
fi
echo "check: minimal fix ($_changed changed lines, $_outside outside windowTake)"
