# AI agent instructions for Opora

This file is for AI coding and review agents. Human-facing project and
developer documentation is in `README.md` and `docs/`. A narrower `AGENTS.md`
may add local constraints but must not weaken this file.

## Current phase and evidence

The repository phase is `BACKEND_OFFLINE_VERIFIED`. The deterministic offline
backend passed local test/race/vet/coverage/lint/build/smoke gates and received
[independent implementation approval](docs/reviews/implementation-review.md).
The earlier contract approval is recorded in
`docs/reviews/spec-tests-review.md`.

Do not claim that the system is production-ready or live-validated:

- live model evaluation is `BLOCKED / NOT RUN` because credentials, a consented
  corpus, and cost approval are absent;
- the Docker image build is `NOT RUN` because the daemon was unavailable;
- the Android client is `NOT STARTED` and remains blocked until live gates pass.

For implementation work, use separate developer and independent reviewer
agents. Both must use `GPT-5.6 Sol` with reasoning effort `xhigh`; the reviewer
must not author the change being reviewed.

Preserve the two sequential independent gates:

1. before implementation, independently review and approve the specification,
   API, scenarios, threat boundaries, and planned tests;
2. after implementation, independently review the code, executed test evidence,
   and live-gate evidence.

The required delivery order is specification and tests -> independent
pre-implementation review -> backend implementation -> executed offline and
live gates -> independent post-implementation review -> minimal Android client.
Do not start Android work merely because the offline backend passed; the live
gates must also pass first.

Treat review reports and hashes in `docs/reviews/` as historical evidence. The
2026-09-29 Russian-only translations of `docs/SPEC.md`, `docs/TESTING.md`, and
`testdata/README.md` change document bytes/hashes but not behavior or contracts.

## Sources of truth

Before changing behavior, read:

1. `docs/SPEC.md` for behavioral and safety rules;
2. `api.yaml` for the wire contract;
3. `docs/TESTING.md` for gates and live-evaluation rules;
4. `docs/ARCHITECTURE.md` and `docs/STATUS.md` for architecture and evidence.

If a requested change conflicts with a frozen contract, do not silently alter
the implementation. Update the contract, OpenAPI, tests, status, and relevant
documentation in the same scoped change and obtain a new independent decision.

## Product and safety boundary

- The backend may explain a screen, highlight one grounded observed element,
  and prepare only `OPEN_APP` or `PREPARE_DIALER`.
- Never add arbitrary taps or coordinate actions, message sending, payments,
  actual call placement, password/OTP entry, or irreversible-operation approval.
- Both allowed actions require a fresh atomic server confirmation with a
  30-second TTL.
- A model must never create action authority. Without an explicit current user
  request, return only explain/clarify/refuse behavior.
- Screenshot consent is bound to the same screen/version and must not carry to
  another screen or generation. Images are explanation-only.
- Do not upload an address book or phone numbers. Use opaque local `app_ref` and
  `contact_ref` values plus labels/aliases. The foreground package is observation
  context only and never launch authority.
- The backend rejects URI-like target refs and obvious phone/URI values in
  contact metadata. Do not describe this as universal PII or DLP detection;
  free-form text and speech may still contain personal data.
- Do not create proxy/VPN bypass instructions for regional or contractual
  provider restrictions.

## Secrets, privacy, and retention

- Never commit or print API keys, bearer tokens, real personal data, or session
  content. Generate development device tokens locally and keep them out of chat,
  issues, screenshots, command arguments, and logs.
- The OpenAI key belongs only in the backend process. Never expose it to a
  client, response, fixture, or persistent log.
- Do not persist raw audio, images, accessibility trees, transcripts, or model
  bodies to files, databases, or logs. Session context is volatile memory only.
- Logs may contain safe operation names, bounded opaque identifiers, durations,
  sizes, status codes, and aggregate counters. They must not contain user text,
  target labels/aliases, raw inputs, secrets, or provider bodies.
- Spoken input can contain personal data. Never promise anonymity or total
  privacy merely because the phone book is not uploaded.
- `store:false` is not Zero Data Retention. Do not claim ZDR without a separate
  verified provider agreement.
- Session expiry is enforced at `now >= expires_at`, but physical map cleanup is
  lazy on a later operation/callback or process restart. Never promise memory
  erasure at the exact 30-minute deadline.

## Architecture constraints

