# GopherLearn clean implementation contract

This directory defines behavior for an independent implementation. Developers
must not read or copy source code, UI components, documentation text, assets,
or configuration from `GopherAI-v1`, `GopherAI-v2`, or
`GopherLearn-open-source`.

## Product boundary

GopherLearn exposes two user-facing conversation modes on one shared platform:

1. **Chat (`chat`)** for general conversation. Each user turn may enable
   knowledge-base grounding and select one or more knowledge collections.
2. **Learning Agent (`learning_agent`)** for tool use, layered memory,
   exercises, mastery updates, and personalized explanations.

The Chat mode contains two internal answer strategies: direct completion and
knowledge-grounded completion. Knowledge Q&A is therefore a per-turn Chat
capability, not a third top-level session mode.

## Runtime architecture

```text
Vue web client
  -> HTTP/SSE API
  -> application services
  -> repositories
  -> MySQL (business facts) / Redis (rebuildable retrieval index, opt-in mode only)

Agent Harness
  -> construct bounded context
  -> call model
  -> validate tool call against server-owned capability set
  -> execute tool runtime
  -> append observation
  -> continue, wait for confirmation, or finish
```

## Shared conversation foundation

All three modes share the following infrastructure:

- authentication, ownership checks, session and message repositories;
- MySQL as the authoritative store for user messages, final assistant
  messages, and the citations attached to a grounded assistant message;
- one SSE event protocol, cancellation model, idempotency policy, and error
  envelope;
- recent messages plus a rolling session summary as the base context;
- one outer conversation runner responsible for context budgets, model-call
  budgets, persistence, observability, and finalization.

RabbitMQ is not the durability boundary for the core chat transcript. A user
message must not be acknowledged merely because it was published to a queue.
Queues may later carry derived or retryable background work such as document
parsing, indexing, embedding, summary refresh, memory-candidate extraction,
learning analytics, and notifications. If core writes are ever published as
events, use a transactional outbox after the MySQL transaction commits.

## Mode and answer strategies

| Session mode | Per-turn option | Internal strategy | Context and capabilities |
|---|---|---|---|
| `chat` | knowledge disabled | `direct_chat` | recent messages + rolling summary; one model completion |
| `chat` | knowledge enabled | `knowledge_qa` | base context + retrieve/rerank + knowledge evidence and citations; one model completion |
| `learning_agent` | not controlled by the Chat knowledge toggle | `agent` | bounded Agent Harness with authorized global/topic/episodic memory, knowledge and learning tools, and confirmation gates |

Direct modes must not be forced through ReAct merely to share code. They share
the outer runner and persistence lifecycle while each strategy owns only the
work required to produce its answer.

