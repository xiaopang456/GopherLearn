# eval/l2l3：回答层（L2）与 Agent 层（L3）的运行程序和断言

这是测评体系里 L2（回答是否来自知识库、引用是否合法并命中证据、无答案题是否如实处理）和 L3（该查资料时查了、不该查时没查、预算内完成）的**运行程序与断言**。检索层 L1 见上一级的 [eval/README.md](../README.md)。

> **先读这一条**：这里的测试和演示全部用**假后端**（`fake/server.mjs`，不调用任何模型、不起任何真实服务），它的结果只证明链路和断言的计算是对的，**不是任何质量结论**，评分报告里也会带"不得引用"的横幅。对真实系统的运行需要先试算并确认预算（见"真实运行前"）。

## 它怎么分工

```
假后端 / 真实后端 ──▶ provider ──▶ 原始记录 record.v1 ──▶ score.mjs ──▶ outcomes.jsonl ──▶ l23 report ──▶ report.md
                       (Node)      (每次尝试一个 JSON)    (Node，离线)     (每次尝试一行)      (Go)
```

- **provider**（`provider/`）：登录 → 每次尝试新建会话、换新 `requestId` → `POST /api/v1/AI/agent/run` → 读该轮步骤（`GET …/runs/:runId/steps`）→ 写一条原始记录。Promptfoo 只是运行外壳（`provider/gl_provider.mjs` 是它的自定义 provider）。
- **断言**（`lib/`）：纯函数 `(记录, 题目) → 判定`。同一套函数既被 Promptfoo 的断言调用（`promptfoo/assertions.cjs`），也被离线评分调用（`score.mjs`），所以两边不会对同一次尝试给出不同判定。**离线评分是权威结果**，Promptfoo 里的通过 / 失败只是它的一个视图。
- **评分**（`score.mjs`）：读原始记录 + 题集，写 `outcomes.jsonl`。离线、可重复，同样的输入得到同样的字节。运行目录里的 `run.json` 记着它对应的题集与语料哈希，哈希对不上就拒绝评分。
- **统计与报告**（Go，`cmd/l23`）：单次成功率、pass^k、配对比较，复用 L1 的聚类自助法和 McNemar 精确检验。

## 数据不进仓库

原始记录里有回答原文和引用片段，来自私有文档。**账本目录、原始记录目录、输出目录只要落在 git 工作区内就会被拒绝**（provider、`build-config.mjs`、`l23 report` 都检查）。仓库里只有合成夹具（`testdata/`）。

## 快速开始（离线、不花钱）

```bash
# 在仓库根目录。全部测试，含对 Go 源码的防漂移测试：
node --test "eval/l2l3/test/*.test.mjs"
cd eval && go test ./cmd/l23 ./internal/l23
```

手动走一遍：用 `fake/scenarios.mjs` 里的两套剧本（`good` 全对，`flawed` 各错一种）对 `eval/testdata/` 的合成语料跑一遍，评分，出报告。`test/e2e.test.mjs` 就是这条链路的自动版本，可以对着读。

## 数据格式

**题集**：L1 的开发集文件（`answerable`、`evidence_sets`，格式见 eval/README.md）；**默认只接受 `split: "dev"` 的行**，混入封存集的行会让 `build-config.mjs` 整个拒绝。封存集要显式开关才能用，见下面的"封存集"。

**L3 标注**（`labels.jsonl`，每题一行，按 id 关联，不改 L1 题集）：

```json
{"id":"q-neap","source":"dev","category":"knowledge_question",
 "expect":["knowledge_search"],"forbid":["memory_draft"],"max_tool_calls":3,"author":"human"}
```

| 字段 | 含义 |
|---|---|
| `expect` | 应当调用的**能力**（不是工具名）：`knowledge_search`、`document_read`、`episode_search`、`topic_memory_read`、`memory_draft`、`history_read` |
| `forbid` | 不应调用的能力，只要有一步出现就违规，成败不论 |
| `max_tool_calls` | 本轮工具调用次数上限 |
| `doc_constraint` | 可选。为 `true` 时要求 `document_read` 读的文档必须出自本轮自己的检索结果；需要读存下来的步骤参数，见"已知限制" |

