package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// ─── buildPreamble ───────────────────────────────────────────────────────────

// preambleTestApp builds a minimal App for buildPreamble tests. Only the fields
// buildPreamble reads are populated; everything else is zero-valued.
func preambleTestApp() *App {
	return &App{
		Cfg: config.DefaultConfig(),
	}
}

func TestBuildPreamble_DateAlwaysPresent(t *testing.T) {
	app := preambleTestApp()
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Current date: Monday, 1 January 2026") {
		t.Errorf("preamble missing date line, got:\n%s", got)
	}
	if !strings.Contains(got, "Treat this as the present moment") {
		t.Errorf("preamble missing 'present moment' guidance, got:\n%s", got)
	}
}

func TestBuildPreamble_NoAgentPrompt(t *testing.T) {
	app := preambleTestApp() // AgentPrompt is ""
	got := app.buildPreamble("Monday, 1 January 2026")
	// The date line should be the first thing in the preamble when there's no
	// agent prompt — no empty leading line.
	if strings.HasPrefix(got, "\n") {
		t.Errorf("preamble should not start with a newline when AgentPrompt is empty")
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("preamble should not contain blank lines when no optional sections are present, got:\n%s", got)
	}
}

func TestBuildPreamble_WithAgentPrompt(t *testing.T) {
	app := preambleTestApp()
	app.AgentPrompt = "You are a helpful assistant."
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.HasPrefix(got, "You are a helpful assistant.") {
		t.Errorf("preamble should start with AgentPrompt, got:\n%s", got)
	}
	if !strings.Contains(got, "Current date:") {
		t.Errorf("preamble missing date after agent prompt")
	}
}

func TestBuildPreamble_CwdFromExec(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{} // Cwd() returns "/work"
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Working directory: /work") {
		t.Errorf("preamble missing cwd from Exec, got:\n%s", got)
	}
}

func TestBuildPreamble_CwdFromCfgWhenNoExec(t *testing.T) {
	app := preambleTestApp()
	app.Exec = nil
	app.Cfg.WorkDir = "/custom/path"
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Working directory: /custom/path") {
		t.Errorf("preamble missing cwd from Cfg.WorkDir, got:\n%s", got)
	}
}

func TestBuildPreamble_NoCwdWhenNoExecNoWorkDir(t *testing.T) {
	app := preambleTestApp()
	app.Exec = nil
	app.Cfg.WorkDir = ""
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "Working directory:") {
		t.Errorf("preamble should not contain cwd when both Exec and WorkDir are empty, got:\n%s", got)
	}
}

func TestBuildPreamble_DockerMountSuffix(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{}                // Cwd = /work
	app.Cfg.ExecMode = "docker"              // enable docker mode
	app.Cfg.HostWorkDir = "/home/user/project" // different from /work
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Working directory: /work (mounted from host: /home/user/project)") {
		t.Errorf("preamble missing docker mount suffix, got:\n%s", got)
	}
}

func TestBuildPreamble_DockerNoMountSuffixWhenSamePath(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{}      // Cwd = /work
	app.Cfg.ExecMode = "docker"
	app.Cfg.HostWorkDir = "/work" // same as Cwd → no mount suffix
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "mounted from host") {
		t.Errorf("preamble should not have mount suffix when HostWorkDir == Cwd, got:\n%s", got)
	}
	if !strings.Contains(got, "Working directory: /work.") {
		t.Errorf("preamble should still contain cwd without mount suffix, got:\n%s", got)
	}
}

func TestBuildPreamble_DockerNoMountSuffixInDirectMode(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{}
	app.Cfg.ExecMode = "direct"
	app.Cfg.HostWorkDir = "/home/user/project"
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "mounted from host") {
		t.Errorf("preamble should not have mount suffix in direct mode, got:\n%s", got)
	}
}

func TestBuildPreamble_SandboxTools(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{sandboxTools: "git 2.39, go 1.26, rustc 1.97"}
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "git 2.39, go 1.26, rustc 1.97.") {
		t.Errorf("preamble missing sandbox tools, got:\n%s", got)
	}
}

