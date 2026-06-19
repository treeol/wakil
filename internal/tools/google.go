package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"wakil/internal/proxy"
)

// GoogleTools returns the google_search and google_fetch_url tool definitions.
// Only called when a Google API key and CX are configured.
func GoogleTools() []proxy.Tool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		b, _ := json.Marshal(map[string]interface{}{"type": "object", "properties": props, "required": required})
		return b
	}

	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "google_search",
			Description: "Search Google using the Custom Search JSON API. Returns ranked results as metadata only (title, url, snippet); it does not fetch the pages. Use google_fetch_url to read a result's full content.",
			Parameters: obj(map[string]interface{}{
				"query":  strProp("The search query"),
				"num":    intProp("Number of results to return (1-10, default 5)"),
				"start":  intProp("Pagination offset (1-based, default 1)"),
				"after":  strProp("Restrict results to pages published on or after this date. Accepts YYYY, YYYY-MM, or YYYY-MM-DD."),
				"before": strProp("Restrict results to pages published on or before this date. Same format as 'after'."),
			}, "query"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "google_fetch_url",
			Description: "Fetch a URL and return its readable text content with HTML stripped. Use this to read the full content of a page found via google_search.",
			Parameters: obj(map[string]interface{}{
				"url":       strProp("The page URL to fetch (must be http:// or https://)"),
				"max_chars": intProp("Maximum characters of text to return (default 5000)"),
			}, "url"),
		}},
	}
}

// CallGoogle executes a google_search call and returns the formatted result
// string plus the list of result URLs (for client-side grounding provenance).
func CallGoogle(apiKey, cx, query string, num, start int, after, before string) (string, []string) {
	if apiKey == "" || cx == "" {
		return "ERROR: GOOGLE_API_KEY and GOOGLE_CX must be configured", nil
	}

	if num < 1 {
		num = 5
	}
	if num > 10 {
		num = 10
	}
	if start < 1 {
		start = 1
	}

	params := url.Values{
		"key":   {apiKey},
		"cx":    {cx},
		"q":     {query},
		"num":   {fmt.Sprintf("%d", num)},
		"start": {fmt.Sprintf("%d", start)},
	}

	if after != "" || before != "" {
		afterStr := "19900101"
		if after != "" {
			s, err := parseGoogleDate(after, false)
			if err != nil {
				return "ERROR: " + err.Error(), nil
			}
			afterStr = s
		}
		beforeStr := time.Now().Format("20060102")
		if before != "" {
			s, err := parseGoogleDate(before, true)
			if err != nil {
				return "ERROR: " + err.Error(), nil
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
		return "ERROR: " + err.Error(), nil
	}
	req.Header.Set("User-Agent", "GoogleSearchMCP/1.0")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "ERROR: " + err.Error(), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Sprintf("ERROR: Google API error (%d): %s", resp.StatusCode, string(body)), nil
	}

	var apiResp struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "ERROR parsing response: " + err.Error(), nil
	}

	if len(apiResp.Items) == 0 {
		return "No results found", nil
	}

	var urls []string
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d results:\n\n", len(apiResp.Items))
	for i, item := range apiResp.Items {
		if item.Link != "" {
			urls = append(urls, item.Link)
		}
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, item.Title, item.Link)
		if item.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", item.Snippet)
		}
		fmt.Fprintln(&sb)
	}
	return strings.TrimRight(sb.String(), "\n"), urls
}

// GoogleFetchURL fetches a URL and returns its readable text content.
func GoogleFetchURL(rawURL string, maxChars int) string {
	if !strings.HasPrefix(strings.ToLower(rawURL), "http://") &&
		!strings.HasPrefix(strings.ToLower(rawURL), "https://") {
		return "ERROR: url must start with http:// or https://"
	}
	if maxChars < 100 {
		maxChars = 5000
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	req.Header.Set("User-Agent", "GoogleSearchMCP/1.0")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "ERROR reading: " + err.Error()
	}

	text := googleStripHTML(string(body))
	if len(text) > maxChars {
		text = strings.TrimRight(text[:maxChars], " \t\n") + "…"
	}
	return text
}

// googleStripHTML removes HTML tags and returns readable text. Uses a simple
// regex-based approach consistent with the searxng stripHTML helper.
func googleStripHTML(s string) string {
	// Remove block elements whose content is never readable text.
	for _, tag := range []string{"script", "style", "noscript", "head"} {
		s = removeHTMLBlock(s, tag)
	}
	// Strip remaining tags.
	tagRe := regexp.MustCompile(`<[^>]+>`)
	s = tagRe.ReplaceAllString(s, " ")
	// Decode common HTML entities.
	s = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&nbsp;", " ",
	).Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// parseGoogleDate converts a flexible date string to YYYYMMDD for the Google
// sort parameter. Accepts YYYY, YYYY-MM, or YYYY-MM-DD (slashes also accepted).
// When end=true, incomplete dates are filled to the last day of the period.
func parseGoogleDate(s string, end bool) (string, error) {
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
