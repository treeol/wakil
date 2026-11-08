package wiring

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"github.com/treeol/wakil/internal/timeline"
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

// hisnOptions builds the hisn.Options for cfg. Shared by the executor and the
// trust command so the risk shown at trust time is the risk startup computes.
// It reads the project file (loadProjectSurface) and so enforces F1 too.
func hisnOptions(cfg config.Config) (hisn.Options, error) {
	defer timeline.Begin("hisn options (project file + signing + identity)")()
	surface, _, err := loadProjectSurface(cfg)
	if err != nil {
		return hisn.Options{}, err
	}
	return hisnOptionsWith(cfg, surface)
}

// hisnOptionsWith is hisnOptions with the project surface supplied by the
// caller. Only `wakil sandbox trust` uses it, to review an unapproved file
// (trust IS the approval step); startup always goes through hisnOptions.
func hisnOptionsWith(cfg config.Config, surface hisn.Surface) (hisn.Options, error) {
	h := cfg.Sandbox.Hisn
	state := hisnStateDir()
	if state == "" {
		return hisn.Options{}, fmt.Errorf("sandbox backend hisn: cannot determine the Wakil state directory")
	}
	home := h.HomeRoot
	if home == "" {
		// 0.5.0 (D1/D5): keyed by the project's store key, one per repository.
		// Old hisn-home/<64-hex> dirs are left in place and no longer used.
		endKey := timeline.Accum("project dir + store key (EvalSymlinks, sha256)")
		key := agent.StoreKey(agent.ProjectDir(cfg))
		endKey()
		if key == "" {
			return hisn.Options{}, fmt.Errorf("sandbox backend hisn: cannot derive a project key for home")
		}
		home = filepath.Join(state, "hisn-home", key)
	}
	endGit := timeline.Begin("signing resolution (git config, ssh key, agent)")
	gitName, gitEmail, signingKey, sshKeys, err := hisnGitSetup(cfg, h)
	endGit()
	if err != nil {
		return hisn.Options{}, err
	}
	return hisn.Options{
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
		ProjectKey:  agent.StoreKey(agent.ProjectDir(cfg)),
		GrantableRW: h.GrantableRW,
		EnvPass:     h.EnvPass,
		TmpSizeMB:   h.TmpSizeMB,
		ShmSizeMB:   h.ShmSizeMB,
		ExecViaCLI:  h.ExecViaCLI,
		Surface:     surface,
		KVR:         kvrOptions(cfg, h),
	}, nil
}

