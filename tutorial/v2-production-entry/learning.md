## AGENT-031：整合入口——让默认应用真正跑在原 V2 Eino Runner 上

> 本节由开发对话框按六条面试主线生成。事实来源是 `wt-v2-gopherlearn-integration`（分支 `integration/v2-production-entry`，起点 `e3ad48ef`）里的真实代码与本轮离线测试；凡未跑过的读数都写明未跑。

### 主线一：原项目有什么问题

迁移线已经把 V2 复制进来并做了 hybrid 装配，但"在仓库里"和"在跑的进程里"是两件事：根 `go.mod` 既不 require 也不 replace `GopherAI` 或 `gopherlearn-v2-hybrid`（`go.mod:1-9`），`cmd/gopherlearn/main.go:15-31` 的 import 全在根模块内，`Dockerfile.backend:25` 只构建 `./cmd/gopherlearn`。也就是说**默认二进制里没有一行 V2 业务代码**，而 README/spec 描述的 Eino ReAct、工具治理、记忆生命周期都属于另一个模块的另一个进程。风险不是"少个功能"，而是把旧自建循环（`internal/agent/harness.go:140`）当成迁移结果——两条路的预算语义（`harness.go:33-44` 对 `v2/service/agent/runner.go:76-80`）、工具面（5 个对 6 个）、事件帧、记忆读取模式都不同，混进一句"迁移完成"会让之后所有对比实验的基线失真。

第二问题是三张路由表互不覆盖：`hybrid/http.go:91` 的兼容白名单只有 5 类前缀，根模块的 `/api/v1/collections`、`/api/v1/memories`、`/api/v1/learning/*`、`/sessions/:id/topic-memory` 在 hybrid 进程 404；V2 的 `/api/v1/AI/*`、`/api/v1/knowledge/documents`、`/api/v1/user/*` 在默认进程 404；而候选树的前端已经把 V2 页面加进主导航——**部署出来就是打开即坏**。

第三问题是数据：`users`/`sessions`/`messages`/`session_summaries`/`episode_memories`/`learning_*`/`topic_progress` 两侧同表名不同列（主键 varchar(96) 字符串 ID 对 int64/uint 自增），而根模块**连库即 AutoMigrate**（`internal/repository/gormmysql/connect.go:106-107`）、V2 的 `migration()` 还带数据改写（`v2/common/mysql/mysql.go:87,120-140`）。"换个入口启动"若共用旧库，等于让两个进程按不同 schema 写同一批表，并且 episode/knowledge 两套后台池没有共享开关、租约互不识别。

### 主线二：为什么选择当前方案

目标是**接通已有能力**，不是再实现一遍。所以保留多模块结构，把已经存在的 `hybrid` 提升为正式运行内核——它做的恰好是整合唯一必需的边界转换：会话 mode 双向翻译（`http.go:212-221`、`:245-253`）、复用原 JWT 中间件（含 DB 复验）、原事件名回译（`agent_sse.go:96-140`）、会话删除时清派生索引并按失败上报（`http.go:310-313`）。本轮新增的只有三样：一个命名明确的正式入口（薄转发到既有 `hybrid.Run`，不复制装配）、一份把隔离要求写死的配置模板、一组把"前端默认导航"与"后端路由表"钉住的契约测试。

入口必须是"选进程"而不是"在根模块里改代码"：Eino 只活在 `hybrid`/`v2` 模块，而 `hybrid/go.mod:94,96` 已经 require 根模块，反向 import 会形成模块环。要么保持双模块交付，要么整体折叠——后者是更大的决定。

旧入口保留为基线入口，但两套**不得同时服务同一份数据**：对比实验需要基线可跑（这是项目 AGENTS.md 的硬要求），而同名表与双 worker 决定同库并行只会互相损坏。所以用独立镜像（`Dockerfile.backend-integrated`）、独立 compose 文件与独立项目名/容器名/卷名/网络名把它们物理隔开，而不是靠"记得别同时开"。

### 主线三：核心链路如何运行

