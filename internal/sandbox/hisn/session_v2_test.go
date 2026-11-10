package hisn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/hisn/ctl"
)

// hisn 0.4.0 names are letters, digits, '.', '_' and '-', at most 64 long
// (measured: 64 passes, 80 is refused).
func TestSessionNameIsValidDeterministicAndBounded(t *testing.T) {
	for _, tc := range []struct{ key, chat string }{
		{"0123456789abcdef", "123e4567-e89b-12d3-a456-426614174000"},
		{"key with space/slash", "chat id:with*odd"},
		{strings.Repeat("k", 100), strings.Repeat("c", 100)},
		{"", ""},
	} {
		n := sessionName(tc.key, tc.chat)
		if len(n) > maxSessionName || n == "" || n[0] == '.' {
			t.Errorf("sessionName(%q,%q) = %q (len %d)", tc.key, tc.chat, n, len(n))
		}
		if !validFakeName(n) {
			t.Errorf("sessionName(%q,%q) = %q is not a valid hisn name", tc.key, tc.chat, n)
		}
		if n != sessionName(tc.key, tc.chat) {
			t.Errorf("sessionName is not deterministic for %q,%q", tc.key, tc.chat)
		}
	}
	// A real chat uuid fits unshortened: wakil-<16 hex>-<36> = 59.
	if n := sessionName("0123456789abcdef", "123e4567-e89b-12d3-a456-426614174000"); n != "wakil-0123456789abcdef-123e4567-e89b-12d3-a456-426614174000" {
		t.Errorf("a normal name must not be shortened: %q", n)
	}
	// Two different long names stay different after shortening.
	a := sessionName(strings.Repeat("k", 100), "chat-a")
	b := sessionName(strings.Repeat("k", 100), "chat-b")
	if a == b {
		t.Errorf("shortened names collide: %q", a)
	}
}

