// Package buildinfo carries build-time metadata injected via -ldflags -X by
// the release pipeline (.goreleaser.yaml). All variables have safe dev
// defaults so plain `go build` / `go install` binaries still work.
package buildinfo

import "runtime/debug"

// Set at link time:
//
//	-X github.com/treeol/wakil/internal/buildinfo.Version=...
//	-X github.com/treeol/wakil/internal/buildinfo.Commit=...
//	-X github.com/treeol/wakil/internal/buildinfo.Date=...
//	-X github.com/treeol/wakil/internal/buildinfo.SandboxImage=ghcr.io/treeol/wakil-sandbox@sha256:...
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
	// SandboxImage is the digest-pinned sandbox image this binary was
	// released with. Empty in non-release (dev) builds.
	SandboxImage = ""
)

// DevSandboxImage is the unpinned local-build image name used only when
// SandboxImage is empty (non-release builds).
const DevSandboxImage = "wakil-dev"

// ResolvedVersion returns Version, falling back to the module version that
// `go install module@version` embeds when no ldflags were given.
func ResolvedVersion() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}
