package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// retrievalTestApp creates an App with a real memory store and optional skill store.
func retrievalTestApp(t *testing.T, isSubagent bool) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	dbPath := filepath.Join(dir, "memory", "test.db")

	store, err := memory.Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}

	prefix := "main"
	if isSubagent {
		prefix = "sub-abc12345"
	}

	app := &App{
		MemoryStore: store,
		SkillStore:  newSkillsProfile(store),
		AgentPrefix: prefix,
		IsSubagent:  isSubagent,
	}

	cleanup := func() { store.Close() }
	t.Cleanup(cleanup)
	return app, cleanup
}

func TestSanitizeFTSQuery(t *testing.T) {
	tests := []struct {
		input string
		want  string // expected substring
	}{
		{"fix the auth bug", `"auth"`}, // short words filtered, "auth" kept
		{"hello world", ""},            // both words < 4 chars after filtering? no, "hello" and "world" are 5 chars
		{"hello world test", `"hello" OR "world" OR "test"`},
		{"", ""},      // empty
		{"a b c", ""}, // all too short
		{"code review checklist", `"code" OR "review" OR "checklist"`},
		{"rm -rf /", `"-rf"`}, // special chars stripped, "rf" kept with leading -
		{`"injection"; DROP TABLE--`, `"injection" OR "DROP" OR "TABLE--"`},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeFTSQuery(tc.input)
			if tc.want == "" && got != "" {
				// For "hello world" we expect non-empty
				if tc.input == "hello world" {
					if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
						t.Errorf("sanitizeFTSQuery(%q) = %q, expected hello and world", tc.input, got)
					}
					return
				}
				if got != "" {
					t.Errorf("sanitizeFTSQuery(%q) = %q, want empty", tc.input, got)
				}
			} else if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("sanitizeFTSQuery(%q) = %q, want to contain %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeFTSQuery_NoFTSOperators(t *testing.T) {
	// FTS5 special characters that could cause syntax errors.
	evil := `query"with*quotes AND NEAR(x,y) (parentheses) :colon`
	got := sanitizeFTSQuery(evil)
	// The result should not contain unescaped FTS5 operators.
	// All tokens should be double-quoted.
	for _, token := range strings.Fields(got) {
		if token != "OR" && (!strings.HasPrefix(token, "\"") || !strings.HasSuffix(token, "\"")) {
			t.Errorf("token %q is not properly quoted", token)
		}
	}
}

func TestRetrieveMemoryContext_WithResults(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Store a few entries.
	_, err := app.MemoryStore.PutActive(ctx, "arch/auth-flow",
		"The auth module uses JWT with refresh tokens. Token expiry is 15min.",
		"summary", memory.TierDurable, "main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.MemoryStore.PutActive(ctx, "decision/use-sqlite",
		"We chose SQLite for the memory store because it's embedded and needs no server.",
		"decision", memory.TierDurable, "main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "fix the auth token bug")
	if result == "" {
		t.Fatal("expected non-empty retrieval result for 'auth token' query")
	}
	if !strings.Contains(result, "auth-flow") {
		t.Errorf("result should contain 'auth-flow' key, got: %s", result)
	}
	if !strings.Contains(result, "JWT") {
		t.Errorf("result should contain 'JWT' value, got: %s", result)
	}
	if !strings.Contains(result, "untrusted data") {
		t.Errorf("result should contain untrusted data framing, got: %s", result)
	}
}

func TestRetrieveMemoryContext_NoResults(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Store an entry that won't match.
	_, err := app.MemoryStore.PutActive(ctx, "arch/auth-flow",
		"Auth uses JWT.", "summary", memory.TierDurable,
		"main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "completely unrelated query about gardening")
	if result != "" {
		t.Errorf("expected empty result for unrelated query, got: %s", result)
	}
}

func TestRetrieveMemoryContext_NilStores(t *testing.T) {
	app := &App{} // no MemoryStore or SkillStore
	ctx := context.Background()

	result := app.retrieveMemoryContext(ctx, "auth token bug")
	if result != "" {
		t.Errorf("expected empty result with nil stores, got: %s", result)
	}
}

func TestRetrieveMemoryContext_TaintLabels(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Store a tainted entry.
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})
	tainted := app.computeTainted()
	if tainted != memory.TaintTrue {
		t.Fatalf("expected TaintTrue after external grounding, got %d", tainted)
	}

	_, err := app.MemoryStore.PutActive(ctx, "arch/web-research",
		"Found info on a website about auth patterns.",
		"summary", memory.TierDurable, "main", "s1", memory.TaintTrue, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "auth patterns research")
	if result == "" {
		t.Fatal("expected non-empty result")
	}
	if !strings.Contains(result, "tainted") {
		t.Errorf("result should contain taint label, got: %s", result)
	}
}

func TestRetrieveMemoryContext_ByteCap(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Store a large entry.
	largeValue := strings.Repeat("auth token security best practice. ", 200)
	_, err := app.MemoryStore.PutActive(ctx, "arch/large-entry",
		largeValue, "summary", memory.TierDurable,
		"main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "auth token security")
	if result == "" {
		t.Fatal("expected non-empty result")
	}
	// The total result should be under the cap (2KB + some slack for header).
	if len(result) > 2500 {
		t.Errorf("result too large: %d bytes (cap is ~2048)", len(result))
	}
}

func TestRetrieveMemoryContext_Skills(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Store a skill (durable active). expectExists=false for a new skill.
	_, err := app.SkillStore.putActiveSkill(ctx, "trello-card-workflow",
		"Per-card workflow: verify → plan → review → implement → review → commit",
		"main", "s1", memory.TaintUnknown, false, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "trello card workflow")
	if result == "" {
		t.Fatal("expected non-empty result for skill search")
	}
	if !strings.Contains(result, "trello-card-workflow") {
		t.Errorf("result should contain skill key, got: %s", result)
	}
}

func TestRetrieveMemoryContext_SubagentCap(t *testing.T) {
	app, _ := retrievalTestApp(t, true) // isSubagent=true
	ctx := context.Background()

	// Store a large entry.
	largeValue := strings.Repeat("auth token security best practice. ", 200)
	_, err := app.MemoryStore.PutActive(ctx, "arch/large-entry",
		largeValue, "summary", memory.TierDurable,
		"main", "s1", memory.TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	result := app.retrieveMemoryContext(ctx, "auth token security")
	if result == "" {
		t.Fatal("expected non-empty result")
	}
	// Subagent cap is 1KB.
	if len(result) > 1500 {
		t.Errorf("subagent result too large: %d bytes (cap is ~1024)", len(result))
	}
}

func TestRetrieveMemoryContext_EmptyQuery(t *testing.T) {
	app, _ := retrievalTestApp(t, false)
	ctx := context.Background()

	// Empty or too-short queries should return "".
	result := app.retrieveMemoryContext(ctx, "")
	if result != "" {
		t.Errorf("expected empty result for empty query, got: %s", result)
	}

	result = app.retrieveMemoryContext(ctx, "a b")
	if result != "" {
		t.Errorf("expected empty result for short tokens, got: %s", result)
	}
}

func TestRetrieveMemoryContext_Timeout(t *testing.T) {
	app, _ := retrievalTestApp(t, false)

	// Create a context that's already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should return "" without panicking.
	result := app.retrieveMemoryContext(ctx, "auth token security")
	if result != "" {
		t.Errorf("expected empty result with cancelled context, got: %s", result)
	}
}
