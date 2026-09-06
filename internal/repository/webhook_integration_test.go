//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

func TestIntegrationConcurrentClaiming(t *testing.T) {
	repository, pool := newIntegrationRepository(t)
	for range 10 {
		createIntegrationJob(t, repository, "claim-"+uuid.NewString())
	}

	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			<-start
			jobs, err := repository.ClaimReadyJobs(context.Background(), 5)
			results <- claimResult{jobs: jobs, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	claimedIDs := make(map[uuid.UUID]struct{})
	claimTokens := make(map[uuid.UUID]struct{})
	for result := range results {
		if result.err != nil {
			t.Fatalf("ClaimReadyJobs() error = %v", result.err)
		}
		for _, job := range result.jobs {
			if _, duplicate := claimedIDs[job.ID]; duplicate {
				t.Fatalf("job %s was returned by more than one claimer", job.ID)
			}
			claimedIDs[job.ID] = struct{}{}
			if job.Status != model.StatusProcessing || job.ClaimToken == nil || job.ProcessingStartedAt == nil {
				t.Fatalf("claimed job has incomplete ownership: %+v", job)
			}
			if _, duplicate := claimTokens[*job.ClaimToken]; duplicate {
				t.Fatalf("claim token %s was reused", *job.ClaimToken)
			}
			claimTokens[*job.ClaimToken] = struct{}{}
		}
	}
	if len(claimedIDs) != 10 {
		t.Fatalf("claimed %d jobs, want 10", len(claimedIDs))
	}

	var processing, owned int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE claim_token IS NOT NULL AND processing_started_at IS NOT NULL)
		FROM webhook_jobs WHERE status = 'processing'`).Scan(&processing, &owned); err != nil {
		t.Fatalf("query claimed state: %v", err)
	}
	if processing != 10 || owned != 10 {
		t.Fatalf("processing/owned rows = %d/%d, want 10/10", processing, owned)
	}
}

func TestIntegrationStaleWorkerCannotFinishNewClaim(t *testing.T) {
	repository, pool := newIntegrationRepository(t)
	created := createIntegrationJob(t, repository, "stale-worker")
	claimedA := claimOne(t, repository)
	if _, err := pool.Exec(context.Background(), `
		UPDATE webhook_jobs SET processing_started_at = now() - interval '2 hours' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("make processing job stale: %v", err)
	}
	recovered, err := repository.RecoverStaleProcessing(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverStaleProcessing() = %d, %v; want 1, nil", recovered, err)
	}
	claimedB := claimOne(t, repository)
	if *claimedA.ClaimToken == *claimedB.ClaimToken {
		t.Fatal("reclaimed job reused claim token")
	}

	err = repository.MarkDelivered(context.Background(), created.ID, *claimedA.ClaimToken)
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("old owner MarkDelivered() error = %v, want ErrClaimLost", err)
	}
	current, err := repository.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if current.Status != model.StatusProcessing || current.ClaimToken == nil || *current.ClaimToken != *claimedB.ClaimToken {
		t.Fatalf("old owner changed current claim: %+v", current)
	}

	if err := repository.MarkDelivered(context.Background(), created.ID, *claimedB.ClaimToken); err != nil {
		t.Fatalf("new owner MarkDelivered() error = %v", err)
	}
	current, err = repository.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetByID() after delivery error = %v", err)
	}
	if current.Status != model.StatusDelivered || current.Attempts != 1 || current.ClaimToken != nil || current.ProcessingStartedAt != nil {
		t.Fatalf("delivered state = %+v", current)
	}
}

