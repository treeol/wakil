package main

import (
	"context"
	"fmt"
	"os"
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
	for _, a := range args {
		if a == "--apply" || a == "-apply" {
			apply = true
		}
	}

	cfg, err := config.LoadConfig(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return ExitError
	}

	memDBPath := agent.MemoryDBPath(cfg.WorkDir)
	if memDBPath == "" {
		fmt.Fprintln(os.Stderr, "cannot determine memory DB path")
		return ExitError
	}
	store, err := memory.Open(memDBPath, cfg.WorkDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory open error:", err)
		return ExitError
	}
	defer store.Close()

	ctx := context.Background()

	entries, err := store.List(ctx, "plan/", "", memory.StatusProposed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory list error:", err)
		return ExitError
	}

	exists := func(chatID string) bool {
		p := agent.SessionFilePath(chatID)
		if p == "" {
			return true // no sessions dir configured: cannot prove death
		}
		_, err := os.Stat(p)
		return err == nil
	}

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
	for _, e := range dead {
		if err := store.Reject(ctx, e.ID, "plan gc: owning session no longer exists"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: reject %s [id: %d]: %v\n", e.Key, e.ID, err)
			continue
		}
		rejected++
	}
	fmt.Fprintf(os.Stdout, "plan gc: rejected %d of %d dead entries\n", rejected, len(dead))
	return ExitOK
}
