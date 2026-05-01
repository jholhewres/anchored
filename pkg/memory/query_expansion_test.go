package memory

import (
	"strings"
	"testing"
)

func TestNormalizeAccents(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"café", "cafe"},
		{"autenticação", "autenticacao"},
		{"São Paulo", "Sao Paulo"},
		{"naïve", "naive"},
		{"garçon", "garcon"},
		{"niño", "nino"},
		{"àéîõü", "aeiou"},
		{"hello", "hello"},
		{"", ""},
		{"ÁÉÍÓÚ", "aeiou"},
		{"Ç", "c"},
	}
	for _, tt := range tests {
		got := NormalizeAccents(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeAccents(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExpandQueryAdvanced_Phrases(t *testing.T) {
	result := ExpandQueryAdvanced(`"exact phrase"`)
	if !strings.Contains(result, `"exact phrase"`) {
		t.Errorf("expected phrase to be preserved, got: %s", result)
	}
	if strings.Contains(result, `OR`) {
		t.Errorf("single phrase should not have OR, got: %s", result)
	}

	result = ExpandQueryAdvanced(`"exact phrase" word1`)
	if !strings.Contains(result, `"exact phrase"`) {
		t.Errorf("phrase missing from result, got: %s", result)
	}
	if !strings.Contains(result, `"word1"`) {
		t.Errorf("word missing from result, got: %s", result)
	}
}

func TestExpandQueryAdvanced_PhraseAccentNormalization(t *testing.T) {
	result := ExpandQueryAdvanced(`"café com leite"`)
	if !strings.Contains(result, `"cafe com leite"`) {
		t.Errorf("accents should be normalized in phrases, got: %s", result)
	}
}

func TestExpandQueryAdvanced_NEAR(t *testing.T) {
	result := ExpandQueryAdvanced(`auth NEAR/5 login`)
	if !strings.Contains(result, "auth NEAR/5 login") {
		t.Errorf("NEAR expression should be preserved, got: %s", result)
	}
}

func TestExpandQueryAdvanced_NEARAccentNormalization(t *testing.T) {
	result := ExpandQueryAdvanced(`autenticação NEAR/3 login`)
	if !strings.Contains(result, "autenticacao NEAR/3 login") {
		t.Errorf("accents should be normalized in NEAR, got: %s", result)
	}
}

func TestExpandQueryAdvanced_Synonyms(t *testing.T) {
	result := ExpandQueryAdvanced("auth")
	if !strings.Contains(result, `"authentication"`) {
		t.Errorf("synonym 'authentication' missing, got: %s", result)
	}
	if !strings.Contains(result, `"authenticate"`) {
		t.Errorf("synonym 'authenticate' missing, got: %s", result)
	}
}

func TestExpandQueryAdvanced_PrefixExpansion(t *testing.T) {
	result := ExpandQueryAdvanced("deploy")
	if !strings.Contains(result, "deploy*") {
		t.Errorf("prefix expansion missing, got: %s", result)
	}
}

func TestExpandQueryAdvanced_StopWords(t *testing.T) {
	result := ExpandQueryAdvanced("the auth")
	if strings.Contains(result, `"the"`) {
		t.Errorf("stop word 'the' should be filtered, got: %s", result)
	}
	if !strings.Contains(result, `"auth"`) {
		t.Errorf("keyword 'auth' should be present, got: %s", result)
	}
}

func TestExpandQueryAdvanced_Empty(t *testing.T) {
	result := ExpandQueryAdvanced("")
	if result != "" {
		t.Errorf("empty input should return empty, got: %s", result)
	}
	result = ExpandQueryAdvanced("   ")
	if result != "" {
		t.Errorf("whitespace input should return empty, got: %s", result)
	}
}

func TestExpandQueryAdvanced_Mixed(t *testing.T) {
	result := ExpandQueryAdvanced(`"memory leak" auth NEAR/5 debug`)

	if !strings.Contains(result, `"memory leak"`) {
		t.Errorf("phrase not found in: %s", result)
	}
	if !strings.Contains(result, "auth NEAR/5 debug") {
		t.Errorf("NEAR not found in: %s", result)
	}
}

func TestExpandQueryAdvanced_NoDuplicates(t *testing.T) {
	result := ExpandQueryAdvanced("test test")
	count := strings.Count(result, `"test"`)
	if count > 1 {
		t.Errorf("duplicates should be removed, got %d occurrences in: %s", count, result)
	}
}

func TestExtractKeywords_StopWords(t *testing.T) {
	kw := ExtractKeywords("the quick brown fox jumps over the lazy dog")
	for _, w := range kw {
		if stopWords[w] {
			t.Errorf("stop word %q should be filtered", w)
		}
	}
}

func TestExpandQueryForFTS_InterfaceUnchanged(t *testing.T) {
	keywords := []string{"auth", "deploy", "test"}
	result := ExpandQueryForFTS(keywords)
	if result == "" {
		t.Error("non-empty keywords should produce non-empty result")
	}
	if result != ExpandQueryForFTS(keywords) {
		t.Error("deterministic output expected")
	}
	if strings.Contains(result, `"test"`) && strings.Contains(result, `test*`) {
	} else {
		t.Errorf("expected both exact and prefix for 'test', got: %s", result)
	}
}

func TestExpandQueryForFTS_Empty(t *testing.T) {
	result := ExpandQueryForFTS(nil)
	if result != "" {
		t.Errorf("nil input should return empty, got: %s", result)
	}
	result = ExpandQueryForFTS([]string{})
	if result != "" {
		t.Errorf("empty slice should return empty, got: %s", result)
	}
}

func TestExpandQueryAdvanced_PortugueseSynonyms(t *testing.T) {
	result := ExpandQueryAdvanced("autenticar")
	if !strings.Contains(result, `"autenticacao"`) {
		t.Errorf("PT synonym missing, got: %s", result)
	}
}

func TestExpandQueryAdvanced_ShortWords(t *testing.T) {
	result := ExpandQueryAdvanced("go fly")
	if strings.Contains(result, `"go"`) {
		t.Error("'go' is a stop word, should not appear")
	}
}
