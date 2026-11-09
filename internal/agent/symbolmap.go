package agent

// symbolmap.go — Symbol-level repo map enrichment (Roadmap #20).
//
// Builds a symbol outline by calling textDocument/documentSymbol per file via
// the LSP manager. Complements the existing file-tree repomap.go.
//
// Design (Mashūra-reviewed):
//   - On-demand only via /repomap --symbols (no startup build, no preamble mutation)
//   - Go-only for v1; other languages fall back to file-tree
//   - Uses documentSymbol (not workspace/symbol — spike confirmed the latter
//     cannot enumerate: 0 results on empty query, capped at 100)
//   - Separate spill key ("repo_symbols") — does not overwrite file-tree
//   - Ranking: shallow > deep, non-test > test (tests excluded in v1)
//   - 32KB cap by dropping whole low-ranked files, not mid-file truncation
//   - Honest status: discovered/selected/attempted/completed/failed/omitted

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/lsp"
	wtools "github.com/treeol/wakil/internal/tools"
)

// fileSymbols holds one file's document symbols, from either the hierarchical
// or the flat LSP response.
type fileSymbols struct {
	path    string
	symbols []lsp.DocumentSymbol
	flat    []lsp.SymbolInformation
}

// SymbolMapResult holds the built symbol outline and metadata.
type SymbolMapResult struct {
	Outline    string
	Discovered int
	Selected   int
	Attempted  int
	Completed  int
	Failed     int
	Omitted    int
	Partial    bool
	Status     string // "success", "partial", "unavailable", "timeout"
}

const symbolMapMaxFiles = 100
const symbolMapMaxBytes = 32 * 1024

// BuildSymbolMap walks the workspace for Go files, calls documentSymbol on each,
// and builds a ranked symbol outline.
func BuildSymbolMap(ctx context.Context, exe fileLister, mgr *lsp.Manager, lang string) (*SymbolMapResult, error) {
	if mgr == nil {
		return nil, fmt.Errorf("LSP manager not available")
	}
	if exe == nil {
		return nil, fmt.Errorf("no executor available")
	}

	// Phase 1: Discover source files.
	files, err := discoverSourceFiles(ctx, exe, lang)
	if err != nil {
		return nil, fmt.Errorf("discover files: %w", err)
	}

	result := &SymbolMapResult{
		Discovered: len(files),
		Status:     "success",
	}

	// Phase 2: Rank and select.
	rankFiles(files)
	if len(files) > symbolMapMaxFiles {
		result.Omitted = len(files) - symbolMapMaxFiles
		files = files[:symbolMapMaxFiles]
		result.Partial = true
	}
	result.Selected = len(files)

	// Phase 3: Call documentSymbol per file.
	// Short-circuit on server-level failure (spawn/capability) — don't retry 100 times.

	results := make([]fileSymbols, 0, len(files))
	serverUnavailable := false

	for _, f := range files {
		if ctx.Err() != nil {
			result.Partial = true
			if result.Status == "success" {
				result.Status = "timeout"
			}
			break
		}
		result.Attempted++
		docSyms, flatSyms, err := mgr.DocumentSymbol(ctx, lang, f)
		if err != nil {
			result.Failed++
			// Check if this is a server-level failure (spawn/capability).
			// If so, abort the loop — no point retrying.
			if isServerLevelError(err) {
				serverUnavailable = true
				// Remaining unattempted files count as omitted.
				result.Omitted += len(files) - result.Attempted
				break
			}
			continue
		}
		result.Completed++
		results = append(results, fileSymbols{path: f, symbols: docSyms, flat: flatSyms})
	}

	// Derive final status with precedence: timeout > unavailable > partial > success.
	if serverUnavailable && result.Completed == 0 {
		result.Status = "unavailable"
	} else if result.Status == "timeout" {
		// keep timeout
	} else if serverUnavailable {
		result.Status = "partial"
		result.Partial = true
	} else if result.Partial {
		if result.Status == "success" {
			result.Status = "partial"
		}
	}

	// If nothing was completed, mark as unavailable (unless already timeout).
	if result.Completed == 0 && result.Attempted > 0 && result.Status != "timeout" {
		result.Status = "unavailable"
	}

	// Phase 4: Render the outline, file by file, with marker-inclusive budget.
	outline, dropped, truncated := renderOutlineBudget(results, symbolMapMaxBytes)
	if dropped > 0 {
		outline += fmt.Sprintf("\n… [%d files omitted — byte cap %d]", dropped, symbolMapMaxBytes)
	}
	if dropped > 0 || truncated {
		result.Partial = true
		if result.Status == "success" {
			result.Status = "partial"
		}
	}

	result.Outline = outline
	return result, nil
}

