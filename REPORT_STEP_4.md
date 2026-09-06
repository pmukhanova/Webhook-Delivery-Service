# Step 4 Report

## Что сделано

- Проведён финальный review architecture, concurrency, ownership, HTTP delivery, configuration и package boundaries.
- Добавлен OpenAPI 3.1 contract для всех существующих REST operations.
- README полностью переписан как компактная техническая документация проекта.
- Добавлены Mermaid architecture diagram, lifecycle, retry table, security и engineering decisions.
- Добавлен один executable curl example без production SSRF bypass или отдельного demo service.
- Создан `INTERVIEW_NOTES.md` с вопросами и ответами по фактической реализации.
- Усилена безопасность integration suite и выполнена project hygiene проверка.

## Architecture review

Границы пакетов остались без масштабного refactoring: handler отвечает за HTTP, service за API use cases, repository за SQL, delivery за dispatcher/workers/client, targetpolicy за outbound address policy, а main только собирает зависимости и управляет lifecycle. Единственные interfaces находятся рядом с потребителями, которым они нужны для тестов; generic repository и DI framework не добавлялись.

Найдена реальная safety-проблема в integration tests: destructive `TRUNCATE` мог использовать fallback на обычный `DATABASE_URL`. Suite теперь требует явный `TEST_DATABASE_URL` и сообщает, что база должна быть disposable. Это уменьшает риск случайной очистки development database.

Старые отчёты содержали персональный абсолютный путь к Docker socket; путь заменён нейтральным описанием. Core concurrency architecture менять не потребовалось: channel имеет одного sender и одного owner закрытия, goroutines ограничены worker count, ticker останавливается, network request использует caller context, rows/response bodies закрываются, transaction завершается до HTTP delivery, а claim token защищает все final updates.

Placeholder module path `github.com/example/webhook-delivery-service` заменён на нейтральный локальный `webhook-delivery-service`, поскольку у рабочей копии нет настроенного Git remote и угадывать будущий hosting namespace нельзя. Перед публикацией module path следует один раз привязать к фактическому URL repository.

Performance sanity review не обнаружил неконтролируемого роста: worker count ограничивает parallel delivery, batch ограничивает claim, `pgxpool` и один HTTP client переиспользуются, новые DB connections и goroutines не создаются на каждую job, claiming и final transition не содержат N+1 queries.

## Documentation

- `README.md` теперь содержит problem statement, features, Mermaid diagram, lifecycle, concurrency model, retry policy, at-least-once guarantee, recovery, security, API, quick start, configuration, testing, project tree и ключевые engineering decisions.
- `api/openapi.yaml` документирует health, create, get и list operations, Idempotency-Key, filters, statuses, payloads, responses и единый error envelope.
- `INTERVIEW_NOTES.md` содержит 16 вопросов по PostgreSQL queue, locking, ownership, retry, crashes, races, SSRF, lifecycle и возможной эволюции под большую нагрузку.
- `examples/create-webhook.sh` предоставляет один happy-path create request с обязательным public `TARGET_URL`.
- Local receiver не добавлялся: production SSRF policy намеренно блокирует localhost, а отдельный insecure bypass или второй service ухудшил бы простоту и безопасность проекта.

## Project hygiene

- Проверено отсутствие `.env`, generated binaries, test binaries, coverage artifacts и временных файлов в repository.
- Поиск не нашёл персональных абсолютных путей, имён пользователя или служебных attribution-фраз.
- `.gitignore` дополнен для `.env`, IDE metadata, `bin/`, `*.test` и coverage output.
- `.env.example` содержит только локальные development defaults и реально используемые variables.
- Example shell script прошёл `sh -n` и помечен executable.
- README commands сверены с Makefile, Compose и актуальными environment variable names.
- README-команда `make test` фактически выполнена с Go 1.24.7 и завершилась успешно.
- Комментарии в Go-коде ограничены обязательным integration build tag; искусственные комментарии, пересказывающие код, отсутствуют.

## Testing

