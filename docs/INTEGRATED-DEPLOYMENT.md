# 整合栈部署参考

这份文件是**正式入口**（`hybrid/cmd/gopherlearn-integrated`）那套整合栈的部署参考：
启动命令、操作员要准备的文件、每一类主张由谁保证（代码拒绝 / 模板默认 / 操作员约束）、
以及搜索索引的接缝细节。

快速开始仍然看 [README](../README.md) 的「本地启动」与「Docker 部署」两节；这里写的是
那些章节放不下的细节。

---

## 1. 启动命令

### 宿主直跑

```bash
# 在仓库根目录
cp config.integrated.example.toml config.integrated.toml.local
# 按 README §5.1 填好占位符
cd hybrid
go build -o ../bin/gopherlearn-integrated.exe ./cmd/gopherlearn-integrated
cd ..
./bin/gopherlearn-integrated.exe -config config.integrated.toml.local
```

入口**只接受显式 `-config`**。缺文件、缺强制字段、`isolated` 非真、`migrate = true`、
`[ordinaryChat]` 出现白名单外的键、普通客户端类型不是 `openai_compatible`、JWT 密钥短于
32 字节——这些都在**任何资源被创建之前**被拒绝。

### 容器直跑

```bash
docker compose -p gopherlearn-integrated -f compose.integrated.yml up --build
```

索引已经是这个栈里的一个服务（`redis`），所以只有**一条命令、一个启动故事**；不需要
先起第二份 compose 文件再让 backend 以 `external` 的方式加入它的网络。

建库是**独立的一步**（入口拒绝在启动时改 schema）：

```bash
docker compose -p gopherlearn-integrated -f compose.integrated.yml --profile init run --rm schemainit
```

---

## 2. 操作员要准备的三份文件

三份都在**仓库根目录**，后缀都是 `.local`（同时命中 `.gitignore` 与 `.dockerignore` 的
`*.local` 规则，不会被提交，也不会进构建上下文）：

| 文件 | 用途 | 谁读它 |
|---|---|---|
| `config.integrated.toml.local` | 后端的唯一配置：模型 key、数据库口令、JWT key、schema 名、Redis 地址、存储目录 | 容器内挂到 `/config/integrated.toml`，入口读它 |
| `redis.integrated.conf.local` | Redis 服务端自己的配置，**访问控制只写在这里** | 容器内只读挂到 `/redis-stack.conf`，是镜像入口唯一会读的那条路径 |
| `.env.compose.integrated.local` | MySQL 容器自身的账号初始化 | compose 的 `env_file` |

可提交的样例分别是 `config.integrated.example.toml`、`redis.integrated.conf.example`、
`.env.compose.integrated.example`；样例里的占位串会被 preflight **具名拒绝**，不会被
当成真口令用掉。

判定「哪些文件被排除」用的是**匹配器**而不是散文：`hybrid/integrated_ignore_matching_test.go`
实现了一份 ignore 规则匹配（`**` / `*` / `?` / `!`、尾斜杠目录规则、最后匹配生效），
先拿一组形似对照自证判据能红，再对两份真实文件下结论。结论是：

```text
任一读法下都排除：config.integrated.toml.local、redis.integrated.conf.local、
                  .env.compose.integrated.local、deploy/redis.integrated.conf.local、sub/dir/nested.local
任一读法下都不排除：config.integrated.example.toml、redis.integrated.conf.example、
                  形如 <stem>.local.toml 的旧名（根层与子目录各一例——它末段是 .toml，没有规则命中它）
```

Docker 侧读数是**静态**的：`Dockerfile.backend-integrated` 的 COPY 行只覆盖 `cmd`、
`internal`、`conversation`、`v2`、`hybrid` 与模块文件，运行阶段只拷二进制与 CA 包，
因此配置文件既不在构建上下文里、也不在镜像层里。**这条结论是按匹配器对文件内容判定的，
不是 `docker build` 实测。**

---

## 3. 措辞分级：什么被代码拒绝，什么只是模板或约定

这张表是这份文档的核心：**同一件事的保证强度是不一样的**，混为一谈会让人以为有代码
兜底的地方其实没有。

