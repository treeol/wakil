// Command google_search_mcp is a Model Context Protocol server that exposes
// Google Search (via Google's Custom Search JSON API) plus a URL reader that
// fetches a page and returns its readable text.
//
// Environment variables:
//
//	GOOGLE_API_KEY - API key from Google Cloud Console
//	GOOGLE_CX      - Search Engine ID from Programmable Search Engine
//
// Usage:
//
//	go run ./cmd/google_search_mcp
//
// Communicates over stdio (the default MCP transport) and works with any MCP
// client: Claude, Cursor, VS Code, wakil, etc.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/html"
)

const (
	userAgent       = "GoogleSearchMCP/1.0"
	maxDownloadBytes = 2_000_000 // cap each fetch at ~2 MB
)

func main() {
	ctx := context.Background()

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "Google Search", Version: "1.0"},
		nil,
	)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "google_search",
		Description: "Search Google using the Custom Search JSON API. Returns ranked results as metadata only (title, url, snippet); it does not fetch the pages. Use fetch_url to read a result's full content.",
	}, googleSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fetch_url",
		Description: "Fetch a URL and return its readable text content with HTML stripped. Use this to read the full content of a page found via google_search, rather than relying on the short search snippet.",
	}, fetchURL)

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Tool: google_search
// ---------------------------------------------------------------------------

type googleSearchInput struct {
	Query  string `json:"query" jsonschema:"description=The search query"`
	Num    int    `json:"num,omitempty" jsonschema:"description=Number of results to return (1-10, default 5),default=5"`
	Start  int    `json:"start,omitempty" jsonschema:"description=Pagination offset (1-based, default 1),default=1"`
	After  string `json:"after,omitempty" jsonschema:"description=Restrict results to pages published on or after this date. Accepts YYYY, YYYY-MM, or YYYY-MM-DD."`
	Before string `json:"before,omitempty" jsonschema:"description=Restrict results to pages published on or before this date. Same format as after."`
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func googleSearch(_ context.Context, _ *mcp.CallToolRequest, in googleSearchInput) (*mcp.CallToolResult, any, error) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	cx := os.Getenv("GOOGLE_CX")
	if apiKey == "" {
		return nil, nil, fmt.Errorf("GOOGLE_API_KEY environment variable is not set")
	}
	if cx == "" {
		return nil, nil, fmt.Errorf("GOOGLE_CX (Programmable Search Engine ID) is not set")
	}

	// Clamp values
	num := in.Num
	if num < 1 {
		num = 5
	}
	if num > 10 {
		num = 10
	}
	start := in.Start
	if start < 1 {
		start = 1
	}

	params := url.Values{
		"key":   {apiKey},
		"cx":    {cx},
		"q":     {in.Query},
		"num":   {fmt.Sprintf("%d", num)},
		"start": {fmt.Sprintf("%d", start)},
	}

	if in.After != "" || in.Before != "" {
		afterStr := "19900101"
		if in.After != "" {
			s, err := parseDate(in.After, false)
			if err != nil {
				return nil, nil, err
			}
			afterStr = s
		}
		beforeStr := time.Now().Format("20060102")
		if in.Before != "" {
			s, err := parseDate(in.Before, true)
			if err != nil {
				return nil, nil, err
			}
			beforeStr = s
		}
		params.Set("sort", fmt.Sprintf("date:r:%s:%s", afterStr, beforeStr))
	} else {
		params.Set("dateRestrict", "m3")
	}

	reqURL := "https://www.googleapis.com/customsearch/v1?" + params.Encode()

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, fmt.Errorf("Google API error (%d): %s", resp.StatusCode, string(body))
	}

	var apiResp struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, nil, fmt.Errorf("failed to decode response: %w", err)
	}

	results := make([]searchResult, 0, len(apiResp.Items))
	for _, item := range apiResp.Items {
		results = append(results, searchResult{
			Title:   item.Title,
			URL:     item.Link,
			Snippet: item.Snippet,
		})
	}

	return nil, results, nil
}

// ---------------------------------------------------------------------------
// Tool: fetch_url
// ---------------------------------------------------------------------------

type fetchURLInput struct {
	URL      string `json:"url" jsonschema:"description=The page URL to fetch (must be http:// or https://)"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"description=Maximum characters of text to return (default 5000),default=5000"`
}

