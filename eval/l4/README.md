# Short-term conversation evaluation (offline infrastructure)

This directory covers rolling summaries and recent-message context. It does not evaluate episodic or long-term memory quality. Stub outputs only verify infrastructure; they are not model-quality or cost evidence.

The offline command requires local Docker and an already downloaded `mysql:8.4` image. It creates its own container, random credentials, loopback port and instance marker; it never accepts an existing database DSN. Cleanup checks the container identity before removing its anonymous volume. Model calls are replaced by a deterministic summary stub.

```powershell
# Run from eval. Both input and output must be outside every Git worktree.
go run ./cmd/l4 -env fake -turns C:\evaluation\turns.json -snapshot C:\evaluation\snapshot.json
```

Input is an array of complete turns with `user`, `assistant`, `user_at` and `assistant_at` (RFC3339). Times must increase strictly. Each user message is persisted first, the product RollingSummaryProvider decides whether to compact, and the supplied assistant response is persisted last. Summary failures remain visible as degraded turns. No agent runs, steps, tools or background events are invented during preparation.

Snapshot archives contain one session's messages and summary plus identity metadata. They preserve message IDs, summary watermarks and evidence IDs, validate ownership and provenance, include a checksum, and refuse overwrites or paths inside repositories. Restore requires empty conversation tables and exactly one matching evaluation user. Knowledge metadata must be seeded separately and match any shared search index's owner, document IDs and active versions.

`Sandbox.InitSynthetic` creates only the minimal demonstration schema. It is not the full production migration. The HTTP provider's injected-session hook currently permits fake backends only; real runs cannot use it.

## Verification

```powershell
go test ./cmd/l4 ./internal/conversation
# Optional isolated MySQL integration test; it creates and removes its own containers.
$env:GL_SANDBOX_TEST = '1'
go test -count=1 ./internal/conversation
```

The tests check product summary trigger ordering, fixed timestamps, references, ownership, nonempty targets, transaction rollback, archive tampering, random instance markers and background-status validation. A local fake-provider test also preserves the existing provider's budget and stop-file behavior.

## Remaining integration

Full-product HTTP startup, production-schema initialization, final-question snapshot restore, corpus metadata seeding, continuous job execution observation and role-attributed proxy ledgers are not connected yet. `BackgroundAudit.Complete` is a validation input, not a monitoring implementation; callers must not set it from polling alone. Current functionality must not be used to certify a real run or issue a paid request.

For full-product runs, the planned protocol uses a fresh database per attempt and a 24-hour episode quiet window. Reports must state: full product service; no background execution before the measured answer; subsequent background calls accounted separately. Before/after job counts alone cannot prove no transient execution. Unattributed or incomplete observations invalidate an attempt.
