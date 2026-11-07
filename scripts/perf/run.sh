#!/usr/bin/env bash
# Measure the lifecycle of Wakil — start, /new, /resume, exit — with the phase
# timeline (WAKIL_TRACE=1) and print a markdown summary.
#
#   scripts/perf/run.sh [-n RUNS] [-b WAKIL_BINARY] REPO_DIR...
#
# Everything the run writes lives under $PERF_DIR (default /tmp/wakil-perf): a
# private XDG data/config tree and a private HOME, so your real sessions,
# memory and config are never read or written. The model endpoint is a local
# stub (fake_provider.py) that answers instantly, so provider latency stays out
# of the numbers; the first run of each repo is reported as "cold" (new stores,
# new hisn home) and the rest as "warm".
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

for repo in "$@"; do
  repo=$(cd "$repo" && pwd)
  name=$(basename "$repo")
  ws="$PERF_DIR/$name"
  rm -rf "$ws"; mkdir -p "$ws/data" "$ws/cfg" "$ws/home"
  if [ -n "${ROOTFS:-}" ] && [ -d "$REAL_HOME/.local/share/hisn" ]; then
    ln -s "$REAL_HOME/.local/share/hisn" "$ws/data/hisn"
  fi
  cat > "$ws/cfg/wakil.json" <<JSON
{"endpoints":{"local":{"kind":"openai","base_url":"http://127.0.0.1:$PORT","model":"fake-model"}},
 "default_endpoint":"local","sandbox":$sandbox}
JSON
  echo "== $name: $RUNS runs (run 1 is cold)" >&2
  for i in $(seq 1 "$RUNS"); do
    # A chat turn (so a transcript exists), /new, /resume of that transcript,
    # /new again, then quit. STEP_WAIT lets each transition settle.
    ( cd "$repo" && \
      XDG_DATA_HOME="$ws/data" XDG_CONFIG_HOME="$ws/cfg" HOME="$ws/home" \
      WAKIL_CONFIG="$ws/cfg/wakil.json" WAKIL_TRACE=1 STEP_WAIT=4 \
      timeout 180 python3 "$here/drive.py" 'hello\r' '/new\r' '@resume' '/new\r' -- \
        "$BIN" --workdir "$repo" >"$ws/run$i.log" 2>&1 ) || echo "run $i exited non-zero (see $ws/run$i.log)" >&2
    # Start each run's chat list afresh so "@resume" always has exactly the
    # session this run created, while the stores stay warm.
    rm -f "$ws"/data/wakil/sessions/*.json
  done
done

python3 "$here/summarize.py" "$PERF_DIR" "$@"
