# GopherLearn

GopherLearn 是一个个人 AI 学习助手，参考并基于 [GopherAI](https://github.com/youngyangyang04/GopherAI) 项目开发，在此基础上自主扩展了学习 Agent 等功能。

本仓库目前是**开发预览版**：核心链路已经跑通，并做过一次小规模的真实模型验收；前端样式与交互还会继续迭代。

| | |
|---|---|
| 正式入口 | `hybrid/cmd/gopherlearn-integrated` |
| 运行时 | Go 后端（HTTP + SSE） + Vue 3 前端 + MySQL（业务数据） + Redis Stack（可重建的检索索引） |
| 许可证 | GPL-3.0，见 [LICENSE](LICENSE)；来源与归属见 [NOTICE.md](NOTICE.md) |
| 依赖许可清单 | [docs/THIRD-PARTY.md](docs/THIRD-PARTY.md) |

> 本文面向第一次接触这个项目的人：怎么配置、怎么跑起来、怎么用。
> 更细的技术合同与历史说明在 [`docs/`](docs/) 下：
> [正式入口技术参考](docs/INTEGRATED-REFERENCE.md)、
> [历史基线开发参考](docs/LEGACY-DEVELOPMENT-REFERENCE.md)、
> [发布就绪报告](docs/RELEASE-READINESS.md)、
> [公开版说明](docs/PUBLIC-RELEASE-NOTES.md)、
> [真实模型验证计划](docs/REAL-MODEL-VALIDATION-PLAN.md)。

---

## 1. 项目介绍

GopherLearn 面向"用自己的资料学习"这件事，提供两种会话形态，共用同一套账号与数据：

- **普通聊天（chat）**：普通对话，流式回答。
- **学习 Agent（learning_agent）**：带工具的自主回路，模型可以在服务端允许的范围内检索你的文档、检索与写入你的长期记忆，并把结果整理成回答。

在此之上还有三个相对独立的表面：

- **用户级知识库**：上传自己的 `.md` / `.txt`，服务端进程内解析、切片、嵌入并建立向量索引；回答时可以检索这些文档，并给出可点开的引用（文档、版本、行号）。
- **记忆**：需要确认才入库的长期记忆、主题记忆、情景记忆；记忆只属于它的主人。
- **学习／测验（quiz）**：按主题出题、作答判分，并把结果累计到主题掌握度上。

公开版**只包含进程内（in-process）的文档处理**：解析、切片、嵌入、索引全部发生在后端进程里。没有独立执行节点、没有协调器、构建里不含任何 RPC 框架。

---

## 2. 主要功能

| 功能 | 说明 |
|---|---|
| 普通聊天与流式回答 | 会话内逐段返回内容，结束后可重新进入会话回读历史消息 |
| 学习 Agent 与工具调用 | 模型在服务端校验过的工具集内工作（如检索文档、检索长期记忆、提交记忆草稿）；工具结果带证据与引用 |
| 文档上传与知识库问答 | 上传 `.md` / `.txt` → 进程内切片 → 真实 embedding → 向量索引；问答结果带 `[n]` 引用，可展开**原文行范围** |
| 记忆管理 | 记忆草稿需人工确认才生效；记忆只对本人可见，可检索、可查看来源 |
| 学习出题 / 作答 / 判分 / 掌握度 | 按主题生成题目，提交答案后给出分项得分与理由，主题掌握度随之更新并可回读 |
| 学习任务恢复 | 刷新页面后，用「**查询／恢复活动任务**」按钮把未结束的测验与当前题目恢复出来 |
| 用户级数据隔离 | 文档、会话、记忆都按账号隔离；跨账号读取一律 404 |

---

## 3. 当前版本与已知限制

**已验证到什么程度**：核心链路（普通聊天、文档问答与引用、Agent 工具往返、出题、判分与掌握度）已经用**真实模型**做过一次小规模验收——15 次真实模型请求，过程中发现并修掉了若干缺陷。详见 [发布就绪报告 §7](docs/RELEASE-READINESS.md)。

**仍然存在的限制**，请在评估时一并考虑：

- **这是开发预览版**，前端界面与交互还会迭代。
- **不含 RPC 文件处理**，也没有多节点分布式执行。
- **当前只支持 `.md` 与 `.txt`**（单文件上限 5 MiB，`knowledgeConfig.maxFileSizeBytes`）。**不支持 PDF**——PDF 解析没有实现，别按"能传就行"来预期。
- **指代文档请用精确文件名**（例如 `seagull7-facts.md`）。检索工具只认文档 id 或**精确文件名**；用标题指代会把所有切片过滤掉，回答会退化成模型的通用知识，并显示"没有找到笔记证据"的提示。
- **检索与判分的普遍质量没有被证明**：只验证过单账号、单个自建文档、一个主题、一次判分。
- **长期稳定性没有被证明**：本轮只跑了几分钟，没有长时间运行、并发或故障注入的验证。
- **四类后台行为没有真实执行过**：滚动摘要、记忆抽取、情景记忆投影、dream 归并——本轮真实调用中它们**一次都没有发生**（触发条件不成立或任务未入队），因此不能说它们"可用"。
- **重排默认关闭**：本轮配置为不启用 rerank。**需要时可以在部署里开启**——推荐走远程重排 API（三行配置 + 一个环境变量，无需显卡），步骤见 [§5.5](#55-开启重排可选)。
- **开启重排后，远程重排的启动自检失败会阻止服务启动**：启动时会发一次真实的最小重排请求来确认密钥、模型名与网络通路，失败就不起来，并给出具体原因。
- **重排密钥的给法与聊天、embedding 密钥不同**：聊天与 embedding 的密钥写在配置文件里，**重排密钥只从环境变量 `RAG_RERANK_AUTH_TOKEN` 读取**，不写进配置文件。
- **模型调用消耗部署者配置的 API 额度**。注册用户不消耗你的额度。

---

## 4. 环境要求

| 组件 | 要求 | 说明 |
|---|---|---|
| Go | **1.25.4 或更新** | 正式入口在 `hybrid` 模块；`v2` 模块同样要求 1.25.4 |
| Node.js | **22.22.2+ / 24.15.0+ / 26+** | 前端在 `web/`；`web/.nvmrc` 固定 24.15.0，`web/package.json` 的 `engines` 写明了区间 |
| MySQL | **8.0 或更新** | 需要一个**属于本次部署自己的** schema 与账号 |
| Redis Stack | 带 **RediSearch** 模块的 Redis | 知识库检索依赖 `FT._LIST` 与向量检索；普通 Redis 没有 RediSearch 模块 |
| 模型服务 | 一个 OpenAI 兼容的 chat 端点 + 一个 OpenAI 兼容的 embedding 端点 | embedding 的向量维度必须与 `ragModelConfig.dimension` 一致 |
| 重排服务（**可选**） | 远程重排 API（推荐）：一个 https 地址 + 模型名 + API Key；或本地 `text-embeddings-inference` 容器 | **不用重排就不用装**。开远程重排不需要显卡；开本地重排建议有 NVIDIA 显卡（没有也能跑，只是慢） |
| SMTP | **可选** | 只用于"邮箱验证码注册"；不配也能跑，已有账号与其它功能不受影响 |
| Docker | 可选 | 只用 Docker 部署时才需要；本机启动不需要 |

---

## 5. 配置说明

### 5.1 部署者需要配置的字段

先复制模板：

```bash
# 在仓库根目录执行
cp config.integrated.example.toml config.integrated.toml.local      # Bash
Copy-Item config.integrated.example.toml config.integrated.toml.local   # PowerShell
```

模板里全是占位符，下面是要替换的字段（字段名以 `config.integrated.example.toml` 为准）：

| 段 | 键 | 用途 | 模板值 |
|---|---|---|---|
| `[isolatedEntry]` | `chatKey` / `chatModel` / `chatBaseURL` | **学习 Agent** 的 chat 模型：地址、模型名、密钥 | `https://provider.invalid/v1` |
| `[isolatedEntry]` | `embeddingKey` | **embedding** 服务的密钥 | — |
| `[ragModelConfig]` | `embeddingModel` / `embeddingBaseUrl` / `dimension` | embedding 的模型名、地址、**向量维度**（必须与模型实际输出一致，且与索引一致） | `dimension = 1024` |
| `[ragModelConfig]` | `chatModelName` / `baseUrl` | 知识库问答路径使用的 chat 模型 | — |
| `[ragModelConfig]` | `rerankEnabled` / `rerankProvider` / `rerankEndpoint` / `rerankModel` / `rerankTimeoutMs` | **重排（可选）**：是否开启、服务形态、地址、模型名、单次调用上限 | 默认全部关闭 |
| `[ordinaryChat]` | `GOPHERLEARN_AI_PROVIDER` / `GOPHERLEARN_OPENAI_BASE_URL` / `GOPHERLEARN_OPENAI_API_KEY` / `GOPHERLEARN_OPENAI_MODEL` | **普通聊天**用的模型：类型必须是 `openai_compatible`，另有地址、密钥、模型名 | — |
| `[ordinaryChat]` | `GOPHERLEARN_OPENAI_ENABLE_THINKING` / `GOPHERLEARN_OPENAI_TEMPERATURE` / `GOPHERLEARN_OPENAI_MAX_OUTPUT_TOKENS` / `GOPHERLEARN_OPENAI_TIMEOUT` / `GOPHERLEARN_CONTEXT_*` / `GOPHERLEARN_SUMMARY_*` | 普通聊天的可选参数，留空即用默认值 | 注释掉 |
| `[mysqlConfig]` | `host` / `port` / `user` / `password` / `databaseName` / `charset` | 本次部署自己的 MySQL schema 与账号 | `gopherlearn-integrated-mysql` / `3306` / `gopherlearn_v2_integration` / `utf8mb4` |
| `[redisConfig]` | `host` / `port` / `db` / `password` | 独立的 Redis Stack 实例；**必须与该实例的口令逐字一致** | `gopherlearn-integrated-redis` / `6379` / `db = 0` |
| `[jwtConfig]` | `key` | 令牌签名密钥，**至少 32 字节**；更换后所有已签发令牌失效 | — |
| `[knowledgeConfig]` | `storageDir` | 上传原文件的存放目录（需要可写） | `data/gopherlearn-integrated/knowledge` |
| `[mainConfig]` | `host` / `port` | 后端监听地址 | `127.0.0.1` / `18082` |
| `[emailConfig]` | `email` / `authcode` / `host` / `port` | 发件邮箱、SMTP 授权码、SMTP 地址与端口 | `smtp.qq.com` / `587` |

> **两路模型配置是分开的，别只填一路。**
> 学习 Agent 走 `[isolatedEntry]` + `[ragModelConfig]`；普通聊天走 `[ordinaryChat]`。
> 正式入口在启动时校验这些字段：**缺任一路，对应功能就会在启动时被拒绝**（例如 `[ordinaryChat]` 的类型不是 `openai_compatible`、密钥为空、维度与模型不符）。

### 5.2 普通注册用户只需要

- 自己的收件邮箱；
- 邮箱收到的六位验证码；
- 自己设的账号密码。

**不需要**提供模型 API Key，**不需要**提供邮箱 SMTP 授权码——这些只由部署者配置一次。

### 5.3 邮件配置是干什么的

`[emailConfig]` 全仓库**只用于一件事：发送注册验证码**。

- 不配：服务器照常启动，**已有账号照常使用**，登录、会话、文档、记忆、学习、Agent 全部可用；只有"新用户注册"走不通，此时 `POST /api/v1/user/captcha` 会返回业务码 `2012`（"邮件服务未配置"），并给出可定位的说明，而不是含糊的"服务繁忙"。
- 配了：`host` / `port` 可选，默认 `smtp.qq.com:587`；也可以指向你自己的中继。

### 5.4 配置是从哪里读进来的

- 正式入口**只读 `-config` 指向的那一个 TOML 文件**。它**不读 `.env` 文件，也不读环境变量里的凭据**（`v2/internal/isolated/settings.go` 里写得很直白：`environment credentials are not read`）。
- **唯一的例外是重排令牌 `RAG_RERANK_AUTH_TOKEN`**：它是密钥，不能写进配置文件，所以开启重排时从环境变量读这一个（见 [§5.5](#55-开启重排可选)）。聊天与 embedding 的密钥仍然只从配置文件读。
- 因此：**`.env.provider.local` 是历史基线入口的东西，正式入口不会自动读取它**。要让正式入口拿到模型配置，只能写进 `config.integrated.toml.local`。
- 真实配置请放在 `config.integrated.toml.local`：后缀 `.local` 同时命中 `.gitignore` 与 `.dockerignore` 的 `*.local` 规则，不会被提交，也不会进构建上下文。
- 模板文件里只有占位符；**任何真实凭据都不要写进模板、日志或报告**。

### 5.5 开启重排（可选）

重排是**第二阶段召回**：先把候选切片按向量相似度取回来，再用交叉编码器对"问题 × 切片"逐对打分重排。**默认关闭**，关闭时行为与现在完全一致。

#### 方式 A · 远程重排 API（推荐，无需显卡）

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
# 单次调用上限（毫秒）。留空即用默认值：远程 5 秒、本地 30 秒
# rerankTimeoutMs = 5000
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

至少 32 字节。**不设这个变量就不会启动**，并会明确告诉你缺它（见下面"启动失败时怎么读提示"）。

**第 3 步 · 照常启动**

```bash
cd hybrid
go build -o ..\bin\gopherlearn-integrated.exe .\cmd\gopherlearn-integrated
..\bin\gopherlearn-integrated.exe -config ..\config.integrated.toml.local
```

启动时会发**一次**最小重排请求做自检（1 个问题 + 1 条切片）：这一步同时证明密钥、模型名与网络通路都是通的，它消耗部署者的一点额度。

**要知道的两件事**：

- **切片正文会发给这家 provider**。embedding 已经把你的切片发给 embedding provider 了，重排是同一类数据外流——如果你要求数据完全不出本机，请用下面的方式 B。
- **重排消耗部署者的额度**，和 chat / embedding 是同一笔账；注册用户不消耗你的额度。

#### 方式 B · 本地重排服务（数据不出本机）

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

- **显卡**：有 NVIDIA 显卡就加 `--gpus all`；没有就**去掉这一行**，服务会退回 CPU 运行——能跑，但慢得多：实测一次 24 条候选在 CPU 上要十几秒。**CPU 场景请把 `rerankTimeoutMs` 调大**（默认 30 秒，必要时调到 60 秒以上），否则重排会超时降级成"看似开启实则没生效"。
- **端口**：容器内是 80，宿主映射成 18083；后端在容器里跑时把地址换成 `http://host.docker.internal:18083`。
- **令牌**：容器用 `--api-key` 收同一个口令，后端用 `RAG_RERANK_AUTH_TOKEN` 发同一个口令。

#### 启动失败时怎么读提示

开启重排但起不来时，终端会给出**一句话的原因**，而不是笼统的"启动失败"：

| 提示里的关键词 | 意思 | 怎么办 |
|---|---|---|
| `the rerank configuration is invalid` | 配置本身有问题：provider 拼错、缺 `rerankModel`、超时为负 | 按 [§5.1](#51-部署者需要配置的字段) 核对 `ragModelConfig` 的重排字段 |
| `RAG_RERANK_AUTH_TOKEN` | 没设这个环境变量，或它不足 32 字节 | 按上面第 2 步设置环境变量 |
| `the rerank service refused the credential` | 服务不认这个 Key | 核对 Key 是否用在这个服务上（本地服务要同时配 `--api-key`） |
| `the rerank service is rate limiting` | 被限流 | 稍后重试，或提高该 provider 的额度 |
| `the rerank service did not answer inside the configured timeout` | 太慢 | 调大 `rerankTimeoutMs`，或换一个更快的重排服务 |
| `the rerank service serves a different model` | 本地服务跑的不是钉死的那个模型 | 用上面的 `--model-id` / `--revision` 重启容器 |
| `the rerank service is unavailable` | 地址不通、服务没起 | 核对地址、端口与服务是否在运行 |

---

## 6. 本地启动

下面的命令都标注了**运行目录**。除特别说明外，都在**仓库根目录**执行。

### 步骤 1 · 准备依赖与基础设施

需要：Go 1.25.4+、Node 22.22.2+/24.15.0+、一个 MySQL 8.0+、一个带 RediSearch 的 Redis Stack。

用 Docker 起这两个基础设施（端口可改，但要同步改配置）：

```bash
# MySQL（宿主端口 13360）
docker run -d --name gopherlearn-mysql -p 127.0.0.1:13360:3306 \
  -e MYSQL_ROOT_PASSWORD='<你自己的 root 口令>' \
  mysql:8.0

# Redis Stack（宿主端口 16360）。口令与配置里的 redisConfig.password 必须一致，
# 因此这里同样用配置文件声明口令：
#   cp redis.integrated.conf.example redis.integrated.conf.local   # 然后替换其中的 requirepass
docker run -d --name gopherlearn-redis -p 127.0.0.1:16360:6379 \
  -v "$PWD/redis.integrated.conf.local:/redis-stack.conf:ro" \
  redis/redis-stack-server:latest
```

> **PowerShell 与 Bash 的差异**：续行符不同（PowerShell 用反引号 `` ` ``，Bash 用 `\`），
> `-v` 里的路径建议写成**绝对路径**（Bash 可用 `"$PWD/..."`，PowerShell 里 `$PWD` 展开成对象，
> 直接写绝对路径最省事）。其余命令两边一致。
>
> `redis.integrated.conf.local` 里的 `requirepass` 必须是**一个未加引号的词**、长度 ≥ 32 字节。

### 步骤 2 · 复制并填写配置模板

```bash
cp config.integrated.example.toml config.integrated.toml.local
```

按 [§5.1](#51-部署者需要配置的字段) 填好占位符。**本地运行**时记得把主机名与端口改成**宿主**上的地址：

```toml
[mainConfig]
host = "127.0.0.1"
port = 18082

[mysqlConfig]
host = "127.0.0.1"
port = 13360

[redisConfig]
host = "127.0.0.1"
port = 16360
db = 0
```

### 步骤 3 · 建库并建账号

正式入口**拒绝**在启动时改 schema，所以库和账号要**带外**创建。下面的口令请交互输入，不要写进命令行、脚本或文档：

```bash
mysql -h 127.0.0.1 -P 13360 -u root -p
```

进入 `mysql>` 提示符后执行（把口令换成你自己的）：

```sql
CREATE DATABASE gopherlearn_v2_integration CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE USER 'gopherlearn_v2_integration'@'%' IDENTIFIED BY '换成你自己的应用口令';
GRANT ALL ON gopherlearn_v2_integration.* TO 'gopherlearn_v2_integration'@'%';
FLUSH PRIVILEGES;
```

### 步骤 4 · 建表（公开版提供的一次性命令）

```bash
# 在仓库根目录执行；go -C 会把程序的工作目录切到 v2/，所以配置写成 ../config.integrated.toml.local
go -C v2 run ./cmd/schemainit -config ../config.integrated.toml.local
```

这个迁移是**增量**的：只补齐缺失的表，不会删除或改写已有数据，可以重复执行。

### 步骤 5 · 编译并启动正式后端

```bash
# 在仓库根目录执行；程序的工作目录是 hybrid/
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
```

想先构建二进制再运行：

```bash
# 在仓库根目录执行，产物写到仓库根的 bin/（该目录已被 Git 忽略）
go -C hybrid build -o ../bin/gopherlearn-integrated ./cmd/gopherlearn-integrated
# 仍在仓库根目录运行
./bin/gopherlearn-integrated -config config.integrated.toml.local
```

> Windows 上 `go build -o` 会**按你写的名字**生成文件（不自动补 `.exe`）；想用 `.exe` 就自己写 `-o ../bin/gopherlearn-integrated.exe`。
> 后端监听 `[mainConfig]` 里的地址，本地默认 `http://127.0.0.1:18082`。

### 步骤 6 · 安装并启动前端

```bash
# 在仓库根目录执行
npm --prefix web ci
```

启动开发服务器（Bash）：

```bash
VITE_DEV_API_ORIGIN=http://127.0.0.1:18082 npm --prefix web run dev
```

PowerShell：

```powershell
$env:VITE_DEV_API_ORIGIN = 'http://127.0.0.1:18082'
npm --prefix web run dev
```

> `VITE_DEV_API_ORIGIN` 告诉开发服务器把 `/api` 转发到哪里。它**默认是 `http://127.0.0.1:8080`**（历史基线后端的地址），不是整合入口的地址，所以这里必须显式设置。
> `VITE_API_BASE_URL` 留空即可：前端会请求它自己被提供的那个源，浏览器不需要 CORS 放行。

### 步骤 7 · 打开网页

前端开发服务器在 <http://127.0.0.1:5273>。

### 步骤 8 · 注册并开始使用

打开 <http://127.0.0.1:5273>，点「用邮箱验证码注册」，填写邮箱 → 发送验证码 → 填写收到的六位码与密码 → 注册 → 登录。

如果验证码收不到：先确认 `[emailConfig]` 已配置（缺配置时接口返回 `2012`），再确认 SMTP 地址、端口、发件地址与授权码正确。

---

## 7. Docker 部署

`compose.integrated.yml` 会拉起 MySQL、Redis Stack、后端与 nginx 前端。**所有命令都带上项目名 `-p` 和配置文件 `-f`**，否则会落到别的配置上。

### 7.1 准备三个本地文件（都不提交）

| 文件 | 从哪来 | 内容 |
|---|---|---|
| `.env.compose.integrated.local` | 复制 `.env.compose.integrated.example`，替换 `MYSQL_ROOT_PASSWORD` | MySQL 容器的 root 口令 |
| `redis.integrated.conf.local` | 复制 `redis.integrated.conf.example`，替换 `requirepass` | Redis 服务端口令（≥ 32 字节） |
| `config.integrated.toml.local` | 复制 `config.integrated.example.toml`，替换占位符 | 后端自己的配置（**容器内**的取值见下） |

### 7.2 容器内与宿主不同

compose 里的 MySQL 与 Redis **不向宿主发布端口**，只有前端发布 `127.0.0.1:8083`。因此容器里的配置要用**容器内的主机名与端口**：

```toml
[mainConfig]
host = "0.0.0.0"      # 容器内要监听所有网卡
port = 8080            # nginx 反代到 backend:8080

[mysqlConfig]
host = "gopherlearn-integrated-mysql"
port = 3306

[redisConfig]
host = "gopherlearn-integrated-redis"
port = 6379
db = 0
```

### 7.3 启动顺序

```bash
# 1) 先起基础设施
docker compose -p gopherlearn-integrated -f compose.integrated.yml up -d mysql redis

# 2) 带外建库建账号（口令交互输入，不要写进命令行）
docker exec -it gopherlearn-integrated-mysql mysql -uroot -p
#   然后在 mysql> 提示符里执行：
#     CREATE DATABASE gopherlearn_v2_integration CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
#     CREATE USER 'gopherlearn_v2_integration'@'%' IDENTIFIED BY '换成你自己的应用口令';
#     GRANT ALL ON gopherlearn_v2_integration.* TO 'gopherlearn_v2_integration'@'%';
#     FLUSH PRIVILEGES;

# 3) 建表（一次）
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm schemainit

# 4) 起应用
docker compose -p gopherlearn-integrated -f compose.integrated.yml up -d --build
```

第 3 步的 `init` profile 用同一个后端镜像、把 `schemainit` 当作入口，跑完即退出；它不在普通的 `up` 里，所以正常启动**不会**去写 schema。

### 7.4 访问与运维

```bash
# 浏览器访问前端
#   http://127.0.0.1:8083

# 看日志
docker compose -p gopherlearn-integrated -f compose.integrated.yml logs -f backend
docker compose -p gopherlearn-integrated -f compose.integrated.yml logs -f redis

# 正常停止（保留数据卷，下次启动数据还在）
docker compose -p gopherlearn-integrated -f compose.integrated.yml stop

# 停止并删除容器与网络（保留数据卷）
docker compose -p gopherlearn-integrated -f compose.integrated.yml down
```

> ⚠️ `docker compose -p gopherlearn-integrated -f compose.integrated.yml down -v` 会**连同数据卷一起删除**（MySQL 数据、Redis 索引、知识库卷都没了）。**不要把它当作普通停止方式**，只在确实要清空这套环境时使用。

### 7.5 关于 `create_host_path: false` 与 preflight

承载操作员文件的 bind 挂载都声明了 `bind.create_host_path: false`（键名就是 `create_host_path`——`create_host_directory` **不是** Compose 的键，Compose v5.1.3 会直接拒绝解析带它的文件）。

实测结论要分清两层：

- **在本机 Windows（Docker Desktop）上**：引擎仍可能把缺失的 bind 源建成一个**空目录**（该键被 compose 接受并保留，但引擎没有执行它）。
- **但 Redis 服务不会因此带病启动**：索引容器的入口是仓库自带的 `deploy/redis-stack-preflight.sh`，它在碰到镜像原入口之前就检查配置——缺文件、空文件、`requirepass` 不合规都会被拒绝，容器起不来、日志里是 `redis-stack preflight: configuration missing`。后端入口同样拒绝在没有 `-config` 文件时启动。

---

## 8. 注册与使用

1. **注册**：邮箱 → 发送验证码 → 填入六位码与密码。验证码由**部署者**配置的 SMTP 发出。
2. **普通聊天**：左侧「对话」→ 新建对话 → 提问。回答逐段返回；关掉页面再回来，消息仍在。
3. **文档问答**：左侧「文档」→ 上传 `.md` / `.txt` → 观察状态从 `pending` 走到 `ready`（这一步会真实调用 embedding）→ 之后在**学习 Agent** 会话里用**精确文件名**提问，回答会带 `[n]` 引用，展开可看原文行范围。
4. **学习 Agent**：左侧「对话」→「新建学习 Agent 会话」，提问时可看到工具调用与工具结果。
5. **学习／测验**：左侧「学习／测验」→ 选择 learning_agent 会话 → 填主题 → 生成原测验 → 作答 → 提交判分。刷新页面后，用「**查询／恢复活动任务**」把未做完的测验恢复出来；「查询原掌握度」可回读主题掌握度。

---

## 9. 常见问题

**Q：启动报 `explicit model settings required; environment credentials are not read` / `explicit independent setting required: …`**
配置没填对，或键名写错。入口要求 `[isolatedEntry]`、`[mysqlConfig]`、`[redisConfig]`、`[mainConfig]`、`[knowledgeConfig]`、`[ragModelConfig]` 的字段**显式存在**；凭据只能写在 TOML 里，环境变量不算。

**Q：注册时提示"邮件服务未配置"（业务码 2012）**
`[emailConfig]` 没配或为空。这只影响新用户注册，已有账号和其它功能照常可用。

**Q：前端能打开，但接口 404 / 500**
多半是 `VITE_DEV_API_ORIGIN` 没设或指错了。它必须指向**正式后端**的地址（本地默认 `http://127.0.0.1:18082`），默认的 `http://127.0.0.1:8080` 是历史基线后端。

**Q：知识库问答一直说"没有找到证据"**
先确认文档状态是 `ready`；再确认提问里用的是**精确文件名**而不是标题（见 [§3](#3-当前版本与已知限制)）。

**Q：能不能传 PDF？**
不能。当前只实现了 `.md` 与 `.txt`，单文件上限 5 MiB。

**Q：检索报"不可用" / 向量检索失败**
Redis 必须是**带 RediSearch 模块的 Redis Stack**（普通 Redis 没有 `FT.*` 命令）；同时 `redisConfig.password` 必须与实例的 `requirepass` 逐字一致。

**Q：Docker 里后端反复重启**
多半是 schema 不存在：先带外建库建账号，再显式跑一次 `--profile init run --rm schemainit`。

**Q：端口被占用**
改 `[mainConfig].port`（后端），前端用 `npm --prefix web run dev -- --port 5274` 换端口；容器栈的前端端口在 `compose.integrated.yml` 里改。

**Q：学习任务刷新后不见了**
点「查询／恢复活动任务」恢复。切换会话**不会**自动恢复，这是页面自己写明的合同。

**Q：模型调用要花钱吗？**
要，消耗**部署者**配置的 API 额度。注册用户不消耗你的额度。

---

## 10. 开发与贡献

仓库是三个 Go 模块 + 一个 npm 包：

| 目录 | 模块 | 作用 |
|---|---|---|
| `v2/` | `GopherAI` | 应用内核：会话、文档、记忆、学习、测验 |
| `hybrid/` | `gopherlearn-v2-hybrid` | **正式入口** `cmd/gopherlearn-integrated`：把内核、Agent 与普通聊天装配起来 |
| 根目录 | `github.com/xiaopang456/GopherLearn` | 历史基线入口 `cmd/gopherlearn`（不是默认启动入口，说明见 [历史基线开发参考](docs/LEGACY-DEVELOPMENT-REFERENCE.md)） |
| `web/` | npm 包 | Vue 3 客户端 |

验证命令（三个模块各自独立，默认不需要 Docker、数据库或网络）：

```bash
# 根模块（历史基线）
go vet ./cmd/... ./internal/... ./conversation/...
go test -count=1 ./cmd/... ./internal/... ./conversation/...

# v2 模块
go -C v2 vet ./...
go -C v2 test -count=1 ./...

# hybrid 模块
go -C hybrid vet ./...
go -C hybrid test -count=1 ./...

# 前端
npm --prefix web ci
npm --prefix web run lint
npm --prefix web run test
npm --prefix web run build
```

> `hybrid` 模块里有三个用例会真正执行 `deploy/*.sh`，需要 PATH 里有 POSIX shell（Windows 上把 Git 的 `usr\bin` 加进 PATH 即可）。

贡献前请读 [CONTRIBUTING.md](CONTRIBUTING.md) 与 [AGENTS.md](AGENTS.md)。

---

## 11. 来源与许可证

- 本项目基于 [GopherAI](https://github.com/youngyangyang04/GopherAI) 开发，并在此之上自主扩展了学习 Agent 等功能。GopherAI 采用 GNU GPL v3.0，本项目**沿用 GPL-3.0**。
- 完整许可证文本见 [LICENSE](LICENSE)。
- 上游归属、相关衍生项目与本次发布范围见 [NOTICE.md](NOTICE.md)。
- 第三方依赖及其各自声明的许可证见 [docs/THIRD-PARTY.md](docs/THIRD-PARTY.md)。
- 历史基线入口的开发说明、环境变量表与冒烟检查见 [docs/LEGACY-DEVELOPMENT-REFERENCE.md](docs/LEGACY-DEVELOPMENT-REFERENCE.md)（**仅适用于历史基线入口**）。