The `learning_agent` row is the design target. What runs today is the loop in
"Agent Harness as implemented": three server-owned tools, and they are not three of the
same kind - two read-only retrieval tools, `knowledge_search` (authorized knowledge
evidence) and `memory_search` (only the caller's confirmed, granted long-term memories),
plus one controlled write, `propose_memory`, which files a *pending* draft. The write tool
is not a retrieval tool: its observation to the model is a status sentence, it yields no
evidence a citation could attach to, and what it stored stays pending until the learner
confirms it on the memory surface, so a proposal cannot re-enter the prompt that made it.
There is still no tool that generates or scores an exercise, and no confirmation happens
inside the loop - the gate is the user's, outside it. Long-term memory remains reachable
only through these tools rather than as ambient context. The rest of the gap is part of the
contract.

The session mode is a server-owned, immutable session fact. Changing between
Chat and Learning Agent creates a new session. Within a Chat session, knowledge
grounding is a per-turn option; the server records the effective option,
selected collection IDs, retrieval evidence, and resulting answer strategy so
the history remains auditable.

## Knowledge retrieval as implemented

Retrieval has two modes and one assembly point. The mode is a startup decision, never a
per-request one, and it changes only how chunks are scored and admitted - what is fused, what
is authorized and what a citation is made of stays the same in both.

```text
GOPHERLEARN_RETRIEVAL_MODE
  deterministic  (default)  -> BM25 + deterministic feature-hash cosine -> fuse
  semantic_redis (opt-in)   -> BM25 + real embedding vectors recalled from RediSearch
                               -> fuse -> BGE cross-encoder re-scores the fused head
                                    |
                          both paths: top-k -> evidence + server-built citations
```

- **`deterministic` is what every existing deployment runs, unchanged.** It opens no
  connection to Redis, contacts no embedding endpoint, calls no reranker, and issues no HTTP
  request of any kind while answering: the store it returns is the store it was handed, and
  the retriever it builds is the same `retrieval.Pipeline` this process built before either
  mode existed. Startup output, defaults, scoring, admission, fusion, top-k and citation
  behaviour are the ones the golden HTTP contract records.
- **`semantic_redis` is explicit opt-in and fails closed.** It assembles a real embedding
  client, a RediSearch-backed vector index, a store decorator that writes a vector beside
  every chunk, and a BGE cross-encoder second stage. If any required piece is missing - the
  embedding service root, model name or width, the index address, the reranker endpoint, or
  the cosine cut - the process refuses to start and names every variable that is missing.
  Setting a semantic-only variable while the mode is still `deterministic` also refuses to
  start, so a half-typed configuration cannot leave a deployment ranking by surface-token
  overlap while an operator believes notes are being read by a model. There is no fallback in
  either direction.
- **The embedder is the original project's Qwen/DashScope-compatible embedding endpoint** - an
  OpenAI-compatible `POST {base}/embeddings` call, 1024 dimensions. **BGE
  (`BAAI/bge-reranker-v2-m3`) is the reranker, not the embedder**: it is a cross-encoder that
  scores a question-and-passage pair, so it cannot produce a stored vector, and nothing in
  this build claims a "BGE embedding".
- **Storage.** Collections, documents and chunks live in the same MySQL database
  as the conversation, through the same transactional repository boundary. A
  document rewrite advances its version and retires the previous chunks in one
  transaction; deleting a document removes its chunks with it, so nothing a user
  deleted can be retrieved again. In `semantic_redis` the vector index is written
  beside those rows and is treated as a cache of a derivable thing: losing it costs a
  re-index, never a learner's data, and a write whose vectors could not be published
  removes what it had already stored so an unstored document is never retrievable.
- **Two index constraints that only a real server could teach.** RediSearch creates its index
  in **database 0 and nowhere else** (`Cannot create index on db != 0`), so
  `GOPHERLEARN_REDIS_DATABASE` accepts only `0` and isolation comes from the key prefix, the
  index name and the authorization filter - not from a database number. And a hyphen inside a
  tag literal is query syntax, not text: `@user_id:{user-1}` fails the parser, so every
  identifier this project generates - which is full of dashes - is escaped on the way in and
  on the way out. Values that escaping cannot make safe are refused at the write boundary
  with a named error instead: the TAG field's own `,` separator, which would store one value
  as two tags and let another user's query match it; and `*?[]\` or a `:` in a key segment,
  which would widen a delete or shift the version a sweep keeps.
- **Scoring.** Okapi BM25 over a latin/CJK-bigram tokenizer, plus cosine
  similarity over a deterministic feature-hashing embedding. The embedder is a
  placeholder that shares surface tokens and no semantics; replacing it with a
  trained model means implementing one interface, and the fusion code does not
  change. `semantic_redis` is that replacement exercised: the placeholder's cosine
  floor of 0.25 was measured to admit an entire document for a question unrelated to
  it once a real model supplied the vectors, which is why the cut is a required
  setting in that mode rather than a default carried over from a different
  embedder's scale.
- **Admission belongs to the scorer, fusion to the pipeline.** Each scorer
  declares, in its own units, where evidence begins: BM25 requires a shared term
  (its zero point), the vector scorer requires a cosine above its configured floor
  (chosen conservatively for the placeholder), and non-finite scores are rejected
  outright. Fusion then reads positions only. The pipeline deliberately holds no
  shared score threshold, because one number cannot fit both an absolute term
  weight and an angle. Both modes call the same fusion function, so the difference
  between them is admitted candidates, not merge arithmetic.
- **Answering.** Under the default `echo` provider this is extractive, not
  generated: passages are quoted with their section path and no provider is
  contacted. Under `openai_compatible` the passages are handed to a chat model,
  which writes the answer. In `deterministic` mode there is no second stage at all;
  in `semantic_redis` the fused head is re-scored by the BGE cross-encoder, the
  reordering is recorded per result, and a reranker that cannot be reached leaves
  the fused order in place and says so - a second stage being down is an
  availability event, not a reason to fail a learner's turn. A malformed answer is
  not tolerated, because garbage from the last stage would reorder evidence the
  first two stages had agreed on.
- **Empty results are honest.** When nothing is admitted, the turn says the
  knowledge base held nothing relevant and attaches no citations, rather than
  citing the head of a ranking that was rejected — and it says so without asking a
  model, so a provider outage cannot hide the difference between "not found" and
  "not reachable".
- **Verification level, stated per dependency.** The index adapter has been exercised against
  a real Redis Stack container (create, KNN ordering, owner and identity isolation, version
  sweep, delete, re-index) from a gated suite that an ordinary `go test ./...` skips. The
  embedding client has been exercised against the real endpoint, and the reranker client
  against a locally served BGE model, in both cases through the production client code rather
  than a script. `cmd/gopherlearn` assembles and serves `semantic_redis`, and the whole chain
  - upload, chunk rows in MySQL, vectors in the index, query embedding, KNN recall, rerank,
  server-built citation, delete-then-forget, cross-user refusal - was driven over HTTP with
  the provider left on `echo`. **What that does not cover: a chat-provider turn in semantic
  mode.** The assistant model was never contacted during this work, so answer quality,
  provider-side behaviour and any vendor other than the endpoints configured here remain
  unverified; a real embedding service is also not something this repository starts, only
  something it calls.

## Assistant model seam

Everything that generates prose goes through one SDK-free interface,
`model.ChatModel`: a closed message-role set, a completion request with generation
knobs, a completion with a normalized finish reason, and a streaming call that
hands over provider deltas as they arrive. Only the adapter in `internal/openai`
knows the wire format, and nothing above it can learn which provider is installed.

- **Modes.** `echo` (the default) wires no model at all: placeholder answers and the
  extractive knowledge generator, which is what the HTTP contract golden measures, and a
  learning agent that is labelled a placeholder in its startup line. It wires no
  summarizer either, so no fake AI summary is manufactured to look like a real one: the
  context an echo turn is handed stays the recent window, and the startup line says
  summaries are disabled.
  The one thing that can be added inside `echo` is the scripted quiz model behind
  `GOPHERLEARN_LOCAL_LEARNING_MODEL=deterministic`, and it is scoped to the quiz workflow
  alone: answers, grounding, the agent and summaries stay placeholders, the startup line
  names the switch and says `NOT a provider`, and the configuration refuses the switch
  together with `openai_compatible` or with any `GOPHERLEARN_OPENAI_*` value left in the
  environment. A script must never stand in for a provider an operator believes is live.
  `openai_compatible` wires one shared chat model into `direct_chat` and
  `knowledge_qa` and runs `learning_agent` through the Agent Harness below. A model that
  cannot carry tool calls makes startup fail: keeping the agent on echo while the
  configuration claims a real provider is the silent downgrade this layer exists to
  refuse.
- **Prompt assembly.** One function lays out all three strategies' prompts: system policy,
  then the sealed rolling summary, then the complete turns it does not cover, then the
  current question. Chat, knowledge-grounded Chat and the Agent Harness call that function;
  there is no second copy of the layout to drift. Derived context is placed in a labelled
  user turn, never in a second system message, because it is material to review rather than
  a rule to enforce - the same reason evidence sits in the user turn. Retrieved passages are
  numbered; identifiers are withheld so the model cannot invent a citation.
- **Context window.** The service reads the stored summary and history under the session
  lock, before this turn's question is stored, so the question reaches the model exactly
  once. The window contains completed `user → assistant` exchanges only: a turn whose model
  call or store failed stays in the transcript as fact but cannot re-enter context, and
  cannot be folded into a later summary either.
  Chat and knowledge turns are bounded by a message count plus a rune count - a rough size
  guard, deliberately **not** a token-accurate budget. A learning-agent turn is bounded by
  all three of a message count, a rune count and an estimated token ceiling, applied
  together in **one** selection: the service hands over candidate exchanges and
  `internal/contextwindow` assembles the prompt, so one transcript is never trimmed twice
  by two owners measuring in two units. The two older ceilings are carried into the new
  budget rather than dropped, because the token figure alone is not equivalent to the rune
  figure it replaced: six thousand estimated tokens is roughly twenty-four thousand runes
  of Latin-script text, so a token-only window would have widened the door it was
  introduced to measure. The tightest of the three always wins, which is what makes adding
  a token dimension able to only ever narrow a turn.
  The estimate is deterministic and local, and it prices what a request actually carries:
  system policy, tool schemas, the current question, the injected summary, the kept
  exchanges, and every tool observation the loop already fed back. It is not a vendor
  tokenizer; the provider's usage report is recorded separately, and a cost that cannot be
  priced is recorded as unknown. See "Rolling session summary" for what covers the
  messages above that window.
- **Per-call check, and what a refusal costs.** Before every model call of an agent turn -
  not only the first - the loop measures the request as it will go out and refuses one whose
  estimate exceeds the input target, or whose estimate plus its effective output limit would
  not fit the declared window. A refused call is not counted against the call budget and not
  billed. The turn ends with an internal error; nothing is sent half-trimmed, no answer is
  invented to close a tool round, and no empty tool result stands in for one that was never
  run. Required parts that cannot fit are refused by a distinct error rather than by dropping
  the learner's question.
- **Budgets and failures.** Generation settings resolve request → configuration →
  provider default. There are no retries. The configured timeout bounds the first
  byte and the gap between stream chunks, never the total length of a healthy
  stream. Caller cancellation, provider timeout, provider error, and an empty answer
  are four distinct outcomes with four distinct codes; a stream that produced text
  and then failed is discarded rather than stored half-written.
- **Live coverage is two authorized runs deep, on one endpoint.** The first, on
  2026-09-25, spent three of three requests with zero failures and zero retries against one
  OpenAI-compatible endpoint and is sealed at `spec/verification/provider-smoke-v1.json`:
  connectivity and call-chain proof - a non-streaming completion, a streamed one, and the
  grounded `knowledge_qa` path. It predates tool calling. The second is
  `cmd/gopherlearn-agent-smoke`, run twice the same day under separate authorizations:
  v1 (`…agent-tool-smoke-v1.json`, `3296df13a837da9a…`) failed after one of two requests -
  the endpoint did answer a `tools` offer with a structured `tool_calls` reply naming
  `knowledge_search`, and this server refused the arguments, for a reason v1's code set
  cannot name - and v2 (`…agent-tool-smoke-v2.json`, schema version 2,
  `a3d5d1c6c42c7162fb9e7bd0a5a1a213dc7448e6cdf016052449abc48cd52048`, run 2026-09-25)
  completed the whole turn: 2/2 requests, 0 provider failures, 0 retries, one executed
  `knowledge_search`, the call id carried into the second request as its `role=tool`
  result, the caller's own discriminator present in that result and the other account's
  absent, the answer repeating the discriminator, and one citation owned by the current
  user's temporary document. That run attests the tool round trip and nothing else: answer
  quality, any other vendor, a trained embedding and the vector index were outside its scope,
  and each of those has its own statement of what has since been executed in "Knowledge
  retrieval as implemented" above.
- **Not built yet.** Usage and finish reasons are not persisted and there is no call ledger
  or cost guard. No queue is used at all. Redis is used only by the opt-in `semantic_redis`
  retrieval mode; the default mode still answers from the deterministic feature-hash
  placeholder, and no deployment gets that index by accident.

## Agent Harness as implemented

The loop above the model is implemented for three capabilities. What the diagram in "Runtime
architecture" describes as a general runner is, today, this much and no more:

```text
authenticated turn (learning_agent, openai_compatible)
  -> ask the model with the server's tool definitions
  -> no structured tool call? that text is the answer
  -> validate name against the registry, and arguments against the tool's own schema
  -> execute as the authenticated principal, never as a principal from the arguments
  -> append a bounded, labelled observation and ask again
  -> stop on an answer, or on the model-call, tool-call, step or time budget
```

- **Only a structured call is a call.** Tool intent comes from the provider's
  `tool_calls` field. A model that prints plausible JSON in its answer text has written
  text: nothing runs. That rule is what keeps a document a user uploaded - which can
  contain a JSON blob that reads like a request - from becoming an instruction. The same
  strictness runs the other way: a call with no name, no id, one id used twice, or a
  type other than `function` is a malformed response and ends the turn. Nothing is
  synthesized to make a broken vendor answer legal, because a repaired call would let the
  loop execute a request the model never validly made.
- **Tool output is data.** Each observation is labelled as such and framed inside the
  transcript, and the system policy states that anything in it demanding a different
  tool, a rule change or a leaked prompt is material to read, not to obey. The
  registry is the enforcement: a model that follows an injected instruction and asks for
  `delete_everything` is refused because no such tool was installed.
- **Three tools, and only what each needs.** `knowledge_search` and `memory_search` declare
  only `query`; `propose_memory` declares `scope`, `topic_key` and `content`. The model is
  never shown a collection identifier, a memory id, a session id or a message id, and
  there is no list-collections or list-memories capability, so a scope argument could only
  ever be a guess; naming one is an undeclared key and the whole call is refused. Each server resolves its own scope - knowledge over the caller's readable
  collections, bounded, then through the same readable gate a grounded turn passes; memory
  inside the grant this server produced - and a refusal is recorded under the name of the
  rule that fired, never with the payload. A memory read that returns nothing says what was
  searched, and one that fails ends the turn: "could not read" must never be delivered to
  the model as "you have nothing".
- **Budgets are checked before they are spent.** A reported count is work that
  happened, never an attempt that was refused. The turn deadline is kept distinct from
  a caller cancellation, so "the loop ran too long" and "the client hung up" cannot be
  confused in a report.
- **One evidence allowance per turn, and per-call caps on top of it.** Each tool bounds its
  own observation, and that is not a ceiling on the prompt: a model that calls three tools
  under three independent caps sends three caps' worth of retrieved text. A turn therefore
  carries one shared allowance (`GOPHERLEARN_CONTEXT_EVIDENCE_MAX_TOKENS`) that every
  evidence-producing call spends from, admitted a whole unit at a time. The two limits are
  reported separately in the observation - "left out because this call was long" is a
  different fact from "left out because this turn has spent its evidence", and a model told
  only the first will search again in a turn that cannot answer.
  The allowance belongs to one turn on one request: created on the run's own stack, dropped
  with it, never shared between users, sessions or concurrent turns, and never persisted.
  It is not a cache and not a recovery mechanism, and a capability that does not spend from
  it is still bounded by the per-call estimate taken before the next model call.
- **Repeated evidence keeps its citation and loses its body.** A second hit on the same
  displayable unit - same kind, same namespace, same record id, same version, same displayed
  range - is acknowledged without resending the text, and the observation says the body was
  already given this turn. The key carries version and displayed range precisely so a
  different revision or a longer excerpt cannot be merged into one that was already sent;
  the citation that survives was earned by text that did reach the model, earlier in the
  same turn. A snippet never describes a wider range than the one displayed.
- **Citations are only for retrieved text the model was shown.** Retrieval may admit more
  chunks than the observation budget can carry; whatever did not reach the text is in
  neither the observation nor `sources`, so a citation can never point at material the
  answer was not built from. A scope narrowed by the collection cap is stated in the
  result, which keeps "not found here" from being reported as "not found anywhere".
- **A watched agent turn publishes text per model call; an unwatched one cannot.** A loop
  does not know which kind of call it is reading until the call has ended, so a plain SSE turn
  runs the loop and re-chunks the finished answer, and every round's text is held until then —
  which is also why a model that narrates before calling a tool cannot break that turn. A turn
  the client asked to watch (`stream_steps: true`) publishes each call's text as its own block
  the moment the provider writes it, and the block's ending frame names what it turned out to
  be: a note about a decision, a candidate answer, or a round that broke. The answer a learner
  is shown is then confirmed by address, not by resemblance: `message.completed` carries
  `final_block_id`, and the service refuses the turn if that block's fragments are not the
  answer it is about to store, or if the stored row differs from them. Both paths produce the
  same final content as the synchronous turn, and neither asks the model an extra question to
  announce that an answer is ready.
- **A prefix is decided in the adapter, not downstream.** Both stream paths ask the same
  question of a finished read loop - did the provider name a finish reason, or send the
  protocol's completion marker, or stop at its output limit? - and answer it with a failure
  whenever the stream just ended. That decision lives with the one layer that can see the wire,
  because a truncated turn is dangerous in two different ways: half a sentence becomes a stored
  assistant message, and half a tool call becomes arguments the model never finished writing.
  A strategy that had to re-derive it would need to know provider details the layer below was
  built to hide.
- **Offline coverage plus one live endpoint.** Every case above is covered with a
  substitute model and a local test server, and the tool-calling path has additionally been
  executed twice against one real OpenAI-compatible endpoint under single-use
  authorizations - see "Live coverage" above for what each record proves. That establishes
  tool-call wire compatibility **for that endpoint and that turn**, not for other vendors,
  not for answer quality, and not for multi-tool or multi-round behaviour. Both live runs
  happened while `knowledge_search` was the only registered tool, so **`memory_search` has
  no live coverage at all**: what one endpoint proved is that it answers a `tools` offer
  with a structured call and accepts the result back, not that it handles two offered tools.
- **Not built.** Long-term memory of any layer *as ambient context*, exercise generation
  and mastery updates,
  confirmation gates, and bounded self-correction after a refused call.
  The registry is a list, so those phases add entries rather than a new loop. A turn does
  receive the session's rolling summary, exactly as the Chat strategies do; that is the
  shared context of "Rolling session summary", not memory that outlives the session.
  Memories that do outlive it are reachable only when the model asks `memory_search`, and
  only inside the grant described in "Long-term memory storage and the confirmation gate".
  A capability that reads another store - an episode recall tool, for instance - joins the
  same two bounds without this loop being changed for it: it receives the turn's evidence
  allowance through the context it is already handed, and whatever it returns is measured by
  the estimate taken before the next model call. Its projection, index, authorization and
  business semantics are its own, and no window rule reaches back into them.

## Rolling session summary

A session keeps one compressed record of its own older turns. It exists because the
recent window alone silently amputates a long conversation: the 13th exchange answers as
if the first 12 never happened, and the only alternative - resend everything - is charged
per token and unbounded.

- **One row per session.** `session_summaries` holds `session_id` (unique), `user_id`,
  `content`, `covered_message_id`, `version`, `updated_at`. The boundary is a message
  identifier, not a timestamp or an offset: identifiers do not shift when older rows are
  appended around them, and a boundary computed from clock or index would. Deleting the
  session deletes the summary, and reading or committing another user's summary reports
  not found - the same refusal every other per-session write uses. The memory store and
  the GORM store are driven by one shared behavioural suite, so neither can drift on
  ownership, cascade, or the version rule.
- **What a turn sees.** System policy, then the sealed summary, then the complete turns
  after the boundary that fit the context budget, then the current question. Summary and
  window cannot overlap by construction: the window is cut at the same boundary the
  summary was written against. All three strategies receive that one value.
- **When it refreshes.** A refresh is due when the complete, answered messages past the
  boundary reach `GOPHERLEARN_SUMMARY_TRIGGER_MESSAGES` (default 12) **or** when their
  estimated token cost reaches `GOPHERLEARN_SUMMARY_TRIGGER_TOKENS` (default 6000). The two
  thresholds answer different questions: a count of stored rows this project can verify, and
  a size estimate that fires on a handful of enormous answers before any count is reached.
  The newest `GOPHERLEARN_SUMMARY_KEEP_RECENT_MESSAGES` (default 8) stay verbatim and are
  never compressed, so the model always reads the end of the conversation in the model's own
  words. Both numbers count messages, and boundaries snap to whole question-answer pairs:
  a keep value that lands mid-pair rounds up, because dropping a question while keeping
  its answer is the one malformed window this design must never produce.
- **Incremental, not a rewrite.** Generation is fed the previous summary plus the pairs
  the boundary does not yet cover; on a first refresh there is no previous summary and the
  material is those pairs alone. A 200-message session therefore never resends 200
  messages, and every refresh advances `covered_message_id` and `version` together.
- **One call, capped, never retried.** Exactly one non-streaming completion, with
  `max_tokens` set to `GOPHERLEARN_SUMMARY_MAX_OUTPUT_TOKENS` (default 2000) and no tools
  offered. The input side is bounded three ways - message count, rune count, and estimated
  tokens over the whole payload, **including the previous summary being revised and the
  labels framing it**, because a bound measured on the new material alone lets a long
  session's request grow on every refresh while each step looks compliant. Counts and runes
  are exact over stored rows; the token figure is a local estimate, and the three are not
  interchangeable units. If the budget leaves no whole pair to compress, the refresh is
  reported as skipped and the turn continues on the recent window: partial exchanges are
  never summarized, and a skipped refresh never advances the boundary. When the *oldest*
  exchange alone cannot fit, that is reported as its own code rather than as the ordinary
  skip, because a session that can never compact again is not a healthy conversation.
- **Scope, per turn.** The migrated rules - token threshold, token input bound, stage
  deadline, failure cooldown - travel with the turn as a `summary.Controls` value, and only
  a learning-agent turn asks for them. Every other strategy gets the legacy set, which is
  the zero value: a caller that forgets to specify is not silently given new behaviour. A
  chat refresh is therefore still triggered by message count alone, bounded by messages and
  runes alone, timed out by the model call alone with no stage deadline of its own, never put
  into a cooldown after a failure, and retried on the next turn exactly as before - the scope
  tests assert exactly that pair.
- **Time, and one budget per turn.** A learning-agent turn is wrapped in a single deadline
  before anything is spent on it, so the refresh and the answer draw on the same clock rather
  than each starting a fresh one. The stage may use at most `GOPHERLEARN_SUMMARY_STAGE_BUDGET`
  (default 8s), never more than `GOPHERLEARN_SUMMARY_TIMEOUT` (default 20s), and never more
  than `1/MaxDeadlineShare` of the turn's remaining time - the summary is an optimization
  over history that is still on disk, so it is the part that yields. A turn with less than
  `1.5s` left skips the stage instead of paying for a call that cannot finish. Chat and
  knowledge turns are not wrapped: their timing is the caller's context, unchanged.
  **The stage bounds the model call and nothing else.** The derived context is handed to the
  summarizing completion and dropped when it returns; the commit that follows is a local write
  and runs on the caller's context. Inheriting a deadline drawn for a provider would let a
  database write fail - and so end the turn - for a reason that has nothing to do with the
  database, and it would make the two events indistinguishable in the code.
- **Failure, and a cooldown per session.** A refresh that was due and failed ends the turn.
  Carrying on with the old summary would let the conversation silently outgrow what that
  summary covers, and the turn would be answered from a context this deployment can no
  longer describe. After a failure the session stops attempting one for
  `GOPHERLEARN_SUMMARY_FAILURE_COOLDOWN` (default 30s); the key is user *and* session
  together, so one conversation's failure cannot cool another's down. The cooldown is
  reported as its own named outcome rather than flattened into a generic internal error, and
  a turn that merely arrived too late to attempt anything does not extend it - lateness is
  not evidence about the model. A skipped stage is not a success: "due but not updated" is
  always distinguishable from "nothing was due".
  **Which context ended the refresh decides whether it counts.** A stage deadline that fires
  while the caller is still waiting is a refusal of this refresh - the summarizing model did
  not answer inside its share - so it opens the cooldown and is reported as
  `summary_stage_timed_out`, its own named internal outcome inside the ordinary internal
  error. A caller who cancels or whose own deadline expires is not evidence about
  summarizing: the cancellation is returned as itself, no cooldown is recorded, and the next
  valid turn gets its attempt. Attribution therefore reads the caller's context and never
  the stage's, because the two events are otherwise identical in the code: a stage's own
  expiry reads exactly like a caller who has left, which would let a slow summarizing model
  cost a fresh attempt on every turn, while recording every non-nil `Err()` instead would
  silence a session because a learner closed the page. This pair of outcomes applies to a
  learning-agent turn only: a chat or knowledge refresh takes neither the stage deadline nor
  the cooldown, so a failed chat refresh simply retries on the next turn as it always did.
- **Order, and what is never in the material.** Session lock → read summary and history →
  store the user message → refresh if due → assemble context → run the strategy → store the
  assistant message. The user message is durably stored before any provider call, the
  summary call included, so a process that dies mid-turn leaves a question on disk rather
  than a paid-for answer nobody kept. The unanswered question of the current turn is not
  part of the material: a summary written before the answer exists would record a
  conclusion that has not been reached. Orphan messages - a question whose model call or
  store failed - are excluded from both the window and the summary.
- **Optimistic concurrency.** A commit states the version it read. Any other version means
  conflict, and conflict means the row is left alone. A loser re-reads the winner's summary
  and continues with it instead of regenerating: paying a second time for text someone
  else already stored would buy nothing. Because turns on one session are serialized by
  the session lock, this is protection against another process or replica, not against a
  rival goroutine in the same request.
- **Failure is the existing failure.** When a refresh is due and fails - provider error,
  timeout, cancellation, empty text - the turn ends with the ordinary model-failure
  semantics: user message kept, no assistant message, no partial answer, and in the
  streaming path the failure arrives as the same `message.failed` event that already
  exists. Nothing is swallowed and nothing is dressed up: the previous summary is not
  presented as a fresh one, because a stale summary that claims to cover messages it does
  not cover is worse than no summary.
- **An unlocatable boundary fails the turn.** If a stored summary names a message this
  session no longer contains, its coverage is unknown, and there is no honest context left
  to build: sending the summary beside a full recent tail re-presents material the summary
  already claims, and dropping the tail answers from text this service cannot verify. The
  turn therefore ends with the integrity error behind `ErrInternal`, keeping the existing
  HTTP and SSE semantics, and nothing else happens - no summarization call, no
  recomputation of old history, and no silent clearing of the row, which is the only
  evidence of the damage. The same rule covers a summary adopted from a race: a row this
  turn did not plan must still be locatable before its boundary is allowed to cut the
  window. In practice this state is unreachable through the service's own writes - the
  boundary is a message id this package just read from that session, and the transcript is
  append-only - which is why refusing is affordable and degrading would not be.
- **Not charged to the Agent Harness budget.** A refresh is an independent call made
  before the strategy runs. An agent turn keeps its own 4 model calls, 3 tool calls, 10
  steps and 45 seconds, and a summary refresh on that turn costs one call on top of them -
  it is context maintenance, not a step of reasoning, so counting it either way would
  misreport one budget or the other.
- **Echo mode has no summarizer.** The mode that wires no provider wires no summary
  generation: no row is ever written, no placeholder text is invented to fill the slot, and
  the context stays the recent window. The startup line reports which of the two modes is
  running and what limits it will use.
- **Not built.** Asynchronous or queue-driven refresh, a summarizer model chosen
  independently of the chat model, per-user or per-session tuning, summary quality
  evaluation, and any compression of knowledge documents. The summary is scoped to one
  session and is forgotten with it; it is not long-term memory. Continuing to answer on the
  previous summary after a refresh was refused - what the reference project did, since its
  summary is an optimization over history still on disk - is deliberately not adopted here:
  this project ends the turn instead, and the alternative is a separate candidate needing
  its own comparison. Neither is a structured summary: no field schema, no per-item
  evidence message ids, and nothing that would turn an unconfirmed inference or an
  assistant's own knowledge into a durable fact about the user. A summary is compressed
  conversation, and a summary migration is not a route for writing long-term memory.

## Citation persistence

Sources are stored with the assistant message, not with the turn request: the
message owns its citations and the history endpoint returns them, which is what
lets a refreshed or reopened session show the same evidence the turn returned.

- One `message_sources` row per citation, ordered by a `position` column that
  preserves the answer's own citation order.
- The message row and its citation rows are written in the same transaction as
  the session activity update. A failure anywhere in that unit rolls the turn
  back, so the system never stores an answer whose citations are missing, nor
  citations whose answer was never saved.
- Deleting a session removes its messages and their citations; the cascade is
  explicit so it behaves identically on every engine.
- The memory and GORM repositories satisfy one shared behavioural suite for
  append order, ownership, detachment of returned slices, citation survival, and
  the session summary's one-row-per-session rule, version check, ownership refusal,
  and cascade on session delete.

## Context and memory

Conversation context, as implemented (see "Rolling session summary" for the rules):

- Recent complete turns are included verbatim within a message-count and rune-count
  budget - a size guard, not a token-accurate budget.
- A rolling per-session summary covers the complete turns older than that window, and is
  stored per session rather than recomputed per turn.
- Direct Chat, knowledge-grounded Chat and the Learning Agent build their context from the
  one shared layout, so no strategy carries its own summary or its own window.
- Summary text is derived context, never instruction: it is labelled as a compressed
  record, placed in a user turn, and cannot change the system policy or which tools exist.

Long-term memory, as implemented (see "Long-term memory storage and the confirmation
gate"):

- The three layers - global, topic, episodic - are modelled, stored, gated by a
  confirmation the user makes, and readable by one bounded, server-filtered tool call from
  inside the Learning Agent.
- Nothing reads them into a turn's standing context. Chat and knowledge answering never
  consult the table at all; a session's summary dies with the session while a memory
  outlives it, and the difference becomes visible to a model only when the Agent asks.

Episodic memory, as implemented (see "Episodic memory: the cross-session projection"):

- One opt-in subsystem (`GOPHERLEARN_EPISODES=on`, default off) owns a second kind of
  memory row: one versioned, cross-session projection card per session, written by a
  background projector after the conversation's assistant message is stored - never through
  the confirmation gate above, and never into the `memories` table.
- An agent turn may recall the caller's own cards across sessions through `episode_search`
  and read the learner's own words behind a card's cited message ids through
  `read_history_message`, both bounded by a per-turn server-side evidence ledger and
  store-side re-verification.

Still planned and not started:

- Extracting candidate memories into the confirmed long-term memory table. Every draft in
  that table was filed by a caller; none was produced by the service. The episodic
  projector writes its own table and does not close this gap.
- A consent store: the authorization seam exists (`MemoryAuthorizer`) and the production
  implementation deliberately grants no topics, so per-subject consent is decided in code
  rather than by the user.
- Mastery and lifecycle machinery on top of topic memory - confidence-weighted updates,
  decay, and the learning loop that would consume them.
- Memory content stays evidence, never executable instruction - the rule the summary and
  the tool result both follow, and the rule any future recall path must keep.

## Episodic memory: the cross-session projection

The episodic subsystem answers a question the confirmation gate deliberately does not:
what happened in a finished conversation, kept in a form a later session can search and
cite. It is a derived record with its own table, its own worker and its own boundary - a
machine writes it without asking, which is exactly why it can never masquerade as a
confirmed fact.

### Generation and durability

- Trigger: when a turn's assistant message is stored, the turn pipeline records one
  durable debounce row in `episode_projection_jobs` (unique per owner and session, desired
  watermark monotonic, available-at quiet window default 10 minutes). The publish is
  best-effort on the turn's own path; a missed publish is repaired by a reconcile loop
  that scans for sessions whose latest assistant message is past the stored watermark.
- Worker: a bounded background loop claims due jobs under a lease (lease must exceed the
  task timeout; attempts capped at 3 by default; recovery passes reclaim expired leases at
  start and while running), projects, indexes, and completes or requeues. A job whose
  desired watermark advanced while it was claimed returns to pending with its budget
  reset; a job that exhausts its attempts is parked as failed with a closed-vocabulary
  error code. Re-delivered publishes are debounce, never duplication: the same job row is
  reused and the projection itself short-circuits at a covered watermark without a model
  call.
- Projection: two model calls per card. A generator (forced `publish_episode` tool call)
  proposes the card; an independent verifier, shown only the assertion and the cited user
  message bodies, trims every assertion that is not entailed - uncertain counts as no. A
  deterministic boundary runs before either: a citation to an absent or foreign-session
  message refuses the whole projection, a citation to an assistant turn drops that entry
  only. A session with nothing durable is stored as an `abstained` row - a persisted
  "we looked and there was nothing" that stays invisible to recall and answers why the
  session has no memory.
- Storage: `episode_memories`, one row per session, optimistic version control where an
  update must move the source watermark forward. The row keeps its source watermark, the
  summary version it read, its assertion identities (server-issued; the model may echo an
  id it was shown, never mint one), and its index status.

### Index, recall, and the evidence boundary

- Retrieval view: in `semantic_redis` mode a RediSearch index with its own name and key
  prefix (`gopherlearn_memory_episodes_v1_idx`, `gopherlearn:memory:episode:`), schema
  verified field-by-field at startup; in `deterministic` mode a process-local view warmed
  from the store at startup. Both follow the same recall rules: exact-topic, identifier,
  lexical and vector channels fused by reciprocal rank, where a vector-only candidate is
  never admitted and lexical-vector agreement must sit within the corroboration depth. An
  unrelated question routes to no read at all; a history-shaped read with no eligible
  candidate abstains with a named reason.
- The store decides: every candidate the view proposes is re-read through the repository -
  owner, live session, active status, indexed view, exclusion of the current session - and
  a candidate's per-version provenance closure is re-checked against live rows. A lagging
  or stale view costs recall, never correctness.
- Agent tools: `episode_search` (recall only the caller's own cross-session cards, never
  the current session's) and `read_history_message` (read back the learner's own words
  behind a card's cited message ids). They are installed together or not at all. Read-back
  rights are granted in two server-side stages and recall alone grants nothing: after the
  tool renders its bounded observation, it hands the server the exact displayed slice, and
  the server grants the intersection of that set with the stored card's current evidence
  closure one id at a time - clipped cards, clipped assertions and partially rendered ids
  grant nothing, and ids the stored card no longer cites are not granted. A store failure
  during that handoff fails the tool call with the original error, adds zero grants, and
  never disturbs grants the same turn earned earlier. The granting card is re-verified in
  the store (id, owner, session, version, active status, indexed view, citation closure,
  watermark) again at the moment of each read, a re-projection invalidates the old
  version's rights, and a new turn starts an empty ledger. Citations attach only to content
  a tool actually returned, through the same observation-source path every other tool uses,
  under the stable record identity `src-episode-<episode id>-v<version>`.
- Deletion: deleting a session deletes its cards and jobs in the store, drops the view rows
  best-effort, and every read re-checks the session boundary. A re-projected card
  invalidates the old version's read-back rights by version comparison alone.

The episodic subsystem changes nothing outside itself when the switch is off: no worker, no
tools, no trigger, no tables written. When it is on, it still never writes the `memories`
table, never proposes a draft, and never injects memory into a turn's standing context.

### Migration differences and explicit omissions

This is not a byte-for-byte context migration. The previous production Agent built Episode
cards into a server-side context builder; clean keeps ordinary Chat and Knowledge context
unchanged and exposes cross-session Episodes through a model-driven `episode_search` tool.
The previous trigger was completed Agent-run specific; clean triggers after every successfully
stored conversation turn because the clean message service is the shared persistence boundary.
Consequently clean can do more background projection work; the feature is off by default and
the 10-minute quiet window is the primary debounce.

The previous **shadow recall** observer is not migrated. The previous mode-gated **assertion
governance and relation pass** are not migrated: clean has stable assertion ids and versioned
cards, but no governance tables, relation classifier or replacement lifecycle. The previous
**message-level Episode provenance** is not migrated either; source/version closure remains on
the Episode row and is validated during reload, without adding derived provenance to messages.
These are explicit limitations, not silently treated as current truth.

Publishing is synchronous only for the durable job-row write, under an independent two-second
context; it is not accurate to call it completely non-blocking. Projection, verification and
embedding are background work and can incur provider/embedding cost after the turn. No MQ,
TTL extraction, image recognition, TTS, retired semantic-memory extraction, or ordinary Chat
automatic long-term-memory injection is part of this system.

One scope statement belongs next to the omissions: the offline tests (in-memory store plus a
scripted model), the repository contract suites, and the gated real-engine tests prove the
rules and boundaries of this section - authorization, versioning, leases, deletion, engine
behavior. They are not evidence about projection quality with a real language model; no
real-provider episode projection has been run, and none of these tests should be cited as
one.

## Long-term memory storage and the confirmation gate

The boundary this phase exists to draw is a thin one: a machine may propose, only a user
may decide. Everything below serves that line. Since Phase 5B.2 the result is read back -
but only through one bounded, server-filtered tool call inside the Learning Agent, never as
a turn's standing context.

- **Two types, not one with a flag.** `domain.MemoryDraft` carries no status field, so a
  candidate cannot be constructed already active; `domain.Memory` carries the status of a
  stored record. `StoredMemory()` is the only way a draft becomes a record, and it always
  yields `pending`. That is a compile-time statement of the rule, which is the point - a
  validated field can be forgotten by the next caller, a missing field cannot be.
- **Two write doors, and `origin` is what keeps them apart.** A person typing in a fact about
  themselves is a different act from a machine proposing one about a conversation, and the
  two need opposite rules, so they get one discriminator and two methods.
  `domain.UserAuthoredMemory` (`user_authored`) has four fields - author, scope, topic key,
  content - and `StoreUserAuthoredMemory` writes it `active`, at the server's own confidence
  of 1, with no provenance rows: the sentence is the evidence, and asking for a second
  confirmation of a decision the submit button just made would be theatre. `domain.MemoryDraft`
  (`agent_proposed`) writes `pending` and must name a session and real messages, because a
  claim about a conversation is only checkable against that conversation. The store methods are
  separate rather than one method with a boolean, and that is the whole protection: loosening
  the gate for the authored door would loosen it for the agent, which is the caller that must
  never gain a way to write itself a fact. `origin` is stored and internal - it is in no
  request body and no response body, and a test fails if either grows one - because a field
  that decides which checks apply cannot be a field the checked party may choose.
- **The lifecycle is closed.** `pending → active → superseded | archived`, plus
  `pending → archived`. `CanTransitionTo` is the one table both stores consult, and
  `superseded` and `archived` are terminal: an old answer returns by a new confirmation,
  never by editing the record that retired it. Rejecting and archiving land in the same
  stored state and differ only in where they start - only a draft is rejected, only an
  active memory is archived - which is why the two are separate operations rather than one
  call with a flag.
- **Confidence never promotes, but it does hide.** No numeric threshold turns a draft into an
  active memory - promotion is a user action in their own account, because a system that
  confirmed beliefs by asking the thing that would later answer from them could not be
  corrected. The floor described under "The threshold is a read rule" applies on the way *out*
  of the store only.
- **Scopes and their key rule.** Global facts must not carry a `topic_key`, topic memories
  must, episodes may. The reason is the versioning key `(user, scope, topic_key)`: a stray
  key on a global memory would split it into two namespaces, each keeping its own surviving
  answer, and a topic listing with no key would return an arbitrary topic's memory.
- **One active version per subject, except episodes.** Anything that makes a global or topic
  memory active - confirming a proposal, or a hand-written add - supersedes whatever was
  standing for the same `(user, scope, topic_key)` in the same transaction, and records
  `superseded_at`. It does not matter which door produced the predecessor: two active answers
  to one subject would leave the recall read choosing between them, which is the decision the
  versioning rule exists to prevent. A hand-written add leaves a *pending* proposal alone,
  though: that row is a decision still being made, not an answer being replaced. Episodes
  accumulate: two experienced events are two facts, and retiring the first would rewrite what
  happened.
- **Optimistic state, not optimistic locking.** The promotion is a conditional write -
  `WHERE status = 'pending'` - so two concurrent confirmations of one draft cannot both
  take effect; the loser finds zero rows and is told which state it actually met. The
  version number that protects the rolling summary is not needed here because the state
  itself is the version.
- **Provenance is part of the proposal, not a remark on the record.** A memory that arrived as
  `agent_proposed` names a source session and at least one message of it, in citation order,
  held in `memory_sources` with a `position` column. Creating a draft verifies the session
  belongs to the caller and that every message is in it; confirming re-runs the same check,
  because the conversation can be deleted in between - and a draft whose evidence is gone is
  refused rather than promoted. A fact nobody can check is not a fact. A `user_authored` row
  is the exception by construction rather than by exemption: the type has nowhere to put a
  session, the write creates no `memory_sources` rows, and `Memory.Validate` refuses an
  authored row that somehow grew provenance - that combination would claim a conversation
  backed a statement nobody took from one. The shared suite asserts both halves, and an
  SQL-level test counts the child rows to prove the absence is real.
- **Deleting a session does not delete memories.** The reference rows stay, deliberately:
  cascading would destroy confirmed facts on the strength of a housekeeping action, and
  several memories can cite the same conversation. What is left dangling is visible and
  correctable; what is silently deleted is neither.
- **Three stores, one suite.** `repositorytest.MemorySuite` runs against the in-memory store,
  against GORM/SQLite, and - behind a DSN environment variable, so an ordinary `go test` needs
  no server - against real MySQL: field rules per scope, pending invisibility, confirm/reject/
  archive outcomes, cross-user isolation in both directions, supersede versus accumulate,
  provenance refusal, and the no-cascade rule. It now also runs the authored door on all
  three: active on arrival with no provenance and no place in the pending queue, refusal plus
  an empty table for an episode, a stray key, an unknown scope or a missing author, and the
  retirement of a standing answer - whoever produced it - without touching another subject,
  another account or an undecided proposal. The SQL side adds restart survival, the child-row
  count that proves an authored record cites nothing, and three rollback cases - a draft whose
  provenance rows fail leaves nothing behind, a confirmation whose supersede step fails leaves
  the predecessor active and the draft pending, and a hand-written replacement whose insert
  fails leaves the answer that was standing still standing, rather than a subject with no
  active memory or with two.
- **Recall is a server decision, made twice.** `MemoryAccess.Recall` filters before it
  renders: the caller's own rows, `active` only, inside a `MemoryGrant` this server built
  through a `MemoryAuthorizer` - never from the request, because in an agent loop the
  requesting party is a model. The production authorizer grants global memory plus the
  current session's episodes and **no topics and no other session's episodes**: a topic key
  is recalled only when something has explicitly authorized it, and this build has no
  consent store to do that. A grant that changes is a code decision with a test attached,
  not a string a caller can pass.
- **`active` is necessary, not sufficient.** Four independent gates decide visibility: the
  scope grant, the lifecycle state, the confidence floor, and the capacity bounds. A memory
  can pass every status question in the database and still never reach a prompt, so a test
  that wants to prove "the answer path does not read memory" must first prove the memory
  was recallable at all.
- **The threshold is a read rule, not a storage rule, and its default is `0.6`.** The
  production floor is `DefaultMemoryMinConfidence = 0.6` - the filter strength the earlier
  long-term-memory pairing experiment measured as M2, not a placeholder. Effective floor is
  `max(0.6, requested)`: a caller may tighten it and may not loosen it, `0` in a request
  means "unset" rather than "keep everything", and there is no parameter that switches the
  filter off. A requested floor outside `0..1` - NaN and either infinity included - is
  refused rather than normalized, because silently coercing a garbage threshold would turn
  an explicit caller error into a silent behavior change. The value is asserted as the
  literal `0.6`, never as "default equals default", so a test still fails if the floor
  drifts toward zero.
- **Three bounds, and one order.** Results per read, runes per memory, runes in total:
  a memory the total cannot carry is dropped whole rather than clipped into a half-fact.
  Order is score, then stored creation time, then the order the store listed - deliberately
  *not* the memory identifier, which sorts like a clock in one store and like a decimal
  counter in the other, and would make a deployment's memory order depend on its database.
- **Lexical matching, called what it is.** Query terms and memory text go through the same
  tokenizer the knowledge pipeline uses, and a memory becomes a candidate only if it shares
  a term. This is not a vector search and claims no semantic understanding; the score is a
  covered-term ratio, comparable only inside one call.
- **The tool shows text and a label, never an identifier.** `memory_search` publishes one
  parameter, `query`. Each rendered block carries a number, a server-written label
  (`全局记忆` / `主题记忆：<key>` / `经历记录`) and the memory text. The memory id, source
  session and source message ids stay server-side: an identifier the model cannot see is an
  identifier it cannot invent, cite selectively, or use to ask for a specific row.
  A memory the observation budget could not carry earns no citation - the same rule
  `knowledge_search` follows for chunks.
- **An empty read and a failed read are different answers.** No authorization says so
  ("本轮没有任何记忆检索授权"), an authorized miss states what was searched, and a recall
  error ends the turn as a tool failure. Turning "could not read" into "you have nothing
  remembered" would let an outage rewrite what the product believes about the user.
- **Chat and knowledge answering are not readers.** Neither strategy consults memory; a
  regression test wraps the store, runs a synchronous and a streamed turn for both a chat
  and a learning-agent session, and fails if the conversation path performs any memory
  operation at all. The test first proves the seeded memory *is* recallable - right scope,
  `active`, confidence at or above the production floor - so a pass means the answer path
  declined to read, not that a filter hid the row. That is what keeps "the tool exists"
  from becoming "memory is context".
- **No real-provider evidence for `memory_search`.** The tool has never been driven through
  a live tool-calling endpoint. The two sealed Agent tool records were taken before it
  existed, so they attest `knowledge_search` only; citing them as proof that `memory_search`
  is provider-compatible would be a claim this repository cannot support. A real smoke needs
  its own authorization and its own record.
- **`propose_memory` is a migrated capability, tightened on the way in.** The original
  GopherAI design had the same tool; this build keeps the intent and re-derives the surface
  for this architecture. Three changes are the point of not copying it. *Argument parsing is
  strict here*: the schema is checked against the argument struct, so a payload carrying
  `user_id` or `status` refuses the whole call instead of having the extra key ignored
  downstream. *No identifier leaves*: the answer to the model is a status sentence, where the
  original returned the draft id and its conflict list as JSON. *The source is a real message*:
  provenance is the turn's stored user message id, not an opaque `agent_run:<id>` reference,
  so a draft can be checked against what was actually said - and a turn with no stored
  question refuses rather than filing a sourceless memory.
  Confidence is owned by the service (`ProposeMemoryDraft` fixes it at 1), never by the
  proposer, and the once-per-turn cap lives in a name-keyed table in `agent` rather than on
  the tool, so a decorator written to count executions cannot loosen it by not forwarding a
  method. What was deliberately left behind: `subject`, `tags`, `durability`, `valid_until`,
  conflict enumeration and `replace`, a global-memory quota, and `RunID`-keyed idempotency.
- **The confirmation surface is a transport, not a second gate.** Phase 5B.3 published
  `POST /api/v1/memories`, `GET /api/v1/memories?state=pending|active` and
  `POST /api/v1/memories/{id}/confirm|reject|archive`, all of which authenticate, decode a
  shape and then call the same `MemoryService` the store owns - the handlers hold no rule of
  their own. Three things follow from that and each has a test:
  - *the request cannot name a state.* `status`, `active`, `superseded_at`, `confidence` and
    both timestamps are unknown fields, refused by the strict decoder, and an action carries
    no body at all (an empty body or `{}` is accepted, anything else is a 400). A draft's
    confidence is fixed by the server at "certain", because its author is the person typing
    it - and a client-supplied low value would otherwise confirm a memory that the recall
    floor then silently hides.
  - *a listing must say which state it wants.* `state` is required, and only `pending` and
    `active` are accepted. Answering `archived` with an empty list would turn an unreadable
    state into a claim that nothing is there.
  - *the wider read is a separate method on purpose.* The owner's management listing is
    `ListMemoriesByStatus`, which spans scopes so a topic memory can be shown without its key
    being known first - the thing `ListActiveMemories` refuses to do. It is not a recall path
    and grants nothing: the prompt-facing read stays where the grants, the floor and the
    capacity limits are applied.
  The lifecycle sentinels the service already returned now reach the client intact as `409`
  codes of their own (`memory_already_confirmed`, `memory_superseded`, `memory_archived`,
  `memory_not_active`, `memory_evidence_missing`) instead of collapsing into a generic
  conflict or a `500`, because the user's next step differs for each. The web client keeps
  its own copy table for those codes rather than surfacing the server's English, and the
  frontend's `http` adapter is what a production build installs; the labelled demo adapter
  exists only behind the dev server.
- **Not built.** Automatic extraction (nothing proposes a draft - the create call is a human
  or a client deciding to store one), topic-consent storage, mastery updates, episodic
  auto-generation, semantic recall, any edit of a stored memory's content, any route that
  revives or lists a retired row, and any memory use outside the one agent tool and the
  confirmation surface above. `ListActiveMemories` is the only read a future prompt may be
  built from, and it filters by status inside the store so a reader cannot forget the filter.

## Learning workflow

Phase 6B implements the quiz loop end to end: evidence selection, question generation,
answering, grading, and one topic projection that later questions read. It persists its state
across requests, so no connection has to stay open and a page reload resumes work rather than
restarting it.

```text
created         -> asking | cancelled | expired
asking          -> created | waiting_answer | cancelled | expired
waiting_answer  -> grading | completed | cancelled | expired
grading         -> waiting_answer | completed | cancelled | expired
completed | cancelled | expired        terminal: no outgoing edge at all
```

Two edges are not forward, and each exists so that one failure does not cost a learner a quiz:
`asking -> created` recovers a generation abandoned by a process that died, and
`grading -> waiting_answer` returns a quiz whose scoring call failed to "answer me again" with
the question still open. `waiting_answer -> completed` is lawful only when the last question was
skipped, so a finished conversation is not stored as an unfinished one. The table lives in
`domain.LearningSessionStatus.CanTransitionTo` and the repository refuses any pair outside it
**before** touching a row, so no caller reaches a state by writing the column.

Ownership is split the same way as the rest of the backend:

- `internal/domain/learning.go` owns the states, the closed transition table, the question
  types, the grade ceilings (60 + 25 + 15 = 100) and the projection rules (recency-weighted
  mean over the last 10 valid attempts, confidence over 6, `insufficient` below 3 attempts,
  `mastered` at 85 with confidence at least 0.5, at most 8 weak points).
- `internal/service/learning*.go` decides *whether* anything happens: it normalizes the topic
  before anything is filed under it, resolves the conversation by owner and only then checks its
  mode, ownership-checks a named collection list before reading a chunk, reserves the
  conversation, releases the reservation on every failure path, and grades.
  The score is this package's arithmetic over the three dimensions a model was asked for; a
  model's own total is not trusted or stored.
- `internal/httpapi/learning.go` authenticates, decodes one shape, calls the service and
  translates the outcome into a stable code. It owns no rule: no status, no score, no
  membership, no ordering decision happens in this file.
- `internal/repository/gormmysql/learning.go` enforces the transition table plus optimistic
  version CAS and a fenced grading lease, so two concurrent confirmations of one answer
  produce exactly one write and a grader whose lease was taken over cannot commit over the
  winner.
- `internal/devmodel` is the only "model" that is not a provider: a scripted question-and-grade
  answer for local development, reachable solely through
  `GOPHERLEARN_LOCAL_LEARNING_MODEL=deterministic` in `echo` mode with no provider value in the
  environment. It implements `model.ChatModel` and not `model.ToolChatModel`, and the assembly
  hands it to the quiz workflow and nothing else - `Assembly.Model` stays nil, so chat,
  knowledge-grounded answers, the agent and summaries keep running on the placeholders `echo`
  has always used. Placing it behind the same seam is the point: its output is parsed, bounded
  and rejected exactly like a provider's, so a local run exercises the real evidence rules,
  question types, citation checks, grade ceilings and projection instead of a shortcut around
  them. What it proves is that pipeline. It cannot read an answer, so it claims nothing about
  one - the verdict is a fixed script chosen by the question, and the question text, the
  reference answer and the feedback all say they are scripted.

Two placements are deliberate and are the reason the HTTP shapes stay small. First, the
**projection has one reading surface**: `mastery`, `confidence`, `valid_attempts`, `level` and
`weak_points` are exposed only by `GET /api/v1/learning/progress`, and the
answer/skip/cancel/start/resume responses carry no projection at all - storing a graded attempt
and its consequence without echoing them keeps a second, drift-prone copy of the learner's
record from existing. Second, **no score or mastery number reaches a model**: what a generation
or grading prompt is told about a learner's record is prose with the measurements removed, so a
number cannot become an input that flatters or punishes the next question. Evidence ordinals
such as `[1]` are positions in a list the server handed over, not judgements, and they are the
only digits a prompt carries. `weak_points` are used only when the projection is reliable
(>= 3 valid attempts); below that the quiz is generated without them rather than aimed at one
grader's single note.

A third placement decides where a quiz may exist at all: **it is bound to a `learning_agent`
conversation.** `Start` resolves the session by owner and only then requires that mode, and the
order is the whole refusal semantics - a conversation that is not yours, or was never stored,
stays the generic `404 not_found` this surface has always answered with, while a `chat`
conversation you do own is a `400 invalid_request`. The refusal is not partially applied: no quiz
row is reserved, no generation call happens, no projection moves, which is why a test can check
it by counting model calls instead of reading an error string. The binding points one way. Only
starting is mode-gated, because only starting spends a model on a conversation's behalf; the
projection is stored under a normalized topic and stays readable by its owner from any page, and
the mastery a quiz produces reaches only learning-agent turns, as prose
(`learningContextNote` in `internal/agent/harness.go`) that no other strategy is given. The web
picker lists only learning-agent conversations so the rule is findable; the server refusal is
what makes it true.

Advancing a quiz is one write, not two. When an answer is graded the store moves the ordinal
**and** the status in the same step - `grading -> waiting_answer` for the next card,
`grading -> completed` for the last one - because a row that keeps saying `grading` after its
answer has been scored describes a judgement that has already happened; `waiting_answer ->
completed` on the last skip is the same rule seen from the other side. `GET
/api/v1/learning/sessions?session_id=...` therefore answers a conversation that is the
caller's own and simply holds nothing with `200` and `data: null`, not with `404`: finishing a
quiz and reloading is the ordinary path, and an idle conversation must stay distinguishable
from one that cannot be read.

Evidence is snapshotted per question at generation time, so grading a week later scores against
the text the generator saw and not against a note edited in between. Expiry, abandoned
generation and an orphaned grading lease are settled inside the read path
(`RecoverLearningSession`), because that is the moment somebody is looking; a background sweeper
would be one more moving part to trust.

## Phase 1 ownership

- Backend developer owns `cmd/`, `internal/`, and backend unit tests.
- Frontend developer owns `web/` only.
- The coordinator owns root files, API contracts, integration tests, CI, and
  release decisions.
