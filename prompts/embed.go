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
//  1. cfg.AgentPromptPath file (if readable) — user override wins. When a custom
//     file is used, the split-prompt feature is bypassed (the full file is the
//     core; no extension is loaded). This preserves backward compatibility.
//  2. Embedded prompts — the split prompts baked into the binary.
//  3. (No further fallback needed — the embeds are always present at build time.)
package prompts

import _ "embed"

//go:embed agent.txt
var EmbeddedAgentPrompt string

//go:embed core.txt
var EmbeddedCorePrompt string

//go:embed extension.txt
var EmbeddedExtensionPrompt string
