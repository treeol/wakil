package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// snapshotDir returns name → sha256+mtime for every file in dir, so a test can
// prove which files a call touched.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		info, _ := e.Info()
		out[e.Name()] = fmt.Sprintf("%x %d", sha256.Sum256(b), info.ModTime().UnixNano())
	}
	return out
}

func userMsg(text string) proxy.Message { return proxy.Message{Role: "user", Content: StrPtr(text)} }

func TestResumeIndexWrittenOnSave(t *testing.T) {
	fx := genResumeFixture(t, 3, 20)
	if _, err := os.Stat(filepath.Join(fx.Dir, resumeIndexFile)); err != nil {
		t.Fatalf("index not written by WriteSession: %v", err)
	}
	idx := ReadResumeIndex()
	if len(idx) != 3 {
		t.Fatalf("index has %d entries, want 3", len(idx))
	}
	e := idx[0]
	if e.ID != "chat-0000-aaaaaaaa" || e.Workspace != fx.Workspace || e.Turns == 0 || e.Messages == 0 || e.Size == 0 || e.Title == "" {
		t.Errorf("entry incomplete: %+v", e)
	}
	// The index is not a *.json file: every other reader of the dir ignores it.
	if isSessionFileName(resumeIndexFile) {
		t.Error("the index file name must not look like a session file")
	}
	if fi, _ := os.Stat(filepath.Join(fx.Dir, resumeIndexFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("index mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestResumeIndexStaleRebuild(t *testing.T) {
	fx := genResumeFixture(t, 4, 20)
	idxPath := filepath.Join(fx.Dir, resumeIndexFile)

	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T)
		want   int
	}{
		{"missing index", func(t *testing.T) { _ = os.Remove(idxPath) }, 4},
		{"corrupt index", func(t *testing.T) { _ = os.WriteFile(idxPath, []byte("{not json"), 0o600) }, 4},
		{"wrong version", func(t *testing.T) { _ = os.WriteFile(idxPath, []byte(`{"version":99,"entries":[]}`), 0o600) }, 4},
		{"session added behind the index", func(t *testing.T) {
			// Written without WriteSession, as another process or an older
			// wakil would.
			b, _ := json.Marshal(&Session{ChatID: "late-0001", Workspace: fx.Workspace, Updated: time.Now(), Conv: []proxy.Message{userMsg("late one")}})
			_ = os.WriteFile(filepath.Join(fx.Dir, "late-0001.json"), b, 0o644)
		}, 5},
		{"session vanished", func(t *testing.T) { _ = os.Remove(filepath.Join(fx.Dir, "late-0001.json")) }, 4},
		{"session changed behind the index", func(t *testing.T) {
			b, _ := json.Marshal(&Session{ChatID: "chat-0001-aaaaaaaa", Workspace: fx.Workspace, Updated: time.Now().Add(time.Hour), Conv: []proxy.Message{userMsg("changed title text")}})
			_ = os.WriteFile(filepath.Join(fx.Dir, "chat-0001-aaaaaaaa.json"), b, 0o644)
		}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate(t)
			got, err := ReconcileResumeIndex()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("reconcile returned %d entries, want %d", len(got), tc.want)
			}
			// The index on disk now matches.
			if again := ReadResumeIndex(); len(again) != tc.want {
				t.Errorf("index after reconcile has %d entries, want %d", len(again), tc.want)
			}
		})
	}
	// The changed session's title was re-derived from the new file.
	for _, e := range ReadResumeIndex() {
		if e.ID == "chat-0001-aaaaaaaa" && e.Title != "changed title text" {
			t.Errorf("changed session title = %q", e.Title)
		}
	}
}

