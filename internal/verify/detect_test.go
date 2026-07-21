package verify

import (
	"testing"
)

func TestDetectCommands_GoMod(t *testing.T) {
	cmds := DetectCommands([]string{"go.mod", "README.md"})
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands for go.mod, got %d", len(cmds))
	}
	if cmds[0].Cmd != "go test ./..." {
		t.Errorf("expected go test ./..., got %s", cmds[0].Cmd)
	}
	if cmds[1].Cmd != "go vet ./..." {
		t.Errorf("expected go vet ./..., got %s", cmds[1].Cmd)
	}
	for _, c := range cmds {
		if c.Source != "detect:go.mod" {
			t.Errorf("expected source detect:go.mod, got %s", c.Source)
		}
	}
}

func TestDetectCommands_PackageJSON(t *testing.T) {
	cmds := DetectCommands([]string{"package.json", "src"})
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command for package.json, got %d", len(cmds))
	}
	if cmds[0].Cmd != "npm test" {
		t.Errorf("expected npm test, got %s", cmds[0].Cmd)
	}
}

func TestDetectCommands_CargoToml(t *testing.T) {
	cmds := DetectCommands([]string{"Cargo.toml", "src"})
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command for Cargo.toml, got %d", len(cmds))
	}
	if cmds[0].Cmd != "cargo test" {
		t.Errorf("expected cargo test, got %s", cmds[0].Cmd)
	}
}

func TestDetectCommands_PyprojectToml(t *testing.T) {
	cmds := DetectCommands([]string{"pyproject.toml"})
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command for pyproject.toml, got %d", len(cmds))
	}
	if cmds[0].Cmd != "pytest" {
		t.Errorf("expected pytest, got %s", cmds[0].Cmd)
	}
}

func TestDetectCommands_MultipleEcosystems(t *testing.T) {
	cmds := DetectCommands([]string{"go.mod", "package.json"})
	if len(cmds) != 3 {
		t.Fatalf("expected 3 commands (go test, go vet, npm test), got %d", len(cmds))
	}
	// Stable order: Go first, then Node.
	if cmds[0].Cmd != "go test ./..." {
		t.Errorf("expected first command go test ./..., got %s", cmds[0].Cmd)
	}
	if cmds[2].Cmd != "npm test" {
		t.Errorf("expected third command npm test, got %s", cmds[2].Cmd)
	}
}

func TestDetectCommands_NoManifests(t *testing.T) {
	cmds := DetectCommands([]string{"README.md", "Makefile", "config.yml"})
	if len(cmds) != 0 {
		t.Fatalf("expected 0 commands for unknown files, got %d", len(cmds))
	}
}

func TestDetectCommands_EmptyInput(t *testing.T) {
	cmds := DetectCommands(nil)
	if len(cmds) != 0 {
		t.Fatalf("expected 0 commands for nil input, got %d", len(cmds))
	}
}

func TestDetectCommands_Deduplication(t *testing.T) {
	// If go.mod appears multiple times in the list, it should only be detected once.
	cmds := DetectCommands([]string{"go.mod", "go.mod", "README.md"})
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands (deduplicated go.mod), got %d", len(cmds))
	}
}
