package tools

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// failingRoundTripper forces every request to fail with a controlled error.
type failingRoundTripper struct{ err error }

func (f *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func TestGoogleTools(t *testing.T) {
	tools := GoogleTools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	if !names["google_search"] {
		t.Error("missing google_search tool")
	}
	if !names["google_fetch_url"] {
		t.Error("missing google_fetch_url tool")
	}
}

func TestCallGoogle_NoAPIKeyLeakOnNetworkError(t *testing.T) {
	const apiKey = "SECRET_KEY_DO_NOT_LEAK_12345"
	const cx = "test_cx"

	// Inject a RoundTripper that returns a *url.Error containing the full
	// URL (which includes the API key). This is exactly what
	// http.Client.Do does on network failure.
	origClient := googleSearchClient
	origBase := googleBaseURL
	defer func() {
		googleSearchClient = origClient
		googleBaseURL = origBase
	}()

	googleSearchClient = &http.Client{
		Transport: &failingRoundTripper{
			err: &url.Error{
				Op:  "Get",
				URL: googleBaseURL + "?key=" + apiKey + "&cx=" + cx + "&q=test",
				Err: errors.New("dial tcp: no such host"),
			},
		},
	}

	result, _ := CallGoogle(apiKey, cx, "test query", 1, 1, "", "")

	// The result must NOT contain the API key (raw or URL-encoded).
	if strings.Contains(result, apiKey) {
		t.Errorf("API key leaked into error output: %s", result)
	}
	if strings.Contains(result, url.QueryEscape(apiKey)) {
		t.Errorf("URL-encoded API key leaked: %s", result)
	}
	// The result must indicate it was a sanitized network error.
	if !strings.HasPrefix(result, "ERROR: Google API request failed:") {
		t.Errorf("expected sanitized error prefix, got: %s", result)
	}
	// The inner diagnostic must survive (useful for debugging).
	if !strings.Contains(result, "no such host") {
		t.Errorf("expected inner error diagnostic, got: %s", result)
	}
}

func TestCallGoogle_NoAPIKeyLeakOnAPIError(t *testing.T) {
	const apiKey = "SECRET_KEY_DO_NOT_LEAK_12345"
	const cx = "test_cx"

	origClient := googleSearchClient
	origBase := googleBaseURL
	defer func() {
		googleSearchClient = origClient
		googleBaseURL = origBase
	}()

	// Simulate a non-200 response whose body echoes the request URL
	// (which contains the API key). The sanitizer must redact the key.
	googleBaseURL = "http://test.invalid"
	googleSearchClient = &http.Client{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "request was: %s?key=%s", r.URL.Path, apiKey)
	}))
	defer srv.Close()
	googleBaseURL = srv.URL

	result, _ := CallGoogle(apiKey, cx, "test query", 1, 1, "", "")

	if strings.Contains(result, apiKey) {
		t.Errorf("API key leaked into non-200 error body: %s", result)
	}
	if !strings.HasPrefix(result, "ERROR: Google API error (400):") {
		t.Errorf("expected API error prefix, got: %s", result)
	}
}

func TestCallGoogle_EmptyCredentials(t *testing.T) {
	result, urls := CallGoogle("", "", "test", 1, 1, "", "")
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("expected error for empty credentials, got: %s", result)
	}
	if len(urls) != 0 {
		t.Errorf("expected no URLs for empty credentials, got %d", len(urls))
	}
}

func TestCallGoogle_Success(t *testing.T) {
	const apiKey = "test_key"
	const cx = "test_cx"

	origClient := googleSearchClient
	origBase := googleBaseURL
	defer func() {
		googleSearchClient = origClient
		googleBaseURL = origBase
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[{"title":"Go Docs","link":"https://go.dev/doc","snippet":"Go documentation"}]}`)
	}))
	defer srv.Close()
	googleBaseURL = srv.URL
	googleSearchClient = &http.Client{}

	result, urls := CallGoogle(apiKey, cx, "golang", 5, 1, "", "")

	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected success, got error: %s", result)
	}
	if !strings.Contains(result, "Go Docs") {
		t.Errorf("expected title in result, got: %s", result)
	}
	if !strings.Contains(result, "https://go.dev/doc") {
		t.Errorf("expected link in result, got: %s", result)
	}
	if len(urls) != 1 || urls[0] != "https://go.dev/doc" {
		t.Errorf("expected 1 URL, got %v", urls)
	}
}

func TestCallGoogle_NoResults(t *testing.T) {
	const apiKey = "test_key"
	const cx = "test_cx"

	origClient := googleSearchClient
	origBase := googleBaseURL
	defer func() {
		googleSearchClient = origClient
		googleBaseURL = origBase
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{}`)
	}))
	defer srv.Close()
	googleBaseURL = srv.URL
	googleSearchClient = &http.Client{}

	result, _ := CallGoogle(apiKey, cx, "nothing matches", 5, 1, "", "")
	if result != "No results found" {
		t.Errorf("expected 'No results found', got: %s", result)
	}
}

func TestParseGoogleDate(t *testing.T) {
	tests := []struct {
		input   string
		end     bool
		want    string
		wantErr bool
	}{
		// Year only
		{"2024", false, "20240101", false},
		{"2024", true, "20241231", false},
		// Year-Month
		{"2024-06", false, "20240601", false},
		{"2024-06", true, "20240630", false},
		{"2024-02", true, "20240229", false}, // 2024 is a leap year
		{"2024-12", true, "20241231", false},
		// Year-Month-Day
		{"2024-06-15", false, "20240615", false},
		{"2024-06-15", true, "20240615", false},
		// Slash variants
		{"2024/06/15", false, "20240615", false},
		{"2024/06", true, "20240630", false},
		// Invalid
		{"abc", false, "", true},
		{"2024-13", false, "", true},    // invalid month
		{"2024-00", false, "", true},    // invalid month
		{"2024-06-32", false, "", true}, // invalid day
		{"2024-06-00", false, "", true}, // invalid day
		{"2024-06-15-extra", false, "", true}, // too many parts
	}

	for _, tt := range tests {
		got, err := parseGoogleDate(tt.input, tt.end)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseGoogleDate(%q, %v) = %q, want error", tt.input, tt.end, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGoogleDate(%q, %v) error: %v", tt.input, tt.end, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseGoogleDate(%q, %v) = %q, want %q", tt.input, tt.end, got, tt.want)
		}
	}
}

func TestAtoi(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"0", 0, false},
		{"123", 123, false},
		{"99999", 99999, false},
		// atoi("") returns 0 with no error — the loop body doesn't
		// execute for an empty string. This is existing behavior.
		{"", 0, false},
		{"abc", 0, true},
		{"12a", 0, true},
		{"-5", 0, true},
	}
	for _, tt := range tests {
		got, err := atoi(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("atoi(%q) = %d, want error", tt.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("atoi(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("atoi(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestGoogleFetchURL_RejectsNonHTTP(t *testing.T) {
	tests := []string{
		"ftp://example.com",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"example.com",
		"",
	}
	for _, u := range tests {
		result := GoogleFetchURL(u, 5000)
		if !strings.HasPrefix(result, "ERROR:") {
			t.Errorf("GoogleFetchURL(%q) = %q, want error", u, result)
		}
	}
}
