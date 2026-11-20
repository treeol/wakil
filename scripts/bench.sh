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

csv_header='date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,tokens_cached,cost_usd,tool_calls,collapses,collapses_turn_end,mashura_rounds'
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

# fmt_cost <n>: format USD cost with 4 decimals ("0.0038"). Non-numeric passes through.
fmt_cost() {
    case "$1" in
        ''|n/a) echo 'n/a' ;;
        *) awk -v v="$1" 'BEGIN { if (v+0 == v) printf "%.4f", v; else printf "%s", v }' ;;
    esac
}

# csv_row <task> <run> <result> <exit_reason> <seconds> <result-json>
# Appends one row to the CSV.
csv_row() {
    task=$1; run=$2; result=$3; exit_reason=$4; secs=$5; rj=$6
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$(date +%Y-%m-%d)" "$wakil_version" "$resolved_model" "$task" "$run" "$result" \
        "$(na "$exit_reason")" "$secs" \
        "$(na "$(json_field "$rj" "tokens_in")")" "$(na "$(json_field "$rj" "tokens_out")")" \
        "$(na "$(json_field "$rj" "tokens_cached")")" \
        "$(na "$(json_field "$rj" "cost_usd")")" "$(na "$(json_field "$rj" "tool_calls")")" \
        "$(na "$(json_field "$rj" "collapses")")" "$(na "$(json_field "$rj" "collapses_turn_end")")" \
        "$(na "$(json_field "$rj" "mashura_rounds")")" >> "$csv_file"
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
        if ! ( cd "$workdir" && "$tdir/setup.sh" ) >"$workdir/.bench-setup.log" 2>&1; then
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
    if grep -rIl --exclude-dir=.git --exclude-dir=.wakil -e 'rand\.NewSource' -e 'rand\.Seed' "$_lw" >/dev/null 2>&1; then
        echo "$prog: LEAK in $_lt — a seeded generator is present in the work folder" >&2
        return 1
    fi
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

    wakil_before=$(wakil_digest "$workdir")
    start_epoch=$(date +%s)
    set +e
    # WAKIL_HOST_WORKDIR points the sandbox at the fixed work folder so
    # the agent runs there, not in the configured work_dir.
    export WAKIL_HOST_WORKDIR="$workdir"

    if [ -n "$model_name" ]; then
        WAKIL_MODEL="$model_name" timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 &
    else
        timeout -k "$kill_grace" "$timeout_sec" "$bin" run --auto --allow-destructive --no-memory $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" </dev/null >"$log_file.out" 2>&1 &
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
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
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
        # the resolved workdir from the binary; a missing value or mismatch
        # means the sandbox mounted the wrong directory. Skip check.sh on
        # wrong/missing workdir and stop the bench.
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

        # Post-run leak guard (file existence only; no git here, the work
        # folder's .git is agent-controlled now): nothing but repo/ was
        # copied in, so one of these names means the agent got hold of the
        # check, hidden files or the solution. Never scored; stops the bench.
        for _n in check.sh hidden solution; do
            if [ -e "$workdir/$_n" ]; then
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
        { printf '\n--- check.sh (%s) ---\n' "$task"; ( cd "$workdir" && "$tdir/check.sh" ) 2>&1; } >>"$log_file" 2>&1
        chk_rc=$?
        set -e
        if [ "$chk_rc" -eq 0 ]; then
            result="pass"
        else
            result="fail"
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
# in the header line when it is known. Reads columns by header name so
# both the current 16-column CSV (with tokens_cached and
# collapses_turn_end) and older CSVs render accurately.
print_table() {
    f=$1; _model="$2"
    if [ -n "$_model" ]; then
        printf '  model: %s\n' "$_model"
    fi
    printf '  %-12s %-8s %-12s %-7s %-16s %-8s %-9s %s\n' "task" "result" "exit_reason" "seconds" "tokens in/out" "cached-%" "cost" "tools collapses mashura"
    awk -F, '
        NR == 1 {
            for (i = 1; i <= NF; i++) col[$i] = i
            next
        }
        {
            task  = col["task"]        ? $(col["task"])        : "n/a"
            res   = col["result"]      ? $(col["result"])      : "n/a"
            ereas = col["exit_reason"] ? $(col["exit_reason"]) : "n/a"
            secs  = col["seconds"]     ? $(col["seconds"])     : "n/a"
            tin   = col["tokens_in"]   ? $(col["tokens_in"])   : "n/a"
            tout  = col["tokens_out"]  ? $(col["tokens_out"])  : "n/a"
            tcach = col["tokens_cached"] ? $(col["tokens_cached"]) : "n/a"
            cost  = col["cost_usd"]    ? $(col["cost_usd"])    : "n/a"
            tools = col["tool_calls"]  ? $(col["tool_calls"])  : "n/a"
            coll  = col["collapses"]   ? $(col["collapses"])   : "n/a"
            mash  = col["mashura_rounds"] ? $(col["mashura_rounds"]) : "n/a"

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
    awk -F, '
        NR == 1 {
            for (i = 1; i <= NF; i++) if ($i == "cost_usd") c = i
            next
        }
        c > 0 && $c != "n/a" && $c != "" { s += $c }
        END { printf "%.4f", s }
    ' "$1"
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
# 'WAKIL_HOST_WORKDIR=<path> wakil sandbox trust' command the user must run
# once (works with any binary, including older ones where positional <path>
# did not override config host_work_dir). The harness itself never runs that
# command and never reads from the terminal.
do_setup() {
    for task in $tasks; do
        tdir="$tasks_dir/$task"
        workdir="$work_root/$task"
        prep_workdir "$task" || return 1
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
{"wakil_version":"stub-solve/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":100,"tokens_out":5,"tokens_cached":75,"cost_usd":0.001234,"tool_calls":2,"collapses":0,"collapses_turn_end":1,"mashura_rounds":0}
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

    echo "== self-test: declined run that still solved the task (expect pass with exit_reason=declined) =="
    # Also asserts that bench.sh passes --allow-destructive to `wakil run`.
    stub_declined="$tmpdir/wakil-declined"
    cat > "$stub_declined" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-declined/0.0.0" ;;
  run)
    shift
    workdir=""
    saw_allow_destructive=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --allow-destructive) saw_allow_destructive=1; shift ;;
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
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
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-declined/0.0.0","model":"stub","exit_reason":"declined","workdir":"$workdir","seconds":0.0,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"tool_calls":2,"collapses":0,"mashura_rounds":0}
JSON
    exit 1 ;;
esac
EOF
    chmod +x "$stub_declined"
    _dec_task="$(echo "$tasks" | awk '{print $1}')"
    task_idx=1
    BENCH_TASK_DIR="$tasks_dir/$_dec_task" run_one "$_dec_task" "self-declined-pass" "$stub_declined"
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
    if [ "$_row_result" != "pass" ] || [ "$_row_reason" != "declined" ]; then
        echo "self-test FAILED: declined-solve: got result=$_row_result exit_reason=$_row_reason, want pass/declined" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    # Same declined stub WITHOUT copying solution → check.sh must run and record fail + exit_reason=declined.
    BENCH_TASK_DIR="" run_one "$_dec_task" "self-declined-fail" "$stub_declined"
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
    if [ "$_row_result" != "fail" ] || [ "$_row_reason" != "declined" ]; then
        echo "self-test FAILED: declined-noop: got result=$_row_result exit_reason=$_row_reason, want fail/declined" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  declined+check.sh OK (--allow-destructive passed; result from check.sh, exit_reason preserved)"

    echo "== self-test: agent-written test code must not fake a pass (expect fail) =="
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
    workdir=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --result-json) workdir=$(dirname "$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    d="$workdir/$BENCH_CHEAT_DIR"
    case "$BENCH_CHEAT" in
      testmain)
        printf 'package %s\nimport ("os"; "testing")\nfunc TestMain(m *testing.M) { os.Exit(0) }\n' "$BENCH_CHEAT_PKG" > "$d/zz_test.go" ;;
      init)
        printf 'package %s\nimport ("fmt"; "os")\nfunc init() { fmt.Println("--- PASS: %s (0.00s)"); fmt.Println("PASS"); os.Exit(0) }\n' "$BENCH_CHEAT_PKG" "$BENCH_CHEAT_TEST" > "$d/zz_init.go" ;;
    esac
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-cheat/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0}
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
        for _how in testmain init; do
            task_idx=1
            BENCH_CHEAT="$_how" BENCH_CHEAT_DIR="$_cdir" BENCH_CHEAT_PKG="$_cpkg" BENCH_CHEAT_TEST="$_ctest" \
                BENCH_TASK_DIR="" run_one "$_ct" "self-cheat-$_how" "$stub_cheat"
            _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
            if [ "$_row_result" != "fail" ]; then
                echo "self-test FAILED: $_ct with a $_how cheat: expected fail, got $_row_result" >&2
                rm -rf "$tmpdir" "$_self_work_root"
                exit 1
            fi
        done
    done
    echo "  TestMain-exit-0 and init()-prints-PASS cheats scored fail"

    echo "== self-test: no generator, seed or check in the work folder or its history =="
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

    echo "== self-test: an agent that plants the solution dir after prep (expect error, check.sh not run) =="
    stub_plant="$tmpdir/wakil-plant"
    cat > "$stub_plant" <<'EOF'
