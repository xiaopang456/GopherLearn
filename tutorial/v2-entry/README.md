# V2 隔离后端入口装配交付

本轮基底：`303b5564d0d822bf7e7d21389e6f8c48ee670ebc`。工作树 `<工作区>/wt-v2-direct-copy`，分支 `migration/v2-direct-copy`。第一阶段提交 `110639b30ad4973188bd525e60d4e9d9c9b5abe5` 固定显式配置、入口和资源生命周期；第二阶段提交包含本文及原服务装配、原生路由和永久回归，以该提交的 HEAD 为交付固定点。

本轮只证明隔离入口的离线装配链。没有接入现有默认 main，没有改 Vue、Collection、API 兼容层或已确认的 SSE，没有业务数据库迁移、真实 Provider 调用、外部服务启动、merge 或 push。真实效果、费用和生产性能尚未验证。学习验收状态 `review_needed`。

## 启动与资源链

独立入口 `v2/cmd/v2-isolated/main.go` → `isolated.Run` → 显式 `Load(path)` → 同一个 `Assemble` → 原 V2 native routes → HTTP server。入口形态（本轮未实际启动）：在 `v2/` 执行 `go run ./cmd/v2-isolated -config <独立配置文件>`。不启用 `v2_entry_reference`，不运行参考 main。

`isolated.example.toml` 空密钥会拒绝启动。必须指定配置路径、isolated=true、独立 MySQL/Redis/HTTP、原文存储目录、embedding 标识/维度和模型配置。保留原 config 默认及校验，不使用原 Load 的私有默认文件/环境覆盖。明确缺失字段也拒绝，即使某个原默认值能够补齐。生产构造器只使用这个显式配置。`migrate=false` 是独立入口默认；只有明确设置 true 才能到达原 InitializeSchema/migrate。本轮初始化测试只是记录调用次数的替身，不执行原业务迁移。SQLite AutoMigrate 仅初始化 t.TempDir 中的测试数据库。

启动时先打开原 GORM DB 和 Redis client，随后按原 main 构造：

1. 原文件存储、chunker、ingestor、知识向量索引/RAG、DocumentService、索引池与清理池；document_read 使用同一个 DocumentService 的原 SourceReader。
2. 原 Session/Message/AgentRun/AgentStep/Summary/学习/长期记忆/Episode DAO；没有 numeric ID 映射，没有另一套 Repository 代替业务查询。
3. 原 memory retriever/usage recorder、显式治理、草稿工具、topic catalogue/resolver、Mastery/TaskState、Summary provider/上下文配置，以及按原模式选择的工具注册。
4. 原 ContextBuilder → Runner → Agent Service；上下文预算、注入、grant、MemoryEvidence/EvidenceSet、最终授权/引用检查、消息持久化沿原链运行。
5. 原 Episode publisher、reconciler、worker、indexer、recall/shadow observer；原 Learning evidence/generator/grader/service。
6. 原 Start 顺序：知识索引 → 知识清理 → Episode → run lease reaper → memory usage。Episode Start 的启动恢复/对账仍在原 Worker 中。失败时回收此前成功启动的资源。

关闭先停止 HTTP 接收，再取消应用，使用退出时新建的 15 秒 context 停止原 workers 并关闭 Redis/DB。失败回收和资源关闭顺序登记在 mechanical-changes.json；没有跨系统新 lease/timeout 规则，也没有 Fatal/Exit 绕过清理。

原退役记忆抽取、Dream、实验 planner 不启动。relation 默认 off；原非 off 分支保留配置条件，不会自动开启。MaxToolCalls=0 的实际请求无工具。源码 legacy_v2+shadow_only 与负责人确认的部署 legacy_v2+inject_enabled 分开验证；公开 selector fixture 是 deployment-agent-selectors.toml，不复制或读取私有部署配置。config input target 28000、DefaultContextConfig 测试默认 20000 均保留。

## 实际模型请求与工具合同

验收从 native HTTP 登录/创建 agent 会话/运行进入同一个 Assemble。运行身份由原 controller/service/DAO 产生，grant 由原 ContextBuilder 生成。未手工注册 grant、构造 RunContext 或模型 Request 代替装配验收。工具负向单测只检验 guard，不算运行链证据。

