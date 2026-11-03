package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/fake"
)

func TestDockerAgentRoot(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/proj": "/mnt/proj",
		"":             "/mnt/work",
		"/":            "/mnt/work",
	} {
		if got := sandbox.DockerAgentRoot(in); got != want {
			t.Errorf("DockerAgentRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathMapDockerStyle(t *testing.T) {
	m := sandbox.NewPathMap("/home/u/proj", "/mnt/proj", "/tmp")
	host, err := m.MapPath("/mnt/proj/a/b.go")
	if err != nil || host != "/home/u/proj/a/b.go" {
		t.Fatalf("MapPath = %q, %v", host, err)
	}
	agent, err := m.UnmapPath("/home/u/proj/a")
	if err != nil || agent != "/mnt/proj/a" {
		t.Fatalf("UnmapPath = %q, %v", agent, err)
	}
	if _, err := m.MapPath("/mnt/projx/evil"); !errors.Is(err, sandbox.ErrNotVisible) {
		t.Errorf("sibling-prefix path must not map, got %v", err)
	}
	if _, err := m.MapPath("rel/path"); err == nil {
		t.Error("relative path must be rejected")
	}
}

// The same pathname exists on the host and inside the sandbox with different
// content: MapPath must refuse to identity-map the private /tmp, while exec
// accepts it as a working directory.
func TestPathMapPrivateTmpNeverIdentityMapped(t *testing.T) {
	m := sandbox.NewPathMap("/work", "/work", "/tmp")
	_, err := m.MapPath("/tmp/wakil-wt-x")
	if !errors.Is(err, sandbox.ErrNoHostPath) {
		t.Fatalf("MapPath(/tmp/..) = %v, want ErrNoHostPath", err)
	}
	if _, err := m.UnmapPath("/tmp/wakil-wt-x"); !errors.Is(err, sandbox.ErrNotVisible) {
		t.Errorf("UnmapPath(/tmp/..) = %v, want ErrNotVisible", err)
	}
	if cwd, err := m.ResolveExecCwd("/tmp/wakil-wt-x"); err != nil || cwd != "/tmp/wakil-wt-x" {
		t.Errorf("ResolveExecCwd(private) = %q, %v", cwd, err)
	}
	if _, err := m.ResolveExecCwd("/etc"); !errors.Is(err, sandbox.ErrNotVisible) {
		t.Errorf("ResolveExecCwd(/etc) = %v, want ErrNotVisible", err)
	}
}

func TestPathMapGrants(t *testing.T) {
	m := sandbox.NewPathMap("/work", "/work", "/tmp")
	if _, err := m.MapPath("/srv/ref/x"); !errors.Is(err, sandbox.ErrNotVisible) {
		t.Fatalf("ungranted path: %v", err)
	}
	m.AddGrant("/srv/ref", false)
	if h, err := m.MapPath("/srv/ref/x"); err != nil || h != "/srv/ref/x" {
		t.Fatalf("granted path: %q %v", h, err)
	}
	m.RemoveGrant("/srv/ref")
	if _, err := m.MapPath("/srv/ref/x"); err == nil {
		t.Fatal("revoked path still maps")
	}
}

func TestGrantManagerTiers(t *testing.T) {
	ctx := context.Background()
	b := fake.New("/work", "/work")
	_ = b.Start(ctx, sandbox.Session{ID: "c", HostCwd: "/work"})
	g := sandbox.NewGrantManager(b, "/work", []string{"/srv/out"})

	// Inside cwd: nothing to do.
	if err := g.Ensure(ctx, "/work/sub", sandbox.TierEdit); err != nil || len(b.Grants) != 0 {
		t.Fatalf("inside cwd: err=%v grants=%v", err, b.Grants)
	}
	// Read tier outside cwd -> ro grant.
	if err := g.Ensure(ctx, "/srv/ref", sandbox.TierRead); err != nil || b.Grants["/srv/ref"] {
		t.Fatalf("read tier: err=%v grants=%v", err, b.Grants)
	}
	if rw, ok := b.Grants["/srv/ref"]; !ok || rw {
		t.Fatalf("expected ro grant, got %v", b.Grants)
	}
	// Edit tier under a grantable prefix -> rw grant.
	if err := g.Ensure(ctx, "/srv/out/x", sandbox.TierEdit); err != nil || !b.Grants["/srv/out/x"] {
		t.Fatalf("edit under prefix: err=%v grants=%v", err, b.Grants)
	}
	// Edit tier outside every prefix -> ErrNotGrantable, no grant attempted.
	before := len(b.Grants)
	if err := g.Ensure(ctx, "/etc/x", sandbox.TierEdit); !errors.Is(err, sandbox.ErrNotGrantable) {
		t.Fatalf("outside prefix: %v", err)
	}
	if len(b.Grants) != before {
		t.Error("a refused rw grant must not fall back to ro")
	}
	// Relative path is rejected.
	if err := g.Ensure(ctx, "rel", sandbox.TierRead); err == nil {
		t.Error("relative path must be rejected")
	}
	// Withdraw revokes.
	if err := g.Withdraw(ctx, "/srv/ref"); err != nil || len(b.Revoked) != 1 {
		t.Fatalf("withdraw: %v revoked=%v", err, b.Revoked)
	}
}

func TestGrantManagerUpgradeRevokesFirst(t *testing.T) {
	ctx := context.Background()
	b := fake.New("/work", "/work")
	_ = b.Start(ctx, sandbox.Session{ID: "c", HostCwd: "/work"})
	g := sandbox.NewGrantManager(b, "/work", []string{"/srv/out"})
	_ = g.Ensure(ctx, "/srv/out/d", sandbox.TierRead)
	if err := g.Ensure(ctx, "/srv/out/d", sandbox.TierEdit); err != nil {
		t.Fatal(err)
	}
	if !b.Grants["/srv/out/d"] || len(b.Revoked) != 1 {
		t.Errorf("ro->rw must revoke then grant: grants=%v revoked=%v", b.Grants, b.Revoked)
	}
}

func TestGrantErrorPropagates(t *testing.T) {
	ctx := context.Background()
	b := fake.New("/work", "/work")
	_ = b.Start(ctx, sandbox.Session{ID: "c", HostCwd: "/work"})
	b.GrantErr = sandbox.ErrPathForbidden
	g := sandbox.NewGrantManager(b, "/work", nil)
	if err := g.Ensure(ctx, "/srv/ref", sandbox.TierRead); !errors.Is(err, sandbox.ErrPathForbidden) {
		t.Fatalf("got %v", err)
	}
}