// The session is created with --name wakil-<projectkey>-<chat>; every later
// hisn call addresses it by its ID (the identity the owner record and the
// browser's argv ownership check are bound to; a name can be released and
// reused).
func TestSessionIsCreatedByNameAndAddressedByID(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, func(o *Options) { o.ProjectKey = "projkey0123456789" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-1", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	const name = "wakil-projkey0123456789-chat-1"
	c := calls(t, fe)
	if !strings.Contains(c, "--session-id chat-1") || !strings.Contains(c, "--name "+name) {
		t.Fatalf("run lacks --name %s:\n%s", name, c)
	}
	if rec, err := b.readRecord("chat-1"); err != nil || rec.Name != name {
		t.Fatalf("owner record name = %q (%v), want %s", rec.Name, err, name)
	}
	// ExecCLI (the CLI exec path), grant and revoke address the session by ID;
	// the exec client labels itself.
	h, err := b.ExecCLI(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Cwd: fe.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := b.Grant(ctxT(t), dir, false); err != nil {
		t.Fatal(err)
	}
	if err := b.Revoke(ctxT(t), dir); err != nil {
		t.Fatal(err)
	}
	c = calls(t, fe)
	for _, want := range []string{"exec --session chat-1 --client wakil@", "grant --session chat-1", "revoke --session chat-1"} {
		if !strings.Contains(c, want) {
			t.Errorf("calls lack %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "--session "+name) {
		t.Errorf("a call addresses the session by NAME (only creation may):\n%s", c)
	}
	// The browser's ownership check compares the argv against ActiveSessionID.
	if b.ActiveSessionID() != "chat-1" {
		t.Errorf("ActiveSessionID = %q, want the id the CLI argv carries", b.ActiveSessionID())
	}
	// Without a project key the name is derived from the workspace path.
	b2 := &Backend{opts: Options{HostCwd: fe.cwd}}
	if n := b2.sessionName("c"); !strings.HasPrefix(n, "wakil-") || !strings.HasSuffix(n, "-c") {
		t.Errorf("derived name %q", n)
	}
}

// The client label is "wakil@<hostname>" unless configured, and reaches the ctl
// path: the fake control server records every client record it receives.
func TestControlRequestsCarryTheClientRecord(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, func(o *Options) { o.ClientLabel = "wakil@testhost" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cl", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if out, info := run(t, b, "echo", "hi"); strings.TrimSpace(out) != "hi" || info.Code != 0 {
		t.Fatalf("exec through ctl failed: %q %+v", out, info)
	}
	data, err := os.ReadFile(filepath.Join(fe.rt(), "cl", "clients.jsonl"))
	if err != nil {
		t.Fatalf("the control server saw no client record: %v", err)
	}
	if !strings.Contains(string(data), `"label":"wakil@testhost"`) || !strings.Contains(string(data), `"req_id":"`+b.cur.inst+`-1"`) {
		t.Fatalf("client record = %s", data)
	}
	// Default label.
	b3 := &Backend{opts: Options{}}
	if got := b3.clientLabel(); !strings.HasPrefix(got, "wakil@") || len(got) <= len("wakil@") {
		t.Errorf("default label = %q", got)
	}
}

// Wakil ends a session with `hisn stop` (not kill) and leaves hisn's stopped
// directory for its audit; the next Reconcile/Start prunes it.
func TestStopUsesHisnStopAndKeepsTheDirectory(t *testing.T) {
	b, fe := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "gs", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	c := calls(t, fe)
	if !strings.Contains(c, "stop --session gs") {
		t.Fatalf("no `hisn stop`:\n%s", c)
	}
	if strings.Contains(c, "kill --session gs") {
		t.Fatalf("Stop used `hisn kill`:\n%s", c)
	}
	if got := fe.stoppedIDs(); len(got) != 1 || got[0] != "gs" {
		t.Fatalf("stopped directory = %v", got)
	}
	m, err := b.listSessions(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if e := m["gs"]; e.Live() || !e.Stopped() {
		t.Fatalf("state after Stop = %+v, want stopped and not live", e)
	}
	// Reconcile prunes it.
	if err := b.Reconcile(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if got := fe.sessionIDs(); len(got) != 0 {
		t.Fatalf("Reconcile left %v", got)
	}
}

// On hisn 0.4.0 a stopped session releases its name at once: the next chat session of the
// same name starts, and neither the Stop nor the second Start prunes (the
// stopped directory stays for hisn's audit). Open prunes once, before either.
func TestStartAfterStopReusesTheNameWithoutPruning(t *testing.T) {
	// This is the 0.4.0 behavior, which is below the supported floor now, so the
	// test lowers MinVersion to keep the version-keyed branch covered.
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.0")
	b, err := openWith(t, fe, func(o *Options) { o.MinVersion = "0.4.0" })
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Count(calls(t, fe), "ls --prune")
	for i := 0; i < 2; i++ {
		if err := b.Start(ctxT(t), sandbox.Session{ID: "reuse", HostCwd: fe.cwd}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err := b.Stop(ctxT(t)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(calls(t, fe), "ls --prune"); got != before {
		t.Errorf("stop/start of a stopped session pruned %d time(s); the audit directory must stay", got-before)
	}
	if got := fe.stoppedIDs(); len(got) != 1 {
		t.Errorf("the stopped directory must remain: %v", got)
	}
}

// A CRASHED directory of this chat keeps its name reserved in hisn 0.4.0; Start
// prunes it (hisn archives it) and takes the name.
func TestStartPrunesACrashedLeftoverOfThisChat(t *testing.T) {
	b, fe := openFake(t)
	name := b.sessionName("cr")
	dir := plantStale(t, fe, "cr")
	_ = os.WriteFile(filepath.Join(dir, "name"), []byte(name), 0o600)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cr", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("a crashed leftover blocked the start: %v", err)
	}
	c := calls(t, fe)
	// Exact order: the listing, then the prune, then the run.
	li, pi, ri := strings.LastIndex(c[:strings.Index(c, "run --detach")], "ls --json"), strings.LastIndex(c[:strings.Index(c, "run --detach")], "ls --prune"), strings.Index(c, "run --detach")
	if !(li >= 0 && pi > li && ri > pi) {
		t.Errorf("want ls --json, then ls --prune, then run (positions %d %d %d):\n%s", li, pi, ri, c)
	}
	if out, info := run(t, b, "echo", "ok"); strings.TrimSpace(out) != "ok" || info.Code != 0 {
		t.Fatalf("replacement unusable: %q %+v", out, info)
	}
}

// A crashed leftover under ANOTHER id that holds our name is pruned too.
func TestStartPrunesACrashedLeftoverHoldingTheName(t *testing.T) {
	b, fe := openFake(t)
	dir := plantStale(t, fe, "older-id")
	_ = os.WriteFile(filepath.Join(dir, "name"), []byte(b.sessionName("nw")), 0o600)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "nw", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("a crashed leftover holding the name blocked the start: %v", err)
	}
}

// A session that carries our name, was not created by us and is live or still
// being created/stopped is somebody else's: Start refuses with ErrSessionInUse
// and names the holder and its state.
func TestStartRefusesALiveSessionHoldingTheName(t *testing.T) {
	b, fe := openFake(t)
	fe.addForeignSession("someone")
	_ = os.WriteFile(filepath.Join(fe.rt(), "someone", "name"), []byte(b.sessionName("mine")), 0o600)
	err := b.Start(ctxT(t), sandbox.Session{ID: "mine", HostCwd: fe.cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) || !strings.Contains(err.Error(), "someone") {
		t.Fatalf("want ErrSessionInUse naming the holder, got %v", err)
	}
	if !contains(fe.sessionIDs(), "someone") {
		t.Fatal("the foreign session was touched")
	}
}

// A foreign session that is still "creating" or "stopping" is not dead: Start
// refuses it as in use, never calls it a stale leftover and never prunes it.
func TestTransitionalForeignSessionIsNeverTreatedAsDead(t *testing.T) {
	for _, state := range []string{"creating", "stopping"} {
		t.Run(state, func(t *testing.T) {
			b, fe := openFake(t)
			fe.addForeignSession("trans")
			_ = os.WriteFile(filepath.Join(fe.rt(), "trans", "state"), []byte(state), 0o600)
			_ = os.WriteFile(filepath.Join(fe.rt(), "trans", "name"), []byte(b.sessionName("mine")), 0o600)
			pre := strings.Count(calls(t, fe), "ls --prune")
			err := b.Start(ctxT(t), sandbox.Session{ID: "mine", HostCwd: fe.cwd})
			if !errors.Is(err, sandbox.ErrSessionInUse) || !strings.Contains(err.Error(), state) {
				t.Fatalf("want ErrSessionInUse naming %q, got %v", state, err)
			}
			if strings.Count(calls(t, fe), "ls --prune") != pre {
				t.Error("a transitional session triggered a prune")
			}
			if strings.Contains(calls(t, fe), "run --detach") {
				t.Error("Start went on to run next to a transitional holder of its name")
			}
		})
	}
	// The same for a transitional directory of OUR chat id.
	b, fe := openFake(t)
	fe.addForeignSession("mine")
	_ = os.WriteFile(filepath.Join(fe.rt(), "mine", "state"), []byte("stopping"), 0o600)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "mine", HostCwd: fe.cwd}); !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("a stopping dir of this id must be in use, got %v", err)
	}
}

// Ended is exactly stopped/crashed/stale; creating and stopping are
// transitional, live is neither.
func TestLsEntryEndedAndTransitional(t *testing.T) {
	for state, want := range map[string][3]bool{ // live, ended, transitional
		"live": {true, false, false}, "running": {true, false, false},
		"stopped": {false, true, false}, "crashed": {false, true, false}, "stale": {false, true, false},
		"creating": {false, false, true}, "stopping": {false, false, true},
		"": {false, false, false}, "weird": {false, false, false},
	} {
		e := lsEntry{State: state}
		if got := [3]bool{e.Live(), e.Ended(), e.transitional()}; got != want {
			t.Errorf("state %q: live/ended/transitional = %v, want %v", state, got, want)
		}
	}
}

// waitEnded does not count a session that is still stopping as over.
func TestWaitEndedIgnoresATransitionalSession(t *testing.T) {
	b, fe := openFake(t)
	fe.addForeignSession("slow")
	_ = os.WriteFile(filepath.Join(fe.rt(), "slow", "state"), []byte("stopping"), 0o600)
	if b.waitEnded(ctxT(t), "slow", 300*time.Millisecond) {
		t.Fatal("a stopping session was reported as ended")
	}
	_ = os.WriteFile(filepath.Join(fe.rt(), "slow", "state"), []byte("stopped"), 0o600)
	if !b.waitEnded(ctxT(t), "slow", 2*time.Second) {
		t.Fatal("a stopped session was not reported as ended")
	}
	if !b.waitEnded(ctxT(t), "no-such-session", time.Second) {
		t.Fatal("a missing session is over")
	}
}

// killSession (Reconcile of a dead owner's LIVE session): `hisn stop` first,
// KILL only if it is still served, and never on an id that is not our instance.
func TestKillSessionStopsThenKillsAndVerifiesTheInstance(t *testing.T) {
	b, fe := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "ks", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	rec, _ := b.readRecord("ks")
	// A token that is not ours: nothing may be signalled.
	if err := b.killSession(ctxT(t), "ks", newToken()); err == nil {
		t.Fatal("killSession on a foreign instance must refuse")
	}
	c := calls(t, fe)
	if strings.Contains(c, "stop --session ks") || strings.Contains(c, "kill --session ks") {
		t.Fatalf("a foreign instance was signalled:\n%s", c)
	}
	// Our token: stop ends it; no KILL is needed.
	if err := b.killSession(ctxT(t), "ks", rec.Token); err != nil {
		t.Fatal(err)
	}
	c = calls(t, fe)
	if !strings.Contains(c, "stop --session ks") || strings.Contains(c, "kill --session ks --signal KILL") {
		t.Fatalf("want stop without KILL:\n%s", c)
	}
	// Stop failing: the KILL step follows.
	b2, fe2 := openFake(t)
	if err := b2.Start(ctxT(t), sandbox.Session{ID: "kk", HostCwd: fe2.cwd}); err != nil {
		t.Fatal(err)
	}
	rec2, _ := b2.readRecord("kk")
	_ = os.WriteFile(filepath.Join(fe2.state, "stop_fail"), nil, 0o600)
	if err := b2.killSession(ctxT(t), "kk", rec2.Token); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(calls(t, fe2), "kill --session kk --signal KILL") {
		t.Fatalf("no KILL after a failed stop:\n%s", calls(t, fe2))
	}
}

// state "stopped" and "crashed" are not live, so a readiness probe never
// accepts them and `ready` says which one it saw.
func TestReadyReportsTheRealState(t *testing.T) {
	b, fe := openFake(t)
	for state, mk := range map[string]func(dir string){
		"stopped": func(d string) { _ = os.WriteFile(filepath.Join(d, "stopped"), nil, 0o600) },
		"crashed": func(d string) { _ = os.WriteFile(filepath.Join(d, "sup.pid"), []byte("1"), 0o600) },
	} {
		id := "rs-" + state
		dir := filepath.Join(fe.rt(), id)
		_ = os.MkdirAll(dir, 0o700)
		mk(dir)
		ok, why := b.ready(ctxT(t), id, "")
		if ok || !strings.Contains(why, state) {
			t.Errorf("%s: ready = %v %q", state, ok, why)
		}
	}
	// A name that does not point at the session is not ready either.
	fe.addForeignSession("named")
	_ = os.WriteFile(filepath.Join(fe.rt(), "named", "name"), []byte("other"), 0o600)
	if ok, why := b.ready(ctxT(t), "named", "wanted"); ok || !strings.Contains(why, "want") {
		t.Errorf("name mismatch: ready = %v %q", ok, why)
	}
}

// An unknown OR MISSING ls schema is an error, not a guess; an empty document
// must not read as "every session vanished".
func TestListSessionsRefusesAnUnknownOrMissingSchema(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	for name, doc := range map[string]string{
		"unknown":     `{"schema":"hisn-ls/v9","sessions":[]}`,
		"missing":     `{"sessions":[]}`,
		"empty":       `{}`,
		"null":        `null`,
		"empty-value": `{"schema":"","sessions":[]}`,
	} {
		if err := os.WriteFile(filepath.Join(fe.state, "ls_raw"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := b.listSessions(ctxT(t)); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
			t.Errorf("%s: want an unsupported-schema error, got %v", name, err)
		}
	}
}

// The client label is cut on a rune boundary: valid UTF-8, at most MaxLabel
// bytes, identical on the ctl path and the CLI path.
func TestClientLabelIsRuneSafeAndConsistent(t *testing.T) {
	long := strings.Repeat("é", 60) // 120 bytes, 2 per rune
	got := ctl.TruncateLabel(long)
	if len(got) > ctl.MaxLabel || !utf8.ValidString(got) || strings.Contains(got, "\uFFFD") || got != strings.Repeat("é", ctl.MaxLabel/2) {
		t.Fatalf("TruncateLabel = %q (len %d)", got, len(got))
	}
	// A rune straddling the limit is dropped whole, not split.
	edge := strings.Repeat("a", ctl.MaxLabel-1) + "é"
	if g := ctl.TruncateLabel(edge); g != strings.Repeat("a", ctl.MaxLabel-1) {
		t.Fatalf("straddling rune: %q", g)
	}
	if g := ctl.TruncateLabel("bad\xffbyte"); !utf8.ValidString(g) {
		t.Fatalf("invalid UTF-8 survived: %q", g)
	}
	b := &Backend{opts: Options{ClientLabel: long}}
	if b.clientLabel() != got {
		t.Fatalf("Options.ClientLabel is not bounded like the ctl label: %q", b.clientLabel())
	}
}

// Request ids never repeat across sessions of one Wakil: each session gets its
// own instance prefix.
func TestRequestIDsDifferAcrossSessions(t *testing.T) {
	b, fe := openFake(t)
	var insts []string
	for _, id := range []string{"r1", "r2"} {
		if err := b.Start(ctxT(t), sandbox.Session{ID: id, HostCwd: fe.cwd}); err != nil {
			t.Fatal(err)
		}
		insts = append(insts, b.cur.inst)
		if _, info := run(t, b, "true"); info.Code != 0 {
			t.Fatalf("%+v", info)
		}
	}
	if insts[0] == insts[1] || insts[0] == "" {
		t.Fatalf("instance prefixes must differ per session: %q", insts)
	}
}

// hisn-ls/v1 (hisn <= 0.3.3) still parses: live and stale.
func TestListSessionsStillReadsV1(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	fe.addForeignSession("old")
	if err := os.WriteFile(filepath.Join(fe.state, "ls_v1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := b.listSessions(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if e := m["old"]; !e.Live() || e.Name != "" {
		t.Fatalf("v1 row = %+v", e)
	}
}

// hisn >= 0.4.1 keeps a stopped session's name reserved (`hisn run --name`
// fails with "name ... is already in use"), so a Start over a stopped
// directory of this chat prunes first. 0.4.0 keeps the stopped directory
// (TestStartAfterStopReusesTheNameWithoutPruning).
func TestStartAfterStopPrunesOn041(t *testing.T) {
	fe := newFakeEnv(t)
	_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.4.2"), 0o600)
	b, err := Open(ctxT(t), fe.options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	fe.setMode("detach041")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "reuse41", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	before := strings.Count(calls(t, fe), "ls --prune")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "reuse41", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("restart over a stopped directory: %v", err)
	}
	if got := strings.Count(calls(t, fe), "ls --prune"); got != before+1 {
		t.Errorf("0.4.1 restart over a stopped directory pruned %d time(s), want 1", got-before)
	}
}
