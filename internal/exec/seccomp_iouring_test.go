package exec

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSeccompIOUringProfile verifies the embedded seccomp profile structure
// and the io_uring delta against the pinned moby default.
func TestSeccompIOUringProfile(t *testing.T) {
	var profile map[string]any
	if err := json.Unmarshal(seccompIOUringProfile, &profile); err != nil {
		t.Fatalf("embedded seccomp profile is not valid JSON: %v", err)
	}

	// defaultAction must be SCMP_ACT_ERRNO (deny by default), not ALLOW.
	if da, _ := profile["defaultAction"].(string); da != "SCMP_ACT_ERRNO" {
		t.Fatalf("defaultAction = %q, want SCMP_ACT_ERRNO (must not be SCMP_ACT_ALLOW)", da)
	}

	// defaultErrnoRet must be present (Docker uses ENOSYS=38 or errno 1;
	// the moby default uses 1).
	if _, ok := profile["defaultErrnoRet"]; !ok {
		t.Error("defaultErrnoRet missing — glibc feature probes may break")
	}

	// archMap must be present (architecture-aware filtering).
	if _, ok := profile["archMap"]; !ok {
		t.Error("archMap missing — profile silently breaks non-amd64 architectures")
	}

	// Extract all unconditionally allowed syscall names (groups with no
	// includes.caps gate). Capability-gated allows (bpf→CAP_BPF,
	// mount→CAP_SYS_ADMIN, reboot→CAP_SYS_BOOT) are fine — under
	// --cap-drop=ALL they won't actually be allowed.
	unconditionalAllowed := make(map[string]bool)
	if syscalls, ok := profile["syscalls"].([]any); ok {
		for _, sc := range syscalls {
			if entry, ok := sc.(map[string]any); ok {
				action, _ := entry["action"].(string)
				if action != "SCMP_ACT_ALLOW" {
					continue
				}
				includes, has := entry["includes"].(map[string]any)
				if has {
					if _, hasCaps := includes["caps"]; hasCaps {
						continue // capability-gated, skip
					}
				}
				if names, ok := entry["names"].([]any); ok {
					for _, n := range names {
						if name, ok := n.(string); ok {
							unconditionalAllowed[name] = true
						}
					}
				}
			}
		}
	}

	// The three io_uring syscalls must be unconditionally allowed.
	for _, syscall := range []string{"io_uring_setup", "io_uring_enter", "io_uring_register"} {
		if !unconditionalAllowed[syscall] {
			t.Errorf("io_uring syscall %q not found in unconditional allowed set", syscall)
		}
	}

	// Baseline-denied syscalls must NOT be unconditionally allowed.
	for _, syscall := range []string{"keyctl", "bpf", "mount", "pivot_root", "reboot"} {
		if unconditionalAllowed[syscall] {
			t.Errorf("baseline-denied syscall %q is unconditionally allowed — profile is too permissive", syscall)
		}
	}
}

// TestDockerHardeningArgs_IOUringDisabled verifies that with DockerIOUring
// false (and IOUringProfilePath empty), no seccomp flag is emitted and the
// output is unchanged from the pre-io_uring baseline.
func TestDockerHardeningArgs_IOUringDisabled(t *testing.T) {
	opts := DockerOpts{
		DockerMemory:    "4g",
		DockerPidsLimit: 512,
	}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "seccomp") {
		t.Errorf("seccomp flag should not appear when IOUringProfilePath is empty, got: %s", joined)
	}
}

// TestDockerHardeningArgs_IOUringEnabled verifies that when IOUringProfilePath
// is set, exactly one --security-opt=seccomp=<path> flag is emitted, and all
// other hardening flags remain present.
func TestDockerHardeningArgs_IOUringEnabled(t *testing.T) {
	opts := DockerOpts{
		DockerMemory:       "4g",
		DockerPidsLimit:    512,
		IOUringProfilePath: "/tmp/wakil-seccomp-test.json",
	}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")

	// seccomp flag must be present.
	seccompFlag := "--security-opt=seccomp=/tmp/wakil-seccomp-test.json"
	if !strings.Contains(joined, seccompFlag) {
		t.Errorf("seccomp flag missing, got: %s", joined)
	}

	// Exactly one seccomp flag.
	count := strings.Count(joined, "--security-opt=seccomp=")
	if count != 1 {
		t.Errorf("expected exactly 1 seccomp flag, got %d: %s", count, joined)
	}

	// All core hardening flags must still be present.
	core := []string{
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp:rw,nosuid,nodev,size=4g",
		"--tmpfs=/etc:rw,nosuid,nodev,size=1m",
	}
	for _, flag := range core {
		if !strings.Contains(joined, flag) {
			t.Errorf("core hardening flag missing with io_uring enabled: %q\ngot: %s", flag, joined)
		}
	}
}
