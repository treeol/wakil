package initcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The file shapes below fix the key order of the generated config: encoding/json
// writes struct fields in declaration order and a one-entry map is stable.

type fileEndpoint struct {
	Kind       string `json:"kind"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
	AuthHeader string `json:"auth_header,omitempty"`
}

type fileSandbox struct {
	Backend string `json:"backend"`
}

type fileMashura struct {
	Level   string   `json:"level"`
	Members []string `json:"members,omitempty"`
}

type fileConfig struct {
	Endpoints       map[string]fileEndpoint `json:"endpoints"`
	DefaultEndpoint string                  `json:"default_endpoint"`
	Approval        string                  `json:"approval"`
	Mashura         *fileMashura            `json:"mashura,omitempty"`
	Sandbox         *fileSandbox            `json:"sandbox,omitempty"`
}

// plan is everything init decided; renderConfig turns it into the file.
type plan struct {
	EndpointName string
	BaseURL      string
	Model        string
	KeyEnv       string // name of the env var holding the key ("" = none)
	KeyHeader    string // "Bearer <key>" when the key was pasted ("" = none)
	Approval     string
	Backend      string // "" = automatic choice, not written
}

// renderConfig returns the minimal pretty-printed config: only the chosen
// values, no _comment keys, 2-space indent, stable key order.
func renderConfig(p plan) ([]byte, error) {
	ep := fileEndpoint{Kind: "openai", BaseURL: p.BaseURL, Model: p.Model}
	switch {
	case p.KeyHeader != "":
		ep.AuthHeader = p.KeyHeader
	case p.KeyEnv != "":
		ep.APIKeyEnv = p.KeyEnv
	}
	// Fresh installs carry no members: Mashūra is identified only by its
	// level, gated by the OpenRouter key the user supplies (amendment 4's
	// fallback member). The example config (config.example.json) shows the
	// member form for copying.
	fc := fileConfig{
		Endpoints:       map[string]fileEndpoint{p.EndpointName: ep},
		DefaultEndpoint: p.EndpointName,
		Approval:        p.Approval,
		Mashura:         &fileMashura{Level: "medium"},
	}
	if p.Backend != "" {
		fc.Sandbox = &fileSandbox{Backend: p.Backend}
	}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// writeConfig stores data at path with mode 0600 in a 0700 directory.
//
//   - No file there: created with O_EXCL, so a file (or symlink) that appears
//     meanwhile is never overwritten.
//   - A file there: only with force, and only after a 0600 backup copy
//     path.bak-<UTC timestamp> was written and verified. The new file is
//     written next to it and renamed over, so the old one is never half-written.
//
// The returned backup path is "" when no backup was needed.
func writeConfig(path string, data []byte, force bool, now time.Time, holdsSecret bool) (backup string, err error) {
	dir := filepath.Dir(path)
	if di, serr := os.Stat(dir); os.IsNotExist(serr) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	} else if serr == nil && holdsSecret {
		if err := secureDir(dir, di); err != nil {
			return "", err
		}
	}
	fi, lerr := os.Lstat(path)
	switch {
	case lerr == nil:
		if !force {
			return "", fmt.Errorf("%s already exists: not touching it (use --force to replace it; a backup is made first)", path)
		}
		if fi.IsDir() {
			return "", fmt.Errorf("%s is a directory", path)
		}
		if backup, err = backupFile(path, now); err != nil {
			return "", fmt.Errorf("backup failed, %s left untouched: %w", path, err)
		}
		if err := replaceFile(path, data); err != nil {
			return backup, err
		}
		return backup, nil
	case os.IsNotExist(lerr):
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", fmt.Errorf("create %s: %w", path, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		return "", nil
	default:
		return "", lerr
	}
}

// secureDir makes sure an existing directory that is about to hold a key is
// not accessible to group or others. Wakil's own config directory is tightened
// to 0700; any other directory (WAKIL_CONFIG pointing elsewhere) is never
// chmod-ed, init refuses instead and points at --key-env.
func secureDir(dir string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if filepath.Base(dir) != "wakil" {
		return fmt.Errorf("%s is accessible to other users (mode %o) and would hold your key: use --key-env NAME so the file stores no key, or choose a private directory", dir, fi.Mode().Perm())
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%s is accessible to other users (mode %o) and could not be tightened to 0700: %w", dir, fi.Mode().Perm(), err)
	}
	return nil
}

// backupFile copies path to path.bak-<UTC timestamp> (mode 0600, never
// overwriting) and verifies the copy.
func backupFile(path string, now time.Time) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	name := path + ".bak-" + now.UTC().Format("20060102T150405Z")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(src); err != nil {
		f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	back, err := os.ReadFile(name)
	if err != nil || string(back) != string(src) {
		_ = os.Remove(name)
		return "", fmt.Errorf("backup copy %s did not verify", name)
	}
	return name, nil
}

// replaceFile writes data to a private temp file in the same directory and
// renames it over path.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
