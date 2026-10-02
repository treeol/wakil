package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The workspace store (ilmql) scopes an answer to the directory a request came
// from by reading the top-level "cwd" request-body field — it does not parse
// prose for a working directory. These tests pin the wire contract: the field
// is present, top-level, and carries the session's absolute working directory.

const sseDone = "data: [DONE]\n\n"

// capturedBody is the raw request body the fake server received, decoded.
func captureCwd(t *testing.T, c *Client) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("unmarshal body %q: %v", raw, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseDone)
	}))
	defer srv.Close()

	c.BaseURL = srv.URL
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return got
}

func TestCwdSentAsTopLevelField(t *testing.T) {
	got := captureCwd(t, &Client{Model: "ilm", ChatID: "t", Cwd: "/mnt/ilmql"})

	cwd, ok := got["cwd"]
	if !ok {
		t.Fatalf("cwd absent from request body; keys=%v", keysOf(got))
	}
	if cwd != "/mnt/ilmql" {
		t.Errorf("cwd = %v, want /mnt/ilmql", cwd)
	}
	// Top-level, beside model and messages — not nested under a wrapper, and
	// not smuggled into the message list as prose.
	if _, nested := got["metadata"].(map[string]any)["cwd"]; nested {
		t.Error("cwd must be a top-level field, not nested under metadata")
	}
}

// cwd is unconditional: it is part of the OpenAI-compatible contract every
// server accepts, so it is not gated by the ilm-proxy-only request shape the
// way Metadata is.
func TestCwdSentForPlainOpenAIKind(t *testing.T) {
	got := captureCwd(t, &Client{
		Model:           "ilm",
		ConfiguredModel: "ilm",
		Kind:            KindOpenAI,
		Cwd:             "/mnt/ilmql",
	})
	if got["cwd"] != "/mnt/ilmql" {
		t.Errorf("cwd = %v, want /mnt/ilmql (KindOpenAI must still carry it)", got["cwd"])
	}
}

func TestCwdPresentOnSubagentPath(t *testing.T) {
	// Subagent clients set NoMemoryWrite but share the parent's workspace; the
	// field must survive that difference.
	got := captureCwd(t, &Client{Model: "ilm", ChatID: "t", Cwd: "/mnt/ilmql", NoMemoryWrite: true})
	if got["cwd"] != "/mnt/ilmql" {
		t.Errorf("cwd = %v, want /mnt/ilmql (NoMemoryWrite must not drop it)", got["cwd"])
	}
}

func TestResolveCwdUsesWorkdirNotProcessCwd(t *testing.T) {
	// The whole point: a pinned workspace wins over the launch directory.
	// Simulate a launch from elsewhere with a workspace argument, the way
	// config.LoadConfig sets WorkDir.
	launch, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Chdir(t.TempDir()) // "launch directory" differs from the workspace

	pinned := t.TempDir()
	got, err := ResolveCwd(pinned)
	if err != nil {
		t.Fatalf("ResolveCwd: %v", err)
	}
	if got != pinned {
		t.Errorf("ResolveCwd(%q) = %q; want the pinned workdir, not the launch dir %q", pinned, got, launch)
	}
}

func TestResolveCwdDockerPathIsUsedVerbatim(t *testing.T) {
	// In docker mode cfg.WorkDir is the in-container mount point. The store
	// resolves by basename, and "/mnt/work" is the fallback — it must survive
	// Abs unchanged rather than being re-rooted onto the launch directory.
	got, err := ResolveCwd("/mnt/ilmql")
	if err != nil {
		t.Fatalf("ResolveCwd: %v", err)
	}
	if got != "/mnt/ilmql" {
		t.Errorf("ResolveCwd = %q, want /mnt/ilmql verbatim", got)
	}
}

func TestResolveCwdFallsBackToProcessCwd(t *testing.T) {
	// Direct mode with no workspace argument leaves WorkDir empty; NewDirectExecutor
	// falls back to the cwd, so Cwd must match or the field would mis-scope.
	got, err := ResolveCwd("")
	if err != nil {
		t.Fatalf("ResolveCwd: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got != wd {
		t.Errorf("ResolveCwd(\"\") = %q, Getwd = %q; want equal", got, wd)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveCwd = %q, want an absolute path", got)
	}
}

func TestResolveCwdIsAbsoluteAndHasUsableBasename(t *testing.T) {
	// The store maps the path to a workspace by basename, so the basename must
	// be the real directory name, not "." or a trailing separator.
	got, err := ResolveCwd(t.TempDir())
	if err != nil {
		t.Fatalf("ResolveCwd: %v", err)
	}
	if base := filepath.Base(got); base == "." || base == string(filepath.Separator) {
		t.Errorf("basename of %q is %q; store would not resolve a workspace", got, base)
	}
}

func TestResolveCwdAbsolutizesRelativeWorkdir(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := ResolveCwd("sub/dir")
	if err != nil {
		t.Fatalf("ResolveCwd: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveCwd(%q) = %q, want absolute", "sub/dir", got)
	}
}

func TestCwdIsSampledOnceNotTrackedLive(t *testing.T) {
	// Resolved once per client, not per request: a chdir after construction
	// must not repoint the client's stored value. Client.Cwd is a plain field
	// copied at construction, so this holds by construction — the test pins
	// that it stays that way if the field is ever made lazy.
	c := &Client{Model: "ilm", ChatID: "t", Cwd: "/mnt/ilmql"}
	before := c.Cwd
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if c.Cwd != before {
		t.Errorf("Client.Cwd changed to %q after chdir; want the sampled %q", c.Cwd, before)
	}
	if got := captureCwd(t, c)["cwd"]; got != "/mnt/ilmql" {
		t.Errorf("wire cwd = %v after chdir, want the sampled /mnt/ilmql", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
