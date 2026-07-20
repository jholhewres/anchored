package main

import "strings"

// detectProvider infers the LLM provider and model behind the current session
// from environment variables, so the cockpit can tell apart e.g. Claude Code on
// Anthropic vs. Claude Code pointed at a GLM endpoint. It is pure (takes an env
// lookup) for testability. Returns ("","") when nothing recognizable is set.
//
// Precedence: an Anthropic base URL pointing at a known third-party gateway
// (z.ai / bigmodel / glm) wins as "glm"; otherwise any ANTHROPIC_* signal is
// "anthropic"; then OPENAI_* is "openai".
func detectProvider(env func(string) string) (provider, model string) {
	anthBase := env("ANTHROPIC_BASE_URL")
	anthModel := env("ANTHROPIC_MODEL")

	if anthBase != "" {
		low := strings.ToLower(anthBase)
		switch {
		case strings.Contains(low, "z.ai"), strings.Contains(low, "bigmodel"), strings.Contains(low, "glm"):
			m := anthModel
			if m == "" {
				m = "glm"
			}
			return "glm", m
		}
	}
	if anthBase != "" || anthModel != "" || env("ANTHROPIC_AUTH_TOKEN") != "" || env("ANTHROPIC_API_KEY") != "" {
		return "anthropic", anthModel
	}

	if env("OPENAI_BASE_URL") != "" || env("OPENAI_API_KEY") != "" || env("OPENAI_MODEL") != "" {
		return "openai", env("OPENAI_MODEL")
	}
	return "", ""
}
