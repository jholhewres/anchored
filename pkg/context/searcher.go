package ctx

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"unicode"
)

const defaultMaxResults = 20

// chunkSearcher abstracts the Store to allow parallel development and test doubles.
type chunkSearcher interface {
	SearchChunks(ctx context.Context, query string, maxResults int, contentType, source, projectID string) ([]ContentSearchResult, error)
}

type Searcher struct {
	store  chunkSearcher
	logger *slog.Logger
}

func NewSearcher(store chunkSearcher, logger *slog.Logger) *Searcher {
	return &Searcher{store: store, logger: logger}
}

func (s *Searcher) Search(ctx context.Context, query string, opts SearchOpts) ([]ContentSearchResult, error) {
	if opts.MaxResults <= 0 {
		opts.MaxResults = defaultMaxResults
	}

	fetchN := opts.MaxResults * 2
	raw, err := s.store.SearchChunks(ctx, query, fetchN, opts.ContentType, opts.Source, opts.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}

	reranked := proximityRerank(raw, query)

	for i := range reranked {
		reranked[i].Snippet = generateSnippet(reranked[i].Snippet, query, 300)
	}

	if len(reranked) > opts.MaxResults {
		reranked = reranked[:opts.MaxResults]
	}

	return reranked, nil
}

func proximityRerank(results []ContentSearchResult, query string) []ContentSearchResult {
	terms := extractTerms(query)
	if len(terms) <= 1 {
		return results
	}

	type scored struct {
		result   ContentSearchResult
		adjScore float64
	}

	scoredResults := make([]scored, len(results))
	for i, r := range results {
		content := strings.ToLower(r.Snippet)
		positions := findTermPositions(content, terms)
		minSpan := computeMinSpan(positions, len(terms))
		boost := proximityBoost(minSpan)
		baseScore := r.Score
		if baseScore < 0 {
			baseScore = -baseScore
		}
		scoredResults[i] = scored{result: r, adjScore: baseScore * boost}
	}

	sort.SliceStable(scoredResults, func(i, j int) bool {
		return scoredResults[i].adjScore > scoredResults[j].adjScore
	})

	out := make([]ContentSearchResult, len(scoredResults))
	for i, s := range scoredResults {
		s.result.Score = s.adjScore
		out[i] = s.result
	}
	return out
}

// proximityBoost: 1.5× at span=1 (adjacent terms), linearly decaying by 0.02 per char, floor 1.0.
func proximityBoost(minSpan int) float64 {
	if minSpan <= 0 {
		return 1.0
	}
	const maxBoost = 1.5
	const decay = 0.02
	boost := maxBoost - float64(minSpan-1)*decay
	if boost < 1.0 {
		return 1.0
	}
	return boost
}

// computeMinSpan uses a sweep-line over sorted term positions to find the
// minimum window containing at least one occurrence of every distinct term.
// Returns 0 when not all terms are present.
func computeMinSpan(positions map[string][]int, numTerms int) int {
	if len(positions) < numTerms {
		return 0
	}

	type posEntry struct {
		pos  int
		term string
	}
	var entries []posEntry
	for t, ps := range positions {
		for _, p := range ps {
			entries = append(entries, posEntry{pos: p, term: t})
		}
	}
	if len(entries) < numTerms {
		return 0
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].pos < entries[j].pos })

	termCount := make(map[string]int)
	distinct := 0
	minSpan := len(entries) + 1
	left := 0

	for right := range entries {
		e := entries[right]
		if termCount[e.term] == 0 {
			distinct++
		}
		termCount[e.term]++

		for distinct == numTerms {
			span := entries[right].pos - entries[left].pos + 1
			if span < minSpan {
				minSpan = span
			}
			termCount[entries[left].term]--
			if termCount[entries[left].term] == 0 {
				distinct--
			}
			left++
		}
	}

	if minSpan > len(entries) {
		return 0
	}
	return minSpan
}

func generateSnippet(content string, query string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}

	terms := extractTerms(query)
	if len(terms) == 0 {
		return snapToWords(content, 0, maxLen)
	}

	lower := strings.ToLower(content)
	positions := findAllTermPositions(lower, terms)
	if len(positions) == 0 {
		return snapToWords(content, 0, maxLen)
	}

	center := densestClusterCenter(positions, maxLen/2)

	half := maxLen / 2
	start := center - half
	if start < 0 {
		start = 0
	}
	end := start + maxLen
	if end > len(content) {
		end = len(content)
		start = end - maxLen
		if start < 0 {
			start = 0
		}
	}

	snippet := snapToWords(content, start, end)

	var prefix, suffix string
	if start > 0 {
		prefix = "..."
	}
	if end < len(content) {
		suffix = "..."
	}
	return prefix + snippet + suffix
}

func densestClusterCenter(positions []int, windowSize int) int {
	if len(positions) == 0 {
		return 0
	}
	sort.Ints(positions)

	bestCenter := positions[0]
	bestCount := 0

	for _, center := range positions {
		lo := center - windowSize
		hi := center + windowSize
		count := 0
		for _, p := range positions {
			if p >= lo && p <= hi {
				count++
			}
		}
		if count > bestCount {
			bestCount = count
			bestCenter = center
		}
	}
	return bestCenter
}

func snapToWords(content string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(content) {
		end = len(content)
	}

	if start > 0 && !unicode.IsSpace(rune(content[start])) {
		for i := start - 1; i >= 0; i-- {
			if unicode.IsSpace(rune(content[i])) {
				start = i + 1
				break
			}
			if i == 0 {
				start = 0
			}
		}
	}

	if end < len(content) && !unicode.IsSpace(rune(content[end])) {
		for i := end; i < len(content); i++ {
			if unicode.IsSpace(rune(content[i])) {
				end = i
				break
			}
		}
	}

	if end <= start {
		return content[start:min(start+300, len(content))]
	}

	return content[start:end]
}

func extractTerms(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	seen := make(map[string]bool, len(fields))
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			terms = append(terms, f)
		}
	}
	return terms
}

func findTermPositions(text string, terms []string) map[string][]int {
	positions := make(map[string][]int, len(terms))
	for _, term := range terms {
		idx := 0
		for {
			pos := strings.Index(text[idx:], term)
			if pos == -1 {
				break
			}
			positions[term] = append(positions[term], idx+pos)
			idx += pos + 1
		}
	}
	return positions
}

func findAllTermPositions(text string, terms []string) []int {
	var all []int
	for _, t := range terms {
		idx := 0
		for {
			pos := strings.Index(text[idx:], t)
			if pos == -1 {
				break
			}
			all = append(all, idx+pos)
			idx += pos + 1
		}
	}
	sort.Ints(all)
	return all
}
