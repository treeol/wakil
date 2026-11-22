#!/bin/sh
# context_usage.sh — count tool calls per tool across Wakil trace files.
#
# Usage:  context_usage.sh TRACE [TRACE ...]
#
# Reads Wakil session trace files (JSONL, see internal/trace/trace.go) and
# prints one line per tool name on stdout:
#
#     TOOL<TAB>CALLS<TAB>SESSIONS
#
# sorted by CALLS (descending), then TOOL (ascending). TOOL is a tool name the
# trace carries; CALLS is how many times it was called across all input files;
# SESSIONS is how many distinct session ids used it.
#
# Two integers go to stderr so stdout stays a clean TSV table:
#   sessions=N sessions_with_tools=M turns=T
#     N = distinct session ids seen in a turn record; M = those that called at
#     least one tool. These are the denominators a module "session share" needs
#     (per-tool SESSIONS cannot be summed — a session using two tools in a group
#     would count twice).
#   skipped K unparsable line(s) in F file(s)
#
# Optional:  --union LABEL=TOOL1,TOOL2 [...]  prints a second table on stdout
#
#     MODULE<TAB>SESSIONS
#
# with the number of DISTINCT sessions that used ANY of the listed tools, so a
# module share is a true union, not a sum.
#
# PRIVACY CONTRACT — this script prints ONLY tool names and integers. It must
# never print tool arguments, tool results, message content, file paths,
# memory keys, model names, workspace names, or anything else from a trace.
# That is why the reader names exactly three fields (type, session_id,
# tool_calls[].name) and has no generic "print the JSON" path. The output is
# checked by scripts/context_usage_test.sh, which feeds it files containing a
# secret marker and asserts the marker never appears on stdout or stderr.
#
# BROKEN LINES — a trace line can fail to parse (a file cut mid-record by an
# older writer that flushed inside a record — see the trace package). Such a
# line is COUNTED and SKIPPED; it never aborts the run. The summary below the
# table reports how many lines were skipped and in how many files.
#
# A file that is not a trace at all (no turn record in a non-empty file) is
# still a hard error (exit 2): silently reporting an empty table would hide a
# wrong path. A file whose every line is broken is reported as such.
#
# Portable POSIX sh. Requires jq (the bench harness already requires it).
#
# HOST COMMAND — run this on the machine that holds the real traces (not in a
# sandbox). Confirm the trace directory with `wakil doctor` if it is not the
# default:
#
#     sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/*.jsonl
#
# Expect TOOL, CALLS and SESSIONS on stdout, and a one-line skip summary on
# stderr. Nothing else in the traces is read.

set -eu

prog="context_usage.sh"

# usage <fd>: print the usage text to stdout (fd 1) or stderr (fd 2).
usage() {
    {
        echo "usage: $prog [--union LABEL=TOOL1,TOOL2 ...] TRACE [TRACE ...]"
        echo "  prints TOOL<TAB>CALLS<TAB>SESSIONS for every tool in the traces"
        echo "  and a session/skipped summary on stderr"
        echo "  --union prints MODULE<TAB>SESSIONS for distinct sessions across tools"
    } >&"${1:-2}"
}

if [ "$#" -eq 0 ]; then
    usage 2
    exit 2
fi
case "${1:-}" in
    -h|--help) usage 1; exit 0 ;;
esac

# Optional --union LABEL=TOOL1,TOOL2 (repeatable): after the table, print the
# number of DISTINCT sessions that used ANY of the listed tools. Per-tool
# session counts cannot be summed (a session using two of them would count
# twice); a module's share needs this union. Integers only, so the privacy
# contract holds.
unions=""
args=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --union)
            [ "$#" -ge 2 ] || { echo "$prog: --union needs LABEL=TOOL1,TOOL2" >&2; exit 2; }
            unions="$unions$2
"
            shift 2
            ;;
        *)
            args="$args$1
"
            shift
            ;;
    esac
done
[ -n "$args" ] || { usage 2; exit 2; }

if ! command -v jq >/dev/null 2>&1; then
    echo "$prog: jq is required but was not found in PATH" >&2
    exit 2
fi

set -f # the args list is newline-separated; disable globbing while splitting
IFS='
'
for f in $args; do
    if [ ! -f "$f" ]; then
        echo "$prog: no such file: $f" >&2
        exit 2
    fi
    if [ ! -r "$f" ]; then
        echo "$prog: not readable: $f" >&2
        exit 2
    fi
done

