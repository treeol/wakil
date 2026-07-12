package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
)

// RepoState is the persisted record of the last terminal settings used in a
// given workspace (repo/folder). It lives in a centralized per-user store
// (not the repo itself) so it never risks being committed, and so it works
// identically whether the workspace is trusted or untrusted.
//
// Deliberately absent: AllowDestructive (the /auto destructive grant) and
// any external-backend consent state. Neither has a field here, so no code
// path can accidentally serialize or restore them — the exclusion is
// structural, not just call-site discipline.
type RepoState struct {
	SchemaVersion int       `json:"schema_version"`
	Workspace     string    `json:"workspace"`
	EndpointName  string    `json:"endpoint_name,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`

	Model            string `json:"model,omitempty"`
	Backend          string `json:"backend,omitempty"`
	SubagentEndpoint string `json:"subagent_endpoint,omitempty"`
	SubagentModel    string `json:"subagent_model,omitempty"`
	RawTools         bool   `json:"raw_tools,omitempty"`

	// AutoApprove is restored in the TUI only. cmd/wakil/run.go never reads
	// or writes this field — see RestoreRepoState's doc comment.
	AutoApprove bool `json:"auto_approve,omitempty"`
}

const repoStateSchemaVersion = 1

// repoStateDir is where per-workspace terminal settings live:
// $WAKIL_REPO_STATE_DIR, else $XDG_DATA_HOME/wakil/repo-state, else
// ~/.local/share/wakil/repo-state. Mirrors sessionsDir()'s resolution order.
func repoStateDir() string {
	if x := os.Getenv("WAKIL_REPO_STATE_DIR"); x != "" {
		return x
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil", "repo-state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil", "repo-state")
}

// repoStateKey resolves ws to a stable, absolute, symlink-evaluated form and
// returns its SHA-256 hex digest for use as a filename. Falls back to Abs
// alone when EvalSymlinks fails (e.g. the directory doesn't exist yet) so a
// missing/racy path never breaks keying — both LoadRepoState and
// updateRepoState route through this single function so they always agree.
// Returns "" for an empty ws (callers treat that as "persistence disabled").
//
// Delegates to workspaceKey (workspace.go), the same canonicalization used
// for session workspace-scoping, so repo-state and session filtering always
// agree on what folder a path belongs to.
func repoStateKey(ws string) string {
	return workspaceKey(ws)
}

func repoStatePath(ws string) string {
	key := repoStateKey(ws)
	if key == "" {
		return ""
	}
	return filepath.Join(repoStateDir(), key+".json")
}

// LoadRepoState returns the stored settings for workspace ws, or (nil, nil)
// if there is nothing to restore: no ws, no file, or a file that fails a
// sanity check (wrong/missing schema version, workspace mismatch after
// re-resolution, or malformed JSON). None of those are treated as hard
// errors — a corrupted or stale repo-state file must never block startup.
func LoadRepoState(ws string) (*RepoState, error) {
	path := repoStatePath(ws)
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil //nolint:nilerr // unreadable file is treated as absent, not fatal
	}
	var st RepoState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, nil //nolint:nilerr // corrupt file is treated as absent, not fatal
	}
	if st.SchemaVersion != repoStateSchemaVersion {
		return nil, nil
	}
	return &st, nil
}

