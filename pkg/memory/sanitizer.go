package memory

import (
	"regexp"
)

type Sanitizer struct {
	patterns []*regexp.Regexp
}

func NewSanitizer(enabled bool) *Sanitizer {
	if !enabled {
		return &Sanitizer{}
	}
	return &Sanitizer{patterns: defaultPatterns()}
}

func (s *Sanitizer) Sanitize(text string) string {
	if len(s.patterns) == 0 {
		return text
	}
	for _, p := range s.patterns {
		text = p.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

func defaultPatterns() []*regexp.Regexp {
	patterns := []string{
		`(?i)(sk|pk|key|token|secret|password|passwd|api[_-]?key|access[_-]?token|refresh[_-]?token|private[_-]?key|auth[_-]?token)\s*[:=]\s*['"]?[a-zA-Z0-9_\-./+=]{8,}['"]?`,
		`-----BEGIN\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----[\s\S]*?-----END\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----`,
		`-----BEGIN\s+SSH\s+PRIVATE\s+KEY-----[\s\S]*?-----END\s+SSH\s+PRIVATE\s+KEY-----`,
		`(?i)AKIA[0-9A-Z]{16}`,
		`(?i)ghp_[a-zA-Z0-9]{36}`,
		`(?i)gho_[a-zA-Z0-9]{36}`,
		`(?i)ghu_[a-zA-Z0-9]{36}`,
		`(?i)ghs_[a-zA-Z0-9]{36}`,
		`(?i)xox[baprs]-[0-9a-z\-]{10,}`,
		`(?i)eyJ[a-zA-Z0-9_-]*\.eyJ[a-zA-Z0-9_-]*\.[a-zA-Z0-9_-]*`,
		`(?i)bearer\s+[a-zA-Z0-9_\-.]+`,
	}

	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err == nil {
			compiled = append(compiled, re)
		}
	}
	return compiled
}
