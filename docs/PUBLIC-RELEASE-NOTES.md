# Public release notes — GopherLearn without the remote executor

This document is the record of what the public release keeps, what it drops,
what changes, what was verified, and what was not. It is written for the
person who will publish the repository, so every claim here is meant to be
checkable.

- Base commit: `8254c601c5dbf78dddbcf978c2a861c2cf43dba5`
- Extraction source: a working tree at `8254c60` with three uncommitted edits
  (`v2/go.mod`, `hybrid/go.mod`, `Dockerfile.backend-integrated`); those edits
  are carried into this release.
- Scope decided by the owner: **publish the GopherLearn that does not depend
  on the RPC framework.**

## 1. What this release is

GopherLearn with **local (in-process) document processing only**. A document
is parsed, chunked, embedded and indexed by the backend's own worker pool, and
a grounded answer retrieves from the resulting vector index. There is no
document-executor node, no coordinator, and no cross-service document
protocol.

Everything else is the product as it was: the learning Agent with its tools
and layered memory, the quiz surface that grades exercises and updates topic
mastery, the user-level knowledge base with versioned documents and owner-only
access, rolling session summaries, episodic memory, and the Vue client.

## 2. Removed

### 2.1 The remote-executor path (the reason for this release)

| Removed | What it was |
|---|---|
| `v2/adapter/documentexec/rpcadapter/` | the RPC client and receiver for the document-executor protocol |
| `v2/adapter/documentexec/discoveryadapter/` | node discovery, capacity and identity adapters |
| `v2/entry/documentcoord/`, `v2/entry/documentexec/` | assembly and CLI for the coordinator and the executor node |
| `v2/cmd/document-coordinator/`, `v2/cmd/document-executor/` | the two standalone processes |
| `v2/cmd/document-migrate/` | created the candidate document-processing schema (`document_processing_jobs`, `document_processing_executions`, `document_executor_capacities`, `document_executor_nodes`, `document_artifact_cleanups`) |
| `v2/service/documentcoord/`, `v2/service/documentexec/` | the coordinator and executor-node services |
| `v2/service/documentops/`, `v2/controller/docops/` | the cross-user operations surface (`/api/v1/ops/document-processing/...`) |
| `v2/api/documentexec/` | the cross-service document protocol contract |
| `v2/common/rag/artifact*.go` (5 files) | retrieval from sealed artifacts, as opposed to from the chunk index |
| `v2/dao/knowledge/{artifact,execution_store,execution_node,coordination,ops_queries,candidate_schema,execution_migration}.go` | the candidate schema, execution stores and coordination queries |
| `v2/model/document_execution.go` | the candidate execution/job/capacity models |
| `v2/internal/isolated/candidate.go` | the candidate runtime, its plan and its wiring |

The `kamaRPC` dependency is gone from `v2/go.mod` and `hybrid/go.mod`, and
`third_party/rpc-framework-a04-r1-frozen/` is **not** part of this release —
neither vendored nor referenced. Nothing in the tree names it.

### 2.2 Frontend

