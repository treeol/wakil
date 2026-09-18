package connect

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
)

func TestGetServerInfo(t *testing.T) {
	h := NewSystemHandler(false)
	resp, err := h.GetServerInfo(context.Background(), connect.NewRequest(&v1alpha1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	if resp.Msg.ApiVersion != "v1alpha1" {
		t.Errorf("ApiVersion = %q, want %q", resp.Msg.ApiVersion, "v1alpha1")
	}
	if len(resp.Msg.Capabilities) == 0 {
		t.Error("Capabilities should not be empty")
	}
	if resp.Msg.Ephemeral != false {
		t.Errorf("Ephemeral = %v, want false", resp.Msg.Ephemeral)
	}
}

func TestGetServerInfo_Ephemeral(t *testing.T) {
	h := NewSystemHandler(true)
	resp, err := h.GetServerInfo(context.Background(), connect.NewRequest(&v1alpha1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	if !resp.Msg.Ephemeral {
		t.Error("Ephemeral should be true")
	}
}

func TestHealth(t *testing.T) {
	h := NewSystemHandler(false)
	resp, err := h.Health(context.Background(), connect.NewRequest(&v1alpha1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Msg.Status != "ready" {
		t.Errorf("Status = %q, want %q", resp.Msg.Status, "ready")
	}
	if resp.Msg.StartedAt == nil {
		t.Error("StartedAt should not be nil")
	}
}
