package verify

// detect.go: project-type → verification command detection from manifest files.
//
// Detection is a FALLBACK. Explicit config (Config.Verify) always wins. When
// config is empty, DetectCommands infers commands from the presence of
// well-known manifest files in the project root. Detection is conservative:
// it only returns commands that are safe defaults for the detected ecosystem.
//
// Known limitations (inherent to file-presence detection):
//   - Monorepos with multiple manifests are not handled (root wins).
//   - package.json may have no "test" script; npm test then exits nonzero
//     with "no test specified" — a false verification failure. We include
//     it anyway because the user can override via config.
//   - Some test runners default to watch mode; the agent layer must enforce
//     a timeout to prevent hanging.
//   - Detection cannot know if tests require services, network, GPU, etc.
//
// These are acceptable for a fallback; explicit config is the escape hatch.

// DetectCommands infers verification commands from a list of project root
// filenames. The list need not be exhaustive — detection checks for the
// presence of specific manifest files and returns commands for each match.
// Multiple ecosystems can be detected (e.g. a repo with both go.mod and
// package.json); commands are returned in a stable, deterministic order.
//
// If no manifests are recognized, an empty slice is returned — the caller
// treats this as "no commands detected" (status skipped, never a silent pass).
func DetectCommands(files []string) []Command {
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	var cmds []Command

	// Go: go.mod present → test + vet.
	if fileSet["go.mod"] {
		cmds = append(cmds,
			Command{Cmd: "go test ./...", Source: "detect:go.mod"},
			Command{Cmd: "go vet ./...", Source: "detect:go.mod"},
		)
	}

	// Node.js: package.json present → npm test.
	// We include npm test as the primary check. npm run lint is NOT included
	// by default because it exits nonzero when no lint script exists, causing
	// false verification failures. Users can add it via config if desired.
	if fileSet["package.json"] {
		cmds = append(cmds,
			Command{Cmd: "npm test", Source: "detect:package.json"},
		)
	}

	// Rust: Cargo.toml present → cargo test.
	if fileSet["Cargo.toml"] {
		cmds = append(cmds,
			Command{Cmd: "cargo test", Source: "detect:Cargo.toml"},
		)
	}

	// Python: pyproject.toml → pytest (the modern standard).
	if fileSet["pyproject.toml"] {
		cmds = append(cmds,
			Command{Cmd: "pytest", Source: "detect:pyproject.toml"},
		)
	}

	return cmds
}
