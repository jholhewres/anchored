package memory

import (
	"context"
	"path/filepath"
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
	if !strings.Contains(result, `NEAR("auth" "login", 5)`) {
		t.Errorf("NEAR expression should be preserved (FTS5 syntax), got: %s", result)
	}
}

func TestExpandQueryAdvanced_NEARAccentNormalization(t *testing.T) {
	result := ExpandQueryAdvanced(`autenticação NEAR/3 login`)
	if !strings.Contains(result, `NEAR("autenticacao" "login", 3)`) {
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
	if !strings.Contains(result, `"deploy"*`) {
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
	if !strings.Contains(result, `NEAR("auth" "debug", 5)`) {
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
	// Exact term always; a prefix term only from five runes (minPrefixRunes).
	if !strings.Contains(result, `"deploy"`) || !strings.Contains(result, `"deploy"*`) {
		t.Errorf("expected both exact and prefix for 'deploy', got: %s", result)
	}
	if !strings.Contains(result, `"test"`) || strings.Contains(result, `"test"*`) {
		t.Errorf("expected only the exact term for the 4-rune 'test', got: %s", result)
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

// A prefix term on a short stem matches far too much ("api*" hits apis,
// apiary, apikey…): prefixes start at five runes.
func TestExpandQuery_PrefixOnlyFromFiveRunes(t *testing.T) {
	for query, want := range map[string]bool{"api": false, "logs": false, "cache": true, "sessão": true} {
		r := ExpandQueryAdvanced(query)
		stem := `"` + NormalizeAccents(query) + `"*`
		if strings.Contains(r, stem) != want {
			t.Errorf("%q → %s: prefix %s present=%v, want %v", query, r, stem, !want, want)
		}
	}
}

// Portuguese and Spanish function words carry no topic and, OR-joined, match
// nearly every memory.
func TestExpandQuery_DropsPortugueseAndSpanishFunctionWords(t *testing.T) {
	cases := map[string][]string{
		"como o gateway verifica os tokens na api":              {"gateway", "tokens"},
		"¿cómo evitamos los enlaces rotos en la documentación?": {"enlaces", "documentacion"},
		"um ajuste ao deploy dos serviços":                      {"ajuste", "deploy", "servicos"},
	}
	for query, keep := range cases {
		r := ExpandQueryAdvanced(query)
		for _, w := range []string{`"os"`, `"na"`, `"como"`, `"los"`, `"en"`, `"la"`, `"um"`, `"ao"`, `"dos"`} {
			if strings.Contains(r, w) {
				t.Errorf("%q: function word %s kept: %s", query, w, r)
			}
		}
		for _, w := range keep {
			if !strings.Contains(r, `"`+w+`"`) {
				t.Errorf("%q: topic word %q dropped: %s", query, w, r)
			}
		}
	}
}

func TestExpandQueryAND_RequiresEveryWord(t *testing.T) {
	q := ExpandQueryAND("deploy banco")
	if strings.Count(q, " AND ") != 1 || !strings.Contains(q, `"deploy"`) || !strings.Contains(q, `"banco"`) {
		t.Fatalf("AND query for two words: %s", q)
	}
	if q := ExpandQueryAND("deploy"); q != "" {
		t.Errorf("one word needs no AND query, got %s", q)
	}
}

// A memory that covers every word of the query outranks one that only
// repeats variants of one of them; the OR query still fills in after it.
func TestSearchBM25_CoverageBeatsRepetition(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "bm25.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	for id, content := range map[string]string{
		"repeats": "deploy deployment release deploy release deploy deployment release",
		"covers":  "we deploy the banco schema on fridays",
	} {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultHybridSearchConfig()
	cfg.TemporalDecayEnabled = false
	h := NewHybridSearcher(store, nil, nil, nil, cfg, nil, nil, nil)
	res, err := h.Search(ctx, "deploy banco", SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Memory.ID != "covers" {
		t.Fatalf("got %v, want covers first and repeats after it", resultIDs(res))
	}
}

// Quoted phrases and NEAR keep their meaning only in the OR form.
func TestExpandQueryAND_LeavesPhrasesAndNEARToTheORForm(t *testing.T) {
	for _, q := range []string{`"token bucket" redis`, "token NEAR/3 bucket redis"} {
		if got := ExpandQueryAND(q); got != "" {
			t.Errorf("%q: AND form %q, want none", q, got)
		}
	}
}

func TestExpandQuery_StopwordsMatchWithoutAccents(t *testing.T) {
	r := ExpandQueryAdvanced("pra dele unos esto ademas quiza gateway")
	for _, w := range []string{`"pra"`, `"dele"`, `"unos"`, `"esto"`, `"ademas"`, `"quiza"`} {
		if strings.Contains(r, w) {
			t.Errorf("function word %s kept: %s", w, r)
		}
	}
	if !strings.Contains(r, `"gateway"`) {
		t.Errorf("topic word dropped: %s", r)
	}
	// Technical terms that looked like function words stay.
	if r := ExpandQueryAdvanced("claude pro plan"); !strings.Contains(r, `"pro"`) {
		t.Errorf("'pro' is a product tier here, got %s", r)
	}
}
