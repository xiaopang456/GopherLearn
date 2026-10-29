# V2 正式入口整合（integration/v2-production-entry）

这棵树把"已迁入的 V2 能力"接成**一套默认应用**：Agent 走原 V2 Eino ReAct，普通聊天保留 GopherLearn 的 DirectChat，RAG／记忆／学习／Episode 用已迁入的 V2 业务服务，前端接口与实际后端一致。保留现有多模块结构，以 hybrid 装配为运行内核；**没有新写第二套 Agent、DAO、记忆或检索实现**，也没有改 Runner、工具、DAO、业务断言、依赖版本或锁文件。

上一轮（HEAD `c0db468f` 之后）按负责人交回的清单做五处修正：凭据文件名、Redis 隔离方式、MySQL 引用、安全保证措辞、以及在**当前候选字节**上重做的离线验证。

本轮（HEAD `db6c327f` 之后）只收口一件事：Redis 常驻部署的凭据接缝与健康门，全部离线。要点是按已封存的镜像入口证据改用入口**实际读取**的 `/redis-stack.conf`，加一份启动前拒绝的 preflight 与一份严格读响应结构的 healthcheck，并把索引从 `deploy/` 里的第二份启动文件收进主 `compose.integrated.yml`；既有范围规则按授权做了窄改（登记见 §6）。本轮零 Docker、零真实连接、零凭据生成、零数据迁移，业务代码／Eino Runner／原有断言／依赖与锁文件未动。

## 0. 固定输入与身份

| 项 | 读数 |
| --- | --- |
| 工作树 | `<工作区>/wt-v2-gopherlearn-integration`（本机绝对路径已从公开版中移除） |
| 分支 | `integration/v2-production-entry` |
| 本轮起点 HEAD | `db6c327f46931f4ab3765cd8d73ca43012f1c88a`（开工 `git status --porcelain` = 0 行；分支与上表同一支） |
| 上一轮起点 HEAD | `c0db468f68997d9ab3304e9ac9cfe8435b8b41dd`（开工 porcelain = 0 行） |
| 整合本体提交 | `217b422`（入口／构建／部署／配置／前端）、`c0db468`（race 读数补记） |
| 起点 | `e3ad48ef`（迁移线末端） |
| 约定 | 父级 `AGENTS.md`（`9f11168b…`）第 150 行的读取义务；本树 `AGENTS.md` 去 CR 后 `81297c1a…`（与目标树同字节）；最新版 `optimization/gopherai-agent/code-learning.md`（本轮读取时 `e245d804…`，为上一轮追加 AGENT-031 后的状态） |
| 其它树 | `GopherLearn-clean`（`2b5150e`，5 条未跟踪项同组）、`wt-v2-direct-copy`（`e3ad48e`，0 行）：本轮零写入 |
| 本地工具链 | `go1.26.1`，`GOTOOLCHAIN=local` 下三模块均可解析；node v24.14.1（仍低于项目声明 24.15） |

## 1. 实际启动链

| 入口 | 模块 | Agent 内核 | 路由表 | 定位 |
| --- | --- | --- | --- | --- |
| `hybrid/cmd/gopherlearn-integrated` | `gopherlearn-v2-hybrid` | 原 V2 Eino ReAct（`v2/service/agent/runner.go:592`） | hybrid 兼容面 5 类前缀 + 其余透传原 V2 Gin | **正式默认应用** |
| `cmd/gopherlearn` | 根模块 | 主线 harness（`internal/agent/harness.go:140`） | `internal/httpapi/router_gin.go:44-57` 14 条 | 历史／基线入口，字节未动 |
| `hybrid/cmd/hybrid-isolated` | `gopherlearn-v2-hybrid` | 同上 | 同上 | 隔离核验入口，字节未动 |

入口是薄转发：`runCLI` 只做 flag 校验、打印一条入口身份行、把 `hybrid.Run` 的错误压成非零退出且不回显配置值。

## 2. 本轮修正清单（5 项）

| # | 修正 | 落点 |
| --- | --- | --- |
| 1 | 凭据文件名统一为 `config.integrated.toml.local`；删除"`*.local` 会匹配 `.local.toml`"的错误说法；四处残留旧字面量清除 | `compose.integrated.yml`、`config.integrated.example.toml`、`README.md`、`tutorial/v2-production-entry/README.md`、新增 `TestNoDeliveryFileStillNamesTheUnignoredConfigFile` |
| 2 | Redis 隔离改为**独立 Redis Stack 实例 + db 0**，删除"非 0 库号才算隔离"的说明与断言；索引名与前缀保持生产值不改 | `config.integrated.example.toml`、新文件 `deploy/compose.integrated.redis.yml`、`compose.integrated.yml`（backend 加入该实例所在的 external 网络）、`hybrid/integrated_delivery_test.go` |
| 3 | MySQL 用已验收引用 `62fb722c…`，去掉未经批准的 `mysql:8.4`；env 文件默认名改为整合栈专有 | `compose.integrated.yml` |
| 4 | 措辞按"代码拒绝／模板默认／操作员约束／未验证"四分，撤销无判据支撑的保证 | 本文件 §5、`README.md`、模板头注释、`learning.md` 更正节 |
| 5 | 在当前候选字节上建 LF 装配重验 6 项；全量 race 保留"未完成" | §7、§8 |

