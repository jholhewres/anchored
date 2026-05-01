package kg

import (
	"testing"
)

func TestExtract_EN_Uses(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Auth service uses Redis for session caching")
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}
	c := candidates[0]
	if c.Predicate != "uses" {
		t.Errorf("expected predicate 'uses', got %q", c.Predicate)
	}
	if c.Subject != "Auth service" {
		t.Errorf("expected subject 'Auth service', got %q", c.Subject)
	}
	if c.Object != "Redis" {
		t.Errorf("expected object 'Redis', got %q", c.Object)
	}
}

func TestExtract_PTBR_Usa(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Serviço de autenticação usa Redis para cache")
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}
	c := candidates[0]
	if c.Predicate != "uses" {
		t.Errorf("expected predicate 'uses', got %q", c.Predicate)
	}
}

func TestExtract_DependsOn(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("API depends on PostgreSQL")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for depends on")
	}
	if candidates[0].Predicate != "depends_on" {
		t.Errorf("expected 'depends_on', got %q", candidates[0].Predicate)
	}
}

func TestExtract_DependeDe(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("A aplicação depende de PostgreSQL")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for depende de")
	}
	if candidates[0].Predicate != "depends_on" {
		t.Errorf("expected 'depends_on', got %q", candidates[0].Predicate)
	}
}

func TestExtract_DeployedOn(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Frontend deployed on Vercel")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for deployed on")
	}
	if candidates[0].Predicate != "deployed_on" {
		t.Errorf("expected 'deployed_on', got %q", candidates[0].Predicate)
	}
}

func TestExtract_ImplantadoEm(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Backend implantado em AWS")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for implantado em")
	}
	if candidates[0].Predicate != "deployed_on" {
		t.Errorf("expected 'deployed_on', got %q", candidates[0].Predicate)
	}
}

func TestExtract_IsA(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Docker is a container runtime")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for is a")
	}
	if candidates[0].Predicate != "is_a" {
		t.Errorf("expected 'is_a', got %q", candidates[0].Predicate)
	}
	if candidates[0].Subject != "Docker" {
		t.Errorf("expected 'Docker', got %q", candidates[0].Subject)
	}
}

func TestExtract_EUm(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Docker é um runtime de containers")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for é um")
	}
	if candidates[0].Predicate != "is_a" {
		t.Errorf("expected 'is_a', got %q", candidates[0].Predicate)
	}
}

func TestExtract_CreatedBy(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("This module was created by John")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for created by")
	}
	if candidates[0].Predicate != "created_by" {
		t.Errorf("expected 'created_by', got %q", candidates[0].Predicate)
	}
}

func TestExtract_CriadoPor(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Este módulo foi criado por João")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for criado por")
	}
	if candidates[0].Predicate != "created_by" {
		t.Errorf("expected 'created_by', got %q", candidates[0].Predicate)
	}
}

func TestExtract_Requires(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Service requires authentication token")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for requires")
	}
	if candidates[0].Predicate != "requires" {
		t.Errorf("expected 'requires', got %q", candidates[0].Predicate)
	}
}

func TestExtract_IntegratesWith(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("App integrates with Stripe")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for integrates with")
	}
	if candidates[0].Predicate != "integrates_with" {
		t.Errorf("expected 'integrates_with', got %q", candidates[0].Predicate)
	}
}

func TestExtract_IntegraCom(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("App integra com Stripe")
	if len(candidates) == 0 {
		t.Fatal("expected candidate for integra com")
	}
	if candidates[0].Predicate != "integrates_with" {
		t.Errorf("expected 'integrates_with', got %q", candidates[0].Predicate)
	}
}

func TestExtract_NoMatch(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Random text with no relationships here")
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(candidates))
	}
}

func TestExtract_MultiplePatterns(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	candidates := ext.Extract("Auth service uses Redis and depends on PostgreSQL")
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 candidates, got %d", len(candidates))
	}
	predicates := map[string]bool{}
	for _, c := range candidates {
		predicates[c.Predicate] = true
	}
	if !predicates["uses"] {
		t.Error("missing 'uses' predicate")
	}
	if !predicates["depends_on"] {
		t.Error("missing 'depends_on' predicate")
	}
}

func TestExtractAndStore_SkipShortText(t *testing.T) {
	ext := NewPatternExtractor(nil, nil)
	err := ext.ExtractAndStore(nil, "short text", nil)
	if err != nil {
		t.Errorf("expected no error for short text, got %v", err)
	}
}

func TestExtractAndStore_MaxTriples(t *testing.T) {
	ext := &PatternExtractor{
		patterns: defaultPatterns(),
		kg:        nil,
		logger:    nil,
		maxTriples: 2,
	}
	candidates := ext.Extract("Service A uses Redis and depends on PostgreSQL and deployed on AWS and created by John and requires Auth")
	if len(candidates) < 3 {
		t.Fatalf("expected at least 3 candidates for maxTriples test, got %d", len(candidates))
	}
}
