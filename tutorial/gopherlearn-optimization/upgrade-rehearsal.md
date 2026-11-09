# 升级演练（2026-11-09）

按 `docs/PUBLIC-RELEASE-NOTES.md` 的「升级步骤」从头到尾实际做了一遍升级：先用 v0.2.1 部署并写入旧数据，
再逐步升级到 PR #53 的版本，逐步核对说明与实际是否一致。

**全程零付费模型调用**：演练用一个本地的一次性假模型服务（`rehearsal/fake-provider.js`，OpenAI 兼容的
`/v1/chat/completions` 与 `/v1/embeddings`），它不联网、不产生任何费用。

## 演练环境

一次性容器、独立端口、独立卷，与正式整合栈互不干扰：

| 服务 | 容器名 | 宿主端口 | 说明 |
|---|---|---|---|
| MySQL | `gl-rehearsal-mysql` | 127.0.0.1:13391 | 镜像与整合栈同 digest |
| Redis Stack | `gl-rehearsal-redis` | 127.0.0.1:16391 | 镜像与整合栈同 digest |
| 假模型服务 | `gl-rehearsal-provider` | 127.0.0.1:18099 | 本地假 provider，零付费 |
| 后端 | `gl-rehearsal-backend` | 127.0.0.1:18092 | v0.2.1 与 HEAD 两版镜像 |
| 前端 | `gl-rehearsal-frontend` | 127.0.0.1:18091 | 重建后验证 |

编排文件 `rehearsal/compose.rehearsal.yml` 是**演练专用**的：服务名、容器名、网络名、卷名、端口都另起一套，
不碰正式整合栈的任何资源。

| 镜像 | 来源 |
|---|---|
| `gl-rehearsal-backend:v021` | tag `v0.2.1`（`2acc6bb`）的源码，`Dockerfile.backend-integrated` |
| `gl-rehearsal-backend:head` | 本分支 `22784e5` 的源码，同一个 Dockerfile |

## 升级前的基线

### 部署 v0.2.1

```bash
# 用 v0.2.1 的 schemainit 建出升级前的表结构
docker compose -p gl-rehearsal -f compose.rehearsal.yml --profile init run --rm schemainit
# -> schema initialized in gopherlearn_rehearsal on gl-rehearsal-mysql:3306

# 用旧配置启动 v0.2.1 的后端
docker compose -p gl-rehearsal -f compose.rehearsal.yml up -d backend
```

旧配置（`rehearsal/config.rehearsal.old.toml`）刻意带着这一版要删的写法：`[ordinaryChat]` 整段、
`[isolatedEntry].embeddingKey`、`[ragModelConfig].baseUrl`。**v0.2.1 正常启动**——它认这些设置。

建出来的表（21 张）里**没有**三张分层摘要表：

```
agent_runs  agent_steps  episode_assertion_relations  episode_assertion_states
episode_memories  episode_projection_jobs  episode_relation_proposals
knowledge_documents  learning_attempts  learning_questions  learning_sessions
long_term_memories  memory_candidates  memory_drafts  memory_dream_runs
memory_extraction_jobs  memory_revisions  messages  session_summaries
sessions  topic_progresses  users
```

### 写入旧数据

| 数据 | 怎么写的 | 结果 |
|---|---|---|
| 用户 | 验证码直接写进 Redis（键 `captcha:<小写邮箱>`，TTL 5 分钟），再调 `POST /api/v1/user/register` | user id=1，username=46402610049 |
| 会话 | `POST /api/v1/sessions`（mode=chat） | 2 个会话 |
| 普通对话的消息 | `POST /api/v1/sessions/{id}/messages` | 4 条消息（2 问 2 答） |
| **旧的纯文本摘要** | SQL 写 `session_summaries.content`（纯散文；这张表没有 HTTP 写入口，只能 SQL 播种） | 1 行，`version=1`，`covered_message_id=1` |
| 知识库文档 | `POST /api/v1/knowledge/documents`（multipart，字段名 `file`） | 1 篇，`index_status=ready`，`chunk_count=1` |
| 知识库索引 | 上传后由索引 worker 建 | Redis 1 个块键 + 三个 FT 索引 |

> **两处踩到的地方**（都不是产品缺陷，是演练编排要适配的地方）：
> 1. v0.2.1 的消息请求体**只接受 `content`**——多传一个 `requestId` 就会被严格解码拒绝（`invalid_request`）。
> 2. 后端镜像以 uid 65532 运行，知识卷归 root 所有时**写不进去**（上传 500）。卷要 chown 给 65532。

### 升级前快照

```
DBSIZE=1
knowledge_chunk_keys=1
FT._LIST=gopherai_episode_v2_idx,gopherai_memory_v2_idx,gopherai_knowledge_chunks_v3_idx
layered_tables=(none)
users=1  sessions=2  messages=4  session_summaries=1  knowledge_documents=1
summary_content=学习者在准备期末复习，重点是集合与映射这一章。
summary_version=1   summary_covered=1
user_name_column=20
```

