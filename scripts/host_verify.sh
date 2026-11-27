#!/bin/sh
# host_verify.sh — verify a wakil work branch from the host.
#
# Usage: scripts/host_verify.sh [--fast] <base-ref>
# (e.g. scripts/host_verify.sh master)
#
# Never touches the user's real config or data. Sets HOME, XDG_CONFIG_HOME,
# XDG_DATA_HOME to a fresh temp dir for everything Wakil itself reads, but
# keeps the user's existing Go caches (GOMODCACHE, GOCACHE, GOPATH) so Go
# does not download a toolchain or modules into the temp home. Uses no API
# key, no network except Go module and tool downloads. Changes nothing in
# the repo outside .tmp/.
#
# --fast skips the -race test (slow).
set -eu

fast=0
if [ "${1:-}" = "--fast" ]; then
  fast=1; shift
fi

base_ref="${1:?usage: host_verify.sh [--fast] <base-ref>}"
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"
# shellcheck source=host_verify_summary.sh
. "$repo_root/scripts/host_verify_summary.sh"
# shellcheck source=host_verify_hisn.sh
. "$repo_root/scripts/host_verify_hisn.sh"

# --- header (read go and git info BEFORE redirecting HOME) ---
go_version=$(go version 2>&1 || echo "go: not found")
go_mod_line=$(head -3 go.mod 2>/dev/null | tail -1 || echo "unknown")
gotoolchain=$(go env GOTOOLCHAIN 2>&1 || echo "unknown")

outdir="$repo_root/.tmp/host-verify/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$outdir"

# --- helpers ---
pass=0; fail=0; skip=0

check() {
  name="$1"; shift
  # `set -e` would abort the script on the first failing check
  # before rc=$? ran (POSIX: a failing unconditioned command
  # exits). Guard with `|| rc=$?` so every check runs and the
  # summary counts all of them.
  # The log holds both output streams of the command (stdout and stderr), whole:
  # a command that keeps its own log (go test) prints it in full, never a tail.
  rc=0
  "$@" >"$outdir/$name.log" 2>&1 || rc=$?
  if [ $rc -eq 0 ]; then
    echo "PASS  $name"
    pass=$((pass+1))
  elif [ $rc -eq 77 ]; then
    echo "SKIP  $name"
    skip=$((skip+1))
  else
    echo "FAIL  $name (exit $rc)"
    fail=$((fail+1))
    hv_summarize "$name" "$outdir/$name.log"
  fi
}

# --- print header ---
echo "=== host_verify.sh ==="
echo "base-ref: $base_ref"
echo "HEAD: $(git rev-parse HEAD)"
echo "branch: $(git rev-parse --abbrev-ref HEAD)"
echo "go: $go_version"
echo "go.mod: $go_mod_line  GOTOOLCHAIN=$gotoolchain"
if pgrep -x wakil >/dev/null 2>&1; then
  echo "WARN: a wakil process is running — go-test and go-test-race can fail on the testutil guard (tests changed the real Wakil directories); stop it, or set WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1 for this run"
fi
echo "output: $outdir"
echo ""

# ------------------------------------------------------------------
# Pre-HOME checks: these need the user's real git config, so they
# run BEFORE HOME is redirected.
# ------------------------------------------------------------------

# --- 1. tracked tree clean (repo-local, no global config needed) ---
unstaged=$(git status --short 2>/dev/null | grep -v '^??' | wc -l)
if [ "$unstaged" -gt 0 ]; then
  echo "FAIL  tracked-tree-clean (unstaged: $unstaged)"
  fail=$((fail+1))
else
  echo "PASS  tracked-tree-clean"
  pass=$((pass+1))
fi

# --- 2. every commit signed (needs global git config for SSH sigs) ---
# git log --format='%G?' needs gpg.ssh.allowedSignersFile for SSH
# signature verification. If HOME is empty or the signing setup is
# missing, SKIP with a reason — never FAIL for a missing setup.
# Empty-range guard: `git log master..master` prints nothing, and
# `echo "" | grep -v ' G$'` counts one blank line as one unsigned
# commit. Skip empty lines so running on the base branch itself
# (0.8.4 fix-forward on master) reports PASS, not a phantom FAIL.
sig_errfile="$outdir/sig.err"
sig_stdout=$(git log --format='%h %G?' "$base_ref..HEAD" 2>"$sig_errfile")
sig_rc=$?
sig_err=$(cat "$sig_errfile" 2>/dev/null || true)
if echo "$sig_err" | grep -q 'allowedSignersFile\|gpg.ssh\|no signing key\|secret key needed'; then
  # git log ran but the signing setup is missing
  echo "SKIP  signed-commits (signing setup unavailable)"
  skip=$((skip+1))
