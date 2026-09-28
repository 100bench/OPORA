# Архитектура Опоры

## Цель и граница доверия

Опора — локальный gateway между Android-клиентом и API провайдера моделей.
Android отправляет только данные текущего явно разрешённого взаимодействия.
Gateway держит ключ провайдера, валидирует лимиты, управляет сессией и
подтверждениями и возвращает структурированный, проверенный результат.

Backend реализован и находится в фазе `BACKEND_OFFLINE_VERIFIED`. Локальные
проверки прошли, а реализация получила
[итоговое независимое одобрение](reviews/implementation-review.md). Live-проверки
моделей ещё не выполнены, Android-клиент не начат.

```text
Android client (planned, not implemented)
    │ external use requires HTTPS termination (not included)
    ▼
Go gateway (single process, loopback HTTP by default)
    ├── HTTP handlers: validation and transport DTOs
    ├── use cases: session, turn, confirmation, cancellation
    ├── domain: state machine and safety policy
    ├── in-memory session store (mutex, TTL, capacity)
    └── provider adapters
            ├── speech-to-text
            └── structured reasoning + vision
```

Gateway убирает ключ и прямой доступ к провайдеру из клиента, но сам по себе не
меняет региональные и договорные условия API. Proxy/VPN не является решением
таких ограничений, и проект не должен предлагать схему обхода.

Runtime по умолчанию слушает `127.0.0.1:8080` и не реализует TLS. Это не
production multi-user boundary. Для будущего мобильного подключения нужен
отдельно спроектированный HTTPS ingress/termination и безопасная сеть.

## Компоненты и зависимости

Текущая реализация намеренно компактна:

```text
internal/app                    HTTP router and transport adapters
internal/service                in-memory state machine and orchestration
internal/domain                 API/domain types and stable errors
internal/policy                 safety validation boundary
internal/validation             strict tree, image and WAV validation
internal/infrastructure/auth    hashed per-device bearer tokens
internal/infrastructure/config  validated environment configuration
internal/infrastructure/openai  Responses and transcription adapters
```

`cmd/service` запускает HTTP runtime и корректно завершает его по SIGINT/SIGTERM;
композиция зависимостей остаётся в `internal/app`. Сложные операции можно
постепенно
выделять в `internal/api/handler/<snake_case_operation>` и
`internal/usecase/<operation>` с отдельными contract/request/response файлами.
Пустые обёртки только ради структуры не нужны. Сквозные проверки размещаются в
`tests/integration` и `tests/scenarios`.

Направление зависимостей — снаружи внутрь: app/handler вызывает service/use
case, тот опирается на domain/policy и собственные интерфейсы, infrastructure
реализует эти интерфейсы. Domain и policy не знают о HTTP, `chi` или OpenAI.

В проекте один модуль `github.com/100bench/OPORA`, один бинарник и один `Dockerfile`. Router —
`chi/v5`, структурные логи — `log/slog`. В MVP нет БД, Redis, очередей и
отдельных worker services. Фонового TTL janitor нет: очистка выполняется лениво
при create/access/callback и полностью при перезапуске.

Текущий Dockerfile — multi-stage build с non-root runtime и source allow-list:
в контекст образа входят только module metadata, `cmd` и `internal`.
Презентации, media, `.env`, секреты и приватные корпуса не копируются. Linux
cross-build проверен, но сам Docker image не собирался: daemon был выключен.

## HTTP-контракт

Канонические DTO и коды ошибок определяет [api.yaml](../api.yaml). Набор
маршрутов MVP:

| Метод и путь | Назначение |
|---|---|
| `GET /health` | liveness процесса |
| `GET /ready` | проверка локальной provider-конфигурации |
| `POST /v1/sessions` | создать сессию |
| `POST /v1/transcriptions` | принять WAV и вернуть/поставить транскрипцию |
| `POST /v1/sessions/{session_id}/turns` | начать новый turn |
| `POST /v1/sessions/{session_id}/confirmations` | атомарно подтвердить действие |
| `POST /v1/sessions/{session_id}/results` | принять наблюдаемый клиентом результат действия |
| `POST /v1/sessions/{session_id}/cancel` | отменить активную работу |

Защищённые маршруты используют bearer per-device token. Backend определяет
device ID из токена, а не доверяет идентификатору из request body. При создании
сессии клиент передаёт только безопасные локальные ссылки:
`apps[{ref,label,aliases?}]` и `contacts[{ref,label,aliases?}]` — без адресной
книги и номеров телефонов. Foreground package в screen snapshot допустим как
контекст текущего экрана, но никогда не является launch authority или
произвольной целью действия.

Успешный ответ имеет один из видов `explain`, `highlight`, `clarify`,
`action_proposal`, `need_image` или `refuse`. Ошибка имеет стабильную оболочку
`{error:{code,message,request_id?}}`.

Идемпотентный повтор запроса не создаёт вторую операцию. Для одинаковых ключа и
hash незавершённый запрос возвращает `409 IN_PROGRESS`, а завершённый повторяет
сохранённый ответ. Тот же ключ с другим hash возвращает `409 CONFLICT`. Клиент
дополнительно дедуплицирует ответы. Идентификаторы сессии и операции непрозрачны
и не несут пользовательских данных.

## Сессия, generation и отмена

Сессии хранятся в mutex-protected map в памяти процесса:

- TTL — 30 минут;
- ёмкость — 100 активных сессий;
- перезапуск не восстанавливает сессии;
- протухшие записи удаляются безопасно относительно активных запросов.

