# Независимое ревью реализации backend

Дата: **2026-09-28**  
Ревьюер: независимый агент `review_implementation` (`GPT-5.6 Sol`, reasoning
`xhigh`)  
Фаза: `BACKEND_OFFLINE_VERIFICATION`  
Решение: **APPROVED — deterministic offline backend implementation**

## Граница решения

Одобрена реализация backend в пределах замороженных `SPEC`, OpenAPI и
тестового контракта. Решение подтверждает локальные deterministic paths,
state/safety/concurrency rules, HTTP/runtime и wire-level provider adapters.

Это решение **не** означает live-валидацию моделей, production readiness,
готовность Docker image или разрешение начинать Android только на основании
offline-тестов. Обязательные live-gates остаются `BLOCKED / NOT RUN` до появления
credentials, явно разрешённого корпуса и cost approval.

Автор реализации и ревьюер — разные агенты. Ревьюер не изменял production-код
или тесты. Принадлежащие ревьюеру изменения ограничены этим отчётом и финальной
секцией в `fixture-amendment-review.md`; deliberate mutation выполнялась только
во временной копии под `/private/tmp` и была удалена.

## Проверенный срез

Проверены:

- `internal/service`: состояния, TTL, generation, cancel, idempotency,
  error replay, deep copies, confirmation/grant/result и provider admission;
- `internal/policy` и `internal/validation`: authority/grounding, запреты,
  tree/image/WAV boundaries и bounded predecode;
- `internal/app`: auth boundary, canonical JSON keys, duplicate/unknown fields,
  body/multipart limits и error envelopes;
- `internal/infrastructure/auth`, `config`, `openai`: hashed bearer lookup,
  fail-fast configuration, pinned models, strict provider DTO/schema,
  `store:false`, bounded responses, cancellation и secret-safe errors;
- `cmd/service`, lifecycle, Makefile, CI, Docker scaffold и синхронизированная
  документация;
- unit, integration и scenario tests, включая добавленные прямые регрессии для
  late transcription completion, result/cancel ordering и post-admission abort.

Замороженные контрактные файлы совпадают с SHA-1 одобренного baseline:

| Артефакт | SHA-1 |
|---|---|
| `api.yaml` | `1eb2c7c4d16072be4bc6bc58d77df6b711672606` |
| `docs/SPEC.md` | `0751ed256e9c9c8228e42af8f1d30928efb2e69d` |
| `docs/TESTING.md` | `5dac7c40bb0fbad95b16ea19e775dfb7ec775127` |

## Результат проверки кода

### State, expiry и concurrency

- Session и transcription ledgers защищены одним mutex, ограничены по размеру
  и используют exact payload hash для pending/success/error replay. Входные
  slices/DTO и возвращаемые replies/grants копируются, поэтому caller не может
  изменить сохранённое состояние через alias.
- Late provider completion повторно проверяет session TTL, generation/state и
  identity ожидаемых transcription ledger/entry. Старый callback не может
  перезаписать replacement-ledger или оживить отменённую/протухшую операцию.
- `Cancel` и `ReportResult` сериализованы тем же mutex. Обе проверенные
  очередности result/cancel терминальны и не восстанавливают cancelled session.
  Протухший action grant очищается перед новым turn; proposal/grant имеют
  точную 30-секундную границу.
- Общий semaphore действительно ограничивает planner и transcriber четырьмя
  фактически выполняющимися вызовами. Permit освобождается только при выходе
  provider goroutine, включая provider, игнорирующий context. После admission
  повторно проверяются client cancellation и 20-секундный deadline; abort до
  provider invocation освобождает slot и возвращает `started=false`.
- Критические race-сценарии используют barriers/injected clock, а не sleeps.

### Authority, privacy и входы

- Только deterministic policy по явной текущей команде может создать
  `OPEN_APP` или `PREPARE_DIALER`. Action proposal от provider отклоняется как
  `UNSAFE`; отрицательные, гипотетические, запрещённые и неоднозначные команды
  не получают action authority.
- Protected/sensitive input закрывается до provider call. Screenshot требует
  consent для той же screen version и не выдаёт полномочий действию. Empty-tree
  fallback выполняется после deterministic safety policy, поэтому явные
  разрешённые команды и запреты не превращаются ошибочно в `need_image`.