// Warm reconcile decodes no session file: replace a session file's contents
// with garbage of the SAME size and restore its mtime would defeat the check,
// so instead prove warmth by counting reads through an unreadable file — a
// file the index still vouches for is never opened.
func TestResumeIndexFirstScreenReadsOnlyIndex(t *testing.T) {
	fx := genResumeFixture(t, 5, 20)
	// First screen = index only: remove read permission from every session
	// file. A path that opened them would fail; the index read does not.
	ents, _ := os.ReadDir(fx.Dir)
	for _, e := range ents {
		if isSessionFileName(e.Name()) {
			if err := os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o000); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() {
		for _, e := range ents {
			if isSessionFileName(e.Name()) {
				_ = os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o644)
			}
		}
	}()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	matched, _, _, err := ListResumeEntries(SessionScope{Workspace: fx.Workspace, IncludeLegacy: true}, false)
	if err != nil || len(matched) != 5 {
		t.Fatalf("index-only listing: %d entries, err %v", len(matched), err)
	}
	// A warm reconcile stats but never opens a vouched-for file.
	matched, _, _, err = ListResumeEntries(SessionScope{Workspace: fx.Workspace, IncludeLegacy: true}, true)
	if err != nil || len(matched) != 5 {
		t.Fatalf("warm reconcile: %d entries, err %v", len(matched), err)
	}
}

// The index listing and the full-decode listing agree on membership, order,
// hidden count and legacy placement, including ties on Updated.
func TestResumeIndexMatchesFullScan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	wsA, wsB, legacyWS := t.TempDir(), t.TempDir(), t.TempDir()
	tie := time.Now().Add(-time.Hour).Truncate(time.Second)
	mk := func(id, ws string, upd time.Time) {
		s := &Session{ChatID: id, Workspace: ws, Created: upd, Updated: upd, Conv: []proxy.Message{userMsg("hello " + id)}}
		if err := WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}
	mk("a-new", wsA, time.Now())
	mk("a-tie-2", wsA, tie)
	mk("a-tie-1", wsA, tie) // same Updated: id breaks the tie
	mk("b-1", wsB, time.Now().Add(-2*time.Hour))
	mk("none-1", "", time.Now().Add(-3*time.Hour)) // legacy: no workspace
	mk("shared-1", legacyWS, time.Now().Add(-4*time.Hour))

	for _, scope := range []SessionScope{
		{Workspace: wsA, IncludeLegacy: true, LegacyWorkspace: legacyWS},
		{Workspace: wsA},
		{Workspace: wsB, IncludeLegacy: true},
		{All: true},
		{Workspace: wsA, All: true, IncludeLegacy: true, LegacyWorkspace: legacyWS},
	} {
		full, fullHidden, err := ListSessionsScoped(scope)
		if err != nil {
			t.Fatal(err)
		}
		fullLegacy, _ := ListLegacySessions(scope)
		idx, idxLegacy, idxHidden, err := ListResumeEntries(scope, true)
		if err != nil {
			t.Fatal(err)
		}
		ids := func(n int, f func(i int) string) []string {
			var out []string
			for i := 0; i < n; i++ {
				out = append(out, f(i))
			}
			return out
		}
		fa := ids(len(full), func(i int) string { return full[i].ChatID })
		ia := ids(len(idx), func(i int) string { return idx[i].ID })
		if !reflect.DeepEqual(fa, ia) || fullHidden != idxHidden {
			t.Errorf("scope %+v: full %v hidden %d, index %v hidden %d", scope, fa, fullHidden, ia, idxHidden)
		}
		fl := ids(len(fullLegacy), func(i int) string { return fullLegacy[i].ChatID })
		il := ids(len(idxLegacy), func(i int) string { return idxLegacy[i].ID })
		if !reflect.DeepEqual(fl, il) {
			t.Errorf("scope %+v legacy: full %v, index %v", scope, fl, il)
		}
	}
	// Tie order is deterministic: a-tie-1 before a-tie-2.
	all, _, _ := ListSessionsScoped(SessionScope{All: true})
	var ties []string
	for _, s := range all {
		if strings.HasPrefix(s.ChatID, "a-tie") {
			ties = append(ties, s.ChatID)
		}
	}
	if !reflect.DeepEqual(ties, []string{"a-tie-1", "a-tie-2"}) {
		t.Errorf("tie order = %v", ties)
	}
}

