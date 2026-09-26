package redact

import (
	"strings"
	"testing"
)

func TestString_RedactsCredentialShapes(t *testing.T) {
	secrets := map[string]string{
		"anthropic":   "use sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_ab for the eval",
		"openai-proj": "OPENAI key sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcd",
		"anchored":    "remote api_key anc_live_9f8e7d6c5b4a39281706f5e4d3c2b1a0",
		"aws":         "AKIAIOSFODNN7EXAMPLE was leaked",
		"github":      "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ pushed",
		"jwt":         "cookie eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"bearer":      "Authorization: Bearer abcdef0123456789abcdef",
		"password":    "password=hunter2hunter2",
		"conn":        "postgres://app:s3cr3tpw@db.internal:5432/app",
	}
	for name, in := range secrets {
		out := String(in)
		if !strings.Contains(out, "[REDACTED") {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
	if out := String("sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_ab"); strings.Contains(out, "AbCdEf") {
		t.Errorf("anthropic key still visible: %q", out)
	}
}

func TestString_LeavesOrdinaryTextAndPlaceholders(t *testing.T) {
	keep := []string{
		"the release gate needs tag equal to VERSION",
		"set token=MY_API_TOKEN in the env",
		"sk-learn is a python library",
		"the task-queue uses sk-style naming",
		"curl -H 'Authorization: Bearer CREDITS_WEBHOOK_BEARER'",
	}
	for _, in := range keep {
		if out := String(in); out != in {
			t.Errorf("ordinary text changed: %q -> %q", in, out)
		}
	}
}

func TestCompile_CustomPatternsAlwaysRedact(t *testing.T) {
	rules, errs := Compile([]string{`INTERNAL-[0-9]{4}`, `(`})
	if len(errs) != 1 {
		t.Fatalf("expected one invalid pattern error, got %v", errs)
	}
	out, n := Apply("ticket INTERNAL-1234 and INTERNAL_PLACEHOLDER", rules)
	if n != 1 || strings.Contains(out, "INTERNAL-1234") {
		t.Errorf("custom rule must redact: %q (n=%d)", out, n)
	}
}

func TestString_RedactsStructuredAndPrefixedCredentials(t *testing.T) {
	secrets := map[string]string{
		"json-key":        `{"api_key":"abcd1234efgh5678ijkl9012mnop"}`,
		"escaped-json":    `{\"access_token\":\"ya29a0AfH6SMBx1234567890abcdef\"}`,
		"env-quoted":      `API_KEY="abcdefghij0123456789klmn"`,
		"secret-key-name": `SECRET_KEY=abcd1234efgh5678`,
		"client-secret":   `client_secret: q8W2e4R6t8Y0u2I4`,
		"https-userinfo":  `clone https://user:hunter2pass@example.com/repo.git`,
		"rediss":          `rediss://:pw12345678@cache.internal:6380/0`,
		"amqp":            `amqp://guest:guestpass99@rabbit:5672/`,
		"basic-auth":      `Authorization: Basic dXNlcjpwYXNzd29yZDEyMw==`,
		"github-pat":      `github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV`,
		"stripe":          `sk_live_51H8abcdefghijklmnopqrst`,
		"gitlab":          `glpat-abcdefghij0123456789`,
		"google":          `key=AIzaSyA1234567890abcdefghijklmnopqrstuv`,
		"openai-legacy":   `sk-abcdefghijklmnopqrstT3BlbkFJabcdefghijklmnopqrst`,
		"underscore-edge": `ENV_sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_ab`,
		// A real key can end in an uppercase segment that looks like an
		// ENV_VAR name; prefixed formats must not go through that guard.
		"ends-uppercase": `sk-ant-api03-AbCdEfGhIjKlMnOp0123456789-XY_QAA`,
	}
	for name, in := range secrets {
		if out := String(in); !strings.Contains(out, "[REDACTED") {
			t.Errorf("%s: nothing redacted in %q", name, out)
		}
	}
}

func TestString_KeepsLookalikesThatAreNotKeys(t *testing.T) {
	keep := []string{
		"the sk-proj-management-dashboard-component renders the list",
		"npm i sk-admin-panel-react-components",
		"import anc_test_helpers from the fixtures",
		"the basic idea is to OR the tokens",
		"max_tokens: 40000",
		"see https://example.com/docs?page=2 for details",
	}
	for _, in := range keep {
		if out := String(in); out != in {
			t.Errorf("lookalike changed: %q -> %q", in, out)
		}
	}
}

func TestString_SecretNamesNeedASecretLookingValue(t *testing.T) {
	if out := String(`"SECRETOAUTHCODE": connection refused`); strings.Contains(out, "REDACTED") {
		t.Errorf("a plain word is not a secret value: %q", out)
	}
	if out := String(`client_secret_rotation: quarterly`); strings.Contains(out, "REDACTED") {
		t.Errorf("a plain word is not a secret value: %q", out)
	}
	for _, in := range []string{`password=hunter2hunter2`, `DB_PASSWORD: correcthorsebatterystaple`} {
		if out := String(in); !strings.Contains(out, "REDACTED") {
			t.Errorf("password not redacted: %q", out)
		}
	}
}
