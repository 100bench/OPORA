# Статус Опоры

Дата обновления документации: **2026-09-29**  
Дата последнего набора исполняемых доказательств: **2026-09-28**  
Текущая фаза: **`BACKEND_OFFLINE_VERIFIED`**

Статусы здесь означают:

- `IMPLEMENTED` — существует реализация и приведено проверяемое доказательство;
- `APPROVED` — контракт/план независимо одобрен, но runtime этим не доказан;
- `PASS` — указанная команда реально выполнена с exit 0 в приведённом срезе;
- `PLANNED` — решение принято и описано, но реализация не доказана;
- `PENDING` — ожидается работа или независимое решение;
- `BLOCKED` — известное внешнее условие не выполнено;
- `NOT RUN` — проверка не запускалась, независимо от наличия тестового файла.

## Срез реализации

| Область | Статус | Доказательство / следующий шаг |
|---|---|---|
| Архитектурные границы MVP | `APPROVED` | [ARCHITECTURE.md](ARCHITECTURE.md), независимое ревью |
| Спецификация поведения | `APPROVED` | [SPEC.md](SPEC.md) проверена независимым ревьюером |
| OpenAPI | `APPROVED` | [api.yaml](../api.yaml) проверен: 8 paths / 20 schemas |
| Тестовый план и fixture amendments | `APPROVED` | [TESTING.md](TESTING.md), [отчёт по фикстурам](reviews/fixture-amendment-review.md); полный offline suite зелёный |
| Offline backend core | `IMPLEMENTED` | Service/policy/validation/transport/runtime покрыты зелёными gates и smoke |
| OpenAI adapters | `IMPLEMENTED` | Wire/security tests зелёные; реальные provider calls `NOT RUN` |
| Build/developer scaffold | `IMPLEMENTED` | Makefile, env, CI и non-root Dockerfile; Docker image build `NOT RUN` |
| Итоговое независимое ревью | `APPROVED` | [Отчёт](reviews/implementation-review.md): deterministic offline backend |
| Live evaluation | `BLOCKED` | Нет ключа, consented corpus и cost approval |
| Android-клиент | `BLOCKED` | Не начат; только после live-gates |

Наличие документа, схемы или stub не означает, что функция `IMPLEMENTED`.

## Изменение документации 2026-09-29

`AGENTS.md` переписан на английском как отдельный регламент только для
AI-агентов. Пользовательская и разработческая документация остаётся
самодостаточной на русском: переведены `SPEC.md`, `TESTING.md` и
`testdata/README.md`, уточнены `README.md`, `DEVELOPMENT.md` и
`ARCHITECTURE.md`. Runtime, API, тесты, конфигурация и поведение этим изменением
не менялись. Backend Go/test/race/vet/lint/build и live-проверки в этой
документационной итерации не запускались и не заявляются; выполнены только
docs link/shell/hash checks, перечисленные в отдельном отчёте.

Перевод изменил байтовое содержимое и текущие хеши документов. Отчёты от
2026-09-28 и зафиксированные в них хеши относятся к историческому
англоязычному baseline до перевода и сохраняются как доказательство
соответствующего ревью; они не переписывались и не выдаются за хеши текущих
переведённых файлов. Docs-only изменение получило
[независимое одобрение](reviews/documentation-review.md) 2026-09-29.

## Проверки

| Gate | Статус | Последнее доказательство |
|---|---|---|
| Независимое ревью docs-only изменения | `APPROVED` | [Отчёт](reviews/documentation-review.md), 2026-09-29; backend executable checks в этой итерации `NOT RUN` |
| Независимое ревью спецификации/тестов | `APPROVED` | [Отчёт ревью](reviews/spec-tests-review.md), 2026-09-28 |
| Финальная offline-проверка после критических regression-тестов | `PASS` | Полные normal/race/vet и combined coverage перезапущены, 2026-09-28 |
| `go test ./...` | `PASS` | Полный набор после добавления трёх прямых regression-тестов, 2026-09-28 |
| `go test -race ./...` | `PASS` | Полный race-набор после добавления трёх прямых regression-тестов, 2026-09-28 |
| `go vet ./...` | `PASS` | Финальный авторский rerun, замечаний нет |
| Запрошенные тесты границ конкурентности | `PASS` | Normal и race, 20 повторов каждого, 2026-09-28 |
| Core combined coverage ≥ 90% | `PASS` | 91.5%: policy 97.8%, service 90.3%, validation 89.4% |
| `golangci-lint run ./...` | `PASS` | exit 0, `0 issues`, 2026-09-28 |
| `make build` | `PASS` | Локальный backend binary собран |
| Linux amd64 cross-build | `PASS` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath ... ./cmd/service` |
| Local process smoke | `PASS` | health 200; ready 503 без ключа; unauth 401; authenticated session→OPEN_APP→confirm→result; SIGINT exit 0; AI calls 0 |
| Docker image build | `NOT RUN` | Docker daemon выключен; cross-build не заменяет image build |
| 40 trees × 3, success ≥ 90% | `BLOCKED` | Нет утверждённого live-корпуса и ключа |
| 20 images × 3, success ≥ 90% | `BLOCKED` | Нет утверждённого live-корпуса и ключа |
| 20 audio, correct intent + parameters ≥ 18/20 | `BLOCKED` | Нет утверждённого live-корпуса и ключа |
| Critical safety violations = 0 | `BLOCKED` | Live-корпус и ключ отсутствуют |
| Независимое итоговое ревью | `APPROVED` | [Отчёт](reviews/implementation-review.md), 2026-09-28 |

## Известные блокеры и риски

- API-ключ OpenAI намеренно не настроен; live вызовы не подтверждены.
- Live-корпус 40 trees, 20 images и 20 audio ещё не утверждён и не проверен на
  отсутствие реальных персональных данных.
- Доступность закреплённых model IDs и региональные/договорные условия должны
  проверяться на целевом аккаунте; обход через proxy/VPN не допускается.
- `store: false` не доказывает Zero Data Retention.
- `/ready` проверяет только локальную конфигурацию и не подтверждает реальный
  доступ к ключу, модели или upstream API.
- Текущий loopback gateway не является production multi-user deployment;
  внешнему мобильному клиенту потребуется HTTPS boundary.
- Android нельзя начинать только на основании offline gates: сначала нужны
  успешные live-gates.
- ScreenSaathi revision `2575808` содержит известные проблемы нормализации
  кириллицы и vision `NoOp`; это reference риска, а не usable backend.

## Следующие шаги

- Не менять одобренные API, safety rules, состояния, лимиты или модели без
  синхронного изменения контрактов и нового независимого решения.
- После credentials, consented corpus и cost approval выполнить live evaluation:
  40 trees × 3, 20 images × 3, 20 authentic audio и zero critical violations.
- Только после успешных live-gates планировать минимальный Android
  client. Голосовой клиент/frontend пока не реализованы.
