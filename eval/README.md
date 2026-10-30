# eval：检索质量评测（L1）

`eval/` 是一个独立的 Go 模块，用来度量 GopherLearn 知识库检索（`rag.RAGService`）的**检索质量**：
给一批问题和人工标注的证据位置，看不同检索方式（向量 / 关键词 / 混合 / 混合+重排）在不同 TopK 下，
前 K 个结果里有没有、排得靠不靠前、混进了多少无关内容。

它**只评检索**，不评回答。回答质量、拒答等属于后面的层次，设计见 `tutorial/gopherlearn-optimization/eval-design.md`。

> **先读这一条**：`-env fake` 用的是离线替身（哈希嵌入、内存索引、词重叠重排）。
> 它的结果**只证明链路和指标计算是对的**，不是检索质量的任何结论，报告里也会带"不得引用"的横幅。
> 真实质量只来自 `-env real`。

## 目录

```
eval/
  cmd/l1/            命令行：check | plan | run | score
  internal/config    评测配置（TOML）与产品配置的读取
  internal/dataset   语料清单、问题集、evidence_sets 的结构校验
  internal/index     通过产品自己的 DocumentIngestor 建索引
  internal/matrix    方法 × TopK 矩阵，每格对应的检索选项
  internal/runner    运行器：缓存、限额、原始记录、manifest
  internal/metrics   覆盖率 / 命中 / 纯度 / MRR / 配对比较
  internal/report    表格与报告
  internal/safety    Redis 守卫（空库或自己的索引才写）
  internal/realenv   真实服务接线（嵌入、重排、Redis）
  internal/fake      离线替身
  internal/pathguard 配置路径不得落在任何 git 工作区内
  testdata/          合成语料（3 篇）与 6 道合成问题，仅用于测试与演示
  config.example.toml
```

它只能用 `GopherAI` 的公开包（`common/rag`、`config`、`service/knowledge`、`model`），
通过 `replace GopherAI => ../v2` 引入；`v2/internal/...` 对它不可见。

## 数据不进仓库

语料、问题集、原始结果和产品配置（含密钥）都是私人数据。**配置里的任何路径只要落在某个 git 工作区内，所有模式都会拒绝启动**
（含符号链接，也含 worktree 里的 `.git` 文件）。所以：

- 把这些文件放在仓库之外，例如 `D:\gopherlearn-eval\`；
- 配置文件用 `.local` 结尾（`*.local` 已被 git 忽略），并同样放在仓库之外；
- 仓库里只有 `config.example.toml` 和 `testdata/` 里的合成数据。

想用 `testdata/` 试一遍，先把它**复制到仓库之外**再指向副本（见"快速开始"）。

## 快速开始（离线、不花钱）

```bash
# 在 eval/ 目录下。D:\gopherlearn-eval-demo 是仓库之外的任意目录。
cp -r testdata /d/gopherlearn-eval-demo/data
cp config.example.toml /d/gopherlearn-eval-demo/eval.toml     # 然后改里面的四个路径
```

`plan` 还要读一份产品配置（评测配置里 `[services].productConfig` 指向的文件），演示用的最小内容是：

```toml
[jwtConfig]
key = "a-jwt-secret-of-at-least-thirty-two-bytes"

