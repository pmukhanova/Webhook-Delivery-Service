CREATE TABLE webhook_jobs (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    target_url TEXT NOT NULL CHECK (length(target_url) BETWEEN 1 AND 2048),
    event_type TEXT NOT NULL CHECK (length(event_type) BETWEEN 1 AND 255),
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'retry', 'delivered', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    next_attempt_at TIMESTAMPTZ NULL,
    last_error TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ NULL,
    CHECK (attempts <= max_attempts)
);

CREATE INDEX webhook_jobs_status_created_at_idx
    ON webhook_jobs (status, created_at DESC);

CREATE INDEX webhook_jobs_created_at_idx
    ON webhook_jobs (created_at DESC);

CREATE INDEX webhook_jobs_due_idx
    ON webhook_jobs (status, next_attempt_at, created_at)
    WHERE status IN ('pending', 'retry');
