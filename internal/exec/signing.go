package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SigningSetup is resolved on the HOST before the sandbox container starts and
// consumed by NewDockerExecutor. It carries everything needed to make git
// SSH-sign commits inside the container without the private key ever entering
// it: the host ssh-agent socket (bind-mounted) and the literal public key
// (injected via GIT_CONFIG_* env, "key::" form — see git-config(1)).
type SigningSetup struct {
	Enabled         bool
	AgentSock       string // resolved host SSH_AUTH_SOCK path
	PublicKey       string // literal public key, e.g. "ssh-ed25519 AAAA… comment"
	AutoSign        bool   // host-effective commit.gpgsign for the workspace repo
	UserName        string // host-effective user.name (empty = don't inject)
	UserEmail       string // host-effective user.email (empty = don't inject)
	SigningKeyValue string // raw user.signingkey value (for error messages); "" when unset
}

// DetectSigning resolves the SSH-signing setup from the host git config.
//
// mode is the ssh_signing config value: "off" (or ""), "auto", or an explicit
// path to a public key file. repoDir is the host workspace directory; git
// config is read with -C repoDir so repo-local values win over global ones,
// mirroring exactly what a commit on the host would do.
//
// Returns a disabled SigningSetup plus a human-readable skip reason when
// signing cannot or should not be enabled. Never returns an error: signing is
// best-effort and must not block startup.
func DetectSigning(mode, repoDir string) (SigningSetup, string) {
	return detectSigning(mode, repoDir)
}

// detectSigning resolves the SSH-signing setup from the host git config.
func detectSigning(mode, repoDir string) (SigningSetup, string) {
	get := func(key string) string { return gitConfigGet(repoDir, key) }
	switch mode {
	case "", "off":
		return SigningSetup{}, "" // feature off; no reason to log
	case "auto":
		// fall through to detection below
	default:
		// Explicit public-key path. Catch obvious non-path values (true/on/1…)
		// early with a message that names the valid forms.
		if !strings.ContainsAny(mode, "/~.") {
			return SigningSetup{}, fmt.Sprintf("ssh_signing: unrecognized value %q (want off | auto | path to a .pub file)", mode)
		}
		return detectWithExplicitKey(mode, repoDir)
	}

	// gpg.format must be ssh — GPG/x509 passthrough is out of scope.
	format := get("gpg.format")
	if format != "ssh" {
		if format == "" {
			return SigningSetup{}, "ssh_signing=auto: host git has no gpg.format configured"
		}
		return SigningSetup{}, fmt.Sprintf("ssh_signing=auto: host gpg.format is %q, not ssh", format)
	}

	signingKey := get("user.signingkey")
	if signingKey == "" {
		return SigningSetup{}, "ssh_signing=auto: host git has no user.signingkey configured"
	}
	pub, reason := resolvePublicKey(signingKey)
	if reason != "" {
		return SigningSetup{}, "ssh_signing=auto: " + reason
	}

	s, finishReason := finishSetup(pub, repoDir)
	if finishReason != "" {
		return SigningSetup{}, finishReason
	}
	s.SigningKeyValue = signingKey
	return s, ""
}

// detectWithExplicitKey handles the "ssh_signing: <path>" form: the key comes
// from the given file, but agent/identity detection still runs.
func detectWithExplicitKey(path, repoDir string) (SigningSetup, string) {
	pub, reason := readPublicKeyFile(path)
	if reason != "" {
		return SigningSetup{}, "ssh_signing: " + reason
	}
	s, finishReason := finishSetup(pub, repoDir)
	if finishReason != "" {
		return SigningSetup{}, finishReason
	}
	s.SigningKeyValue = path
	return s, ""
}

