#!/bin/sh
# host_verify_summary_test.sh — asserts what hv_summarize (host_verify_summary.sh)
# prints for saved logs: one with the testutil guard block, one with a named
# test failure, and the fallbacks. No Go, no network.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=host_verify_summary.sh
. "$here/host_verify_summary.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/hv-summary.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
tab=$(printf '\t')
bad=0

# expect <case> <check-name> <log> <want>: compares the whole output.
expect() {
    got=$(hv_summarize "$2" "$3")
    if [ "$got" = "$4" ]; then
        echo "  PASS $1"
    else
        echo "  FAIL $1" >&2
        printf '  want:\n%s\n  got:\n%s\n' "$4" "$got" >&2
        bad=$((bad + 1))
    fi
}

# 1. Guard block: every package passed its tests, the guard failed two of them.
cat > "$tmp/guard.log" <<EOF
ok  ${tab}github.com/treeol/wakil/internal/config${tab}0.2s
testutil: tests changed the real Wakil directories (set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 to ignore, e.g. while a real chat runs):
  created /home/u/.local/share/wakil/sessions/abc
  grew /home/u/.local/share/wakil/input_history (10 -> 20 bytes)
FAIL${tab}github.com/treeol/wakil/internal/agent${tab}3.1s
testutil: tests changed the real Wakil directories (set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 to ignore, e.g. while a real chat runs):
  created /home/u/.local/share/wakil/sessions/abc
FAIL${tab}github.com/treeol/wakil/internal/exec${tab}2.0s
FAIL
EOF
want="  FAIL${tab}github.com/treeol/wakil/internal/agent${tab}3.1s
  FAIL${tab}github.com/treeol/wakil/internal/exec${tab}2.0s
  testutil: tests changed the real Wakil directories (set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 to ignore, e.g. while a real chat runs):
    created /home/u/.local/share/wakil/sessions/abc
    grew /home/u/.local/share/wakil/input_history (10 -> 20 bytes)
  testutil: tests changed the real Wakil directories (set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 to ignore, e.g. while a real chat runs):
    created /home/u/.local/share/wakil/sessions/abc
  hint: a real Wakil session was running; stop it, or set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 for this run."
expect "go-test-race log with the guard block" go-test-race "$tmp/guard.log" "$want"

# 2. A named test failure, no guard: the FAIL line of the package only, no hint.
cat > "$tmp/named.log" <<EOF
--- FAIL: TestCount (0.00s)
    count_test.go:12: got 3, want 4
FAIL
FAIL${tab}github.com/treeol/wakil/internal/count${tab}0.3s
ok  ${tab}github.com/treeol/wakil/internal/other${tab}0.1s
EOF
want="  FAIL${tab}github.com/treeol/wakil/internal/count${tab}0.3s"
expect "go-test log with a named test failure" go-test "$tmp/named.log" "$want"

# 3. A go-test log with neither (a build failure): the tail.
printf '# github.com/treeol/wakil/internal/x\nx.go:1:1: syntax error\n' > "$tmp/build.log"
want="  # github.com/treeol/wakil/internal/x
  x.go:1:1: syntax error"
expect "go-test log with neither: the tail" go-test "$tmp/build.log" "$want"

# 4. Any other check: the last 15 lines, indented.
i=1; : > "$tmp/long.log"
while [ "$i" -le 20 ]; do printf 'line %d\n' "$i" >> "$tmp/long.log"; i=$((i + 1)); done
want=$(i=6; while [ "$i" -le 20 ]; do printf '  line %d\n' "$i"; i=$((i + 1)); done)
expect "other check: the last 15 lines" vet "$tmp/long.log" "$want"

[ "$bad" -eq 0 ] || { echo "host_verify_summary_test: $bad failed" >&2; exit 1; }
echo "host_verify_summary_test: ok"
