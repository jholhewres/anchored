package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	processingLease     = 30 * time.Second
	processingPollEvery = 250 * time.Millisecond
	workerFinalizeLimit = 5 * time.Second
	embeddingJobKind    = "embedding"
)

func (s *Service) durableProcessingSpecs(skipEmbed bool) []ProcessingJobSpec {
	if _, ok := s.store.(ProcessingQueueStore); !ok {
		return nil
	}
	var specs []ProcessingJobSpec
	if !skipEmbed && s.embedder != nil {
		generation := s.currentEmbeddingGenerationID()
		if _, generationAware := s.store.(EmbeddingGenerationStore); generationAware && generation == "" {
			if err := s.ensureCurrentEmbeddingGeneration(context.Background()); err != nil {
				s.logger.Warn("initialize embedding generation for durable write failed", "error", err)
			}
			generation = s.currentEmbeddingGenerationID()
		}
		if generation == "" {
			// Compatibility for non-generation queue stores.
			generation = s.embedder.Model()
		}
		specs = append(specs, ProcessingJobSpec{
			Kind: embeddingJobKind, Generation: generation,
		})
	}
	return specs
}

func (s *Service) ensureDurableWorkers() {
	if s.store == nil {
		return
	}
	_, processing := s.store.(ProcessingQueueStore)
	_, outbox := s.store.(RemoteOutboxStore)
	if !processing && !outbox {
		return
	}
	if s.embedder != nil && s.currentEmbeddingGenerationID() == "" {
		if _, ok := s.store.(EmbeddingGenerationStore); ok {
			if err := s.ensureCurrentEmbeddingGeneration(context.Background()); err != nil {
				if s.logger == nil {
					s.logger = slog.Default()
				}
				s.logger.Warn("initialize embedding generation for worker failed", "error", err)
			}
		}
	}
	s.workerOnce.Do(func() {
		if s.logger == nil {
			s.logger = slog.Default()
		}
		if s.shutdown == nil {
			s.shutdown = make(chan struct{})
		}
		s.workerWake = make(chan struct{}, 1)
		s.workerOwner = newUUID()
		s.logDerivedWorkHealth()
		s.wg.Add(1)
		go s.runDurableWorkers()
	})
	s.signalDurableWorkers()
}

func (s *Service) logDerivedWorkHealth() {
	healthStore, ok := s.store.(DerivedWorkHealthStore)
	if !ok {
		return
	}
	health, err := healthStore.DerivedWorkHealth(context.Background())
	if err != nil {
		s.logger.Warn("derived work health unavailable", "error", err)
		return
	}
	now := time.Now().UTC()
	oldestAge := func(at *time.Time) time.Duration {
		if at == nil || at.After(now) {
			return 0
		}
		return now.Sub(*at).Round(time.Second)
	}
	s.logger.Info("durable derived work health",
		"processing_pending", health.Processing.Counts[string(JobPending)],
		"processing_active", health.Processing.Counts[string(JobProcessing)],
		"processing_failed", health.Processing.Counts[string(JobFailed)],
		"processing_oldest_pending_age", oldestAge(health.Processing.OldestPending),
		"outbox_pending", health.Outbox.Counts[string(OutboxPending)],
		"outbox_active", health.Outbox.Counts[string(OutboxProcessing)],
		"outbox_dead_letter", health.Outbox.Counts[string(OutboxDeadLetter)],
		"outbox_oldest_pending_age", oldestAge(health.Outbox.OldestPending),
	)
}

func (s *Service) signalDurableWorkers() {
	if s.workerWake == nil {
		return
	}
	select {
	case s.workerWake <- struct{}{}:
	default:
	}
}

func (s *Service) runDurableWorkers() {
	defer s.wg.Done()
	ticker := time.NewTicker(processingPollEvery)
	defer ticker.Stop()
	for {
		s.drainDurableWork()
		select {
		case <-s.shutdown:
			return
		case <-s.workerWake:
		case <-ticker.C:
		}
	}
}

func (s *Service) drainDurableWork() {
	ctx, cancel := context.WithTimeout(context.Background(), processingLease)
	defer cancel()
	if s.embedder != nil {
		s.drainProcessingKind(ctx, embeddingJobKind)
	}
	s.deliveryMu.RLock()
	hasDeliverer := s.remoteDeliverer != nil
	s.deliveryMu.RUnlock()
	if hasDeliverer {
		s.drainRemoteOutbox(ctx)
	}
}

func (s *Service) drainProcessingKind(ctx context.Context, kind string) {
	queue, ok := s.store.(ProcessingQueueStore)
	if !ok {
		return
	}
	inputs, ok := s.store.(ProcessingRevisionStore)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		now := time.Now().UTC()
		job, err := queue.ClaimProcessingJob(ctx, kind, s.workerOwner, now, processingLease)
		if err != nil {
			s.logger.Warn("claim durable processing job failed", "kind", kind, "error", err)
			return
		}
		if job == nil {
			return
		}
		if err := s.processClaimedJob(ctx, inputs, job); err != nil {
			retryAt := processingRetryAt(now, job.Attempts)
			finalizeCtx, finalizeCancel := context.WithTimeout(
				context.Background(),
				workerFinalizeLimit,
			)
			if _, failErr := queue.FailProcessingJob(
				finalizeCtx, job.ID, s.workerOwner, err.Error(), now, retryAt,
			); failErr != nil {
				s.logger.Warn("record durable processing failure failed", "job_id", job.ID, "error", failErr)
			}
			finalizeCancel()
			continue
		}
		finalizeCtx, finalizeCancel := context.WithTimeout(
			context.Background(),
			workerFinalizeLimit,
		)
		if err := queue.CompleteProcessingJob(
			finalizeCtx,
			job.ID,
			s.workerOwner,
			time.Now().UTC(),
		); err != nil {
			s.logger.Warn("complete durable processing job failed", "job_id", job.ID, "error", err)
		}
		finalizeCancel()
	}
}