另有一处顺带加强：`.dockerignore` 增加 `**/*.local`（`*.local` 只覆盖上下文根层）。该仓 `git ls-files "*.local"` 为 0 行，所以这条规则只可能排除凭据与本地状态，不会掩盖代码或证据。

## 2.1 本轮改动清单（Redis 常驻接缝，9 项）

| # | 改动 | 落点 |
| --- | --- | --- |
| 1 | 启动前拒绝的 preflight：只看入口实际读的 `/redis-stack.conf`，缺失／空／不可读／`requirepass` 不恰好一条／值不合约定（占位串、含引号或空白、短于 32 字节）都在交给镜像入口之前拒 | 新文件 `deploy/redis-stack-preflight.sh`；`compose.integrated.yml` 的 `entrypoint: ["/bin/sh", "/redis-stack-preflight.sh"]` |
| 2 | 健康检查重写：从配置文件现读口令，经 `REDISCLI_AUTH` 交给 `redis-cli -2 --json FT._LIST`，只有"JSON 数组且元素全为字符串"算健康（空数组算），认证被拒／连不上／模块缺失／`(error)`／不可解析一律非零 | 新文件 `deploy/redis-stack-healthcheck.sh`；`healthcheck.test` 指向它 |
| 3 | 索引收进主栈文件，删掉第二份启动文件与"先起它"的歧义；backend `depends_on: redis: condition: service_healthy`；网络由 `external` 借用改为本栈自有的 `internal: true` | `compose.integrated.yml`（重写）、`git rm deploy/compose.integrated.redis.yml` |
| 4 | 既有范围规则窄改：从"根下恰好一个文件声明 redis"改为"只允许两份**登记过**的声明"（`compose.redis.yml` 与 `compose.integrated.yml`），基线三份文件仍禁止声明／引用／`include` 索引，陌生第三份仍红，基线资源名复用仍红 | `internal/app/retrieval_stack_config_test.go`：`TestRedisAppearsInExactlyOneComposeFile` → `TestRedisIsDeclaredOnlyInRegisteredComposeFiles` |
| 5 | 结构判据 + **整脚本执行**：替身 `redis-cli` 记录 argv 与其自身环境，替身入口脚本记录"是否真的被执行"；口令泄漏边界是量出来的，不是看字符串；每条反例都断言退出码与具名原因 | 新文件 `hybrid/integrated_redis_seam_test.go`（6 个 Test） |
| 6 | 忽略匹配改成真匹配器（`**`、`*`、`?`、`!`、目录规则、最后匹配生效），根层／子目录／旧错误文件名／公开模板四类都跑；无分隔符模式的两种读法**都断言** | 新文件 `hybrid/integrated_ignore_matching_test.go` |
| 7 | 服务端配置的可提交样例：占位串本身会被 preflight 拒绝，所以能入库；端到端读它一次 | 新文件 `redis.integrated.conf.example` |
| 8 | 两份部署脚本钉 `eol=lf`：容器里的 `/bin/sh` 直接执行它们，一次 CRLF 检出会把 shebang 变成 `/bin/sh\r` | `.gitattributes` |
| 9 | 措辞与失效路径更正：`.dockerignore` 里"`*.local` 只覆盖根层"改为按两种读法说明；模板注释里指向已删除文件的引用改为 `compose.integrated.yml` 的 redis 服务，并补上"服务端口令必须与此处逐字相同" | `.dockerignore`、`config.integrated.example.toml`、本文件 §3/§4/§5/§6/§7/§9/§11 |

上一轮的 5 项修正（凭据文件名、Redis 隔离方式、MySQL 引用、措辞分级、LF 重验）落点见 §2 原表；本轮未撤销其中任何一条。

## 3. 启动命令

宿主直跑（cmd 语法，逐行独立）：

```cmd
cd /d <工作区>\wt-v2-gopherlearn-integration
copy config.integrated.example.toml config.integrated.toml.local
cd hybrid
go build -o ..\bin\gopherlearn-integrated.exe .\cmd\gopherlearn-integrated
cd ..
bin\gopherlearn-integrated.exe -config config.integrated.toml.local
```

入口只接受显式 `-config`，不读环境变量凭据；缺文件、缺强制字段、`isolated` 非真、`migrate=true`、`[ordinaryChat]` 未知键、普通客户端不是 `openai_compatible`、JWT 密钥短于 32 字节，都在任何资源创建之前拒绝。

容器直跑（需 Docker 授权，本轮未执行）。索引已是本栈的一个服务，**只有一条命令、一个启动故事**：

```cmd
docker compose -p gopherlearn-integrated -f compose.integrated.yml up --build
```

`deploy/compose.integrated.redis.yml` 已删除；上一轮那种"先起第二份文件、backend 再以 `external` 加入它建的网络"的写法不再成立，`compose.integrated.yml` 里只留这一条命令注释（有断言数它出现的次数）。

## 4. 配置模板与三类文件的忽略证据

需要操作人员在仓库根准备的文件（本轮只给模板与路径，不生成任何真实值）：

