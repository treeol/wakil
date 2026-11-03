package hisn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox"
)

// ---- grant ledger: one source of truth ----

func ledgerPair(t *testing.T) (*Backend, *exec.BackendExecutor, *fakeEnv) {
	t.Helper()
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.GrantableRW = []string{"/srv/out"}
	b, err := Open(ctxT(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	ex := exec.NewBackendExecutor(b, exec.BackendOptions{Label: "hisn", GrantableRW: []string{"/srv/out"}})
	t.Cleanup(func() { _ = ex.Close() })
	return b, ex, fe
}

// After a dead-session replacement the executor's GrantManager and the
// backend's PathMap report the IDENTICAL grant set, and Withdraw still works
// (the manager did not forget what the backend restored).
func TestGrantLedgerIdenticalAfterReplacement(t *testing.T) {
	b, ex, _ := ledgerPair(t)
	ctx := ctxT(t)
	if err := ex.BindChat(ctx, "ledger"); err != nil {
		t.Fatal(err)
	}
	if err := ex.EnsureAccess(ctx, "/srv/ref", sandbox.TierRead); err != nil {
		t.Fatal(err)
	}
	if err := ex.EnsureAccess(ctx, "/srv/out/sub", sandbox.TierEdit); err != nil {
		t.Fatal(err)
	}
	want := []sandbox.GrantEntry{{Path: "/srv/out/sub", RW: true}, {Path: "/srv/ref", RW: false}}
	if got := ex.Grants(); !reflect.DeepEqual(got, want) {
		t.Fatalf("before: manager = %+v, want %+v", got, want)
	}

	killSupervisorHard(t, b)
	if _, err := ex.RunShell(ctx, "true"); err == nil || !errors.Is(err, sandbox.ErrSessionDead) {
		t.Fatalf("call on the dead session: err = %v, want ErrSessionDead", err)
	}
	// The next call starts the replacement (same chat id) and replays.
	if out, err := ex.RunShell(ctx, "echo up"); err != nil || !strings.Contains(out, "up") {
		t.Fatalf("replacement not usable: %q %v", out, err)
	}
	if got := b.Grants(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PathMap after replacement = %+v, want %+v", got, want)
	}
	if mgr, pm := ex.Grants(), b.Grants(); !reflect.DeepEqual(mgr, pm) {
		t.Fatalf("ledgers differ after replacement:\n manager %+v\n pathmap %+v", mgr, pm)
	}
	// The manager still knows the grants: Withdraw must reach the backend.
	if err := ex.WithdrawAccess(ctx, "/srv/ref"); err != nil {
		t.Fatal(err)
	}
	if mgr, pm := ex.Grants(), b.Grants(); !reflect.DeepEqual(mgr, pm) || len(pm) != 1 || pm[0].Path != "/srv/out/sub" {
		t.Fatalf("after Withdraw: manager %+v pathmap %+v, want only /srv/out/sub in both", mgr, pm)
	}
}

// Grants never travel to a session of ANOTHER chat id.
func TestStartDoesNotCarryGrantsToAnotherSession(t *testing.T) {
	var diag strings.Builder
	old := errWriter
	SetDiagnostics(&diag)
	t.Cleanup(func() { SetDiagnostics(old) })
	fe := newFakeEnv(t)
	b, err := Open(ctxT(t), fe.options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-a", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if err := b.Grant(ctxT(t), "/srv/ref", false); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-b", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if g := b.Grants(); len(g) != 0 {
		t.Fatalf("grants leaked into another chat's session: %+v", g)
	}
	if _, err := b.MapPath("/srv/ref/x"); err == nil {
		t.Fatal("path map still resolves a grant of the previous chat")
	}
}
