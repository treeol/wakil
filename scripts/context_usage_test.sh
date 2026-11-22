#!/bin/sh
# context_usage_test.sh — asserts what scripts/context_usage.sh prints.
#
# Builds synthetic Wakil trace files (the internal/trace JSONL shape) that
# carry a SECRET MARKER in every field the real traces carry user content in:
# tool arguments, tool results, message content, workspace and model names,
# a memory key, and an extra field inside a tool_calls entry. Then:
#
#   1. checks the counts the script prints are exactly right;
#   2. checks the output layout is exactly TOOL<TAB>CALLS<TAB>SESSIONS;
#   3. asserts the marker never appears on stdout OR stderr, on every path:
#      a normal run, several files, --help, a missing file, an unreadable file,
#      a directory argument, a non-trace JSON file, and a malformed trace.
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
# proves nothing is evaluated, only printed. The heredoc is QUOTED so the shell
# does not expand $(...) at fixture-write time; the literal text is what the
# script must print, and no file must be created.
cat > "$tmp/meta.jsonl" <<'EOF'
{"type":"turn","session_id":"s4","ts":"2026-11-22T10:00:05Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"weird$(touch PWNED_PATH)-name","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}]}
EOF
# Substitute the marker placeholder with a real path after the heredoc, so the
# fixture is built literally but points at this temp dir.
sed "s|PWNED_PATH|$tmp/pwned|" "$tmp/meta.jsonl" > "$tmp/meta2.jsonl"
"$script" "$tmp/meta2.jsonl" > "$tmp/meta.out" 2>/dev/null || true
if [ -e "$tmp/pwned" ]; then
    fail "tool name was evaluated by the shell"
fi
if grep -q 'weird' "$tmp/meta.out"; then
    pass "an unusual tool name is printed, not evaluated"
else
    fail "unusual tool name missing from the output"
fi

# --- case 4b: the marker inside a tool_calls entry never reaches output ----
#
# The marker is placed in a tool_calls object's extra field, which is the one
# place the reader walks that could plausibly echo a whole object.
cat > "$tmp/intool.jsonl" <<EOF
{"type":"turn","session_id":"s6","ts":"2026-11-22T10:00:06Z","sft_eligible":false,"turn_index":1,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false,"arguments":"$MARKER-in-toolcall","result":"$MARKER-result-in-toolcall"}]}
EOF
got=$("$script" "$tmp/intool.jsonl" 2>"$tmp/intool.err")
printf '%s\n' "$got" > "$tmp/intool.out"
assert_no_marker "marker inside tool_calls (stdout)" "$tmp/intool.out"
assert_no_marker "marker inside tool_calls (stderr)" "$tmp/intool.err"
if [ "$got" = "$(printf 'TOOL\tCALLS\tSESSIONS\nread_file\t1\t1')" ]; then
    pass "an entry with extra fields still counts"
else
    fail "entry with extra fields"
    printf '  got: %s\n' "$got" >&2
fi

# --- case 5: help ---------------------------------------------------------
if "$script" --help > "$tmp/help.out" 2>"$tmp/help.err"; then
    pass "--help exits zero"
else
    fail "--help exits non-zero"
fi
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

# --- case 8b: an unreadable file is an error ---------------------------
#
# Skipped when running as root, where -r is always true regardless of mode.
if [ "$(id -u)" -eq 0 ]; then
    echo "  SKIP unreadable file (running as root)"
else
    printf '{"type":"turn","session_id":"s7","tool_calls":[]}\n' > "$tmp/noread.jsonl"
    chmod 000 "$tmp/noread.jsonl"
    if "$script" "$tmp/noread.jsonl" > /dev/null 2>"$tmp/noread.err"; then
        fail "an unreadable file should exit non-zero"
    else
        pass "an unreadable file exits non-zero"
    fi
    assert_no_marker "unreadable file (stderr)" "$tmp/noread.err"
    chmod 644 "$tmp/noread.jsonl"