## 逐步升级

### 第 1、2 步 · 改配置

按说明把配置收拢成一处一个：嵌入密钥挪到 `[ragModelConfig].embeddingApiKey`，删掉没人用的
`[ragModelConfig].baseUrl`，删掉整段 `[ordinaryChat]`。

**实测：新版后端拿旧配置启动，被指名拒绝**，与说明一致：

```
$ docker run --rm --network gl-rehearsal-data \
    -v config.rehearsal.old.toml:/config/rehearsal.toml:ro \
    -v gl-rehearsal-knowledge-data:/data \
    gl-rehearsal-backend:head -config /config/rehearsal.toml
gopherlearn integrated entry: agent=original V2 Eino React, chat=retained DirectChat, business migration refused
retired configuration setting: isolatedEntry.embeddingKey moved to ragModelConfig.embeddingApiKey,
beside the embedding model name and address it belongs to. Move the key there and delete this line.
（退出码 1）
```

### 第 3、4、5 步 · Redis 的 AOF

这三步的实测已在上一轮做过（一次性容器，500 个键，两种做法对比），结果照抄不再重跑：
**只改配置直接重启会带着空索引起来**（`DBSIZE` 0，下一次快照把 dump.rdb 覆盖成 189 字节/1 键）；
**运行中 `CONFIG SET appendonly yes` 等 `aof_rewrite_in_progress:0` 再重启**，`DBSIZE` 保持完整。

本次演练的 Redis 在升级过程中被**重建过一次**（第 9 步换入口），重建后 `DBSIZE` 仍是 **1**，数据没丢。

### 第 6 步 · 换后端

```bash
docker compose -p gl-rehearsal -f compose.rehearsal.yml build backend   # 用新一版的代码构建
```

### 第 7 步 · 建表（schemainit）

**这是本次演练最重要的一处发现：不做这一步，新版后端起不来。**

新后端拿新配置启动，报的是**缺表**：

```
Error 1146 (42S02): Table 'gopherlearn_rehearsal.session_summary_segment_jobs' doesn't exist
...
integrated entry refused or failed; check the configuration requirements and the schema,
Redis instance and storage directory it points at
```

原因在代码里是明确的：正式整合入口拒绝 `isolatedEntry.migrate = true`（`hybrid/entry.go`），
而建表只在 `s.Entry.Migrate == true` 时发生（`v2/internal/isolated/resources.go:58`），
所以**启动时既不建表，也不改列宽**。

跑新版的 schemainit：

```bash
docker compose -p gl-rehearsal -f compose.rehearsal.yml --profile init run --rm schemainit
```

结果：

```
三张分层摘要表：session_summary_segment_jobs, session_summary_segments, session_whiteboards
messages.user_name 列宽：20 -> 50
```

**与说明一致**：只建缺的东西，不删表、不改写已有的行。

### 第 8 步 · summaryconvert

```bash
go -C v2 run ./cmd/summaryconvert -config ../config.toml
```

预演（不带 `-apply`）：分类并报数，**不写一行库**：

```
row=65ed51ae-... user=1 mode="normal" shape=prose action=would_convert reason="convertible; this run writes nothing"
read=1 more=false shapes: current=0 legacy_json=0 prose=1 empty=0 damaged=0
actions: converted=0 would_convert=1 unchanged=0 failed=0 refused=0
```

加上 `-apply`：

```
row=65ed51ae-... user=1 mode="normal" shape=prose action=converted reason="converted"
read=1 more=false shapes: current=0 legacy_json=0 prose=1 empty=0 damaged=0
actions: converted=1 would_convert=0 unchanged=0 failed=0 refused=0
```

转换后的那一行——**与说明一致**（机械无损、水位线不动、版本号 +1）：

```
content = {"schema_version":2,"current_goal":"","confirmed_decisions":[],
           "completed_items":["学习者在准备期末复习，重点是集合与映射这一章。"],
           "open_loops":[],"user_corrections":[],"key_errors":null,"references":null}
version = 2   （转换前是 1）
covered = 1   （水位线没动）
```

> **演练发现的一处说明缺口**：上面的命令是**源码运行**。整合栈的 MySQL **不发布宿主端口**，
> 所以在宿主机上跑这个命令根本连不上数据库：
> ```
> connect mysql: dial tcp lookup gopherlearn-integrated-mysql: no such host
> ```
> 演练里绕过它的办法是给 MySQL 临时开一个宿主端口、用一份指向 `127.0.0.1` 的配置——
> **这正是发布说明现在写清的那件事**：这条命令要在能访问到数据库的地方跑。
> 在容器网络里用 `golang` 镜像跑它也不行：栈内网络是 `internal` 的，拉不到 Go 依赖
> （`dial tcp: lookup proxy.golang.org ... server misbehaving`）。
> **已据实改写发布说明**：写明这条命令的前提是"配置里的 MySQL 地址可达"，并给出两种跑法与各自的限制。

