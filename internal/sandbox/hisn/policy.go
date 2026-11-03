package hisn

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// policyFile mirrors the hisn-policy/v1 TOML schema (only what Wakil sets).
type policyFile struct {
	Version string        `toml:"version"`
	Session policySession `toml:"session"`
	Mounts  policyMounts  `toml:"mounts"`
	Env     policyEnv     `toml:"env"`
}

type policySession struct {
	Cwd      string `toml:"cwd"`
	Home     string `toml:"home"`
	Network  string `toml:"network"`
	Hostname string `toml:"hostname"`
	// 0.2.1: /dev/shm tmpfs (always on for Wakil sessions; Chrome/Node and
	// POSIX shm users need it) and the /tmp tmpfs size, both MiB.
	Shm     bool  `toml:"shm"`
	ShmSize int64 `toml:"shm_size"`
	TmpSize int64 `toml:"tmp_size"`
}

type policyMounts struct {
	GrantableRW []string `toml:"grantable_rw"`
}

type policyEnv struct {
	Pass []string `toml:"pass"`
}

// buildPolicy renders the session policy. network is "host" by decision:
// builds need the network and hisn offers no filtering in between. shm is
// true by decision: /dev/shm is needed by real toolchains (Chrome, Node,
// POSIX shm), and hisn mounts it with /dev's flags, not /tmp's.
func buildPolicy(cwd, home, hostname string, grantableRW, envPass []string, shmSize, tmpSize int64) ([]byte, error) {
	p := policyFile{
		Version: "hisn-policy/v1",
		Session: policySession{Cwd: cwd, Home: home, Network: "host", Hostname: hostname,
			Shm: true, ShmSize: shmSize, TmpSize: tmpSize},
		Mounts: policyMounts{GrantableRW: nonNil(grantableRW)},
		Env:    policyEnv{Pass: nonNil(envPass)},
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(p); err != nil {
		return nil, fmt.Errorf("encoding hisn policy: %w", err)
	}
	return buf.Bytes(), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// writePolicy writes the policy (0600) into dir and returns its path.
func writePolicy(dir, id string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, id+".toml")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	return p, nil
}
