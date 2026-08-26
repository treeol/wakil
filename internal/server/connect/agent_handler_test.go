package connect

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/store/agentstore"

	_ "modernc.org/sqlite"
)

// testAgentSetup creates an agent store + handler for testing.
func testAgentSetup(t *testing.T, role core.Role) (*AgentHandler, *agentstore.Store, *sql.DB) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	schema := `CREATE TABLE agents (
		id            TEXT PRIMARY KEY,
		tenant_id     TEXT NOT NULL,
		name          TEXT NOT NULL,
		head_rev_id   TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL,
		UNIQUE(tenant_id, name)
	);
	CREATE TABLE agent_revisions (
		id            TEXT PRIMARY KEY,
		tenant_id     TEXT NOT NULL,
		agent_id      TEXT NOT NULL,
		rev_number    INTEGER NOT NULL,
		spec          TEXT NOT NULL,
		spec_hash     TEXT NOT NULL,
		created_by    TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		UNIQUE(agent_id, rev_number)
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	store := agentstore.New(db)
	resolver := &fakeResolver{principal: core.Principal{
		TenantID: "tnt_test",
		UserID:   "usr_admin",
		Role:     role,
	}}
	handler := NewAgentHandler(store, resolver)

	t.Cleanup(func() { db.Close() })
	return handler, store, db
}

func TestCreateAgent(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	resp, err := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "code-reviewer",
	}))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	a := resp.Msg.Agent
	if a.Id == "" || !strings.HasPrefix(a.Id, "agt_") {
		t.Errorf("expected agent ID with agt_ prefix, got %q", a.Id)
	}
	if a.Name != "code-reviewer" {
		t.Errorf("expected name %q, got %q", "code-reviewer", a.Name)
	}
	if a.TenantId != "tnt_test" {
		t.Errorf("expected tenant_id %q, got %q", "tnt_test", a.TenantId)
	}
	if a.CreatedAt == nil {
		t.Error("expected non-nil created_at")
	}
}

func TestCreateAgentMemberDenied(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleMember)

	_, err := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "test",
	}))
	if err == nil {
		t.Fatal("expected error for member role")
	}
}

func TestCreateAgentValidation(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	_, err := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "",
	}))
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestCreateAgentRevision(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	// Create an agent first.
	createResp, err := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "test-agent",
	}))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Msg.Agent.Id

	// Create a revision.
	revResp, err := handler.CreateAgentRevision(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRevisionRequest{
		AgentId: agentID,
		Spec:    `{"prompt":"You are a helpful agent","model":"gpt-4"}`,
	}))
	if err != nil {
		t.Fatalf("CreateAgentRevision: %v", err)
	}

	rev := revResp.Msg.Revision
	if rev.Id == "" || !strings.HasPrefix(rev.Id, "rev_") {
		t.Errorf("expected revision ID with rev_ prefix, got %q", rev.Id)
	}
	if rev.RevNumber != 1 {
		t.Errorf("expected rev_number 1, got %d", rev.RevNumber)
	}
	if rev.SpecHash == "" {
		t.Error("expected non-empty spec_hash")
	}
	if rev.AgentId != agentID {
		t.Errorf("expected agent_id %q, got %q", agentID, rev.AgentId)
	}

	// Create a second revision — rev_number should be 2.
	rev2Resp, err := handler.CreateAgentRevision(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRevisionRequest{
		AgentId: agentID,
		Spec:    `{"prompt":"Updated prompt","model":"gpt-4o"}`,
	}))
	if err != nil {
		t.Fatalf("CreateAgentRevision 2: %v", err)
	}
	if rev2Resp.Msg.Revision.RevNumber != 2 {
		t.Errorf("expected rev_number 2, got %d", rev2Resp.Msg.Revision.RevNumber)
	}
}

func TestListAgents(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	for _, name := range []string{"agent-a", "agent-b"} {
		_, err := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
			Name: name,
		}))
		if err != nil {
			t.Fatalf("CreateAgent %q: %v", name, err)
		}
	}

	resp, err := handler.ListAgents(context.Background(), connect.NewRequest(&v1alpha1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(resp.Msg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(resp.Msg.Agents))
	}
}

func TestListAgentRevisions(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	createResp, _ := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "rev-test",
	}))
	agentID := createResp.Msg.Agent.Id

	for i := 0; i < 3; i++ {
		_, err := handler.CreateAgentRevision(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRevisionRequest{
			AgentId: agentID,
			Spec:    `{"v":` + string(rune('0'+i)) + `}`,
		}))
		if err != nil {
			t.Fatalf("CreateAgentRevision %d: %v", i, err)
		}
	}

	resp, err := handler.ListAgentRevisions(context.Background(), connect.NewRequest(&v1alpha1.ListAgentRevisionsRequest{
		AgentId: agentID,
	}))
	if err != nil {
		t.Fatalf("ListAgentRevisions: %v", err)
	}
	if len(resp.Msg.Revisions) != 3 {
		t.Fatalf("expected 3 revisions, got %d", len(resp.Msg.Revisions))
	}
	// Should be ordered by rev_number DESC.
	if resp.Msg.Revisions[0].RevNumber != 3 {
		t.Errorf("expected first revision rev_number 3, got %d", resp.Msg.Revisions[0].RevNumber)
	}
}

func TestDeleteAgent(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	createResp, _ := handler.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "to-delete",
	}))
	agentID := createResp.Msg.Agent.Id

	_, err := handler.DeleteAgent(context.Background(), connect.NewRequest(&v1alpha1.DeleteAgentRequest{
		Id: agentID,
	}))
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	// Verify deleted.
	_, err = handler.GetAgent(context.Background(), connect.NewRequest(&v1alpha1.GetAgentRequest{
		Id: agentID,
	}))
	if err == nil {
		t.Fatal("expected NotFound after delete")
	}
}

func TestAgentCrossTenantIsolation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	schema := `CREATE TABLE agents (
		id            TEXT PRIMARY KEY,
		tenant_id     TEXT NOT NULL,
		name          TEXT NOT NULL,
		head_rev_id   TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL,
		UNIQUE(tenant_id, name)
	);
	CREATE TABLE agent_revisions (
		id            TEXT PRIMARY KEY,
		tenant_id     TEXT NOT NULL,
		agent_id      TEXT NOT NULL,
		rev_number    INTEGER NOT NULL,
		spec          TEXT NOT NULL,
		spec_hash     TEXT NOT NULL,
		created_by    TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		UNIQUE(agent_id, rev_number)
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	store := agentstore.New(db)
	ctx := context.Background()

	// Tenant A creates an agent.
	if err := store.Create(ctx, agentstore.CreateParams{
		ID: "agt_a1", TenantID: "tnt_a", Name: "agent-a",
	}); err != nil {
		t.Fatalf("create for tnt_a: %v", err)
	}

	// Tenant B lists agents — should see nothing.
	rows, err := store.List(ctx, "tnt_b")
	if err != nil {
		t.Fatalf("list for tnt_b: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("tenant B should see 0 agents, got %d", len(rows))
	}

	// Tenant B tries to get tenant A's agent.
	_, err = store.Get(ctx, "agt_a1", "tnt_b")
	if err == nil {
		t.Error("tenant B should not find tenant A's agent")
	}

	// Tenant B tries to delete tenant A's agent.
	if err := store.Delete(ctx, "agt_a1", "tnt_b"); err == nil {
		t.Error("tenant B should not be able to delete tenant A's agent")
	}

	// Tenant B tries to create a revision on tenant A's agent.
	_, err = store.CreateRevision(ctx, agentstore.CreateRevisionParams{
		ID: "rev_b1", TenantID: "tnt_b", AgentID: "agt_a1", Spec: "{}", CreatedBy: "usr_b",
	})
	if err == nil {
		t.Error("tenant B should not create revision on tenant A's agent")
	}
}

func TestAgentHandlerHTTP(t *testing.T) {
	handler, _, _ := testAgentSetup(t, core.RoleAdmin)

	mux := http.NewServeMux()
	path, h := wakilv1alpha1connect.NewAgentServiceHandler(handler)
	mux.Handle(path, h)

	server := httptest.NewServer(mux)
	defer server.Close()

	client := wakilv1alpha1connect.NewAgentServiceClient(http.DefaultClient, server.URL)

	// Create.
	createResp, err := client.CreateAgent(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRequest{
		Name: "http-test-agent",
	}))
	if err != nil {
		t.Fatalf("CreateAgent over HTTP: %v", err)
	}
	agentID := createResp.Msg.Agent.Id

	// Create revision.
	_, err = client.CreateAgentRevision(context.Background(), connect.NewRequest(&v1alpha1.CreateAgentRevisionRequest{
		AgentId: agentID,
		Spec:    `{"prompt":"test"}`,
	}))
	if err != nil {
		t.Fatalf("CreateAgentRevision over HTTP: %v", err)
	}

	// List agents.
	listResp, err := client.ListAgents(context.Background(), connect.NewRequest(&v1alpha1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents over HTTP: %v", err)
	}
	if len(listResp.Msg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(listResp.Msg.Agents))
	}

	// List revisions.
	revResp, err := client.ListAgentRevisions(context.Background(), connect.NewRequest(&v1alpha1.ListAgentRevisionsRequest{
		AgentId: agentID,
	}))
	if err != nil {
		t.Fatalf("ListAgentRevisions over HTTP: %v", err)
	}
	if len(revResp.Msg.Revisions) != 1 {
		t.Fatalf("expected 1 revision, got %d", len(revResp.Msg.Revisions))
	}

	// Delete.
	_, err = client.DeleteAgent(context.Background(), connect.NewRequest(&v1alpha1.DeleteAgentRequest{
		Id: agentID,
	}))
	if err != nil {
		t.Fatalf("DeleteAgent over HTTP: %v", err)
	}
}
