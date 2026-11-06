package wiring

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// newHisnExecutor builds the executor for sandbox.backend "hisn". Every
// failure is returned: there is no fallback to docker and never an unsandboxed
// executor. The session itself starts lazily, once a chat is bound.
// rootfsFor returns the session rootfs: the project's store name wins over
// sandbox.hisn.rootfs; "" = host toolchain. Only the global config may name a
// path (the loader rejects it in the project file).
func rootfsFor(global string, s hisn.Surface) string {
	if s.Rootfs != "" {
		return s.Rootfs
	}
	return global
}

func newHisnExecutor(cfg config.Config) (exec.Executor, error) {
	// Defence in depth for hand-built configs (LoadConfig already resolves
	// the backend and forces browser_enabled off under hisn).
	if cfg.ExecMode != "docker" {
		return nil, fmt.Errorf("sandbox backend hisn cannot be combined with exec mode %q", cfg.ExecMode)
	}
	if cfg.BrowserEnabled {
		return nil, fmt.Errorf("browser_enabled is not supported with sandbox backend hisn")
	}
	h := cfg.Sandbox.Hisn
	state := hisnStateDir()
	if state == "" {
		return nil, fmt.Errorf("sandbox backend hisn: cannot determine the Wakil state directory")
	}
	home := h.HomeRoot
	if home == "" {
		// 0.5.0 (D1/D5): keyed by the project's store key, one per repository.
		// Old hisn-home/<64-hex> dirs are left in place and no longer used.
		key := agent.StoreKey(agent.ProjectDir(cfg))
		if key == "" {
			return nil, fmt.Errorf("sandbox backend hisn: cannot derive a project key for home")
		}
		home = filepath.Join(state, "hisn-home", key)
	}
	surface, _, err := loadProjectSurface(cfg)
	if err != nil {
		return nil, err
	}
	gitName, gitEmail, signingKey, sshKeys, err := hisnGitSetup(cfg, h)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hisn.SetDiagnostics(os.Stderr)
	b, err := hisn.Open(ctx, hisn.Options{
		Binary:      h.Binary,
		StateDir:    filepath.Join(state, "hisn"),
		HomeRoot:    home,
		Home:        h.Home,
		Rootfs:      rootfsFor(h.Rootfs, surface),
		SSHAgent:    h.SSHAgent,
		SSHKeys:     sshKeys,
		GitName:     gitName,
		GitEmail:    gitEmail,
		SigningKey:  signingKey,
		KnownHosts:  knownHostsPath(h.SSHAgent),
		HostCwd:     cfg.HostWorkDir,
		GrantableRW: h.GrantableRW,
		EnvPass:     h.EnvPass,
		TmpSizeMB:   h.TmpSizeMB,
		ShmSizeMB:   h.ShmSizeMB,
		ExecViaCLI:  h.ExecViaCLI,
		Surface:     surface,
		KVR:         kvrOptions(cfg, h),
	})
	if err != nil {
		return nil, err
	}
	return exec.NewBackendExecutor(b, exec.BackendOptions{
		Label:       "hisn",
		GrantableRW: h.GrantableRW,
	}), nil
}

// hisnStateDir is Wakil's per-user state root (same resolution as the other
// durable stores: $XDG_DATA_HOME/wakil, else ~/.local/share/wakil).
func hisnStateDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil")
}

// kvrOptions derives the in-sandbox staging service settings: enabled unless
// kvr_disabled, binary from sandbox.hisn.kvr_binary or PATH (it must run
// inside the sandbox; the backend checks that and says why when not).
func kvrOptions(cfg config.Config, h config.HisnConfig) hisn.KVROptions {
	o := hisn.KVROptions{Enabled: !cfg.KVRDisabled, MaxEntries: cfg.KVRMaxEntries,
		SweepIntervalSecs: cfg.KVRSweepIntervalSecs, SnapshotSecs: cfg.KVRSnapshotIntervalSecs}
	if !o.Enabled {
		return o
	}
	switch {
	case h.KVRBinary != "":
		o.Binary = h.KVRBinary
	default:
		if p, err := osexec.LookPath("kvr-server"); err == nil {
			o.Binary = p
		}
	}
	return o
}

// hisnGitSetup resolves the session git identity and SSH signing (P3) on the
// host. Identity: sandbox.hisn.git_identity_* override, else the repo-scoped
// host user.name / user.email. Signing: ssh_signing through exec.DetectSigning,
// which reads only the PUBLIC key. A signing setup that cannot be resolved is an
// error under hisn: it is never skipped silently (the docker path logs a skip).
func hisnGitSetup(cfg config.Config, h config.HisnConfig) (name, email, signingKey string, sshKeys []string, err error) {
	sshKeys = append([]string(nil), h.SSHKeys...)
	if !h.SSHAgent && (cfg.SSHSigning != "" && cfg.SSHSigning != "off") {
		return "", "", "", nil, fmt.Errorf("ssh_signing needs sandbox.hisn.ssh_agent = true under hisn")
	}
	name, email = h.GitIdentityName, h.GitIdentityEmail
	if name == "" || email == "" {
		hn, he := exec.HostIdentity(cfg.HostWorkDir)
		if name == "" {
			name = hn
		}
		if email == "" {
			email = he
		}
	}
	if cfg.SSHSigning == "" || cfg.SSHSigning == "off" {
		return name, email, "", sshKeys, nil
	}
	// Trusted scope only: a repo-local signing key is writable from the sandbox
	// and would otherwise widen the host agent allowlist (review B1).
	setup, skip := exec.DetectSigningTrusted(cfg.SSHSigning, cfg.HostWorkDir)
	if skip != "" || !setup.Enabled {
		return "", "", "", nil, fmt.Errorf("ssh_signing under hisn cannot be enabled: %s", skip)
	}
	if email == "" {
		return "", "", "", nil, fmt.Errorf("ssh_signing needs a git identity email (sandbox.hisn.git_identity_email or host user.email)")
	}
	// The signing key's fingerprint joins the agent allowlist, so the session
	// agent can sign with it. An explicit ssh_keys list is kept as well.
	fp, err := hisn.PublicKeyLineFingerprint(setup.PublicKey)
	if err != nil {
		return "", "", "", nil, err
	}
	if !containsStr(sshKeys, fp) {
		sshKeys = append(sshKeys, fp)
	}
	return name, email, setup.PublicKey, sshKeys, nil
}

// knownHostsPath is the host ~/.ssh/known_hosts, mounted read-only only when
// the agent (an ssh transport) is on. "" = none.
func knownHostsPath(agent bool) string {
	if !agent {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
