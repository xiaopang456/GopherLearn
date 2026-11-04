# GopherLearn 配置参考

正式入口 `hybrid/cmd/gopherlearn-integrated` 的**完整配置参考**。README 只讲「最少要填什么」，这份文档讲「所有字段、默认值、校验规则与可选功能的完整步骤」。

- 配置模板：[`config.integrated.example.toml`](../config.integrated.example.toml)
- 入门步骤：[README §5 配置说明](../README.md#5-配置说明)
- 凭据放哪：[CREDENTIAL-HANDLING.md](CREDENTIAL-HANDLING.md)

本文所有字段名、默认值与校验规则都对照 `config.integrated.example.toml` 与当前 `main` 的代码核实过。

---

## 目录

- [配置从哪读](#配置从哪读)
- [三处模型配置各管什么](#三处模型配置各管什么)
- [完整字段表](#完整字段表)
  - [`[isolatedEntry]`](#isolatedentry)
  - [`[mainConfig]`](#mainconfig)
  - [`[mysqlConfig]`](#mysqlconfig)
  - [`[redisConfig]`](#redisconfig)
  - [Redis 持久化](#redis-持久化服务端的配置不是-toml-里的键)
  - [`[jwtConfig]`](#jwtconfig)
  - [`[knowledgeConfig]`](#knowledgeconfig)
  - [`[ragModelConfig]`](#ragmodelconfig)
  - [`[agentConfig]`](#agentconfig)
  - [`[emailConfig]`](#emailconfig)
  - [`[ordinaryChat]`](#ordinarychat)
- [思考开关与方言](#思考开关与方言)
- [重排（可选）](#重排可选)
- [启动失败提示表](#启动失败提示表)

---

## 配置从哪读

- 正式入口**只读 `-config` 指向的那一个 TOML 文件**。它**不读 `.env` 文件，也不读环境变量里的凭据**（`v2/internal/isolated/settings.go` 里写得很直白：`environment credentials are not read`）。
- **唯一的例外是重排令牌 `RAG_RERANK_AUTH_TOKEN`**：它是密钥，不能写进配置文件，所以开启重排时从环境变量读这一个。聊天与 embedding 的密钥仍然只从配置文件读。
- 因此：**`.env.provider.local` 是历史基线入口的东西，正式入口不会自动读取它**。要让正式入口拿到模型配置，只能写进 `config.integrated.toml.local`。
- 真实配置请放在 `config.integrated.toml.local`：后缀 `.local` 同时命中 `.gitignore` 与 `.dockerignore` 的 `*.local` 规则，不会被提交，也不会进构建上下文。
- 模板文件里只有占位符；**任何真实凭据都不要写进模板、日志或报告**。

---

## 三处模型配置各管什么

同一个 chat 模型的名字通常要写三处，但**决定用哪个模型的只有两处**——这是最容易填错的地方。三段各管一段：

| 位置 | 管谁 | 取哪些字段 |
|---|---|---|
| `[isolatedEntry]` | **学习 Agent** 的 chat 客户端 | `chatKey`、`chatModel`、`chatBaseURL` |
| `[ragModelConfig]` | **embedding 服务**的模型名/地址/密钥/维度（四件都在这一段）；情景关系分类器**身份标签**里的模型名（不发给 provider，见下表）；思考开关的方言 | `embeddingModel`、`embeddingBaseUrl`、`embeddingApiKey`、`dimension`、`chatModelName`、`chatThinkingDialect` |
| `[ordinaryChat]` | **普通聊天**（直连，不经 Agent 回路）的 chat 客户端 | `GOPHERLEARN_OPENAI_BASE_URL`、`GOPHERLEARN_OPENAI_API_KEY`、`GOPHERLEARN_OPENAI_MODEL` |

**同一个 chat 模型要写两遍**：学习 Agent 与普通聊天各自配置，所以大多数部署会把 `[isolatedEntry]` 与 `[ordinaryChat]` 填成同一个模型、同一个地址。embedding 是另一件事，用同一家或另一家都行。

代码上的对应关系：

| 字段 | 被谁读 | 位置 |
|---|---|---|
| `[isolatedEntry].chatKey` / `chatModel` / `chatBaseURL` | 构造学习 Agent 的 chat 客户端 | `v2/internal/isolated/dependencies.go`（`ProductionDependencies` → `ChatModel`） |
| `[ragModelConfig].embeddingApiKey` | 构造 embedder，与它认证的模型名、地址在同一段 | `v2/internal/isolated/dependencies.go`（`ProductionDependencies` → `Embedder`）；是否必填由入口判（`v2/internal/isolated/settings.go`），因为默认配置不该自带密钥 |
| `[ragModelConfig].chatModelName` | 情景记忆**关系分类器的身份标签**——它不进请求体，答问题用的模型来自 `[isolatedEntry].chatModel` | `v2/internal/isolated/wiring.go`（`episoderelation.NewChatClassifier` 的 `Chat` 是 Agent 那个客户端，`ModelName` 只拼进 `identity`） |
| `[ragModelConfig].chatThinkingDialect` | 决定思考开关怎么写进请求体 | `v2/internal/isolated/dependencies.go` → `aihelper.ThinkingExtraFields` |
| `[ordinaryChat].*` | 构造普通聊天的 OpenAI 兼容客户端 | `conversation/client.go` → `internal/openai` |

> 曾经有一个 `ragModelConfig.baseUrl`：**没有任何客户端从它构建**，只在启动校验里要求非空。它已被删除，留着它的配置文件会被**指名拒绝**并说明地址该写在哪里（`isolatedEntry.chatBaseURL`）。宽容地忽略才是危险的一条：那会让运维以为配置了地址，而程序用的是别处。
>
> `chatModelName` 同理容易被误解：它**不是**发给 provider 的模型名，只被拼进情景关系分类器的身份字符串（`relation_<名字>_<版本>`），身份是「关系判定提案」的键。所以把它写成与 `chatModel` 不同的值**不会报错**，但会出现"提案键写着 A 模型、实际答题的是 B 模型"——而"模型升级就换键"这个设计正是靠它俩一致才成立。**请与 `[isolatedEntry].chatModel` 写成同一个值。**

---

## 完整字段表

「必填」一列的判定依据：入口在**装载期**（`v2/internal/isolated/settings.go` 的 `Load` 与 `Validate`）会拒绝的配置。

### `[isolatedEntry]`

这一段是正式入口自己的信封，与历史基线入口无关。

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `isolated` | ✅ | — | 必须写 `true`。写成 `false` 启动被拒（`isolatedEntry.isolated must be explicitly true`） |
| `migrate` | ✅ | — | 必须写 `false`。正式入口是业务迁移的**拒绝方**，写 `true` 启动被拒（`hybrid entry refuses business migration`） |
| `chatKey` | ✅ | — | 学习 Agent 的 chat 密钥，不能为空 |
| `chatModel` | ✅ | — | 学习 Agent 的 chat 模型名，不能为空 |
| `chatBaseURL` | ✅ | — | 学习 Agent 的 chat 服务地址，不能为空。末尾不必带 `/chat/completions`，客户端会自己拼 |

`chatKey` / `chatModel` / `chatBaseURL` 任何一项为空，或 `[ragModelConfig].embeddingApiKey` 为空，都会得到 `explicit model settings required; environment credentials are not read`。嵌入密钥为什么写在 `[ragModelConfig]` 而它的"必填"由入口判：默认配置不该自带密钥，而只读模型名/地址的工具（建表命令、离线规划）也要能加载同一份文件。

### `[mainConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `host` | ✅ | — | 后端监听地址，不能为空 |
| `port` | ✅ | — | 后端监听端口，必须大于 0 |
| `appName` | | 空 | 应用名 |

模板给的是 `127.0.0.1:18082`（避开历史基线后端的 8080）。容器里跑要改成 `0.0.0.0:8080`，因为共用的 `nginx.frontend.conf` 反代到 `backend:8080`。

### `[mysqlConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `host` | ✅ | — | 不能为空 |
| `port` | ✅ | — | 必须大于 0 |
| `user` | ✅ | — | 不能为空 |
| `databaseName` | ✅ | — | 不能为空。**整合栈独占的 schema 名**；入口只强制「必须显式写」，写旧库名它不会拦 |
| `charset` | ✅ | — | 不能为空，惯例 `utf8mb4` |
| `password` | | 空 | 启动**不检查**它，但填不对就连不上库 |

### `[redisConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `host` | ✅ | — | 不能为空 |
| `port` | ✅ | — | 必须大于 0 |
| `db` | ✅ | — | 必须是显式写出的键（值可以是 0） |
| `password` | | 空 | 启动**不检查**，但必须与实例的 `requirepass` **逐字一致**，否则表现为检索不可用。`deploy/redis-stack-preflight.sh` 要求服务端那个值至少 32 字节且不是占位串 |

隔离靠**独立实例**，不靠库号：在共用实例上选一个非 0 库不是隔离，也不会被入口发现。

#### Redis 持久化（服务端的配置，不是 TOML 里的键）

`[redisConfig]` 只管**连到哪一台**，不管那一台**怎么存**。存的方式在 Redis 服务端这一侧，
它决定「Redis 重启之后检索索引还在不在、还全不全」——MySQL 里记着「文档已就绪」，
Redis 里如果少了几百个块，检索会悄悄变差，而且**不会有任何报错**。

服务端这一侧要满足三件事：

| 要做到 | 怎么做 |
|---|---|
| 写入不只活在内存里 | 服务端配置里开 `appendonly yes` + `appendfsync everysec`（最多丢一秒的写入）；RDB 快照也保留 |
| 正常停止不丢数据 | 用 `deploy/redis-stack-start.sh` 起服务，别用镜像自带的入口 |
| 数据目录在容器外 | 把数据目录挂到 `/data`（compose 文件里的 `gopherlearn-integrated-redis-data` 卷） |

镜像自带的入口脚本把 `redis-server` 作为 **shell 的子进程**跑起来：停止信号先到 shell，
shell 死掉，服务还活着，容器在宽限期结束后被强杀——服务来不及写最后那张快照，
**正常停止就会丢掉上次快照之后的全部写入**。`deploy/redis-stack-start.sh` 做的是同样的事，
但最后一步用 `exec` 交出进程，服务自己就是 1 号进程，收到停止信号后写完快照再退出。

服务端配置的模板是 [`redis.integrated.conf.example`](../redis.integrated.conf.example)，
里面已经带上了这三项持久化设置；复制成 `redis.integrated.conf.local` 时不用删。
**老部署的配置里没有这三行**：preflight 会在启动时打印一行提示（分类信息，不含配置内容），
但不会拒绝启动；照上面的说法更新配置并重启一次即可，AOF 会把重启前没落盘的内容补上。

完整的启动与验证方式见 [整合栈部署参考 · 搜索索引](INTEGRATED-DEPLOYMENT.md#4-搜索索引redis-stack)。

### `[jwtConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `key` | ✅ | — | 令牌签名密钥，**至少 32 字节**；`GopherAI-v1` 这个已退役的默认值被直接拒绝。更换后所有已签发令牌失效 |
| `expire_duration` | | `24` | 令牌有效期，单位小时，取值必须在 **1..168** 之间 |
| `issuer` | | `gopherai` | 不能为空。取值本身不是隔离判据 |
| `subject` | | `gopherai-user` | 不能为空 |

### `[knowledgeConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `storageDir` | ✅ | `data/knowledge` | 上传原文件的存放目录，需要可写 |
| `maxFileSizeBytes` | | `5242880`（5 MiB） | 单文件上限，必须大于 0 |
| `maxDocumentsPerUser` | | `100` | 每账号文档数上限，必须大于 0 |
| `maxTotalSizeBytes` | | `52428800`（50 MiB） | 单账号总量上限，必须 ≥ `maxFileSizeBytes` |
| `workerCount` | | `2` | 索引工作线程数，必须大于 0 |
| `queueSize` | | `100` | 索引队列长度，必须 ≥ `workerCount` |
| `scanIntervalSeconds` | | `5` | 扫描间隔（秒），必须大于 0 |
| `indexTimeoutSeconds` | | `300` | 单次索引超时（秒），必须大于 0 |
| `executionMode` | | 空 | 这个版本**只提供进程内处理**：省略或 `legacy` 都是同一个进程内处理；写出别的值（例如从旧配置继承下来的其它取值）会在启动时被拒绝，而不是悄悄降级成本地处理 |

文档状态由文档所有者自己的接口查看：`GET /api/v1/knowledge/documents/:id/processing`。

### `[ragModelConfig]`

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `embeddingModel` | ✅ | `qwen3.7-text-embedding` | embedding 模型名，不能为空 |
| `embeddingBaseUrl` | ✅ | `https://dashscope.aliyuncs.com/compatible-mode/v1` | embedding 服务地址，不能为空。客户端会自己拼 `/embeddings` |
| `dimension` | ✅ | `1024` | 向量维度，必须大于 0，且**必须与模型实际输出一致**；改它等于改索引 schema，和已有向量对不上 |
| `chatModelName` | ✅ | `qwen3.8-max` | 情景关系分类器**身份标签**里的模型名，不能为空；**它不决定答题用哪个模型**（那个来自 `[isolatedEntry].chatModel`），但必须与它写成同一个值，否则提案键与实际模型不一致。见上面「三处模型配置各管什么」 |
| `embeddingApiKey` | ✅（入口判） | 空（**不给默认值**） | embedding 服务的密钥。它与模型名、地址在同一段；正式入口要求它非空 |
| `chatThinkingDialect` | | `qwen` | 思考开关的方言，取值 `qwen` / `deepseek` / `none`，**写错的值在启动时就被拒** |
| `rerankEnabled` | | `false` | 是否开启重排 |
| `rerankProvider` | | `local` | 重排服务形态：`local`（本地模型服务）或 `dashscope`（远程重排 API） |
| `rerankEndpoint` | 开启重排时 ✅ | `http://host.docker.internal:18081` | 服务地址。本地服务填服务根地址，远程服务填**完整重排 URL** |
| `rerankModel` | 远程时 ✅ | 空 | 远程服务要用的模型名；本地服务钉死一个模型，忽略这个字段 |
| `rerankTimeoutMs` | | 远程 `3000`、本地 `30000` | 单次调用上限（毫秒），不能为负。留空即用该形态的默认值。一次重排超时或服务端不可用时会立刻再试一次（再试最多 3 秒，且本轮剩余时间不足 5 秒就不再试）；仍然失败或遇到限流、凭据被拒等错误，这一次检索退回未重排的顺序继续，不会让整轮失败，知识检索那一步的摘要会写明「本次重排失败，使用未重排结果」 |

### `[agentConfig]`

整段省略就沿用 `v2/config/config.go` 的原有默认。这里只列常用与值得知道的。

| 键 | 默认值 | 说明 |
|---|---|---|
| `agentThinkingEnabled` | `false` | **学习 Agent 的思考开关**。打开后 Agent 自己那几次模型调用会带上思考开关；**强制指定工具的那一次仍然不带思考**（供应商在思考模式下会拒绝指定工具的 `tool_choice`）。后台任务（摘要、情景投影、关系分类等）不读这个开关，永远是关闭的 |
| `maxToolCalls` | `3` | 单轮最多工具调用数 |
| `maxModelCalls` | `4` | 单轮最多模型调用数 |
| `maxGraphSteps` | `10` | 单轮最多图步数 |
| `deadlineSeconds` | `45` | 单轮时间预算（思考关闭时） |
| `deadlineThinkingSeconds` | `60` | 单轮时间预算（**思考开启时**）。思考开启的回答要等思考写完再写，实测最后那一次生成本身就要四十秒左右，用 45 秒会把这些轮次切在答案出现之前；思考关闭的轮次仍用 `deadlineSeconds` |
| `knowledgeDefaultTopK` | `5` | 知识库默认取几段 |
| `knowledgeMaxTopK` | `8` | 知识库最多取几段 |
| `knowledgeRelevanceThreshold` | `0.80` | **服务端预检索的相关性门槛**。模型回答之前，服务端先拿这个问题去检索学习者的笔记，重排分数高于这个值的片段才会作为证据放进提示词；低于门槛就不给证据，只在窗口里说明"这次没找到高相关内容"。取值 0 到 1，**0 表示完全不启用预检索**（没有重排服务的部署本来也拿不到重排分数）。**注意**：预检索跑不起来时（没有重排、重排或向量化失败），退回的是**改动前的强制检索规则**——除寒暄、回忆对话、明说别查、别人的资料外，其余问题仍然强制先查一次笔记；预检索真正跑起来（命中或低于门槛）之后，才由模型自己决定要不要再搜 |
| `memoryReadMode` | `legacy_v2` | 长期记忆的整条读取路径模式 |
| `episodeRecallMode` | `shadow_only` | 情景召回模式 |
| `episodeRelationMode` | `off` | 情景关系抽取模式 |
| `summaryGroupItemLimit` | `20` | 摘要里**每一组**最多留多少条。超出时按「留最新、丢最早」截断，**整份摘要不会被丢掉** |
| `summaryOpenLoopsItemLimit` | `20` | 摘要里「待办」这一组最多留多少条。待办涨得最快，单独给一个上限，方便实测发现砍太狠时调大 |
| `summaryEvidenceIDLimit` | `64` | 整份摘要最多引用多少条**不同的消息编号**。超出时丢最旧的条目 |

三个上限是**预算**，不是"合法/非法"的开关：以前某一组超限会让整份摘要被拒，于是这个会话的摘要从此不再更新，只能退回旧摘要加原始消息。现在超限走截断，被截掉的部分会在日志和指标里如实记一笔（`summary_truncated`），也会在评测报告里单独计数。

- 把 `summaryOpenLoopsItemLimit` 调大时，`summaryEvidenceIDLimit` 通常也要一起调大：每条待办至少引用一条消息，条目多了、编号上限没跟上，结果还是会被引用上限截掉。
- 上限调小 = 少留历史、省 token；上限调大 = 多留历史，但摘要更长、更占上下文预算。

`maxToolCalls` / `maxModelCalls` / `maxGraphSteps` 三个数必须**互相够得着**，否则启动报错并说明是哪一处不够：

- `maxModelCalls` 至少是 `maxToolCalls + 1`：每轮工具都要一次"决定调用工具"的生成，最后还要一次生成来写答案。
- `maxGraphSteps` 至少是 `maxModelCalls + maxToolCalls`：ReAct 图里每个模型调用和每轮工具各算一个节点。

例如想放宽到 5 次工具调用，就写 `maxToolCalls = 5`、`maxModelCalls = 6`、`maxGraphSteps = 16`；只写前一个，另外两个够不着，新名额是纸上的。

`memoryTopicStandout` 与 `memoryTopicMargin` **没有默认值**：启用相关模式却不给出这两个数，是启动失败。

### `[emailConfig]`

全仓库**只用于一件事：发送注册验证码**。

| 键 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `email` | 否 | 空 | 发件地址（同时用作 SMTP 登录用户名）。留空即视为未配置 |
| `authcode` | 否 | 空 | 该邮箱的 SMTP 授权码（**不是登录密码**）。留空即视为未配置 |
| `host` | 否 | `smtp.qq.com` | SMTP 主机。留空用历史默认值 |
| `port` | 否 | `587` | SMTP 端口。留 0 用历史默认值 |

- **不配**：服务器照常启动，**已有账号照常使用**，登录、会话、文档、记忆、学习、Agent 全部可用；只有「新用户注册」走不通，此时 `POST /api/v1/user/captcha` 返回业务码 `2012`（邮件服务未配置），而不是含糊的「服务繁忙」。
- 只想在本机验证注册链路时，把 `host`/`port` 指向一个本地 SMTP 捕获服务即可，不需要真的把邮件发出去。

### `[ordinaryChat]`

这一段是**普通聊天**（直连，不经 Agent 回路）的配置。**键名是白名单**：白名单外的键在装载期直接被拒（`unsupported ordinaryChat setting`）。

必填四项：

| 键 | 必填 | 说明 |
|---|---|---|
| `GOPHERLEARN_AI_PROVIDER` | ✅ | 必须是 `openai_compatible`（其它值启动被拒） |
| `GOPHERLEARN_OPENAI_BASE_URL` | ✅ | 不能为空；必须是 http(s) 且不带凭据的 URL |
| `GOPHERLEARN_OPENAI_API_KEY` | ✅ | 不能为空 |
| `GOPHERLEARN_OPENAI_MODEL` | ✅ | 模型名，不能为空 |

可选项（留空即用默认值；**默认值以代码为准，不是模板里注释掉的示例值**）：

| 键 | 默认值 | 取值范围 / 说明 |
|---|---|---|
| `GOPHERLEARN_OPENAI_ENABLE_THINKING` | 未设置 | `true` / `false`。**未设置时这个字段根本不上线**（它不是 OpenAI schema 的一部分，严格的 provider 可能拒绝） |
| `GOPHERLEARN_OPENAI_THINKING_DIALECT` | `qwen` | `qwen` / `deepseek` / `none`，其它值启动被拒 |
| `GOPHERLEARN_OPENAI_TEMPERATURE` | `0`（即「不指定」） | `0..2`。留空等于「什么都不问」，沿用 provider 自己的默认 |
| `GOPHERLEARN_OPENAI_MAX_OUTPUT_TOKENS` | `1024` | 必须是正整数 |
| `GOPHERLEARN_OPENAI_TIMEOUT` | `30s` | 必须是正的 Go duration。它同时约束「等响应头」和「流已开后等下一段」 |
| `GOPHERLEARN_CONTEXT_MAX_MESSAGES` | `12` | 必须是正整数 |
| `GOPHERLEARN_CONTEXT_MAX_RUNES` | `6000` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_TRIGGER_MESSAGES` | `12` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_TRIGGER_TOKENS` | `6000` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_KEEP_RECENT_MESSAGES` | `8` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_MAX_INPUT_TOKENS` | `8000` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_MAX_OUTPUT_TOKENS` | `2000` | 必须是正整数 |
| `GOPHERLEARN_SUMMARY_TIMEOUT` | `20s` | 必须是正的 Go duration（如 `20s`） |
| `GOPHERLEARN_SUMMARY_STAGE_BUDGET` | `8s` | 必须是正的 Go duration |
| `GOPHERLEARN_SUMMARY_FAILURE_COOLDOWN` | `30s` | 必须是正的 Go duration |

> 上下文预算同时按**条数**和**字符数**限制：只限一条会被另一条限住（12 句寒暄，或两个巨大的回答）。

---

## 思考开关与方言

`[ragModelConfig].chatThinkingDialect` 决定思考开关在请求体里怎么写：

| 取值 | 写法 | 说明 |
|---|---|---|
| `qwen` | `enable_thinking`（Qwen 扩展） | 默认值 |
| `deepseek` | `thinking = {type = "enabled"｜"disabled"}`（DeepSeek 扩展） | DeepSeek 用这个 |
| `none` | 完全不发思考开关 | ⚠️ 不建议对 DeepSeek 用，见下 |

**换模型服务时按对方认的写法填**：认不出来的字段会被忽略，而被忽略意味着沿用对方自己的默认值（**DeepSeek 的默认值是思考开**）。

### DeepSeek 的特殊要求：`reasoning_content`

DeepSeek 在多轮工具调用里要求每一轮 assistant 都带一个 `reasoning_content` 字段，少了或为 `null` 都会被拒：

```text
HTTP 400  The `reasoning_content` in the thinking mode must be passed back to the API.
```

客户端会把**思考过**的那一轮的思考内容带回去。还有一种轮次本来就没有思考内容：Agent 先问一个指定工具，那一次调用是**关着思考**的（DeepSeek 不接受「思考模式 + 指定工具名」的 `tool_choice`），所以那一轮没有思考内容；紧接着的自由路径是开着思考的，它把那一轮带出去。

客户端会**给这一轮补一个空的 `reasoning_content`**——空字符串，因为这一轮确实没有思考内容，DeepSeek 接受空字符串、不接受缺失或 `null`。

这个补字段有三个精确的性质：

- 发生在请求体序列化之后、发出之前；只动需要补的那一轮消息，其它内容（字段顺序、数字格式、空格）**一个字节都不变**；
- 已经带着 `reasoning_content` 的消息（不管值是真实思考内容、空字符串还是 `null`）**一律不碰**；
- 只在**方言是 `deepseek` 且这次请求思考开着**的时候才注册。方言不是 `deepseek`，或者思考关着，请求体与之前**逐字节一致**，走的是原来那条路径。

**普通聊天路径不受影响**：它不做工具调用，没有需要回传的 assistant 轮，DeepSeek 上可以开思考。

> DeepSeek 一律用 `chatThinkingDialect = "deepseek"`，**不要用 `none`**：`none` 不发任何思考字段，DeepSeek 会沿用自己的默认（思考开），而 Agent 强制指定工具的那一次要发指定工具名的 `tool_choice`，在思考模式下会被拒。

---

## 重排（可选）

重排是**第二阶段召回**：先把候选切片按向量相似度取回来，再用交叉编码器对「问题 × 切片」逐对打分重排。**默认关闭**，关闭时行为与不部署重排完全一致。

### 方式 A · 远程重排 API（推荐，无需显卡）

远程重排是大多数部署应该选的路：**三行配置 + 一个环境变量**，不需要显卡、不需要本地模型、不需要额外容器。

**第 1 步 · 在配置文件里写地址与模型名**

```toml
[ragModelConfig]
rerankEnabled = true
# 服务形态：dashscope = 远程重排 API（阿里云百炼）；local = 本地模型服务
rerankProvider = "dashscope"
# 远程服务给出的完整重排地址（不是服务根地址）。
# 百炼的地址与你的 chat / embedding 服务是同一个域名，把业务空间 ID 换上去即可：
rerankEndpoint = "https://<你的业务空间ID>.cn-beijing.maas.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank"
# 要用的重排模型名，例如 qwen3-rerank
rerankModel = "qwen3-rerank"
# 单次调用上限（毫秒）。留空即用默认值：远程 3 秒、本地 30 秒
# rerankTimeoutMs = 3000
```

> **远程默认 5 秒是实测出来的**：24 条候选连测 5 次，中位数 **0.52 秒**、最慢 **0.89 秒**，
> 5 秒留足了抖动与限流余量，又不会让一个卡住的请求把一次问答吊在那里。
> **本地跑 CPU 时请调大**（默认 30 秒仍可能不够，见下面的方式 B）。

> **地址怎么填**：重排地址就是上面那串 `.../api/v1/services/rerank/text-rerank/text-rerank`，
> **和你 `[ragModelConfig]` 里 chat / embedding 用的域名是同一个**（同一个业务空间、同一把 Key）。
> 把 `<你的业务空间ID>` 换成你 chat 地址里那一段即可。

**第 2 步 · 把 API Key 放进环境变量**

重排令牌是密钥，**不写进配置文件**。启动前设置：

```bash
# Bash
export RAG_RERANK_AUTH_TOKEN='<你的重排服务 API Key>'
```

```powershell
# PowerShell
$env:RAG_RERANK_AUTH_TOKEN = '<你的重排服务 API Key>'
```

至少 32 字节。**不设这个变量就不会启动**，并会明确告诉你缺它（见下面「启动失败时怎么读提示」）。

**第 3 步 · 照常启动**

```bash
# 在仓库根目录执行
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
```

启动时会发**一次**最小重排请求做自检（1 个问题 + 1 条切片）：这一步同时证明密钥、模型名与网络通路都是通的，它消耗部署者的一点额度。**自检失败就不会启动**，具体原因见下面「启动失败时怎么读提示」。

**要知道的两件事**：

- **切片正文会发给这家 provider**。embedding 已经把你的切片发给 embedding provider 了，重排是同一类数据外流——如果你要求数据完全不出本机，请用下面的方式 B。
- **重排消耗部署者的额度**，和 chat / embedding 是同一笔账；注册用户不消耗你的额度。

### 方式 B · 本地重排服务（数据不出本机）

本地跑一个 `text-embeddings-inference` 容器，进程只监听本机：

```bash
docker run -d --name gopherlearn-reranker -p 127.0.0.1:18083:80 \
  --gpus all \
  ghcr.io/huggingface/text-embeddings-inference:120-1.9 \
  --model-id BAAI/bge-reranker-v2-m3 \
  --revision 953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e \
  --api-key '<与 RAG_RERANK_AUTH_TOKEN 相同的值>'
```

```toml
[ragModelConfig]
rerankEnabled = true
rerankProvider = "local"
rerankEndpoint = "http://127.0.0.1:18083"
```

- **显卡**：有 NVIDIA 显卡就加 `--gpus all`；没有就**去掉这一行**，服务会退回 CPU 运行——能跑，但慢得多：实测一次 24 条候选在 CPU 上要十几秒。**CPU 场景请把 `rerankTimeoutMs` 调大**（默认 30 秒，必要时调到 60 秒以上），否则重排会超时降级成「看似开启实则没生效」。
- **端口**：容器内是 80，宿主映射成 18083；后端在容器里跑时把地址换成 `http://host.docker.internal:18083`。
- **令牌**：容器用 `--api-key` 收同一个口令，后端用 `RAG_RERANK_AUTH_TOKEN` 发同一个口令。
- 本地服务钉死一个模型，`rerankModel` 不需要填。

---

## 启动失败提示表

启动失败时终端会给出**一句话的原因**，而不是笼统的「启动失败」。

| 提示里的关键词 | 意思 | 怎么办 |
|---|---|---|
| `explicit independent setting required: <段.键>` | 某个必须显式写出的键没写 | 按上表补上那个键（多半是 `[ragModelConfig].dimension` 或 `[redisConfig].db`） |
| `isolatedEntry.isolated must be explicitly true` | `[isolatedEntry].isolated` 不是 `true` | 改成 `true` |
| `hybrid entry refuses business migration` | `[isolatedEntry].migrate` 写成了 `true` | 改成 `false` |
| `explicit model settings required; environment credentials are not read` | chat 三项之一为空，或嵌入密钥为空 | 补上 `[isolatedEntry]` 的 `chatKey` / `chatModel` / `chatBaseURL`，以及 `[ragModelConfig].embeddingApiKey`；**环境变量不算数** |
| `explicit independent MySQL host/port/database/user/charset required` | `[mysqlConfig]` 有一项为空 | 补上 |
| `explicit independent Redis host/port required` | `[redisConfig]` 的 host 或 port 为空 | 补上 |
| `explicit HTTP host/port required` | `[mainConfig]` 的 host 或 port 为空 | 补上 |
| `chat model name is required` | `[ragModelConfig].chatModelName` 为空 | 补上（它与 `[isolatedEntry].chatModel` 写成同一个值） |
| `retired configuration setting: ragModelConfig.baseUrl was removed …` | 文件里还留着已废弃的 `baseUrl` | 删掉那一行；Agent 的地址只来自 `[isolatedEntry].chatBaseURL` |
| `retired configuration setting: isolatedEntry.embeddingKey moved …` | 嵌入密钥还写在 `[isolatedEntry]` | 挪成 `[ragModelConfig].embeddingApiKey` |
| `embedding model name, base URL and positive dimension are required` | `[ragModelConfig]` 的 embedding 三项有一项不对 | 补上，`dimension` 必须大于 0 |
| `JWT_SECRET must contain at least 32 bytes` | `[jwtConfig].key` 短于 32 字节 | 换成一个至少 32 字节的随机串 |
| `the retired default JWT secret is forbidden` | `[jwtConfig].key` 用了已退役的默认值 | 换一个新的 |
| `expire_duration must be between 1 and 168 hours` | 令牌有效期超出范围 | 改成 1..168 之间的数 |
| `issuer and subject must not be empty` | `[jwtConfig].issuer` 或 `subject` 为空 | 补上 |
| `storageDir must not be empty` | `[knowledgeConfig].storageDir` 为空 | 补上 |
| `executionMode must be empty or "legacy"` | 写了当前版本不支持的处理模式 | 删掉这一行（省略就是进程内处理） |
| `the chat thinking dialect must be qwen, deepseek or none` | `chatThinkingDialect` 拼错了 | 改成 `qwen` / `deepseek` / `none` 之一 |
| `unsupported ordinaryChat setting` | `[ordinaryChat]` 里出现了白名单外的键 | 只保留白名单里的键（见上表） |
| `explicit openai_compatible ordinary client required` | `[ordinaryChat].GOPHERLEARN_AI_PROVIDER` 不是 `openai_compatible` | 改成 `openai_compatible` |
| `the rerank configuration is invalid` | 重排配置本身有问题：provider 拼错、缺 `rerankModel`、超时为负 | 核对 `ragModelConfig` 的重排字段 |
| `RAG_RERANK_AUTH_TOKEN` | 没设这个环境变量，或它不足 32 字节 | 按上面的第 2 步设置环境变量 |
| `the rerank service refused the credential` | 服务不认这个 Key | 核对 Key 是否用在这个服务上（本地服务要同时配 `--api-key`） |
| `the rerank service is rate limiting` | 被限流 | 稍后重试，或提高该 provider 的额度 |
| `the rerank service did not answer inside the configured timeout` | 太慢 | 调大 `rerankTimeoutMs`，或换一个更快的重排服务 |
| `the rerank service serves a different model` | 本地服务跑的不是钉死的那个模型 | 用上面的 `--model-id` / `--revision` 重启容器 |
| `the rerank service is unavailable` | 地址不通、服务没起 | 核对地址、端口与服务是否在运行 |
| `GOPHERLEARN_OPENAI_TIMEOUT must be a positive Go duration` | 普通聊天的超时写法不对 | 用 `30s` 这类 Go duration |
| `GOPHERLEARN_OPENAI_TEMPERATURE must be a number between 0 and 2` | 温度超范围 | 改成 `0..2` |
| `GOPHERLEARN_OPENAI_MAX_OUTPUT_TOKENS must be a positive integer` | 输出上限不是正整数 | 改正 |
