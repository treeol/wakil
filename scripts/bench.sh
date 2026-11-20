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
             [--timeout SECONDS] [--mashura LEVEL] [--self-test]
  or:  $prog --setup

  --wakil PATH      wakil binary to run (default: from PATH)
  --model NAME      model for all runs (default: the user's configured model)
  --task NAME       only this task (default: all tasks in bench/tasks/)
  --runs N          runs per task (default: 1)
  --timeout SECONDS per-run timeout in seconds (default: 600)
  --mashura LEVEL   auto-counsel: off|low|medium|high (default: off)
  --self-test       run every task against stub wakil binaries
  --setup           create all task work folders and print the one
                    'wakil sandbox trust <path>' command per folder
USAGE
}

wakil_bin="wakil"
model_name=""
only_task=""
runs=1
timeout_sec=600
mashura_level="off"
self_test=0
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
        --self-test) self_test=1; shift ;;
        --setup) do_setup=1; shift ;;
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

# Fixed work folders: stable paths so the user's one-time sandbox
# acknowledgement holds across runs. Each task runs in
# ${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>.
cache_home="${XDG_CACHE_HOME:-$HOME/.cache}"
work_root="$cache_home/wakil-bench/work"

# Metric columns come from jq parsing of the per-run result JSON; without
# jq they are honestly n/a, but say so once rather than per row.
if ! command -v jq >/dev/null 2>&1; then
    echo "$prog: warning: jq not found — metric columns (tokens, cost, counts) will be n/a" >&2
fi

csv_header='date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,cost_usd,tool_calls,collapses,mashura_rounds'
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
    # cannot make do_setup or prep_workdir run rm -rf on an arbitrary path.
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
set -- $tasks
total_tasks=$#
task_idx=0

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

# csv_row <task> <run> <result> <exit_reason> <seconds> <result-json>
# Appends one row to the CSV.
csv_row() {
    task=$1; run=$2; result=$3; exit_reason=$4; secs=$5; rj=$6
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$(date +%Y-%m-%d)" "$wakil_version" "$resolved_model" "$task" "$run" "$result" \
        "$(na "$exit_reason")" "$secs" \
        "$(na "$(json_field "$rj" "tokens_in")")" "$(na "$(json_field "$rj" "tokens_out")")" \
        "$(na "$(json_field "$rj" "cost_usd")")" "$(na "$(json_field "$rj" "tool_calls")")" \
        "$(na "$(json_field "$rj" "collapses")")" "$(na "$(json_field "$rj" "mashura_rounds")")" >> "$csv_file"
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

    # Move .wakil/ aside so it survives the folder cleanup untouched.
    _wakil_saved=""
    if [ -d "$workdir/.wakil" ]; then
        _wakil_saved="${TMPDIR:-/tmp}/bench-wakil-saved.$$"
        mv "$workdir/.wakil" "$_wakil_saved"
    fi

    # Delete everything in the folder (it is now empty of .wakil/).
    if [ -d "$workdir" ]; then
        rm -rf "$workdir"
    fi
    mkdir -p "$workdir"

    # Move .wakil/ back so the directory inode and content are untouched.
    if [ -n "$_wakil_saved" ]; then
        mv "$_wakil_saved" "$workdir/.wakil"
    fi

    # Fresh copy + git init + one commit with a repo-local identity.
    cp -R "$tdir/repo/." "$workdir/"
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

    # Optional setup hook (task-level setup.sh, runs in the work copy
    # before the agent). The hook's own location is the task dir; it
    # should address the work copy as the working directory.
    if [ -x "$tdir/setup.sh" ]; then
        ( cd "$workdir" && "$tdir/setup.sh" ) >/dev/null 2>&1 || true
    fi
}

