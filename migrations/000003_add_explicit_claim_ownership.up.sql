ALTER TABLE webhook_jobs
    ADD COLUMN claim_token UUID NULL,
    ADD COLUMN processing_started_at TIMESTAMPTZ NULL;

UPDATE webhook_jobs
SET status = 'retry',
    next_attempt_at = now(),
    last_error = 'processing job reset while adding explicit claim ownership',
    updated_at = now()
WHERE status = 'processing';

ALTER TABLE webhook_jobs
    ADD CONSTRAINT webhook_jobs_processing_ownership_check CHECK (
        (status = 'processing' AND claim_token IS NOT NULL AND processing_started_at IS NOT NULL)
        OR
        (status <> 'processing' AND claim_token IS NULL AND processing_started_at IS NULL)
    );

DROP INDEX webhook_jobs_processing_updated_at_idx;

CREATE INDEX webhook_jobs_processing_started_at_idx
    ON webhook_jobs (processing_started_at)
    WHERE status = 'processing';
