// Package redact removes credential-shaped substrings from text before it is
// stored, logged or shown. The built-in rules are the ones the memory
// sanitizer has always used, plus the key formats seen leaking in practice
// (Anthropic and OpenAI project keys, anchored remote keys), so every caller
// that writes user text somewhere durable can share one definition.
package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// Rule is one pattern and its replacement template ($1, $2 keep groups).
type Rule struct {
	pattern     *regexp.Regexp
	replacement string
	// custom marks user-configured patterns. Built-in rules skip ENV_VAR-style
	// placeholder names (false positives); custom patterns are explicit user
	// intent and always redact, placeholder-shaped or not.
	custom bool
	// strict marks provider formats with a distinctive prefix: they skip the
	// placeholder guard (a real key can end in an uppercase segment) and are
	// checked by valid instead.
	strict bool
	valid  func(match string) bool
	// hints are lowercase substrings one of which any match contains; a rule
	// whose hints are all absent from the text is skipped without running
	// its regex.
	hints []string
}

type ruleDef struct {
	pattern     string
	replacement string
	strict      bool
	valid       func(string) bool
	hints       []string
}

// hasDigit rejects prefixed lookalikes such as package names
// (sk-proj-management-dashboard): generated keys always carry digits.
func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// secretValue accepts the value of a password/secret assignment only when it
// looks like one: a digit, or 16+ characters (a passphrase). Plain words after
// a name that merely contains "secret" ("SECRETOAUTHCODE": connection) are
// not credentials.
func secretValue(match string) bool {
	i := strings.LastIndexAny(match, ":=")
	if i < 0 {
		return true
	}
	v := strings.Trim(match[i+1:], ` "'\`)
	return hasDigit(v) || len(v) >= 16
}

// keyName is a field name that starts with one of the given words (token,
// token_secret) followed by an assignment in env, YAML or JSON form, quoted or
// escaped: \"access_token\":\"…\". The name's own prefix (access_, DB_)
// stays in the text untouched, so the match can start at the keyword, which
// keeps the scan cheap.
func keyName(words string) string {
	return `(?i)((?:` + words + `)[a-z0-9_\-]*)\\?["']?\s*[:=]\s*\\?["']?`
}

var builtinDefs = []ruleDef{
	{pattern: keyName(`api[_-]?key|apikey|access[_-]?key`) + `[a-zA-Z0-9_\-./+=]{20,}\\?["']?`, replacement: `$1=[REDACTED]`, hints: []string{"key"}},
	{pattern: keyName(`token|bearer`) + `[a-zA-Z0-9_\-./+=]{20,}\\?["']?`, replacement: `$1=[REDACTED]`, hints: []string{"token", "bearer"}},
	{pattern: keyName(`password|passwd|pwd`) + `[^'"\s\\,}]{8,}\\?["']?`, replacement: `$1=[REDACTED]`, valid: secretValue, hints: []string{"pass", "pwd"}},
	{pattern: keyName(`secret|credential`) + `[^'"\s\\,}]{8,}\\?["']?`, replacement: `$1=[REDACTED]`, valid: secretValue, hints: []string{"secret", "credential"}},
	{pattern: `-----BEGIN\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----[\s\S]*?-----END\s+(RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----`, replacement: `[REDACTED]`, hints: []string{"private"}},
	{pattern: `eyJ[a-zA-Z0-9_-]*\.eyJ[a-zA-Z0-9_-]*\.[a-zA-Z0-9_-]*`, replacement: `[REDACTED]`, hints: []string{"eyj"}},
	{pattern: `(?i)AKIA[0-9A-Z]{16}`, replacement: `[REDACTED]`, hints: []string{"akia"}},
	{pattern: `(?i)gh[pouscr]_[a-zA-Z0-9]{36}`, replacement: `[REDACTED]`, hints: []string{"gh"}},
	{pattern: `(?i)xox[baprs]-[0-9a-z\-]{10,}`, replacement: `[REDACTED]`, hints: []string{"xox"}},
	// Provider keys recognised by their prefix alone, with no "key=" before
	// them. The leading group keeps the character before the key, so an
	// underscore (ENV_sk-ant-…) does not hide it the way \b would.
	{pattern: `(^|[^A-Za-z0-9])sk-(?:ant|proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}`, replacement: `$1[REDACTED]`, strict: true, valid: hasDigit, hints: []string{"sk-"}},
	{pattern: `(^|[^A-Za-z0-9])sk-[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20}`, replacement: `$1[REDACTED]`, strict: true, hints: []string{"t3blbkfj"}},
	{pattern: `(^|[^A-Za-z0-9])anc_(?:live|test)_[A-Za-z0-9]{16,}`, replacement: `$1[REDACTED]`, strict: true, valid: hasDigit, hints: []string{"anc_"}},
	{pattern: `(^|[^A-Za-z0-9])(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}`, replacement: `$1[REDACTED]`, strict: true, valid: hasDigit, hints: []string{"_live_", "_test_"}},
	{pattern: `github_pat_[A-Za-z0-9_]{22,}`, replacement: `[REDACTED]`, strict: true, hints: []string{"github_pat_"}},
	{pattern: `glpat-[A-Za-z0-9_\-]{20}`, replacement: `[REDACTED]`, strict: true, hints: []string{"glpat-"}},
	{pattern: `AIza[0-9A-Za-z_\-]{35}`, replacement: `[REDACTED]`, strict: true, hints: []string{"aiza"}},
	{pattern: `(?i)(sk|pk|private[_-]?key)\s*[:=]\s*['"]?[a-zA-Z0-9_\-./+=]{8,}['"]?`, replacement: `$1=[REDACTED]`, hints: []string{"sk", "pk", "private"}},
	{pattern: `(?i)(authorization\s*:\s*basic)\s+[A-Za-z0-9+/]{8,}={0,2}`, replacement: `$1 [REDACTED]`, hints: []string{"basic"}},
	{pattern: `(?i)bearer\s+[a-zA-Z0-9_\-.+/=]{10,}`, replacement: `bearer [REDACTED]`, hints: []string{"bearer"}},
	// Credentials in a URL's userinfo, any scheme (https, rediss, amqp,
	// postgres…). The password part runs to the last @ before the path, so a
	// password containing @ is redacted whole.
	{pattern: `(?i)\b([a-z][a-z0-9+.\-]*)://[^\s/@:]*:[^\s/]+@`, replacement: `$1://[REDACTED]@`, hints: []string{"://"}},
}

var builtin = func() []Rule {
	rules := make([]Rule, 0, len(builtinDefs))
	for _, d := range builtinDefs {
		rules = append(rules, Rule{pattern: regexp.MustCompile(d.pattern), replacement: d.replacement, strict: d.strict, valid: d.valid, hints: d.hints})
	}
	return rules
}()

// Builtin returns the built-in rules.
func Builtin() []Rule { return append([]Rule(nil), builtin...) }

// Compile turns user patterns into rules that always redact to [REDACTED],
// returning an error for each pattern that does not compile.
func Compile(patterns []string) ([]Rule, []error) {
	var rules []Rule
	var errs []error
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("compile %q: %w", p, err))
			continue
		}
		rules = append(rules, Rule{pattern: re, replacement: "[REDACTED]", custom: true})
	}
	return rules, errs
}

