package agent

// Workspace identity report (0.4.4, M1/D3). Pure reporting over the existing
// derivations — nothing here changes how a store key is computed.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crypto/sha256"
	"encoding/hex"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// Identity source labels.
const (
	SourceCwd           = "cwd"
	SourceEnvPath       = "WAKIL_WORKSPACE_PATH"
	SourceConfig        = "config"
	SourceEnvKey        = "WAKIL_WORKSPACE_KEY"
	sourceConfigHost    = "config host_workdir (docker mode)"
	sourceConfigWorkDir = "config workdir (direct mode)"
)

// WorkspaceKeys is what a chat started with cfg keys its per-project state by,
// next to what `plan gc` from the same directory would use.
type WorkspaceKeys struct {
	// Chat path: agent.ProjectDir(cfg), the input to every chat store key.
	ChatPath       string
	ChatPathSource string
	// MemoryKey is the memory store key (directory name under memory/): the
	// SHA-256 of the canonical ChatPath truncated to 16 hex, unless
	// WAKIL_WORKSPACE_KEY overrides it.
	MemoryKey    string
	MemoryDBPath string
	// FullKey is the untruncated workspace key (repo-state, session scoping).
	FullKey string
	// SessionHostID is the sessionhost workspace id (wsp_<16 hex>, hashed from
	// the canonical ChatPath) and SessionHostKey the directory
	// under sessionhost/ holding its database.
	SessionHostID     string
	SessionHostKey    string
	SessionHostDBPath string
	// KeyOverride is set when WAKIL_WORKSPACE_KEY replaces the hashed key.
	KeyOverride bool
	// Cwd is the launch directory (canonical), and GCPath/GCMemoryKey what
	// `wakil plan gc` run from there would resolve: it ignores both
	// WAKIL_WORKSPACE_PATH and the config workspace.
	Cwd         string
	GCPath      string
	GCMemoryKey string
	// Diverges is true when the chat's memory key differs from the one plan gc
	// resolves from the same directory (the 0.4.3 / C4 situation).
	Diverges bool
	// LegacyKey is the pre-0.5.0 shared store key (the old derivation from the
	// legacy workspace path), and LegacyEntries the active entries it holds
	// (-1 when the store does not exist). Read-only: the count never creates it.
	LegacyKey     string
	LegacyDBPath  string
	LegacyEntries int
}

// ResolveWorkspaceKeys computes the report for cfg. The working directory and
// the WAKIL_WORKSPACE_PATH / WAKIL_WORKSPACE_KEY environment are read at call
// time.
func ResolveWorkspaceKeys(cfg config.Config) WorkspaceKeys {
	var k WorkspaceKeys
	k.ChatPath = ProjectDir(cfg)
	switch {
	case cfg.ExecMode == "direct" && cfg.WorkDir != "":
		k.ChatPathSource = sourceConfigWorkDir
	case cfg.ExecMode != "direct" && cfg.HostWorkDir != "":
		k.ChatPathSource = sourceConfigHost
	default:
		k.ChatPathSource = SourceCwd
	}
	// When the config itself defaulted the directory to the cwd, say so.
	if wd, err := os.Getwd(); err == nil && sameDir(k.ChatPath, wd) {
		k.ChatPathSource = SourceCwd
	}

	k.KeyOverride = os.Getenv("WAKIL_WORKSPACE_KEY") != ""
	k.FullKey = WorkspaceKey(k.ChatPath)
	if p := MemoryDBPath(k.ChatPath); p != "" {
		k.MemoryDBPath = p
		k.MemoryKey = filepath.Base(filepath.Dir(p))
	}
	k.SessionHostID = SessionHostWorkspaceID(k.ChatPath)
	if p := SessionHostDBPath(k.ChatPath); p != "" {
		k.SessionHostDBPath = p
		k.SessionHostKey = filepath.Base(filepath.Dir(p))
	}

	if wd, err := os.Getwd(); err == nil {
		k.Cwd = canonicalDir(wd)
		k.GCPath = k.Cwd
		if p := MemoryDBPath(k.GCPath); p != "" {
			k.GCMemoryKey = filepath.Base(filepath.Dir(p))
		}
	}
	k.Diverges = k.MemoryKey != "" && k.GCMemoryKey != "" && k.MemoryKey != k.GCMemoryKey
	k.LegacyEntries = -1
	return k
}

// WithLegacy fills the legacy fields for cfg. It opens the legacy store
// read-only and only when the file exists, so it never creates a store.
func (k WorkspaceKeys) WithLegacy(cfg config.Config) WorkspaceKeys {
	p := LegacyMemoryDBPath(cfg)
	if p == "" {
		return k
	}
	k.LegacyDBPath = p
	k.LegacyKey = filepath.Base(filepath.Dir(p))
	if _, err := os.Stat(p); err != nil {
		return k
	}
	ls, err := memory.OpenReadOnly(p, "")
	if err != nil {
		return k
	}
	defer ls.Close()
	if es, err := ls.List(context.Background(), "", "", "active"); err == nil {
		k.LegacyEntries = len(es)
	}
	return k
}

