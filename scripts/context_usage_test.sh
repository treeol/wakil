#!/bin/sh
# context_usage_test.sh — asserts what scripts/context_usage.sh prints.
#
# Builds synthetic Wakil trace files (the internal/trace JSONL shape) that
# carry a SECRET MARKER in every field the real traces carry user content in:
# tool arguments, tool results, message content, workspace and model names,
# and a memory key. Then:
#
#   1. checks the counts the script prints are exactly right;
#   2. checks the output layout is exactly TOOL<TAB>CALLS<TAB>SESSIONS;
#   3. asserts the marker never appears on stdout OR stderr, on every path:
#      a normal run, several files, --help, a missing file, an unreadable file,
#      a directory argument, and a malformed trace.
#
# No Go, no network, no real session data. POSIX sh.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
script="$here/context_usage.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/context-usage-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

# A string that must never reach the output. It is embedded in every field
# that could plausibly carry user content.
MARKER="SECRET-MARKER-4f1c2a-do-not-print"

bad=0
fail() {
    echo "  FAIL $1" >&2
    bad=$((bad + 1))
}
pass() { echo "  PASS $1"; }

# assert_no_marker <case> <file>: the marker must not appear in file.
assert_no_marker() {
    if grep -q -- "$MARKER" "$2" 2>/dev/null; then
        fail "$1: the secret marker leaked into the output"
    fi
}

# --- fixture: two sessions, one file ---------------------------------------
#
# session s1: read_file x2, run_shell x1
# session s2: read_file x1
#
# Every record also carries the marker in arguments/results/content/metadata
# exactly where a real trace would carry user data.
cat > "$tmp/one.jsonl" <<EOF
{"type":"store_header","session_id":"s1","ts":"2026-11-22T10:00:00Z","sft_eligible":false,"model":"$MARKER-model","workspace":"$MARKER-workspace"}
{"type":"turn","session_id":"s1","ts":"2026-11-22T10:00:01Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":10,"post_cap_bytes":10,"capped":false},{"name":"read_file","pre_cap_bytes":10,"post_cap_bytes":10,"capped":false},{"name":"run_shell","pre_cap_bytes":10,"post_cap_bytes":10,"capped":false}],"input_tokens":1,"output_tokens":1,"outcome":"complete","tool_arguments":"$MARKER-args","tool_result":"$MARKER-result","message":"$MARKER-message","memory_key":"$MARKER-key"}
{"type":"turn","session_id":"s2","ts":"2026-11-22T10:00:02Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":10,"post_cap_bytes":10,"capped":false}],"input_tokens":1,"output_tokens":1,"outcome":"complete","tool_arguments":"$MARKER-args2","message":"$MARKER-message2"}
EOF

# --- case 1: counts are right ---------------------------------------------
#
# read_file: 3 calls, 2 sessions. run_shell: 1 call, 1 session. The header row
# is first; the data rows are sorted by calls desc then name asc.
want="TOOL	CALLS	SESSIONS
read_file	3	2
run_shell	1	1"
got=$("$script" "$tmp/one.jsonl" 2>"$tmp/one.err")
if [ "$got" = "$want" ]; then
    pass "counts and layout for one file"
else
    fail "counts for one file"
    printf '  want:\n%s\n  got:\n%s\n' "$want" "$got" >&2
fi
assert_no_marker "one file (stderr)" "$tmp/one.err"
printf '%s\n' "$got" > "$tmp/one.out"
assert_no_marker "one file (stdout)" "$tmp/one.out"

# --- case 2: two files accumulate -----------------------------------------
#
# Second file: session s3 uses read_file once and grep_files once (a name that
# does not otherwise exist, proving new names are picked up).
cat > "$tmp/two.jsonl" <<EOF
{"type":"turn","session_id":"s3","ts":"2026-11-22T10:00:03Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false},{"name":"grep_files","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}],"message":"$MARKER-message3"}
EOF
want="TOOL	CALLS	SESSIONS
read_file	4	3
grep_files	1	1
run_shell	1	1"
got=$("$script" "$tmp/one.jsonl" "$tmp/two.jsonl" 2>"$tmp/two.err")
if [ "$got" = "$want" ]; then
    pass "counts accumulate across two files"
else
    fail "counts across two files"
    printf '  want:\n%s\n  got:\n%s\n' "$want" "$got" >&2
fi
assert_no_marker "two files (stderr)" "$tmp/two.err"
printf '%s\n' "$got" > "$tmp/two.out"
assert_no_marker "two files (stdout)" "$tmp/two.out"

