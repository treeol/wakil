package exec

import (
	"strings"
	"testing"
)

// TestDockerHardeningArgs_AllFlagsPresent verifies that dockerHardeningArgs
// (the production function used by NewDockerExecutor) includes every mandatory
// hardening flag when configured with defaults.
func TestDockerHardeningArgs_AllFlagsPresent(t *testing.T) {
	opts := DockerOpts{
		DockerMemory:    "2g",
		DockerPidsLimit: 512,
		DockerCaps:      []string{"CHOWN"},
	}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")

	required := []string{
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp:rw,nosuid,nodev,size=100m",
		"--memory=2g",
		"--pids-limit=512",
		"--cap-add=CHOWN",
	}
	for _, flag := range required {
		if !strings.Contains(joined, flag) {
			t.Errorf("hardening flag missing from dockerHardeningArgs output: %q\ngot: %s", flag, joined)
		}
	}
}

// TestDockerHardeningArgs_NoCapsReAdded verifies that when DockerCaps is empty,
// no --cap-add flags are present (strictest configuration).
func TestDockerHardeningArgs_NoCapsReAdded(t *testing.T) {
	opts := DockerOpts{
		DockerMemory:    "2g",
		DockerPidsLimit: 512,
		DockerCaps:      nil,
	}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--cap-add") {
		t.Errorf("expected no --cap-add flags when DockerCaps is empty, got: %s", joined)
	}
}

// TestDockerHardeningArgs_NoResourceLimits verifies that when memory/pids are
// zero/empty, those flags are omitted.
func TestDockerHardeningArgs_NoResourceLimits(t *testing.T) {
	opts := DockerOpts{
		DockerMemory:    "",
		DockerPidsLimit: 0,
	}
	args := dockerHardeningArgs(opts)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--memory") {
		t.Errorf("expected no --memory flag when DockerMemory is empty, got: %s", joined)
	}
	if strings.Contains(joined, "--pids-limit") {
		t.Errorf("expected no --pids-limit flag when DockerPidsLimit is 0, got: %s", joined)
	}
}

// TestDockerHardeningArgs_CoreFlagsAlwaysPresent verifies that the four core
// hardening flags are present even with zero-value DockerOpts (no config).
func TestDockerHardeningArgs_CoreFlagsAlwaysPresent(t *testing.T) {
	args := dockerHardeningArgs(DockerOpts{})
	joined := strings.Join(args, " ")
	core := []string{
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp:rw,nosuid,nodev,size=100m",
		"--tmpfs=/etc:rw,nosuid,nodev,size=1m",
	}
	for _, flag := range core {
		if !strings.Contains(joined, flag) {
			t.Errorf("core hardening flag missing with zero-value opts: %q\ngot: %s", flag, joined)
		}
	}
}