**仅 L3 的题**（`l3only.jsonl`）：没有标准证据的题，`l3_only: true`、`author: "human"`；标注里的 `source` 只取 `dev`（指向 L1 开发集里的题）或 `l3only`（指向这个文件）。检查器 `check.mjs` 核对：能力名合法、id 对得上、"应检索"占 40%–60%、新写的题不含产品按规则处理的措辞。

```bash
node eval/l2l3/check.mjs --labels L --dataset D --l3-only N
```

**原始记录 `record.v1`**：`arm`、`qid`、`rep`、`env`、`http`、`statusCode`、`runId`、`replayed`、`answer`、`answerBasis`、`knowledgeStatus`、`knowledgeReason`、`sources`、`toolCalls`、`modelCalls`、`steps`（`stepsError` 在读取失败时出现）、`error`、`durationMs`。失败（超时、非 JSON、读不到步骤）都会留成记录，不丢。

**结果 `outcome.v1`**：每次尝试一行，含 L2、L3 两层各自的 `verdict`、归类 `class`、各断言明细和代价。字段由 `test/e2e.test.mjs` 写成 `testdata/outcomes.fixture.jsonl`，Go 测试读同一份文件，两边任一改了格式都会失败。

## 断言的判定规则

每个断言返回四种判定之一：`pass`、`fail`、`needs_judge`（结构状态判不了，交判分模型）、`error`（服务没答出来或记录残缺，**不算回答的失败**）。一层的判定：有 `error` 就是 `error`，否则有 `fail` 就是 `fail`，否则有 `needs_judge` 就是 `needs_judge`，都没有才是 `pass`；只给数字或标记的断言（引用覆盖率、`fallback_citation`）不参与。

### L2 可答题

| 断言 | 通过条件 |
|---|---|
| `state_valid` | 三个状态值都在产品的枚举里（否则 `error`，后面的断言不再评） |
| `answered_from_knowledge` | `answerBasis = personal_knowledge` 且 `knowledgeStatus = grounded`。否则按原因归类：`refused`、`general_knowledge`、`no_evidence`、`unexpected_basis`；`unavailable`、`invalid_grounding` 是 `error` |
| `citation_present` | 至少一条引用 |
| `citation_legal` | 每条引用的文档在语料清单里，行号 `1 ≤ start ≤ end ≤ 该文件行数`。归类：`citation_unknown_doc`、`citation_out_of_range` |
| `citation_hits_gold` | 引用与**任一**备选证据集至少共有 1 行（与 L1 的 MRR 口径一致）。归类：`citation_miss` |
| `citation_coverage` | 只是数字：最佳备选证据集被引用覆盖的行比例，不判通过 |

整体成功 = 前五条都通过。要点覆盖、忠实度由判分模型加入（尚未实现）。

### L2 无答案题——只按结构状态

| 结构状态 | 判定 |
|---|---|
| `refused`（`no_evidence`） | `pass`，档位 `abstained`（如实说明没有证据，计 0 档） |
| `general_knowledge` + `no_evidence` | `needs_judge`：已声明来源，声明之后的内容是否编造要判分模型看 |
| `personal_knowledge` + `grounded`（有引用） | `needs_judge`："有依据的否定回答"在这里，结构上区分不了"有依据地说没有"与"把无关片段当答案" |
| `unavailable`、`invalid_grounding`、枚举非法 | `error` |
| 其他（如 `conversation`） | `fail`，`unexpected_basis` |

**不用"必须拒答"的字面规则**：合理的回答有很多种说法，按措辞判会误判。判定与回答的措辞无关。+1 / −1 的记分档要等判分模型，这里只给 0 档和"待判"。

### L3 工具调用

| 断言 | 通过条件 |
|---|---|
| `tools_consistency` | `step_type = tool` 的步骤条数等于 `/agent/run` 返回的 `toolCalls`；步骤读不到是 `error`。对不上时后面三条不评 |
| `tools_required` | `expect` 里的每个能力，至少有一个**状态为 completed** 的步骤用过（失败的调用不算"查了"） |
| `tools_forbidden` | `forbid` 里的能力**没有任何步骤**用过（失败的调用也算调用了） |
| `tools_budget` | `toolCalls ≤ max_tool_calls` |
| `doc_constraint` | 可选，见上 |

