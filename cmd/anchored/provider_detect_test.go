package main

import "testing"

func TestDetectProvider(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name         string
		vars         map[string]string
		wantProvider string
		wantModel    string
	}{
		{"anthropic default", map[string]string{"ANTHROPIC_MODEL": "claude-opus-4-8", "ANTHROPIC_API_KEY": "x"}, "anthropic", "claude-opus-4-8"},
		{"glm via z.ai base url", map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic", "ANTHROPIC_MODEL": "glm-4.6"}, "glm", "glm-4.6"},
		{"glm via bigmodel, no model", map[string]string{"ANTHROPIC_BASE_URL": "https://open.bigmodel.cn/api/anthropic"}, "glm", "glm"},
		{"anthropic via auth token only", map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok"}, "anthropic", ""},
		{"openai", map[string]string{"OPENAI_API_KEY": "sk", "OPENAI_MODEL": "gpt-5.4"}, "openai", "gpt-5.4"},
		{"unknown", map[string]string{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, m := detectProvider(env(tc.vars))
			if p != tc.wantProvider || m != tc.wantModel {
				t.Fatalf("detectProvider = (%q,%q), want (%q,%q)", p, m, tc.wantProvider, tc.wantModel)
			}
		})
	}
}
