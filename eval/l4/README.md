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

## Complete-product offline checks

`cmd/l4-product` adds a full-schema path. Preparation uses the real model summary generator and rolling provider with a deterministic fake model. Answer runs restore into a fresh owned MySQL database, assemble the complete hybrid product with its original workers and authenticated HTTP handlers, ask one question, and shut down. Episode quiet time is 24 hours. Model/vector transports are fake; the command supports only `-env fake`.

```powershell
go run ./cmd/l4-product -env fake -phase prepare -input C:\evaluation\turns.json -output-dir C:\evaluation\prepared
go run ./cmd/l4-product -env fake -phase answer -input C:\evaluation\prepared\snapshot.json -question "Recall this conversation" -output-dir C:\evaluation\attempt
```

The transactional MySQL journal installs INSERT/UPDATE/DELETE triggers on both task tables before startup. UPDATE records both images, so a brief claim/reset/delete cannot disappear between samples. An auto-increment answer barrier orders transitions without comparing database and process wall clocks. Final collection runs after successful worker shutdown through a separate observer pool, verifies unchanged trigger definitions and the barrier, and rejects pre-answer execution. Before/after counts are retained as additional evidence. These triggers add measurement overhead and are confined to disposable evaluation databases.

Model wrappers preserve attribution across tool binding and streaming. Complete first-system-prompt matching classifies calls using the checkout's source prompts; unknown roles are refused and invalidate telemetry. The ledger records call start/end, input/prompt hashes and provider usage snapshots, retaining missing usage as null. Usage tests include cached prompt tokens; fake output is not a measured monetary cost. A separate loopback HTTP stub verifies the production SDK protocol, but it is not yet connected to the command's service entry.

Full-schema initialization calls the original product migration. Immutable document seeding validates the owner, active version, product storage path, size and normalized content hash separately from the session snapshot. The offline command currently seeds no corpus and uses empty fake retrieval. An integration test warms 40 turns and restores the same snapshot into three fresh databases; all three actual model contexts contain both the product summary and recent messages while background jobs remain pending.

## Remaining integration

The offline path currently calls the public full-product assembly with explicit fake transport dependencies. It still needs a final check through the unchanged standard `hybrid.Run` entry and a controlled model proxy, plus binding of the immutable corpus metadata to its shared real index. Paid execution, budget authorization, provider price accounting and real outcome quality are not implemented or authorized by this command. It must not certify a real run or issue a paid request. `BackgroundAudit.Complete` cannot be set from polling alone; the offline collector derives it from successful journal verification, model coverage and shutdown.

For full-product runs, the planned protocol uses a fresh database per attempt and a 24-hour episode quiet window. Reports must state: full product service; no background execution before the measured answer; subsequent background calls accounted separately. Before/after job counts alone cannot prove no transient execution. Unattributed or incomplete observations invalidate an attempt.
