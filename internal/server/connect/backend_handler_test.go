package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/crypto"
	"github.com/treeol/wakil/internal/store/backendstore"

	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// testBackendSetup creates a backend store + master key + handler for testing.
func testBackendSetup(t *testing.T) (*BackendHandler, *backendstore.Store, *crypto.MasterKey, *sql.DB) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Create the backends table directly (skip migration runner).
	schema := `CREATE TABLE backends (
		id               TEXT PRIMARY KEY,
		tenant_id        TEXT NOT NULL,
		label            TEXT NOT NULL,
		backend_type     TEXT NOT NULL,
		base_url         TEXT NOT NULL DEFAULT '',
		api_key_cipher   BLOB NOT NULL,
		api_key_dek      BLOB NOT NULL,
		api_key_data_nonce BLOB NOT NULL,
		api_key_dek_nonce  BLOB NOT NULL,
		api_key_key_id   TEXT NOT NULL,
		api_key_last_four TEXT NOT NULL DEFAULT '',
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL,
		FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
	)`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create backends table: %v", err)
	}

	key, _ := crypto.GenerateMasterKey()
	mk, _ := crypto.NewMasterKey("v1", key)
	store := backendstore.New(db)
	resolver := &fakeResolver{principal: core.Principal{
		TenantID: "tnt_test",
		UserID:   "usr_admin",
		Role:     core.RoleAdmin,
	}}
	handler := NewBackendHandler(store, mk, resolver)

	t.Cleanup(func() { db.Close() })
	return handler, store, mk, db
}

func TestCreateBackend(t *testing.T) {
	handler, _, _, _ := testBackendSetup(t)

	resp, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "OpenAI Prod",
		BackendType: "openai",
		BaseUrl:     "https://api.openai.com/v1",
		ApiKey:      "sk-abcdefghijklmnopqrstuvwxyz0123456789",
	}))
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	b := resp.Msg.Backend
	if b.Id == "" {
		t.Fatal("backend ID is empty")
	}
	if b.Label != "OpenAI Prod" {
		t.Fatalf("label = %q, want %q", b.Label, "OpenAI Prod")
	}
	if b.BackendType != "openai" {
		t.Fatalf("backend_type = %q", b.BackendType)
	}
	if b.ApiKeyLastFour != "6789" {
		t.Fatalf("last_four = %q, want %q", b.ApiKeyLastFour, "6789")
	}
	if b.CreatedAt == nil {
		t.Fatal("created_at is nil")
	}
}

func TestCreateBackendNoMasterKey(t *testing.T) {
	// Without a master key, backend RPCs should return Unimplemented.
	store := backendstore.New(nil)
	resolver := &fakeResolver{principal: core.Principal{Role: core.RoleAdmin}}
	handler := NewBackendHandler(store, nil, resolver)

	_, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "test",
		BackendType: "openai",
		ApiKey:      "sk-test",
	}))
	if err == nil {
		t.Fatal("CreateBackend without master key should fail")
	}
	if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("expected master key error, got: %v", err)
	}
}