- App/contact targets остаются локальными refs. URI-подобные refs, `tel:`,
  `sms:`, `intent:`, URL и очевидные телефонные последовательности в
  структурированных contact metadata отклоняются. Это намеренно консервативная
  проверка очевидных значений, а не обещание универсального PII detector.
- WAV проверяет согласованные rate/alignment/length. PNG/JPEG проверяются по
  точному envelope, включая trailing/concatenated data; dimensions и 4 MP limit
  проверяются через bounded header/decode-config до полного decode. Base64 для
  idempotency hash декодируется строго, поэтому неканонический invalid payload
  не может получить cached success.

### HTTP, provider и runtime

- HTTP decoder требует canonical case-sensitive keys, отклоняет case aliases,
  duplicates, unknown fields/trailing JSON и ограничивает bodies/multipart до
  разбора. Это закрывает возможность переписать `sensitive`, `protected` или
  `consent` ключом другого регистра.
- Provider adapter использует pinned model IDs, strict recursive JSON Schema с
  `additionalProperties:false`, `store:false`, bounded raw output и выключенные
  redirects. Canonical raw-map parsing и case-fold duplicate detection покрывают
  outer Responses/STT envelopes и inner structured DTO.
- Bearer tokens загружаются из обязательной JSON-конфигурации, хранятся в
  hashed lookup и сравниваются без доверия к device ID из body. Ключ провайдера
  не возвращается в API/ошибках и не логируется вместе с пользовательским
  содержимым.
- `/health` — только liveness; `/ready` проверяет локальную конфигурацию без
  upstream call. Default bind — loopback, TLS/production ingress не заявлены.

## Целостность тестов и fixture amendment

Пять ранее одобренных исправлений фикстур и шестая timing-only коррекция
проверены в применённом срезе:

1. WAV positive fixtures получили корректные `ByteRate`/`BlockAlign`, а
   несогласованные значения остались отдельными negative cases.
2. Успешный `ReplyExplain` содержит непустой `Text`; пустой text явно
   отклоняется.
3. Near-256-KiB tree распределён по 121 node с `text <= 2048`; 122-node
   aggregate overflow и per-node overflow проверяются отдельно.
4. Machine `request_id` больше не наследует пробелы из subtest names; invalid
   ID с пробелом остаётся negative case.
5. Provider-cap fake действительно игнорирует cancellation и выходит только по
   test gate.
6. Финальный `active == 0` ждёт фактического выхода всех provider calls через
   отдельный provider `WaitGroup`, а не только возврата отменённых callers.

Assertions не ослаблены, `t.Skip`, `testing.Short` и исключающие build tags не
добавлены. Добавленные tests усиливают проверки expiry/identity,
result-vs-cancel и post-admission safety.

Репозиторий не содержит Git metadata (`git status` возвращает `not a git
repository`), поэтому полный исторический byte-for-byte diff изменённых test
files восстановить нельзя. Для неизменяемых контрактов использованы baseline
hashes выше; изменённые tests проверены вручную по каждому утверждённому repair,
сохранённым assertions и текущему полному набору negative cases. Это
ограничение доказательства отражено явно, а не выдано за Git-based diff audit.

Текущие SHA-1 ключевых изменённых test files:

| Артефакт | SHA-1 |
|---|---|
| `internal/app/router_test.go` | `d03033f9c7f263cdfeed1dc5d45c44c0a877d4db` |
| `internal/policy/policy_test.go` | `c078fe8605952921bc090ec286da9713c17b4531` |
| `internal/service/service_test.go` | `ce327fdd4c3728b46bbb81139a1a6767f0acfcb9` |
| `internal/service/critical_regression_test.go` | `38d7372de411f4b5a572c5f808582ae80aa02441` |
| `internal/validation/validation_test.go` | `bb151cdb0a816ca5c870d8dc39bc49bcaa0c4bca` |
| `internal/infrastructure/openai/client_test.go` | `b8e631d83553c977451e5091d54405d8faa2ba6a` |

## Самостоятельно выполненные проверки

Команды выполнены ревьюером 2026-09-28 в
`/Users/maksim/Desktop/помош`, Go `1.26.3`, с локальным toolchain, offline
module mode и отдельным cache в `/private/tmp`.

