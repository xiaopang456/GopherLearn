# 修正轮验证记录（integration/v2-production-entry，起点 `c0db468f`）

本文件只放逐字读数与被否证的证据；给操作人员的指示写在 `README.md`。

## 1. 凭据文件名：忽略匹配实测

命令与工作树：`wt-v2-gopherlearn-integration`（分支 `integration/v2-production-entry`），路径均**未创建**、只用 `check-ignore` 判模式，因此不涉及任何真实凭据。

```text
$ git check-ignore -v config.integrated.toml.local
.gitignore:5:*.local	config.integrated.toml.local

$ git check-ignore -v redis.integrated.conf.local
.gitignore:5:*.local	redis.integrated.conf.local

$ git check-ignore -v deploy/redis.integrated.conf.local
.gitignore:5:*.local	deploy/redis.integrated.conf.local

$ git check-ignore -v .env.compose.integrated.local
.gitignore:5:*.local	.env.compose.integrated.local

$ git check-ignore -v config.integrated.local.toml
（无输出，exit 1 —— 未被忽略）

$ git ls-files "*.local" | wc -l
0
```

被否证的旧名就是 `config.integrated.local.toml`：`.gitignore:5` 与 `.dockerignore` 的 `*.local` 匹配的是**后缀**，该名的末段是 `.toml`，两条规则都不命中；上一轮"复制成 `config.integrated.local.toml`（`*.local` 已排除）"的说法是错的，本轮按负责人清单改正为 `config.integrated.toml.local`，并新增断言 `TestNoDeliveryFileStillNamesTheUnignoredConfigFile`（该断言同时把"旧名确实不满足后缀规则"编码为可失败的检查）。

`.dockerignore` 增加一行 `**/*.local`（原 `*.local` 只覆盖上下文根层）。可复核性由 `git ls-files "*.local"` = 0 行支撑：该仓没有任何已跟踪内容以 `.local` 结尾，所以这条规则只能排除凭据与本地状态，不掩盖代码或证据。**未执行 `docker build`**，上下文排除的判据是文件内容 + `Dockerfile.backend-integrated` 的 COPY 行范围（测试已把"运行阶段不出现配置"钉成断言）。

## 2. 镜像引用逐字核对

负责人给定串与本仓已验收证据里的串（`grep -roh` 于 `tutorial/`）：

```text
mysql:8.0.46@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b
mysql@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b
redis/redis-stack-server:7.4.0-v8@sha256:39adcbb083b240642cd5738a75c4959c0393ef2a40fa545f2e1c7b55798eeda4
redis/redis-stack-server@sha256:39adcbb083b240642cd5738a75c4959c0393ef2a40fa545f2e1c7b55798eeda4
```

两种形态在本仓都已存在（例：`tutorial/v2-docker-credential-plan/README.md`、`tutorial/v2-j1-episode/episode-deliveries.json`、`tutorial/v2-j1-knowledge/deliverable-hashes.json`）。部署文件采用**只留摘要**的形态，摘要与给定值逐字相同；tag 写在相邻注释里。基线 8B.1 的 `mysql@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b` 不出现在整合文件里（断言覆盖），未经批准的 `mysql:8.4` 也已从 `compose.integrated.yml` 移除。

第一次写入时我把该摘要抄错了一个字符（`…a0fcee4a…` 写成 `…a0fcaa4a…`），发现后按上述树内字符串逐字校正；这条记录留着，是为了说明核对方式是"与证据文件比字节"，不是"看着像"。

## 3. LF 测试装配：来源与被否证的第一次尝试

装配脚本在树外（`GopherAI-main/tmp-build-lf-assembly.py`），输出目录 `GopherAI-main/tmp-lf-assembly-v2-production-entry/`。

