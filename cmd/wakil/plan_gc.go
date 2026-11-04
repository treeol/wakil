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

// runPlanGC runs the `wakil plan gc` subcommand: lists memory entries under
// plan/current (and plan/tainted/) whose owning session no longer has a
// session file on disk, and optionally deletes them.
//
// Dry-run (default): prints counts and the dead entry keys.
// --apply: deletes the dead entries via MemoryStore.Forget.
//
// The liveness check is the same /resume uses: the session file exists and
// is loadable from the sessions directory. A plan/current entry is dead when
// its SessionID's session file no longer exists. A plan/tainted/<chatID>/
// entry is dead when <chatID>'s session file no longer exists.
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

	// List all plan/ entries (plan/current and plan/tainted/).
	entries, err := store.List(ctx, "plan/", "", memory.StatusActive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory list error:", err)
		return ExitError
	}

	sessDir := agent.SessionsDir()
	var dead, alive []*memory.Entry
	for _, e := range entries {
		// Only consider plan/current and plan/tainted/<chatID>/current keys.
		if e.Key != "plan/current" && !strings.HasPrefix(e.Key, "plan/tainted/") {
			continue
		}
		// Determine the owning chatID for the session-file liveness check.
		chatID := e.SessionID
		if strings.HasPrefix(e.Key, "plan/tainted/") {
			// plan/tainted/<chatID>/current — extract chatID from the key.
			parts := strings.SplitN(e.Key, "/", 4)
			if len(parts) >= 3 {
				chatID = parts[2]
			}
		}
		if chatID == "" {
			// No sessionID and no chatID in key — treat as dead.
			dead = append(dead, e)
			continue
		}
		path := filepath.Join(sessDir, chatID+".json")
		if _, err := os.Stat(path); err != nil {
			dead = append(dead, e)
		} else {
			alive = append(alive, e)
		}
	}

	fmt.Fprintf(os.Stdout, "plan gc: %d entries, %d dead, %d alive\n", len(dead)+len(alive), len(dead), len(alive))
	for _, e := range dead {
		fmt.Fprintf(os.Stdout, "  dead: %s [id: %d]\n", e.Key, e.ID)
	}

	if !apply {
		if len(dead) > 0 {
			fmt.Fprintf(os.Stdout, "dry run — %d dead entries would be deleted (pass --apply)\n", len(dead))
		}
		return ExitOK
	}

	deleted := 0
	for _, e := range dead {
		if err := store.Forget(ctx, e.Key); err != nil {
			fmt.Fprintf(os.Stderr, "warning: forget %s: %v\n", e.Key, err)
			continue
		}
		deleted++
	}
	fmt.Fprintf(os.Stdout, "plan gc: deleted %d of %d dead entries\n", deleted, len(dead))
	return ExitOK
}