elif [ $sig_rc -ne 0 ]; then
  echo "SKIP  signed-commits (git log failed: $(echo "$sig_err" | head -1))"
  skip=$((skip+1))
else
  unsigned=$(printf '%s\n' "$sig_stdout" | grep -v '^$' | grep -vc ' G$' || true)
  # grep -c returns 1 when no lines match; the logical count is then 0.
  if [ "$unsigned" -eq 0 ]; then
    echo "PASS  signed-commits"
    pass=$((pass+1))
  else
    echo "FAIL  signed-commits ($unsigned unsigned)"
    fail=$((fail+1))
  fi
fi

# ------------------------------------------------------------------
# Preserve Go caches, then redirect HOME for Wakil isolation.
# ------------------------------------------------------------------
GOMODCACHE_REAL=$(go env GOMODCACHE 2>/dev/null || true)
GOCACHE_REAL=$(go env GOCACHE 2>/dev/null || true)
GOPATH_REAL=$(go env GOPATH 2>/dev/null || true)
HOME_REAL="${HOME:-}"

tmproot="$(mktemp -d)"
cleanup() {
  chmod -R u+w "$tmproot" 2>/dev/null || true
  rm -rf "$tmproot" 2>/dev/null || true
  echo ""
  echo "=== summary ==="
  echo "PASS: $pass  FAIL: $fail  SKIP: $skip"
}
trap cleanup EXIT

export TMPROOT="$tmproot"
export HOME="$tmproot/home"
export XDG_CONFIG_HOME="$tmproot/xdg-config"
export XDG_DATA_HOME="$tmproot/xdg-data"
if [ -n "$GOMODCACHE_REAL" ]; then export GOMODCACHE="$GOMODCACHE_REAL"; fi
if [ -n "$GOCACHE_REAL" ];   then export GOCACHE="$GOCACHE_REAL"; fi
if [ -n "$GOPATH_REAL" ];    then export GOPATH="$GOPATH_REAL"; fi
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"

# ------------------------------------------------------------------
# Post-HOME checks: these run with HOME pointing at the temp dir.
# git commands here (git show, git ls-files) don't need the global
# signing config — they only read file contents.
# ------------------------------------------------------------------

# --- 3. build + version ---
check build-and-version sh -c 'mkdir -p "$TMPROOT/bin" && go build -o "$TMPROOT/bin/wakil" ./cmd/wakil/ && "$TMPROOT/bin/wakil" --version'

# --- 4. vet, gofmt, lint ---
# Exit-code honesty: a check must report the status of the command it
# runs, not of a trailing `| tail`/`| head` — those return their own
# (always-zero) status and masked real FAILs (e.g. golangci-lint `unused`
# findings in the TUI screen harness, 0.8.4 CI lint job). The checks
# below write to a log and propagate the runner's exit code, and print that
# log whole, so the log kept in the output directory is the full output;
# `check()` prints the summary of a failure from it (hv_summarize).
check vet sh -c 'go vet ./... >"$TMPROOT/vet.log" 2>&1; rc=$?; cat "$TMPROOT/vet.log"; exit $rc'
# .tmp*/ (scratch worktrees and notes, gitignored) is skipped explicitly, not
# only by being untracked: a force-added file there must not fail the check.
check gofmt sh -c 'out=$(gofmt -l $(git ls-files "*.go" | grep -v "^\.tmp") 2>&1); if [ -n "$out" ]; then echo "$out"; exit 1; fi; echo "gofmt clean"'
if command -v golangci-lint >/dev/null 2>&1; then
  check golangci-lint sh -c 'golangci-lint run >"$TMPROOT/golangci-lint.log" 2>&1; rc=$?; cat "$TMPROOT/golangci-lint.log"; exit $rc'
else
  echo "SKIP  golangci-lint (not installed)"
  skip=$((skip+1))
fi

# --- 4b. hisn floor ---
# The go-test steps use WAKIL_HISN_BIN when it is exported, else hisn on PATH;
# real-hisn always uses PATH. A binary below MinHisnVersion makes the session
# tests skip quietly (go-test) or the step print a bare SKIP (real-hisn), so
# say it here in one line per binary: found, required, where to get it. No
# hisn at all is a SKIP; nothing is installed and no binary runs beyond
# --version. The binary selection is unchanged.
hisn_floor_min=$(grep -o 'MinHisnVersion = "[^"]*"' internal/sandbox/backend.go 2>/dev/null | head -1 | sed 's/.*"\([^"]*\)".*/\1/')
if [ -n "$hisn_floor_min" ]; then
  check hisn-floor hv_hisn_floor_check "$hisn_floor_min"
