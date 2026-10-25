package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/styleguide"
)

// styleGuideState holds a read-once immutable catalog and turn-local evidence.
// Selection is published in atomic.Value so RPC readers and child construction
// cannot race. Explicit named languages are session-sticky; tool-path evidence
// lasts only for the current turn. Compaction and collapse preserve selection.
type styleGuideState struct {
	catalog  *styleguide.Catalog
	explicit []string
	disabled map[string]bool
	path     []string
}

var emptyStyleSelection = styleguide.Selection{}

// SetStyleGuideCatalog installs an immutable, read-once guide catalog.
// Rebuilding an App is what reloads configured files; per-turn delivery does not.
func (a *App) SetStyleGuideCatalog(catalog *styleguide.Catalog) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.styleState.catalog = catalog
	a.resetStyleGuides()
}

func (a *App) styleSelection() styleguide.Selection {
	if v := a.styleActive.Load(); v != nil {
		return v.(styleSnapshot).active
	}
	return emptyStyleSelection
}

func (a *App) styleSnapshot() (*styleguide.Catalog, []string) {
	if v := a.styleActive.Load(); v != nil {
		s := v.(styleSnapshot)
		return s.catalog, append([]string(nil), s.explicit...)
	}
	return nil, nil
}

type styleSnapshot struct {
	catalog  *styleguide.Catalog
	explicit []string
	active   styleguide.Selection
}

// publishStyleSelection builds from caller-owned fields, then publishes one
// immutable snapshot. Callers hold stateMu (or are constructing an unpublished
// child); no I/O or callbacks occur here.
func (a *App) publishStyleSelection() {
	selection, _ := a.styleState.catalog.Select(a.styleState.explicit, a.styleState.path)
	a.styleActive.Store(styleSnapshot{
		catalog:  a.styleState.catalog,
		explicit: append([]string(nil), a.styleState.explicit...),
		active:   selection,
	})
}

func (a *App) prepareStyleGuides(userText string) {
	notices := a.updateStyleGuides(userText)
	for _, notice := range notices {
		fmt.Fprintln(a.Out, notice)
	}
}

// updateStyleGuides changes activation and returns notices to emit after its
// lock is released. No output, I/O, or callbacks occur under stateMu.
func (a *App) updateStyleGuides(userText string) (notices []string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.styleState.catalog == nil {
		return nil
	}
	a.styleState.path = nil
	lowerText := strings.ToLower(userText)
	if strings.Contains(lowerText, "disable style guides") || strings.Contains(lowerText, "disable all style guides") {
		if a.styleState.disabled == nil {
			a.styleState.disabled = map[string]bool{}
		}
		a.styleState.disabled["*"] = true
		a.styleState.explicit = nil
		a.publishStyleSelection()
		return nil
	}
	if id, enable, ok := a.styleState.catalog.GuideDirective(userText); ok {
		if a.styleState.disabled == nil {
			a.styleState.disabled = map[string]bool{}
		}
		if enable {
			if a.styleState.disabled["*"] {
				return nil
			}
			delete(a.styleState.disabled, id)
			if !containsString(a.styleState.explicit, id) {
				if len(a.styleState.explicit) >= styleguide.MaxGuides {
					if warning := a.styleOmittedOnce([]string{id}); warning != "" {
						notices = append(notices, warning)
					}
				} else {
					a.styleState.explicit = append(a.styleState.explicit, id)
				}
			}
		} else {
			a.styleState.disabled[id] = true
			a.styleState.explicit = removeString(a.styleState.explicit, id)
		}
		a.publishStyleSelection()
		return append(notices, a.styleSelectionNotices(nil)...)
	}
	if strings.Contains(lowerText, "enable style guides") || strings.Contains(lowerText, "enable all style guides") {
		delete(a.styleState.disabled, "*")
		a.publishStyleSelection()
		return nil
	}
	if a.styleState.disabled["*"] {
		a.publishStyleSelection()
		return nil
	}
	candidates := a.styleState.catalog.NamedLanguages(userText)
	if len(candidates) > 0 && hasCodingStyleIntent(userText) {
		for _, id := range candidates {
			if !a.styleState.catalog.Has(id) || a.styleState.disabled[id] {
				continue
			}
			if !containsString(a.styleState.explicit, id) {
				// Explicit languages are session-sticky and cannot be evicted
				// by incidental tool paths.
				if len(a.styleState.explicit) >= styleguide.MaxGuides {
					if warning := a.styleOmittedOnce([]string{id}); warning != "" {
						notices = append(notices, warning)
					}
					continue
				}
				a.styleState.explicit = append(a.styleState.explicit, id)
			}
		}
	}
	// Request filenames select only for this request. Tool paths observed
	// below are additional evidence, also cleared at the next request.
	for token := range strings.FieldsSeq(userText) {
		token = strings.Trim(token, "`'\"()[]{}<>,.;:!?")
		if id := a.styleState.catalog.LanguageForPath(token); id != "" && !a.styleState.disabled[id] && !containsString(a.styleState.explicit, id) && !containsString(a.styleState.path, id) {
			a.styleState.path = append(a.styleState.path, id)
		}
	}
	beforeIDs := strings.Join(a.styleSelection().IDs(), "\x00")
	a.publishStyleSelection()
	return append(notices, a.styleSelectionNotices(&beforeIDs)...)
}

