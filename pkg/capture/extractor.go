package capture

import (
	"regexp"
	"strings"
)

// ExtractionResult holds the extracted information from session content.
type ExtractionResult struct {
	Decisions    []string `json:"decisions,omitempty"`
	Facts        []string `json:"facts,omitempty"`
	Topics       []string `json:"topics,omitempty"`
	QualityScore float64  `json:"quality_score"`
}

// SummaryExtractor extracts structured information from text using rule-based patterns.
type SummaryExtractor struct {
	decisionPatterns []*regexp.Regexp
	factPatterns     []*regexp.Regexp
}

// NewSummaryExtractor creates a new extractor with compiled patterns.
func NewSummaryExtractor() *SummaryExtractor {
	return &SummaryExtractor{
		decisionPatterns: []*regexp.Regexp{
			// EN patterns
			regexp.MustCompile(`(?i)(?:decided\s+to|decision:\s*|we\s+will|agreed\s+(?:to|on)|going\s+with|opted\s+for|chose\s+to|let's\s+use|will\s+use)\s+(.+)`),
			regexp.MustCompile(`(?i)(?:switched\s+to|migrated\s+to|moved\s+to)\s+(.+)`),
			// PT-BR patterns
			regexp.MustCompile(`(?i)(?:decidimos|decisão:\s*|vamos\s+|concordamos\s+em|optamos\s+por|escolhemos)\s*(.+)`),
		},
		factPatterns: []*regexp.Regexp{
			// Declarative statements with is/are/uses/has/contains
			regexp.MustCompile(`(?i)(\w[\w\s-]+?)\s+(?:is|are|uses|has|contains|supports|requires|depends\s+on|provides|implements)\s+(.+?)[.!]`),
			// PT-BR declarative
			regexp.MustCompile(`(?i)(\w[\w\s-]+?)\s+(?:é|são|usa|tem|contém|suporta|requer|depende\s+de|fornece|implementa)\s+(.+?)[.!]`),
		},
	}
}

// Extract processes content and returns structured extraction results.
func (e *SummaryExtractor) Extract(content string) *ExtractionResult {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	result := &ExtractionResult{}

	// Extract sentences
	sentences := extractSentences(content)
	if len(sentences) == 0 {
		return result
	}

	// Extract decisions
	seen := make(map[string]bool)
	for _, sentence := range sentences {
		for _, pattern := range e.decisionPatterns {
			matches := pattern.FindStringSubmatch(sentence)
			if len(matches) > 1 && strings.TrimSpace(matches[1]) != "" {
				decision := cleanExtracted(matches[0])
				if !seen[decision] {
					result.Decisions = append(result.Decisions, decision)
					seen[decision] = true
				}
			}
		}
	}

	// Extract facts
	for _, sentence := range sentences {
		for _, pattern := range e.factPatterns {
			matches := pattern.FindStringSubmatch(sentence)
			if len(matches) > 0 && strings.TrimSpace(matches[0]) != "" {
				fact := cleanExtracted(matches[0])
				if !seen[fact] {
					result.Facts = append(result.Facts, fact)
					seen[fact] = true
				}
			}
		}
	}

	// Extract topics (capitalized words that aren't sentence starters)
	topics := extractTopics(content)
	for _, topic := range topics {
		if !seen[topic] {
			result.Topics = append(result.Topics, topic)
			seen[topic] = true
		}
	}

	// Compute quality score
	totalSentences := len(sentences)
	if totalSentences > 0 {
		extractable := len(result.Decisions)*3 + len(result.Facts)*2 + len(result.Topics)
		result.QualityScore = float64(extractable) / float64(totalSentences)
		if result.QualityScore > 1.0 {
			result.QualityScore = 1.0
		}
	}

	return result
}

// extractSentences splits content into individual sentences.
func extractSentences(text string) []string {
	// Split on sentence-ending punctuation followed by space or end
	var sentences []string
	current := strings.Builder{}

	for _, r := range text {
		current.WriteRune(r)
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			s := strings.TrimSpace(current.String())
			if len(s) > 10 { // Skip very short fragments
				sentences = append(sentences, s)
			}
			current.Reset()
		}
	}

	// Don't forget the last fragment
	if current.Len() > 0 {
		s := strings.TrimSpace(current.String())
		if len(s) > 10 {
			sentences = append(sentences, s)
		}
	}

	return sentences
}

// cleanExtracted normalizes extracted text.
func cleanExtracted(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".,;!?")
	s = strings.TrimSpace(s)
	// Collapse multiple spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// extractTopics extracts capitalized multi-word phrases that look like topics.
func extractTopics(content string) []string {
	// Simple topic extraction: find capitalized phrases
	re := regexp.MustCompile(`\b([A-Z][a-z]+(?:\s+[A-Z][a-z]+)+)\b`)
	matches := re.FindAllString(content, -1)

	seen := make(map[string]bool)
	var topics []string
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if !seen[m] && len(m) > 3 {
			topics = append(topics, m)
			seen[m] = true
		}
	}
	return topics
}
