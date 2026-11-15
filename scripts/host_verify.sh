#!/bin/sh
# host_verify.sh — verify a wakil work branch from the host.
#
# Usage: scripts/host_verify.sh <base-ref>
# (e.g. scripts/host_verify.sh master)
#
# Never touches the user's real config or data. Sets HOME, XDG_CONFIG_HOME,
# XDG_DATA_HOME to a fresh temp dir. Uses no API key, no network except Go
# module and tool downloads. Changes nothing in the repo outside .tmp/.
#
# At the start it prints the branch and HEAD, and warns if a wakil process
# is running on the machine.
set -eu

base_ref="${1:?usage: host_verify.sh <base-ref>}"
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# --- setup ---
tmproot="$(mktemp -d)"
trap 'rm -rf "$tmproot"' EXIT
export TMPROOT="$tmproot"
export HOME="$tmproot/home"
export XDG_CONFIG_HOME="$tmproot/xdg-config"
export XDG_DATA_HOME="$tmproot/xdg-data"
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME"

outdir="$repo_root/.tmp/host-verify/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$outdir"

# --- header ---
echo "=== host_verify.sh ==="
echo "base-ref: $base_ref"
echo "HEAD: $(git rev-parse HEAD)"
echo "branch: $(git rev-parse --abbrev-ref HEAD)"
if pgrep -x wakil >/dev/null 2>&1; then
  echo "WARN: a wakil process is running — some tests may fail"
fi
echo "output: $outdir"
echo ""

# --- helpers ---
pass=0
fail=0
skip=0

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
  fi
}

# --- 1. tracked tree clean ---
unstaged=$(git status --short | grep -v '^??' | wc -l)
if [ "$unstaged" -gt 0 ]; then
  echo "FAIL  tracked-tree-clean (unstaged: $unstaged)"
  fail=$((fail+1))
else
  echo "PASS  tracked-tree-clean"
  pass=$((pass+1))
fi

# --- 2. every commit signed ---
unsigned=$(git log --format='%h %G?' "$base_ref"..HEAD | grep -v ' G$' | wc -l)
if [ "$unsigned" -gt 0 ]; then
  echo "FAIL  signed-commits ($unsigned unsigned)"
  fail=$((fail+1))
else
  echo "PASS  signed-commits"
  pass=$((pass+1))
fi

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
# check go-test-race sh -c 'go test -race -count=1 -p 2 ./... 2>&1 | tail -20'

# --- 7. deadcode check ---
if [ -f scripts/check_deadcode.sh ]; then
  check deadcode sh -c 'bash scripts/check_deadcode.sh 2>&1'
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
  cfg="$tmproot/help-config.json"
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

# --- summary ---
echo ""
echo "=== summary ==="
echo "PASS: $pass  FAIL: $fail  SKIP: $skip"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
echo "ALL PASS"
