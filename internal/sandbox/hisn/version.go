package hisn

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/timeline"
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
//
// When sandbox.hisn.ssh_agent is true, SSH_AUTH_SOCK must be set and point at a
// socket — the host-side supervisor needs it (F4), and failing here (plan time)
// is earlier and clearer than exit 125 at the first tool call.
func (b *Backend) Preflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endV := timeline.Begin("hisn --version")
	out, err := b.output(ctx, false, "--version")
	endV()
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
	b.ver = got
	if f := b.opts.Surface.needs03(); len(f) > 0 && !b.profileMode() {
		return fmt.Errorf("sandbox backend hisn: %w", featureError(f, got))
	}
	if b.opts.Surface.Rootfs != "" {
		want032, _ := ParseVersion(ProfileHomeMinVersion)
		if !VersionAtLeast(got, want032) {
			return fmt.Errorf("sandbox backend hisn: sandbox.hisn.rootfs needs hisn >= %s (found %d.%d.%d): upgrade hisn or unset rootfs", ProfileHomeMinVersion, got[0], got[1], got[2])
		}
	}
	endCheck := timeline.Begin("hisn check")
	out, err = b.output(ctx, false, "check")
	endCheck()
	if err != nil {
		return fmt.Errorf("sandbox backend hisn: `hisn check` failed — this host cannot run hisn sessions:\n%s", tailLines(out, 12))
	}
	if b.opts.SSHAgent {
		if err := preflightSSHAgent(); err != nil {
			return err
		}
	}
	return nil
}

// preflightSSHAgent checks that SSH_AUTH_SOCK is set and points at a socket.
// The host-side hisn supervisor needs it (F4); failing here is earlier and
// clearer than exit 125 at the first tool call.
func preflightSSHAgent() error {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return fmt.Errorf("sandbox.hisn.ssh_agent: SSH_AUTH_SOCK is not set (ssh_agent = true requires a host agent; start one with `ssh-agent` and add a key with `ssh-add`)")
	}
	fi, err := os.Stat(sock)
	if err != nil {
		return fmt.Errorf("sandbox.hisn.ssh_agent: SSH_AUTH_SOCK %q: %w (ssh_agent = true requires a host agent socket)", sock, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("sandbox.hisn.ssh_agent: SSH_AUTH_SOCK %q is not a socket (ssh_agent = true requires a host agent socket)", sock)
	}
	return nil
}

// profileMode reports whether sessions start from a hisn-profile/v1 file
// (hisn >= ProfileMinVersion); older hisn gets the bare --policy document.
func (b *Backend) profileMode() bool {
	want, _ := ParseVersion(ProfileMinVersion)
	return VersionAtLeast(b.ver, want)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
