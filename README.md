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
> [配置参考](docs/CONFIGURATION.md)、
> [正式入口技术参考](docs/INTEGRATED-REFERENCE.md)、
> [整合栈部署参考](docs/INTEGRATED-DEPLOYMENT.md)、
> [凭据处理](docs/CREDENTIAL-HANDLING.md)、
> [历史基线开发参考](docs/LEGACY-DEVELOPMENT-REFERENCE.md)、
> [发布说明](docs/PUBLIC-RELEASE-NOTES.md)、
> [真实模型验证计划](docs/REAL-MODEL-VALIDATION-PLAN.md)。

---

## 快速开始

从零到能用，六步。每一步都链接到后面更详细的说明：

1. **准备 MySQL 与 Redis Stack** —— 一个 MySQL 8.0+ 和一个带 RediSearch 的 Redis Stack，见 [§4 环境要求](#4-环境要求) 与 [§6 步骤 1](#步骤-1--准备依赖与基础设施)。
2. **复制配置模板** —— `cp config.integrated.example.toml config.integrated.toml.local`，见 [§5 配置说明](#5-配置说明)。
3. **填必填的几项** —— 只有密钥、地址、口令、目录要改，其余照抄示例，见 [§5.1 最少要填的配置](#51-最少要填的配置)。
4. **建库、建账号、建表** —— 正式入口不自己改 schema，要带外建，见 [§6 步骤 3](#步骤-3--建库并建账号) 与 [§6 步骤 4](#步骤-4--建表一次性命令)。
5. **启动后端与前端** —— 两条 `go run` / `npm run dev`，见 [§6 步骤 5](#步骤-5--编译并启动正式后端) 与 [§6 步骤 6](#步骤-6--安装并启动前端)。
6. **注册，开始使用** —— 邮箱验证码注册，见 [§8 注册与使用](#8-注册与使用)。

> 只想先跑起来：直接照抄 [§5.1 的完整示例](#51-最少要填的配置) 里的那段 TOML，把密钥和口令换成你自己的。

---

## 1. 项目介绍

GopherLearn 面向"用自己的资料学习"这件事，提供两种会话形态，共用同一套账号与数据：

- **普通聊天（chat）**：普通对话，流式回答。
- **学习 Agent（learning_agent）**：带工具的自主回路，模型可以在服务端允许的范围内检索你的文档、检索与写入你的长期记忆，并把结果整理成回答。

在此之上还有三个相对独立的表面：

- **用户级知识库**：上传自己的 `.md` / `.txt`，服务端进程内解析、切片、嵌入并建立向量索引；回答时可以检索这些文档，并给出可点开的引用（文档、版本、行号）。
- **记忆**：需要确认才入库的长期记忆、主题记忆、情景记忆；记忆只属于它的主人。
- **学习／测验（quiz）**：按主题出题、作答判分，并把结果累计到主题掌握度上。

文档处理**全部在进程内（in-process）完成**：解析、切片、嵌入、索引都发生在后端进程里。

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

这是**开发预览版**：核心链路已经跑通，前端样式与交互还会继续迭代。

**部署和使用前请知道这几条**：

- **文档只支持 `.md` 与 `.txt`**（单文件上限 5 MiB），不支持 PDF。
- **模型调用消耗部署者配置的 API 额度**，注册用户不消耗你的额度。
- **检索重排默认关闭**，需要更准的检索时按 [§5.2 可选功能](#52-可选功能) 开启。
- **指代文档要用精确文件名**（例如 `notes.md`），检索工具只认文档 id 或精确文件名。
- **检索与判分的普遍质量没有被证明**，只做过单账号、单个自建文档、一次判分的小规模验证。

验证到什么程度、还有哪些技术层面的限制，见 [发布说明 · 已经验证过的能力](docs/PUBLIC-RELEASE-NOTES.md#已经验证过的能力)与 [发布说明 · 已知限制](docs/PUBLIC-RELEASE-NOTES.md#已知限制)。

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

先复制模板：

```bash
# 在仓库根目录执行
cp config.integrated.example.toml config.integrated.toml.local      # Bash
Copy-Item config.integrated.example.toml config.integrated.toml.local   # PowerShell
```

配置**只从一个文件读进来**（`config.integrated.toml.local`）。下面三节分别讲：**最少要填什么**、**可选功能怎么开**、**配置与密钥从哪来**。

### 5.1 最少要填的配置

部署者真正**必须**给的，就是下面这些。除两个口令外，少一个都会在**启动时被拒绝**；那两个口令启动时不检查，但填不对就连不上。

| 段 | 必填的键 | 说明 |
|---|---|---|
| `[isolatedEntry]` | `isolated`、`migrate`、`chatKey`、`chatModel`、`chatBaseURL` | `isolated` 必须写 `true`，`migrate` 必须写 `false`；三个 chat 项不能为空 |
| `[ragModelConfig]` | `embeddingModel`、`embeddingBaseUrl`、`embeddingApiKey`、`dimension`、`chatModelName` | `dimension` 必须与 embedding 模型实际输出的维度一致，写错会让已有向量对不上；嵌入密钥与它认证的模型在同一段 |
| `[mysqlConfig]` | `host`、`port`、`user`、`databaseName`、`charset`、`password` | 本次部署自己的 schema 与账号 |
| `[redisConfig]` | `host`、`port`、`db`、`password` | 独立的 Redis Stack 实例；口令必须与实例的 `requirepass` **逐字一致** |
| `[mainConfig]` | `host`、`port` | 后端监听地址 |
| `[knowledgeConfig]` | `storageDir` | 上传原文件的存放目录，需要可写 |
| `[jwtConfig]` | `key` | 令牌签名密钥，**至少 32 字节**；换掉它会让所有已签发令牌失效 |

**模型相关的两段配置分别管什么**——这是最容易填错的地方：

- `[isolatedEntry]` —— **chat 模型**（地址、模型名、密钥）。学习 Agent 与普通对话现在共用这一个模型：一轮普通对话就是同一个回路上不开能力的一轮，所以不再单独配第二个 chat 客户端；
- `[ragModelConfig]` —— **embedding 服务**的模型名、地址、密钥与向量维度（四件都在这一段），情景关系分类器**身份标签**里的模型名（它不决定答题用哪个模型，但请写成和上面 `chatModel` 一样的值），以及**思考开关的方言**。

原来的 `[ordinaryChat]` 一段已经废弃：写它会被指名拒绝并说明原因，而不是被忽略掉。

下面是一个**可以直接抄**的例子（DeepSeek 聊天 + 百炼 embedding）：只有密钥和口令要换成你自己的，其余值照抄即可。

```toml
[isolatedEntry]
isolated = true
migrate = false
# 学习 Agent 的 chat 模型
chatKey = "<你的 DeepSeek API Key>"
chatModel = "deepseek-flash"
chatBaseURL = "https://api.deepseek.com/v1"

[ragModelConfig]
# embedding 的模型名、地址、密钥与向量维度（四件都在这一段）
embeddingModel = "qwen3.7-text-embedding"
embeddingBaseUrl = "https://dashscope.aliyuncs.com/compatible-mode/v1"
embeddingApiKey = "<你的百炼 API Key>"
dimension = 1024
# 情景关系分类器的身份标签用这个名字（答题用的模型来自上面的 chatModel）：请写成和 chatModel 一样的值
chatModelName = "deepseek-flash"
# 思考开关的方言：DeepSeek 用 deepseek（默认的 qwen 写法它不认）
chatThinkingDialect = "deepseek"

[mysqlConfig]
host = "127.0.0.1"
port = 13360
user = "gopherlearn_v2_integration"
password = "<你的应用口令>"
databaseName = "gopherlearn_v2_integration"
charset = "utf8mb4"

[redisConfig]
host = "127.0.0.1"
port = 16360
db = 0
# 必须与 Redis 实例的 requirepass 逐字一致
password = "<与 redis.integrated.conf.local 里的 requirepass 一样的值>"

[jwtConfig]
# 至少 32 字节；换掉它会让所有已签发的令牌失效
key = "<换成你自己的随机串，至少 32 字节>"

[knowledgeConfig]
# 上传原文件的存放目录，需要可写
storageDir = "data/gopherlearn-integrated/knowledge"

[mainConfig]
host = "127.0.0.1"
port = 18082
```

> 换 chat provider 只改**一处**：`[isolatedEntry]` 的 `chatKey` / `chatModel` / `chatBaseURL`。学习 Agent 与普通对话共用这一个模型，所以过去要重复填的第二处（`[ordinaryChat]`）已经没有。`chatModelName` 是关系分类器的身份标签，**照抄 `chatModel`**；`chatThinkingDialect` 改成对方认的写法（默认 `qwen`）。
> 换 embedding 服务只改一段：`[ragModelConfig]` 的 `embeddingModel` / `embeddingBaseUrl` / `embeddingApiKey`，外加 `dimension` 要与新模型的实际输出维度一致。
> 完整字段表、每个字段的默认值与校验规则见 [配置参考](docs/CONFIGURATION.md)。

### 5.2 可选功能

**检索重排（默认关闭）** —— 开启后候选切片会再用交叉编码器重排，检索更准，也多一次模型调用。
改 `[ragModelConfig]` 的 `rerankEnabled`、`rerankProvider`、`rerankEndpoint`、`rerankModel`，并把重排密钥放进环境变量 `RAG_RERANK_AUTH_TOKEN`（**密钥不写进配置文件**）。
远程 API 与本地模型服务两种形态、地址怎么填、超时怎么调，见 [配置参考 · 重排](docs/CONFIGURATION.md#重排可选)。

**深度思考（默认关闭）** —— 开启后学习 Agent 的模型调用会带上思考开关，答案更慢也更贵。
改 `[agentConfig]` 的 `agentThinkingEnabled = true`；思考开关具体怎么写由 `[ragModelConfig]` 的 `chatThinkingDialect` 决定（DeepSeek 用 `deepseek`）。
方言的取值与含义、以及 DeepSeek 上多轮工具调用的那个特殊要求，见 [配置参考 · 思考开关与方言](docs/CONFIGURATION.md#思考开关与方言)。

**注册邮件（默认不配置）** —— 只有「邮箱验证码注册」需要它。
配 `[emailConfig]` 的 `email` 与 `authcode`（SMTP 授权码，不是登录密码），`host` / `port` 留空则用 `smtp.qq.com:587`。
不配也能启动，已有账号与其它功能不受影响，只是新用户注册走不通（接口返回业务码 `2012`）。

### 5.3 配置从哪里读、密钥放哪里

- 正式入口**只读 `-config` 指向的那一个 TOML 文件**：它不读 `.env`，也不读环境变量里的凭据（`v2/internal/isolated/settings.go` 写得很直白：`environment credentials are not read`）。
- **唯一的例外是重排令牌 `RAG_RERANK_AUTH_TOKEN`**：它是密钥，不能写进配置文件，所以开启重排时只从环境变量读这一个。聊天与 embedding 的密钥仍然只从配置文件读。
- 真实配置请放在 `config.integrated.toml.local`：后缀 `.local` 同时命中 `.gitignore` 与 `.dockerignore` 的 `*.local` 规则，不会被提交，也不会进构建上下文。
- 模板文件里只有占位符；**任何真实凭据都不要写进模板、日志或报告**。更多见 [凭据处理](docs/CREDENTIAL-HANDLING.md)。

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
#
# 三处都要照做：
#   * 数据目录挂出来（-v ...:/data + REDIS_DATA_DIR=/data）：不挂的话容器一删，索引就没了。
#   * 用仓库里的 deploy/redis-stack-start.sh 起服务：镜像自带的入口会把 redis-server 放在
#     shell 下面，停止信号到不了服务本身，正常停止时最后那张快照就不会写，容器一停，
#     上次快照之后写入的内容全丢（详见「Redis 持久化」一节）。
#   * 示例配置里已经开了 AOF（appendonly yes），写入不会只活在内存里。
docker run -d --name gopherlearn-redis -p 127.0.0.1:16360:6379 \
  -v "$PWD/redis.integrated.conf.local:/redis-stack.conf:ro" \
  -v "$PWD/deploy/redis-stack-start.sh:/redis-stack-start.sh:ro" \
  -v gopherlearn-redis-data:/data \
  -e REDIS_DATA_DIR=/data \
  --entrypoint /bin/sh \
  redis/redis-stack-server:latest /redis-stack-start.sh
```

> **PowerShell 与 Bash 的差异**：续行符不同（PowerShell 用反引号 `` ` ``，Bash 用 `\`），
> `-v` 里的路径建议写成**绝对路径**（Bash 可用 `"$PWD/..."`，PowerShell 里 `$PWD` 展开成对象，
> 直接写绝对路径最省事）。其余命令两边一致。
>
> `redis.integrated.conf.local` 里的 `requirepass` 必须是**一个未加引号的词**、长度 ≥ 32 字节。

#### Redis 持久化

检索索引在 Redis 里，MySQL 里只记着「这个文档已经就绪」。所以 Redis 重启后如果索引是旧的或
不完整的，检索会**悄悄变差**：命中的块变少、找不到本该找到的文档，但不会有任何报错。

因此这一份部署要满足两件事：

| 要做到 | 怎么做 |
|---|---|
| 写入不只活在内存里 | 配置文件里开 `appendonly yes` + `appendfsync everysec`（示例配置已经开了）；RDB 快照也保留 |
| 正常停止不丢数据 | 用 `deploy/redis-stack-start.sh` 起服务（见上面的命令），并把数据目录挂到 `/data` |

镜像自带的入口脚本会把 `redis-server` 作为 **shell 的子进程**跑起来：停止信号先到 shell，
shell 死掉，服务还活着，容器在宽限期结束后被强杀——**服务来不及写最后那张快照**。仓库里的
`deploy/redis-stack-start.sh` 做的是同样的事，但最后一步用 `exec` 交出进程，服务自己就是
1 号进程，收到停止信号后会写完快照再退出。

改这两处之后需要**重启一次 Redis 容器**才生效；重启前写入的内容不会丢（AOF 会补上）。

### 步骤 2 · 复制并填写配置模板

```bash
cp config.integrated.example.toml config.integrated.toml.local
```

按 [§5.1 最少要填的配置](#51-最少要填的配置) 填好占位符。**本地运行**时主机名与端口要指向**宿主**上的地址——§5.1 的示例已经是本地值，不用再改。

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

### 步骤 4 · 建表（一次性命令）

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

开发服务器起来后打开 <http://127.0.0.1:5273>。

### 步骤 7 · 注册并开始使用

打开 <http://127.0.0.1:5273>，按 [§8 注册与使用](#8-注册与使用) 注册后即可开始使用。

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
   注册者**不需要**提供模型 API Key，**不需要**提供 SMTP 授权码——那两样只由部署者配置一次。
2. **普通聊天**：左侧「对话」→ 新建对话 → 提问。回答逐段返回；关掉页面再回来，消息仍在。
3. **文档问答**：左侧「文档」→ 上传 `.md` / `.txt` → 观察状态从 `pending` 走到 `ready`（这一步会真实调用 embedding）→ 之后在**学习 Agent** 会话里用**精确文件名**提问，回答会带 `[n]` 引用，展开可看原文行范围。
4. **学习 Agent**：左侧「对话」→「新建学习 Agent 会话」，提问时可看到工具调用与工具结果。
5. **学习／测验**：左侧「学习／测验」→ 选择 learning_agent 会话 → 填主题 → 生成原测验 → 作答 → 提交判分。刷新页面后，用「**查询／恢复活动任务**」把未做完的测验恢复出来；「查询原掌握度」可回读主题掌握度。

---

## 9. 常见问题

**Q：启动报 `explicit model settings required; environment credentials are not read` / `explicit independent setting required: …`**
配置没填对，或键名写错。入口要求 `[isolatedEntry]`、`[mysqlConfig]`、`[redisConfig]`、`[mainConfig]`、`[knowledgeConfig]`、`[ragModelConfig]` 的字段**显式存在**；凭据只能写在 TOML 里，环境变量不算。

**Q：升级后启动报 `retired configuration setting: …`**
配置文件里留着这个版本**不再读取**的键。它被指名拒绝而不是被忽略，是因为宽容地忽略会让程序与配置各说各话——一条没人用的地址或密钥看起来还在，实际已经不算数。提示会写清改哪里：`ragModelConfig.baseUrl` 删掉（Agent 的地址只用 `isolatedEntry.chatBaseURL`）；`isolatedEntry.embeddingKey` 挪成 `ragModelConfig.embeddingApiKey`。

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
| `hybrid/` | `gopherlearn-v2-hybrid` | **正式入口** `cmd/gopherlearn-integrated`：装配内核，Agent 回合与普通对话回合共用同一条回路 |
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