func TestBuildPreamble_NoSandboxToolsWhenEmpty(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{sandboxTools: ""}
	got := app.buildPreamble("Monday, 1 January 2026")
	// The sandbox tools line should not appear at all when empty.
	// Check that no line ends with a bare period from an empty tools entry.
	for _, line := range strings.Split(got, "\n") {
		if line == "." || strings.HasSuffix(line, "\n.") {
			t.Errorf("preamble should not contain bare-period line from empty tools, got line: %q", line)
		}
	}
}

func TestBuildPreamble_NoSandboxToolsWhenNoExec(t *testing.T) {
	app := preambleTestApp()
	app.Exec = nil
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "Sandbox tools:") {
		t.Errorf("preamble should not include sandbox tools when no Exec, got:\n%s", got)
	}
}

func TestBuildPreamble_LSP(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{}
	app.LSP = &lsp.Manager{} // non-nil is enough; buildPreamble doesn't call methods on it
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "LSP code intelligence available") {
		t.Errorf("preamble missing LSP section, got:\n%s", got)
	}
	if !strings.Contains(got, "lsp_definition") {
		t.Errorf("preamble LSP section missing tool names, got:\n%s", got)
	}
}

func TestBuildPreamble_Browser(t *testing.T) {
	app := preambleTestApp()
	app.Exec = &fakeExecutor{}
	app.Browser = &browser.Manager{} // non-nil is enough
	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Headless browser available") {
		t.Errorf("preamble missing browser section, got:\n%s", got)
	}
	if !strings.Contains(got, "browser_screenshot") {
		t.Errorf("preamble browser section missing tool names, got:\n%s", got)
	}
}

func TestBuildPreamble_MemoryDigest(t *testing.T) {
	app, cleanup := preambleMemoryTestApp(t)
	defer cleanup()

	ctx := context.Background()
	// Store a durable entry so Stats returns nonzero counts.
	_, err := app.MemoryStore.PutActive(ctx, "arch/test-entry",
		"Test content for memory digest.", "note",
		memory.TierDurable, "main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Memory:") {
		t.Errorf("preamble missing memory digest section, got:\n%s", got)
	}
	if !strings.Contains(got, "1 active durable") {
		t.Errorf("preamble memory section should show 1 active durable entry, got:\n%s", got)
	}
	if !strings.Contains(got, "arch/test-entry") {
		t.Errorf("preamble should list recent key 'arch/test-entry', got:\n%s", got)
	}
	if !strings.Contains(got, "memory_search") {
		t.Errorf("preamble should mention memory_search, got:\n%s", got)
	}
}

func TestBuildPreamble_NoMemoryDigestWhenEmpty(t *testing.T) {
	app, cleanup := preambleMemoryTestApp(t)
	defer cleanup()
	// No entries stored — Stats returns all zeros → no memory line.
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "Memory:") {
		t.Errorf("preamble should not contain memory section when empty, got:\n%s", got)
	}
}

func TestBuildPreamble_NoMemoryDigestWhenNilStore(t *testing.T) {
	app := preambleTestApp()
	app.MemoryStore = nil
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "Memory:") {
		t.Errorf("preamble should not contain memory section when MemoryStore is nil, got:\n%s", got)
	}
}