# jq program, run with -R so input is raw text and each LINE is parsed on its
# own. That is what makes a single broken line non-fatal. It extracts ONLY two
# things and never echoes any other field:
#
#   - .session_id of a turn record
#   - each .tool_calls[].name of a turn record
#
# Everything else (arguments, results, content, paths) is ignored because it is
# never named. Non-turn records (store_header) are skipped silently.
#
# Two control-prefixed kinds of output line are emitted, so the shell can total
# them in one place:
#
#   \001T<TAB>name<TAB>session   one tool call
#   \001S                        one line that did not parse
#
# A parse failure yields the sentinel object {"__bad__":true}; a real record
# can never carry that key, so the two cases stay distinct even if the line is
# valid JSON of another shape.
jq_prog='
  select(length > 0)
  | (try (fromjson) catch {"__bad__": true}) as $r
  | if ($r | type) == "object" and ($r.__bad__ == true) then "\u0001S"
    elif ($r | type) != "object" then "\u0001S"
    elif $r.type != "turn" then empty
    else
      (($r.session_id // "") | tostring) as $sid
      | "\u0001U\t\($sid)",
        (($r.tool_calls // []) | .[]
         | select(type == "object")
         | select(.name | type == "string")
         | "\u0001T\t\(.name)\t\($sid)")
    end
'

tmp=$(mktemp "${TMPDIR:-/tmp}/context-usage.XXXXXX")
trap 'rm -f "$tmp" "$tmp.totals" "$tmp.parsed" "$tmp.sets" "$tmp.sessions"' EXIT HUP INT TERM

skipped_lines=0
skipped_files=0
turns_seen=0

for f in $args; do
    # Parse line by line; broken lines come back as \001S and are counted, not
    # fatal. A jq-level failure (unreadable file, jq crash) is still fatal.
    if ! jq -R -r "$jq_prog" "$f" 2>/dev/null > "$tmp.parsed"; then
        echo "$prog: failed to read trace: $f" >&2
        exit 2
    fi
    n=$(grep -c "$(printf '\001')S" "$tmp.parsed" 2>/dev/null || true)
    [ -n "${n:-}" ] || n=0
    if [ "$n" -gt 0 ]; then
        skipped_lines=$((skipped_lines + n))
        skipped_files=$((skipped_files + 1))
    fi
    grep "$(printf '\001')T" "$tmp.parsed" >> "$tmp" 2>/dev/null || true
    # Same data reversed as "session<TAB>tool" for the --union computation.
    awk -F '\t' '$1 == "\001T" { print $3 "\t" $2 }' "$tmp.parsed" >> "$tmp.sets" 2>/dev/null || true
    u=$(grep -c "$(printf '\001')U" "$tmp.parsed" 2>/dev/null || true)
    [ -n "${u:-}" ] || u=0
    turns_seen=$((turns_seen + u))
    grep "$(printf '\001')U" "$tmp.parsed" >> "$tmp.sessions" 2>/dev/null || true
    rm -f "$tmp.parsed"
done

# A file of valid JSON that is not a trace (no turn records at all) yields no
# lines. That is not the same as "the trace has no tool calls": a non-empty
# file with no turn record is almost certainly the wrong file, and silently
# reporting an empty table would hide it. Only genuinely empty input is an
# empty report.
input_bytes=0
for f in $args; do
    b=$(wc -c < "$f" 2>/dev/null || echo 0)
    input_bytes=$((input_bytes + b))
done
if [ "$input_bytes" -gt 0 ] && [ "$turns_seen" -eq 0 ]; then
    if [ "$skipped_lines" -gt 0 ]; then
        echo "$prog: no turn records in the given file(s) ($skipped_lines broken line(s) in $skipped_files file(s)) — are these Wakil traces?" >&2
    else
        echo "$prog: no turn records found in the given file(s) — are these Wakil traces?" >&2
    fi
    exit 2
fi

# Totals: per tool, call count and distinct session count. awk keeps two
# associative arrays; nothing but the tool name and two integers is printed.
#
# The pipeline's exit status is checked explicitly: POSIX sh has no pipefail,
# and without this an awk or sort failure would be masked by the final printf.
if ! awk -F '\t' '
    $1 == "" { next }
    {
        name = $2
        sid  = $3
        calls[name]++
        if (sid != "" && !((name SUBSEP sid) in seen)) {
            seen[name SUBSEP sid] = 1
            sessions[name]++
        }
    }
    END {
        for (n in calls) {
            printf "%s\t%d\t%d\n", n, calls[n], sessions[n] + 0
        }
    }
' "$tmp" > "$tmp.totals"; then
    echo "$prog: failed to total the tool counts" >&2
    exit 2
fi

{
    printf 'TOOL\tCALLS\tSESSIONS\n'
    LC_ALL=C sort -t "$(printf '\t')" -k2,2nr -k1,1 "$tmp.totals"
} || {
    echo "$prog: failed to sort the report" >&2
    exit 2
}

# Optional per-module unions: distinct sessions that used ANY listed tool.
if [ -n "$unions" ]; then
    printf 'MODULE\tSESSIONS\n'
    printf '%s' "$unions" | while IFS= read -r spec; do
        [ -n "$spec" ] || continue
        label=${spec%%=*}
        tools=${spec#*=}
        # Build an awk membership test from the comma list.
        awk -F '\t' -v label="$label" -v tools="$tools" '
            BEGIN { n = split(tools, t, ","); for (i = 1; i <= n; i++) want[t[i]] = 1 }
            ($2 in want) { seen[$1] = 1 }
            END { c = 0; for (s in seen) c++; printf "%s\t%d\n", label, c }
        ' "$tmp.sets"
    done
fi

# Session totals and the skip summary go to stderr so stdout stays a clean,
# sortable table. SESSIONS_ALL counts every distinct session id seen in a turn
# record (the denominator a module share needs); SESSIONS_WITH_TOOLS counts
# those that called at least one tool.
sess_all=0
sess_tools=0
if [ -s "$tmp.sessions" ]; then
    sess_all=$(LC_ALL=C sort -u "$tmp.sessions" | wc -l | tr -d ' ')
fi
if [ -s "$tmp.sets" ]; then
    sess_tools=$(cut -f1 "$tmp.sets" | LC_ALL=C sort -u | wc -l | tr -d ' ')
fi
echo "$prog: sessions=$sess_all sessions_with_tools=$sess_tools turns=$turns_seen" >&2
if [ "$skipped_lines" -gt 0 ]; then
    echo "$prog: skipped $skipped_lines unparsable line(s) in $skipped_files file(s)" >&2
fi
