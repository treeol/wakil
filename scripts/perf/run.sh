#!/usr/bin/env bash
# Measure the lifecycle of Wakil — start, /new, /resume, exit — with the phase
# timeline (WAKIL_TRACE=1) and print a markdown summary.
#
#   scripts/perf/run.sh [-n RUNS] [-b WAKIL_BINARY] REPO_DIR...
#
# Everything the run WRITES lives under $PERF_DIR (default /tmp/wakil-perf): a
# private XDG data/config tree and a private HOME, so your real sessions,
# memory and config are never written. The model endpoint is a local stub
# (fake_provider.py) that answers instantly, so provider latency stays out of
# the numbers; the first run of each repo is reported as "cold" (new stores,
# new hisn home) and the rest as "warm".
#
# The run READS one directory from your real environment, read-only: the hisn
# project approvals ($XDG_DATA_HOME/wakil/hisn/approved). A repo's
# .wakil/sandbox.json approval lives there, and a private tree has none — so
# startup would refuse the repo with "has not been approved" and no timeline
# would be written. Only the `approved/` subdirectory is linked into the private
# tree; hisn's StateDir is the parent, where hisn writes its own runtime state
# (owner records, sessions, lock), so it MUST stay private — a symlink does not
# enforce read-only, and linking the whole dir would let hisn write into the real
# tree. The repo's own .wakil/sandbox.json is read by wakil itself, unchanged.
#
# If a run produces no timeline, run.sh says why (approval missing, non-zero
# exit, or a logline the driver printed) instead of silently reporting nothing.
#
# Environment:
#   HISN_BIN   hisn executable (default: hisn from PATH)
#   ROOTFS     hisn rootfs store name to run in (default: host toolchain); the
#              store is linked from $HOME/.local/share/hisn into the private tree
#   PERF_DIR   work directory (default /tmp/wakil-perf)
#   PORT       stub provider port (default 18765)
set -euo pipefail

RUNS=5
BIN=""
while getopts "n:b:" o; do
  case $o in n) RUNS=$OPTARG ;; b) BIN=$OPTARG ;; *) exit 2 ;; esac
