package dream

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

type DreamReport struct {
	TotalMemories int           `json:"total_memories"`
	ExactDupes    int           `json:"exact_dupes"`
	NearDupes     int           `json:"near_dupes"`
	Actions       []DreamAction `json:"actions,omitempty"`
}

type DreamAction struct {
	ID              string  `json:"id"`
	MemoryID        string  `json:"memory_id"`
	RelatedMemoryID string  `json:"related_memory_id,omitempty"`
	ActionType      string  `json:"action_type"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason"`
}

type DreamAnalyzer struct {
	db          *sql.DB
	vectorCache *memory.VectorCache
	config      DreamConfig
	logger      *slog.Logger
}

func NewAnalyzer(db *sql.DB, vectorCache *memory.VectorCache, cfg DreamConfig, logger *slog.Logger) *DreamAnalyzer {
	if logger == nil {
		logger = slog.Default()
	}
	return &DreamAnalyzer{db: db, vectorCache: vectorCache, config: cfg, logger: logger}
}

func (a *DreamAnalyzer) Analyze(ctx context.Context) (*DreamReport, error) {
	report := &DreamReport{}

	rows, err := a.db.QueryContext(ctx,
		"SELECT id, content, content_hash, project_id, created_at FROM memories WHERE deleted_at IS NULL ORDER BY created_at ASC")
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()

	type memInfo struct {
		id          string
		content     string
		contentHash string
		projectID   *string
		createdAt   time.Time
	}

	var memories []memInfo
	hashGroups := make(map[string][]int)

	for rows.Next() {
		var m memInfo
		if err := rows.Scan(&m.id, &m.content, &m.contentHash, &m.projectID, &m.createdAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		idx := len(memories)
		memories = append(memories, m)
		if m.contentHash != "" {
			hashGroups[m.contentHash] = append(hashGroups[m.contentHash], idx)
		}
	}

	report.TotalMemories = len(memories)
	if len(memories) == 0 {
		return report, nil
	}

	// Tier 1: Exact dedup by content_hash
	seen := make(map[string]bool)
	for hash, indices := range hashGroups {
		if len(indices) <= 1 {
			continue
		}
		report.ExactDupes += len(indices) - 1
		for i := 1; i < len(indices); i++ {
			older := memories[indices[0]]
			newer := memories[indices[i]]
			actionID := fmt.Sprintf("dedup-exact-%s-%s", older.id, newer.id)
			report.Actions = append(report.Actions, DreamAction{
				ID:              actionID,
				MemoryID:        older.id,
				RelatedMemoryID: newer.id,
				ActionType:      "dedup",
				Confidence:      1.0,
				Reason:          fmt.Sprintf("exact content_hash match: %s", truncate(hash, 16)),
			})
			seen[older.id] = true
		}
	}

	// Tier 2: Semantic near-dup via vector cosine similarity
	if a.vectorCache != nil && a.vectorCache.Len() > 0 {
		allVecs := a.vectorCache.All()

		type vecEntry struct {
			id  string
			vec []float32
		}
		var entries []vecEntry
		for id, vec := range allVecs {
			if len(vec) == 0 {
				continue
			}
			entries = append(entries, vecEntry{id: id, vec: vec})
		}

		compareCount := 0
		for i := 0; i < len(entries) && compareCount < a.config.MaxPairwiseCompare; i++ {
			for j := i + 1; j < len(entries) && compareCount < a.config.MaxPairwiseCompare; j++ {
				compareCount++
				aVec := entries[i].vec
				bVec := entries[j].vec

				score := cosineSimilarity(aVec, bVec)
				if score >= a.config.DedupThreshold {
					idA := entries[i].id
					idB := entries[j].id

					if seen[idA] && seen[idB] {
						continue
					}

					report.NearDupes++
					actionID := fmt.Sprintf("dedup-near-%s-%s", idA, idB)
					report.Actions = append(report.Actions, DreamAction{
						ID:              actionID,
						MemoryID:        idA,
						RelatedMemoryID: idB,
						ActionType:      "dedup",
						Confidence:      score,
						Reason:          fmt.Sprintf("cosine=%.3f (near-duplicate)", score),
					})
				}
			}
		}
	}

	// Tier 3: Contradiction detection
	if a.vectorCache != nil && a.vectorCache.Len() > 0 {
		allVecs := a.vectorCache.All()
		type vecEntry struct {
			id  string
			vec []float32
		}
		var entries []vecEntry
		for id, vec := range allVecs {
			if len(vec) == 0 {
				continue
			}
			entries = append(entries, vecEntry{id: id, vec: vec})
		}

		contentMap := make(map[string]string)
		for _, m := range memories {
			contentMap[m.id] = m.content
		}

		compareCount := 0
		for i := 0; i < len(entries) && compareCount < a.config.MaxPairwiseCompare; i++ {
			for j := i + 1; j < len(entries) && compareCount < a.config.MaxPairwiseCompare; j++ {
				compareCount++
				score := cosineSimilarity(entries[i].vec, entries[j].vec)

				if score >= 0.7 && score < a.config.DedupThreshold {
					contentA := contentMap[entries[i].id]
					contentB := contentMap[entries[j].id]

					if contentA == "" || contentB == "" {
						continue
					}

					hasNegation := detectNegation(contentA, contentB)
					hasAntonyms := detectAntonyms(contentA, contentB)

					if hasNegation || hasAntonyms {
						report.Actions = append(report.Actions, DreamAction{
							ID:              fmt.Sprintf("contradict-%s-%s", entries[i].id, entries[j].id),
							MemoryID:        entries[i].id,
							RelatedMemoryID: entries[j].id,
							ActionType:      "contradiction",
							Confidence:      0.6,
							Reason: func() string {
								if hasNegation && hasAntonyms {
									return "negation + antonym pattern"
								} else if hasNegation {
									return "negation pattern"
								}
								return "antonym pattern"
							}(),
						})
					}
				}
			}
		}
	}

	sort.Slice(report.Actions, func(i, j int) bool {
		return report.Actions[i].Confidence > report.Actions[j].Confidence
	})

	return report, nil
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (sqrt(normA) * sqrt(normB))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func SaveDreamRun(ctx context.Context, db *sql.DB, id string, report *DreamReport, status string) error {
	_, err := db.ExecContext(ctx,
		"INSERT OR REPLACE INTO dream_runs (id, started_at, finished_at, memories_analyzed, actions_proposed, status) VALUES (?, ?, ?, ?, ?, ?)",
		id, time.Now().Add(-time.Minute), time.Now(), report.TotalMemories, len(report.Actions), status)
	return err
}

func SaveDreamActions(ctx context.Context, db *sql.DB, runID string, actions []DreamAction) error {
	for _, a := range actions {
		id := a.ID
		if id == "" {
			id = fmt.Sprintf("action-%d", time.Now().UnixNano())
		}
		_, err := db.ExecContext(ctx,
			"INSERT OR IGNORE INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, status) VALUES (?, ?, ?, ?, ?, ?, ?, 'proposed')",
			id, runID, a.MemoryID, a.RelatedMemoryID, a.ActionType, a.Confidence, a.Reason)
		if err != nil {
			return fmt.Errorf("save action: %w", err)
		}
	}
	return nil
}