// finishSetup validates the agent socket and fills the identity fields.
func finishSetup(pub, repoDir string) (SigningSetup, string) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return SigningSetup{}, "ssh_signing: SSH_AUTH_SOCK is not set (no ssh-agent)"
	}
	if resolved, err := filepath.EvalSymlinks(sock); err == nil {
		sock = resolved
	}
	if _, err := os.Stat(sock); err != nil {
		return SigningSetup{}, fmt.Sprintf("ssh_signing: agent socket %s: %v", sock, err)
	}

	return SigningSetup{
		Enabled:   true,
		AgentSock: sock,
		PublicKey: pub,
		AutoSign:  func() bool { v, _, _ := gitConfigProbeBool(repoDir, "commit.gpgsign"); return v }(),
		UserName:  gitConfigGet(repoDir, "user.name"),
		UserEmail: gitConfigGet(repoDir, "user.email"),
	}, ""
}

// resolvePublicKey turns a user.signingkey value into a literal public key.
// Accepted forms (git-config(1) gpg.ssh / user.signingKey):
//   - "key::ssh-ed25519 AAAA…"       → literal, use as-is (prefix stripped)
//   - "ssh-ed25519 AAAA…"            → already a literal key
//   - "/path/to/key.pub"             → read the file
//   - "/path/to/key" (private path)  → read "<path>.pub" if present;
//     NEVER open the private key file itself.
func resolvePublicKey(v string) (string, string) {
	if strings.HasPrefix(v, "key::") {
		return strings.TrimSpace(strings.TrimPrefix(v, "key::")), ""
	}
	if strings.HasPrefix(v, "ssh-") || strings.HasPrefix(v, "sk-ssh-") {
		return strings.TrimSpace(v), ""
	}
	path := expandHome(v)
	if !strings.HasSuffix(path, ".pub") {
		// user.signingkey often points at the PRIVATE key; use the .pub
		// sibling and never read the private file.
		path += ".pub"
	}
	return readPublicKeyFile(path)
}