| 文件 | 用途 | 被谁排除 |
| --- | --- | --- |
| `config.integrated.toml.local` | 后端唯一配置（含模型 key、DB 口令、JWT key），挂到 `/config/integrated.toml` | `.gitignore:5 *.local`；`.dockerignore` 的 `*.local` 与 `**/*.local` |
| `redis.integrated.conf.local` | Redis 服务端配置，挂到入口实际读取的 `/redis-stack.conf`；访问控制只写在这里 | 同上；样例是可提交的 `redis.integrated.conf.example`，其占位串会被 preflight 具名拒绝 |
| `.env.compose.integrated.local` | MySQL 容器自身的账号初始化 | 同上；默认名与基线 `.env.compose.local` 不同 |

三份都在仓库根：`compose.integrated.yml` 的挂载源逐字写着 `./config.integrated.toml.local`、`./redis.integrated.conf.local`，`env_file` 默认 `.env.compose.integrated.local`（有断言读实际结构核这三条源路径，不靠注释）。

判定用的是**匹配器**而不是散文：`hybrid/integrated_ignore_matching_test.go` 实现了一份 ignore 规则匹配（`**`／`*`／`?`／`!`、尾斜杠目录规则、最后匹配生效），先拿 23 条形似对照自证判据能红，再对两份真实文件下结论。无分隔符模式有两种都被引用的读法——Docker 文档表格说只匹配上下文根层，moby 实际用的匹配器按 `.gitignore` 语义匹配任意深度——本轮**两种读法都断言**，所以交付不依赖哪一种：

```text
任一读法下都排除：config.integrated.toml.local、redis.integrated.conf.local、
                  .env.compose.integrated.local、deploy/redis.integrated.conf.local、sub/dir/nested.local
任一读法下都不排除：config.integrated.example.toml、redis.integrated.conf.example、
                  形如 <stem>.local.toml 的旧名（根层与子目录各一例——它末段是 .toml，没有规则命中它）
只有"根层读法"才需要 **/*.local：去掉该行后子目录那份在根层读法下不再被排除，在任意深度读法下仍被 *.local 覆盖
两条后缀行都去掉后，上述 .local 路径一律不再被排除（证明判定确实是这两行在做，不是别处的巧合）
```

git 侧另有上一轮的实测读数（`git check-ignore -v` 四条命中 `.gitignore:5`、旧名 exit 1、`git ls-files "*.local"` = 0 行），逐字记在 `verification.md`。

本轮按匹配器读数改了一处措辞：`.dockerignore` 里原写"`*.local` 只覆盖上下文根层，递归那条才管子目录"，这是把两种读法当成一种讲了；现在文件里写的是"两种读法都不依赖，所以两行都留着"。同族的还有说明文档里的路径示例（`deploy/redis.integrated.conf.local` 已改回仓库根，因为挂载源就是根路径）。

Docker 侧读数是静态的：`Dockerfile.backend-integrated` 的 COPY 行只覆盖 `cmd`、`internal`、`conversation`、`v2`、`hybrid` 与模块文件，运行阶段只拷二进制与 CA 包，因此配置文件既不在上下文也不在层里——`TestBuildConfigurationSelectsTheIntegratedEntry` 把"运行阶段不出现 config"钉成断言。**未执行 `docker build`，所以"上下文排除"是按匹配器对文件内容判定的，不是 Docker 实测。**

## 5. 措辞分级：什么被代码拒绝，什么只是模板或约定

