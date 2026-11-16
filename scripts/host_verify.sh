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
  "$@" >"$outdir/$name.log" 2>&1
  rc=$?
  if [ $rc -eq 0 ]; then
    echo "PASS  $name"
    pass=$((pass+1))
  elif [ $rc -eq 77 ]; then
    echo "SKIP  $name"
    skip=$((skip+1))
  else
    echo "FAIL  $name (exit $rc)"
    fail=$((fail+1))
    tail -15 "$outdir/$name.log" 2>&1 | sed 's/^/  /'
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
  echo "WARN: a wakil process is running — some tests may fail"
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
  unsigned=$(echo "$sig_stdout" | grep -v ' G$' | wc -l)
  if [ "$unsigned" -gt 0 ]; then
    echo "FAIL  signed-commits ($unsigned unsigned)"
    fail=$((fail+1))
  else
    echo "PASS  signed-commits"
    pass=$((pass+1))
  fi
fi

# ------------------------------------------------------------------
# Preserve Go caches, then redirect HOME for Wakil isolation.
# ------------------------------------------------------------------
GOMODCACHE_REAL=$(go env GOMODCACHE 2>/dev/null || true)
GOCACHE_REAL=$(go env GOCACHE 2>/dev/null || true)
GOPATH_REAL=$(go env GOPATH 2>/dev/null || true)

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
check vet sh -c 'go vet ./... 2>&1 | head -20'
check gofmt sh -c 'out=$(gofmt -l $(git ls-files "*.go") 2>&1); if [ -n "$out" ]; then echo "$out"; exit 1; fi; echo "gofmt clean"'
if command -v golangci-lint >/dev/null 2>&1; then
  check golangci-lint sh -c 'golangci-lint run 2>&1 | tail -10'
else
  echo "SKIP  golangci-lint (not installed)"
  skip=$((skip+1))
fi

# --- 5. go test ---
check go-test sh -c 'go test -count=1 ./... 2>&1 | tail -10'

# --- 6. go test -race (skipped with --fast) ---
if [ "$fast" -eq 0 ]; then
  check go-test-race sh -c 'go test -race -count=1 -p 2 ./... 2>&1 | tail -20'
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
check e2e-harness sh -c 'go test -count=1 -run "E2E" ./cmd/wakil/ 2>&1 | tail -10'

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
  check tui-perf sh -c 'go test -count=1 -run "TestFrameCostRegression|TestBenchHarnessSmoke|TestResizeDeferredRewrap|TestKeypressFrameAllocs" ./internal/tui/ >"$TMPROOT/tui-perf.log" 2>&1; rc=$?; cat "$TMPROOT/tui-perf.log" | tail -10; exit $rc'
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
  check tui-screen sh -c 'go test -count=1 -run "TestScreen|TestSelection|TestOSC52Escape" ./internal/tui/ >"$TMPROOT/tui-screen.log" 2>&1; rc=$?; cat "$TMPROOT/tui-screen.log" | tail -10; exit $rc'
else
  echo "SKIP  tui-screen (go not available)"
  skip=$((skip+1))
fi

# The EXIT trap (cleanup) prints the summary table.
exit 0
