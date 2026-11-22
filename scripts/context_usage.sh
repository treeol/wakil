#!/bin/sh
# context_usage.sh — count tool calls per tool across Wakil trace files.
#
# Usage:  context_usage.sh TRACE [TRACE ...]
#
# Reads Wakil session trace files (JSONL, see internal/trace/trace.go) and
# prints one line per tool name:
#
#     TOOL<TAB>CALLS<TAB>SESSIONS
#
# sorted by CALLS (descending), then TOOL (ascending). TOOL is a tool name the
# trace carries; CALLS is how many times it was called across all input files;
# SESSIONS is how many distinct session ids used it.
#
# PRIVACY CONTRACT — this script prints ONLY tool names and integers. It must
# never print tool arguments, tool results, message content, file paths,
# memory keys, model names, workspace names, or anything else from a trace.
# That is why the reader names exactly three fields (type, session_id,
# tool_calls[].name) and has no generic "print the JSON" path. The output is
# checked by scripts/context_usage_test.sh, which feeds it files containing a
# secret marker and asserts the marker never appears on stdout or stderr.
#
# A file that is not a readable JSONL trace is a hard error (exit 2): a
# silently skipped file would make a usage report quietly wrong.
#
# Portable POSIX sh. Requires jq (the bench harness already requires it).

set -eu

prog="context_usage.sh"

# usage <fd>: print the usage text to stdout (fd 1) or stderr (fd 2).
usage() {
    {
        echo "usage: $prog TRACE [TRACE ...]"
        echo "  prints TOOL<TAB>CALLS<TAB>SESSIONS for every tool in the traces"
    } >&"${1:-2}"
}

if [ "$#" -eq 0 ]; then
    usage 2
    exit 2
fi
case "${1:-}" in
    -h|--help) usage 1; exit 0 ;;
esac

if ! command -v jq >/dev/null 2>&1; then
    echo "$prog: jq is required but was not found in PATH" >&2
    exit 2
fi

for f in "$@"; do
    if [ ! -f "$f" ]; then
        echo "$prog: no such file: $f" >&2
        exit 2
    fi
    if [ ! -r "$f" ]; then
        echo "$prog: not readable: $f" >&2
        exit 2
    fi
done

# jq program. It extracts ONLY two things and never echoes any other field:
#
#   - .session_id of a turn record
#   - each .tool_calls[].name of a turn record
#
# Everything else in a record (arguments, results, content, paths) is ignored
# because it is never named. Non-turn records (store_header) are skipped.
# Emits "name<TAB>session" pairs; the shell totals them so that the output
# shape lives in one place (the awk program) and the test can assert it.
jq_prog='
select(type == "object" and .type == "turn")
| ((.session_id // "") | tostring) as $sid
| (.tool_calls // [])
| .[]
| select(type == "object")
| select(.name | type == "string")
| [(.name), $sid]
| @tsv
'

tmp=$(mktemp "${TMPDIR:-/tmp}/context-usage.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

for f in "$@"; do
    # Parse this file; on malformed input stop with a message naming only the
    # file, never anything from inside it.
    if ! jq -r "$jq_prog" "$f" 2>/dev/null >> "$tmp"; then
        echo "$prog: not a readable JSONL trace: $f" >&2
        exit 2
    fi
done

# Totals: per tool, call count and distinct session count. awk keeps two
# associative arrays; nothing but the tool name and two integers is printed.
awk -F '\t' '
    $1 == "" { next }
    {
        name = $1
        sid  = $2
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
' "$tmp" | LC_ALL=C sort -t "$(printf '\t')" -k2,2nr -k1,1 | {
    printf 'TOOL\tCALLS\tSESSIONS\n'
    cat
}