| 主张 | 类别 | 判据 |
| --- | --- | --- |
| 缺 `-config`／非法 TOML／缺 14 个强制字段 → 拒绝启动 | 代码确实拒绝 | `hybrid/entry.go:20-49`、`v2/internal/isolated/settings.go:37-41`，测试 `TestIntegratedConfigTemplateLoadsAndRefusesCrossings` 的 dropped-field 负例 |
| `isolatedEntry.migrate = true` → 拒绝启动 | 代码确实拒绝 | `hybrid/entry.go:27-29`，同上测试的 migrate 负例 |
| `[ordinaryChat]` 出现白名单外键（如 Agent 预算）→ 拒绝 | 代码确实拒绝 | `hybrid/entry.go:36-44`、`conversation/client.go:13-19`，负例已补 |
| 普通客户端退回 `echo` → 拒绝 | 代码确实拒绝 | `conversation/client.go:37-39`，负例已补 |
| JWT key < 32 字节或等于被撤换默认值 → 拒绝 | 代码确实拒绝 | `v2/config/config.go:523-538` |
| schema 名／Redis host+port+db／storageDir **必须显式写出来** | 代码确实拒绝（只拒绝"没写"） | `settings.go:37-41,50-55` |
| 模板把 `gopherlearn_v2_integration`／`gopherlearn-integrated-redis`／`data/gopherlearn-integrated/knowledge`／`db = 0` 作为取值 | 模板默认值 | `config.integrated.example.toml`；**入口不会识别"这属于基线"**，因此"不共用旧库"不是代码保证，本轮不再那样写 |
| 使用整合栈自己的 schema、独立 Redis 实例、独占源目录、独立 compose 项目名与容器/卷/网络名 | 操作人员必须保证的隔离约束 | 模板与栈文件把取值摆成整合栈专有；`TestIntegratedStackDeclaresTheApprovedArtifacts`、`TestIntegratedIndexRunsAsARegisteredService`、`TestRedisIsDeclaredOnlyInRegisteredComposeFiles` 校验文件形态与声明范围 |
| 配置缺失／空／`requirepass` 不恰好一条／值是占位串或含引号、短于 32 字节 → **不交给镜像入口**，容器不会把服务起起来 | 脚本确实拒绝（本轮已把整脚本跑起来验证，替身入口脚本记录是否被执行） | `deploy/redis-stack-preflight.sh`，`TestPreflightRefusesBeforeTheServerStarts` 的 9 条反例 + 1 条放行 |
| 认证被拒／连不上／模块缺失／`(error)`／不是 JSON 数组／元素不全是字符串 → 健康检查非零退出 | 脚本确实拒绝（同上，用替身 `redis-cli` 喂 21 条响应形态） | `deploy/redis-stack-healthcheck.sh`，`TestHealthcheckScriptAcceptsAndRefuses` 的 4 条健康 + 21 条非健康 |
| 口令不出现在 argv、不出现在脚本输出，只出现在那一个探针子进程的环境里 | 已按字节量出（离线、替身）；**真实容器内该环境的可见范围未实测**（同 uid 与 root 可读，这点是陈述而非读数） | `TestHealthcheckLeakBoundaryIsMeasured` |
| 挂载路径就是入口读的那条（`/redis-stack.conf`），且 preflight 是容器的 entrypoint | 结构判据：读 `compose.integrated.yml` 的实际服务结构 + 镜像入口证据；`docker compose up` 的真实挂载未执行 | `indexServiceJudged` 及其 14 条变异 |
| 索引镜像能起、数据卷权限可用、健康检查通过、`up` 后服务可登录并跑一轮 Agent | 尚未真实验证 | 本轮零 Docker、零真实连接；`compose.integrated.yml` 与两份脚本头部署名列出这几条 |

另外两条边界也照此分开：`hybrid.Run` 装载配置通过 ≠ 索引可用；路由表覆盖前端默认导航 ≠ 真实数据库读写正确。

## 6. 搜索索引：独立实例、DB0、接缝收口与一处既有规则的窄改

- 隔离方式沿上轮更正：**独立 Redis Stack 实例**，配置里 `db = 0`（验收批 R1/R2、J1、Episode 实际用的就是那台实例的 db 0）。"非 0 库号才算隔离"的说明与断言已删除；在共用实例上挑一个非 0 库不是隔离，入口也发现不了。`redisConfig.host` 必须等于本栈索引的 `container_name`、`db` 必须为 0，这两条现在由 `indexWiringJudged` 判，并配"指到基线实例"和"改成 db 2"两条反例。
- 索引名与键前缀保持生产值：`gopherai_knowledge_chunks_v3_idx`、`gopherai_episode_v2_idx`、`gopherai_memory_v2_idx` 与 `gopherai:*` 前缀。本轮不再靠"部署文件里提到过这三个名字"来证明没改名，而是从使用它们的源码读回比对（`v2/common/redis/key.go`、`episode_index.go`、`memory_index.go` 三个常量）。冲突由"换一台服务器"消除，不由改名消除。
- 接缝按**已封存的镜像入口证据**接：`entry-manifest.json` 记该镜像 `Cmd: ["/entrypoint.sh"]`、环境里 `REDIS_ARGS` 为空；`entrypoint.sh.txt:8-10` 只在 `/redis-stack.conf` 存在时才设 `CONFFILE`，`:25-36` 恒加 `--protected-mode no --daemonize no` 与模块参数后接 `${REDIS_ARGS}`。所以：
  - 服务端配置挂到 `/redis-stack.conf`（只读；本轮记录里写的是 `create_host_directory: false`，**后续更正**：那个键名是**拼错的**——它根本不是 Compose 的键，Compose v5.1.3 会报 `additional properties 'create_host_directory' not allowed`；正确键名是 `create_host_path`，现已用正确键名把 flag 加回。另一层保证始终在 preflight：`[ -f "$CONF" ] || refuse "configuration missing"`，检查不过就不 exec 镜像原入口；见 `hybrid/integrated_redis_seam_test.go`）；
  - 容器入口换成 `deploy/redis-stack-preflight.sh`：检查通过前**不 exec** 镜像原入口，因此不存在"配置没到位但服务起来了"的中间态；检查不过就是容器起不来，而不是无口令起来；
  - 健康门是 `deploy/redis-stack-healthcheck.sh`，不是 `redis-cli PING`：`PING` 只证明端口在听，而 `FT._LIST` 的应答形态才证明搜索模块活着；
  - 口令不进 argv、不进本文件、不进 `REDIS_ARGS`（argv 会出现在 `ps` 与 `docker inspect` 里，而 `REDIS_ARGS` 正是镜像入口原样追加到服务命令行的那个通道）。
  - backend `depends_on: redis: condition: service_healthy`，两个服务同在本栈自有的 `gopherlearn-integrated-data`（`internal: true`）上，索引不发布宿主端口。
