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
	// processingIdlePollMax caps the poll interval an idle worker backs off
	// to. A local save wakes the worker at once; the poll only picks up work
	// queued by other processes.
	processingIdlePollMax = 2 * time.Second
	// unloadedClaimGrace is how long a process whose model is unloaded leaves
	// a new job, queued by another process, to one that has it loaded. The
	// job still gets done if none claims it by then.
	unloadedClaimGrace  = time.Minute
	workerFinalizeLimit = 5 * time.Second
	embeddingJobKind    = "embedding"

	// processingMaxBatchPerWake bounds how many jobs one drain pass handles
	// before yielding, and processingBusyThrottle is the rest between
	// consecutive full batches. Together they duty-cycle the background worker so
	// a large backlog (e.g. a corpus-wide re-embed after an embedding-generation
	// change) drains steadily WITHOUT pegging the CPU. Previously each pass
	// drained flat-out for up to processingLease, which — multiplied across the
	// hub daemon plus every per-session process, and ONNX's own thread pool —
	// saturated the machine (observed load ~15 on 12 cores).
	processingMaxBatchPerWake = 8
	processingBusyThrottle    = 400 * time.Millisecond
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
			Kind: s.jobKindFor(generation), Generation: generation,
		})
		// While a legacy generation answers queries, new memories also go
		// into it, so search stays complete until the switch.
		if legacy := s.legacyGenerationID(); legacy != "" && legacy != generation {
			specs = append(specs, ProcessingJobSpec{Kind: embeddingJobKind, Generation: legacy})
		}
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
	lastGenerationCheck := time.Now()
	idle := processingPollEvery
	woken := false
	for {
		if time.Since(lastGenerationCheck) >= generationCheckEvery {
			lastGenerationCheck = time.Now()
			ctx, cancel := s.shutdownContext()
			s.checkActiveGeneration(ctx)
			cancel()
		}
		busy, worked := s.drainDurableWork(woken)
		if busy {
			// Backlog remains: rest briefly so background processing never
			// saturates the CPU, then keep draining.
			select {
			case <-s.shutdown:
				return
			case <-time.After(processingBusyThrottle):
			}
			continue
		}
		// Nothing to do: poll less and less often, up to the cap.
		if worked {
			idle = processingPollEvery
		} else {
			idle = min(2*idle, processingIdlePollMax)
		}
		wait := time.NewTimer(min(idle, time.Until(lastGenerationCheck.Add(generationCheckEvery))))
		select {
		case <-s.shutdown:
			wait.Stop()
			return
		case <-s.workerWake:
			wait.Stop()
			woken, idle = true, processingPollEvery
		case <-wait.C:
			woken = false
		}
	}
}

// drainDurableWork runs the queued jobs. woken is true when a save in this
// process asked for the pass. busy reports a batch cut short with work left,
// worked that anything ran.
func (s *Service) drainDurableWork(woken bool) (busy, worked bool) {
	ctx, cancel := context.WithTimeout(context.Background(), processingLease)
	defer cancel()
	if s.embedder != nil {
		for _, kind := range []string{embeddingJobKind, embeddingJobKindV2} {
			capped, n := s.drainProcessingKind(ctx, kind, s.claimCutoff(woken))
			busy = busy || capped
			worked = worked || n > 0
		}
	}
	s.deliveryMu.RLock()
	hasDeliverer := s.remoteDeliverer != nil
	s.deliveryMu.RUnlock()
	if hasDeliverer && s.drainRemoteOutbox(ctx) {
		worked = true
	}
	return busy, worked
}

// claimCutoff is the creation time a job must predate for this pass to claim
// it: zero (any job) unless the pass is a poll in a process whose model is
// unloaded. Loading the model costs about a second and 500 MB, so a fresh
// job waits unloadedClaimGrace for a process that has it loaded (the one that
// saved it, usually) before an idle one loads it.
func (s *Service) claimCutoff(woken bool) time.Time {
	if woken {
		return time.Time{}
	}
	if lazy, ok := s.embedder.(interface{ Loaded() bool }); ok && !lazy.Loaded() {
		return time.Now().Add(-unloadedClaimGrace)
	}
	return time.Time{}
}

// processingClaimer is the queue an idle worker can ask before claiming.
type processingClaimer interface {
	HasClaimableProcessingJob(ctx context.Context, kind string, generations []string, createdBefore, now time.Time) (bool, error)
	ClaimProcessingJobCreatedBefore(ctx context.Context, kind, owner string, generations []string, createdBefore, now time.Time, lease time.Duration) (*ProcessingJob, error)
}

