package hisn

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// ParseVersion extracts major.minor.patch from `hisn --version` output.
func ParseVersion(s string) ([3]int, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, fmt.Errorf("cannot parse hisn version from %q", strings.TrimSpace(s))
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		n, _ := strconv.Atoi(m[i+1])
		v[i] = n
	}
	return v, nil
}

// VersionAtLeast reports whether got >= want.
func VersionAtLeast(got, want [3]int) bool {
	for i := 0; i < 3; i++ {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// Preflight verifies that the hisn binary exists, is at least MinVersion and
// that the host can run sandboxes (`hisn check` exits 0). Any failure is an
// error: there is no fallback to another backend or to running unsandboxed.
func (b *Backend) Preflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := b.output(ctx, false, "--version")
	if err != nil {
		return fmt.Errorf("sandbox backend hisn: cannot run %q --version: %w (install hisn >= %s or set sandbox.backend to docker)", b.opts.Binary, err, b.opts.MinVersion)
	}
	got, err := ParseVersion(out)
	if err != nil {
		return fmt.Errorf("sandbox backend hisn: %w", err)
	}
	want, err := ParseVersion(b.opts.MinVersion)
	if err != nil {
		return err
	}
	if !VersionAtLeast(got, want) {
		return fmt.Errorf("sandbox backend hisn: hisn %d.%d.%d is too old, need >= %s", got[0], got[1], got[2], b.opts.MinVersion)
	}
	if out, err := b.output(ctx, false, "check"); err != nil {
		return fmt.Errorf("sandbox backend hisn: `hisn check` failed — this host cannot run hisn sessions:\n%s", tailLines(out, 12))
	}
	return nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