Agent 回合：前端 `POST /api/v1/sessions/:id/stream`（会话 mode=`learning_agent`）→ `http.go:91` 判定属兼容面 → `:467 stream()` 读回会话 → mode==Agent → `:476 agentStream()` → 校验 `RunRequest` → `agent_sse.go:175 s.agent.Agent.Run(ctx, request, sink)` → `v2/service/agent/service.go:213` → `:295 Runner.Run` → `runner.go:592 react.NewAgent`（Eino）→ 绑定原工具 → 帧回译 `message.started` / 步骤与公开文本块 / `message.completed`，且在发终态前回读持久化的助手行并比对字节。

普通聊天回合：同一个 URL，mode=`chat` → `http.go:514 s.chat.Stream` → `conversation/conversation.go:46-51`（只装 DirectChat 一个策略）→ 根模块 `MessageService` → 显式配置的 `openai_compatible` 客户端。这条链上没有 Runner、没有工具、没有记忆提供者、没有 MQ 协作者。

启动链：`gopherlearn-integrated -config <file>` → `hybrid/entry.go:20 loadEntry`（原 `entry.Load` + `[ordinaryChat]` 白名单校验 + **migrate=true 直接拒绝**）→ `:81 Assemble` → `entry.Assemble` → `v2/internal/isolated/wiring.go:382`（原资源、5 个后台池、原路由表）→ `conversation.New(同一个 *gorm.DB)` → `newHTTPHandler` → Serve；关停是有界 Shutdown 再回收资源，错误只上报不回填配置值。

### 主线四：关键代码锚点

1. `hybrid/entry.go:27-29` —— 入口在装载期拒绝业务迁移，连显式 `migrate = true` 也拒，所以"启动"永远不等于"改库"。
2. `hybrid/http.go:91-94` —— 一张判定式把请求分给"主线兼容面"与"原 V2 路由表"，这是整个整合的分流点，也是缺接线的唯一来源。
3. `hybrid/agent_sse.go:175` + `v2/service/agent/runner.go:592` —— 兼容面的 Agent 回合落到原 Service.Run，即落到 Eino ReAct；主线契约 URL 与原内核由此接上，而不必改 Runner。
4. `conversation/client.go:13-19,37-39` —— 普通聊天只认那 18 个保留键，且必须显式选择 `openai_compatible`；Agent 预算键写在这里会被装载期拒绝。
5. `hybrid/integrated_surface_test.go` —— 把默认导航要问的 35 条路径逐条对两张表核验，并用 `document_read`（只存在于原 V2 工具面，`v2/service/agent/tool/document_read.go:17`）作为"跑的是原内核而不是自建循环"的区分特征。

### 主线五：并发、失败和边界如何处理

