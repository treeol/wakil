package exec

import (
	"strings"
	"testing"
)

// TestDockerURI_RelativePath verifies that a relative path (as the model
// passes) is resolved against hostMount and translated correctly.
func TestDockerURI_RelativePath(t *testing.T) {
	d := &DockerExecutor{
		workspaceRoot: "/mnt/wakil",
		hostMount:     "/work/wakil",
	}

	uri, err := d.HostPathToURI("internal/agent/app.go")
	if err != nil {
		t.Fatalf("HostPathToURI with relative path: %v", err)
	}
	if !strings.HasPrefix(uri, "file:///mnt/wakil/") {
		t.Errorf("URI = %q, expected container path prefix file:///mnt/wakil/", uri)
	}
	if strings.Contains(uri, "/work/wakil") {
		t.Errorf("HOST PATH LEAK: URI %q contains host mount path", uri)
	}
	if !strings.HasSuffix(uri, "internal/agent/app.go") {
		t.Errorf("URI = %q, expected to end with internal/agent/app.go", uri)
	}
}

// TestDockerURI_Translation verifies the host→container→host round trip.
func TestDockerURI_Translation(t *testing.T) {
	d := &DockerExecutor{
		workspaceRoot: "/mnt/wakil",
		hostMount:     "/work/wakil",
	}

	// Host path → container URI
	uri, err := d.HostPathToURI("/work/wakil/internal/agent/app.go")
	if err != nil {
		t.Fatalf("HostPathToURI: %v", err)
	}
	if !strings.HasPrefix(uri, "file:///mnt/wakil/") {
		t.Errorf("URI = %q, expected container path prefix file:///mnt/wakil/", uri)
	}
	// The LEAK SENTINEL: the host path must NOT appear in the URI.
	if strings.Contains(uri, "/work/wakil") {
		t.Errorf("HOST PATH LEAK: URI %q contains the host mount path — gopls would silently return empty results", uri)
	}

	// Container URI → host path (round trip)
	hostPath, err := d.URIToHostPath(uri)
	if err != nil {
		t.Fatalf("URIToHostPath: %v", err)
	}
	if hostPath != "/work/wakil/internal/agent/app.go" {
		t.Errorf("round trip = %q, want original host path", hostPath)
	}
}

// TestDockerURI_HostPathLeakRejected verifies that a path outside the host
// mount is rejected (the leak guard).
func TestDockerURI_HostPathLeakRejected(t *testing.T) {
	d := &DockerExecutor{
		workspaceRoot: "/mnt/wakil",
		hostMount:     "/work/wakil",
	}

	// A path outside the host mount should error.
	_, err := d.HostPathToURI("/etc/passwd")
	if err == nil {
		t.Error("expected error for path outside host mount, got nil — host-path leak not guarded")
	}
}

// TestDockerURI_GOROOTPathRejected verifies that a URI outside workspaceRoot
// (e.g. a GOROOT path from go-to-definition on fmt.Println) returns an error,
// not a silent mapping.
func TestDockerURI_GOROOTPathRejected(t *testing.T) {
	d := &DockerExecutor{
		workspaceRoot: "/mnt/wakil",
		hostMount:     "/work/wakil",
	}

	// gopls returns GOROOT paths like file:///usr/local/go/src/fmt/print.go
	_, err := d.URIToHostPath("file:///usr/local/go/src/fmt/print.go")
	if err == nil {
		t.Error("expected error for GOROOT path (outside workspace root), got nil — silent mapping is the trap")
	}
}

// TestDirectURI_RelativePath verifies relative path resolution in direct mode.
func TestDirectURI_RelativePath(t *testing.T) {
	e := &DirectExecutor{root: "/work/wakil"}

	uri, err := e.HostPathToURI("internal/agent/app.go")
	if err != nil {
		t.Fatalf("HostPathToURI with relative path: %v", err)
	}
	if uri != "file:///work/wakil/internal/agent/app.go" {
		t.Errorf("URI = %q, want file:///work/wakil/internal/agent/app.go", uri)
	}
}

// TestDirectURI_Identity verifies direct mode is identity.
func TestDirectURI_Identity(t *testing.T) {
	e := &DirectExecutor{root: "/work/wakil"}

	uri, err := e.HostPathToURI("/work/wakil/main.go")
	if err != nil {
		t.Fatalf("HostPathToURI: %v", err)
	}
	if uri != "file:///work/wakil/main.go" {
		t.Errorf("URI = %q, want file:///work/wakil/main.go", uri)
	}

	hostPath, err := e.URIToHostPath(uri)
	if err != nil {
		t.Fatalf("URIToHostPath: %v", err)
	}
	if hostPath != "/work/wakil/main.go" {
		t.Errorf("round trip = %q, want original", hostPath)
	}
}
