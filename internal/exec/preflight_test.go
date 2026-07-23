package exec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDockerPreflight_SucceedsWithDocker verifies that dockerPreflight passes
// when Docker CLI and daemon are available. Skips if they're not.
func TestDockerPreflight_SucceedsWithDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not available: %v: %s", err, strings.TrimSpace(string(out)))
	}

	if err := dockerPreflight(); err != nil {
		t.Errorf("dockerPreflight with Docker available: unexpected error: %v", err)
	}
}

// TestDockerPreflight_FakeSuccess verifies that dockerPreflight returns nil
// when the fake docker binary exits 0 (simulating a healthy daemon).
func TestDockerPreflight_FakeSuccess(t *testing.T) {
	fakeDir := t.TempDir()
	fakeDocker := filepath.Join(fakeDir, "docker")
	const fakeScript = `#!/bin/sh
exit 0
`
	if err := os.WriteFile(fakeDocker, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	if err := dockerPreflight(); err != nil {
		t.Errorf("dockerPreflight with fake healthy daemon: unexpected error: %v", err)
	}
}

// TestDockerPreflight_NoDockerBinary verifies that dockerPreflight returns a
// clear "docker not found" error when the docker binary is not on PATH.
func TestDockerPreflight_NoDockerBinary(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	err := dockerPreflight()
	if err == nil {
		t.Fatal("expected error when docker binary is not on PATH")
	}
	if !strings.Contains(err.Error(), "docker not found") {
		t.Errorf("error should mention 'docker not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "--exec direct") {
		t.Errorf("error should suggest --exec direct, got: %v", err)
	}
}

// TestCheckDockerImage_MissingImage verifies that checkDockerImage returns a
// clear error with build instructions for a non-existent image.
func TestCheckDockerImage_MissingImage(t *testing.T) {
	requireDocker(t) // needs docker CLI + daemon

	// Use an image name that definitely doesn't exist locally.
	missingImage := "wakil-test-nonexistent-image-do-not-build"

	err := checkDockerImage(missingImage)
	if err == nil {
		t.Fatal("expected error for missing image")
	}
	if !strings.Contains(err.Error(), "not found locally") {
		t.Errorf("error should mention 'not found locally', got: %v", err)
	}
	if !strings.Contains(err.Error(), missingImage) {
		t.Errorf("error should name the missing image %q, got: %v", missingImage, err)
	}
	if !strings.Contains(err.Error(), "docker build") {
		t.Errorf("error should suggest 'docker build', got: %v", err)
	}
	if !strings.Contains(err.Error(), "--exec direct") {
		t.Errorf("error should suggest --exec direct, got: %v", err)
	}
}

// TestCheckDockerImage_RegistryImageSuggestsPull verifies that for images
// containing "/" (registry references), the error message suggests pull
// instead of build.
func TestCheckDockerImage_RegistryImageSuggestsPull(t *testing.T) {
	requireDocker(t)

	missingImage := "ghcr.io/test/nonexistent:v1"

	err := checkDockerImage(missingImage)
	if err == nil {
		t.Fatal("expected error for missing registry image")
	}
	if !strings.Contains(err.Error(), "not found locally") {
		t.Errorf("error should mention 'not found locally', got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker pull") {
		t.Errorf("error should suggest 'docker pull' for registry images, got: %v", err)
	}
	if strings.Contains(err.Error(), "docker build") {
		t.Errorf("error should NOT suggest 'docker build' for registry images, got: %v", err)
	}
}

// TestCheckDockerImage_ExistingImage verifies that checkDockerImage passes
// when the image exists locally. Uses the wakil-dev image if available.
func TestCheckDockerImage_ExistingImage(t *testing.T) {
	requireDocker(t)

	// Check if the wakil-dev image exists. If not, skip.
	if err := exec.Command("docker", "image", "inspect", "wakil-dev:latest").Run(); err != nil {
		t.Skip("wakil-dev image not available")
	}

	if err := checkDockerImage("wakil-dev:latest"); err != nil {
		t.Errorf("checkDockerImage with existing image: unexpected error: %v", err)
	}
}

// TestDockerPreflight_FakeDaemonNotRunning verifies the daemon-not-running
// detection by placing a fake "docker" script on PATH that simulates the
// daemon-down error.
func TestDockerPreflight_FakeDaemonNotRunning(t *testing.T) {
	fakeDir := t.TempDir()
	fakeDocker := filepath.Join(fakeDir, "docker")
	const fakeScript = `#!/bin/sh
echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
exit 1
`
	if err := os.WriteFile(fakeDocker, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	err := dockerPreflight()
	if err == nil {
		t.Fatal("expected error for daemon not running")
	}
	if !strings.Contains(err.Error(), "daemon is not running") {
		t.Errorf("error should mention 'daemon is not running', got: %v", err)
	}
	if !strings.Contains(err.Error(), "--exec direct") {
		t.Errorf("error should suggest --exec direct, got: %v", err)
	}
}

// TestDockerPreflight_FakePermissionDenied verifies the permission-denied
// detection by placing a fake "docker" script that simulates the
// permission error.
func TestDockerPreflight_FakePermissionDenied(t *testing.T) {
	fakeDir := t.TempDir()
	fakeDocker := filepath.Join(fakeDir, "docker")
	const fakeScript = `#!/bin/sh
echo "docker: Got permission denied while trying to connect to the Docker daemon socket"
exit 1
`
	if err := os.WriteFile(fakeDocker, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	err := dockerPreflight()
	if err == nil {
		t.Fatal("expected error for permission denied")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should mention 'permission denied', got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker group") {
		t.Errorf("error should suggest adding user to docker group, got: %v", err)
	}
}

// TestDockerPreflight_FakeGenericError verifies the generic fallback message
// for an unrecognized docker info failure.
func TestDockerPreflight_FakeGenericError(t *testing.T) {
	fakeDir := t.TempDir()
	fakeDocker := filepath.Join(fakeDir, "docker")
	const fakeScript = `#!/bin/sh
echo "some weird docker error"
exit 1
`
	if err := os.WriteFile(fakeDocker, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir)

	err := dockerPreflight()
	if err == nil {
		t.Fatal("expected error for generic docker info failure")
	}
	if !strings.Contains(err.Error(), "docker info failed") {
		t.Errorf("error should mention 'docker info failed', got: %v", err)
	}
}