以下为实际首个模型请求捕获，包含每个工具完整 description/JSON Schema 的黄金文件在 v2/internal/isolated/testdata，摘要及哈希见 schema-freeze.json。Eino/原 Runner 的强制工具步骤可能收窄一个请求的集合，因此这张表不是固定“所有工具都开启”表。

| 原模式/真实问题条件 | 实际请求工具集合 |
| --- | --- |
| legacy_v2，普通问候，shadow_only 或 inject_enabled | knowledge_search、document_read、read_history_message |
| legacy_v2，明确写入意图 | propose_memory、read_history_message |
| source_priority_v2，普通问候 | search_uploaded_documents、document_read、read_history_message、search_past_sessions |
| source_priority_v2，明确写入意图，原草稿优先步骤 | save_memory_draft |
| MaxToolCalls=0 | 无工具 |

注册集合经过 checkedRegistry 拒绝未知名称/能力和重复名称/能力别名；原工具对象和 Schema 没有重写。原 history Tools 会在无 grant 时隐藏 capability，本轮按用户要求另增 StaticTools，由隔离装配调用；原 Tools 的行为和所有原测试保持。新方法复用原执行 handler：无 grant 返回 refused，不查库；有 grant 仍检查来源、版本、水位、会话/用户、可引用角色和当回合证据。静态注册不授予读取权。

永久回归包含原生登录、创建会话、运行/步骤/消息持久化、无 grant 拒绝、摘要真实 grant 回读、Episode shadow/inject 回读差别、原 SourceReader 返回片段之外的原文、原草稿 pending/确认卡片、不暗写 active memory、原 global retriever 注入、原 Learning Current 仓储路由、重复/未知工具拒绝、部分原 Worker 启动失败回收、显式初始化开关及两种默认。

## 原测试与失败归属