func newHisnExecutor(cfg config.Config) (exec.Executor, error) {
	// Defence in depth for hand-built configs (LoadConfig already resolves
	// the backend).
	if cfg.ExecMode != "docker" {
		return nil, fmt.Errorf("sandbox backend hisn cannot be combined with exec mode %q", cfg.ExecMode)
	}
	opts, err := hisnOptions(cfg)
	if err != nil {
		return nil, err
	}
	signingKey := opts.SigningKey
	allowedSignersPath := ""
	if signingKey != "" {
		allowedSignersPath = filepath.Join(hisn.SandboxHome, ".wakil-allowed-signers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hisn.SetDiagnostics(os.Stderr)
	// Reconcile is deferred: Open skips it, and the returned closure runs it
	// after the first prompt (the ~608ms `hisn ls --prune` round trip is off
	// the start path). Preflight, plan and the risk check still run here.
	opts.SkipReconcile = true
	endOpen := timeline.Begin("hisn.Open (preflight, plan)")
	b, err := hisn.Open(ctx, opts)
	endOpen()
	if err != nil {
		return nil, err
	}
	h := cfg.Sandbox.Hisn
	be := exec.NewBackendExecutor(b, exec.BackendOptions{
		Label:              "hisn",
		GrantableRW:        h.GrantableRW,
		SigningEnforced:    signingKey != "",
		SigningKey:         signingKey,
		AllowedSignersPath: allowedSignersPath,
	})
	// Stash the deferred reconcile on the executor so the caller can run it
	// after the first prompt. Best-effort; errors go to stderr (the wiring
	// layer's errWriter is the hisn package's, not reachable here).
	be.SetDeferredReconcile(func(rctx context.Context) {
		end := timeline.Begin("hisn reconcile (ls --prune, orphans)")
		err := b.Reconcile(rctx)
		end()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hisn: orphan reconcile: %v\n", err)
		}
	})
	return be, nil
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
// host, at plan time. Identity: sandbox.hisn.git_identity_* override, else the
// repo-scoped host user.name / user.email.
//
// Signing (0.5.1, per-repo): ssh_signing="off" forces signing off. Otherwise
// the REPO's effective git config decides — read on the host with the repo as
// cwd, so a repo-local .git/config wins over the user's global config, exactly
// as a host commit would. commit.gpgsign != true → signing off, nothing
// injected. commit.gpgsign = true → the repo's user.signingkey is resolved on
// the host (key:: literal, .pub path, or private path + .pub sibling) and the
// plan FAILS if the key cannot be resolved or the host agent cannot hold it:
// never a silent unsigned start.
//
// ssh_keys (sandbox.hisn.ssh_keys) is the agent-forwarding allowlist ONLY —
// which keys the session agent exposes. It does not gate signing: the resolved
// signing key's fingerprint is appended to the session allowlist so the
// session agent can sign with it. The sandbox cannot change the signing
// choice: the key is injected at COMMAND scope (see BackendExecutor.signingEnv)
// and the private key stays in the host agent.
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
	// Per-repo resolution, on the host, before any session exists. An explicit
	// key (ssh_signing = "<path to .pub>") overrides the repo's
	// user.signingkey; "auto" reads the repo's own config. Reject obvious
	// non-path values (true/on/1…) with a message naming the valid forms —
	// they would otherwise become bogus "<value>.pub" reads.
	explicit := ""
	if cfg.SSHSigning != "auto" {
		if !strings.ContainsAny(cfg.SSHSigning, "/~.") {
			return "", "", "", nil, fmt.Errorf("ssh_signing: unrecognized value %q (want off | auto | path to a .pub file)", cfg.SSHSigning)
		}
		explicit = cfg.SSHSigning
	}
	rs, err := exec.ResolveRepoSigning(cfg.HostWorkDir, explicit)
	if err != nil {
		return "", "", "", nil, err
	}
	if !rs.Signing {
		return name, email, "", sshKeys, nil
	}
	if !h.SSHAgent {
		return "", "", "", nil, fmt.Errorf("repo %s: commit.gpgsign is true — signing needs sandbox.hisn.ssh_agent = true (the session signs through the host agent)", cfg.HostWorkDir)
	}
	if email == "" {
		return "", "", "", nil, fmt.Errorf("ssh_signing needs a git identity email (sandbox.hisn.git_identity_email or host user.email)")
	}
	fp, err := hisn.PublicKeyLineFingerprint(rs.PublicKey)
	if err != nil {
		return "", "", "", nil, err
	}
	// The host agent must hold the key: signing goes through the agent
	// proxy, and a private key never enters the sandbox. Load it once when
	// the private path is known; a key the agent still does not hold fails
	// the plan (never a silent unsigned start).
	if !exec.AgentHoldsFingerprint(fp) {
		if rs.PrivateKeyPath != "" {
			if err := exec.AgentAddKey(rs.PrivateKeyPath); err != nil {
				return "", "", "", nil, fmt.Errorf("repo %s: signing is required (commit.gpgsign=true) with key %q (fingerprint %s), but it could not be loaded into the host ssh agent: %w", cfg.HostWorkDir, rs.SigningKeyValue, fp, err)
			}
		}
		if !exec.AgentHoldsFingerprint(fp) {
			return "", "", "", nil, fmt.Errorf("repo %s: signing is required (commit.gpgsign=true) with key %q (fingerprint %s), but the host ssh agent does not hold it — load it with `ssh-add` (the session starts signed or not at all)", cfg.HostWorkDir, rs.SigningKeyValue, fp)
		}
	}
	// Append the fingerprint to the SESSION forwarding allowlist (not gated by
	// ssh_keys — 0.5.1): the session agent must be able to see the signing key.
	if !containsStr(sshKeys, fp) {
		sshKeys = append(sshKeys, fp)
	}
	return name, email, rs.PublicKey, sshKeys, nil
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