func TestBuildPreamble_MemoryMidTierAndPending(t *testing.T) {
	app, cleanup := preambleMemoryTestApp(t)
	defer cleanup()

	ctx := context.Background()
	// Store a mid-tier entry (shows as "mid-tier" in the digest).
	_, err := app.MemoryStore.PutActive(ctx, "scratch/temp",
		"Temporary note.", "note",
		memory.TierMid, "main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// Store a proposed (pending) durable entry.
	_, err = app.MemoryStore.PutProposed(ctx, "decision/pending-test",
		"Pending decision.", "decision",
		"main", "s1", memory.TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	got := app.buildPreamble("Monday, 1 January 2026")
	if !strings.Contains(got, "Memory:") {
		t.Errorf("preamble missing memory digest, got:\n%s", got)
	}
	if !strings.Contains(got, "1 mid-tier") {
		t.Errorf("preamble should show 1 mid-tier entry, got:\n%s", got)
	}
	if !strings.Contains(got, "1 pending proposal") {
		t.Errorf("preamble should show 1 pending proposal, got:\n%s", got)
	}
}

func TestBuildPreamble_KitchenSink(t *testing.T) {
	app, cleanup := preambleMemoryTestApp(t)
	defer cleanup()

	ctx := context.Background()
	_, err := app.MemoryStore.PutActive(ctx, "arch/kitchen-sink",
		"Full preamble test.", "note",
		memory.TierDurable, "main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	app.AgentPrompt = "You are Wakil."
	app.Exec = &fakeExecutor{sandboxTools: "git 2.39, go 1.26"}
	app.Cfg.ExecMode = "docker"
	app.Cfg.HostWorkDir = "/home/user/wakil"
	app.LSP = &lsp.Manager{}
	app.Browser = &browser.Manager{}

	got := app.buildPreamble("Monday, 1 January 2026")

	// Verify all sections present and in order.
	checks := []struct {
		name    string
		substr  string
	}{
		{"agent_prompt", "You are Wakil."},
		{"date", "Current date: Monday, 1 January 2026"},
		{"cwd", "Working directory: /work (mounted from host: /home/user/wakil)"},
		{"sandbox_tools", "git 2.39, go 1.26."},
		{"lsp", "LSP code intelligence available"},
		{"browser", "Headless browser available"},
		{"memory", "Memory:"},
	}
	prevIdx := 0
	for _, c := range checks {
		idx := strings.Index(got, c.substr)
		if idx < 0 {
			t.Errorf("preamble missing %s section", c.name)
			continue
		}
		if idx < prevIdx {
			t.Errorf("preamble section %s (idx %d) appeared before previous section (idx %d) — order wrong", c.name, idx, prevIdx)
		}
		prevIdx = idx
	}
}

// memoryTestApp creates an App with a real on-disk memory store in a temp dir.
func preambleMemoryTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory", "test.db")
	store, err := memory.Open(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg:         config.DefaultConfig(),
		MemoryStore: store,
	}
	return app, func() { store.Close() }
}

// ─── RecordInferenceCost ────────────────────────────────────────────────────

// TestRecordInferenceCost_NilGuards verifies the early-return guards: nil Costs,
// nil Client, and zero-usage all result in no-op (no panic, no row recorded).
func TestRecordInferenceCost_NilGuards(t *testing.T) {
	t.Run("nil_costs", func(t *testing.T) {
		app := &App{
			Cfg:    config.DefaultConfig(),
			Client: &proxy.Client{Model: "test"},
			Costs:  nil,
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1000, OutputTok: 500, Exact: true})
		// Should not panic (nil-safe tracker — all methods are no-ops).
		app.RecordInferenceCost()
	})

	t.Run("nil_client", func(t *testing.T) {
		app := &App{
			Cfg:    config.DefaultConfig(),
			Client: nil,
			Costs:  proxy.NewCostTracker(),
		}
		// Should not panic.
		app.RecordInferenceCost()
		_, rows := app.Costs.Snapshot()
		if len(rows) != 0 {
			t.Errorf("expected 0 rows with nil client, got %d", len(rows))
		}
	})

	t.Run("zero_usage", func(t *testing.T) {
		app := &App{
			Cfg:    config.DefaultConfig(),
			Client: &proxy.Client{Model: "test"},
			Costs:  proxy.NewCostTracker(),
		}
		// Zero tokens → no-op.
		app.RecordInferenceCost()
		_, rows := app.Costs.Snapshot()
		if len(rows) != 0 {
			t.Errorf("expected 0 rows with zero usage, got %d", len(rows))
		}
	})
}

// TestRecordInferenceCost_NoBackendLegacy verifies the legacy path: when no
// backend is known (usedBackend == ""), the source key is the aggregate
// "inference" and confidence is ConfModeled (or ConfApprox when Exact=false).
func TestRecordInferenceCost_NoBackendLegacy(t *testing.T) {
	t.Run("modeled", func(t *testing.T) {
		app := &App{
			Cfg: config.Config{
				Costs: config.CostsConfig{
					Inference: config.InferenceRate{USDPer1MTokens: 5},
				},
			},
			Client: &proxy.Client{Model: "test"},
			Costs:  proxy.NewCostTracker(),
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: true})
		// No SetLastUsedBackend → usedBackend == ""
		app.RecordInferenceCost()

		_, rows := app.Costs.Snapshot()
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		row := rows[0]
		if row.Source != proxy.CostSourceInference {
			t.Errorf("Source = %q, want %q", row.Source, proxy.CostSourceInference)
		}
		// (1M + 500k) / 1M * 5 = 7.5
		if row.CostUSD != 7.5 {
			t.Errorf("CostUSD = %v, want 7.5", row.CostUSD)
		}
		if row.Confidence != proxy.ConfModeled {
			t.Errorf("Confidence = %q, want %q", row.Confidence, proxy.ConfModeled)
		}
		if !row.Priced {
			t.Error("expected Priced=true")
		}
	})

	t.Run("approx_when_not_exact", func(t *testing.T) {
		app := &App{
			Cfg: config.Config{
				Costs: config.CostsConfig{
					Inference: config.InferenceRate{USDPer1MTokens: 5},
				},
			},
			Client: &proxy.Client{Model: "test"},
			Costs:  proxy.NewCostTracker(),
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: false})
		app.RecordInferenceCost()

		_, rows := app.Costs.Snapshot()
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Confidence != proxy.ConfApprox {
			t.Errorf("Confidence = %q, want %q (Exact=false → approx)", rows[0].Confidence, proxy.ConfApprox)
		}
	})

	t.Run("unpriced_when_no_rate", func(t *testing.T) {
		app := &App{
			Cfg:    config.DefaultConfig(), // no Inference rate configured
			Client: &proxy.Client{Model: "test"},
			Costs:  proxy.NewCostTracker(),
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: true})
		app.RecordInferenceCost()

		_, rows := app.Costs.Snapshot()
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Priced {
			t.Error("expected Priced=false when no inference rate configured")
		}
	})
}

