package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/styleguide"
)

func enabledStyleApp(t *testing.T, url string) *App {
	t.Helper()
	app := newTestApp(url, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	catalog, err := styleguide.NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.SetStyleGuideCatalog(catalog)
	return app
}

func TestStyleGuideRequestActivationAndNoPersistence(t *testing.T) {
	var mu sync.Mutex
	var requests [][]proxy.Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []proxy.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", contentChunk("done"))
	}))
	defer srv.Close()
	app := enabledStyleApp(t, srv.URL)
	if _, err := app.Send(context.Background(), "Tell me about your day"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(context.Background(), "Implement a Python function"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(context.Background(), "Continue"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requestHasStyle(requests[0]) {
		t.Fatal("unrelated first request activated a guide")
	}
	if !requestHasStyle(requests[1]) || !strings.Contains(DerefStr(requests[1][0].Content), "Language style guidance") {
		t.Fatalf("style not leading system message: %+v", requests[1])
	}
	if !requestHasStyle(requests[2]) {
		t.Fatal("explicit selection should remain active on the next request")
	}
	for _, m := range app.Conv {
		if strings.Contains(DerefStr(m.Content), "Language style guidance") {
			t.Fatal("guide was persisted in Conv")
		}
	}
}

func TestStyleGuidePathEvidenceTurnScopedAndNextIteration(t *testing.T) {
	var mu sync.Mutex
	var requests [][]proxy.Message
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []proxy.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, body.Messages)
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: "+toolCallFrames("r1", "read_file", `{"path":"script.py"}`)[0]+"\n\n")
			for _, frame := range toolCallFrames("r1", "read_file", `{"path":"script.py"}`)[1:] {
				fmt.Fprint(w, "data: "+frame+"\n\n")
			}
		} else {
			fmt.Fprintf(w, "data: %s\n\n", contentChunk("done"))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	exec := newFakeExecutor()
	exec.files["script.py"] = "print('hello')"
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	catalog, err := styleguide.NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.SetStyleGuideCatalog(catalog)
	if _, err := app.Send(context.Background(), "read the script"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Send(context.Background(), "Continue"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requestHasStyle(requests[0]) || !requestHasStyle(requests[1]) {
		t.Fatal("tool path should affect only the next inference in its turn")
	}
	if requestHasStyle(requests[2]) {
		t.Fatal("path evidence leaked into the next request")
	}
}

func TestStyleGuideSurvivesCollapseAndResets(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.SetContextMode(configContextLongSession())
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("stable preamble"), Pinned: true},
		{Role: "user", Turn: 1, Content: StrPtr("Implement Python")},
		{Role: "assistant", Turn: 1, Content: StrPtr("done")},
	}
	app.collapsed = map[int]CollapseRecord{1: {Turn: 1, Note: "archived"}}
	app.prepareStyleGuides("Implement a Python function")
	messages := app.requestMessages()
	if !requestHasStyle(messages) || app.styleSelection().Bytes() == 0 {
		t.Fatal("active guide missing from collapsed request")
	}
	for _, m := range app.Conv {
		if strings.Contains(DerefStr(m.Content), "Language style guidance") {
			t.Fatal("collapse projection persisted a guide")
		}
	}
	app.NewConversationTransition("new")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("new conversation retained selection")
	}
	app.prepareStyleGuides("Implement a Python function")
	app.InstallSession(&Session{ChatID: "loaded", Conv: []proxy.Message{{Role: "user", Content: StrPtr("Implement Go")}}})
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("loaded session retained selection")
	}
	app.prepareStyleGuides("Implement a Python function")
	app.SetConv([]proxy.Message{{Role: "user", Content: StrPtr("Implement Go")}})
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("SetConv retained selection")
	}
}

func configContextLongSession() string { return "long-session" }

func TestStyleGuideInsertionAndSize(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("preamble")},
		{Role: "system", Content: StrPtr("extension")},
		{Role: "user", Content: StrPtr("request")},
	}
	app.prepareStyleGuides("Implement a Python function")
	got := app.requestMessages()
	if DerefStr(got[1].Content) != app.styleSelection().Text() {
		t.Fatalf("style insertion = %+v", got)
	}
	if len(app.Conv) != 3 || app.sentSize(app.Conv) != TranscriptSize(got) {
		t.Fatal("measured and delivered guide bytes differ")
	}
	app.styleState.path = []string{"go"}
	app.publishStyleSelection()
	if len(got) != 4 || len(app.requestMessages()) != 4 || !strings.Contains(DerefStr(got[1].Content), "python") || strings.Contains(DerefStr(got[1].Content), "## go") {
		t.Fatal("adding guide mutated the earlier independent request slice")
	}
}

