// Package prompts embeds the default agent operating instructions so the wakil
// binary is self-contained. The embedded prompt is the full agent prompt from
// prompts/agent.txt — it is used as the fallback when no agent_prompt_path is
// configured or the configured file cannot be read.
//
// Precedence at runtime (see cmd/wakil/main.go loadAgentPrompt):
//  1. cfg.AgentPromptPath file (if readable) — user override wins.
//  2. EmbeddedAgentPrompt — the full prompt baked into the binary.
//  3. (No further fallback needed — the embed is always present at build time.)
package prompts

import _ "embed"

//go:embed agent.txt
var EmbeddedAgentPrompt string
