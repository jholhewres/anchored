package memory

import (
	"strings"
	"testing"
)

func TestSanitizer_APIKeys(t *testing.T) {
	s := NewSanitizer(true)
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"API key with equals", `API_KEY=sk-abc123def456ghi789jkl0`, `API_KEY=[REDACTED]`},
		{"api_key with colon", `api_key: sk-abc123def456ghi789jkl0`, `api_key=[REDACTED]`},
		{"apikey variant", `apikey="sk-abc123def456ghi789jkl0"`, `apikey=[REDACTED]`},
		{"access key variant", `access_key=sk-abc123def456ghi789jkl0mno`, `access_key=[REDACTED]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Sanitize(tc.input)
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] in output, got: %s", got)
			}
			if !strings.HasPrefix(got, strings.Split(tc.want, "[REDACTED]")[0]) {
				t.Errorf("expected key name preserved, got: %s", got)
			}
		})
	}
}

func TestSanitizer_Tokens(t *testing.T) {
	s := NewSanitizer(true)
	cases := []struct {
		name  string
		input string
	}{
		{"token with equals", `token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9`},
		{"auth_token", `auth_token: abcdefghijklmnopqrstuvwxyz123456`},
		{"refresh_token", `refresh_token="ghijklmnopqrstuvwxyz123456"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Sanitize(tc.input)
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] in: %s", got)
			}
		})
	}
}

func TestSanitizer_Passwords(t *testing.T) {
	s := NewSanitizer(true)
	got := s.Sanitize(`password = "supersecretpassword123"`)
	if !strings.Contains(got, "password=[REDACTED]") {
		t.Errorf("expected password=[REDACTED], got: %s", got)
	}

	got = s.Sanitize(`pwd=mysecret123`)
	if !strings.Contains(got, "pwd=[REDACTED]") {
		t.Errorf("expected pwd=[REDACTED], got: %s", got)
	}
}

func TestSanitizer_Secrets(t *testing.T) {
	s := NewSanitizer(true)
	got := s.Sanitize(`secret = myverylongsecretvalue`)
	if !strings.Contains(got, "secret=[REDACTED]") {
		t.Errorf("expected secret=[REDACTED], got: %s", got)
	}

	got = s.Sanitize(`credential=longcredentialvalue123`)
	if !strings.Contains(got, "credential=[REDACTED]") {
		t.Errorf("expected credential=[REDACTED], got: %s", got)
	}
}

func TestSanitizer_PrivateKeys(t *testing.T) {
	s := NewSanitizer(true)
	key := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGy0AHB7Lso
-----END RSA PRIVATE KEY-----`
	got := s.Sanitize(key)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] for PEM block, got: %s", got)
	}
	if strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("PEM content should be fully redacted, got: %s", got)
	}
}

func TestSanitizer_JWT(t *testing.T) {
	s := NewSanitizer(true)
	jwt := `eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c`
	got := s.Sanitize(jwt)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] for JWT, got: %s", got)
	}
	if strings.Contains(got, "eyJ") {
		t.Errorf("JWT content should be fully redacted, got: %s", got)
	}
}

func TestSanitizer_AWSAccessKey(t *testing.T) {
	s := NewSanitizer(true)
	got := s.Sanitize(`aws_access_key_id = AKIAIOSFODNN7EXAMPLE`)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] for AWS key, got: %s", got)
	}
	if strings.Contains(got, "AKIA") {
		t.Errorf("AWS key should be fully redacted, got: %s", got)
	}
}

func TestSanitizer_GitHubTokens(t *testing.T) {
	s := NewSanitizer(true)
	tokens := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz1234567890",
		"gho_abcdefghijklmnopqrstuvwxyz1234567890",
		"ghu_abcdefghijklmnopqrstuvwxyz1234567890",
		"ghs_abcdefghijklmnopqrstuvwxyz1234567890",
	}
	for _, tok := range tokens {
		got := s.Sanitize(tok)
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("expected [REDACTED] for %s, got: %s", tok[:4], got)
		}
	}
}

func TestSanitizer_GenericTokenPatterns(t *testing.T) {
	s := NewSanitizer(true)
	cases := []struct {
		name  string
		input string
	}{
		{"api_key_var", "api_key=AKIAIOSFODNN7EXAMPLE"},
		{"bearer_header", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.test.sig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Sanitize(tc.input)
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] for %s, got: %s", tc.name, got)
			}
		})
	}
}

func TestSanitizer_ConnectionStrings(t *testing.T) {
	s := NewSanitizer(true)
	cases := []struct {
		name  string
		input string
	}{
		{"mongodb", `mongodb://admin:s3cret@mongo.example.com:27017/db`},
		{"mongodb+srv", `mongodb+srv://user:p@ssw0rd@cluster.example.com/db`},
		{"postgresql", `postgresql://postgres:mydbpassword@db.example.com:5432/mydb`},
		{"postgres", `postgres://root:secret@localhost/app`},
		{"mysql", `mysql://root:rootpassword@127.0.0.1:3306/app`},
		{"redis", `redis://:mypassword@redis.example.com:6379/0`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Sanitize(tc.input)
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("expected [REDACTED] in: %s", got)
			}
			if strings.Contains(got, "s3cret") || strings.Contains(got, "p@ssw0rd") ||
				strings.Contains(got, "mydbpassword") || strings.Contains(got, "rootpassword") ||
				strings.Contains(got, "mypassword") {
				t.Errorf("password should be redacted, got: %s", got)
			}
		})
	}
}

