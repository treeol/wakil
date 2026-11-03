package sandbox

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DockerAgentRoot is the path the docker backend shows the agent for a host
// workspace directory: /mnt/<basename>. It is the only place that encodes the
// /mnt convention.
func DockerAgentRoot(hostDir string) string {
	base := filepath.Base(hostDir)
	if hostDir == "" || base == "." || base == string(filepath.Separator) {
		return "/mnt/work"
	}
	return "/mnt/" + base
}

// Within reports whether p equals root or lies below it (lexically; both are
// cleaned first).
func Within(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// PathMap is the path model shared by backends. Two namespaces exist:
//
//   - execution paths: anything the sandbox resolves (workspace, grants, the
//     private temp dir);
//   - host paths: what Wakil itself may open with os.*.
//
// Sandbox-private paths are NEVER identity-mapped to the host: session /tmp/x
// and host /tmp/x are different objects with the same name.
type PathMap struct {
	HostRoot  string   // host workspace
	AgentRoot string   // same workspace as the agent sees it
	Private   []string // sandbox-private prefixes (execution-only), e.g. /tmp

	mu      sync.RWMutex
	grants  map[string]bool // active grant paths (identical on host and agent side)
	grantRW map[string]bool // true if the grant was rw (for replay)
}

// NewPathMap returns a PathMap with cleaned roots.
func NewPathMap(hostRoot, agentRoot string, private ...string) *PathMap {
	return &PathMap{
		HostRoot:  filepath.Clean(hostRoot),
		AgentRoot: filepath.Clean(agentRoot),
		Private:   private,
		grants:    map[string]bool{},
		grantRW:   map[string]bool{},
	}
}

// AddGrant records an active grant (identity-mapped host path). The rw
// flag is recorded so the grant can be replayed with the same mode after a
// session replacement.
func (m *PathMap) AddGrant(p string, rw bool) {
	m.mu.Lock()
	m.grants[filepath.Clean(p)] = true
	m.grantRW[filepath.Clean(p)] = rw
	m.mu.Unlock()
}

// RemoveGrant forgets a grant.
func (m *PathMap) RemoveGrant(p string) {
	m.mu.Lock()
	delete(m.grants, filepath.Clean(p))
	delete(m.grantRW, filepath.Clean(p))
	m.mu.Unlock()
}

// ResetGrants forgets every grant (the session ended).
func (m *PathMap) ResetGrants() {
	m.mu.Lock()
	m.grants = map[string]bool{}
	m.grantRW = map[string]bool{}
	m.mu.Unlock()
}

// Grants returns a snapshot of the active grant paths and their rw mode.
func (m *PathMap) Grants() []GrantEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]GrantEntry, 0, len(m.grants))
	for g := range m.grants {
		out = append(out, GrantEntry{Path: g, RW: m.grantRW[g]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// GrantEntry is one active grant with its rw mode.
type GrantEntry struct {
	Path string
	RW   bool
}

func (m *PathMap) granted(p string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for g := range m.grants {
		if Within(g, p) {
			return true
		}
	}
	return false
}

func (m *PathMap) isPrivate(p string) bool {
	for _, pr := range m.Private {
		if Within(pr, p) {
			return true
		}
	}
	return false
}

func requireAbs(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("sandbox: path %q is not absolute", p)
	}
	return nil
}

// MapPath implements Backend.MapPath.
func (m *PathMap) MapPath(agentPath string) (string, error) {
	if err := requireAbs(agentPath); err != nil {
		return "", err
	}
	p := filepath.Clean(agentPath)
	if Within(m.AgentRoot, p) {
		rel, _ := filepath.Rel(m.AgentRoot, p)
		return filepath.Join(m.HostRoot, rel), nil
	}
	if m.granted(p) {
		return p, nil
	}
	if m.isPrivate(p) {
		return "", fmt.Errorf("%w: %s", ErrNoHostPath, p)
	}
	return "", fmt.Errorf("%w: %s", ErrNotVisible, p)
}

// UnmapPath implements Backend.UnmapPath.
func (m *PathMap) UnmapPath(hostPath string) (string, error) {
	if err := requireAbs(hostPath); err != nil {
		return "", err
	}
	p := filepath.Clean(hostPath)
	if Within(m.HostRoot, p) {
		rel, _ := filepath.Rel(m.HostRoot, p)
		return filepath.Join(m.AgentRoot, rel), nil
	}
	if m.granted(p) {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotVisible, p)
}

// ResolveExecCwd validates an agent-side working directory for exec. Unlike
// MapPath it also accepts sandbox-private paths, which execute fine but have
// no host representation.
func (m *PathMap) ResolveExecCwd(agentPath string) (string, error) {
	if err := requireAbs(agentPath); err != nil {
		return "", err
	}
	p := filepath.Clean(agentPath)
	if _, err := m.MapPath(p); err == nil {
		return p, nil
	}
	if m.isPrivate(p) {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotVisible, p)
}
