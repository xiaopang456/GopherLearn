# 隔离 V2 入口：学习与面试说明

事实依据为本轮 README、原 main 冻结来源、实际请求 Schema 及 test-summary。实现交付和学习验收分开，当前 `review_needed`；需要闭卷口述。没有真实模型收益、费用或生产性能证据，效果尚未验证。

## 1. 旧问题

原模块已复制并能编译，但模块之间是否能够组成真实入口链还没有证据。直接启用参考 main 会连回私有默认配置、原普通 AIHelper/MQ 以及当前不批准启用的能力。只用手工 RunContext/Request 或 grant 调一次工具，也不能证明认证、原 DAO、上下文和 Runner 已接在一起。

## 2. 选型理由

保留 V2 原 Model/DAO 和独立 Go module，以新的显式入口选择原 main 的装配声明。传入的是模型、embedding、向量/数据库基础设施工厂；业务 Repository 是原代码。这样离线 Fake 能替换外部边界，同时仍验证 native HTTP → 原 Service → 原 SQL DAO → ContextBuilder → Runner。生产不读取环境凭据，只认显式独立配置。

## 3. 核心链路

显式配置通过原默认/校验 → 资源打开 → 原知识/记忆/Episode/Learning 服务按原模式构造 → 原 Worker Start 恢复/对账 → 原 native 登录取得 JWT → 创建 agent 会话 → 原 AgentRun/Message 持久化 → ContextBuilder 注入摘要/记忆/Episode 并生成 grant → 原工具注册/Runner 请求 → document_read 原 SourceReader 或 history 原授权回读 → 原 EvidenceSet/最终引用门控 → 原答案与步骤落库。退出先关 HTTP，使用新建有界 context 取消/停止原 Worker，关闭基础设施。

工具是否注册与一次执行是否获授权必须分开理解：静态 history capability 不意味着可以枚举 numeric message ID。只有本轮真实可见注入生成的 grant 才能让原 handler 继续查询，查询还会重验来源/会话/版本/水位。无 grant 回归从 native HTTP 验证 refused，并非手工构造权限。

## 4. 五个代码锚点

- v2/internal/isolated/entry.go、settings.go：显式配置如何到达 Assemble，退出 context 为什么必须新建。
- v2/internal/isolated/wiring.go：选择原 main 声明与原模式条件，如何避免连到普通 MQ 链。
- v2/service/agent/context_builder.go、runner.go：注入、预算、回读与最终授权协作，保留原状态机和账本。
- v2/service/agent/tool/history_message.go、v2/service/knowledge/source_reader.go：capability/grant 分离与原文长度/哈希/所有权检查。
- v2/internal/isolated/assembly_test.go、chain_test.go、contracts_test.go：实际请求如何从 HTTP 捕获完整 Schema，数据库与工具如何从原链被调用。

## 5. 异常与验证边界

缺配置在连接之前拒绝。原 Episode Start 恢复失败后回收已启动知识资源和 DB；Stop 使用退出时新建的 15 秒 context。原运行预算、失败分类、摘要水位、证据授权、重试和普通消息事务没有重写。

测试有三个层次：离线装配通过；根模块 34 项原冻结评估失败均在固定 303b 副本复现；V2 16 项原冻结评估门控失败维持。真实 MySQL/Redis/Provider 未执行。SQLite 能验证原多数 SQL 读取/持久化，却不能模拟 MySQL GET_LOCK、Episode MySQL upsert 等原子写入语义，不能把它的通过当成生产事务证明。成功断言放到明确的 MySQL 门控继续保留，当前只编译。

原 source_priority_v3 helper 的预算接线冲突按源保留并拒绝启动；它与来源规范化后 hash 相同。不要以“入口更容易启动”为理由统一实验预算或改变断言。

## 6. 替代方案与取舍

可以把所有组件改成通用接口/Fake Repository 来降低离线依赖，但会绕开本轮要证明的真实 DAO/授权/生命周期。也可以在默认 main 同时接前端和 SSE，但这会把来源保真和兼容适配混在同一次验收。本轮选择独立入口，代价是要保留真实数据库门控、已知来源失败以及后续 API/Collection/SSE/普通会话接缝。复制与装配完成并不等于整体迁移或优化成立。

## 面试问题与参考回答

**1. 如何证明模块不是仅仅能够编译？**

使用与可执行入口同一个 Assemble，通过原 native 登录、创建会话和 Agent HTTP route 调用，让原 DAO 产生运行身份和消息，捕获真正传给模型的请求和 Schema，再核对持久化、回读与授权。没有手工组装 Request 或 grant 替代入口证据。当前只证明离线链路，真实 Provider 效果尚未验证。

**2. 为什么 history 能力要静态注册，授权却按回合生成？**

注册告诉模型有哪些动作，授权告诉服务端当前证据允许读哪些原消息。注册不能按 Episode 开关消失，也不能因有 numeric ID 就允许读取。本轮 StaticTools 复用原 handler 和 grant 重验，保留原 Tools 的既有测试合同。

**3. 为什么 document_read 不能返回检索 snippet？**

snippet 是召回切片，原文读取需要原 SourceReader 核对所有权、文档版本、路径、长度和哈希，然后读原始文件范围。永久回归把片段之外的文字放在文件中，实际后续模型请求必须收到它，才能证明接到原文存储链。

**4. 中途启动失败如何清理？**

入口资源拥有者登记成功启动的原 Worker 和基础设施，失败返回后触发同一 Close。退出时新建有界 context，取消应用并等待原 Stop，再关 Redis/SQL。没有用 Fatal 或 Exit 绕过清理，也不改变原 Worker 的恢复/重试或租约算法。

**5. build/vet 通过而 test 有失败，能说迁移完成吗？**

不能。必须区分来源冻结门控失败、新增接线失败和没有运行的真实集成。本轮原失败已保留并复验，新增离线装配通过；MySQL 原子治理、Episode 真发布/索引和模型效果没有证据，整体迁移仍需下一轮外围兼容装配。

**6. 为什么不直接修 source_priority_v3 的预算冲突？**

本轮授权是原装配与保真，预算冲突已存在于原 helper，修它会成为业务/实验行为变更。先保留错误、拒绝启动并记录来源，再由单独任务确定修复及评估标准；不能为凑绿改默认数字。

闭卷验收：先画入口、授权与数据库的整体链，再解释无 grant、原文哈希错误、启动恢复失败和 MySQL 门控四个场景。能定位少量锚点并主动口述边界后，再进入学习验收。


## 2026-10-07 history 可见性勘误

负责人撤回旧“无 grant 也静态提供 read_history_message”要求，确认其为协调指令错误。此文上方为 70436c6 交付历史记录；其中静态能力说明与旧工具表已由后续恢复原 Tools 的纠偏替代。装配 registry、本回合 Tools 返回与执行读取授权须分别理解。来源/旧 Schema/机械清单/评估失败不改写，当前工具表、哈希及验证见 [../v2-history-correction/README.md](../v2-history-correction/README.md)。