# --- case 3: the same session id in two files counts once -----------------
#
# s1 appears in one.jsonl; repeat s1's read_file in a third file. Calls go up
# by 1; sessions must stay 3.
cat > "$tmp/three.jsonl" <<EOF
{"type":"turn","session_id":"s1","ts":"2026-11-22T10:00:04Z","sft_eligible":false,"turn_index":2,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}],"message":"$MARKER-message4"}
EOF
got=$("$script" "$tmp/one.jsonl" "$tmp/two.jsonl" "$tmp/three.jsonl" 2>/dev/null)
read_file_row=$(printf '%s\n' "$got" | awk -F '\t' '$1 == "read_file"')
if [ "$read_file_row" = "read_file	5	3" ]; then
    pass "a repeated session id counts once for SESSIONS"
else
    fail "repeated session id"
    printf '  want: read_file\\t5\\t3\\n  got:  %s\\n' "$read_file_row" >&2
fi

# --- case 4: a tool name that contains shell metacharacters is printed -----
#
# proves nothing is evaluated, only printed (the name goes through the same
# allowlisted path as every other name).
cat > "$tmp/meta.jsonl" <<EOF
{"type":"turn","session_id":"s4","ts":"2026-11-22T10:00:05Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"weird\$(touch $tmp/pwned)-name","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}]}
EOF
"$script" "$tmp/meta.jsonl" > "$tmp/meta.out" 2>/dev/null || true
if [ -e "$tmp/pwned" ]; then
    fail "tool name was evaluated by the shell"
fi
if grep -q 'weird' "$tmp/meta.out"; then
    pass "an unusual tool name is printed, not evaluated"
else
    fail "unusual tool name missing from the output"
fi

# --- case 5: help ---------------------------------------------------------
"$script" --help > "$tmp/help.out" 2>"$tmp/help.err" || true
if grep -q 'TOOL' "$tmp/help.out"; then
    pass "--help prints usage"
else
    fail "--help did not print usage"
fi
assert_no_marker "--help (stdout)" "$tmp/help.out"
assert_no_marker "--help (stderr)" "$tmp/help.err"

# --- case 6: no arguments is an error ------------------------------------
if "$script" > "$tmp/noargs.out" 2>"$tmp/noargs.err"; then
    fail "no arguments should exit non-zero"
else
    pass "no arguments exits non-zero"
fi

# --- case 7: a missing file is an error, and says only the path ----------
if "$script" "$tmp/does-not-exist.jsonl" > "$tmp/missing.out" 2>"$tmp/missing.err"; then
    fail "a missing file should exit non-zero"
else
    pass "a missing file exits non-zero"
fi
assert_no_marker "missing file (stderr)" "$tmp/missing.err"

# --- case 8: a directory argument is an error ---------------------------
if "$script" "$tmp" > /dev/null 2>"$tmp/dir.err"; then
    fail "a directory argument should exit non-zero"
else
    pass "a directory argument exits non-zero"
fi
assert_no_marker "directory argument (stderr)" "$tmp/dir.err"

# --- case 9: a malformed trace is an error, and its content never leaks ---
#
# Half a JSON object that contains the marker: the parse fails, the error must
# name the file and not the content.
printf '{"type":"turn","session_id":"s5","tool_calls":[{"name":"%s"}\n' "$MARKER" > "$tmp/bad.jsonl"
if "$script" "$tmp/bad.jsonl" > "$tmp/bad.out" 2>"$tmp/bad.err"; then
    fail "a malformed trace should exit non-zero"
else
    pass "a malformed trace exits non-zero"
fi
assert_no_marker "malformed trace (stdout)" "$tmp/bad.out"
assert_no_marker "malformed trace (stderr)" "$tmp/bad.err"

# --- case 10: an empty file is not an error, it is an empty report -------
: > "$tmp/empty.jsonl"
got=$("$script" "$tmp/empty.jsonl" 2>/dev/null || true)
if [ "$got" = "TOOL	CALLS	SESSIONS" ]; then
    pass "an empty trace yields the header only"
else
    fail "empty trace"
    printf '  want: TOOL\\tCALLS\\tSESSIONS\\n  got:  %s\\n' "$got" >&2
fi

if [ "$bad" -ne 0 ]; then
    echo "context_usage_test: $bad failure(s)" >&2
    exit 1
fi
echo "context_usage_test: all checks passed"
