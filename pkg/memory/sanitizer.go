package memory

import (
	"log/slog"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/redact"
)

type Sanitizer struct {
	rules  []redact.Rule
	logger *slog.Logger
}

func NewSanitizer(cfg config.SanitizerConfig) *Sanitizer {
	return &Sanitizer{
		rules:  buildRules(cfg),
		logger: slog.Default(),
	}
}

func (s *Sanitizer) Sanitize(text string) string {
	if len(s.rules) == 0 {
		return text
	}
	text, count := redact.Apply(text, s.rules)
	if count > 0 {
		s.logger.Debug("sanitizer: redacted patterns", "count", count)
	}
	return text
}

// buildRules returns the shared built-in rules plus the user's custom
// patterns, or none when the sanitizer is disabled.
func buildRules(cfg config.SanitizerConfig) []redact.Rule {
	if !cfg.Enabled {
		return nil
	}
	rules := redact.Builtin()
	custom, errs := redact.Compile(cfg.Patterns)
	for _, err := range errs {
		slog.Warn("sanitizer: skipping invalid custom pattern", "error", err)
	}
	return append(rules, custom...)
}