TTL считается от последней принятой аутентифицированной операции. При
`now >= expires_at` запись уже недоступна, но физически удаляется лениво при
следующей операции/callback или перезапуске — это не precise memory wipe. На
сессию допускается до 128 idempotency entries;
после достижения лимита новые operation IDs отклоняются без удаления защиты от
повтора. Для глобальной транскрипции используется эквивалентный device-scoped
ledger до 128 записей.

Каждый новый turn отменяет предыдущий и увеличивает `generation`. Результат
принимается, только если его session/generation всё ещё актуальны. Явный cancel
терминален для сессии: новая работа требует новой сессии. После отмены или нового
turn поздний ответ провайдера считается stale и не может изменить состояние.
Idempotency защищает сервер от повторного выполнения, а client dedup — UI от
повторного отображения.

Одновременно выполняются не более четырёх запросов к провайдеру. На каждый
устанавливается hard timeout не более 20 секунд и распространяется отмена
клиентского контекста.

Концептуальные состояния сессии: `ACTIVE`, `PROCESSING`,
`AWAITING_CONFIRMATION`, `ACTION_ISSUED`, `CANCELLED` и `EXPIRED`. Подтверждение
выполняет переход атомарно: повторный или просроченный запрос не может выпустить
действие второй раз.

## Обработка запроса

1. Handler проверяет content type, размер и структуру, затем нормализует DTO.
2. Use case сверяет сессию, idempotency key, generation и допустимый переход.
3. Audio проходит speech-to-text через
   `gpt-4o-mini-transcribe-2025-12-15`.
4. Для ограниченных явных русских launch/dialer/highlight intents policy может
   вернуть детерминированный offline-результат без provider call. Иначе
   accessibility tree или изображение вместе с запросом передаются в
   `gpt-5.4-mini-2026-03-17` с требованием structured output.
5. Ответ модели декодируется строго по схеме и проходит domain policy.
6. Объяснение и ссылки на элементы возвращаются клиенту. Action proposal
   создаётся только из фактического запроса пользователя. В принятом для MVP
   консервативном контракте и `OPEN_APP`, и `PREPARE_DIALER` получают
   одноразовое подтверждение сроком 30 секунд.
7. Android самостоятельно выполняет разрешённый platform intent. Backend не
   инициирует звонок и не нажимает элементы интерфейса.

## Политика действий

По умолчанию результат read-only. Разрешены:

- `OPEN_APP` с проверенной непрозрачной `app_ref`;
- `PREPARE_DIALER` только как подготовка системного dialer; номер разрешается
  локально из ссылки на избранный контакт.

Запрещены raw coordinates, произвольные UI taps, платежи, отправка сообщений,
звонок без участия пользователя, пароли, OTP и изменение security settings.
Модель не может предлагать или инициировать действие без соответствующего
пользовательского intent: при отсутствующем или неоднозначном intent ответом
служит explain/clarify/refuse, а не action proposal.
Изображение используется только для объяснения и не может быть источником
authority для действия. Sensitive nodes удаляются до provider call, а
protected/sensitive экран отклоняется fail-closed.
Ссылка подсветки должна включать идентичность/версию экрана и стабильную ссылку
на элемент. Разрешение на screenshot выдаётся отдельно для каждого экрана и
версии; переносить его между generation нельзя.

## Данные и наблюдаемость

Исходное аудио, screenshot, accessibility tree и transcript существуют только
в памяти на время сессии. Они не попадают в постоянные логи или файлы. В логах
допустимы request/session IDs, размеры, latency, выбранный путь обработки,
категория ошибки и агрегированные метрики, но не пользовательское содержимое.

Даже без адресной книги аудио может содержать имя, номер или иные персональные
данные. Архитектура минимизирует передачу, но не делает запрос анонимным.
Фильтр отклоняет URI-подобные target refs и очевидные phone/URI patterns в
contact metadata, но не является универсальным PII detector.

`/ready` не выполняет сетевой вызов: он проверяет наличие непустого ключа,
закреплённых model IDs и валидного endpoint. Поэтому `200` означает только
локальную готовность конфигурации, а не валидность ключа или доступность модели.

`store: false` следует включать в запросах, где это поддерживается, однако это
не эквивалент Zero Data Retention. Сроки хранения и eligibility проверяются по
условиям аккаунта и актуальной документации провайдера.

Основные официальные материалы:

- [Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs)
- [Images and vision](https://developers.openai.com/api/docs/guides/images-vision)
- [Speech to text](https://developers.openai.com/api/docs/guides/speech-to-text)
- [Data controls](https://developers.openai.com/api/docs/guides/your-data)
- [Supported countries and territories](https://developers.openai.com/api/docs/supported-countries)

## Ограничения ресурсов

| Вход/ресурс | Лимит MVP |
|---|---:|
| WAV PCM | 16 kHz, mono, 16-bit, ≤ 30 s, ≤ 1 MiB |
| JPEG/PNG | ≤ 5 MiB, ≤ 4 MP |
| Accessibility tree | ≤ 500 nodes, ≤ 256 KiB |
| Активные сессии | ≤ 100 |
| Session TTL | 30 min |
| Confirmation TTL | 30 s |
| Provider timeout | ≤ 20 s |
| Provider concurrency | ≤ 4 |

Лимиты проверяются до дорогой обработки. Нарушение возвращает стабильную
машиночитаемую ошибку и не отправляет содержимое провайдеру.
