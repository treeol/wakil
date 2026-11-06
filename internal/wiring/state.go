package wiring

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// RunState runs `wakil state ...`. Only `migrate --from-legacy` exists (C3).
func RunState(args []string, out, errw io.Writer) int {
	if len(args) >= 1 && args[0] == "migrate" {
		return runStateMigrate(args[1:], out, errw)
	}
	fmt.Fprintln(errw, "usage: wakil state migrate --from-legacy [--apply]")
	return 2
}

// runStateMigrate copies attributable legacy memory entries into project stores.
// Dry-run unless --apply. It never deletes and never writes the legacy store.
func runStateMigrate(args []string, out, errw io.Writer) int {
	apply, fromLegacy := false, false
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		case "--from-legacy":
			fromLegacy = true
		default:
			fmt.Fprintf(errw, "state migrate: unknown argument %q (usage: wakil state migrate --from-legacy [--apply])\n", a)
			return 2
		}
	}
	if !fromLegacy {
		fmt.Fprintln(errw, "state migrate: --from-legacy is required (usage: wakil state migrate --from-legacy [--apply])")
		return 2
	}
	// The legacy key depends on the configured exec mode and host_workdir
	// (agent.LegacyWorkspacePath), so the real config is loaded, exactly as a
	// chat would load it. A missing endpoint is a fatal error here, as for chat.
	cfg, err := config.LoadConfig(nil)
	if err != nil {
		fmt.Fprintln(errw, "state migrate: config:", err)
		return 1
	}
	legacyDB := agent.LegacyMemoryDBPath(cfg)
	if legacyDB == "" {
		fmt.Fprintln(errw, "state migrate: cannot determine the legacy store path")
		return 1
	}
	projects := knownProjects(cfg)
	already := map[string]map[int64]bool{}
	for _, p := range projects {
		m, merr := agent.LoadMigrationMarkers(p)
		if merr != nil {
			fmt.Fprintln(errw, "state migrate:", merr)
			return 1
		}
		already[p] = m
	}
	ctx := context.Background()
	plan, err := agent.PlanLegacyMigration(ctx, legacyDB, agent.LegacyWorkspacePath(cfg), projects, already)
	if err != nil {
		fmt.Fprintln(errw, "state migrate:", err)
		return 1
	}
	mode := "dry-run (nothing written; add --apply to copy)"
	if apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "legacy store: %s\nmode: %s\n", legacyDB, mode)
	toCopy := 0
	for _, n := range plan.PerProject {
		toCopy += n
	}
	fmt.Fprintf(out, "legacy entries: %d active; %d to copy, %d already copied, %d stay legacy (%d with conflicting evidence)\n",
		plan.Total, toCopy, plan.AlreadyTotal, plan.Unattributed, plan.Ambiguous)
	if plan.SharedWriter > 0 {
		fmt.Fprintf(out, "%d stay legacy (writer workspace was the shared store)\n", plan.SharedWriter)
	}
	projs := make([]string, 0, len(plan.PerProject))
	for p := range plan.PerProject {
		projs = append(projs, p)
	}
	sort.Strings(projs)
	for _, p := range projs {
		fmt.Fprintf(out, "  %-6d -> %s\n", plan.PerProject[p], p)
	}
	if !apply {
		fmt.Fprintln(out, "run with --apply to copy these (the legacy store is kept).")
		return 0
	}
	copied, newer, err := agent.ApplyLegacyMigration(ctx, plan)
	if err != nil {
		fmt.Fprintf(errw, "state migrate: copied %d before the error: %v\n", copied, err)
		return 1
	}
	fmt.Fprintf(out, "copied %d entries; skipped %d where the project already holds a newer value. The legacy store is unchanged.\n", copied, newer)
	return 0
}

// knownProjects is every project directory a chat has run in (from saved
// sessions), plus this launch directory. Only these are attribution targets.
func knownProjects(cfg config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(agent.ProjectDir(cfg))
	if sess, err := agent.ListSessions(); err == nil {
		for _, s := range sess {
			if s.Workspace != "" {
				add(filepath.Clean(s.Workspace))
			}
		}
	}
	return out
}

// LegacyNotice is the one first-start line for the project (0.5.0, D4), or "".
func LegacyNotice(cfg config.Config) string { return agent.LegacyNotice(cfg) }
