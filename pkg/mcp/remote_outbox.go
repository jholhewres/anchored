package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

const autoRouteOutboxKind = "anchored.auto-route.v1"

// remoteOutboxEnvelope keeps automatic routing intent durable without doing
// network I/O before the local SQLite commit. The worker resolves the current
// server/project mapping immediately before delivery.
type remoteOutboxEnvelope struct {
	Kind   string                  `json:"kind"`
	CWD    string                  `json:"cwd"`
	Memory remotesync.RemoteMemory `json:"memory"`
}

func (s *Server) saveRemoteIdempotent(
	ctx context.Context,
	item memory.RemoteOutboxItem,
) memory.RemoteOutboxDeliveryResult {
	var payload remotesync.RemoteMemory
	var envelope remoteOutboxEnvelope
	if err := json.Unmarshal(item.Payload, &envelope); err == nil &&
		envelope.Kind == autoRouteOutboxKind {
		r := s.resolveAutoRemoteWriteTarget(ctx, envelope.CWD)
		if r.Held() {
			return memory.RemoteOutboxDeliveryResult{Held: true, HeldReason: r.HeldReason()}
		}
		entry, projectID := r.Entry, r.ProjectID
		if entry == nil || projectID == "" {
			return memory.RemoteOutboxDeliveryResult{
				TransportError: fmt.Errorf("automatic remote project is not currently resolvable"),
			}
		}
		if !entry.AutoSyncEnabled() {
			return memory.RemoteOutboxDeliveryResult{
				StatusCode: http.StatusNoContent,
				Ack:        []byte(`{"skipped":"auto_sync_disabled"}`),
			}
		}
		payload = envelope.Memory
		payload.ProjectID = projectID
		result := s.deliverRemoteOutboxPayload(ctx, item, *entry, payload)
		switch {
		case result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden:
			// A refused credential holds the repository's writes, as it does
			// at resolution, rather than dead-lettering them. Outside a
			// repository there is nothing to hold per repository, so the
			// refusal fails as before.
			if _, _, inRepo := s.repositoryRemoteKeys(envelope.CWD); inRepo {
				refused := remotesync.ProjectResolution{HeldBy: entry.Name, HeldStatus: result.StatusCode}
				s.holdAutoRemoteTarget(envelope.CWD, refused)
				return memory.RemoteOutboxDeliveryResult{Held: true, HeldReason: refused.HeldReason()}
			}
		case result.TransportError != nil || result.StatusCode >= http.StatusInternalServerError:
			// The remote may have gone down since it was resolved: the next
			// attempt resolves again, and is held if it is still down.
			s.forgetAutoRemoteTarget(envelope.CWD)
		}
		return result
	}

	entry := s.remoteEntryForOutbox(item.Remote)
	if entry == nil {
		return memory.RemoteOutboxDeliveryResult{
			TransportError: fmt.Errorf("remote %q is not configured", item.Remote),
		}
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return memory.RemoteOutboxDeliveryResult{
			StatusCode: http.StatusUnprocessableEntity,
			Ack:        []byte(err.Error()),
		}
	}
	return s.deliverRemoteOutboxPayload(ctx, item, *entry, payload)
}

func (s *Server) deliverRemoteOutboxPayload(
	ctx context.Context,
	item memory.RemoteOutboxItem,
	entry config.RemoteEntry,
	payload remotesync.RemoteMemory,
) memory.RemoteOutboxDeliveryResult {
	client := remotesync.NewClientFromEntry(entry, "mcp")
	response, err := client.SaveRemoteIdempotent(ctx, payload, item.OperationID)
	if err != nil {
		var remoteErr *remotesync.RemoteError
		if !errors.As(err, &remoteErr) {
			return memory.RemoteOutboxDeliveryResult{TransportError: err}
		}
		_, retryAfter := remotesync.RemoteErrorMetadata(err)
		payloadSnapshot, _ := json.Marshal(payload)
		payloadSum := sha256.Sum256(payloadSnapshot)
		return memory.RemoteOutboxDeliveryResult{
			StatusCode: remoteErr.StatusCode,
			Ack:        []byte(remoteErr.Body),
			RetryAfter: parseRetryAfter(retryAfter, time.Now().UTC()),
			SamePayloadConflict: samePayloadConflict(
				[]byte(remoteErr.Body),
				hex.EncodeToString(payloadSum[:]),
			),
		}
	}
	ack, err := json.Marshal(response)
	if err != nil {
		return memory.RemoteOutboxDeliveryResult{TransportError: err}
	}
	return memory.RemoteOutboxDeliveryResult{
		StatusCode: http.StatusCreated,
		Ack:        ack,
	}
}

func (s *Server) remoteEntryForOutbox(remote string) *config.RemoteEntry {
	if s.cfg == nil {
		return nil
	}
	for name, configured := range s.cfg.Remotes {
		entry := configured
		if entry.Name == "" {
			entry.Name = name
		}
		if remote == name || remote == entry.Name || remote == entry.ServerURL {
			return &entry
		}
	}
	return nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func samePayloadConflict(body []byte, payloadHash string) bool {
	var response struct {
		PayloadHash         string `json:"payload_hash"`
		ExistingPayloadHash string `json:"existing_payload_hash"`
		Error               struct {
			PayloadHash         string `json:"payload_hash"`
			ExistingPayloadHash string `json:"existing_payload_hash"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	for _, candidate := range []string{
		response.PayloadHash,
		response.ExistingPayloadHash,
		response.Error.PayloadHash,
		response.Error.ExistingPayloadHash,
	} {
		if candidate != "" && candidate == payloadHash {
			return true
		}
	}
	return false
}
