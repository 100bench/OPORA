# Независимое ревью спецификации и тестов

Дата: **2026-09-28**  
Ревьюер: независимый агент `spec_review` (`GPT-5.6 Sol`, reasoning `xhigh`)  
Фаза: `SPEC_AND_TESTS`  
Решение: **APPROVED — разрешена реализация backend core в рамках утверждённых контрактов**

## Граница решения

Одобрение относится к спецификации, OpenAPI-контракту, тестовому плану,
компилируемым контрактам и RED-тестам backend. Оно не является доказательством
работоспособности backend, не одобряет Android-клиент и не заменяет обязательное
независимое ревью после реализации.

Автор проверяемого изменения и ревьюер — разные агенты. Ревьюер не изменял
спецификацию, API, production-код или тесты; единственный принадлежащий ревьюеру
артефакт — этот отчёт.

## Проверенные артефакты

- `AGENTS.md`, `go.mod`, `go.sum`;
- `docs/SPEC.md`, `docs/TESTING.md`, `docs/ARCHITECTURE.md`,
  `docs/DEVELOPMENT.md`, `docs/STATUS.md`;
- `api.yaml`;
- domain/service/policy/validation contracts и явные `ErrNotImplemented` stubs;
- HTTP contract tests, policy tests, validation tests, service/state/concurrency
  tests и OpenAI provider wire tests — всего 37 верхнеуровневых test groups.

## Что подтверждено ревью

1. Product/safety boundary допускает только `OPEN_APP` по непрозрачному
   `app_ref` и `PREPARE_DIALER` по локальному `contact_ref`. Package текущего
   экрана — только наблюдаемый контекст. Произвольные taps/coordinates,
   сообщения, платежи, пароли, OTP и фактический звонок запрещены.
2. Только явная текущая команда пользователя может создать action proposal.
   Отрицательные, гипотетические, неоднозначные команды и текст экрана не дают
   полномочий. Предусмотрена регрессия кириллицы и `Ё/ё`.
3. Accessibility tree является основным каналом. Screenshot опционален, требует
   отдельного consent для той же версии экрана и не может разрешить действие.
   Screenshot при protected/sensitive screen или любом sensitive node
   отклоняется до provider call; tree-only путь фильтрует sensitive nodes.
4. Контракты фиксируют лимиты дерева, PNG/JPEG и WAV, включая фактический
   decode/parse, граничные размеры, malformed/trailing payload и strict
   multipart/JSON transport.
5. Сессии ограничены 100 записями и idle TTL 30 минут; request ledgers
   ограничены. Idempotency различает pending/success/error replay и payload
   conflict, не создавая повторный логический результат.
6. Generation, cancel и stale-result проверки выполняются после возврата
   provider, включая provider, игнорирующий cancel. Глобальный provider cap 4
   совместно покрывает planning и transcription; permit удерживается до
   фактического выхода provider call. Mutex не должен удерживаться через I/O.
7. Confirmation использует реальный выданный `action_id` и атомарную привязку
   device/session/generation/screen/action hash. Проверены fresh proposals,
   `29.999s`/`30s`, confirm/decline, exact replay, tamper, consumption и
   cross-device/session isolation. Result принимается только для issued grant;
   kind/status mismatch и `call_connected` запрещены.
8. Provider DTO не содержит серверные `action_id`, `generation` и
   `screen_version`: их штампует service после проверки. Wire tests структурно
   проверяют `text.format` strict JSON Schema, рекурсивный
   `additionalProperties:false`, `store:false`, model/path/method/headers,
   bounded raw output parsing, ambiguous/refusal/tool output, in-flight cancel и
   отсутствие секретов в ошибках. Это согласуется с официальными материалами
   OpenAI по [Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs),
   [Responses API](https://developers.openai.com/api/reference/cli/resources/responses/methods/create)
   и [data controls](https://developers.openai.com/api/docs/guides/your-data).

## Самостоятельно выполненные проверки

Все команды выполнены ревьюером 2026-09-28 в
`/Users/maksim/Desktop/помош` с локальным Go `1.26.3`.

| Команда | Наблюдаемый результат |
|---|---|
| `gofmt -l cmd internal` | exit 0, вывод пустой |
| `ruby -e 'require "yaml"; ... YAML.safe_load(File.read("api.yaml"), aliases: true) ...'` | exit 0; OpenAPI YAML разобран, 8 paths / 20 schemas |
| `GOTOOLCHAIN=local GOCACHE=/private/tmp/pomosh-go-build GOPROXY=off GOSUMDB=off go test -run '^$' ./...` | exit 0; компилируются все packages и tests |
| `GOTOOLCHAIN=local GOCACHE=/private/tmp/pomosh-go-build GOPROXY=off GOSUMDB=off go test -race -run '^$' ./...` | exit 0; race-instrumented compile всех packages/tests |
| `GOTOOLCHAIN=local GOCACHE=/private/tmp/pomosh-go-build GOPROXY=off GOSUMDB=off go vet ./...` | exit 0, замечаний нет |
| `GOTOOLCHAIN=local GOCACHE=/private/tmp/pomosh-go-build GOPROXY=off GOSUMDB=off go test ./...` | ожидаемый exit 1: `internal/app` PASS; positive provider/policy/service/validation tests RED на явном `not implemented` |

Последняя команда — не успешный implementation gate. Это проверяемый RED
baseline: business logic и provider adapters по-прежнему являются заглушками и
не выданы за готовый продукт.

## Обязательные условия после этого одобрения

- Реализация должна удовлетворить текущим тестам; удалять, пропускать или
  ослаблять их для получения зелёного результата нельзя.
- Изменение API, safety rule, состояния, лимита или модели требует синхронного
  изменения SPEC/OpenAPI/TESTING/STATUS и нового независимого решения.
- До merge реализации обязательны полноценные `go test ./...`,
  `go test -race ./...`, `go vet ./...` и не менее 90% statement coverage для
  совокупности `internal/policy`, `internal/service`, `internal/validation`.
- После реализации требуется второй независимый review кода и результатов.
- Live gates остаются **BLOCKED / NOT RUN**: нет утверждённого consented corpus,
  credentials и cost approval. Требуются 40 trees × 3, 20 images × 3,
  20 authentic audio с минимум 18/20 корректных intent+parameters и ноль
  critical safety violations. `store:false` не является обещанием Zero Data
  Retention. Android остаётся заблокирован до успешных backend и live gates.

## Итог

**APPROVED.** Спецификация и тестовый план достаточно согласованы и
содержательны, чтобы начинать реализацию backend core без изменения границ
продукта. Это одобрение не означает, что backend уже реализован или готов к
эксплуатации.
