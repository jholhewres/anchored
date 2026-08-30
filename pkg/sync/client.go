package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// Client is a minimal HTTP client for the anchored_oss sync server.
// It is deliberately simple: no retries, no background goroutines, no auto-sync.
// All operations are explicit and require a server URL.
type Client struct {
	httpClient *http.Client
	serverURL  string
	apiKey     string
	clientID   string
}

const (
	maxProjectSkillsResponseBytes = 256 * 1024
	maxProjectSkillResponseBytes  = 1024 * 1024
)

// NewClient creates a sync client from RemoteConfig.
// Returns nil when RemoteConfig.Enabled is false.
func NewClient(cfg config.RemoteConfig, clientID string) *Client {
	if !cfg.Enabled {
		return nil
	}
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		serverURL:  cfg.ServerURL,
		apiKey:     cfg.APIKey,
		clientID:   clientID,
	}
}

// HTTPError represents a non-2xx response from the sync server.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("sync server returned %d: %s", e.Status, e.Body)
}

type RemoteError struct {
	StatusCode int
	Body       string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote server returned %d: %s", e.StatusCode, e.Body)
}

// remoteErrorDetails carries additive response metadata without changing the
// exported RemoteError struct shape. Downstream callers may still use legacy
// positional literals such as RemoteError{status, body}.
type remoteErrorDetails struct {
	remote     *RemoteError
	code       string
	retryAfter string
}

func (e *remoteErrorDetails) Error() string { return e.remote.Error() }
func (e *remoteErrorDetails) Unwrap() error { return e.remote }

// RemoteErrorMetadata returns sanitized protocol metadata when the error came
// from an idempotent save or detailed search. Legacy errors return empty
// strings.
func RemoteErrorMetadata(err error) (code, retryAfter string) {
	var details *remoteErrorDetails
	if errors.As(err, &details) {
		return details.code, details.retryAfter
	}
	return "", ""
}

func IsRemoteForbidden(err error) bool {
	var re *RemoteError
	return errors.As(err, &re) && re.StatusCode == http.StatusForbidden
}

func IsRemoteUnavailable(err error) bool {
	var re *RemoteError
	return errors.As(err, &re) && re.StatusCode >= http.StatusInternalServerError
}

func NewClientFromEntry(entry config.RemoteEntry, clientID string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		serverURL:  entry.ServerURL,
		apiKey:     entry.APIKey,
		clientID:   clientID,
	}
}

// Push sends classified memories to the remote server.
// All memories are validated through RemoteSafetyFilter before sending using
// the same projectRoot the caller used for preview, so a memory classified as
// syncable cannot be silently re-blocked here. Personal preferences
// (PreferenceScope=="user") are rejected as a defense-in-depth net even if
// the caller forgot to pre-filter them.
//
// The caller SHOULD run ClassifyForPreview first and only push syncable
// memories; this method is the last line of defense.
func (c *Client) Push(ctx context.Context, req SyncPushRequest) (*SyncPushResponse, error) {
	filtered := make([]SyncMemory, 0, len(req.Memories))
	var rejections []string
	for i := range req.Memories {
		m := req.Memories[i]
		if m.Category == "event" || m.Category == "preference" {
			rejections = append(rejections, fmt.Sprintf("memory %s blocked: category_remote_blocked", m.ID))
			continue
		}

		if m.PreferenceScope == "user" {
			rejections = append(rejections, fmt.Sprintf("memory %s blocked: personal_preference", m.ID))
			continue
		}

		result := RemoteSafetyFilter(m.Content, nil, req.ProjectRoot)
		if result.Blocked {
			rejections = append(rejections, fmt.Sprintf("memory %s blocked: %s", m.ID, violationReason(result.Violations)))
			continue
		}
		// Always send the rewritten (safe) content, never the raw one.
		m.Content = result.Content
		filtered = append(filtered, m)
	}

	// Short-circuit when nothing survives the local filter — no point spending
	// a network round-trip and an auth token to push zero memories.
	if len(filtered) == 0 {
		return &SyncPushResponse{
			Accepted: 0,
			Rejected: len(rejections),
			Errors:   rejections,
		}, nil
	}

	// Advertise capabilities so the server returns its policy hints. The
	// presence of the object is the opt-in signal; the per-feature flags stay
	// false until a later wave implements them. Set centrally here so every
	// push path negotiates without each caller remembering to.
	if req.ClientCapabilities == nil {
		req.ClientCapabilities = &ClientCapabilities{}
	}

	// Partition into batches under the server's per-sync cap (default 500) so
	// a large store still syncs — an oversized single request is rejected
	// wholesale by the server. Batches are sent sequentially and their results
	// aggregated; the policy hint and resolved project come from the last one.
	agg := &SyncPushResponse{}
	for start := 0; start < len(filtered); start += maxPushBatch {
		end := start + maxPushBatch
		if end > len(filtered) {
			end = len(filtered)
		}
		batch := req
		batch.Memories = filtered[start:end]
		resp, err := c.pushBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		agg.Accepted += resp.Accepted
		agg.Rejected += resp.Rejected
		agg.Errors = append(agg.Errors, resp.Errors...)
		if resp.ProjectID != "" {
			agg.ProjectID = resp.ProjectID
		}
		if resp.Policy != nil {
			agg.Policy = resp.Policy
		}
	}

	if len(rejections) > 0 {
		agg.Rejected += len(rejections)
		agg.Errors = append(agg.Errors, rejections...)
	}

	return agg, nil
}

