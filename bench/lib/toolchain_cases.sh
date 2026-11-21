#!/bin/sh
# toolchain_cases.sh <tree> — black-box cases for a host whose Go cannot run the
# task checks. <tree> holds scripts/bench.sh and bench/ (the self-test passes
# the repo; a review can pass a "git archive" of an older commit to see the
# cases fail there). Each case copies the tree to a temp dir and runs the real
# bench.sh in it with a stub "go" first in PATH, a stub "wakil" that solves the
# task, and a temp XDG_CACHE_HOME: no model, no network, nothing outside the
# temp dir is touched.
#
# The stub go prints the error a host with Go 1.25.1 gives for a go.mod that
# needs 1.26 and exits 1:
#   go: go.mod requires go >= 1.26 (running go 1.25.1; GOTOOLCHAIN=local)
# Cases: preflight-stops, preflight-stops-on-check, check-error-not-fail.
set -eu
root=$(CDPATH='' cd -- "${1:?usage: toolchain_cases.sh <tree>}" && pwd)
real_go=$(command -v go) || { echo "toolchain_cases: go is needed (the \"env-ok\" stub forwards go env to it)" >&2; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/bench-tccases.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
host_err='go: go.mod requires go >= 1.26 (running go 1.25.1; GOTOOLCHAIN=local)'
bad=0

mkdir "$tmp/stub"
# MODE=all: every go command fails (a host whose Go is 1.25.1; "local" says so).
# MODE=env-ok: "go env" answers (real go), every other command fails.
cat > "$tmp/stub/go" <<STUB
#!/bin/sh
case "\$MODE:\$*" in
  all:"env GOVERSION")
    if [ "\${GOTOOLCHAIN:-}" = local ]; then echo go1.25.1; exit 0; fi ;;
  env-ok:env*) exec "$real_go" "\$@" ;;
esac
echo "$host_err" >&2
exit 1
STUB
cat > "$tmp/stub/wakil" <<'STUB'
#!/bin/sh
case "$1" in
  version) echo "stub-solve/0.0.0" ;;
  run)
    shift
    rj=""
    while [ $# -gt 0 ]; do
      case "$1" in --result-json) rj=$2; shift 2 ;; *) shift ;; esac
    done
    : > "$BENCH_MARKER"
    cp -R "$BENCH_TASK_DIR/solution/." "$WAKIL_HOST_WORKDIR/"
    printf '{"wakil_version":"stub","model":"stub","exit_reason":"pass","workdir":"%s","seconds":0.0}\n' "$WAKIL_HOST_WORKDIR" > "$rj"
    exit 0 ;;
esac
STUB
chmod +x "$tmp/stub/go" "$tmp/stub/wakil"

# run_case <name> <MODE> [VAR=value]...: copies the tree, runs one bench; sets
# rc, $tmp/<name>/{out,err}, and the tree dir in $t.
run_case() {
    name=$1; mode=$2; shift 2
    t="$tmp/$name/tree"
    mkdir -p "$t"
    cp -R "$root/scripts" "$root/bench" "$t/"
    # The harness resolves the toolchain in its repo root, so the root go.mod
    # must be there (a host with an older Go switches because of it).
    cp "$root/go.mod" "$t/go.mod"
    rm -rf "$t/bench/results"
    rc=0
    env -u GOTOOLCHAIN -u GOFLAGS -u GOPROXY -u GOENV \
        PATH="$tmp/stub:$PATH" MODE="$mode" XDG_CACHE_HOME="$tmp/$name/cache" \
        BENCH_TASK_DIR="$t/bench/tasks/fix-test" BENCH_MARKER="$tmp/$name/wakil-ran" "$@" \
        "$t/scripts/bench.sh" --wakil "$tmp/stub/wakil" --task fix-test --runs 1 \
        >"$tmp/$name/out" 2>"$tmp/$name/err" || rc=$?
}

verdict() {
    if [ -z "$2" ]; then
        echo "  PASS $1"
    else
        echo "  FAIL $1: $2"
        bad=$((bad + 1))
    fi
}

# csv_rows <dir>: number of result rows (lines after the header) in any CSV.
csv_rows() {
    n=0
    for f in "$1"/bench/results/*-bench.csv; do
        [ -f "$f" ] || continue
        n=$((n + $(wc -l < "$f") - 1))
    done
    echo "$n"
}

# (a) The preflight stops the run: non-zero, exactly one stderr line naming the
# required and the found Go, no CSV row, no model request (the stub wakil
# leaves a marker when it runs).
check_preflight() {
    name=$1; found=$2
    why=""
    # grep -c exits 1 on zero matches; under set -e that would end this script
    # inside an assignment, so the count is taken with a fallback.
    lines=$(grep -c . "$tmp/$name/err" || true)
    [ "$rc" -ne 0 ] || why="$why exit 0;"
    [ "$lines" -eq 1 ] || why="$why stderr is not one line ($lines);"
    grep -q 'go >= 1\.26' "$tmp/$name/err" || why="$why line lacks the required version 1.26;"
    grep -q "found $found" "$tmp/$name/err" || why="$why line lacks the found version $found;"
    [ "$(csv_rows "$t")" -eq 0 ] || why="$why a CSV row was written;"
    [ ! -e "$tmp/$name/wakil-ran" ] || why="$why the stub wakil ran (a model request would have been made);"
    verdict "$name" "$why"
}

echo "toolchain cases on $root"
run_case preflight-stops all
check_preflight preflight-stops go1.25.1
echo "    stderr: $(head -n 1 "$tmp/preflight-stops/err" || true)"

run_case preflight-stops-on-check env-ok
expected=$(cd "$tmp/preflight-stops-on-check/tree" && GOPROXY=off "$real_go" env GOVERSION | cut -d' ' -f1)
check_preflight preflight-stops-on-check "$expected"

# (b) With the preflight bypassed (test only), a toolchain error inside the
# check is an "error" with a "check:" reason in the log, never "fail".
run_case check-error-not-fail all BENCH_TEST_SKIP_PREFLIGHT=1
why=""
[ -e "$tmp/check-error-not-fail/wakil-ran" ] || why="$why the run did not happen;"
csv=""
for f in "$t"/bench/results/*-bench.csv; do
    [ -f "$f" ] && { csv=$f; break; }
done
row=""
[ -z "$csv" ] || row=$(sed -n 2p "$csv")
case "$row" in
    *,fix-test,1,error,check-error,*) ;;
    *,fix-test,1,fail,*) why="$why result is fail, not error;" ;;
    *) why="$why unexpected row: $row;" ;;
esac
grep -qh '^check: environment error: ' "$t"/bench/results/fix-test-1-*.log 2>/dev/null || why="$why the log has no 'check: environment error' line;"
verdict check-error-not-fail "$why"
[ -z "$row" ] || echo "    row: $row"

[ "$bad" -eq 0 ] || { echo "toolchain cases: $bad failed" >&2; exit 1; }
echo "toolchain cases: ok"
