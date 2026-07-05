package exec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SigningSetup is resolved on the HOST before the sandbox container starts and
// consumed by NewDockerExecutor. It carries everything needed to make git
// SSH-sign commits inside the container without the private key ever entering
// it: the host ssh-agent socket (bind-mounted) and the literal public key
// (injected via GIT_CONFIG_* env, "key::" form — see git-config(1)).
type SigningSetup struct {
	Enabled   bool
	AgentSock string // resolved host SSH_AUTH_SOCK path
	PublicKey string // literal public key, e.g. "ssh-ed25519 AAAA… comment"
	AutoSign  bool   // host-effective commit.gpgsign for the workspace repo
	UserName  string // host-effective user.name (empty = don't inject)
	UserEmail string // host-effective user.email (empty = don't inject)
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
	format := gitConfigGet(repoDir, "gpg.format")
	if format != "ssh" {
		if format == "" {
			return SigningSetup{}, "ssh_signing=auto: host git has no gpg.format configured"
		}
		return SigningSetup{}, fmt.Sprintf("ssh_signing=auto: host gpg.format is %q, not ssh", format)
	}

	signingKey := gitConfigGet(repoDir, "user.signingkey")
	if signingKey == "" {
		return SigningSetup{}, "ssh_signing=auto: host git has no user.signingkey configured"
	}
	pub, reason := resolvePublicKey(signingKey)
	if reason != "" {
		return SigningSetup{}, "ssh_signing=auto: " + reason
	}

	return finishSetup(pub, repoDir)
}

// detectWithExplicitKey handles the "ssh_signing: <path>" form: the key comes
// from the given file, but agent/identity detection still runs.
func detectWithExplicitKey(path, repoDir string) (SigningSetup, string) {
	pub, reason := readPublicKeyFile(path)
	if reason != "" {
		return SigningSetup{}, "ssh_signing: " + reason
	}
	return finishSetup(pub, repoDir)
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
		AutoSign:  gitConfigGetBool(repoDir, "commit.gpgsign"),
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

// gitConfigGet returns the host-effective value of a git config key for the
// given repo directory (repo-local wins over global, matching host behavior).
// Empty string when unset or on any error.
func gitConfigGet(repoDir, key string) string {
	cmd := exec.Command("git", "-C", repoDir, "config", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitConfigGetBool reads a boolean key with git's own canonicalization
// (--type=bool maps yes/on/1 → "true"), so all spellings are honored.
func gitConfigGetBool(repoDir, key string) bool {
	cmd := exec.Command("git", "-C", repoDir, "config", "--get", "--type=bool", key)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
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