| 主张 | 类别 | 判据 |
|---|---|---|
| 缺 `-config` / 非法 TOML / 缺强制字段 → 拒绝启动 | **代码确实拒绝** | `hybrid/entry.go`、`v2/internal/isolated/settings.go`；`TestIntegratedConfigTemplateLoadsAndRefusesCrossings` 的 dropped-field 负例 |
| `isolatedEntry.migrate = true` → 拒绝启动 | **代码确实拒绝** | `hybrid/entry.go`；同上测试的 migrate 负例 |
| `[ordinaryChat]` 出现白名单外键 → 拒绝 | **代码确实拒绝** | `hybrid/entry.go`、`conversation/client.go` |
| 普通客户端退回 `echo` → 拒绝 | **代码确实拒绝** | `conversation/client.go` |
| JWT key < 32 字节或等于被撤换的默认值 → 拒绝 | **代码确实拒绝** | `v2/config/config.go` |
| schema 名 / Redis host+port+db / storageDir **必须显式写出来** | **代码确实拒绝**（只拒绝"没写"） | `v2/internal/isolated/settings.go` |
| 模板把 `gopherlearn_v2_integration` / `gopherlearn-integrated-redis` / `data/gopherlearn-integrated/knowledge` / `db = 0` 作为取值 | **模板默认值** | `config.integrated.example.toml`；**入口不会识别"这属于别的栈"**，因此"不共用旧库"不是代码保证 |
| 使用本栈自己的 schema、独立 Redis 实例、独占源目录、独立 compose 项目名与容器/卷/网络名 | **操作员必须保证的隔离约束** | `TestIntegratedStackDeclaresTheApprovedArtifacts`、`TestIntegratedIndexRunsAsARegisteredService`、`TestRedisIsDeclaredOnlyInRegisteredComposeFiles` |
| 配置缺失 / 空 / `requirepass` 不恰好一条 / 值是占位串或含引号 / 短于 32 字节 → **不交给镜像入口**，容器不会把服务起起来 | **脚本确实拒绝** | `deploy/redis-stack-preflight.sh`；`TestPreflightRefusesBeforeTheServerStarts` |
| 认证被拒 / 连不上 / 模块缺失 / `(error)` / 不是 JSON 数组 / 元素不全是字符串 → 健康检查非零退出 | **脚本确实拒绝** | `deploy/redis-stack-healthcheck.sh`；`TestHealthcheckScriptAcceptsAndRefuses` |
| 口令不出现在 argv、不出现在脚本输出，只出现在那一个探针子进程的环境里 | **已按字节量出（离线）**；**真实容器内该环境的可见范围未实测** | `TestHealthcheckLeakBoundaryIsMeasured` |
| 挂载路径就是入口读的那条（`/redis-stack.conf`），且 preflight 是容器的 entrypoint | **结构判据**：读 compose 的实际服务结构 + 镜像入口证据 | `indexServiceJudged` 及其变异用例 |
| 索引镜像能起、数据卷权限可用、健康检查通过、`up` 后服务可登录并跑一轮 Agent | **尚未真实验证** | compose 与两份脚本头部的部署名列出这几条 |

两条边界同样要分开：**配置装载通过 ≠ 索引可用**；**路由表覆盖前端默认导航 ≠ 真实数据库
读写正确**。

---

## 4. 搜索索引（Redis Stack）

- **独立实例**。配置里 `db = 0`；"非 0 库号才算隔离"的说法与断言已删除——在共用实例上
  挑一个非 0 库不是隔离，入口也发现不了。`redisConfig.host` 必须等于本栈索引的
  `container_name`、`db` 必须为 0，这两条由 `indexWiringJudged` 判，并配"指到别的实例"
  和"改成 db 2"两条反例。
- **索引名与键前缀保持生产值**：`gopherai_knowledge_chunks_v3_idx`、
  `gopherai_episode_v2_idx`、`gopherai_memory_v2_idx` 与 `gopherai:*` 前缀。判据是**从
  使用它们的源码读回比对**（`v2/common/redis/key.go` 等常量），不是"部署文件里提到过
  这三个名字"。
- **接缝按镜像入口证据接**：镜像的 `/entrypoint.sh` 只在 `/redis-stack.conf` 存在时才设
  `CONFFILE`，否则会用**没有任何访问控制**的配置把服务起起来。因此：
  - 服务端配置挂到 `/redis-stack.conf`（只读）；
  - 容器入口换成 `deploy/redis-stack-preflight.sh`：检查通过前**不 exec** 镜像原入口，
    所以不存在"配置没到位但服务起来了"的中间态——检查不过就是容器起不来；
  - 健康门是 `deploy/redis-stack-healthcheck.sh`，**不是 `redis-cli PING`**：`PING` 只证明
    端口在听，而 `FT._LIST` 的应答形态才证明搜索模块活着；
  - 口令不进 argv、不进 `REDIS_ARGS`（argv 会出现在 `ps` 与 `docker inspect` 里，
    而 `REDIS_ARGS` 正是镜像入口原样追加到服务命令行的那个通道）；
  - backend `depends_on: redis: condition: service_healthy`，两个服务同在本栈自有的
    内部网络上，索引**不发布宿主端口**。
- **口令的可见范围**：探针在运行时从配置文件现取值，经 `REDISCLI_AUTH` 交给探针子进程
  （`redis-cli` 官方记录的非 argv 通道）。代价是：探针存活期间，这个值在**该容器内同 uid
  的进程与 root** 的进程环境里可读。它对 Redis 客户端的同伴、对其它容器、对宿主进程表
  都不可见。这条边界的实测记录见 §3 的最后几行。

---

## 5. 镜像引用

| 用途 | 引用 |
|---|---|
| MySQL | `mysql@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b` |
| Redis Stack | `redis/redis-stack-server@sha256:39adcbb083b240642cd5738a75c4959c0393ef2a40fa545f2e1c7b55798eeda4` |

本仓的既有规则是**摘要钉死后 tag 是装饰且会被摘要覆盖**，所以这里只写摘要、不写 tag。
`TestRedisComposeRunsTheModuleBearingImageOnly` 对索引镜像就是这么要求的。

**初始化是独立的一步**：`compose.integrated.yml` 不建 schema、不建账号、不跑 `grant`、
不做迁移；入口继续拒绝 `migrate = true`。首次建库用
`docker compose ... --profile init run --rm schemainit`。