// SessionHostWorkspaceID is the sessionhost workspace id for a project
// directory: "wsp_" + the first 8 bytes of sha256(canonical path), hex. The
// path is canonicalised first, so a symlinked and a real path give the same id
// and the id agrees with the store key (0.5.0, C1). wiring.WorkspaceIDFromConfig
// computes the same value (locked by TestWorkspaceIDMatchesAgentReport).
// Sessions written before 0.5.0 carry the raw-path id (see LegacySessionHostWorkspaceID).
func SessionHostWorkspaceID(ws string) string {
	sum := sha256.Sum256([]byte(canonicalWorkspace(ws)))
	return "wsp_" + hex.EncodeToString(sum[:8])
}

// LegacySessionHostWorkspaceID is the pre-0.5.0 id: sha256 of the RAW path. It
// is computed only to find sessions recorded under it (read-through, C2); it is
// never written.
func LegacySessionHostWorkspaceID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "wsp_" + hex.EncodeToString(sum[:8])
}

// Format renders the multi-line report printed by --print-workspace-key.
func (k WorkspaceKeys) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "workspace path:     %s   [source: %s]\n", orNone(k.ChatPath), k.ChatPathSource)
	keySrc := "sha256(canonical path)"
	if k.KeyOverride {
		keySrc = SourceEnvKey + " (verbatim override)"
	}
	fmt.Fprintf(&b, "memory store key:   %s   [source: %s, truncated to 16 hex]\n", orNone(k.MemoryKey), keySrc)
	fmt.Fprintf(&b, "  memory db:        %s\n", orNone(k.MemoryDBPath))
	fmt.Fprintf(&b, "workspace key:      %s   [source: %s]\n", orNone(k.FullKey), keySrc)
	fmt.Fprintf(&b, "sessionhost id:     %s   [source: sha256(canonical path) first 8 bytes]\n", orNone(k.SessionHostID))
	fmt.Fprintf(&b, "sessionhost key:    %s   [source: %s, truncated to 16 hex]\n", orNone(k.SessionHostKey), keySrc)
	fmt.Fprintf(&b, "  sessionhost db:   %s\n", orNone(k.SessionHostDBPath))
	fmt.Fprintf(&b, "plan gc from here:  %s   [source: %s]  memory key %s\n", orNone(k.GCPath), SourceCwd, orNone(k.GCMemoryKey))
	if k.ChatPath == "" {
		b.WriteString("NOTE: no workspace path (direct mode without a workdir argument): a chat started with this config keys no per-project store; pass the project directory as an argument or set WAKIL_WORKSPACE_PATH.\n")
	}
	// The legacy key is the 0.4.x shared-store key (WorkspaceKey of the old
	// WorkspacePath()), independent of the project. When it equals this chat's
	// project key there is no separate legacy store: report none, never the
	// project's own store as if it were legacy.
	switch {
	case k.LegacyKey == "" || k.LegacyKey == k.MemoryKey:
		b.WriteString("legacy store:       none (no pre-0.5.0 shared store distinct from this project)\n")
	case k.LegacyEntries >= 0:
		fmt.Fprintf(&b, "legacy store:       %s   [pre-0.5.0 shared store, read-only: %d active entries]\n", k.LegacyKey, k.LegacyEntries)
		fmt.Fprintf(&b, "  legacy db:        %s\n", k.LegacyDBPath)
	default:
		fmt.Fprintf(&b, "legacy store:       %s   [pre-0.5.0 shared store: none on this host]\n", k.LegacyKey)
	}
	return b.String()
}

// EnvOverrideLine is the one startup line for D3: non-empty only when
// WAKIL_WORKSPACE_PATH is set. It names the path, the resulting store keys and
// that the variable overrides the cwd.
func (k WorkspaceKeys) EnvOverrideLine() string {
	if os.Getenv("WAKIL_WORKSPACE_PATH") == "" {
		return ""
	}
	s := fmt.Sprintf("workspace: WAKIL_WORKSPACE_PATH=%s is IGNORED for store keys since 0.5.0 — keys follow the working directory %s: memory key %s, sessionhost key %s (id %s)",
		os.Getenv("WAKIL_WORKSPACE_PATH"), k.ChatPath, orNone(k.MemoryKey), orNone(k.SessionHostKey), orNone(k.SessionHostID))
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func canonicalDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return canonicalDir(a) == canonicalDir(b)
}