- 第一次尝试**无效**：`git archive` 同样受 `core.autocrlf=true` 影响，解出的 `Dockerfile.backend`、`compose.mysql.yml`、`cmd/gopherlearn/main.go` 等仍带 CR，六项照旧失败（消息与工作树完全相同）。该次读数不作为结论。
- 修正后：`git -C <tree> -c core.autocrlf=false archive --format=tar c0db468f`，tar 76,922,880 字节，sha256 前缀 `0a1603181b67e9b3`；解包后对 4 个探针文件做"含 CR 即中止"自检；再叠加 19 个本轮工作字节（剥 CR 后写入，7 个因此变化）。
- 装配指纹（3,450 个文件的路径+内容；三份交付文档不计入，否则数值自我依赖）：
  - 无效那次（archive 仍带 CR）：`a999e53e3273f82bc1fbc8cf8b444194d2fb3e7177702a387d3607b6585f0aac`（全树哈希，未排除文档）
  - 倒数第二版：`c99d0352d325cfc32abbbf3807a99c43c8602152d55f3d2a4b421f854415634a` ← 根模块**全量套件**零 `FAIL` 行的读数出自这一版
  - **最终版**：`a264be8a1c8a53d25dc429e67f8b30c6427b6754266fd2e3be9fc4255047f750` ← 两版唯一字节差是入口 stderr 的一句提示文本，不在根模块测试输入内
- 装配环境补项：`web/node_modules/` 不入库也不在归档里，因此三条"前端经真实 socket"的 hybrid 测试（`TestFrontendAgentComposableThroughHybridHTTP`、`TestLearningPageLogicThroughLocalSocket`、`TestMemoryFrontendPageAndAgentCardThroughOriginalHTTP`）在装配内首先报 `Cannot find package 'vue'`——那是装配缺依赖，不是代码失败；把该目录复制进装配后同三条与全套 70 项一起通过（`ok 129.852s`）。这条差异如实记在这里，避免"装配全绿"被读成无须准备。

## 4. 原 6 条 CRLF 相关失败：同一检查在两处的逐字对比

工作树（CRLF 检出，本轮修正后仍未重渲染）：

```text
--- FAIL: TestDrainWindowIsShorterThanTheContainerStopWindow
    shutdown_test.go:212: main.go no longer declares shutdownGrace as a whole number of seconds
--- FAIL: TestRuntimeImagesHoldNeitherToolchainNorSource
    docker_stack_config_test.go:121: Dockerfile.backend declares no GO_IMAGE default
--- FAIL: TestProxyLeavesTheBodyLimitToTheBackendAndStreamsUnbuffered
    docker_stack_config_test.go:175: the request body limit is no longer a `const maxRequestBytes = N << M` in internal/httpapi
--- FAIL: TestMySQLContainerIsGivenTimeToCloseItself
    docker_stack_config_test.go:286: compose.mysql.yml gives the mysql service no stop_grace_period
--- FAIL: TestRedisComposeRunsTheModuleBearingImageOnly
    retrieval_stack_config_test.go:77: no top-level "services:" line
--- FAIL: TestRedisAppearsInExactlyOneComposeFile
    retrieval_stack_config_test.go:143: no top-level "services:" line
```

LF 装配（**当前候选字节 + 本轮修改**，不是起点兄弟树）：

```text
ok  	github.com/xiaopang456/GopherLearn/cmd/gopherlearn	0.188s
--- PASS: TestComposeStackCarriesNoSecretsAndNoExtraServices
--- PASS: TestRuntimeImagesHoldNeitherToolchainNorSource
--- PASS: TestProxyLeavesTheBodyLimitToTheBackendAndStreamsUnbuffered
--- PASS: TestDockerignoreExcludesCredentialsAndSealedRecords
--- PASS: TestMySQLContainerIsGivenTimeToCloseItself
--- PASS: TestRedisComposePublishesLoopbackOnly
--- PASS: TestRedisComposeRunsTheModuleBearingImageOnly
--- PASS: TestRedisAppearsInExactlyOneComposeFile
ok  	github.com/xiaopang456/GopherLearn/internal/app	0.137s
```

