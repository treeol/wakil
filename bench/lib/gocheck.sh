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
#     (no GOFLAGS/GOENV/GOWORK/GOTOOLCHAIN/cgo from the agent or the user),
#     requiring rc 0, no "--- FAIL" and a "--- PASS: <name>" line per test.
# Limit: this stops code that cheats inside the test process; it does not
# sandbox agent code that the check builds and runs (add-flag runs the agent's
# binary on the host) — see bench/README.md.

gocheck_fail() {
    echo "check: $*" >&2
    exit 1
}

# gocheck_prepare <task-dir> "<allowed stdlib imports, space separated>"
gocheck_prepare() {
    GC_TDIR=$(CDPATH='' cd -- "$1" && pwd) || exit 1
    GC_ALLOW=$2
    GC_TMP=$(mktemp -d "${TMPDIR:-/tmp}/bench-check.XXXXXX") || exit 1
    trap 'rm -rf "$GC_TMP"' EXIT

    if [ -n "$(find . \( -path ./.git -o -path ./.wakil \) -prune -o -type l -print | head -n 1)" ]; then
        gocheck_fail "a symlink exists in the work folder"
    fi
    for f in go.work go.work.sum vendor; do
        [ ! -e "$f" ] || gocheck_fail "$f exists in the work folder"
    done

    mkdir "$GC_TMP/w"
    tar --exclude=./.git --exclude=./.wakil -cf "$GC_TMP/w.tar" . || gocheck_fail "cannot copy the work folder"
    tar -xf "$GC_TMP/w.tar" -C "$GC_TMP/w" || gocheck_fail "cannot copy the work folder"
    cd "$GC_TMP/w" || exit 1

    find . -type f -name '*_test.go' -delete
    find . -type d -name testdata -prune -exec rm -rf {} +
    rm -f go.sum
    cp "$GC_TDIR/repo/go.mod" go.mod

    # The Go environment is fixed here, not inherited.
    export GOFLAGS=-buildvcs=false GOWORK=off GOENV=off GOTOOLCHAIN=local CGO_ENABLED=0 GOPROXY=off
    unset GOROOT GOTMPDIR GOEXPERIMENT GOARCH GOOS

    GC_MOD=$(awk '$1 == "module" { print $2; exit }' go.mod)
    # -deps walks the import closure of ./..., not just the directories ./...
    # matches: a package in a "_x" or ".x" directory is skipped by ./... but
    # can still be imported, so it must be checked too. Standard-library
    # packages are filtered out (their own imports are not the agent's).
    go list -e -deps -f '{{if not .Standard}}{{.ImportPath}}|{{join .Imports " "}}|{{if .Error}}{{.Error}}{{end}}|{{join .SFiles " "}}{{join .CgoFiles " "}}{{join .CFiles " "}}{{join .CXXFiles " "}}{{join .MFiles " "}}{{join .HFiles " "}}{{join .FFiles " "}}{{join .SwigFiles " "}}{{join .SwigCXXFiles " "}}{{join .SysoFiles " "}}{{end}}' ./... > "$GC_TMP/list.txt" 2>"$GC_TMP/list.err" \
        || gocheck_fail "go list failed: $(head -n 3 "$GC_TMP/list.err")"
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
    [ -d "$GC_TDIR/hidden" ] || gocheck_fail "task has no hidden/ tree"
    cp -R "$GC_TDIR/hidden/." . || gocheck_fail "cannot restore the hidden tests"
    if ! go test -count=1 -v "$pkg" > "$GC_TMP/test.out" 2>&1; then
        cat "$GC_TMP/test.out"
        gocheck_fail "go test failed"
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
