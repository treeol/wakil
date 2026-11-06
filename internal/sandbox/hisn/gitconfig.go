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
// git at it with GIT_CONFIG_GLOBAL. Only the public key and identity are
// written; the agent socket is reached through the [ssh] table, never a raw
// path. It returns the surface with the env the session needs.
//
// F8: the file is written on the HOST under HomeRoot, but GIT_CONFIG_GLOBAL and
// the allowedSignersFile value in the gitconfig must point at in-session paths
// — hisn remounts the persist dir at SandboxHome (/run/hisn/home), so the host
// path does not exist inside the sandbox. The GIT_SSH_COMMAND
// UserKnownHostsFile is a same-path RO mount (the host path IS valid inside the
// sandbox), so it stays as is.
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
	// and refuses unknown hosts. hisn's mounts are same-path only (a
	// source:target form is rejected), so the file keeps its host path.
	if s.SSHAgent && b.opts.KnownHosts != "" {
		env["GIT_SSH_COMMAND"] = "ssh -o UserKnownHostsFile=" + b.opts.KnownHosts + " -o StrictHostKeyChecking=yes"
		s.MountsRO = append(append([]string(nil), s.MountsRO...), b.opts.KnownHosts)
	}
	if len(env) > 0 {
		s.EnvSet = env
	}
	return s, nil
}

// writeSessionGitConfig writes .wakil-gitconfig (and the allowed_signers file
// when signing) into home (host HomeRoot), mode 0600. It returns the gitconfig
// path. The allowed_signers file is written at <home>/.wakil-allowed-signers
// (host), but the gitconfig's allowedSignersFile value uses the in-session
// path (<SandboxHome>/.wakil-allowed-signers) — hisn remounts the persist dir
// at SandboxHome, so the host path does not exist inside the sandbox.
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
		// gitconfig value points at the in-session path.
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