同一装配内根模块全量套件（`./cmd/... ./internal/... ./conversation/...`）输出中 `FAIL` 行数：**0**。

两点值得单独记下：`TestRedisAppearsInExactlyOneComposeFile` 会 glob 仓库根的 `compose*.yml`，因此它读到了本轮新增的 `compose.integrated.yml`——LF 下仍通过，说明新文件满足那条范围规则（也正因如此，整合索引被放在 `deploy/`，见交付说明 §6 的冲突登记）。`TestDockerignoreExcludesCredentialsAndSealedRecords` 在新增 `**/*.local` 后仍通过。

## 5. 测试与 race 读数

| 项 | 命令 | 结果 |
| --- | --- | --- |
| hybrid 全量（无 tag，工作树 CRLF） | `go test . -count=1` | **ok，70 项**，117.647s |
| hybrid 全量（无 tag，最终 LF 装配） | 同上，在 `tmp-lf-assembly-v2-production-entry/hybrid` | **ok，70 项**，129.852s（需先补 `web/node_modules`，见 §3） |
| 定向 race（hybrid 整合契约 + 两条流式路径） | `go test . -race -count=1 -timeout 15m -run "Integrated\|NoDeliveryFile\|BuildConfiguration\|ChatAndAgentTurns"` | 第一次 23.020s FAIL，唯一失败是交付说明里残留旧文件名（文档缺陷，非代码）；README 修正后复跑 **ok，26.293s**，无 `WARNING: DATA RACE` |
| 入口包 race | `go test ./cmd/gopherlearn-integrated -race -count=1 -timeout 15m` | **ok，1.480s** |
| 全量 race（默认 10m） | `go test . -race -count=1` | `FAIL … 600.443s` 且无 `--- FAIL: Test…` 行＝撞时限中止，**仍记未完成**；上面两条定向读数不替代它 |
| 工作树内那 6 条（本轮字节再跑一次） | `go test ./cmd/gopherlearn/ ./internal/app/ -run "…"` | 仍 6 条 FAIL，消息与上一轮逐字相同（`shutdown_test.go:212`、`docker_stack_config_test.go:121/175/286`、`retrieval_stack_config_test.go:77/143`），与 §4 的对比表一致 |

## 6. 本轮（起点 `db6c327f`，Redis 常驻接缝收口）逐字读数

零 Docker、零真实端口查询、零真实服务连接、零凭据生成、零数据迁移、零真实资源删除。所有"执行"都是把两份部署脚本对着**替身命令 + 合成口令**跑，不触碰任何网络。

### 6.1 身份

```text
git rev-parse HEAD          db6c327f46931f4ab3765cd8d73ca43012f1c88a
git branch --show-current   integration/v2-production-entry
git status --porcelain      0 行（开工）
```

封存入口证据（本轮判据的依据，只读）：`tutorial/v2-j1-knowledge/frozen/image-entry-evidence/entrypoint.sh.txt` 第 8-10 行只在 `/redis-stack.conf` 存在时设 `CONFFILE`，第 25-36 行恒加 `--protected-mode no --daemonize no` 与模块参数、行尾接 `${REDIS_ARGS}`；同目录 `entry-manifest.json` 记该镜像 `Cmd: ["/entrypoint.sh"]`、环境里 `REDIS_ARGS` 为空。

### 6.2 整脚本执行（仓库外替身运行器，工作树字节）