else
  echo "SKIP  hisn-floor (MinHisnVersion not found in internal/sandbox/backend.go)"
  skip=$((skip+1))
fi

# --- 5. go test ---
check go-test sh -c 'go test -count=1 ./... >"$TMPROOT/go-test.log" 2>&1; rc=$?; cat "$TMPROOT/go-test.log"; exit $rc'

# --- 6. go test -race (skipped with --fast) ---
if [ "$fast" -eq 0 ]; then
  check go-test-race sh -c 'go test -race -count=1 -p 2 ./... >"$TMPROOT/go-test-race.log" 2>&1; rc=$?; cat "$TMPROOT/go-test-race.log"; exit $rc'
else
  echo "SKIP  go-test-race (--fast)"
  skip=$((skip+1))
fi

# --- 7. deadcode check ---
# deadcode needs golang.org/x/tools; probe first, SKIP if unavailable.
if [ -f scripts/check_deadcode.sh ]; then
  if go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./... >/dev/null 2>&1; then
    check deadcode sh -c 'bash scripts/check_deadcode.sh 2>&1'
  else
    echo "SKIP  deadcode (golang.org/x/tools not available)"
    skip=$((skip+1))
  fi
else
  echo "SKIP  deadcode (no script)"
  skip=$((skip+1))
fi

# --- 8. config-docs + version-pins ---
if [ -f scripts/check_config_docs.sh ]; then
  check config-docs sh -c 'bash scripts/check_config_docs.sh 2>&1'
else
  echo "SKIP  config-docs (no script)"
  skip=$((skip+1))
fi
if [ -f scripts/check_go_version_pins.sh ]; then
  check version-pins sh -c 'bash scripts/check_go_version_pins.sh 2>&1'
else
  echo "SKIP  version-pins (no script)"
  skip=$((skip+1))
fi

# --- 8b. bench harness: shellcheck + self-test ---
# The self-test runs against stub binaries only (no model, no sandbox, no
# network) and writes into a temp XDG_CACHE_HOME, never the user's cache.
if command -v shellcheck >/dev/null 2>&1; then
  check bench-shellcheck sh -c 'shellcheck -s sh scripts/bench.sh bench/tasks/*/*.sh bench/lib/*.sh'
else
  echo "SKIP  bench-shellcheck (shellcheck not installed)"
  skip=$((skip+1))
fi
check bench-selftest sh -c 'mkdir -p "$TMPROOT/bench-cache" && XDG_CACHE_HOME="$TMPROOT/bench-cache" sh scripts/bench.sh --self-test'

# --- 8c. the failure summary of this script ---
check host-verify-summary sh -c 'sh scripts/host_verify_summary_test.sh'
check host-verify-hisn sh -c 'sh scripts/host_verify_hisn_test.sh'

# --- 9. DefaultConfig diff ---
check default-config-diff sh -c '
  git show "$base_ref":internal/config/config.go > "$TMPROOT/master_config.go"
  cat internal/config/config.go > "$TMPROOT/head_config.go"
  master_cfg=$(sed -n "/^func DefaultConfig/,/^}/p" "$TMPROOT/master_config.go")
  head_cfg=$(sed -n "/^func DefaultConfig/,/^}/p" "$TMPROOT/head_config.go")
  if [ "$master_cfg" = "$head_cfg" ]; then
    echo "DefaultConfig: identical"
  else
    echo "$master_cfg" > "$TMPROOT/m.txt"
    echo "$head_cfg" > "$TMPROOT/h.txt"
    diff "$TMPROOT/m.txt" "$TMPROOT/h.txt"
    echo "FAIL: DefaultConfig differs"
    exit 1
  fi
'

# --- 10. help text ---
check help-text sh -c '
  cfg="$TMPROOT/help-config.json"
  printf "{\"endpoints\":{\"test\":{\"kind\":\"openrouter\",\"base_url\":\"http://10.9.8.7\",\"model\":\"test-model\"}},\"host\":\"10.9.8.7\",\"port\":11499,\"model\":\"test-model\"}" > "$cfg"
  out=$(WAKIL_CONFIG="$cfg" "$TMPROOT/bin/wakil" --help 2>&1)
  if echo "$out" | grep -q "10.9.8.7" || echo "$out" | grep -q "11499" || echo "$out" | grep -q "test-model"; then
    echo "FAIL: --help shows config values as defaults"
    exit 1
  fi
  echo "PASS: --help hides config values"
'

