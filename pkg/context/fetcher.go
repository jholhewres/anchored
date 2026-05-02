package ctx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
)

const maxResponseBodySize = 10 * 1024 * 1024 // 10MB

type cacheEntry struct {
	result    *FetchResult
	fetchedAt time.Time
}

type Fetcher struct {
	client  *http.Client
	cache   map[string]cacheEntry
	cacheMu sync.RWMutex
	cacheTTL time.Duration
	logger  *slog.Logger
}

func NewFetcher(timeout, cacheTTL time.Duration, logger *slog.Logger) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Fetcher{
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return nil
			},
		},
		cache:    make(map[string]cacheEntry),
		cacheTTL: cacheTTL,
		logger:   logger,
	}
}

func (f *Fetcher) FetchAndConvert(ctx context.Context, url string) (*FetchResult, error) {
	// 1. Check cache
	f.cacheMu.RLock()
	if entry, ok := f.cache[url]; ok && time.Since(entry.fetchedAt) < f.cacheTTL {
		cached := *entry.result
		cached.FromCache = true
		f.cacheMu.RUnlock()
		return &cached, nil
	}
	f.cacheMu.RUnlock()

	// 2. Build request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Anchored/1.0")

	// 3. Execute request
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// 4. Size check
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if len(body) > maxResponseBodySize {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBodySize)
	}

	// 5. Convert based on content type
	contentType := resp.Header.Get("Content-Type")
	var markdown string

	switch {
	case strings.Contains(contentType, "text/html"):
		markdown, err = htmlToMarkdown(string(body))
		if err != nil {
			return nil, fmt.Errorf("converting HTML to markdown: %w", err)
		}
	case strings.Contains(contentType, "text/plain"), strings.Contains(contentType, "text/markdown"):
		markdown = string(body)
	case strings.Contains(contentType, "application/json"):
		var v interface{}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, fmt.Errorf("parsing JSON: %w", err)
		}
		pretty, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("pretty-printing JSON: %w", err)
		}
		markdown = string(pretty)
	default:
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}

	result := &FetchResult{
		Markdown:    markdown,
		ContentType: contentType,
		URL:         url,
		FetchedAt:   time.Now(),
		FromCache:   false,
	}

	// 6. Store in cache
	f.cacheMu.Lock()
	f.cache[url] = cacheEntry{result: result, fetchedAt: time.Now()}
	f.cacheMu.Unlock()

	return result, nil
}

func (f *Fetcher) ClearCache() {
	f.cacheMu.Lock()
	f.cache = make(map[string]cacheEntry)
	f.cacheMu.Unlock()
}

func htmlToMarkdown(html string) (string, error) {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
		),
	)
	result, err := conv.ConvertString(html)
	if err != nil {
		return "", err
	}
	return result, nil
}
