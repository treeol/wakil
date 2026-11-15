package agent

import (
	"sync"
	"testing"
)

var sessionClientGrantEligible = map[string]bool{
	"read_file":         true,
	"read_file_full":    true,
	"search_files":      true,
	"find_files":        true,
	"list_dir":          true,
	"lsp_definition":    true,
	"lsp_references":    true,
	"lsp_hover":         true,
	"lsp_symbols":       true,
	"write_file":        true,
	"write_binary_file": true,
	"edit_file":         true,
	"run_shell":         true,
	"run_background":    true,
}

// TestGrantEligibilityParity ensures the agent's grantEligibleTools matches
// the session-client copy exactly — the two maps are kept in sync here so
// the TUI's list cannot drift from the agent's enforcement.
func TestGrantEligibilityParity(t *testing.T) {
	for tool := range grantEligibleTools {
		if !sessionClientGrantEligible[tool] {
			t.Errorf("agent grants %q but the session-client list does not", tool)
		}
	}
	for tool := range sessionClientGrantEligible {
		if !grantEligibleTools[tool] {
			t.Errorf("session-client grants %q but agent does not", tool)
		}
	}
}

func TestAddGrantRejectsIneligible(t *testing.T) {
	for _, tool := range []string{"delete_file", "move_file", "external_backend"} {
		app := &App{}
		app.AddGrant(tool) // never grant-eligible
		if len(app.Grants()) != 0 {
			t.Errorf("expected 0 grants after ineligible AddGrant(%q), got %d", tool, len(app.Grants()))
		}
	}
}

func TestAddGrantAcceptsWriteAndShell(t *testing.T) {
	for _, tool := range []string{"write_file", "write_binary_file", "edit_file", "run_shell", "run_background"} {
		app := &App{}
		app.AddGrant(tool)
		if !app.HasGrant(tool) {
			t.Errorf("expected grant for %q", tool)
		}
	}
}

// A run_shell/run_background grant never covers a destructive command — the
// same IsDestructiveShell classification that suspends /auto.
func TestGrantEligibleForCallDestructiveShell(t *testing.T) {
	if GrantEligibleForCall("run_shell", "$ rm -rf /tmp\n  (direct)") {
		t.Error("destructive run_shell must not be grant-covered")
	}
	if GrantEligibleForCall("run_background", "$ rm -rf /tmp (background)") {
		t.Error("destructive run_background must not be grant-covered")
	}
	if !GrantEligibleForCall("run_shell", "$ ls -la\n  (direct)") {
		t.Error("read-only run_shell should be grant-covered")
	}
	if !GrantEligibleForCall("write_file", "path=x") {
		t.Error("write_file should be grant-covered")
	}
	if GrantEligibleForCall("delete_file", "path=x") {
		t.Error("delete_file is never grant-covered")
	}
	if GrantEligibleForCall("external_backend", "switching") {
		t.Error("external_backend is never grant-covered")
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