# run_one <task> <run> <wakil-bin>
# Sets up the fixed work folder, runs wakil under the timeout, then
# checks. The task's check and reference solution are never copied into
# the work folder. Always returns 0 (a failed task is a result, not a
# harness error); a crashed run still leaves a CSV row.
run_one() {
    task=$1; run=$2; bin=$3
    tdir="$tasks_dir/$task"
    workdir="$work_root/$task"

    prep_workdir "$task" || return 1

    task_text=$(cat "$tdir/task.txt")
    result_json="$workdir/.bench-result.json"
    log_file="$results_dir/${task}-${run}-$(date +%H%M%S).log"

    # Progress: one line before each run so a long session is not silent.
    printf '[%d/%d] %s (run %s/%d) … running, log: %s\n' \
        "$task_idx" "$total_tasks" "$task" "$run" "$runs" "$log_file"

    start_epoch=$(date +%s)
    set +e
    # WAKIL_HOST_WORKDIR points the sandbox at the fixed work folder so
    # the agent runs there, not in the configured work_dir.
    export WAKIL_HOST_WORKDIR="$workdir"

    if [ -n "$model_name" ]; then
        WAKIL_MODEL="$model_name" timeout -k 10 "$timeout_sec" "$bin" run --auto --no-memory $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1 &
    else
        timeout -k 10 "$timeout_sec" "$bin" run --auto --no-memory $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1 &
    fi
    run_pid=$!

    # Heartbeat: while the run is in progress, print one short line with the
    # elapsed time every 30s (only when stdout is a terminal), so a long
    # task does not look hung. Nothing extra when output is piped or
    # redirected. Skipped when the timeout is too short for a heartbeat.
    # The heartbeat runs in the background so it does not inflate the
    # recorded duration; wait in the foreground captures the real exit
    # code and the real elapsed time.
    if [ "$is_tty" -eq 1 ] && [ "$timeout_sec" -gt 30 ]; then
        (
            while kill -0 "$run_pid" 2>/dev/null; do
                sleep 30
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

    # Result and exit_reason for the CSV row and the summary line.
    if [ "$wak_rc" -eq 124 ]; then
        result="timeout"
    elif [ "$wak_rc" -ne 0 ]; then
        result="error"
    elif [ "$chk_rc" -eq 0 ]; then
        result="pass"
    else
        result="fail"
    fi
    exit_reason=$(json_field "$result_json" "exit_reason")

    # Post-run "not acknowledged" detection: when the run failed and the
    # log mentions the sandbox risk error, the exit reason is
    # sandbox-not-acknowledged and the bench stops with the label named.
    # The gate also fires when exit_reason is "error" (the binary writes
    # "error" on bootstrap failure, which overwrites the empty-string
    # gate and suppresses the scan).
    if [ "$result" != "pass" ]; then
        if [ -z "$exit_reason" ] || [ "$exit_reason" = "error" ]; then
            if grep -q "sandbox risk is not acknowledged" "$log_file" 2>/dev/null; then
                result="error"
                exit_reason="sandbox-not-acknowledged"
            fi
        fi
    fi

    csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"

    # A sandbox-not-acknowledged result is a harness-level error, not a
    # model failure: stop the whole bench and name the label and the
    # one command to run. The label was detected by the post-run log
    # scan above. This check runs before the workdir guard so the
    # "not acknowledged" message prints first.
    if [ "$exit_reason" = "sandbox-not-acknowledged" ]; then
        echo "$prog: bench stopped — task $task run $run: sandbox risk is not acknowledged in $workdir" >&2
        echo "  run 'wakil sandbox trust $workdir' to review and acknowledge it" >&2
        return 1
    fi

    # Workdir guard: verify the agent ran in the fixed folder we set
    # up, not in the user's configured work_dir. The result JSON carries
    # the resolved workdir from the binary; a missing value or mismatch
    # means the sandbox mounted the wrong directory. A missing value is
    # an error (the binary must report its workdir).
    _actual_workdir=$(json_field "$result_json" "workdir")
    if [ -z "$_actual_workdir" ]; then
        echo "$prog: MISSING WORKDIR in $task run $run: binary did not report workdir" >&2
        result="error"
        exit_reason="missing-workdir"
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
        cat "$log_file" 2>/dev/null || true
        return 1
    fi
    if [ "$_actual_workdir" != "$workdir" ]; then
        echo "$prog: WRONG WORKDIR in $task run $run: binary reports workdir=$_actual_workdir, expected $workdir — agent ran in the wrong folder" >&2
        result="error"
        exit_reason="wrong-workdir"
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
        cat "$log_file" 2>/dev/null || true
        return 1
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
        _summary="$_summary · \$$_cost"
    fi
    if [ "$result" != "pass" ] && [ -n "$exit_reason" ]; then
        _summary="$_summary · exit $exit_reason"
    fi
    printf '%s\n' "$_summary"

    # The agent under test must never see the check, its hidden files, or
    # the reference solution: nothing but repo/ was copied in. A stub that
    # creates them is a harness violation, surfaced in the self-test.
    if [ -e "$workdir/check.sh" ] || [ -e "$workdir/hidden" ] || [ -d "$workdir/solution" ]; then
        echo "$prog: LEAK in $task run $run — check/hidden/solution was present in the work folder" >&2
    fi
}

# find_prev_csv: the most recent bench CSV whose model matches the
# resolved model (not this run's, not a self-test). Echoes path.
# Self-test CSVs live in $results_dir/selftest/ and are excluded by
# the top-level glob (results_dir/20*-bench.csv matches direct children
# only, not the selftest/ subdir).
find_prev_csv() {
    _want_model="${1:-}"
    last=""
    for f in "$results_dir"/20*-bench.csv; do
        [ -f "$f" ] || continue
        [ "$f" = "$csv_file" ] && continue
        # Filter by model: the CSV's model is in column 3 ($3).
        _f_model=$(awk -F, 'NR==2 { print $3 }' "$f" 2>/dev/null)
        [ -n "$_want_model" ] && [ "$_f_model" != "$_want_model" ] && continue
        last="$f"
    done
    printf '%s' "$last"
}

# print_table <csv> [model]: per-task summary of a CSV, with the model
# in the header line when it is known.
print_table() {
    f=$1; _model="$2"
    if [ -n "$_model" ]; then
        printf '  model: %s\n' "$_model"
    fi
    printf '  %-12s %-8s %-12s %-8s %s\n' "task" "result" "exit_reason" "seconds" "tokens in/out  tool_calls collapses mashura"
    awk -F, 'NR>1 { printf "  %-12s %-8s %-12s %-8s %s/%s  %s %s %s\n", $4, $6, $7, $8, $9, $10, $12, $13, $14 }' "$f"
}

# sum_cost <csv>: total cost_usd (n/a and empty cells contribute 0).
sum_cost() {
    awk -F, 'NR>1 && $11 != "n/a" && $11 != "" { s += $11 } END { printf "%.6f", s }' "$1"
}

# run_all <wakil-bin>: run every task `runs` times.
run_all() {
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        i=1
        while [ "$i" -le "$runs" ]; do
            run_one "$task" "$i" "$1" || return 1
            i=$((i + 1))
        done
    done
}

# ---- --setup ---------------------------------------------------------------
# Creates all task work folders and prints, per folder, the exact
# 'wakil sandbox trust <path>' command the user must run once. The
# harness itself never runs that command and never reads from the
# terminal.
do_setup() {
    for task in $tasks; do
        tdir="$tasks_dir/$task"
        workdir="$work_root/$task"
        prep_workdir "$task" || return 1
        echo "wakil sandbox trust $workdir"
    done
    echo
    echo "Run each command above once. The acknowledgement is keyed on the"
    echo "path (sha256 of the canonical path) and the content hash of"
    echo ".wakil/sandbox.json (sha256 of its raw bytes). Emptying and"
    echo "refilling the folder does not invalidate it as long as the risk"
    echo "label (and thus the sandbox.json content) is the same."
}

# ---- self-test -------------------------------------------------------------
self_test() {
    # Self-test CSVs go to a separate subdir so find_prev_csv never
    # picks them up as an earlier run.
    selftest_dir="$results_dir/selftest"
    mkdir -p "$selftest_dir"
    _saved_csv_file="$csv_file"
    _saved_work_root="$work_root"
    csv_file="$selftest_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
    printf '%s\n' "$csv_header" > "$csv_file"
    # Use a temp work root so the self-test never touches the real one.
    _self_work_root=$(mktemp -d "${TMPDIR:-/tmp}/bench-selftest-work.XXXXXX")
    work_root="$_self_work_root"

    tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/bench-selftest.XXXXXX")

    # Stub that changes nothing (writes a result-json with the correct
    # workdir so the guard passes, but no solution — check.sh fails).
    stub_noop="$tmpdir/wakil-noop"
    cat > "$stub_noop" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-noop/0.0.0" ;;
  run)
    shift
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-noop/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":0,"collapses":0,"mashura_rounds":0}
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
{"wakil_version":"stub-solve/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"tool_calls":2,"collapses":0,"mashura_rounds":0}
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
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-slow/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":0,"collapses":0,"mashura_rounds":0}
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
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-wrong/0.0.0","model":"stub","exit_reason":"pass","workdir":"/tmp/definitely-wrong","seconds":0.0,"tokens_in":1,"tokens_out":1,"cost_usd":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_wrong"

    echo "== self-test: stub that changes nothing (expect fail on every task) =="
    task_idx=0
    _fail_count=0
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="" run_one "$task" "self-noop" "$stub_noop"
        _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
        [ "$_row_result" = "fail" ] || { echo "self-test FAILED: noop $task: expected fail, got $_row_result" >&2; _fail_count=$((_fail_count + 1)); }
    done
    [ "$_fail_count" -eq 0 ] || { rm -rf "$tmpdir" "$_self_work_root"; exit 1; }

    echo "== self-test: stub that applies the solution (expect pass on every task) =="
    task_idx=0
    _pass_count=0
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="$tasks_dir/$task" run_one "$task" "self-solve" "$stub_solve"
        _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
        [ "$_row_result" = "pass" ] || { echo "self-test FAILED: solve $task: expected pass, got $_row_result" >&2; _pass_count=$((_pass_count + 1)); }
    done
    [ "$_pass_count" -eq 0 ] || { rm -rf "$tmpdir" "$_self_work_root"; exit 1; }

    echo "== self-test: stub that sleeps past the timeout (expect timeout) =="
    # Use the harness's own timeout path with a short per-run timeout so
    # the whole self-test stays fast: the slow stub is killed at 2s.
    _saved_timeout="$timeout_sec"
    timeout_sec=2
    task_idx=1
    BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-slow" "$stub_slow"
    timeout_sec="$_saved_timeout"
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    [ "$_row_result" = "timeout" ] || { echo "self-test FAILED: slow stub: expected timeout, got $_row_result" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }

    echo "== self-test: stub that reports a wrong workdir (expect error + stop) =="
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

    echo "== self-test: stub that reports a missing workdir (expect error + stop) =="
    # Stub that writes a result-json without a workdir field — the guard
    # must treat a missing workdir as an error.
    stub_missing="$tmpdir/wakil-missing"
    cat > "$stub_missing" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-missing/0.0.0" ;;
  run)
    shift
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-missing/0.0.0","model":"stub","exit_reason":"pass","seconds":0.0,"tokens_in":1,"tokens_out":1,"cost_usd":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0}
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

    echo "== self-test: fixed-path freshness (expect marker gone in run 2) =="
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

    echo "== self-test: .wakil/ preservation (sandbox.json byte-identical after two refills) =="
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

    echo "== self-test: fixed-path stability (expect same path across runs) =="
    _stab_task="$(echo "$tasks" | awk '{print $1}')"
    _stab_path1="$work_root/$_stab_task"
    prep_workdir "$_stab_task"
    _stab_path2="$work_root/$_stab_task"
    if [ "$_stab_path1" != "$_stab_path2" ]; then
        echo "self-test FAILED: path changed between runs ($_stab_path1 -> $_stab_path2)" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  path stability OK ($_stab_path1)"

    echo
    echo "== self-test summary =="
    echo "CSV: $csv_file"
    print_table "$csv_file" "$resolved_model"
    echo "total cost: $(sum_cost "$csv_file")"
    echo
    echo "self-test done (harness worked)"

    rm -rf "$tmpdir" "$_self_work_root"
    # Restore the real-run work root and CSV so main() and find_prev_csv
    # see them.
    work_root="$_saved_work_root"
    csv_file="$_saved_csv_file"
}

main() {
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
    csv_file="$results_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
    printf '%s\n' "$csv_header" > "$csv_file"

    run_all "$wakil_bin" || exit 1

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