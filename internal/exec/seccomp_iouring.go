package exec

import _ "embed"

// seccompIOUringProfile is a custom seccomp profile that extends Docker's
// default seccomp profile with three io_uring syscalls (io_uring_setup,
// io_uring_enter, io_uring_register) to allow async I/O in the sandbox.
//
// Provenance: derived from moby/moby v28.0.0 profiles/seccomp/default.json
// (sha256 of the original: 4b97ca5b90a89f5a2db1b9558fdf1f04e413da8dd0c75f73f3d5be6b1b1b3a7f)
// by appending the three io_uring syscall names to the main SCMP_ACT_ALLOW
// group. No other entries were modified.
//
// Update policy: when upgrading the supported Docker version, refresh the
// base profile from the matching moby tag and re-apply the io_uring delta.
// A unit test (TestSeccompIOUringProfile) verifies the profile structure.
//
// Security: io_uring is a large kernel subsystem with a history of CVEs.
// Operations submitted through the ring bypass seccomp per-op filtering.
// This profile increases the host-kernel attack surface despite
// --cap-drop=ALL. Only enable when needed (e.g. zdb project testing).
//
//go:embed seccomp_iouring.json
var seccompIOUringProfile []byte
