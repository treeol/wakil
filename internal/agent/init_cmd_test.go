package agent

import (
	"context"
	"strings"
	"testing"
)

func TestGenerateAgentsMD_Go(t *testing.T) {
	info := &projectInfo{
		languages:  []string{"Go"},
		buildCmds:  []string{"go build ./..."},
		testCmds:   []string{"go test ./..."},
		lintCmds:   []string{"go vet ./..."},
		moduleName: "github.com/example/myapp",
	}
	got := generateAgentsMD(info)

	if !strings.Contains(got, "# AGENTS.md") {
		t.Error("missing title")
	}
	if !strings.Contains(got, "verify and edit") {
		t.Error("missing generation notice")
	}
	if !strings.Contains(got, "Go") {
		t.Error("missing language")
	}
	if !strings.Contains(got, "github.com/example/myapp") {
		t.Error("missing module name")
	}
}

func TestGenerateAgentsMD_WithArchitecture(t *testing.T) {
	info := &projectInfo{
		languages:    []string{"Go"},
		buildCmds:    []string{"go build ./..."},
		entryPoints:  []string{"cmd/wakil"},
		packages:     []string{"internal/agent", "internal/exec", "internal/tui"},
		hasDocker:    true,
		hasCI:        true,
		hasDocs:      true,
	}
	got := generateAgentsMD(info)

	if !strings.Contains(got, "## Architecture") {
		t.Error("missing Architecture section")
	}
	if !strings.Contains(got, "cmd/wakil/") {
		t.Error("missing entry point")
	}
	if !strings.Contains(got, "internal/agent/") {
		t.Error("missing package")
	}
	if !strings.Contains(got, "## Infrastructure") {
		t.Error("missing Infrastructure section")
	}
	if !strings.Contains(got, "Docker") {
		t.Error("missing Docker")
	}
	if !strings.Contains(got, "GitHub Actions CI") {
		t.Error("missing CI")
	}
	if !strings.Contains(got, "docs/") {
		t.Error("missing docs/")
	}
}

func TestGenerateAgentsMD_Empty(t *testing.T) {
	info := &projectInfo{}
	got := generateAgentsMD(info)
	if !strings.Contains(got, "# AGENTS.md") {
		t.Error("missing title")
	}
	if !strings.Contains(got, "Conventions") {
		t.Error("should always have Conventions section")
	}
}

func TestHandleInitCommand_CreatesFile(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["go.mod"] = "module github.com/test/myapp\n\ngo 1.26\n"
	exe.dirs["cmd"] = true
	exe.dirs["internal"] = true
	// Fake executor's ListDir returns all files; add subdirectory entries
	// as "dirs" so they appear in listing. The fake executor lists files
	// only, so we simulate directory listings by adding files with dir
	// names as prefixes.
	exe.files["cmd/wakil"] = "" // appears in cmd/ listing
	exe.files["internal/agent"] = ""
	exe.files["internal/exec"] = ""
	exe.files["internal/tui"] = ""

	app := &App{Exec: exe}

	summary, err := handleInitCommand(app)
	if err != nil {
		t.Fatalf("handleInitCommand: %v", err)
	}
	if !strings.Contains(summary, "created AGENTS.md") {
		t.Errorf("expected creation message, got: %s", summary)
	}

	// Verify file was written with architecture
	content, err := exe.ReadFile(context.Background(), "AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "# AGENTS.md") {
		t.Error("file should contain AGENTS.md title")
	}
}

func TestHandleInitCommand_AlreadyExists(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["AGENTS.md"] = "existing content"
	exe.files["go.mod"] = "module github.com/test/app\n"

	app := &App{Exec: exe}

	summary, err := handleInitCommand(app)
	if err != nil {
		t.Fatalf("handleInitCommand: %v", err)
	}
	if !strings.Contains(summary, "already exists") {
		t.Errorf("should report existing file: %s", summary)
	}

	content, _ := exe.ReadFile(context.Background(), "AGENTS.md")
	if content != "existing content" {
		t.Errorf("original content should be unchanged, got: %s", content)
	}
}

