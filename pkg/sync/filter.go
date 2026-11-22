package sync

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
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

// A path ends at whitespace or at a ':', ',' or ';', which join paths in a
// list (PATH=/a:/b), so each path of a list is checked on its own.
var (
	linuxHomeRe = regexp.MustCompile(`/home/[^/\s:,;]+(?:/[^\s:,;]*)?`)
	macOSHomeRe = regexp.MustCompile(`/Users/[^/\s:,;]+(?:/[^\s:,;]*)?`)
	winHomeRe   = regexp.MustCompile(`[A-Za-z]:\\Users\\[^\\/\s:,;]+(?:\\[^\s:,;]*)?`)
	tildeHomeRe = regexp.MustCompile(`~/[^\s:,;]+`)
	// tmpPathsRe and rootHomeRe match only a whole first segment where a path
	// starts (the start of the text, a URL's "//", or a character a path
	// cannot hold), so a folder named tmp or root inside another path, or
	// /rootfs, is not taken for them. The path is group 1.
	tmpPathsRe     = regexp.MustCompile(`(?:^|//|[^\w./~-])((?:/tmp|/var/folders|/private/tmp)(?:/[^\s:,;]*)?)(?:$|[^\w.-])`)
	rootHomeRe     = regexp.MustCompile(`(?:^|//|[^\w./~-])((?:/root|/var/root)(?:/[^\s:,;]*)?)(?:$|[^\w.-])`)
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
// and a path inside projectRoot is sent relative to it. The triple is also
// read as the sentences it states, since a secret rule may need the name in
// the predicate and the value in the object ("db password: X").
func SafeTriplesForRemote(triples []SyncTriple, projectRoot string) ([]SyncTriple, int) {
	safe, withheld := filterTriples(triples, projectRoot)
	return safe, len(withheld)
}

// filterTriples is SafeTriplesForRemote that says, for each triple held back,
// which one and why ("triple 2 blocked: secret_pattern"), numbered from 1 in
// the order given.
func filterTriples(triples []SyncTriple, projectRoot string) ([]SyncTriple, []string) {
	safe := make([]SyncTriple, 0, len(triples))
	var withheld []string
	for i, t := range triples {
		out, reason := SafeTriple(t, projectRoot)
		if reason != "" {
			withheld = append(withheld, fmt.Sprintf("triple %d (%s %s) blocked: %s",
				i+1, tripleLabel(t.Subject, projectRoot), tripleLabel(t.Predicate, projectRoot), reason))
			continue
		}
		safe = append(safe, out)
	}
	return safe, withheld
}

// tripleLabel names a triple's field in the withheld list: as written when it
// passes the filter alone, so no secret or path is echoed, and cut short.
func tripleLabel(field, projectRoot string) string {
	if RemoteSafetyFilter(field, nil, projectRoot).Blocked {
		return "[withheld]"
	}
	return truncate(field, 40)
}

// SafeTriple is SafeTriplesForRemote for one triple: the triple to send, or
// the reason it stays local ("" when it may go).
func SafeTriple(t SyncTriple, projectRoot string) (SyncTriple, string) {
	for _, sentence := range []string{
		t.Subject + " " + t.Predicate + ": " + t.Object,
		t.Predicate + ": " + t.Object,
		t.Subject + ": " + t.Object,
	} {
		if r := RemoteSafetyFilter(sentence, nil, projectRoot); r.Blocked {
			return SyncTriple{}, violationReason(r.Violations)
		}
	}
	for _, field := range []*string{&t.Subject, &t.Predicate, &t.Object} {
		r := RemoteSafetyFilter(*field, nil, projectRoot)
		if r.Blocked {
			return SyncTriple{}, violationReason(r.Violations)
		}
		*field = r.Content
	}
	return t, ""
}

// TripleFromKG is the wire form of a local knowledge-graph triple.
func TripleFromKG(t kg.Triple) SyncTriple {
	return SyncTriple{Subject: t.Subject, Predicate: t.Predicate, Object: t.Object, Confidence: t.Confidence}
}

// TriplesFromKG is TripleFromKG for a list.
func TriplesFromKG(triples []kg.Triple) []SyncTriple {
	out := make([]SyncTriple, len(triples))
	for i, t := range triples {
		out[i] = TripleFromKG(t)
	}
	return out
}

func detectLocalPaths(content string, projectRoot string, violations []RemoteSafetyViolation) (string, []RemoteSafetyViolation) {
	rewritten := content
	projectRoot = trimRoot(projectRoot)

	for _, re := range []*regexp.Regexp{linuxHomeRe, macOSHomeRe, winHomeRe, tildeHomeRe} {
		matches := re.FindAllString(rewritten, -1)
		for _, m := range matches {
			if underRoot(m, projectRoot) {
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

	for _, re := range []*regexp.Regexp{tmpPathsRe, rootHomeRe} {
		for _, groups := range re.FindAllStringSubmatch(rewritten, -1) {
			if underRoot(groups[1], projectRoot) {
				continue
			}
			violations = append(violations, RemoteSafetyViolation{
				Field:   "content",
				Pattern: re.String(),
				Value:   truncate(groups[1], maxViolationValue),
				Reason:  "local_path",
			})
		}
	}

	if projectRoot != "" {
		rewritten = relativizeRoot(rewritten, projectRoot)
	}

	return rewritten, violations
}

// trimRoot drops a project root's trailing separators; a root that is only
// separators (the filesystem root) counts as no root, so nothing is inside it.
func trimRoot(root string) string {
	return strings.TrimRight(root, `/\`)
}

// underRoot reports whether a path found in text is root or inside it. The
// punctuation that ends a sentence is not part of the path, and a slash path
// is cleaned first, so /home/dev/proj/../other is outside. A sibling that only
// shares root's prefix, such as /home/dev/proj-private beside /home/dev/proj,
// is outside too.
func underRoot(p, root string) bool {
	if root == "" {
		return false
	}
	p = strings.TrimRight(p, ".,;:!?)]}>'\"`")
	// As written, so the rewrite to "./" finds the root in it...
	if !startsWithRoot(p, root) {
		return false
	}
	// ...and once cleaned, so it does not climb back out with "..".
	return !strings.HasPrefix(p, "/") || startsWithRoot(path.Clean(p), root)
}

func startsWithRoot(p, root string) bool {
	if !strings.HasPrefix(p, root) {
		return false
	}
	rest := p[len(root):]
	return rest == "" || rest[0] == '/' || rest[0] == '\\'
}

// relativizeRoot rewrites root, where it ends a path segment, as "." so a path
// inside the project is sent relative to it. A longer name that only starts
// with root is a sibling and stays as written.
func relativizeRoot(content, root string) string {
	var b strings.Builder
	for {
		i := strings.Index(content, root)
		if i < 0 {
			b.WriteString(content)
			return b.String()
		}
		b.WriteString(content[:i])
		content = content[i+len(root):]
		if continuesName(content) {
			b.WriteString(root)
		} else {
			b.WriteString(".")
		}
	}
}

// continuesName reports whether rest carries on the path segment the root
// ended in. A period does only when a name byte follows it: "proj.git" is
// another name, "proj." ends a sentence.
func continuesName(rest string) bool {
	if rest == "" || !isPathNameByte(rest[0]) {
		return false
	}
	return rest[0] != '.' || (len(rest) > 1 && isPathNameByte(rest[1]))
}

func isPathNameByte(c byte) bool {
	return c == '.' || c == '-' || c == '_' ||
		('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// remoteSanitizer holds the secret rules the remote safety filter applies:
// the built-in ones plus the user's own sanitizer patterns, which apply here
// even when the local sanitizer is off, since what leaves the machine must
// never be held to less than what is kept on it.
var remoteSanitizer atomic.Pointer[memory.Sanitizer]

func init() {
	UseSecretPatterns(nil)
}

// UseSecretPatterns sets the user's own secret patterns (config
// sanitizer.patterns) on top of the built-in rules for everything the remote
// safety filter checks: memories, triples and pushes. A process that sends
// to a remote calls it once its config is loaded.
func UseSecretPatterns(patterns []string) {
	remoteSanitizer.Store(memory.NewSanitizer(config.SanitizerConfig{Enabled: true, Patterns: patterns}))
}

func detectSecrets(content string) (string, []RemoteSafetyViolation) {
	sanitized := remoteSanitizer.Load().Sanitize(content)

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
