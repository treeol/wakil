package hisn

import (
	"encoding/json"
	"testing"
)

// realV2 is `hisn ls --json` captured from hisn 0.4.0 for a running named
// session after one client touched it.
const realV2 = `{"schema":"hisn-ls/v2","sessions":[{"cwd":"/tmp/hws","dir":"/tmp/hx/hisn/probe1","execs":[],"hostname":"hisn","id":"probe1","lease":null,"members":[{"first_seen":"2026-11-07T21:19:56Z","label":"local","last_seen":"2026-11-07T21:44:41Z","uid":1000}],"name":"wk-test","network":"none","pid":null,"rootfs":null,"services":[],"started":"2026-11-07T21:19:53Z","state":"running"}]}`

func TestLsEntryParsesV2(t *testing.T) {
	var doc struct {
		Schema   string    `json:"schema"`
		Sessions []lsEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(realV2), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != "hisn-ls/v2" || len(doc.Sessions) != 1 {
		t.Fatalf("schema/sessions = %q/%d", doc.Schema, len(doc.Sessions))
	}
	e := doc.Sessions[0]
	if e.ID != "probe1" || e.Name != "wk-test" || e.State != "running" || e.Dir != "/tmp/hx/hisn/probe1" || e.Cwd != "/tmp/hws" {
		t.Fatalf("entry = %+v", e)
	}
	if len(e.Members) != 1 || e.Members[0].Label != "local" || e.Members[0].UID != 1000 {
		t.Fatalf("members = %+v", e.Members)
	}
	if e.Lease != nil {
		t.Fatalf("lease = %+v, want nil", e.Lease)
	}
	if !e.Live() {
		t.Fatal("a running v2 session must be live")
	}
}

func TestLsEntryLive(t *testing.T) {
	for state, want := range map[string]bool{
		"live": true, "running": true, // v1, v2
		"stale": false, "creating": false, "stopping": false, "stopped": false, "crashed": false, "": false,
	} {
		if got := (lsEntry{State: state}).Live(); got != want {
			t.Errorf("state %q: Live() = %v, want %v", state, got, want)
		}
	}
}

func TestLsEntryV2LeaseAndClients(t *testing.T) {
	const doc = `{"id":"a","name":"n","state":"running","lease":{"idle_timeout":"30m","remaining":"29m"},"clients":[{"uid":1000,"pid":42,"label":"wakil@rubin"}]}`
	var e lsEntry
	if err := json.Unmarshal([]byte(doc), &e); err != nil {
		t.Fatal(err)
	}
	if e.Lease == nil || e.Lease.IdleTimeout != "30m" || len(e.Clients) != 1 || e.Clients[0].Label != "wakil@rubin" {
		t.Fatalf("entry = %+v", e)
	}
}
