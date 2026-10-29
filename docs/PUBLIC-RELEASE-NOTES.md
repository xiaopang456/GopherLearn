# GopherLearn 首版发布说明（开发预览版）

> 版本建议：**v0.1.0-preview.1**（仅为建议；本仓库尚未创建标签或 Release）。

## 这是什么

GopherLearn 是一个个人 AI 学习助手，参考并基于 [GopherAI](https://github.com/youngyangyang04/GopherAI)
项目开发，在此基础上自主扩展了学习 Agent 等功能。它把「自己的资料」变成可以追问的学习材料：
上传文档后可以就文档提问并看到出处，出题作答后判分并累计主题掌握度。

## 这次公开的形态

- **开发预览版**：核心链路已跑通，前端样式与交互会继续迭代。
- **正式入口**：`hybrid/cmd/gopherlearn-integrated`（Go 后端 + Vue 3 前端 + MySQL + Redis Stack）。
- **文档处理在进程内完成**：解析、切片、嵌入、索引都在后端进程里，没有独立的执行节点、
  协调器或跨服务协议。

## 已经验证过的能力

以下功能在**真实模型**下做过一次小规模验收（单账号、单个自建文档、少量请求）：

| 能力 | 验证到什么程度 |
|---|---|
| 普通聊天 | 流式回答可用，会话重新进入后可回读历史 |
| 文档上传与知识库问答 | 上传 → 进程内切片 → 真实 embedding → 索引；回答带 `[n]` 引用，可展开原文行范围 |
| 学习 Agent 与工具调用 | 观察到真实的工具调用往返与工具结果 |
| 学习出题 | 能按主题出题，题目带文档证据定位 |
| 作答、判分与掌握度 | 判分给出分项得分与理由，主题掌握度写入并可回读 |
| 学习任务恢复 | 刷新页面后可用「查询／恢复活动任务」恢复未结束的测验 |

## 怎么跑起来

**完整步骤在仓库根目录的 [README.md](../README.md)**：环境要求 → 配置说明 → 本地启动（八步，
每步标明运行目录）→ Docker 部署 → 注册与使用 → 常见问题。

最短路径（仓库根目录）：

```bash
cp config.integrated.example.toml config.integrated.toml.local   # 填占位符
go -C v2 run ./cmd/schemainit -config ../config.integrated.toml.local
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
# 另开一个终端
npm --prefix web ci
VITE_DEV_API_ORIGIN=http://127.0.0.1:18082 npm --prefix web run dev
```

## 已知限制

- **不含 RPC 文件处理**，也没有多节点分布式执行。
- **只支持 `.md` 与 `.txt`**（单文件上限 5 MiB）。**不支持 PDF**。
- **指代文档请用精确文件名**（例如 `notes.md`）。检索工具只认文档 id 或精确文件名；
  用标题指代会把切片全部过滤掉，回答会退化成模型的通用知识。
- **四类后台行为没有真实执行过**：滚动摘要、记忆抽取、情景记忆投影、dream 归并。
  小规模验收中它们一次都没有触发，因此不能说它们"可用"。
- **检索与判分的普遍质量没有被证明**：只验证过单账号、单个文档、一个主题、一次判分。
- **长期稳定性没有被证明**：没有长时间运行、并发或故障注入的验证。
- **前端**仍会继续迭代。
- **开启重排后，远程重排的启动自检失败会阻止服务启动**：启动时会发一次真实的最小重排请求
  （1 个问题 + 1 条切片）来确认密钥、模型名与网络通路；这一步失败，服务就不起来，
  并在终端给出具体原因（凭据被拒 / 被限流 / 超时 / 模型不符 / 服务不可达）。
  这是刻意的：重排配错时宁可起不来，也不要在问答中途悄悄降级。
- **重排密钥的给法与聊天、embedding 密钥不同**：聊天与 embedding 的密钥写在配置文件的
  `[isolatedEntry]` 段里，而重排密钥**只从环境变量 `RAG_RERANK_AUTH_TOKEN` 读取**，
  不写进配置文件（见 README §5.4 与 §5.5）。

## 许可与来源

- 本项目基于 [GopherAI](https://github.com/youngyangyang04/GopherAI) 开发，并沿用其
  **GNU GPL v3.0** 许可。许可证全文见 [LICENSE](../LICENSE)。
- 上游归属、相关衍生项目与本次发布范围见 [NOTICE.md](../NOTICE.md)。
- 第三方依赖及其各自声明的许可证见 [THIRD-PARTY.md](THIRD-PARTY.md)。

## 更详细的材料

| 文件 | 内容 |
|---|---|
| [RELEASE-READINESS.md](RELEASE-READINESS.md) | 发布就绪报告：改了什么、哪些修好了、验证怎么复现、哪些还没验证 |
| [INTEGRATED-REFERENCE.md](INTEGRATED-REFERENCE.md) | 正式入口技术参考：学习测验接口合同、三个模块的验证命令 |
| [LEGACY-DEVELOPMENT-REFERENCE.md](LEGACY-DEVELOPMENT-REFERENCE.md) | 历史基线入口的开发说明（**仅供查阅历史实现**，不是快速开始） |
| [REAL-MODEL-VALIDATION-PLAN.md](REAL-MODEL-VALIDATION-PLAN.md) | 真实模型验证的额度口径与执行方案 |
| `spec/API.md`、`spec/ARCHITECTURE.md` | 接口合同与架构说明 |