// Apply runs rules over text and reports how many matches it redacted.
func Apply(text string, rules []Rule) (string, int) {
	count := 0
	lower := strings.ToLower(text)
	for _, r := range rules {
		rule := r
		if !anyHint(lower, rule.hints) {
			continue
		}
		text = rule.pattern.ReplaceAllStringFunc(text, func(match string) string {
			// Placeholder guard: documentation and examples reference secrets
			// by NAME ("Bearer CREDITS_WEBHOOK_BEARER", "token=MY_API_TOKEN").
			// Redacting the name protects nothing and destroys the text's
			// value, so ENV_VAR-shaped tokens pass through untouched. Prefixed
			// provider formats are validated on their own instead.
			if !rule.custom && !rule.strict && isPlaceholderMatch(match) {
				return match
			}
			if rule.valid != nil && !rule.valid(match) {
				return match
			}
			count++
			return rule.pattern.ReplaceAllString(match, rule.replacement)
		})
	}
	return text, count
}

func anyHint(lower string, hints []string) bool {
	if len(hints) == 0 {
		return true
	}
	for _, h := range hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// String applies the built-in rules.
func String(text string) string {
	out, _ := Apply(text, builtin)
	return out
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

// isPlaceholderMatch reports whether a match ends in an ENV_VAR-style
// placeholder name rather than an actual secret value.
func isPlaceholderMatch(match string) bool {
	tok := trailingTokenRe.FindString(match)
	return tok != "" && placeholderTokenRe.MatchString(tok)
}
