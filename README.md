# Webhook Delivery Service

A compact Go service that accepts webhook jobs and delivers them asynchronously with retries and PostgreSQL-backed concurrency control.

## Problem

Webhook targets can be temporarily unavailable, return errors, or time out. The delivery process can crash after sending a request but before recording its result. Multiple workers must not process the same database claim concurrently, while clients may safely repeat job creation after losing an API response. This project handles those cases with idempotent inserts, transactional claiming, bounded workers, retries, and stale-job recovery.

## Features

- REST API for creating and inspecting webhook jobs
- Asynchronous HTTP delivery through a fixed worker pool
- PostgreSQL-backed queue with atomic claiming
- Exponential retry backoff and maximum attempts
- Idempotent creation using `Idempotency-Key`
- Explicit claim ownership and stale-processing recovery
- Graceful shutdown with context cancellation
- SSRF-aware outbound transport with redirects disabled
- Unit, race, concurrency, and PostgreSQL integration tests
- GitHub Actions CI

## Architecture

```mermaid
flowchart TD
    Client -->|POST /api/v1/webhooks| API[REST API]
    API -->|idempotent insert| DB[(PostgreSQL)]
    DB -->|claim ready jobs| Dispatcher
    Dispatcher --> Channel
    Channel --> Workers[Fixed Worker Pool]
    Workers -->|HTTP POST| Target[HTTP Target]
    Workers -->|delivered / retry / failed| DB
    Recovery[Stale Recovery] -->|expired processing to retry| DB
```

The API and workers share one `pgxpool.Pool`. PostgreSQL remains the source of truth; the in-memory channel only hands already-claimed jobs to available workers.

## Request lifecycle

1. `POST /api/v1/webhooks` validates the header, URL, event type, and JSON payload.
2. PostgreSQL inserts one `pending` job. Reusing an `Idempotency-Key` with the same target, event type, and semantic JSON payload returns that row; different request data returns `409 Conflict`.
3. The dispatcher atomically claims an eligible job and changes it to `processing`.
4. A worker sends the original payload to the target with stable webhook headers.
5. A 2xx response becomes `delivered`; a retryable result becomes `retry`; a permanent result becomes `failed`.
6. Due retries re-enter the same claim and delivery flow.

## Concurrency model

The pool starts a fixed number of worker goroutines. A single dispatcher sends claimed jobs through an unbuffered channel, and the pool closes that channel only after the dispatcher exits.

`FOR UPDATE SKIP LOCKED` lets concurrent dispatchers claim different rows without waiting on one another. Each claim also generates a UUID `claim_token`. Final state updates require both the job ID and current token, so an old worker cannot overwrite a job that was recovered and claimed again.

## Retry policy

| Result | Action |
|---|---|
| HTTP 2xx | Delivered |
| Network error | Retry |
| Timeout | Retry |
| HTTP 408 | Retry |
| HTTP 429 | Retry |
| HTTP 5xx | Retry |
| Other HTTP 4xx | Failed |
| HTTP 3xx | Failed; redirects are not followed |

Backoff starts at 2 seconds and doubles for each recorded attempt: 2s, 4s, 8s, 16s, up to a 60s cap. A retryable result on the last allowed attempt becomes `failed`.

## Delivery guarantees

Delivery is **at-least-once**, not exactly-once. A target can accept a webhook and return 2xx just before the service crashes, leaving no recorded success; recovery will send the job again.

`X-Webhook-ID` remains stable across attempts and can be used by the receiver as its deduplication key. `X-Webhook-Event` contains the submitted event type.

## Crash recovery

Every claim stores `processing_started_at` and a new `claim_token`. The dispatcher periodically returns processing jobs older than `PROCESSING_TIMEOUT` to `retry` and clears their ownership. A late worker holding the previous token can no longer update the row.

## Security

- Only HTTP and HTTPS target URLs are accepted.
- Loopback, private, link-local, unspecified, multicast, and other non-public destination IPs are blocked for IPv4 and IPv6.
- DNS is resolved and validated immediately before dialing; the connection uses the validated numeric IP without a second lookup.
- Redirects and environment HTTP proxies are disabled for delivery requests.
- Webhook payloads are not written to delivery logs.

This is a focused SSRF mitigation, not an absolute security boundary or domain allowlist.

