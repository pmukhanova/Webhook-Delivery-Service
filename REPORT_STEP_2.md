# Step 2 Report

## Что реализовано

- Периодический dispatcher без busy loop с ограниченным batch claiming.
- Фиксированный worker pool, channel между dispatcher и workers и ожидание goroutines через `sync.WaitGroup`.
- Атомарный PostgreSQL claiming через транзакцию, `FOR UPDATE SKIP LOCKED` и перевод jobs в `processing`.
- HTTP POST исходного payload с общим переиспользуемым client, timeout, служебными headers и закрытием response body.
- Классификация 2xx, 408, 429, 4xx, 5xx, network errors и timeouts.
- Retry с exponential backoff, ограничением в 60 секунд и переходом в `failed` после `max_attempts`.
- Периодическое восстановление stale `processing` jobs.
- Graceful shutdown dispatcher/workers/HTTP server и тесты конфигурации, delivery, worker pool и dispatcher.

## Измененные файлы

- `cmd/server/main.go` — подключает dispatcher, общий HTTP client и worker pool к lifecycle приложения.
- `internal/config/config.go` — читает и валидирует worker count, batch size и delivery/dispatch/processing timeouts.
- `internal/config/config_test.go` — проверяет defaults и отказ при небезопасной или некорректной конфигурации.
- `internal/delivery/backoff.go` — содержит чистую функцию exponential backoff.
- `internal/delivery/backoff_test.go` — проверяет последовательность backoff и максимальное ограничение.
- `internal/delivery/client.go` — выполняет HTTP delivery и классифицирует результат.
- `internal/delivery/client_test.go` — проверяет HTTP-классификацию, headers, payload, network error, timeout и закрытие body.
- `internal/delivery/dispatcher.go` — восстанавливает stale jobs, claim'ит ready jobs и передаёт их workers.
- `internal/delivery/dispatcher_test.go` — проверяет dispatch, отсутствие busy loop и остановку по context cancellation.
- `internal/delivery/worker.go` — обрабатывает jobs, рассчитывает attempts/backoff и сохраняет итоговый статус.
- `internal/delivery/worker_test.go` — проверяет status transitions и завершение worker по cancellation/close channel.
- `internal/delivery/pool.go` — владеет channel и lifecycle фиксированного набора workers.
- `internal/delivery/pool_test.go` — проверяет фактическое ограничение concurrency и остановку pool.
- `internal/repository/webhook.go` — добавляет SQL claiming, фиксацию delivery result и stale recovery.
- `internal/handler/http.go` — минимально усиливает Step 1 validation для безопасного `X-Webhook-Event` header.
- `internal/handler/http_test.go` — проверяет отклонение event type с переводом строки.
- `migrations/000002_add_processing_recovery_index.*.sql` — добавляет и удаляет partial index для stale recovery.
- `.env.example` и `docker-compose.yml` — документируют и передают background configuration.
- `README.md` — описывает asynchronous delivery, новые environment variables и race tests.
- `Makefile` — добавляет отдельную команду `race`.

## Архитектура background processing

Flow имеет вид:

`PostgreSQL -> Dispatcher -> unbuffered Channel -> Worker Pool -> HTTP Target -> PostgreSQL status update`.

`Pool.Run` создаёт channel и worker goroutines. Dispatcher является единственным sender. После возврата dispatcher из-за cancellation только pool закрывает channel, поэтому `send on closed channel` невозможен. Workers только читают channel, завершаются по закрытию или `ctx.Done()`, а pool ждёт их через `WaitGroup`.

Channel намеренно unbuffered: claimed job передаётся только готовому worker. Это ограничивает prefetch и уменьшает время, когда job уже имеет статус `processing`, но ещё не начала HTTP delivery. Dispatcher может держать в памяти не более одного ограниченного batch; при cancellation непереданные jobs остаются `processing` и будут восстановлены по timeout.

## PostgreSQL claiming

Ключевая схема SQL упрощённо выглядит так:

