package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// HandleStyleCommand reports catalog-backed state or changes this session's
// selection. It never reads guide files, changes configuration, or invokes a model.
func (a *App) HandleStyleCommand(fields []string) string {
	if len(fields) == 1 {
		return a.styleStatus()
	}
	if len(fields) != 3 || (fields[1] != "enable" && fields[1] != "disable") {
		return "usage: /styles | /styles enable|disable <language|all>"
	}
	return a.setStyleGuide(fields[1] == "enable", fields[2])
}

func (a *App) styleStatus() string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	catalog := a.styleState.catalog
	if catalog == nil || len(catalog.Guides()) == 0 {
		return "style guides: disabled or none configured\n  enable in config with \"style_guides_enabled\": true, then restart"
	}
	active := map[string]bool{}
	for _, id := range a.styleSelection().IDs() {
		active[id] = true
	}
	var b strings.Builder
	b.WriteString("style guides (loaded at startup):")
	_, omitted := catalog.Select(a.styleState.explicit, nil)
	omittedIDs := map[string]bool{}
	for _, id := range omitted {
		omittedIDs[id] = true
	}
	for _, guide := range catalog.Guides() {
		state := "available"
		if a.styleState.disabled["*"] || a.styleState.disabled[guide.ID] {
			state = "disabled"
		} else if omittedIDs[guide.ID] {
			state = "selected; omitted by limit"
		}
		if active[guide.ID] {
			state = "active"
		}
		fmt.Fprintf(&b, "\n  %s (%s): %s", guide.ID, guide.Source, state)
	}
	b.WriteString("\nUse /styles enable|disable <language|all>; project AGENTS.md and your request take priority.")
	return b.String()
}

func (a *App) setStyleGuide(enable bool, name string) string {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	catalog := a.styleState.catalog
	if catalog == nil || len(catalog.Guides()) == 0 {
		return "style guides are not enabled; set style_guides_enabled in config and restart"
	}
	name = strings.ToLower(name)
	if name == "all" {
		if a.styleState.disabled == nil {
			a.styleState.disabled = map[string]bool{}
		}
		if enable {
			delete(a.styleState.disabled, "*")
		} else {
			a.styleState.disabled["*"] = true
			a.styleState.explicit = nil
			a.styleState.path = nil
		}
		a.publishStyleSelection()
		return a.styleCommandResult(enable, "all")
	}
	id := catalog.Resolve(name)
	if id == "" || !catalog.Has(id) {
		loaded := catalog.Guides()
		names := make([]string, 0, len(loaded))
		for _, guide := range loaded {
			names = append(names, guide.ID)
		}
		sort.Strings(names)
		return fmt.Sprintf("style guide %q is not loaded; available: %s", name, strings.Join(names, ", "))
	}
	if a.styleState.disabled == nil {
		a.styleState.disabled = map[string]bool{}
	}
	if enable {
		if a.styleState.disabled["*"] {
			return "all style guides are disabled; run /styles enable all first"
		}
		delete(a.styleState.disabled, id)
		if !containsString(a.styleState.explicit, id) {
			a.styleState.explicit = append(a.styleState.explicit, id)
		}
	} else {
		a.styleState.disabled[id] = true
		a.styleState.explicit = removeString(a.styleState.explicit, id)
		a.styleState.path = removeString(a.styleState.path, id)
	}
	a.publishStyleSelection()
	return a.styleCommandResult(enable, id)
}

func (a *App) styleCommandResult(enable bool, id string) string {
	// Caller holds stateMu.
	verb := "disabled"
	if enable {
		verb = "enabled"
	}
	result := "style guide " + id + ": " + verb
	if enable && id == "all" {
		result = "style guides: automatic activation re-enabled; individual opt-outs preserved"
	}
	if _, omitted := a.styleState.catalog.Select(a.styleState.explicit, nil); len(omitted) > 0 {
		result += fmt.Sprintf("\n⚠ not active due to the %d-guide/%d-byte limit: %s", styleguide.MaxGuides, styleguide.MaxMessageBytes, strings.Join(omitted, ", "))
	}
	if active := a.styleSelection().Sources(); len(active) > 0 {
		result += "\nactive: " + strings.Join(active, ", ")
	} else {
		result += "\nactive: none"
	}
	return result
}

func (a *App) styleSelection() styleguide.Selection {
	if v := a.styleActive.Load(); v != nil {
		return v.(styleSnapshot).active
	}
	return emptyStyleSelection
}

func (a *App) styleSnapshot() styleSnapshot {
	if v := a.styleActive.Load(); v != nil {
		s := v.(styleSnapshot)
		s.explicit = append([]string(nil), s.explicit...)
		s.disabled = copyStyleDisabled(s.disabled)
		s.path = append([]string(nil), s.path...)
		return s
	}
	return styleSnapshot{}
}

type styleSnapshot struct {
	catalog  *styleguide.Catalog
	explicit []string
	path     []string
	disabled map[string]bool
	active   styleguide.Selection
}

func copyStyleDisabled(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for id, disabled := range in {
		out[id] = disabled
	}
	return out
}

// publishStyleSelection builds from caller-owned fields, then publishes one
// immutable snapshot. Callers hold stateMu (or are constructing an unpublished
// child); no I/O or callbacks occur here.
func (a *App) publishStyleSelection() {
	explicit := a.styleState.explicit
	paths := a.styleState.path
	if a.styleState.disabled["*"] {
		explicit, paths = nil, nil
	} else {
		explicit = a.enabledStyleIDs(explicit)
		paths = a.enabledStyleIDs(paths)
	}
	selection, _ := a.styleState.catalog.Select(explicit, paths)
	a.styleActive.Store(styleSnapshot{
		catalog:  a.styleState.catalog,
		explicit: append([]string(nil), a.styleState.explicit...),
		path:     append([]string(nil), a.styleState.path...),
		disabled: copyStyleDisabled(a.styleState.disabled),
		active:   selection,
	})
}

func (a *App) enabledStyleIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !a.styleState.disabled[id] {
			out = append(out, id)
		}
	}
	return out
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
		return []string{Dim("· style guides disabled for this session")}
	}
	if id, enable, ok := a.styleState.catalog.GuideDirective(userText); ok {
		if a.styleState.disabled == nil {
			a.styleState.disabled = map[string]bool{}
		}
		if enable {
			if a.styleState.disabled["*"] {
				return []string{Yellow("style guides remain globally disabled; use /styles enable all first")}
			}
			delete(a.styleState.disabled, id)
			if !containsString(a.styleState.explicit, id) {
				a.styleState.explicit = append(a.styleState.explicit, id)
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
				// Keep overflow requests pending; a freed slot promotes them.
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
	projection := a.withPruneReminder(a.contextProjection())
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

func requestHasStyleGuide(messages []proxy.Message) bool {
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(DerefStr(message.Content), "Language style guidance") {
			return true
		}
	}
	return false
}
