package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
)

// BackendInfo describes one available backend as reported by the proxy.
type BackendInfo struct {
	Name     string
	External bool     // true = routes to an external/cloud provider
	Caps     []string // optional modality capabilities (e.g. ["chat","vision"])
}

const backendsTimeout = 5 * time.Second

// backendListJSON mirrors the /v1/ilm/backends response shape.
type backendListJSON struct {
	Backends []struct {
		Name     string   `json:"name"`
		External bool     `json:"external"`
		Caps     []string `json:"caps,omitempty"`
	} `json:"backends"`
}

// FetchBackendList attempts to retrieve the backend list from the proxy at
// /v1/ilm/backends. Returns (list, true) on success; (nil, false) when the
// endpoint is absent, returns an unexpected status, or cannot be parsed.
func FetchBackendList(ctx context.Context, httpc *http.Client, baseURL, auth string) ([]BackendInfo, bool) {
	base := strings.TrimRight(baseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, backendsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1/ilm/backends", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var parsed backendListJSON
	if json.Unmarshal(body, &parsed) != nil {
		return nil, false
	}
	if len(parsed.Backends) == 0 {
		return nil, false
	}
	out := make([]BackendInfo, len(parsed.Backends))
	for i, b := range parsed.Backends {
		out[i] = BackendInfo{Name: b.Name, External: b.External, Caps: b.Caps}
	}
	return out, true
}

// FetchBackendListWithFallback fetches /v1/ilm/backends from the proxy. On
// success it logs the names to out. On failure it builds a synthetic list from
// cfg.ExternalBackends (all marked external) and logs a note. Either way the
// returned slice is safe to use for IsExternalBackend checks; nil means both
// the endpoint and the config list were empty (treat all backends as local).
//
// kind=openai endpoints have no /v1/ilm/backends — the call is skipped
// entirely (no request fired) and the config-list fallback applies directly,
// which is the same place a failed fetch already lands.
func FetchBackendListWithFallback(ctx context.Context, httpc *http.Client, cfg config.Config, out io.Writer) []BackendInfo {
	if cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI {
		if len(cfg.ExternalBackends) == 0 {
			return nil
		}
		out2 := make([]BackendInfo, len(cfg.ExternalBackends))
		for i, name := range cfg.ExternalBackends {
			out2[i] = BackendInfo{Name: name, External: true}
		}
		return out2
	}
	list, ok := FetchBackendList(ctx, httpc, cfg.BaseURL, cfg.AuthHeader())
	if ok {
		var names []string
		for _, b := range list {
			names = append(names, b.Name)
		}
		fmt.Fprintf(out, "backends: %d available (%s)\n", len(list), strings.Join(names, ", "))
		return list
	}
	// Fallback: use the config list (all flagged external by definition).
	if len(cfg.ExternalBackends) == 0 {
		return nil
	}
	fmt.Fprintf(out, "backends: /v1/ilm/backends unavailable — using config external_backends list (%d)\n",
		len(cfg.ExternalBackends))
	out2 := make([]BackendInfo, len(cfg.ExternalBackends))
	for i, name := range cfg.ExternalBackends {
		out2[i] = BackendInfo{Name: name, External: true}
	}
	return out2
}

// modelListJSON mirrors the /v1/ilm/models response shape.
type modelListJSON struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// FetchModelList attempts to retrieve the model list from the proxy at
// /v1/ilm/models. Returns (names, true) on success; (nil, false) when the
// endpoint is absent, returns an unexpected status, or cannot be parsed.
func FetchModelList(ctx context.Context, httpc *http.Client, baseURL, auth string) ([]string, bool) {
	base := strings.TrimRight(baseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, backendsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1/ilm/models", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var parsed modelListJSON
	if json.Unmarshal(body, &parsed) != nil {
		return nil, false
	}
	if len(parsed.Models) == 0 {
		return nil, false
	}
	out := make([]string, len(parsed.Models))
	for i, m := range parsed.Models {
		out[i] = m.Name
	}
	return out, true
}

// openAIModelListJSON mirrors the standard OpenAI /v1/models response shape
// (served by llama.cpp server and OpenRouter alike).
type openAIModelListJSON struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchModelListForEndpoint returns the model-name list appropriate to the
// active endpoint's kind: ilm-proxy → /v1/ilm/models (unchanged); openai →
// best-effort GET {base_url}/v1/models (OpenAI standard), silently empty on
// any failure — a bare server without the route just yields no completion.
func FetchModelListForEndpoint(ctx context.Context, httpc *http.Client, cfg config.Config) []string {
	ep := cfg.ActiveEndpoint()
	if ep.Kind == config.EndpointKindOpenAI {
		names, _ := fetchOpenAIModelList(ctx, httpc, ep.BaseURL, cfg.AuthHeader())
		return names
	}
	names, _ := FetchModelList(ctx, httpc, cfg.BaseURL, cfg.AuthHeader())
	return names
}

// fetchOpenAIModelList fetches the standard /v1/models list. Same tolerance
// contract as FetchModelList: (nil, false) on any failure.
func fetchOpenAIModelList(ctx context.Context, httpc *http.Client, baseURL, auth string) ([]string, bool) {
	base := strings.TrimRight(baseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, backendsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var parsed openAIModelListJSON
	if json.Unmarshal(body, &parsed) != nil {
		return nil, false
	}
	if len(parsed.Data) == 0 {
		return nil, false
	}
	out := make([]string, len(parsed.Data))
	for i, m := range parsed.Data {
		out[i] = m.ID
	}
	return out, true
}

// ResolveSubagentBackend returns the backend name to use for a subagent
// dispatch, given the parent session's current SelectedBackend and the
// subagent_backend config setting:
//
//   - "inherit" (or ""): use the parent's current backend (least surprise —
//     same cost tier as main unless the user explicitly changes it)
//   - "default": empty string → proxy default, no X-Ilm-Backend header
//   - any other value: that literal name, pinned (e.g. "llama" for the
//     cheap-labor pattern — heavy reasoning external, sub-tasks local)
func ResolveSubagentBackend(parentBackend, cfgSetting string) string {
	switch cfgSetting {
	case "inherit", "":
		return parentBackend
	case "default":
		return ""
	default:
		return cfgSetting
	}
}

// IsExternalBackend reports whether name is known to be an external backend.
// It checks the proxy-fetched list first, then cfg.ExternalBackends. When both
// are empty (no backend-list endpoint and no config list) it returns false —
// treat unknown backends as local for a safe default.
func IsExternalBackend(list []BackendInfo, cfg config.Config, name string) bool {
	if name == "" {
		return false
	}
	for _, b := range list {
		if b.Name == name {
			return b.External
		}
	}
	// Fall back to the config list.
	for _, ext := range cfg.ExternalBackends {
		if ext == name {
			return true
		}
	}
	return false
}
