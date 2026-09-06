package agent

import (
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/core/sessionclient"
)

// TestGrantEligibilityParity ensures the agent's grantEligibleTools matches
// sessionclient's copy exactly — they must not drift (the TUI uses the
// sessionclient copy to decide whether to show the 'g' key; the agent uses
// its own to enforce the allowlist).
func TestGrantEligibilityParity(t *testing.T) {
	for tool := range grantEligibleTools {
		if !sessionclient.IsGrantEligible(tool) {
			t.Errorf("agent grants %q but sessionclient does not", tool)
		}
	}
	for tool := range sessionclient.GrantEligibleTools() {
		if !grantEligibleTools[tool] {
			t.Errorf("sessionclient grants %q but agent does not", tool)
		}
	}
}

func TestAddGrantRejectsIneligible(t *testing.T) {
	app := &App{}
	app.AddGrant("run_shell") // not eligible
	if len(app.Grants()) != 0 {
		t.Errorf("expected 0 grants after ineligible AddGrant, got %d", len(app.Grants()))
	}
}

func TestAddGrantDedup(t *testing.T) {
	app := &App{}
	app.AddGrant("read_file")
	app.AddGrant("read_file") // duplicate — should be deduped
	app.AddGrant("search_files")
	grants := app.Grants()
	if len(grants) != 2 {
		t.Errorf("expected 2 grants after dedup, got %d: %v", len(grants), grants)
	}
}

func TestHasGrant(t *testing.T) {
	app := &App{}
	if app.HasGrant("read_file") {
		t.Error("expected no grant before AddGrant")
	}
	app.AddGrant("read_file")
	if !app.HasGrant("read_file") {
		t.Error("expected grant after AddGrant")
	}
	if app.HasGrant("search_files") {
		t.Error("did not expect grant for search_files")
	}
}

func TestClearGrants(t *testing.T) {
	app := &App{}
	app.AddGrant("read_file")
	app.AddGrant("search_files")
	app.ClearGrants()
	if len(app.Grants()) != 0 {
		t.Errorf("expected 0 grants after ClearGrants, got %d", len(app.Grants()))
	}
}

func TestGrantsDefensiveCopy(t *testing.T) {
	app := &App{}
	app.AddGrant("read_file")
	g := app.Grants()
	g[0].Tool = "run_shell"
	// The internal state must not be affected by the caller mutating the returned slice.
	if app.HasGrant("run_shell") {
		t.Error("mutating Grants() return value affected internal state")
	}
	if !app.HasGrant("read_file") {
		t.Error("internal grant was corrupted by caller mutation")
	}
}

func TestConcurrentAddGrant(t *testing.T) {
	app := &App{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.AddGrant("read_file")
			app.AddGrant("search_files")
			app.AddGrant("list_dir")
		}()
	}
	wg.Wait()
	grants := app.Grants()
	if len(grants) != 3 {
		t.Errorf("expected 3 grants after concurrent AddGrant, got %d: %v", len(grants), grants)
	}
}