### 第 9 步 · 换 Redis 的启动入口

```bash
docker compose -p gl-rehearsal -f compose.rehearsal.yml up -d --force-recreate redis
```

**要重建，不是 restart**——重建才会带上新的挂载与新的入口。重建后 `DBSIZE` 仍是 **1**，数据没丢。

### 第 10 步 · 重启后核对键数

```
DBSIZE = 1        （升级前是 1，没有变少）
knowledge chunk keys = 1
FT._LIST = gopherai_episode_v2_idx, gopherai_memory_v2_idx,
           gopherai_summary_segment_v1_idx, gopherai_knowledge_chunks_v3_idx
```

注意比升级前**多了一个**索引：`gopherai_summary_segment_v1_idx`——这是新版分层摘要的索引，
键数没变是因为分层摘要的数据在 MySQL 里，不在 Redis。

## 最终核对

| # | 核对项 | 结果 |
|---|---|---|
| 1 | 服务正常启动 | ✓ 容器 running、restarts=0，`GET /api/v1/me` → 200 |
| 2 | 旧会话能打开 | ✓ `GET /api/v1/sessions/{id}/messages` → 200，4 条历史原样在（升级前写的两条问答） |
| 3 | 旧会话能继续聊 | ✓ `POST …/messages` → 200，`assistant_message` 有内容 |
| 4 | 旧摘要已转换 | ✓ 现行 JSON 形状（`schema_version=2`）、`version` 1→2、`covered_message_id` 不动 |
| 5 | 三张新表存在 | ✓ `session_whiteboards` / `session_summary_segments` / `session_summary_segment_jobs`；新后端还真的写入了 1 行白板、1 行分段 |
| 6 | Redis 键数没有减少 | ✓ `DBSIZE` 1 → 1；知识块键 1 → 1 |
| 7 | 启动校验报告一致 | ✓ `index_audit status=audit compared=1 blocks=1 missing=0 extra=0 index_problems=0` / `knowledge_index_audit mode=audit status=consistent` —— 与升级前同一组数字 |

数据量核对：`users=1`、`sessions=2`、`messages` 4→6（多了升级后新问的一轮）、`knowledge_documents=1`、
`session_summaries=1`，没有丢。

## 演练发现与据此修改的地方

| # | 发现 | 处理 |
|---|---|---|
| 1 | 新版后端在缺三张分层摘要表时**启动失败**（`Error 1146 ... session_summary_segment_jobs doesn't exist`），而旧版说明写的是"由服务启动时的自动迁移建好" | **改说明**：新增第 7 步（`schemainit` 建表），并把两处"服务启动时的自动迁移"改掉 |
| 2 | 第 8 步的 `summaryconvert` 是源码运行命令，整合栈的 MySQL 不发布宿主端口，宿主机上跑会 `no such host`；栈内用 `golang` 镜像跑则因 `internal` 网络拉不到 Go 依赖而不通 | **改说明**：写清这一步的前提是"配置里的 MySQL 地址可达"，给出宿主直跑与栈内跑两种跑法，并写明后端镜像目前还没有 `/summaryconvert` 这个二进制 |
| 3 | 第 8 步（旧第 8 步）说的是"换新入口"，容易被读成换后端的入口 | **改说明**：写清换的是 **Redis 容器**的入口（`deploy/redis-stack-start.sh`），后端入口本来就没变，并给出重建命令 |
| 4 | 索引校验的配置路径写成了 `[startupIndexAudit].mode` | **改说明**：改成 `[knowledgeConfig.startupIndexAudit].mode`；并写清 rebuild 只管"缺块"，不管"索引本身的问题" |

演练中发现的两处**产品**问题：无。（缺表是文档说错了，不是代码问题；`summaryconvert` 连不上是部署拓扑的
事实，命令本身正确。）

## 用完的清理

演练结束后删净：

- 容器：`gl-rehearsal-mysql` / `-redis` / `-provider` / `-backend` / `-frontend`（`docker compose down -v --remove-orphans`）；
- 卷：`gl-rehearsal-mysql-data`、`gl-rehearsal-redis-data`、`gl-rehearsal-knowledge-data`；
- 网络：`gl-rehearsal-data`、`gl-rehearsal-web`；
- 镜像：`gl-rehearsal-backend:v021`、`gl-rehearsal-backend:head`；
- v0.2.1 的 worktree（`wt-rehearsal-v021`）；
- 演练目录（`rehearsal/`）与临时脚本。

演练全程没有碰正式整合栈的任何资源：没有用 `gopherlearn-integrated-*` 的容器名、网络名、卷名或宿主端口。
