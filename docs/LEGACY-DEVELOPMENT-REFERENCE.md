# 历史基线开发参考（LEGACY DEVELOPMENT REFERENCE）

> **仅适用于历史基线入口，不是本项目正式应用的快速开始。**
> 本项目的正式入口是 `hybrid/cmd/gopherlearn-integrated`；它的安装、配置与启动步骤全部在
> 仓库根目录的 [README.md](../README.md) 里。本文件只是把仓库里**历史基线入口**
> （根模块 `cmd/gopherlearn`）的开发说明、环境变量表与冒烟检查集中存放，供查阅历史实现时使用。
> 这里的 `GOPHERLEARN_*` 环境变量属于基线入口的配置面；正式入口读的是
> `config.integrated.toml.local`（见 [INTEGRATED-REFERENCE.md](INTEGRATED-REFERENCE.md)）。

## Development status

Backend phases 1-4B.2 are accepted: session modes, GORM/MySQL storage, the Gin
routes with their golden contract, the offline RAG backend with citation
persistence, the model seam with its OpenAI-compatible adapter, and a learning-Agent
Harness with a server-owned tool at the time of the sealed run - `knowledge_search` -
whose full two-request tool round trip has now been executed once against a real provider
and sealed. The Vue client's
knowledge-base management, per-turn knowledge toggle, citation display, history
restoration and cross-account isolation are complete.

Phase 5A is implemented in the same codebase and not yet accepted: chat, knowledge
answering and the learning Agent now share one context - a per-session rolling summary
plus the recent complete turns - instead of each keeping its own window, and the summary
is one stored row per session, so it survives a restart exactly like the messages under
`mysql` and is as volatile as they are under `memory`.

Automatic episodic memory is now implemented behind its own opt-in switch
(`GOPHERLEARN_EPISODES=on`, see "Episodic memory (cross-session experience)" under
Configuration): a background projector turns each finished conversation into one versioned
cross-session experience card, indexes it for retrieval, and an agent turn can recall it
across sessions through `episode_search` with read-back through `read_history_message`. It
is not yet accepted the way the phases above were, and it shares the confirmation-gate
boundary with everything else in this list: nothing it produces ever enters the
user-confirmed memory path.

Still not built: automatic extraction into the confirmed long-term memory table (nothing
proposes a draft on its own), and a topic consent store that would authorize topic recall. An agent turn being watched (`stream_steps: true`) does publish each
model call's text as it is produced, and folds a tool round's narration away as a note once the
answer is confirmed; an unwatched agent turn is still re-chunked after the answer exists, and
no path asks the model an extra question to announce that it is ready to answer.
Topic mastery is no longer on this list: Phase 6B computes it server-side and the quiz loop
reads it back, but nothing outside the quiz consumes it yet - a Chat or Knowledge answer is
not personalized from it.

Phase 5B.1, 5B.2 and 5B.3 are implemented in the same codebase and not yet accepted:
long-term memory now has a home, a bounded way to be read back, and the confirmation screen
a decision is actually made on. Three layers are modelled -
`global` for stable facts and preferences, `topic` for understanding of one subject keyed
by `topic_key`, and `episodic` for experienced episodes with time, session and message
evidence. A memory arrives by one of two doors, and `origin` - an internal stored field,
never a request or response field - records which. Someone typing in a fact about themselves
(`domain.UserAuthoredMemory`, global or topic only) IS the confirmation: it is stored `active`
with the server's own `confidence` of 1 and cites no conversation, because the sentence is its
own evidence. A machine's proposal (`MemoryDraft`) is the opposite: a candidate that cannot
represent any state but pending, which has to name a real session and real messages of its own
owner, and which only `MemoryService.ConfirmMemoryDraft` - a user decision in one transaction,
never a model's judgement - can promote. Confirming or replacing a global or topic memory
retires the previous active version of the same subject (`superseded`); episodes accumulate. A
proposal's citation is re-checked when it is confirmed, a deleted session leaves the memories
standing - the reference survives, the confirmation of a draft whose evidence is gone is
refused - and an episode can only ever come from a conversation, so it is not hand-writable at
all.

Phase 5B.2 adds the read side, and only for the learning Agent: `MemoryAccess` recalls a
user's own `active` memories with a deterministic lexical match inside a grant this server
builds (global facts plus the current session's episodes; no topic and no other session's
episodes until a consent store exists), bounded by result count, per-item length, total
length and a confidence floor, and ordered by score then stored creation order rather than
by any row order. The floor defaults to `0.6` - the filter strength the long-term-memory
pairing experiment measured as M2 - and a caller can only tighten it: the effective floor is
`max(0.6, requested)`, `0` in a request means "unset" rather than "show everything", and
there is no parameter that turns the filter off. `active` is necessary but not sufficient
for visibility; a row still has to clear the scope grant, the floor and the capacity limits.
Chat and knowledge-grounded Chat are untouched by all of this: neither reads the memory
table, and a test fails if the conversation path performs any memory operation during a
turn.

Phase 5B.3 adds the write side that a person operates - the memory HTTP API and the screen
on top of it. `POST /api/v1/memories` stores what the caller typed, for `global` or `topic`
only: it comes back `active` in the same `200` with no provenance to check and no second
confirmation to click, because submitting the sentence IS the decision. A machine's candidate
is not on this route at all - it arrives only through the agent's `propose_memory` - so the
screen has one kind of row to wait on. `GET` with `?state=pending` or `?state=active` lists
the caller's own drafts and confirmed memories, and
`POST /api/v1/memories/{id}/confirm|reject|archive` makes the three lifecycle moves. The
route publishes no way to name a state: `status`, `origin`, `active`, `superseded_at`,
`confidence`, the timestamps and the source fields are unknown keys and are refused, an
action carries no body at all, a
draft whose cited conversation has since been deleted is confirmed nowhere (`409
memory_evidence_missing`) while staying on the pending list, and another user's memory is
indistinguishable from one that never existed. `confirm` is still the store's conditional
write, so four concurrent confirmations produce exactly one success, and a hand-written
replacement retires the previous active answer to the same subject in one transaction. The
frontend is a
confirmation screen only - drafts with their scope, topic key, provenance and confidence,
and confirm / reject / archive buttons; it proposes nothing, and the demo adapter it can run
on in `npm run dev` labels its own rows as demo data, while a production build always talks
to the server.

Phase 5B.4 gives the learning agent one write of its own - `propose_memory` - and it is
the same capability the upstream project had, re-derived for this architecture rather
than copied. The model may say `scope`, `topic_key` and `content`; everything that decides
whose memory it is and what it rests on is filled in from the turn, and the only stored
message it may cite is this turn's own question, which is already in the database (the answer
is not, so it cannot be cited and nothing older can be named). What comes back to the model is
one sentence: a draft is waiting, and whether it takes effect is the user's decision on the
Memory page. No memory id, message id or store error ever reaches it, the result carries no
citation, and a pending draft is invisible to recall, so a proposal cannot become the agent's
own evidence. Confidence is fixed by the service at "certain", the capability may run once per
turn, and a repeat of the same claim returns the row already waiting instead of a second copy.
Tightened relative to the upstream design: strict argument parsing (a payload carrying `user_id` or
`status` is refused whole, not ignored downstream), no identifiers in the result, and
message-level provenance. Deliberately not carried over: `subject`, `tags`, `durability`,
`valid_until`, conflict enumeration with `replace`, a global-memory quota, and `RunID`-keyed
run-level idempotency. There is no provider smoke for `propose_memory` at all - its only
evidence so far is offline.