// TestRecordInferenceCost_LocalBackend verifies the local-backend path: a known
// backend that is NOT external → source is "inference·<backend>", confidence is
// ConfModeled (or ConfApprox when Exact=false), cost from the flat Inference
// rate (cached tokens folded into InputTok, no split-rate).
func TestRecordInferenceCost_LocalBackend(t *testing.T) {
	t.Run("modeled_exact", func(t *testing.T) {
		app := &App{
			Cfg: config.Config{
				Costs: config.CostsConfig{
					Inference: config.InferenceRate{USDPer1MTokens: 5},
				},
			},
			Client: &proxy.Client{Model: "llama"},
			Costs:  proxy.NewCostTracker(),
			BackendList: []BackendInfo{
				{Name: "llama", External: false},
			},
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: true})
		app.Client.SetLastUsedBackend("llama")
		app.RecordInferenceCost()

		_, rows := app.Costs.Snapshot()
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		row := rows[0]
		wantSource := proxy.CostSourceInfPrefix + "llama"
		if row.Source != wantSource {
			t.Errorf("Source = %q, want %q", row.Source, wantSource)
		}
		// (1M + 500k) / 1M * 5 = 7.5 (flat rate, no split)
		if row.CostUSD != 7.5 {
			t.Errorf("CostUSD = %v, want 7.5 (flat local rate)", row.CostUSD)
		}
		if row.Confidence != proxy.ConfModeled {
			t.Errorf("Confidence = %q, want %q", row.Confidence, proxy.ConfModeled)
		}
	})

	t.Run("approx_when_not_exact", func(t *testing.T) {
		app := &App{
			Cfg: config.Config{
				Costs: config.CostsConfig{
					Inference: config.InferenceRate{USDPer1MTokens: 5},
				},
			},
			Client: &proxy.Client{Model: "llama"},
			Costs:  proxy.NewCostTracker(),
			BackendList: []BackendInfo{
				{Name: "llama", External: false},
			},
		}
		app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: false})
		app.Client.SetLastUsedBackend("llama")
		app.RecordInferenceCost()

		_, rows := app.Costs.Snapshot()
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		if rows[0].Confidence != proxy.ConfApprox {
			t.Errorf("Confidence = %q, want %q (local + Exact=false → approx)", rows[0].Confidence, proxy.ConfApprox)
		}
	})
}

// TestRecordInferenceCost_ExternalApprox verifies that an external backend with
// Exact=false produces ConfApprox (not ConfExact).
func TestRecordInferenceCost_ExternalApprox(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/openai/gpt-4o": {InputUSDPer1M: 10, OutputUSDPer1M: 30},
				},
			},
		},
		Client: &proxy.Client{Model: "openai/gpt-4o"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "openrouter", External: true},
		},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, Exact: false})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Confidence != proxy.ConfApprox {
		t.Errorf("Confidence = %q, want %q (external + Exact=false → approx)", row.Confidence, proxy.ConfApprox)
	}
	// Source should be "inference·openrouter/openai/gpt-4o"
	wantSource := proxy.CostSourceInfPrefix + "openrouter/openai/gpt-4o"
	if row.Source != wantSource {
		t.Errorf("Source = %q, want %q", row.Source, wantSource)
	}
}

