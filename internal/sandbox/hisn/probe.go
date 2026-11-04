package hisn

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Probe asks whether the hisn binary can be the DEFAULT backend: it is found,
// is at least DefaultMinVersion and `hisn check` exits 0. It is read-only (no
// session, no state) and bounded. reason is "" exactly when hisn is usable;
// otherwise it is a short sentence for the startup banner. binary "" means
// "hisn" in PATH.
func Probe(ctx context.Context, binary, defaultMin string) (version, reason string) {
	if binary == "" {
		binary = "hisn"
	}
	// Reuse the one command factory (scrubbed environment) through a bare
	// Backend: nothing else of the backend is touched.
	b := &Backend{opts: Options{Binary: binary}}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := b.output(ctx, false, "--version")
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") || strings.Contains(err.Error(), "no such file") {
			return "", binary + " not found in PATH"
		}
		return "", fmt.Sprintf("%s --version failed: %v", binary, err)
	}
	got, err := ParseVersion(out)
	if err != nil {
		return "", fmt.Sprintf("cannot parse %s --version output", binary)
	}
	version = fmt.Sprintf("%d.%d.%d", got[0], got[1], got[2])
	want, err := ParseVersion(defaultMin)
	if err != nil {
		return version, "internal: bad minimum version " + defaultMin
	}
	if !VersionAtLeast(got, want) {
		return version, fmt.Sprintf("hisn %s is older than %s", version, defaultMin)
	}
	if cout, err := b.output(ctx, false, "check"); err != nil {
		return version, "hisn check failed: " + firstLine(tailLines(cout, 12), "this host cannot run hisn sessions")
	}
	return version, ""
}

// firstLine returns the first FAIL line of a `hisn check` report when there
// is one, else the first non-empty line, else def.
func firstLine(report, def string) string {
	var first string
	for _, l := range strings.Split(report, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.Contains(l, "FAIL") {
			return l
		}
		if first == "" {
			first = l
		}
	}
	if first != "" {
		return first
	}
	return def
}
