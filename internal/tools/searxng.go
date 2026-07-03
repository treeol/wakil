package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// SearxngTools returns the searxng_search and searxng_url_read tool definitions.
// Only called when a SearXNG URL is configured.
func SearxngTools() []proxy.Tool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	enumProp := func(desc string, values ...string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "string",
			"description": desc,
			"enum":        values,
		}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		b, _ := json.Marshal(map[string]interface{}{"type": "object", "properties": props, "required": required})
		return b
	}

	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "searxng_search",
			Description: "Search the web using SearXNG. " +
				"Use 'categories' to target working engines: " +
				"'it' (github, stackoverflow, docker, pypi…), " +
				"'news' (bing news, qwant news, wikinews…), " +
				"'science' (arxiv, pubmed, google scholar…), " +
				"'general' (wikipedia, brave…). " +
				"Default category is 'it'.",
			Parameters: obj(map[string]interface{}{
				"query": strProp("Search query"),
				"categories": enumProp(
					"Search category — controls which engines are used",
					"it", "news", "science", "general", "images", "videos",
					"music", "files", "social media", "map",
				),
				"time_range": enumProp("Limit results by age", "day", "month", "year"),
				"engines":    strProp("Comma-separated engine names to force (e.g. 'github,stackoverflow'). Overrides categories."),
			}, "query"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "searxng_url_read",
			Description: "Fetch and return the plain-text content of a URL.",
			Parameters: obj(map[string]interface{}{
				"url": strProp("URL to fetch"),
			}, "url"),
		}},
	}
}

// CallSearxng executes a searxng_search call and returns the formatted result
// string plus the list of result URLs (for client-side grounding provenance).
func CallSearxng(baseURL, query, categories, timeRange, engines string) (string, []string) {
	params := url.Values{
		"q":      {query},
		"format": {"json"},
	}
	if categories != "" {
		params.Set("categories", categories)
	} else {
		params.Set("categories", "it") // default to category with working engines
	}
	if timeRange != "" {
		params.Set("time_range", timeRange)
	}
	if engines != "" {
		params.Set("engines", engines)
	}

	target := strings.TrimRight(baseURL, "/") + "/search?" + params.Encode()
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(target)
	if err != nil {
		return "ERROR: " + err.Error(), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "ERROR reading response: " + err.Error(), nil
	}

	var result struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Engine  string `json:"engine"`
		} `json:"results"`
		UnresponsiveEngines [][]string `json:"unresponsive_engines"`
		NumberOfResults     int        `json:"number_of_results"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "ERROR parsing response: " + err.Error(), nil
	}

	if len(result.Results) == 0 {
		var issues []string
		for _, e := range result.UnresponsiveEngines {
			if len(e) >= 2 {
				issues = append(issues, e[0]+": "+e[1])
			}
		}
		msg := "No results found"
		if len(issues) > 0 {
			msg += " (engine issues: " + strings.Join(issues, "; ") + ")"
		}
		return msg, nil
	}

	var urls []string
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d results:\n\n", len(result.Results))
	for i, r := range result.Results {
		if i >= 10 {
			break
		}
		if r.URL != "" {
			urls = append(urls, r.URL)
		}
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Content != "" {
			content := r.Content
			if len(content) > 500 {
				content = content[:500] + "…"
			}
			fmt.Fprintf(&sb, "   %s\n", content)
		}
		fmt.Fprintln(&sb)
	}
	return strings.TrimRight(sb.String(), "\n"), urls
}

// FetchURL returns the plain-text content of a URL (for searxng_url_read).
func FetchURL(rawURL string) string {
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(rawURL)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return "ERROR reading: " + err.Error()
	}
	return stripHTML(string(body))

}

func stripHTML(s string) string {
	// Remove block elements whose content is never readable text.
	for _, tag := range []string{"script", "style", "noscript", "head"} {
		s = removeHTMLBlock(s, tag)
	}
	// Strip remaining tags, replace with space so words don't run together.
	var out strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			out.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			out.WriteRune(r)
		}
	}
	// Decode a handful of common HTML entities.
	text := out.String()
	text = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&nbsp;", " ",
	).Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

// removeHTMLBlock removes everything between <tag …> and </tag> (case-insensitive).
func removeHTMLBlock(s, tag string) string {
	open := "<" + tag
	close := "</" + tag + ">"
	for {
		lo := strings.Index(strings.ToLower(s), open)
		if lo < 0 {
			break
		}
		// Find the end of the opening tag first (skip attributes).
		gt := strings.Index(s[lo:], ">")
		if gt < 0 {
			break
		}
		start := lo
		// Now find the closing tag.
		hi := strings.Index(strings.ToLower(s[lo+gt+1:]), close)
		if hi < 0 {
			// No closing tag — strip to end.
			s = s[:start]
			break
		}
		end := lo + gt + 1 + hi + len(close)
		s = s[:start] + " " + s[end:]
	}
	return s
}