// isServerLevelError returns true if the error indicates the server itself
// cannot be used (spawn failure, capability unsupported), as opposed to a
// per-file error (timeout, decode failure).
func isServerLevelError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "does not support") ||
		strings.Contains(s, "spawn failed") ||
		strings.Contains(s, "initialize failed") ||
		strings.Contains(s, "not found")
}

// renderFileSection renders one file's symbols as a section.
func renderFileSection(path string, docSyms []lsp.DocumentSymbol, flatSyms []lsp.SymbolInformation) string {
	var b strings.Builder
	b.WriteString(relPath(path) + ":\n")
	if len(docSyms) > 0 {
		for _, s := range docSyms {
			line := s.Range.Start.Line + 1
			fmt.Fprintf(&b, "  %s %s (line %d)\n", symbolKindName(s.Kind), s.Name, line)
		}
	} else if len(flatSyms) > 0 {
		for _, s := range flatSyms {
			line := s.Location.Range.Start.Line + 1
			fmt.Fprintf(&b, "  %s %s (line %d)\n", symbolKindName(s.Kind), s.Name, line)
		}
	}
	return b.String()
}

// renderOutlineBudget renders file sections in rank order until the next
// section would push the output past the budget (budget minus a fixed marker
// reserve). It returns the outline, the number of files dropped by that cut,
// and whether the first file was included truncated because it alone exceeded
// the budget. In that case nothing is counted as dropped. Byte-cap drops do
// not feed result.Omitted.
func renderOutlineBudget(results []fileSymbols, maxBytes int) (outline string, dropped int, truncated bool) {
	const markerBudget = 100 // reserve space for truncation marker
	fileBudget := maxBytes - markerBudget

	var b strings.Builder
	rendered := 0
	for _, r := range results {
		section := renderFileSection(r.path, r.symbols, r.flat)
		if b.Len()+len(section) > fileBudget {
			break
		}
		b.WriteString(section)
		rendered++
	}

	if rendered == 0 && len(results) > 0 {
		// First file was too large — include it truncated, marked on that file only.
		section := renderFileSection(results[0].path, results[0].symbols, results[0].flat)
		if len(section) > fileBudget {
			section = truncateUTF8(section[:fileBudget], fileBudget) + "\n  (truncated)\n"
			return section, 0, true
		}
		return section, 0, false
	}
	return b.String(), len(results) - rendered, false
}