- **同库双写**：整合入口只强制"配置必须显式写出"——schema 名、Redis host/port/db、HTTP 地址、源文件目录缺任何一项都在装载期拒绝（`v2/internal/isolated/settings.go:37-41`），入口永不建表（`migrate=true` 直接拒，`hybrid/entry.go:27-29`）。但入口**认不出**"这个名字属于基线"，所以"不共用旧库"是部署约束：模板给的是整合栈专有取值，部署文件用独立镜像、独立 compose 项目名/容器名/卷名/网络名。搜索隔离按**独立 Redis Stack 实例**做，配置里就是那台实例的 `db = 0`（验收批实际用的也是 db 0）；把非 0 库号当隔离手段是错的，在共用实例上换库号也不会被入口发现。
- **取消与终态**：Agent 流的写失败会立刻取消 ctx 并且不再补发终态帧；失败分类沿用原 `ErrorCodeForAssembly`（busy / conflict / finished / 模型失败映射不同状态码）；普通聊天只在非终态且 ctx 未取消时补一条 `message.failed`。
- **鉴权与用户隔离**：兼容面用原 JWT 中间件，错误信封只在 `CodeInvalidToken` 时反译成 401，其余统一 503（不把内部错误原文交给前端）；会话与历史读写都带 `user_id` 归属条件，删除时派生索引清理失败上报 `503 derived_cleanup_failed` 而不是假装 SQL 回滚。
- **测试证据**：`go test ./hybrid`（无 tag）70 项通过（整合本体 +5：路由表覆盖、暂缓面缺席、两条模型路不串、构建选择新入口、模板可装载；修正轮 +3：独立索引实例与 DB0、栈产物与镜像引用、凭据文件名与被否证的旧名），入口包 3 项，前端 50 文件 1036 项。真实 MySQL／Redis／Provider／Docker 一律**未执行**，所以"能跑起来"仍属未验证；全量 `-race` 撞默认 10 分钟时限记"未完成"，只有受影响路径的定向 race 有通过读数。
- **一个诚实的读数（本轮改正了自己的证明方式）**：本 worktree 按 `core.autocrlf=true` 检出成 CRLF，根模块有 6 条按行解析 `main.go`/`Dockerfile`/`compose`/`nginx` 的测试因此红。上一轮的归因只拿**起点兄弟树**证明，本轮改为按当前候选字节建一份全新 LF 装配（`git -c core.autocrlf=false archive` + 本轮工作字节叠加，登记指纹）再跑同一批检查：6 条全绿、根模块全量套件零 `FAIL` 行，于是"CRLF 形态差异、不是代码回归"这个结论落在被审查的那份字节上。装配的第一次尝试本身是无效读数（`git archive` 也做 eol 转换，产物仍带 CR），这条弯路记在 `verification.md`。本轮没有重渲染工作树去消红，只把差异登记为待决。

### 主线六：为什么不用其他方案、代价是什么

- **折成单模块**：收益是"一条 `go build` 就产出默认应用"。代价是必须先解开模块环（全量重命名 import path）、合并依赖图（gin 1.10.1↔1.11.0、crypto 0.48↔0.53、uuid 1.3↔1.6，外加 eino/toml/golang-jwt/mcp-go）、把 `go 1.24.0` 抬到 1.25 并换镜像，还要让同名表与同名工具在同一个包空间里改名。这等于替三件尚未决定的事默认选边，故不在本轮。
- **只改 Dockerfile 继续构建旧入口**：最省事，但把"迁移完成"建立在错误内核上，正是要消除的误判。
- **当前方案的局限**：兼容面仍只覆盖 5 类前缀，其余靠透传；Collection 主题记忆、旧 `/api/v1/memories` 与 `/api/v1/learning/*` 在整合应用里没有出口，本轮把它们从默认导航撤下并留下断言，而不是假装可用；账号与令牌仍是两套体系（整合入口统一采用 V2 体系），旧用户数据不迁移；前端如果拿这套整合构建去跑旧入口，Collection 面板会是空的——这是已知取舍，已写入交付说明。

### 面试问题与参考回答

**Q1 你这个"整合"到底改了什么？为什么不是把 V2 代码拷进主模块？**
结论：我改的是**入口与边界**，不是业务实现。原因/链路：V2 能力活在 `hybrid`/`v2` 两个模块里，而 `hybrid` 已经依赖根模块，反向 import 会成模块环，所以"接通"只能表现为让正式入口选 hybrid 装配（`hybrid/entry.go:81` → `entry.Assemble` → 原 Runner），并给构建/部署一条明确选它的路径（`Dockerfile.backend-integrated`）。边界/取舍：代价是仍然存在两个二进制、两套配置体系，我用独立镜像与独立 compose 项目名来保证它们不会同库并行，而不是假装已经合并。

**Q2 怎么证明默认 Agent 请求真的跑在原 V2 Eino Runner 上，而不是主线的自建循环？**
结论：靠两条证据链，不靠"看起来对"。原因/链路：路由层用 `gin.Engine.Routes()` 读原 V2 路由表，逐条断言前端默认导航要的 35 条路径要么在 V2 表、要么在 hybrid 兼容表白名单里；行为层断言 Agent 回合绑定过工具集合，且集合里出现 `document_read`——这个工具只存在于原 V2 工具面，主线 harness 没有。边界/取舍：这只证明"离线装配确实走到了原 Runner"，模型效果与真实 Provider 行为仍未验证；另外默认配置（`memoryReadMode=legacy_v2`、`episodeRecallMode=shadow_only`）下 `memory_search` 等工具就是不绑定，我把"未绑定"也钉成断言，防止以后被悄悄放宽。