```text
== healthy responses (expect rc=0) ==
empty_array            rc=0 | redis-stack health: ok (indexes=0)
one_string             rc=0 | redis-stack health: ok (indexes=1)
three_strings          rc=0 | redis-stack health: ok (indexes=3)
== responses that must NOT read as healthy (expect rc!=0) ==
number_element         rc=1 | redis-stack health: reply contains a non-string element
mixed_element          rc=1 | redis-stack health: reply contains a non-string element
object_reply           rc=1 | redis-stack health: reply is not a JSON array
garbage                rc=1 | redis-stack health: reply is not a JSON array
unbalanced_quote       rc=1 | redis-stack health: reply is not a JSON array
bare_token             rc=1 | redis-stack health: reply contains a non-string element
truncated_open         rc=1 | redis-stack health: reply is not a JSON array
module_missing         rc=1 | redis-stack health: search module missing
auth_missing_err       rc=1 | redis-stack health: authentication rejected
auth_no_err_prefix     rc=1 | redis-stack health: authentication rejected
conn_refused           rc=1 | redis-stack health: connection failed
noauth_inside_array    rc=1 | redis-stack health: authentication rejected
conf_without_requirepass rc=1 | redis-stack health: requirepass is not exactly one directive
conf_with_two_directives rc=1 | redis-stack health: requirepass is not exactly one directive
conf_secret_too_short  rc=1 | redis-stack health: requirepass is shorter than the agreed floor
conf_placeholder       rc=1 | redis-stack health: requirepass is shorter than the agreed floor
conf_absent            rc=1 | redis-stack health: configuration unreadable
== client itself failing (expect rc!=0) ==
client_error           rc=1 | redis-stack health: probe command failed
== leak boundary on the accepted case ==
leak_case              rc=0 | redis-stack health: ok (indexes=1)
  in child argv    : 0
  in script output : 0
  child env channel: 1
  argv line        : ARGV:-2 --json FT._LIST
== preflight refusals before the server starts (expect rc!=0) ==
pre_no_directive       rc=1 | redis-stack preflight: requirepass must appear exactly once
pre_two_directives     rc=1 | redis-stack preflight: requirepass must appear exactly once
pre_too_short          rc=1 | redis-stack preflight: requirepass is shorter than the agreed floor
pre_placeholder        rc=1 | redis-stack preflight: requirepass is an unfilled placeholder
pre_quoted             rc=1 | redis-stack preflight: requirepass carries quoting or whitespace
pre_empty_file         rc=1 | redis-stack preflight: configuration empty
pre_missing_file       rc=1 | redis-stack preflight: configuration missing
== preflight accepted case (expect rc=0 and the real entrypoint reached) ==
pre_good               rc=0 | entered-real-entrypoint
```

三条如实说明，不让它们被排版掩盖：

- `conf_placeholder` 在 healthcheck 侧报的是"短于地板"而不是"占位串"——健康脚本没有占位串规则（preflight 才有），那条 27 字节的 `REPLACE_WITH_REDIS_PASSWORD` 先撞上 ≥32 字节地板。这一条在 Go 断言里也按 "shorter than the agreed floor" 钉住，不写成它没做的事。
- `pre_spaces`（`requirepass has embedded words here`）同理：两份脚本都用 awk 的第二字段读值，含空格的值只会取到第一个 token `has`，于是被地板拒绝。也就是说"服务端与检查脚本读到不同口令"这个担心在该形态下不成立（两边取同一 token）；真正生效的判据是地板＋单 token 白名单，不是"检测空格"。这条在 Go 表里也按实际原因登记。
- 上面这份替身读数比 Go 版少一条（`pre_spaces` 那行在 Go 里是 `{"a value with embedded spaces", ..., "shorter than the agreed floor"}`），两边原因一致，条数不同，不混着报。

同一批用例的 Go 版本（`TestHealthcheckScriptAcceptsAndRefuses`：4 条健康 + 21 条非健康；`TestPreflightRefusesBeforeTheServerStarts`：9 条拒绝 + 1 条放行 + 1 条签入样例）退出码与具名原因与上面一致；Go 版还多断言了 preflight 拒绝时**替身入口脚本的执行次数为 0**，那才是"启动前拒绝"的真正判据。

### 6.3 两次"撤掉判据看它是否变红"（工作树内，按字节备份还原）