func TestStyleGuideFinalHardMaxCheck(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("done")})
	defer srv.Close()
	app := enabledStyleApp(t, srv.URL)
	app.Cfg.HardMaxBytes = 100
	app.CtxLimit.NCtx = 100
	if _, err := app.Send(context.Background(), "Implement a Python function"); err == nil || !errors.Is(err, ErrCurrentTurnTooLarge) {
		t.Fatalf("expected pre-send current-turn-too-large error, got %v", err)
	}
}

func TestStyleGuideFilenameIsTurnScoped(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.prepareStyleGuides("script.py")
	if app.styleSelection().Bytes() == 0 {
		t.Fatal("recognized filename did not activate its guide")
	}
	app.prepareStyleGuides("Continue")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("filename became a sticky language selection")
	}
}

func TestStyleGuideExplicitCapWarns(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	var out strings.Builder
	app.Out = &out
	app.prepareStyleGuides("Implement Python and Golang")
	app.prepareStyleGuides("Implement TypeScript")
	if len(app.styleSelection().IDs()) != 2 || !strings.Contains(out.String(), "not active: typescript\x1b[0m") {
		t.Fatalf("third explicit language was not rejected visibly: %v %q", app.styleSelection().IDs(), out.String())
	}
}

func TestStyleGuideOptOutSuppressesStickyAndPaths(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.prepareStyleGuides("Implement Python")
	app.prepareStyleGuides("Disable the Python guide")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("opt-out retained sticky guide")
	}
	app.prepareStyleGuides("script.py.")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("opt-out did not suppress path activation")
	}
	app.prepareStyleGuides("Enable the Python guide")
	if app.styleSelection().Bytes() == 0 {
		t.Fatal("explicit re-enable did not restore guide")
	}
	app.prepareStyleGuides("Disable style guides")
	app.prepareStyleGuides("main.go")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("global opt-out did not suppress paths")
	}
}

func TestStyleGuideDisableCannotBeBypassed(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.prepareStyleGuides("Disable the Python guide")
	app.prepareStyleGuides("Implement Python")
	app.observeStyleToolPath("read_file", `{"path":"script.py"}`, true)
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("per-language disable was bypassed by later activation")
	}
	app.prepareStyleGuides("Disable style guides")
	app.prepareStyleGuides("Enable the Python guide")
	app.prepareStyleGuides("Continue")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("global disable was bypassed by per-language enable")
	}
}

func TestStyleGuideEnableOmissionWarnings(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	var out strings.Builder
	app.Out = &out
	app.prepareStyleGuides("Implement Python and Golang")
	app.prepareStyleGuides("Enable the TypeScript guide")
	if !strings.Contains(out.String(), "not active: typescript\x1b[0m") {
		t.Fatalf("enable count limit was silent: %q; state=%#v omitted=%v", out.String(), app.styleState, app.styleOmitted)
	}
}