**Q3 普通聊天为什么还要留主线的 DirectChat？两边不是重复实现吗？**
结论：是刻意分工，不是重复。原因/链路：V2 侧根本没有普通对话路由（`v2/router/isolated.go:14-15` 的注释自己写明普通 AIHelper/TTS/兼容路由缺席），而主线已有完整的消息服务与预算/摘要实现，`conversation/` 只是把它跨模块边界暴露出来，`hybrid/assembly.go:39` 用同一个 `*gorm.DB` 建服务，绝不注入 Runner、工具或记忆提供者。边界/取舍：代价是同一个进程里有两套模型客户端与两套事件帧来源，所以我写了"一条聊天回合不触碰 Agent 模型、一条 Agent 回合不触碰普通模型"的双向断言。

**Q4 两套同名表列不兼容，你怎么保证不会把用户库搞坏？**
结论：靠"拒绝 + 显式 + 命名不共用"三层，但三层的强度不一样。原因/链路：入口在装载期拒绝 `migrate = true`（`hybrid/entry.go:27-29`），并拒绝任何没写出来的强制字段——schema 名、Redis host/port/db、HTTP 地址、源目录缺一项就起不来（`settings.go:37-41`）；但"这个名字是不是旧库"入口判断不了，所以不共用是**部署约束**：模板给的是整合栈专有取值，部署文件用独立镜像/容器/卷/网络名、独立 compose 项目名。边界/取舍：我没有提供建表通道，所以整合入口上线前需要一次**显式**的隔离 schema 初始化——这是故意留下的摩擦，宁可让部署卡住，也不让启动顺带改库；同样地，把"绝对不可能共库"说成代码保证是过头的，本轮已按负责人清单把这类措辞降级。

**Q5 前端接口与后端一致这件事，你用什么防止它再次腐化？**
结论：两侧各一条可失败断言。原因/链路：前端测试挂载真实 Shell，断言启动只发 `/api/v1/me`、不发 Collection 列表，且主导航四项的目标 href 与路由解析结果固定；服务端测试把同一批路径要求逐条对两张路由表核验。边界/取舍：deep link 到旧页面仍会拿到 404/501，这是已知残留——那些页面属于基线入口，我没有用重定向去掩盖，而是在交付说明里点名。

**Q6 这个方案最大的风险或代价在哪？**
结论：两套体系并存的时间被拉长。原因/链路：配置面（env 与 TOML、duration 与整型秒、`_MAX_STEPS` 与 `MAX_GRAPH_STEPS`）、账号体系、路由表都是两套，整合入口只做边界翻译，不做换算和统一，所以每一项都得有人显式决定归一。边界/取舍：我拒绝把"折叠成单模块"当顺手改动，因为它会替三件未决的事默认选边；当前方案用隔离把风险压住，代价是运维要多认一个入口和一个 compose 文件。

## 更正（2026-10-15，负责人交回清单）

上面正文里三处说法按本轮核验降级，正文保留不改写，逐字证据见 `verification.md`：

1. 入口只拒绝"没显式写"与 `migrate=true`；**"不会共享旧 schema"没有判据**，那是不共用取值＋独立命名的部署约束。
2. Redis 隔离改为**独立实例 + `db = 0`**；"非 0 库号才算隔离"的说明与断言删除，索引名/前缀保持生产值。
3. 凭据文件名改正为 `config.integrated.toml.local`（`*.local` 匹配后缀，旧名谁都不命中），错误说明删除并补可失败断言；`.dockerignore` 增加 `**/*.local`。
4. 镜像引用改用负责人批准的摘要（MySQL `62fb722c…`、Redis Stack `39adcbb0…`），去掉未批准的 `mysql:8.4`；摘要逐字与树内已验收证据核对过。
5. 那 6 条 CRLF 假红的归因重做在**当前候选字节**上（全新 LF 装配 + 指纹登记），第一次装配无效的记录保留。

