package agent

import (
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// classifyRequest determines whether a user message needs the full system
// prompt (extension guidance) or can be served with the core-only prompt.
// Returns true when the message looks like a task that needs the extension
// (full prompt), false for chit-chat that the core prompt can handle alone.
//
// The classifier is conservative: when in doubt, it returns true (full),
// because loading the extension unnecessarily is cheaper than missing needed
// guidance. All safety gates are in the core prompt regardless, so a false
// negative never removes a safeguard.
//
// This is a static, deterministic Go classifier — no model calls, no latency.
// It uses simple heuristics: message length, task/action verbs, code/path
// references, and keyword detection. It is intentionally over-broad: better
// to activate too often than too rarely.
func classifyRequest(userText string) bool {
	text := strings.TrimSpace(userText)
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)

	// Greetings and social niceties — never need the extension.
	if isGreeting(lower) {
		return false
	}

	// Code/file references: paths, file extensions, backtick code, URLs,
	// shell prompts. Check these BEFORE the short-message rejection so
	// short references like "foo/bar.go" (10 chars) still activate.
	if hasCodeReference(text) {
		return true
	}

	// Any task keyword triggers activation.
	if hasTaskKeyword(lower) {
		return true
	}

	// Short chit-chat messages that aren't greetings, don't contain code
	// references, and don't have task keywords don't need the extension.
	if len(text) < 30 {
		return false
	}

	// Longer messages (30+ chars) that aren't greetings: default to true
	// (conservative — might be a task without explicit keywords).
	return true
}

// taskKeywords are verbs and nouns that strongly indicate a task needing
// the full workflow guidance. Matched as substrings (case-insensitive) so
// "implementing", "implemented", "debugging" all match.
var taskKeywords = []string{
	// Action verbs
	"implement", "fix", "debug", "build", "test", "deploy",
	"refactor", "optimi", "migrat", "configur", "install",
	"create", "write", "edit", "update", "delete", "remove",
	"check", "analy", "review", "search", "find", "investigat",
	"run", "exec", "commit", "push", "lint", "format",
	"compil", "typecheck", "vet",
	// Task nouns
	"repo", "code", "file", "function", "class", "method",
	"bug", "error", "fail", "crash", "issue",
	"test", "spec", "doc", "document",
	"feature", "task", "ticket", "branch", "commit", "merge",
	"api", "endpoint", "database", "query", "schema",
	"docker", "container", "server", "service",
	"git", "github", "gitlab",
	// Question patterns that imply a task
	"how do i", "how to", "how can i", "what is the",
	"why does", "why is", "why doesn't",
	"can you", "could you", "please",
}

// greetingPatterns are messages that are purely social — no task intent.
var greetingPatterns = []string{
	"hi", "hello", "hey", "hi there", "hello there",
	"thanks", "thank you", "thx", "ty",
	"ok", "okay", "got it", "understood", "makes sense",
	"cool", "nice", "great", "awesome", "perfect",
	"yes", "no", "nope", "yep", "yeah",
	"bye", "goodbye", "see you",
	"morning", "afternoon", "evening",
}

func hasTaskKeyword(lower string) bool {
	for _, kw := range taskKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isGreeting(lower string) bool {
	text := strings.TrimSpace(lower)
	for _, g := range greetingPatterns {
		if text == g {
			return true
		}
	}
	// "hi there", "hello there", etc. with punctuation.
	stripped := strings.TrimRight(text, "!.,;?")
	for _, g := range greetingPatterns {
		if stripped == g {
			return true
		}
	}
	return false
}

func hasCodeReference(text string) bool {
	// File paths with extensions: foo/bar.go, script.py, etc.
	for _, ext := range []string{".go", ".py", ".ts", ".js", ".rs", ".rb", ".java", ".c", ".cpp", ".h", ".sh", ".yml", ".yaml", ".json", ".toml", ".md", ".sql", ".proto", ".html", ".css", ".tsx", ".jsx"} {
		if strings.Contains(text, ext) {
			return true
		}
	}
	// Backtick-enclosed code.
	if strings.Contains(text, "`") {
		return true
	}
	// URLs.
	if strings.Contains(text, "http://") || strings.Contains(text, "https://") || strings.Contains(text, "ftp://") {
		return true
	}
	// Dollar-sign shell prompts.
	if strings.HasPrefix(strings.TrimSpace(text), "$ ") {
		return true
	}
	return false
}

// maybeActivateExtension checks whether the extension prompt should be
// appended to a.Conv as a second system message. This happens when:
//   - /prompt core is active (IsPromptCore),
//   - the extension hasn't already been activated this session,
//   - AgentExtensionPrompt is non-empty (not configured in full mode or with
//     a custom prompt path), and
//   - the classifier detects the user message as a task (classifyRequest).
//
// Once activated, the extension stays for the session (sticky). The message
// is inserted after Conv[0] (the core preamble) with Turn==0 and Pinned so
// it survives compaction and long-session collapse projection (which passes
// through Turn==0 messages unconditionally). Must be called on the turn
// goroutine after ensurePreamble and before appendConv(userMsg).
func (a *App) maybeActivateExtension(userText string) {
	if !a.IsPromptCore() {
		return
	}
	// extensionActivated is turn-goroutine-only (like curTurn): Send is
	// serialized by the turn admission gate (admitTurn), so no concurrent
	// activation is possible. The initial check avoids the convMu round-trip
	// on the common sticky path.
	if a.extensionActivated {
		return
	}
	extPrompt := a.AgentExtensionPrompt
	if extPrompt == "" {
		return
	}
	if !classifyRequest(userText) {
		return
	}

	a.convMu.Lock()
	defer a.convMu.Unlock()

	// Re-check under convMu: NewConversation (stateMu+convMu) resets the
	// flag; while that can't race with the turn goroutine (turn admission),
	// the lock keeps the Conv mutation and readers consistent.

	// Build the extension system message. Turn==0 so collapse projection
	// passes it through. Pinned so compaction preserves it. Insert after
	// Conv[0] (the core preamble) — never between a tool-call and its result.
	extMsg := proxy.Message{
		Role:    "system",
		Content: StrPtr(extPrompt),
		Pinned:  true,
		Turn:    0,
	}
	if len(a.Conv) > 0 && a.Conv[0].Role == "system" {
		// Insert after Conv[0].
		a.Conv = append(a.Conv, extMsg)
		// Move the new message to position 1: swap Conv[1] and Conv[last].
		// Actually, append puts it at the end. We need to insert at index 1.
		// Use slice manipulation: [0] + [ext] + [1:].
		insertAt := 1
		if insertAt > len(a.Conv)-1 {
			insertAt = len(a.Conv) - 1
		}
		// Shift everything from index 1 onward one position right.
		// But the ext is already at the end — we need to move it to index 1.
		// Swap from end to index 1.
		for i := len(a.Conv) - 1; i > insertAt; i-- {
			a.Conv[i] = a.Conv[i-1]
		}
		a.Conv[insertAt] = extMsg
	} else {
		// No preamble yet — just append. ensurePreamble should have run
		// already, but guard against edge cases.
		a.Conv = append(a.Conv, extMsg)
	}

	a.extensionActivated = true
}