func TestStyleGuideEnableCombinedByteLimitWarning(t *testing.T) {
	dir := t.TempDir()
	var content [2]string
	for i := range content {
		content[i] = strings.Repeat("x", 3700)
	}
	paths := map[string]string{}
	for i, id := range []string{"python", "go"} {
		path := filepath.Join(dir, id+".md")
		if err := os.WriteFile(path, []byte(content[i]), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[id] = path
	}
	catalog, err := styleguide.NewCatalog(true, paths)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp("http://unused", newFakeExecutor(), nil)
	app.SetStyleGuideCatalog(catalog)
	var out strings.Builder
	app.Out = &out
	app.prepareStyleGuides("Enable the Python guide")
	app.prepareStyleGuides("Enable the Golang guide")
	if !strings.Contains(out.String(), "not active: go\x1b[0m") {
		t.Fatalf("enable combined-byte limit was silent: %q", out.String())
	}
	if got := app.styleSelection().IDs(); len(got) != 1 || got[0] != "python" {
		t.Fatalf("request guides disagree with notice: %v", got)
	}
}

func TestStyleGuideCompactionExcludesGuideAndKeepsNewest(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.prepareStyleGuides("Implement Python")
	app.Cfg.KeepBytes = app.styleSelection().Bytes()
	app.Cfg.CompactAt = 1
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr(strings.Repeat("old", 20))},
		{Role: "assistant", Content: StrPtr("old answer")},
		{Role: "user", Content: StrPtr("newest task")},
		{Role: "assistant", Content: StrPtr("newest answer")},
	}
	var summaryInput string
	ok, err := app.Compact(context.Background(), func(_ context.Context, input string) (string, error) {
		summaryInput = input
		return "summary", nil
	}, true)
	if err != nil || !ok {
		t.Fatalf("Compact = %v, %v", ok, err)
	}
	if strings.Contains(summaryInput, "Language style guidance") {
		t.Fatal("guide entered summarizer input")
	}
	if app.styleSelection().Bytes() == 0 {
		t.Fatal("compaction discarded selection")
	}
	foundNewest := false
	for _, m := range app.Conv {
		if DerefStr(m.Content) == "newest task" {
			foundNewest = true
		}
	}
	if !foundNewest {
		t.Fatal("exhausted keep budget summarized newest task")
	}
}

func TestStyleCommandPendingPromotionAndUserStatus(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.HandleStyleCommand([]string{"/styles", "enable", "go"})
	app.HandleStyleCommand([]string{"/styles", "enable", "python"})
	app.HandleStyleCommand([]string{"/styles", "enable", "typescript"})
	if got := app.HandleStyleCommand([]string{"/styles"}); !strings.Contains(got, "typescript (embedded): selected; omitted by limit") {
		t.Fatalf("omitted status = %q", got)
	}
	app.HandleStyleCommand([]string{"/styles", "disable", "go"})
	if got := app.styleSelection().IDs(); len(got) != 2 || got[0] != "python" || got[1] != "typescript" {
		t.Fatalf("pending guide not promoted: %v", got)
	}
	path := filepath.Join(t.TempDir(), "rust.md")
	if err := os.WriteFile(path, []byte("RUST-RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := styleguide.NewCatalog(true, map[string]string{"rs": path})
	if err != nil {
		t.Fatal(err)
	}
	app.SetStyleGuideCatalog(catalog)
	if got := app.HandleStyleCommand([]string{"/styles"}); !strings.Contains(got, "rust (user): available") {
		t.Fatalf("user source status = %q", got)
	}
}

func TestStyleSnapshotCopiesPathAndDisables(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	app.HandleStyleCommand([]string{"/styles", "enable", "python"})
	app.HandleStyleCommand([]string{"/styles", "disable", "python"})
	app.observeStyleToolPath("read_file", `{"path":"main.go"}`, true)
	snapshot := app.styleSnapshot()
	if len(snapshot.path) != 1 || snapshot.path[0] != "go" || !snapshot.disabled["python"] {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	snapshot.path[0] = "python"
	delete(snapshot.disabled, "python")
	if app.styleState.path[0] != "go" || !app.styleState.disabled["python"] {
		t.Fatal("snapshot shares mutable state")
	}
}

func TestStyleGuideSubagentSnapshot(t *testing.T) {
	var subStyle atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			subStyle.Store(strings.Contains(string(body), "Language style guidance"))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(`{"objective":"test","findings":[]}`)+"\n\ndata: [DONE]\n\n")
		}
	}))
	defer srv.Close()
	app := enabledStyleApp(t, srv.URL)
	app.prepareStyleGuides("Implement a Python function")
	app.dispatchSubagent(context.Background(), "inspect", io.Discard, "", "", "")
	if !subStyle.Load() {
		t.Fatal("child did not inherit active style guide")
	}
	if len(app.styleSelection().IDs()) != 1 {
		t.Fatal("child modified parent selection")
	}
}