// readPublicKeyFile reads a .pub file and returns its first line. Refuses
// paths that do not end in .pub as a guard against ever reading private keys.
func readPublicKeyFile(path string) (string, string) {
	path = expandHome(path)
	if !strings.HasSuffix(path, ".pub") {
		return "", fmt.Sprintf("refusing to read %q: signing key must be a .pub file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Sprintf("reading public key %s: %v", path, err)
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	if line == "" {
		return "", fmt.Sprintf("public key file %s is empty", path)
	}
	if !strings.HasPrefix(line, "ssh-") && !strings.HasPrefix(line, "sk-ssh-") {
		return "", fmt.Sprintf("%s does not look like an SSH public key", path)
	}
	return line, ""
}

// RepoSigning is the per-repository signing decision, resolved on the HOST at
// plan time (0.5.1). The sandbox never decides or changes the signing key: it
// only receives the resolved public literal (at COMMAND scope, see
// signingEnv) and the agent-forwarded private key. PrivatePath is a HOST path
// and never crosses into the sandbox.
type RepoSigning struct {
	Signing         bool   // repo commits are signed (commit.gpgsign true)
	PublicKey       string // literal public key line; "" when !Signing
	PrivateKeyPath  string // host path of the private key when known; "" otherwise
	SigningKeyValue string // raw user.signingkey value (for error messages)
}

// ResolveRepoSigning reads the repo's effective git config ON THE HOST — with
// repoDir as the working directory, so a repo-local .git/config wins over the
// user's global config, exactly as a commit on the host would resolve it
// (0.5.1: per-repo resolution).
//
// commit.gpgsign not true (false or unset) means signing is OFF: nothing is
// injected and no error is returned. commit.gpgsign=true is a HARD requirement:
// an unset user.signingkey or a key that cannot be resolved is an error that
// fails the plan (never a silent unsigned start).
//
// explicitKey (non-empty) overrides the repo's user.signingkey (the
// ssh_signing "<path to a .pub file>" form). It is resolved the same way.
//
// Fail-closed: a git invocation that itself fails (exit code other than 1 =
// unset: missing git, dubious ownership, corrupt config) is an ERROR that
// fails the plan — it is never mistaken for "signing is off" (a silent
// unsigned start).
func ResolveRepoSigning(repoDir, explicitKey string) (RepoSigning, error) {
	v, set, err := gitConfigProbeBool(repoDir, "commit.gpgsign")
	if err != nil {
		return RepoSigning{}, fmt.Errorf("repo %s: reading commit.gpgsign: %w", repoDir, err)
	}
	if !set || !v {
		return RepoSigning{}, nil
	}
	key := explicitKey
	if key == "" {
		key, set, err = gitConfigProbe(repoDir, "user.signingkey")
		if err != nil {
			return RepoSigning{}, fmt.Errorf("repo %s: reading user.signingkey: %w", repoDir, err)
		}
		if !set {
			return RepoSigning{}, fmt.Errorf("repo %s: commit.gpgsign is true but user.signingkey is unset — set it (or set commit.gpgsign false), or turn wakil ssh_signing off", repoDir)
		}
	}
	format, fset, err := gitConfigProbe(repoDir, "gpg.format")
	if err != nil {
		return RepoSigning{}, fmt.Errorf("repo %s: reading gpg.format: %w", repoDir, err)
	}
	if fset && format != "" && format != "ssh" {
		return RepoSigning{}, fmt.Errorf("repo %s: gpg.format is %q — only ssh signing is supported", repoDir, format)
	}
	// gpg.format unset is treated as ssh intent: wakil signs with ssh keys only,
	// and signing is required either way (commit.gpgsign=true).
	pub, priv, reason := resolveSigningKey(key, repoDir)
	if reason != "" {
		return RepoSigning{}, fmt.Errorf("repo %s: resolving user.signingkey %q: %s", repoDir, key, reason)
	}
	return RepoSigning{Signing: true, PublicKey: pub, PrivateKeyPath: priv, SigningKeyValue: key}, nil
}

// resolveSigningKey turns a user.signingkey value into a literal public key and
// the private-key path when the value names one. repoDir is the repository
// directory the config was read WITH (`git -C repoDir`): a relative path is
// resolved against it, exactly as a host commit in that repo would resolve it
// (git runs with the repo as cwd), never against wakil's own process cwd.
// Accepted forms (git-config(1) gpg.ssh / user.signingKey):
//   - "key::ssh-ed25519 AAAA…"       → literal, use as-is (prefix stripped)
//   - "ssh-ed25519 AAAA…"            → already a literal key
//   - "/path/to/key.pub"             → read the file; private key is the .pub-
//     less sibling
//   - "/path/to/key" (private path)  → read "<path>.pub" if present, never
//     open the private key file itself
func resolveSigningKey(v, repoDir string) (pub, priv, reason string) {
	if strings.HasPrefix(v, "key::") {
		return strings.TrimSpace(strings.TrimPrefix(v, "key::")), "", ""
	}
	if strings.HasPrefix(v, "ssh-") || strings.HasPrefix(v, "sk-ssh-") {
		return strings.TrimSpace(v), "", ""
	}
	path := expandHome(v)
	if !filepath.IsAbs(path) && repoDir != "" {
		// git resolves a relative user.signingkey against the repo's cwd;
		// mirror that, never wakil's process cwd.
		path = filepath.Join(repoDir, path)
	}
	if strings.HasSuffix(path, ".pub") {
		// user.signingkey often points at the PRIVATE key; use the .pub
		// sibling and never read the private file.
		pub, reason = readPublicKeyFile(path)
		return pub, strings.TrimSuffix(path, ".pub"), reason
	}
	pub, reason = readPublicKeyFile(path + ".pub")
	return pub, path, reason
}

// AgentFingerprints lists the SHA256 fingerprints the HOST ssh-agent holds
// (ssh-add -l). An unreachable agent, or one holding nothing, yields an empty
// list — never an error: the caller decides whether a missing key is fatal.
func AgentFingerprints() []string {
	out, err := exec.Command("ssh-add", "-l").Output()
	if err != nil {
		return nil
	}
	var fps []string
	for _, line := range strings.Split(string(out), "\n") {
		// "<bits> SHA256:… <comment> (<type>)"
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[1], "SHA256:") {
			fps = append(fps, f[1])
		}
	}
	return fps
}

// AgentHoldsFingerprint reports whether the host ssh-agent holds fp.
func AgentHoldsFingerprint(fp string) bool {
	for _, f := range AgentFingerprints() {
		if f == fp {
			return true
		}
	}
	return false
}

// AgentAddKey loads a private key into the HOST ssh-agent, one bounded
// attempt. A passphrase-protected key with no askpass fails here (and the
// plan fails loud with that context). The key enters the agent on the host;
// it is never copied anywhere else.
func AgentAddKey(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh-add", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh-add %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitConfigGet returns the host-effective value of a git config key for the
// given repo directory (repo-local wins over global, matching host behavior).
// Empty string when unset or on any error.
func gitConfigGet(repoDir, key string) string {
	v, set, err := gitConfigProbe(repoDir, key)
	if err != nil || !set {
		return ""
	}
	return v
}

// gitConfigProbe reads a git config key with the repo as cwd (host-effective:
// repo-local wins over global). set is false only for a confirmed unset key
// (git exit 1). Every other git failure is an ERROR — a missing git binary,
// dubious ownership, a corrupt config must never be mistaken for "unset"
// (fail-closed: a gpgsign=true repo must not silently start unsigned).
func gitConfigProbe(repoDir, key string) (value string, set bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "config", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", false, nil // confirmed unset
		}
		return "", false, fmt.Errorf("git config --get %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// gitConfigProbeBool reads a boolean key with git's own canonicalization
// (--type=bool maps yes/on/1 → "true"), so all spellings are honored. A git
// failure is an error, never a silent "false" (see gitConfigProbe).
func gitConfigProbeBool(repoDir, key string) (value bool, set bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "config", "--get", "--type=bool", key)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, false, nil // confirmed unset
		}
		return false, false, fmt.Errorf("git config --get --type=bool %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)) == "true", true, nil
}

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// signingEnv builds the docker run args for an enabled SigningSetup: the
// agent socket bind-mount plus GIT_CONFIG_* env pairs ("command" scope —
// overrides all config files, nothing written to disk in the container).
func signingEnv(s SigningSetup) []string {
	if !s.Enabled {
		return nil
	}
	args := []string{
		"-v", s.AgentSock + ":/ssh-agent.sock",
		"-e", "SSH_AUTH_SOCK=/ssh-agent.sock",
	}
	type kv struct{ k, v string }
	pairs := []kv{
		{"gpg.format", "ssh"},
		{"user.signingkey", "key::" + s.PublicKey},
	}
	// Mirror the host-effective per-repo value: inject only when the host
	// would auto-sign. Manual `git commit -S` works either way because
	// format+key are always present.
	if s.AutoSign {
		pairs = append(pairs, kv{"commit.gpgsign", "true"})
	}
	if s.UserName != "" {
		pairs = append(pairs, kv{"user.name", s.UserName})
	}
	if s.UserEmail != "" {
		pairs = append(pairs, kv{"user.email", s.UserEmail})
	}
	args = append(args, "-e", fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs)))
	for i, p := range pairs {
		args = append(args,
			"-e", fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p.k),
			"-e", fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p.v),
		)
	}
	return args
}

// HostIdentity returns the repo-scoped host user.name and user.email, as a
// host commit in repoDir would use them. Empty values mean "not configured".
func HostIdentity(repoDir string) (name, email string) {
	return gitConfigGet(repoDir, "user.name"), gitConfigGet(repoDir, "user.email")
}