- `gofmt -w cmd internal` — успешно; `gofmt -l cmd internal` не вывел файлов.
- `go vet ./...` — успешно, без замечаний.
- `go test ./...` — успешно.
- `go test -count=1 -race ./...` — успешно, тесты действительно перезапущены без cache.
- `go test -count=1 -coverprofile=... ./...` — успешно.
- `go test -tags=integration -run '^$' ./...` — integration-tagged code успешно скомпилирован.
- PostgreSQL integration tests не выполнялись: Docker daemon и локальная PostgreSQL недоступны.
- `git diff --check` — успешно.
- Дополнительный поиск trailing whitespace по всем файлам — чисто; это важно, поскольку в текущей рабочей копии ещё нет tracked baseline.
- `docker compose config --quiet` — успешно.
- OpenAPI YAML разобран и проверен на наличие всех четырёх operations.
- CI YAML разобран без синтаксических ошибок.
- `docker compose up --build -d` — запускался и завершился ошибкой из-за отсутствующего локального Docker socket.

Все Go-команды выполнялись Go 1.24.7 из временного toolchain, поскольку `go` отсутствует в PATH среды.

## Coverage

Фактически измеренный общий unit coverage: **55.3% statements**.

- `internal/config`: 92.9%
- `internal/delivery`: 77.3%
- `internal/handler`: 74.3%
- `internal/targetpolicy`: 73.7%
- `cmd/server`, `model`, `repository` и `service`: 0% в unit profile; repository покрывается отдельным integration suite, который не выполнялся в текущей среде.

Coverage не увеличивался искусственными тестами для простых getters/wiring.

## CI

Workflow остаётся одним компактным job на `push` и `pull_request`. Он настраивает stable Go, поднимает PostgreSQL 16 с healthcheck, проверяет formatting и vet, запускает unit/race tests, применяет существующие migrations через `migrate/migrate`, выполняет integration suite с race detector и проверяет clean diff.

Локально подтверждена синтаксическая корректность YAML и соответствие command/env names проекту. Сам GitHub Actions workflow в этой рабочей среде не запускался, поэтому его remote execution status не заявляется.

## Known limitations

- Delivery имеет at-least-once semantics.
- Duplicate возможен в crash window между успешным target response и `MarkDelivered`.
- PostgreSQL queue рассчитана на текущий scope и не заявлена как решение для massive throughput.
- Нет per-target rate limiting или отдельных concurrency limits.
- Нет external message broker.
- PostgreSQL integration suite подготовлен, но не был фактически выполнен в текущей среде.
- Stale recovery не увеличивает attempts при неизвестном результате прерванной delivery.
- SSRF policy разрешает любые global-unicast targets и не является domain allowlist или абсолютной sandbox.
- List API использует только limit без cursor pagination.
- Для больших response bodies конкретное HTTP connection может не переиспользоваться после ограниченного drain.
- Перед публикацией требуется заменить локальный module path на фактический repository import path.
- Все файлы рабочей копии пока untracked; commit намеренно не создавался без явного запроса пользователя.

## Why this project is technically interesting

- Идемпотентное создание опирается на database unique constraint, а не race-prone предварительный SELECT.
- PostgreSQL используется как transactional queue без лишнего broker для текущего scope.
- `FOR UPDATE SKIP LOCKED` обеспечивает конкурентное непересекающееся claiming.
- Explicit claim token предотвращает stale-worker writes после recovery и reclaim.
- Fixed worker pool и channel демонстрируют bounded Go concurrency и понятный ownership.
- Retry classification и exponential backoff имеют однозначную attempts semantics.
- At-least-once guarantee и невозможность exactly-once HTTP delivery документированы честно.
- SSRF policy встроена непосредственно в DNS-to-dial path и учитывает redirects/proxies.

## Final code review risks

- SQL execution plan и fairness claiming при большой таблице и постоянном потоке jobs.
- Migration 000003 вверх/вниз на базе с существующими processing rows.
- Race recovery против реального медленного target и late owner update.
- Поведение safe dialer с platform-specific DNS ordering, IPv4/IPv6 и connection reuse.
- Shutdown при одновременно зависшем DNS, недоступной PostgreSQL и заполненном batch.
- Destructive integration cleanup при неверно указанном `TEST_DATABASE_URL`.

## Project status

Код, unit/race tests, документация, OpenAPI, CI definition и interview materials готовы для code review и технического собеседования. Проект структурно готов к демонстрации, но перед утверждением полностью проверенного local demo необходимо один раз запустить Docker Compose, migrations, PostgreSQL integration suite и реальный delivery flow в среде с работающим Docker daemon.