[ragModelConfig]
embeddingModel = "synthetic-embedding"
embeddingBaseUrl = "https://embedding.example.invalid/v1"
chatModelName = "synthetic-chat"
baseUrl = "https://chat.example.invalid/v1"
dimension = 1024
```

（`check` 与 `run -env fake` 不读取这个文件，`productConfig` 路径写个占位即可；`plan` 读它是为了取产品的单文件大小上限，且不需要重排令牌。）然后：

```bash
go run ./cmd/l1 check -config D:\gopherlearn-eval-demo\eval.toml     # 校验输入，不连任何服务
go run ./cmd/l1 plan  -config D:\gopherlearn-eval-demo\eval.toml     # 预估块数与调用次数，对照限额
go run ./cmd/l1 run   -config D:\gopherlearn-eval-demo\eval.toml -env fake
go run ./cmd/l1 score -config D:\gopherlearn-eval-demo\eval.toml -run <上一步打印的 run directory>
```

`go test ./...` 全部离线：不调用任何模型，不连接 Redis（唯一的 Redis 集成测试默认跳过，见"测试"）。

## 语料清单 `corpus.json`

```json
{
  "name": "my-notes-2026-10",
  "root": "files",
  "documents": [
    {"file": "go/memory-model.md", "sha256": "ab12…"},
    {"file": "notes/orchard.md"}
  ]
}
```

- `file` 是文档在清单里的名字（斜杠分隔的相对路径，不含 `..`、反斜杠、冒号），**证据里的 `file` 必须与它一致**。
- `root` 可选，相对清单所在目录；缺省就是清单所在目录。
- `sha256` 可选；写了就会校验，文件改动后会报错，而不是悄悄改变结果。
- 只支持产品入库支持的类型：`.md` 与 `.txt`。其它类型在 `plan` 阶段就会报错，不会等到花钱之后。
- 语料哈希只取决于（文件名, 内容哈希）的集合，与清单的排版和顺序无关；它进入索引指纹和每次运行的 manifest。

## 问题集 `dataset.jsonl` 与 `evidence_sets`

每行一个 JSON 对象：

| 字段 | 类型 | 必填 | 含义 |
|---|---|---|---|
| `id` | 字符串 | 是 | 唯一编号 |
| `question` | 字符串 | 是 | 问题原文 |
| `answerable` | 布尔 | 是 | 语料里是否有答案（必须显式写，缺省不当作 `false`） |
| `evidence_sets` | 数组 | 可答题必填；不可答题必须省略或为空 | 见下 |
| `split` | `"dev"` / `"sealed"` | 是 | 调试用 / 封存用（见"封存集"） |
| `topic`、`type` | 字符串 | 否 | 分组标签，仅作记录 |

### `evidence_sets` 的格式

`evidence_sets` 是**备选证据集的列表**：

```
evidence_sets = [ 证据集, 证据集, … ]        # 命中其中任何一个，就算找到了证据
证据集        = [ 证据段, 证据段, … ]        # 同一个证据集里的各段都需要
证据段        = {"file": 文档名, "start": 起始行, "end": 结束行}
```

- 行号**从 1 开始**，**闭区间**（`start` 与 `end` 两行都算），按 `\n` 数行（`\r\n` 算一个换行，末尾没有换行的最后一行也算一行）。
- **一个证据集里的所有段都计入覆盖率**：证据集覆盖率 = 该集所有行里被检索结果包含的比例。只覆盖两段中的一段，是部分覆盖，不是零也不是全部。
- **多个证据集是"备选"**：答案在文档里有两处各自独立的依据时，各写一个证据集；评分取覆盖得最好的那一个，覆盖率并列时取先列出的。
- 行以（文件, 行号）计：不同文件的同号行不会混淆；相邻块重叠的行只算一次。
- 不可答题（`answerable: false`）不写证据，评测只记录它们的 top-1 分数分布。

示例（它们都在 `testdata/dataset.jsonl` 里，对应 `testdata/corpus/` 的合成文档，可以对照检查）：

```jsonl
{"id":"q-neap","question":"How does the tidal range behave during neap tides?","answerable":true,"evidence_sets":[[{"file":"tide-tables.md","start":14,"end":16}]],"split":"dev","topic":"tides","type":"factual"}
{"id":"q-wick-glass","question":"Why does a pointed wick blacken the chimney, and how is the chimney washed?","answerable":true,"evidence_sets":[[{"file":"lantern-maintenance.md","start":8,"end":9},{"file":"lantern-maintenance.md","start":13,"end":14}]],"split":"dev","topic":"lantern","type":"multi-span"}
{"id":"q-fuel","question":"Which fuel does the lantern take, and when may it be topped up?","answerable":true,"evidence_sets":[[{"file":"lantern-maintenance.md","start":18,"end":19}],[{"file":"lantern-maintenance.md","start":16,"end":18}]],"split":"dev","topic":"lantern","type":"alternatives"}
{"id":"q-tungsten","question":"What is the boiling point of tungsten?","answerable":false,"split":"dev","topic":"none","type":"unanswerable"}
```

- `q-neap`：一个证据集，一段（第 14–16 行）。
- `q-wick-glass`：**一个证据集、两段**——两段都需要；只命中其中一段时覆盖率是该集总行数的一部分。
- `q-fuel`：**两个备选证据集**——命中任一个即可，评分取更好的那个。
- `q-tungsten`：不可答，没有 `evidence_sets`。

标注建议：

- **只标正文行。** 块的行号范围覆盖的是正文行；标题行（`## xxx`）有时被并入某个块（该块的范围会包含它），有时只进标题路径而不在任何块的行号内，取决于切块。把标题行放进必须覆盖的范围，会让覆盖率受切块影响。
- 证据段宜小不宜大：段越大，越容易让"覆盖率 ≥ 阈值"退化成"取到整篇文档"。
- 评测读取文件的方式与产品入库一致，所以行号要对着**入库的那个文件**数，不要对着编辑器里经过格式化的副本数。

