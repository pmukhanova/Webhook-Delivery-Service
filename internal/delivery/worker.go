package delivery

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/repository"
)

const maxLastErrorLength = 500

type resultStore interface {
	MarkDelivered(context.Context, uuid.UUID, uuid.UUID) error
	ScheduleRetry(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error
	MarkFailed(context.Context, uuid.UUID, uuid.UUID, string) error
}

type jobDeliverer interface {
	Deliver(context.Context, model.WebhookJob) Result
}

type Worker struct {
	id        int
	store     resultStore
	deliverer jobDeliverer
	logger    *slog.Logger
	now       func() time.Time
}

func NewWorker(id int, store resultStore, deliverer jobDeliverer, logger *slog.Logger) *Worker {
	return &Worker{id: id, store: store, deliverer: deliverer, logger: logger, now: time.Now}
}

func (w *Worker) Run(ctx context.Context, jobs <-chan model.WebhookJob) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			w.process(ctx, job)
		}
	}
}

func (w *Worker) process(ctx context.Context, job model.WebhookJob) {
	if job.ClaimToken == nil {
		w.logger.Error("processing webhook has no claim token", "event", "webhook_claim_lost", "webhook_id", job.ID)
		return
	}
	claimToken := *job.ClaimToken
	attempt := job.Attempts + 1
	startedAt := w.now()
	log := w.logger.With(
		"event", "webhook_delivery_started",
		"worker_id", w.id,
		"webhook_id", job.ID,
		"event_type", job.EventType,
		"attempt", attempt,
		"target_host", targetHost(job.TargetURL),
	)
	log.Info("webhook delivery started")

	result := w.deliverer.Deliver(ctx, job)
	duration := w.now().Sub(startedAt)
	fields := []any{"webhook_id", job.ID, "event_type", job.EventType, "attempt", attempt,
		"target_host", targetHost(job.TargetURL), "http_status", result.HTTPStatus, "duration", duration}

	switch result.Outcome {
	case OutcomeDelivered:
		if err := w.store.MarkDelivered(ctx, job.ID, claimToken); err != nil {
			w.logUpdateError("persist delivered webhook", fields, err)
			return
		}
		w.logger.Info("webhook delivered", append(fields, "event", "webhook_delivered", "result", "delivered")...)
	case OutcomeRetry:
		lastError := truncate(result.Error, maxLastErrorLength)
		if attempt >= job.MaxAttempts {
			if err := w.store.MarkFailed(ctx, job.ID, claimToken, lastError); err != nil {
				w.logUpdateError("persist failed webhook", fields, err)
				return
			}
			w.logger.Warn("webhook failed", append(fields, "event", "webhook_failed", "result", "max_attempts_reached", "error", lastError)...)
			return
		}
		nextAttemptAt := w.now().Add(Backoff(attempt))
		if err := w.store.ScheduleRetry(ctx, job.ID, claimToken, nextAttemptAt, lastError); err != nil {
			w.logUpdateError("persist webhook retry", fields, err)
			return
		}
		w.logger.Warn("webhook retry scheduled", append(fields, "event", "webhook_retry_scheduled", "result", "retry", "next_attempt_at", nextAttemptAt, "error", lastError)...)
	case OutcomeFailed:
		lastError := truncate(result.Error, maxLastErrorLength)
		if err := w.store.MarkFailed(ctx, job.ID, claimToken, lastError); err != nil {
			w.logUpdateError("persist failed webhook", fields, err)
			return
		}
		w.logger.Warn("webhook failed", append(fields, "event", "webhook_failed", "result", "permanent_failure", "error", lastError)...)
	default:
		lastError := truncate("unexpected delivery outcome", maxLastErrorLength)
		w.logger.Error("deliverer returned unexpected outcome", append(fields, "event", "webhook_delivery_outcome_invalid", "outcome", result.Outcome)...)
		if attempt >= job.MaxAttempts {
			if err := w.store.MarkFailed(ctx, job.ID, claimToken, lastError); err != nil {
				w.logUpdateError("persist failed webhook", fields, err)
				return
			}
			w.logger.Warn("webhook failed", append(fields, "event", "webhook_failed", "result", "max_attempts_reached", "error", lastError)...)
			return
		}
		nextAttemptAt := w.now().Add(Backoff(attempt))
		if err := w.store.ScheduleRetry(ctx, job.ID, claimToken, nextAttemptAt, lastError); err != nil {
			w.logUpdateError("persist webhook retry", fields, err)
			return
		}
		w.logger.Warn("webhook retry scheduled", append(fields, "event", "webhook_retry_scheduled", "result", "unexpected_outcome", "next_attempt_at", nextAttemptAt, "error", lastError)...)
	}
}

func (w *Worker) logUpdateError(message string, fields []any, err error) {
	if errors.Is(err, repository.ErrClaimLost) {
		w.logger.Warn(message, append(fields, "event", "webhook_claim_lost", "result", "stale_claim")...)
		return
	}
	w.logger.Error(message, append(fields, "error", err)...)
}

func targetHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "invalid"
	}
	return parsed.Host
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