- **既有范围规则做了窄改（本轮经授权）**：`internal/app/retrieval_stack_config_test.go` 的 `TestRedisAppearsInExactlyOneComposeFile` 原要求"仓库根 `compose*.yml` 里恰好只有 `compose.redis.yml` 声明 redis"，把索引写进 `compose.integrated.yml` 会撞它——上轮因此把索引放在 `deploy/`，负责人本轮指出那是绕开 glob 而不是修规则。现改为 `TestRedisIsDeclaredOnlyInRegisteredComposeFiles`：允许两份**登记过**的声明，其余照旧禁止——基线三份文件（`compose.app.yml`/`compose.mysql.yml`/`compose.provider.yml`）不得出现 redis 字样、不得引用 `compose.redis.yml`、不得用 `include:`；出现陌生第三份仍红；基线容器/卷/网络名出现在整合文件里仍红。同一条内还按排序比较"实际声明者集合 == 登记者集合"，多一份少一份都红。
- `hybrid/integrated_delivery_test.go` 里原有两条此刻变成假陈述的断言已删（"主文件不得声明 redis"、"backend 加入 `gopherlearn-integrated-index-net`"——该网络名随第二份文件一起消失），另两条 `TestIntegratedIndexRunsOnItsOwnInstanceWithDB0`/`mustFindNetwork` 整体移进 `hybrid/integrated_redis_seam_test.go`：**逐条搬，无一条判据被丢**（镜像摘要、tag 与摘要不并存、非 Stack 镜像拒绝、`FT._LIST` 证明、`REDIS_DATA_DIR` 与挂载一致、只读挂载与 `create_host_path`〔键名更正：原先记的是拼错的 `create_host_directory`〕、不发布端口、独立容器/卷/网络身份、不借基线资源、host/container 对齐、db=0、生产索引名不改）。搬过去的原因只有一个：这些判据现在能对同一份字节做变异对照。
- 顺带把 `TestBuildConfigurationSelectsTheIntegratedEntry` 里"每个 bind 源都必须以 `.local` 结尾"这条收窄为"`.local` 文件或**登记过的部署脚本**，且脚本必须真在仓库里"——因为本栈现在确实要挂两份签入的脚本进容器。判据抽成 `mountedSourcesJudged`，配三条反例（挂 `./hybrid` 源码树、挂签入的 example 配置、登记一个不存在的脚本）。基线资源名清单收敛成一份 `baselineResourceNames`，两处调用点共用，不再各抄一遍。

**凭据接缝的机械差异（不声称继承验收批的凭据证明）**：已验收批次的口令交接是**一次性批次装置**——由 `docker exec -i` 把口令写进容器私有 tmpfs 后由门控使用，批次结束即回收（见 `tutorial/v2-tmpfs-*` 与 `v2-credential-start-experiment`）。常驻 `docker compose up` 没有那个交接步骤，所以本栈用第三种形态：操作员自备 `redis.integrated.conf.local`，只读挂成 `/redis-stack.conf`，preflight 在启动前判它，healthcheck 在运行时从同一份文件现取值并经 `REDISCLI_AUTH` 交给探针子进程。两条链的机制不同，上轮的 (i)/(ii) 候选已收敛到 (i)（因为它就是该镜像入口唯一会读的路径），(ii) 之所以排除是证据支持的：`REDIS_ARGS` 的值会出现在服务进程的 argv 里。真实容器内 `REDISCLI_AUTH` 的可见范围（同 uid 与 root 可读该子进程环境）是这条接缝的成本，本轮没有真机读数；上轮的"两条候选均未验证"改为"形态已定，行为只在离线替身下验证过"。

## 6.1 镜像引用

| 用途 | 引用 | 写法说明 |
| --- | --- | --- |
| MySQL | `mysql@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b` | 负责人给定 `mysql:8.0.46@sha256:62fb…`；此处去掉 tag 只留摘要，摘要逐字相同。理由是本仓既有规则"摘要钉定后 tag 是装饰且会被摘要覆盖"（同库 `TestRedisComposeRunsTheModuleBearingImageOnly` 对索引镜像就是这么要求的）。若要求保留 tag 字面，说一声即可改回 |
| Redis Stack | `redis/redis-stack-server@sha256:39adcbb083b240642cd5738a75c4959c0393ef2a40fa545f2e1c7b55798eeda4` | 同上，给定 tag 为 `7.4.0-v8`；两串摘要均已在本树已验收证据中逐字出现（`tutorial/v2-j1-episode/episode-deliveries.json`、`tutorial/v2-docker-credential-plan/README.md` 等） |
| 基线 MySQL | `mysql@sha256:7dcddc01…`（`compose.mysql.yml`，8B.1） | 未被整合栈复用；测试断言整合文件里不出现该摘要，也不出现 `mysql:8.4` |

**初始化仍未接线，且这是有意的**：`compose.integrated.yml` 不建 schema、不建账号、不跑 `grant`、不做迁移；入口继续拒绝 `migrate=true`。隔离 schema 的首次建立需要一个显式授权动作（候选是 `v2/cmd/v2-isolated` 配一份 `migrate=true` 的隔离配置跑一次），方案单列待批准，见 §9。

## 7. 离线验证读数（`GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`）

