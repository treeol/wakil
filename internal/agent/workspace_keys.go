package agent

// Workspace identity report (0.4.4, M1/D3). Pure reporting over the existing
// derivations — nothing here changes how a store key is computed.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crypto/sha256"
	"encoding/hex"

	"github.com/treeol/wakil/internal/config"
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
	// Chat path: cfg.WorkspacePath(), the input to every chat store key.
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
	// the RAW ChatPath, not the canonical one) and SessionHostKey the directory
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
}

// ResolveWorkspaceKeys computes the report for cfg. The working directory and
// the WAKIL_WORKSPACE_PATH / WAKIL_WORKSPACE_KEY environment are read at call
// time.
func ResolveWorkspaceKeys(cfg config.Config) WorkspaceKeys {
	var k WorkspaceKeys
	k.ChatPath = ChatProjectPath(cfg)
	switch {
	case os.Getenv("WAKIL_WORKSPACE_PATH") != "":
		k.ChatPathSource = SourceEnvPath
	case cfg.ExecMode == "direct" && cfg.WorkDir != "":
		k.ChatPathSource = sourceConfigWorkDir
	case cfg.ExecMode != "direct" && cfg.HostWorkDir != "":
		k.ChatPathSource = sourceConfigHost
	default:
		k.ChatPathSource = SourceConfig
	}
	// When the config itself defaulted the directory to the cwd, say so.
	if k.ChatPathSource != SourceEnvPath {
		if wd, err := os.Getwd(); err == nil && sameDir(k.ChatPath, wd) {
			k.ChatPathSource = SourceCwd
		}
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
	return k
}

// SessionHostWorkspaceID is the sessionhost workspace id for a workspace path:
// "wsp_" + the first 8 bytes of sha256(raw path), hex. wiring.WorkspaceIDFromConfig
// computes the same value (locked by TestWorkspaceIDMatchesAgentReport); it is
// duplicated here only because wiring imports agent, not the other way round.
func SessionHostWorkspaceID(ws string) string {
	sum := sha256.Sum256([]byte(ws))
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
	fmt.Fprintf(&b, "sessionhost id:     %s   [source: sha256(raw workspace path) first 8 bytes]\n", orNone(k.SessionHostID))
	fmt.Fprintf(&b, "sessionhost key:    %s   [source: %s, truncated to 16 hex]\n", orNone(k.SessionHostKey), keySrc)
	fmt.Fprintf(&b, "  sessionhost db:   %s\n", orNone(k.SessionHostDBPath))
	fmt.Fprintf(&b, "plan gc from here:  %s   [source: %s]  memory key %s\n", orNone(k.GCPath), SourceCwd, orNone(k.GCMemoryKey))
	if k.ChatPath == "" {
		b.WriteString("NOTE: no workspace path (direct mode without a workdir argument): a chat started with this config keys no per-project store; pass the project directory as an argument or set WAKIL_WORKSPACE_PATH.\n")
	}
	if k.Diverges {
		fmt.Fprintf(&b, "WARNING: a chat started here keys its memory by %s but `wakil plan gc` from this directory reads %s — see %s\n",
			k.MemoryKey, k.GCMemoryKey, "docs/DECISIONS-2026-11.md (C4)")
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
	s := fmt.Sprintf("workspace: WAKIL_WORKSPACE_PATH=%s overrides the working directory — memory key %s, sessionhost key %s (id %s)",
		k.ChatPath, orNone(k.MemoryKey), orNone(k.SessionHostKey), orNone(k.SessionHostID))
	if k.Diverges {
		s += fmt.Sprintf("; `wakil plan gc` from %s would read memory key %s", k.Cwd, orNone(k.GCMemoryKey))
	}
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