### 结构校验

加载时会一次性列出所有问题（带行号），包括：`file` 不在清单内、`start < 1`、`end < start`、`end` 超过文件行数、
可答题没有证据集、不可答题带了证据集、`id` 重复或为空、`split` 不是 `dev`/`sealed`、出现未知字段、一行里有多个 JSON。
这只是**结构**校验：它不知道某个行区间是否真的回答了问题，那是标注的责任。

### 封存集

`split: "sealed"` 的题默认不会被跑（`run`/`plan` 默认 `-split dev`）。调参、调配置只看 `dev`；
定稿后再用 `-split sealed` 跑一次，得到最终报告的数字。题集文件的哈希记入每次运行的 manifest，
`score` 发现题集或语料与运行时不一致会拒绝打分。

## 配置 `config.example.toml`

四组设置：

- `[paths]`：语料清单、题集、结果目录（绝对路径，均在仓库之外）。每次运行在结果目录下新建一个 `<UTC时间>-<配置摘要>/`，**永不覆盖**已有目录。
- `[services]`：`productConfig` 指向**产品配置文件**（与产品隔离入口读取的同一格式，由 `config.LoadExplicit` 读取，所以评测不会与产品对模型、维度、重排的读法产生偏差）；`evalUserID` 是评测块所属的用户 id。
- `[matrix]`：`methods`（`vector`/`keyword`/`hybrid`/`hybrid+rerank`）与 `topK`。`hybrid+rerank` 的 TopK 不能超过 24（重排只看 24 个候选）。
- `[metrics]`：`hitThreshold`（命中阈值，默认 0.5，**须在看结果之前定好**；写小数点，`1.0` 而不是 `1`）、`bootstrapSeed`、`bootstrapResamples`。
- `[limits]`：`maxEmbeddingCalls`、`maxRerankCalls`，**默认 0 = 不允许任何付费调用**。配置里出现未知键会报错（拼错限额名不会悄悄回到默认）。

产品配置里：嵌入密钥在 `[isolatedEntry].embeddingKey`；重排令牌**只**从环境变量 `RAG_RERANK_AUTH_TOKEN` 读取，且只在重排启用时。
真实运行要求 `ragModelConfig.dimension = 1024` 且 `redisConfig.db = 0`（产品的块索引固定如此）。

## 四个模式

| 模式 | 作用 | 花钱？ |
|---|---|---|
| `check` | 校验配置、语料清单、题集 | 否 |
| `plan` | 用产品入库路径切块，预估块数、嵌入与重排调用数，对照限额 | 否 |
| `run -env fake` | 在离线替身上跑完整矩阵 | 否 |
| `run -env real` | 在真实服务上跑 | **是** |
| `score -run <目录>` | 把一次运行打成表格，离线、可重复 | 否 |

