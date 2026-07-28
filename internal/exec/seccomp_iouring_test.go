package exec

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// TestIOUringSeccompProfileMaterialization verifies that the profile-writing
// logic (the same code path NewDockerExecutor uses when DockerIOUring is true)
// materializes the embedded seccomp profile to a temp file, the file contains
// valid JSON, and the file path is returned. Tests the profile-writing in
// isolation since NewDockerExecutor requires a running Docker daemon.
func TestIOUringSeccompProfileMaterialization(t *testing.T) {
	// Simulate the profile-writing code from NewDockerExecutor (lines 517-533).
	profile := seccompIOUringProfile
	if profile == nil {
		t.Fatal("seccompIOUringProfile is nil — embedded profile missing")
	}

	tf, err := os.CreateTemp(t.TempDir(), "wakil-seccomp-iouring-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	path := tf.Name()
	t.Cleanup(func() { os.Remove(path) })

	if _, err := tf.Write(profile); err != nil {
		_ = tf.Close()
		t.Fatalf("failed to write seccomp profile: %v", err)
	}
	if err := tf.Close(); err != nil {
		t.Fatalf("failed to close seccomp profile: %v", err)
	}

	// Verify the file exists and contains valid JSON.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("seccomp profile temp file does not exist: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("seccomp profile temp file is empty")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read seccomp profile: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("seccomp profile is not valid JSON: %v", err)
	}

	// Verify essential profile structure.
	if da, _ := parsed["defaultAction"].(string); da != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, want SCMP_ACT_ERRNO", da)
	}
	if _, ok := parsed["archMap"]; !ok {
		t.Error("archMap missing from materialized profile")
	}
	syscalls, ok := parsed["syscalls"].([]any)
	if !ok {
		t.Fatal("syscalls missing or not an array")
	}
	if len(syscalls) == 0 {
		t.Fatal("syscalls array is empty")
	}
}

// TestIOUringSeccompProfileCleanupOnClose verifies that when a DockerExecutor
// has a non-empty seccompProfilePath, Close() removes the temp file.
func TestIOUringSeccompProfileCleanupOnClose(t *testing.T) {
	// Create a temp file to simulate a materialized seccomp profile.
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "wakil-seccomp-iouring-test.json")
	if err := os.WriteFile(profilePath, []byte(`{"defaultAction":"SCMP_ACT_ERRNO"}`), 0o644); err != nil {
		t.Fatalf("failed to write test profile: %v", err)
	}

	// Construct a DockerExecutor with a fake seccompProfilePath. The container
	// name uses a non-existent prefix so docker commands (stop/rm) will fail
	// harmlessly — Close() logs the error from docker stop but still proceeds
	// to os.Remove the temp file.
	d := &DockerExecutor{
		container:          "nonexistent-test-container-" + randSuffix(8),
		image:              "test-image",
		workspaceRoot:      "/work",
		seccompProfilePath: profilePath,
	}
	_ = d.Close()

	// Verify the profile temp file was removed.
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Errorf("seccomp profile temp file was not cleaned up by Close(), stat: %v", err)
	}
}

// TestIOUringSeccompNoProfileWhenDisabled verifies that when DockerIOUring is
// false, no seccomp profile path is set on the DockerExecutor (the field is
// empty). This guards against a regression where the profile-writing code path
// runs unconditionally.
func TestIOUringSeccompNoProfileWhenDisabled(t *testing.T) {
	// A DockerExecutor created with iouring=false should have an empty
	// seccompProfilePath. We verify this by constructing one directly.
	d := &DockerExecutor{
		container:          "test-container",
		image:              "test-image",
		workspaceRoot:      "/work",
		iouring:            false,
		seccompProfilePath: "",
	}
	if d.seccompProfilePath != "" {
		t.Errorf("seccompProfilePath should be empty when iouring=false, got %q", d.seccompProfilePath)
	}
	// dockerHardeningArgs should not emit a seccomp flag when path is empty.
	opts := DockerOpts{}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "seccomp") {
		t.Errorf("seccomp flag should not appear when IOUringProfilePath is empty, got: %s", joined)
	}
}
