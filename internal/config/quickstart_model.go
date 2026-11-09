package config

import (
	"encoding/json"
	"fmt"

	wakil "github.com/treeol/wakil"
)

// QuickstartModel returns the default OpenRouter model from the embedded
// config.quickstart.json. `wakil init` and the docs share this one value.
func QuickstartModel() (string, error) {
	var q struct {
		Endpoints map[string]EndpointConfig `json:"endpoints"`
	}
	if err := json.Unmarshal(wakil.QuickstartConfig, &q); err != nil {
		return "", fmt.Errorf("embedded config.quickstart.json: %w", err)
	}
	ep, ok := q.Endpoints["openrouter"]
	if !ok || ep.Model == "" {
		return "", fmt.Errorf("embedded config.quickstart.json has no openrouter model")
	}
	return ep.Model, nil
}
