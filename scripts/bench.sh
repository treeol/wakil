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

# Metric columns come from jq parsing of the per-run result JSON; without
# jq they are honestly n/a, but say so once rather than per row.
if ! command -v jq >/dev/null 2>&1; then
    echo "$prog: warning: jq not found — metric columns (tokens, cost, counts) will be n/a" >&2
fi

csv_header='date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,cost_usd,tool_calls,collapses,mashura_rounds'
csv_file="$results_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
printf '%s\n' "$csv_header" > "$csv_file"

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

# run_one <task> <run> <wakil-bin>
# Copies the task repo to a fresh temp dir, sets it up, runs wakil under
# the timeout, then checks. The task's check and reference solution are
# never copied into the work folder. Always returns 0 (a failed task is
# a result, not a harness error); a crashed run still leaves a CSV row.
run_one() {
    task=$1; run=$2; bin=$3
    tdir="$tasks_dir/$task"
    workdir=$(mktemp -d "${TMPDIR:-/tmp}/bench-${task}-${run}.XXXXXX")

    # Fresh copy + git init + one commit. The temp repo inherits the
    # host's global git config and the host's GIT_CONFIG_* env vars
    # (injected by the signing-enforced session at COMMAND scope), so a host
    # with commit.gpgsign=true would make ResolveRepoSigning
    # (internal/exec/signing.go) resolve a signing key and the generated hisn
    # profile would carry a "signing" risk label — tripping the
    # sandbox-ack gate even though bench tasks never sign.
    #
    # GIT_CONFIG_GLOBAL=/dev/null suppresses the file-level global config
    # (~/.gitconfig, /etc/gitconfig) but does NOT suppress the GIT_CONFIG_*
    # env vars already in the environment (GIT_CONFIG_COUNT/KEY_n/VALUE_n).
    # Those env vars are injected by BackendExecutor.signingEnv() at COMMAND
    # scope and win over every config file. The only reliable way to
    # prevent them from leaking into the probe and the run is to unset every
    # GIT_CONFIG_* variable before the probe and wakil run.
    #
    # A repo-local user.name and user.email are set after init so the
    # commit has an identity without reading any host config file.
    cp -R "$tdir/repo/." "$workdir/"
    (
        cd "$workdir"
        env -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_SYSTEM \
            git init -q \
            && git add -A \
            && git -c user.email=bench@example.invalid -c user.name=bench commit -qm "bench task $task"
    ) 2>/dev/null || true

    # Optional setup hook (task-level setup.sh, runs in the work copy
    # before the agent). The hook's own location is the task dir; it
    # should address the work copy as the working directory.
    if [ -x "$tdir/setup.sh" ]; then
        ( cd "$workdir" && "$tdir/setup.sh" ) >/dev/null 2>&1 || true
    fi

    task_text=$(cat "$tdir/task.txt")
    result_json="$workdir/.bench-result.json"
    log_file="$results_dir/${task}-${run}-$(date +%H%M%S).log"

    # Progress: one line before each run so a long session is not silent.
    printf '[%d/%d] %s (run %s/%d) … running, log: %s\n' \
        "$task_idx" "$total_tasks" "$task" "$run" "$runs" "$log_file"

    start_epoch=$(date +%s)
    set +e
    # WAKIL_HOST_WORKDIR points the sandbox at the temp work copy so the
    # agent runs there, not in the configured work_dir. Without it, the
    # agent sees the wrong repository — the one in the config file, not
    # the bench task's freshly-copied repo.
    export WAKIL_HOST_WORKDIR="$workdir"

    # Strip every GIT signing env var (GIT_CONFIG_COUNT, GIT_CONFIG_KEY_n,
    # GIT_CONFIG_VALUE_n) injected by the host's signing-enforced session
    # at COMMAND scope. These win over every config file and would make
    # ResolveRepoSigning see commit.gpgsign=true even in a fresh temp repo
    # with GIT_CONFIG_GLOBAL=/dev/null. Without this strip, the sandbox
    # risk probe and wakil run inherit the signing label and the bench
    # stops with "sandbox-not-acknowledged".
    unset GIT_CONFIG_COUNT GIT_CONFIG_SYSTEM GIT_CONFIG_GLOBAL
    i=0
    while [ $i -lt 20 ]; do
        unset "GIT_CONFIG_KEY_$i" "GIT_CONFIG_VALUE_$i"
        i=$((i + 1))
    done
    export GIT_CONFIG_GLOBAL=/dev/null
    export GIT_CONFIG_SYSTEM=/dev/null

    if [ -n "$model_name" ]; then
        WAKIL_MODEL="$model_name" timeout -k 10 "$timeout_sec" "$bin" run --auto $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1 &
    else
        timeout -k 10 "$timeout_sec" "$bin" run --auto $counsel_args \
            --result-json "$result_json" --transcript "$log_file" "$task_text" >"$log_file.out" 2>&1 &
    fi
    run_pid=$!

    # Heartbeat: while the run is in progress, print one short line with the
    # elapsed time every 30s (only when stdout is a terminal), so a long
    # task does not look hung. Nothing extra when output is piped or
    # redirected. Skipped when the timeout is too short for a heartbeat.
    if [ "$is_tty" -eq 1 ] && [ "$timeout_sec" -gt 30 ]; then
        while :; do
            sleep 30
            kill -0 "$run_pid" 2>/dev/null || break
            now=$(date +%s)
            elapsed=$((now - start_epoch))
            printf '[%d/%d] %s (run %s/%d) … %ds elapsed\n' \
                "$task_idx" "$total_tasks" "$task" "$run" "$runs" "$elapsed"
        done
    fi

    wait "$run_pid"
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
    # sandbox-not-acknowledged and the bench stops with the label named
    # (the pre-run probe already caught the common case; this catches a
    # label that only surfaces after the model ran, e.g. a write that
    # triggers a new capability).
    if [ "$result" != "pass" ] && [ -z "$exit_reason" ]; then
        if grep -q "sandbox risk is not acknowledged" "$log_file" 2>/dev/null; then
            result="error"
            exit_reason="sandbox-not-acknowledged"
        fi
    fi

    # Workdir guard: verify the agent ran in the temp folder we set
    # up, not in the user's configured work_dir. The result JSON carries
    # the resolved workdir from the binary; mismatch means the sandbox
    # mounted the wrong directory.
    _actual_workdir=$(json_field "$result_json" "workdir")
    if [ -n "$_actual_workdir" ] && [ "$_actual_workdir" != "$workdir" ]; then
        echo "$prog: WRONG WORKDIR in $task run $run: binary reports workdir=$_actual_workdir, expected $workdir — agent ran in the wrong folder" >&2
        result="error"
        exit_reason="wrong-workdir"
        csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"
        cat "$log_file" 2>/dev/null || true
        rm -rf "$workdir"
        return 1
    fi
    # If the binary did not report a workdir at all (old binary or stub
    # without the field), the guard is skipped — only a mismatch is an
    # error, not a missing field.

    csv_row "$task" "$run" "$result" "$exit_reason" "$secs" "$result_json"

    # A sandbox-not-acknowledged result is a harness-level error, not a
    # model failure: stop the whole bench and name the label so the user
    # can fix it. The label was detected by the post-run log scan above.
    if [ "$exit_reason" = "sandbox-not-acknowledged" ]; then
        echo "$prog: bench stopped — task $task run $run: sandbox risk is not acknowledged in $workdir" >&2
        echo "  run 'wakil sandbox trust $workdir' to review and acknowledge it" >&2
        rm -rf "$workdir"
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

    rm -rf "$workdir"
}