func TestSanitizer_BearerToken(t *testing.T) {
	s := NewSanitizer(true)
	got := s.Sanitize(`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc123defghij`)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED], got: %s", got)
	}
	if strings.Contains(got, "eyJ") {
		t.Errorf("JWT after Bearer should be redacted, got: %s", got)
	}
}

func TestSanitizer_SK_PK(t *testing.T) {
	s := NewSanitizer(true)
	got := s.Sanitize(`sk=abcdefghijklmnopqrstuvwxyz`)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] for sk=, got: %s", got)
	}

	got = s.Sanitize(`pk = abcdefghijklmnopqrstuvwxyz123`)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] for pk=, got: %s", got)
	}
}

func TestSanitizer_LegitimateContent(t *testing.T) {
	s := NewSanitizer(true)
	cases := []struct {
		name  string
		input string
	}{
		{"short password warning", `Use a password of at least 8 characters`},
		{"github repo URL", `https://github.com/owner/repo/pull/123`},
		{"API key in docs", `The API key can be found in settings`},
		{"token in discussion", `We should rotate the auth token regularly`},
		{"bearer in text", `The bearer of this certificate is John Doe`},
		{"redis command", `redis-cli SET mykey myvalue`},
		{"secret in news", `The company disclosed a secret strategy`},
		{"short secret", `secret=ab`},
		{"postgres mention", `We use PostgreSQL for the main database`},
		{"AWS in docs", `AWS EC2 instances can be launched via console`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Sanitize(tc.input)
			if strings.Contains(got, "[REDACTED]") {
				t.Errorf("legitimate content should NOT be redacted, got: %s", got)
			}
			if got != tc.input {
				t.Errorf("content changed unexpectedly:\n  want: %s\n  got:  %s", tc.input, got)
			}
		})
	}
}

func TestSanitizer_Disabled(t *testing.T) {
	s := NewSanitizer(false)
	input := `password = supersecretvalue12345`
	got := s.Sanitize(input)
	if got != input {
		t.Errorf("disabled sanitizer should not modify input, got: %s", got)
	}
}

func TestSanitizer_EmptyAndNil(t *testing.T) {
	s := NewSanitizer(true)

	if got := s.Sanitize(""); got != "" {
		t.Errorf("empty input should stay empty, got: %s", got)
	}

	if got := s.Sanitize("hello world"); got != "hello world" {
		t.Errorf("plain text should stay unchanged, got: %s", got)
	}
}

func TestSanitizer_MultiplePatterns(t *testing.T) {
	s := NewSanitizer(true)
	input := `DB_URL=postgresql://admin:s3cret@db:5432/app
API_KEY=sk-abcdefghijklmnopqrstuvwxyz12345
TOKEN=ghp_abcdefghijklmnopqrstuvwxyz123456`
	got := s.Sanitize(input)

	redactedCount := strings.Count(got, "[REDACTED]")
	if redactedCount < 3 {
		t.Errorf("expected at least 3 redactions, got %d: %s", redactedCount, got)
	}
}
