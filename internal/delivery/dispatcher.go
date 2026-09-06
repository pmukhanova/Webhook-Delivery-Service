package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

type dispatchStore interface {
	ClaimReadyJobs(context.Context, int) ([]model.WebhookJob, error)
	RecoverStaleProcessing(context.Context, time.Time) (int64, error)
}

type Dispatcher struct {
	store             dispatchStore
	interval          time.Duration
	batchSize         int
	processingTimeout time.Duration
	logger            *slog.Logger
	now               func() time.Time
}

func NewDispatcher(store dispatchStore, interval time.Duration, batchSize int, processingTimeout time.Duration, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store: store, interval: interval, batchSize: batchSize,
		processingTimeout: processingTimeout, logger: logger, now: time.Now,
	}
}

func (d *Dispatcher) Run(ctx context.Context, jobs chan<- model.WebhookJob) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		d.dispatch(ctx, jobs)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) dispatch(ctx context.Context, jobs chan<- model.WebhookJob) {
	if ctx.Err() != nil {
		return
	}
	recovered, err := d.store.RecoverStaleProcessing(ctx, d.now().Add(-d.processingTimeout))
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Error("recover stale processing webhooks", "error", err)
		}
		return
	}
	if recovered > 0 {
		d.logger.Warn("stale processing webhooks recovered", "count", recovered)
	}

	claimed, err := d.store.ClaimReadyJobs(ctx, d.batchSize)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Error("claim ready webhooks", "error", err)
		}
		return
	}
	for _, job := range claimed {
		d.logger.Info("webhook claimed", "event", "webhook_claimed", "webhook_id", job.ID, "event_type", job.EventType, "attempt", job.Attempts+1)
		select {
		case <-ctx.Done():
			return
		case jobs <- job:
		}
	}
}