Phase 6B is written and its contract is closed: the learning loop has its own HTTP surface,
this build serves it, and `spec/API.md` together with the golden HTTP contract records its
success shapes, refusal codes and state transitions. The phase has not been signed off as a
delivered milestone, but its two open shape decisions have been - the response DTO and the
`grading -> waiting_answer` recovery edge were reviewed and their fix accepted in this closure,
so the shapes described below are what this build serves, not a proposal. What the quiz surface
still lacks is evidence about a model rather than about a contract, and that is stated at the
end of this section.
A learner names a topic, this server selects evidence from that
learner's own notes, generates exactly three questions with a model, stores an immutable
snapshot of the evidence behind each one, scores every answer against the dimensions
(correctness 60, completeness 25, clarity 15 - the total is ours, never the model's), and
folds the result into one projection per topic. Six routes, all bearer-authenticated:
`POST /api/v1/learning/sessions`, `GET /api/v1/learning/sessions`,
`POST /api/v1/learning/sessions/{id}/answer`, `.../skip`, `.../cancel` and
`GET /api/v1/learning/progress`. The contract - success shapes, error codes and the seven-state
machine - is written up in `spec/API.md` ("Learning workflow (quiz surface)"), and every route
above is asserted row by row in the golden HTTP contract under
`internal/httpapi/testdata/http_contract.golden`.

Two placement decisions are the shape of that contract, not details of it. Mastery, confidence,
valid-attempt count, level and weak points are readable in exactly one place -
`GET /api/v1/learning/progress` - and the answer, skip, cancel, start and resume responses carry
no projection at all: the graded attempt and the projection it caused are both stored and
neither is echoed, so no second copy of a learner's record exists that a correction could leave
behind. And the client never sends a number in the other direction either: there is no score,
status, owner or timestamp field on any learning route, a body on `cancel` is a 400, and an
attempt to send a score is refused as an unknown field with no write performed. A learner
cannot grade themselves, so the projection keeps meaning something.

A quiz is bound to one kind of conversation. `POST /api/v1/learning/sessions` checks ownership
first and conversation mode second, so starting on your own plain chat conversation is a 400
`invalid_request`, while a conversation you do not own - or one that does not exist - keeps
answering exactly the 404 `not_found` it answered before the rule existed. The refusal writes
nothing: no quiz row, no model call, no progress change. Only *starting* is gated; reading the
projection is not, because `GET /api/v1/learning/progress` is keyed by topic rather than by
conversation, and the mastery a learning-agent quiz produces is read back into agent turns and
into nothing else - a chat conversation never sees it. The web picker lists only
learning-agent conversations for the same reason, and that is a usability filter sitting on top
of the server rule, not a substitute for it.

Every request body must be valid UTF-8, on every route, and the check runs before the JSON is
parsed. A body in another encoding answers the same `400 invalid_request` any malformed body
gets, and it leaves nothing behind: no row, no model call, no consumed quiz `submission_id`, no
change to the conversation. The alternative was silent damage - `encoding/json` replaces bytes
it cannot read with U+FFFD and reports nothing, so a GBK or Shift-JIS client would have stored a
learner's own sentence as a run of replacement marks and seen a `200`. A lone surrogate escape
(`"\ud800"`) is refused for the same reason, while a correctly paired one (`"\ud83d\udd2c"`) is
just 🔬 and is stored as submitted. Chinese, emoji and characters outside the Basic Multilingual
Plane such as 𠮷 are ordinary valid UTF-8 and pass through untouched.

What is not built is stated in the same place: no feedback or "explain my answer" endpoint, no
list of past quizzes, no resuming a terminal one, no recomputation endpoint, no automatic quiz,
and no environment override for the question count (3), the quiz lifetime (24h) or the
projection thresholds. `memory_search` remains unproved against a real provider, and no part of
the quiz surface has been run against one either: every number above comes from the offline
suite.

The quiz can nevertheless be driven end to end with no provider at all: with
`GOPHERLEARN_LOCAL_LEARNING_MODEL=deterministic` in echo mode, the real `LearningService`, the
real seven-state machine and the real HTTP routes run against a scripted question-and-grade
model, which is what a browser session needs to exercise the flow. It proves the pipeline and
not a judgement - the grade is chosen by the question, never by what the learner typed - and it
reaches no other answer path (see "Assistant model provider" above). One production rule is
worth knowing before such a run: a chunk of notes shorter than 128 runes is not evidence, so a
notebook of headings and one-line bullets answers `learning_no_evidence` instead of being
padded into a quiz.

Two real provider runs have been executed, both on 2026-09-25, each under its own
single-use authorization. The first is the plain provider smoke: three requests spent of
a cap of three, zero failures, zero retries, and the unrelated question cost nothing.
Its record is
[`spec/verification/provider-smoke-v1.json`](../spec/verification/provider-smoke-v1.json)
(SHA-256 `ee0ba6c4cda0109dde14590b0a80720d75bc04eeca9dbf6713e76495c307aeb8`), and it is
a one-time piece of evidence: never overwritten, never re-run without a new
authorization. What it proved is connectivity and the call chain - a non-streaming
completion, a streamed completion, and one grounded `knowledge_qa` turn through the
production assembly. It does not attest answer quality, a trained embedding, Redis, or
the Agent Harness, and it says nothing about vendors other than the endpoint used.

Phase 11A gives the retrieval backend a real second option and changes nothing about the
first. `GOPHERLEARN_RETRIEVAL_MODE=semantic_redis` embeds chunks through any OpenAI-compatible
`/embeddings` endpoint, keeps the vectors in a RediSearch index, recalls by cosine KNN inside
the caller's own collections, and re-ranks the fused head with a cross-encoder;
`deterministic` remains the default and its startup behaviour, scoring and citations are
what they were. Both modes share one fusion implementation, so the difference is which
signals admit a chunk, not how admissions are merged. What has been executed: the retrieval
and index layers offline against fakes; the index adapter against a real Redis Stack
container (its own gated suite, skipped by an ordinary `go test ./...`), which is where the
database-0 and dash-escaping rules below came from; the embedding client against a real
1024-dimensional endpoint; and the reranker client against a locally served
`BAAI/bge-reranker-v2-m3`. `cmd/gopherlearn` reads the mode at startup, so both are reachable
from the binary, and the startup line says which one is live. What is still not established
here: retrieval *quality* against a human-judged corpus, and a semantic run that also answers
with a real assistant model - the chain below is verified with the offline generator, because
an embedding endpoint and a chat provider are different dependencies and only the first was
in scope.

Behavior contracts live in [`spec/`](../spec/). They intentionally describe
requirements without incorporating source code or documentation from earlier
projects.

## Application entries

The repository now holds two runnable backends, and which one a host runs is a
deployment choice rather than a code flag:

| Entry | Module | Agent execution | Route table it answers | Role |
|---|---|---|---|---|
| `hybrid/cmd/gopherlearn-integrated` | `gopherlearn-v2-hybrid` | the original V2 Eino ReAct (`v2/service/agent/runner.go`) | the mainline contract paths served by the compatibility surface, every other path passed to the original V2 router | the integrated application |
| `cmd/gopherlearn` | `github.com/xiaopang456/GopherLearn` | the mainline harness (`internal/agent/harness.go`) | the fourteen paths in `internal/httpapi/router_gin.go` | historical baseline, kept so a comparison run has something to compare against |

They must not serve one dataset at a time: both write `users`, `sessions`,
`messages`, `session_summaries`, `episode_memories` and the `learning_*` tables,
with column definitions that are not interchangeable, and each starts its own
episodic and knowledge workers. Two things, kept apart because only one of them is
a code guarantee:

- the entry refuses to start unless the schema name, the Redis host and port, the
  database number, the HTTP address and the knowledge directory are all written out
  explicitly, and it refuses `isolatedEntry.migrate = true`, so starting it never
  changes a schema;
- which values an operator writes is a deployment constraint. Naming a baseline
  schema, a shared Redis instance or the baseline knowledge directory is not
  detected by the entry, because "that one belongs to the other stack" is not
  something a loader can know. The template, the compose files and the resource
  names are what keep the two apart.

```cmd
cd hybrid
go build -o ..\bin\gopherlearn-integrated.exe .\cmd\gopherlearn-integrated
..\bin\gopherlearn-integrated.exe -config ..\config.integrated.toml.local
```

`config.integrated.example.toml` documents every key with placeholders only;
copy it to `config.integrated.toml.local` and fill it in. The `.local` suffix is what
both `.gitignore` and `.dockerignore` match, so the file is neither committed nor
sent into a build context; a name of the form `<stem>.local.toml` would match
neither rule, because the pattern is a suffix and not a substring. The entry reads no
credentials from the environment. Container deployment uses
`Dockerfile.backend-integrated` with `compose.integrated.yml`, which keeps its own
project, container, volume and network names for the same reason. The search index is
one service of that same file - one command, one startup story - and it is started
through `deploy/redis-stack-preflight.sh`, which refuses to hand over to the image's own
entrypoint unless the server configuration the entrypoint really reads
(`/redis-stack.conf`) is present, readable and carries exactly one conforming
`requirepass` line. Health is `deploy/redis-stack-healthcheck.sh`, which asks the search
module (`FT._LIST`) and accepts only a JSON array of strings. Neither script, nor the
compose file, nor the command line ever carries the credential value: the operator's own
`redis.integrated.conf.local` is the only place it lives, and `redis.integrated.conf.example`
documents the shape. Both deployment scripts are pinned to LF in `.gitattributes`, because
`/bin/sh` inside the container executes them byte for byte.

Ordinary chat stays on the retained `DirectChat` responder with its own model
client: it is not an agent turn, gets no tools, no memory provider and no queue.
Collection management, the session-level topic-memory switch and the legacy
`/api/v1/memories` and `/api/v1/learning` surfaces belong to the baseline entry;
the integrated application answers with the original V2 knowledge, memory and
learning routes instead, and the front-end navigation reflects that.

## Running the baseline entry

This section is about the **historical baseline** entry, `cmd/gopherlearn`. It
is not the application: it is kept so a comparison run has something to compare
against, and it answers a different route table (the fourteen paths in
`internal/httpapi/router_gin.go`) from a different module. The application -
the learning Agent, layered memory, the quiz surface, the V2 knowledge base and
the Vue client that talks to them - is the integrated entry, and its commands
are in "Quick start" above.

| | `hybrid/cmd/gopherlearn-integrated` | `cmd/gopherlearn` |
|---|---|---|
| What it is | **the application** | the historical baseline, kept so a comparison run has something to compare against |
| Route table | the mainline contract paths plus the original V2 router | the fourteen paths in `internal/httpapi/router_gin.go` |
| Configuration | `config.integrated.toml.local` (see "Quick start") | environment variables |
| Needs | MySQL + Redis + a model provider | nothing (in-memory, deterministic answers) |

The integrated entry is what the Vue client in `web/` is written against.
Running the baseline entry with that client produces a front end whose routes
the backend does not serve.

The one thing the baseline process needs is a signing secret. The bash form sets
it for a single command; PowerShell and `cmd.exe` have no equivalent prefix, so
copying the block below into them fails before Go is involved - the forms after
this are the same command, not new options.

```bash
GOPHERLEARN_JWT_SECRET="replace-me-with-a-long-random-value" \
go run ./cmd/gopherlearn
```

```powershell
$env:GOPHERLEARN_JWT_SECRET = "replace-me-with-a-long-random-value"
go run ./cmd/gopherlearn
```

```bat
set "GOPHERLEARN_JWT_SECRET=replace-me-with-a-long-random-value" && go run ./cmd/gopherlearn
```

With no other variables the process stores sessions and messages in memory, uses no
provider, and answers from deterministic placeholders and extractive knowledge
quotes. It listens on `:8080`. Building a binary works the same way:

```bash
go build -o bin/gopherlearn ./cmd/gopherlearn
```

### Keeping the data: MySQL on Docker, backend on the host

`memory` stays the default - it is what every example above and the whole offline
test suite use, and nothing here changes that. `mysql` exists for the one question
memory cannot answer: what happens to a learner's records when the process stops.

`compose.mysql.yml` starts **only MySQL**. There is no application image, and the
Go backend and the Vite dev server keep running from the host exactly as above;
there is no Redis and no RabbitMQ service here either - the vector index has its own
file (`compose.redis.yml`) and is opt-in - and no provider. The stack is:

```bash
cp .env.mysql.example .env.mysql.local       # then replace both passwords with generated values
docker compose --env-file .env.mysql.local -f compose.mysql.yml up -d --wait

GOPHERLEARN_JWT_SECRET="replace-me-with-a-long-random-value" \
GOPHERLEARN_STORAGE=mysql \
GOPHERLEARN_MYSQL_DSN="user:password@tcp(127.0.0.1:3308)/gopherlearn" \
go run ./cmd/gopherlearn
```

The same start in PowerShell, where the prefix form above does not work:

```powershell
Copy-Item .env.mysql.example .env.mysql.local   # then replace both passwords
docker compose --env-file .env.mysql.local -f compose.mysql.yml up -d --wait

$env:GOPHERLEARN_JWT_SECRET = "replace-me-with-a-long-random-value"
$env:GOPHERLEARN_STORAGE = "mysql"
$env:GOPHERLEARN_MYSQL_DSN = "user:password@tcp(127.0.0.1:3308)/gopherlearn"
go run ./cmd/gopherlearn
```

What is deliberate in that file: the image is pinned by digest (MySQL 8.0.46) so
"the version we verified" cannot quietly become a moving tag; the port is published
on `127.0.0.1` only; the data lives in a named volume owned by this project; the
server stores `utf8mb4`, which is what the Chinese learner content this product
handles requires; a healthcheck gates readiness, because the image runs a
throwaway server during initialisation and "started" is not "connectable"; and
`stop_grace_period: 30s` gives the server time to close itself.

That last one is measured, not guessed. Under Docker's 10-second default the container
exited **137** - SIGKILL, which is what an operator reads as a crash even though no row had
been lost. With 30s, `docker compose stop` returned in 1.3s, the log shows
`Received SHUTDOWN ... via user signal` followed by `Shutdown complete`, and the container
exited **0**. Stopping again while a transaction was open took 3.4s and still exited 0: the
server killed the connection, the transaction rolled back as a whole (two rows inserted, zero
survived - never one), and the committed data and the 134-column schema fingerprint came back
unchanged on the same volume. A static test pins the window at 30s or more and fails if the
overlay ever shortens it.
`.env.mysql.local` is not committed and the example file holds placeholders only -
compose refuses to start if any credential is missing rather than defaulting to an
empty password.

`Migrate` and `MigrateKnowledge` run at startup against an empty schema; the
deployment is expected to hold 14 tables. The connection string is never logged,
and a failed startup prints a scrubbed message.

Two behaviours are worth knowing before pointing a browser at this. Both were measured by
restarting the container under a still-running backend, and neither moved a row: the SQL
fingerprint of every table was byte-identical afterwards.

- While MySQL is actually down, requests answer `500`. That is the database being
  unavailable, and it is the correct answer. In one measured restart it lasted a couple of
  seconds, and the server was already answering queries before the compose healthcheck
  reported `healthy` - so `--wait` is conservative rather than racy.
- After MySQL comes back, **one request per pooled connection that was torn can still answer
  `500`**, and the next one succeeds. The tear is not tied to a restart: anything that closes
  an idle connection - a restart, a `KILL`, a firewall reaping it, `wait_timeout` - leaves a
  dead socket in the pool, and the first request that draws it pays for it. On Linux the driver
  notices before handing the connection to a query and the request succeeds; on a Windows dev
  host that check is compiled out, which is where the `500` shows up.

A write that dies this way never reached the server, and a write torn **inside** a transaction
rolls back completely: a re-chunk killed while it waited on a row lock left the document's
chunk count and chunk rows both at their original values, with no orphans. The `500` is
therefore an honest answer, and nothing here retries a request on your behalf - a client that
retried a write after such an error could duplicate it. The quiz answer route is the
exception by design: `submission_id` makes a replay return the stored verdict instead of
grading twice.

Since Phase 8B.3 the pool also retires a connection it has not used for 90 seconds
(`DefaultConnMaxIdleTime` in `internal/repository/gormmysql/connect.go`), which bounds how
long a dead socket can sit in the free list on a host where the driver cannot probe it. That
narrows the window and does not close it: a connection torn while a request is already in
flight, or one drawn inside those 90 seconds, still answers with an error, and an unreachable
database always does. **None of this is high availability or a lossless rolling restart** - a
database restart costs this process a handful of failed requests, which is the honest
expectation for a single-instance deployment.

Finally, there is no half-started mode: with `storage=mysql` and no reachable database the
process exits during startup with a scrubbed message rather than serving traffic.

[`spec/verification/mysql-runtime-v1.json`](../spec/verification/mysql-runtime-v1.json)
records what the first real-MySQL run measured and what it did not;
[`spec/verification/mysql-runtime-v2.json`](../spec/verification/mysql-runtime-v2.json)
records the idle bound that came out of the follow-up diagnosis, including the negative
control that shows the bound - not the platform - is what recovers the connection.

## Running the whole stack on Docker

`compose.app.yml` adds the backend and the web client to the MySQL service from the section
above, so one command brings up the same three things the previous section described as
separate processes:

```bash
cp .env.compose.example .env.compose.local   # three different generated secrets, plus the DSN
docker compose --env-file .env.compose.local \
  -f compose.mysql.yml -f compose.app.yml up --build
```

```powershell
Copy-Item .env.compose.example .env.compose.local   # three different generated secrets, plus the DSN
docker compose --env-file .env.compose.local -f compose.mysql.yml -f compose.app.yml up --build
```

Then open `http://127.0.0.1:8081/`. To stop: the same command with `down` (add `-v` only if
the data should go too).

| Piece | How it runs |
|---|---|
| MySQL | the 8B.1 service unchanged: pinned 8.0.46 image, named volume, healthcheck, published on `127.0.0.1:3308` |
| backend | built by `Dockerfile.backend` (Go 1.24.10 → `scratch`), non-root, listening on `0.0.0.0:8080` **inside the compose network only** - no host port |
| web | built by `Dockerfile.frontend` (Node → `nginx:1.27-alpine`), the only host-visible entry at `127.0.0.1:8081`, proxying `/api/` to `backend:8080` |

The database sits on `gopherlearn-app-data`, which is an **internal** network; the client sits
only on `gopherlearn-app-web`, which the backend also joins. Being on a different network is
not what keeps the client away from MySQL - on one Docker host, bridges route to each other,
and a frontend container that never joined the database network still opened MySQL's container
address on 3306 while that network was an ordinary bridge. Marking it internal removes the
route, and the same probe then times out. `compose.mysql.yml` on its own still starts just MySQL
exactly as in 8B.1, on its own volume; the stack uses a separate volume and a separate
container name, so starting one never disturbs the other.

What the stack does **not** pretend to be:

- **This is one machine's development stack, not a high-availability deployment and not a
  zero-downtime rolling restart.** There is one replica of each piece, one network, one volume.
  A stop is observable from the client: requests that arrive after it are refused.
- `frontend` starts as soon as `backend` starts, and the backend has no health route (adding
  one would be an API change), so the page can be reachable a moment before the API answers.
  A first request that races startup returns an error and a reload works; nothing retries on
  the client's behalf.
- No Redis, no RabbitMQ, no provider. `GOPHERLEARN_AI_PROVIDER` stays `echo` and the quiz is
  driven by the scripted local model, so this stack exercises plumbing and not judgement.
- **Server-sent events are configured to pass through unbuffered, and that is what is deployed;
  it is not what has been proven on this host.** The live container's own `nginx -T` shows
  `proxy_buffering off`, `proxy_cache off`, `gzip off` and HTTP/1.1 on the stream route, and a
  proxied turn does arrive in several reads with the first bytes before the response finishes.
  A attempt to discriminate buffered from unbuffered delivery by timing produced no backpressure
  here - a client that paused mid-stream and shrank its receive buffer still let Nginx report
  `request_time` equal to `upstream_response_time` - so "the proxy holds the answer until the
  upstream finishes" was neither demonstrated nor ruled out by measurement.
- **Stopping the backend is graceful but bounded.** On `SIGINT`/`SIGTERM` the process stops
  accepting new connections first, then waits up to `shutdownGrace` (10s) for requests already
  running, so an open event stream reaches its terminal event instead of being cut; if the
  window expires it closes the rest and logs that the stop was forced. `stop_grace_period` in
  `compose.app.yml` (30s) is larger on purpose, so Docker's SIGKILL never races the drain - a
  test reads both numbers and fails if they are ever reordered. Measured with 24 parallel
  streams in flight: `docker stop` returned in 0.8s, every stream saw its terminal event, and
  the container exited `0`. What this does **not** buy: a request slower than the window is
  still cut, and there is no retry, because replaying a request whose outcome is unknown turns
  one possible success into two possible side effects.
- **Rotating `.env.compose.local` does not rotate the passwords inside an initialised MySQL
  data directory** - the users are created once, from the file that was present at first start.
  Point a new env file at an old volume and the backend simply cannot connect, which looks like
  a broken stack. Either change the users inside MySQL (`ALTER USER`) or start from a fresh
  round-specific volume, which is what the stack does.
- `.env.compose.local` is the only place the real values live (`*.local` is ignored). Secrets
  reach the containers through `env_file`, never through an interpolated `${VAR}` - a compose
  file that interpolated them would put them in every resolved configuration and every
  terminal that reads one. That separation is also why `docker compose config` has two modes:
  `config --no-env-resolution` shows the structure with no secret in it and is the one to
  paste into a report, while a plain `config` resolves the env file and prints the values, so
  it is the operator's own view and never an artifact. A missing variable is caught elsewhere:
  an absent env file is a hard compose error, a missing `MYSQL_ROOT_PASSWORD` makes the MySQL
  image refuse to start, and a missing JWT secret or DSN makes the backend exit with a named
  complaint.

## Configuration

Every variable the backend reads. Values below are examples; never commit a real
secret, and note that a provider key is only ever sent as an `Authorization`
header - it is not logged, not placed in a URL, and not written to any artifact.

### Server and authentication

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_JWT_SECRET` | signing secret; required, startup fails when unset | - |
| `GOPHERLEARN_JWT_ISSUER` | JWT issuer claim | `gopherlearn` |
| `GOPHERLEARN_HTTP_ADDR` | listen address, or port alone | `:8080` |

### Storage

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_STORAGE` | `memory` or `mysql`; anything else fails startup | `memory` |
| `GOPHERLEARN_MYSQL_DSN` | connection string, required when mode is `mysql`; never logged | - |
| `GOPHERLEARN_MYSQL_CONNECT_TIMEOUT` | Go duration bounding the connect attempt | `5s` |

Selecting `mysql` without a DSN stops the process rather than silently running on
volatile storage.

### Assistant model provider

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_AI_PROVIDER` | `echo` or `openai_compatible` | `echo` |
| `GOPHERLEARN_OPENAI_BASE_URL` | service root or full endpoint, e.g. `https://api.example.com/v1` | required in `openai_compatible` |
| `GOPHERLEARN_OPENAI_API_KEY` | bearer token, header only | required in `openai_compatible` |
| `GOPHERLEARN_OPENAI_MODEL` | model name sent in the request body | required in `openai_compatible` |
| `GOPHERLEARN_OPENAI_ENABLE_THINKING` | `true`/`false` provider extension; unset means the field is not sent at all | unset |
| `GOPHERLEARN_OPENAI_TEMPERATURE` | default sampling temperature, `0`-`2`; unset or `0` omits the field | unset |
| `GOPHERLEARN_OPENAI_MAX_OUTPUT_TOKENS` | default reply budget | `1024` |
| `GOPHERLEARN_OPENAI_TIMEOUT` | Go duration for the first byte and for the gap between streamed chunks | `30s` |

`openai_compatible` fails startup if any of the three required values is missing;
it never falls back to `echo` behind an operator's back.

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_LOCAL_LEARNING_MODEL` | local development switch: `deterministic` gives the **quiz workflow only** a scripted question-and-grade model, so a browser can drive the real workflow with no provider; any other non-empty value fails startup | unset (no scripted model) |

The switch is refused together with `GOPHERLEARN_AI_PROVIDER=openai_compatible` and
together with *any* `GOPHERLEARN_OPENAI_*` value left in the environment - even while the
mode is still `echo`. A process that names a provider must never answer a learner's quiz
from a script, and choosing silently in either direction would make the configuration
lie, so it stops starting instead. Conversations, knowledge-grounded answers, the learning
agent and session summaries are unaffected: with the switch on and no provider, they are
still the same deterministic placeholders `echo` has always used, and the startup line
says `local_learning_model=deterministic … NOT a provider`.

What that mode proves is the pipeline, not a judgement. Questions and grades come from a
script that cannot read an answer: the grade depends on the question, not on what the
learner typed, and both the question text and the feedback say they are scripted. See
"Learning workflow" below.

### Knowledge retrieval mode

A grounded turn retrieves evidence in one of two modes. The mode is a startup decision,
never a per-request one, and it changes only *how chunks are ranked*: the citations that
follow an answer are built by the server from the chunks it published, in both modes, and no
mode lets a model name a source it was not shown.

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_RETRIEVAL_MODE` | `deterministic` or `semantic_redis` | `deterministic` |
| `GOPHERLEARN_EMBEDDING_BASE_URL` | OpenAI-compatible embedding service root (`POST {base}/embeddings`) | required in `semantic_redis` |
| `GOPHERLEARN_EMBEDDING_MODEL` | model name sent with each batch | required in `semantic_redis` |
| `GOPHERLEARN_EMBEDDING_DIMENSIONS` | vector width; must match the model | required in `semantic_redis` |
| `GOPHERLEARN_REDIS_ADDRESS` | `host:port` or `redis://` / `rediss://` for the vector index | required in `semantic_redis` |
| `GOPHERLEARN_RERANK_ENDPOINT` | cross-encoder service root (`POST {root}/rerank`) | required in `semantic_redis` |
| `GOPHERLEARN_EMBEDDING_API_KEY` | bearer token, header only | unset (no auth) |
| `GOPHERLEARN_EMBEDDING_TIMEOUT` | Go duration per batch | `30s` |
| `GOPHERLEARN_EMBEDDING_BATCH_SIZE` | texts per request | `10` |
| `GOPHERLEARN_REDIS_PASSWORD` | `AUTH` for the index | unset |
| `GOPHERLEARN_REDIS_DATABASE` | database selector; **only `0` is accepted** | `0` |
| `GOPHERLEARN_REDIS_INDEX_NAME` | RediSearch index to create and query | `gopherlearn_knowledge_chunks_v1_idx` |
| `GOPHERLEARN_RERANK_AUTH_TOKEN` | bearer token, only if set; must be ≥ 32 bytes | unset |
| `GOPHERLEARN_VECTOR_MIN_SIMILARITY` | cosine cut for the vector stage, `(0, 1]`; **required in `semantic_redis`** | no default in that mode |

**`deterministic` is the default and behaves exactly as it did before a second mode existed.**
It opens no connection to Redis, sends no request to any embedding endpoint, calls no reranker,
and makes no outbound HTTP call of any kind while answering a grounded turn; the only network
it can use is the assistant provider you configured, and with `GOPHERLEARN_AI_PROVIDER=echo`
there is none. The store it assembles with is the store the process was handed, and the
retriever is the same `retrieval.Pipeline` this build has always built: BM25 plus a signed
feature-hash vector over the same tokens, fused by reciprocal rank. That vector stage is a
placeholder with real arithmetic behind it and no semantics at all, which is why
`VectorScorer` is cut at 0.25 rather than at 0 - see the comment on
`DefaultMinCosineSimilarity`, and the reason that number is not inherited below.

**`semantic_redis` is an explicit opt-in, and it is a real stack, not a re-labelled
placeholder**: an embedding client that calls an OpenAI-compatible
`POST {base}/embeddings` endpoint, a Redis/RediSearch vector index that answers cosine KNN
inside the caller's authorized collections, and a **BGE cross-encoder reranker** that
re-scores the fused head. Two names worth keeping straight, because they answer different
questions in an interview: the embedding model is this deployment's
**Qwen/DashScope-compatible endpoint at 1024 dimensions**, and **`BAAI/bge-reranker-v2-m3` is
the reranker** - a cross-encoder that scores a question-and-passage pair, so it cannot produce
a stored vector and nothing in this build should be described as a "BGE embedding". What the
mode does *not* replace is everything a client can observe: the same `Scorer` admission
contract, the same fusion function, the same top-k, the same abstention when no scorer admits
a chunk, and citations still built by the server from the chunks it published.

Two index rules are stated here because a real server, not the documentation, produced them.
**RediSearch creates its index in database 0 and nowhere else** (`Cannot create index on db !=
0`), so `GOPHERLEARN_REDIS_DATABASE` accepts only `0`, and isolation comes from the key
prefix, the index name and the authorization filter. **Hyphens in identifiers are escaped
according to RediSearch's TAG rules**: `@user_id:{user-1}` fails that query parser outright,
so every id this project generates - which is full of dashes - is escaped both when it is
written into a tag field and when it is used in a filter, while values that escaping cannot
make safe (the TAG separator `,`, SCAN pattern characters, or a `:` inside a key segment) are
refused at the write boundary with a named error.

**There is no fallback in either direction.** `semantic_redis` with a missing variable fails
startup and names every one that is missing - the embedding service root, model name and
width, the Redis address, the reranker endpoint and the cosine cut are all required.
`deterministic` with any semantic-only variable set also fails, naming it: an operator who
typed embedding settings clearly meant semantic, and starting anyway would keep retrieving by
surface-token overlap while their notes are believed to be read by a model.

One measured reason the cut has no default in this mode. Against a real 1024-dimensional
embedding service, with a four-section document and a question that shares nothing with any of
it, the placeholder's `0.25` admitted **all four** chunks as evidence; `0.75`, `0.85` and
`0.95` all brought it down to **one**, and the relevant question still cited the two sections
that actually answer it at every one of those values. The single remaining citation comes from
the lexical stage - BM25 admits any non-zero term overlap - and it is present in
`deterministic` mode too, so it is a pre-existing property of the ranking rules rather than
something semantic mode introduced. What the sweep does establish is that a cosine threshold is
a property of the model that produced the numbers, which is why the deployment names it and
this build refuses to guess.

Two constraints came from running against a real server rather than from reading docs:

* **The index lives in database 0.** `FT.CREATE` answers `Cannot create index on db != 0`
  for any other selection, so `GOPHERLEARN_REDIS_DATABASE` accepts only `0` and isolation
  comes from the key prefix, the index name and - for tests - a throwaway server.
* **A tag value cannot contain an unescaped dash.** `@user_id:{user-index-1}` fails the
  query parser; the escaped form is what matches. Since the identifiers here are all
  dash-shaped, this made the first real search fail outright. Values that cannot be
  represented safely at all - the TAG field's own `,` separator, or a SCAN pattern
  character such as `*` in an id, which would widen a delete across owners - are refused
  on the write path with a named error.

Every vector carries an **identity**: a digest of the embedding protocol, model name,
width and the chunker's version. The index will not answer a query whose identity differs
from the vectors it holds, so changing model or chunking costs an explicit re-index rather
than a ranking that quietly mixes two incompatible readings of the same document. The index
is a cache of a derived thing: MySQL holds the chunks, so `Synchronizer.Reindex` can rebuild
it from scratch and losing the volume loses no learner data. A failed write removes the
vectors it had just published, so a document that is not stored is never retrievable.

Startup also verifies the index rather than trusting its name: `EnsureIndex` runs one probe
query at the configured width, which turns "an index from another build is sitting at this
name" into a startup failure instead of a learner's question that answers nothing.

The cross-encoder is required in this mode and local-only by default: `GOPHERLEARN_RERANK_ENDPOINT`
must point at a loopback or docker-internal host unless the allowlist is widened deliberately,
because the alternative is quietly sending document text outward. It gets 5s (10s ceiling)
and its failure to *be reached* is tolerated - the turn is answered from the fused ranking -
while a malformed answer is not tolerated and fails the turn, because a second stage that
returns garbage would reorder real evidence. One measured deployment fact about that budget:
the same BGE model served on CPU re-ranked 3 short passages in about 3s, so a full 24-candidate
re-rank does not fit inside any timeout this client allows - the guard is right for a GPU
server and the fallback is what a CPU server will use.

The mode is decided in `cmd/gopherlearn` at startup: the process reads `GOPHERLEARN_RETRIEVAL_MODE`,
assembles the stack, creates or verifies the index before it serves, and logs the line
`retrieval=semantic_redis embedder=<model>:<width> index=<name>@<host>:<port>/db0
rerank=<model> vector_cut=<number> identity=<12 hex>`. No key, no password and no request
path appears in it. In `deterministic` mode the same line reads
`retrieval=deterministic embedder=deterministic-feature-hash-v1:256 index=none rerank=off`,
the store object is the one the process always used, and nothing opens a socket outside the
process.

Verification level, per dependency, because "wired up" and "actually executed" are different
claims and this file should not let them blur:

| Piece | Level reached |
|---|---|
| RediSearch index adapter | executed against a **real Redis Stack container** in a throwaway volume: index creation, KNN ordering with exact 1.0/0.0 similarities, owner isolation, vector-identity isolation, version sweep, document delete, end-to-end store decorator and re-index. Its own gated suite, which an ordinary `go test ./...` skips. |
| Embedding client | executed against the **real embedding endpoint** (a Qwen/DashScope-compatible service, 1024 dimensions) through the production client: vector count per batch, width, order, error shapes, batch splitting. |
| BGE reranker client | executed against a **locally served `BAAI/bge-reranker-v2-m3`**, through the production client: the relevant passage ranked first, score ordering, `/health`, refusal of malformed shapes, graceful degradation when the service is unreachable. |
| `semantic_redis` assembly | reachable **from `cmd/gopherlearn`** and driven end to end over HTTP: upload → chunks in MySQL → chunk vectors embedded and indexed → query embedded → KNN recall → BGE re-rank → server-built citation; then delete-then-unrecallable, and cross-user refusal. |
| Assistant chat provider | **not part of this verification.** The chain above was run with `GOPHERLEARN_AI_PROVIDER=echo`, so no chat completion was requested and the answers were extractive. A semantic-mode turn answered by a real provider, and retrieval *quality* against a judged corpus, remain open. |

To run the index this mode talks to:

```bash
docker compose -f compose.redis.yml up -d --wait
```

It publishes `127.0.0.1:6380` and holds nothing but re-derivable vectors. See
[Running the whole stack on Docker](#running-the-whole-stack-on-docker) for the application
stack, which does not include Redis.

### Conversation context

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_CONTEXT_MAX_MESSAGES` | most stored messages a model turn may see | `12` |
| `GOPHERLEARN_CONTEXT_MAX_RUNES` | rune ceiling on the chat and knowledge window | `6000` |
| `GOPHERLEARN_CONTEXT_WINDOW_TOKENS` | declared context capacity a learning-agent turn is checked against | `32768` |
| `GOPHERLEARN_CONTEXT_INPUT_TARGET_TOKENS` | ceiling on the estimated input of one agent request | `20000` |
| `GOPHERLEARN_CONTEXT_OUTPUT_RESERVE_TOKENS` | what the declared window keeps for the reply | `4000` |
| `GOPHERLEARN_CONTEXT_SUMMARY_MAX_TOKENS` | ceiling on the summary block injected into an agent turn | `2000` |
| `GOPHERLEARN_CONTEXT_HISTORY_MAX_TOKENS` | ceiling on the verbatim recent exchanges of an agent turn | `6000` |
| `GOPHERLEARN_CONTEXT_HISTORY_FETCH_LIMIT` | stored messages read as candidates before the budget chooses | `128` |
| `GOPHERLEARN_CONTEXT_EVIDENCE_MAX_TOKENS` | one whole agent turn's shared allowance for evidence bodies, across every tool call | `6500` |

Two different budgets, and the difference is the point. Chat and knowledge turns are
bounded by a message count and a rune count - approximate size guards, not a token
budget, and unchanged by the work below. A learning-agent turn is additionally bounded in
estimated tokens, measured over everything the request will carry: the system prompt, the
tool schemas, the current question, the injected summary, the kept exchanges, and every
tool observation the loop has already fed back.

Only completed question-and-answer exchanges enter a prompt; a question whose answer
failed stays in the transcript but cannot re-enter context. The learning agent's history
selection applies a message ceiling, a rune ceiling and the token ceiling together, and the
tightest of the three wins: carrying the two older limits into the new budget is what makes
a token ceiling able to only ever narrow a turn. It is not equivalent on its own - 6000
estimated tokens is roughly 24000 runes of Latin-script text - so a token-only window would
have grown the door it was introduced to measure.

Every figure is validated at startup and refused by name - an illegal value never becomes
a default, and `input target + output reserve` must fit inside the declared window, with
the provider's own output limit fitting inside the reserve. The declared window is a
budget this process holds itself to, **not** a measured model capacity: the estimator is
deterministic and local, the provider's usage report is a separate figure, and a cost this
server cannot price is recorded as unknown. See
[`spec/ARCHITECTURE.md`](../spec/ARCHITECTURE.md).

### Rolling session summary

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_SUMMARY_TRIGGER_MESSAGES` | uncovered complete messages needed before a refresh is due | `12` |
| `GOPHERLEARN_SUMMARY_TRIGGER_TOKENS` | estimated tokens of uncovered material that makes a refresh due on its own | `6000` |
| `GOPHERLEARN_SUMMARY_KEEP_RECENT_MESSAGES` | most recent messages always sent verbatim, never compressed | `8` |
| `GOPHERLEARN_SUMMARY_MAX_INPUT_TOKENS` | ceiling on one summarizing request, including the summary it is revising | `8000` |
| `GOPHERLEARN_SUMMARY_MAX_OUTPUT_TOKENS` | reply budget for the single summarization request | `2000` |
| `GOPHERLEARN_SUMMARY_TIMEOUT` | hard cap on the summarizing call | `20s` |
| `GOPHERLEARN_SUMMARY_STAGE_BUDGET` | most of one turn the refresh may spend | `8s` |
| `GOPHERLEARN_SUMMARY_FAILURE_COOLDOWN` | how long a session stops attempting a refresh after one failed | `30s` |

A session keeps one summary row that records where it stops: the id of the last
message folded into it. Every turn is then assembled the same way for all three
strategies - system prompt, sealed summary, the complete turns after the boundary
that fit the context budget, and the current question - so the summary and the
recent window never overlap and the question is never counted twice. When enough
uncovered messages have piled up past the boundary, the next turn compresses them
and advances the boundary and the version together; a concurrent writer that got
there first is adopted rather than overwritten, and the answer is not paid for
twice. Turns that failed - a question with no stored answer - belong to neither
the summary nor the window.

The migrated summary controls - the token threshold, the token input bound, the stage
budget and the failure cooldown - apply to `learning_agent` turns only. A chat or knowledge
turn is still triggered by message count, trimmed by messages and runes, given no stage
budget of its own, and left retrying after a failure exactly as it did before: adopting the
agent's summarising policy for every strategy would have changed when those turns pay for a
call, how long they wait, and whether a failed refresh silences the session afterwards.

The thresholds are two of them, and they answer different questions. Twelve uncovered
messages is a count this project can verify against stored rows; `GOPHERLEARN_SUMMARY_TRIGGER_TOKENS`
is an estimate over the same exchanges, so a session of four enormous answers can become
due for a refresh without ever reaching a message count. A token figure is an estimate
from `internal/contextwindow`, never a vendor tokenizer. The summarizing request is bounded
three ways - messages, runes, and estimated tokens over the whole payload *including the
previous summary it is revising* - because a bound on the new material alone lets a
long-running session's request grow every time it runs.

A refresh costs one extra model call on a non-streaming completion. That call is not
charged against the Agent Harness's call counts below: an agent turn may still spend 4
model calls on its own loop and pay one more for the summary. It does draw on the same
**time** budget, which is the point of the stage and deadline-share settings: a refresh
bounded only by its own timeout could consume most of the turn and leave the reply to
expire. In `echo` mode no summarizer is wired, so no summary is ever written and no
artificial one is invented: the context stays the recent window, which the startup line
says.

### Episodic memory (cross-session experience)

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_EPISODES` | `on` enables the whole subsystem; any other value, including absence, keeps every existing behaviour byte-identical | off |
| `GOPHERLEARN_EPISODE_QUIET_WINDOW` | debounce: how long a session's projection waits after a completed turn | `10m` |
| `GOPHERLEARN_EPISODE_SCAN_INTERVAL` | how often the worker scans for due jobs | `3s` |
| `GOPHERLEARN_EPISODE_RECONCILE_INTERVAL` | how often missed publishes are repaired by the gap scan | `1m` |
| `GOPHERLEARN_EPISODE_TASK_TIMEOUT` | budget for one projection (model calls included) | `30s` |
| `GOPHERLEARN_EPISODE_LEASE` | claim lease; must exceed the task timeout | `1m` |
| `GOPHERLEARN_EPISODE_RECOVERY_INTERVAL` | how often expired leases are reclaimed while running | `30s` |
| `GOPHERLEARN_EPISODE_RETRY_DELAY` | wait before the next attempt after a failure | `15s` |
| `GOPHERLEARN_EPISODE_MAX_ATTEMPTS` | projection attempts before a job is parked as failed | `3`, capped at `10` |
| `GOPHERLEARN_EPISODE_MAX_MESSAGES` | most complete messages one projection may read | `4096` |
| `GOPHERLEARN_EPISODE_MAX_INPUT_TOKENS` | estimate-based input budget per generator call | `12000` |
| `GOPHERLEARN_EPISODE_MAX_OUTPUT_TOKENS` | output budget for the projection tool call | `1800` |
| `GOPHERLEARN_EPISODE_WORKER_COUNT` | background projectors (1 keeps claims strictly ordered) | `1` |
| `GOPHERLEARN_EPISODE_QUEUE_SIZE` | in-process claim queue depth | `32` |

What `on` actually wires, end to end: when a turn's assistant message is stored, the turn
pipeline records one durable debounce row per session (`episode_projection_jobs`, unique per
owner and session, with a desired watermark that only advances). A background worker claims
due rows under a lease, reads the conversation from the store, asks the model to project it
into one structured card per session (synopsis, topics, and five assertion slots - decisions,
open loops, corrections, preferences, durable experiences - every entry citing the USER
messages it rests on), runs a second, independent verifier call that trims every assertion
the cited user messages do not entail, and saves the card under optimistic version control
(`episode_memories`, one row per session). A session that holds nothing durable is recorded
as an `abstained` row rather than an absence. The saved card is embedded into a retrieval
view that is separate from the knowledge index (`gopherlearn_memory_episodes_v1_idx` /
`gopherlearn:memory:episode:` in `semantic_redis` mode, a process-local view rebuilt from the
store at startup in `deterministic` mode). Recall runs the same fusion discipline as the
rest of the pipeline: exact-topic, identifier, lexical and vector channels fused with reciprocal
rank, where a vector-only candidate is never admitted and every candidate is re-read through
the store - owner, live session, active status, indexed view, and a per-version provenance
closure - before it can become evidence. Deleting a session removes its Episode rows and
projection jobs in the same repository transaction as the session delete, then best-effort
drops the derived view rows; every read re-checks the store, so an index-cleanup failure
costs recall, never correctness. The publisher is synchronous only for the durable job write
and uses an independent 2-second timeout; it is not accurate to describe publishing as
"completely non-blocking". Generation, verification and embedding are background work and
can add provider and embedding cost after the user turn has completed.

`on` requires `GOPHERLEARN_AI_PROVIDER=openai_compatible`: a placeholder model cannot project
a conversation, and a worker that only ever produces failures is refused at startup instead of
being run. Two agent tools come with it - `episode_search`, which recalls the caller's own
cross-session cards (never the current session's), and `read_history_message`, which reads
back the learner's own words behind a card's cited message ids - and they are installed
together or not at all. Read-back rights are granted in two server-side stages, and neither
stage trusts the model. Recall alone grants nothing: it returns candidate cards to the tool.
Only after the tool has rendered its bounded observation does it hand the server the exact
displayed slice, and the server grants exactly the intersection of that displayed set with the
stored card's current evidence closure, one message id at a time - a card clipped by the
card-count or length budget, an assertion cut short, or a partially rendered id grants
nothing, and an id the stored card no longer cites is not granted even if it was displayed.
A store read failure during that handoff fails the whole tool call - the observation is
returned as an error, the batch adds zero grants, and grants the same turn legally earned
earlier stay intact. The granting card is then re-verified in the store (id, version, active
status, indexed view, citation closure, watermark) again at the moment of each read, and only
a message that cleared every gate is registered as a citation. Re-projecting a card to a new
version invalidates the old version's read-back rights. Each recalled card's citation uses a
stable record identity, `src-episode-<episode id>-v<version>`, so repeated hits on the same
card version deduplicate and cards that share a title and date never collapse into one
citation. A new turn starts an empty ledger: no grant crosses a turn boundary. The retrieval
mode the episodic view uses follows the same `GOPHERLEARN_RETRIEVAL_MODE` decision as the
knowledge stack, and the deterministic view ranks by surface terms only - it exercises the
same rules, it does not claim semantic quality.

What the tests prove, and what they do not: the offline suite runs the whole chain on the
in-memory repository with a scripted model, the repository contract suites run on an embedded
engine, and separately gated tests run the same contract and engine-specific race scenarios
on a real MySQL server and the real Redis episode index. All of that is evidence about rules
and boundaries - authorization, versioning, leases, deletion, engine behavior. None of it is
evidence about projection quality with a real language model: no real-provider episode
projection or verification run has been executed, and no offline test should be read as
proving how well a real model summarizes or verifies conversations.

No RabbitMQ, TTL memory extraction, image recognition, TTS, retired semantic-memory
extraction path, or ordinary-Chat automatic long-term-memory injection was added.

### Learning agent

| Variable | Meaning | Default |
|---|---|---|
| `GOPHERLEARN_AGENT_MAX_MODEL_CALLS` | most times one agent turn may ask the model | `4`, capped at `12` |
| `GOPHERLEARN_AGENT_MAX_TOOL_CALLS` | most tools one agent turn may execute | `3`, capped at `12` |
| `GOPHERLEARN_AGENT_MAX_STEPS` | most rounds through the agent loop | `10`, capped at `24` |
| `GOPHERLEARN_AGENT_DEADLINE` | Go duration bounding the whole agent turn | `45s`, capped at `5m` |
| `GOPHERLEARN_CONTEXT_EVIDENCE_MAX_TOKENS` | one turn's shared allowance for evidence bodies across every tool call | `6500` |

In `openai_compatible` mode a `learning_agent` session runs an outer loop: the model
chooses whether to call one of the registered tools - `knowledge_search` over the caller's
documents, `memory_search` over the caller's confirmed long-term memories (Phase 5B.2),
`propose_memory`, which files a pending draft (Phase 5B.4), and, when
`GOPHERLEARN_EPISODES=on`, the episodic pair `episode_search` and `read_history_message`
(see "Episodic memory (cross-session experience)") - this server validates the tool
name and its arguments, executes it as the authenticated user, and feeds back a bounded,
labelled result. The two retrieval tools publish `query` and nothing else: the server
resolves the scope itself, so naming a collection, naming a memory, naming a topic, or
inventing a `user_id` is an undeclared field and ends the turn - nothing is quietly
narrowed, dropped or defaulted. `propose_memory` is a different kind of tool and declares
`scope`, `topic_key` and `content`; it records a candidate and decides nothing, because
what it stores stays `pending` until it is confirmed on the memory surface, its result
carries no evidence a citation could attach to, and pending drafts never reach recall - so a
proposal cannot round-trip back into the prompt that produced it. Memory recall is read-only
and filtered before it is rendered: only `active` memories of the authenticated user, only
inside the server's own authorization grant, only at or above the confidence floor (default
`0.6`, tighten-only - a request can raise it but never lower it, and there is no way to
switch it off), and never with the identifiers that would let a model point at a row it did
not read.
Exhausted budgets, an unknown tool and an invalid argument set are distinct failures, and
a provider that cannot carry tool calls stops startup rather than falling back to a
placeholder echo. In `echo` mode the agent is an explicit placeholder, which the startup
line states.

A search whose scope was narrowed by the cap says so in its result, and a citation is
attached only to a passage the model was actually shown: a chunk the length budget left
out is neither in the observation nor in `sources`. The same rule holds for memory - a
memory the tool did not render cannot be cited, and a read that found nothing says so in
terms of what was searched rather than claiming the user has no memories.

Two further bounds apply to a whole turn rather than to one call, because a per-call cap is
not a ceiling on the prompt. `GOPHERLEARN_CONTEXT_EVIDENCE_MAX_TOKENS` is one allowance for
the turn's evidence bodies *added together* across every tool call: a model that searches
three times cannot send three tools' worth of retrieved text. And before **each** model
call - not only the first - the loop estimates the request as it will actually go out,
including the tool schemas, the arguments, and every observation it already fed back, and
refuses to send one that exceeds the input target or whose estimate plus its output limit
would not fit the declared window. A refused call is never counted and never billed: the
turn stops with an internal error rather than paying to discover the limit, rather than
continuing on a truncated transcript, and rather than inventing an answer or an empty tool
result. The two accounts are deliberately separate: the evidence allowance counts what new
evidence text this turn introduced, while the per-call estimate counts everything still in
front of the model, old bodies included.

Repeated evidence is the case that makes the distinction worth stating. A second hit on the
same record - same kind, same namespace, same record id, same version, same displayed range
- keeps its citation and drops its body, and the observation says the text was already given
this turn. A different version or a different displayed range is a different unit and is not
merged. A passage omitted because the allowance ran out gets neither body nor citation, and
says which of the two reasons applied. Deduplication state belongs to one turn on one
request: it is not shared between users, sessions or concurrent turns, it is not persisted,
and it is not a cache.

An agent turn has two streaming shapes, and they differ only in how much the client is told
while the loop runs. A plain turn cannot know which kind of model call it is reading until the
call ends, so its text is held for the round and the SSE path emits the same content as the
synchronous turn, chunked after the fact. A turn the client asked to watch - `stream_steps:
true`, accepted on a Learning Agent turn and refused on a chat or knowledge one - publishes each
call's public text as its own block while the provider writes it, and the block's ending frame
says what it turned out to be: a note about a decision (`process`), the answer the server
intends to store (`answer_candidate`), or a round that broke (`failed`). So a model that says
"let me look that up" and then calls a tool keeps working, its sentence stays on screen as the
note it was, and the answer that follows is confirmed by address: `message.completed` carries
`final_block_id`, and the server refuses the turn rather than confirming text that is not that
block's bytes. No path asks the model an extra question to decide when to answer, and no path
sends the same characters twice - a watched turn has no `message.delta`. A provider stream that
stops early - no finish reason and no completion marker, or one the provider cut at its output
limit - is a prefix rather than an answer, so the adapter refuses it: nothing is stored, no
`message.completed` is sent, and the text already on screen closes as an unfinished block. A
session-summary refresh that happens to be due on an agent turn is a separate, earlier call and
is not charged against the four model calls above.

The agent is not the quiz either: it has no tool that generates an exercise, scores an
answer, or reads mastery back, and the tools it can be given remain `knowledge_search`,
`memory_search`, `propose_memory` and - only with the episodic switch on - the read-only
`episode_search`/`read_history_message` pair. The quiz is its own surface with
its own routes, shapes and state machine - see "Learning workflow (quiz surface)".

`memory_search` and `propose_memory` have never been run against a real provider. Both
sealed tool records predate them - each names `knowledge_search` and no other tool - so the
wire contract of every tool registered after it is tested against substitutes and a local
HTTP server, and nothing claims a vendor endpoint proved them.


---

## Provider smoke check

`cmd/gopherlearn-model-smoke` spent one authorization on 2026-09-25 and is not run
again without a new one. An authorized run spends exactly three provider requests - one
non-streaming completion, one streamed completion, and one grounded `knowledge_qa` turn
through the real `MessageService → retrieval → model generator` path - then asks an
unrelated question that must reach the provider zero times, and writes
`spec/verification/provider-smoke-v1.json`.

```bash
GOPHERLEARN_AI_PROVIDER=openai_compatible \
GOPHERLEARN_OPENAI_BASE_URL="https://api.example.com/v1" \
GOPHERLEARN_OPENAI_API_KEY="$YOUR_KEY" \
GOPHERLEARN_OPENAI_MODEL="model-name" \
go run ./cmd/gopherlearn-model-smoke -authorize model-smoke-3
```

It refuses before building a client, creating a directory, or sending anything when
the `-authorize` phrase is missing or mistyped, when the provider is not
`openai_compatible`, when base URL, API key or model is absent, or when the output
file already exists - a previous result is evidence and is never overwritten. The key
is read from the process environment only; no `.env` file is opened and no directory
is searched for credentials.

The report is `schema_version: 2`. It is written with `O_CREATE|O_EXCL`, so two runs
started at once cannot both record: the loser exits non-zero and leaves the older
bytes alone. A run that fails part-way is still archived - each step keeps its own
status, request count and counts already received, and a failed step records a null
answer digest and `nonempty: false` even when bytes had arrived before it broke. The
process then exits non-zero. Each of the three answers carries its own SHA-256, taken
over the bytes exactly as returned (emptiness is judged after trimming, the digest is
not).

The recorded file holds hashes, counts and machine-readable codes only: no prompt, no
answer text, no `Authorization` header, no key or derivative of it, and no error text
from the provider, which can echo a prompt back. On a failed execution the process
prints nothing but the closed-set failure codes, the numeric provider status and the
result file path.

## Agent tool-calling smoke check

`cmd/gopherlearn-agent-smoke` has been run twice, each under its own single-use
authorization, on 2026-09-25. Both records are sealed and read-only.

**v1** - `spec/verification/agent-tool-smoke-v1.json` (SHA-256
`3296df13a837da9acd4428d4cd6531a07b59aa12ed300b2f9172ec8e86ab785a`, 1283 bytes, schema
version 1): **failed after one of its two allowed requests, and it is still the evidence
that the vendor speaks native tool calling.** The endpoint received a `tools` offer and
answered with a structured `tool_calls` reply naming `knowledge_search` with a non-empty
call id, `failures=0`, `provider_status=0`, usage 642/111/753. The turn then ended because
this server refused the arguments; **which of our own rules fired cannot be recovered from
the v1 record**, whose code set reported a budget it had no evidence for. v1 is not
reinterpreted, corrected or overwritten.

**v2** - `spec/verification/agent-tool-smoke-v2.json` (SHA-256
`a3d5d1c6c42c7162fb9e7bd0a5a1a213dc7448e6cdf016052449abc48cd52048`, 1583 bytes, schema
version 2, run 2026-09-25): **the full round trip, clean.** 2 of 2 requests spent, 0
provider failures, 0 retries, empty `failure_codes`; one `knowledge_search` tool call whose
id was carried through into the second request as its `role=tool` result; the tool result
contained the caller's own discriminator and not the other account's; the final answer
stated that discriminator; one citation, belonging to the current user's temporary
document. Its argument shape: a single JSON object of 59 bytes, `query` present as a
26-character non-blank string, zero undeclared keys, no `collection_ids`.

The difference between the two runs is the parameter surface this server advertised: v1
offered `collection_ids` as well, and the same endpoint's call was refused. v2 narrowed the
published schema to `query` alone and recorded the shape of what arrived. That is a statement
about what changed between the two records, not a reconstruction of v1's cause, which v1's
evidence cannot name.

An authorized run spends exactly two provider requests. It seeds an in-memory user,
collection and one document carrying a unique marker, plus a second user holding a
document with the same marker and a different discriminator - `cycle-90d` for the caller,
`cycle-7d` for the other account - then asks a question whose answer is that discriminator,
through the real `MessageService -> agent.Harness -> knowledge_search -> retrieval` path.
The first response must be a structured `tool_calls` reply naming `knowledge_search`, the
server must run the search as the authenticated user, and the second response must answer
from the `role=tool` result. A model that answers directly, writes JSON in its prose, names
an unknown tool, invents an argument key, asks for more than one tool, produces no citation
or returns an empty answer fails the run - no retry, no second attempt, no fallback. So
does a run whose tool result did not reach the second request, whose tool result lacks the
caller's discriminator, whose tool result carries the other account's, or whose final answer
never states the discriminator it was given.

```bash
GOPHERLEARN_AI_PROVIDER=openai_compatible GOPHERLEARN_OPENAI_BASE_URL="https://api.example.com/v1" GOPHERLEARN_OPENAI_API_KEY="$YOUR_KEY" GOPHERLEARN_OPENAI_MODEL="model-name" go run ./cmd/gopherlearn-agent-smoke -authorize agent-tool-smoke-3
```

The record goes to `spec/verification/agent-tool-smoke-v2.json`, created with
`O_CREATE|O_EXCL`; the v1 record and the provider record are never opened for writing, and
the suite's `TestMain` guard checks all three before and after every run. It states
structural facts only: counts, closed-set codes, the observed tool name, whether the call id
survived the round trip, four attribution booleans, citation count and ownership, the
answer's SHA-256 over the bytes as stored, how many times the tool's `Execute` was actually
entered, whether a result was produced, whether that result reached the second request, and
the shape of the arguments (how many calls, whether the payload was one object, how long the
query was in characters, how many undeclared keys, whether a scope was named). Reason codes
name which of this server's own rules refused a call - `arguments_unknown_field`,
`arguments_not_object`, `foreign_collection_in_scope`, `tool_call_budget` and the rest -
because that is a fact about our parser, not vendor text. Never recorded: the question, the
answer, the tool observation, the discriminators, any argument value or key name, any
collection id, the key, the `Authorization` header, the full endpoint URL.

### What the tool-calling evidence does not establish

Both runs went against **one** OpenAI-compatible endpoint, on one day, with one model and
one question. From them it does **not** follow that:

- any other provider or model is tool-call compatible - this is a single endpoint's answer,
  not a conformance result;
- answer quality has been evaluated in any systematic way. The record asserts attribution -
  the caller's own fact reached the answer - not correctness, helpfulness or pedagogy;
- the streamed agent path works against a vendor. Neither record was taken with `stream_steps`,
  so neither shows a real provider's text blocks being published as they were produced; the
  blocking provider in `internal/agent/stream_test.go` and the real-loop test in
  `internal/service/agent_stream_regression_test.go` are offline evidence about our own loop;
- more than one tool, several tool rounds in one turn, or a bounded recovery after a
  refused call has been demonstrated against a provider. Each record is one tool round
  trip against one endpoint. All three are implemented and covered offline, which is a
  different claim from "proved against a vendor";
- a trained embedding or a Redis full-text index exists in this build. Neither is
  implemented: recall is lexical plus a deterministic pseudo-embedding over the
  in-process store, and nothing here suggests a vector index is present.

Two entries an earlier draft of this list declared unimplemented have since been built, and
saying so is the point of keeping the list honest. Long-term memory now has storage, a
confirmation gate the model cannot pass, one bounded read tool and one controlled write tool.
Exercise generation exists as the quiz surface above, with its own routes, state machine and
topic projection. Neither has been run against a real provider, and the local scripted model
that drives a browser through the flow proves the pipeline rather than any judgement.


> 三个 Go 模块与前端包的**验证命令**见 [INTEGRATED-REFERENCE.md](INTEGRATED-REFERENCE.md#verification)。