// maxPushBatch caps how many memories the client sends per request. It sits
// under the server's default per-sync cap (500) so a routine large-store push
// is partitioned client-side instead of being rejected wholesale.
const maxPushBatch = 400

// pushBatch sends one already-filtered batch to the compat push endpoint.
func (c *Client) pushBatch(ctx context.Context, req SyncPushRequest) (*SyncPushResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal push request: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "/api/v1/sync/push", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("push request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("push failed: %w", &HTTPError{Status: resp.StatusCode, Body: string(respBody)})
	}

	var pushResp SyncPushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pushResp); err != nil {
		return nil, fmt.Errorf("decode push response: %w", err)
	}
	return &pushResp, nil
}

// Pull fetches new/updated memories from the remote server since the given watermark.
// The response memories are not filtered — the server is trusted to send safe content.
func (c *Client) Pull(ctx context.Context, req SyncPullRequest) (*SyncPullResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal pull request: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, "/api/v1/sync/pull", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("pull request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull failed: %w", &HTTPError{Status: resp.StatusCode, Body: string(respBody)})
	}

	var pullResp SyncPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pullResp); err != nil {
		return nil, fmt.Errorf("decode pull response: %w", err)
	}

	return &pullResp, nil
}

func (c *Client) SaveRemote(ctx context.Context, mem RemoteMemory) (*SaveRemoteResponse, error) {
	return c.saveRemote(ctx, mem, "", false)
}

// SaveRemoteIdempotent saves a remote memory with an optional operation ID.
// A non-empty operation ID is sent as Idempotency-Key so callers can safely
// replay an outbox delivery after an ambiguous transport failure.
func (c *Client) SaveRemoteIdempotent(ctx context.Context, mem RemoteMemory, operationID string) (*SaveRemoteResponse, error) {
	return c.saveRemote(ctx, mem, operationID, true)
}

