# GopherLearn 发布说明（开发预览版）

> 当前版本：**v0.3.0**。v0.1.1 起可以开启检索重排序，见 [README §5.2 可选功能](../README.md#52-可选功能) 与 [配置参考 · 重排](CONFIGURATION.md#重排可选)。

## 待发布

- Agent 单轮默认上限由工具 3 次、模型 4 次调整为工具 4 次、模型 5 次；预检索仍计入工具次数。截止时间和图步数上限不变。
- 知识库引用支持半角与全角方括号，引用编号仍须对应真实来源。

## v0.3.0 · 分层摘要与一次部署整理

这一版是一次发布里的两件事合在一起：**分层摘要**——长会话的窗口带的不再是一份滚动摘要，而是"现在是什么"和"当时说过什么"两层结构化的状态，并且默认开启；以及**一次部署整理**——普通对话与学习 Agent 走同一条回路、模型配置收拢成一处一个、Redis 停止时不再丢数据、界面上的知识库开关改成按轮选择。整理部分多数是**升级前要改配置**的破坏性变更，步骤见下面第一节。

### 升级步骤（先做这几件）

从 v0.2.1 升级到这一版，按下面的顺序做。顺序是有讲究的：配置改在前，备份紧随其后，然后才是换后端、
跑转换、重启和重建前端。

**1. 改配置：模型配置收拢成一处一个。** 嵌入服务的密钥挪到 `[ragModelConfig]`，那条没人用的地址删掉：

```toml
# 改之前
[isolatedEntry]
chatKey = "..."
chatModel = "..."
chatBaseURL = "https://provider.invalid/v1"
embeddingKey = "..."                        # ← 这一行挪走

[ragModelConfig]
embeddingModel = "..."
embeddingBaseUrl = "https://embedding.invalid/v1"
baseUrl = "https://provider.invalid/v1"     # ← 这一行删掉，没有任何客户端用它

# 改之后
[isolatedEntry]
chatKey = "..."
chatModel = "..."
chatBaseURL = "https://provider.invalid/v1"  # Agent 的地址只有这一处

[ragModelConfig]
embeddingModel = "..."
embeddingBaseUrl = "https://embedding.invalid/v1"
embeddingApiKey = "..."                      # ← 密钥挪到这里，与它的模型名、地址同段
```

**2. 改配置：删掉 `[ordinaryChat]` 整段。** 普通对话与学习 Agent 现在用同一个模型，就是
`[isolatedEntry]` 里那套地址、模型名与密钥；普通对话只是这一轮**没有开启知识库能力**而已。
过去把同一个模型在两段里各填一遍的写法不再需要：

```toml
# 改之前
[isolatedEntry]
chatKey = "..."
chatModel = "..."
chatBaseURL = "https://provider.invalid/v1"

[ordinaryChat]                                        # ← 整段删掉
GOPHERLEARN_AI_PROVIDER = "openai_compatible"
GOPHERLEARN_OPENAI_BASE_URL = "https://provider.invalid/v1"
GOPHERLEARN_OPENAI_API_KEY = "..."
GOPHERLEARN_OPENAI_MODEL = "..."
# GOPHERLEARN_CONTEXT_MAX_MESSAGES、GOPHERLEARN_SUMMARY_* 等可选项一并作废

# 改之后
[isolatedEntry]
chatKey = "..."
chatModel = "..."
chatBaseURL = "https://provider.invalid/v1"          # Agent 与普通对话共用这一处
```

这两处**旧写法会被指名拒绝，不是被忽略**：配置文件里留着一行不再读取的地址或密钥，看起来还在、实际已经不算数，配置与程序各说各话——这类差别最后表现为一次莫名的失败或一笔莫名的费用，而不是一个错误。所以带这些行的配置**起不来**，报错会写清改哪里（原文见下面「模型配置改成一处一个」与「普通聊天不再有独立的模型配置」两节）。

`[ordinaryChat]` 里的上下文预算与摘要触发项**没有替代品**：一轮对话带多少历史、什么时候生成摘要，由程序自己的装配决定，配置文件里不再有第二套要填的值。

**3. 改配置：Redis 加上 AOF。** 在 `redis.integrated.conf.local` 里补上这三行（示例文件
`redis.integrated.conf.example` 里已经带上了，复制时不要删）：

```ini
appendonly yes
appendfsync everysec
save 3600 1 300 100 60 10000
```

**但这一步不能只改完就重启**——原因见下面第 5 步的实测。

**4. 备份：先把 Redis 的快照落下来并留一份副本。** 这一步在改 AOF 之前做，也在做别的任何
改动之前做：

```bash
docker compose -f compose.integrated.yml exec redis redis-cli -a "$REDIS_PASSWORD" SAVE
docker compose -f compose.integrated.yml exec redis redis-cli -a "$REDIS_PASSWORD" --rdb /data/dump.rdb
# 把数据卷里的 dump.rdb 复制一份到卷外面：它就是出问题时的最后一道退路
docker compose -f compose.integrated.yml cp redis:/data/dump.rdb ./dump.rdb.$(date +%Y%m%d).backup
```

**5. Redis 运行中开启 AOF，等重写完成，再改配置重启。** 这一步的顺序是实测出来的，不是推测。
用整合栈同款镜像（`redis/redis-stack-server`，digest 固定）在一次性容器里跑过两种做法，结论如下。

**先说不能做的那种**：把 `appendonly yes` 写进配置、然后直接重启，**服务器会带着一个空的索引起来**。
实测：容器里先有 500 个键（只写了 `dump.rdb`，没有 AOF 文件），把 `appendonly yes` 加进配置再重启，
`DBSIZE` 变成 **0**——Redis 启动时读的是那个刚建出来的空 AOF 文件，不是 `dump.rdb`。
此刻 `dump.rdb` **还没有被改**（大小和时间戳都没变，那 500 个键还在文件里），但服务器已经是空的了；
再下一次快照（`save` 策略会自己做）就会把 `dump.rdb` 写成那份空数据——实测从 18417 字节 500 个键变成
189 字节 1 个键，**原来那份数据就没了**。而升级前备份的那一份另起一个容器读，仍然是 500 个键。

**能做的做法**：在 Redis **运行中**开启 AOF，等重写完成，再改配置重启：

```bash
# ① 运行中开启 AOF
docker compose -f compose.integrated.yml exec redis redis-cli -a "$REDIS_PASSWORD" CONFIG SET appendonly yes

# ② 等重写完成：aof_rewrite_in_progress 变成 0 且 aof_last_bgrewrite_status 是 ok 才算完
docker compose -f compose.integrated.yml exec redis redis-cli -a "$REDIS_PASSWORD" INFO persistence
#    aof_enabled:1
#    aof_rewrite_in_progress:0
#    aof_last_bgrewrite_status:ok
```

重写完成之后再把 `appendonly yes` 写进配置文件（第 3 步已经写好了），然后按第 8 步重启。
**实测**：同样 500 个键，走这条流程重启之后 `DBSIZE` 是 **501**（500 个原有键，加上重写完成后新写的那个），
原来那个键和重写后新写的键都在。

**6. 换后端：先构建新版后端（或拉取新镜像），这一版的后端带了两件新东西，
**旧的镜像里都没有**：

- **分层摘要**（白板与摘要分段）——旧后端不写也不读，配置里的 `summarySegmentMode` 对它没有意义。
- **旧摘要的自动转换**——`summaryconvert` 是新后端里的命令，旧镜像里没有这个命令。

所以这两件事都要等后端换成新的才生效；只改配置不换后端，升级等于没做。

```bash
# 从源码起：用这一版的代码构建
docker compose -f compose.integrated.yml build backend
# 或用发布的镜像：把 compose 文件里的 backend 镜像换成新版本
```

**7. 建表：用新版的 `schemainit` 升级数据库结构。** 分层摘要要用的三张新表
（白板、摘要分段、分段任务）**不会自己出现**：正式整合入口拒绝在启动时改 schema
（`isolatedEntry.migrate = true` 会被入口拒绝），所以启动时既不会建表、也不会顺手把
`messages.user_name` 的列宽改宽。建表是**独立的一步**，要在新版后端**启动之前**做：

```bash
# 源码起：这一版的代码里有这个命令
go -C v2 run ./cmd/schemainit -config ../config.integrated.toml.local
# 容器起：compose 文件里备好了同一个命令的服务（用后端镜像里的 /schemainit）
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm schemainit
```

这一步是**加法**的：只建缺的表、列和索引，不删表、不改写已有的行，已经写进去的数据原样留着。
跑完会看到一行 `schema initialized in <库名> on <主机>:<端口>`。

**8. 跑一次 `summaryconvert`。** 把旧的普通对话摘要转成这一版读得懂的形状。

**跑两遍：先预演，看清楚要改什么；再正式转换。**

```bash
# ① 预演：逐行分类并报数（现行 / 根 JSON / 纯散文 / 读不出来 / 空行），不写一行库
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm summaryconvert \
  -config /config/integrated.toml

# ② 正式转换：把两种旧形状机械地、无损地写成现行 JSON，水位线不动，版本号 +1
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm summaryconvert \
  -config /config/integrated.toml -apply
```

> **为什么两遍都要写 `-config`**：`docker compose run` 的 SERVICE 之后第一个参数是**整条命令**，
> 不是追加给命令的参数。所以只写 `run --rm summaryconvert -apply` 会把服务里配好的
> `-config /config/integrated.toml` 顶掉，命令转而去找它自己的默认路径 `config/config.toml`，
> 然后以 `load configuration: … no such file or directory` 结束。**两遍都写全 `-config`**。

这个服务用**后端镜像里的 `/summaryconvert`**，与后端在同一个网络里，因此能连到 MySQL——整合栈的
MySQL 不发布宿主端口，在宿主机上直接跑这条命令是连不上数据库的（`no such host`）。

**默认只预演，不自动跑，也不在启动时跑**——跑之前那些会话照常工作，只是摘要停在原地。转不了的行
（读不出来的 JSON、引用不是消息号因而无法无损搬运、超出当前文档预算）一律**列出来、不改那一行**。

> **备选：从源码跑。** 手里有源码、且配置里的 MySQL 地址在宿主机上可达时（比如数据库端口已发布），
> 也可以不走容器：
> ```bash
> go -C v2 run ./cmd/summaryconvert -config ../config.integrated.toml.local
> # 加上 -apply 才是正式转换
> ```
> 两种跑法做的是同一件事，用哪一个都行；**先预演、再转换**的顺序不变。

**9. 换 Redis 的启动入口：重建 redis 容器。** 这里换入口的是 **Redis 容器，不是后端**。

镜像自带的 `/entrypoint.sh` 把 `redis-server` 当作 **shell 的子进程**跑起来：停止信号先到 shell，
shell 死掉、服务还活着，容器在宽限期结束后被强杀，**最后那张快照来不及写**——正常停止就会丢掉
上次快照之后的全部写入。这一版换用仓库里的 `deploy/redis-stack-start.sh` 接手：同样的命令、
同样的模块，但最后一步是 `exec`，`redis-server` 自己就是 1 号进程，收到停止信号后写完快照再退出。

**后端的入口没有变**：它本来就直接以 `/gopherlearn-integrated` 启动，不经 shell 包一层。

用 compose 起的（**要重建，不是 restart**——重建才会带上新的挂载与新的入口）：

```bash
docker compose -p gopherlearn-integrated -f compose.integrated.yml up -d --force-recreate redis
```

按 README §6 步骤 1 手工 `docker run` 起的：把原来那条命令换成 README 里的新版——要**多挂一个数据目录**
（`/data`）、**多挂 `deploy/redis-stack-start.sh`**，并用它当入口：

```bash
docker rm -f gopherlearn-redis
docker run -d --name gopherlearn-redis -p 127.0.0.1:16360:6379 \
  -v "$PWD/redis.integrated.conf.local:/redis-stack.conf:ro" \
  -v "$PWD/deploy/redis-stack-start.sh:/redis-stack-start.sh:ro" \
  -v gopherlearn-redis-data:/data \
  -e REDIS_DATA_DIR=/data \
  --entrypoint /bin/sh \
  redis/redis-stack-server:latest /redis-stack-start.sh
```

**10. 重启后核对 Redis 的键数。** 这一步是上面那套流程的验收：

```bash
docker compose -f compose.integrated.yml exec redis redis-cli -a "$REDIS_PASSWORD" DBSIZE
```

**核对的是"重启前后键数没有变少"**，不是某个固定的数字——每个部署的索引大小不同。少了就是有问题，
按下面这段处理。

**11. 重新构建并重启前端。** 界面这一版有变化（知识库开关改成按轮选择、会话列表里 Agent 会话置顶）。`web/package.json` 与 `web/package-lock.json` 未改动，不需要重新执行 `npm --prefix web ci`，**重新构建一次前端**即可。

**万一索引真的空了怎么办：Redis 里存的是检索索引，不是数据。** 索引可以**从 MySQL 重建**——
MySQL 里存着每一个文档的原文与它该有多少个块，Redis 里只是这些块的可检索副本。所以索引空了
不等于资料没了：服务**启动时的索引校验**会把这种情况**报告出来**（`knowledge_index_audit`：
比过了几个文档版本、索引里少了几个、多出来几个、有没有索引本身的问题），默认只报告不改东西；
把 `[knowledgeConfig.startupIndexAudit].mode` 配成 `rebuild`，会把**块数比记录少的那些文档**
重新送去索引（`rebuildLimit` 限着一次最多送多少个回去，默认 20）。这个校验**从不拒绝启动**，
也从不删东西。

**rebuild 只管"缺块"，不管"索引本身的问题"**：块数比记录少的文档会被退回重建；索引本身的问题
（索引被删掉、自报的文档数与键数对不上、schema 不符、仍在索引中、有索引失败）**只报告、不退回
文档**——块都在的文档不算缺块，退回去只会把已经在 Redis 里的块重新嵌入一遍。

**数据库结构要升一次，但不用手写 DDL**：白板、分段、分段任务三张新表，以及 `messages.user_name`
的列宽，都由**第 7 步的 `schemainit`** 建好（它只建缺的东西，不改已有的行）。这一步**不能省**——
正式整合入口拒绝在启动时改 schema，所以服务启动时不会替你建表，也不会替你改列宽。

**分层摘要没有新增必填项**：不写 `summarySegmentMode` 就是 `on`（默认开启）。想留在旧的滚动摘要上，就在 `[agentConfig]` 里显式写 `summarySegmentMode = "off"`，重启即可。

**客户端还有一处要改**：流式回答的帧换了字段名。最要改的是 `delta` 变成 `content`：`content` 不是新到的一段，而是截至此刻的**整段回答**，所以客户端要用它**替换**已经显示的内容，不能再往后**追加**——追加会把回答显示两遍。完整对照见下面「流式回答的帧换了字段名」一节。仓库里的界面客户端已同步改好；直接对接 `/stream` 的第三方客户端要按那张表改字段名。

### 分层摘要

长会话以前只带一份滚动摘要：越往后写，它越像"把前面所有轮次压成一段话"，而同一段话要同时回答「现在是什么样」和「当时说过什么」这两件不一样的事。这一版把它拆成两层结构化的状态，并且**默认开启**。

#### 窗口里带的是什么

- **白板**（表 `session_whiteboards`）：当前还成立的事实——目标、计划、每个计划项现在的值。一个值被改过以后，白板上会留着旧值并标出「已改为」，没标的那个才是现在的值。
- **摘要分段**（表 `session_summary_segments`）：早先那些轮次的压缩记录，按主题分段，每条都带消息编号作为出处。分段既可以常驻在窗口里，也可以按当前问题检索出来再放进去；模型还能用新工具 `browse_summary_segments` 自己翻这个会话更早的分段。

窗口里同时写明了三者之间的优先级：**最近的消息 > 当前状态（白板）> 摘要分段**。三者不一致时，最近的消息说的算；分段和当前状态不一致时，当前状态算。

#### 翻过的分段下一轮还在

模型翻过一个分段、或者一个问题挑中了一个分段之后，这个分段会被记下来，下一轮它仍然留在常驻段里，不必再问一次。**最新的分段永远不会被挤出去**：记录只是往常驻段里加，渲染时从最新的一段往回走，放不下的先让位给新的，最近用过的那几段占剩下的名额。

#### 新的配置项：`summarySegmentMode`

**默认 `on`。** 三种模式：

| 模式 | 行为 |
|---|---|
| `on`（默认） | 写入并读回：白板和分段都进窗口，不再带旧的滚动摘要 |
| `shadow` | 只写不读：写入照常发生，模型看到的还是旧的滚动摘要，供上线前观察写入侧 |
| `off` | 不写，回到旧的滚动摘要 |

**升级时要注意的一件事**：这个键**不写就是 `on`**。配置文件里没有这一行，等于选了分层摘要，而不是选了旧的滚动摘要。想留在旧行为上，就显式写 `off`；已经写进去的白板和分段不删，改回 `on` 之后新的分段会接着写。

#### 摘要装不下时改成裁剪

一份摘要超出预算（每组条目数、待办条数、引用编号数，或者 token 上限）以前会被**整份拒绝**，于是这个会话的摘要从此不再更新，只能退回旧摘要加原始消息。现在超限走**裁剪**：最旧的条目先让位，整份摘要不会因为多了一条就被丢掉。被裁掉的部分会记一笔（`summary_truncated`），评测报告里单独计数。

#### 引用会被检查

摘要里的每一条都要引用**这一轮确实给过模型的用户消息**。模型引用了别的消息，或者凭空造了一个编号，这份摘要会被判为不合格而不是照单全收；从上一份摘要原样保留下来的条目按原样保留，不做这项检查。

#### 验证到什么程度

在长会话评测里，分层摘要这一侧明显更好：**合计 130/132 对 101/131**（答对的次数 / 有效测量次数）。

分母是**有效测量次数**：只排除了**确认的环境失败**（上游超时这类不该记在产品账上的失败）。**答错与未提供可用答案都留在分母里**，它们是产品的结果，不是要排除掉的。

评测跑的是分层摘要这一侧的完整路径：写入、读回、预算裁剪、引用检查。

**没有经过评测验证的一项**：**「翻过的分段下一轮还在」**（最近用过的分段回到常驻段）。评测的每个场景只问一个问题，问完就结束，没有"下一轮"可观察，所以这条路径只有装配层的测试覆盖，没有评测数据。

### Redis 停止时的落盘与默认开启 AOF

这一项没有面向学习者的新功能，改的是**部署**：检索索引在 Redis 里，Redis 停止或崩溃时的落盘方式决定了「重启之后索引还在不在、还全不全」。已有部署需要更新一次 Redis 配置并重启（步骤见上面第 3、4、5 步），还要改**两处模型配置项**——旧写法会被指名拒绝，服务不会带着没人用的配置启动。

#### 改了什么

- **停止容器不再丢数据。** 镜像自带的入口脚本把 `redis-server` 放在 shell 下面跑：停止信号先到 shell，shell 死掉、服务还活着，容器在宽限期结束后被强杀，服务来不及写最后那张快照。结果是**正常停止就会丢掉上次快照之后的全部写入**——重启之后索引是旧的，检索悄悄变差，而且没有任何报错。现在服务自己就是 1 号进程，收到停止信号后写完快照再退出。
- **默认开启 AOF。** 每次写入都追加到文件里，最多丢一秒的写入；RDB 快照照旧保留。
- **老配置会被提醒。** 没开 AOF 的配置启动时会在日志里看到一行提示（只有分类，不含配置内容），但**不会拒绝启动**。

### 模型配置改成一处一个（升级前要先改这两处）

嵌入服务的密钥过去写在 `[isolatedEntry].embeddingKey`，而它的模型名与地址写在 `[ragModelConfig]` 里——一件事被拆成两段。`[ragModelConfig].baseUrl` 则会被程序读进来，但**没有任何客户端用它拨号**：学习 Agent 的地址一直取 `[isolatedEntry].chatBaseURL`。这一版把它们收拢成一处一个：密钥与它认证的模型名、地址放在同一段，那条没人用的地址删掉。

**旧写法会被指名拒绝，不是被忽略。** 这是刻意的：配置文件里留着一行不再读取的地址或密钥，看起来还在、实际已经不算数，配置与程序各说各话——这类差别最后表现为一次莫名的失败或一笔莫名的费用，而不是一个错误。所以带这两行的配置**起不来**，报错会写清改哪里。

改法见上面升级步骤第 1 步（键名与 `config.integrated.example.toml` 一致）。

没改就启动会看到下面两句之一（**原文照抄**；实际输出在行首还带日志时间戳，`exit status 1` 是 Go 的 `go run` 补的一行）：

```text
load configuration: retired configuration setting: ragModelConfig.baseUrl was removed: no client is built from it. The learning Agent's chat address comes from isolatedEntry.chatBaseURL. Delete this line.

load configuration: retired configuration setting: isolatedEntry.embeddingKey moved to ragModelConfig.embeddingApiKey, beside the embedding model name and address it belongs to. Move the key there and delete this line.
```

这两句是 `go -C v2 run ./cmd/schemainit -config <你的配置文件>` 打出来的——README 的最短路径里它就排在改配置之后、起服务之前，所以按正常顺序升级的话，你最先在这一步看到原因。直接跑正式入口 `hybrid/cmd/gopherlearn-integrated` 时只会看到一句笼统的 `integrated entry refused or failed; check the configuration requirements and the schema, Redis instance and storage directory it points at`，**具体原因要用 `schemainit` 那一步看**。入口把配置错误压成一句通用话不是这一版带来的，已单独记下来待修。

顺带一条：`OPENAI_BASE_URL` 这个环境变量不再被读来填上面那个地址（它以前写进的就是那个没人用的字段）。重排密钥的给法不变，仍然只从环境变量 `RAG_RERANK_AUTH_TOKEN` 读。

### 普通聊天不再有独立的模型配置（升级前要删掉这一段）

`[ordinaryChat]` 已经废弃。一轮普通对话与一轮学习 Agent 对话现在走同一条回路、用同一个 chat 模型，也就是 `[isolatedEntry]` 里那套地址、模型名与密钥；普通对话只是这一轮**没有开启知识库能力**而已。过去把同一个模型在两段里各填一遍的写法不再需要。

旧写法会被**指名拒绝**，不是被忽略——理由与上面 `ragModelConfig.baseUrl` 那条相同：文件里留着一行程序不再读取的模型地址或密钥，看起来还在生效，实际不算数，最后会表现成一笔说不清的费用或一次说不清的失败，而不是一个错误。所以带这一段的服务**起不来**，报错会写清改哪里（**原文照抄**；实际输出行首还带日志时间戳）：

```text
retired configuration setting: [ordinaryChat] was removed. A conversation turn is now generated by the learning Agent's chat model, the one this deployment already configures under [isolatedEntry] for the address, model and key it calls, so no second client is built from [ordinaryChat] and its context and summary settings are not read either. Delete the [ordinaryChat] section.
```

改法就是删掉整段，见上面升级步骤第 2 步。

### 流式回答的帧换了字段名（对客户端是破坏性变更）

普通对话的流式回答与学习 Agent 用的是同一套事件名（`message.started` / `message.delta` / `message.completed` / `message.failed`），但帧里装的东西统一到了一起：

| 帧 | 改之前 | 现在 |
|---|---|---|
| `started` | `data: {}` | `data: {"run_id":"…"}` |
| `delta` | `data: {"delta":"新增的一段"}` | `data: {"run_id":"…","content":"截至此刻的整段回答","publication":"model_stream"｜"validated_snapshot"}` |
| `completed` | `data: {"content":"…","message_id":"2"}` | 额外带 `run_id`、`publication`，以及本轮结果 `original_v2`（含上下文用量）；引用非空时带 `sources` |
| `failed` | 只有 `code` / `message` | 额外带 `run_id`、`native_code`、`message`；本轮有终止原因时还带 `termination_reason` |

**最要改的是 `delta` 变成 `content`**：`content` 不是新到的一段，而是截至此刻的**整段回答**，所以客户端要用它**替换**已经显示的内容，不能再往后**追加**——追加会把回答显示两遍。同一帧重复送达、或 `completed` 再把同一份回答重述一遍，替换写法都只显示一次。

`delta` 与 `completed` 上的 `publication` 说明这一帧的文字是哪一种，两个值都会保留：`model_stream` 是模型正在写、还没写完的当前文本；`validated_snapshot` 是本轮已经核对完（引用与依据都查过）的答案。`message.completed` 上还有第三个值 `replay`，表示这一轮没有重新生成，发出的是同一个请求标识此前已存下的答案。客户端不需要按这三个值走不同分支——它们都写进同一个 `content` 字段，替换即可——但想区分"正在长"与"已经定了"，读这个字段就够了。

正文什么时候开始出，按这一轮有没有开启知识库分两种：**关**的一轮没有检索、没有证据，也就没有需要核对的引用，所以正文**边生成边发**，第一段在模型写完之前就已经送到；**开**的一轮要先核对回答里的引用才允许发出，所以正文首帧会等到本轮生成结束，`message.started` 仍然在开始时就发出并刷新，界面照常能显示"正在生成"。两种情况下每一帧都是快照，所以中途看到的文字最后被 `message.completed` 替换一次是正常行为，不是重复输出。直接对接 `/stream` 的第三方客户端需要按上面的字段改名调整；仓库里的界面客户端已同步改好。

### 知识库开关改成每一轮的选择，Agent 会话置顶

输入框上方的「使用知识库」现在是一个**按轮生效**的开关：打开的一轮可以检索你上传的资料，关闭的一轮只按对话本身的上下文和通用知识回答。开关记在本机，默认打开；关掉以后下一轮仍然保持关闭，直到你再次打开。一次回答失败后重试，用的仍是这一轮当初的选择，不会因为中途改了开关而变成另一个问题。

这个开关只出现在服务端允许检索资料的会话里（当前是学习 Agent 会话）。普通对话会话不再显示知识库入口：旧的选择知识库控件指向的接口在这套入口里并不存在，一直点不动，这一版把它删掉了，而不是留一个永远灰着的按钮。

会话列表里，学习 Agent 会话排在最上面，同一组内按最近更新排序；排序只影响显示，不改任何会话的数据，也不影响服务端返回的顺序。

### 旧形状的普通对话摘要：不再更新，指标里记 `previous_unreadable`

普通对话以前把摘要写成两种旧形状：一种是模型返回的**纯散文**，另一种是**结构化文档**，但里面的引用是字符串而不是消息号。这一版的摘要器只认自己那一份结构化 JSON，所以旧形状的行**读不出来**。

读不出来时不再花冤枉钱：到点要压缩时先读库里那一行，读不出来就**跳过这一轮**——一次模型调用都不发，也**不进失败冷却**（旧内容不会在两次对话之间自己变可读）。指标里记 `summary_outcome{reason="previous_unreadable"}`，回答照常带着水位线之后的原始消息，学习者只是拿不到"压缩过的更早历史"。这类会话的摘要会一直停在旧那一行，直到有人转换它。

转换由上面升级步骤第 8 步那个 `summaryconvert` 服务做（后端镜像里的命令，与后端同一个网络，
所以能连到数据库），**默认只预演**，不自动跑，也不在启动时跑：

| 命令 | 做什么 |
|---|---|
| `docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm summaryconvert -config /config/integrated.toml` | 逐行分类并报告数量：现行 / 根 JSON / 纯散文 / 读不出来 / 空行。不写一行库 |
| 同一条命令加上 `-apply` | 把两种旧形状**机械地、无损地**写成现行 JSON，水位线**不动**，版本号 +1 |

转换**不调用模型**，也不需要任何 Provider 配置。纯散文按段落放进现行的字符串分组（`completed_items`），每段不超过单条上限，超长就在段落边界或上限处切开，切开的几段拼回去仍是原文。根 JSON 的各个分组、文本与引用原样搬过来；它多出来的 `legacy_text`（这张表没有对应字段）也一并放进 `completed_items`，不是只报一个丢弃字数。写回前用产品自己的读取器解码转换结果，通不过就不写；写进去的就是这份已验证文档的规范 JSON。

转不了的一律**列出来、不改那一行**：读不出来的 JSON、引用不是消息号因而无法无损搬运的行、超出当前文档预算的行（条目太多、目标太长）。宁可这一行停在原地，也不写一份少了内容的摘要。

"当前文档预算"就是这份配置里的那三个摘要上限，判定方式与运行时摘要器完全一致：部署调小了就拒掉装不下的行，调大了不会误拒装得下的行。

### 已废弃的普通对话链路整段删掉

上一版起，普通对话已经不再有自己的一套客户端：写进配置文件的 `[ordinaryChat]` 整段会被指名拒绝，一轮普通对话与一轮学习 Agent 对话走同一条回路、用同一个模型。这一版把那套**已经没有人走的代码本身**删掉了，而不是让它留在树里等人误读：

- 根模块里那层普通对话门面（`conversation/`）、它自己的模型客户端配置、以及普通对话专用的一套摘要链（自己的摘要计划、自己的摘要器、单独的"冻结"开关）一并删除。整合入口不再构造普通对话服务，也不再保留它自己的存储适配层。
- 仍然被整合入口使用的那几个 HTTP 帮手（读请求体、写 `{"data":…,"error":…}` 信封、把服务端错误翻译成状态码）搬到了根模块的 `httpwire/`：读请求体的那套检查——超过长度上限、不是合法 UTF-8、出现接收方不认识的字段、后面还跟着第二份文档——仍然只有一份实现，整合入口用的就是它。
- 根模块自己的**历史基线入口**（`cmd/gopherlearn`）不受影响：它的那套会话服务、它自己的摘要与环境变量配置都还在，`docs/LEGACY-DEVELOPMENT-REFERENCE.md` 描述的就是它。

行为上没有变化：整合入口里普通对话会话的行为，与删掉之前逐字一致——同一套模型、同一套存储、同一套 HTTP 契约。

### 消息表的用户名列放宽到与用户名同宽

`messages.user_name` 过去声明成 `varchar(20)`，而它存的值是会话所有者的用户名，用户名自己那一列是 `varchar(50)`。名字在 21 到 50 个字符之间时，这一行要么写不进去（MySQL 严格模式下直接报错），要么被截断成一个对不上任何人的名字。这一版把两列对齐到同一个宽度。

已有的表会在升级步骤第 7 步的 `schemainit` 里就地改宽，**不需要手工写 DDL**；已有的行不受影响。

### 聊天与摘要的请求现在带着输出长度上限

一轮回答和一次摘要以前都不带 `max_tokens`：回复能有多长，由模型服务端自己的默认值决定，而那个默认值不是这套部署能写进预算里的数字——预算表上写着上限，请求里却没有，于是那个数字只能算一个打算，不是一个约束。

窗口本来就为回复留了位置（`contextOutputReserveTokens`，默认 4000），这一版把它作为输出上限随每一轮生成一起发出去；摘要用自己那一份上限（`contextMaxSummaryTokens`）。

两处取的是同一个配置值，量的却不是同一件事：请求里的上限由服务端按它自己的方式算输出长度，事后检查按本地的估算量整理之后的文档长度（也就是序列化后的那份 JSON）。所以事后检查仍然可能判定一份文档装不下——这时这次摘要会被**裁剪**到装得下为止，而不是被拒绝（见[分层摘要 · 摘要装不下时改成裁剪](#摘要装不下时改成裁剪)）。

### 代码注释只说明行为

代码注释里有一些对读代码的人不说明任何事的标记。这一版把它们去掉，每条注释只留下这段代码在做什么。已写好的行为没有变化。

### 数据库结构要升一次（用 `schemainit`，不用手写 DDL）

白板、分段、分段任务三张新表，加上 `messages.user_name` 的列宽，都由**升级步骤第 7 步的
`schemainit`** 建好；除这两处之外，数据表结构没有变化。**这一步是独立的一步**，因为正式整合
入口拒绝在启动时改 schema——服务启动时不会替你建表，也不会替你改列宽。旧形状的摘要**内容**要靠上面那条 `summaryconvert` 命令逐行转换，这不是一次迁移：命令不自动跑，跑之前那些会话照常工作，只是摘要停在原地不再推进。

## v0.2.1 · 界面说法中文化与地址简化

这一版没有面向学习者的新功能，只把界面上的说法改成人话、把地址简化，并修掉几处质量问题。

### 新增

- **回答依据卡片**：每轮回答下方多一张「回答依据」卡片，如实说明这一答依据的是你上传的资料、
  之前的对话，还是通用知识；资料里有没有找到出处；以及为什么是这个结论。
  具体用了哪些资料，由回答下方的「引用来源」逐条列出。
  开发构建（`npm --prefix web run dev`）下这张卡片还带一块「调试信息」，里面有原始状态码、
  原始标识符与接口返回的原始内容，方便排错；发布构建里没有这一块。
- **离线题集校验与出题准备工具（面向开发者）**：`eval/` 目录新增 `qcheck`、`qprep` 两个命令，
  用于在本地校验题集与准备题目。**只在本地使用**，不影响运行中的服务，也不需要额外部署任何东西。

### 改进

- **页面地址去掉版本号后缀**，旧地址会自动跳转：

  | 旧地址 | 新地址 |
  |---|---|
  | `/register/v2` | `/register` |
  | `/knowledge/v2-documents` | `/knowledge/documents` |
  | `/memory/v2` | `/memory` |
  | `/learning/v2` | `/learning` |

  书签、站内链接与历史记录里的链接都不会失效；建议把收藏的地址更新成新地址。
- **执行过程更好读**：步骤标题去掉多余的「工具调用 ·」前缀；
  查找历史对话的工具显示为「查找过往对话」，与「查找记忆」区分开——前者搜聊天记录，后者搜长期记住的东西。
- **页面上的说法都换成中文**，覆盖对话、记忆、文档、学习四个页面：
  - 状态标签：文档的「就绪／失败／等待处理」，记忆的「生效中／已归档」，测验的「等你作答／已完成／已过期」；
  - 记忆草稿卡从一张术语表变成一张问句：标题「记忆草稿」、范围「主题／全局」、有效期「有期限／长期有效」，
    三个按钮是「确认新增／确认替换／拒绝这条草稿」；处理完只说一句「这条记忆已保存，之后回答时会想起它」；
  - 错误提示不再直接甩代号：以前显示的是 `agent_run_busy` 这类代号，现在是一句人话
    「上一轮回答还没结束，等它结束再发。」；服务端没给出可读说法时，就只显示服务端自己的话，不替它编；
  - 引用来源只显示「版本 N」，不再出现内部编号，也不再出现「尚未接通」这类说明；
  - 注册页的密码提示不再夹带版本号字样。

### 修复

- 修掉了发布前自动检查偶发失败的问题，对使用者没有影响：检查本身没错，
  但检查里写的字符串可能恰好被页面自己生成的随机编号凑出来，于是成败由随机数决定。

### 从 v0.2.0 升级

- **无需数据库迁移**：v0.2.0 到 v0.2.1 之间数据表结构没有变化。
- **配置文件没有新增的必填项**：沿用现有配置即可。
- **前端依赖没有变化**：`web/package.json` 与 `web/package-lock.json` 未改动，
  不需要重新执行 `npm --prefix web ci`；**重新构建一次前端**即可拿到新界面。
- 如果你的书签指向上面列出的旧地址，它们会自动跳转，但建议改为新地址。

## v0.2.0 · 界面改版与执行过程回看

### 新增

- **全新界面**：统一的设计规范；对话、文档、文档原文、记忆、学习测验、登录注册、404 页面全部重做；
  导航栏带文字标签；手机宽度下可正常使用。
- **Agent 执行过程可回看**：刷新页面或打开历史对话后，每个回答下面都能展开这一轮的执行步骤
  （工具、耗时、状态），与实时显示一致。工具名显示为中文，例如"检索知识库""阅读文档原文""查找记忆"；
  不认识的工具名原样显示。
- **检索测评程序（面向开发者）**：仓库新增 `eval/` 模块，可在同一批问题上比较纯向量、纯关键词、
  混合、混合加重排等检索方式和不同的取段数，并报告覆盖率、命中率、MRR 等指标；测评数据不包含在仓库中。
  用法见 [eval/README.md](../eval/README.md)。
- **带选项的检索入口（面向开发者）**：供测评使用，默认检索行为不变。

### 修复

- 普通对话中"知识库开关"始终不可用、点击后跳到无关页面的问题：现在只有在确实有可引用的知识库时才显示该开关。

### 变更

- 历史消息接口在 Agent 会话的消息中新增 `run_id` 字段；普通对话的消息不含此字段。
  另新增两个只读接口，用来读回一个会话的执行记录与某一轮的步骤。
- 删除了旧版页面：`/knowledge`、`/memory`、`/learning`、`/register` 现在显示 404 页。
  （保留的地址是 `/knowledge/v2-documents`、`/memory/v2`、`/learning/v2` 和 `/register/v2`。）
- 前端新增依赖 naive-ui（MIT 许可），已列入 [THIRD-PARTY.md](THIRD-PARTY.md)。
- 仓库文档整理。

### 从 v0.1.1 升级

- **无需数据库迁移**：v0.1.1 到 v0.2.0 之间数据表结构没有变化，新增的读取接口只读取已有的表。
- **配置文件没有新增的必填项**：沿用现有配置即可，配置示例里只改了注释措辞。
- 前端依赖有变化，升级后请重新执行 `npm --prefix web ci` 再构建或启动前端。
- 如果你的书签或外部链接指向上面列出的旧地址，请改为新地址。

## 这是什么

GopherLearn 是一个个人 AI 学习助手，参考并基于 [GopherAI](https://github.com/youngyangyang04/GopherAI)
项目开发，在此基础上自主扩展了学习 Agent 等功能。它把「自己的资料」变成可以追问的学习材料：
上传文档后可以就文档提问并看到出处，出题作答后判分并累计主题掌握度。

## 这次公开的形态

- **开发预览版**：核心链路已跑通，前端样式与交互会继续迭代。
- **正式入口**：`hybrid/cmd/gopherlearn-integrated`（Go 后端 + Vue 3 前端 + MySQL + Redis Stack）。
- **文档处理在进程内完成**：解析、切片、嵌入、索引都在后端进程里。

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

**完整步骤在仓库根目录的 [README.md](../README.md)**：快速开始（六步，每步链接到详细小节）→ 环境要求 → 配置说明 → 本地启动（七步，
每步标明运行目录）→ Docker 部署 → 注册与使用 → 常见问题。配置的完整字段表在 [CONFIGURATION.md](CONFIGURATION.md)。

最短路径（仓库根目录）：

```bash
cp config.integrated.example.toml config.integrated.toml.local   # 填占位符
go -C v2 run ./cmd/schemainit -config ../config.integrated.toml.local
go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.toml.local
# 另开一个终端
npm --prefix web ci
VITE_DEV_API_ORIGIN=http://127.0.0.1:18082 npm --prefix web run dev
```

## 行为变化与错误码

这一版修掉了几处对外可见的行为差异。表里写的是**修完之后**的契约：

| 场景 | 修之前 | 现在 |
|---|---|---|
| 新用户注册，验证码请求 | `4001 服务繁忙`（笼统） | 邮件未配置：`2012 邮件服务未配置，无法发送验证码：请在配置文件的 [emailConfig] 段填写发件地址与授权码`；已配置但投递失败：`2013 验证码邮件发送失败，请稍后重试或检查邮件服务配置` |
| 请求体绑定失败（记忆创建/纠正/归档/删除/草稿决议等） | `500` + `4001 服务繁忙` | `400` + `2001 请求参数错误`；真正的服务端错误仍然是 `500` |
| 上传的不是 multipart 请求 | `500` + `4001` | `400` + `2001`；超出大小限制仍然是 `413` + `7002` |
| 兼容接口未认证时响应体 | `401`，**空 body** | `401` + `{"data":null,"error":{"code":"unauthorized","message":"authentication is required"}}` |
| 上传缺少 `file` 字段 | `400` + `7001 仅支持有效的 .md 或 .txt 文件` | `400` + `7011 缺少文件字段 file`；真正的 `.pdf` 仍然报 `7001` |

所有权核验、跨用户隔离、JWT 校验与来源权限边界**没有被改动**，相关测试保持通过。

## 已知限制

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
  不写进配置文件（见 README §5.2 与 §5.3，以及 [配置参考 · 重排](CONFIGURATION.md#重排可选)）。

## 许可与来源

- 本项目基于 [GopherAI](https://github.com/youngyangyang04/GopherAI) 开发，并沿用其
  **GNU GPL v3.0** 许可。许可证全文见 [LICENSE](../LICENSE)。
- 上游归属、相关衍生项目与本次发布范围见 [NOTICE.md](../NOTICE.md)。
- 第三方依赖及其各自声明的许可证见 [THIRD-PARTY.md](THIRD-PARTY.md)。

## 更详细的材料

| 文件 | 内容 |
|---|---|
| [CONFIGURATION.md](CONFIGURATION.md) | 配置参考：完整字段表、默认值、思考开关与方言、重排详细步骤、启动失败提示表 |
| [INTEGRATED-REFERENCE.md](INTEGRATED-REFERENCE.md) | 正式入口技术参考：学习测验接口合同、三个模块的验证命令 |
| [INTEGRATED-DEPLOYMENT.md](INTEGRATED-DEPLOYMENT.md) | 整合栈部署参考：启动命令、配置模板与忽略规则、每一类主张由谁保证、搜索索引接缝 |
| [CREDENTIAL-HANDLING.md](CREDENTIAL-HANDLING.md) | 凭据处理：口令放在哪、不放在哪、哪些做法不要用 |
| [LEGACY-DEVELOPMENT-REFERENCE.md](LEGACY-DEVELOPMENT-REFERENCE.md) | 历史基线入口的开发说明（**仅供查阅历史实现**，不是快速开始） |
| [REAL-MODEL-VALIDATION-PLAN.md](REAL-MODEL-VALIDATION-PLAN.md) | 真实模型验证的额度口径与执行方案 |
| `spec/API.md`、`spec/ARCHITECTURE.md` | 接口合同与架构说明 |
