package hisn

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GitIdentity is the commit identity written into the session gitconfig.
// Empty fields are omitted; both empty means no [user] section.
type GitIdentity struct {
	Name  string
	Email string
}

// GitSigning describes SSH commit signing for the session. Off when Key is "".
// Key is the PUBLIC key line (ssh-ed25519 AAAA… comment) read on the host; the
// private key is never read or copied.
type GitSigning struct {
	Key           string // public key line; "" = signing off
	AllowedSigner string // path of the allowed_signers file (inside the home)
}

var (
	// gitName/gitEmail guard against newlines or control characters that would
	// let a value break out of its gitconfig line.
	gitValueRe = regexp.MustCompile(`^[^\x00-\x1f\x7f]{0,256}$`)
	// pubKeyRe is a single OpenSSH public-key line, no key material beyond it.
	pubKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/=]+( [^\x00-\x1f\x7f]*)?$`)
)

// ValidateGitIdentity rejects values that could not be written safely.
func ValidateGitIdentity(id GitIdentity) error {
	if !gitValueRe.MatchString(id.Name) || strings.Contains(id.Name, "\"") {
		return fmt.Errorf("git.identity name: invalid characters")
	}
	if !gitValueRe.MatchString(id.Email) || strings.Contains(id.Email, "\"") {
		return fmt.Errorf("git.identity email: invalid characters")
	}
	return nil
}

// renderGitConfig returns the session gitconfig. It is written into the
// persisted home (never the repository) and selected with GIT_CONFIG_GLOBAL.
//
// F9: signing settings are injected at COMMAND scope via GIT_CONFIG_* env
// vars on every exec (see BackendExecutor.signingEnv), so a repo-local
// .git/config with its own user.signingkey cannot shadow them — command scope
// beats local and global. The session gitconfig carries identity AND signing
// settings as a defense-in-depth fallback: on git < 2.31 (which does not
// support GIT_CONFIG_COUNT), the gitconfig still enforces signing. On git >=
// 2.31, the COMMAND-scope env wins, so the gitconfig signing settings are
// redundant but harmless.
func renderGitConfig(id GitIdentity, sg GitSigning) ([]byte, error) {
	if err := ValidateGitIdentity(id); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# written by wakil for this session; not the repository's config\n")
	if id.Name != "" || id.Email != "" {
		b.WriteString("[user]\n")
		if id.Name != "" {
			fmt.Fprintf(&b, "\tname = %q\n", id.Name)
		}
		if id.Email != "" {
			fmt.Fprintf(&b, "\temail = %q\n", id.Email)
		}
	}
	if sg.Key != "" {
		if !pubKeyRe.MatchString(sg.Key) {
			return nil, fmt.Errorf("ssh signing key is not a single OpenSSH public-key line")
		}
		if sg.AllowedSigner == "" || !filepath.IsAbs(sg.AllowedSigner) {
			return nil, fmt.Errorf("ssh signing needs an absolute allowed_signers path")
		}
		fmt.Fprintf(&b, "[user]\n\tsigningkey = %q\n", "key::"+sg.Key)
		b.WriteString("[gpg]\n\tformat = ssh\n")
		fmt.Fprintf(&b, "[gpg \"ssh\"]\n\tallowedSignersFile = %q\n", sg.AllowedSigner)
		b.WriteString("[commit]\n\tgpgsign = true\n")
		b.WriteString("[tag]\n\tgpgsign = true\n")
	}
	return []byte(b.String()), nil
}

// withGitConfig writes the session gitconfig into the persisted home and points
// git at it with GIT_CONFIG_GLOBAL. Only identity (user.name, user.email) is
// written to the gitconfig — signing settings are injected at COMMAND scope
// via GIT_CONFIG_* env vars on every exec (F9, see BackendExecutor.signingEnv),
// so a repo-local .git/config cannot shadow the enforced signing key. The
// allowed_signers file is still written under HomeRoot so the in-session agent
// can verify SSH signatures.
//
// F8: the file is written on the HOST under HomeRoot, but GIT_CONFIG_GLOBAL
// and the allowedSignersFile value in the gitconfig must point at in-session
// paths — hisn remounts the persist dir at SandboxHome (/run/hisn/home), so
// the host path does not exist inside the sandbox. The GIT_SSH_COMMAND
// UserKnownHostsFile points at the in-session RO mount KnownHostsMount (hisn
// "src:dst", 0.3.3); the host path does not exist inside the sandbox.
func (b *Backend) withGitConfig(s Surface) (Surface, error) {
	env := map[string]string{}
	for k, v := range s.EnvSet {
		env[k] = v
	}
	if b.opts.GitName != "" || b.opts.GitEmail != "" || b.opts.SigningKey != "" {
		if _, err := writeSessionGitConfig(b.opts.HomeRoot,
			GitIdentity{Name: b.opts.GitName, Email: b.opts.GitEmail},
			GitSigning{Key: b.opts.SigningKey}); err != nil {
			return s, err
		}
		// GIT_CONFIG_GLOBAL uses the in-session path (F8): hisn remounts the
		// persist dir at SandboxHome, so the host path does not exist there.
		env["GIT_CONFIG_GLOBAL"] = filepath.Join(SandboxHome, ".wakil-gitconfig")
	}
	// An ssh transport (agent on) pins known_hosts to the host file, read-only,
	// and refuses unknown hosts. The host file is mounted at KnownHostsMount
	// inside the session ("src:dst", hisn 0.3.3) and GIT_SSH_COMMAND names that
	// in-session path.
	if s.SSHAgent && b.opts.KnownHosts != "" {
		env["GIT_SSH_COMMAND"] = "ssh -o UserKnownHostsFile=" + KnownHostsMount + " -o StrictHostKeyChecking=yes"
		s.MountsRO = append(append([]string(nil), s.MountsRO...), b.opts.KnownHosts+":"+KnownHostsMount)
	}
	if len(env) > 0 {
		s.EnvSet = env
	}
	return s, nil
}

// writeSessionGitConfig writes .wakil-gitconfig (and the allowed_signers file
// when signing) into home (host HomeRoot), mode 0600. It returns the gitconfig
// path. The gitconfig carries identity only (F9); the allowed_signers file is
// written so the in-session agent can verify SSH signatures. The
// allowedSignersFile value uses the in-session path (<SandboxHome>/.wakil-
// allowed-signers) — hisn remounts the persist dir at SandboxHome, so the host
// path does not exist inside the sandbox.
func writeSessionGitConfig(home string, id GitIdentity, sg GitSigning) (string, error) {
	if sg.Key != "" {
		sg.AllowedSigner = filepath.Join(SandboxHome, ".wakil-allowed-signers")
	}
	if sg.Key != "" && id.Email == "" {
		return "", fmt.Errorf("ssh signing needs git.identity email for the allowed signers file")
	}
	body, err := renderGitConfig(id, sg)
	if err != nil {
		return "", err
	}
	if sg.Key != "" {
		// allowed_signers: "<identity> <key>". Identity is the commit email.
		// Written at the HOST path (<home>/.wakil-allowed-signers); the
		// gitconfig value points at the in-session path. Signing settings are
		// also injected at COMMAND scope via GIT_CONFIG_* env (F9) — the
		// gitconfig is a defense-in-depth fallback for git < 2.31.
		hostSigner := filepath.Join(home, ".wakil-allowed-signers")
		line := fmt.Sprintf("%s %s\n", id.Email, sg.Key)
		if err := os.WriteFile(hostSigner, []byte(line), 0o600); err != nil {
			return "", fmt.Errorf("writing allowed signers: %w", err)
		}
	}
	p := filepath.Join(home, ".wakil-gitconfig")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		return "", fmt.Errorf("writing session gitconfig: %w", err)
	}
	return p, nil
}
