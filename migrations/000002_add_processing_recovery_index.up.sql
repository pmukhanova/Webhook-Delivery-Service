CREATE INDEX webhook_jobs_processing_updated_at_idx
    ON webhook_jobs (updated_at)
    WHERE status = 'processing';