§7.1–§7.5 是正式入口轮与修正轮的读数（起点 `c0db468f`）；**本轮（起点 `db6c327f`）的读数在 §7.6**，两批不是同一次运行，计数也不可互相顶替。

### 7.1 当前工作树

| 检查 | 结果 |
| --- | --- |
| `go build ./...` + `go vet ./...`（hybrid） | 通过 |
| `go test ./hybrid`（无 tag） | **70 项通过**，117.647s（上一轮 67 → 本轮 +3：独立索引实例与 DB0、栈产物与镜像引用、凭据文件名与被否证的旧名）；合计含 tag 门 86 项 |
| `go test ./cmd/...`（hybrid 两入口包） | 均 ok（`gopherlearn-integrated` 3 项、`hybrid-isolated` 未变） |
| `go build ./...` + `go vet ./...`（v2） | 通过 |
| 根模块 scoped `build`/`vet` | 通过（`go build ./...` 在起点即因 `tutorial/v2-host-port-tool/probe.go` 失败，与本轮无关） |
| 前端 `npm test` | **50 文件 / 1036 项通过**（与上一轮同，本轮未改前端） |
| 定向 race（hybrid 整合契约 + 两条流式路径；入口包） | ok，26.293s / ok，1.480s，无 `WARNING: DATA RACE`；第一次 23.020s 因文档残留旧文件名而 FAIL，见 §7.4 |
| 全量 `go test -race ./hybrid`（默认 10m） | **仍记"未完成"**：上一轮跑出 `FAIL … 600.443s`、无 `--- FAIL: Test…` 行＝撞时限中止。定向通过**不替代**这条读数 |

### 7.2 原 6 条 CRLF 相关失败：在当前候选字节上重验

上一轮的归因是拿起点兄弟树做的，本轮改为按负责人要求做**全新临时 LF 装配**：

- 装配来源：`git -C <tree> -c core.autocrlf=false archive c0db468f`（76,922,880 字节，tar sha256 前缀 `0a160318`）解到 `GopherAI-main/tmp-lf-assembly-v2-production-entry/`，再叠加本轮 19 个工作字节（CR 剥离后写入，7 个因此变化）。
- 装配指纹：`a264be8a1c8a53d25dc429e67f8b30c6427b6754266fd2e3be9fc4255047f750`，覆盖 3,450 个文件的路径+内容；三份交付文档参与装配与测试但**不计入指纹**（把指纹写回被哈希的文档会让数值自我依赖）。
- 读数与装配对应关系：根模块四个包（含那 6 条）与 hybrid 全部包在**上述指纹的装配**里为 `ok`（117.492s / 2.046s / 6.562s 等）；根模块**全量套件**（`./cmd/... ./internal/... ./conversation/...`，0 条 `FAIL` 行）是在上一版装配（指纹 `c99d0352…`）跑的，两者唯一字节差是入口 stderr 的一句提示文本，该文件不在根模块测试的输入里——这条区分写清楚，不冒充同一份装配。
- 第一次尝试踩过坑：`git archive` 同样受 `core.autocrlf` 影响，产出带 CR，六项照旧失败（那次无效装配的全树指纹 `a999e53e…`）；脚本现加"关键文件含 CR 即中止"的自检，上面的指纹是修正后、且排除文档的最终装配。

同一批定向检查在该 LF 装配里的结果：

| 测试 | 工作树（CRLF） | LF 装配（当前候选） |
| --- | --- | --- |
| `TestDrainWindowIsShorterThanTheContainerStopWindow` | FAIL | **PASS** |
| `TestRuntimeImagesHoldNeitherToolchainNorSource` | FAIL | **PASS** |
| `TestProxyLeavesTheBodyLimitToTheBackendAndStreamsUnbuffered` | FAIL | **PASS** |
| `TestMySQLContainerIsGivenTimeToCloseItself` | FAIL | **PASS** |
| `TestRedisComposeRunsTheModuleBearingImageOnly` | FAIL | **PASS** |
| `TestRedisAppearsInExactlyOneComposeFile` | FAIL | **PASS** |
| 根模块全量套件（`./cmd/... ./internal/... ./conversation/...`） | 未跑（有 6 项已知假红） | **零 FAIL 行**（LF 装配内全绿） |

结论修正为可复核的形式：那 6 项在**当前候选字节 + LF 检出**下全绿，在**同一字节 + 本工作树 CRLF 检出**下全红；差异只在检出行尾，与本轮新增文件无关（`TestRedisAppearsInExactlyOneComposeFile` 会读到我新增的 `compose.integrated.yml`，LF 下仍通过，说明新文件本身满足该范围规则）。本轮**没有**重渲染工作树，也没有用起点兄弟树替代当前候选做验证。

### 7.3 契约证明