func (s *Service) processClaimedJob(
	ctx context.Context,
	inputs ProcessingRevisionStore,
	job *ProcessingJob,
) error {
	revision, current, err := inputs.ProcessingRevision(ctx, job.RevisionID)
	if err != nil {
		return err
	}
	if !current || revision.IsTombstone {
		return nil
	}
	switch job.Kind {
	case embeddingJobKind:
		if s.embedder == nil {
			return fmt.Errorf("embedding provider unavailable")
		}
		if generations, ok := s.store.(EmbeddingGenerationStore); ok {
			generation, err := generations.EmbeddingGeneration(ctx, job.Generation)
			if err != nil {
				return err
			}
			if generation == nil && job.Generation == s.embedder.Model() {
				// Upgrade queued work written by the pre-generation worker:
				// execute it in the now-identified current generation.
				generation, err = generations.EmbeddingGeneration(
					ctx, s.currentEmbeddingGenerationID(),
				)
				if err != nil {
					return err
				}
			}
			if generation == nil {
				return nil
			}
			identity, current := s.currentEmbeddingIdentity()
			if !current || !generation.Identity.Compatible(identity) ||
				generation.State == EmbeddingGenerationRetired {
				// Never ask the configured provider to produce vectors for an
				// unrelated semantic space. Reconciliation can requeue this
				// revision if that identity becomes current again.
				return nil
			}
			if err := s.embedRevisionForGeneration(ctx, generations, generation, revision); err != nil {
				return err
			}
			_, err = s.tryActivateEmbeddingGeneration(ctx, generations, generation.ID)
			return err
		}
		if s.cache == nil {
			return fmt.Errorf("embedding cache unavailable")
		}
		vecs, err := s.embedder.Embed(ctx, []string{revision.Memory.Content})
		if err != nil {
			return err
		}
		if len(vecs) == 0 {
			return fmt.Errorf("embedding provider returned no vectors")
		}
		if err := s.cache.Put(ctx, revision.Memory.Content, s.embedder.Model(), vecs[0], true); err != nil {
			return err
		}
		_, err = inputs.UpdateEmbeddingForRevision(
			ctx, revision.MemoryID, revision.RevisionID, vecs[0],
		)
		return err
	default:
		return fmt.Errorf("unsupported processing job kind %q", job.Kind)
	}
}

func processingRetryAt(now time.Time, attempt int) time.Time {
	delay := time.Second
	for i := 1; i < attempt && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	return now.Add(delay)
}

// SetRemoteOutboxDeliverer registers the transport adapter and immediately
// reconciles pending records left by an earlier process.
func (s *Service) SetRemoteOutboxDeliverer(deliverer RemoteOutboxDeliverer) {
	s.deliveryMu.Lock()
	s.remoteDeliverer = deliverer
	s.deliveryMu.Unlock()
	s.ensureDurableWorkers()
}

func (s *Service) drainRemoteOutbox(ctx context.Context) {
	queue, ok := s.store.(RemoteOutboxStore)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		now := time.Now().UTC()
		item, err := queue.ClaimRemoteOutbox(ctx, s.workerOwner, now, processingLease)
		if err != nil {
			s.logger.Warn("claim remote outbox failed", "error", err)
			return
		}
		if item == nil {
			return
		}
		s.deliveryMu.RLock()
		deliverer := s.remoteDeliverer
		s.deliveryMu.RUnlock()
		if deliverer == nil {
			_, _ = queue.FailRemoteOutbox(
				ctx, item.OperationID, s.workerOwner, "transport_unavailable",
				"remote outbox deliverer unavailable", now,
				OutboxRetryAtFor(item.OperationID, now, item.Attempts, 0), false,
			)
			return
		}
		result := deliverer(ctx, *item)
		disposition := ClassifyOutboxResult(
			result.StatusCode, result.SamePayloadConflict,
			result.TransportError != nil,
		)
		if disposition == OutboxDispositionDelivered {
			finalizeCtx, finalizeCancel := context.WithTimeout(
				context.Background(),
				workerFinalizeLimit,
			)
			if err := queue.DeliverRemoteOutbox(
				finalizeCtx,
				item.OperationID,
				s.workerOwner,
				result.Ack,
				time.Now().UTC(),
			); err != nil {
				s.logger.Warn("ack remote outbox failed", "operation_id", item.OperationID, "error", err)
			}
			finalizeCancel()
			continue
		}
		errorClass := "permanent_http"
		message := fmt.Sprintf("remote returned HTTP %d", result.StatusCode)
		permanent := disposition == OutboxDispositionPermanent
		if result.TransportError != nil {
			errorClass = "transport"
			message = result.TransportError.Error()
		} else if !permanent {
			errorClass = "retryable_http"
		} else if result.StatusCode == 409 {
			errorClass = "idempotency_conflict"
		}
		finalizeCtx, finalizeCancel := context.WithTimeout(
			context.Background(),
			workerFinalizeLimit,
		)
		_, err = queue.FailRemoteOutbox(
			finalizeCtx, item.OperationID, s.workerOwner, errorClass, message, now,
			OutboxRetryAtFor(
				item.OperationID, now, item.Attempts, result.RetryAfter,
			),
			permanent,
		)
		finalizeCancel()
		if err != nil {
			s.logger.Warn("record remote outbox failure failed", "operation_id", item.OperationID, "error", err)
		}
	}
}