| Removed | Why |
|---|---|
| `web/src/composables/useOpsAccess.js` | only existed to decide whether to show the monitor entry |
| `web/src/views/OpsProcessingView.vue` | the monitor page for the removed operations surface |
| `web/src/api/docOps.js` → `getOpsStatus` / `listOpsEvents` / `streamOpsEvents` | the operations client; `getDocumentProcessing` (the owner's own processing state) is kept |
| `web/test/docOpsStream.test.js`, `web/test/opsProcessingView.test.js` | tests for the removed surface |

### 2.3 Tests that went with the removed feature

Deleted with their feature: the RPC adapter, coordinator, executor, ops and
candidate-schema test files; the artifact repository tests; and
`hybrid/knowledge_baseline_dataset_test.go`, which read a private dataset from
another repository (`KamaRPC-Go-main`).

**Moved rather than deleted**, because they cover behaviour this release still
has:

| Was | Now | Why |
|---|---|---|
| `v2/service/documentexec/legacy_mysql_test.go` | `v2/dao/knowledge/legacy_unmigrated_test.go` | it proves the legacy repository still works on a schema created before the candidate columns existed |
| `v2/dao/knowledge/artifact_legacy_test.go` | `v2/dao/knowledge/legacy_schema_test.go` | the isolated-schema fixture the legacy DAO integration test needs |

**Trimmed rather than deleted**: `v2/router/o3_test.go` and
`v2/controller/knowledge/o3_test.go` keep their local-processing assertions and
lose their candidate-mode ones.

### 2.4 Internal development records

`tutorial/` went from 8,908 tracked files to 17. What was dropped:

- the whole RPC design and acceptance tree (`tutorial/rpc-document-processing/`,
  6,574 files: run logs, compressed build and test captures, JSONL datasets);
- the per-round experiment directories (`v2-j1-*`, `v2-tmpfs-*`,
  `v2-real-gates-plan`, `v2-host-port-tool`, `evaluation/`, and others);
- the private document dataset reference and every machine-specific absolute
  path inside them.

Two things were also removed from the Go tree, because they are the same kind
of material:

| Removed | What it was |
|---|---|
| `v2/internal/summaryreadeval/` (127 files, ~2.2 MB) | the internal harness for the paid summary/episode real runs: frozen plans, authorization phrases, run ledgers. Its own test package failed in the committed tree because the tree still held a consumed run's ledger, manifest and validation report. |
| `v2/cmd/summaryread*` (8 commands) | the commands that drove that harness. Nothing in the product imports either. |

What is kept, and why, is listed in `tutorial/README.md`: the example
configuration the test suite reads, the deployment and verification notes the
configuration template points at, and the small sealed image-entry evidence
that `deploy/redis-stack-preflight.sh` cites by file and line number.

`v2/exp033/` went as well: two generated rehearsal artefacts that a test
recreates on its own.

### 2.5 Added back, because the release needs it

| Added | Why |
|---|---|
| `v2/cmd/schemainit` | The integrated entry refuses to change a schema, so a first-time installation needs one explicit command to create the tables. The command that used to do that went with the RPC feature, and without a replacement the release could not be installed from empty at all. It only creates what is missing: the migration is additive and never drops or rewrites existing rows. |

## 3. Behaviour changes

| # | Change | Who notices |
|---|---|---|
| B-1 | `/api/v1/ops/document-processing/{status,events,events/stream,metrics}` is **permanently 404**. | Nobody who ran the legacy mode: those routes were only routed in candidate mode and already answered 404 there. A deployment that ran candidate mode loses its cross-user monitor; the owner's own `…/:id/processing` is unaffected. |
| B-2 | `knowledgeConfig.executionMode` accepts only `""` or `"legacy"`. Any other value **refuses startup**. | A configuration carried over from a candidate build now fails loudly instead of silently running locally. |
| B-3 | `knowledgeConfig.candidateCoordinatorConfig` and `knowledgeConfig.opsUsers` are gone from the configuration surface. | TOML keys that are no longer known are silently ignored, so an old file still starts; the keys simply have no effect. |
| B-4 | `GET /api/v1/knowledge/documents/:id/processing` always answers `{"mode":"legacy","status":<index_status>}`. | The rich shape (stages, attempts, progress bar, localized failure label) belonged to the removed pipeline. |
| B-5 | **The owner's processing panel now polls in local mode.** | It previously polled only when `mode === 'candidate'`, so in the local mode it showed one answer and never refreshed - a stale status on a document that was still indexing. It now polls while the status is `pending` or `indexing` and stops once the document is ready or failed. |
| B-6 | The navigation rail no longer probes `/api/v1/ops/...` when the client boots. | One fewer request per page load. |
| B-7 | `go build ./...` at the repository root now succeeds. | It failed before, because `tutorial/` held Go files in two different packages (`rpc-document-processing/LEARN-E2`, `LEARN-E3`) and a file importing packages the root module does not have. Those files are gone. |
| B-8 | The three candidate columns (`processing_job_id`, `active_execution_id`, `active_pipeline_fingerprint`) stay in `knowledge_documents`. | Deliberate: no schema migration is needed, and the legacy repository keeps writing them as empty. `Omit(...)` in the DAO is kept so the legacy INSERT still works on a database that predates them. |

### 3.1 Behaviour changes from the closeout round

These are the contract changes made after the extraction, on top of the
behaviour the product already had. Each one is covered by a test.

| # | Change | Who notices |
|---|---|---|
| B-9 | `POST /api/v1/user/captcha` answers **2012** when no SMTP is configured (`[emailConfig]` has no sender address or authorization code) and **2013** when the relay rejects the message. Before, both were a generic `4001 服务繁忙`. | A reader who fills in `[emailConfig]` sees no difference. A reader who does not is now told what is missing instead of being told the server is busy. The server still starts, and every existing account keeps working. |
| B-10 | A request body that fails to bind answers **400** with `2001 请求参数错误` instead of 500; an upload that is not multipart answers **400** instead of 500. | Only clients that were reading a 500 for their own malformed request. Real server errors are still 500. |
| B-11 | An upload without a `file` field answers **7011 缺少文件字段 file** instead of the misleading `7001 仅支持有效的 .md 或 .txt 文件`. A real `.pdf` still answers 7001. | Only clients that sent the wrong field name. |
| B-12 | A 401 on the compatibility surface now carries the standard `{data,error}` envelope instead of an empty body. The status code is unchanged. | Only clients that parsed the body of an unauthenticated answer. |
| B-13 | The compose file's bind mounts that carry the operator's own files declare `bind.create_host_path: false`, and the checked-in preflight stays the container's entrypoint. The file also gained a profile-gated `schemainit` service, because the entry refuses to change a schema and this stack's MySQL publishes no port, so the tables could not be created from the host. | Operators of the integrated stack: the tables can be created with `docker compose --profile init run --rm schemainit`, and the schema is still never a startup side effect. A missing configuration is refused by the preflight before the server is reached. Note the key name: `create_host_directory` is **not** a Compose key and Docker Compose v5.1.3 refuses to parse a file that carries it — the correct key is `create_host_path`. |

## 4. What was verified, and how

All of it in a **clean directory** that contains a copy of this release and
nothing else — no reference to the original workspace, no `third_party/`, no
second checkout. The reproducible copy of that environment lives outside the
repository, at `…\GopherLearn-release-checks\release-closeout-v1\verify`
(`tools/` and `logs/` sit next to it); the temporary copy used during the
extraction round has been removed.

| Check | Result |
|---|---|
| Self-containment | The only two `replace` directives are `GopherAI => ../v2` (root module) and `github.com/xiaopang456/GopherLearn => ..` (hybrid). Both stay inside the tree. No file in the tree contains `kamaRPC` or `third_party/rpc`. |
| `go build ./...` — root, `v2`, `hybrid` | pass |
| `go build ./hybrid/cmd/gopherlearn-integrated` | pass, binary produced |
| `go vet ./...` — all three modules | pass (this also compiles every test file) |
| `go test ./...` — `v2` | ✅ **green**. At extraction time it still carried the four failures the source tree had (`config`, `internal/summaryreadeval`, `middleware/jwt`, `utils/myjwt`); the closeout round fixed the three that were missing a test environment and removed the fourth with its feature. See `RELEASE-READINESS.md` §1.4. |
| `go test ./...` — `hybrid` | ✅ **green** once a POSIX shell is on `PATH`. The three cases that fail without one execute `deploy/*.sh`, and the suite deliberately **fails rather than skips** when it cannot find a shell. |
| `go test ./...` — root module | ✅ **green** in the closeout round. It carried the same two failures as the source tree (`TestDrainWindowIsShorterThanTheContainerStopWindow` in `cmd/gopherlearn` and `TestRedisIsDeclaredOnlyInRegisteredComposeFiles` in `internal/app`). Both turned out to be **CRLF artefacts**: the two files they read (`cmd/gopherlearn/main.go`, `compose.integrated.yml`) were checked out with CRLF, so the assertions could not match at all. The files are now normalised to LF and pinned by `.gitattributes`; **no assertion was changed**. See `RELEASE-READINESS.md` §1.6. |
| `npm ci` / `npm run lint` / `npm test` / `npm run build` — `web` | pass; 52 test files, 1053 tests, lint clean, production bundle built |
| The integrated entry starts from the clean tree | yes: it announces its entry line, opens the knowledge index worker and the cleanup worker, and serves `/api/v1/me` |

### 4.2 The closeout round's functional check

The closeout round re-verified the release end to end in a clean environment
with its own schema (`gopherlearn_release_v1`), its own Redis database, a local
non-forwarding stand-in for the model endpoints, and a **local SMTP capture
service** — the captcha mail went into that capture and nowhere else.

| Behaviour | Result |
|---|---|
| request a captcha → the mail is captured → read the six digits out of the captured message → register → log in → `/me` | pass, through the real mail path. **The code was never written straight into Redis.** |
| a wrong code / an expired code / a duplicate registration | refused as the contract says: 2008 / 2008 / 2002 |
| no `[emailConfig]` at all | the server starts, every existing account keeps working, and only the captcha request says what is missing (2012) |
| upload a `.md` → the in-process worker chunks, embeds and indexes it → `ready` | pass; `chunk_count: 2` |
| `GET …/:id/processing` | `{"mode":"legacy","status":"ready"}` |
| a second account reading the first account's documents, sources, sessions and memories | 404 / empty lists — cross-user isolation holds |
| the removed operations surface and the baseline collection endpoints | 404 |
| the docker-compose stack | images built, schema created out of band, tables created by the profile-gated `schemainit` service, four containers up, client 200 on `http://127.0.0.1:8083/`, `/api/v1/me` refusing an unauthenticated call with 401 |

The evidence is in the repository-external logs under
`…\GopherLearn-release-checks\release-closeout-v1\logs\`.

### 4.1 Functional check against the running public build

The backend was started from the clean directory against an **isolated MySQL
schema** (`gopherlearn_public_verify`), **Redis database 1**, and a **local
stand-in** for the embedding and chat endpoints on `127.0.0.1:18999`. The
stand-in returns a deterministic hash-derived 1024-dimensional vector per
input, so the in-process pipeline could be exercised without calling a paid
provider.

| Behaviour | Result |
|---|---|
| login, `/me`, two accounts | pass |
| session create / list / delete | pass |
| second account sees no sessions | pass (empty) |
| **upload a `.md`** | `201`, `index_status: pending` |
| **the in-process worker indexed it** | `index_status: ready`, `chunk_count: 2`, `active_version: 1` — the worker parsed, chunked, called the stand-in embedder and stored the vectors |
| `GET …/:id/processing` | `{"mode":"legacy","status":"ready"}` — the local shape |
| `GET …/:id/source?version=1&start_line=1&end_line=4` | the original lines, with their numbers |
| `/api/v1/ops/document-processing/status` and `/metrics` | `404` — the removed surface |
| memory create, and the same `idempotency_key` again | pass, the same memory id (replay) |
| second account: document list, document read, source read, session messages, memory list | empty list / `404` / `404` / `404` / empty list — **cross-user isolation holds** |
| cleanup | the verification memory, document and session were deleted again |

**What this does not prove**: retrieval *quality*. The vectors came from a
stand-in, so this run shows that the pipeline, its storage and its permissions
work — not that embeddings are meaningful. Retrieval quality was not measured
in this release and is not claimed.

## 5. Not verified, and open items

### 5.1 The pre-release functional audit, and what happened to it

These are upstream defects the audit found. The closeout round **fixed** the
ones that block a public release and **registered** the two that are not
defects but contract decisions.

| # | Defect | Where | Status |
|---|---|---|---|
| F-01 | A new user cannot register in a default deployment: the captcha is delivered by e-mail and no SMTP is configured, so `POST /api/v1/user/captcha` answered "service busy" and `POST /api/v1/auth/register` answered 501. | `v2/service/user/user.go`, `hybrid/http.go` | **fixed** — the SMTP host is configurable, the two failure modes have their own codes (2012 / 2013), and a missing `[emailConfig]` no longer stops the server or any existing account. See B-9. |
| F-02 | A request body that fails to bind answers **500** instead of 400 (memory create / patch / archive / delete / draft resolve). | `v2/controller/memory/memory.go` | **fixed** — 400 with `2001`; real server errors are still 500. See B-10. |
| F-03 | A document upload whose body is not multipart answers **500** instead of 400. | `v2/controller/knowledge/document.go` | **fixed** — 400; an oversized upload is still 413. See B-10. |
| F-04 | A chat-mode session ignores `requestId`, so the same key creates two sessions (agent mode does dedupe). | `v2/service/session/isolated.go` | **not changed on purpose** — the API contract (`spec/API.md`) promises only `title` and `mode` for a chat session; `requestId` is the learning Agent's operation key. Making chat idempotent would be a new promise, so the limitation is registered instead of quietly widened. |
| F-05 | The file-size limit is 2 MiB on one page and 5 MiB on the other; the server allows 5 MiB. | `web/src/api/knowledge.js` vs `v2/config/config.go` | **fixed on the client** — the upload page now says that 5 MiB is the server's default (`knowledgeConfig.maxFileSizeBytes`) and that the server's own message is the authority. |
| F-06 | `/knowledge`, `/memory` and `/learning` are still routed but their APIs belong to the baseline entry, so the pages show misleading errors. | `web/src/router/index.js` | **fixed as an explanation** — the three baseline pages carry a permanent notice saying which entry they belong to, with a link to the integrated entry's own page. Their failure-message contract ("a 404 has its own sentence and leaks nothing") was deliberately left alone. |
| F-07 | A 401 on the compatibility surface carries no body, so the `{data,error}` envelope is not honoured there. | `hybrid/http.go` | **fixed** — same status code, standard envelope. See B-12. |
| F-08 | A wrong upload field name is reported as an unsupported file type. | `v2/controller/knowledge/document.go` | **fixed** — 7011 for a missing `file` field, 7001 still for a real `.pdf`. See B-11. |

### 5.2 Not verified in this release

| Item | Why |
|---|---|
| Retrieval quality, answer quality, quiz grading, the learning Agent against a real provider | **Small-scale real run done this round**: ordinary chat, document upload → in-process index → grounded answer with citations and line ranges, a real `knowledge_search` tool round trip, three-question quiz generation with evidence references, and one graded answer with the mastery projection written and read back. This is **one account, one self-built document, one topic, one graded answer** - it is not a quality result. See `RELEASE-READINESS.md` §7. |
| Rolling summary, memory extraction, episode projection, dream consolidation | **Not exercised.** In the real run these four background model calls never happened: the summary trigger did not fire, no memory-extraction job was enqueued (the agent turn proposed a draft, which the product rule skips), and the episode projection job was still `pending` when the run stopped. |
| Resuming an open quiz after a page reload | **Verified** with a fixture task and no model call: reload the page, pick the learning_agent conversation, press the button - the task, its progress, the question and the source line range all come back. The request is `GET /api/v1/AI/agent/learning/current?sessionId=…`; the button is **enabled only once a conversation is selected**, and switching conversations does not resume on its own (the page says so). See `RELEASE-READINESS.md` §7.8. |
| The remote-executor path | removed from this release. It was never exercised in the source tree either: it ran in legacy mode there. |
| PDF document processing | not implemented in this codebase at all; only `.md` and `.txt` are accepted. |
| The `ops` monitor | removed with its feature. |
| Delivery to a real mailbox | the closeout round sent the captcha only to a **local SMTP capture service** and read the code out of the captured message. Real relay behaviour - delivery, bounce, STARTTLS - was not exercised. |
| Upgrading an existing deployment | the closeout round installed from empty. No upgrade path over existing data was tested. |
| The CI workflow | `.github/workflows/ci.yml` was written for the three-module layout, but it was not executed on a runner. |

The docker-compose deployment **is** verified: the images were built, the stack
was brought up, the schema was created out of band, the tables were created by
the profile-gated `schemainit` service, and the client answered on
`http://127.0.0.1:8083/` (200) with `/api/v1/me` correctly refusing an
unauthenticated call (401). The evidence is in the repository-external logs
(`…\GopherLearn-release-checks\release-closeout-v1\logs\docker-verify.log`).

## 6. How to run this release

The repository's [README.md](../README.md) is the entry point for a first-time
user: it explains the features, the configuration fields, the local start and
the Docker deployment, and states plainly what is verified and what is not.

The shortest path, from the repository root:

```bash
cp config.integrated.example.toml config.integrated.toml.local   # fill it in
go -C v2 run ./cmd/schemainit -config ../config.integrated.toml.local
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
```

The client is in `web/`; point the dev proxy at the integrated entry's
address — see `web/README.md`.

With Docker, the integrated stack is `compose.integrated.yml`. It needs three
operator-owned files that are not in the repository (they end in `.local` and
are git-ignored): `config.integrated.toml.local`,
`redis.integrated.conf.local` and `.env.compose.integrated.local`. The schema
and its account are created out of band, the tables are created by the
profile-gated init service, and the client is on `http://127.0.0.1:8083/`:

```bash
docker compose -p gopherlearn-integrated -f compose.integrated.yml up -d --build
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm schemainit
```

The compose file deliberately does not create the schema: the application entry
refuses to change one, so `up` fails loudly when the tables are absent.

## 7. Licensing

- `LICENSE` — GNU General Public License v3.0, carried over from the upstream
  GopherAI packaging.
- `NOTICE.md` — upstream provenance (`youngyangyang04/GopherAI`, GPLv3), the
  related public derivative consulted during development, and the scope note
  for this release.
- `docs/THIRD-PARTY.md` — the resolved dependency inventory with the license
  each package declares. Regenerate it with `tools/audit-licenses.ps1`.

The remote-executor framework that earlier development builds vendored under
`third_party/` is **AGPL-3.0** and is **not** part of this release, so it
contributes no dependency and no license here. That is a licensing outcome of
the scope decision, not a separate change.
