#!/bin/sh
# host_verify_summary.sh — what scripts/host_verify.sh prints for a failed
# check. Sourced, not run.
#
# A failed check shows the tail of its log, except go-test and go-test-race:
# their logs are long, and the lines that matter (which package failed, and the
# testutil guard that fails a package when the real Wakil directories changed,
# for example because a real Wakil session was running) are far from the tail.
# For those two it prints the "FAIL <pkg>" lines and the guard block, plus one
# hint line when the guard fired. It falls back to the tail when neither is
# there. The full log stays in the output directory.

hv_guard_hint="hint: a real Wakil session was running; stop it, or set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 for this run."

# hv_summarize <check-name> <log-file>: print the failure summary, indented by
# two spaces.
hv_summarize() {
    hv_name=$1; hv_log=$2
    case "$hv_name" in
        go-test|go-test-race)
            hv_fails=$(grep -E '^FAIL[[:space:]]+[^[:space:]]' "$hv_log" || true)
            # The guard block: its first line, then the "created"/"grew" lines
            # (isolate.go) that follow it.
            hv_guard=$(awk '
                /^testutil: tests changed the real Wakil directories/ { on = 1; print; next }
                on && /^  (created|grew) / { print; next }
                { on = 0 }
            ' "$hv_log")
            if [ -z "$hv_fails" ] && [ -z "$hv_guard" ]; then
                tail -n 15 "$hv_log" | sed 's/^/  /'
                return 0
            fi
            [ -z "$hv_fails" ] || printf '%s\n' "$hv_fails" | sed 's/^/  /'
            if [ -n "$hv_guard" ]; then
                printf '%s\n' "$hv_guard" | sed 's/^/  /'
                printf '  %s\n' "$hv_guard_hint"
            fi
            ;;
        *)
            tail -n 15 "$hv_log" | sed 's/^/  /'
            ;;
    esac
}