# --- 11. release notes ---
check release-notes sh -c '
  newest=$(grep -E "^## \[" CHANGELOG.md | head -2 | tail -1 | sed "s/.*\[\(.*\)\].*/\1/")
  if scripts/release_notes.sh "$newest" 2>&1 | grep -q "^#"; then
    echo "PASS: release_notes.sh $newest"
  else
    echo "FAIL: release_notes.sh $newest"
    exit 1
  fi
  if scripts/release_notes.sh 9.9.9 2>&1; then
    echo "FAIL: release_notes.sh 9.9.9 should exit non-zero"
    exit 1
  fi
  echo "PASS: release_notes.sh 9.9.9 exits non-zero"
'

# --- 12. e2e harness tests in cmd/wakil ---
check e2e-harness sh -c 'go test -count=1 -run "E2E" ./cmd/wakil/ >"$TMPROOT/e2e-harness.log" 2>&1; rc=$?; cat "$TMPROOT/e2e-harness.log"; exit $rc'

# --- 13. CHANGELOG unreleased heading ---
check changelog-heading sh -c '
  if grep -q "\[Unreleased\]" CHANGELOG.md; then
    grep -c "^## \[" CHANGELOG.md | head -1
    echo "PASS: [Unreleased] heading present"
  else
    echo "FAIL: [Unreleased] heading missing"
    exit 1
  fi
'

# --- 14. TUI benchmark regression (Part 2, 0.8.4) ---
# Runs the frame-cost regression test and the Part 1 smoke/soak in
# isolation (the full go-test above already covers them, but this gate
# names the cost so a failure points at the frame, not "go-test" in
# general). SKIP-partial: the soak is 98s; -run only targets the
# regression test + smoke, which together are ~4s.
#
# Exit-code honesty (Mashūra 0.8.4 review): the earlier form piped
# `go test ... | tail`, which returns tail's status, so a failing test
# printed PASS. Writing to a log and checking the command's exit status
# first is the portable fix: POSIX sh has no PIPESTATUS and `set -o
# pipefail` is not POSIX (dash exits 2 on it, which would kill this
# check). The rewritten form below has no pipe and no non-POSIX option.
if command -v go >/dev/null 2>&1; then
  # Frame-cost regression (incl. the resize limit), the deferred
  # resize re-wrap test, the per-frame allocation budget and the
  # benchmark smoke. Part C: resize limit + allocation budget are
  # asserted here, not just logged.
  check tui-perf sh -c 'go test -count=1 -run "TestFrameCostRegression|TestBenchHarnessSmoke|TestResizeDeferredRewrap|TestKeypressFrameAllocs" ./internal/tui/ >"$TMPROOT/tui-perf.log" 2>&1; rc=$?; cat "$TMPROOT/tui-perf.log"; exit $rc'
else
  echo "SKIP  tui-perf (go not available)"
  skip=$((skip+1))
fi

# --- 15. TUI screen tests on real output (Part 5, 0.8.4) ---
# The screen harness drives a real tea.Program over in-memory pipes;
# each test may SKIP with a reason when the FakeFacade cannot surface
# the event through Send (approval prompts, Mashūra panels). A SKIP is
# reported as SKIP here rather than FAIL, so the script stays honest
# about what cannot be driven in this environment.
#
# Same exit-code care as 14: go test's status is propagated; `go test`
# exit 77 = skip-all, which check() maps to SKIP with a reason.
if command -v go >/dev/null 2>&1; then
  # Screen tests on real output + the Part A raw-input selection
  # tests (SGR bytes through the real tea.Program): TestScreen,
  # TestSelection, TestOSC52Escape. Part C: the raw-input drag
  # regression tests run here, so a mid-drag jump fails this gate.
  check tui-screen sh -c 'go test -count=1 -run "TestScreen|TestSelection|TestOSC52Escape" ./internal/tui/ >"$TMPROOT/tui-screen.log" 2>&1; rc=$?; cat "$TMPROOT/tui-screen.log"; exit $rc'
else
  echo "SKIP  tui-screen (go not available)"
  skip=$((skip+1))
fi

# --- 16. Real-hisn integration tests (hisn 0.6.7) ---
# Runs the TestIntegration* suite against the real hisn on PATH, with
# the user's REAL HOME restored for that command (the tests read
# ~/.ssh/known_hosts, the host ssh agent, etc.). The check is:
#   SKIP  — hisn is missing or older than the minimum (MinHisnVersion);
#   FAIL  — any integration test fails;
#   FAIL  — every test skipped while hisn is new enough (a skip when
#           the host cannot run sessions is fine; a skip when the host
#           CAN but the test did not exercise it is a coverage gap).
# `go test` exit 77 (skip-all) is mapped to SKIP by check(). A real
# failure exits non-77, so it is FAIL. The "all skipped, hisn new
# enough" case is detected by grepping the log: if hisn >= MinHisnVersion
# but no test ran (only SKIP lines), that is a FAIL.

