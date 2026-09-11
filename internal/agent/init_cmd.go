package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/treeol/wakil/internal/verify"
)

// projectInfo holds detected project conventions for AGENTS.md generation.
type projectInfo struct {
	languages  []string
	buildCmds  []string
	testCmds   []string
	lintCmds   []string
	moduleName string
	// entryPoints are the main package directories (e.g. cmd/wakil).
	entryPoints []string
	// packages are key directories under internal/ or src/ that form the
	// project's architecture.
	packages []string
	// hasDocs is true if a docs/ directory exists.
	hasDocs bool
	// hasDocker is true if a Dockerfile exists.
	hasDocker bool
	// hasCI is true if a .github directory exists.
	hasCI bool
}

// fileLister is the minimal interface detectProject needs from the executor.
type fileLister interface {
	ListDir(ctx context.Context, path string) (string, error)
	ReadFile(ctx context.Context, path string) (string, error)
}

// detectProject reads the workspace root listing and manifest files to infer
// project conventions. It uses the executor for sandbox-safe file access.
func detectProject(ctx context.Context, exe fileLister) (*projectInfo, error) {
	if exe == nil {
		return nil, fmt.Errorf("no executor available")
	}
	listing, err := exe.ListDir(ctx, ".")
	if err != nil {
		return nil, fmt.Errorf("list workspace root: %w", err)
	}

	// Parse the listing — entries are newline-separated names, dirs may have
	// trailing "/".
	files := make(map[string]bool)
	dirs := make(map[string]bool)
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "/") {
			name := strings.TrimSuffix(line, "/")
			dirs[name] = true
		} else {
			files[line] = true
		}
	}

	// Use verify.DetectCommands for test commands — reuse existing logic.
	fileNames := sortedKeys(files)
	verifyCmds := verify.DetectCommands(fileNames)
	info := &projectInfo{}

	for _, cmd := range verifyCmds {
		info.testCmds = append(info.testCmds, cmd.Cmd)
	}

	// Detect language + build + lint from manifests.
	if files["go.mod"] {
		info.languages = append(info.languages, "Go")
		info.buildCmds = append(info.buildCmds, "go build ./...")
		info.lintCmds = append(info.lintCmds, "go vet ./...")
		if content, err := exe.ReadFile(ctx, "go.mod"); err == nil {
			for _, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(line, "module ") {
					info.moduleName = strings.TrimSpace(strings.TrimPrefix(line, "module "))
					break
				}
			}
		}
	}

	if files["package.json"] {
		info.languages = append(info.languages, "JavaScript/TypeScript")
		if files["pnpm-lock.yaml"] {
			info.buildCmds = append(info.buildCmds, "pnpm build")
			info.testCmds = appendIfMissing(info.testCmds, "pnpm test")
			info.lintCmds = append(info.lintCmds, "pnpm lint")
		} else if files["yarn.lock"] {
			info.buildCmds = append(info.buildCmds, "yarn build")
			info.testCmds = appendIfMissing(info.testCmds, "yarn test")
			info.lintCmds = append(info.lintCmds, "yarn lint")
		} else {
			info.buildCmds = append(info.buildCmds, "npm run build")
			info.testCmds = appendIfMissing(info.testCmds, "npm test")
			info.lintCmds = append(info.lintCmds, "npm run lint")
		}
	}

	if files["Cargo.toml"] {
		info.languages = append(info.languages, "Rust")
		info.buildCmds = append(info.buildCmds, "cargo build")
		info.testCmds = appendIfMissing(info.testCmds, "cargo test")
		info.lintCmds = append(info.lintCmds, "cargo clippy")
	}

	if files["pyproject.toml"] {
		info.languages = append(info.languages, "Python")
		info.buildCmds = append(info.buildCmds, "pip install -e .")
		info.testCmds = appendIfMissing(info.testCmds, "pytest")
		info.lintCmds = append(info.lintCmds, "ruff check .")
	}

	if files["Makefile"] {
		// Makefile is a build tool, not a language.
		info.buildCmds = appendIfMissing(info.buildCmds, "make")
	}

	// Detect entry points (cmd/ directory with Go main packages).
	if dirs["cmd"] {
		if cmdListing, err := exe.ListDir(ctx, "cmd"); err == nil {
			for _, line := range strings.Split(cmdListing, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				line = strings.TrimSuffix(line, "/")
				if !strings.HasPrefix(line, ".") {
					info.entryPoints = append(info.entryPoints, filepath.Join("cmd", line))
				}
			}
			sort.Strings(info.entryPoints)
		}
	}

	// Detect architecture packages under internal/.
	if dirs["internal"] {
		if intListing, err := exe.ListDir(ctx, "internal"); err == nil {
			for _, line := range strings.Split(intListing, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				line = strings.TrimSuffix(line, "/")
				if !strings.HasPrefix(line, ".") {
					info.packages = append(info.packages, "internal/"+line)
				}
			}
			sort.Strings(info.packages)
		}
	}

	// Also check src/ for Node projects.
	if dirs["src"] {
		if srcListing, err := exe.ListDir(ctx, "src"); err == nil {
			for _, line := range strings.Split(srcListing, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				line = strings.TrimSuffix(line, "/")
				if !strings.HasPrefix(line, ".") {
					info.packages = append(info.packages, "src/"+line)
				}
			}
			sort.Strings(info.packages)
		}
	}

	info.hasDocs = dirs["docs"]
	info.hasDocker = files["Dockerfile"] || files["Dockerfile.daemon"]
	info.hasCI = dirs[".github"]

	return info, nil
}

