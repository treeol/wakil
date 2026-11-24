#!/bin/sh
# bench.sh — benchmark harness for Wakil ("bench").
#
# Runs each task under bench/tasks/<name>/ through "wakil run" and records
# one CSV row per run in bench/results/. See bench/README.md.
#
# Usage:
#   scripts/bench.sh [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
#                    [--timeout SECONDS] [--mashura LEVEL] [--self-test]
#   scripts/bench.sh --setup
#
# Defaults: wakil from PATH, the user's configured model, all tasks, 1 run,
# 600s per task, Mashura off.
#
# Exit 0 when the harness itself worked, whatever the task results;
# non-zero only on harness errors.
set -eu

prog="${prog:="${0##*/}"}"

# The external programs a run of this harness calls (the script, bench/lib and
# the task checks; shell builtins are not listed). The self-test builds its
# "tool missing" PATHs from this list and nothing else (tool_dir), so a case
# never depends on which directory the host keeps a tool in.
harness_tools="awk basename cat cp date dirname env find flock git grep head jq mkdir mktemp mv rm sed sha256sum sleep sort tail tar timeout tr"
# Programs only the self-test (and bench/lib/toolchain_cases.sh) call on top.
selftest_tools="chmod cut id ln touch wc sh"

# Checked before anything else runs, with shell builtins only (command -v): a
# missing tool gives this one line, never a shell error from the first call.
# The tools a task needs beyond these (go, python3) are checked where they are
# resolved (resolve_go, check_task_tools).
_need="$harness_tools"
for _a in "$@"; do
    case "$_a" in --self-test) _need="$harness_tools $selftest_tools" ;; esac
done
missing_tools=""
for _t in $_need; do
    command -v "$_t" >/dev/null 2>&1 || missing_tools="$missing_tools $_t"
done
if [ -n "$missing_tools" ]; then
    echo "$prog: required tool(s) not found:$missing_tools" >&2
    exit 1
fi
unset _need _a _t

# Detect whether stdout is a terminal (heartbeat progress lines are
# terminal-only; nothing extra when output is piped or redirected).
if [ -t 1 ]; then
    is_tty=1
else
    is_tty=0
fi

usage() {
    cat <<USAGE
usage: $prog [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
             [--timeout SECONDS] [--mashura LEVEL] [--context-modules on|off]
             [--no-preflight] [--self-test]
  or:  $prog --setup

  --wakil PATH      wakil binary to run (default: from PATH)
  --model NAME      model for all runs (default: the user's configured model)
  --task NAME       only this task (default: all tasks in bench/tasks/)
  --runs N          runs per task (default: 1)
  --timeout SECONDS per-run timeout in seconds (default: 600)
  --mashura LEVEL   auto-counsel: off|low|medium|high (default: off)
  --context-modules on|off  set WAKIL_CONTEXT_MODULES for the runs (default: off)
  --no-preflight    skip the Go preflight (test use). The run writes its CSV
                    and logs under the cache (wakil-bench/nopreflight), never
                    under bench/results
  --self-test       run every task against stub wakil binaries
  --setup           create all task work folders and print the one
                    'WAKIL_HOST_WORKDIR=<path> wakil sandbox trust' command per folder
USAGE
}

wakil_bin="wakil"
# Seconds between the TERM that "timeout" sends at the deadline and the KILL
# it sends to a process that ignored it (BENCH_KILL_GRACE; the self-test
# shortens it).
kill_grace="${BENCH_KILL_GRACE:-10}"
case "$kill_grace" in *[!0-9]*|'') echo "${0##*/}: BENCH_KILL_GRACE must be a positive integer (got $kill_grace)" >&2; exit 2 ;; esac
[ "$kill_grace" -ge 1 ] || { echo "${0##*/}: BENCH_KILL_GRACE must be >= 1" >&2; exit 2; }
# Upper bound for one check.sh, setup.sh or preflight run (BENCH_CHECK_TIMEOUT,
# seconds): a hung "go build" of agent code must not hang the harness. A hit is
# an error ("check: timed out"), not a fail.
check_timeout="${BENCH_CHECK_TIMEOUT:-900}"
case "$check_timeout" in *[!0-9]*|'') echo "${0##*/}: BENCH_CHECK_TIMEOUT must be a positive integer (got $check_timeout)" >&2; exit 2 ;; esac
[ "$check_timeout" -ge 1 ] || { echo "${0##*/}: BENCH_CHECK_TIMEOUT must be >= 1" >&2; exit 2; }
# Seconds between two heartbeat lines of a run on a terminal. A variable so the
# self-test can give the heartbeat's sleep a number nothing else uses.
hb_interval=30
model_name=""
only_task=""
runs=1
timeout_sec=600
mashura_level="off"
context_modules="off"
self_test=0
skip_preflight=0
do_setup=0

while [ $# -gt 0 ]; do
    case "$1" in
        --wakil)
            [ $# -ge 2 ] || { echo "$prog: --wakil requires a path" >&2; exit 2; }
            wakil_bin="$2"; shift 2 ;;
        --model)
            [ $# -ge 2 ] || { echo "$prog: --model requires a name" >&2; exit 2; }
            model_name="$2"; shift 2 ;;
        --task)
            [ $# -ge 2 ] || { echo "$prog: --task requires a name" >&2; exit 2; }
            only_task="$2"; shift 2 ;;
        --runs)
            [ $# -ge 2 ] || { echo "$prog: --runs requires a number" >&2; exit 2; }
            case "$2" in *[!0-9]*|'') echo "$prog: --runs must be a positive integer (got $2)" >&2; exit 2 ;; esac
            [ "$2" -ge 1 ] || { echo "$prog: --runs must be >= 1" >&2; exit 2; }
            runs="$2"; shift 2 ;;
        --timeout)
            [ $# -ge 2 ] || { echo "$prog: --timeout requires a number" >&2; exit 2; }
            case "$2" in *[!0-9]*|'') echo "$prog: --timeout must be a positive integer (got $2)" >&2; exit 2 ;; esac
            [ "$2" -ge 1 ] || { echo "$prog: --timeout must be >= 1" >&2; exit 2; }
            timeout_sec="$2"; shift 2 ;;
        --mashura)
            [ $# -ge 2 ] || { echo "$prog: --mashura requires a level" >&2; exit 2; }
            case "$2" in off|low|medium|high) ;; *) echo "$prog: --mashura must be off|low|medium|high (got $2)" >&2; exit 2 ;; esac
            mashura_level="$2"; shift 2 ;;
        --context-modules)
            [ $# -ge 2 ] || { echo "$prog: --context-modules requires on or off" >&2; exit 2; }
            case "$2" in on|off) ;; *) echo "$prog: --context-modules must be on or off (got $2)" >&2; exit 2 ;; esac
            context_modules="$2"; shift 2 ;;
        --self-test) self_test=1; shift ;;
        --no-preflight) skip_preflight=1; shift ;;
        --setup) do_setup=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "$prog: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

# Repo root (the directory containing this script's parent).
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
bench_dir="$repo_root/bench"
tasks_dir="$bench_dir/tasks"
results_dir="$bench_dir/results"
# bench/results is created by a real run only (main); --setup and --self-test
# never write into the repo.

# Fixed work folders: stable paths so the user's one-time sandbox
# acknowledgement holds across runs. Each task runs in
# ${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>.
cache_home="${XDG_CACHE_HOME:-$HOME/.cache}"
work_root="$cache_home/wakil-bench/work"
# One lock file per task, outside the work folder (the folder is emptied and
# refilled before every run, so a lock inside it would not survive).
lock_root="$cache_home/wakil-bench/locks"
# The per-run result JSON lives here, outside the work folder: the agent can
# write into its folder, so a result file in it could be forged (workdir,
# exit_reason) or replaced by a symlink the host-side wakil would then follow.
run_root="$cache_home/wakil-bench/run"

# Tools the harness cannot work without are checked at the top of this file
# (harness_tools). jq reads the result JSON (without it the workdir guard sees
# no workdir and every run would stop as "missing-workdir"), flock guards the
# work folders, timeout bounds each run, sha256sum digests .wakil/, git builds
# the work folder's commit, tar copies the work folder for the Go checks.

# --no-preflight skips the Go preflight so the toolchain cases can drive a
# check through a broken go. It is a flag, never an environment variable: no
# variable alone can switch the preflight off. A run that skips it prints one
# warning line and writes its CSV and logs only under the cache directory
# (never bench/results, never where find_prev_csv looks), so it cannot be
# mistaken for a measured run. The self-test has its own redirect.
if [ "$skip_preflight" -eq 1 ] && [ "$self_test" -eq 1 ]; then
    echo "$prog: --no-preflight and --self-test cannot be combined" >&2
    exit 2
fi
if [ "$skip_preflight" -eq 1 ]; then
    echo "$prog: warning: --no-preflight: the Go preflight is skipped; results go to $cache_home/wakil-bench/nopreflight, not bench/results" >&2
    [ "$self_test" -eq 1 ] || results_dir="$cache_home/wakil-bench/nopreflight"
fi

csv_header='date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,tokens_cached,cost_usd,tool_calls,collapses,collapses_turn_end,mashura_rounds,context_modules,modules_loaded,valid,provider'
# CSV is created lazily: --setup and --self-test do not write a real-run
# CSV (a header-only file would be picked up by find_prev_csv as the
# "earlier run"). main() creates it before run_all only.
csv_file=""

# The resolved model for the CSV: from --model, else the result-json of
# the first run (resolved model, not requested), else "unknown". The
# header and CSV never show the literal "config" when --model is not
# given; a real run fills it from --result-json.
resolved_model="$model_name"
[ -n "$resolved_model" ] || resolved_model="unknown"

# --mashura level -> auto-counsel args for `wakil run`. "off" omits
# --auto-counsel; other levels pass it.
counsel_args=""
case "$mashura_level" in
    off) counsel_args="" ;;
    low|medium|high) counsel_args="--auto-counsel" ;;
esac

# Wakil version string (from the binary: `version` is universal).
wakil_version="$("$wakil_bin" version 2>/dev/null | head -n 1 | tr -d '\r' || echo 'unknown')"
[ -n "$wakil_version" ] || wakil_version="unknown"

# Task list.
if [ -n "$only_task" ]; then
    # Validate --task against the list of task folder names so a typo
    # cannot make do_setup or prep_workdir run rm -rf on an arbitrary path;
    # the name also becomes a lock file name, so no "/" and no leading ".".
    case "$only_task" in
        */*|.*|'') echo "$prog: --task: invalid task name '$only_task'" >&2; exit 2 ;;
    esac
    if [ ! -d "$tasks_dir/$only_task" ]; then
        echo "$prog: --task: no task named '$only_task' in $tasks_dir" >&2
        exit 2
    fi
    tasks="$only_task"
else
    tasks=""
    for d in "$tasks_dir"/*/; do
        [ -d "$d" ] || continue
        tasks="$tasks$(basename "$d") "
    done
    tasks=${tasks% }
fi
[ -n "$tasks" ] || { echo "$prog: no tasks found in $tasks_dir" >&2; exit 1; }

# Total task count for progress lines ([i/N]).
# shellcheck disable=SC2086 # word splitting is the point: $tasks is a space-separated list of task names
set -- $tasks
total_tasks=$#
task_idx=0

# ---- Go toolchain for the checks -------------------------------------------
# The Go tasks' checks (and long-read's setup.sh) must not depend on what the
# host's own Go happens to be: the host's Go can be older than a task's go.mod
# (then "GOTOOLCHAIN=local" can never build it), and a check must never
# download anything. So the toolchain is resolved once, here, in the repo root
# with the caller's environment (the same way a developer's "go version" in
# the repo resolves it: the already cached toolchain the repo's go.mod
# selects), and every check runs with GOTOOLCHAIN set to that exact version
# and GOPROXY=off (bench/lib/gocheck.sh, gocheck_env).
#
# go_toolchain is empty when no selected task needs Go. The cache locations
# are resolved with the same environment because the checks run with
# GOENV=off, which hides "go env -w" settings.
go_toolchain=""
go_root=""
go_modcache=""
go_cache=""
go_path=""
st_go_private=""

# go_required_version: the highest "go" directive of the selected tasks' go.mod
# files (task repos and the long-read generator), e.g. 1.26.
go_required_version() {
    for _gr_t in $tasks; do
        for _gr_f in "$tasks_dir/$_gr_t/repo/go.mod" "$tasks_dir/$_gr_t/gen/go.mod"; do
            [ -f "$_gr_f" ] && awk '$1 == "go" { print $2; exit }' "$_gr_f"
        done
    done | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1
}

# go_found_version: the Go the harness found: the resolved toolchain, else the
# installed Go ("local", no switching), else "none".
go_found_version() {
    if [ -n "$go_toolchain" ] && [ "$go_toolchain" != local ]; then
        echo "$go_toolchain"
    elif command -v go >/dev/null 2>&1; then
        _gf=$(cd "$repo_root" && GOFLAGS='' GOTOOLCHAIN=local GOPROXY=off go env GOVERSION 2>/dev/null | head -n 1)
        echo "${_gf:-unknown}"
    else
        echo "no go in PATH"
    fi
}

# go_fail_line <detail>: the one line printed when the Go checks cannot run.
# It names the Go the tasks need and the Go that was found, then go's own
# words (the cause can be something else than the version, e.g. GOSUMDB=off or
# an uncached toolchain, and go says so).
go_fail_line() {
    echo "$prog: the Go checks cannot run: the tasks need go >= $(go_required_version), found $(go_found_version) — $1" >&2
}

# go_skip_note <why>: under --no-preflight a failed resolve falls back to the
# "go" on PATH. That is a test-only mode, but the cause must not stay hidden.
go_skip_note() {
    echo "$prog: warning: --no-preflight: no Go toolchain was resolved ($1); the checks use the go on PATH" >&2
}

# resolve_go: fill go_toolchain, go_root and the cache locations, or print one
# line and return 1. go_root is where the resolved toolchain lives on disk
# ("go env GOROOT" of the caller's environment): every Go command of a check,
# of a task setup and of the preflight runs that toolchain's own bin/go, so none
# of them looks "go" up again by name or through HOME. --no-preflight (test
# only, read here and in preflight_go) lets a failed resolve fall back to
# "local" so the self-test can drive a check through a broken go.
resolve_go() {
    go_root=""
    _rg_need=0
    for _rg_t in $tasks; do
        [ -f "$tasks_dir/$_rg_t/repo/go.mod" ] && _rg_need=1
    done
    [ "$_rg_need" -eq 1 ] || return 0
    if ! command -v go >/dev/null 2>&1; then
        [ "$skip_preflight" -ne 1 ] || { go_skip_note "go is not installed"; go_toolchain=local; return 0; }
        go_fail_line "go is not installed (see bench/README.md)"
        return 1
    fi
    _rg_err=$(mktemp "${TMPDIR:-/tmp}/bench-resolve.XXXXXX") || return 1
    # GOPROXY=off: resolving must not download either. GOFLAGS= : the caller's
    # flags (-mod=..., -modfile=...) must not steer which toolchain is found.
    if ! _rg_v=$(cd "$repo_root" && GOFLAGS='' GOPROXY=off go env GOVERSION 2>"$_rg_err"); then
        _rg_msg=$(head -n 1 "$_rg_err")
        rm -f "$_rg_err"
        [ "$skip_preflight" -ne 1 ] || { go_skip_note "$_rg_msg"; go_toolchain=local; return 0; }
        go_fail_line "$_rg_msg"
        return 1
    fi
    rm -f "$_rg_err"
    # "go1.26.11" (possibly followed by an experiment list); a development
    # build cannot be pinned by name, so it stays on "local" (no go_root).
    _rg_v=${_rg_v%% *}
    case "$_rg_v" in
        go[0-9]*) go_toolchain="$_rg_v" ;;
        *) go_toolchain="local" ;;
    esac
    if [ "$go_toolchain" != local ]; then
        # The toolchain's own go, run without GOROOT and without switching,
        # must report the version that was just resolved: a caller-exported
        # GOROOT, a wrapper or a shim would otherwise pair this version label
        # with another tree.
        _rg_root=$(cd "$repo_root" && GOFLAGS='' GOPROXY=off go env GOROOT 2>/dev/null) || _rg_root=""
        _rg_rv=""
        if [ -n "$_rg_root" ] && [ -x "$_rg_root/bin/go" ]; then
            _rg_rv=$(cd "$repo_root" && env -u GOROOT GOFLAGS= GOENV=off GOPROXY=off GOTOOLCHAIN=local "$_rg_root/bin/go" env GOVERSION 2>/dev/null | head -n 1) || _rg_rv=""
        fi
        _rg_rv=${_rg_rv%% *}
        if [ "$_rg_rv" != "$go_toolchain" ]; then
            _rg_msg="the toolchain root '${_rg_root:-unknown}' does not hold a go that reports $go_toolchain (it reports '${_rg_rv:-nothing}')"
            [ "$skip_preflight" -ne 1 ] || { go_skip_note "$_rg_msg"; go_toolchain=local; return 0; }
            go_fail_line "$_rg_msg"
            return 1
        fi
        go_root="$_rg_root"
    fi
    go_modcache=$(cd "$repo_root" && GOFLAGS='' GOPROXY=off go env GOMODCACHE 2>/dev/null) || go_modcache=""
    go_cache=$(cd "$repo_root" && GOFLAGS='' GOPROXY=off go env GOCACHE 2>/dev/null) || go_cache=""
    go_path=$(cd "$repo_root" && GOFLAGS='' GOPROXY=off go env GOPATH 2>/dev/null) || go_path=""
}

# with_go_env <command>...: run a check or setup hook with the resolved Go
# settings in its environment (and nowhere else: the agent never sees them),
# under timeout (check_timeout, kill_grace; status 124/137 on a hit). Runs in a
# subshell.
with_go_env() {
    (
        unset BENCH_PREFLIGHT BENCH_GOTOOLCHAIN BENCH_GOROOT BENCH_GOMODCACHE BENCH_GOCACHE BENCH_GOPATH BENCH_GO_TEST_TIMEOUT
        # "go test" gets its own -timeout, below the outer bound, so a hanging
        # test binary ends as a test failure (decided by gocheck_blame) and
        # not as a timed-out check (an error that stops the bench).
        _wg_t=$((check_timeout * 2 / 3))
        [ "$_wg_t" -ge 1 ] || _wg_t=1
        BENCH_GO_TEST_TIMEOUT="$_wg_t"; export BENCH_GO_TEST_TIMEOUT
        # The binary time limit of add-flag follows the harness's kill grace.
        BENCH_KILL_GRACE="$kill_grace"; export BENCH_KILL_GRACE
        if [ -n "$go_toolchain" ]; then
            BENCH_GOTOOLCHAIN="$go_toolchain"; export BENCH_GOTOOLCHAIN
            [ -z "$go_root" ] || { BENCH_GOROOT="$go_root"; export BENCH_GOROOT; }
            [ -z "$go_modcache" ] || { BENCH_GOMODCACHE="$go_modcache"; export BENCH_GOMODCACHE; }
            [ -z "$go_cache" ] || { BENCH_GOCACHE="$go_cache"; export BENCH_GOCACHE; }
            [ -z "$go_path" ] || { BENCH_GOPATH="$go_path"; export BENCH_GOPATH; }
        fi
        timeout -k "$kill_grace" "$check_timeout" "$@"
    )
}