## AGENT-032：常驻部署的凭据接缝——把"配置没到位"变成唯一的启动失败形态

（2026-10-15，起点 `db6c327f`，只做离线收口，不执行真实部署）

### 主线一：原项目有什么问题

已验收批次的口令交接是**一次性批次装置**：`docker exec -i` 把值写进容器私有 tmpfs，门控用完即回收。常驻 `docker compose up` 没有"批后回收"这个生命周期，那一步在常驻形态里没有对应物。上一轮因此把索引写在第二份 compose 文件里、主栈以 `external` 网络借用，健康检查的判据是"应答里出现 `FT._LIST`"这种字符串级要求。真正的洞在镜像入口的语义上：`entrypoint.sh.txt:8-10` 只有在 `/redis-stack.conf` **存在**时才设 `CONFFILE`，否则 `redis-server` 带着零配置启动——也就是说"文件忘了放"不是一起不来的事故，而是一台**没有任何访问控制**的服务；而它仍会被端口级健康检查判成健康。

### 主线二：为什么选择当前方案

接缝必须落在入口真正读的位置，而不是落在"传参数我们更熟"的位置。封存证据里该镜像 `Cmd` 只有 `/entrypoint.sh`、环境里 `REDIS_ARGS` 为空且被入口原样追加到服务命令行（`entrypoint.sh.txt:25-36`），所以 `--requirepass` 或 `REDIS_ARGS` 两条路都会把值放进服务进程 argv——`ps` 与 `docker inspect` 都看得见。留下的形态只有一种：只读挂载 `/redis-stack.conf`，并在**交给镜像入口之前**判这份文件。检查放在容器入口（preflight）而不是放在应用侧，是因为应用侧再严格也已经把一个开放实例拉起来了。

### 主线三：核心链路如何运行

`compose.integrated.yml` 的 redis 服务把 `entrypoint` 换成 `/bin/sh /redis-stack-preflight.sh` → 脚本 `set -eu`（`:16`）后按顺序判存在、可读、非空（`:28-30`）、`requirepass` 恰好一条（`:34-35`）、值非空（`:37-38`）、不是 `REPLACE_*` 占位、是单 token 无引号无空白（`:43-48`）、长度 ≥32 字节（`:49`）→ 全过才 `exec "$ENTRY"`（`:53`）把同一个 PID 交给镜像原入口，且**不追加任何自己的参数** → 服务端从配置文件读到口令。健康门 `redis-stack-healthcheck.sh` 从同一份文件现读值（`:38`），经 `REDISCLI_AUTH` 调 `redis-cli -2 --json FT._LIST`（`:43`），先分类拒掉认证／连不上／模块缺失／`(error)`（`:46-53`），再要求形状是 JSON 数组且剥掉字符串后只剩分隔符（`:58-75`），最后打印 `ok (indexes=N)`（`:77`）。backend 侧 `depends_on: redis: condition: service_healthy`，两个服务同在本栈自有的 `internal: true` 网络上，索引不发布宿主端口。

### 主线四：关键代码锚点

`deploy/redis-stack-preflight.sh:16-53`、`deploy/redis-stack-healthcheck.sh:24-77`、`compose.integrated.yml:75-133`（服务声明与两个挂载）、`redis.integrated.conf.example`（可提交的样例，占位串本身会被拒）、判据 `hybrid/integrated_redis_seam_test.go:141`（结构判据）与 `:535`（挂载源判据）、执行验证 `:721`（跑整脚本的运行器）、`:771`（25 条响应形态）、`:902`（9 条拒绝＋1 条放行＋样例端到端）、忽略匹配 `hybrid/integrated_ignore_matching_test.go`、窄改后的范围规则 `internal/app/retrieval_stack_config_test.go:143`。

### 主线五：并发、失败和边界如何处理

