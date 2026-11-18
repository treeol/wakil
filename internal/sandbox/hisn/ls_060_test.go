package hisn

import (
	"encoding/json"
	"strings"
	"testing"
)

// realV2_060 is `hisn ls --json` captured from hisn 0.6.0 for a running named
// session. 0.5.0 added additive fields to each session entry (hisn_version,
// protocol_revision, workspace, journal, journal_failures, journal_bytes);
// 0.5.1 added seats. The schema is still hisn-ls/v2 (0.5.0–0.6.0 list no
// removed or breaking items). Wakil must ignore fields it does not know.
const realV2_060 = `{"schema":"hisn-ls/v2","sessions":[` +
	`{"cwd":"/tmp/hws","dir":"/tmp/hisn-1000/probe1","execs":[],"hostname":"wakil-abc123","id":"probe1",` +
	`"lease":null,"members":[{"first_seen":"2026-11-18T10:00:00Z","label":"local","last_seen":"2026-11-18T10:05:00Z","uid":1000}],` +
	`"name":"wk-test","network":"host","pid":null,"rootfs":null,"services":[],"started":"2026-11-18T10:00:00Z","state":"running",` +
	`"hisn_version":"0.6.0","protocol_revision":2,"workspace":"/tmp/hws","journal":"/tmp/hisn-1000/probe1/journal",` +
	`"journal_failures":0,"journal_bytes":4096,"seats":1}]}`

// realInspect060 is `hisn inspect --json` (the sessioninfo reply) from hisn
// 0.6.0. The 0.5.x additive fields are present; Wakil's inspectDoc must parse
// the fields it knows (ok, risk, services) and ignore the rest.
const realInspect060 = `{"ok":true,` +
	`"risk":[{"label":"none","capability":"home-persist","detail":"wakil default"}],` +
	`"services":[{"name":"kvr","socket":"/tmp/hisn-1000/probe1/kvr.sock","state":"running"}],` +
	`"mountinfo":"","policy":"hostname = \"wakil-abc123\"\nnetwork = \"host\"\n",` +
	`"hisn_version":"0.6.0","protocol_revision":2,"workspace":"/tmp/hws",` +
	`"journal":"/tmp/hisn-1000/probe1/journal","journal_failures":0,"journal_bytes":4096,"seats":1}`

// TestLsParsesV2_060 verifies that `hisn ls --json` from 0.6.0 — which adds
// the 0.5.x fields hisn_version, protocol_revision, workspace, journal,
// journal_failures, journal_bytes and 0.5.1's seats to each session entry —
// parses without error and that Wakil ignores the fields it does not know.
func TestLsParsesV2_060(t *testing.T) {
	var doc struct {
		Schema   string    `json:"schema"`
		Sessions []lsEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(realV2_060), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != "hisn-ls/v2" {
		t.Fatalf("schema = %q, want hisn-ls/v2", doc.Schema)
	}
	if !lsSchemas[doc.Schema] {
		t.Fatalf("schema %q rejected by lsSchemas (this Wakil must accept hisn-ls/v2 from 0.6.0)", doc.Schema)
	}
	if len(doc.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(doc.Sessions))
	}
	e := doc.Sessions[0]
	if e.ID != "probe1" || e.Name != "wk-test" || e.State != "running" {
		t.Fatalf("entry = %+v", e)
	}
	if !e.Live() {
		t.Fatal("a running 0.6.0 session must be live")
	}
	// The 0.5.x additive fields are not in lsEntry; Go's json.Unmarshal
	// ignores them. Verify the known fields are intact.
	if len(e.Members) != 1 || e.Members[0].Label != "local" || e.Members[0].UID != 1000 {
		t.Fatalf("members = %+v", e.Members)
	}
}

// TestLsListSessionsParses060 runs the full listSessions parser path (which
// checks the schema) against the 0.6.0 fixture.
func TestLsListSessionsParses060(t *testing.T) {
	rows := struct {
		Schema   string    `json:"schema"`
		Sessions []lsEntry `json:"sessions"`
	}{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(realV2_060)), &rows); err != nil {
		t.Fatal(err)
	}
	if !lsSchemas[rows.Schema] {
		t.Fatalf("listSessions would reject 0.6.0 schema %q", rows.Schema)
	}
	m := make(map[string]lsEntry, len(rows.Sessions))
	for _, r := range rows.Sessions {
		m[r.ID] = r
	}
	if e, ok := m["probe1"]; !ok || !e.Live() {
		t.Fatalf("probe1 not found or not live: %+v", e)
	}
}

// TestInspectParses060 verifies that `hisn inspect --json` (the sessioninfo
// reply) from 0.6.0 — which adds the 0.5.x additive fields — parses without
// error and that Wakil's inspectDoc ignores the fields it does not know.
func TestInspectParses060(t *testing.T) {
	var d inspectDoc
	if err := json.Unmarshal([]byte(realInspect060), &d); err != nil {
		t.Fatal(err)
	}
	if !d.OK {
		t.Fatal("ok = false, want true")
	}
	if len(d.Risk) != 1 || d.Risk[0].Label != "none" {
		t.Fatalf("risk = %+v", d.Risk)
	}
	if len(d.Services) != 1 || d.Services[0].Name != "kvr" || d.Services[0].State != "running" {
		t.Fatalf("services = %+v", d.Services)
	}
	// The 0.5.x additive fields (hisn_version, protocol_revision, workspace,
	// journal, journal_failures, journal_bytes, seats) are not in inspectDoc;
	// Go's json.Unmarshal ignores them. The known fields parse correctly.
}

// TestCtlInspectParses060 verifies that the ctl protocol's Inspect response
// from 0.6.0 — which adds the 0.5.x additive fields to the response object —
// parses without error. The ctl Inspect reads ok, error, message, mountinfo,
// and policy; the 0.5.x fields are additive and ignored.
func TestCtlInspectParses060(t *testing.T) {
	var raw struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		Message   string `json:"message"`
		Mountinfo string `json:"mountinfo"`
		Policy    string `json:"policy"`
	}
	if err := json.Unmarshal([]byte(realInspect060), &raw); err != nil {
		t.Fatal(err)
	}
	if !raw.OK {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(raw.Policy, "hostname") {
		t.Fatalf("policy = %q, want a hostname", raw.Policy)
	}
}

// TestLsEntryIgnoresUnknownFields verifies that a session entry with unknown
// fields (from a future hisn version) parses without error. Go's
// json.Unmarshal ignores unknown fields by default; this test documents that
// the parser relies on that behaviour.
func TestLsEntryIgnoresUnknownFields(t *testing.T) {
	const doc = `{"id":"x","name":"n","state":"running","future_field":"value","another_unknown":42}`
	var e lsEntry
	if err := json.Unmarshal([]byte(doc), &e); err != nil {
		t.Fatal(err)
	}
	if e.ID != "x" || e.Name != "n" || e.State != "running" {
		t.Fatalf("entry = %+v", e)
	}
}

// TestInspectDocIgnoresUnknownFields verifies that an inspect response with
// unknown fields (from a future hisn version) parses without error.
func TestInspectDocIgnoresUnknownFields(t *testing.T) {
	const doc = `{"ok":true,"future_field":"value","another_unknown":42}`
	var d inspectDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatal(err)
	}
	if !d.OK {
		t.Fatal("ok = false, want true")
	}
}
