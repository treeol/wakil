package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/treeol/wakil/internal/config"
)

// showRow is one effective value and where it came from.
type showRow struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"` // default | file | env | flag
}

// redactTail keeps the last 4 characters of a secret; anything shorter than 9
// characters is redacted entirely so a short secret is not mostly revealed.
func redactTail(s string) string {
	if s == "" {
		return ""
	}
	if len(s) < 9 {
		return "[redacted]"
	}
	return "…" + s[len(s)-4:]
}

// redactAuthHeader redacts an Authorization value fully.
func redactAuthHeader(s string) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == "--"+name || a == "-"+name || strings.HasPrefix(a, "--"+name+"=") || strings.HasPrefix(a, "-"+name+"=") {
			return true
		}
	}
	return false
}

func firstEnv(getenv func(string) string, names ...string) (string, bool) {
	for _, n := range names {
		if getenv(n) != "" {
			return n, true
		}
	}
	return "", false
}

// buildShowRows computes the effective config of argv (the flags after
// `config show` are not config flags; callers pass the flags to resolve with).
// It reads the config file but never writes anything.
func buildShowRows(argv []string, getenv func(string) string) ([]showRow, error) {
	cfg, err := config.LoadConfig(argv)
	if err != nil {
		return nil, err
	}
	path, explicit := config.ConfigPathFromArgs(argv)
	var file map[string]json.RawMessage
	if b, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(b, &file)
	}
	inFile := func(k string) bool { _, ok := file[k]; return ok }

	// Per-endpoint keys present in the file for the selected endpoint.
	var epFile map[string]json.RawMessage
	if raw, ok := file["endpoints"]; ok {
		var eps map[string]map[string]json.RawMessage
		if json.Unmarshal(raw, &eps) == nil {
			epFile = eps[cfg.EndpointName]
		}
	}
	epHas := func(k string) bool { _, ok := epFile[k]; return ok }

	var rows []showRow
	add := func(k, v, src string) { rows = append(rows, showRow{k, v, src}) }

	// config path
	switch {
	case explicit:
		add("config_path", path, "flag")
	case getenv("WAKIL_CONFIG") != "":
		add("config_path", path, "env")
	default:
		add("config_path", path, "default")
	}

	// endpoint
	switch {
	case inFile("default_endpoint"):
		add("endpoint", cfg.EndpointName, "file")
	case len(cfg.Endpoints) == 1:
		add("endpoint", cfg.EndpointName, "file")
	default:
		add("endpoint", cfg.EndpointName, "default")
	}
	kind := cfg.Endpoint.Kind
	src := "default"
	if epHas("kind") {
		src = "file"
	}
	add("endpoint.kind", kind, src)

	switch {
	case hasFlag(argv, "base-url"):
		add("endpoint.base_url", cfg.Endpoint.BaseURL, "flag")
	case envSet(getenv, "ILM_BASE_URL", "WAKIL_BASE_URL"):
		add("endpoint.base_url", cfg.Endpoint.BaseURL, "env")
	case epHas("base_url") || inFile("base_url"):
		add("endpoint.base_url", cfg.Endpoint.BaseURL, "file")
	default:
		add("endpoint.base_url", cfg.Endpoint.BaseURL, "default")
	}
	switch {
	case hasFlag(argv, "model"):
		add("endpoint.model", cfg.Endpoint.Model, "flag")
	case envSet(getenv, "ILM_MODEL", "WAKIL_MODEL"):
		add("endpoint.model", cfg.Endpoint.Model, "env")
	case epHas("model") || inFile("model"):
		add("endpoint.model", cfg.Endpoint.Model, "file")
	default:
		add("endpoint.model", cfg.Endpoint.Model, "default")
	}

	// credentials
	if cfg.Endpoint.APIKeyEnv != "" {
		set := "not set in this environment"
		if strings.TrimSpace(getenv(cfg.Endpoint.APIKeyEnv)) != "" {
			set = "set"
		}
		add("endpoint.api_key_env", cfg.Endpoint.APIKeyEnv+" ("+set+")", "file")
	}
	if cfg.Endpoint.AuthHeader != "" {
		add("endpoint.auth_header", redactAuthHeader(cfg.Endpoint.AuthHeader), "file")
	}
	if cfg.APIKey != "" {
		switch {
		case hasFlag(argv, "api-key"):
			add("api_key", redactTail(cfg.APIKey), "flag")
		case envSet(getenv, "ILM_API_KEY", "WAKIL_API_KEY"):
			add("api_key", redactTail(cfg.APIKey), "env")
		default:
			add("api_key", redactTail(cfg.APIKey), "file")
		}
	}

	// approval
	mode := "ask"
	if cfg.AutoApprove || cfg.Approval == "auto" {
		mode = "auto"
	}
	switch {
	case hasFlag(argv, "auto"):
		add("approval", mode, "flag")
	case inFile("approval"):
		add("approval", mode, "file")
	default:
		add("approval", mode, "default")
	}

	// sandbox
	backend := cfg.Sandbox.Backend
	bsrc := "default"
	switch {
	case hasFlag(argv, "sandbox") || hasFlag(argv, "exec"):
		bsrc = "flag"
	case envSet(getenv, "WAKIL_SANDBOX_BACKEND", "WAKIL_EXEC_MODE", "ILM_EXEC_MODE"):
		bsrc = "env"
	case epFileSandbox(file) || inFile("exec_mode"):
		bsrc = "file"
	}
	if cfg.Sandbox.BackendDefaulted {
		backend = "automatic (hisn when usable, else docker)"
	}
	add("sandbox.backend", backend, bsrc)

	return rows, nil
}

func epFileSandbox(file map[string]json.RawMessage) bool {
	raw, ok := file["sandbox"]
	if !ok {
		return false
	}
	var sb map[string]json.RawMessage
	if json.Unmarshal(raw, &sb) != nil {
		return false
	}
	_, ok = sb["backend"]
	return ok
}

func envSet(getenv func(string) string, names ...string) bool {
	_, ok := firstEnv(getenv, names...)
	return ok
}

// runConfigShow implements `wakil config show [--json] [config flags]`: the
// remaining arguments are the ones wakil itself accepts (--config, --model,
// --sandbox, ...), so the "flag" source is real. It returns the exit code.
// Nothing is written.
func runConfigShow(w, errw io.Writer, args []string, getenv func(string) string) int {
	asJSON := false
	var argv []string
	for _, a := range args {
		if a == "--json" || a == "-json" {
			asJSON = true
			continue
		}
		argv = append(argv, a)
	}
	rows, err := buildShowRows(argv, getenv)
	if err != nil {
		var nc *config.NoConfigError
		if errors.As(err, &nc) {
			fmt.Fprintln(errw, err)
			return 1
		}
		fmt.Fprintln(errw, "config error:", err)
		return 2
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		return 0
	}
	width := 0
	for _, r := range rows {
		if len(r.Key) > width {
			width = len(r.Key)
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%-*s  %s  [%s]\n", width, r.Key, r.Value, r.Source)
	}
	return 0
}
