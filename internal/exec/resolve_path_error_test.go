package exec

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestResolvePathErrorKeepsCommandOutput pins the normal case: readlink
// reported a cause on stdout, which is the message we want to surface.
func TestResolvePathErrorKeepsCommandOutput(t *testing.T) {
	err := resolvePathError("/tmp/foo.go", "readlink: No such file or directory\n", fmt.Errorf("exit status 1"))
	if !strings.Contains(err.Error(), "readlink: No such file or directory") {
		t.Fatalf("command output lost: %v", err)
	}
}

// TestResolvePathErrorFallsBackToExecError is the regression test for the
// dangling-colon bug: when readlink produced no output, the message ended in
// "resolving path %q: " with nothing after it. The exec error must be used.
func TestResolvePathErrorFallsBackToExecError(t *testing.T) {
	err := resolvePathError("/tmp/foo.go", "", fmt.Errorf("exit status 127"))
	got := err.Error()
	if strings.HasSuffix(got, ": ") || strings.HasSuffix(got, ":") {
		t.Fatalf("message still ends in a dangling colon: %q", got)
	}
	if !strings.Contains(got, "exit status 127") {
		t.Fatalf("exec error not surfaced: %q", got)
	}
}

// TestResolvePathErrorBlankOutputAndNoErr covers the degenerate case.
func TestResolvePathErrorBlankOutputAndNoErr(t *testing.T) {
	err := resolvePathError("/tmp/foo.go", "   \n", nil)
	if !strings.Contains(err.Error(), "no output from readlink") {
		t.Fatalf("unexpected message: %q", err.Error())
	}
}

// TestResolvePathErrorPreservesPath confirms the offending path stays quoted in
// the message — the confinement breaker parses it back out.
func TestResolvePathErrorPreservesPath(t *testing.T) {
	err := resolvePathError("/mnt/other/foo.go", "", fmt.Errorf("boom"))
	if !strings.Contains(err.Error(), `"/mnt/other/foo.go"`) {
		t.Fatalf("quoted path missing: %q", err.Error())
	}
}

// TestResolvePathErrorWrapsExecError pins the %w contract: the underlying error
// must stay unwrappable so callers can errors.Is/errors.As through the chain.
func TestResolvePathErrorWrapsExecError(t *testing.T) {
	sentinel := errors.New("readlink failed")
	err := resolvePathError("/tmp/foo.go", "   \n", sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error chain broken — sentinel not recoverable: %v", err)
	}
}

// TestResolvePathErrorHandlesPathWithSpaces checks the quoted path survives
// verbatim — the confinement breaker extracts it from this text.
func TestResolvePathErrorHandlesPathWithSpaces(t *testing.T) {
	err := resolvePathError("/tmp/my dir/a b.go", "", errors.New("boom"))
	if !strings.Contains(err.Error(), `"/tmp/my dir/a b.go"`) {
		t.Fatalf("spaced path not preserved: %q", err.Error())
	}
}
