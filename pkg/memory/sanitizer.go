package memory

import (
	"log/slog"
	"regexp"
)

// redactionRule holds a regex and its replacement template. Use $1, $2 etc. to preserve captured groups.
type redactionRule struct {
	pattern    *regexp.Regexp
	replacement string
}

type Sanitizer struct {
	rules  []redactionRule
	logger *slog.Logger
}

func NewSanitizer(enabled bool) *Sanitizer {
	return &Sanitizer{
		rules:  defaultRules(enabled),
		logger: slog.Default(),
	}
}

func (s *Sanitizer) Sanitize(text string) string {
	if len(s.rules) == 0 {
		return text
	}
	count := 0
	for _, r := range s.rules {
		before := text
		text = r.pattern.ReplaceAllString(text, r.replacement)
		if text != before {
			n := r.pattern.FindAllStringIndex(before, -1)
			count += len(n)
		}
	}
	if count > 0 {
		s.logger.Debug("sanitizer: redacted patterns", "count", count)
	}
	return text
}

func defaultRules(enabled bool) []redactionRule {
	if !enabled {
		return nil
	}

	type ruleDef struct {
		pattern    string
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

	rules := make([]redactionRule, 0, len(defs))
	for _, d := range defs {
		re, err := regexp.Compile(d.pattern)
		if err == nil {
			rules = append(rules, redactionRule{pattern: re, replacement: d.replacement})
		}
	}
	return rules
}
