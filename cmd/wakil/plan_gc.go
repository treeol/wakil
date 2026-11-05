package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// planGCVerdict classifies one quarantined plan entry for `wakil plan gc`.
type planGCVerdict int

const (
	planGCSkip  planGCVerdict = iota // not a plan key this command handles
	planGCAlive                      // owning session file exists
	planGCDead                       // owning session file is gone
	planGCKeep                       // owner unknown — never delete what we cannot attribute
)

// classifyPlanEntry decides the fate of one PROPOSED plan entry. sessionExists
// is the liveness probe (the session file for a chat ID); it is injected so
// the policy is testable without a sessions directory.
//
// Policy (data-loss guard, Mashūra round 1):
//   - only plan/current and plan/tainted/<chatID>/current are considered;
//   - the owner is the chat ID in the key for plan/tainted/*, else the
//     entry's SessionID; an entry with no attributable owner is KEPT;
//   - liveness is "the session file exists" — nothing else.
func classifyPlanEntry(key, sessionID string, sessionExists func(chatID string) bool) planGCVerdict {
	owner := sessionID
	switch {
	case key == "plan/current":
	case strings.HasPrefix(key, "plan/tainted/"):
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[3] != "current" {
			return planGCSkip
		}
		owner = parts[2]
	default:
		return planGCSkip
	}
	if owner == "" {
		return planGCKeep
	}
	if sessionExists(owner) {
		return planGCAlive
	}
	return planGCDead
}

// runPlanGC runs the `wakil plan gc` subcommand.
//
// It looks ONLY at PROPOSED (quarantined) entries under plan/current and
// plan/tainted/<chatID>/current whose owning session no longer has a session
// file. An ACTIVE plan/current — the one a handoff reads — is never listed
// and never touched. Dry-run by default; --apply rejects the dead entries via
// MemoryStore.Reject (status → rejected, recoverable in the DB; nothing is
// hard-deleted and the key is not forgotten).
func runPlanGC(args []string) int {
	apply := false
	var projectDir, storePath string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--apply":
			apply = true
		case a == "--project" || a == "--store":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				fmt.Fprintf(os.Stderr, "plan gc: %s needs a value\n", a)
				return ExitError
			}
			if a == "--project" {
				projectDir = args[i+1]
			} else {
				storePath = args[i+1]
			}
			i++
		default:
			// An unknown or misspelled flag must fail, not fall back to a dry
			// run that looks like a normal result.
			fmt.Fprintf(os.Stderr, "plan gc: unknown argument %q (usage: wakil plan gc [--apply] [--project <dir>] [--store <path>])\n", a)
			return ExitError
		}
	}

	cfg, err := config.LoadConfig(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return ExitError
	}

	ws, err := agentWorkspace(cfg, projectDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitError
	}
	store, memDBPath, err := agent.PlanGCStore(ws, storePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitError
	}
	fmt.Fprintf(os.Stdout, "store: %s\n", memDBPath)
	defer store.Close()

	ctx := context.Background()

	entries, err := store.List(ctx, "plan/", "", memory.StatusProposed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory list error:", err)
		return ExitError
	}

	// Only a confirmed absence (ENOENT) counts as death. Any other stat error
	// (permissions, I/O) means the session state is unknown, so the entry is
	// kept: --apply must never reject a row whose owner we could not check.
	exists := planGCSessionExists

	var dead []*memory.Entry
	alive, kept := 0, 0
	for _, e := range entries {
		switch classifyPlanEntry(e.Key, e.SessionID, exists) {
		case planGCDead:
			dead = append(dead, e)
		case planGCAlive:
			alive++
		case planGCKeep:
			kept++
		}
	}

	fmt.Fprintf(os.Stdout, "plan gc: %d quarantined plan entries, %d dead, %d alive, %d kept (owner unknown)\n",
		len(dead)+alive+kept, len(dead), alive, kept)
	for _, e := range dead {
		fmt.Fprintf(os.Stdout, "  dead: %s [id: %d]\n", e.Key, e.ID)
	}

	if !apply {
		if len(dead) > 0 {
			fmt.Fprintf(os.Stdout, "dry run — %d dead entries would be rejected (pass --apply)\n", len(dead))
		}
		return ExitOK
	}

	rejected := 0
	failed := 0
	for _, e := range dead {
		if err := store.Reject(ctx, e.ID, "plan gc: owning session no longer exists"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: reject %s [id: %d]: %v\n", e.Key, e.ID, err)
			failed++
			continue
		}
		rejected++
	}
	fmt.Fprintf(os.Stdout, "plan gc: rejected %d of %d dead entries\n", rejected, len(dead))
	if failed > 0 {
		// A partial failure is not success: the caller must see a non-zero exit.
		return ExitError
	}
	return ExitOK
}

// agentWorkspace picks the workspace gc resolves the store for. Without
// --project it is the current directory, made absolute and symlink-resolved —
// the same key a chat started in that directory gets (workspaceKey canonicalizes
// with Abs+EvalSymlinks). It deliberately does NOT use cfg.WorkspacePath():
// that honours WAKIL_WORKSPACE_PATH and the config workspace, so gc could open
// the store of a different project than the one the user is standing in.
func agentWorkspace(_ config.Config, projectDir string) (string, error) {
	return ResolveProject(projectDir)
}

// ResolveProject returns the canonical project directory and the store key for
// the CLI's cwd-based identity (flagDir, else cwd): Abs then EvalSymlinks, then
// agent.WorkspaceKey. It is the gc source of truth. It deliberately ignores
// WAKIL_WORKSPACE_PATH and the config workspace; chat keys by
// config.Config.WorkspacePath() instead (see agent.ChatProjectPath). With
// WAKIL_WORKSPACE_PATH unset, both resolve to the same store for the same
// directory (locked by TestResolveProjectMatchesChatForCwd).
func ResolveProject(flagDir string) (string, error) {
	dir := flagDir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("plan gc: cannot determine current directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("plan gc: resolving %q: %w", dir, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}

// planGCSessionExists reports whether a session file for chatID may exist.
// Only a confirmed ENOENT returns false (the owner is provably gone). Any
// other stat error returns true: an unreadable owner is unknown, not dead.
func planGCSessionExists(chatID string) bool {
	if !planGCSessionsDirPresent() {
		return true // no sessions directory to check against: cannot prove death
	}
	p := agent.SessionFilePath(chatID)
	if p == "" {
		return true // no sessions dir configured: cannot prove death
	}
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

// planGCSessionsDirPresent reports whether the sessions directory exists. If it
// is missing (for example WAKIL_SESSIONS_DIR points at the wrong place), every
// session file would look absent and --apply would reject every quarantined
// plan. A missing directory proves nothing about any owner, so gc treats all
// owners as unknown instead.
func planGCSessionsDirPresent() bool {
	dir := agent.SessionsDir()
	if dir == "" {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}