`-env` 没有默认值，必须显式给出，真实运行不会被误启动。`run` 还有：`-split dev|sealed`、`-rebuild`、`-commit <sha>`（写进 manifest；`go build` 不一定能自动写入提交号）、`-verbose`（保留产品检索的逐次日志，默认静音）。

### 真实运行前

1. `l1 plan` 看预估：嵌入调用 = 建索引（每篇文档 ⌈块数/10⌉ 次）+ 每个不同的问题 1 次；重排调用 = 每个不同的问题 1 次 + 启动检查 1 次。数字是上界，Redis 里已有同指纹索引时建索引的调用为 0。
2. 把 `[limits]` 设成不小于预估的值。**真实运行的计划超过限额时，`run` 在第一次付费调用之前、也在创建结果目录之前就拒绝**（离线 `-env fake` 不受限额约束）。 运行中还有一道兜底：真要超出时那次调用不会发出。
3. 用一个**专用的、空的 Redis 数据库**（库 0）。

### 缓存与付费调用

查询向量与重排结果按内容缓存（失败也缓存，**不重试**）：同一个问题无论出现在多少个格子里，只付一次嵌入、一次重排。
这依赖产品的一条性质：强制重排时重排的候选池总是融合列表的前 24 个，所以 `TopK=12` 与 `TopK=20` 重排的是同一批候选，前者就是后者的前缀。
记录里的 `embedding_calls` / `rerank_calls` 是该条记录实际引起的真实调用数（缓存命中为 0）。

### Redis 守卫

真实运行会往 Redis 写块，所以守卫只在两种情况下允许写：库是**空的**，或库里有本评测留下的指纹键 `eval:l1:fingerprint`。

- 指纹 = 语料哈希 + 嵌入模型 + 维度 + 切块设置 + `EmbeddingContentVersion` + 评测用户。相同则**复用**已有索引（不再付嵌入费）；不同则拒绝，要加 `-rebuild`。
- `-rebuild` 只删除评测用户名下的块与指纹键，别的键不动。
- 库里有数据但没有指纹键（比如你自己应用的数据）：**任何开关都拒绝**。
- 建索引前先写 `building:<指纹>` 标记，成功后才写正式指纹；半途失败的库不会被当成可复用的，也不会被当成"外来数据"，用 `-rebuild` 可修复。

### 运行产物

每个运行目录里：`manifest.json`（提交号、配置摘要、环境 `fake`/`real`、嵌入与重排模型、语料哈希、题集版本、索引指纹、矩阵单元格、计划值）、
`records.jsonl`（每题每格一条原始记录）、`summary.json`（实际调用数、是否完整）。
原始记录只存文档 / 版本 / 行号 / 名次 / 分数，**不含块文本、问题文本和任何密钥**；失败只记录一个短代码（如 `unavailable:embedding_unavailable`），不记录服务商的错误信息。

## 矩阵与重排的两种规则

`hybrid+rerank` 分两行：

- **`production`（生产规则行）**：`RerankDefault`，只有 `TopK ≤ 8` 才重排；更大的 K 产品不重排，该格会被记为 `bypassed`，结果与同 K 的 `hybrid` 相同。
- **`forced`（强制重排行）**：`RerankForceOn`，用来回答"K 更大时重排会不会有用"这个产品自己从不问的问题。`TopK ≤ 8` 时两条规则做的是同一件事，所以强制行只在 `TopK > 8` 时出现，不重复跑。

其余三种方法都强制关闭重排，即使整个运行挂着重排器，也不会影响它们的结果。候选深度沿用产品公式：每个通道取 `max(TopK×6, 30)`，会重排时取 48，封顶 100。

## 指标口径：命中@K 与 MRR 不同

汇总表的表头和脚注**分别**写明两个口径，请不要互换解读：