# preflight_go: before any run (and so before any model request) prove that
# the Go the checks use can run the Go tasks: setup.sh and every Go check are
# run once on a pristine task tree (repo/ with the reference solution on top;
# the check runs in BENCH_PREFLIGHT=1 mode, where any failure is an
# environment error). One line on stderr and return 1 when one cannot; nothing
# is written to the results.
preflight_go() {
    [ -n "$go_toolchain" ] || return 0
    [ "$skip_preflight" -ne 1 ] || return 0
    _pf_root="$cache_home/wakil-bench/preflight"
    mkdir -p "$_pf_root" || { echo "$prog: cannot create $_pf_root" >&2; return 1; }
    for _pf_t in $tasks; do
        _pf_d="$tasks_dir/$_pf_t"
        [ -f "$_pf_d/repo/go.mod" ] || continue
        _pf_tmp=$(mktemp -d "$_pf_root/$_pf_t.XXXXXX") || return 1
        _pf_out="$_pf_tmp.out"
        _pf_bad=""
        if [ -x "$_pf_d/setup.sh" ]; then
            mkdir "$_pf_tmp/s"
            # As prep_workdir does: the task's repo/ is in place before setup.sh runs.
            cp -R "$_pf_d/repo/." "$_pf_tmp/s/" || _pf_bad="cannot copy $_pf_t for its setup"
            [ -n "$_pf_bad" ] || ( cd "$_pf_tmp/s" && with_go_env "$_pf_d/setup.sh" ) >"$_pf_out" 2>&1 9>&- || _pf_bad="setup of $_pf_t: $(head -n 1 "$_pf_out")"
        fi
        if [ -z "$_pf_bad" ] && grep -Eq 'gocheck_prepare|gocheck_env' "$_pf_d/check.sh"; then
            [ -d "$_pf_d/solution" ] || _pf_bad="task $_pf_t has no solution/ to run its check on"
        fi
        if [ -z "$_pf_bad" ] && grep -Eq 'gocheck_prepare|gocheck_env' "$_pf_d/check.sh"; then
            mkdir "$_pf_tmp/c"
            cp -R "$_pf_d/repo/." "$_pf_tmp/c/" && cp -R "$_pf_d/solution/." "$_pf_tmp/c/" || _pf_bad="cannot copy $_pf_t"
            if [ -z "$_pf_bad" ]; then
                ( cd "$_pf_tmp/c" && with_go_env env BENCH_PREFLIGHT=1 "$_pf_d/check.sh" ) >"$_pf_out" 2>&1 9>&- || _pf_bad="check of $_pf_t: $(grep -m1 '^check:' "$_pf_out" || head -n 1 "$_pf_out")"
            fi
        fi
        rm -rf "$_pf_tmp" "$_pf_out"
        if [ -n "$_pf_bad" ]; then
            go_fail_line "$_pf_bad"
            return 1
        fi
    done
}

# check_result_for_rc <status>: the result class of a check's exit status:
# 0 pass, 1 fail (the check ran and judged the agent's work), anything else
# error (the check could not judge: bench/lib/gocheck.sh uses 99; 124/137 a
# timeout; 126/127 a missing interpreter; ...).
check_result_for_rc() {
    case "$1" in
        0) echo pass ;;
        1) echo fail ;;
        *) echo error ;;
    esac
}

# na <value>: the CSV cell for a value a binary did not report.
na() {
    case "$1" in
        '') echo 'n/a' ;;
        null) echo 'n/a' ;;
        *) echo "$1" ;;
    esac
}

# fmt_k <n>: "18k" for 18000, "500" for 500. Non-numeric passes through.
fmt_k() {
    case "$1" in
        ''|n/a) echo 'n/a' ;;
        *[!0-9]*) echo "$1" ;;
        *)
            if [ "$1" -ge 1000 ]; then
                printf '%dk' "$(( $1 / 1000 ))"
            else
                printf '%d' "$1"
            fi
            ;;
    esac
}

# json_field <file> <field>: the named top-level field, or empty when the
# file is absent, jq is missing, or the field is null. Never fails.
json_field() {
    _file=$1; _field=$2
    if [ -f "$_file" ] && command -v jq >/dev/null 2>&1; then
        jq -r ".${_field} // empty" "$_file" 2>/dev/null || echo ""
    else
        echo ""
    fi
}

# fmt_cost <n>: format USD cost with 4 decimals ("0.0038"). Non-numeric passes through.
fmt_cost() {
    case "$1" in
        ''|n/a) echo 'n/a' ;;
        *) awk -v v="$1" 'BEGIN { if (v+0 == v) printf "%.4f", v; else printf "%s", v }' ;;
    esac
}

# csv_field <value>: one CSV cell. Every cell of the results file goes
# through this function. A CR or LF in the value becomes a space (a result
# row is one physical line, so line-based readers and "tail -1" stay valid),
# then RFC 4180 quoting: a value with a comma or a double quote is wrapped in
# double quotes and its quotes are doubled. The values come from outside (a
# model name from the config, a version line from the binary, a reason from
# the result JSON), so none is trusted to be plain.
csv_field() {
    _cf=$(printf '%s' "$1" | tr '\r\n' '  ')
    case "$_cf" in
        *,*|*\"*)
            _cf=$(printf '%s' "$_cf" | sed 's/"/""/g')
            printf '"%s"' "$_cf" ;;
        *) printf '%s' "$_cf" ;;
    esac
}

# csv_line <value>...: the cells joined by commas, one line.
csv_line() {
    _cl_first=1
    for _cl_v in "$@"; do
        [ "$_cl_first" -eq 1 ] || printf ','
        _cl_first=0
        csv_field "$_cl_v"
    done
    printf '\n'
}

# CSV_AWK_LIB: a quote-aware splitter shared by every reader of the results
# file (POSIX awk only, no gawk extensions). csvsplit(line, arr) fills
# arr[1..n] with the cells of one line and returns n.
CSV_AWK_LIB='
function csvsplit(line, a,    n, i, c, field, inq, L) {
    split("", a)
    n = 0; field = ""; inq = 0; L = length(line)
    for (i = 1; i <= L; i++) {
        c = substr(line, i, 1)
        if (inq) {
            if (c == "\"") {
                if (substr(line, i + 1, 1) == "\"") { field = field "\""; i++ }
                else inq = 0
            } else field = field c
        } else if (c == "\"") inq = 1
        else if (c == ",") { a[++n] = field; field = "" }
        else field = field c
    }
    a[++n] = field
    return n
}
'

# csv_cell <file> <row|last> <col>: one cell of one row (rows count from 1,
# the header is row 1).
csv_cell() {
    awk -v want="$2" -v col="$3" "$CSV_AWK_LIB"'
        { rows[NR] = $0 }
        END {
            r = (want == "last") ? NR : want + 0
            if (r < 1 || r > NR) exit
            csvsplit(rows[r], a)
            print a[col]
        }' "$1"
}

# csv_row <task> <run> <result> <exit_reason> <seconds> <result-json>
# Appends one row to the CSV. The last two columns are appended (never
# inserted): valid (true, or false:<reason>) and provider (the OpenRouter
# provider that served the requests, when the binary reports it; empty
# otherwise, including non-OpenRouter backends the binary does not name).
# A completed collapse-long run with 0 collapses is not scorable: its
# result is rewritten to "invalid" here (single computation point, so
# every call site behaves identically) and valid is false:no_collapse.
# A collapse-long run whose user's config is unparseable never launched:
# the bench writes result=error, exit_reason=config-unreadable, and
# valid=false:config (the config-unreadable reason is more specific than
# a generic error and is checked before the generic error branch).
# Timeout and error results are never valid.
csv_row() {
    task=$1; run=$2; result=$3; exit_reason=$4; secs=$5; rj=$6
    _collapses=$(json_field "$rj" "collapses")
    if [ "$task" = "collapse-long" ] && { [ "$result" = "pass" ] || [ "$result" = "fail" ]; } && { [ -z "$_collapses" ] || [ "$_collapses" = "0" ] || [ "$_collapses" = "null" ]; }; then
        result="invalid"
    fi
    case "$result" in
        invalid) _valid="false:no_collapse" ;;
        timeout) _valid="false:timeout" ;;
        error) case "$exit_reason" in
                config-unreadable) _valid="false:config" ;;
                *) _valid="false:error" ;;
            esac ;;
        *) _valid="true" ;;
    esac
    _provider=$(json_field "$rj" "provider")
    csv_line "$(date +%Y-%m-%d)" "$wakil_version" "$resolved_model" "$task" "$run" "$result" \
        "$(na "$exit_reason")" "$secs" \
        "$(na "$(json_field "$rj" "tokens_in")")" "$(na "$(json_field "$rj" "tokens_out")")" \
        "$(na "$(json_field "$rj" "tokens_cached")")" \
        "$(na "$(json_field "$rj" "cost_usd")")" "$(na "$(json_field "$rj" "tool_calls")")" \
        "$(na "$_collapses")" "$(na "$(json_field "$rj" "collapses_turn_end")")" \
        "$(na "$(json_field "$rj" "mashura_rounds")")" \
        "$(na "$(json_field "$rj" "context_modules")")" \
        "$(na "$(json_field "$rj" "modules_loaded")")" \
        "$_valid" "$_provider" >> "$csv_file"
}

# prep_workdir <task>: empty and refill the fixed work folder from the
# task's repo/, preserving the .wakil/ directory (the user's approved
# .wakil/sandbox.json lives there and the approval is keyed on its
# content — sha256 of its raw bytes). The .wakil/ directory is moved
# aside (mv) before the folder is emptied and moved back after, so it
# is never copied or re-created — the directory inode and its content
# stay untouched. A repo-local identity ("wakil-bench" / "bench@localhost")
# is set after git init so the commit has an identity without reading any
# host config file. The bench runs with the user's normal profile — no
# GIT_CONFIG_GLOBAL override, no GIT_CONFIG_* unsetting, no commit.gpgsign
# override.
prep_workdir() {
    task=$1
    tdir="$tasks_dir/$task"
    workdir="$work_root/$task"

    # Recover .wakil/ from an interrupted prep: if a prior prep_workdir was
    # killed after moving .wakil/ aside but before moving it back, the saved
    # copy sits as a sibling of the work folder. Move it back before anything
    # else so the approval is not lost. The saved path is under $work_root
    # (same filesystem, so mv is an atomic rename, not a copy+delete).
    _wakil_saved=""
    _wakil_save_dir="$work_root/.wakil-saved-$task"
    if [ -d "$_wakil_save_dir" ] && [ ! -d "$workdir/.wakil" ]; then
        mkdir -p "$workdir" || { echo "$prog: prep_workdir: cannot create $workdir for recovery" >&2; return 1; }
        mv "$_wakil_save_dir" "$workdir/.wakil" || { echo "$prog: prep_workdir: cannot recover saved .wakil from $_wakil_save_dir" >&2; return 1; }
    elif [ -d "$_wakil_save_dir" ] && [ -d "$workdir/.wakil" ]; then
        echo "$prog: prep_workdir: both saved ($_wakil_save_dir) and active ($workdir/.wakil) .wakil/ exist — cannot decide which to keep; remove one manually" >&2
        return 1
    fi

    # Move .wakil/ aside so it survives the folder cleanup untouched. The
    # saved path is a sibling under $work_root (same filesystem) so mv is an
    # atomic rename, not a copy+delete. If the mv fails, abort before the
    # wipe — otherwise .wakil/ would be permanently deleted.
    if [ -d "$workdir/.wakil" ]; then
        _wakil_saved="$_wakil_save_dir"
        mv "$workdir/.wakil" "$_wakil_saved" || { echo "$prog: prep_workdir: cannot save .wakil to $_wakil_saved — aborting before wipe" >&2; return 1; }
    fi

    # Delete everything in the folder (it is now empty of .wakil/). This
    # function runs on the left of "||", where "set -e" does not apply, so
    # every step is checked by hand: a failed wipe must never leave files of
    # an earlier attempt in what is meant to be a fresh folder.
    if [ -d "$workdir" ]; then
        rm -rf "$workdir" 2>/dev/null || true
    fi
    mkdir -p "$workdir" || { echo "$prog: prep_workdir: cannot create $workdir" >&2; return 1; }

    # Move .wakil/ back so the directory inode and content are untouched.
    if [ -n "$_wakil_saved" ]; then
        mv "$_wakil_saved" "$workdir/.wakil" || { echo "$prog: prep_workdir: cannot restore .wakil in $workdir" >&2; return 1; }
    fi

    # Nothing but .wakil/ may be left of the earlier run.
    _stale=$(find "$workdir" -mindepth 1 -maxdepth 1 ! -name .wakil 2>/dev/null | head -n 1)
    if [ -n "$_stale" ]; then
        echo "$prog: prep_workdir: $workdir was not emptied (e.g. $_stale) — aborting" >&2
        return 1
    fi

    # Fresh copy of the task's repo/, then the optional setup hook, then one
    # commit with a repo-local identity. The hook runs BEFORE the commit so
    # nothing it generates from (a generator, a seed) can end up in the
    # folder's git history; a failing hook aborts the run (a half-built task
    # must not be scored as a model failure). The hook's own location is the
    # task dir (outside the work folder); it addresses the work copy as its
    # working directory.
    cp -R "$tdir/repo/." "$workdir/" || { echo "$prog: prep_workdir: cannot copy $tdir/repo into $workdir" >&2; return 1; }
    if [ -x "$tdir/setup.sh" ]; then
        if ! ( cd "$workdir" && with_go_env "$tdir/setup.sh" ) >"$workdir/.bench-setup.log" 2>&1 9>&-; then
            echo "$prog: prep_workdir: setup.sh of $task failed — aborting:" >&2
            cat "$workdir/.bench-setup.log" >&2
            return 1
        fi
        rm -f "$workdir/.bench-setup.log"
    fi
    (
        cd "$workdir"
        git init -q
        git config user.name "wakil-bench"
        git config user.email "bench@localhost"
        # Exclude .wakil/ so it does not show up as untracked (it is
        # the sandbox approval file, not part of the task repo).
        printf '.wakil/\n' >> .git/info/exclude
        git add -A
        git commit -qm "bench task $task"
    ) 2>/dev/null
    _prep_rc=$?
    if [ "$_prep_rc" -ne 0 ]; then
        echo "$prog: prep_workdir: git init or commit failed for $task in $workdir (exit $_prep_rc) — aborting" >&2
        return 1
    fi

    # The agent under test must not be able to see the check, the hidden
    # files, the solution, or what a task's fixture was generated from.
    leak_check "$workdir" "$task" || return 1
}

# leak_check <workdir> <task>: fatal pre-agent guard. The work folder was
# just built; nothing of the task's check, hidden files, solution or
# generator may be in it, in its tree or in its git history. Runs BEFORE the
# agent, so the git command reads a repository the harness itself created
# (never run git in the work folder after the agent: its .git/config is
# agent-controlled).
leak_check() {
    _lw=$1; _lt=$2
    for _n in check.sh hidden solution gen setup.sh; do
        if [ -e "$_lw/$_n" ]; then
            echo "$prog: LEAK in $_lt — $_n is present in the work folder $_lw" >&2
            return 1
        fi
    done
    if [ -d "$_lw/cmd/gen" ]; then
        echo "$prog: LEAK in $_lt — a generator (cmd/gen) is present in the work folder" >&2
        return 1
    fi
    # grep: 0 a match (leak), 1 none, anything else = the scan did not work
    # (an unsupported option, an unreadable file): fail closed, never "clean".
    _lg_rc=0
    grep -rIl --exclude-dir=.git --exclude-dir=.wakil -e 'rand\.NewSource' -e 'rand\.Seed' "$_lw" >/dev/null 2>&1 || _lg_rc=$?
    case "$_lg_rc" in
        0) echo "$prog: LEAK in $_lt — a seeded generator is present in the work folder" >&2; return 1 ;;
        1) ;;
        *) echo "$prog: cannot scan $_lw for a seeded generator (grep status $_lg_rc) — aborting" >&2; return 1 ;;
    esac
    _hist=$(git -C "$_lw" log --all --name-only --format= 2>/dev/null | sort -u)
    for _n in check.sh setup.sh; do
        case "
$_hist
" in
            *"