// generateAgentsMD produces an AGENTS.md template from detected project info.
// Commands are marked as suggestions — the user should verify them.
func generateAgentsMD(info *projectInfo) string {
	var b strings.Builder
	b.WriteString("# AGENTS.md\n")
	b.WriteString("<!-- Generated by /init — verify and edit these suggestions -->\n")

	if len(info.languages) > 0 {
		b.WriteString("\n## Language\n\n")
		b.WriteString("- " + strings.Join(info.languages, ", ") + "\n")
	}

	if info.moduleName != "" {
		b.WriteString("\n## Module\n\n")
		b.WriteString(fmt.Sprintf("- %s\n", info.moduleName))
	}

	if len(info.buildCmds) > 0 {
		b.WriteString("\n## Build\n\n")
		for _, cmd := range info.buildCmds {
			b.WriteString(fmt.Sprintf("- `%s`\n", cmd))
		}
	}

	if len(info.testCmds) > 0 {
		b.WriteString("\n## Test\n\n")
		for _, cmd := range info.testCmds {
			b.WriteString(fmt.Sprintf("- `%s`\n", cmd))
		}
	}

	if len(info.lintCmds) > 0 {
		b.WriteString("\n## Lint\n\n")
		for _, cmd := range info.lintCmds {
			b.WriteString(fmt.Sprintf("- `%s`\n", cmd))
		}
	}

	// Architecture section — the most useful part.
	if len(info.entryPoints) > 0 || len(info.packages) > 0 {
		b.WriteString("\n## Architecture\n\n")
		if len(info.entryPoints) > 0 {
			b.WriteString("Entry points:\n")
			for _, ep := range info.entryPoints {
				b.WriteString(fmt.Sprintf("- `%s/` — main package\n", ep))
			}
		}
		if len(info.packages) > 0 {
			if len(info.entryPoints) > 0 {
				b.WriteString("\n")
			}
			b.WriteString("Core packages:\n")
			for _, pkg := range info.packages {
				b.WriteString(fmt.Sprintf("- `%s/`\n", pkg))
			}
		}
	}

	// Note infrastructure presence.
	var infra []string
	if info.hasDocker {
		infra = append(infra, "Docker")
	}
	if info.hasCI {
		infra = append(infra, "GitHub Actions CI")
	}
	if info.hasDocs {
		infra = append(infra, "docs/")
	}
	if len(infra) > 0 {
		b.WriteString("\n## Infrastructure\n\n")
		for _, item := range infra {
			b.WriteString(fmt.Sprintf("- %s\n", item))
		}
	}

	b.WriteString("\n## Conventions\n\n")
	b.WriteString("<!-- Add project-specific conventions here -->\n")

	return b.String()
}

// handleInitCommand implements /init: detects project conventions from the
// workspace root and creates an AGENTS.md file if one does not already exist.
// Returns the summary message and any error.
func handleInitCommand(app *App) (string, error) {
	if app.Exec == nil {
		return "", fmt.Errorf("no executor available")
	}

	cwd := app.Exec.Cwd()
	if cwd == "" {
		cwd = app.Cfg.WorkDir
	}
	if cwd == "" {
		return "", fmt.Errorf("cannot determine working directory")
	}

	// Check if AGENTS.md already exists.
	if _, err := app.Exec.ReadFile(context.Background(), "AGENTS.md"); err == nil {
		return fmt.Sprintf("AGENTS.md already exists at %s — /init will not overwrite it", filepath.Join(cwd, "AGENTS.md")), nil
	}

	// Detect project info.
	info, err := detectProject(context.Background(), app.Exec)
	if err != nil {
		return "", fmt.Errorf("detect project: %w", err)
	}

	// Generate and write AGENTS.md.
	content := generateAgentsMD(info)
	if _, err := app.Exec.WriteFile(context.Background(), "AGENTS.md", content); err != nil {
		return "", fmt.Errorf("write AGENTS.md: %w", err)
	}

	// Summarize what was detected.
	var detected []string
	if len(info.languages) > 0 {
		detected = append(detected, "language: "+strings.Join(info.languages, ", "))
	}
	if info.moduleName != "" {
		detected = append(detected, "module: "+info.moduleName)
	}
	if len(info.entryPoints) > 0 {
		detected = append(detected, fmt.Sprintf("%d entry points", len(info.entryPoints)))
	}
	if len(info.packages) > 0 {
		detected = append(detected, fmt.Sprintf("%d packages", len(info.packages)))
	}
	if len(info.buildCmds) > 0 {
		detected = append(detected, "build: "+strings.Join(info.buildCmds, ", "))
	}
	if len(info.testCmds) > 0 {
		detected = append(detected, "test: "+strings.Join(info.testCmds, ", "))
	}

	summary := "created AGENTS.md at " + filepath.Join(cwd, "AGENTS.md")
	if len(detected) > 0 {
		summary += " (" + strings.Join(detected, "; ") + ")"
	} else {
		summary += " (no project manifests detected — edit the file to add your conventions)"
	}
	summary += "\n\nThe file will be included in your system context on the next turn."
	return summary, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendIfMissing(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}
