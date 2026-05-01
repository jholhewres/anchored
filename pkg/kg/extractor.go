package kg

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

type TripleCandidate struct {
	Subject    string
	Predicate  string
	Object     string
	Confidence float64
}

type extractionPattern struct {
	regex        *regexp.Regexp
	predicate    string
	confidence   float64
	subjectGroup int
	objectGroup  int
}

type PatternExtractor struct {
	patterns  []extractionPattern
	kg        *KG
	logger    *slog.Logger
	maxTriples int
}

func NewPatternExtractor(kg *KG, logger *slog.Logger) *PatternExtractor {
	if logger == nil {
		logger = slog.Default()
	}

	p := &PatternExtractor{
		kg:        kg,
		logger:    logger,
		maxTriples: 5,
	}
	p.patterns = defaultPatterns()
	return p
}

func (p *PatternExtractor) Extract(text string) []TripleCandidate {
	var candidates []TripleCandidate
	for _, pat := range p.patterns {
		matches := pat.regex.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if pat.subjectGroup >= len(m) || pat.objectGroup >= len(m) {
				continue
			}
			subject := strings.TrimSpace(m[pat.subjectGroup])
			object := strings.TrimSpace(m[pat.objectGroup])
			if subject == "" || object == "" {
				continue
			}
			candidates = append(candidates, TripleCandidate{
				Subject:    subject,
				Predicate:  pat.predicate,
				Object:     object,
				Confidence: pat.confidence,
			})
		}
	}
	return candidates
}

func (p *PatternExtractor) ExtractAndStore(ctx context.Context, text string, projectID *string) error {
	if len(text) < 20 {
		return nil
	}

	candidates := p.Extract(text)

	count := len(candidates)
	if count > p.maxTriples {
		count = p.maxTriples
	}

	for i := 0; i < count; i++ {
		c := candidates[i]
		if _, err := p.kg.AddTriple(ctx, c.Subject, c.Predicate, c.Object, projectID); err != nil {
			p.logger.Debug("kg extractor: failed to store triple", "subject", c.Subject, "predicate", c.Predicate, "object", c.Object, "error", err)
			continue
		}
		p.logger.Debug("kg extractor: stored triple", "subject", c.Subject, "predicate", c.Predicate, "object", c.Object, "confidence", c.Confidence)
	}

	p.logger.Debug("kg extractor: extracted triples", "total", len(candidates), "stored", count)
	return nil
}

func defaultPatterns() []extractionPattern {
	word := `([A-Za-z]\w[\w.\-]*)`
	return []extractionPattern{
		// "X uses Y" / "X usa Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:uses?|usa)\s+` + word),
			predicate:    "uses",
			confidence:   0.9,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X depends on Y" / "X depende de Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:depends?\s+on|depende\s+de)\s+` + word),
			predicate:    "depends_on",
			confidence:   0.85,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X deployed on Y" / "X implantado em Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:deployed?\s+on|implantad[oa]\s+em)\s+` + word),
			predicate:    "deployed_on",
			confidence:   0.8,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X is a Y" / "X é um Y" / "X é uma Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:is\s+a|é\s+(?:um|uma))\s+` + word),
			predicate:    "is_a",
			confidence:   0.9,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X created by Y" / "X criado por Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:created\s+by|criad[oa]\s+por)\s+` + word),
			predicate:    "created_by",
			confidence:   0.85,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X requires Y" / "X requer Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:requires?|requer)\s+` + word),
			predicate:    "requires",
			confidence:   0.8,
			subjectGroup: 1,
			objectGroup:  2,
		},
		// "X integrates with Y" / "X integra com Y"
		{
			regex:        regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9_.\- ]{0,40}?)\s+(?:integrates?\s+with|integra\s+com)\s+` + word),
			predicate:    "integrates_with",
			confidence:   0.75,
			subjectGroup: 1,
			objectGroup:  2,
		},
	}
}
