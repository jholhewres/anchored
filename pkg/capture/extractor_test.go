package capture

import (
	"strings"
	"testing"
)

func TestExtract_DecisionPatterns(t *testing.T) {
	e := NewSummaryExtractor()

	tests := []struct {
		name    string
		input   string
		wantAny bool
	}{
		{
			name:    "decided to",
			input:   "We decided to use PostgreSQL for the database.",
			wantAny: true,
		},
		{
			name:    "going with",
			input:   "Going with REST API for the backend.",
			wantAny: true,
		},
		{
			name:    "PT-BR decidimos",
			input:   "Decidimos usar Go para o backend do projeto.",
			wantAny: true,
		},
		{
			name:    "we will",
			input:   "We will implement caching with Redis.",
			wantAny: true,
		},
		{
			name:    "no decision",
			input:   "The weather is nice today. I like coffee.",
			wantAny: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := e.Extract(tt.input)
			if result == nil && tt.wantAny {
				t.Error("expected decisions, got nil result")
			}
			if result != nil && tt.wantAny && len(result.Decisions) == 0 {
				t.Errorf("expected at least one decision, got 0")
			}
			if result != nil && !tt.wantAny && len(result.Decisions) > 0 {
				t.Errorf("expected no decisions, got %v", result.Decisions)
			}
		})
	}
}

func TestExtract_FactPatterns(t *testing.T) {
	e := NewSummaryExtractor()

	tests := []struct {
		name    string
		input   string
		wantAny bool
	}{
		{
			name:    "uses fact",
			input:   "The project uses Go 1.24 for the backend service.",
			wantAny: true,
		},
		{
			name:    "is fact",
			input:   "PostgreSQL is the primary database for this service.",
			wantAny: true,
		},
		{
			name:    "no fact",
			input:   "Hello world! How are you?",
			wantAny: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := e.Extract(tt.input)
			if result == nil && tt.wantAny {
				t.Error("expected facts, got nil result")
				return
			}
			if result != nil && tt.wantAny && len(result.Facts) == 0 {
				t.Errorf("expected at least one fact, got 0 for: %s", tt.input)
			}
		})
	}
}

func TestExtract_QualityScore(t *testing.T) {
	e := NewSummaryExtractor()

	high := e.Extract("We decided to use PostgreSQL for the database. The project uses Go 1.24. The API provides REST endpoints for user management.")
	if high == nil || high.QualityScore < 0.3 {
		t.Errorf("expected high quality score, got %v", high)
	}

	low := e.Extract("Hello there. How are you doing today? Nice weather we're having. Let's get coffee sometime.")
	if low != nil && low.QualityScore > 0.5 {
		t.Errorf("expected low quality score, got %f", low.QualityScore)
	}
}

func TestExtract_Multilingual(t *testing.T) {
	e := NewSummaryExtractor()

	result := e.Extract("Decidimos usar PostgreSQL para o banco de dados. O projeto usa Go 1.24 no backend.")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Decisions) == 0 {
		t.Error("expected PT-BR decision extraction")
	}
}

func TestExtract_EmptyContent(t *testing.T) {
	e := NewSummaryExtractor()

	result := e.Extract("")
	if result != nil {
		t.Errorf("expected nil for empty content, got %+v", result)
	}

	result = e.Extract("   ")
	if result != nil {
		t.Errorf("expected nil for whitespace content, got %+v", result)
	}
}

func TestExtract_NoExtractableContent(t *testing.T) {
	e := NewSummaryExtractor()

	result := e.Extract("Random text without any structure or patterns to extract meaningful information from.")
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.QualityScore > 0.3 {
		t.Errorf("expected low quality score for unstructured content, got %f", result.QualityScore)
	}
}

func TestExtract_Deduplication(t *testing.T) {
	e := NewSummaryExtractor()

	result := e.Extract("We decided to use PostgreSQL. We decided to use PostgreSQL.")
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	seen := make(map[string]bool)
	for _, d := range result.Decisions {
		if seen[d] {
			t.Errorf("duplicate decision found: %s", d)
		}
		seen[d] = true
	}
}

func TestExtract_FormattedSummary(t *testing.T) {
	e := NewSummaryExtractor()
	result := e.Extract("We decided to use PostgreSQL for persistence. The project uses Go 1.24. React provides the frontend framework.")
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	var parts []string
	if len(result.Decisions) > 0 {
		parts = append(parts, "### Decisions")
		for _, d := range result.Decisions {
			parts = append(parts, "- "+d)
		}
	}
	if len(result.Facts) > 0 {
		parts = append(parts, "### Key Facts")
		for _, f := range result.Facts {
			parts = append(parts, "- "+f)
		}
	}

	summary := strings.Join(parts, "\n")
	if summary == "" {
		t.Error("expected non-empty summary")
	}
}