// updateRepoState loads the existing repo-state for ws (or starts a fresh
// one), applies mutate to change only the field(s) the caller just set, and
// writes the result back atomically (temp file + rename, same pattern as
// WriteSession). mutate must set fields from values already in the caller's
// scope — never by reading them back from other App state — so an unrelated
// setting is never accidentally re-snapshotted (see repo-state-plan.md fix
// #1). No-ops silently when ws is empty. Best-effort: errors are returned
// for callers that want to know, but callers in the TUI command path ignore
// them (a failed save must never interrupt the session).
func updateRepoState(ws string, mutate func(*RepoState)) error {
	if ws == "" {
		return nil
	}
	st, err := LoadRepoState(ws)
	if err != nil {
		return err
	}
	if st == nil {
		st = &RepoState{SchemaVersion: repoStateSchemaVersion, Workspace: ws}
	}
	mutate(st)
	st.SchemaVersion = repoStateSchemaVersion
	st.Workspace = ws
	st.UpdatedAt = time.Now()

	dir := repoStateDir()
	if dir == "" {
		return errors.New("cannot determine repo-state directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := repoStatePath(ws)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RestoreRepoStateResult carries the literal values RestoreRepoState applied
// this run, so the caller can re-resolve context limits using the exact
// strings ApplyModelOverride/SelectedBackend received — never by reading
// back through App state afterward. That distinction matters because
// ApplyModelOverride clears App.SelectedModel to "" for openai-kind
// endpoints; reading it back post-restore would silently probe context
// limits with an empty model string.
type RestoreRepoStateResult struct {
	Note    string // human-readable summary of what was restored, or "" if nothing applied
	Model   string // literal model string applied, or "" if none
	Backend string // literal backend name applied, or "" if none
}

// RestoreRepoState applies the stored settings for app's workspace, subject
// to this-run precedence: an explicit --model/ILM_MODEL always beats a
// restored model (app.Cfg.ModelExplicit), and an explicit --auto always
// beats a restored auto-approve state (app.Cfg.AutoExplicit). Must be called
// strictly after app is fully constructed (it reads app.Cfg.EndpointName,
// app.SessionWorkspace(), etc.) and only on a fresh conversation — callers
// resuming a saved session (--resume/--resume-id) must skip this entirely,
// so a remembered folder preference never silently changes the model or
// backend mid-transcript of a session that predates it.
//
// AutoApprove restore is meaningful only in the interactive TUI: headless
// wakil run never routes tool confirmation through App.AutoApprove (it
// checks RunFlags.Auto directly in headlessConfirmer — see cmd/wakil/run.go),
// so cmd/wakil/run.go must never call this function. Restoring AutoApprove
// there would silently create a new unattended-approval path that bypasses
// --auto. This function does not itself distinguish TUI from headless
// callers; the constraint is enforced by never wiring this call into
// cmd/wakil/run.go, not by a runtime check here.
//
// AllowDestructive is never touched — RepoState has no field for it.
func RestoreRepoState(app *App) RestoreRepoStateResult {
	ws := app.SessionWorkspace()
	st, err := LoadRepoState(ws)
	if err != nil || st == nil {
		return RestoreRepoStateResult{}
	}

	var applied []string
	var result RestoreRepoStateResult

	// A model/backend string recorded under one endpoint is meaningless sent
	// to another (different auth, different model namespace) — skip both
	// when the active endpoint has changed since the state was written.
	endpointMatches := st.EndpointName == "" || st.EndpointName == app.Cfg.EndpointName
	if endpointMatches {
		if !app.Cfg.ModelExplicit && st.Model != "" {
			ApplyModelOverride(app, st.Model)
			result.Model = st.Model
			applied = append(applied, "model="+st.Model)
		}
		if st.Backend != "" && app.Cfg.ActiveEndpoint().Kind == config.EndpointKindIlmProxy {
			// ilm-proxy kind only — openai-kind /backend reconfigures the
			// whole endpoint (kind/base_url/auth/sampling) via
			// handleEndpointSwitch and is deliberately not persisted here.
			app.SelectedBackend = st.Backend
			result.Backend = st.Backend
			applied = append(applied, "backend="+st.Backend)
		}
	}

	if st.SubagentEndpoint != "" {
		if _, err := app.Cfg.NormalizeEndpoint(st.SubagentEndpoint); err == nil {
			app.SubagentEndpointOverride = st.SubagentEndpoint
			applied = append(applied, "subagent="+st.SubagentEndpoint)
		}
		// Stale/missing endpoint (no longer in config): silently skipped,
		// never hard-fails startup.
	}
	if st.SubagentModel != "" {
		app.SubagentModelOverride = st.SubagentModel
		applied = append(applied, "submodel="+st.SubagentModel)
	}

	app.RawTools = st.RawTools // always applies; false is a valid restored value

	if !app.Cfg.AutoExplicit {
		app.AutoApprove = st.AutoApprove
		if st.AutoApprove {
			applied = append(applied, "auto=on")
		}
	}

	if len(applied) > 0 {
		result.Note = "repo-state: restored " + strings.Join(applied, ", ") +
			" (folder: " + ws + ") — /repostate to inspect or clear"
	}
	return result
}

// SaveRepoState is a thin convenience wrapper for command handlers that only
// need to patch one or two fields. Prefer calling updateRepoState directly
// when a command needs to set multiple related fields atomically (e.g.
// /backend setting both Backend and Model in one write).
func (a *App) saveRepoState(mutate func(*RepoState)) {
	_ = updateRepoState(a.SessionWorkspace(), mutate)
}

// ClearRepoState deletes the stored settings for app's workspace, if any.
// Used by /repostate clear. Never errors on a missing file.
func ClearRepoState(app *App) error {
	path := repoStatePath(app.SessionWorkspace())
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// DescribeRepoState renders the /repostate (no-arg) output: the resolved
// workspace, where its state file would live, and its stored contents (or a
// note that none exist yet).
func DescribeRepoState(app *App) string {
	ws := app.SessionWorkspace()
	if ws == "" {
		return "repo-state: no workspace resolved for this session (nothing to persist)"
	}
	path := repoStatePath(ws)
	st, err := LoadRepoState(ws)
	if err != nil || st == nil {
		return "repo-state: none yet for " + ws + "\n  (would be written to " + path + ")"
	}
	var b strings.Builder
	b.WriteString("repo-state for " + ws + ":\n")
	b.WriteString("  file: " + path + "\n")
	b.WriteString("  updated: " + st.UpdatedAt.Format("2006-01-02 15:04:05") + "\n")
	if st.EndpointName != "" {
		b.WriteString("  endpoint: " + st.EndpointName + "\n")
	}
	if st.Model != "" {
		b.WriteString("  model: " + st.Model + "\n")
	}
	if st.Backend != "" {
		b.WriteString("  backend: " + st.Backend + "\n")
	}
	if st.SubagentEndpoint != "" {
		b.WriteString("  subagent endpoint: " + st.SubagentEndpoint + "\n")
	}
	if st.SubagentModel != "" {
		b.WriteString("  subagent model: " + st.SubagentModel + "\n")
	}
	if st.RawTools {
		b.WriteString("  raw tools: on\n")
	}
	if st.AutoApprove {
		b.WriteString("  auto: on\n")
	}
	b.WriteString("(/repostate clear to delete this file)")
	return b.String()
}