- 默认 Agent 到达原 Eino Runner：路由层用 `gin.Engine.Routes()` 核 35 条默认导航路径；行为层断言绑定工具集合含 `document_read`（主线 harness 无此工具）。默认配置（`memoryReadMode=legacy_v2`、`episodeRecallMode=shadow_only`）下 `memory_search` 等不绑定，该"未绑定"也写成断言，防止以后被悄悄放宽。
- 普通聊天零 Agent 模型调用；Agent 回合零普通模型调用。
- 构建/部署选择新入口：`Dockerfile.backend-integrated` 的构建行、多阶段终态 scratch、运行阶段不含配置/凭据、`ARG GO_IMAGE` 默认值取自 `hybrid/go.mod` 的 `go` 指令（不降版本）。
- 配置模板可被入口装载器接受，四类越界被具名拒绝（migrate、预算混入、echo 兜底、缺强制字段）。
- 鉴权/用户复验、短写取消、SSE 终态、删除级联与派生索引失败上报仍由既有套件覆盖（`TestAgentSSEShortWriteCancelsWithoutCompletion` 等），本轮未新增也未改动它们。

### 7.4 过程中出现过的一次失败（文档缺陷，已修）

`TestNoDeliveryFileStillNamesTheUnignoredConfigFile` 第一次跑红，报的是本交付说明自己仍写着被否证的旧文件名（作为 `git check-ignore` 的反例证据）。判据不该为文档让步：改为把逐字读数与旧名移进 `verification.md`（证据件），本文件只留"形如 `<stem>.local.toml`"的描述。该测试的扫描范围（四处指令性文件）与"旧名确实不满足后缀规则"的断言保留，因此这是一次真实的红→绿，不是删断言。

### 7.5 定向 race

见 §7.1 末两行：整合契约与流式路径 ok 26.293s、入口包 ok 1.480s、无 `WARNING: DATA RACE`；全量 race 的"未完成"读数原样保留，未被定向通过替代。

### 7.6 本轮（Redis 常驻接缝收口）读数

