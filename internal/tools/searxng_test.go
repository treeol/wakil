package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearxngToolsShape(t *testing.T) {
	tools := SearxngTools()
	if len(tools) != 2 {
		t.Fatalf("want 2 searxng tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
		if len(tl.Function.Parameters) == 0 {
			t.Errorf("%s has empty parameters schema", tl.Function.Name)
		}
	}
	if !names["searxng_search"] || !names["searxng_url_read"] {
		t.Errorf("missing expected tool names: %v", names)
	}
}

func TestCallSearxngFormatsResults(t *testing.T) {
	var gotQuery, gotCategories string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotCategories = r.URL.Query().Get("categories")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"First","url":"https://a.example/1","content":"alpha","engine":"github"},
			{"title":"Second","url":"https://b.example/2","content":"beta","engine":"pypi"}
		]}`))
	}))
	defer srv.Close()

	out, urls := CallSearxng(srv.URL, "golang channels", "", "", "")
	if gotQuery != "golang channels" {
		t.Errorf("query forwarded = %q", gotQuery)
	}
	if gotCategories != "it" {
		t.Errorf("default category should be 'it', got %q", gotCategories)
	}
	if !strings.Contains(out, "2 results") || !strings.Contains(out, "First") || !strings.Contains(out, "https://b.example/2") {
		t.Errorf("formatted output missing expected content:\n%s", out)
	}
	if len(urls) != 2 || urls[0] != "https://a.example/1" {
		t.Errorf("result URLs = %v", urls)
	}
}

func TestCallSearxngNoResultsReportsEngineIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[],"unresponsive_engines":[["github","timeout"]]}`))
	}))
	defer srv.Close()

	out, urls := CallSearxng(srv.URL, "q", "news", "day", "github,pypi")
	if urls != nil {
		t.Errorf("no results should yield nil URLs, got %v", urls)
	}
	if !strings.Contains(out, "No results") || !strings.Contains(out, "github: timeout") {
		t.Errorf("should surface engine issues; got %q", out)
	}
}

func TestCallSearxngErrorPaths(t *testing.T) {
	// Unreachable host → ERROR, no panic.
	out, urls := CallSearxng("http://127.0.0.1:1", "q", "", "", "")
	if !strings.HasPrefix(out, "ERROR") || urls != nil {
		t.Errorf("unreachable host should return ERROR/nil; got %q / %v", out, urls)
	}

	// Malformed JSON → parse error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	out, _ = CallSearxng(srv.URL, "q", "", "", "")
	if !strings.Contains(out, "ERROR parsing") {
		t.Errorf("malformed JSON should report parse error; got %q", out)
	}
}

func TestFetchURLStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>T</title><style>x{}</style></head>` +
			`<body><script>ignored()</script><p>Hello &amp; welcome</p></body></html>`))
	}))
	defer srv.Close()

	got := FetchURL(srv.URL)
	if strings.Contains(got, "ignored") || strings.Contains(got, "x{}") {
		t.Errorf("script/style content must be stripped; got %q", got)
	}
	if !strings.Contains(got, "Hello & welcome") {
		t.Errorf("readable text + entity decode expected; got %q", got)
	}
}

func TestStripHTML(t *testing.T) {
	in := `<div class="a">one</div><b>two</b>&nbsp;three<script>nope</script>`
	got := stripHTML(in)
	if got != "one two three" {
		t.Errorf("stripHTML = %q, want %q", got, "one two three")
	}
}

func TestRemoveHTMLBlockUnclosed(t *testing.T) {
	// An unclosed block is stripped to end-of-string.
	got := removeHTMLBlock("keep <script>tail with no close", "script")
	if strings.Contains(got, "tail") {
		t.Errorf("unclosed block should be stripped to end; got %q", got)
	}
	if !strings.Contains(got, "keep") {
		t.Errorf("content before the block must survive; got %q", got)
	}
}
