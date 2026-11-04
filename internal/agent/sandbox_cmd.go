package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox"
)

// renderSandboxState renders the /sandbox command output. The tool layer sees
// only the optional exec.SandboxStatuser capability, never the backend.
func (a *App) renderSandboxState() string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, ok := sandboxStatus(ctx, a.Exec)
	if !ok {
		return "sandbox: " + a.Exec.Describe()
	}
	return FormatSandboxStatus(st)
}

func sandboxStatus(ctx context.Context, e exec.Executor) (sandbox.Status, bool) {
	if s, ok := e.(exec.SandboxStatuser); ok {
		return s.SandboxStatus(ctx), true
	}
	return sandbox.Status{}, false
}

// FormatSandboxStatus renders a Status for /sandbox.
func FormatSandboxStatus(st sandbox.Status) string {
	var b strings.Builder
	b.WriteString(st.Banner())
	if st.Mode != "" {
		fmt.Fprintf(&b, "\n  mode: %s", st.Mode)
	}
	if st.Profile != "" {
		fmt.Fprintf(&b, "\n  profile: %s (hisn profile, used as is)", st.Profile)
	}
	if st.ProfilePath != "" {
		fmt.Fprintf(&b, "\n  profile file: %s", st.ProfilePath)
	}
	if st.Reason != "" {
		fmt.Fprintf(&b, "\n  note: %s", st.Reason)
	}
	for _, c := range st.Capabilities {
		fmt.Fprintf(&b, "\n  capability: %s [%s]", c.Capability, c.Label)
	}
	for _, s := range st.Services {
		fmt.Fprintf(&b, "\n  service: %s (%s)%s", s.Name, orDash(s.State), sockSuffix(s.Socket))
	}
	for _, f := range st.Forwards {
		fmt.Fprintf(&b, "\n  forward: %s", f)
	}
	for _, f := range st.HostServices {
		fmt.Fprintf(&b, "\n  host service: %s", f)
	}
	if len(st.EnvSet) > 0 {
		fmt.Fprintf(&b, "\n  env set: %s", strings.Join(st.EnvSet, ", "))
	}
	for _, g := range st.Grants {
		mode := "ro"
		if g.RW {
			mode = "rw"
		}
		fmt.Fprintf(&b, "\n  grant: %s (%s)", g.Path, mode)
	}
	if st.KVR != "" {
		fmt.Fprintf(&b, "\n  staging (kvr): %s", st.KVR)
	}
	for _, n := range st.Notes {
		fmt.Fprintf(&b, "\n  hisn note: %s", n)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sockSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}