#!/bin/sh
case "$1" in
  version) echo "stub-plant/0.0.0" ;;
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
    mkdir -p "$workdir/solution" "$workdir/.wakil"
    echo planted > "$workdir/.wakil/state"
    cat > "$workdir/.bench-result.json" <<JSON
{"wakil_version":"stub-plant/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0}
JSON
    exit 0 ;;
esac
EOF
    chmod +x "$stub_plant"
    task_idx=1
    _rc=0
    _plant_err="$tmpdir/plant.err"
    ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-plant" "$stub_plant" ) 2>"$_plant_err" || _rc=$?
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
    if [ "$_rc" -ne 1 ] || [ "$_row_result" != "error" ] || [ "$_row_reason" != "leak-detected" ]; then
        echo "self-test FAILED: planted solution dir: rc=$_rc result=$_row_result reason=$_row_reason, want 1/error/leak-detected" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    grep -q 'changed files under .*/.wakil' "$_plant_err" || { echo "self-test FAILED: a change under .wakil/ was not reported" >&2; cat "$_plant_err" >&2; rm -rf "$tmpdir" "$_self_work_root"; exit 1; }
    echo "  planted solution/ -> error leak-detected; .wakil/ change reported"

    echo "== self-test: stub that sleeps past the timeout (expect timeout, exit 124) =="
    # Use the harness's own timeout path with a short per-run timeout so
    # the whole self-test stays fast: the slow stub is killed at 2s.
    _saved_timeout="$timeout_sec"
    timeout_sec=2
    task_idx=1
    _slow_task="$(echo "$tasks" | awk '{print $1}')"
    BENCH_TASK_DIR="" run_one "$_slow_task" "self-slow" "$stub_slow"
    timeout_sec="$_saved_timeout"
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
    if [ "$_row_result" != "timeout" ] || [ "$_row_reason" != "timeout" ]; then
        echo "self-test FAILED: slow stub: got result=$_row_result exit_reason=$_row_reason, want timeout/timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi

    echo "== self-test: stub that ignores TERM (expect timeout via exit 137, check.sh not run) =="
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
{"wakil_version":"stub-deaf/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tool_calls":1,"collapses":0,"mashura_rounds":0}
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
    _row_result=$(tail -1 "$csv_file" | awk -F, '{ print $6 }')
    _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
    _row_secs=$(tail -1 "$csv_file" | awk -F, '{ print $8 }')
    if [ "$_rc" -ne 0 ] || [ "$_row_result" != "timeout" ] || [ "$_row_reason" != "timeout" ]; then
        echo "self-test FAILED: TERM-ignoring stub: rc=$_rc result=$_row_result exit_reason=$_row_reason, want 0/timeout/timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if [ "$_row_secs" -lt 3 ]; then
        echo "self-test FAILED: TERM-ignoring stub ended after ${_row_secs}s; the KILL path (timeout 2s + grace 1s) was not exercised" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    if grep -q 'check.sh (' "$results_dir"/"$_slow_task"-self-deaf-*.log 2>/dev/null; then
        echo "self-test FAILED: check.sh ran after a timeout" >&2
        rm -rf "$tmpdir" "$_self_work_root"; exit 1
    fi
    echo "  TERM-ignoring run -> timeout (exit 137 after ${_row_secs}s), no check.sh, no sandbox-not-acknowledged"

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

    echo "== self-test: sandbox approval error detection (both wordings, before workdir guard) =="
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
    workdir=""
    while [ \$# -gt 0 ]; do
      case "\$1" in
        --result-json) workdir=\$(dirname "\$2"); shift 2 ;;
        --transcript) shift 2 ;;
        *) shift ;;
      esac
    done
    echo "executor error: $_case_msg" >&2
    cat > "\$workdir/.bench-result.json" <<JSON
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
        _row_reason=$(tail -1 "$csv_file" | awk -F, '{ print $7 }')
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

    echo "== self-test: cached-% and 4-decimal cost formatting =="
    _tbl_out=$(print_table "$csv_file" "$resolved_model")
    if ! printf '%s\n' "$_tbl_out" | grep -q '75%'; then
        echo "self-test FAILED: print_table missing cached-% (75%) column" >&2
        printf '%s\n' "$_tbl_out" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    if ! printf '%s\n' "$_tbl_out" | grep -q '\$0\.0012'; then
        echo "self-test FAILED: print_table cost not formatted with 4 decimals (\$0.0012)" >&2
        printf '%s\n' "$_tbl_out" >&2
        rm -rf "$tmpdir" "$_self_work_root"
        exit 1
    fi
    echo "  cached-% (75%) and 4-decimal cost (\$0.0012) OK"

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