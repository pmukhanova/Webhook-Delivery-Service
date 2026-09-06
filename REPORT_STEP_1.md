# Step 1 Report

## Что реализовано

- REST API для создания webhook job, получения одной job и списка с фильтром `status` и ограничением `limit`.
- Обязательный `Idempotency-Key` и атомарная защита от дублей на уровне PostgreSQL.
- Валидация URL, типа события, JSON payload, UUID, статуса и limit; единый JSON-формат ошибок.
- PostgreSQL-схема, up/down migrations, полезные индексы и ограничения целостности.
- Health endpoint с проверкой PostgreSQL, graceful shutdown и структурированные логи через `log/slog`.
- Dockerfile, Docker Compose и table-driven тесты HTTP-слоя.

## Измененные файлы

- `cmd/server/main.go` — собирает зависимости, подключается к PostgreSQL и управляет жизненным циклом HTTP-сервера.
- `internal/config/config.go` — читает адрес сервера и обязательный `DATABASE_URL` из environment variables.
- `internal/model/webhook.go` — содержит модель webhook job и допустимые статусы.
- `internal/repository/webhook.go` — выполняет параметризованные PostgreSQL-запросы через `pgxpool`.
- `internal/service/webhook.go` — связывает HTTP use cases с repository и создаёт UUID новых jobs.
- `internal/handler/http.go` — реализует маршруты, валидацию, JSON responses и преобразование ошибок.
- `internal/handler/http_test.go` — проверяет основные успешные и ошибочные HTTP-сценарии без реальной БД.
- `migrations/000001_create_webhook_jobs.up.sql` — создаёт таблицу, constraints и индексы.
- `migrations/000001_create_webhook_jobs.down.sql` — удаляет таблицу для отката миграции.
- `Dockerfile` — собирает минимальный непривилегированный container image приложения.
- `docker-compose.yml` — описывает PostgreSQL, запуск миграций и приложение с dependency health conditions.
- `Makefile` — содержит короткие команды разработки и запуска.
- `go.mod` и `go.sum` — фиксируют модуль и версии Go-зависимостей.
- `.env.example` — показывает локальные настройки Compose без production credentials.
- `.gitignore` — исключает локальные environment-файлы и артефакты.
- `README.md` — кратко описывает запуск, конфигурацию, API и тесты.

## Архитектурные решения

1. HTTP flow разделён на `handler -> service -> repository`: handler отвечает за HTTP и валидацию, service — за use case и UUID, repository — только за SQL. Единственный интерфейс расположен у handler, где он практически нужен для быстрых тестов; интерфейс repository не добавлялся.
2. Идемпотентность обеспечивается `UNIQUE (idempotency_key)` и `INSERT ... ON CONFLICT DO NOTHING RETURNING`. Предварительный `SELECT` не используется, поэтому конкурентные запросы не могут вставить две записи; проигравший запрос после конфликта читает существующую job.
3. PostgreSQL хранит payload как `JSONB`, статусы ограничены `CHECK`, числовые поля защищены от некорректных значений, а timestamps и начальные значения задаются defaults базы.
4. Индекс `(created_at DESC)` обслуживает общий список, `(status, created_at DESC)` — список с фильтром, а частичный `(status, next_attempt_at, created_at)` подготовлен только для будущего выбора due `pending/retry` jobs.
5. SQL остаётся статическим и параметризованным через pgx. Это исключает SQL injection и не требует query builder.
6. Миграции выполняются стандартным образом через отдельный `migrate/migrate` container; приложение стартует только после healthcheck PostgreSQL и успешной миграции.

## Flow запроса

`POST /api/v1/webhooks` сначала проверяет `Idempotency-Key`, декодирует ограниченное по размеру тело, запрещает неизвестные поля и валидирует URL, event type и payload. Handler передаёт request context и данные в service, service создаёт UUID и вызывает repository. Repository выполняет один параметризованный `INSERT`, PostgreSQL выставляет `pending`, `attempts = 0`, `max_attempts = 5` и timestamps, после чего handler возвращает `202 Accepted` с ID и статусом.

При повторном `Idempotency-Key` unique constraint отклоняет вставку внутри `ON CONFLICT`. `RETURNING` не возвращает строку, repository загружает запись по тому же ключу и API снова отвечает `202` с ID и текущим статусом существующей job. Это работает и при конкурентных запросах: PostgreSQL сериализует конфликт уникального индекса, а новый row не создаётся.

## Проверки

- `gofmt -w cmd internal` — выполнен успешно; последующий `gofmt -l cmd internal` не вывел файлов.
- `go vet ./...` — выполнен успешно без замечаний с официальным Go 1.24.7 toolchain, загруженным во временную директорию, поскольку `go` отсутствовал в PATH.
- `go test ./...` — выполнен успешно; `internal/handler` прошёл, остальные packages не содержат тестов.
- `go test -race ./...` — дополнительная проверка выполнена успешно.
- `docker compose config` — выполнен успешно, итоговая Compose-конфигурация валидна.
- `docker compose up --build -d` — запускался, но не выполнен: Docker daemon недоступен (`.../.docker/run/docker.sock: no such file or directory`).
- curl-проверки не выполнялись, потому что без Docker daemon приложение и PostgreSQL не удалось запустить. Результат не подменялся mock-проверкой.

## Найденные и исправленные проблемы

- Исправлена синтаксическая ошибка закрытия table-driven subtest, обнаруженная первым запуском `gofmt`/`go test`.
- Маршруты зарегистрированы точно без обязательного trailing slash, как указано в API-контракте.
- Добавлен отдельный индекс `created_at DESC`: составной индекс со статусом не оптимизировал бы нефильтрованный текущий список.
- Для неизвестного route и неподдерживаемого метода добавлены JSON-ошибки вместо стандартного plain-text ответа router.
- После изменений повторно выполнены formatter, vet, обычные тесты и race detector.

## Известные ограничения / TODO

- Нет интеграционного теста repository с реальным PostgreSQL; конкурентная идемпотентность опирается на PostgreSQL unique constraint и требует проверки при доступном Docker.
- Нет пагинации по cursor/offset: на Step 1 реализован только требуемый limit.
- Нет аутентификации, TLS termination, метрик и request tracing.
- Worker pool, retry delivery и любое background processing намеренно не реализованы: они относятся к Step 2.

## Что проверить на внешнем code review

- Поведение `INSERT ... ON CONFLICT` при реально конкурентных запросах с одним ключом.
- Достаточность и стоимость трёх индексов на ожидаемом профиле запросов.
- Границы HTTP-валидации URL и payload для будущей защиты delivery-компонента от SSRF.
- Корректность lifecycle Compose migration container при повторных стартах и откатах.
- Нужна ли продукту проверка совпадения body при повторном `Idempotency-Key`, поскольку текущий контракт всегда возвращает первую job.

## Следующий этап

- Реализовать безопасное конкурентное получение due jobs через транзакцию и `FOR UPDATE SKIP LOCKED`.
- Добавить ограниченный worker pool и корректное завершение workers.
- Выполнять HTTP delivery с timeouts и обязательным закрытием response body.
- Добавить exponential backoff, `next_attempt_at`, фиксацию `last_error` и переходы статусов.
- Покрыть retry/state transitions интеграционными тестами с PostgreSQL и тестовым webhook endpoint.