两份脚本各自独立、都从文件现读，不共享任何中间状态，所以不存在"preflight 读到的值和 healthcheck 用的值不一致"这种缝；失败一律只打印类别，不回显文件内容、行或值。边界上刻意保留两件事：**空数组是健康的**（模块活着但还没建索引，是首次部署的真实形态，判它不健康就等于永远起不来），以及**非 0 库号不是隔离**（`indexWiringJudged` 直接拒 `db != 0`，并配一条"改成 db 2"的反例）。`REDISCLI_AUTH` 的成本写在脚本头：那一个探针子进程的环境在同容器同 uid（以及 root）眼里可读，这是常驻形态换掉"批后回收"所付的代价，不是可以宣布消失的成本。

### 主线六：为什么不用其他方案、代价是什么

排掉的三条都有具体理由：`REDIS_ARGS`/`--requirepass` 进 argv（可见面更大，且证据显示入口会原样追加）；`docker exec -i` 注入（无常驻生命周期语义，值还要经宿主 shell）；用官方 `redis` 镜像（答不了 `FT.CREATE`，`image: redis:` 一类在判据里直接拒）。改名换前缀也不采用——隔离来自换一台服务器，索引名保持生产值，并且现在是从 `v2/common/redis/*.go` 的三个常量读回比对，不再靠部署文件里"提过这三个名字"。代价三条：操作人员要自备一份 `.local` 服务端配置；**服务端 `requirepass` 与 `redisConfig.password` 必须逐字相同，而这一点没有任何代码判据**（preflight 只看服务端那一半）；判据是在替身命令与合成口令下执行出来的，真机 `docker compose up` 后镜像入口是否真按这条路取配置仍未验证。

### 面试问题与参考回答

**Q1 健康检查为什么不能用 `redis-cli PING`？**
结论：`PING` 只证明有人在听那个端口，而这条链要的恰恰是"搜索模块能应答"。原因/链路：Redis Stack 的价值在 `redisearch` 模块，模块没装载时服务照样能 `PING`，但 `FT.CREATE` 会返回 unknown command——用 `PING` 当门，backend 会在"健康"的索引上跑到一半才失败，症状看起来像应用 bug。所以我探 `FT._LIST` 并要求答案是字符串数组，`unknown command`／`NOAUTH`／`Could not connect` 各自映射到具名拒绝与非零退出。边界/取舍：代价是健康门依赖 `redis-cli` 与 `-2 --json` 的 RESP2 形态（这套形态在已验收批次里真机核过），以及空数组必须被算作健康。

**Q2 你说"启动前就拒绝"，凭什么不是脚本里写了一句 `exit 1`？**
结论：判据是"镜像入口到底有没有被执行"，不是字符串。原因/链路：测试把容器入口换成一份替身脚本，它会落一个 marker；九条反例（缺文件、空文件、没有 `requirepass`、两条、空值、占位串、带引号、短于 32 字节）每条都断言退出码非零**且 marker 计数仍为 0**；放行那条断言 marker 恰好 1 次、且替身收到的 argv 为空（证明没往服务命令行塞参数）。反向对照也做了：把 `refuse()` 里的 `exit 1` 撤掉，九条全部改报"仍然交接给了入口"，脚本按字节还原后摘要与变异前一致。边界/取舍：这套判据是离线替身下的，真机上"镜像入口是否只读 `/redis-stack.conf`"由封存证据支撑，不是本轮实测。

**Q3 口令的泄漏边界你们怎么量，不是怎么声明？**
结论：让替身客户端自己记录 argv 与自身环境，再去查那三份记录。原因/链路：健康门的通过用例跑完后断言三件事——子进程 argv 恰好是 `-2 --json FT._LIST`（没有值）、脚本 stdout+stderr 不含该值、子进程环境里恰好一条 `REDISCLI_AUTH=<值>`。所以"不进 argv、不进输出、只进那一个子进程环境"是被量出来的三条结论，而不是注释里的承诺；同一条判据也顺手把 compose 文件里出现 `requirepass <值>`、`--requirepass`、`REDIS_ARGS` 三种形态都判红。边界/取舍：容器内同 uid 进程可读到那份环境这件事我没法用离线读数消掉，只能把它写进脚本头和交付说明，留给真机决策。