func TestCreateBackendValidation(t *testing.T) {
	handler, _, _, _ := testBackendSetup(t)

	tests := []struct {
		name     string
		req      *v1alpha1.CreateBackendRequest
		errField string
	}{
		{"empty label", &v1alpha1.CreateBackendRequest{
			BackendType: "openai",
			ApiKey:      "sk-test",
		}, "label"},
		{"empty backend_type", &v1alpha1.CreateBackendRequest{
			Label:  "test",
			ApiKey: "sk-test",
		}, "backend_type"},
		{"empty api_key", &v1alpha1.CreateBackendRequest{
			Label:       "test",
			BackendType: "openai",
		}, "api_key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := handler.CreateBackend(context.Background(), connect.NewRequest(tt.req))
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestCreateBackendPermissionDenied(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	defer db.Close()
	db.Exec(`CREATE TABLE backends (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, label TEXT NOT NULL, backend_type TEXT NOT NULL, base_url TEXT DEFAULT '', api_key_cipher BLOB NOT NULL, api_key_dek BLOB NOT NULL, api_key_data_nonce BLOB NOT NULL, api_key_dek_nonce BLOB NOT NULL, api_key_key_id TEXT NOT NULL, api_key_last_four TEXT DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)

	key, _ := crypto.GenerateMasterKey()
	mk, _ := crypto.NewMasterKey("v1", key)
	store := backendstore.New(db)

	resolver := &fakeResolver{principal: core.Principal{
		TenantID: "tnt_test",
		Role:     core.RoleMember, // not admin/owner
	}}
	handler := NewBackendHandler(store, mk, resolver)

	_, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "test",
		BackendType: "openai",
		ApiKey:      "sk-test",
	}))
	if err == nil {
		t.Fatal("CreateBackend with member role should fail")
	}
	if !strings.Contains(err.Error(), "owners and admins") {
		t.Fatalf("expected permission denied, got: %v", err)
	}
}

func TestListBackends(t *testing.T) {
	handler, _, _, _ := testBackendSetup(t)

	// Create two backends.
	for _, label := range []string{"Backend A", "Backend B"} {
		_, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
			Label:       label,
			BackendType: "openai",
			ApiKey:      "sk-abcdefghijklmnopqrstuvwxyz0123456789",
		}))
		if err != nil {
			t.Fatalf("CreateBackend %s: %v", label, err)
		}
	}

	resp, err := handler.ListBackends(context.Background(), connect.NewRequest(&v1alpha1.ListBackendsRequest{}))
	if err != nil {
		t.Fatalf("ListBackends: %v", err)
	}
	if len(resp.Msg.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(resp.Msg.Backends))
	}
	// Verify no API key material is returned.
	for _, b := range resp.Msg.Backends {
		if b.ApiKeyLastFour == "" {
			t.Fatal("last_four is empty")
		}
	}
}

func TestDeleteBackend(t *testing.T) {
	handler, _, _, _ := testBackendSetup(t)

	createResp, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "To Delete",
		BackendType: "openai",
		ApiKey:      "sk-test1234567890",
	}))
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	id := createResp.Msg.Backend.Id
	_, err = handler.DeleteBackend(context.Background(), connect.NewRequest(&v1alpha1.DeleteBackendRequest{Id: id}))
	if err != nil {
		t.Fatalf("DeleteBackend: %v", err)
	}

	// Verify it's gone.
	listResp, _ := handler.ListBackends(context.Background(), connect.NewRequest(&v1alpha1.ListBackendsRequest{}))
	if len(listResp.Msg.Backends) != 0 {
		t.Fatal("backend still listed after delete")
	}
}

func TestUpdateBackend(t *testing.T) {
	handler, _, mk, db := testBackendSetup(t)

	createResp, err := handler.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "Original",
		BackendType: "openai",
		ApiKey:      "sk-abcdefghijklmnopqrstuvwxyz0123456789",
	}))
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	id := createResp.Msg.Backend.Id

	// Update label + API key.
	updateResp, err := handler.UpdateBackend(context.Background(), connect.NewRequest(&v1alpha1.UpdateBackendRequest{
		Id:     id,
		Label:  "Updated",
		ApiKey: "sk-newkey1234567890abcdefghijklmnop",
	}))
	if err != nil {
		t.Fatalf("UpdateBackend: %v", err)
	}
	if updateResp.Msg.Backend.Label != "Updated" {
		t.Fatalf("label = %q, want %q", updateResp.Msg.Backend.Label, "Updated")
	}
	if updateResp.Msg.Backend.ApiKeyLastFour != "mnop" {
		t.Fatalf("last_four = %q, want %q", updateResp.Msg.Backend.ApiKeyLastFour, "mnop")
	}

	// Verify the old key can no longer decrypt (was replaced).
	row, ek, err := backendstore.New(db).Get(context.Background(), id, "tnt_test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = row
	decrypted, err := backendstore.DecryptAPIKey(mk, ek, "tnt_test", id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(decrypted) != "sk-newkey1234567890abcdefghijklmnop" {
		t.Fatalf("decrypted = %q, want new key", string(decrypted))
	}
}

func TestBackendHandlerViaHTTP(t *testing.T) {
	handler, _, _, _ := testBackendSetup(t)

	mux := http.NewServeMux()
	path, h := wakilv1alpha1connect.NewBackendServiceHandler(handler)
	mux.Handle(path, h)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := wakilv1alpha1connect.NewBackendServiceClient(srv.Client(), srv.URL)

	resp, err := client.CreateBackend(context.Background(), connect.NewRequest(&v1alpha1.CreateBackendRequest{
		Label:       "HTTP Test",
		BackendType: "openai",
		ApiKey:      "sk-abcdefghijklmnopqrstuvwxyz0123456789",
	}))
	if err != nil {
		t.Fatalf("CreateBackend via HTTP: %v", err)
	}
	if resp.Msg.Backend.Label != "HTTP Test" {
		t.Fatalf("label = %q", resp.Msg.Backend.Label)
	}
	if resp.Msg.Backend.ApiKeyLastFour != "6789" {
		t.Fatalf("last_four = %q", resp.Msg.Backend.ApiKeyLastFour)
	}
}

func TestEncryptedKeyCannotBeSwappedAcrossRows(t *testing.T) {
	// AAD binding: ciphertext from row A should not decrypt with row B's context.
	_, _, mk, _ := testBackendSetup(t)

	// Create two backends.
	ek1, err := backendstore.EncryptAPIKey(mk, []byte("key-one"), "tnt_test", "be_1")
	if err != nil {
		t.Fatalf("Encrypt key 1: %v", err)
	}
	ek2, err := backendstore.EncryptAPIKey(mk, []byte("key-two"), "tnt_test", "be_2")
	if err != nil {
		t.Fatalf("Encrypt key 2: %v", err)
	}

	// Try to decrypt ek1 with row be_2's AAD — should fail.
	_, err = backendstore.DecryptAPIKey(mk, ek1, "tnt_test", "be_2")
	if err == nil {
		t.Fatal("decrypting key-1 with row be_2's AAD should fail (AAD binding)")
	}

	// Try to decrypt ek2 with row be_1's AAD — should fail.
	_, err = backendstore.DecryptAPIKey(mk, ek2, "tnt_test", "be_1")
	if err == nil {
		t.Fatal("decrypting key-2 with row be_1's AAD should fail (AAD binding)")
	}

	// Correct AAD should work.
	dec1, err := backendstore.DecryptAPIKey(mk, ek1, "tnt_test", "be_1")
	if err != nil {
		t.Fatalf("Decrypt key-1 with correct AAD: %v", err)
	}
	if string(dec1) != "key-one" {
		t.Fatalf("decrypted = %q, want %q", string(dec1), "key-one")
	}

	// Different tenant should fail.
	_, err = backendstore.DecryptAPIKey(mk, ek1, "tnt_other", "be_1")
	if err == nil {
		t.Fatal("decrypting with different tenant should fail (AAD binding)")
	}
}

// fakeResolver implements principalResolver for testing.
type fakeResolver struct {
	principal core.Principal
}

func (f *fakeResolver) Resolve(ctx context.Context) (core.Principal, error) {
	return f.principal, nil
}

// fakeErrResolver always returns an error, for testing error mapping.
type fakeErrResolver struct {
	err error
}

func (f *fakeErrResolver) Resolve(ctx context.Context) (core.Principal, error) {
	return core.Principal{}, f.err
}

// Suppress unused import warning.
var _ = fmt.Sprintf
