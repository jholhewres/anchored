package memory

import (
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jholhewres/anchored/pkg/config"
)

// redactionRule holds a regex and its replacement template. Use $1, $2 etc. to preserve captured groups.
type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
	// custom marks user-configured patterns. Built-in rules skip ENV_VAR-style
	// placeholder names (false positives); custom patterns are explicit user
	// intent and always redact, placeholder-shaped or not.
	custom bool
}

type Sanitizer struct {
	rules  []redactionRule
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
	count := 0
	for _, r := range s.rules {
		rule := r
		text = rule.pattern.ReplaceAllStringFunc(text, func(match string) string {
			// Placeholder guard: documentation and examples reference secrets
			// by NAME ("Bearer CREDITS_WEBHOOK_BEARER", "token=MY_API_TOKEN").
			// Redacting the name protects nothing and destroys the memory's
			// value, so ENV_VAR-shaped tokens pass through untouched.
			if !rule.custom && isPlaceholderMatch(match) {
				return match
			}
			count++
			return rule.pattern.ReplaceAllString(match, rule.replacement)
		})
	}
	if count > 0 {
		s.logger.Debug("sanitizer: redacted patterns", "count", count)
	}
	return text
}

var (
	// placeholderTokenRe matches ENV_VAR-style names: uppercase words joined
	// by underscores, no digits. Real credentials (mixed case, hex, prefixed
	// keys like AKIA.../sk_live_...) never take this shape.
	placeholderTokenRe = regexp.MustCompile(`^[A-Z]+(?:_[A-Z]+)+$`)
	// trailingTokenRe captures the credential-shaped tail of a rule match —
	// the part the replacement would redact.
	trailingTokenRe = regexp.MustCompile(`[A-Za-z0-9_]+$`)
)

// isPlaceholderMatch reports whether a redaction-rule match ends in an
// ENV_VAR-style placeholder name rather than an actual secret value.
func isPlaceholderMatch(match string) bool {
	tok := trailingTokenRe.FindString(match)
	return tok != "" && placeholderTokenRe.MatchString(tok)
}

func buildRules(cfg config.SanitizerConfig) []redactionRule {
	if !cfg.Enabled {
		return nil
	}

	type ruleDef struct {
		pattern     string
		replacement string
	}
	defs := []ruleDef{
		{
			`(?i)(api[_-]?key|apikey|access[_-]?key)\s*[:=]\s*['"]?[a-zA-Z0-9_\-./+=]{20,}['"]?`,
			`$1=[REDACTED]`,
		},
		{
			`(?i)(token|bearer|auth[_-]?token|access[_-]?token|refresh[_-]?token)\s*[:=]\s*['"]?[a-zA-Z0-9_\-./+=]{20,}['"]?`,
			`$1=[REDACTED]`,
		},
		{
			`(?i)(password|passwd|pwd)\s*[:=]\s*['"]?[^'"\s]{8,}['"]?`,
			`$1=[REDACTED]`,
		},
		{
			`(?i)(secret|credential)\s*[:=]\s*['"]?[^'"\s]{8,}['"]?`,
			`$1=[REDACTED]`,
		},

		{
			`-----BEGIN\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----[\s\S]*?-----END\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----`,
			`[REDACTED]`,
		},

		{
			`eyJ[a-zA-Z0-9_-]*\.eyJ[a-zA-Z0-9_-]*\.[a-zA-Z0-9_-]*`,
			`[REDACTED]`,
		},

		{
			`(?i)AKIA[0-9A-Z]{16}`,
			`[REDACTED]`,
		},

		{
			`(?i)gh[pouscr]_[a-zA-Z0-9]{36}`,
			`[REDACTED]`,
		},

		{
			`(?i)xox[baprs]-[0-9a-z\-]{10,}`,
			`[REDACTED]`,
		},

		{
			`(?i)(sk|pk|private[_-]?key)\s*[:=]\s*['"]?[a-zA-Z0-9_\-./+=]{8,}['"]?`,
			`$1=[REDACTED]`,
		},

		{
			`(?i)bearer\s+[a-zA-Z0-9_\-.]{10,}`,
			`bearer [REDACTED]`,
		},

		{
			`(?i)((?:mongodb(?:\+srv)?|postgres(?:ql)?|mysql|redis))://[^@\s]*:[^@\s]+@`,
			`$1://[REDACTED]@`,
		},
	}

	rules := make([]redactionRule, 0, len(defs)+len(cfg.Patterns))
	for _, d := range defs {
		re, err := regexp.Compile(d.pattern)
		if err == nil {
			rules = append(rules, redactionRule{pattern: re, replacement: d.replacement})
		}
	}

	for _, p := range cfg.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			slog.Warn("sanitizer: skipping invalid custom pattern", "pattern", p, "error", fmt.Errorf("compile: %w", err))
			continue
		}
		rules = append(rules, redactionRule{pattern: re, replacement: "[REDACTED]", custom: true})
	}

	return rules
}
