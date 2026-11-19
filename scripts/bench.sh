#!/bin/sh
# bench.sh — benchmark harness for Wakil ("bench").
#
# Runs each task under bench/tasks/<name>/ through "wakil run" and records
# one CSV row per run in bench/results/. See bench/README.md.
#
# Usage:
#   scripts/bench.sh [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
#                    [--timeout SECONDS] [--mashura LEVEL] [--self-test]
#
# Defaults: wakil from PATH, the user's configured model, all tasks, 1 run,
# 600s per task, Mashura off.
#
# Exit 0 when the harness itself worked, whatever the task results;
# non-zero only on harness errors.
set -eu

prog="${0##*/}"

usage() {
    cat <<USAGE
usage: $prog [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
             [--timeout SECONDS] [--mashura LEVEL] [--self-test]

  --wakil PATH      wakil binary to run (default: from PATH)
  --model NAME      model for all runs (default: the user's configured model)
  --task NAME       only this task (default: all tasks in bench/tasks/)
  --runs N          runs per task (default: 1)
  --timeout SECONDS per-run timeout in seconds (default: 600)
  --mashura LEVEL   auto-counsel: off|low|medium|high (default: off)
  --self-test       run every task against stub wakil binaries
USAGE
}

wakil_bin="wakil"
model_name=""
only_task=""
runs=1
timeout_sec=600
mashura_level="off"
self_test=0

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
            timeout_sec="$2"; shift 2 ;;
        --mashura)
            [ $# -ge 2 ] || { echo "$prog: --mashura requires a level" >&2; exit 2; }
            case "$2" in off|low|medium|high) ;; *) echo "$prog: --mashura must be off|low|medium|high (got $2)" >&2; exit 2 ;; esac
            mashura_level="$2"; shift 2 ;;
        --self-test) self_test=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "$prog: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

# Repo root (the directory containing this script's parent).
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
bench_dir="$repo_root/bench"
tasks_dir="$bench_dir/tasks"
results_dir="$bench_dir/results"
mkdir -p "$results_dir"

csv_header='date,wakil_version,model,task,run,result,seconds,tokens_in,tokens_out,cost_usd,tool_calls,collapses,mashura_rounds'
csv_file="$results_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
printf '%s\n' "$csv_header" > "$csv_file"

# The resolved model for the CSV: from --model, else the result-json of the
# first run (resolved model, not requested), else "config".
resolved_model="$model_name"
[ -n "$resolved_model" ] || resolved_model="config"

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

# na <value>: the CSV cell for a value a binary did not report.
na() {
    case "$1" in
        '') echo 'n/a' ;;
        null) echo 'n/a' ;;
        *) echo "$1" ;;
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

# csv_row <task> <run> <wakil-rc> <check-rc> <seconds> <result-json>
# Appends one row to the CSV. <check-rc> is only read when <wakil-rc> is 0.
csv_row() {
    task=$1; run=$2; wak_rc=$3; chk_rc=$4; secs=$5; rj=$6
    if [ "$wak_rc" -eq 124 ]; then
        result="timeout"
    elif [ "$wak_rc" -ne 0 ]; then
        result="error"
    elif [ "$chk_rc" -eq 0 ]; then
        result="pass"
    else
        result="fail"
    fi
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$(date +%Y-%m-%d)" "$wakil_version" "$resolved_model" "$task" "$run" "$result" "$secs" \
        "$(na "$(json_field "$rj" "tokens_in")")" "$(na "$(json_field "$rj" "tokens_out")")" \
        "$(na "$(json_field "$rj" "cost_usd")")" "$(na "$(json_field "$rj" "tool_calls")")" \
        "$(na "$(json_field "$rj" "collapses")")" "$(na "$(json_field "$rj" "mashura_rounds")")" >> "$csv_file"
}

# run_one <task> <run> <wakil-bin>
# Copies the task repo to a fresh temp dir, sets it up, runs wakil under
# the timeout, then checks. The task's check and reference solution are
# never copied into the work folder. Always returns 0 (a failed task is
# a result, not a harness error); a crashed run still leaves a CSV row.
run_one() {
    task=$1; run=$2; bin=$3
    tdir="$tasks_dir/$task"
    workdir=$(mktemp -d "${TMPDIR:-/tmp}/bench-${task}-${run}.XXXXXX")

    # Fresh copy + git init + one commit.
    cp -R "$tdir/repo/." "$workdir/"
    ( cd "$workdir" && git init -q && git add -A && git -c user.email=bench@example.invalid -c user.name=bench commit -qm "bench task $task" ) 2>/dev/null || true

    # Optional setup hook (task-level setup.sh, runs in the work copy
    # before the agent). The hook's own location is the task dir; it
    # should address the work copy as the working directory.
    if [ -x "$tdir/setup.sh" ]; then
        ( cd "$workdir" && "$tdir/setup.sh" ) >/dev/null 2>&1 || true
    fi

    task_text=$(cat "$tdir/task.txt")
    result_json="$workdir/.bench-result.json"
    log_file="$results_dir/${task}-${run}-$(date +%H%M%S).log"

    start_epoch=$(date +%s)
    set +e
    if [ -n "$model_name" ]; then
        WAKIL_MODEL="$model_name" timeout -k 10 "$timeout_sec" "$bin" run --auto $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1
    else
        timeout -k 10 "$timeout_sec" "$bin" run --auto $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1
    fi
    wak_rc=$?
    set -e
    end_epoch=$(date +%s)
    secs=$((end_epoch - start_epoch))
    cat "$log_file.out" >> "$log_file" 2>/dev/null || true
    rm -f "$log_file.out"

    # Resolve the model from the result file when we did not set one and a
    # real binary answered (honest identity: resolved, not requested).
    if [ -z "$model_name" ] && [ -f "$result_json" ]; then
        rm_model=$(json_field "$result_json" "model")
        [ -n "$rm_model" ] && resolved_model="$rm_model"
    fi

    # check.sh only when the run itself did not error out. It lives in
    # the task dir (never in the work folder) and runs in the work folder.
    chk_rc=1
    if [ "$wak_rc" -eq 0 ]; then
        set +e
        ( cd "$workdir" && "$tdir/check.sh" ) >/dev/null 2>&1
        chk_rc=$?
        set -e
    fi

    csv_row "$task" "$run" "$wak_rc" "$chk_rc" "$secs" "$result_json"

    # The agent under test must never see the check, its hidden files, or
    # the reference solution: nothing but repo/ was copied in. A stub that
    # creates them is a harness violation, surfaced in the self-test.
    if [ -e "$workdir/check.sh" ] || [ -e "$workdir/hidden" ] || [ -d "$workdir/solution" ]; then
        echo "$prog: LEAK in $task run $run — check/hidden/solution was present in the work folder" >&2
    fi

    rm -rf "$workdir"
}