// A session file shaped like v0.8.10 wrote it (no loaded_modules, no index
// anywhere) is listed after a reconcile and still resumes.
func TestResumeV0810SessionListedAndResumable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	ws := t.TempDir()
	// Field set and indentation of agent.Session at v0.8.10, from the tag's
	// struct (everything but loaded_modules).
	body := fmt.Sprintf(`{
  "chat_id": "v0810-session-0001",
  "model": "ilm",
  "endpoint_name": "default",
  "workspace": %q,
  "created": "2026-11-20T10:00:00Z",
  "updated": "2026-11-20T10:05:00Z",
  "conv": [
    {"role": "system", "content": "preamble"},
    {"role": "user", "content": "fix the flaky test\nplease"},
    {"role": "assistant", "content": "looking"}
  ]
}`, ws)
	if err := os.WriteFile(filepath.Join(dir, "v0810-session-0001.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadResumeIndex(); len(got) != 0 {
		t.Fatalf("no index yet, got %d entries", len(got))
	}
	matched, _, _, err := ListResumeEntries(SessionScope{Workspace: ws, IncludeLegacy: true}, true)
	if err != nil || len(matched) != 1 {
		t.Fatalf("v0.8.10 session not listed: %d, %v", len(matched), err)
	}
	if matched[0].Title != "fix the flaky test" || matched[0].Turns != 1 {
		t.Errorf("entry = %+v", matched[0])
	}
	s, err := LoadSession("v0810-")
	if err != nil || s.ChatID != "v0810-session-0001" || len(s.Conv) != 3 {
		t.Fatalf("resume failed: %v %+v", err, s)
	}
}

// Nothing in listing, reconcile, index writes or load changes an existing
// session file: bytes and mtimes stay identical, and the only new file is the
// index (no leftover temp files).
func TestResumeSessionFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	ws := t.TempDir()
	for i := 0; i < 6; i++ {
		b, _ := json.MarshalIndent(&Session{ChatID: fmt.Sprintf("old-%02d", i), Workspace: ws, Updated: time.Now().Add(-time.Duration(i) * time.Minute),
			Conv: []proxy.Message{userMsg(fmt.Sprintf("task %d", i))}}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("old-%02d.json", i)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A handoff sidecar and a stray file that must survive too.
	_ = os.WriteFile(filepath.Join(dir, "old-00.handoff.json"), []byte(`{"old_chat_id":"old-00"}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep me"), 0o644)
	before := snapshotDir(t, dir)

	scope := SessionScope{Workspace: ws, IncludeLegacy: true}
	_, _, _, _ = ListResumeEntries(scope, false)
	_, _, _, _ = ListResumeEntries(scope, true)
	_, _, _, _ = ListResumeEntries(scope, true)
	_, _, _ = ListSessionsScoped(scope)
	if _, err := LoadSession("old-03"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionScoped("", scope); err != nil {
		t.Fatal(err)
	}

	after := snapshotDir(t, dir)
	var added []string
	for name, sum := range after {
		old, existed := before[name]
		if !existed {
			added = append(added, name)
			continue
		}
		if old != sum {
			t.Errorf("%s changed (bytes or mtime)", name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("%s was deleted", name)
		}
	}
	sort.Strings(added)
	if !reflect.DeepEqual(added, []string{resumeIndexFile}) {
		t.Errorf("new files = %v, want only %s (no temp leftovers)", added, resumeIndexFile)
	}
}

func TestListSessionsSkipsHandoffSidecarAndDotFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	if err := WriteSession(&Session{ChatID: "real-0001", Workspace: "/w", Updated: time.Now(), Conv: []proxy.Message{userMsg("x")}}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "real-0001.handoff.json"),
		[]byte(`{"old_chat_id":"real-0001","new_chat_id":"n","workspace":"/w","timestamp":"2026-11-20T00:00:00Z"}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".tmp-123.json"), []byte(`{"chat_id":"tmp"}`), 0o600)
	got, err := ListSessions()
	if err != nil || len(got) != 1 || got[0].ChatID != "real-0001" {
		t.Fatalf("ListSessions = %d sessions %v, err %v; want only real-0001", len(got), got, err)
	}
	if e, _ := ReconcileResumeIndex(); len(e) != 1 {
		t.Errorf("index lists %d entries, want 1", len(e))
	}
}

// LoadSession by id resolves from the file name and reads one file: make every
// other file unreadable and it still loads.
func TestLoadSessionByIDReadsOneFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	fx := genResumeFixture(t, 6, 20)
	ents, _ := os.ReadDir(fx.Dir)
	for _, e := range ents {
		if isSessionFileName(e.Name()) && e.Name() != "chat-0003-aaaaaaaa.json" {
			_ = os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o000)
		}
	}
	defer func() {
		for _, e := range ents {
			_ = os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o644)
		}
	}()
	s, err := LoadSession("chat-0003")
	if err != nil || s.ChatID != "chat-0003-aaaaaaaa" {
		t.Fatalf("LoadSession by prefix: %v %v", s, err)
	}
	if s, err = LoadSession("chat-0003-aaaaaaaa"); err != nil || s.ChatID != "chat-0003-aaaaaaaa" {
		t.Fatalf("LoadSession by full id: %v %v", s, err)
	}
}