done
shift $((OPTIND - 1))
[ $# -ge 1 ] || { echo "usage: $0 [-n RUNS] [-b WAKIL_BINARY] REPO_DIR..." >&2; exit 2; }

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
PERF_DIR=${PERF_DIR:-/tmp/wakil-perf}
PORT=${PORT:-18765}
HISN_BIN=${HISN_BIN:-hisn}
REAL_HOME=$HOME
REAL_XDG_DATA=${XDG_DATA_HOME:-$HOME/.local/share}
mkdir -p "$PERF_DIR"

if [ -z "$BIN" ]; then
  BIN="$PERF_DIR/wakil"
  (cd "$root" && go build -o "$BIN" ./cmd/wakil)
fi

python3 "$here/fake_provider.py" "$PORT" >/dev/null 2>&1 &
stub=$!
trap 'kill $stub 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do curl -s -m 1 "http://127.0.0.1:$PORT/props" >/dev/null && break; sleep 0.1; done

rootfs_json=""
if [ -n "${ROOTFS:-}" ]; then rootfs_json=",\"rootfs\":\"$ROOTFS\""; fi
sandbox="{\"backend\":\"hisn\",\"hisn\":{\"binary\":\"$HISN_BIN\"$rootfs_json}}"

# link_approved_readonly: make the REAL hisn trust records visible read-only at
#   $ws/data/wakil/hisn/approved
# while leaving StateDir itself a real, writable, private directory. Only the
# project approvals live under `approved/`; hisn's own runtime state (owner
# records, sessions, lock, profile TOMLs) shares the parent StateDir and MUST
# be private — symlinking the whole state dir would let hisn write its runtime
# state into the real tree. A symlink does not enforce read-only, so we link
# ONLY the approvals subdirectory, which wakil reads (never writes) at startup.
link_approved_readonly() {
  local real_hisn=$1 dst_hisn=$2
  local src="$real_hisn/approved"
  [ -d "$src" ] || return 0
  mkdir -p "$dst_hisn"
  rm -rf "$dst_hisn/approved"
  ln -s "$src" "$dst_hisn/approved"
}

# count_timelines WS: number of timeline files a workspace has so far. Tolerates
# the no-match case (set -o pipefail would otherwise abort the script).
count_timelines() {
  local n
  n=$(find "$1/data/wakil/traces" -name 'timeline-*.json' 2>/dev/null | wc -l)
  echo "${n:-0}"
}

for repo in "$@"; do
  repo=$(cd "$repo" && pwd)
  name=$(basename "$repo")
  ws="$PERF_DIR/$name"
  rm -rf "$ws"; mkdir -p "$ws/data" "$ws/cfg" "$ws/home"
  # The real hisn project approvals (for .wakil/sandbox.json) — linked as the
  # `approved/` subdirectory only; the state dir itself stays private so hisn's
  # runtime writes never touch the real tree.
  link_approved_readonly "$REAL_XDG_DATA/wakil/hisn" "$ws/data/wakil/hisn"
  if [ -n "${ROOTFS:-}" ] && [ -d "$REAL_HOME/.local/share/hisn" ]; then
    ln -s "$REAL_HOME/.local/share/hisn" "$ws/data/hisn"
  fi
  cat > "$ws/cfg/wakil.json" <<JSON
{"endpoints":{"local":{"kind":"openai","base_url":"http://127.0.0.1:$PORT","model":"fake-model"}},
 "default_endpoint":"local","sandbox":$sandbox}
JSON
  if [ ! -e "$ws/data/wakil/hisn/approved" ] && [ -f "$repo/.wakil/sandbox.json" ]; then
    echo "== $name: note — $repo has .wakil/sandbox.json but no hisn approvals were found at $REAL_XDG_DATA/wakil/hisn/approved;" >&2
    echo "   startup will refuse it with 'has not been approved' unless the repo needs no approval. Run 'wakil sandbox trust' in it first." >&2
  fi
  echo "== $name: $RUNS runs (run 1 is cold)" >&2
  for i in $(seq 1 "$RUNS"); do
    # A chat turn (so a transcript exists), /new, /resume of that transcript,
    # /new again, then quit. STEP_WAIT lets each transition settle.
    before=$(count_timelines "$ws")
    ( cd "$repo" && \
      XDG_DATA_HOME="$ws/data" XDG_CONFIG_HOME="$ws/cfg" HOME="$ws/home" \
      WAKIL_CONFIG="$ws/cfg/wakil.json" WAKIL_TRACE=1 STEP_WAIT=4 \
      timeout 180 python3 "$here/drive.py" 'hello\r' '/new\r' '@resume' '/new\r' -- \
        "$BIN" --workdir "$repo" >"$ws/run$i.log" 2>&1 ) || echo "run $i exited non-zero (see $ws/run$i.log)" >&2
    after=$(count_timelines "$ws")
    if [ "$after" -le "$before" ]; then
      echo "!! run $i produced NO timeline — the run did not reach the traced exit." >&2
      # Name the most likely cause from the log rather than leaving it silent.
      if grep -qa "has not been approved" "$ws/run$i.log"; then
        echo "   cause: the repo's .wakil/sandbox.json is not approved. Run 'wakil sandbox trust' in $repo," >&2
        echo "   or point the script at the tree that holds the approval ($REAL_XDG_DATA/wakil/hisn)." >&2
      elif grep -qa "prompt-seen: False" "$ws/run$i.log"; then
        echo "   cause: the TUI never reached its prompt (startup refused or aborted before the first frame)." >&2
        echo "   If $repo has a .wakil/sandbox.json, it must be approved for this workspace — run 'wakil sandbox trust' there." >&2
        echo "   Last lines of $ws/run$i.log:" >&2
        tail -3 "$ws/run$i.log" >&2
      elif grep -qa "startup aborted\|startup error" "$ws/run$i.log"; then
        echo "   cause: startup aborted — see the lines below." >&2
        grep -a "startup aborted\|startup error" "$ws/run$i.log" | tail -3 >&2
      else
        echo "   cause: unknown — last lines of $ws/run$i.log follow." >&2
        tail -5 "$ws/run$i.log" >&2
      fi
    fi
    # Start each run's chat list afresh so "@resume" always has exactly the
    # session this run created, while the stores stay warm.
    rm -f "$ws"/data/wakil/sessions/*.json
  done
done

python3 "$here/summarize.py" "$PERF_DIR" "$@"