// TestRecordInferenceCost_ExternalExactConfidence verifies that an external
// backend with Exact=true produces ConfExact — the confidence the existing
// cachedtok tests don't explicitly assert.
func TestRecordInferenceCost_ExternalExactConfidence(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/openai/gpt-4o": {InputUSDPer1M: 10, OutputUSDPer1M: 30},
				},
			},
		},
		Client: &proxy.Client{Model: "openai/gpt-4o"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "openrouter", External: true},
		},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Confidence != proxy.ConfExact {
		t.Errorf("Confidence = %q, want %q (external + Exact=true → exact)", rows[0].Confidence, proxy.ConfExact)
	}
}

// TestRecordInferenceCost_ExternalUnpriced verifies that an external backend
// with no matching InferenceBackends rate still records a row (Priced=false,
// CostUSD=0) so the source renders "—" rather than being silently dropped.
func TestRecordInferenceCost_ExternalUnpriced(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				// No rate for "openrouter/some-model" configured.
				InferenceBackends: map[string]config.ModelRate{},
			},
		},
		Client: &proxy.Client{Model: "some-model"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "openrouter", External: true},
		},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (unpriced external still records), got %d", len(rows))
	}
	row := rows[0]
	if row.Priced {
		t.Error("expected Priced=false for external backend with no rate")
	}
	if row.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 (unpriced)", row.CostUSD)
	}
	// Confidence is still exact — the usage data is real, just unpriced.
	if row.Confidence != proxy.ConfExact {
		t.Errorf("Confidence = %q, want %q", row.Confidence, proxy.ConfExact)
	}
}

// TestRecordInferenceCost_ModelPrefixStripping verifies that when the model
// field already carries the backend prefix (e.g.
// "openrouter/anthropic/claude-opus-4-8"), the prefix is stripped so the cost
// key is not doubled.
func TestRecordInferenceCost_ModelPrefixStripping(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/anthropic/claude-opus-4-8": {InputUSDPer1M: 15, OutputUSDPer1M: 75},
				},
			},
		},
		Client: &proxy.Client{Model: "openrouter/anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "openrouter", External: true},
		},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	// Source should NOT be doubled: "inference·openrouter/anthropic/claude-opus-4-8"
	// not "inference·openrouter/openrouter/anthropic/claude-opus-4-8"
	wantSource := proxy.CostSourceInfPrefix + "openrouter/anthropic/claude-opus-4-8"
	if row.Source != wantSource {
		t.Errorf("Source = %q, want %q (prefix should be stripped)", row.Source, wantSource)
	}
	if strings.Count(row.Source, "openrouter/") != 1 {
		t.Errorf("source should contain 'openrouter/' exactly once, got: %s", row.Source)
	}
	// 1M * 15/1M + 100k * 75/1M = 15 + 7.5 = 22.5
	if row.CostUSD != 22.5 {
		t.Errorf("CostUSD = %v, want 22.5", row.CostUSD)
	}
}

// TestRecordInferenceCost_ModelPrefixNotStrippedForNonExternal verifies that
// the prefix-stripping logic only fires for external backends — a local backend
// whose model happens to start with the backend name is left alone.
func TestRecordInferenceCost_ModelPrefixNotStrippedForNonExternal(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				Inference: config.InferenceRate{USDPer1MTokens: 5},
			},
		},
		Client: &proxy.Client{Model: "llama/llama-3"}, // local model, not stripped
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "llama", External: false},
		},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 500_000, Exact: true})
	app.Client.SetLastUsedBackend("llama")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// Source for local backends doesn't include the model at all:
	// "inference·llama" (not "inference·llama/llama-3")
	wantSource := proxy.CostSourceInfPrefix + "llama"
	if rows[0].Source != wantSource {
		t.Errorf("Source = %q, want %q (local backend source doesn't include model)", rows[0].Source, wantSource)
	}
}