| Проверка | Наблюдаемый результат |
|---|---|
| `test -z "$(gofmt -l cmd internal tests)"` | exit 0; неотформатированных Go-файлов нет |
| `GOTOOLCHAIN=local GOCACHE=/private/tmp/pomosh-go-build-review-final GOPROXY=off GOSUMDB=off COVERPROFILE=/private/tmp/pomosh-core-review-final.cover make check` | exit 0; `go vet`, весь normal suite, весь race suite и coverage gate прошли |
| `go tool cover -func=/private/tmp/pomosh-core-review-final.cover` с тем же `GOCACHE` | total **91.5%**; policy 97.8%, service 90.3%, validation 89.4% |
| `go test -race -count=20 ./internal/service -run 'TestNFR10_ExpiredTranscriptionCompletionCannotOverwriteReplacementLedger\|TestNFR11_ResultCancelRaceIsTerminalAndCannotRevive\|TestNFR13_PostAdmissionGuardsDoNotInvokeProviderOrLeakSlot'` с тем же offline env | exit 0; все три boundary-регрессии прошли 20 раз под race detector |
| `GOLANGCI_LINT_CACHE=/private/tmp/pomosh-golangci-review-final golangci-lint run ./...` с тем же offline env | exit 0; `0 issues` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath ... ./cmd/service` с offline env | exit 0; Linux amd64 binary собран во временный путь и удалён |
| YAML parse `api.yaml` | exit 0; 8 paths / 20 schemas |
| поиск `t.Skip`, `testing.Short`, исключающих build tags и credential-like secret | пропусков gate и реального ключа не найдено |

Дополнительно координатор выполнил process smoke: `/health` 200, `/ready` 503
без ключа, unauthenticated request 401, authenticated synthetic
session → `OPEN_APP` → confirm → result, затем SIGINT exit 0; provider calls 0.
Это offline synthetic доказательство, не live model evaluation.
После последней test-only коррекции координатор также повторил uncached
`go test -count=1 -race -timeout 90s ./...`; все packages прошли.

## Deliberate guard mutation

В отдельной временной копии
`/private/tmp/pomosh-review-final-mutation.nKfPAF` guard в `runTurn`, который
отклоняет provider-created `action_proposal`, был намеренно изменён с
`if out.Reply.Kind == domain.ReplyActionProposal` на `if false && ...`.

Команда:

```sh
go test -count=1 ./internal/service \
  -run '^TestServiceAdditionalExpiryCancellationAndUnsafeProviderAction$'
```

дала ожидаемый `FAIL` на assertion
`provider-created authority=<nil>` (`service_additional_test.go:203`). Значит,
критический guard не является непроверяемым декоративным кодом: его удаление
ловится safety test. Временная копия удалена; production tree не изменялся.

## Не выполнено и остаточные ограничения

- Live evaluation: 40 trees × 3, 20 images × 3, 20 authentic audio и проверка
  zero critical violations — **BLOCKED / NOT RUN**.
- Реальная доступность закреплённых model IDs, валидность ключа, upstream и
  региональные/договорные условия — **NOT VERIFIED**. Обход через proxy/VPN не
  допускается.
- Docker image build — **NOT RUN**, потому что daemon выключен. Успешный Linux
  cross-build не заменяет сборку image.
- Android-клиент — **NOT STARTED / BLOCKED** до успешных live-gates.
- `store:false` не доказывает Zero Data Retention. `/ready` не является
  upstream probe. In-memory state исчезает при рестарте, а lazy TTL cleanup не
  обещает физическое стирание памяти ровно в момент deadline.
- Фильтр структурированных targets ловит очевидные phone/URI patterns, но не
  любую кодированную/обфусцированную PII; free-form utterance/STT может содержать
  персональные данные.

## Итог

**APPROVED в пределах deterministic offline backend.** Блокирующих дефектов в
проверенном замороженном срезе не осталось; обязательные normal/race/vet и
coverage ≥ 90% gates выполнены, критические guards и concurrency boundaries
проверены. Одобрение не закрывает live-gates и само по себе не разрешает
называть систему production-ready или переходить к Android без последующей
live-проверки.