所有数字为顶层 Go tests，排除子测试。根模块和嵌套 V2 分别执行 build/vet/test；使用 GOTOOLCHAIN=local、离线缓存、凭据环境白名单和人工配置。日志、各轮 results.json 和 test-summary.json 可复核。固定提交保存全部历轮日志的无损 logs/*.log.gz；log-artifacts.json 记录解压前后哈希，原未压缩日志也保留在施工目录。

| 模块 | build / vet | 最终 test |
| --- | --- | --- |
| 根模块 | 均通过 | 1318 通过、34 失败、13 跳过 |
| V2 独立模块 | 均通过 | 1243 通过、16 失败、88 跳过 |
| 新 MySQL 装配门控 | tagged vet/compile 通过 | 未执行；没有连接 MySQL |

根模块 34 项失败全部在固定 303b 临时本地副本中复现，集中 QD-003I/runner、QD-004A/validator、QD-004B；原生产代码和冻结评估没有改变。该副本三包复验为 101 通过、35 失败，另多一项 TestIgnoredProductionSourceIsRefusedByTheRealCommand，错误为该用例的“未修改副本”仍触发已冻结的 source file-set 漂移。本轮没有将这项波动归为收益。失败集合与诊断分别保留，不声称完全相同结果。

V2 的 16 项失败与上一轮相同，均在 internal/summaryreadeval 的原冻结输出目录/计划门控；原来源隔离复验记录在 ../v2-direct-copy/source-eval-replay.log。没有删除、放宽、Skip 原断言或修改冻结评估。新增默认离线装配回归无失败。

历轮新增夹具问题及修正保留日志：knowledgePolicy 错值、误把条件工具当固定全集、缺 Episode SourceHandles、错误原文长度/哈希、问句未命中原历史判定、Learning fixture 没有 active task；均修正人工输入/初始状态，未改原业务断言。全局显式写入依赖原 MySQL GET_LOCK，SQLite 不支持；其成功断言完整保存在 mysql_integration_test.go 的独立 v2_entry_mysql 构建门控，要求显式 -v2-entry-mysql-config、已初始化且为空的 v2_entry_test_* DB、migrate=false。仅编译/vet，未执行；没有伪造 GET_LOCK 或重写 DAO。新离线 global recall 是初始记忆夹具读取测试，不充当写入成功证据。

另两项来源限制需保留：source_priority_v3 在原 agentRunnerConfig 中 UsesMemorySearchTool=false，草稿优先却要求非零 propose-memory limit，原预算装配因此拒绝。该 helper 与来源 gofmt 后哈希完全一致，新增回归记录拒绝和清理，未自行修预算。Episode publisher 的原 MySQL IF/VALUES/GREATEST upsert 在 SQLite 会产生原非致命 enqueue 日志；publisher 接线已触达，但真正入队/跨实例恢复与索引持久化仍需 MySQL/Redis 门控，不能由 SQLite pass 宣称完成。

88 个 V2、13 个根模块跳过来自原独立集成门控。真实 MySQL/Redis Stack、reranker、Provider 等未执行；部分原 disposable MySQL 测试会探测本机 33506，连接拒绝后按原门控跳过。新 tagged MySQL 用例另列未执行，不混入默认 test skip 数。

## 来源、机械差异与复核

- additional-source-freeze.json 先冻结 13 个必要原文件，additional-copy-manifest.json 逐项记录复制结果；用户/认证/邮件依赖原样复制，session 只提取原创建及查询声明，完整来源归档为 .txt，不恢复 AIHelper/MQ。
- wiring-source.json、wiring-vs-reference.diff、original-helper-identity.json 记录原 main 来源跨度、全部装配差异和 5 个原 helper 的一致性；原测试 Fake 声明复用来源见 reused-fakes.json。
- mechanical-changes.json 登记新装配工件哈希、原因及三项有意变更的冻结文件：go.mod、go.sum、history_message.go。原 303b 来源清单不改写；check_integrity.py 作为本阶段增补核对原来源、原复制、已保留改进、完整 Schema 及 Git index 字节。
- verify.py 以唯一轮次名运行离线复核：full=分别检查两模块，v2=仅重跑嵌套模块，gates=只 vet/编译 MySQL 门控且不执行测试。遇已有 config/config.toml 明确拒绝覆盖。replay_root_baseline.py 创建并销毁临时本地固定提交副本，不修改任何既有 worktree、branch 或执行者成果。

## 未接通项目与下一轮接缝

| 项目 | 状态与后续要求 |
| --- | --- |
| 默认 cmd/gopherlearn / 当前前端与 API | 冻结不变；隔离 native V2 routes 仅供验收，认证/字段/分页合同需后续兼容装配 |
| 普通会话 | 原 V2 agent 创建入口已通；GopherLearn 冻结的无 MQ 普通对话和持久化尚未接入，不能恢复原普通 AIHelper |
| SSE / 手写记忆与确认卡片 | 当前已确认实现冻结；隔离旧原生流路由不是前端兼容方案，也不代表批准 SSE 回退 |
| Collection / 上传/UI 分组 | 未做 Collection 与 V2 KnowledgeDocument 合同映射；只接原知识服务/路由/存储/索引与清理池 |
| 真正数据库初始化、MySQL 原子治理、Episode 发布/索引/恢复 | 只实现初始化接线和离线 lifecycle；真实集成、真实数据迁移未执行 |
| Learning 出题/评分、摘要生成、memory index 模型语义 | 原服务已装配并保留原测试；没有真实 Provider 质量、耗时、费用证明 |
| source_priority_v3 / 实验 planner / relation | v3 原预算冲突未修；planner 隔离入口明确拒绝，relation 默认 off；后续变更需独立批准/评估 |

完成本轮隔离入口交付后停止，等待下一轮外围装配指令。不能宣告整体迁移完成。


## 2026-10-07 history 可见性勘误

负责人撤回旧“无 grant 也静态提供 read_history_message”要求，确认其为协调指令错误。此文上方为 70436c6 交付历史记录；其中静态能力说明与旧工具表已由后续恢复原 Tools 的纠偏替代。装配 registry、本回合 Tools 返回与执行读取授权须分别理解。来源/旧 Schema/机械清单/评估失败不改写，当前工具表以 `v2/service/agent/tool/` 的源码为准，哈希与验证读数见 [../v2-production-entry/README.md](../v2-production-entry/README.md)（那份纠偏记录本身不在本次公开快照内）。
