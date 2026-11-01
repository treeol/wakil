package main

import (
	"fmt"
	"io"
	"runtime"

	"github.com/treeol/wakil/internal/buildinfo"
	"github.com/treeol/wakil/internal/config"
)

// printVersion writes the `wakil version` report: version, commit, build
// date, the sandbox image this binary is paired with, and the Go version.
func printVersion(w io.Writer) {
	img := buildinfo.SandboxImage
	if img == "" {
		img = config.DefaultSandboxImage() + " (unpinned dev build)"
	}
	fmt.Fprintf(w, "wakil %s\n", buildinfo.ResolvedVersion())
	fmt.Fprintf(w, "commit:        %s\n", buildinfo.Commit)
	fmt.Fprintf(w, "date:          %s\n", buildinfo.Date)
	fmt.Fprintf(w, "sandbox image: %s\n", img)
	fmt.Fprintf(w, "go:            %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