func (c *Client) saveRemote(ctx context.Context, mem RemoteMemory, operationID string, includeErrorMetadata bool) (*SaveRemoteResponse, error) {
	filtered := []string{"event", "preference"}
	for _, blocked := range filtered {
		if mem.Category == blocked {
			return nil, fmt.Errorf("category %q blocked for remote save", mem.Category)
		}
	}

	result := RemoteSafetyFilter(mem.Content, nil, "")
	if result.Blocked {
		return nil, fmt.Errorf("content blocked by safety filter: %s", violationReason(result.Violations))
	}
	mem.Content = result.Content

	body, err := json.Marshal(mem)
	if err != nil {
		return nil, fmt.Errorf("marshal save request: %w", err)
	}

	headers := make(http.Header)
	if operationID != "" {
		headers.Set("Idempotency-Key", operationID)
	}
	resp, err := c.doRequestWithHeaders(ctx, http.MethodPost, "/v1/memories", bytes.NewReader(body), headers)
	if err != nil {
		return nil, fmt.Errorf("save request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, remoteErrorFromResponse(resp, respBody, includeErrorMetadata)
	}
	if resp.StatusCode >= 400 {
		return nil, remoteErrorFromResponse(resp, respBody, includeErrorMetadata)
	}

	var saveResp SaveRemoteResponse
	if err := json.Unmarshal(respBody, &saveResp); err != nil {
		return nil, fmt.Errorf("decode save response: %w", err)
	}
	return &saveResp, nil
}

func remoteErrorFromResponse(resp *http.Response, body []byte, includeMetadata bool) error {
	remoteErr := &RemoteError{
		StatusCode: resp.StatusCode,
		Body:       string(body),
	}
	if !includeMetadata {
		return remoteErr
	}
	return &remoteErrorDetails{
		remote:     remoteErr,
		code:       structuredRemoteErrorCode(body),
		retryAfter: safeRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func structuredRemoteErrorCode(body []byte) string {
	var payload struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	var nested struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(payload.Error, &nested) == nil && nested.Code != "" {
		return safeRemoteErrorCode(nested.Code)
	}
	return safeRemoteErrorCode(payload.Code)
}

func safeRemoteErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 128 {
		return ""
	}
	for i := range len(code) {
		c := code[i]
		if (c < 'a' || c > 'z') &&
			(c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') &&
			c != '_' && c != '-' && c != '.' {
			return ""
		}
	}
	return code
}

func safeRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	deltaSeconds := true
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			deltaSeconds = false
			break
		}
	}
	if deltaSeconds {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func (c *Client) SearchRemote(ctx context.Context, projectID string, query string, limit int) ([]RemoteSearchResult, error) {
	response, err := c.SearchRemoteDetailed(ctx, projectID, query, limit)
	if err != nil {
		var details *remoteErrorDetails
		if errors.As(err, &details) {
			// Preserve the legacy concrete error contract for callers that use
			// a direct type assertion instead of errors.As.
			return nil, details.remote
		}
		return nil, err
	}
	if response.Results == nil {
		return nil, nil
	}
	results := make([]RemoteSearchResult, len(response.Results))
	for i := range response.Results {
		hit := response.Results[i]
		results[i] = RemoteSearchResult{
			ID: hit.ID, Category: hit.Category, Content: hit.Content,
			ProjectID: hit.ProjectID, Source: hit.Source,
			AuthorName: hit.AuthorName, UpdatedAt: hit.UpdatedAt,
		}
	}
	return results, nil
}

// SearchRemoteDetailed preserves effective-mode telemetry even when the
// server returns no hits. Only the declared semantic_unavailable capability
// response permits the single lexical fallback.
func (c *Client) SearchRemoteDetailed(ctx context.Context, projectID string, query string, limit int) (*RemoteSearchResponse, error) {
	response, err := c.searchRemoteMode(ctx, projectID, query, limit, "semantic")
	if isSemanticUnavailable(err) {
		response, err = c.searchRemoteMode(ctx, projectID, query, limit, "text")
		response.RequestedMode = "semantic"
		response.Fallback = true
		response.FallbackReason = "semantic_unavailable"
	}
	return response, err
}

// SearchProjectSkills returns compact descriptors for active skills attached
// to one remote project. The optional intent is passed as q; this endpoint is
// intentionally separate from memory search so skills never enter sync data.
func (c *Client) SearchProjectSkills(ctx context.Context, projectID, intent string) ([]RemoteSkillDescriptor, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	path := "/v1/projects/" + urlQueryEscape(projectID) + "/skills"
	if intent != "" {
		path += "?q=" + urlQueryEscape(intent)
	}
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("search skills request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readRemoteResponseBody(resp.Body, maxProjectSkillsResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read skills response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &RemoteError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var skills []RemoteSkillDescriptor
	if err := json.Unmarshal(body, &skills); err != nil {
		return nil, fmt.Errorf("decode skills response: %w", err)
	}
	return skills, nil
}

// LoadProjectSkill loads one active, attached skill body by its stable slug.
// It does not cache or otherwise persist the Markdown instruction content.
func (c *Client) LoadProjectSkill(ctx context.Context, projectID, slug string) (*RemoteSkill, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	if strings.TrimSpace(slug) == "" {
		return nil, fmt.Errorf("skill slug is required")
	}
	requestedSlug := strings.TrimSpace(slug)
	path := "/v1/projects/" + urlQueryEscape(projectID) + "/skills/" + urlQueryEscape(requestedSlug)
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("load skill request: %w", err)
	}
	defer resp.Body.Close()

	body, err := readRemoteResponseBody(resp.Body, maxProjectSkillResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read skill response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &RemoteError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var skill RemoteSkill
	if err := json.Unmarshal(body, &skill); err != nil {
		return nil, fmt.Errorf("decode skill response: %w", err)
	}
	if err := validateRemoteSkill(&skill, requestedSlug); err != nil {
		return nil, fmt.Errorf("invalid skill response: %w", err)
	}
	return &skill, nil
}

func readRemoteResponseBody(r io.Reader, maxBytes int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d-byte limit", maxBytes)
	}
	return body, nil
}

func validateRemoteSkill(skill *RemoteSkill, requestedSlug string) error {
	if skill == nil {
		return fmt.Errorf("empty response")
	}
	if strings.TrimSpace(skill.Slug) == "" {
		return fmt.Errorf("missing slug")
	}
	if skill.Slug != requestedSlug {
		return fmt.Errorf("slug does not match requested skill")
	}
	if skill.Status != "active" {
		return fmt.Errorf("skill is not active")
	}
	if skill.Version <= 0 {
		return fmt.Errorf("missing version")
	}
	if skill.Content == "" {
		return fmt.Errorf("missing content")
	}
	hash, err := canonicalSkillContentHash(skill.ContentHash)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(skill.Content))
	if hash != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("content hash does not match content")
	}
	skill.ContentHash = hash
	return nil
}