```text
变异 1：healthcheck 的 `*) fail "reply is not a JSON array" ;;` → `*) : ;;`
        TestHealthcheckScriptAcceptsAndRefuses → FAIL
        还原后 sha 前缀 a775dbdc75f087cf（与变异前一致）
变异 2：preflight 的 refuse() 去掉 `exit 1`（只打印继续走）
        TestPreflightRefusesBeforeTheServerStarts → 9 条 "still handed over to the entrypoint"
        还原后 sha 前缀 d02584eaf342104a（与变异前一致）
```

### 6.4 忽略匹配读数

匹配器判据在 `hybrid/integrated_ignore_matching_test.go`：23 条形似对照（含 `!` 与顺序、目录规则、`**/` 与 `/**`、`?` 不跨分隔符）先自证能红，再对两份真实文件下结论。无分隔符模式的两种读法（Docker 文档表格＝只根层；moby 实际匹配器＝任意深度）**都断言**，因为本轮没有 Docker 可以去判它到底按哪一种。

`git check-ignore` 实测（只读，本轮新跑）：

```text
git check-ignore -v config.integrated.toml.local        -> .gitignore:5:*.local
git check-ignore -v redis.integrated.conf.local         -> .gitignore:5:*.local
git check-ignore -v .env.compose.integrated.local       -> .gitignore:5:*.local
git check-ignore -v deploy/redis.integrated.conf.local  -> .gitignore:5:*.local
git check-ignore    redis.integrated.conf.example       -> exit 1（未被忽略：样例必须可提交）
git check-ignore    config.integrated.example.toml      -> exit 1（同上）
git ls-files "*.local"                                  -> 0 行（后缀规则不掩盖任何已跟踪内容）
```

`git check-attr -a deploy/redis-stack-healthcheck.sh` → `text: set`、`eol: lf`（preflight 同）；对照项 `compose.integrated.yml` → 两项都 unspecified，说明新增规则没有超出这两份文件。

### 6.5 交付件字节（工作副本 sha256；`cr` 为该文件工作树里的 CR 字节数）

| 路径 | 字节 | CR | sha256 |
| --- | --- | --- | --- |
| `deploy/redis-stack-preflight.sh` | 2,602 | 0 | `d02584eaf342104aabea12cc02e99fc90a34213338183819f9226520c694c8db` |
| `deploy/redis-stack-healthcheck.sh` | 3,532 | 0 | `a775dbdc75f087cf436aa2bafcb6a9c1fb65f43e15618e91f2de65c8f0033003` |
| `redis.integrated.conf.example` | 1,339 | 0 | `538b09b43e8e4cf8651d282c2590630fddb2e1d4969bddc24f9a7a8bc09918be` |
| `compose.integrated.yml` | 10,714 | 0 | `326ae66d4ec8cc42e4101ad26a8bebd76b708b27075b3dbc728ac79bffed9c86` |
| `config.integrated.example.toml` | 5,751 | 0 | `9c61aedaabb8ce007748a7ed9670c00a3d3e74df4edbfa03568f38a659829a57` |
| `hybrid/integrated_redis_seam_test.go` | 41,103 | 0 | `80ee922e081daf616230e643d532c7d7ea6061d0847b9bc97d1e414bfc907c67` |
| `hybrid/integrated_ignore_matching_test.go` | 10,830 | 0 | `1e5acc47812a3eaccf289540b9fef87e6d2e8896463b8b374f194fbc9e87e677` |
| `hybrid/integrated_delivery_test.go` | 14,534 | 0 | `ac4f7dc7ebd5ea309995b9818518865328877574a7bec4a421cff596b63e6863` |
| `internal/app/retrieval_stack_config_test.go` | 8,989 | 193 | `ce4830da1aa357a7188866c3b6646a5ccda93407a7f320ba631e4a3bead12f86` |
| `.gitattributes` | 1,466 | 18 | `7053972b4f6f7efb3841bdf30ce87d41be8a7fa7a7a906615820d47c090b5ba9` |
| `.dockerignore` | 1,638 | 57 | `993a38653ea6a91894c3284a03f86e175edb736e53d497d8eeff588542fb24d3` |
| `tutorial/v2-production-entry/README.md` | 35,205 | 0 | `de4862f4b0185fa2658bdc02621552c9da148b004907aa200d899b32d245ae80` |
| `tutorial/v2-production-entry/learning.md` | 22,972 | 0 | `bdca2baf6f4aabedc7eec24552f473acc943cbb31ca7057a791d48b0f0417fa5` |