type fetchURLOutput struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func fetchURL(_ context.Context, _ *mcp.CallToolRequest, in fetchURLInput) (*mcp.CallToolResult, any, error) {
	if !strings.HasPrefix(strings.ToLower(in.URL), "http://") &&
		!strings.HasPrefix(strings.ToLower(in.URL), "https://") {
		return nil, nil, fmt.Errorf("url must start with http:// or https://")
	}

	maxChars := in.MaxChars
	if maxChars < 100 {
		maxChars = 5000
	}

	req, err := http.NewRequest(http.MethodGet, in.URL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("network error fetching %s: %w", in.URL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	charset := charsetFrom(contentType)

	textBody := decodeBytes(raw, charset)

	var title, text string
	looksHTML := strings.Contains(strings.ToLower(contentType), "html") ||
		strings.Contains(strings.ToLower(textBody[:min(2000, len(textBody))]), "<html")
	if looksHTML {
		title, text = htmlToText(textBody)
	} else {
		title, text = "", collapseWS(textBody)
	}

	truncated := len(text) > maxChars
	if truncated {
		text = strings.TrimRight(text[:maxChars], " \t\n") + "…"
	}

	out := fetchURLOutput{
		URL:       in.URL,
		Title:     title,
		Text:      text,
		Truncated: truncated,
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseDate converts a flexible date string to YYYYMMDD for the Google sort
// parameter. Accepts YYYY, YYYY-MM, or YYYY-MM-DD (slashes also accepted).
// When end=true, incomplete dates are filled to the last day of the period.
func parseDate(s string, end bool) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "/", "-"))
	parts := strings.Split(s, "-")
	switch len(parts) {
	case 1:
		year, err := atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		if end {
			return fmt.Sprintf("%d1231", year), nil
		}
		return fmt.Sprintf("%d0101", year), nil
	case 2:
		year, err := atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		month, err := atoi(parts[1])
		if err != nil || month < 1 || month > 12 {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		if end {
			last := 31
			if month < 12 {
				last = time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
			}
			return fmt.Sprintf("%d%02d%02d", year, month, last), nil
		}
		return fmt.Sprintf("%d%02d01", year, month), nil
	case 3:
		year, err := atoi(parts[0])
		if err != nil {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		month, err := atoi(parts[1])
		if err != nil || month < 1 || month > 12 {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		day, err := atoi(parts[2])
		if err != nil || day < 1 || day > 31 {
			return "", fmt.Errorf("unrecognized date format: %q", s)
		}
		return fmt.Sprintf("%d%02d%02d", year, month, day), nil
	default:
		return "", fmt.Errorf("unrecognized date format: %q", s)
	}
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// charsetFrom extracts the charset from a Content-Type header, defaulting to UTF-8.
func charsetFrom(contentType string) string {
	if idx := strings.Index(strings.ToLower(contentType), "charset="); idx >= 0 {
		cs := contentType[idx+8:]
		if semi := strings.Index(cs, ";"); semi >= 0 {
			cs = cs[:semi]
		}
		cs = strings.TrimSpace(strings.Trim(cs, `"`))
		if cs != "" {
			return cs
		}
	}
	return "utf-8"
}

// decodeBytes decodes raw bytes using the given charset label, falling back to
// UTF-8 with replacement on error.
func decodeBytes(raw []byte, charset string) string {
	// For UTF-8 (the overwhelmingly common case), just string() it.
	charset = strings.ToLower(charset)
	if charset == "utf-8" || charset == "utf8" || charset == "us-ascii" || charset == "ascii" {
		return string(raw)
	}
	// Fallback: best-effort, replace invalid bytes.
	return strings.ToValidUTF8(string(raw), "�")
}

var (
	wsCollapse1 = regexp.MustCompile(`[ \t\f\v\r]+`)
	wsCollapse2 = regexp.MustCompile(` *\n *`)
	wsCollapse3 = regexp.MustCompile(`\n{3,}`)
)

// collapseWS collapses runs of whitespace while keeping paragraph breaks.
func collapseWS(s string) string {
	s = wsCollapse1.ReplaceAllString(s, " ")
	s = wsCollapse2.ReplaceAllString(s, "\n")
	s = wsCollapse3.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// htmlToText extracts visible text (and the <title>) from an HTML document.
func htmlToText(htmlStr string) (title, text string) {
	doc, err := html.Parse(strings.NewReader(htmlStr))
	if err != nil {
		// Malformed markup: fall back to a crude tag strip.
		crude := regexp.MustCompile(`<[^>]+>`)
		return "", collapseWS(crude.ReplaceAllString(htmlStr, " "))
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template", "svg":
				return // skip entirely
			case "title":
				// title is handled separately below
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	walk(doc)

	// Extract title separately.
	var extractTitle func(*html.Node) string
	extractTitle = func(n *html.Node) string {
		if n.Type == html.ElementNode && n.Data == "title" {
			var t strings.Builder
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					t.WriteString(c.Data)
				}
			}
			return collapseWS(t.String())
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if s := extractTitle(c); s != "" {
				return s
			}
		}
		return ""
	}
	title = extractTitle(doc)

	// Insert line breaks at block-level boundaries by re-walking with markers.
	var b2 strings.Builder
	var walkBlock func(*html.Node)
	walkBlock = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template", "svg", "title":
				return
			case "p", "div", "br", "li", "tr", "section", "article", "header",
				"footer", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol",
				"table", "blockquote":
				b2.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			b2.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkBlock(c)
		}
	}
	walkBlock(doc)

	text = collapseWS(b2.String())
	return title, text
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
