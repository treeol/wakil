// Package project loads the per-project sandbox configuration
// (<workspace>/.wakil/sandbox.json) and keeps the USER's approval of it.
//
// The file is read-only for the agent (see exec.ProtectedSandboxFile), but a
// shell-capable agent could still author one that takes effect at the next
// start. Wakil's state directory is outside every sandbox, so it is the trust
// root: a file that asks for anything is only honoured when its sha256 matches
// the hash the user approved there (`wakil sandbox trust`). There is
// deliberately no flag and no non-interactive path that approves.
package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// RelPath is the project file, relative to the workspace.
const RelPath = ".wakil/sandbox.json"

// File is the schema of .wakil/sandbox.json. Unknown keys are errors.
type File struct {
	Hisn HisnSection `json:"hisn"`
}

// HisnSection holds the hisn-specific keys.
type HisnSection struct {
	Services     []hisn.Service     `json:"services,omitempty"`
	Forward      []hisn.Forward     `json:"forward,omitempty"`
	HostServices []hisn.HostService `json:"host_services,omitempty"`
	EnvSet       map[string]string  `json:"env_set,omitempty"`
	MountsRO     []string           `json:"mounts_ro,omitempty"`
	MountsRW     []string           `json:"mounts_rw,omitempty"`
	// RiskAck is the user's acknowledgement, verbatim as hisn expects it
	// (e.g. "root-equivalent"). Wakil never writes it.
	RiskAck string `json:"risk_ack,omitempty"`
	// Home overrides sandbox.hisn.home for this project ("persist" | "tmpfs").
	// Name only: the persistent path is never set per project.
	Home string `json:"home,omitempty"`
	// Profile names a hisn profile (built-in or $HISN_PROFILES) used instead
	// of generation; Wakil then only adds grants.
	Profile string `json:"profile,omitempty"`
}

// Loaded is a parsed project file.
type Loaded struct {
	Path    string
	Present bool
	Raw     []byte
	Hash    string // sha256 hex of Raw; "" when absent
	Surface hisn.Surface
}

// Needs reports whether the file asks for anything that widens or changes the
// sandbox, i.e. whether it needs the user's approval before it is honoured.
func (l Loaded) Needs() bool {
	return l.Present && (!l.Surface.Empty() || l.Surface.RiskAck != "")
}

// Load reads <cwd>/.wakil/sandbox.json. An absent file is not an error.
func Load(cwd string) (Loaded, error) {
	path := filepath.Join(cwd, filepath.FromSlash(RelPath))
	l := Loaded{Path: path}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("%s: %w", RelPath, err)
	}
	if !fi.Mode().IsRegular() {
		return l, fmt.Errorf("%s must be a regular file, not a symlink or special file", RelPath)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return l, fmt.Errorf("%s: %w", RelPath, err)
	}
	f, err := Parse(raw)
	if err != nil {
		return l, fmt.Errorf("%s: %w", RelPath, err)
	}
	sum := sha256.Sum256(raw)
	l.Present, l.Raw, l.Hash = true, raw, hex.EncodeToString(sum[:])
	h := f.Hisn
	l.Surface = hisn.Surface{
		Services: h.Services, Forward: h.Forward, HostServices: h.HostServices,
		EnvSet: h.EnvSet, MountsRO: h.MountsRO, MountsRW: h.MountsRW,
		RiskAck: h.RiskAck, Profile: h.Profile, Home: h.Home,
	}
	return l, nil
}

// Parse decodes the file strictly: an unknown key is an error naming it, and
// trailing data after the document is refused.
func Parse(raw []byte) (File, error) {
	var f File
	if len(bytes.TrimSpace(raw)) == 0 {
		return f, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Hisn.Home != "" && f.Hisn.Home != "persist" && f.Hisn.Home != "tmpfs" {
		return f, fmt.Errorf("hisn.home %q: want \"persist\" or \"tmpfs\"", f.Hisn.Home)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return f, errors.New("invalid JSON: unexpected data after the document")
	}
	return f, nil
}

// ---- approval store ----

func approvalPath(stateDir, wsKey string) string {
	return filepath.Join(stateDir, "approved", wsKey)
}

// Approved reports whether the user approved exactly this file content for the
// workspace.
func Approved(stateDir, wsKey, hash string) bool {
	if hash == "" || wsKey == "" {
		return false
	}
	b, err := os.ReadFile(approvalPath(stateDir, wsKey))
	return err == nil && strings.TrimSpace(string(b)) == hash
}

// Approve records the user's approval. Callers must have obtained it
// interactively; nothing else may call this.
func Approve(stateDir, wsKey, hash string) error {
	if hash == "" || wsKey == "" {
		return errors.New("nothing to approve")
	}
	p := approvalPath(stateDir, wsKey)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(hash+"\n"), 0o600)
}

// ErrNotApproved is returned when a project file that needs approval has none.
type ErrNotApproved struct{ Path string }

func (e *ErrNotApproved) Error() string {
	return fmt.Sprintf("%s has not been approved (or changed since): review it, then run `wakil sandbox trust` yourself — an agent cannot approve it", e.Path)
}

// Summary describes what the file asks for, one fact per line, for the user
// to review before approving.
func Summary(l Loaded) []string {
	s := l.Surface
	var out []string
	if s.Profile != "" {
		out = append(out, fmt.Sprintf("uses hisn profile %q as is (Wakil adds only the grants below)", s.Profile))
	}
	for _, v := range s.Services {
		out = append(out, fmt.Sprintf("service %q runs INSIDE the sandbox: %s", v.Name, strings.Join(v.Exec, " ")))
	}
	for _, v := range s.Forward {
		out = append(out, fmt.Sprintf("forward %q exposes the HOST socket %s inside at %s", v.Name, v.Socket, v.Expose))
	}
	for _, v := range s.HostServices {
		out = append(out, fmt.Sprintf("host service %q RUNS ON THE HOST as you: %s (socket %s → %s)", v.Name, strings.Join(v.Exec, " "), v.Socket, v.Expose))
	}
	for _, p := range s.MountsRO {
		out = append(out, "read-only mount: "+p)
	}
	for _, p := range s.MountsRW {
		out = append(out, "READ-WRITE mount: "+p)
	}
	if len(s.EnvSet) > 0 {
		keys := make([]string, 0, len(s.EnvSet))
		for k := range s.EnvSet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, "sets environment variables: "+strings.Join(keys, ", "))
	}
	if s.RiskAck != "" {
		out = append(out, fmt.Sprintf("acknowledges risk %q (your acknowledgement: hisn refuses to start when it does not match what the profile does)", s.RiskAck))
	}
	if len(out) == 0 {
		out = append(out, "asks for nothing beyond the defaults")
	}
	return out
}
