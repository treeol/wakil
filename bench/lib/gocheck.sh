#!/bin/sh
# gocheck.sh — shared helper for the check.sh of the Go tasks. Sourced, not run.
#
# The agent can write code that makes "go test" report success without the
# hidden test having run (a TestMain that exits 0, an init() that prints a
# PASS line and exits, a go.mod/go.work/vendor tree that redirects the build).
# So the check never runs the agent's tree. It copies the work folder to a
# temp dir (no .git, no .wakil) and there:
#   - refuses symlinks, go.work and vendor/ in the work folder;
#   - deletes every *_test.go and testdata/ (the agent's tests are ignored,
#     not penalised);
#   - restores the task's reference go.mod and drops go.sum;
#   - refuses any non-test package that imports outside an allow-list (so
#     agent code cannot reach os, syscall, runtime, unsafe, embed, cgo) or that
#     carries assembly/C/cgo files;
#   - copies the task's hidden/ tree in and runs it with a clean Go environment
#     (gocheck_env: no GOFLAGS/GOENV/GOWORK/cgo from the agent or the user, the
#     toolchain pinned to the one bench.sh resolved, GOPROXY=off so nothing is
#     ever downloaded), requiring rc 0, no "--- FAIL" and a "--- PASS: <name>"
#     line per test.
#
# Exit status of a check (bench.sh maps it, see check_result_for_rc):
#   0   pass
#   1   fail: the check ran and judged the agent's work. Only gocheck_fail
#       exits 1, and it leaves a verdict marker; a status 1 without the marker
#       (a failed cp, a python traceback, "set -e" on a bare command) is turned
#       into 99 by the EXIT trap, so an accident can never read as a judgement.
#   99  error: the check could not judge (toolchain missing or too old, no
#       temp space, a hidden tree that is missing...). Any other status
#       (126/127 missing interpreter, 137 killed, a timeout) is an error too.
# Every go command that can fail goes through gocheck_go: when it fails, the
# same check is run once on the task's reference solution (gocheck_probe); if
# that fails too the environment is to blame (error), else the agent's tree is
# (fail). BENCH_PREFLIGHT=1 marks such a run (bench.sh sets it for its
# preflight and the probe sets it itself): the tree is the reference solution,
# so any failure is an environment error.
#
# Limit: this stops code that cheats inside the test process; it does not
# sandbox agent code that the check builds and runs (add-flag runs the agent's
# binary on the host) — see bench/README.md.

# gocheck_init <task-dir>: temp dir, EXIT trap (the status mapping above).
# Every check calls it first; sourcing this file does nothing by itself.
gocheck_init() {
    GC_TDIR=$(CDPATH='' cd -- "$1" && pwd) || exit 99
    GC_TMP=$(mktemp -d "${TMPDIR:-/tmp}/bench-check.XXXXXX") || { echo "check: environment error: cannot create a temp dir" >&2; exit 99; }
    trap gocheck_exit EXIT
}

gocheck_exit() {
    _ge_rc=$?
    trap - EXIT
    if [ "$_ge_rc" -eq 1 ] && [ ! -e "${GC_TMP:-/nonexistent}/verdict" ]; then
        echo "check: environment error: the check ended with status 1 without a verdict of its own" >&2
        _ge_rc=99
    fi
    [ -z "${GC_TMP:-}" ] || rm -rf "$GC_TMP"
    exit "$_ge_rc"
}

# gocheck_fail <reason>: the check ran and the agent's work is wrong.
gocheck_fail() {
    if [ "${BENCH_PREFLIGHT:-}" = 1 ]; then
        gocheck_error "the reference solution does not pass: $*"
    fi
    echo "check: $*" >&2
    [ -z "${GC_TMP:-}" ] || : > "$GC_TMP/verdict"
    exit 1
}

# gocheck_error <reason>: the check could not judge.
gocheck_error() {
    echo "check: environment error: $*" >&2
    exit 99
}