```sql
BEGIN;

WITH ready AS (
    SELECT id
    FROM webhook_jobs
    WHERE attempts < max_attempts
      AND (status = 'pending'
           OR (status = 'retry' AND next_attempt_at <= now()))
    ORDER BY ...
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE webhook_jobs AS jobs
SET status = 'processing',
    updated_at = clock_timestamp(),
    next_attempt_at = NULL
FROM ready
WHERE jobs.id = ready.id
RETURNING jobs.*;

COMMIT;
```

Repository явно начинает transaction, полностью читает `RETURNING`, закрывает rows и выполняет commit. Только после возврата committed jobs dispatcher отправляет их workers, поэтому transaction никогда не остаётся открытой во время HTTP request.

`FOR UPDATE SKIP LOCKED` блокирует выбранные rows для текущей transaction, а конкурентная claim пропускает эти rows. Перевод в `processing` происходит в том же statement до commit, поэтому два worker pools или экземпляра сервиса не должны получить одну job.

`updated_at`, возвращённый claim, используется как простой claim token при `MarkDelivered`, `ScheduleRetry` и `MarkFailed`. Если stale recovery и новый claim уже изменили row, поздний старый worker не сможет перезаписать новый state.

## Retry semantics

`attempts` означает число delivery attempts, результат которых сохранён в PostgreSQL. Новая job имеет `attempts = 0`; worker обрабатывает попытку `attempts + 1`. Успех, retryable result и permanent failure увеличивают значение один раз в соответствующем атомарном update.

- Любой HTTP 2xx считается успехом и переводит job в `delivered`.
- Network error, timeout, HTTP 408, 429 и 5xx считаются retryable.
- Остальные ответы, включая HTTP 400, считаются permanent failure.
- Для retryable failure при `attempt < max_attempts` ставится `retry` и `next_attempt_at = now + backoff`.
- Backoff равен 2, 4, 8, 16 секунд далее по степеням двойки, но не больше 60 секунд.
- Если текущая попытка достигла `max_attempts`, retryable result переводится в `failed`; off-by-one проверяется как `attempts + 1 >= max_attempts`.

## Crash recovery

Dispatcher перед каждым claim ищет `processing` jobs с `updated_at` старше `PROCESSING_TIMEOUT` (default 60 секунд) и переводит их в `retry` с немедленным `next_attempt_at`. Partial index по `updated_at WHERE status = 'processing'` поддерживает этот запрос.

Если процесс упал после target request, но до сохранения результата, после recovery возможна повторная доставка. Это сознательная at-least-once семантика: без idempotency на стороне target невозможно одновременно гарантировать отсутствие потерь и дублей.

Recovery не увеличивает `attempts`, потому что факт завершения предыдущего HTTP request неизвестен. Компромисс — повторяющиеся process crashes сами по себе не исчерпывают `max_attempts`. Startup validation требует, чтобы processing timeout превышал worst-case ожидание configured batch плюс delivery timeout, снижая риск восстановления живой job.

## Graceful shutdown

1. SIGINT/SIGTERM отменяет единый application context.
2. Context HTTP connections, dispatcher и выполняемых delivery requests получает cancellation.
3. Dispatcher прекращает claiming и возвращается в pool.
4. Pool, как владелец channel, закрывает его; workers завершают текущую cancellation-aware операцию или выходят сразу.
5. HTTP server выполняет `Shutdown` с отдельным десятисекундным timeout.
6. Main ждёт завершения worker pool, после чего deferred `pgxpool.Close` освобождает соединения.
7. Job, result которой не удалось сохранить из-за cancellation, остаётся `processing` и восстанавливается после `PROCESSING_TIMEOUT` на следующем запуске/цикле.

## Проверки

- `gofmt -w cmd internal` — успешно; `gofmt -l cmd internal` не вывел файлов.
- `go vet ./...` — успешно, без вывода.
- `go test ./...` — успешно; packages `internal/config`, `internal/delivery` и `internal/handler` прошли.
- `go test -race ./...` — успешно.
- `go test -race -count=10 ./internal/delivery` — дополнительный десятикратный прогон успешно.
- `git diff --check` — успешно, без замечаний.
- `docker compose config` — успешно, Compose configuration валидна.
- `docker compose up --build -d` — запускался и завершился ошибкой: Docker daemon недоступен, локальный Docker socket отсутствует.
- PostgreSQL integration и реальные curl delivery scenarios не выполнялись, поскольку containers не удалось запустить. Их успешность не заявляется.