func TestIntegrationEligibilityAndRecovery(t *testing.T) {
	repository, pool := newIntegrationRepository(t)

	t.Run("ready retry is claimed", func(t *testing.T) {
		truncateJobs(t, pool)
		job := createIntegrationJob(t, repository, "ready-retry")
		setRetryAt(t, pool, job.ID, time.Now().Add(-time.Minute))
		claimed := claimOne(t, repository)
		if claimed.ID != job.ID || claimed.Status != model.StatusProcessing {
			t.Fatalf("claimed job = %+v", claimed)
		}
	})

	t.Run("future retry is not claimed", func(t *testing.T) {
		truncateJobs(t, pool)
		job := createIntegrationJob(t, repository, "future-retry")
		setRetryAt(t, pool, job.ID, time.Now().Add(time.Hour))
		claimed, err := repository.ClaimReadyJobs(context.Background(), 1)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("ClaimReadyJobs() = %v, %v; want empty", claimed, err)
		}
	})

	t.Run("fresh processing is not recovered", func(t *testing.T) {
		truncateJobs(t, pool)
		createIntegrationJob(t, repository, "fresh-processing")
		claimOne(t, repository)
		recovered, err := repository.RecoverStaleProcessing(context.Background(), time.Now().Add(-time.Hour))
		if err != nil || recovered != 0 {
			t.Fatalf("RecoverStaleProcessing() = %d, %v; want 0, nil", recovered, err)
		}
	})

	t.Run("stale processing is recovered", func(t *testing.T) {
		truncateJobs(t, pool)
		job := createIntegrationJob(t, repository, "stale-processing")
		claimOne(t, repository)
		if _, err := pool.Exec(context.Background(), `
			UPDATE webhook_jobs SET processing_started_at = now() - interval '2 hours' WHERE id = $1`, job.ID); err != nil {
			t.Fatalf("make job stale: %v", err)
		}
		recovered, err := repository.RecoverStaleProcessing(context.Background(), time.Now().Add(-time.Hour))
		if err != nil || recovered != 1 {
			t.Fatalf("RecoverStaleProcessing() = %d, %v; want 1, nil", recovered, err)
		}
		current, _ := repository.GetByID(context.Background(), job.ID)
		if current.Status != model.StatusRetry || current.ClaimToken != nil || current.ProcessingStartedAt != nil {
			t.Fatalf("recovered state = %+v", current)
		}
	})
}

func TestIntegrationFinalUpdatesRequireOwner(t *testing.T) {
	repository, pool := newIntegrationRepository(t)

	t.Run("schedule retry", func(t *testing.T) {
		truncateJobs(t, pool)
		job := createIntegrationJob(t, repository, "schedule-owner")
		claimed := claimOne(t, repository)
		if err := repository.ScheduleRetry(context.Background(), job.ID, uuid.New(), time.Now(), "wrong owner"); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("wrong owner error = %v, want ErrClaimLost", err)
		}
		if err := repository.ScheduleRetry(context.Background(), job.ID, *claimed.ClaimToken, time.Now(), "retry"); err != nil {
			t.Fatalf("owner ScheduleRetry() error = %v", err)
		}
		current, _ := repository.GetByID(context.Background(), job.ID)
		if current.Status != model.StatusRetry || current.Attempts != 1 || current.ClaimToken != nil {
			t.Fatalf("retry state = %+v", current)
		}
	})

	t.Run("mark failed", func(t *testing.T) {
		truncateJobs(t, pool)
		job := createIntegrationJob(t, repository, "failed-owner")
		claimed := claimOne(t, repository)
		if err := repository.MarkFailed(context.Background(), job.ID, uuid.New(), "wrong owner"); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("wrong owner error = %v, want ErrClaimLost", err)
		}
		if err := repository.MarkFailed(context.Background(), job.ID, *claimed.ClaimToken, "HTTP 400"); err != nil {
			t.Fatalf("owner MarkFailed() error = %v", err)
		}
		current, _ := repository.GetByID(context.Background(), job.ID)
		if current.Status != model.StatusFailed || current.Attempts != 1 || current.ClaimToken != nil {
			t.Fatalf("failed state = %+v", current)
		}
	})
}

