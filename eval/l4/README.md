# Short-term conversation evaluation (offline infrastructure)

This directory covers rolling summaries and recent-message context. It does not evaluate episodic or long-term memory quality. Stub outputs only verify infrastructure; they are not model-quality or cost evidence.

The commands require local Docker and an already downloaded `mysql:8.4` image. The full-product path also requires `redis/redis-stack-server:latest`. Containers use fresh identities and loopback ports; no existing database DSN is accepted. Cleanup checks container ownership before removing its anonymous volumes. Model calls use deterministic stubs with no upstream forwarding.

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

`cmd/l4-product` adds the full-schema path. Preparation uses the real model summary generator and rolling provider with a deterministic fake model. Answer runs restore into a fresh owned MySQL database and call the unchanged `hybrid.Run` used by `cmd/gopherlearn-integrated`. The original production factories, native login, HTTP handlers and workers run against disposable MySQL/Redis and loopback fake chat/embedding/rerank endpoints. Thinking is explicitly requested using the DeepSeek dialect; hosted reranking remains enabled, including its original startup probe. No assembly dependencies or routes are replaced. Episode quiet time is 24 hours; the command supports only `-env fake`.

```powershell
go run ./cmd/l4-product -env fake -phase prepare -input C:\evaluation\turns.json -output-dir C:\evaluation\prepared -stop-file C:\evaluation\STOP
go run ./cmd/l4-product -env fake -phase answer -input C:\evaluation\prepared\snapshot.json -question "Recall this conversation" -qid synthetic-case -arm current -rep 0 -output-dir C:\evaluation\attempt -stop-file C:\evaluation\STOP
```

The transactional MySQL journal installs INSERT/UPDATE/DELETE triggers on both task tables before startup. UPDATE records both images, so a brief claim/reset/delete cannot disappear between samples. An auto-increment answer barrier orders transitions without comparing database and process wall clocks. Final collection runs after successful standard-entry shutdown through a separate observer pool, verifies unchanged trigger definitions and the barrier, and rejects pre-answer execution. Before/after/stopped counts and task timestamps are retained as additional evidence. Task wall times are interpreted in the standard entry's local zone and exported as UTC; trigger times are already UTC. The barrier is recorded after the response arrives, so work in that short boundary gap is conservatively rejected too. Triggers add measurement overhead and are confined to disposable evaluation databases.

The standard-entry ledger observes actual HTTP requests, including individual SDK retry attempts. Complete first-system-prompt matching classifies calls using the checkout's source prompts; unknown roles are refused and invalidate telemetry. Requests retain model identity, status, start/end and complete payload hashes including tool declarations. JSON and fragmented SSE usage snapshots preserve input/output/cache counts; absent fields remain unmetered. Streaming cumulative snapshots use the last reported totals rather than their sum. Calls after answer collection are attributed to background work while preserving their original purpose. Fake calls establish no real token cost; monetary pricing remains separate. Production chat and embedding SDK round trips are tested against the loopback stub. Preheat wrappers also preserve attribution across tool binding and streaming.

Full-schema initialization calls the original product migration. Immutable document seeding validates the owner, active version, product storage path, size and normalized content hash separately from the session snapshot. Offline fixtures have an empty knowledge index; real corpus/index preparation is a separate operation. Integration tests cover a 40-turn history and a 42-turn history whose final question triggers another summary. Three repetitions restore the same snapshot into fresh databases; actual model contexts contain both the product summary and recent messages while background jobs remain pending. The latter test records one startup rerank probe, one final-question summary call and one answer call separately in each repetition.

Preparation retains each turn's message IDs, summary watermark and degradation flag in `warmup.json`. Answer output includes `result.json` and compact `record.jsonl` with `qid`, `arm`, `rep`, `condition`, `env`, `answer`, `error` when applicable, and `backgroundAudit`. Scoring tools can consume the JSONL record directly. Manifests bind input/output hashes; run provenance records checkout revision/content, dirty state, functional settings and snapshot digests. Restore latency includes isolation checks and SQL restore, but excludes container startup and schema initialization. Stop files are checked before work and watched during it; interrupts/deadlines cancel requests and trigger owned-resource cleanup.

## Diagnosing a failed summary

`cmd/l4-summary` replays one conversation through the product's own rolling summarizer and stops at the first compaction failure. The product's log prints the type of an error but not its text, so a failed compaction cannot be explained from a run's logs; this command keeps the whole error chain, the step the failure came from, and the body the model returned.

```powershell
go run ./cmd/l4-summary -repository C:\checkout -input C:\evaluation\history.json -scenario my-case `
  -output-dir C:\evaluation\diagnosis -proxy http://127.0.0.1:18150 -chat-model deepseek-flash
```

The model endpoint is the operator's counting proxy, so the credentials stay with the operator and every call is metered. The command never rewrites a request and never replaces a product dependency: the summary policy is the product's own configuration, and the knowledge index is not part of a summary.

A saved diagnosis can be read again without a model call:

```powershell
go run ./cmd/l4-summary -reclassify C:\evaluation\diagnosis\diagnosis.json -output-dir C:\evaluation\diagnosis-again
```

## Scoring output directories

A run directory may hold more than one scored directory when the scoring rules change between two rounds of the same run. In that case the **later directory is the authoritative one** and the earlier one is kept only as a comparison. For the conversation run that changed the fact scoring, `score-fact2/` is the authoritative directory and `score-fact/` is the superseded first round, kept for comparison only. A report must name the scored directory it used.

## Scope and real execution

This implements the offline execution chain. Paid execution remains disabled. A real evaluation separately needs a frozen reviewed dataset, immutable corpus metadata bound to its shared index, a budget-enforcing upstream proxy, approved price accounting and calibrated semantic judgments. The fake command cannot certify model quality or real cost. `BackgroundAudit.Complete` is derived from journal verification, model coverage and successful shutdown, never from polling alone.

For full-product runs, the planned protocol uses a fresh database per attempt and a 24-hour episode quiet window. Reports must state: full product service; no background execution before the measured answer; subsequent background calls accounted separately. Before/after job counts alone cannot prove no transient execution. Unattributed or incomplete observations invalidate an attempt.

For real scenario runs, provide `-run-id`, `-arm`, `-qid` and `-condition`.
The command derives the same evaluation user for preparation and all repeats,
and a different user for each run, configuration, scenario and condition.
Scenario users have an intentionally empty knowledge base: this evaluation
measures conversation history. Do not provide a knowledge seed or explicit user
identity for these phases. Before answering, the evaluator refuses users with entries in either derived
memory index, preventing repeats from inheriting a previous attempt’s state.
The hash-derived ID has a small collision probability, so operators must check
the generated identity manifest for duplicates before dispatch.
