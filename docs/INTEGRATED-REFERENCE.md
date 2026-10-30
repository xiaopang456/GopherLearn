# 正式入口技术参考（INTEGRATED REFERENCE）

本文件是**本项目正式应用**（`hybrid/cmd/gopherlearn-integrated`）的技术参考：
学习／测验（quiz）接口合同、以及三个 Go 模块与前端包的验证命令。

面向新用户的安装、配置与启动步骤在仓库根目录的 [README.md](../README.md)；
历史基线入口的开发说明在 [LEGACY-DEVELOPMENT-REFERENCE.md](LEGACY-DEVELOPMENT-REFERENCE.md)。

## Learning workflow (quiz)

The quiz is a separate surface from the agent: six bearer-authenticated routes, no tool call
and no chat turn involved. It is still bound to the agent's *session mode* - a quiz starts only
on a `learning_agent` conversation.

| Method + path | Success | Body / query |
|---|---|---|
| `POST /api/v1/learning/sessions?session_id={id}` | `201` | `{"topic": "...", "collection_ids": []}` - `{id}` must name a `learning_agent` conversation; the caller's own `chat` one is a `400 invalid_request` that leaves nothing behind |
| `GET /api/v1/learning/sessions?session_id={id}` | `200` | query only - `data: null` when this conversation holds no open quiz |
| `POST /api/v1/learning/sessions/{quiz_id}/answer` | `200` | `{"submission_id": "...", "ordinal": 1, "answer": "..."}` |
| `POST /api/v1/learning/sessions/{quiz_id}/skip` | `200` | `{"submission_id": "..."}` |
| `POST /api/v1/learning/sessions/{quiz_id}/cancel` | `200` | no body (an empty body or `{}`; anything else is 400) |
| `GET /api/v1/learning/progress?topic={text}` | `200` | query only |

Every answer uses the standard `{"data": ..., "error": ...}` envelope. Resume is the one place
where an absence is a success: a conversation the caller owns that holds no open quiz answers
`200` with `{"data":null,"error":null}`, because finishing or cancelling a quiz and then
reloading is the ordinary path through this feature - while a conversation that is not the
caller's, and one that was never stored, both stay `404 not_found` and indistinguishable, so
nothing here can be used to probe what exists and an idle conversation is never reported as a
server error. A `GET /api/v1/learning/progress` for a topic with no stored attempt is the same
kind of absence and gets the same answer: `404`, never a projection of zeros, because `0` would
be a measurement of a learner who has made none. The five quiz routes
return the same object: `id`, `session_id`, `topic`, `status`, `current_question`,
`total_questions`, `question` (`{ordinal, type, question}`, omitted when the quiz is terminal)
and `expires_at` - and no grade, score or projection field. `GET /learning/progress` returns
`topic_key`, `topic`, `mastery`, `confidence`, `valid_attempts`, `level`, `weak_points` and
`last_attempt`. Full field semantics, the ten refusal codes (`learning_session_active`,
`learning_answer_busy`, `learning_submission_conflict`, `learning_session_closed`,
`learning_no_evidence`, `learning_question_order`, `learning_generation_failed`,
`learning_grading_failed`, `learning_grading_invalid`, `learning_unavailable`, plus
`invalid_request`/`unauthorized`/`not_found`/`internal_error`) and the seven-state machine
(`created -> asking -> waiting_answer -> grading -> completed`, with `cancelled`, `expired`, and
the two recovery edges `asking -> created` and `grading -> waiting_answer`) are in
`spec/API.md`; the transition table itself is enforced by the repository, not by the handler.

Nothing here is configured by the environment. Question count (3), quiz lifetime (`24h`), the
recency window (10 valid attempts), the reliability gate (3 attempts before a level is anything
but `insufficient`), the mastered bar (`mastery >= 85` with `confidence >= 0.5`) and the
weak-point cap (8) are constants in `internal/domain`, so a deployment cannot quietly change what
a quiz is - and a projection read after such a change would describe a different measurement.


---

## Verification

The repository holds three Go modules and one npm package. Each is verified
separately, and none of them needs Docker, a database or the network for a
default run.

```bash
# root module - the baseline entry
gofmt -l cmd internal conversation
go vet ./cmd/... ./internal/... ./conversation/...
go test -count=1 ./cmd/... ./internal/... ./conversation/...

# v2 module - the application kernel
cd v2
gofmt -l .
go vet ./...
go test -count=1 ./...

# hybrid module - the integrated entry
cd ../hybrid
gofmt -l .
go vet ./...
go test -count=1 ./...

# web client
cd ../web
npm ci
npm run lint
npm run test
npm run build
```

`go test -count=1 -race ./internal/...` (root module) is the slower variant of
the same suites.

Those commands need no Docker, no database and no network: the SQL store is exercised
through an embedded engine. The same behavioural suites also run against a real MySQL
server, and that path is opt-in by environment variable so a default run can never fail
for want of a container:

```bash
GOPHERLEARN_MYSQL_INTEGRATION_DSN="root:<password>@tcp(127.0.0.1:3308)/mysql" \
go test -count=1 ./internal/repository/gormmysql/ -run TestRealMySQLContract -v
```

Without that variable the test skips with a reason. With it, the test creates a throwaway
schema, migrates it through the production path, runs the shared repository, memory,
learning and knowledge suites against MySQL, and drops the schema - which is where the
engine-specific rules get checked rather than assumed: that one open quiz per conversation
is enforced by `uniq_learning_open` and not by caller politeness, that a nullable key lets a
finished quiz release its conversation, that a stale summary commit is refused without
touching the row, and that four-byte characters survive the round trip.

The HTTP contract is a golden file: `internal/httpapi/testdata/http_contract.golden` holds
one recorded request/response pair per contract row. Status, headers and body are compared
exactly, apart from what legitimately varies per run - issued tokens, clock-derived ids and
deadlines, which the canonicalizer rewrites by shape. A response that gains or loses a field
therefore fails the suite, and that is how "no score and no projection in a quiz response"
stays true. `-update-golden` rewrites the file on purpose, so a diff there is a contract change
to read line by line, not noise to regenerate away; no row count is quoted in prose, because
rows are appended by the surface they describe and a stale count is worse than none.

The tests need no database, broker, container, or network: storage behaviour runs
against an in-memory repository and a pure-Go SQLite file, and the provider client is
exercised only against a local test HTTP server. The suite also cannot touch the real
smoke record: `cmd/gopherlearn-model-smoke` compares that path before and after every
run, so a record that was absent must not appear and one that was present must come out
byte-identical. One live call has since been made, on 2026-09-25, against one
OpenAI-compatible endpoint - enough to prove this client's wire format works there, and
not enough to claim compatibility with any other vendor.