func (s *Service) drainProcessingKind(ctx context.Context, kind string, createdBefore time.Time) (hitBatchCap bool, processed int) {
	queue, ok := s.store.(ProcessingQueueStore)
	if !ok {
		return false, 0
	}
	inputs, ok := s.store.(ProcessingRevisionStore)
	if !ok {
		return false, 0
	}
	claimer, precheck := queue.(processingClaimer)
	var generations []string
	if isEmbeddingJobKind(kind) {
		generations = s.servedJobGenerations()
	}
	if precheck {
		has, err := claimer.HasClaimableProcessingJob(ctx, kind, generations, createdBefore, time.Now().UTC())
		if err == nil && !has {
			return false, 0
		}
	}
	for ; processed < processingMaxBatchPerWake && ctx.Err() == nil; processed++ {
		now := time.Now().UTC()
		var job *ProcessingJob
		var err error
		switch scoped, ok := queue.(generationScopedClaimer); {
		case precheck:
			job, err = claimer.ClaimProcessingJobCreatedBefore(ctx, kind, s.workerOwner, generations, createdBefore, now, processingLease)
		case ok && isEmbeddingJobKind(kind):
			job, err = scoped.ClaimProcessingJobIn(ctx, kind, s.workerOwner, generations, now, processingLease)
		default:
			job, err = queue.ClaimProcessingJob(ctx, kind, s.workerOwner, now, processingLease)
		}
		if err != nil {
			s.logger.Warn("claim durable processing job failed", "kind", kind, "error", err)
			return false, processed
		}
		if job == nil {
			return false, processed
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
	// Hit the per-wake batch cap with the context still live — more jobs are
	// probably queued; signal the caller to throttle instead of spinning.
	return ctx.Err() == nil, processed
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
	case embeddingJobKind, embeddingJobKindV2:
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
			if generation == nil || generation.State == EmbeddingGenerationRetired {
				return nil
			}
			if s.providerFor(generation.Identity) == nil {
				// The claim only takes jobs of the generations this process
				// serves, so this is a pipeline that changed since. Finishing
				// is safe: reconciliation re-arms a finished job whose vector
				// is missing when a process that embeds this space runs it.
				return nil
			}
			if err := s.embedRevisionForGeneration(ctx, generations, generation, revision); err != nil {
				return err
			}
			// A vector of the active generation reaches the cache as it is
			// written (PutEmbeddingVector). Re-enabling that generation after
			// every job reloaded the whole corpus into the cache each time; only
			// a generation still being built can change state here.
			if generation.State != EmbeddingGenerationBuilding {
				return nil
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

// drainRemoteOutbox delivers the due outbox items and reports whether it
// handled any.
func (s *Service) drainRemoteOutbox(ctx context.Context) (worked bool) {
	queue, ok := s.store.(RemoteOutboxStore)
	if !ok {
		return false
	}
	if pre, ok := queue.(interface {
		HasDeliverableRemoteOutbox(ctx context.Context, now time.Time) (bool, error)
	}); ok {
		if has, err := pre.HasDeliverableRemoteOutbox(ctx, time.Now().UTC()); err == nil && !has {
			return false
		}
	}
	for ctx.Err() == nil {
		now := time.Now().UTC()
		item, err := queue.ClaimRemoteOutbox(ctx, s.workerOwner, now, processingLease)
		if err != nil {
			s.logger.Warn("claim remote outbox failed", "error", err)
			return worked
		}
		if item == nil {
			return worked
		}
		worked = true
		s.deliveryMu.RLock()
		deliverer := s.remoteDeliverer
		s.deliveryMu.RUnlock()
		if deliverer == nil {
			_, _ = queue.FailRemoteOutbox(
				ctx, item.OperationID, s.workerOwner, "transport_unavailable",
				"remote outbox deliverer unavailable", now,
				OutboxRetryAtFor(item.OperationID, now, item.Attempts, 0), false,
			)
			return worked
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
	return worked
}

// generationScopedClaimer claims only jobs of the given generations.
type generationScopedClaimer interface {
	ClaimProcessingJobIn(ctx context.Context, kind, owner string, generations []string, now time.Time, lease time.Duration) (*ProcessingJob, error)
}

func isEmbeddingJobKind(kind string) bool {
	return kind == embeddingJobKind || kind == embeddingJobKindV2
}

// servedJobGenerations lists the generations this process embeds into: the
// current pipeline's, the legacy one it serves (if any), and the model name
// jobs written before generations existed carry.
func (s *Service) servedJobGenerations() []string {
	out := []string{}
	if g := s.currentEmbeddingGenerationID(); g != "" {
		out = append(out, g)
	}
	if g := s.legacyGenerationID(); g != "" {
		out = append(out, g)
	}
	if s.embedder != nil {
		out = append(out, s.embedder.Model())
	}
	return out
}