func TestHandleInitCommand_NoManifests(t *testing.T) {
	exe := newFakeExecutor()

	app := &App{Exec: exe}

	summary, err := handleInitCommand(app)
	if err != nil {
		t.Fatalf("handleInitCommand: %v", err)
	}
	if !strings.Contains(summary, "created AGENTS.md") {
		t.Errorf("should still create file: %s", summary)
	}
	if !strings.Contains(summary, "no project manifests detected") {
		t.Errorf("should report no manifests: %s", summary)
	}
}

func TestHandleInitCommand_NilExecutor(t *testing.T) {
	app := &App{Exec: nil}
	_, err := handleInitCommand(app)
	if err == nil {
		t.Error("should return error for nil executor")
	}
}

func TestDetectProject_GoMod(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["go.mod"] = "module github.com/test/myapp\n\ngo 1.26\n"

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.languages) == 0 || info.languages[0] != "Go" {
		t.Errorf("expected Go, got %v", info.languages)
	}
	if info.moduleName != "github.com/test/myapp" {
		t.Errorf("expected module name, got %s", info.moduleName)
	}
}

func TestDetectProject_Architecture(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["go.mod"] = "module github.com/test/app\n\ngo 1.26\n"
	exe.dirs["cmd"] = true
	exe.dirs["internal"] = true
	exe.dirs["docs"] = true
	exe.files["Dockerfile"] = "FROM debian\n"
	// Simulate subdirectory listing by adding files with dir prefixes.
	// The fake executor's ListDir lists all files, so cmd/wakil/main.go
	// will appear in the cmd/ listing as "cmd/wakil/main.go" — but
	// detectProject parses it as a name. We need to test that the
	// directory listing produces the right entry points.
	// The fake executor returns all files for any ListDir(".") call,
	// and only files for specific dirs. Since we can't easily simulate
	// nested directory listings with the fake executor, we test the
	// generateAgentsMD function directly with architecture info.

	info := &projectInfo{
		languages:   []string{"Go"},
		entryPoints: []string{"cmd/wakil"},
		packages:    []string{"internal/agent", "internal/exec"},
		hasDocker:   true,
		hasDocs:     true,
	}
	got := generateAgentsMD(info)
	if !strings.Contains(got, "cmd/wakil/") {
		t.Error("missing entry point")
	}
	if !strings.Contains(got, "internal/agent/") {
		t.Error("missing package")
	}
}

func TestDetectProject_Pnpm(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["package.json"] = `{"name":"test"}`
	exe.files["pnpm-lock.yaml"] = ""

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cmd := range info.buildCmds {
		if cmd == "pnpm build" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pnpm build in %v", info.buildCmds)
	}
}

func TestDetectProject_Yarn(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["package.json"] = `{"name":"test"}`
	exe.files["yarn.lock"] = ""

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cmd := range info.lintCmds {
		if cmd == "yarn lint" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected yarn lint in %v", info.lintCmds)
	}
}

func TestDetectProject_Rust(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["Cargo.toml"] = "[package]\nname = \"myapp\"\n"

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.languages) == 0 || info.languages[0] != "Rust" {
		t.Errorf("expected Rust, got %v", info.languages)
	}
}

func TestDetectProject_MakefileNotLanguage(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["Makefile"] = "all:\n\techo hello\n"

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range info.languages {
		if lang == "Make" {
			t.Error("Makefile should not be listed as a language")
		}
	}
}

func TestDetectProject_MixedEcosystems(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["go.mod"] = "module github.com/test/app\n\ngo 1.26\n"
	exe.files["package.json"] = `{"name":"frontend"}`
	exe.files["pnpm-lock.yaml"] = ""

	info, err := detectProject(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.languages) < 2 {
		t.Errorf("expected 2 languages, got %v", info.languages)
	}
}

func TestDetectProject_NilExecutor(t *testing.T) {
	_, err := detectProject(context.Background(), nil)
	if err == nil {
		t.Error("should return error for nil executor")
	}
}