- **命中@K（覆盖率 ≥ 0.5）**：取该题各个证据集里被前 K 个结果覆盖得最好的一个，其**行覆盖率 ≥ 阈值**（默认 0.5，含等号）即命中。表头里的数字取自 `hitThreshold`。
- **MRR（至少共有 1 行）**：取前 K 个结果里**第一个与任一证据集至少共有 1 行**的结果，计 `1/名次`，没有则计 0，对可答题取平均。

所以一个结果只要与证据共有 1 行，就能让这题的 MRR 大于 0，却不一定让它命中。

其余指标（同样按行计）：

- **平均覆盖率@K**：各题最佳证据集的行覆盖率的平均。
- **平均纯度@K**：前 K 个结果的行并集中，属于最佳证据集的比例（并集为空记 0）。
- **候选池命中**与**失败分类**：对融合后、重排前的候选（至多 24 条）按命中@K 的同一口径算覆盖。命中@K 的题为"正常"；没命中但候选池覆盖了证据为"**排得不好**"（召回了又被排出去）；候选池也没覆盖为"**召回不到**"；检索本身失败的单列为"失败"，按 0 分计入而不是丢弃。
- **不可答题**：只记录 top-1 分数的分布（最小/中位/最大）和空结果数，不据此定拒答阈值。不同方法的分数含义不同，不能跨方法比。

### 配对比较

挑战者（`vector`、`keyword`、`hybrid+rerank` 的各行）与同 K 的 `hybrid` 做配对比较，逐题求差：

- 命中率差、覆盖率差、MRR 差，各给 95% 区间。区间用**按文档分簇的自助法**（重抽的是文档，因为同一文档的问题不独立），种子和次数在配置里，所以报告可逐位复现。只有一个文档时给不出区间（n/a）。
- 命中的显著性用 **McNemar 精确检验**（只看两边命中不一致的题，双侧，对称二项），同时给出"仅挑战者命中 / 仅基线命中"的题数。
- 给出**最小可检测效应**（α = 0.05、效能 80% 的正态近似）：这么多题大约能可靠分辨的最小差值。差值小于它时，"不显著"不等于"没有差异"。它是规划用的估计，不是检验结果。
- 被 `bypassed` 的生产规则行与 `hybrid` 完全相同，不做比较。

## 测试

```bash
go vet ./... && go test -count=1 ./...
```

全部离线：假嵌入、假重排、内存索引。测试覆盖：入库块与直接切块逐块一致（含行号）、块行号与证据行号的约定、
矩阵每格的选项语义（向量格不走关键词通道、关键词格不嵌入查询、`ForceOff`、强制重排的前缀性质）、
缓存使每题只付一次调用且"计划值 = 实际花费"、限额失败即停、失败被记录且不重试、守卫的各种库状态、
指标的手算夹具（单/多证据集、并列、多段必需、跨文件不混行、阈值含等号）、McNemar 与最小可检测效应的手算值。

可选的 Redis 集成测试（`internal/realenv`，默认跳过）：

```bash
GOPHERAI_REDIS_TEST_ADDR=127.0.0.1:6379 GOPHERAI_REDIS_TEST_PASSWORD=… go test -count=1 ./internal/realenv
```

它只在库 0 为空或已是评测索引时运行，否则自动跳过，不会碰你自己的数据。

## 已知限制

- 离线替身的关键词检索是 BM25 风格的子串计数，与 RediSearch 的分词不同；只有 `-env real` 的关键词排名才代表产品行为。
- 记录里的耗时是有缓存时的检索耗时，**不是延迟测量**；L1 只评质量。
- `commit` 默认取构建信息，`go run` 和部分 worktree 里取不到，会记成 `unknown`；真实运行请用 `-commit $(git rev-parse HEAD)`。
- 只评 `.md`/`.txt`；块的行号范围只覆盖正文行（见"标注建议"）。
- `hitThreshold` 是行覆盖率，不判断语义；它与"检索到的内容是否足以回答"不是一回事。
- 真实运行没有放进 CI（要花钱、要 Redis Stack）；CI 的 `eval` 任务只跑上面的离线测试。