func canonicalSkillContentHash(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) >= len("sha256:") && strings.EqualFold(value[:len("sha256:")], "sha256:") {
		value = value[len("sha256:"):]
	}
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("invalid content hash")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("invalid content hash")
	}
	return strings.ToLower(value), nil
}

// searchRemoteMode performs exactly one remote search attempt. Keeping the
// retry policy in SearchRemote makes it explicit that only a declared semantic
// capability failure may change retrieval modes.
func (c *Client) searchRemoteMode(ctx context.Context, projectID string, query string, limit int, mode string) (*RemoteSearchResponse, error) {
	metadata := &RemoteSearchResponse{
		RequestedMode: mode,
		EffectiveMode: mode,
	}
	url := fmt.Sprintf("/v1/memories/search?project_id=%s&q=%s&limit=%d&mode=%s",
		urlQueryEscape(projectID),
		urlQueryEscape(query),
		limit,
		urlQueryEscape(mode),
	)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metadata, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if effectiveMode := strings.TrimSpace(resp.Header.Get("X-Anchored-Effective-Mode")); effectiveMode != "" {
		metadata.EffectiveMode = effectiveMode
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return metadata, remoteErrorFromResponse(resp, respBody, true)
	}
	if resp.StatusCode >= 400 {
		return metadata, remoteErrorFromResponse(resp, respBody, true)
	}

	var results []RemoteSearchHit
	if err := json.Unmarshal(respBody, &results); err != nil {
		return metadata, fmt.Errorf("decode search response: %w", err)
	}
	effectiveMode := strings.TrimSpace(resp.Header.Get("X-Anchored-Effective-Mode"))
	if effectiveMode == "" && len(results) > 0 {
		effectiveMode = results[0].EffectiveMode
	}
	if effectiveMode == "" {
		// Older servers do not emit mode metadata. The requested wire mode is
		// the safest compatibility inference; it does not imply a fallback.
		effectiveMode = mode
	}
	metadata.Results = results
	metadata.EffectiveMode = effectiveMode
	return metadata, nil
}

// isSemanticUnavailable recognizes the protocol's single safe lexical
// fallback signal. Status alone is deliberately insufficient: validation,
// authorization, rate-limit, transport, and server failures must retain their
// original semantics rather than silently changing the search mode.
func isSemanticUnavailable(err error) bool {
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) || remoteErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}

	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(remoteErr.Body), &payload) == nil &&
		payload.Error.Code == "semantic_unavailable"
}