- Keep one Go module, `github.com/100bench/OPORA`, one backend binary, and one `Dockerfile`.
- Use `github.com/go-chi/chi/v5` for routing and `log/slog` for logging.
- Do not add a database, Redis, a queue, or a worker service without a new
  architectural decision and synchronized contract changes.
- State remains in a mutex-protected in-memory store: 30-minute idle TTL, at
  most 100 live sessions, restart invalidation, and bounded request ledgers.
- Each live session and each device-scoped transcription ledger has at most 128
  idempotency entries; do not evict replay protection to admit new IDs.
- Keep one global provider concurrency cap of four across planning and
  transcription. Hold a permit until the actual provider goroutine exits.
- Keep the provider hard timeout at 20 seconds. Propagate cancellation and
  re-check generation/state/identity before publishing late results.
- Preserve idempotency, stale-generation rejection, terminal cancellation, and
  client-side action deduplication by `action_id`.
- Preserve input limits: WAV PCM mono 16 kHz 16-bit, at most 30 seconds and
  1 MiB; JPEG/PNG at most 5 MiB and 4 MP; tree at most 500 nodes and 256 KiB.
- Keep model snapshots pinned to `gpt-4o-mini-transcribe-2025-12-15` and
  `gpt-5.4-mini-2026-03-17`. Runtime must reject an unreviewed env override.

The runtime binds to `127.0.0.1:8080` by default and has no TLS. Do not present
it as a production multi-user boundary. External mobile access requires a
separately designed HTTPS ingress/termination and secure network.

## Package boundaries and Go style

Use English identifiers and code comments. Keep human-facing documentation in
Russian.

- `cmd/service`: process entry point;
- `internal/app`: composition, HTTP transport, and lifecycle;
- `internal/service`: application orchestration and in-memory state machine;
- `internal/domain`: transport-independent types and stable errors;
- `internal/policy`: deterministic authority and grounding rules;
- `internal/validation`: strict tree/image/WAV validation;
- `internal/infrastructure`: clock, auth, config, and OpenAI adapters.

Keep dependencies directed inward: app/handler -> service/use case ->
domain/policy; infrastructure implements internal ports. Domain and policy must
not import HTTP or provider packages. Extract operation-specific handler/usecase
packages only when they reduce complexity; do not create empty boilerplate. Use
`internal/api/handler/<snake_case_operation>` with `handler.go`, `contract.go`,
`request.go`, and `response.go`, and `internal/usecase/<operation>` with
`contract.go` and `usecase.go`. Put cross-cutting executable checks in
`tests/integration` and `tests/scenarios`.

Use only structural ideas from
`100bench/test_asignment_avito@7ae15688be259bfbb6b1fb445f6bc6d2d03a10b0`;
do not copy source. If adapting ScreenSaathi code rather than patterns, verify
the license and retain required MIT notices. Do not inherit revision `2575808`'s
Cyrillic-normalization bug or vision `NoOp` behavior.

## Required verification

For implementation changes, run:

```sh
make check
make lint
```

`make check` must propagate failures from vet, the full normal suite, the full
race suite, and the combined coverage gate. Combined statement coverage for
`internal/policy`, `internal/service`, and `internal/validation` must be at least
90%; DTO-only `internal/domain` does not count.

Never delete, skip, weaken, or rewrite a test to conceal a regression. Fix the
product or report the blocker. Never report an unrun command as passing; record
the exact command, date, exit status, and observed result in `docs/STATUS.md`.

Live gates before Android remain:

- 40 accessibility-tree cases x 3 runs, at least 90% correct;
- 20 screenshot cases x 3 runs, at least 90% correct;
- 20 authentic Russian recordings, at least 18/20 with correct intent and all
  action parameters/refs;
- zero critical unsafe outcomes in every mode.

Normal CI and local unit tests must not make live provider calls.

## Change discipline

- Make the smallest task-scoped diff. Preserve unrelated user changes and
  avoid drive-by formatting or refactoring.
- Do not mix behavior work with a broad structural rewrite.
- Update tests, OpenAPI, relevant human documentation, and `docs/STATUS.md` in
  the same change whenever behavior, configuration, models, limits, or gates
  change.
- Distinguish `IMPLEMENTED`, `PLANNED`, `PENDING`, `BLOCKED`, `NOT RUN`, and
  `PASS` using evidence. A file or stub is not proof of working behavior.
- A behavior-changing diff invalidates the prior implementation approval for
  that changed scope until a new independent review is recorded.