# gocheck_probe: run this task's check on its reference solution (repo/ with
# solution/ on top) in a fresh tree. Returns 0 when it passes (the environment
# works), 1 otherwise with GC_PROBE_MSG set to the first "check:" line.
gocheck_probe() {
    _gp="$GC_TMP/probe"
    rm -rf "$_gp"
    GC_PROBE_MSG="cannot build the reference tree"
    mkdir -p "$_gp" || return 1
    cp -R "$GC_TDIR/repo/." "$_gp/" || return 1
    if [ -d "$GC_TDIR/solution" ]; then
        cp -R "$GC_TDIR/solution/." "$_gp/" || return 1
    fi
    _gp_rc=0
    ( cd "$_gp" && BENCH_PREFLIGHT=1 "$GC_TDIR/check.sh" ) > "$GC_TMP/probe.out" 2>&1 || _gp_rc=$?
    [ "$_gp_rc" -ne 0 ] || return 0
    GC_PROBE_MSG=$(grep '^check:' "$GC_TMP/probe.out" | head -n 1)
    [ -n "$GC_PROBE_MSG" ] || GC_PROBE_MSG="the reference check ended with status $_gp_rc"
    return 1
}

# gocheck_blame <reason>: something failed that agent code can cause. Decide
# whether the agent or the environment is to blame.
gocheck_blame() {
    if [ "${BENCH_PREFLIGHT:-}" = 1 ]; then
        gocheck_error "$*"
    fi
    if ! gocheck_probe; then
        gocheck_error "$* — and the reference solution does not pass here either: $GC_PROBE_MSG"
    fi
    gocheck_fail "$*"
}

# gocheck_go <go args>...: run go; on failure print its output and blame.
gocheck_go() {
    if go "$@" > "$GC_TMP/go.out" 2>&1; then
        return 0
    fi
    cat "$GC_TMP/go.out"
    gocheck_blame "go $* failed"
}

# gocheck_bin <stdin-file> <arg>...: run the agent-built binary $GC_BIN with
# the file as stdin. Sets GC_BIN_RC and GC_BIN_OUT (stdout, trailing newlines
# stripped); stderr goes to $GC_TMP/bin.err. A status of 125/126/127 means the
# binary could not be started (noexec filesystem, missing loader): the
# environment, not the agent's behaviour.
gocheck_bin() {
    _gb_in=$1; shift
    GC_BIN_RC=0
    "$GC_BIN" "$@" < "$_gb_in" > "$GC_TMP/bin.out" 2> "$GC_TMP/bin.err" || GC_BIN_RC=$?
    GC_BIN_OUT=$(cat "$GC_TMP/bin.out")
    case "$GC_BIN_RC" in
        125|126|127) gocheck_error "cannot run the built binary (status $GC_BIN_RC): $(head -n 1 "$GC_TMP/bin.err")" ;;
    esac
}

# gocheck_env: the fixed Go environment of every check and of setup.sh. Not
# inherited from the agent or the user, except what bench.sh resolved once at
# start in the repo root and passes in BENCH_*: the toolchain (GOTOOLCHAIN is
# that exact version, so a toolchain already in the local cache is used even
# when the host's own Go is older than a task's go.mod) and the cache
# locations (GOENV=off hides "go env -w" settings, so they are handed over
# explicitly). GOPROXY=off: a check never downloads a toolchain or a module.
gocheck_env() {
    if [ -z "${BENCH_GOTOOLCHAIN:-}" ]; then
        echo "check: BENCH_GOTOOLCHAIN is not set (checks are started by scripts/bench.sh)" >&2
        exit 99
    fi
    export GOTOOLCHAIN="$BENCH_GOTOOLCHAIN" GOPROXY=off
    export GOFLAGS=-buildvcs=false GOWORK=off GOENV=off CGO_ENABLED=0
    [ -z "${BENCH_GOMODCACHE:-}" ] || export GOMODCACHE="$BENCH_GOMODCACHE"
    [ -z "${BENCH_GOCACHE:-}" ] || export GOCACHE="$BENCH_GOCACHE"
    [ -z "${BENCH_GOPATH:-}" ] || export GOPATH="$BENCH_GOPATH"
    unset GOROOT GOTMPDIR GOEXPERIMENT GOARCH GOOS GOCACHEPROG
}

