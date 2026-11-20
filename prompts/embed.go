// Package prompts embeds the default agent operating instructions so the wakil
// binary is self-contained. Three embedded prompts are available:
//
//   - EmbeddedAgentPrompt — the full prompt from prompts/agent.txt. Used as the
//     fallback when no agent_prompt_path is configured or the configured file
//     cannot be read. Also used when /prompt full is active (the default).
//
//   - EmbeddedCorePrompt — the core-only prompt from prompts/core.txt. Used when
//     /prompt core is active. Contains identity, safety gates, basic protocol,
//     and honesty invariants — enough for simple chit-chat turns.
//
//   - EmbeddedExtensionPrompt — the workflow extension from prompts/extension.txt.
//     Lazy-loaded as a second system message when /prompt core is active and the
//     classifier detects a task that needs the full workflow guidance.
//
// Precedence at runtime (see wiring/bootstrap.go loadAgentPrompt):
//  1. cfg.AgentPromptPath file (if readable) — user override wins. The custom
//     file replaces the whole prompt in both full and core mode: no embedded
//     core or extension is loaded, so the custom file is the only system prompt.
//  2. Embedded prompts — the split prompts baked into the binary.
//  3. (No further fallback needed — the embeds are always present at build time.)
package prompts

import _ "embed"

//go:embed agent.txt
var EmbeddedAgentPrompt string

// PromptVersion is the current version of the embedded agent prompt. It
// matches the "# prompt-version: <n>" first line of agent.txt. When a user's
// override file has no version line or a lower one, a warning is printed at
// start-up and in /prompt. The version line is stripped before the prompt is
// used (both embedded and override).
const PromptVersion = 1

//go:embed core.txt
var EmbeddedCorePrompt string

//go:embed extension.txt
var EmbeddedExtensionPrompt string