只看结果，不要求调用顺序。工具名先映射成能力（同一能力有两套名字：`knowledge_search` / `search_uploaded_documents` 等），对照表只在 `lib/capabilities.mjs` 里写一处；不认识的工具名是 `unknown`，只记录，不参与任何 expect / forbid。

## pass^k 与比较

一次尝试**成功**＝该层的判定是 `pass`；`needs_judge` 与 `error` 不算成功，但单列。报告同时给两种分母：待判和错误都按失败；只在"通过 + 失败"里算。pass^k 用不放回的估计：一道题 n 次里成功 c 次，k 次全部成功的概率是 C(c,k)/C(n,k)，对题取平均；报告 k = n 和 k = 2，重复次数不足 k 的题被排除并写明。配对比较逐题进行，区间用按簇的自助法，McNemar 看"全部重复都成功"。

## 真实运行前

**没有授权不要对真实系统运行。** 授权之后：

1. 设置 `GL_L23_ENV=real`（没有默认值，`fake` 时只许连回环地址）。
2. 预算：`GL_L23_MAX_RUNS` 必须大于 0，provider 在每次请求**之前**把这次尝试写进账本，到上限就拒绝；不重试。试算：题数 × 臂数 × 重复次数 = `/agent/run` 次数，每次约 3.4 次聊天模型调用（含后台任务），另有判分调用（判分模型尚未实现）。
3. 所有模型调用经一个带硬上限、记 token 账本的计数代理（本仓库不提供，需自备）。
4. 每个臂用独立的数据库和 Redis（或运行前完整重置）。
5. 用 `promptfoo/build-config.mjs [--split dev|sealed] <run-spec.json> <输出目录>` 生成配置并写 `run.json`；`npx promptfoo@0.125.1 eval -c … --no-cache -j 1` 运行。**生成的配置没有用真实的 Promptfoo 跑过**（CI 不安装它）；第一次真实使用前先用 `npx promptfoo@0.125.1 validate` 检查。

## 封存集

封存集只在里程碑（基线、发版）运行，**默认拒绝**。要运行它，必须显式写 `--split sealed`（或在 run-spec 里写 `"split": "sealed"`；命令行与 spec 不一致就拒绝）。规则：

- `datasetPath` 只能是封存集题集：里面有任何不是 `split: "sealed"` 的行，整个拒绝（反过来，开发集运行里出现封存集的行也整个拒绝，报错里会提示需要 `--split sealed`）；
- 封存集运行**只评回答层（L2）**：run-spec 里不许有 `labelsPath` 和 `l3OnlyPath`；
- `run.json` 记录这次运行属于哪个 split；同一个原始记录目录不能先后用于开发集和封存集运行；
- 命令会打印一行 `SEALED RUN`，提醒这是里程碑运行、不要据结果调参。

## 已知限制

- **`doc_constraint` 暂不可用于真实运行**：读步骤的公开接口刻意不返回参数（它们可能含用户问题和检索片段），而存下来的参数在 `agent_steps` 表里。断言只依赖一个 `inspector` 接口（`inspect(runId) → 每步的能力与文档 id`），这里只有假的实现（`fake/server.mjs` 的 `fakeInspector`）；读测试库的实现要等有测试库时再写和验证。没有 inspector 时该断言标为"未评"。
- **`tools_consistency` 在真实系统上没验证过**：它假定步骤里的工具步骤条数等于 `toolCalls`。第一次真实运行时如果系统性地对不上，要看是假设错了还是步骤读取有问题，不要直接忽略。
- 产品对除问候以外的几乎所有问题都要求"落地"（`requiresGrounding`），所以"不应检索"的题在真实系统上很可能被判为违规；这是要测出来的行为，不是标注错误。
- 耗时是整次 `/agent/run` 的耗时；返回里没有 token 用量，后台任务的调用也看不到，这些要看计数代理的账本。
- 要点覆盖、忠实度、"是否编造"需要判分模型，目前未实现，这里只留了 `needs_judge` 的出口。
