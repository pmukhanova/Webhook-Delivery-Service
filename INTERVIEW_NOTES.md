# Interview Notes

## Почему PostgreSQL используется как queue?

Jobs уже являются частью основной модели данных, поэтому PostgreSQL даёт durability, transactions, constraints и locking без отдельной инфраструктуры. Для текущего масштаба это проще в эксплуатации и достаточно для демонстрации корректного claiming. Цена решения — polling и меньшая специализация под очень большой поток сообщений по сравнению с broker.

## Как работает FOR UPDATE SKIP LOCKED?

Каждый claimer выбирает только ready rows и ставит на них row locks. `SKIP LOCKED` заставляет конкурентную transaction пропустить уже занятые rows, а не ждать их. В той же transaction выбранные jobs переводятся в `processing`, после чего commit происходит до HTTP delivery.

## Зачем нужен claim_token?

Одного `status = processing` недостаточно: stale job может быть восстановлена и выдана другому worker, пока старый worker ещё выполняется. Новый UUID создаётся для каждого claim, а все final updates требуют текущий token. Поэтому поздний результат старого owner получает `ErrClaimLost` и не меняет строку.

## Возможна ли duplicate delivery?

Да. Если target уже принял request, но процесс завершился до записи `delivered`, stale recovery приведёт к повторной отправке. Стабильный `X-Webhook-ID` позволяет receiver дедуплицировать такие повторы.

## Почему не exactly-once?

HTTP side effect на внешнем target и commit в локальной PostgreSQL нельзя объединить в одну atomic transaction. Без участия receiver сервис должен выбирать между возможной потерей и возможным повтором; здесь выбрана at-least-once delivery с явным deduplication key.

## Как работает retry?

Network errors, timeout, HTTP 408, 429 и 5xx считаются retryable. После каждой зафиксированной неудачи `attempts` увеличивается, а `next_attempt_at` получает задержку 2s, 4s, 8s и далее до cap 60s. На последней разрешённой попытке job становится `failed`; прочие 4xx и 3xx завершаются permanent failure сразу.

## Что означает attempts?

Это число delivery attempts, результат которых успешно записан в PostgreSQL. Claim сам по себе значение не меняет. Успешная доставка, запланированный retry и permanent failure увеличивают его ровно один раз; recovery неизвестного результата после crash не увеличивает.

## Что происходит при crash worker?

Job остаётся `processing` с `processing_started_at` и token старого owner. Dispatcher после `PROCESSING_TIMEOUT` очищает ownership и переводит её в ready retry. Если старый worker всё же вернётся, его token уже недействителен.

## Как работает graceful shutdown?

SIGINT/SIGTERM отменяет общий application context. Dispatcher перестаёт claim'ить jobs, outbound requests получают cancellation, pool закрывает channel только после остановки sender и ждёт workers через `WaitGroup`. HTTP server получает отдельный bounded shutdown, затем закрывается общий pgx pool.

## Какие race conditions были возможны?

Предварительный SELECT перед idempotent INSERT создал бы race, поэтому используется unique constraint с `ON CONFLICT`. Concurrent dispatchers могли выбрать одну job без row locking, поэтому claim использует `SKIP LOCKED`. Старый worker мог перезаписать новый claim после recovery, поэтому final updates проверяют explicit token. Channel закрывает только pool после возврата единственного sender.

## Как защищаемся от SSRF?

URL ограничен HTTP(S), credentials и literal internal addresses запрещены. Перед каждым новым connection custom dialer один раз разрешает hostname, отклоняет весь answer при наличии non-public IP и передаёт underlying dialer уже numeric validated IP. Proxies и redirects отключены, поэтому они не обходят этот путь. Это focused mitigation, а не абсолютная sandbox или domain allowlist.

## Почему worker pool фиксированного размера?

Worker count задаёт верхнюю границу одновременных outbound requests и числа worker goroutines. Это защищает process, database и targets от concurrency, пропорциональной размеру очереди. Dispatcher и unbuffered channel подают работу только доступным workers.

## Как обеспечивается идемпотентное создание job?

`idempotency_key` имеет PostgreSQL unique constraint. Repository делает `INSERT ... ON CONFLICT DO NOTHING RETURNING`; при конфликте читает существующую строку. Корректность не зависит от предварительной проверки в приложении и сохраняется при concurrent requests.

## Почему transaction не держится во время HTTP request?

Transaction нужна только для выбора и смены ownership. Она commit'ится до передачи job worker, поэтому медленный target не удерживает row lock и database connection. Результат записывается отдельным conditional update по claim token.

## Кто владеет channel и почему нет send on closed channel?

Channel создаёт pool. Dispatcher — единственный sender, workers — только receivers. `Pool.Run` вызывает dispatcher синхронно и закрывает channel лишь после его возврата, затем ждёт workers; после close уже нет goroutine, имеющей право отправлять.

## Что бы ты изменил для значительно большей нагрузки?

Сначала измерил бы contention, polling cost, delivery latency и распределение нагрузки по targets. Возможные направления — отдельный message broker, horizontal worker processes, partitioning jobs, per-target concurrency/rate limits, richer observability и operational DLQ workflow. Эти изменения нужны только после появления требований и измерений; текущий проект не заявляет готовность к massive scale.
