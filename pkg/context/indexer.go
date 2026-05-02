package ctx

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Indexer orchestrates chunking and store insertion with content-hash deduplication.
type Indexer struct {
	store      *Store
	chunker    *Chunker
	db         *sql.DB
	logger     *slog.Logger
	defaultTTL int
}

// NewIndexer creates an Indexer. If defaultTTL <= 0, 336 (14 days) is used.
// If logger is nil, slog.Default() is used.
func NewIndexer(store *Store, chunker *Chunker, db *sql.DB, defaultTTL int, logger *slog.Logger) *Indexer {
	if logger == nil {
		logger = slog.Default()
	}
	if defaultTTL <= 0 {
		defaultTTL = 336
	}
	return &Indexer{
		store:      store,
		chunker:    chunker,
		db:         db,
		logger:     logger,
		defaultTTL: defaultTTL,
	}
}

// IndexContent chunks markdown content by headings and inserts unique chunks into the store.
// Returns a sourceGroupID linking all chunks from this indexing operation.
func (ix *Indexer) IndexContent(ctx context.Context, content string, source string, label string, sessionID string, contentType string) (string, error) {
	if content == "" {
		return "", nil
	}

	sourceGroupID := newID()
	chunks := ix.chunker.Chunk([]byte(content))

	for _, cd := range chunks {
		if cd.Content == "" {
			continue
		}

		contentHash := sha256Hex(cd.Content)
		dup, err := ix.isDuplicate(ctx, contentHash)
		if err != nil {
			return "", fmt.Errorf("dedup check: %w", err)
		}
		if dup {
			ix.logger.Debug("skipping duplicate chunk", "hash", contentHash)
			continue
		}

		meta, _ := json.Marshal(indexerMetadata{
			ContentHash:   contentHash,
			SourceGroupID: sourceGroupID,
		})

		chunkLabel := label
		if cd.Heading != "" {
			chunkLabel = cd.Heading
		}

		chunk := &Chunk{
			SessionID:   sessionID,
			Source:      source,
			Label:       chunkLabel,
			Content:     cd.Content,
			Metadata:    string(meta),
			ContentType: contentType,
			IndexedAt:   time.Now(),
			TTLHours:    ix.defaultTTL,
		}

		if err := ix.store.InsertChunk(ctx, chunk); err != nil {
			return "", fmt.Errorf("insert chunk: %w", err)
		}
	}

	return sourceGroupID, nil
}

// IndexRaw indexes non-markdown content (shell output, etc.) by splitting at newline boundaries.
// Returns a sourceGroupID linking all chunks from this indexing operation.
func (ix *Indexer) IndexRaw(ctx context.Context, content string, source string, label string, sessionID string) (string, error) {
	if content == "" {
		return "", nil
	}

	sourceGroupID := newID()
	parts := splitRawContent(content, ix.chunker.maxBytes)

	for _, part := range parts {
		if part == "" {
			continue
		}

		contentHash := sha256Hex(part)
		dup, err := ix.isDuplicate(ctx, contentHash)
		if err != nil {
			return "", fmt.Errorf("dedup check: %w", err)
		}
		if dup {
			ix.logger.Debug("skipping duplicate chunk", "hash", contentHash)
			continue
		}

		meta, _ := json.Marshal(indexerMetadata{
			ContentHash:   contentHash,
			SourceGroupID: sourceGroupID,
		})

		chunk := &Chunk{
			SessionID:   sessionID,
			Source:      source,
			Label:       label,
			Content:     part,
			Metadata:    string(meta),
			ContentType: "code",
			IndexedAt:   time.Now(),
			TTLHours:    ix.defaultTTL,
		}

		if err := ix.store.InsertChunk(ctx, chunk); err != nil {
			return "", fmt.Errorf("insert chunk: %w", err)
		}
	}

	return sourceGroupID, nil
}

func (ix *Indexer) isDuplicate(ctx context.Context, contentHash string) (bool, error) {
	var id string
	err := ix.db.QueryRowContext(ctx,
		`SELECT id FROM content_chunks WHERE metadata LIKE '%' || ? || '%' LIMIT 1`,
		contentHash,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

type indexerMetadata struct {
	ContentHash   string `json:"content_hash"`
	SourceGroupID string `json:"source_group_id,omitempty"`
}

func sha256Hex(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

// splitRawContent splits raw text at newline boundaries, keeping each chunk
// at or below maxBytes.
func splitRawContent(content string, maxBytes int) []string {
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	if len(content) <= maxBytes {
		return []string{content}
	}

	var parts []string
	lines := strings.Split(content, "\n")
	var buf strings.Builder

	for _, line := range lines {
		lineLen := len(line) + 1
		if buf.Len() > 0 && buf.Len()+lineLen > maxBytes {
			parts = append(parts, strings.TrimRight(buf.String(), "\n"))
			buf.Reset()
		}

		if len(line) > maxBytes {
			if buf.Len() > 0 {
				parts = append(parts, strings.TrimRight(buf.String(), "\n"))
				buf.Reset()
			}
			parts = append(parts, line)
			continue
		}

		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
	}

	if buf.Len() > 0 {
		parts = append(parts, strings.TrimRight(buf.String(), "\n"))
	}

	return parts
}
