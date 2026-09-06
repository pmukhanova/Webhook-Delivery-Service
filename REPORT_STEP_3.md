# Step 3 Report

## Что реализовано

- Ownership processing jobs переведён с `updated_at` на явный UUID `claim_token`.
- Stale recovery использует отдельный `processing_started_at` и очищает ownership.
- Добавлен настоящий PostgreSQL integration suite под build tag `integration`.
- Добавлены concurrency tests для claiming и Idempotency-Key, а также stale-worker test.
- Добавлена SSRF-защита на уровне URL validation и непосредственного outbound dial.
- Redirects отключены; environment HTTP proxies не используются.
- Добавлен GitHub Actions CI с PostgreSQL 16, migrations, unit/race/integration checks.
- Обновлены Makefile, README и дополнительные unit tests безопасности.

## Изменения схемы БД

Migration `000003_add_explicit_claim_ownership` добавляет:

- `claim_token UUID NULL` — уникальный идентификатор конкретного владения job;
- `processing_started_at TIMESTAMPTZ NULL` — момент начала текущего processing lease;
- `webhook_jobs_processing_ownership_check` — требует оба ownership-поля для `processing` и запрещает их для остальных статусов;
- partial index `webhook_jobs_processing_started_at_idx` для поиска stale processing jobs.

Старый partial index по `updated_at` удаляется. При миграции уже существующие `processing` jobs безопасно возвращаются в `retry`, потому что для них невозможно восстановить корректный token задним числом. Down migration удаляет новые columns/constraint/index и восстанавливает прежний recovery index.

## Claim ownership model

1. `ClaimReadyJobs` блокирует ready candidates через `FOR UPDATE SKIP LOCKED`.
2. В том же atomic statement status меняется на `processing`, PostgreSQL генерирует новый `claim_token`, а `processing_started_at` фиксируется через `clock_timestamp()`.
3. Transaction полностью читает `RETURNING` и выполняет commit до передачи job worker и до HTTP request.
4. Worker получает job вместе с token и передаёт его в `MarkDelivered`, `ScheduleRetry` или `MarkFailed`.
5. Каждый final update использует `WHERE id = $1 AND status = 'processing' AND claim_token = $2` и при успехе очищает token и processing timestamp.
6. Ноль affected rows возвращается как `ErrClaimLost`; worker логирует `webhook_claim_lost` и не пытается перезаписать актуальный state.

В stale-worker сценарии recovery очищает token A, после чего новый claim создаёт token B. Поздний Worker A не проходит conditional update с token A. Только Worker B с token B может завершить новую обработку.

## PostgreSQL integration tests

Файл `internal/repository/webhook_integration_test.go` собирается с `-tags=integration`, использует обязательный `TEST_DATABASE_URL`, реальный `pgxpool` и уже применённые migrations проекта. Перед каждым top-level test и каждым stateful subtest таблица очищается через `TRUNCATE`; suite предназначен только для отдельной test database.

Реализованы сценарии:

- две одновременные `ClaimReadyJobs` получают непересекающиеся ID;
- все claimed rows имеют `processing`, token и `processing_started_at`;
- tokens разных claims не повторяются;
- `pending -> processing` и ready `retry -> processing`;
- future retry не claim'ится;
- stale processing восстанавливается, fresh processing не восстанавливается;
- stale Worker A не может завершить claim Worker B;
- `MarkDelivered`, `ScheduleRetry` и `MarkFailed` требуют актуального owner token;
- job с `attempts == max_attempts` не claim'ится;
- восемь concurrent inserts с одним Idempotency-Key создают одну row и возвращают один ID.

Suite успешно compile-checked с integration build tag, но фактически против PostgreSQL в этой среде не запускался из-за недоступного Docker daemon.

## SSRF protection

На создании webhook проверяются синтаксис URL, HTTP/HTTPS scheme, отсутствие credentials, literal IP и специальные localhost names. Основная защита находится в `http.Transport.DialContext`, поэтому она действует непосредственно перед каждым новым connection:

1. hostname разрешается injected resolver ровно один раз;
2. все полученные IP проверяются до dial;
3. loopback, private, link-local, unspecified, multicast и остальные non-global-unicast destinations блокируются для IPv4 и IPv6;
4. при наличии хотя бы одного запрещённого IP весь resolution отклоняется;
5. underlying dialer получает numeric validated IP, а не hostname — второго DNS lookup между validation и connection нет.

Environment proxy отключён, иначе proxy мог бы разрешить исходный private hostname вне контролируемого dialer. Established connections могут переиспользоваться: они уже соединены с проверенным numeric public IP; каждое новое connection снова проходит resolution и policy. HTTP redirects полностью отключены через `CheckRedirect`, а 3xx классифицируется как permanent failure, поэтому redirect на private address не обходится.

Unit tests не используют внешний DNS или интернет: fake resolver/dialer проверяют public resolution, private/loopback/link-local и смешанный DNS result, точный numeric dial и отсутствие второго resolve. Table-driven URL tests покрывают localhost, IPv4/IPv6 loopback/private/link-local, metadata address `169.254.169.254`, unsupported scheme, malformed URL, credentials и public IPv4/IPv6.