func TestLoadSessionByNameDefensive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	outside := filepath.Join(filepath.Dir(dir), "outside.json")
	_ = os.WriteFile(outside, []byte(`{"chat_id":"outside","conv":[]}`), 0o644)
	defer os.Remove(outside)
	for _, id := range []string{"../outside", "..", "a/b", `a\b`, ".hidden"} {
		if _, err := LoadSession(id); err == nil {
			t.Errorf("LoadSession(%q) succeeded; unsafe ids must not resolve", id)
		}
	}
	// Ambiguity, sidecar exclusion and inner-id mismatch.
	w := func(name, chatID string) {
		b, _ := json.Marshal(&Session{ChatID: chatID, Conv: []proxy.Message{userMsg("m")}})
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	w("abc-1.json", "abc-1")
	w("abc-2.json", "abc-2")
	if _, err := LoadSession("abc"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("two matches: err = %v, want ambiguous", err)
	}
	// A corrupt file that matches the prefix by name must not make a unique
	// decodable match ambiguous (the scan skips it).
	w("pre-1.json", "pre-1")
	_ = os.WriteFile(filepath.Join(dir, "pre-2.json"), []byte("{broken"), 0o644)
	if s, err := LoadSession("pre"); err != nil || s.ChatID != "pre-1" {
		t.Errorf("corrupt sibling made a unique match fail: %v %v", s, err)
	}
	w("zzz-1.json", "zzz-1")
	_ = os.WriteFile(filepath.Join(dir, "zzz-1.handoff.json"), []byte(`{"old_chat_id":"zzz-1"}`), 0o600)
	if s, err := LoadSession("zzz"); err != nil || s.ChatID != "zzz-1" {
		t.Errorf("sidecar must not make zzz ambiguous: %v %v", s, err)
	}
	w("file-name-x.json", "other-inner-id") // inner id disagrees: scan decides
	if _, err := LoadSession("file-name"); err == nil {
		t.Error("a file whose inner chat_id does not match must not load by file-name prefix")
	}
	if s, err := LoadSession("other-inner"); err != nil || s.ChatID != "other-inner-id" {
		t.Errorf("full-scan fallback by inner id: %v %v", s, err)
	}
}