$_n
"*) echo "$prog: LEAK in $_lt — $_n is in the work folder's git history" >&2; return 1 ;;
        esac
    done
    case "$_hist" in
        *hidden/*|*solution/*|*gen/*|*cmd/gen*) echo "$prog: LEAK in $_lt — hidden/solution/generator paths are in the work folder's git history" >&2; return 1 ;;
    esac
    return 0
}

# wakil_digest <workdir>: a digest of the files under <workdir>/.wakil, read
# only. Empty when there is no such directory. The harness never writes into
# .wakil/; it only notices when the agent did (a file left there survives the
# refill and could carry state from one run to the next).
wakil_digest() {
    [ -d "$1/.wakil" ] || return 0
    ( cd "$1/.wakil" && find . -type f -exec sha256sum {} + 2>/dev/null | sort | sha256sum | awk '{ print $1 }' )
}

# lock_task <task>: take the exclusive lock of one task's work folder on
# fd 9 (flock -n: never waits). Two benches on the same task would otherwise
# empty and refill the same folder under each other. A held lock prints a
# message and returns 75 without touching the folder; the caller must not
# write a CSV row for it. The fd stays open until unlock_task.
#
# No program the bench starts may inherit the lock: a flock belongs to the open
# file description, so one leaked descriptor (an orphaned "sleep" of the
# heartbeat was the one that blocked --runs 2) keeps the lock after the bench
# let go. Every call that runs with the lock held is therefore made with 9>&-
# (run_one, run_all, do_setup): the shell keeps the lock on a close-on-exec copy
# of the descriptor for the length of the call, and nothing it starts inherits
# it. The heartbeat is a fork, not an exec, so it is torn down by hand (see
# run_one_locked) and unlock_task releases the lock itself (flock -u), which
# frees it even if some descriptor did leak.
lock_task() {
    if ! command -v flock >/dev/null 2>&1; then
        echo "$prog: flock not found — refusing to run: two benches on one task would delete each other's work folder" >&2
        return 1
    fi
    mkdir -p "$lock_root" || { echo "$prog: cannot create $lock_root" >&2; return 1; }
    _lk_file="$lock_root/$1.lock"
    exec 9>"$_lk_file"
    if ! flock -n 9; then
        exec 9>&-
        echo "$prog: task $1 is already being run by another bench (lock $_lk_file) — its work folder was not touched" >&2
        return 75
    fi
}

# unlock_task: release the lock taken by lock_task.
unlock_task() {
    flock -u 9 2>/dev/null || true
    exec 9>&-
}

# run_one <task> <run> <wakil-bin>
# Sets up the fixed work folder, runs wakil under the timeout, then
# checks. The task's check and reference solution are never copied into
# the work folder. Always returns 0 (a failed task is a result, not a
# harness error); a crashed run still leaves a CSV row.
run_one() {
    lock_task "$1" || return $?
    _ro_rc=0
    run_one_locked "$@" 9>&- || _ro_rc=$?
    unlock_task
    return "$_ro_rc"
}

# run_one_locked <task> <run> <wakil-bin>: run_one with the task lock held.
run_one_locked() {
    task=$1; run=$2; bin=$3
    tdir="$tasks_dir/$task"
    workdir="$work_root/$task"
    hb_pid=""   # never carry a heartbeat pid over from an earlier run

    prep_workdir "$task" || return 1

    task_text=$(cat "$tdir/task.txt")
    mkdir -p "$run_root" || { echo "$prog: cannot create $run_root" >&2; return 1; }
    result_json="$run_root/$task.json"
    rm -f "$result_json"
    log_file="$results_dir/${task}-${run}-$(date +%H%M%S).log"
    st_last_log="$log_file"

    # Progress: one line before each run so a long session is not silent.
    printf '[%d/%d] %s (run %s/%d) … running, log: %s\n' \
        "$task_idx" "$total_tasks" "$task" "$run" "$runs" "$log_file"

    wakil_before=$(wakil_digest "$workdir")
    start_epoch=$(date +%s)
    set +e
    # WAKIL_HOST_WORKDIR points the sandbox at the fixed work folder so
    # the agent runs there, not in the configured work_dir.
    export WAKIL_HOST_WORKDIR="$workdir"
    export WAKIL_CONTEXT_MODULES="$context_modules"
    # collapse-long only: a bench-only collapse-threshold override through
    # a merged per-run config file (WAKIL_CONFIG). Wakil reads exactly one
    # config file (defaults <- file <- env <- flags, internal/config
    # LoadConfig) and the two collapse keys are file-only, so the harness
    # merges the user's own resolved config with only those two keys and
    # points the run at the merged file. The ceiling (45,000 tokens) and
    # min_bytes (2,000) are identical for modules off and on, so every model
    # collapses at the same projected size regardless of its probed window
    # (a fixture big enough for a 1M-token model would time out small ones).
    # Wakil itself is unchanged. The merged file carries the user's
    # endpoints (with keys) — it lives in a private 0700 temp dir at mode
    # 0600, is deleted right after the run on every path, and its contents,
    # path, and key values are never printed or logged. The run log records
    # only the two non-secret override numbers.
    # Resolving the user's config mirrors Wakil's defaultConfigPath:
    # $WAKIL_CONFIG if set, else ~/.config/wakil/config.json.
    _wakil_config_env=""
    if [ "$task" = "collapse-long" ]; then
        _user_cfg="${WAKIL_CONFIG:-$HOME/.config/wakil/config.json}"
        if [ ! -f "$_user_cfg" ]; then
            # Wakil tolerates a missing file (defaults + env apply); merge
            # onto an empty object so env-only setups still run.
            _user_cfg=""
        fi
        _bench_cfg_tmp=$(mktemp -d 2>/dev/null) || { echo "$prog: cannot create private config dir for $task" >&2; set -e; return 1; }
        _cfg_file="$_bench_cfg_tmp/config.json"
        if [ -n "$_user_cfg" ]; then
            # Merge the user's file with only the two collapse keys. jq
            # stderr is discarded (a malformed user file would print
            # surrounding context); the exit code decides validity of the
            # user file. An unparseable user file means wakil itself would
            # fail on it — that is recorded via the config-unreadable
            # exit_reason below, not by a bench abort.
            ( umask 077 && jq '. + {context_collapse_ceiling_tokens: 45000, context_collapse_min_bytes: 2000}' "$_user_cfg" > "$_cfg_file" ) 2>/dev/null
            if [ $? -ne 0 ] || [ ! -s "$_cfg_file" ]; then
                rm -f "$_cfg_file"
                printf 'collapse-long override: user config unparseable (bench did not launch; valid=false:config)\n' >> "$log_file" 2>/dev/null || true
                rm -rf "$_bench_cfg_tmp"
                _bench_cfg_tmp=""
                csv_row "$task" "$run" "error" "config-unreadable" 0 ""
                return 0
            fi
        else
            printf '%s\n' '{"context_collapse_ceiling_tokens":45000,"context_collapse_min_bytes":2000}' > "$_cfg_file"
        fi
        _wakil_config_env="WAKIL_CONFIG=$_cfg_file"
        printf 'collapse-long override: merged config (ceiling=45000 min_bytes=2000)\n' >> "$log_file" 2>/dev/null || true
    fi

    if [ -n "$model_name" ]; then
        if [ -n "$_wakil_config_env" ]; then
            env "WAKIL_CONFIG=$_cfg_file" "WAKIL_MODEL=$model_name" timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
                --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 9>&- &
        else
            WAKIL_MODEL="$model_name" timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
                --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 9>&- &
        fi
    else
        if [ -n "$_wakil_config_env" ]; then
            env "WAKIL_CONFIG=$_cfg_file" timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
                --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 9>&- &
        else
            timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
                --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 9>&- &
        fi
    fi
    run_pid=$!

    # Heartbeat: while the run is in progress, print one short line with the
    # elapsed time every 30s (only when stdout is a terminal), so a long
    # task does not look hung. Nothing extra when output is piped or
    # redirected. Skipped when the timeout is too short for a heartbeat.
    # The heartbeat runs in the background so it does not inflate the
    # recorded duration; wait in the foreground captures the real exit
    # code and the real elapsed time.
    if [ "$is_tty" -eq 1 ] && [ "$timeout_sec" -gt "$hb_interval" ]; then
        # The sleep runs in the background and is waited for, so the TERM that
        # ends the heartbeat is acted on at once (a trap waits for a foreground
        # command to finish) and takes the sleep with it: nothing of the
        # heartbeat outlives the run. "exit 0" keeps the shell from reporting
        # the signal.
        (
            _hb_sleep=""
            trap '[ -z "$_hb_sleep" ] || kill "$_hb_sleep" 2>/dev/null; exit 0' TERM
            while kill -0 "$run_pid" 2>/dev/null; do
                sleep "$hb_interval" &
                _hb_sleep=$!
                wait "$_hb_sleep" 2>/dev/null || true
                _hb_sleep=""
                kill -0 "$run_pid" 2>/dev/null || break
                now=$(date +%s)
                elapsed=$((now - start_epoch))
                printf '[%d/%d] %s (run %s/%d) … %ds elapsed\n' \
                    "$task_idx" "$total_tasks" "$task" "$run" "$runs" "$elapsed"
            done
        ) &
        hb_pid=$!
    fi

    wait "$run_pid"
    wak_rc=$?
    set -e
    # Kill the heartbeat if it is still running (timeout may have killed
    # the run before the heartbeat's kill -0 saw it).
    if [ -n "${hb_pid:-}" ]; then
        kill "$hb_pid" 2>/dev/null || true
        wait "$hb_pid" 2>/dev/null || true
    fi
    # The merged carry-secrets config is deleted on every run path, before
    # any guard return below can skip it. The EXIT/INT/TERM backstop (the
    # composed st_on_exit for self-test, or the normal-mode trap) removes
    # any orphan if the bench shell itself exits with an error.
    if [ -n "${_bench_cfg_tmp:-}" ]; then
        rm -rf "$_bench_cfg_tmp"
        _bench_cfg_tmp=""
    fi
    end_epoch=$(date +%s)
    secs=$((end_epoch - start_epoch))
    if [ "$(wakil_digest "$workdir")" != "$wakil_before" ]; then
        echo "$prog: warning: $task run $run changed files under $workdir/.wakil — it survives the refill and may carry state into the next run" >&2
    fi
    cat "$log_file.out" >> "$log_file" 2>/dev/null || true
    rm -f "$log_file.out"

    # Resolve the model from the result file when we did not set one and a
    # real binary answered (honest identity: resolved, not requested).
    if [ -z "$model_name" ] && [ -f "$result_json" ]; then
        rm_model=$(json_field "$result_json" "model")
        [ -n "$rm_model" ] && resolved_model="$rm_model"
    fi

    exit_reason=$(json_field "$result_json" "exit_reason")

    # Timeout: "timeout" exits 124 when the command ended on the TERM it sent
    # at the deadline, and 137 (128 + KILL) when the command ignored TERM and
    # was killed after the grace period. Both are a timeout: the run was cut
    # off mid-way, so the result JSON it may have written before hanging
    # (exit_reason "pass", workdir) is stale, the log is not looked at (it is
    # agent-controlled text), and neither the workdir guard nor check.sh runs.
    # A 137 that arrives well before the deadline is a kill from outside
    # (e.g. the OOM killer); it is still no completed run, so it is also
    # "timeout", but the exit_reason says "killed".
    if [ "$wak_rc" -eq 124 ] || [ "$wak_rc" -eq 137 ]; then
        result="timeout"
        exit_reason="timeout"
        if [ "$wak_rc" -eq 137 ] && [ "$secs" -lt "$timeout_sec" ]; then
            exit_reason="killed"
        fi
        # The result JSON of a cut-off run is stale or half-written, so its
        # metrics are not recorded either (an empty path reads as n/a).
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" ""
    else
        # Post-run "not acknowledged" / "has not been approved" detection: when
        # the run did not succeed and the log mentions either sandbox approval
        # error, the exit reason is sandbox-not-acknowledged and the bench stops
        # with the folder and the one command to run, BEFORE any workdir guard
        # message or check.sh.
        if [ "$wak_rc" -ne 0 ]; then
            if [ -z "$exit_reason" ] || [ "$exit_reason" = "error" ]; then
                if grep -q "sandbox risk is not acknowledged" "$log_file" 2>/dev/null || \
                   grep -q "has not been approved" "$log_file" 2>/dev/null; then
                    result="error"
                    exit_reason="sandbox-not-acknowledged"
                    csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
                    echo "$prog: bench stopped — task $task run $run: sandbox approval required for $workdir" >&2
                    echo "  run: WAKIL_HOST_WORKDIR=$workdir wakil sandbox trust" >&2
                    return 1
                fi
            fi
        fi

        # Workdir guard: verify the agent ran in the fixed folder we set
        # up, not in the user's configured work_dir. The result JSON carries
        # the executor's own workspace root and the resolved sandbox backend.
        # The root is a HOST path for hisn (symlink-resolved) and direct, so it
        # is compared with the folder and with its "pwd -P" form; for docker it
        # is a container path (/mnt/<name>) that can never equal a host path, so
        # it can neither be compared nor trusted: the run is an error, not a
        # silent pass and not a wrong-workdir false alarm. An absent backend
        # field (an older binary, a stub) means the plain comparison. check.sh
        # is skipped on every failure and the bench stops.
        _actual_workdir=$(json_field "$result_json" "workdir")
        _backend=$(json_field "$result_json" "sandbox_backend")
        if [ -z "$_actual_workdir" ]; then
            echo "$prog: MISSING WORKDIR in $task run $run: binary did not report workdir" >&2
            result="error"
            exit_reason="missing-workdir"
            csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
            cat "$log_file" 2>/dev/null || true
            return 1
        fi
        if [ "$_backend" = "docker" ]; then
            echo "$prog: workdir not verifiable (docker) in $task run $run: the binary reports the container path $_actual_workdir, not the host folder $workdir" >&2
            result="error"
            exit_reason="workdir-not-verifiable"
            csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
            cat "$log_file" 2>/dev/null || true
            return 1
        fi
        _real_workdir=$(cd "$workdir" 2>/dev/null && pwd -P) || _real_workdir="$workdir"
        if [ "$_actual_workdir" != "$workdir" ] && [ "$_actual_workdir" != "$_real_workdir" ]; then
            echo "$prog: WRONG WORKDIR in $task run $run: binary reports workdir=$_actual_workdir, expected $workdir — agent ran in the wrong folder" >&2
            result="error"
            exit_reason="wrong-workdir"
            csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
            cat "$log_file" 2>/dev/null || true
            return 1
        fi

        # Post-run leak guard (file existence only; no git here, the work
        # folder's .git is agent-controlled now): nothing but repo/ was
        # copied in, so one of these names means the agent got hold of the
        # check, hidden files or the solution. Never scored; stops the bench.
        for _n in check.sh hidden solution; do
            if [ -e "$workdir/$_n" ] || [ -L "$workdir/$_n" ]; then
                echo "$prog: LEAK in $task run $run — $_n is present in the work folder after the run" >&2
                result="error"
                exit_reason="leak-detected"
                csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
                return 1
            fi
        done

        # Run check.sh after every completed run (pass, declined, or error).
        # Result is pass or fail from check.sh; exit_reason keeps whatever
        # the binary reported. check.sh lives in the task dir (never in the
        # work folder) and runs in the work folder.
        set +e
        { printf '\n--- check.sh (%s) ---\n' "$task"; ( cd "$workdir" && with_go_env "$tdir/check.sh" ) 2>&1; } >>"$log_file" 2>&1 9>&-
        chk_rc=$?
        set -e
        result=$(check_result_for_rc "$chk_rc")
        if [ "$result" = "error" ]; then
            # The check could not judge: not a "fail" of the agent. It is not
            # scored, the same environment would give the same on the next
            # run, so the bench stops (like the guards above).
            case "$chk_rc" in
                124|137) _chk_why="timed out after ${check_timeout}s" ;;
                *) _chk_why="ended with status $chk_rc" ;;
            esac
            printf 'check: error: check.sh %s\n' "$_chk_why" >>"$log_file"
            exit_reason="check-error"
            csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
            echo "$prog: bench stopped — task $task run $run: the check could not judge ($_chk_why); see $log_file" >&2
            grep '^check:' "$log_file" | tail -n 2 >&2 || true
            return 1
        fi
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
    fi

    # Progress: one line after each run with the result, elapsed time,
    # tokens, cost, and exit reason (for non-pass results, from
    # --result-json when available).
    _tok_in=$(json_field "$result_json" "tokens_in")
    _tok_out=$(json_field "$result_json" "tokens_out")
    _cost=$(json_field "$result_json" "cost_usd")
    _summary="${result} ${secs}s"
    if [ -n "$_tok_in" ] && [ -n "$_tok_out" ]; then
        _summary="$_summary · $(fmt_k "$_tok_in") in / $(fmt_k "$_tok_out") out"
    fi
    if [ -n "$_cost" ]; then
        _summary="$_summary · \$$(fmt_cost "$_cost")"
    fi
    if [ "$result" != "pass" ] && [ -n "$exit_reason" ]; then
        _summary="$_summary · exit $exit_reason"
    fi
    printf '%s\n' "$_summary"

}

# find_prev_csv: the most recent bench CSV whose model matches the
# resolved model (not this run's, not a self-test). Echoes path.
# Self-test CSVs live in the cache (wakil-bench/selftest), never under
# results_dir.
find_prev_csv() {
    _want_model="${1:-}"
    last=""
    for f in "$results_dir"/20*-bench.csv; do
        [ -f "$f" ] || continue
        [ "$f" = "$csv_file" ] && continue
        # Filter by model: the CSV's model is in column 3 ($3).
        _f_model=$(csv_cell "$f" 2 3 2>/dev/null)
        [ -n "$_want_model" ] && [ "$_f_model" != "$_want_model" ] && continue
        last="$f"
    done
    printf '%s' "$last"
}

# print_table <csv> [model]: per-task summary of a CSV, with the model
# in the header line when it is known. Reads columns by header name so
# both the current 16-column CSV (with tokens_cached and
# collapses_turn_end) and older CSVs render accurately.
print_table() {
    f=$1; _model="$2"
    if [ -n "$_model" ]; then
        printf '  model: %s\n' "$_model"
    fi
    printf '  %-12s %-8s %-12s %-7s %-16s %-8s %-9s %s\n' "task" "result" "exit_reason" "seconds" "tokens in/out" "cached-%" "cost" "tools collapses mashura"
    awk "$CSV_AWK_LIB"'
        NR == 1 {
            n = csvsplit($0, f)
            for (i = 1; i <= n; i++) col[f[i]] = i
            next
        }
        {
            csvsplit($0, f)
            task  = col["task"]        ? f[col["task"]]        : "n/a"
            res   = col["result"]      ? f[col["result"]]      : "n/a"
            ereas = col["exit_reason"] ? f[col["exit_reason"]] : "n/a"
            secs  = col["seconds"]     ? f[col["seconds"]]     : "n/a"
            tin   = col["tokens_in"]   ? f[col["tokens_in"]]   : "n/a"
            tout  = col["tokens_out"]  ? f[col["tokens_out"]]  : "n/a"
            tcach = col["tokens_cached"] ? f[col["tokens_cached"]] : "n/a"
            cost  = col["cost_usd"]    ? f[col["cost_usd"]]    : "n/a"
            tools = col["tool_calls"]  ? f[col["tool_calls"]]  : "n/a"
            coll  = col["collapses"]   ? f[col["collapses"]]   : "n/a"
            mash  = col["mashura_rounds"] ? f[col["mashura_rounds"]] : "n/a"

            cpct = "n/a"
            if (tin != "" && tin != "n/a" && tcach != "" && tcach != "n/a" && (tin + 0) > 0) {
                cpct = sprintf("%.0f%%", (tcach + 0) * 100.0 / (tin + 0))
            }
            cstr = "n/a"
            if (cost != "" && cost != "n/a") {
                cstr = sprintf("$%.4f", cost + 0)
            }
            printf "  %-12s %-8s %-12s %-7s %-16s %-8s %-9s %s %s %s\n", \
                task, res, ereas, secs, tin "/" tout, cpct, cstr, tools, coll, mash
        }
    ' "$f"
}

# sum_cost <csv>: total cost_usd formatted to 4 decimals (n/a and empty
# cells contribute 0). Reads cost_usd by header name.
sum_cost() {
    awk "$CSV_AWK_LIB"'
        NR == 1 {
            n = csvsplit($0, f)
            for (i = 1; i <= n; i++) if (f[i] == "cost_usd") c = i
            next
        }
        { csvsplit($0, f) }
        c > 0 && f[c] != "n/a" && f[c] != "" { s += f[c] }
        END { printf "%.4f", s }
    ' "$1"
}

# run_all <wakil-bin>: run every task `runs` times. The lock of a task is taken
# once for the whole series (no other bench can slip in between two runs) and
# released on every way out: the end of the series, a run that stops the bench,
# and nothing at all when the lock is refused (75, nothing was taken).
run_all() {
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        lock_task "$task" || return $?
        i=1
        _ra_rc=0
        while [ "$i" -le "$runs" ]; do
            run_one_locked "$task" "$i" "$1" 9>&- || { _ra_rc=$?; break; }
            i=$((i + 1))
        done
        unlock_task
        [ "$_ra_rc" -eq 0 ] || return "$_ra_rc"
    done
}

# ---- --setup ---------------------------------------------------------------
# Creates all task work folders and prints, per folder, the exact
# 'WAKIL_HOST_WORKDIR=<path> wakil sandbox trust' command the user must run
# once (works with any binary, including older ones where positional <path>
# did not override config host_work_dir). The harness itself never runs that
# command and never reads from the terminal.
do_setup() {
    for task in $tasks; do
        tdir="$tasks_dir/$task"
        workdir="$work_root/$task"
        lock_task "$task" || return $?
        _su_rc=0
        prep_workdir "$task" 9>&- || _su_rc=1
        unlock_task
        [ "$_su_rc" -eq 0 ] || return 1
        echo "WAKIL_HOST_WORKDIR=$workdir wakil sandbox trust"
    done
    echo
    echo "Run each command above once. The acknowledgement is keyed on the"
    echo "path (sha256 of the canonical path) and the content hash of"
    echo ".wakil/sandbox.json (sha256 of its raw bytes). Emptying and"
    echo "refilling the folder does not invalidate it as long as the risk"
    echo "label (and thus the sandbox.json content) is the same."
}

# ---- self-test -------------------------------------------------------------
# tool_dir <dir> [omit...]: make <dir> hold links to exactly the programs a run
# of the harness needs (harness_tools, sh, and go and python3 for the tasks),
# minus the ones named. A case that simulates a missing tool uses this
# directory, and only this directory, as its PATH: it loses the tool under test
# and nothing else, whatever directory the host keeps its tools in.
tool_dir() {
    _td_dir=$1; shift
    mkdir -p "$_td_dir" || return 1
    for _td_t in $harness_tools sh go python3; do
        for _td_o in "$@"; do
            [ "$_td_t" != "$_td_o" ] || continue 2
        done
        if [ "$_td_t" = go ] && [ -n "$go_root" ] && [ -x "$go_root/bin/go" ]; then
            # The resolved toolchain's own go, so a program that looks "go" up
            # on this PATH (a child bench) finds the toolchain without HOME.
            _td_bin="$go_root/bin/go"
        else
            _td_bin=$(command -v "$_td_t") || continue
        fi
        case "$_td_bin" in
            /*) ln -s "$_td_bin" "$_td_dir/$_td_t" || return 1 ;;
        esac
    done
}

# st_case <heading>: print a case heading and forget the task log of the
# case before (st_last_log), so a failure report never shows another case's log.
st_case() {
    st_last_log=""
    st_fail_logs=""
    echo "$1"
}

# st_failure_report: what a failed self-test case prints before the
# temporary directories go: for each failing task its "check:" reason lines and
# the last 15 lines of its log. st_fail_logs names the logs of the failing
# tasks (a case whose logs live elsewhere sets it); without it the log of the
# last task run of the failing case (st_last_log, set by every run) is the one,
# and a case that ran no task prints nothing here (its own message says why).
# Called from the EXIT trap of a self-test that did not end with status 0, so
# every case gets it, whichever of its exits fires.
st_fail_logs=""
st_last_log=""
st_failure_report() {
    _sf_logs="$st_fail_logs"
    [ -n "$_sf_logs" ] || _sf_logs="$st_last_log"
    [ -n "$_sf_logs" ] || return 0
    for _sf_l in $_sf_logs; do
        [ -f "$_sf_l" ] || continue
        echo "---- log of the failing task: $_sf_l" >&2
        grep '^check:' "$_sf_l" >&2 || echo "(no 'check:' reason line in this log)" >&2
        echo "---- last 15 lines:" >&2
        tail -n 15 "$_sf_l" >&2
    done
    echo "----" >&2
}

# st_on_exit: EXIT trap of --self-test. A non-zero status prints the failure
# report; the private Go cache directory (go build leaves read-only files) is
# removed on every way out, as is any merged collapse-long config dir left
# by an interrupted run.
st_on_exit() {
    _se_rc=$?
    trap - EXIT
    [ "$_se_rc" -eq 0 ] || st_failure_report
    if [ -n "${st_go_private:-}" ]; then
        chmod -R u+w "$st_go_private" 2>/dev/null || true
        rm -rf "$st_go_private"
    fi
    if [ -n "${_bench_cfg_tmp:-}" ]; then
        rm -rf "$_bench_cfg_tmp"
    fi
    exit "$_se_rc"
}

self_test() {
    # Self-test CSVs and logs go to the cache, never into the repo, and never
    # where find_prev_csv looks for an earlier run. A marker file lets the end
    # of the self-test assert that nothing under bench/results changed.
    # One directory per self-test run: the cases read the logs they just wrote
    # with globs, and the logs of an earlier run (a green sandbox run leaves
    # some) must not satisfy or break them.
    selftest_base="$cache_home/wakil-bench/selftest"
    mkdir -p "$selftest_base" || { echo "$prog: cannot create $selftest_base" >&2; exit 1; }
    find "$selftest_base" -mindepth 1 -maxdepth 1 -name 'run.*' -mtime +7 -exec rm -rf {} + 2>/dev/null || true
    selftest_dir=$(mktemp -d "$selftest_base/run.XXXXXX") || { echo "$prog: cannot create a run directory in $selftest_base" >&2; exit 1; }
    _st_marker=$(mktemp "${TMPDIR:-/tmp}/bench-selftest-marker.XXXXXX")
    _saved_results_dir="$results_dir"
    results_dir="$selftest_dir"
    _saved_csv_file="$csv_file"
    _saved_work_root="$work_root"
    csv_file="$selftest_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
    printf '%s\n' "$csv_header" > "$csv_file"
    # Use a temp work root so the self-test never touches the real one.
    _self_work_root=$(mktemp -d "${TMPDIR:-/tmp}/bench-selftest-work.XXXXXX")
    work_root="$_self_work_root"

    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/bench-selftest.XXXXXX")
    # The self-test takes its locks in its own directory, never the real ones.
    _saved_lock_root="$lock_root"
    lock_root="$tmpdir/locks"
    _saved_run_root="$run_root"
    run_root="$tmpdir/run"

    # Fake user config for collapse-long: merge tests must never read the
    # host's real ~/.config/wakil/config.json (that carries live API keys).
    # The file has fake endpoints so the merge has something to keep, and
    # the bench places its merged copy in a private 0700 dir at 0600.
    # Every run_one against collapse-long in this self-test inherits this
    # WAKIL_CONFIG; other tasks ignore it.
    _fake_user_cfg="$tmpdir/fake-user-config.json"
    printf '%s\n' '{"endpoints":{"fake":{"base_url":"https://example.invalid/v1","model":"fake-model","api_key":"AKIA_FAKE_SENTINEL"}},"context_collapse_ceiling_tokens":200000,"context_collapse_min_bytes":2000}' > "$_fake_user_cfg"
    _saved_wakil_config="${WAKIL_CONFIG:-}"
    export WAKIL_CONFIG="$_fake_user_cfg"

    # Stub that changes nothing (writes a result-json with the correct
    # workdir so the guard passes, but no solution — check.sh fails).
    stub_noop="$tmpdir/wakil-noop"
    cat > "$stub_noop" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-noop/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$result_json" <<JSON
{"wakil_version":"stub-noop/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":0,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_noop"

    # Stub that copies the task's solution/ into the work folder and writes
    # a result-json, like a --result-json-aware binary that solved the task.
    stub_solve="$tmpdir/wakil-solve"
    cat > "$stub_solve" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-solve/0.0.0" ;;
  run)
    shift  # run
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    if [ -n "$BENCH_TASK_DIR" ] && [ -d "$BENCH_TASK_DIR/solution" ]; then
      cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    fi
    # collapse-long is only scorable with collapses>=1; the solve stub
    # emits 3 there — at the chosen 45,000-token ceiling the replay measured
    # 3 mid-turn collapses in both modes (plan §8.7) — so a solved
    # collapse-long run is pass/valid, not invalid (a real binary that
    # solved it after collapsing reports >0).
    _collapses=0
    case "$workdir" in */collapse-long) _collapses=3 ;; esac
    cat > "$result_json" <<JSON
{"wakil_version":"stub-solve/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":100,"tokens_out":5,"tokens_cached":75,"cost_usd":0.001234,"tool_calls":2,"collapses":$_collapses,"collapses_turn_end":1,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_solve"

    # Stub that sleeps past any timeout (writes a result-json first so the
    # workdir guard passes, then sleeps until the timeout kills it).
    stub_slow="$tmpdir/wakil-slow"
    cat > "$stub_slow" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-slow/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$result_json" <<JSON
{"wakil_version":"stub-slow/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":0,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    sleep 3600 ;;
esac
EOF
    chmod +x "$stub_slow"

    # Stub that reports a wrong workdir in result-json (the guard
    # must catch this and stop the bench with wrong-workdir).
    stub_wrong="$tmpdir/wakil-wrong"
    cat > "$stub_wrong" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-wrong/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$result_json" <<JSON
{"wakil_version":"stub-wrong/0.0.0","model":"stub","exit_reason":"pass","workdir":"/tmp/definitely-wrong","seconds":0.0,"tokens_in":1,"tokens_out":1,"cost_usd":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_wrong"

    st_case "== self-test: stub that changes nothing (expect fail on every task; collapse-long: invalid, no collapse) =="
    task_idx=0
    _fail_count=0
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="" run_one "$task" "self-noop" "$stub_noop"
        _row_result=$(csv_cell "$csv_file" last 6)
        _row_valid=$(csv_cell "$csv_file" last 19)
        if [ "$task" = "collapse-long" ]; then
            # The noop stub reports collapses:0, so a completed collapse-long
            # run is not scorable: result invalid, valid false:no_collapse.
            if [ "$_row_result" != "invalid" ] || [ "$_row_valid" != "false:no_collapse" ]; then
                echo "self-test FAILED: noop $task: expected invalid/false:no_collapse, got $_row_result/$_row_valid" >&2; _fail_count=$((_fail_count + 1))
            fi
        else
            [ "$_row_result" = "fail" ] || { echo "self-test FAILED: noop $task: expected fail, got $_row_result" >&2; _fail_count=$((_fail_count + 1)); }
        fi
    done
    [ "$_fail_count" -eq 0 ] || { rm -rf "$tmpdir" "$_self_work_root"; exit 1; }

    st_case "== self-test: stub that applies the solution (expect pass on every task; collapse-long needs collapses>=1) =="
    task_idx=0
    _pass_count=0
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="$tasks_dir/$task" run_one "$task" "self-solve" "$stub_solve"
        _row_result=$(csv_cell "$csv_file" last 6)
        [ "$_row_result" = "pass" ] || { echo "self-test FAILED: solve $task: expected pass, got $_row_result" >&2; _pass_count=$((_pass_count + 1)); }
    done
    [ "$_pass_count" -eq 0 ] || { rm -rf "$tmpdir" "$_self_work_root"; exit 1; }

    st_case "== self-test: go-empty-home (one Go check with an empty HOME, GOTOOLCHAIN=local and a failing go first on PATH passes) =="
    # The check must reach the toolchain the harness resolved at start by its
    # location, not by looking "go" up again: here the only "go" on PATH is one
    # that fails the way a host does when HOME does not hold the cached
    # toolchain.
    if [ -n "$go_toolchain" ] && [ "$go_toolchain" != local ] && [ -d "$tasks_dir/fix-test/solution" ]; then
        _eh_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-emptyhome.XXXXXX")
        tool_dir "$_eh_dir/bin" go || { echo "self-test FAILED: go-empty-home: cannot build the tool directory" >&2; rm -rf "$tmpdir" "$_self_work_root" "$_eh_dir"; exit 1; }
        printf '#!/bin/sh\necho "go: downloading %s (linux/amd64)" >&2\necho "go: download %s for linux/amd64: toolchain not available" >&2\nexit 1\n' "$go_toolchain" "$go_toolchain" > "$_eh_dir/bin/go"
        chmod +x "$_eh_dir/bin/go"
        mkdir -p "$_eh_dir/home" "$_eh_dir/w"
        cp -R "$tasks_dir/fix-test/repo/." "$_eh_dir/w/" && cp -R "$tasks_dir/fix-test/solution/." "$_eh_dir/w/"
        _eh_rc=0
        ( cd "$_eh_dir/w" && PATH="$_eh_dir/bin" HOME="$_eh_dir/home" GOTOOLCHAIN=local GOPROXY=off with_go_env "$tasks_dir/fix-test/check.sh" ) >"$_eh_dir/out" 2>&1 || _eh_rc=$?
        _eh_res=$(check_result_for_rc "$_eh_rc")
        if [ "$_eh_res" != pass ]; then
            echo "self-test FAILED: go-empty-home: the check ended with status $_eh_rc ($_eh_res), want pass:" >&2
            tail -n 5 "$_eh_dir/out" >&2
            rm -rf "$tmpdir" "$_self_work_root" "$_eh_dir"; exit 1
        fi
        rm -rf "$_eh_dir"
        echo "  fix-test check with an empty HOME and only a failing go on PATH: pass"
    else
        echo "  skipped (no resolved Go toolchain or no fix-test task)"
    fi

    st_case "== self-test: collapse-long merged config (keeps endpoints, two keys, private, deleted, no secret in logs) =="
    # The recording stub asserts what the merged config contains and its
    # modes, and records the merged path so the harness case can assert
    # deletion afterwards. It writes a valid result-json (solve) so the
    # run completes normally.
    stub_record="$tmpdir/wakil-record"
    cat > "$stub_record" <<'EOF'
#!/bin/sh
# Recording stub for the merged-config cases. Asserts the merged config
# keeps the fake endpoints block and sets exactly the two collapse keys,
# that the file is 0600 and the dir 0700, and records the merged path.
_fail=0
_cfg="$WAKIL_CONFIG"
[ -n "$_cfg" ] || { echo "RECORD: WAKIL_CONFIG unset" >> "$BENCH_ST_RECORD"; exit 97; }
echo "RECORD: merged=$_cfg mode=$(stat -c %a "$_cfg" 2>/dev/null || echo nope) dir_mode=$(stat -c %a "$(dirname "$_cfg")" 2>/dev/null || echo nope)" >> "$BENCH_ST_RECORD"
jq -e '.endpoints.fake and (.context_collapse_ceiling_tokens == 45000) and (.context_collapse_min_bytes == 2000)' "$_cfg" >/dev/null 2>&1 \
  || { echo "RECORD: merged content wrong" >> "$BENCH_ST_RECORD"; _fail=1; }
# Delegate to the solve behaviour (copy solution, write result-json).
workdir=$WAKIL_HOST_WORKDIR
result_json=""
shift
while [ $# -gt 0 ]; do
  case "$1" in
    --result-json) result_json=$2; shift 2 ;;
    --transcript) shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$BENCH_TASK_DIR" ] && [ -d "$BENCH_TASK_DIR/solution" ]; then
  cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
fi
_collapses=2
case "$workdir" in */collapse-long) _collapses=2 ;; esac
cat > "$result_json" <<JSON
{"wakil_version":"stub-record/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":100,"tokens_out":5,"tokens_cached":75,"cost_usd":0.001234,"tool_calls":2,"collapses":$_collapses,"collapses_turn_end":1,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
exit $_fail
EOF
    chmod +x "$stub_record"
    # Success path: merged config deleted, row pass, no sentinel in log/CSV/stdout.
    BENCH_ST_RECORD="$tmpdir/st-record" run_one collapse-long "self-merged" "$stub_record" >"$tmpdir/st-merged-stdout" 2>&1 || { echo "self-test FAILED: merged success run returned non-zero" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _m_file=$(sed -n 's/^RECORD: merged=\([^ ]*\).*/\1/p' "$tmpdir/st-record" 2>/dev/null | head -1)
    _m_mode=$(sed -n 's/.*mode=\([^ ]*\).*/\1/p' "$tmpdir/st-record" 2>/dev/null | head -1)
    _m_dmode=$(sed -n 's/.*dir_mode=\([^ ]*\).*/\1/p' "$tmpdir/st-record" 2>/dev/null | head -1)
    grep -q "merged content wrong" "$tmpdir/st-record" 2>/dev/null && { echo "self-test FAILED: merged content wrong (keeps endpoints + two keys)" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ "$_m_mode" = "600" ] || { echo "self-test FAILED: merged file mode=$_m_mode, want 600" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ "$_m_dmode" = "700" ] || { echo "self-test FAILED: merged dir mode=$_m_dmode, want 700" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ -n "$_m_file" ] && [ ! -e "$_m_file" ] || { echo "self-test FAILED: merged config still exists: $_m_file" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ -n "$_m_file" ] && [ ! -e "$(dirname "$_m_file")" ] || { echo "self-test FAILED: merged config dir still exists" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    grep -q "AKIA_FAKE_SENTINEL" "$tmpdir/st-merged-stdout" "$log_file" "$csv_file" 2>/dev/null && { echo "self-test FAILED: fake secret reached stdout/log/CSV" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _row_result=$(csv_cell "$csv_file" last 6); _row_valid=$(csv_cell "$csv_file" last 19)
    { [ "$_row_result" = "pass" ] && [ "$_row_valid" = "true" ]; } || { echo "self-test FAILED: merged success: expected pass/true, got $_row_result/$_row_valid" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # Timeout and failure paths: the merged config is deleted too.
    timeout_sec=1 BENCH_TASK_DIR="" run_one collapse-long "self-merged-timeout" "$stub_slow" >"$tmpdir/st-merged-timeout-stdout" 2>&1 || true
    grep -q "AKIA_FAKE_SENTINEL" "$tmpdir/st-merged-timeout-stdout" "$log_file" "$csv_file" 2>/dev/null && { echo "self-test FAILED: fake secret reached stdout/log/CSV on timeout" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    grep -rq "context_collapse_ceiling_tokens" "$tmpdir" 2>/dev/null && { echo "self-test FAILED: merged config left somewhere under tmpdir on timeout" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # Failure path (non-zero stub via stub_wrong is an error row): also deleted.
    BENCH_TASK_DIR="" run_one collapse-long "self-merged-wrong" "$stub_wrong" >"$tmpdir/st-merged-wrong-stdout" 2>&1 || true
    grep -q "AKIA_FAKE_SENTINEL" "$tmpdir/st-merged-wrong-stdout" "$log_file" "$csv_file" 2>/dev/null && { echo "self-test FAILED: fake secret reached stdout/log/CSV on error" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # Missing config: merge onto {} (env-only setup still runs).
    _missing_cfg="$tmpdir/definitely-missing.json"
    WAKIL_CONFIG="$_missing_cfg" BENCH_TASK_DIR="" run_one collapse-long "self-merged-missing" "$stub_noop" >"$tmpdir/st-merged-missing-stdout" 2>&1 || true
    _row_result=$(csv_cell "$csv_file" last 6); _row_valid=$(csv_cell "$csv_file" last 19)
    { [ "$_row_result" = "invalid" ] && [ "$_row_valid" = "false:no_collapse" ]; } || { echo "self-test FAILED: missing config: expected invalid/false:no_collapse, got $_row_result/$_row_valid" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # Unparseable config: row is error/config-unreadable with valid=false:config.
    _bad_cfg="$tmpdir/bad.json"
    printf '%s\n' 'not json at all {' > "$_bad_cfg"
    WAKIL_CONFIG="$_bad_cfg" BENCH_TASK_DIR="" run_one collapse-long "self-merged-bad" "$stub_noop" >"$tmpdir/st-merged-bad-stdout" 2>&1 || true
    _row_result=$(csv_cell "$csv_file" last 6); _row_reason=$(csv_cell "$csv_file" last 7); _row_valid=$(csv_cell "$csv_file" last 19)
    { [ "$_row_result" = "error" ] && [ "$_row_reason" = "config-unreadable" ] && [ "$_row_valid" = "false:config" ]; } || { echo "self-test FAILED: bad config: expected error/config-unreadable/false:config, got $_row_result/$_row_reason/$_row_valid" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # Non-collapse-long runs never set WAKIL_CONFIG (no merge branch).
    BENCH_TASK_DIR="" run_one fix-test "self-merged-noncl" "$stub_noop" >"$tmpdir/st-merged-noncl-stdout" 2>&1 || true
    grep -q "collapse-long override" "$log_file" 2>/dev/null && { echo "self-test FAILED: non-collapse-long run logged an override" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  merged config: endpoints kept + two keys, 0600/0700, deleted on success/failure/timeout, no sentinel in logs, missing→run, bad→false:config"

    st_case "== self-test: toolchain-pinned (every go call of a check or setup runs with the resolved GOTOOLCHAIN and GOPROXY=off) =="
    # A recording go in front of PATH, forwarding to the real one. It logs the
    # environment of each call; the real runs below are the ordinary solve and
    # prep paths, so the log shows what the checks and setup.sh really used.
    if [ -n "$go_toolchain" ]; then
        # With a resolved toolchain root the checks run that root's own bin/go
        # first on PATH, so the recorder stands in for the root: a fake root
        # whose bin/go logs and forwards to the real one. Without a root (a
        # development-build Go) the recorder sits in front of PATH as before.
        _pin_want_tc="$go_toolchain"
        _pin_saved_root="$go_root"
        if [ -n "$go_root" ]; then
            _real_go="$go_root/bin/go"
            mkdir -p "$tmpdir/gorec/root/bin"
            _pin_rec="$tmpdir/gorec/root/bin/go"
            go_root="$tmpdir/gorec/root"
            _pin_want_tc=local
        else
            _real_go=$(command -v go)
            mkdir -p "$tmpdir/gorec"
            _pin_rec="$tmpdir/gorec/go"
        fi
        cat > "$_pin_rec" <<REC
#!/bin/sh
echo "GOTOOLCHAIN=\$GOTOOLCHAIN GOPROXY=\$GOPROXY GOENV=\$GOENV GOFLAGS=\$GOFLAGS GOWORK=\$GOWORK GO111MODULE=\$GO111MODULE args=\$*" >> "\$BENCH_GO_LOG"
exec "$_real_go" "\$@"
REC
        chmod +x "$_pin_rec"
        _saved_path="$PATH"
        [ -n "$_pin_saved_root" ] || { PATH="$tmpdir/gorec:$PATH"; export PATH; }
        _pin_calls=0
        for task in $tasks; do
            [ -f "$tasks_dir/$task/repo/go.mod" ] || continue
            BENCH_GO_LOG="$tmpdir/gorec/$task.log"; export BENCH_GO_LOG
            : > "$BENCH_GO_LOG"
            task_idx=1
            if [ -x "$tasks_dir/$task/setup.sh" ]; then
                prep_workdir "$task" || { PATH="$_saved_path"; echo "self-test FAILED: toolchain-pinned: prep_workdir $task" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
            else
                BENCH_TASK_DIR="$tasks_dir/$task" run_one "$task" "self-pinned" "$stub_solve"
            fi
            _n=$(wc -l < "$BENCH_GO_LOG")
            _bad=$(grep -vc "^GOTOOLCHAIN=$_pin_want_tc GOPROXY=off GOENV=off GOFLAGS=-buildvcs=false GOWORK=off GO111MODULE=on " "$BENCH_GO_LOG" || true)
            if [ "$_n" -eq 0 ] || [ "$_bad" -ne 0 ]; then
                PATH="$_saved_path"; export PATH
                echo "self-test FAILED: toolchain-pinned: $task made $_n go call(s), $_bad without GOTOOLCHAIN=$_pin_want_tc GOPROXY=off:" >&2
                cat "$BENCH_GO_LOG" >&2
                rm -rf "$tmpdir" "$_self_work_root"; exit 1
            fi
            # Every "go test" of a check carries a -timeout below the outer bound.
            _gt_bad=$(grep ' args=test ' "$BENCH_GO_LOG" | awk -v max="$check_timeout" '{ if (match($0, /-timeout=[0-9]+s/)) { v = substr($0, RSTART + 9, RLENGTH - 10) + 0; if (v >= 1 && v < max) next } print }' | head -n 1)
            if [ -n "$_gt_bad" ]; then
                PATH="$_saved_path"; export PATH
                echo "self-test FAILED: toolchain-pinned: $task ran go test without a -timeout below BENCH_CHECK_TIMEOUT ($check_timeout): $_gt_bad" >&2
                rm -rf "$tmpdir" "$_self_work_root"; exit 1
            fi
            _pin_calls=$((_pin_calls + _n))
        done
        PATH="$_saved_path"; export PATH
        go_root="$_pin_saved_root"
        unset BENCH_GO_LOG
        echo "  $_pin_calls go call(s) of checks and setup, all with GOTOOLCHAIN=$_pin_want_tc GOPROXY=off${_pin_saved_root:+ through the resolved toolchain bin/go}"
    else
        echo "  skipped (no Go task selected)"
    fi

    st_case "== self-test: rc-mapping, verdict-required, preflight-real-go (the result class of a check) =="
    # check_result_for_rc: only 0 and an explicit 1 are verdicts.
    for _m in "0 pass" "1 fail" "2 error" "99 error" "124 error" "126 error" "127 error" "137 error"; do
        _m_rc=${_m% *}; _m_want=${_m#* }
        _m_got=$(check_result_for_rc "$_m_rc")
        [ "$_m_got" = "$_m_want" ] || { echo "self-test FAILED: rc-mapping: status $_m_rc -> $_m_got, want $_m_want" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    done
    # verdict-required: a check that ends with status 1 by accident (a failed
    # command under set -e) must come out as 99; gocheck_fail stays 1, and
    # BENCH_PREFLIGHT turns a fail into 99.
    _vr="$tmpdir/verdict"
    mkdir -p "$_vr/lib" "$_vr/t"
    cp "$bench_dir/lib/gocheck.sh" "$_vr/lib/gocheck.sh"
    for _v in accident:99 judged:1 judged-pre:99 clean:0; do
        _v_name=${_v%:*}; _v_want=${_v#*:}
        cat > "$_vr/t/check.sh" <<VEOF
#!/bin/sh
set -eu
. "$_vr/lib/gocheck.sh"
gocheck_init "$_vr/t"
case "$_v_name" in
    accident) cp /nonexistent/file /nonexistent/dest ;;
    judged|judged-pre) gocheck_fail "wrong answer" ;;
    clean) : ;;
esac
VEOF
        chmod +x "$_vr/t/check.sh"
        _v_pre=""; [ "$_v_name" != judged-pre ] || _v_pre=1
        _v_rc=0
        BENCH_PREFLIGHT="$_v_pre" "$_vr/t/check.sh" >"$_vr/out" 2>&1 || _v_rc=$?
        if [ "$_v_rc" -ne "$_v_want" ]; then
            echo "self-test FAILED: verdict-required: $_v_name ended with $_v_rc, want $_v_want" >&2
            cat "$_vr/out" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
    done
    # preflight-real-go: the preflight runs setup.sh and every Go check on the
    # reference solutions; with the real Go it must accept them (main ran it
    # before this self-test started; this runs it again, explicitly).
    if [ -n "$go_toolchain" ]; then
        preflight_go || { echo "self-test FAILED: preflight-real-go: the preflight refused the real Go" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    fi
    echo "  status 0/1 map to pass/fail, every other status to error; an unannounced status 1 becomes 99; the preflight accepts the real Go"

    st_case "== self-test: stub go (host Go too old): preflight-stops, preflight-stops-on-check, check-error-not-fail =="
    # A black-box driver runs a copy of this bench.sh with a stub go that
    # prints "go: go.mod requires go >= 1.26 (running go 1.25.1;
    # GOTOOLCHAIN=local)". It forwards "go env" to the real go in one case, so
    # it needs a Go (and the fix-test task).
    if [ -n "$go_toolchain" ] && [ -d "$tasks_dir/fix-test" ] && [ -f "$bench_dir/lib/toolchain_cases.sh" ]; then
        _tc_out="$tmpdir/toolchain-cases.out"
        if ! sh "$bench_dir/lib/toolchain_cases.sh" "$repo_root" >"$_tc_out" 2>&1; then
            echo "self-test FAILED: toolchain cases:" >&2
            cat "$_tc_out" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        grep -E '^  PASS' "$_tc_out"
    else
        echo "  skipped (no Go task selected or no fix-test task)"
    fi

    st_case "== self-test: binary-hang (add-flag: the agent's binary sleeps past the limit -> fail, check: binary timed out) =="
    # A fake agent that writes a cmd/grep which ignores TERM and sleeps. The
    # limit is 2s and the grace 1s, so the case stays seconds long; the check
    # must judge the agent (fail), not hang and not blame the environment.
    if [ -d "$tasks_dir/add-flag" ]; then
        stub_hang="$tmpdir/wakil-hang"
        cat > "$stub_hang" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-hang/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$workdir/cmd/grep/main.go" <<'GO'
package main

import (
	"os/signal"
	"syscall"
	"time"
)

func main() {
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(time.Hour)
}
GO
    cat > "$result_json" <<JSON
{"wakil_version":"stub-hang/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0}
JSON
    exit 0 ;;
esac
EOF
        chmod +x "$stub_hang"
        task_idx=1
        _hang_t0=$(date +%s)
        _saved_grace="$kill_grace"; kill_grace=1
        BENCH_BINARY_TIMEOUT=2 BENCH_TASK_DIR="" run_one add-flag "self-hang" "$stub_hang" >/dev/null
        kill_grace="$_saved_grace"
        _hang_secs=$(( $(date +%s) - _hang_t0 ))
        _row_result=$(csv_cell "$csv_file" last 6)
        if [ "$_row_result" != "fail" ] || ! grep -q '^check: binary timed out' "$results_dir"/add-flag-self-hang-*.log 2>/dev/null; then
            echo "self-test FAILED: binary-hang: result=$_row_result, want fail with 'check: binary timed out'" >&2
            tail -n 5 "$results_dir"/add-flag-self-hang-*.log >&2 2>/dev/null || true
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        if [ "$_hang_secs" -gt 40 ]; then
            echo "self-test FAILED: binary-hang took ${_hang_secs}s (the limit did not hold)" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        echo "  hanging binary -> fail, 'check: binary timed out' (${_hang_secs}s)"
    fi

    st_case "== self-test: binary-exit (add-flag: right output but exit status 1, or the binary's own 126, must be fail, not pass and not error) =="
    if [ -d "$tasks_dir/add-flag" ]; then
        stub_bexit="$tmpdir/wakil-bexit"
        cat > "$stub_bexit" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-bexit/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    sed -i "s/lineNo := 0/lineNo := 0\n\tdefer os.Exit($BENCH_EXIT)/" "$workdir/cmd/grep/main.go"
    cat > "$result_json" <<JSON
{"wakil_version":"stub-bexit/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0}
JSON
    exit 0 ;;
esac
EOF
        chmod +x "$stub_bexit"
        for _be in 1 126; do
            task_idx=1
            BENCH_EXIT=$_be BENCH_TASK_DIR="$tasks_dir/add-flag" run_one add-flag "self-bexit-$_be" "$stub_bexit" >/dev/null
            _row_result=$(csv_cell "$csv_file" last 6)
            if [ "$_row_result" != "fail" ]; then
                echo "self-test FAILED: binary-exit: output right, exit status $_be: got $_row_result, want fail" >&2
                tail -n 5 "$results_dir"/add-flag-self-bexit-$_be-*.log >&2 2>/dev/null || true
                rm -rf "$tmpdir" "$_self_work_root"; exit 1
            fi
        done
        echo "  exit status 1 and the binary's own 126 -> fail"
    fi

    st_case "== self-test: declined run that still solved the task (expect pass with exit_reason=declined) =="
    # Also asserts that bench.sh passes --allow-destructive to `wakil run`.
    stub_declined="$tmpdir/wakil-declined"
    cat > "$stub_declined" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-declined/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    saw_allow_destructive=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --allow-destructive) saw_allow_destructive=1; shift ;;
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    if [ "$saw_allow_destructive" -ne 1 ]; then
      echo "missing --allow-destructive flag" >&2
      exit 3
    fi
    if [ -n "$BENCH_TASK_DIR" ] && [ -d "$BENCH_TASK_DIR/solution" ]; then
      cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    fi
    cat > "$result_json" <<JSON
{"wakil_version":"stub-declined/0.0.0","model":"stub","exit_reason":"declined","workdir":"$workdir","seconds":0.0,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"tool_calls":2,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 1 ;;
esac
EOF
    chmod +x "$stub_declined"
    _dec_task="$(echo "$tasks" | awk '{print $1}')"
    task_idx=1
    BENCH_TASK_DIR="$tasks_dir/$_dec_task" run_one "$_dec_task" "self-declined-pass" "$stub_declined"
    _row_result=$(csv_cell "$csv_file" last 6)
    _row_reason=$(csv_cell "$csv_file" last 7)
    if [ "$_row_result" != "pass" ] || [ "$_row_reason" != "declined" ]; then
        echo "self-test FAILED: declined-solve: got result=$_row_result exit_reason=$_row_reason, want pass/declined" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    # Same declined stub WITHOUT copying solution → check.sh must run and record fail + exit_reason=declined.
    BENCH_TASK_DIR="" run_one "$_dec_task" "self-declined-fail" "$stub_declined"
    _row_result=$(csv_cell "$csv_file" last 6)
    _row_reason=$(csv_cell "$csv_file" last 7)
    if [ "$_row_result" != "fail" ] || [ "$_row_reason" != "declined" ]; then
        echo "self-test FAILED: declined-noop: got result=$_row_result exit_reason=$_row_reason, want fail/declined" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  declined+check.sh OK (--allow-destructive passed; result from check.sh, exit_reason preserved)"

    st_case "== self-test: wrong solutions fail on the three new tasks =="
    # collapse-long: solution with one wrong sum (collapses>0 so the run is
    # scorable) must fail the check.
    if [ -d "$tasks_dir/collapse-long" ]; then
        stub_wrongreport="$tmpdir/wakil-wrongreport"
        cat > "$stub_wrongreport" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-wrongreport/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    printf '{"alpha": 999, "beta": 131, "delta": 65, "epsilon": 161, "gamma": 113, "zeta": 132}\n' > "$workdir/report.json"
    printf '{"wakil_version":"stub-wrongreport/0.0.0","model":"stub","exit_reason":"pass","workdir":"%s","seconds":0.0,"tool_calls":1,"collapses":2,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}\n' "$workdir" > "$result_json"
    exit 0 ;;
esac
EOF
        chmod +x "$stub_wrongreport"
        task_idx=1
        BENCH_TASK_DIR="$tasks_dir/collapse-long" run_one collapse-long "self-wrongreport" "$stub_wrongreport"
        _row_result=$(csv_cell "$csv_file" last 6)
        [ "$_row_result" = "fail" ] || { echo "self-test FAILED: collapse-long wrong report: expected fail, got $_row_result" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    fi
    # refactor-multi: renamed but kept the old signature (callers still
    # pass 2 args against a 3-arg NewSum would not build; here the wrong
    # solution keeps OldAdd behaviour under NewSum with a wrong K in one
    # package) must fail the hidden tests.
    if [ -d "$tasks_dir/refactor-multi" ]; then
        stub_wrongk="$tmpdir/wakil-wrongk"
        cat > "$stub_wrongk" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-wrongk/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    sed -i 's/shared.NewSum(a, b, 3)/shared.NewSum(a, b, 99)/' "$workdir/p03/p03.go"
    printf '{"wakil_version":"stub-wrongk/0.0.0","model":"stub","exit_reason":"pass","workdir":"%s","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}\n' "$workdir" > "$result_json"
    exit 0 ;;
esac
EOF
        chmod +x "$stub_wrongk"
        task_idx=1
        BENCH_TASK_DIR="$tasks_dir/refactor-multi" run_one refactor-multi "self-wrongk" "$stub_wrongk"
        _row_result=$(csv_cell "$csv_file" last 6)
        [ "$_row_result" = "fail" ] || { echo "self-test FAILED: refactor-multi wrong K: expected fail, got $_row_result" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    fi
    # debug-subtle: correct fix plus an extra change outside the buggy
    # function (a 10-line helper added to the package) must fail minimal.
    if [ -d "$tasks_dir/debug-subtle" ]; then
        stub_bloated="$tmpdir/wakil-bloated"
        cat > "$stub_bloated" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-bloated/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    cat >> "$workdir/window/window.go" <<'GO'

// ExtraHelper is an unnecessary helper the agent added outside the fix.
func ExtraHelper(x int) int {
	y := x + 1
	y = y * 2
	y = y - 3
	y = y + 4
	y = y * 5
	y = y - 6
	y = y + 7
	return y
}
GO
    printf '{"wakil_version":"stub-bloated/0.0.0","model":"stub","exit_reason":"pass","workdir":"%s","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}\n' "$workdir" > "$result_json"
    exit 0 ;;
esac
EOF
        chmod +x "$stub_bloated"
        task_idx=1
        BENCH_TASK_DIR="$tasks_dir/debug-subtle" run_one debug-subtle "self-bloated" "$stub_bloated"
        _row_result=$(csv_cell "$csv_file" last 6)
        [ "$_row_result" = "fail" ] || { echo "self-test FAILED: debug-subtle bloated fix: expected fail (minimal-fix rule), got $_row_result" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    fi
    echo "  wrong report (collapse-long), wrong K (refactor-multi), bloated fix (debug-subtle) all scored fail"

    st_case "== self-test: agent-written test code must not fake a pass (expect fail) =="
    # A fake agent that leaves the bug in place and writes code that makes
    # "go test" succeed without the hidden test running: a TestMain that
    # exits 0, and a package-level init() that prints a PASS line and exits.
    # Each must be scored "fail" by the task's own check.sh.
    stub_cheat="$tmpdir/wakil-cheat"
    cat > "$stub_cheat" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-cheat/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    d="$workdir/$BENCH_CHEAT_DIR"
    # A task whose check has earlier, cheaper failure points (rename: the old
    # name must be gone) starts from the reference solution with the behaviour
    # broken, so only the hidden test stands between the cheat and a pass.
    if [ -n "$BENCH_CHEAT_SOLUTION" ]; then
      cp -R "$BENCH_CHEAT_SOLUTION/." "$workdir/"
      sed -i 's/a + b/a - b/' "$workdir"/shared/*.go
    fi
    case "$BENCH_CHEAT" in
      testmain)
        printf 'package %s\nimport ("os"; "testing")\nfunc TestMain(m *testing.M) { os.Exit(0) }\n' "$BENCH_CHEAT_PKG" > "$d/zz_test.go" ;;
      underscore)
        # A package in a "_x" directory is skipped by ./... but still imported.
        mod=$(awk '$1 == "module" { print $2; exit }' "$workdir/go.mod")
        mkdir -p "$d/_x"
        printf 'package x\nimport ("fmt"; "os")\nfunc init() { fmt.Println("--- PASS: %s (0.00s)"); fmt.Println("PASS"); os.Exit(0) }\n' "$BENCH_CHEAT_TEST" > "$d/_x/x.go"
        printf 'package %s\nimport _ "%s/%s/_x"\n' "$BENCH_CHEAT_PKG" "$mod" "$BENCH_CHEAT_DIR" > "$d/zz_x.go" ;;
      init)
        printf 'package %s\nimport ("fmt"; "os")\nfunc init() { fmt.Println("--- PASS: %s (0.00s)"); fmt.Println("PASS"); os.Exit(0) }\n' "$BENCH_CHEAT_PKG" "$BENCH_CHEAT_TEST" > "$d/zz_init.go" ;;
    esac
    cat > "$result_json" <<JSON
{"wakil_version":"stub-cheat/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_cheat"
    for _c in "fix-test wordcount wordcount TestCountCaseInsensitive" "rename shared shared TestBehaviour"; do
        # shellcheck disable=SC2086 # word splitting is the point: a fixed 4-field record
        set -- $_c
        _ct=$1; _cdir=$2; _cpkg=$3; _ctest=$4
        [ -d "$tasks_dir/$_ct" ] || continue
        for _how in testmain init underscore; do
            task_idx=1
            _csol=""
            [ "$_ct" != rename ] || _csol="$tasks_dir/rename/solution"
            BENCH_CHEAT="$_how" BENCH_CHEAT_DIR="$_cdir" BENCH_CHEAT_PKG="$_cpkg" BENCH_CHEAT_TEST="$_ctest" BENCH_CHEAT_SOLUTION="$_csol" \
                BENCH_TASK_DIR="" run_one "$_ct" "self-cheat-$_how" "$stub_cheat"
            _row_result=$(csv_cell "$csv_file" last 6)
            if [ "$_row_result" != "fail" ]; then
                echo "self-test FAILED: $_ct with a $_how cheat: expected fail, got $_row_result" >&2
                rm -rf "$tmpdir" "$_self_work_root"
                exit 1
            fi
            # It must have failed in the check's own logic (a "check: ..."
            # line), not because the cheat file was never written.
            if ! grep -q '^check: ' "$results_dir/$_ct-self-cheat-$_how-"*.log 2>/dev/null; then
                echo "self-test FAILED: $_ct with a $_how cheat failed without a check: reason" >&2
                rm -rf "$tmpdir" "$_self_work_root"
                exit 1
            fi
        done
    done
    echo "  TestMain-exit-0, init()-prints-PASS and underscore-package cheats scored fail, each with a check: reason"

    st_case "== self-test: no generator, seed or check in the work folder or its history =="
    # The real long-read folder, as built by prep_workdir: only docs/ and
    # go.mod, nothing generated-from in the tree or in git history.
    if [ -d "$tasks_dir/long-read" ]; then
        prep_workdir long-read || { echo "self-test FAILED: prep_workdir long-read" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
        _lr="$work_root/long-read"
        for _bad in cmd gen main.go setup.sh check.sh solution hidden; do
            if [ -e "$_lr/$_bad" ]; then
                echo "self-test FAILED: long-read work folder contains $_bad" >&2
                rm -rf "$tmpdir" "$_self_work_root"; exit 1
            fi
        done
        if git -C "$_lr" log --all --name-only --format= | grep -E 'cmd/gen|main\.go|check\.sh|setup\.sh|solution|hidden'; then
            echo "self-test FAILED: long-read git history names a generator or check file" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        if [ "$(find "$_lr/docs" -type f | wc -l)" -ne 60 ]; then
            echo "self-test FAILED: long-read docs/ does not hold 60 files" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
    fi
    # leak_check itself: each kind of leak must be refused (and a clean folder
    # accepted), tree and history.
    _lk="$tmpdir/leakcase"
    for _case in clean check hidden solution gen seed history; do
        rm -rf "$_lk"; mkdir -p "$_lk"
        ( cd "$_lk" && git init -q && git config user.name t && git config user.email t@localhost && echo x > ok.txt )
        case "$_case" in
            check) echo x > "$_lk/check.sh" ;;
            hidden) mkdir "$_lk/hidden" ;;
            solution) mkdir "$_lk/solution" ;;
            gen) mkdir -p "$_lk/cmd/gen" ;;
            seed) printf 'package main\nvar _ = rand.NewSource(42)\n' > "$_lk/g.go" ;;
            history) mkdir -p "$_lk/cmd/gen"; echo x > "$_lk/cmd/gen/main.go" ;;
        esac
        ( cd "$_lk" && git add -A && git commit -qm c )
        [ "$_case" = history ] && ( cd "$_lk" && git rm -rq cmd && git commit -qm drop )
        if leak_check "$_lk" leakcase 2>/dev/null; then _lrc=0; else _lrc=1; fi
        if [ "$_case" = clean ] && [ "$_lrc" -ne 0 ]; then
            echo "self-test FAILED: leak_check refused a clean folder" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        if [ "$_case" != clean ] && [ "$_lrc" -ne 1 ]; then
            echo "self-test FAILED: leak_check accepted a folder with a $_case leak" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
    done
    echo "  no generator/seed/check in tree or history; leak_check refuses all six leak kinds"

    st_case "== self-test: an agent that plants the solution dir after prep (expect error, check.sh not run) =="
    stub_plant="$tmpdir/wakil-plant"
    cat > "$stub_plant" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-plant/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    mkdir -p "$workdir/solution" "$workdir/.wakil"
    echo planted > "$workdir/.wakil/state"
    cat > "$result_json" <<JSON
{"wakil_version":"stub-plant/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_plant"
    task_idx=1
    _rc=0
    _plant_err="$tmpdir/plant.err"
    ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-plant" "$stub_plant" ) 2>"$_plant_err" || _rc=$?
    _row_result=$(csv_cell "$csv_file" last 6)
    _row_reason=$(csv_cell "$csv_file" last 7)
    if [ "$_rc" -ne 1 ] || [ "$_row_result" != "error" ] || [ "$_row_reason" != "leak-detected" ]; then
        echo "self-test FAILED: planted solution dir: rc=$_rc result=$_row_result reason=$_row_reason, want 1/error/leak-detected" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    grep -q 'changed files under .*/.wakil' "$_plant_err" || { echo "self-test FAILED: a change under .wakil/ was not reported" >&2; cat "$_plant_err" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  planted solution/ -> error leak-detected; .wakil/ change reported"

    st_case "== self-test: CSV cells with a comma, a quote and a newline (model, version) =="
    # csv_field itself.
    _nl='
'
    for _cse in 'plain|plain' 'a,b|"a,b"' 'say "hi"|"say ""hi"""' 'gpt,"x"|"gpt,""x"""'; do
        _in=${_cse%%|*}; _want=${_cse#*|}
        _got=$(csv_field "$_in")
        [ "$_got" = "$_want" ] || { echo "self-test FAILED: csv_field '$_in' = '$_got', want '$_want'" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    done
    _got=$(csv_field "line1${_nl}line2")
    [ "$_got" = "line1 line2" ] || { echo "self-test FAILED: csv_field with a newline = '$_got'" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # A whole row through run_one: the model name carries a comma, a quote and
    # a newline, the binary's version line a comma and a quote.
    _saved_model="$model_name"; _saved_resolved="$resolved_model"; _saved_version="$wakil_version"
    model_name="gpt,\"x\"${_nl}beta"; resolved_model="$model_name"; wakil_version='v1,"rc"'
    task_idx=1
    _csv_task="$(echo "$tasks" | awk '{print $1}')"
    _lines_before=$(wc -l < "$csv_file")
    BENCH_TASK_DIR="" run_one "$_csv_task" "self-csv" "$stub_noop"
    model_name="$_saved_model"; resolved_model="$_saved_resolved"; wakil_version="$_saved_version"
    _lines_after=$(wc -l < "$csv_file")
    if [ "$((_lines_after - _lines_before))" -ne 1 ]; then
        echo "self-test FAILED: one run added $((_lines_after - _lines_before)) lines to the CSV, want 1" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    _c_ver=$(csv_cell "$csv_file" last 2); _c_model=$(csv_cell "$csv_file" last 3)
    _c_task=$(csv_cell "$csv_file" last 4); _c_res=$(csv_cell "$csv_file" last 6)
    if [ "$_c_ver" != 'v1,"rc"' ] || [ "$_c_model" != 'gpt,"x" beta' ] || [ "$_c_task" != "$_csv_task" ] || [ "$_c_res" != "fail" ]; then
        echo "self-test FAILED: awkward CSV row read back as version='$_c_ver' model='$_c_model' task='$_c_task' result='$_c_res'" >&2
        tail -1 "$csv_file" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if [ "$(tail -1 "$csv_file" | awk -F, '{ print NF }')" -eq 16 ]; then
        echo "self-test FAILED: the awkward row still splits into 16 plain cells (nothing was quoted)" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    _tbl=$(print_table "$csv_file" "x")
    printf '%s\n' "$_tbl" | grep -q "$_csv_task" || { echo "self-test FAILED: print_table cannot read the quoted row" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  csv_field and a full row with comma/quote/newline read back intact (one line)"

    st_case "== self-test: old-format CSV still readable; valid/provider columns appended =="
    # A literal old CSV: the exact header and one row from a real historical
    # bench results file (14 columns, before tokens_cached and the later
    # appends). print_table, sum_cost and find_prev_csv must read it.
    _old_csv="$tmpdir/old-bench.csv"
    printf 'date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,cost_usd,tool_calls,collapses,mashura_rounds\n' > "$_old_csv"
    printf '2026-11-20,wakil v0.8.8-0.20261119230229-39b3c386d69d+dirty,unknown,steps,1,error,sandbox-not-acknowledged,0,n/a,n/a,n/a,n/a,n/a,n/a\n' >> "$_old_csv"
    printf '2026-11-20,wakil v0.8.8,unknown,steps,1,pass,pass,5,1000,200,0.010000,2,0,0\n' >> "$_old_csv"
    _old_tbl=$(print_table "$_old_csv" "unknown")
    printf '%s\n' "$_old_tbl" | grep -q 'steps' || { echo "self-test FAILED: print_table cannot read an old-format CSV" >&2; printf '%s\n' "$_old_tbl" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _old_cost=$(sum_cost "$_old_csv")
    [ "$_old_cost" = "0.0100" ] || { echo "self-test FAILED: sum_cost on old CSV = $_old_cost, want 0.0100" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _old_model=$(csv_cell "$_old_csv" 2 3)
    [ "$_old_model" = "unknown" ] || { echo "self-test FAILED: old CSV model read as '$_old_model'" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # New columns: header ends with valid,provider; a fresh row carries them.
    printf '%s\n' "$csv_header" | grep -q ',valid,provider$' || { echo "self-test FAILED: csv_header does not end with ,valid,provider: $csv_header" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _new_valid=$(csv_cell "$csv_file" last 19); _new_prov=$(csv_cell "$csv_file" last 20)
    [ "$_new_valid" = "true" ] || { echo "self-test FAILED: last row valid='$_new_valid', want true (pass row)" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ -z "$_new_prov" ] || { echo "self-test FAILED: last row provider='$_new_prov', want empty (stub reports none)" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  old 14-col CSV readable (table, cost 0.0100, model); new header ends ,valid,provider; pass row valid=true provider empty"

    st_case "== self-test: a second bench on a task that is already being run (expect exit 75, folder untouched) =="
    # The first bench holds the task's lock (lock_task, as run_one does) and
    # its folder holds a marker. A second run_one on the same task, in a
    # subshell with its own open file description, must refuse with a clear
    # message, return 75, write no CSV row and leave the marker alone. After
    # the first bench lets go, the same call works and refills the folder.
    _lock_task="$(echo "$tasks" | awk '{print $1}')"
    _lock_workdir="$work_root/$_lock_task"
    prep_workdir "$_lock_task"
    echo keep > "$_lock_workdir/.bench-lock-marker"
    lock_task "$_lock_task" || { echo "self-test FAILED: first lock_task" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _lock_err="$tmpdir/lock.err"
    _lines_before=$(wc -l < "$csv_file")
    _rc=0
    ( BENCH_TASK_DIR="" run_one "$_lock_task" "self-lock" "$stub_noop" ) 2>"$_lock_err" >/dev/null || _rc=$?
    _lines_after=$(wc -l < "$csv_file")
    if [ "$_rc" -ne 75 ]; then
        echo "self-test FAILED: second bench on a locked task returned $_rc, want 75" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    grep -q "task $_lock_task is already being run by another bench" "$_lock_err" || { echo "self-test FAILED: no clear message for a locked task" >&2; cat "$_lock_err" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ -f "$_lock_workdir/.bench-lock-marker" ] || { echo "self-test FAILED: the second bench touched the locked work folder" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    [ "$_lines_before" -eq "$_lines_after" ] || { echo "self-test FAILED: a refused bench wrote a CSV row" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    # A different task is not blocked by the lock of this one.
    for _other in $tasks; do
        [ "$_other" = "$_lock_task" ] && continue
        ( lock_task "$_other" ) || { echo "self-test FAILED: the lock of $_lock_task blocked task $_other" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
        break
    done
    unlock_task
    BENCH_TASK_DIR="" run_one "$_lock_task" "self-lock-free" "$stub_noop" >/dev/null
    [ ! -e "$_lock_workdir/.bench-lock-marker" ] || { echo "self-test FAILED: after the lock was released the folder was not refilled" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  held lock -> exit 75, message, folder and CSV untouched; released lock -> run proceeds"

    st_case "== self-test: --runs 2 on a terminal (heartbeat on): the series holds the lock, no started program does, nothing is left alive, a second bench is refused =="
    # The heartbeat only runs when stdout is a terminal, so the case switches it
    # on (is_tty) with an interval no other sleep in the harness uses, and runs
    # the real run_all with a stub that finishes long before one interval. Shims
    # in front of the harness's tools report when they were started holding the
    # lock file open (same device and inode, so a symlinked cache directory
    # cannot hide it). The stub, which runs inside every run, lists the
    # processes holding the lock file (only the bench shell and its forked
    # heartbeat may), proves a fresh flock is refused, and starts a second bench
    # on the same task, which must exit 75 without touching the folder.
    _ls_task="$(echo "$tasks" | awk '{print $1}')"
    _ls_dir="$tmpdir/lockseries"
    if [ ! -r "/proc/$$/comm" ] || [ ! -d "/proc/$$/fd" ]; then
        echo "  skipped (needs /proc: Linux only)"
    else
    mkdir -p "$_ls_dir/shim" "$_ls_dir/cache/wakil-bench"
    for _sh in git find sha256sum sleep timeout cp mv rm cat grep date sort mktemp head tail awk sed tr wc; do
        _sh_real=$(command -v "$_sh") || continue
        case "$_sh_real" in /*) ;; *) continue ;; esac
        cat > "$_ls_dir/shim/$_sh" <<SHIM
#!/bin/sh
for _f in /proc/\$\$/fd/*; do
    if [ "\$_f" -ef "\$BENCH_LOCKFILE" ]; then echo "$_sh" >> "\$BENCH_LEAKLOG"; fi
done
echo "$_sh \$*" >> "\$BENCH_CALLLOG"
exec "$_sh_real" "\$@"
SHIM
        chmod +x "$_ls_dir/shim/$_sh"
    done
    _ls_stub="$_ls_dir/wakil-lockstub"
    cat > "$_ls_stub" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-lock/0.0.0" ;;
  run)
    shift
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    for f in /proc/[0-9]*/fd/*; do
      if [ "$f" -ef "$BENCH_LOCKFILE" ]; then
        p=${f#/proc/}; p=${p%%/*}
        c=$(cat "/proc/$p/comm" 2>/dev/null)
        [ "$c" = "$BENCH_SHELL_COMM" ] || echo "holder $p $c" >> "$BENCH_HOLDLOG"
      fi
    done 2>/dev/null
    if flock -n "$BENCH_LOCKFILE" true; then echo free >> "$BENCH_FLOCKLOG"; else echo held >> "$BENCH_FLOCKLOG"; fi
    echo keep > "$WAKIL_HOST_WORKDIR/.bench-lock-marker"
    XDG_CACHE_HOME="$BENCH_CACHE" sh "$BENCH_SCRIPT" --no-preflight --wakil "$0" --runs 1 --task "$BENCH_TASK" >"$BENCH_2NDOUT" 2>&1
    echo "$?" >> "$BENCH_2NDRC"
    [ -f "$WAKIL_HOST_WORKDIR/.bench-lock-marker" ] && echo ok >> "$BENCH_MARKLOG"
    printf '{"model":"stub","exit_reason":"pass","workdir":"%s"}\n' "$WAKIL_HOST_WORKDIR" > "$result_json"
    exit 0 ;;
esac
EOF
    chmod +x "$_ls_stub"
    _ls_lock_root="$lock_root"; _ls_is_tty="$is_tty"; _ls_hb="$hb_interval"; _ls_runs="$runs"; _ls_tasks="$tasks"; _ls_path="$PATH"
    lock_root="$_ls_dir/cache/wakil-bench/locks"
    _ls_hb_test=37; is_tty=1; hb_interval=$_ls_hb_test; runs=2; tasks="$_ls_task"; task_idx=0
    BENCH_LOCKFILE="$lock_root/$_ls_task.lock"; BENCH_LEAKLOG="$_ls_dir/leak.log"; BENCH_HOLDLOG="$_ls_dir/hold.log"
    BENCH_FLOCKLOG="$_ls_dir/flock.log"; BENCH_2NDRC="$_ls_dir/2nd.rc"; BENCH_2NDOUT="$_ls_dir/2nd.out"
    BENCH_MARKLOG="$_ls_dir/mark.log"; BENCH_CALLLOG="$_ls_dir/calls.log"; BENCH_CACHE="$_ls_dir/cache"; BENCH_SCRIPT="$0"; BENCH_TASK="$_ls_task"
    BENCH_SHELL_COMM=$(cat "/proc/$$/comm"); BENCH_RUN_MARK="$_ls_dir"
    export BENCH_CALLLOG BENCH_LOCKFILE BENCH_LEAKLOG BENCH_HOLDLOG BENCH_FLOCKLOG BENCH_2NDRC BENCH_2NDOUT BENCH_MARKLOG BENCH_CACHE BENCH_SCRIPT BENCH_TASK BENCH_SHELL_COMM BENCH_RUN_MARK
    : > "$BENCH_CALLLOG"; : > "$BENCH_LEAKLOG"; : > "$BENCH_HOLDLOG"; : > "$BENCH_FLOCKLOG"; : > "$BENCH_2NDRC"; : > "$BENCH_MARKLOG"
    _ls_rows_before=$(wc -l < "$csv_file")
    _ls_t0=$(date +%s)
    PATH="$_ls_dir/shim:$PATH"; export PATH
    _ls_rc=0
    BENCH_TASK_DIR="" run_all "$_ls_stub" >"$_ls_dir/run.out" 2>"$_ls_dir/run.err" || _ls_rc=$?
    PATH="$_ls_path"; export PATH
    _ls_secs=$(( $(date +%s) - _ls_t0 ))
    _ls_rows=$(( $(wc -l < "$csv_file") - _ls_rows_before ))
    _ls_free_after=no
    flock -n "$BENCH_LOCKFILE" true && _ls_free_after=yes
    _ls_left=""
    for _p in /proc/[0-9]*; do
        [ -r "$_p/environ" ] || continue
        case "$(cat "$_p/comm" 2>/dev/null)" in sleep|timeout|wakil-lockstub) ;; *) continue ;; esac
        if tr '\0' '\n' < "$_p/environ" 2>/dev/null | grep -qx "BENCH_RUN_MARK=$BENCH_RUN_MARK"; then
            _ls_left="$_ls_left ${_p#/proc/}:$(cat "$_p/comm")"
        fi
    done
    _ls_hb_alive=no
    [ -z "${hb_pid:-}" ] || ! kill -0 "$hb_pid" 2>/dev/null || _ls_hb_alive=yes
    lock_root="$_ls_lock_root"; is_tty="$_ls_is_tty"; hb_interval="$_ls_hb"; runs="$_ls_runs"; tasks="$_ls_tasks"
    unset BENCH_CALLLOG BENCH_LOCKFILE BENCH_LEAKLOG BENCH_HOLDLOG BENCH_FLOCKLOG BENCH_2NDRC BENCH_2NDOUT BENCH_MARKLOG BENCH_CACHE BENCH_SCRIPT BENCH_TASK BENCH_SHELL_COMM BENCH_RUN_MARK
    _ls_why=""
    grep -qx "sleep $_ls_hb_test" "$_ls_dir/calls.log" || _ls_why="$_ls_why the heartbeat never ran (no sleep $_ls_hb_test was started: the case would prove nothing);"
    [ "$_ls_rc" -eq 0 ] || _ls_why="$_ls_why run_all returned $_ls_rc ($(head -n 1 "$_ls_dir/run.err"));"
    [ "$_ls_rows" -eq 2 ] || _ls_why="$_ls_why --runs 2 wrote $_ls_rows rows, want 2;"
    [ ! -s "$_ls_dir/leak.log" ] || _ls_why="$_ls_why programs started holding the lock: $(sort -u "$_ls_dir/leak.log" | tr '\n' ' ');"
    [ ! -s "$_ls_dir/hold.log" ] || _ls_why="$_ls_why processes holding the lock besides the bench shell: $(tr '\n' ';' < "$_ls_dir/hold.log");"
    [ "$(tr '\n' ' ' < "$_ls_dir/flock.log")" = "held held " ] || _ls_why="$_ls_why the lock was not held inside both runs ($(tr '\n' ' ' < "$_ls_dir/flock.log"));"
    [ "$(tr '\n' ' ' < "$_ls_dir/2nd.rc")" = "75 75 " ] || _ls_why="$_ls_why a second bench on the task did not exit 75 in both runs ($(tr '\n' ' ' < "$_ls_dir/2nd.rc"));"
    grep -q "task $_ls_task is already being run by another bench" "$_ls_dir/2nd.out" 2>/dev/null || _ls_why="$_ls_why the second bench printed no refusal;"
    [ "$(tr '\n' ' ' < "$_ls_dir/mark.log")" = "ok ok " ] || _ls_why="$_ls_why the second bench touched the work folder;"
    [ "$_ls_free_after" = yes ] || _ls_why="$_ls_why the lock is still held after run_all;"
    [ -z "$_ls_left" ] || _ls_why="$_ls_why processes of the run are still alive:$_ls_left;"
    [ "$_ls_hb_alive" = no ] || _ls_why="$_ls_why the heartbeat is still alive;"
    [ "$_ls_secs" -lt 25 ] || _ls_why="$_ls_why the series took ${_ls_secs}s (the heartbeat sleep of ${_ls_hb}s was waited for);"
    if [ -n "$_ls_why" ]; then
        echo "self-test FAILED: lock-series:$_ls_why" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    echo "  --runs 2 with the heartbeat on: 2 rows in ${_ls_secs}s; lock held in both runs, no started program holds it, a second bench exits 75 in both runs, nothing left alive, lock free afterwards"
    fi

    st_case "== self-test: stub that sleeps past the timeout (expect timeout, exit 124) =="
    # Use the harness's own timeout path with a short per-run timeout so
    # the whole self-test stays fast: the slow stub is killed at 2s.
    _saved_timeout="$timeout_sec"
    timeout_sec=2
    task_idx=1
    _slow_task="$(echo "$tasks" | awk '{print $1}')"
    BENCH_TASK_DIR="" run_one "$_slow_task" "self-slow" "$stub_slow"
    timeout_sec="$_saved_timeout"
    _row_result=$(csv_cell "$csv_file" last 6)
    _row_reason=$(csv_cell "$csv_file" last 7)
    if [ "$_row_result" != "timeout" ] || [ "$_row_reason" != "timeout" ]; then
        echo "self-test FAILED: slow stub: got result=$_row_result exit_reason=$_row_reason, want timeout/timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi

    st_case "== self-test: stub that ignores TERM (expect timeout via exit 137, check.sh not run) =="
    # A stub that traps TERM and keeps running: timeout sends TERM at the
    # deadline, waits the grace period, then KILLs it, and exits 137. The
    # stub has already written a result JSON saying "pass" with the right
    # workdir, and it leaves a fully correct solution in the folder, so a
    # harness that ran the guard or check.sh on a killed run would score it
    # "pass". Its transcript also carries a sandbox-approval message, which
    # must not turn the timeout into sandbox-not-acknowledged.
    stub_deaf="$tmpdir/wakil-deaf"
    cat > "$stub_deaf" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-deaf/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    if [ -n "$BENCH_TASK_DIR" ] && [ -d "$BENCH_TASK_DIR/solution" ]; then
      cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    fi
    cat > "$result_json" <<JSON
{"wakil_version":"stub-deaf/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":999,"tokens_out":999,"cost_usd":9.99,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    echo "executor error: sandbox risk is not acknowledged (label signing)" >&2
    trap '' TERM
    while :; do sleep 1; done ;;
esac
EOF
    chmod +x "$stub_deaf"
    _saved_timeout="$timeout_sec"; _saved_grace="$kill_grace"
    timeout_sec=2; kill_grace=1
    task_idx=1
    _rc=0
    BENCH_TASK_DIR="$tasks_dir/$_slow_task" run_one "$_slow_task" "self-deaf" "$stub_deaf" || _rc=$?
    timeout_sec="$_saved_timeout"; kill_grace="$_saved_grace"
    _row_result=$(csv_cell "$csv_file" last 6)
    _row_reason=$(csv_cell "$csv_file" last 7)
    _row_secs=$(csv_cell "$csv_file" last 8)
    if [ "$_rc" -ne 0 ] || [ "$_row_result" != "timeout" ] || [ "$_row_reason" != "timeout" ]; then
        echo "self-test FAILED: TERM-ignoring stub: rc=$_rc result=$_row_result exit_reason=$_row_reason, want 0/timeout/timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if [ "$_row_secs" -lt 3 ]; then
        echo "self-test FAILED: TERM-ignoring stub ended after ${_row_secs}s; the KILL path (timeout 2s + grace 1s) was not exercised" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    _row_tok=$(csv_cell "$csv_file" last 9); _row_cost=$(csv_cell "$csv_file" last 12)
    if [ "$_row_tok" != "n/a" ] || [ "$_row_cost" != "n/a" ]; then
        echo "self-test FAILED: a timeout row recorded metrics from the untrusted result JSON (tokens_in=$_row_tok cost=$_row_cost)" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if grep -q 'check.sh (' "$results_dir"/"$_slow_task"-self-deaf-*.log 2>/dev/null; then
        echo "self-test FAILED: check.sh ran after a timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    echo "  TERM-ignoring run -> timeout (exit 137 after ${_row_secs}s), no check.sh, no sandbox-not-acknowledged"

    st_case "== self-test: workdir guard per backend (docker not verifiable; hisn resolved path; direct mismatch) =="
    # One fake binary whose result JSON is set by the caller: the workdir it
    # reports and the sandbox backend it names (empty = an older binary with
    # no sandbox_backend field). Each case states the result, the exit_reason
    # and whether the bench stops (rc 1) or goes on to check.sh (rc 0).
    stub_fake="$tmpdir/wakil-fake"
    cat > "$stub_fake" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-fake/0.0.0" ;;
  run)
    shift
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    wd=$BENCH_FAKE_WORKDIR
    [ "$wd" != "@real" ] || wd=$(cd "$WAKIL_HOST_WORKDIR" && pwd -P)
    [ "$wd" != "@same" ] || wd=$WAKIL_HOST_WORKDIR
    backend=""
    [ -z "$BENCH_FAKE_BACKEND" ] || backend=",\"sandbox_backend\":\"$BENCH_FAKE_BACKEND\""
    printf '{"wakil_version":"stub-fake/0.0.0","model":"stub","exit_reason":"pass","workdir":"%s"%s,"seconds":0.0}\n' "$wd" "$backend" > "$result_json"
    exit 0 ;;
esac
EOF
    chmod +x "$stub_fake"
    _g_task="$(echo "$tasks" | awk '{print $1}')"
    ln -s "$_self_work_root" "$tmpdir/linkroot"
    _g_real_root="$work_root"
    # name|work_root|reported workdir|backend|want rc|want result|want exit_reason|want stderr text
    for _gc in \
        "docker|plain|/mnt/$_g_task|docker|1|error|workdir-not-verifiable|workdir not verifiable (docker)" \
        "docker-same-path|plain|@same|docker|1|error|workdir-not-verifiable|workdir not verifiable (docker)" \
        "hisn-same|plain|@same|hisn|0|fail|pass|" \
        "hisn-symlinked-root|link|@real|hisn|0|fail|pass|" \
        "direct-same|plain|@same|direct|0|fail|pass|" \
        "direct-mismatch|plain|/tmp/definitely-wrong|direct|1|error|wrong-workdir|WRONG WORKDIR" \
        "hisn-mismatch|plain|/tmp/definitely-wrong|hisn|1|error|wrong-workdir|WRONG WORKDIR" \
        "legacy-no-backend|plain|@same||0|fail|pass|" \
        "legacy-mismatch|plain|/tmp/definitely-wrong||1|error|wrong-workdir|WRONG WORKDIR"; do
        _g_name=$(echo "$_gc" | cut -d'|' -f1); _g_root=$(echo "$_gc" | cut -d'|' -f2)
        _g_wd=$(echo "$_gc" | cut -d'|' -f3); _g_be=$(echo "$_gc" | cut -d'|' -f4)
        _g_rc=$(echo "$_gc" | cut -d'|' -f5); _g_res=$(echo "$_gc" | cut -d'|' -f6)
        _g_why=$(echo "$_gc" | cut -d'|' -f7); _g_msg=$(echo "$_gc" | cut -d'|' -f8)
        case "$_g_root" in link) work_root="$tmpdir/linkroot" ;; *) work_root="$_g_real_root" ;; esac
        task_idx=1
        _g_err="$tmpdir/guard-$_g_name.err"
        _rc=0
        BENCH_FAKE_WORKDIR="$_g_wd" BENCH_FAKE_BACKEND="$_g_be" BENCH_TASK_DIR="" \
            run_one "$_g_task" "self-guard-$_g_name" "$stub_fake" 2>"$_g_err" >/dev/null || _rc=$?
        work_root="$_g_real_root"
        _g_got_res=$(csv_cell "$csv_file" last 6); _g_got_why=$(csv_cell "$csv_file" last 7)
        if [ "$_rc" -ne "$_g_rc" ] || [ "$_g_got_res" != "$_g_res" ] || [ "$_g_got_why" != "$_g_why" ]; then
            echo "self-test FAILED: workdir guard case $_g_name: rc=$_rc result=$_g_got_res exit_reason=$_g_got_why, want $_g_rc/$_g_res/$_g_why" >&2
            cat "$_g_err" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        if [ -n "$_g_msg" ] && ! grep -q -F "$_g_msg" "$_g_err"; then
            echo "self-test FAILED: workdir guard case $_g_name: stderr lacks '$_g_msg'" >&2
            cat "$_g_err" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
        # A guard stop must not have run check.sh.
        if [ "$_g_rc" -eq 1 ] && grep -q 'check.sh (' "$results_dir/$_g_task-self-guard-$_g_name-"*.log 2>/dev/null; then
            echo "self-test FAILED: workdir guard case $_g_name: check.sh ran after a guard stop" >&2
            rm -rf "$tmpdir" "$_self_work_root"; exit 1
        fi
    done
    echo "  docker -> error workdir-not-verifiable (check.sh skipped); hisn resolved/symlinked path accepted; direct and hisn mismatch -> wrong-workdir; no backend field -> plain comparison"

    st_case "== self-test: stub that reports a wrong workdir (expect error + stop) =="
    _saved_timeout="$timeout_sec"
    timeout_sec=5
    task_idx=1
    _rc=0
    ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-wrong" "$stub_wrong" ) || _rc=$?
    timeout_sec="$_saved_timeout"
    if [ "$_rc" -ne 1 ]; then
        echo "self-test FAILED: wrong-workdir guard did not stop the bench (exit=$_rc)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  wrong-workdir guard fired correctly (bench stopped with exit 1)"

    st_case "== self-test: stub that reports a missing workdir (expect error + stop) =="
    # Stub that writes a result-json without a workdir field — the guard
    # must treat a missing workdir as an error.
    stub_missing="$tmpdir/wakil-missing"
    cat > "$stub_missing" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-missing/0.0.0" ;;
  run)
    shift
    workdir=$WAKIL_HOST_WORKDIR
    result_json=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) result_json=$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$result_json" <<JSON
{"wakil_version":"stub-missing/0.0.0","model":"stub","exit_reason":"pass","seconds":0.0,"tokens_in":1,"tokens_out":1,"cost_usd":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0,"context_modules":"off","modules_loaded":""}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_missing"
    _saved_timeout="$timeout_sec"
    timeout_sec=5
    task_idx=1
    _rc=0
    ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-missing" "$stub_missing" ) || _rc=$?
    timeout_sec="$_saved_timeout"
    if [ "$_rc" -ne 1 ]; then
        echo "self-test FAILED: missing-workdir guard did not stop the bench (exit=$_rc)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  missing-workdir guard fired correctly (bench stopped with exit 1)"

    st_case "== self-test: sandbox approval error detection (both wordings, before workdir guard) =="
    for _msg_case in "not-ack:sandbox risk is not acknowledged (label signing)" "not-approved:.wakil/sandbox.json has not been approved (or changed since)"; do
        _case_name=${_msg_case%%:*}
        _case_msg=${_msg_case#*:}
        stub_unapproved="$tmpdir/wakil-$_case_name"
        cat > "$stub_unapproved" <<EOF
#!/bin/sh
case "\$1" in
  version) echo "stub-$_case_name/0.0.0" ;;
  run)
    shift
    workdir=\$WAKIL_HOST_WORKDIR
    result_json=""
    while [ \$# -gt 0 ]; do
      case "\$1" in
        --result-json) result_json=\$2; shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    echo "executor error: $_case_msg" >&2
    cat > "\$result_json" <<JSON
{"wakil_version":"stub-$_case_name/0.0.0","model":"stub","exit_reason":"error","seconds":0.0}
JSON
    exit 3 ;;
esac
EOF
        chmod +x "$stub_unapproved"
        _saved_timeout="$timeout_sec"
        timeout_sec=5
        task_idx=1
        _err_out="$tmpdir/unapproved-$_case_name.err"
        _rc=0
        ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-$_case_name" "$stub_unapproved" ) 2>"$_err_out" || _rc=$?
        timeout_sec="$_saved_timeout"
        if [ "$_rc" -ne 1 ]; then
            echo "self-test FAILED: $_case_name did not stop the bench (exit=$_rc)" >&2
            rm -rf "$tmpdir" "$_self_work_root"
            exit 1
        fi
        _row_reason=$(csv_cell "$csv_file" last 7)
        if [ "$_row_reason" != "sandbox-not-acknowledged" ]; then
            echo "self-test FAILED: $_case_name exit_reason=$_row_reason, want sandbox-not-acknowledged" >&2
            rm -rf "$tmpdir" "$_self_work_root"
            exit 1
        fi
        if ! grep -q "WAKIL_HOST_WORKDIR=.* wakil sandbox trust" "$_err_out"; then
            echo "self-test FAILED: $_case_name did not print WAKIL_HOST_WORKDIR=<path> wakil sandbox trust" >&2
            cat "$_err_out" >&2
            rm -rf "$tmpdir" "$_self_work_root"
            exit 1
        fi
        if grep -q "MISSING WORKDIR" "$_err_out"; then
            echo "self-test FAILED: $_case_name printed MISSING WORKDIR before sandbox approval stop" >&2
            cat "$_err_out" >&2
            rm -rf "$tmpdir" "$_self_work_root"
            exit 1
        fi
    done
    echo "  sandbox approval stop OK (both wordings caught before workdir guard)"

    st_case "== self-test: fixed-path freshness (expect marker gone in run 2) =="
    # Run 1 leaves a marker file; run 2 must not see it (the folder is
    # emptied and refilled before each run).
    _fresh_task="$(echo "$tasks" | awk '{print $1}')"
    _fresh_workdir="$work_root/$_fresh_task"
    prep_workdir "$_fresh_task"
    touch "$_fresh_workdir/.bench-marker-from-run-1"
    prep_workdir "$_fresh_task"
    if [ -e "$_fresh_workdir/.bench-marker-from-run-1" ]; then
        echo "self-test FAILED: marker file survived prep_workdir (folder not emptied)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  freshness OK (marker gone after refill)"

    st_case "== self-test: .wakil/ preservation (sandbox.json byte-identical after two refills) =="
    # A .wakil/sandbox.json with unusual formatting (indentation, trailing
    # newline) placed before a run must survive two refills byte-identical
    # (the approval is keyed on its content hash — sha256 of its raw
    # bytes). A marker file elsewhere must be gone after each refill. No
    # other file under .wakil/ should be created by the harness.
    _wakil_task="$(echo "$tasks" | awk '{print $1}')"
    _wakil_workdir="$work_root/$_wakil_task"
    prep_workdir "$_wakil_task"
    mkdir -p "$_wakil_workdir/.wakil"
    # Unusual formatting: 2-space indent + trailing newline.
    printf '  {"hisn": {\n    "risk_ack": "signing"\n  }}\n' > "$_wakil_workdir/.wakil/sandbox.json"
    _sandbox_hash_before=$(sha256sum "$_wakil_workdir/.wakil/sandbox.json" | awk '{print $1}')
    _wakil_file_count_before=$(find "$_wakil_workdir/.wakil" -type f | wc -l)
    # First refill.
    touch "$_wakil_workdir/.bench-marker-from-run-1"
    prep_workdir "$_wakil_task"
    _sandbox_hash_after=$(sha256sum "$_wakil_workdir/.wakil/sandbox.json" 2>/dev/null | awk '{print $1}')
    if [ "$_sandbox_hash_before" != "$_sandbox_hash_after" ]; then
        echo "self-test FAILED: .wakil/sandbox.json not preserved after refill 1 (hash $_sandbox_hash_before -> $_sandbox_hash_after)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    if [ -e "$_wakil_workdir/.bench-marker-from-run-1" ]; then
        echo "self-test FAILED: marker survived refill 1 (folder not emptied)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    _wakil_file_count_after=$(find "$_wakil_workdir/.wakil" -type f | wc -l)
    if [ "$_wakil_file_count_before" != "$_wakil_file_count_after" ]; then
        echo "self-test FAILED: file count under .wakil/ changed (before=$_wakil_file_count_before after=$_wakil_file_count_after)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    # Second refill.
    touch "$_wakil_workdir/.bench-marker-from-run-2"
    prep_workdir "$_wakil_task"
    _sandbox_hash_after2=$(sha256sum "$_wakil_workdir/.wakil/sandbox.json" 2>/dev/null | awk '{print $1}')
    if [ "$_sandbox_hash_before" != "$_sandbox_hash_after2" ]; then
        echo "self-test FAILED: .wakil/sandbox.json not preserved after refill 2 (hash $_sandbox_hash_before -> $_sandbox_hash_after2)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    if [ -e "$_wakil_workdir/.bench-marker-from-run-2" ]; then
        echo "self-test FAILED: marker survived refill 2 (folder not emptied)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    _wakil_file_count_after2=$(find "$_wakil_workdir/.wakil" -type f | wc -l)
    if [ "$_wakil_file_count_before" != "$_wakil_file_count_after2" ]; then
        echo "self-test FAILED: file count under .wakil/ changed after refill 2 (before=$_wakil_file_count_before after=$_wakil_file_count_after2)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  .wakil/ preservation OK (sandbox.json byte-identical after 2 refills, no extra files, markers gone)"

    st_case "== self-test: fixed-path stability (expect same path across runs) =="
    # Two real preparations: the folder is at the same canonical path, and the
    # second one refilled it (a marker the first run's agent could have left
    # is gone).
    _stab_task="$(echo "$tasks" | awk '{print $1}')"
    prep_workdir "$_stab_task" || { echo "self-test FAILED: prep_workdir $_stab_task" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _stab_path1=$(cd "$work_root/$_stab_task" && pwd -P)
    : > "$work_root/$_stab_task/agent-left-this"
    prep_workdir "$_stab_task" || { echo "self-test FAILED: second prep_workdir $_stab_task" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    _stab_path2=$(cd "$work_root/$_stab_task" && pwd -P)
    if [ "$_stab_path1" != "$_stab_path2" ] || [ -e "$work_root/$_stab_task/agent-left-this" ]; then
        echo "self-test FAILED: path or freshness changed between runs ($_stab_path1 -> $_stab_path2)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  path stability OK ($_stab_path1), second prep refilled the folder"

    st_case "== self-test: cached-% and 4-decimal cost formatting =="
    _tbl_out=$(print_table "$csv_file" "$resolved_model")
    if ! printf '%s\n' "$_tbl_out" | grep -q '75%'; then
        echo "self-test FAILED: print_table missing cached-% (75%) column" >&2
        printf '%s\n' "$_tbl_out" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    # shellcheck disable=SC2016 # the pattern is a literal "$0.0012" for grep, not a shell expansion
    if ! printf '%s\n' "$_tbl_out" | grep -q '\$0\.0012'; then
        echo "self-test FAILED: print_table cost not formatted with 4 decimals (\$0.0012)" >&2
        printf '%s\n' "$_tbl_out" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  cached-% (75%) and 4-decimal cost (\$0.0012) OK"

    st_case "== self-test: context_modules CSV columns present =="
    # The CSV header must include context_modules and modules_loaded.
    if ! printf '%s\n' "$csv_header" | grep -q 'context_modules'; then
        echo "self-test FAILED: CSV header missing context_modules" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if ! printf '%s\n' "$csv_header" | grep -q 'modules_loaded'; then
        echo "self-test FAILED: CSV header missing modules_loaded" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    # The stub result-json includes context_modules=off, modules_loaded=[].
    # The csv_row function reads them via json_field, so the CSV rows must
    # contain "off" in the context_modules column.
    if ! awk -F, '$1!=""' "$csv_file" | grep -q 'off'; then
        echo "self-test FAILED: CSV rows missing context_modules=off" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    echo "  context_modules and modules_loaded columns OK"

    st_case "== self-test: nothing written into the repo's bench/results =="
    if [ -d "$_saved_results_dir" ] && [ -n "$(find "$_saved_results_dir" -newer "$_st_marker" 2>/dev/null | head -n 1)" ]; then
        echo "self-test FAILED: the self-test wrote under $_saved_results_dir:" >&2
        find "$_saved_results_dir" -newer "$_st_marker" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    case "$csv_file" in "$cache_home"/wakil-bench/selftest/*) ;; *) echo "self-test FAILED: CSV not in the cache selftest dir: $csv_file" >&2; exit 1 ;; esac
    echo "  CSV and logs under $selftest_dir; bench/results untouched"

    st_case "== self-test: --no-preflight is the only way to skip the preflight, and says so =="
    # A normal run with the old variables set (and nothing else) must neither
    # skip the preflight nor warn about it; the flag prints exactly one warning
    # line that names the cache directory. Both runs stop at the unknown task,
    # before any model request, with a temp cache.
    _np_cache=$(mktemp -d "${TMPDIR:-/tmp}/bench-nopre.XXXXXX")
    _np_env=$(XDG_CACHE_HOME="$_np_cache" BENCH_TEST_SKIP_PREFLIGHT=1 BENCH_ALLOW_SKIP_PREFLIGHT=1 sh "$0" --wakil /nonexistent/binary --runs 1 --task nonexistent 2>&1 || true)
    if printf '%s\n' "$_np_env" | grep -q 'no-preflight\|SKIP_PREFLIGHT'; then
        echo "self-test FAILED: the old variables changed or warned in a normal run:" >&2
        printf '%s\n' "$_np_env" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_np_cache"; exit 1
    fi
    _np_flag=$(XDG_CACHE_HOME="$_np_cache" sh "$0" --no-preflight --wakil /nonexistent/binary --runs 1 --task nonexistent 2>&1 || true)
    if [ "$(printf '%s\n' "$_np_flag" | grep -c -- '--no-preflight: the Go preflight is skipped')" -ne 1 ] || ! printf '%s\n' "$_np_flag" | grep -q "$_np_cache/wakil-bench/nopreflight"; then
        echo "self-test FAILED: --no-preflight must print exactly one warning line naming the cache directory:" >&2
        printf '%s\n' "$_np_flag" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_np_cache"; exit 1
    fi
    rm -rf "$_np_cache"
    echo "  old variables ignored without a warning; --no-preflight warns once"

    st_case "== self-test: .wakil/ recovered after interrupted prep_workdir =="
    # Simulate an interrupted prep: .wakil/ is in the saved directory but
    # the work folder has no .wakil/. The next prep_workdir must recover it.
    _rec_task="$(echo "$tasks" | awk '{print $1}')"
    _rec_workdir="$work_root/$_rec_task"
    _rec_saved="$work_root/.wakil-saved-$_rec_task"
    prep_workdir "$_rec_task" >/dev/null 2>&1
    mkdir -p "$_rec_workdir/.wakil"
    printf '  {"hisn": {\n    "risk_ack": "signing"\n  }}\n' > "$_rec_workdir/.wakil/sandbox.json"
    _rec_hash_before=$(sha256sum "$_rec_workdir/.wakil/sandbox.json" | awk '{print $1}')
    # Simulate the interrupt: move .wakil to the saved location, then wipe
    # the folder (as if the kill happened after the save mv but before the
    # restore mv).
    mv "$_rec_workdir/.wakil" "$_rec_saved"
    rm -rf "$_rec_workdir"
    mkdir -p "$_rec_workdir"
    # Now run prep_workdir again: it must find the saved copy and restore it.
    prep_workdir "$_rec_task" >/dev/null 2>&1
    if [ ! -f "$_rec_workdir/.wakil/sandbox.json" ]; then
        echo "self-test FAILED: .wakil/sandbox.json not recovered after interrupted prep" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    _rec_hash_after=$(sha256sum "$_rec_workdir/.wakil/sandbox.json" | awk '{print $1}')
    if [ "$_rec_hash_before" != "$_rec_hash_after" ]; then
        echo "self-test FAILED: .wakil/sandbox.json hash changed after recovery ($_rec_hash_before -> $_rec_hash_after)" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    if [ -d "$_rec_saved" ]; then
        echo "self-test FAILED: saved .wakil dir not consumed by recovery (still at $_rec_saved)" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    echo "  .wakil/ recovery OK (sandbox.json byte-identical after interrupted prep)"

    st_case "== self-test: prep_workdir abort paths (saved copy and live .wakil/ both exist; the save fails) =="
    _ab_task="$(echo "$tasks" | awk '{print $1}')"
    _ab_workdir="$work_root/$_ab_task"
    _ab_saved="$work_root/.wakil-saved-$_ab_task"
    prep_workdir "$_ab_task" >/dev/null 2>&1
    mkdir -p "$_ab_workdir/.wakil" "$_ab_saved"
    printf 'live\n' > "$_ab_workdir/.wakil/marker"
    printf 'saved\n' > "$_ab_saved/marker"
    : > "$_ab_workdir/agent-file"
    _ab_err=$(prep_workdir "$_ab_task" 2>&1 >/dev/null) && _ab_rc=0 || _ab_rc=$?
    if [ "$_ab_rc" -eq 0 ] || ! printf '%s' "$_ab_err" | grep -q 'both saved' || [ "$(cat "$_ab_workdir/.wakil/marker")" != live ] || [ "$(cat "$_ab_saved/marker")" != saved ] || [ ! -e "$_ab_workdir/agent-file" ]; then
        echo "self-test FAILED: with a saved copy and a live .wakil/ prep_workdir must stop and touch nothing (rc=$_ab_rc): $_ab_err" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    rm -rf "$_ab_saved"
    # The save fails: the mv target's parent is a read-only directory. The
    # wipe must not run.
    _ab_ro=$(mktemp -d "${TMPDIR:-/tmp}/bench-ro.XXXXXX")
    _ab_work_save="$work_root"
    work_root="$_ab_ro/work"
    mkdir -p "$work_root/$_ab_task/.wakil"
    printf 'live\n' > "$work_root/$_ab_task/.wakil/marker"
    : > "$work_root/$_ab_task/agent-file"
    chmod 555 "$work_root"
    if [ "$(id -u)" -ne 0 ] && ! (: > "$work_root/probe") 2>/dev/null; then
        _ab_err=$(prep_workdir "$_ab_task" 2>&1 >/dev/null) && _ab_rc=0 || _ab_rc=$?
        if [ "$_ab_rc" -eq 0 ] || ! printf '%s' "$_ab_err" | grep -q 'aborting before wipe' || [ ! -e "$work_root/$_ab_task/agent-file" ] || [ "$(cat "$work_root/$_ab_task/.wakil/marker")" != live ]; then
            chmod 755 "$work_root"; work_root="$_ab_work_save"
            echo "self-test FAILED: a failed .wakil save must abort before the wipe (rc=$_ab_rc): $_ab_err" >&2
            rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_ab_ro"; exit 1
        fi
        echo "  both-exist and failed-save abort without touching the folder"
    else
        echo "  both-exist aborts; failed-save case skipped (running as root or the directory stayed writable)"
    fi
    chmod 755 "$work_root"; work_root="$_ab_work_save"
    rm -rf "$_ab_ro"

    st_case "== self-test: leak_check fails closed when grep cannot scan; python3 up front; resolve_go ignores GOFLAGS and an experiment suffix =="
    _lk_task="$(echo "$tasks" | awk '{print $1}')"
    prep_workdir "$_lk_task" >/dev/null 2>&1
    _lk_stub=$(mktemp -d "${TMPDIR:-/tmp}/bench-grepstub.XXXXXX")
    printf '#!/bin/sh\necho "grep: unrecognized option" >&2\nexit 2\n' > "$_lk_stub/grep"
    chmod +x "$_lk_stub/grep"
    _lk_err=$(PATH="$_lk_stub:$PATH" leak_check "$work_root/$_lk_task" "$_lk_task" 2>&1) && _lk_rc=0 || _lk_rc=$?
    if [ "$_lk_rc" -eq 0 ] || ! printf '%s' "$_lk_err" | grep -q 'cannot scan'; then
        echo "self-test FAILED: leak_check passed a folder that grep could not scan (rc=$_lk_rc): $_lk_err" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_lk_stub"; exit 1
    fi
    rm -rf "$_lk_stub"
    # python3 missing: a task whose check or setup uses it stops the run up front.
    _py_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-nopy.XXXXXX")
    tool_dir "$_py_dir" python3 || { echo "self-test FAILED: cannot build the tool directory for the python3 case" >&2; rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_py_dir"; exit 1; }
    _py_err=$(PATH="$_py_dir" "$_py_dir/sh" "$0" --wakil /nonexistent/binary --runs 1 --task long-read 2>&1 || true)
    if [ -e "$_py_dir/python3" ] || printf '%s' "$_py_err" | grep -q 'required tool' || ! printf '%s' "$_py_err" | grep -q 'python3 is needed by task long-read'; then
        echo "self-test FAILED: a missing python3 was not reported up front for long-read: $_py_err" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_py_dir"; exit 1
    fi
    rm -rf "$_py_dir"
    # resolve_go: a stub go that fails when GOFLAGS is set, and answers with an
    # experiment suffix.
    _rg_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-rggo.XXXXXX")
    # shellcheck disable=SC2016 # the single quotes are the point: this writes the stub go script, whose $GOFLAGS and $* must stay literal
    printf '#!/bin/sh\n[ -z "$GOFLAGS" ] || { echo "go: GOFLAGS leaked: $GOFLAGS" >&2; exit 1; }\ncase "$*" in\n  "env GOVERSION") echo "go1.26.11 X:aliastypeparams,jsonv2" ;;\n  "env GOROOT") echo "%s/root" ;;\n  "env GOMODCACHE") echo /tmp/m ;;\n  "env GOCACHE") echo /tmp/c ;;\n  "env GOPATH") echo /tmp/p ;;\n  *) exit 1 ;;\nesac\n' "$_rg_dir" > "$_rg_dir/go"
    chmod +x "$_rg_dir/go"
    mkdir -p "$_rg_dir/root/bin"
    cp "$_rg_dir/go" "$_rg_dir/root/bin/go"
    _rg_saved_tc="$go_toolchain"; _rg_saved_path="$PATH"
    _rg_saved_root="$go_root"; _rg_saved_mc="$go_modcache"; _rg_saved_gc="$go_cache"; _rg_saved_gp="$go_path"
    go_toolchain=""
    PATH="$_rg_dir:$PATH"; GOFLAGS=-mod=vendor; export GOFLAGS
    _rg_err=$(resolve_go 2>&1) && _rg_rc=0 || _rg_rc=$?
    resolve_go >/dev/null 2>&1 || true
    _rg_got="$go_toolchain"
    unset GOFLAGS; PATH="$_rg_saved_path"; go_toolchain="$_rg_saved_tc"
    go_root="$_rg_saved_root"; go_modcache="$_rg_saved_mc"; go_cache="$_rg_saved_gc"; go_path="$_rg_saved_gp"
    rm -rf "$_rg_dir"
    if [ "$_rg_rc" -ne 0 ] || [ "$_rg_got" != "go1.26.11" ]; then
        echo "self-test FAILED: resolve_go with GOFLAGS set and an experiment suffix: rc=$_rg_rc toolchain=$_rg_got: $_rg_err" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"; exit 1
    fi
    echo "  leak_check fail-closed, python3 up front, GOFLAGS cleared, experiment suffix OK"

    st_case "== self-test: tar in the required-tools list =="
    # A PATH that holds exactly the harness's tools minus tar (tool_dir) must
    # give the one "required tool(s) not found: tar" line and exit 1: nothing
    # else may be lost with tar (the old case lost dirname when both lived in one
    # directory), so the output is exactly that line and nothing more.
    _no_tar_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-no-tar.XXXXXX")
    tool_dir "$_no_tar_dir" tar || { echo "self-test FAILED: cannot build the tool directory for the tar case" >&2; rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_no_tar_dir"; exit 1; }
    _tar_rc=0
    _tar_err=$(PATH="$_no_tar_dir" "$_no_tar_dir/sh" "$0" --wakil /nonexistent/binary --runs 1 2>&1) || _tar_rc=$?
    if [ -e "$_no_tar_dir/tar" ] || [ "$_tar_rc" -ne 1 ] || [ "$_tar_err" != "$prog: required tool(s) not found: tar" ]; then
        echo "self-test FAILED: tar not reported as the one missing tool (rc=$_tar_rc, want the single line '$prog: required tool(s) not found: tar'):" >&2
        printf '%s\n' "$_tar_err" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_no_tar_dir"; exit 1
    fi
    echo "  tar required OK (the one line, exit 1, with every other tool on PATH)"
    rm -rf "$_no_tar_dir"

    st_case "== self-test: tools-first (a missing tool gives one line naming every missing tool, before anything else runs) =="
    # Each case's PATH is tool_dir minus the named tools: nothing else is lost.
    # The output must be exactly the one line (a shell error from a program used
    # before the check would be a second line), and the exit status 1. The
    # message is compared whole, never grepped for English text.
    _tf_n=0
    for _tf in "dirname" "dirname head tar" "jq"; do
        _tf_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-toolsfirst.XXXXXX")
        # shellcheck disable=SC2086 # word splitting is the point: a short list of tool names
        tool_dir "$_tf_dir" $_tf || { echo "self-test FAILED: tools-first: cannot build the tool directory" >&2; rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_tf_dir"; exit 1; }
        # The tools are reported in the harness's own order, not the case's.
        _tf_want="$prog: required tool(s) not found:"
        for _tf_t in $harness_tools; do
            case " $_tf " in *" $_tf_t "*) _tf_want="$_tf_want $_tf_t" ;; esac
        done
        _tf_rc=0
        _tf_out=$(PATH="$_tf_dir" "$_tf_dir/sh" "$0" --wakil /nonexistent/binary --runs 1 2>&1) || _tf_rc=$?
        _tf_help=$(PATH="$_tf_dir" "$_tf_dir/sh" "$0" -h 2>&1) || true
        if [ "$_tf_rc" -ne 1 ] || [ "$_tf_out" != "$_tf_want" ] || [ "$_tf_help" != "$_tf_want" ]; then
            echo "self-test FAILED: tools-first without [$_tf]: rc=$_tf_rc, want exactly the line '$_tf_want':" >&2
            printf '%s\n' "$_tf_out" >&2
            printf -- '-h gives: %s\n' "$_tf_help" >&2
            rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_tf_dir"; exit 1
        fi
        rm -rf "$_tf_dir"
        _tf_n=$((_tf_n + 1))
    done
    echo "  $_tf_n cases (dirname; dirname+head+tar; jq): one line naming every missing tool, exit 1, also for -h"

    st_case "== self-test: tools-list-sufficient (every task, run for real with only the listed tools on PATH) =="
    # The solve stub on every task, through a real bench.sh process whose whole
    # PATH is tool_dir (the harness's tools, sh, go, python3). A program the
    # list leaves out would show as "not found" in the output or as a row that
    # is not "pass". --no-preflight keeps the CSV and logs under the case's own
    # cache and HOME; the Go caches are the self-test's private ones, the toolchain
    # is the one resolved at start (tool_dir links its own go).
    _ts_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-toolsuff.XXXXXX")
    tool_dir "$_ts_dir/bin" || { echo "self-test FAILED: tools-list-sufficient: cannot build the tool directory" >&2; rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_ts_dir"; exit 1; }
    cp "$tmpdir/wakil-solve" "$_ts_dir/wakil"
    # A private, empty HOME: the host's own git configuration (signing needs gpg
    # or ssh-keygen, which are not tools of the harness) must not decide whether
    # the case passes. Only the Go caches are carried over.
    mkdir -p "$_ts_dir/home"
    _ts_bad=""
    _ts_n=0
    for _ts_t in $tasks; do
        _ts_out="$_ts_dir/$_ts_t.out"
        _ts_rc=0
        env -i PATH="$_ts_dir/bin" HOME="$_ts_dir/home" TMPDIR="${TMPDIR:-/tmp}" \
            XDG_CACHE_HOME="$_ts_dir/cache" BENCH_TASK_DIR="$tasks_dir/$_ts_t" \
            ${go_cache:+GOCACHE="$go_cache"} ${go_modcache:+GOMODCACHE="$go_modcache"} ${go_path:+GOPATH="$go_path"} \
            "$_ts_dir/bin/sh" "$0" --no-preflight --wakil "$_ts_dir/wakil" --task "$_ts_t" --runs 1 >"$_ts_out" 2>&1 || _ts_rc=$?
        _ts_row=$(sed -n 2p "$_ts_dir"/cache/wakil-bench/nopreflight/*-bench.csv 2>/dev/null | awk -F, '{ print $6 }')
        if [ "$_ts_rc" -ne 0 ] || grep -qi 'not found' "$_ts_out" || [ "$_ts_row" != pass ]; then
            _ts_bad="$_ts_bad $_ts_t(rc=$_ts_rc,row=$_ts_row: $(grep -i -m1 'not found' "$_ts_out" || head -n 1 "$_ts_out"))"
            # Keep what the failing task logged: the inner log and the inner
            # bench's own output, under the self-test's directory, so the exit
            # report can show them after this case removed its directory.
            mkdir -p "$selftest_dir/tools-list-sufficient"
            for _ts_lg in "$_ts_dir"/cache/wakil-bench/nopreflight/*.log; do
                [ -f "$_ts_lg" ] || continue
                cat "$_ts_out" "$_ts_lg" > "$selftest_dir/tools-list-sufficient/$_ts_t.log"
            done
            [ -f "$selftest_dir/tools-list-sufficient/$_ts_t.log" ] || cp "$_ts_out" "$selftest_dir/tools-list-sufficient/$_ts_t.log"
            st_fail_logs="$st_fail_logs $selftest_dir/tools-list-sufficient/$_ts_t.log"
        fi
        rm -rf "$_ts_dir/cache/wakil-bench/nopreflight"
        _ts_n=$((_ts_n + 1))
    done
    if [ -n "$_ts_bad" ]; then
        echo "self-test FAILED: tools-list-sufficient:$_ts_bad" >&2
        rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_ts_dir"; exit 1
    fi
    rm -rf "$_ts_dir"
    echo "  $_ts_n task(s) solved and checked with a PATH of exactly the listed tools"

    st_case "== self-test: failure-report (a failed case prints the check: reason and the last 15 lines of the task log) =="
    # A nested self-test on a copy of the tree whose fix-test repo already holds
    # the solution: the stub that changes nothing then passes where the case
    # expects a fail, so the nested self-test fails on purpose, on its first
    # case. The failure must come with the "check:" reason line and the log
    # tail of the task that failed, printed before its directories were removed.
    if [ -d "$tasks_dir/fix-test/solution" ]; then
        _fr_dir=$(mktemp -d "${TMPDIR:-/tmp}/bench-failrep.XXXXXX")
        mkdir -p "$_fr_dir/tree"
        cp -R "$repo_root/scripts" "$repo_root/bench" "$_fr_dir/tree/"
        cp "$repo_root/go.mod" "$_fr_dir/tree/go.mod"
        rm -rf "$_fr_dir/tree/bench/results"
        cp -R "$tasks_dir/fix-test/solution/." "$_fr_dir/tree/bench/tasks/fix-test/repo/"
        _fr_rc=0
        XDG_CACHE_HOME="$_fr_dir/cache" sh "$_fr_dir/tree/scripts/bench.sh" --self-test --task fix-test >"$_fr_dir/out" 2>"$_fr_dir/err" || _fr_rc=$?
        _fr_why=""
        [ "$_fr_rc" -eq 1 ] || _fr_why="$_fr_why the nested self-test ended with $_fr_rc, want 1;"
        grep -q 'self-test FAILED: noop fix-test: expected fail, got pass' "$_fr_dir/err" || _fr_why="$_fr_why no failure message;"
        grep -q '^---- log of the failing task: .*/fix-test-self-noop-' "$_fr_dir/err" || _fr_why="$_fr_why no log header for the failing task;"
        grep -q '^check: ok (TestCountCaseInsensitive)$' "$_fr_dir/err" || _fr_why="$_fr_why no check: reason line;"
        grep -q '^---- last 15 lines:$' "$_fr_dir/err" || _fr_why="$_fr_why no log tail header;"
        grep -q '^--- check.sh (fix-test) ---$' "$_fr_dir/err" || _fr_why="$_fr_why the log tail lacks the check.sh section;"
        if [ -n "$_fr_why" ]; then
            echo "self-test FAILED: failure-report:$_fr_why" >&2
            cat "$_fr_dir/err" >&2
            rm -rf "$tmpdir" "$_self_work_root" "$_st_marker" "$_fr_dir"; exit 1
        fi
        rm -rf "$_fr_dir"
        echo "  forced failure prints the reason line, the log header and the log tail"
    else
        echo "  skipped (no fix-test task)"
    fi

    echo
    echo "== self-test summary =="
    echo "CSV: $csv_file"
    print_table "$csv_file" "$resolved_model"
    echo "total cost: $(sum_cost "$csv_file")"
    echo
    echo "self-test done (harness worked)"

    rm -rf "$tmpdir" "$_self_work_root" "$_st_marker"
    # Restore the real-run work root, results dir and CSV so main() and
    # find_prev_csv see them.
    results_dir="$_saved_results_dir"
    work_root="$_saved_work_root"
    lock_root="$_saved_lock_root"
    run_root="$_saved_run_root"
    csv_file="$_saved_csv_file"
    if [ -n "${_saved_wakil_config:-}" ]; then
        export WAKIL_CONFIG="$_saved_wakil_config"
    else
        unset WAKIL_CONFIG
    fi
}

# check_task_tools: tools a task's check needs beyond the required ones above
# (python3 for long-read). Checked before the first run so the failure names
# the tool instead of surfacing as a check error after the model was paid.
check_task_tools() {
    for _ct_t in $tasks; do
        for _ct_f in "$tasks_dir/$_ct_t"/check.sh "$tasks_dir/$_ct_t"/setup.sh; do
            [ -f "$_ct_f" ] || continue
            if grep -q 'python3' "$_ct_f" && ! command -v python3 >/dev/null 2>&1; then
                echo "$prog: python3 is needed by task $_ct_t ($(basename "$_ct_f")) and was not found" >&2
                return 1
            fi
        done
    done
}

main() {
    check_task_tools || exit 1
    resolve_go || exit 1
    if [ "$self_test" -eq 1 ]; then
        trap st_on_exit EXIT
        # The self-test never writes into the user's Go caches: the toolchain
        # is only read (go_root), build and module caches are private to this
        # run. Before the preflight, which already runs real Go commands.
        if [ -n "$go_toolchain" ]; then
            st_go_private=$(mktemp -d "${TMPDIR:-/tmp}/bench-gocache.XXXXXX") || exit 1
            go_cache="$st_go_private/build"; go_modcache="$st_go_private/mod"; go_path="$st_go_private/path"
        fi
    fi
    preflight_go || exit 1

    if [ "$do_setup" -eq 1 ]; then
        do_setup
        return 0
    fi

    if [ "$self_test" -eq 1 ]; then
        self_test
        return 0
    fi

    case "$wakil_bin" in
        */*) [ -x "$wakil_bin" ] || { echo "$prog: wakil binary not executable: $wakil_bin" >&2; exit 1; } ;;
    esac

    # Create the real-run CSV only now (--setup and --self-test return
    # above, so they never create a real-run CSV that find_prev_csv
    # would pick up).
    mkdir -p "$results_dir" || { echo "$prog: cannot create $results_dir" >&2; exit 1; }
    csv_file="$results_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
    printf '%s\n' "$csv_header" > "$csv_file"

    run_all "$wakil_bin" || exit $?

    echo
    echo "== this run =="
    echo "CSV: $csv_file"
    print_table "$csv_file" "$resolved_model"
    echo "total cost: $(sum_cost "$csv_file")"

    prev=$(find_prev_csv "$resolved_model")
    if [ -n "$prev" ]; then
        echo
        echo "== most recent earlier run (model $resolved_model) =="
        echo "CSV: $prev"
        print_table "$prev" "$resolved_model"
        echo "total cost: $(sum_cost "$prev")"
    fi
}

main "$@"