# version_ge lives in scripts/host_verify_hisn.sh (sourced at the top).

# Self-test for version_ge — runs before the real-hisn check so a broken
# comparison is caught before it hides a real hisn. Prints each case with the
# result it EXPECTS (true/false) and, on a mismatch, the result it got.
echo "=== version_ge self-test ==="
vt_pass=0; vt_fail=0
version_ge_test() {
  desc="$1"; in_a="$2"; in_b="$3"; expect="$4"
  # version_ge normalises into the globals a and b, so print the inputs from
  # their own copies.
  if version_ge "$in_a" "$in_b"; then got=0; else got=1; fi
  want=$([ "$expect" -eq 0 ] && echo true || echo false)
  if [ "$got" -eq "$expect" ]; then
    echo "  OK   $desc: version_ge $in_a $in_b → $want"
    vt_pass=$((vt_pass+1))
  else
    echo "  FAIL $desc: version_ge $in_a $in_b → $([ "$got" -eq 0 ] && echo true || echo false) (expected $want)"
    vt_fail=$((vt_fail+1))
  fi
}
version_ge_test "equal"           "0.6.0"  "0.6.0" 0
version_ge_test "lower patch"     "0.6.0"  "0.6.1" 1
version_ge_test "higher patch"    "0.6.1"  "0.6.0" 0
version_ge_test "lower minor"     "0.5.9"  "0.6.0" 1
version_ge_test "higher minor"    "0.7.0"  "0.6.0" 0
version_ge_test "0.10.0 vs 0.9.0" "0.10.0" "0.9.0" 0
version_ge_test "with suffix"     "0.6.0-dev1" "0.6.0" 0
version_ge_test "hisn prefix"     "hisn 0.6.0" "0.6.0" 0
version_ge_test "lower major"     "0.9.0"  "1.0.0" 1
echo "=== version_ge self-test: $vt_pass pass, $vt_fail fail ==="
if [ "$vt_fail" -gt 0 ]; then
  echo "FAIL  version-ge-self-test ($vt_fail cases failed)"
  fail=$((fail+1))
fi

if command -v go >/dev/null 2>&1 && command -v hisn >/dev/null 2>&1; then
  hisn_ver=$(hisn --version 2>&1 | head -1 || echo "unknown")
  # Read the minimum from the source (internal/sandbox.MinHisnVersion).
  min_ver=$(grep -o 'MinHisnVersion = "[^"]*"' internal/sandbox/backend.go 2>/dev/null | head -1 | sed 's/.*"\([^"]*\)".*/\1/')
  if [ -n "$min_ver" ] && version_ge "$hisn_ver" "$min_ver"; then
    # Run with the REAL HOME restored (the temp HOME has no ssh agent,
    # no known_hosts). WAKIL_HISN_BIN points at the real binary so the
    # test does not LookPath from the temp HOME.
    export WAKIL_HISN_BIN_BAK="${WAKIL_HISN_BIN:-}"
    export WAKIL_HISN_BIN="$(command -v hisn)"
    HOME_SAVE="$HOME"
    export HOME="$HOME_REAL"
    check real-hisn sh -c '
      go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/ \
        >"$TMPROOT/real-hisn.log" 2>&1
      rc=$?
      cat "$TMPROOT/real-hisn.log"
      if ! grep -q "^--- PASS\|^--- FAIL" "$TMPROOT/real-hisn.log"; then
        echo "FAIL: all integration tests skipped with hisn '"$hisn_ver"' (>= '"$min_ver"')"
        exit 1
      fi
      exit $rc
    '
    export HOME="$HOME_SAVE"
    export WAKIL_HISN_BIN="$WAKIL_HISN_BIN_BAK"
    unset WAKIL_HISN_BIN_BAK
  else
    echo "SKIP  real-hisn ($hisn_ver < ${min_ver:-unknown})"
    skip=$((skip+1))
  fi
else
  echo "SKIP  real-hisn (go or hisn not available)"
  skip=$((skip+1))
fi

# The EXIT trap (cleanup) prints the summary table.
# STEP 2 (0.8.4 release): exit non-zero when any check FAILs, so the
# script's exit status is honest. Previously this was an unconditional
# `exit 0`, which meant a FAIL did not fail the run.
exit "$fail"
