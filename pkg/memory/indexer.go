package memory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MemoryIndexer watches directories and incrementally indexes markdown/jsonl files.
// It uses polling (os.Stat) instead of filesystem events to avoid external dependencies.
type MemoryIndexer struct {
	svc      *Service
	paths    []string
	interval time.Duration
	logger   *slog.Logger
	done     chan struct{}
	wg       sync.WaitGroup

	mu      sync.Mutex
	lastMod map[string]time.Time // path → last known mtime (debounce)
	started bool
	stopped bool
}

const (
	defaultIndexInterval = 5 * time.Minute
	maxChunkChars        = 2000 // ~500 tokens approximation
	chunkOverlapChars    = 100
	jsonlMaxLines        = 100
	indexerSource        = "indexer"
)

var headingRe = regexp.MustCompile(`(?m)^#{1,6}\s+`)

func NewMemoryIndexer(svc *Service, paths []string, logger *slog.Logger) *MemoryIndexer {
	if logger == nil {
		logger = slog.Default()
	}
	if len(paths) == 0 {
		paths = []string{}
	}
	return &MemoryIndexer{
		svc:      svc,
		paths:    paths,
		interval: defaultIndexInterval,
		logger:   logger.With("component", "indexer"),
		done:     make(chan struct{}),
		lastMod:  make(map[string]time.Time),
	}
}

// SetInterval changes the polling interval. Must be called before Start.
func (idx *MemoryIndexer) SetInterval(d time.Duration) {
	idx.interval = d
}

// Start performs initial indexing of all watched paths, then starts a
// background goroutine that polls for changes.
func (idx *MemoryIndexer) Start() {
	idx.mu.Lock()
	if idx.started {
		idx.mu.Unlock()
		return
	}
	idx.started = true
	idx.mu.Unlock()

	ctx := context.Background()

	// Initial full index
	for _, p := range idx.paths {
		if err := idx.indexPath(ctx, p); err != nil {
			idx.logger.Warn("initial index failed", "path", p, "error", err)
		}
	}

	// Background polling
	idx.wg.Add(1)
	go func() {
		defer idx.wg.Done()
		ticker := time.NewTicker(idx.interval)
		defer ticker.Stop()

		for {
			select {
			case <-idx.done:
				return
			case <-ticker.C:
				for _, p := range idx.paths {
					if err := idx.indexPath(ctx, p); err != nil {
						idx.logger.Warn("polling index failed", "path", p, "error", err)
					}
				}
			}
		}
	}()
}

// Stop terminates the background polling goroutine.
func (idx *MemoryIndexer) Stop() {
	idx.mu.Lock()
	if idx.stopped {
		idx.mu.Unlock()
		return
	}
	idx.stopped = true
	idx.mu.Unlock()

	close(idx.done)
	idx.wg.Wait()
}

// IndexNow triggers immediate indexing of a specific file or directory.
func (idx *MemoryIndexer) IndexNow(path string) error {
	return idx.indexPath(context.Background(), path)
}

// indexPath recursively walks a directory (or processes a single file) and
// indexes supported files.
func (idx *MemoryIndexer) indexPath(ctx context.Context, root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", root, err)
	}

	if !info.IsDir() {
		return idx.maybeIndexFile(ctx, root, info)
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip errors, continue walking
		}
		if d.IsDir() {
			return nil
		}
		if !isSupportedExt(path) {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		return idx.maybeIndexFile(ctx, path, fi)
	})
}

// maybeIndexFile checks mtime debounce then SHA-256 before indexing.
func (idx *MemoryIndexer) maybeIndexFile(ctx context.Context, path string, fi os.FileInfo) error {
	idx.mu.Lock()
	last, seen := idx.lastMod[path]
	idx.mu.Unlock()

	if seen && !fi.ModTime().After(last) {
		return nil
	}

	idx.mu.Lock()
	idx.lastMod[path] = fi.ModTime()
	idx.mu.Unlock()

	hash, err := calculateSHA256(path)
	if err != nil {
		return fmt.Errorf("sha256 %s: %w", path, err)
	}

	stored, err := idx.getStoredHash(path)
	if err != nil {
		return fmt.Errorf("check stored hash %s: %w", path, err)
	}
	if stored == hash {
		return nil
	}

	if err := idx.indexFile(ctx, path); err != nil {
		return fmt.Errorf("index %s: %w", path, err)
	}

	if err := idx.updateIndexedFile(path, hash); err != nil {
		idx.logger.Warn("failed to update indexed_files", "path", path, "error", err)
	}

	idx.logger.Info("indexed file", "path", path)
	return nil
}

// indexFile reads a file, chunks it, and saves each chunk.
func (idx *MemoryIndexer) indexFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	if !isText(data) {
		return nil
	}

	content := string(data)
	if strings.TrimSpace(content) == "" {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	dir := filepath.Dir(path)

	var chunks []string

	switch ext {
	case ".md", ".mdc":
		chunks = chunkMarkdown(content, maxChunkChars, chunkOverlapChars)
	case ".jsonl":
		chunks = chunkJSONL(content, jsonlMaxLines)
	default:
		return nil
	}

	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if err := idx.svc.SaveRaw(ctx, chunk, "fact", indexerSource, dir); err != nil {
			idx.logger.Warn("failed to save chunk", "path", path, "error", err)
		}
	}

	return nil
}

func (idx *MemoryIndexer) getStoredHash(path string) (string, error) {
	var hash string
	err := idx.svc.StoreDB().QueryRow(
		"SELECT sha256 FROM indexed_files WHERE path = ?", path,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

func (idx *MemoryIndexer) updateIndexedFile(path, hash string) error {
	_, err := idx.svc.StoreDB().Exec(
		`INSERT INTO indexed_files (path, sha256, indexed_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(path) DO UPDATE SET sha256 = excluded.sha256, indexed_at = CURRENT_TIMESTAMP`,
		path, hash,
	)
	return err
}

func chunkMarkdown(content string, maxChars, overlapChars int) []string {
	sections := splitByHeadings(content)
	var chunks []string

	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		if len(section) <= maxChars {
			chunks = append(chunks, section)
			continue
		}
		// Split oversized section
		for start := 0; start < len(section); {
			end := start + maxChars
			if end > len(section) {
				end = len(section)
			}
			chunks = append(chunks, section[start:end])
			start = end - overlapChars
			if start >= len(section) {
				break
			}
			if start < 0 {
				start = 0
			}
		}
	}

	return chunks
}

func splitByHeadings(content string) []string {
	indices := headingRe.FindAllStringIndex(content, -1)
	if len(indices) == 0 {
		return []string{content}
	}

	var sections []string
	if indices[0][0] > 0 {
		pre := strings.TrimSpace(content[:indices[0][0]])
		if pre != "" {
			sections = append(sections, pre)
		}
	}

	for i, loc := range indices {
		start := loc[0]
		var end int
		if i+1 < len(indices) {
			end = indices[i+1][0]
		} else {
			end = len(content)
		}
		section := strings.TrimSpace(content[start:end])
		if section != "" {
			sections = append(sections, section)
		}
	}

	return sections
}

func chunkJSONL(content string, maxLines int) []string {
	lines := strings.Split(content, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < maxLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			kept = append(kept, line)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if len(kept) == 0 {
		return nil
	}
	return []string{strings.Join(kept, "\n")}
}

func calculateSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	reader := bufio.NewReader(f)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func isSupportedExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".mdc" || ext == ".jsonl"
}

func isText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}
