# Независимое ревью исправлений тестовых фикстур

Дата: **2026-09-28**  
Ревьюер: независимый агент `spec_review` (`GPT-5.6 Sol`, reasoning `xhigh`)  
Фаза: `IMPLEMENTATION`  
Applied-diff ревьюер: независимый агент `review_implementation`
(`GPT-5.6 Sol`, reasoning `xhigh`)  
Статус: **APPROVED — APPLIED DIFF VERIFIED**

## Область

Ревью ограничено пятью обнаруженными противоречиями в ранее одобренных
фикстурах и одной последующей timing-only коррекцией теста:

1. WAV positive fixture без корректных `ByteRate`/`BlockAlign`;
2. successful `ReplyExplain` fixtures с пустым обязательным `Text`;
3. near-256-KiB tree fixture с `node.text` больше допустимых 2048 символов;
4. `TestFR31` request IDs, построенные из имён subtests с пробелами.
5. provider-cap fake, который заявлен как noncooperative, но выходит по
   `ctx.Done()` и поэтому недетерминированно освобождает permit до проверяемого
   assertion.
6. Финальный `active == 0` в provider-cap test выполнялся после возврата caller
   goroutines, но до гарантированного фактического выхода всех noncooperative
   provider calls.

Допустимы только исправления тестовых данных и явные negative assertions,
которые сохраняют или усиливают проверку замороженного контракта. Изменение
production-кода, лимитов, API, safety rules или ослабление существующих
assertions этим amendment не одобряется.

## Независимая оценка разрешённых исправлений

Все пять исходных фикстур действительно могут давать ложное падение до
проверяемого поведения. Следующие изменения тестов **допустимы и не ослабляют
замороженный контракт**:

1. Заполнить в positive WAV helper согласованные `ByteRate` и `BlockAlign`;
   оставить или добавить negative cases для нулевых/несогласованных значений.
2. Добавить непустой `Text` в successful `ReplyExplain` fixtures; отдельно
   утверждать, что пустой explain text отклоняется.
3. Построить near-limit tree из нескольких валидных nodes, у каждого
   `text <= 2048`, сохранив раздельные negative assertions для per-node limit и
   aggregate `256 KiB` limit. Например, при текущей compact JSON-форме 121 node
   по 2048 ASCII-символов остаются ниже 256 KiB, а 122 превышают лимит.
4. Отделить человекочитаемое имя subtest от machine `request_id` и использовать
   ID, соответствующие `^[A-Za-z0-9._:-]+$`; invalid ID с пробелом оставить
   явным negative case.
5. Сделать только fake в
   `TestNFR13_ProviderCapQueuesFifthAndReleasesOnExit` намеренно
   noncooperative: он игнорирует context cancellation и выходит только через
   test gate. Assertions `active == 4`, fifth queued, один выход разрешает вход
   пятого и итоговый `active == 0` должны сохраниться. Это не разрешает менять
   production cancellation или удерживать cooperative HTTP call после его
   фактического выхода.
6. Добавить отдельный provider-call `WaitGroup` и проверять итоговый
   `active == 0` только после фактического выхода всех пяти provider calls.
   Caller `WaitGroup` и все исходные assertions о cap/queue/release сохраняются.

## Проверка применённого diff

Независимый post-implementation ревьюер проверил итоговые test files и
подтвердил:

- positive WAV helpers содержат согласованные `ByteRate`/`BlockAlign`, при этом
  нулевые значения остаются negative cases;
- successful `ReplyExplain` содержит непустой `Text`, а empty text проверяется
  отдельно как invalid;
- валидная near-limit tree использует 121 node по 2048 ASCII-символов; 122-node
  aggregate overflow и per-node overflow остаются раздельными negative cases;
- machine request IDs отделены от имён subtests, а invalid ID с пробелом не
  удалён;
- fake в `TestNFR13_ProviderCapQueuesFifthAndReleasesOnExit` игнорирует context,
  пятый вызов остаётся queued до реального выхода одного из четырёх, и финальная
  проверка ждёт provider `WaitGroup` перед assertion `active == 0`.

Исходные safety/boundary assertions сохранены; `t.Skip`, `testing.Short` и
исключающие build tags не добавлены. Контрактные файлы не изменились относительно
одобренного baseline:

| Артефакт | SHA-1 |
|---|---|
| `api.yaml` | `1eb2c7c4d16072be4bc6bc58d77df6b711672606` |
| `docs/SPEC.md` | `0751ed256e9c9c8228e42af8f1d30928efb2e69d` |
| `docs/TESTING.md` | `5dac7c40bb0fbad95b16ea19e775dfb7ec775127` |

Текущие SHA-1 непосредственно затронутых test files:

| Артефакт | SHA-1 |
|---|---|
| `internal/policy/policy_test.go` | `c078fe8605952921bc090ec286da9713c17b4531` |
| `internal/service/service_test.go` | `ce327fdd4c3728b46bbb81139a1a6767f0acfcb9` |
| `internal/validation/validation_test.go` | `bb151cdb0a816ca5c870d8dc39bc49bcaa0c4bca` |

Репозиторий не содержит Git metadata, поэтому полный исторический byte-diff
из baseline восстановить нельзя. Для неизменённых контрактов проверены hashes;
для изменённых tests выполнена ручная структурная проверка всех шести repairs,
сохранённых assertions и negative cases. Ограничение доказательства не скрыто и
не заменено утверждением о несуществующем Git diff.

## Проверки

После применения repairs независимый ревьюер выполнил полный offline gate:

```sh
GOTOOLCHAIN=local \
GOCACHE=/private/tmp/pomosh-go-build-review-final \
GOPROXY=off GOSUMDB=off \
COVERPROFILE=/private/tmp/pomosh-core-review-final.cover \
make check
```

Результат: exit 0; normal tests, race tests, vet и совокупный coverage gate
прошли, coverage составил 91.5%. Полный `golangci-lint run ./...` завершился
с `0 issues`.

## Итог

**APPROVED / APPLIED DIFF VERIFIED.** Все пять исходно одобренных fixture
repairs и шестая timing-only коррекция применены узко, не меняют замороженный
контракт и не ослабляют тестовые утверждения.
