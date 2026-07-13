#!/bin/sh
# Wakil sandbox entrypoint.
#
# Starts kvr-server (staging KV store) in the background, then runs the main
# command (typically "sleep infinity"). On SIGTERM (from docker stop), signals
# kvr-server for graceful shutdown (which triggers a snapshot save if
# configured), waits for it to exit, then terminates the main command.
#
# No readiness loop here — readiness is checked host-side by Wakil's Go code
# which PINGs the mounted UDS socket path. See internal/exec/exec.go.

# Ensure the socket directory exists (on the staging mount or tmpfs).
SOCKET_DIR="$(dirname "${KVR_SOCKET_PATH:-/run/kvr/kvr.sock}")"
mkdir -p "$SOCKET_DIR"

# Remove stale socket file from a previous crash (kvr-server handles this too,
# but this avoids any edge case where the file exists but isn't a socket).
rm -f "${KVR_SOCKET_PATH:-/run/kvr/kvr.sock}" 2>/dev/null

# Start kvr-server in the background.
kvr-server &
KVR_PID=$!

# Start the main command in the background.
"$@" &
MAIN_PID=$!

# Cleanup: signal kvr for graceful shutdown, wait for it, then stop main.
cleanup() {
    kill -TERM "$KVR_PID" 2>/dev/null
    wait "$KVR_PID" 2>/dev/null
    kill -TERM "$MAIN_PID" 2>/dev/null
    wait "$MAIN_PID" 2>/dev/null
}

# On SIGTERM: run cleanup (kvr snapshot → stop main).
trap 'cleanup; exit 0' TERM

# Wait for the main command to exit (normal container lifecycle).
wait $MAIN_PID
EXIT_STATUS=$?

# On normal main exit, also gracefully stop kvr (snapshot save).
kill -TERM "$KVR_PID" 2>/dev/null
wait "$KVR_PID" 2>/dev/null

exit $EXIT_STATUS
