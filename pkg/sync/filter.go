package sync

import (
	"regexp"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

type RemoteSafetyViolation struct {
	Field   string
	Pattern string
	Value   string
	Reason  string
}

type RemoteSafetyResult struct {
	Content    string
	Allowed    bool
	Blocked    bool
	Violations []RemoteSafetyViolation
	// Rewritten is true when Content was modified (paths or secrets redacted).
	// A memory can be both Rewritten and Blocked — rewriting reduces sensitive
	// content but other violations may still block sync.
	Rewritten bool
}

var (
	linuxHomeRe    = regexp.MustCompile(`/home/[^/\s]+(?:/[\S]*)?`)
	macOSHomeRe    = regexp.MustCompile(`/Users/[^/\s]+(?:/[\S]*)?`)
	winHomeRe      = regexp.MustCompile(`[A-Za-z]:\\Users\\[^\\/\s]+(?:\\[^\s]*)?`)
	tildeHomeRe    = regexp.MustCompile(`~/[\S]+`)
	tmpPathsRe     = regexp.MustCompile(`(?:/tmp|/var/folders|/private/tmp)(?:/[\S]*)?`)
	awsAccessKeyRe = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	googleAPIKeyRe = regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)
	credURIRe      = regexp.MustCompile(`(?i)\b(mongodb|postgres|postgresql|mysql|redis)(\+srv)?:\/\/[^\s/@]*:[^\s/@]+@`)
)

var knownSecretPrefixes = []struct {
	needle string
	label  string
}{
	{"sk_live_", "stripe live key"},
	{"sk_test_", "stripe test key"},
	{"rk_live_", "stripe restricted key"},
	{"ghp_", "github personal token"},
	{"gho_", "github oauth token"},
	{"ghu_", "github user token"},
	{"ghs_", "github server token"},
	{"xoxb-", "slack bot token"},
	{"xoxp-", "slack user token"},
	{"hooks.slack.com/services/T", "slack webhook"},
	{"AMAZONS3ACCESSKEY", "aws s3 literal"},
	{"-----BEGIN PRIVATE KEY-----", "pem private key"},
	{"-----BEGIN RSA PRIVATE KEY-----", "pem rsa private key"},
}

const maxViolationValue = 50

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func RemoteSafetyFilter(content string, metadata map[string]any, projectRoot string) RemoteSafetyResult {
	result := RemoteSafetyResult{
		Content: content,
		Allowed: true,
	}

	result.Content, result.Violations = detectLocalPaths(content, projectRoot, result.Violations)

	secretContent, secretViolations := detectSecrets(result.Content)
	if len(secretViolations) > 0 {
		result.Violations = append(result.Violations, secretViolations...)
		result.Content = secretContent
	}

	if metadata != nil {
		result.Violations = detectPersonalPreference(metadata, result.Violations)
	}

	if result.Content != content {
		result.Rewritten = true
	}

	if len(result.Violations) > 0 {
		result.Blocked = true
		result.Allowed = false
	}

	return result
}

// SafeTriplesForRemote returns the triples that may leave the machine and how
// many it held back. Subject, predicate and object are free text the model
// writes, so each goes through RemoteSafetyFilter as a memory's content does:
// a secret, or a local path outside projectRoot, holds the whole triple back,
// and a path inside projectRoot is sent relative to it.
func SafeTriplesForRemote(triples []SyncTriple, projectRoot string) ([]SyncTriple, int) {
	safe := make([]SyncTriple, 0, len(triples))
	for _, t := range triples {
		if out, ok := safeTriple(t, projectRoot); ok {
			safe = append(safe, out)
		}
	}
	return safe, len(triples) - len(safe)
}

func safeTriple(t SyncTriple, projectRoot string) (SyncTriple, bool) {
	for _, field := range []*string{&t.Subject, &t.Predicate, &t.Object} {
		r := RemoteSafetyFilter(*field, nil, projectRoot)
		if r.Blocked {
			return SyncTriple{}, false
		}
		*field = r.Content
	}
	return t, true
}

func detectLocalPaths(content string, projectRoot string, violations []RemoteSafetyViolation) (string, []RemoteSafetyViolation) {
	rewritten := content

	for _, re := range []*regexp.Regexp{linuxHomeRe, macOSHomeRe, winHomeRe, tildeHomeRe} {
		matches := re.FindAllString(rewritten, -1)
		for _, m := range matches {
			if projectRoot != "" && strings.HasPrefix(m, projectRoot) {
				continue
			}
			violations = append(violations, RemoteSafetyViolation{
				Field:   "content",
				Pattern: re.String(),
				Value:   truncate(m, maxViolationValue),
				Reason:  "local_path",
			})
		}
	}

	tmpMatches := tmpPathsRe.FindAllString(rewritten, -1)
	for _, m := range tmpMatches {
		violations = append(violations, RemoteSafetyViolation{
			Field:   "content",
			Pattern: tmpPathsRe.String(),
			Value:   truncate(m, maxViolationValue),
			Reason:  "local_path",
		})
	}

	if projectRoot != "" {
		newContent := strings.ReplaceAll(rewritten, projectRoot+"/", "./")
		newContent = strings.ReplaceAll(newContent, projectRoot, ".")
		if newContent != rewritten {
			rewritten = newContent
		}
	}

	return rewritten, violations
}

func detectSecrets(content string) (string, []RemoteSafetyViolation) {
	s := memory.NewSanitizer(config.SanitizerConfig{Enabled: true})
	sanitized := s.Sanitize(content)

	if label := knownSecretLabel(content); label != "" {
		if sanitized == content {
			sanitized = "[REDACTED]"
		}
		return sanitized, []RemoteSafetyViolation{
			{
				Field:   "content",
				Pattern: label,
				Value:   "secrets detected and redacted",
				Reason:  "secret_pattern",
			},
		}
	}

	if sanitized == content {
		return content, nil
	}

	return sanitized, []RemoteSafetyViolation{
		{
			Field:   "content",
			Pattern: "sanitizer_rules",
			Value:   "secrets detected and redacted",
			Reason:  "secret_pattern",
		},
	}
}

func knownSecretLabel(content string) string {
	for _, p := range knownSecretPrefixes {
		if strings.Contains(content, p.needle) {
			return p.label
		}
	}
	if awsAccessKeyRe.FindString(content) != "" {
		return "aws access key"
	}
	if googleAPIKeyRe.FindString(content) != "" {
		return "google api key"
	}
	if m := credURIRe.FindStringSubmatch(content); m != nil {
		return m[1] + ":// with credentials"
	}
	return ""
}

func detectPersonalPreference(metadata map[string]any, violations []RemoteSafetyViolation) []RemoteSafetyViolation {
	scope, ok := metadata["scope"].(string)
	if !ok || scope != "user" {
		return violations
	}
	return append(violations, RemoteSafetyViolation{
		Field:   "metadata.scope",
		Pattern: "scope=user",
		Value:   "user",
		Reason:  "personal_preference",
	})
}