`cr>0` 的三个文件在本 worktree 是 CRLF/混行检出（`core.autocrlf=true`），提交后 blob 归一成 LF，因此这三个的"工作副本 sha"不等于 blob sha——按 §5 的口径以工作副本字节登记，不冒充提交态。CR=0 的件提交后逐条对 `git cat-file blob HEAD:<path>` 复核，读数记在 §7。

上层学习主线 `optimization/gopherai-agent/code-learning.md` 本轮追加 AGENT-032 一节：`bc4edf5eba3ebbab…`（1,024,054 字节）→ `2f895c8f0bd57689847d631adfa45a92712999076c3d93fe2a1dc74a5acb8b20`（1,031,711 字节；追加块 7,657 字节，与 `learning.md` 内同源块逐字节相同，两侧 sha 均为 `a20830487ad9ddd5aa5f9addac7f8758b8464b667f35d01491e90f515d139ca7`）。上一轮登记的 `e245d804…` 是当时的状态，引用旧值的说明需按新值改。

## 7. 提交后的字节复核、LF 装配读数与 race

### 7.1 提交

| 项 | 读数 |
| --- | --- |
| 接缝主体提交 | `e544633b13e574422ff27e57d2354ebaf0014a56`（16 条路径：新增 5、修改 10、删除 1 `deploy/compose.integrated.redis.yml`） |
| 提交后 `git status --porcelain` | 0 行（本节读数的补记是第二笔，登记在文末） |
| 其它树 | `GopherLearn-clean` `2b5150e`／5 条未跟踪项原样；`wt-v2-direct-copy` `e3ad48e`／0 行 |

### 7.2 blob 与工作副本逐件核对（`git cat-file blob HEAD:<path>`）

| 路径 | 结论 | blob sha256 前缀 |
| --- | --- | --- |
| `deploy/redis-stack-preflight.sh` | 与工作副本相同 | `d02584eaf342104a` |
| `deploy/redis-stack-healthcheck.sh` | 相同 | `a775dbdc75f087cf` |
| `redis.integrated.conf.example` | 相同 | `538b09b43e8e4cf8` |
| `compose.integrated.yml` | 相同 | `326ae66d4ec8cc42` |
| `config.integrated.example.toml` | 相同 | `9c61aedaabb8ce00` |
| `hybrid/integrated_redis_seam_test.go` | 相同 | `80ee922e081daf61` |
| `hybrid/integrated_ignore_matching_test.go` | 相同 | `1e5acc47812a3eac` |
| `hybrid/integrated_delivery_test.go` | 相同 | `ac4f7dc7ebd5ea30` |
| `tutorial/v2-production-entry/README.md` | 相同 | `de4862f4b0185fa2` |
| `tutorial/v2-production-entry/learning.md` | 相同 | `bdca2baf6f4aabed` |
| `internal/app/retrieval_stack_config_test.go` | **不同**（工作副本混行 CR=193，blob 归一成 LF） | `8f5f93c0102e73c4` |
| `.gitattributes` | 不同（CR=18） | `da28edd8d62f9e7e` |
| `.dockerignore` | 不同（CR=57） | `b62ab6af740ad83d` |
| `README.md` | 不同（既有 CRLF 件，本轮插入行为 LF → 混行） | `4ec39b4aff9d86a4` |

两份被容器执行的脚本 blob＝工作字节＝LF，这条是 `.gitattributes` 那两条规则存在的理由，也是它可核的形态。四个"不同"都是行尾渲染差，不是内容改动；按 §5 口径登记，不重渲染工作树去消掉它。