fi

# --- case 8c: valid JSON that is not a trace is an error ----------------
#
# A non-empty file with no turn record is almost certainly the wrong file; an
# empty report would hide the mistake.
printf '{"hello":"world","value":1}\n' > "$tmp/notatrace.jsonl"
if "$script" "$tmp/notatrace.jsonl" > /dev/null 2>"$tmp/notatrace.err"; then
    fail "a non-trace JSON file should exit non-zero"
else
    pass "a non-trace JSON file exits non-zero"
fi
assert_no_marker "non-trace file (stderr)" "$tmp/notatrace.err"

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

# --- case 9b: a file CUT MID-LINE (the 65536-byte shape) still reports ------
#
# A trace whose last line was cut mid-record: the good records must still be
# counted, the broken tail skipped, and the skip summary must name the count.
cat > "$tmp/cut.jsonl" <<EOF
{"type":"turn","session_id":"c1","ts":"2026-11-22T10:01:00Z","sft_eligible":false,"turn_index":0,"turn_type":"tool_loop","tool_calls":[{"name":"read_file","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}],"message":"$MARKER-cut-msg"}
{"type":"turn","session_id":"c1","ts":"2026-11-22T10:01:01Z","sft_eligible":false,"turn_index":1,"turn_ty
EOF
got=$("$script" "$tmp/cut.jsonl" 2>"$tmp/cut.err")
if [ "$got" = "$(printf 'TOOL\tCALLS\tSESSIONS\nread_file\t1\t1')" ]; then
    pass "a file cut mid-line still reports its good records"
else
    fail "cut-mid-line file"
    printf '  got: %s\n' "$got" >&2
fi
if grep -q 'skipped 1 unparsable line(s) in 1 file(s)' "$tmp/cut.err"; then
    pass "the cut line is reported as skipped"
else
    fail "cut line not reported"
    printf '  stderr: %s\n' "$(cat "$tmp/cut.err")" >&2
fi
printf '%s\n' "$got" > "$tmp/cut.out"
assert_no_marker "cut-mid-line (stdout)" "$tmp/cut.out"
assert_no_marker "cut-mid-line (stderr)" "$tmp/cut.err"

# --- case 9c: a VALID record after a broken line is still counted ---------
#
# The resumed-session shape: a torn tail, then a new session's header and turns
# appended after it.
cat > "$tmp/resumed.jsonl" <<EOF
{"type":"turn","session_id":"r1","ts":"2026-11-22T10:02:00Z","sft_eligible":false,"turn_index":0,"turn_type":"tool_loop","tool_calls":[{"name":"run_shell","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}]}
{"type":"turn","session_id":"r1","turn_ca
{"type":"store_header","session_id":"r1","ts":"2026-11-22T10:03:00Z","sft_eligible":false,"model":"m","workspace":"w"}
{"type":"turn","session_id":"r1","ts":"2026-11-22T10:03:01Z","sft_eligible":false,"turn_index":0,"turn_type":"tool_loop","tool_calls":[{"name":"edit_file","pre_cap_bytes":1,"post_cap_bytes":1,"capped":false}],"message":"$MARKER-resumed"}
EOF
got=$("$script" "$tmp/resumed.jsonl" 2>"$tmp/resumed.err")
if printf '%s\n' "$got" | grep -q "^edit_file	1	1$" && printf '%s\n' "$got" | grep -q "^run_shell	1	1$"; then
    pass "a valid record after a broken line is still counted"
else
    fail "record after a broken line was lost"
    printf '  got: %s\n' "$got" >&2
fi
printf '%s\n' "$got" > "$tmp/resumed.out"
assert_no_marker "resumed file (stdout)" "$tmp/resumed.out"
assert_no_marker "resumed file (stderr)" "$tmp/resumed.err"

# --- case 9d: the secret marker INSIDE a broken line never leaks ----------
#
# jq's parse-error text can quote the offending input; stderr must stay quiet
# about it. The broken line carries the marker in a string field.
printf '{"type":"turn","session_id":"b1","note":"%s-broken","tool_ca\n' "$MARKER" > "$tmp/brokensecret.jsonl"
printf '{"type":"turn","session_id":"b1","tool_calls":[{"name":"list_dir"}]}\n' >> "$tmp/brokensecret.jsonl"
"$script" "$tmp/brokensecret.jsonl" > "$tmp/brokensecret.out" 2> "$tmp/brokensecret.err" || true
assert_no_marker "marker inside a broken line (stdout)" "$tmp/brokensecret.out"
assert_no_marker "marker inside a broken line (stderr)" "$tmp/brokensecret.err"

# --- case 9e: --union counts DISTINCT sessions across tools ---------------
#
# Two sessions: s_a used both dispatch_subagent and dispatch_subagents; s_b used
# only dispatch_subagent. The union is 2, NOT 2+1=3.
cat > "$tmp/union.jsonl" <<'EOF'
{"type":"turn","session_id":"s_a","tool_calls":[{"name":"dispatch_subagent"},{"name":"dispatch_subagents"}]}
{"type":"turn","session_id":"s_b","tool_calls":[{"name":"dispatch_subagent"}]}
EOF
got=$("$script" "$tmp/union.jsonl" --union "subagents=dispatch_subagent,dispatch_subagents" 2>/dev/null)
row=$(printf '%s\n' "$got" | awk -F '\t' '$1 == "subagents"')
if [ "$row" = "$(printf 'subagents\t2')" ]; then
    pass "--union counts distinct sessions, not the sum"
else
    fail "--union counted wrongly"
    printf '  want: subagents\t2\n  got:  %s\n' "$row" >&2
fi

# --- case 9f: the session denominator is on stderr -----------------------
if "$script" "$tmp/union.jsonl" 2>"$tmp/denom.err" >/dev/null; then
    if grep -q 'sessions=2 sessions_with_tools=2' "$tmp/denom.err"; then
        pass "session denominator reported on stderr"
    else
        fail "session denominator missing"
        printf '  stderr: %s\n' "$(cat "$tmp/denom.err")" >&2
    fi
fi

# --- case 9g: a scalar tool_calls does not abort the run ------------------
#
# A malformed turn whose tool_calls is a string, not an array: it must be
# skipped (or contribute nothing), never exit 2 over the whole file.
cat > "$tmp/scalar.jsonl" <<'EOF'
{"type":"turn","session_id":"sc1","tool_calls":"not-an-array"}
{"type":"turn","session_id":"sc1","tool_calls":[{"name":"run_shell"}]}
EOF
if got=$("$script" "$tmp/scalar.jsonl" 2>/dev/null); then
    if printf '%s\n' "$got" | grep -q "^run_shell	1	1$"; then
        pass "a scalar tool_calls does not abort the run"
    else
        fail "scalar tool_calls lost the good record"
    fi
else
    fail "a scalar tool_calls aborted the run"
fi

# --- case 9h: an empty session id is not counted as a session -------------
cat > "$tmp/nosid.jsonl" <<'EOF'
{"type":"turn","tool_calls":[{"name":"list_dir"}]}
EOF
"$script" "$tmp/nosid.jsonl" 2>"$tmp/nosid.err" >/dev/null || true
if grep -q 'sessions=0' "$tmp/nosid.err"; then
    pass "an empty session id is not counted"
else
    fail "empty session id counted"
    printf '  stderr: %s\n' "$(cat "$tmp/nosid.err")" >&2
fi

# --- case 9i: --union without '=' is rejected ----------------------------
if "$script" "$tmp/one.jsonl" --union "novalue" >/dev/null 2>&1; then
    fail "--union without = should exit non-zero"
else
    pass "--union without = is rejected"
fi

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