// PushTriples sends a batch of knowledge-graph triples to the remote server
// for a previously-resolved project. The server applies the same hardening
// rules as anchored_oss does for memories (logical dedup, functional
// supersession, alias resolution).
//
// Caller is expected to have already resolved the remote projectID via a
// memory sync (the server's project_claim flow). PushTriples does not perform
// safety filtering — triples are entity strings, not free text, and the
// server's quality/policy filter doesn't apply to them.
func (c *Client) PushTriples(ctx context.Context, projectID string, triples []SyncTriple) (*SyncTripleResponse, error) {
	if projectID == "" {
		return nil, fmt.Errorf("PushTriples: projectID is required")
	}
	if len(triples) == 0 {
		return &SyncTripleResponse{}, nil
	}

	body, err := json.Marshal(SyncTripleRequest{Triples: triples})
	if err != nil {
		return nil, fmt.Errorf("marshal triples request: %w", err)
	}

	path := "/v1/projects/" + urlQueryEscape(projectID) + "/triples"
	resp, err := c.doRequest(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("push triples request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, &RemoteError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	if resp.StatusCode >= 400 {
		return nil, &RemoteError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var out SyncTripleResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode triples response: %w", err)
	}
	return &out, nil
}

// RemoteProject is a minimal view of a project as returned by GET /v1/projects.
type RemoteProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	RemoteKey string `json:"remote_key"`
	// RemoteKeyV1 is the legacy routing key servers >= v0.4.7 expose alongside
	// the canonical one; empty on older servers.
	RemoteKeyV1 string `json:"remote_key_v1"`
}

// GetProjectByID returns the listed project with the given ID, or nil when the
// listing doesn't contain it (or fails). Used to verify that a forced/linked
// target project actually belongs to the repository being synced.
func (c *Client) GetProjectByID(ctx context.Context, id string) *RemoteProject {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil
	}
	for i := range projects {
		if projects[i].ID == id {
			return &projects[i]
		}
	}
	return nil
}

// ListProjects returns the projects the configured API key can access on the
// remote server. Used by `remote sync` to validate linked project IDs (and pick
// a live default) instead of blindly trusting the local link list, which can go
// stale when the server's projects change.
func (c *Client) ListProjects(ctx context.Context) ([]RemoteProject, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/v1/projects", nil)
	if err != nil {
		return nil, fmt.Errorf("list projects request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, &RemoteError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var projects []RemoteProject
	if err := json.Unmarshal(body, &projects); err != nil {
		return nil, fmt.Errorf("decode projects response: %w", err)
	}
	return projects, nil
}

// ResolveProjectIDByRemoteKey returns the ID of the remote project whose
// remote_key matches, or "" when none does (or the listing fails). Thin
// wrapper over ResolveProjectIDByRemoteKeys for single-key callers.
func (c *Client) ResolveProjectIDByRemoteKey(ctx context.Context, remoteKey string) string {
	pid, _ := c.ResolveProjectIDByRemoteKeys(ctx, remoteKey)
	return pid
}

// ResolveProjectIDByRemoteKeys probes the remote project listing for the first
// of keys (in order) that matches, returning the project ID and the key that
// matched, or ("", "") when none does (or the listing fails). The listing is
// fetched once and matched in memory, so passing multiple keys (e.g. canonical
// then legacy) costs a single round-trip. Empty keys are skipped.
func (c *Client) ResolveProjectIDByRemoteKeys(ctx context.Context, keys ...string) (projectID, matchedKey string) {
	hasKey := false
	for _, k := range keys {
		if k != "" {
			hasKey = true
			break
		}
	}
	if !hasKey {
		return "", ""
	}
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return "", ""
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		for _, p := range projects {
			if p.RemoteKey == k {
				return p.ID, k
			}
		}
	}
	return "", ""
}

func urlQueryEscape(s string) string {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isURLSafe(c) {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

func isURLSafe(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doRequestWithHeaders(ctx, method, path, body, nil)
}

func (c *Client) doRequestWithHeaders(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	url := c.serverURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return c.httpClient.Do(req)
}
