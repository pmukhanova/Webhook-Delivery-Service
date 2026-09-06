package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

var (
	ErrNotFound            = errors.New("webhook job not found")
	ErrClaimLost           = errors.New("webhook job is no longer owned by this claim")
	ErrIdempotencyConflict = errors.New("idempotency key belongs to a different request")
)

type WebhookRepository struct {
	pool *pgxpool.Pool
}

func NewWebhookRepository(pool *pgxpool.Pool) *WebhookRepository {
	return &WebhookRepository{pool: pool}
}

func (r *WebhookRepository) Create(ctx context.Context, input model.CreateWebhook) (model.WebhookJob, error) {
	const insertQuery = `
		INSERT INTO webhook_jobs (id, idempotency_key, target_url, event_type, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, idempotency_key, target_url, event_type, payload, status,
		          attempts, max_attempts, next_attempt_at, last_error,
		          created_at, updated_at, delivered_at, claim_token, processing_started_at`

	job, err := scanJob(r.pool.QueryRow(ctx, insertQuery,
		input.ID, input.IdempotencyKey, input.TargetURL, input.EventType, input.Payload,
	))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.WebhookJob{}, err
	}

	return r.getByIdempotencyKey(ctx, input)
}

func (r *WebhookRepository) GetByID(ctx context.Context, id uuid.UUID) (model.WebhookJob, error) {
	const query = `
		SELECT id, idempotency_key, target_url, event_type, payload, status,
		       attempts, max_attempts, next_attempt_at, last_error,
		       created_at, updated_at, delivered_at, claim_token, processing_started_at
		FROM webhook_jobs
		WHERE id = $1`
	job, err := scanJob(r.pool.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.WebhookJob{}, ErrNotFound
	}
	return job, err
}

func (r *WebhookRepository) getByIdempotencyKey(ctx context.Context, input model.CreateWebhook) (model.WebhookJob, error) {
	const query = `
		SELECT id, idempotency_key, target_url, event_type, payload, status,
		       attempts, max_attempts, next_attempt_at, last_error,
		       created_at, updated_at, delivered_at, claim_token, processing_started_at,
		       target_url = $2 AND event_type = $3 AND payload = $4::jsonb
		FROM webhook_jobs
		WHERE idempotency_key = $1`
	var sameRequest bool
	job, err := scanJob(r.pool.QueryRow(ctx, query,
		input.IdempotencyKey, input.TargetURL, input.EventType, input.Payload,
	), &sameRequest)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.WebhookJob{}, ErrNotFound
	}
	if err != nil {
		return model.WebhookJob{}, err
	}
	if !sameRequest {
		return model.WebhookJob{}, ErrIdempotencyConflict
	}
	return job, nil
}

func (r *WebhookRepository) List(ctx context.Context, status *model.Status, limit int) ([]model.WebhookJob, error) {
	const query = `
		SELECT id, idempotency_key, target_url, event_type, payload, status,
		       attempts, max_attempts, next_attempt_at, last_error,
		       created_at, updated_at, delivered_at, claim_token, processing_started_at
		FROM webhook_jobs
		WHERE ($1::text IS NULL OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2`

	var statusValue *string
	if status != nil {
		value := string(*status)
		statusValue = &value
	}
	rows, err := r.pool.Query(ctx, query, statusValue, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := make([]model.WebhookJob, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (r *WebhookRepository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

func (r *WebhookRepository) ClaimReadyJobs(ctx context.Context, limit int) ([]model.WebhookJob, error) {
	const query = `
		WITH ready AS (
			SELECT id
			FROM webhook_jobs
			WHERE attempts < max_attempts
			  AND (
				status = 'pending'
				OR (status = 'retry' AND next_attempt_at <= now())
			  )
			ORDER BY
				CASE WHEN status = 'retry' THEN next_attempt_at ELSE created_at END,
				created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE webhook_jobs AS jobs
		SET status = 'processing', claim_token = gen_random_uuid(),
		    processing_started_at = clock_timestamp(), updated_at = clock_timestamp(),
		    next_attempt_at = NULL
		FROM ready
		WHERE jobs.id = ready.id
		RETURNING jobs.id, jobs.idempotency_key, jobs.target_url, jobs.event_type,
		          jobs.payload, jobs.status, jobs.attempts, jobs.max_attempts,
		          jobs.next_attempt_at, jobs.last_error, jobs.created_at,
		          jobs.updated_at, jobs.delivered_at, jobs.claim_token,
		          jobs.processing_started_at`

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	rows, err := tx.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	jobs := make([]model.WebhookJob, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (r *WebhookRepository) MarkDelivered(ctx context.Context, id, claimToken uuid.UUID) error {
	const query = `
		UPDATE webhook_jobs
		SET status = 'delivered', attempts = attempts + 1, delivered_at = now(),
		    next_attempt_at = NULL, last_error = NULL, updated_at = now(),
		    claim_token = NULL, processing_started_at = NULL
		WHERE id = $1 AND status = 'processing' AND claim_token = $2`
	return r.execProcessingUpdate(ctx, query, id, claimToken)
}

func (r *WebhookRepository) ScheduleRetry(ctx context.Context, id, claimToken uuid.UUID, nextAttemptAt time.Time, lastError string) error {
	const query = `
		UPDATE webhook_jobs
		SET status = 'retry', attempts = attempts + 1, next_attempt_at = $3,
		    last_error = $4, updated_at = now(), claim_token = NULL,
		    processing_started_at = NULL
		WHERE id = $1 AND status = 'processing' AND claim_token = $2`
	return r.execProcessingUpdate(ctx, query, id, claimToken, nextAttemptAt, lastError)
}

func (r *WebhookRepository) MarkFailed(ctx context.Context, id, claimToken uuid.UUID, lastError string) error {
	const query = `
		UPDATE webhook_jobs
		SET status = 'failed', attempts = attempts + 1, next_attempt_at = NULL,
		    last_error = $3, updated_at = now(), claim_token = NULL,
		    processing_started_at = NULL
		WHERE id = $1 AND status = 'processing' AND claim_token = $2`
	return r.execProcessingUpdate(ctx, query, id, claimToken, lastError)
}

func (r *WebhookRepository) RecoverStaleProcessing(ctx context.Context, cutoff time.Time) (int64, error) {
	const query = `
		UPDATE webhook_jobs
		SET status = 'retry', next_attempt_at = now(),
		    last_error = 'processing timeout expired; previous delivery outcome is unknown',
		    claim_token = NULL, processing_started_at = NULL, updated_at = now()
		WHERE status = 'processing' AND processing_started_at < $1`
	tag, err := r.pool.Exec(ctx, query, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *WebhookRepository) execProcessingUpdate(ctx context.Context, query string, args ...any) error {
	tag, err := r.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner, extraDestinations ...any) (model.WebhookJob, error) {
	var job model.WebhookJob
	destinations := []any{
		&job.ID, &job.IdempotencyKey, &job.TargetURL, &job.EventType, &job.Payload,
		&job.Status, &job.Attempts, &job.MaxAttempts, &job.NextAttemptAt,
		&job.LastError, &job.CreatedAt, &job.UpdatedAt, &job.DeliveredAt,
		&job.ClaimToken, &job.ProcessingStartedAt,
	}
	destinations = append(destinations, extraDestinations...)
	err := row.Scan(destinations...)
	return job, err
}