func TestIntegrationMaxAttemptsAreNotClaimed(t *testing.T) {
	repository, pool := newIntegrationRepository(t)
	job := createIntegrationJob(t, repository, "max-attempts")
	if _, err := pool.Exec(context.Background(), `
		UPDATE webhook_jobs SET status = 'retry', attempts = max_attempts, next_attempt_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("prepare max-attempt job: %v", err)
	}
	claimed, err := repository.ClaimReadyJobs(context.Background(), 1)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("ClaimReadyJobs() = %v, %v; want empty", claimed, err)
	}
}

func TestIntegrationConcurrentIdempotency(t *testing.T) {
	repository, pool := newIntegrationRepository(t)
	const callers = 8
	start := make(chan struct{})
	results := make(chan model.WebhookJob, callers)
	errorsChannel := make(chan error, callers)
	var workers sync.WaitGroup
	workers.Add(callers)
	for range callers {
		go func() {
			defer workers.Done()
			<-start
			job, err := repository.Create(context.Background(), model.CreateWebhook{
				ID: uuid.New(), IdempotencyKey: "shared-key", TargetURL: "https://8.8.8.8/webhook",
				EventType: "created", Payload: json.RawMessage(`{"id":1}`),
			})
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- job
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatalf("concurrent Create() error = %v", err)
	}
	var expectedID uuid.UUID
	for job := range results {
		if expectedID == uuid.Nil {
			expectedID = job.ID
		}
		if job.ID != expectedID {
			t.Fatalf("idempotent Create returned IDs %s and %s", expectedID, job.ID)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_jobs WHERE idempotency_key = 'shared-key'`).Scan(&count); err != nil {
		t.Fatalf("count idempotent rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("idempotent row count = %d, want 1", count)
	}
}

func TestIntegrationIdempotencyRequestSemantics(t *testing.T) {
	repository, pool := newIntegrationRepository(t)
	first, err := repository.Create(context.Background(), model.CreateWebhook{
		ID: uuid.New(), IdempotencyKey: "request-semantics", TargetURL: "https://8.8.8.8/webhook",
		EventType: "created", Payload: json.RawMessage(`{"object":{"a":1,"b":2},"values":[1,2]}`),
	})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	same, err := repository.Create(context.Background(), model.CreateWebhook{
		ID: uuid.New(), IdempotencyKey: "request-semantics", TargetURL: "https://8.8.8.8/webhook",
		EventType: "created", Payload: json.RawMessage("{ \"values\": [1, 2], \"object\": {\"b\": 2, \"a\": 1} }"),
	})
	if err != nil {
		t.Fatalf("semantically equal Create() error = %v", err)
	}
	if same.ID != first.ID {
		t.Fatalf("semantically equal Create() ID = %s, want %s", same.ID, first.ID)
	}

	conflicts := []struct {
		name      string
		targetURL string
		eventType string
		payload   json.RawMessage
	}{
		{name: "target URL", targetURL: "https://8.8.4.4/webhook", eventType: "created", payload: json.RawMessage(`{"object":{"a":1,"b":2},"values":[1,2]}`)},
		{name: "event type", targetURL: "https://8.8.8.8/webhook", eventType: "updated", payload: json.RawMessage(`{"object":{"a":1,"b":2},"values":[1,2]}`)},
		{name: "payload", targetURL: "https://8.8.8.8/webhook", eventType: "created", payload: json.RawMessage(`{"object":{"a":1,"b":3},"values":[1,2]}`)},
	}
	for _, test := range conflicts {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.Create(context.Background(), model.CreateWebhook{
				ID: uuid.New(), IdempotencyKey: "request-semantics", TargetURL: test.targetURL,
				EventType: test.eventType, Payload: test.payload,
			})
			if !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("Create() error = %v, want ErrIdempotencyConflict", err)
			}
		})
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_jobs WHERE idempotency_key = 'request-semantics'`).Scan(&count); err != nil {
		t.Fatalf("count idempotent rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("idempotent row count = %d, want 1", count)
	}
}

type claimResult struct {
	jobs []model.WebhookJob
	err  error
}

func newIntegrationRepository(t *testing.T) (*WebhookRepository, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for integration tests; use a dedicated disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create integration database pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping integration database: %v", err)
	}
	truncateJobs(t, pool)
	return NewWebhookRepository(pool), pool
}

func truncateJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var databaseName string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("read current integration database name: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(databaseName), "_test") {
		t.Fatalf("refusing destructive integration cleanup: database %q does not end with _test", databaseName)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE TABLE webhook_jobs`); err != nil {
		t.Fatalf("truncate webhook_jobs (apply project migrations first): %v", err)
	}
}

func createIntegrationJob(t *testing.T, repository *WebhookRepository, key string) model.WebhookJob {
	t.Helper()
	job, err := repository.Create(context.Background(), model.CreateWebhook{
		ID: uuid.New(), IdempotencyKey: key, TargetURL: "https://8.8.8.8/webhook",
		EventType: "created", Payload: json.RawMessage(`{"id":1}`),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return job
}

func claimOne(t *testing.T, repository *WebhookRepository) model.WebhookJob {
	t.Helper()
	jobs, err := repository.ClaimReadyJobs(context.Background(), 1)
	if err != nil {
		t.Fatalf("ClaimReadyJobs() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ClaimReadyJobs() returned %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

func setRetryAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, nextAttemptAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE webhook_jobs SET status = 'retry', next_attempt_at = $2, updated_at = now() WHERE id = $1`, id, nextAttemptAt); err != nil {
		t.Fatalf("prepare retry job: %v", err)
	}
}