### 7.3 全新 LF 装配（当前候选提交，不是起点兄弟树）

```text
来源    git -C <tree> -c core.autocrlf=false archive e544633b…（tar 76,349,440 字节，
        sha256 d04825de641c68f14aaec757bba032d79368e0493432e8376710bf9993922b7f）
落点    GopherAI-main/tmp-lf-assembly-redis-seam/（新目录；上一轮的 tmp-lf-assembly-v2-production-entry 不覆盖）
文件数  3,457（指纹覆盖 3,454：三份交付文档参与装配与测试但不计入指纹，避免自我依赖）
指纹    ca5ff15219670846e9bf77ee8241c9b8230b61506be4dc682e5c2402ff8fabed
自检    关键文件（README、cmd/gopherlearn/main.go、四份 compose、Dockerfile.backend、两份 internal/app 测试、
        部署脚本）CR 字节数逐条 = 0；再对 cmd/ internal/ compose.integrated.yml Dockerfile.backend 全量 grep CR，
        命中即中止（本轮未中止）
```

装配内读数：

| 检查 | 结果 |
| --- | --- |
| 根模块 scoped `build` | 通过 |
| 根模块全量 `test`（`./cmd/... ./internal/... ./conversation/...`） | **24 个包全 ok，零 FAIL 行**（`internal/app` 6.876s、`gormmysql` 44.233s） |
| 那 6 条 CRLF 相关项 | `--- PASS` 六条齐全：`TestDrainWindowIsShorterThanTheContainerStopWindow`、`TestRuntimeImagesHoldNeitherToolchainNorSource`、`TestProxyLeavesTheBodyLimitToTheBackendAndStreamsUnbuffered`、`TestMySQLContainerIsGivenTimeToCloseItself`、`TestRedisComposeRunsTheModuleBearingImageOnly`、`TestRedisIsDeclaredOnlyInRegisteredComposeFiles` |
| hybrid 全量（无 tag，`./...`） | ok 127.633s；入口包 ok 0.278s；`hybrid-isolated` ok 0.268s |
| hybrid 顶层计数 | 77 项，与工作树同一读数（同一批字节，两种检出形态） |
| 定向 race（LF 装配，整合／索引／接缝／匹配器／构建／双模型／基线隔离） | ok 46.701s |
| 入口包 race | ok 1.371s |
| `WARNING: DATA RACE` 出现次数（上述 race 运行内扫描） | 0 |

装配环境补项（同 §3 的偏离，不隐藏）：`web/node_modules/` 不在归档里，三条"前端经真实 socket"的测试需要它；本轮从工作树复制 5,693 个文件进装配后，那三条与其余 77 项一起 ok。`node_modules` 不计入上面的指纹（它不是候选字节）。

### 7.4 工作树内的定向 race（与 LF 装配分列）

```text
go test -race -count=1 -timeout 900s -run 'Healthcheck|Preflight|Index|Mounted|IgnoreMatcher|Dockerignore' .
ok  	gopherlearn-v2-hybrid	25.675s
```

全量 `go test -race ./hybrid` 的上一轮记录（`FAIL … 600.443s`，无 `--- FAIL: Test…` 行＝撞默认 10m 时限中止）**原样保留，仍记"未完成"**；本轮没有重跑全量 race，上面两条定向读数不替代它，也不构成"全量 race 已通过"。

### 7.5 本轮没有做、也没有声称的事

零 Docker／零 `docker build`／零 `docker compose`／零真实端口查询／零真实 MySQL·Redis·Provider·SMTP 连接／零凭据生成（合成串 `synthetic-redis-credential-0123456789abcdef` 只出现在测试夹具里）／零数据迁移／零真实资源删除／未 merge·push／未更新 `GopherLearn-clean`／未动业务代码、Eino Runner、依赖版本或锁文件。上面所有"通过"都是离线判据：脚本在替身命令下执行、compose 与忽略规则按字节读取、装配只是同内容的另一种行尾形态。