Все Go-команды выполнены официальным Go 1.24.7 toolchain из временной директории, так как `go` отсутствует в PATH среды.

## Tests

Покрыты HTTP 200 и 204 как delivered; HTTP 500, 429 и 408 как retry; HTTP 400 как failed; network error и client timeout как retry. Проверен переход retryable failure в failed на последней попытке, расчёт нескольких backoff и max backoff.

Worker tests проверяют получение job из channel, фиксацию трёх видов результата, остановку по context cancellation и закрытию channel. Pool test блокирует delivery и подтверждает, что одновременно работают ровно два configured workers. Dispatcher test подтверждает отправку claimed job, один dispatch до далёкого tick и prompt cancellation. Отдельно проверены defaults/invalid config, webhook request body/headers и закрытие target response body.

Integration test двух конкурентных `ClaimReadyJobs` намеренно не подменялся fake-тестом: он требует реального PostgreSQL и остаётся невыполненным из-за недоступного Docker daemon.

## Найденные и исправленные проблемы

- Buffered jobs channel при периодическом dispatch мог создавать лишний prefetch уже claimed jobs; channel заменён на unbuffered rendezvous.
- Простая проверка только `status = processing` допускала позднему worker перезаписать result после stale recovery/reclaim; updates теперь дополнительно сверяют claim timestamp.
- Небезопасная комбинация большого batch, малого worker count и короткого processing timeout могла восстановить ещё живую job; добавлена startup validation worst-case времени batch.
- Rollback claim transaction мог получить уже отменённый request context; cleanup использует отдельный короткий rollback context.
- Response body закрывался в реализации, но это не проверялось тестом; добавлен явный close test.
- После добавления `X-Webhook-Event` выяснилось, что Step 1 разрешал переводы строк в `event_type`; validation минимально усилена, а deliverer имеет defensive permanent-failure check.
- Loopback bind для `httptest.NewServer` запрещён sandbox; delivery unit tests переведены на управляемый `RoundTripper`, не ослабляя проверку HTTP request/response logic.

## Известные ограничения

- Гарантия доставки — at-least-once: crash/timeout около момента успешного target response может привести к дублю.
- PostgreSQL claiming и migrations не проверены integration test в текущей среде.
- `updated_at` служит claim token вместо отдельного lease UUID; это просто, но заслуживает проверки на точность timestamp конкретного PostgreSQL deployment.
- Cancellation не пытается сохранить retry через отдельный uncancelled context; такая job восстанавливается по processing timeout.
- Dispatcher может оставить непереданную часть уже claimed batch в `processing` при shutdown; recovery возвращает её позже.
- Response body читается максимум на 64 KiB перед закрытием; для больших responses конкретное соединение может не переиспользоваться.
- Нет SSRF-защиты target URL, webhook signing, authentication, rate limiting, metrics и отдельной dead-letter системы.

## Что проверить на внешнем code review

- План выполнения и конкурентное поведение CTE с `FOR UPDATE SKIP LOCKED` на реальном PostgreSQL.
- Off-by-one semantics `attempts + 1 >= max_attempts` во всех status transitions.
- Надёжность `updated_at` как claim token и поведение при recovery, позднем HTTP response и новом claim.
- Channel ownership и отсутствие зависания dispatcher на send при одновременном cancellation.
- Shutdown при медленном target и недоступной PostgreSQL, включая освобождение pool connections.
- Достаточность формулы минимального `PROCESSING_TIMEOUT` для нестандартных batch/worker settings.
- Возможность duplicate delivery между успешным HTTP response и database update как часть at-least-once контракта.

## Следующий этап

- Добавить PostgreSQL integration suite для concurrent claiming, migrations и state transitions.
- Определить и документировать внешний delivery/idempotency contract для получателей webhook.
- Добавить SSRF policy и webhook signature verification scheme.
- Добавить метрики очереди, delivery latency, retries и structured request correlation.
- Рассмотреть отдельный lease token и более точную operational recovery policy после нагрузочных тестов.