// styleSelectionNotices reports actual delivered IDs and all requested IDs
// omitted by count/byte limits. Callers hold stateMu; output happens later.
func (a *App) styleSelectionNotices(beforeIDs *string) []string {
	var notices []string
	if beforeIDs != nil {
		after := a.styleSelection().Sources()
		if len(after) > 0 && *beforeIDs != strings.Join(a.styleSelection().IDs(), "\x00") {
			notices = append(notices, Dim("· style guides active: "+strings.Join(after, ", ")))
		}
	}
	if _, omitted := a.styleState.catalog.Select(a.styleState.explicit, nil); len(omitted) > 0 {
		if warning := a.styleOmittedOnce(omitted); warning != "" {
			notices = append(notices, warning)
		}
	}
	return notices
}

func (a *App) styleOmittedOnce(ids []string) string {
	if a.styleOmitted == nil {
		a.styleOmitted = map[string]bool{}
	}
	var fresh []string
	for _, id := range ids {
		if !a.styleOmitted[id] {
			a.styleOmitted[id] = true
			fresh = append(fresh, id)
		}
	}
	if len(fresh) > 0 {
		return Yellow("⚠ style guide limit reached; not active: " + strings.Join(fresh, ", "))
	}
	return ""
}

func (a *App) observeStyleToolPath(toolName, rawArgs string, ok bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if !ok || a.styleState.catalog == nil {
		return
	}
	if toolName != "read_file" && toolName != "read_file_full" && toolName != "write_file" && toolName != "edit_file" {
		return
	}
	var args struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(rawArgs), &args) != nil || args.Path == "" {
		return
	}
	id := a.styleState.catalog.LanguageForPath(filepath.Base(args.Path))
	if id != "" && !a.styleState.disabled["*"] && !a.styleState.disabled[id] && !containsString(a.styleState.path, id) && !containsString(a.styleState.explicit, id) {
		a.styleState.path = append(a.styleState.path, id)
		a.publishStyleSelection()
	}
}

func removeString(items []string, remove string) []string {
	out := items[:0]
	for _, item := range items {
		if item != remove {
			out = append(out, item)
		}
	}
	return out
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func hasCodingStyleIntent(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range []string{
		"code", "coding", "program", "script", "function", "class", "module",
		"implement", "refactor", "fix", "debug", "test", "lint", "format",
		"style", "convention", "idiom", "review", "write", "edit", "build",
		"compile", "typecheck", "api", "library", "package", "repository",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	for _, ext := range []string{".go", ".py", ".ts", ".js", ".rs", ".tsx", ".jsx"} {
		if strings.Contains(lower, ext) {
			return true
		}
	}
	return strings.Contains(text, "`")
}

func (a *App) resetStyleGuides() {
	// Callers hold stateMu; session reset already owns it, while the catalog
	// setter is a construction-time API.
	a.styleState.path = nil
	a.styleState.explicit = nil
	a.styleState.disabled = nil
	a.styleOmitted = nil
	a.styleActive.Store(styleSnapshot{catalog: a.styleState.catalog})
}

// requestMessages adds the transient guide immediately after Conv[0], before
// the optional core extension. It never changes Conv or collapse records. The
// returned slice owns its backing array whenever a guide is active.
func (a *App) requestMessages() []proxy.Message {
	projection := a.contextProjection()
	text := a.styleSelection().Text()
	if text == "" {
		return projection
	}
	insertAt := 0
	if len(projection) > 0 && projection[0].Role == "system" {
		insertAt = 1
	}
	out := make([]proxy.Message, 0, len(projection)+1)
	out = append(out, projection[:insertAt]...)
	content := text
	out = append(out, proxy.Message{Role: "system", Content: &content})
	out = append(out, projection[insertAt:]...)
	return out[:len(out):len(out)]
}

func (a *App) sentSize(messages []proxy.Message) int {
	return TranscriptSize(messages) + a.styleSelection().Bytes()
}
