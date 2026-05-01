package capture

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// ToolCall represents a single tool call during a session.
type ToolCall struct {
	Tool      string
	Input     string
	Output    string
	Timestamp time.Time
}

// AutoCaptureManager handles automatic session summary capture with quality gates.
type AutoCaptureManager struct {
	store     SaveStorer
	extractor *SummaryExtractor
	sanitizer *memory.Sanitizer
	logger    *slog.Logger
}

// SaveStorer is the subset of memory.Service needed for auto-capture.
type SaveStorer interface {
    SaveWithOptions(ctx context.Context, opts memory.SaveOptions) (*memory.Memory, error)
}

// NewAutoCaptureManager creates a new auto-capture manager.
func NewAutoCaptureManager(store SaveStorer, extractor *SummaryExtractor, sanitizer *memory.Sanitizer, logger *slog.Logger) *AutoCaptureManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &AutoCaptureManager{
		store:     store,
		extractor: extractor,
		sanitizer: sanitizer,
		logger:    logger,
	}
}

// CaptureSession processes session tool calls and saves a summary if quality is sufficient.
func (m *AutoCaptureManager) CaptureSession(ctx context.Context, sessionID string, toolCalls []ToolCall, cwd string) error {
	if len(toolCalls) == 0 {
		m.logger.Debug("auto-capture: empty session, skipping", "session_id", sessionID)
		return nil
	}

	// 1. Concatenate tool call inputs/outputs
	var parts []string
	for _, tc := range toolCalls {
		if tc.Input != "" {
			parts = append(parts, tc.Input)
		}
		if tc.Output != "" {
			parts = append(parts, tc.Output)
		}
	}
	content := strings.Join(parts, "\n")

	// 2. Sanitize (credential redaction)
	if m.sanitizer != nil {
		content = m.sanitizer.Sanitize(content)
	}

	// 3. Extract structured info
	result := m.extractor.Extract(content)
	if result == nil {
		m.logger.Debug("auto-capture: no extractable content", "session_id", sessionID)
		return nil
	}

	// 4. Quality gate: skip if score too low
	if result.QualityScore < 0.2 {
		m.logger.Debug("auto-capture: quality score below threshold", "session_id", sessionID, "score", result.QualityScore)
		return nil
	}

	// 5. Build formatted summary
	var summaryParts []string
	summaryParts = append(summaryParts, "## Session Summary\n")

	if len(result.Decisions) > 0 {
		summaryParts = append(summaryParts, "### Decisions")
		for _, d := range result.Decisions {
			summaryParts = append(summaryParts, "- "+d)
		}
	}

	if len(result.Facts) > 0 {
		summaryParts = append(summaryParts, "### Key Facts")
		for _, f := range result.Facts {
			summaryParts = append(summaryParts, "- "+f)
		}
	}

	if len(result.Topics) > 0 {
		summaryParts = append(summaryParts, "### Topics")
		for _, t := range result.Topics {
			summaryParts = append(summaryParts, "- "+t)
		}
	}

	summary := strings.Join(summaryParts, "\n")

	// 6. Length gate: skip if too short
	if len(summary) < 50 {
		m.logger.Debug("auto-capture: summary too short", "session_id", sessionID, "len", len(summary))
		return nil
	}

	// 7. Build metadata
	_ = memory.MemoryMetadata{
		Source:        "auto_capture",
		SessionID:     sessionID,
		CaptureReason: fmt.Sprintf("session end (quality=%.2f, decisions=%d, facts=%d)", result.QualityScore, len(result.Decisions), len(result.Facts)),
		QualityScore:  result.QualityScore,
	}

	// 8. Save via SaveWithOptions (handles dedup via content_hash)
	_, err := m.store.SaveWithOptions(ctx, memory.SaveOptions{
		Content:  summary,
		Category: "summary",
		Source:   "auto_capture",
		CWD:      cwd,
	})

	if err != nil {
		return fmt.Errorf("auto-capture save: %w", err)
	}

	m.logger.Info("auto-capture: session summary saved",
		"session_id", sessionID,
		"quality_score", result.QualityScore,
		"decisions", len(result.Decisions),
		"facts", len(result.Facts),
	)

	return nil
}