func TestStyleCommandReportsActualStateAndControls(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	if got := app.HandleStyleCommand([]string{"/styles"}); !strings.Contains(got, "go (embedded): available") {
		t.Fatalf("initial status = %q", got)
	}
	if got := app.HandleStyleCommand([]string{"/styles", "enable", "python"}); !strings.Contains(got, "active: python (embedded)") {
		t.Fatalf("enable = %q", got)
	}
	if got := app.HandleStyleCommand([]string{"/styles"}); !strings.Contains(got, "python (embedded): active") {
		t.Fatalf("active status = %q", got)
	}
	app.HandleStyleCommand([]string{"/styles", "disable", "python"})
	app.prepareStyleGuides("script.py")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("disabled language reactivated from a path")
	}
	if got := app.HandleStyleCommand([]string{"/styles", "enable", "unknown"}); !strings.Contains(got, "not loaded") {
		t.Fatalf("unknown language = %q", got)
	}
	app.HandleStyleCommand([]string{"/styles", "disable", "all"})
	if got := app.HandleStyleCommand([]string{"/styles", "enable", "go"}); !strings.Contains(got, "enable all first") {
		t.Fatalf("global disable bypass = %q", got)
	}
	app.HandleStyleCommand([]string{"/styles", "enable", "all"})
	app.prepareStyleGuides("main.go")
	if app.styleSelection().Bytes() == 0 {
		t.Fatal("global re-enable did not resume automatic activation")
	}
	app.HandleStyleCommand([]string{"/styles", "disable", "go"})
	app.HandleStyleCommand([]string{"/styles", "disable", "all"})
	app.HandleStyleCommand([]string{"/styles", "enable", "all"})
	app.prepareStyleGuides("main.go")
	if app.styleSelection().Bytes() != 0 {
		t.Fatal("enable all erased the per-language opt-out")
	}
}

func TestStyleCommandRustAndStrictParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rust.md")
	if err := os.WriteFile(path, []byte("RUST-RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := styleguide.NewCatalog(true, map[string]string{"rs": path})
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp("http://unused", newFakeExecutor(), nil)
	app.SetStyleGuideCatalog(catalog)
	for _, line := range []string{"/styles enable rust", "/styles enable rs"} {
		handled, _, cmd := HandleTUICommand(line, app)
		if !handled || cmd == nil {
			t.Fatalf("%q was not dispatched", line)
		}
		if got := app.styleSelection().IDs(); len(got) != 1 || got[0] != "rust" {
			t.Fatalf("%q selected %v", line, got)
		}
	}
	app.prepareStyleGuides("Use the rust guide for this code")
	if got := app.styleSelection().IDs(); len(got) != 1 || got[0] != "rust" {
		t.Fatalf("natural Rust selection = %v", got)
	}
	for _, line := range []string{"/styles enable", "/styles enable rust extra", "/styles toggle rust"} {
		handled, _, cmd := HandleTUICommand(line, app)
		msg, ok := cmd().(SysNoteMsg)
		if !handled || !ok || !strings.Contains(msg.Text, "usage:") {
			t.Errorf("%q result = %#v", line, cmd())
		}
	}
}

func TestStyleCommandConcurrentWithRequests(t *testing.T) {
	app := enabledStyleApp(t, "http://unused")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 100 {
				app.requestMessages()
				app.sentSize(app.contextProjection())
				app.observeStyleToolPath("read_file", `{"path":"main.go"}`, true)
				if (i+j)%2 == 0 {
					app.HandleStyleCommand([]string{"/styles"})
				} else {
					app.HandleStyleCommand([]string{"/styles", "enable", "python"})
					app.HandleStyleCommand([]string{"/styles", "disable", "python"})
				}
			}
		}()
	}
	wg.Wait()
}

func TestStyleCatalogDoesNotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rust.md")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := styleguide.NewCatalog(true, map[string]string{"rs": path})
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp("http://unused", newFakeExecutor(), nil)
	app.SetStyleGuideCatalog(catalog)
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.HandleStyleCommand([]string{"/styles", "enable", "rust"})
	if !strings.Contains(app.styleSelection().Text(), "original") || strings.Contains(app.styleSelection().Text(), "changed") {
		t.Fatal("command reloaded configured guide")
	}
}

func requestHasStyle(messages []proxy.Message) bool {
	for _, m := range messages {
		if strings.Contains(DerefStr(m.Content), "Language style guidance") {
			return true
		}
	}
	return false
}