Текущая policy не является domain allowlist и не анализирует поведение самого удалённого public сервиса. Она защищает сетевое соединение сервиса, но не может запретить публичному target самостоятельно обращаться во внутреннюю сеть.

## CI

`.github/workflows/ci.yml` запускается на `push` и `pull_request`. Один job использует stable Go через `actions/setup-go@v7` и PostgreSQL 16 service container с healthcheck.

CI выполняет formatting check, `go vet ./...`, unit tests, race tests, применяет существующие SQL migrations через тот же `migrate/migrate:v4.18.3`, запускает integration suite с race detector и завершает `git diff --check`. Workflow имеет read-only permission к repository contents и ограничение времени.

## At-least-once semantics

Сервис по-прежнему имеет at-least-once, а не exactly-once semantics. Если target обработал request и вернул 2xx, но процесс завершился до `MarkDelivered`, recovery позже создаст новый claim и повторит отправку.

`X-Webhook-ID` стабилен для всех attempts одной job, поэтому receiver может реализовать собственную дедупликацию. Claim token защищает только внутреннее состояние PostgreSQL от stale workers; он не устраняет внешний crash window и не должен трактоваться как exactly-once guarantee.

## Проверки

- `gofmt -w cmd internal` — успешно; финальный `gofmt -l cmd internal` не вывел файлов.
- `go vet ./...` — успешно, без замечаний.
- `go test ./...` — успешно; config, delivery, handler и targetpolicy packages прошли.
- `go test -race ./...` — успешно.
- `go test -tags=integration -run '^$' ./...` — integration-tagged suite успешно скомпилирован; сами integration tests этим command не выполнялись.
- PostgreSQL integration tests — не запускались, потому что PostgreSQL container недоступен без Docker daemon.
- `git diff --check` — успешно, без замечаний.
- `docker compose config --quiet` — успешно.
- `.github/workflows/ci.yml` — синтаксически разобран Ruby YAML parser без ошибки.
- `docker compose up --build -d` — запускался и завершился ошибкой: локальный Docker socket отсутствует.

Все Go-команды выполнены официальным Go 1.24.7 toolchain из временной директории, поскольку `go` отсутствует в PATH среды.

## Найденные и исправленные проблемы

- `updated_at` был нестрогим ownership marker; заменён на случайный UUID token, проверяемый всеми final updates.
- Recovery ориентировался на общий timestamp изменений; теперь используется только `processing_started_at`.
- Stale worker мог логироваться как обычная database error; добавлена отдельная ожидаемая ошибка `ErrClaimLost` и structured event.
- Environment proxy мог обойти custom destination dial policy; proxy support у outbound transport отключён.
- Redirect мог направить client на private destination; redirects полностью отключены и покрыты тестом.
- DNS result с одновременно public и private IP мог быть неоднозначным; policy отклоняет весь такой result.
- Safe dialer первоначально выбирал только первый public address; добавлен последовательный fallback по уже проверенному набору без повторного DNS.
- Старый README curl использовал localhost target, который корректно блокируется новой policy; пример заменён на public HTTPS URL.

## Известные ограничения

- PostgreSQL integration suite написан, но не был исполнен против реальной БД в текущей среде.
- Integration tests используют destructive `TRUNCATE` и требуют выделенную test database; их нельзя направлять на development/production database.
- At-least-once crash window допускает duplicate delivery.
- PostgreSQL должен поддерживать built-in `gen_random_uuid()`; выбранные PostgreSQL 16/17 images это обеспечивают.
- Stale recovery не увеличивает `attempts`, поскольку результат прерванного request неизвестен; повторяющиеся process crashes сами не исчерпывают лимит.
- SSRF policy разрешает любые global-unicast destinations и не предоставляет domain allowlist.
- Большой response body читается не более чем на 64 KiB перед закрытием, поэтому такое конкретное connection может не переиспользоваться.

## Что проверить на внешнем code review

- Выполнение migration 000003 вверх/вниз на PostgreSQL с реальными существующими processing rows.
- Execution plan и непересечение результатов concurrent `FOR UPDATE SKIP LOCKED` claims.
- Все ветки final state transitions на предмет обязательного token predicate и очистки ownership columns.
- Гонку recovery против позднего Worker A и нового Worker B на реальном PostgreSQL.
- Semantics `attempts` на последней retryable и permanent attempts.
- DNS/IP policy для IPv4-mapped IPv6, mixed answers, connection reuse и cancellation во время resolution/dial.
- Отсутствие SSRF bypass через proxy, redirects, URL credentials и hostname normalization.
- Безопасность destructive integration cleanup и корректность CI migration command.

## Следующий этап

- Запустить CI и integration suite на реальном PostgreSQL, устранить обнаруженные environment-specific расхождения.
- Добавить короткий reproducible demo script и interview-ready architecture diagram.
- Провести небольшой load/concurrency test и зафиксировать результаты без изменения core architecture.
- Добавить OpenAPI-описание текущего API и финальные примеры error/delivery flows.
- Подготовить repository metadata, license и компактное описание ключевых trade-offs для презентации проекта.