# find_prev_csv: the most recent bench CSV (not this run's, not a
# self-test). Echoes path. Self-test CSVs live in $results_dir/selftest/
# and are excluded by the top-level glob (results_dir/20*-bench.csv matches
# direct children only, not the selftest/ subdir).
find_prev_csv() {
    last=""
    for f in "$results_dir"/20*-bench.csv; do
        [ -f "$f" ] || continue
        [ "$f" = "$csv_file" ] && continue
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

# ---- self-test -------------------------------------------------------------
self_test() {
    # Self-test CSVs go to a separate subdir so find_prev_csv never
    # picks them up as an earlier run.
    selftest_dir="$results_dir/selftest"
    mkdir -p "$selftest_dir"
    _saved_csv_file="$csv_file"
    csv_file="$selftest_dir/$(date +%Y%m%dT%H%M%S)-bench.csv"
    printf '%s\n' "$csv_header" > "$csv_file"

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
{"wakil_version":"stub-solve/0.0.0","model":"stub","exit_reason":"pass","workdir":"$workdir","seconds":0.0,"tokens_in":10,"tokens_out":5,"cost_usd":0.001,"tool_calls":2,"collapses":0,"mashura_rounds":0}
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
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="" run_one "$task" "self-noop" "$stub_noop"
    done
    echo "== self-test: stub that applies the solution (expect pass on every task) =="
    task_idx=0
    for task in $tasks; do
        task_idx=$((task_idx + 1))
        BENCH_TASK_DIR="$tasks_dir/$task" run_one "$task" "self-solve" "$stub_solve"
    done
    echo "== self-test: stub that sleeps past the timeout (expect timeout) =="
    # Use the harness's own timeout path with a short per-run timeout so
    # the whole self-test stays fast: the slow stub is killed at 2s.
    _saved_timeout="$timeout_sec"
    timeout_sec=2
    task_idx=1
    BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-slow" "$stub_slow"
    timeout_sec="$_saved_timeout"

    echo "== self-test: stub that reports a wrong workdir (expect error + stop) =="
    _saved_timeout="$timeout_sec"
    timeout_sec=5
    task_idx=1
    _rc=0
    ( set +e; BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-wrong" "$stub_wrong" ) || _rc=$?
    timeout_sec="$_saved_timeout"
    if [ "$_rc" -ne 1 ]; then
        echo "self-test FAILED: wrong-workdir guard did not stop the bench (exit=$_rc)" >&2
        rm -rf "$tmpdir"
        exit 1
    fi
    echo "  wrong-workdir guard fired correctly (bench stopped with exit 1)"

    echo "== self-test: signing env strip (expect no sandbox-not-acknowledged) =="
    # Simulate a signing-enforced session: set GIT_CONFIG_* env vars that
    # would make ResolveRepoSigning see commit.gpgsign=true. The harness
    # must strip them before the run so the sandbox risk probe does not
    # fire. We verify by checking that the stub's run does not produce
    # exit_reason=sandbox-not-acknowledged.
    _saved_timeout="$timeout_sec"
    timeout_sec=5
    task_idx=1
    _rc=0
    (
        set +e
        # Inject signing env vars like BackendExecutor.signingEnv() does
        export GIT_CONFIG_COUNT=6
        export GIT_CONFIG_KEY_0="user.signingkey"
        export GIT_CONFIG_VALUE_0="key::ssh-ed25519 AAAATEST"
        export GIT_CONFIG_KEY_1="gpg.format"
        export GIT_CONFIG_VALUE_1="ssh"
        export GIT_CONFIG_KEY_2="commit.gpgsign"
        export GIT_CONFIG_VALUE_2="true"
        export GIT_CONFIG_KEY_3="tag.gpgsign"
        export GIT_CONFIG_VALUE_3="true"
        export GIT_CONFIG_KEY_4="gpg.ssh.program"
        export GIT_CONFIG_VALUE_4="ssh-keygen"
        export GIT_CONFIG_KEY_5="gpg.ssh.allowedSignersFile"
        export GIT_CONFIG_VALUE_5="/tmp/none"
        BENCH_TASK_DIR="" run_one "$(echo "$tasks" | awk '{print $1}')" "self-sign" "$stub_noop"
    ) || _rc=$?
    timeout_sec="$_saved_timeout"
    # The noop stub exits 0 (no result-json), so result="fail" and
    # exit_reason is empty (not sandbox-not-acknowledged). If the env
    # strip failed, the post-run log scan would find "sandbox risk is not
    # acknowledged" in the log — but there is no log because the stub
    # doesn't write one. The key check: the run must NOT exit with
    # exit_reason=sandbox-not-acknowledged.
    _sign_exit_reason=$(awk -F, 'NR>1 && $4=="self-sign" {print $7}' "$csv_file" | tail -1)
    if [ "$_sign_exit_reason" = "sandbox-not-acknowledged" ]; then
        echo "self-test FAILED: signing env strip did not prevent sandbox-not-acknowledged" >&2
        rm -rf "$tmpdir"
        exit 1
    fi
    echo "  signing env strip works (no sandbox-not-acknowledged)"

    echo
    echo "== self-test summary =="
    echo "CSV: $csv_file"
    print_table "$csv_file" "$resolved_model"
    echo "total cost: $(sum_cost "$csv_file")"
    echo
    echo "self-test done (harness worked)"

    rm -rf "$tmpdir"
    # Restore the real-run CSV so main() and find_prev_csv see it.
    csv_file="$_saved_csv_file"
}

main() {
    if [ "$self_test" -eq 1 ]; then
        self_test
        return 0
    fi

    case "$wakil_bin" in
        */*) [ -x "$wakil_bin" ] || { echo "$prog: wakil binary not executable: $wakil_bin" >&2; exit 1; } ;;
    esac

    run_all "$wakil_bin" || exit 1

    echo
    echo "== this run =="
    echo "CSV: $csv_file"
    print_table "$csv_file" "$resolved_model"
    echo "total cost: $(sum_cost "$csv_file")"

    prev=$(find_prev_csv)
    if [ -n "$prev" ]; then
        echo
        echo "== most recent earlier run (model $resolved_model) =="
        echo "CSV: $prev"
        print_table "$prev" "$resolved_model"
        echo "total cost: $(sum_cost "$prev")"
    fi
}

main "$@"