# find_prev_csv: the most recent bench CSV (not this run's). Echoes path.
find_prev_csv() {
    last=""
    for f in "$results_dir"/20*-bench.csv; do
        [ -f "$f" ] || continue
        [ "$f" = "$csv_file" ] && continue
        last="$f"
    done
    printf '%s' "$last"
}

# print_table <csv>: per-task summary of a CSV.
print_table() {
    f=$1
    printf '  %-12s %-8s %-8s %s\n' "task" "result" "seconds" "tokens in/out  tool_calls collapses mashura"
    awk -F, 'NR>1 { printf "  %-12s %-8s %-8s %s/%s  %s %s %s\n", $4, $6, $7, $8, $9, $11, $12, $13 }' "$f"
}

# sum_cost <csv>: total cost_usd (n/a and empty cells contribute 0).
sum_cost() {
    awk -F, 'NR>1 && $10 != "n/a" && $10 != "" { s += $10 } END { printf "%.6f", s }' "$1"
}

# run_all <wakil-bin>: run every task `runs` times.
run_all() {
    for task in $tasks; do
        i=1
        while [ "$i" -le "$runs" ]; do
            run_one "$task" "$i" "$1"
            i=$((i + 1))
        done
    done
}

# ---- self-test -------------------------------------------------------------
self_test() {
    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/bench-selftest.XXXXXX")

    # Stub that changes nothing (and writes no result-json, like a binary
    # built before --result-json existed: tokens stay n/a).
    stub_noop="$tmpdir/wakil-noop"
    cat > "$stub_noop" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-noop/0.0.0" ;;
  run) exit 0 ;;
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
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    if [ -n "$BENCH_TASK_DIR" ] && [ -d "$BENCH_TASK_DIR/solution" ]; then
      cp -R "$BENCH_TASK_DIR/solution/." "$workdir/"
    fi
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-solve/0.0.0","model":"stub","exit_reason":"pass","seconds":0.0,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"tool_calls":2,"collapses":0,"mashura_rounds":0}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_solve"

    # Stub that sleeps past any timeout.
    stub_slow="$tmpdir/wakil-slow"
    cat > "$stub_slow" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-slow/0.0.0" ;;
  run) sleep 3600 ;;
esac
EOF
    chmod +x "$stub_slow"

    echo "== self-test: stub that changes nothing (expect fail on every task) =="
    for task in $tasks; do
        BENCH_TASK_DIR="" run_one "$task" "self-noop" "$stub_noop"
    done
    echo "== self-test: stub that applies the solution (expect pass on every task) =="
    for task in $tasks; do
        BENCH_TASK_DIR="$tasks_dir/$task" run_one "$task" "self-solve" "$stub_solve"
    done
    echo "== self-test: stub that sleeps past the timeout (expect timeout) =="
    # Use the harness's own timeout path with a short per-run timeout so
    # the whole self-test stays fast: the slow stub is killed at 2s.
    _saved_timeout="$timeout_sec"
    timeout_sec=2
    BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-slow" "$stub_slow"
    timeout_sec="$_saved_timeout"

    echo
    echo "== self-test summary =="
    echo "CSV: $csv_file"
    print_table "$csv_file"
    echo "total cost: $(sum_cost "$csv_file")"
    echo
    echo "self-test done (harness worked)"

    rm -rf "$tmpdir"
}

main() {
    if [ "$self_test" -eq 1 ]; then
        self_test
        return 0
    fi

    case "$wakil_bin" in
        */*) [ -x "$wakil_bin" ] || { echo "$prog: wakil binary not executable: $wakil_bin" >&2; exit 1; } ;;
    esac

    run_all "$wakil_bin"

    echo
    echo "== this run =="
    echo "CSV: $csv_file"
    print_table "$csv_file"
    echo "total cost: $(sum_cost "$csv_file")"

    prev=$(find_prev_csv)
    if [ -n "$prev" ]; then
        echo
        echo "== most recent earlier run (model $resolved_model) =="
        echo "CSV: $prev"
        print_table "$prev"
        echo "total cost: $(sum_cost "$prev")"
    fi
}

main "$@"