## API

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/health` | Check application and PostgreSQL health |
| `POST` | `/api/v1/webhooks` | Create or return an idempotent webhook job |
| `GET` | `/api/v1/webhooks/{id}` | Get one webhook job |
| `GET` | `/api/v1/webhooks` | List jobs with optional `status` and `limit` filters |

The complete contract is in [api/openapi.yaml](api/openapi.yaml).

## Quick start

Requirements: Docker with the Compose plugin.

```sh
cp .env.example .env
docker compose up --build -d
curl http://localhost:8080/health
```

Create a job using a public receiver URL. Localhost targets are intentionally rejected by the SSRF policy.

```sh
curl -i http://localhost:8080/api/v1/webhooks \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: payment-123' \
  -d '{"target_url":"https://example.com/webhook","event_type":"payment.completed","payload":{"payment_id":"123","amount":5000}}'
```

Copy the returned `id` and inspect the job:

```sh
curl http://localhost:8080/api/v1/webhooks/REPLACE_WITH_JOB_ID
```

For a real successful delivery, replace `example.com` with a public test receiver you control. The reusable example is [examples/create-webhook.sh](examples/create-webhook.sh).

Stop the project with `make down`.

## Configuration

| Variable | Required | Default | Purpose |
|---|---:|---:|---|
| `DATABASE_URL` | Outside Compose | — | PostgreSQL connection string |
| `HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `WORKER_COUNT` | No | `4` | Maximum parallel deliveries |
| `JOB_BATCH_SIZE` | No | worker count | Maximum jobs claimed per dispatch |
| `DISPATCH_INTERVAL` | No | `1s` | Delay between dispatch cycles |
| `DELIVERY_TIMEOUT` | No | `5s` | Timeout for one outbound request |
| `PROCESSING_TIMEOUT` | No | `60s` | Age at which a processing claim is recovered |

`PROCESSING_TIMEOUT` must exceed the validated worst-case wait and delivery time for the configured batch and worker count. Compose database variables are listed in [.env.example](.env.example).

## Testing

```sh
make test
make test-race
make check
```

Integration tests run destructive cleanup and therefore require a dedicated disposable PostgreSQL database with all project migrations applied:

```sh
POSTGRES_DB=webhook_test docker compose -p webhook-test up -d postgres
POSTGRES_DB=webhook_test docker compose -p webhook-test run --rm migrate
TEST_DATABASE_URL='postgres://webhook:webhook@localhost:5432/webhook_test?sslmode=disable' make test-integration
docker compose -p webhook-test down -v
```

## Project structure

```text
api/                    OpenAPI contract
cmd/server/             application entry point
examples/               minimal request example
internal/handler/       HTTP transport and validation
internal/service/       API use cases
internal/repository/    PostgreSQL persistence and claiming
internal/delivery/      dispatcher, worker pool, retry, HTTP delivery
internal/targetpolicy/  outbound destination policy
migrations/             ordered SQL up/down migrations
```

## Engineering decisions

### PostgreSQL instead of an external message broker

For this project scope, PostgreSQL keeps operational complexity low while still demonstrating durable jobs, transactions, locking, retries, and recovery. A broker would add infrastructure without fixing the unavoidable HTTP delivery crash window.

### FOR UPDATE SKIP LOCKED

Concurrent claimers lock different ready rows instead of waiting on the same work. Selection and the transition to `processing` occur atomically in one transaction, which avoids a SELECT/unlock/UPDATE race.

### Explicit claim token

`status = processing` alone cannot distinguish an old worker from a worker that reclaimed the job after recovery. A new token per claim makes every final update conditional on current ownership.

### At-least-once instead of exactly-once

The service cannot atomically combine an external HTTP side effect with a PostgreSQL commit. Receiver cooperation and deduplication by `X-Webhook-ID` are required to handle the duplicate crash window.

### Fixed worker pool

A configured worker count bounds outbound concurrency and goroutine growth. The service does not create a new goroutine for every webhook.

### SSRF-safe outbound transport

User-controlled URLs cannot be passed directly to an unrestricted client. Validation is performed in the dial path, where DNS results are checked and the exact allowed IP is used for the connection; redirects and proxies are disabled to close common bypasses.