// discoverSourceFiles walks the filesystem and returns source file paths,
// skipping vendor, testdata, generated, and test files.
func discoverSourceFiles(ctx context.Context, exe fileLister, lang string) ([]string, error) {
	var files []string
	visits := 0
	const maxVisits = 500
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if depth > 10 || visits >= maxVisits {
			return nil
		}
		visits++
		listing, err := exe.ListDir(ctx, dir)
		if err != nil {
			if dir == "." {
				return fmt.Errorf("list root: %w", err)
			}
			return nil // skip unreadable dirs
		}
		for _, line := range strings.Split(listing, "\n") {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			full := line
			if dir != "." {
				full = filepath.Join(dir, line)
			}
			full = strings.TrimSuffix(full, "/")
			if strings.HasSuffix(line, "/") {
				dirName := strings.TrimSuffix(line, "/")
				if isExcludedDir(dirName) {
					continue
				}
				if err := walk(full, depth+1); err != nil {
					return err
				}
			} else if isSourceFile(line, lang) && !isExcludedFile(line) {
				files = append(files, full)
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return nil, err
	}
	return files, nil
}

func isExcludedDir(name string) bool {
	switch name {
	case "vendor", "testdata", "node_modules", ".git", "dist", "build",
		"target", "__pycache__", "tmp", "coverage":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "."
}

func isSourceFile(name, lang string) bool {
	switch lang {
	case "go":
		return strings.HasSuffix(name, ".go")
	case "rust":
		return strings.HasSuffix(name, ".rs")
	case "python":
		return strings.HasSuffix(name, ".py")
	case "typescript":
		return strings.HasSuffix(name, ".ts") && !strings.HasSuffix(name, ".d.ts")
	case "javascript":
		return strings.HasSuffix(name, ".js")
	case "c":
		return strings.HasSuffix(name, ".c") || strings.HasSuffix(name, ".h")
	case "cpp":
		return strings.HasSuffix(name, ".cpp") || strings.HasSuffix(name, ".cc") || strings.HasSuffix(name, ".hpp")
	}
	return false
}

func isExcludedFile(name string) bool {
	if strings.HasSuffix(name, "_test.go") {
		return true
	}
	if strings.HasSuffix(name, ".pb.go") || strings.HasSuffix(name, ".pb.validate.go") {
		return true
	}
	if strings.HasSuffix(name, "_gen.go") || strings.HasSuffix(name, "_generated.go") {
		return true
	}
	return false
}

func rankFiles(files []string) {
	sort.SliceStable(files, func(i, j int) bool {
		si := strings.Count(files[i], "/")
		sj := strings.Count(files[j], "/")
		if si != sj {
			return si < sj
		}
		return files[i] < files[j]
	})
}

func symbolKindName(k lsp.SymbolKind) string {
	switch k {
	case lsp.SymFile:
		return "file"
	case lsp.SymModule:
		return "module"
	case lsp.SymNamespace:
		return "namespace"
	case lsp.SymPackage:
		return "package"
	case lsp.SymClass:
		return "class"
	case lsp.SymMethod:
		return "method"
	case lsp.SymProperty:
		return "property"
	case lsp.SymField:
		return "field"
	case lsp.SymConstructor:
		return "constructor"
	case lsp.SymEnum:
		return "enum"
	case lsp.SymInterface:
		return "interface"
	case lsp.SymFunction:
		return "func"
	case lsp.SymVariable:
		return "var"
	case lsp.SymConstant:
		return "const"
	case lsp.SymStruct:
		return "struct"
	case lsp.SymTypeParameter:
		return "typeparam"
	default:
		return fmt.Sprintf("kind%d", int(k))
	}
}

func relPath(p string) string {
	return strings.TrimPrefix(p, "./")
}

func handleSymbolMapCommand(ctx context.Context, app *App) (string, error) {
	if app == nil {
		return "", fmt.Errorf("no app available")
	}
	if app.LSP == nil {
		// Fallback: suggest file-tree.
		return "", fmt.Errorf("LSP not enabled — use /repomap for file-tree outline, or enable lsp_enabled in config")
	}
	if app.Exec == nil {
		return "", fmt.Errorf("no executor available")
	}

	lang := "go"

	repoCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := BuildSymbolMap(repoCtx, app.Exec, app.LSP, lang)
	if err != nil {
		return "", fmt.Errorf("build symbol map: %w", err)
	}

	spillPath := wtools.SpillToCache(app.chatID(), "repo_symbols", result.Outline)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Symbol map (%s): %d discovered, %d selected, %d attempted, %d completed, %d failed, %d omitted.",
		lang, result.Discovered, result.Selected, result.Attempted, result.Completed, result.Failed, result.Omitted))
	sb.WriteString(" Status: " + result.Status)
	if result.Partial {
		sb.WriteString(" (partial)")
	}
	sb.WriteString("\n\n")
	if spillPath != "" {
		sb.WriteString(fmt.Sprintf("Full symbol outline at: %s\n\n", spillPath))
	}
	preview := result.Outline
	if len(preview) > 2048 {
		preview = truncateUTF8(preview[:2048], 2048) + "…"
	}
	sb.WriteString(preview)
	return sb.String(), nil
}