全部离线，命令前缀同为 `GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`，工作树仍是 CRLF 检出、未重渲染。

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1`（hybrid，无 tag） | **ok，顶层 77 项**，111.012s（上轮 70：本轮新增 8 个顶层测试、合并删除 1 个 `TestIntegratedIndexRunsOnItsOwnInstanceWithDB0`） |
| `go test -tags hybrid_touch_mysql -v .`（hybrid） | 顶层 79 项，111.436s；**1 条具名 FAIL**＝`TestMySQLAppendMessageSameTimestampZeroChangesCommits` 缺 `-hybrid-touch-mysql-config` 时按设计 `t.Fatal("explicit -hybrid-touch-mysql-config is required")`。该门本轮只编译不执行，与本轮改动无关，也未因凑绿而放宽 |
| 整脚本执行（两份部署脚本） | ok 4.398s：健康 4 条（空数组／1 个／3 个名字／含点与连字符）＋非健康 21 条＋泄漏边界 1 条＋preflight 拒绝 9 条＋放行 1 条＋签入样例端到端 1 条 |
| 忽略匹配器 | ok 0.280s：23 条形似对照自证能红，再对 `.dockerignore`／`.gitignore` 两种读法各下一遍断言 |
| 根模块 scoped `build` / `vet` | 通过（`go build ./...` 在起点即因 `tutorial/v2-host-port-tool/probe.go` 失败，与本轮无关） |
| 根模块 scoped `test`（`./cmd/... ./internal/... ./conversation/...`） | 6 条 FAIL，全部是 §7.2 已登记的 CRLF 假红；改名后的 `TestRedisIsDeclaredOnlyInRegisteredComposeFiles` 红因与改名前逐字相同（`retrieval_stack_config_test.go:158: no top-level "services:" line`），`./conversation/...` 单独跑 ok 0.100s |
| 前端（本轮未改任何 `web/` 文件） | `npm test` 复跑：**50 文件 / 1036 项通过**，5.95s，与上一轮同一读数 |
| 工作树内定向 race（接缝六组） | `go test -race -run 'Healthcheck\|Preflight\|Index\|Mounted\|IgnoreMatcher\|Dockerignore' .` → ok 25.675s |
| **全新 LF 装配（当前候选提交 `e544633b`）** | 根模块全量 24 个包**零 FAIL 行**；§7.2 那 6 条 CRLF 项逐条 `--- PASS`（含改名后的 `TestRedisIsDeclaredOnlyInRegisteredComposeFiles`）；hybrid 无 tag `./...` ok 127.633s、顶层仍 77 项；装配内定向 race ok 46.701s、入口包 race ok 1.371s、`WARNING: DATA RACE` 出现 0 次。装配来源/指纹/自检与逐条读数在 `verification.md` §7.3 |
| 全量 `go test -race ./hybrid` | **仍记"未完成"**：本轮没有重跑，上一轮 `FAIL … 600.443s`（撞默认 10m 时限、无 `--- FAIL: Test…` 行）的读数原样保留，上面几条定向通过不替代它 |

两次"撤掉真判据看它是否变红"的对照（都在工作树做，按字节备份还原，sha 前后一致）：

| 撤掉的判据 | 结果 |
| --- | --- |
| healthcheck 里"不是 JSON 数组就拒"那条分支（`*) fail ...` → `*) : ;;`） | `TestHealthcheckScriptAcceptsAndRefuses` 转红；还原后 `deploy/redis-stack-healthcheck.sh` sha 前缀 `a775dbdc…`，与变异前相同 |
| preflight 的 `refuse()` 不再 `exit 1`（只打印继续走） | 9 条反例逐条报 "refused … but still handed over to the entrypoint"（计数 9）；还原后 `deploy/redis-stack-preflight.sh` sha 前缀 `d02584ea…`，与变异前相同 |

这两条是"绿色可能是空转"的防守：健康门的判据落在退出码与替身入口脚本是否被执行上，不在字符串上。

## 8. 前端默认导航与接口一致性

- 主导航 4 项：对话、文档（V2）、记忆治理（V2）、学习／测验（V2），全部命中整合进程实际服务的路径。
- Shell 启动只发 `/api/v1/me`，不再预取 `/api/v1/collections`；聊天页知识链接指向 V2 文档页；登录页注册链接指向原 V2 邮箱验证码契约（兼容面注册仍是硬 501）。
- 集合管理、会话级 `topic-memory`、旧 `/api/v1/memories` 与 `/api/v1/learning/*` 属基线入口能力，已从默认导航撤下，deep-link 到它们仍会拿到 404/501——这是已知残留，没有用重定向掩盖。

## 9. 仍需负责人决定

1. 隔离 schema 的首次建立：命令、账号、位置与授权（本轮不提供自动通道）。
2. Redis 常驻接缝的**真机一次实测**：形态本轮已离线收敛（挂载 `/redis-stack.conf` + preflight + `FT._LIST` 健康门，理由见 §6），但镜像入口是否真在该路径取配置、卷权限、健康门在真服务上的应答形态、以及 `REDISCLI_AUTH` 在容器内的可见范围都还没有真机读数。上轮列的候选 (ii)（`REDIS_ARGS` 注入）本轮按证据排除：它的值会进服务进程 argv。
3. 既有范围规则的窄改是否可接受（本轮按授权做了）：`TestRedisAppearsInExactlyOneComposeFile` → `TestRedisIsDeclaredOnlyInRegisteredComposeFiles`，从"恰好一个文件"改为"两份登记者"；禁项一条没少（见 §6）。规则形状由实施窗口改动，请复核。
4. 镜像引用是否必须保留 `name:tag@sha256:` 字面（本轮按仓内既有规则写成 digest-only，摘要未变）。
5. 本 worktree 的 CRLF 检出是否重渲染为 LF；本轮另在 `.gitattributes` 加了两条只钉部署脚本的 `eol=lf`（理由见 §2.1 第 8 行），这种"不重渲染、只钉被执行的文件"的做法是否可接受。
6. `GO_IMAGE=golang:1.25-bookworm` 未经拉取验证。
7. 旧入口/基线镜像的长期去留；Collection／主题记忆／旧记忆与出题 REST 的最终归属。
8. `redisConfig.password` 与 `redis.integrated.conf.local` 里的 `requirepass` 必须逐字相同，否则表现为检索认证失败。**没有任何代码判据发现两边不一致**——preflight 只判服务端那一半，入口只判配置是否显式。要不要一个启动前的一致性检查属接线/业务改动，本轮没做。
9. `docker compose up` 时缺 `./redis.integrated.conf.local` 的实际失败形态未实测。**后续更正与实测**：键名应为 `create_host_path`（拼错的 `create_host_directory` 会让 compose 连解析都过不去）；用正确键名后，隔离临时路径里实测到：配置存在→服务正常起来并 healthy；配置缺失→**宿主上仍被建出一个空目录**（Docker Desktop/Windows 不执行该键，A/B 对照带/不带该键行为相同），但**服务起不来**——容器重启循环，日志逐行 `redis-stack preflight: configuration missing`。

## 10. 回退方案

整合本体集中在：一个入口目录、根级四个新文件（`compose.integrated.yml`、`Dockerfile.backend-integrated`、`config.integrated.example.toml`、`redis.integrated.conf.example`）、`deploy/` 两份部署脚本、六个新测试文件、五个前端文件，外加 `.gitattributes` 两条行尾规则与 `.dockerignore` 的注释更正。回退＝撤销这些路径，既有文件不受影响；默认入口与基线镜像始终未改。本轮（接缝）另有一处**既有文件的行为改动**需要一起回退：`internal/app/retrieval_stack_config_test.go` 的那条范围规则窄改，以及 `hybrid/integrated_delivery_test.go` 的两处断言迁移。

```cmd
cd /d <工作区>\wt-v2-gopherlearn-integration
git revert --no-commit <本轮提交号>
git status --porcelain
```

## 11. 计数口径（两栏分列，不相加）

| 栏 | 含义 | 组成 |
| --- | --- | --- |
| 真实门控 43 项 | 上一轮真实资源验收的门控读数 | MySQL 36 ＋ Redis 5 ＋ 联合 2 |
| 离线 hybrid 测试 | `go test ./hybrid`（无 tag）顶层数 | 历史登记 43（`68296da`）；62（整合前）→ 70（正式入口轮）→ **77（本轮接缝）**；带 `hybrid_touch_mysql` 编译时顶层 79，其中 MySQL 那条门在无 `-hybrid-touch-mysql-config` 时具名拒绝执行，不计入"通过" |

本轮 §7.6 的 77 项属后一栏，不能用来声称真实门控通过；两栏也不可相加。