# gocheck_prepare <task-dir> "<allowed stdlib imports, space separated>"
gocheck_prepare() {
    gocheck_init "$1"
    GC_ALLOW=$2
    gocheck_env

    if [ -n "$(find . \( -path ./.git -o -path ./.wakil \) -prune -o -type l -print | head -n 1)" ]; then
        gocheck_fail "a symlink exists in the work folder"
    fi
    for f in go.work go.work.sum vendor; do
        [ ! -e "$f" ] || gocheck_fail "$f exists in the work folder"
    done

    mkdir "$GC_TMP/w" || gocheck_error "cannot create $GC_TMP/w"
    tar --exclude=./.git --exclude=./.wakil -cf "$GC_TMP/w.tar" . || gocheck_error "cannot copy the work folder"
    tar -xf "$GC_TMP/w.tar" -C "$GC_TMP/w" || gocheck_error "cannot copy the work folder"
    cd "$GC_TMP/w" || gocheck_error "cannot enter the copy of the work folder"

    find . -type f -name '*_test.go' -delete
    find . -type d -name testdata -prune -exec rm -rf {} +
    rm -f go.sum
    cp "$GC_TDIR/repo/go.mod" go.mod || gocheck_error "cannot restore the task's go.mod"

    GC_MOD=$(awk '$1 == "module" { print $2; exit }' go.mod)
    # -deps walks the import closure of ./..., not just the directories ./...
    # matches: a package in a "_x" or ".x" directory is skipped by ./... but
    # can still be imported, so it must be checked too. Standard-library
    # packages are filtered out (their own imports are not the agent's).
    go list -e -deps -f '{{if not .Standard}}{{.ImportPath}}|{{join .Imports " "}}|{{if .Error}}{{.Error}}{{end}}|{{join .SFiles " "}}{{join .CgoFiles " "}}{{join .CFiles " "}}{{join .CXXFiles " "}}{{join .MFiles " "}}{{join .HFiles " "}}{{join .FFiles " "}}{{join .SwigFiles " "}}{{join .SwigCXXFiles " "}}{{join .SysoFiles " "}}{{end}}' ./... > "$GC_TMP/list.txt" 2>"$GC_TMP/list.err" \
        || gocheck_blame "go list failed: $(head -n 3 "$GC_TMP/list.err")"
    while IFS='|' read -r p imps err extra; do
        [ -n "$p" ] || continue
        [ -z "$err" ] || gocheck_fail "package $p: $err"
        [ -z "$extra" ] || gocheck_fail "package $p has assembly/C/cgo files: $extra"
        for i in $imps; do
            case "$i" in
                "$GC_MOD"|"$GC_MOD"/*) continue ;;
            esac
            case " $GC_ALLOW " in
                *" $i "*) continue ;;
            esac
            gocheck_fail "package $p imports $i, which is not allowed (allowed: $GC_ALLOW and the module's own packages)"
        done
    done < "$GC_TMP/list.txt"
}

# gocheck_run <package> <TestName>...
# Restores the hidden tests and runs them; see the header for what must hold.
gocheck_run() {
    pkg=$1; shift
    [ -d "$GC_TDIR/hidden" ] || gocheck_error "task has no hidden/ tree"
    cp -R "$GC_TDIR/hidden/." . || gocheck_error "cannot restore the hidden tests"
    if ! go test -count=1 -v "$pkg" > "$GC_TMP/test.out" 2>&1; then
        cat "$GC_TMP/test.out"
        gocheck_blame "go test failed"
    fi
    if grep -q '^--- FAIL' "$GC_TMP/test.out"; then
        cat "$GC_TMP/test.out"
        gocheck_fail "a test failed"
    fi
    for n in "$@"; do
        grep -q "^--- PASS: $n " "$GC_TMP/test.out" || {
            cat "$GC_TMP/test.out"
            gocheck_fail "test $n did not report PASS"
        }
    done
    echo "check: ok ($*)"
}