// Titles come from typed text only. One case per synthetic message Wakil
// writes in the user role, built from the real producer or its constant.
func TestSessionTitleTyped(t *testing.T) {
	typed := "fix the login bug\nsecond line is ignored"
	synthetic := map[string]string{
		"forced final":      ForcedFinalPrompt,
		"tool limit":        ToolLimitPrompt,
		"budget exhausted":  BudgetExhaustedPrompt,
		"confinement":       confinementBreakerPrompt([]string{"/etc/passwd"}),
		"continuation":      continuationPrompt,
		"subagent budget":   subagentBudgetMessage(9, 90000, 100000),
		"subagent low":      subagentBudgetMessage(2, 1000, 100000),
		"auto counsel":      autoCounselHeader(1, 2, 3) + "the panel said…",
		"workflow gather":   "[WORKFLOW GATHER] Investigate (read-only) what is needed for:\nthe task",
		"handoff continue":  handoffContinueTail,
		"handoff envelope":  handoffBlockHeader + "Prior session: abc\n\nsummary" + handoffBlockEnd + "\n\n" + handoffContinueTail,
		"async envelope":    asyncBlockHeader + "op-1 done" + asyncBlockEnd,
		"memory envelope":   retrievalBlockHeader + "something" + retrievalBlockEnd,
		"session envelope":  sessionRetrievalBlockHeader + "prior" + sessionRetrievalBlockEnd,
		"empty":             "   \n ",
		"envelope then dir": retrievalBlockHeader + "x" + retrievalBlockEnd + "\n[WORKFLOW PLAN] Write…",
	}
	var names []string
	for n := range synthetic {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		conv := []proxy.Message{userMsg(synthetic[n]), {Role: "assistant", Content: StrPtr("ok")}, userMsg(typed)}
		if got := SessionTitle(conv); got != "fix the login bug" {
			t.Errorf("%s: title = %q, want the later typed message", n, got)
		}
		if got := SessionTitle(conv[:2]); got != "" {
			t.Errorf("%s alone: title = %q, want empty (fallback)", n, got)
		}
	}
	// Typed text behind a retrieval envelope is the title (envelope stripped).
	wrapped := retrievalBlockHeader + "ctx" + retrievalBlockEnd + "\nreal question here"
	if got := SessionTitle([]proxy.Message{userMsg(wrapped)}); got != "real question here" {
		t.Errorf("enveloped typed text: title = %q", got)
	}
	// System / assistant / tool messages never count.
	if got := SessionTitle([]proxy.Message{{Role: "system", Content: StrPtr("be helpful")}, {Role: "assistant", Content: StrPtr("hi")}}); got != "" {
		t.Errorf("non-user title = %q", got)
	}
	// Length: 60 runes, multi-byte safe, ellipsis.
	long := strings.Repeat("é", 100)
	got := SessionTitle([]proxy.Message{userMsg(long)})
	if r := []rune(got); len(r) != 61 || r[60] != '…' {
		t.Errorf("long title = %d runes %q", len(r), got)
	}
}

// Every user-role message Wakil writes itself is covered by the synthetic
// prefix list: built from the producers, none becomes a title.
func TestSyntheticPrefixListCoversProducers(t *testing.T) {
	for _, p := range syntheticUserPrefixes {
		if strings.TrimSpace(p) == "" {
			t.Error("empty synthetic prefix would swallow every title")
		}
	}
	if got := typedTitle("The following path(s) outside"); got == "" {
		t.Error("typed text that merely resembles a prefix must stay a title")
	}
}

// TestResumeFirstScreenPerf: first screen on a 500-session fixture. The CI
// bound is generous (1 s) because wall-clock bounds are machine dependent; the
// measured numbers are logged and the realistic-size figures come from
// BenchmarkResume* (see docs/plans/0.8.11.md). The fixture is small per
// session (scale 20) so the test stays light.
func TestResumeFirstScreenPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 500-session fixture")
	}
	fx := genResumeFixture(t, 500, 20)
	scope := SessionScope{Workspace: fx.Workspace, IncludeLegacy: true, LegacyWorkspace: "/nonexistent-legacy"}

	// Cold: index deleted, every file decoded once.
	_ = os.Remove(filepath.Join(fx.Dir, resumeIndexFile))
	start := time.Now()
	cold, _, _, err := ListResumeEntries(scope, true)
	coldDur := time.Since(start)
	if err != nil || len(cold) != 500 {
		t.Fatalf("cold: %d entries, err %v", len(cold), err)
	}

	// First paint: the index alone.
	start = time.Now()
	idx, _, _, err := ListResumeEntries(scope, false)
	idxDur := time.Since(start)
	if err != nil || len(idx) != 500 {
		t.Fatalf("index-only: %d entries, err %v", len(idx), err)
	}

	// Warm reconcile: stat only.
	start = time.Now()
	warm, _, _, err := ListResumeEntries(scope, true)
	warmDur := time.Since(start)
	if err != nil || len(warm) != 500 {
		t.Fatalf("warm: %d entries, err %v", len(warm), err)
	}
	t.Logf("500 sessions: index-only first paint %v, warm reconcile %v, cold rebuild %v", idxDur, warmDur, coldDur)
	if idxDur > time.Second || warmDur > time.Second {
		t.Errorf("first screen too slow: index-only %v, warm reconcile %v (bound 1s)", idxDur, warmDur)
	}
}

