DROP INDEX IF EXISTS webhook_jobs_processing_started_at_idx;

ALTER TABLE webhook_jobs
    DROP CONSTRAINT IF EXISTS webhook_jobs_processing_ownership_check,
    DROP COLUMN IF EXISTS processing_started_at,
    DROP COLUMN IF EXISTS claim_token;

CREATE INDEX webhook_jobs_processing_updated_at_idx
    ON webhook_jobs (updated_at)
    WHERE status = 'processing';
