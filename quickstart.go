// Package wakil holds files from the repository root that the binary embeds.
package wakil

import _ "embed"

// QuickstartConfig is config.quickstart.json, the single source of the default
// OpenRouter model used by `wakil init` and the documentation.
//
//go:embed config.quickstart.json
var QuickstartConfig []byte
