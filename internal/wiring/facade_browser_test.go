package wiring

// B2 (0.4.4): rotating or closing a conversation (/new, /handoff, chat end)
// stops its browser; the next conversation gets its own.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

type countingLauncher struct{ launches, teardowns atomic.Int32 }

func (c *countingLauncher) Launch(context.Context) (func() error, error) {
	c.launches.Add(1)
	return func() error { c.teardowns.Add(1); return nil }, nil
}

func TestFacadeClose_StopsBrowser(t *testing.T) {
	cl := &countingLauncher{}
	mgr := browser.NewManagerForTest(cl, time.Hour)
	if err := mgr.TouchForTest(); err != nil { // a browser tool call
		t.Fatal(err)
	}
	if !mgr.Running() || cl.launches.Load() != 1 {
		t.Fatalf("browser not running after a tool call (launches=%d)", cl.launches.Load())
	}

	app := &agent.App{}
	f := newWiringFacade(app, nil, nil, &AppResources{BrowserMgr: mgr}, core.Principal{TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner})
	f.app = app
	closeFacadeBrowser(f)

	if mgr.Running() {
		t.Fatal("rotating/closing the conversation must stop its browser")
	}
	if cl.teardowns.Load() != 1 {
		t.Errorf("teardowns = %d, want 1", cl.teardowns.Load())
	}
	// Idempotent (CloseResources also closes it on the exit path).
	closeFacadeBrowser(f)
	if cl.teardowns.Load() != 1 {
		t.Errorf("second close tore down again: %d", cl.teardowns.Load())
	}
}

// The real Close path (not just the helper) must stop the browser: build a
// facade over a real App and close it.
func TestWiringFacadeClose_ReachesBrowserStop(t *testing.T) {
	cl := &countingLauncher{}
	mgr := browser.NewManagerForTest(cl, time.Hour)
	if err := mgr.TouchForTest(); err != nil {
		t.Fatal(err)
	}
	f := newTestFacade(t) // real App + host turn handle; Cleanup also closes it
	f.resources = &AppResources{BrowserMgr: mgr}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if mgr.Running() || cl.teardowns.Load() != 1 {
		t.Fatalf("Facade.Close left the browser running (running=%v teardowns=%d)", mgr.Running(), cl.teardowns.Load())
	}
}