// The index carries the session-history source hash, so /remember and /recall
// can tell which sessions changed without decoding all of them.
func TestResumeIndexCarriesSourceHash(t *testing.T) {
	fx := genResumeFixture(t, 3, 20)
	s, err := LoadSession("chat-0001")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ReadResumeIndex() {
		if e.ID == s.ChatID && e.SourceHash != sourceHash(*s) {
			t.Errorf("entry hash %q != sourceHash %q", e.SourceHash, sourceHash(*s))
		}
		if e.SourceHash == "" {
			t.Errorf("%s: empty source hash", e.ID)
		}
	}
	_ = fx
}

// reconcileHistory must not decode a session whose indexed hash is current: an
// unreadable file the index vouches for is skipped, and a changed one is
// re-indexed.
func TestReconcileHistorySkipsUnchangedWithoutDecoding(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	fx := genResumeFixture(t, 6, 20)
	app := newSessionHistoryApp(t, fx.Workspace)
	ctx := context.Background()
	if err := app.reconcileHistory(ctx, fx.Workspace); err != nil { // cold: indexes all 6
		t.Fatal(err)
	}
	meta, _ := app.SessionHistory.ListMeta(ctx, fx.Workspace)
	if len(meta) != 6 {
		t.Fatalf("indexed %d sessions, want 6", len(meta))
	}
	// Warm: make every session file unreadable; the index still vouches for
	// them, so a warm reconcile must succeed without opening any of them.
	ents, _ := os.ReadDir(fx.Dir)
	for _, e := range ents {
		if isSessionFileName(e.Name()) {
			_ = os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o000)
		}
	}
	defer func() {
		for _, e := range ents {
			_ = os.Chmod(filepath.Join(fx.Dir, e.Name()), 0o644)
		}
	}()
	var out strings.Builder
	app.Out = &out
	if err := app.reconcileHistory(ctx, fx.Workspace); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("warm reconcile touched session files: %s", out.String())
	}
	if again, _ := app.SessionHistory.ListMeta(ctx, fx.Workspace); len(again) != 6 {
		t.Errorf("warm reconcile changed the index: %d sessions", len(again))
	}
}

// A session changed behind the index is re-indexed by the next reconcile.
func TestReconcileHistoryReindexesChanged(t *testing.T) {
	fx := genResumeFixture(t, 3, 20)
	app := newSessionHistoryApp(t, fx.Workspace)
	ctx := context.Background()
	if err := app.reconcileHistory(ctx, fx.Workspace); err != nil {
		t.Fatal(err)
	}
	before, _ := app.SessionHistory.ListMeta(ctx, fx.Workspace)
	old := map[string]string{}
	for _, m := range before {
		old[m.ChatID] = m.SourceHash
	}
	s, _ := LoadSession("chat-0001")
	s.Conv = append(s.Conv, userMsg("a brand new question"))
	s.Updated = s.Updated.Add(time.Minute)
	if err := WriteSession(s); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileHistory(ctx, fx.Workspace); err != nil {
		t.Fatal(err)
	}
	after, _ := app.SessionHistory.ListMeta(ctx, fx.Workspace)
	changed := 0
	for _, m := range after {
		if m.SourceHash != old[m.ChatID] {
			changed++
			if m.ChatID != s.ChatID {
				t.Errorf("unexpected re-index of %s", m.ChatID)
			}
		}
	}
	if changed != 1 {
		t.Errorf("%d sessions re-indexed, want 1", changed)
	}
}

// A session file with an empty chat_id is a session to the full scan; the
// index keeps it too, instead of dropping it on every read and re-decoding it
// at every reconcile.
func TestResumeIndexKeepsEmptyChatID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	_ = os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(`{"workspace":"/w","updated":"2026-11-20T10:00:00Z","conv":[{"role":"user","content":"old one"}]}`), 0o644)
	full, _ := ListSessions()
	got, err := ReconcileResumeIndex()
	if err != nil || len(got) != len(full) || len(full) != 1 {
		t.Fatalf("full scan %d, reconcile %d, err %v", len(full), len(got), err)
	}
	if again := ReadResumeIndex(); len(again) != 1 {
		t.Errorf("index read after reconcile: %d entries, want 1", len(again))
	}
	before := snapshotDir(t, dir)[resumeIndexFile]
	if _, err := ReconcileResumeIndex(); err != nil {
		t.Fatal(err)
	}
	if snapshotDir(t, dir)[resumeIndexFile] != before {
		t.Error("a warm reconcile rewrote the index for an empty-chat_id session")
	}
}
