# 发布就绪报告（release closeout）

本轮目标：在**不依赖 RPC 框架**的公开版上收口功能缺陷、补齐安装与注册路径、让遗留测试失败有据可依，并在**独立干净环境**里重新验证。

- 公开版目录：本仓库（仓库根目录即公开版）
- 本轮未改动的目录：原 `GopherLearn`、`GopherLearn-clean`、面试版、RPC 项目与历史实验工作树
- 本轮未推送、未建立公开仓库、未启动摘要优化、未调用真实 LLM 或 embedding
- 范围与移除细节见 `PUBLIC-RELEASE-NOTES.md`；依赖许可清单见 `THIRD-PARTY.md`；真实模型验证计划见 `REAL-MODEL-VALIDATION-PLAN.md`
- 原始验证日志在**仓库外**的验证目录里（`<工作区>\GopherLearn-release-checks\release-closeout-v1\logs\`），不随本仓库发布

---

## 1. 本轮修改

### 1.1 功能缺陷（先复现，再最小修复，再回归）

| 编号 | 修前 | 修后 | 改动位置 |
|---|---|---|---|
| **F-01** 新用户无法注册：验证码请求只回答「服务繁忙」 | `4001 服务繁忙`；日志里是 `gomail: invalid address ""` | 未配置邮件：`2012 邮件服务未配置，无法发送验证码：请在配置文件的 [emailConfig] 段填写发件地址与授权码`；已配置但 relay 失败：`2013 验证码邮件发送失败，请稍后重试或检查邮件服务配置`。启动时也会打印一行同样的提示 | `v2/config/config.go`（`EmailConfig` 增加 `host`/`port`）、`v2/common/email/email.go`（SMTP 主机可配 + `ErrNotConfigured`/`ErrSendFailed`）、`v2/common/code/code.go`（2012/2013）、`v2/service/user/user.go`（按错误类型分流） |
| **F-02** 请求体绑定失败被当成服务端错误 | `500` + `4001 服务繁忙`（记忆创建/纠正/归档/删除/草稿决议共 8 种情形） | `400` + `2001 请求参数错误`；真正的服务端错误仍是 `500` | `v2/controller/memory/memory.go`（`respondError` 新增绑定错误分支，排在已知业务映射之前） |
| **F-03** 非 multipart 的上传被当成服务端错误 | `500` + `4001` | `400` + `2001`；超限请求仍是 `413` + `7002` | `v2/controller/knowledge/document.go`（`http.ErrNotMultipart` → 400） |
| **F-07** 兼容接口未认证时响应体为空 | `401`，**空 body** | `401` + `{"data":null,"error":{"code":"unauthorized","message":"authentication is required"}}` | `hybrid/http.go`（按捕获到的状态码决定信封，而不是按有没有 body） |
| **F-08** 缺少 `file` 字段被误报成扩展名错误 | `400` + `7001 仅支持有效的 .md 或 .txt 文件` | `400` + `7011 缺少文件字段 file`；真正的 `.pdf` 仍报 `7001` | `v2/controller/knowledge/document.go`（本地定义 7011 与其文案） |

> F-07、F-08 的原始复现与修后取值记录在仓库外的 `logs/` 里；公开仓库内只保留回归测试。
> **所有权核验、跨用户隔离、JWT 校验与来源权限边界都没有被改动**，相关测试全部保持通过。

### 1.2 注册路径补齐

- `config.integrated.example.toml` 新增完整的 `[emailConfig]` 段：`email`、`authcode`、`host`、`port`，并写明「缺这一段会怎样」（服务器照常启动、已有账号照常工作，只有注册走不通，并给出 2012 的具体提示）。
- `host`/`port` 可配置，默认仍是 `smtp.qq.com:587`，老部署不受影响；指向本地捕获服务即可自测。
- 新增 `v2/cmd/schemainit`：**入口拒绝改 schema，所以首次建库是一条显式的命令**，而不是启动副作用。原先提供这件事的命令随 RPC 一起被移除了，公开版必须补回一个只做建库、不做别的工具。
- **缺配置时的行为已确定并实测**：服务器照常启动、登录 / 会话 / 文档 / 记忆 / 学习全部照常工作，只有「请求验证码」回答 `2012 邮件服务未配置…`；启动时打印一行同样的提示。已有的账号不受任何影响。
- **Docker 部署路径补齐**：`Dockerfile.backend-integrated` 现在一并产出 `/schemainit`（运行层是 `scratch`，没有 shell，而这个栈的 MySQL 又不开端口——没有第二个二进制，操作者根本无法建表，后端会永远重启循环）；`compose.integrated.yml` 增加 profile 门控的 `schemainit` 服务，普通 `up` 不会执行它；README 的「With Docker」一节写全了三个 `.local` 文件、带外建库、建表与访问地址。

### 1.3 界面一致性

| 问题 | 修前 | 修后 |
|---|---|---|
| 文档处理面板在本地模式下**从不轮询** | 面板只读一次，文档还在 `indexing` 也看不到变化 | 按 `pending`/`indexing` 轮询，`ready`/`failed` 停止（`web/src/components/DocumentProcessingPanel.vue`） |
| 三个历史基线页面显示误导性的空列表 | 「还没有知识库」「这条记忆已经不在了」「这一步没有成功」 | 页面顶部常驻说明这组接口属于历史基线入口，并直接给出整合入口里对应页面的链接（`web/src/components/BaselineSurfaceNotice.vue`） |
| 正式上传页的体积说明与后端配置脱节 | 写死「单文件 5 MiB」 | 写明这是服务端 `knowledgeConfig.maxFileSizeBytes` 的默认值，改动后以服务端返回的提示为准 |

> 三个基线页面的失败文案契约（「404 有专属句子、不泄露信息」）**没有改动**：为此先写的「按 404 推断路由不存在」的做法会让一个按资源维度的 404 变成另一种含义，测试已因此拒绝；改成不依赖运行时猜测的常驻说明。

### 1.4 遗留测试失败的处理

逐个列出并分类，**没有删除、跳过或弱化任何断言**：

| 失败用例 | 分类 | 处理 |
|---|---|---|
| `GopherAI/middleware/jwt` 全部（`TestAuthWritesCompleteIdentityToContext` 等） | **缺少明确的测试环境**（树内没有 `config/config.toml`，`config.GetConfig()` 直接 panic） | 测试改为显式安装一份配置（`config.Use`），断言一条未动。同类的 `GopherAI/utils/myjwt` 一并处理 |
| `GopherAI/config`（`TestReadOnlyAccessorsReportWhatTheRunWillUse`） | 同上 | 测试改为自己写一份临时配置文件再读，断言一条未动 |
| `GopherAI/internal/summaryreadeval` 16 项 | **内部实验材料**，且产品代码零依赖（只有 8 个 `cmd/summaryread*` 实验命令引用它） | 整包与其 8 个实验命令移出公开版；随之移出的还有上次真实运行的账本与清单（约 2.2 MB / 127 文件），它们正是公开内容里不该带的内部实验材料 |
| `hybrid` 的 `TestHealthcheckScriptAcceptsAndRefuses`、`TestHealthcheckLeakBoundaryIsMeasured`、`TestPreflightRefusesBeforeTheServerStarts` | **缺少明确的测试环境**（这三个用例要执行 `deploy/*.sh`，找不到 POSIX shell 时项目**刻意失败而不是跳过**） | 不改代码、不改断言；在 `CONTRIBUTING.md` 写明「把 Git 的 `usr\bin` 放进 PATH 即可全绿」，并在本报告记录 |
| `hybrid` 的 `TestMemoryNativeHTTPOriginalLifecycleAndTombstone` | **非 RPC 变化导致的过期断言**（F-02 把绑定失败从 500 改成 400，而旧断言明确要求 500） | 断言随契约更新为 400，注释写明为什么 |
| `hybrid` 的 `TestRegistrationMailFailureCleanup` | 同上（F-01 把邮件失败从通用 4001 改成专属 2013） | 断言随契约更新为 2013，清理行为的断言一条未动 |
| 根模块 `cmd/gopherlearn` 的 `TestDrainWindowIsShorterThanTheContainerStopWindow`、根模块 `internal/app` 的 `TestRedisIsDeclaredOnlyInRegisteredComposeFiles` | **原判为「本轮暂未解决」的既有失败，本轮查明根因并修掉**（见 §1.6：两处都是 CRLF 造成的断言失效，不是业务缺陷） | 行尾归一 + `.gitattributes` 钉住，断言一条未动，两条现已通过 |

### 1.5 一处**合同机制修正**：`bind.create_host_path`（正确键）+ 保留 preflight

上一轮把 compose 里 `bind.create_host_directory: false` 当成"Compose v5 不再支持的键"而**删除**了四个 `bind:` 块，并把"缺配置即拒绝启动"整体交给 preflight。本轮查清了根因：**那个键名本身是拼错的**。

| | 事实 | 证据 |
|---|---|---|
| `create_host_directory` | **不是 Compose 的键**。Compose v5.1.3 直接拒绝解析：``validating …: services.missing.volumes.0.bind additional properties 'create_host_directory' not allowed`` | 隔离临时路径实测（`docker compose config --quiet` 退出码 1） |
| `create_host_path` | **是 Compose 的键**，且被正确保留在解析结果里 | 同上，退出码 0，解析输出含 `create_host_path: false` |

所以问题从来不是"版本不兼容"，而是"键名写错了"。本轮的修法：**用正确的键名把 flag 加回去，preflight 保留不动**。compose 里 5 个 `bind:` 挂载（redis 的配置与两份脚本、backend 的配置、schemainit 的配置）都声明了 `create_host_path: false`。

#### 隔离临时路径的实测（不碰已有运行环境）

隔离目录 `…\.tmp-bind-verify`，独立 project 名 `bindverify`，只起 `redis` 这一个服务；`…\.tmp-bind-probe` 另做一组同引擎、同镜像、同一时刻的 A/B 对照。

| 情形 | 是否创建宿主目录 | 是否启动服务 | 观测到的形态 |
|---|---|---|---|
| 配置**存在**（`redis.integrated.conf.local` 在位） | —（本来就在） | ✅ **启动成功** | `gopherlearn-integrated-redis \| Up (healthy)`；preflight 通过并 exec 镜像入口 |
| 配置**缺失**（文件已删） | ⚠️ **创建了**（空的**目录**） | ❌ **没有启动** | 容器进入重启循环；日志逐行 `redis-stack preflight: configuration missing` |
| A/B 对照：同样的缺失源，**带** `create_host_path: false` vs **不带** | **两者行为完全相同**（都建成目录） | 两者都启动容器 | 同一引擎、同一镜像、同一时刻 |

**因此结论要分两层说，不能混成一句**：

1. **"服务不会带着缺失的配置起来"是成立的，而且有两个独立承担者**：`create_host_path: false`（在遵守该键的平台上，缺文件直接创建失败）与 preflight（本机实测：容器起不来，日志点名 `configuration missing`）。
2. **"缺失的配置不会在宿主上留下痕迹"在本机不成立**：Docker Desktop（Windows）会照样把缺失的 bind 源建成空目录，`create_host_path: false` 被 compose 接受并保留，但**引擎没有执行它**。这一点是 A/B 实测出来的，不是推测。
3. 在遵守该键的平台（Linux）上，第 2 条由 compose 承担；在本机上，第 2 条**没有人承担**——这是已知限制，已登记在 §3.3。

#### 判据怎么变（`hybrid/integrated_redis_seam_test.go`）

| 判据 | 变更前 | 变更后 |
|---|---|---|
| 配置挂载 | 必须只读 | 必须只读 **且** 带 `create_host_path: false`（缺了要报出来） |
| 拼写 | 不允许出现 `create_host_directory` | 不允许出现 `create_host_directory`，并把原因写清楚（"不是 Compose 的键，正确键是 create_host_path"） |
| 变异对照 | 「把这行加回来要怎样」 | 换成两条：「删掉 flag 要怎样」（必须报出来）与「把键名拼错要怎样」（必须报出来） |
| preflight | 保留 | 保留：文件级断言（`[ -f "$CONF" ] || refuse "configuration missing"` 与 `exec "$ENTRY"` 两句）+ 执行级断言（`TestPreflightRefusesBeforeTheServerStarts` 实跑脚本，断言非零退出、断言镜像入口一次都没被触达） |

> 结论没有变弱：上一轮把 compose 层的保证整个删掉、把责任全推给 preflight；本轮把 compose 层的保证**用正确的键名补回来**，同时保留 preflight 作为第二道。
> 受影响的定向用例（5 个）已跑通；**未重跑无关测试**。

### 1.6 根模块两个红灯的根因：**CRLF 让断言本身失效**（已修）

上一轮把这两个失败登记为「既有失败、与公开版范围无关」。本轮查清了根因：它们**不是**业务缺陷，而是**行尾造成的断言失效**——公开版是从 Windows 工作树拷出来的，两个被断言的文件仍是 CRLF：

| 失败用例 | 断言怎么读文件 | CRLF 下发生了什么 | 修法 |
|---|---|---|---|
| `internal/app` · `TestRedisIsDeclaredOnlyInRegisteredComposeFiles` | 逐行找顶层的 `services:`（`line != "services:"`） | 那一行实际是 `"services:\r"`，**永远匹配不上**，于是报 `no top-level "services:" line`——看起来像"compose 文件结构不对"，其实是行尾 | `compose.integrated.yml` 归一为 LF；`.gitattributes` 加 `compose.integrated.yml text eol=lf` |
| `cmd/gopherlearn` · `TestDrainWindowIsShorterThanTheContainerStopWindow` | 正则 `(?m)^const shutdownGrace = (\d+) \* time\.Second$` 读秒数 | 行尾的 `\r` 落在 `$` 之前，正则不匹配，于是报 `main.go no longer declares shutdownGrace as a whole number of seconds`——看起来像"代码改了"，其实没改 | `cmd/gopherlearn/main.go` 归一为 LF；`.gitattributes` 加 `cmd/gopherlearn/main.go text eol=lf` |

要点：

- **没有改任何断言，也没有跳过任何用例**——改的只是两个文件的行尾，以及给它们加 `.gitattributes` 规则，保证在 Linux 上重新检出还是 LF、断言照样成立。
- 这两个文件本来就随公开版一起发布，而这两个用例也本来就在公开版里，所以这是**公开版自身的缺陷**，不能按"原树也红"放过。
- 归一后根模块 `go test ./cmd/... ./internal/... ./conversation/...` **全绿**（退出码 0）。

### 1.7 面向新用户的文档重整（本轮）

| 改动 | 内容 |
|---|---|
| `README.md` 重写 | 首页改为**中文为主**，按「项目介绍 → 主要功能 → 当前版本与已知限制 → 环境要求 → 配置说明 → 本地启动 → Docker 部署 → 注册与使用 → 常见问题 → 开发与贡献 → 来源与许可证」组织。**没有编造截图**，也没有放空图片链接。原文里的历史基线开发说明、基线入口的环境变量表与冒烟检查不再堆在首页 |
| 新的 `docs/LEGACY-DEVELOPMENT-REFERENCE.md` | 承接被移出的历史基线材料（`Development status`、`Application entries`、基线入口的运行方式与 compose、基线的 `GOPHERLEARN_*` 环境变量表、provider / agent 冒烟检查）。开头写明「**仅适用于历史基线入口，不是公开版正式应用的快速开始**」，并说明这些环境变量属于基线配置面、正式入口读的是 `config.integrated.toml.local` |
| 新的 `docs/INTEGRATED-REFERENCE.md` | 承接仍与正式入口相关的技术参考：学习／测验（quiz）接口合同、三个 Go 模块与前端包的验证命令 |
| 新的 `.env.compose.integrated.example` | 整合栈专用的环境变量模板（原先 README 指向的 `.env.compose.example` 是**历史基线**的模板，会让新用户复制错文件）。只含占位符 |
| `config.integrated.example.toml` | **未改动**：字段本来就完整，核对后确认无需新增集成版模板 |
| `web/README.md` | 启动片段改成与首页一致的写法（不再用 `cd hybrid && …` 的相对路径写法），补 PowerShell 的环境变量写法，并去掉「没有任何组件/样式/文案取自任何更早项目」这类与项目归属不一致的绝对表述 |

**首页内容的核对结果**（逐条对着代码与实测记录核对，不是照抄旧文档）：

- 功能描述只写公开版**实际提供**的能力，并明确写出：无 RPC 文件处理、只支持 `.md`/`.txt`（**不支持 PDF**）、按精确文件名指代文档更可靠。
- 「已验证」与「未验证」分开写：核心链路经过小规模真实模型验收（15 次真实请求），而检索与判分的**普遍质量**、**长期稳定性**、以及四类后台行为（摘要／记忆抽取／episode／dream）**本轮没有真实执行过**，都写成了限制。
- 配置说明按真实字段分成「部署者要配的」与「普通注册用户只要的」，并写明**正式入口只读 `-config` 指向的 TOML、不读 `.env` 文件、不读环境变量里的凭据**（依据 `v2/internal/isolated/settings.go`），因此 `.env.provider.local` 属于历史基线入口、正式入口不会自动读取。
- 启动步骤每条都标注了运行目录，命令统一用 `go -C <module> …`（已在干净环境里实测：`go -C v2 run ./cmd/schemainit -config ../config.integrated.example.toml` 与 `go -C hybrid run ./cmd/gopherlearn-integrated -config ../config.integrated.example.toml` 都**正确解析到仓库根的模板文件**并按预期因模板里的占位值而拒绝启动）。前端代理固定写 `VITE_DEV_API_ORIGIN=http://127.0.0.1:18082`，并给出 PowerShell 与 Bash 两种写法。
- Docker 段落里每条命令都带 `-p gopherlearn-integrated -f compose.integrated.yml`；数据库口令一律**交互输入**，不写进命令行；`down -v` 被明确标注为「会删除数据卷，不要当作普通停止方式」；`create_host_path: false` 与 preflight 的说明按实测写成两段（本机 Windows 仍可能留下空目录，但 Redis 服务不会带缺配置启动），没有改写成「所有平台都保证不创建目录」。
- 历史基线入口 `cmd/gopherlearn` **没有**被写成默认启动入口，只在开发章节与 `docs/LEGACY-DEVELOPMENT-REFERENCE.md` 里出现。

**与文档修改相称的检查**（未重跑模型验收）：相对链接与命令里出现的仓库路径全部可解析；模板不含真实凭据；被移动文档里的 `spec/…` 链接改成了 `../spec/…`；`hybrid` 里读取 README 的那条契约测试仍然通过。

## 2. 干净环境复现命令

验证目录与日志在**仓库外**：`<工作区>\GopherLearn-release-checks\release-closeout-v1\`
（`verify\` 是公开版的一份全新拷贝：没有借用原项目的源码、本地配置、`node_modules` 或构建产物；`npm ci` 重新安装；数据库、Redis 库号与知识库目录都是独立的）。

```bash
# 0) 取一份全新的公开版拷贝，自己装依赖
cd GopherLearn-release-checks/release-closeout-v1/verify
npm --prefix web ci

# 1) 从公开模板生成配置（脚本会检查模板里是否还有未替换的占位符）
powershell -File ../tools/make-verify-config.ps1

# 2) 首次建库（公开入口拒绝在启动时改 schema）
go -C v2 run ./cmd/schemainit -config config.integrated.toml.local

# 3) 启动本地替身与正式入口
node ../tools/fake-provider.js 18999 1024     # embedding / chat 的本地非转发替身
node ../tools/smtp-capture.js 1025 ../logs/mail # 邮件捕获（邮件只进这里，不外发）
./bin/gopherlearn-integrated.exe -config config.integrated.toml.local

# 4) 完整功能验证
powershell -File ../tools/verify-functional.ps1

# 5) 缺邮件配置时的行为
powershell -File ../tools/verify-no-mail.ps1

# 6) Docker 一键部署
powershell -File ../tools/make-verify-docker-config.ps1
#    宿主直跑与容器栈不能共用同一份配置：宿主跑指向 127.0.0.1:13360/16360，
#    容器里要指向 compose 的 DNS 名与 3306/6379。两份都由**同一份公开模板**生成。
powershell -File ../tools/verify-docker.ps1
#    它的实际步骤：构建镜像 → up（无 schema 时后端拒绝启动）→
#    docker exec 建 schema 与账号 → docker compose --profile init run --rm schemainit →
#    四个容器 Up → http://127.0.0.1:8083/ 返回 200
```

## 3. 通过、失败、未验证

### 3.1 通过

| 项 | 结果 |
|---|---|
| 三模块构建（根 / `v2` / `hybrid`） | ✅ `go build ./...` |
| `go vet ./...`（三模块） | ✅ |
| `go test ./...`（`v2`） | ✅ **全绿**（修前有 4 个包失败） |
| `go test ./...`（`hybrid`） | ✅ **全绿**（修前有 3 个用例失败，根因是 PATH 上没有 POSIX shell） |
| `go test ./...`（根模块） | ✅ **全绿**（修前 2 个既有失败，根因是 CRLF，见 §1.6） |
| `web`：lint / 测试 / 生产构建 | ✅ lint 通过；**52 文件 / 1053 项测试全过**；生产包构建成功 |
| 干净环境：三模块构建 | ✅ 独立目录内通过 |
| 干净环境：从公开模板生成配置 | ✅ 无残留占位符（脚本会主动报错） |
| 干净环境：首次建库 | ✅ `schema initialized in gopherlearn_release_v1` |
| 干净环境：请求验证码 → **邮件被捕获** → 从邮件正文读出六位码 → 注册 → 登录 → `/me` | ✅ 全程走真实邮件路径，**没有直接写 Redis** |
| 干净环境：错误验证码 / 过期验证码 / 重复注册 | ✅ 2008 / 2008 / 2002 |
| 干净环境：会话创建、列表、删除 | ✅ |
| 干净环境：上传 `.md` → 进程内 worker 切片 + embedding + 索引 → `ready` | ✅ `chunk_count: 2`，`active_version: 1` |
| 干净环境：处理状态与原文行号 | ✅ `{"mode":"legacy","status":"ready"}`；按行返回原文 |
| 干净环境：跨用户隔离 | ✅ 第二个账号读文档 / 原文 / 会话全部 404，列表为空 |
| 干净环境：记忆创建、幂等重放、跨用户不可见 | ✅ |
| 干净环境：已移除的运维接口与基线集合接口 | ✅ 一律 404 |
| **compose 部署契约的判据修正（§1.5）** | ✅ 用正确键名 `bind.create_host_path: false` 把 flag 加回 5 个 `bind:` 挂载，preflight 原样保留；定向用例（5 个）全绿：flag 缺失要报出来、键名拼错要报出来、配置挂载仍只读且来自操作者文件、entrypoint 仍指向 preflight；preflight 的「缺文件即拒绝」由执行用例证明（镜像入口一次都没被触达） |
| 无 RPC 源码、无本机目录外 replace | ✅ 树内无 `kamaRPC` / `third_party/rpc`；两处 `replace` 都在树内 |
| **缺邮件配置时：启动给出提示** | ✅ `registration by e-mail captcha is unavailable: [emailConfig] has no sender address or authorization code; the server starts and every existing account keeps working` |
| **缺邮件配置时：验证码请求点名问题** | ✅ `2012 邮件服务未配置，无法发送验证码：请在配置文件的 [emailConfig] 段填写发件地址与授权码`（不再是含糊的「服务繁忙」） |
| **缺邮件配置时：已有账号照常可用** | ✅ 登录 200、`/me` 200、会话 200、文档 200、记忆 200、学习进度 200 —— 只有注册这一条路走不通 |
| **Docker 一键部署：构建镜像** | ✅ `gopherlearn-backend-integrated:1` 与 `gopherlearn-frontend-integrated:1` 都构建成功 |
| **Docker 一键部署：无 schema 时后端拒绝启动** | ✅ 后端进入重启循环，正是「入口拒绝改 schema」的设计 |
| **Docker 一键部署：带外建 schema 与账号** | ✅ `docker exec gopherlearn-integrated-mysql mysql -uroot …` 成功 |
| **Docker 一键部署：建表** | ✅ `docker compose --profile init run --rm schemainit` → `schema initialized in gopherlearn_release_stack on gopherlearn-integrated-mysql:3306` |
| **Docker 一键部署：四个容器全部 Up** | ✅ backend / frontend / mysql(healthy) / redis(healthy) |
| **Docker 一键部署：经 nginx 访问** | ✅ 前端 `http://127.0.0.1:8083/` → 200；`/api/v1/me` → 401（无令牌，正确） |
| **Docker 一键部署：本轮改动后复跑** | ✅ 又完整跑了一遍：构建镜像 → 无 schema 时后端拒绝启动 → 带外建 schema 与账号 → `--profile init run --rm schemainit` → `schema initialized in gopherlearn_release_stack on gopherlearn-integrated-mysql:3306` → 四容器 Up → 前端 200 / `/api/v1/me` 401。日志：`logs/docker-verify-final.log` |
| Docker 一键部署：容器内发验证码邮件 | ⚠️ 得到 `2013 验证码邮件发送失败` —— 这是 `net/smtp` 的既有规则：明文连接时 `PlainAuth` 只允许 localhost 主机名，而容器里用的是 `host.docker.internal`。**真实中继走 STARTTLS，不受此限**；应用侧语义（把失败说清楚）已在容器里得到验证 |

### 3.2 失败（如实保留）

**无。** 三个模块的测试现在都通过：

| 模块 | 命令 | 结果 |
|---|---|---|
| `v2` | `go -C v2 test -count=1 ./...` | ✅ 退出码 0（4 次串行运行均绿） |
| `hybrid` | `go -C hybrid test -count=1 ./...`（PATH 需含 Git 的 `usr\bin`） | ✅ 退出码 0 |
| 根模块 | `go -C <root> test -count=1 ./cmd/... ./internal/... ./conversation/...` | ✅ 退出码 0 |

> 一次**并发**运行（v2 全套与 hybrid 全套同时跑）时 v2 出现过一次未能定位到具体包的 FAIL，随后三次串行运行全部通过；那次并发是排查原因，不再复现，如实登记在此。

### 3.3 未验证

| 项 | 原因 |
|---|---|
| 检索质量、回答质量、出题与判分质量 | **已在真实模型下小规模验证**（见 §7）；只覆盖单账号、单个自建文档、一次出题与一次判分，**不构成整体质量结论** |
| 摘要行为、记忆抽取行为 | 本轮真实调用中**摘要未被触发**（触发条件不成立），**记忆抽取任务没有入队**（Agent 那轮提出了草稿，按产品规则跳过抽取）。episode 投影在停止时仍是 `pending`，因此这三类后台模型调用本轮**一次都没有发生**，其行为仍未验证 |
| 邮件送达真实邮箱 | 本轮只发到本地捕获服务；真实中继的送达、退信与 STARTTLS 行为未验证 |
| 容器内向宿主机发明文 SMTP 邮件 | `net/smtp` 的 `PlainAuth` 规则：明文连接只允许 localhost 主机名，容器里的 `host.docker.internal` 被拒。真实中继走 STARTTLS，不受影响 |
| 既有部署的升级路径 | 本轮是从零安装验证，没有在已有数据的实例上做升级验证 |
| 「缺失的 bind 源不在宿主上留下痕迹」 | Docker Desktop（Windows）上实测**不成立**：引擎照样把缺失的 bind 源建成空目录，`create_host_path: false` 被 compose 接受并保留但未被引擎执行（A/B 对照，带/不带该键行为相同）。遵守该键的平台（Linux）上由 compose 承担，本机上没有承担者。服务不会被带着缺失的配置起来，这一点由 preflight 实测保证（容器重启循环 + `configuration missing` 日志） |

## 4. 剩余发布阻断

| # | 阻断 | 状态 |
|---|---|---|
| **B-1** | ~~Docker 一键部署未在本批验证~~ | ✅ **已解决**：实测走通「构建镜像 → 无 schema 时拒绝启动 → 带外建 schema 与账号 → `--profile init run --rm schemainit` 建表 → 四个容器 Up → 经 nginx 访问 200」。过程中发现并修掉了两个真实缺口：① 镜像里没有建库工具（`scratch` 运行层无 shell、MySQL 容器又不开端口，操作者根本没法建表，后端会永远重启循环）—— 现在 `Dockerfile.backend-integrated` 一并产出 `/schemainit`，compose 增加 profile 门控的 `schemainit` 服务；② compose 里 `bind.create_host_directory: false` **被 Compose v5.1.3 拒绝解析** —— 根因是**键名拼错了**（正确键是 `create_host_path`），不是版本不兼容。已用正确键名把 flag 加回 5 个 `bind:` 挂载，preflight 原样保留，README、`tutorial/v2-production-entry/README.md` 与本报告都已更正；隔离临时路径的实测矩阵见 §1.5 |
| **B-2** | ~~根模块两个既有测试失败~~ | ✅ **已解决**：根因是 CRLF 让两处断言读不到文件内容（`compose.integrated.yml` 的 `services:` 行、`cmd/gopherlearn/main.go` 的 `shutdownGrace`），不是业务缺陷。已把两个文件归一为 LF 并用 `.gitattributes` 钉住，**断言一条未改**，根模块现已全绿。详见 §1.6 |
| **B-3** | **GPL-3.0 发布方案已准备，最终发布待负责人确认** | `LICENSE`（GPL-3.0）与 `NOTICE.md`（上游 `youngyangyang04/GopherAI` 归属、相关衍生、本版范围）已就位；**许可证的最终选择与发布仍待负责人确认** |
| **B-4** | 依赖声明按已核对范围表述 | `docs/THIRD-PARTY.md` 是本次实测生成的清单（根 26 / `v2` 74 / `hybrid` 76 个 Go 模块 + 199 个 npm 包）。**不能据此宣称所有依赖都没有许可义务**；四个 eino-ext 子模块与 volc-sdk 的许可证在父模块目录里，清单如实标注了这一点 |
| **B-5** | 公开内容卫生 | ✅ 已核两轮：**第一轮**无密钥、无 `*.local`、无 `.env`、无私钥、无账号口令文件、无密钥形状的字面量；**第二轮**又查出 5 处本机绝对路径并清掉（`docs/RELEASE-READINESS.md` 3 处、`tutorial/v2-production-entry/README.md` 2 处、`tutorial/v2-entry/README.md` 1 处），另外把 `tools/audit-licenses.ps1` 里写死的本机路径改成**从脚本自身位置推导仓库根**，这样它换一台机器也能跑（改完重跑一次，清单内容与原来逐行一致：根 26 / `v2` 74 / `hybrid` 76 个 Go 模块 + 199 个 npm 包） |

## 5. 最小真实模型验证计划

见 `docs/REAL-MODEL-VALIDATION-PLAN.md`（本轮已按"分列 + 统一口径"重算）。摘要：

- 覆盖普通聊天、Agent 与工具调用、文档问答与引用、出题与判分；
- **按类别分列**：用户请求 5 次；chat ≤ 9；embedding ≤ 4；摘要 0（触发条件 `summaryTriggerMessages=12` / `summaryTriggerTokens=6000` 在本计划的会话形态下不成立，是推导出的上界而不是"未计入按 0"）；后台 ≤ 24（记忆抽取 2 + episode 投影 5 + dream 1，按 `MaxAttempts=3` 的最坏重试计）；
- **总模型调用上限 37**（正常路径 21），输入 token 上限 450,000、输出 token 上限 48,000、墙钟 20 分钟，金额栏留空（需要负责人选定的模型与公开单价才换算得出来）；
- **本轮已获负责人授权并已执行**，实际用量与结果见 §7；计划文档里的额度仍是提案口径，真实用量按 §7 的实测值。

## 6. 保留 / 移除 / 行为变化

见 `PUBLIC-RELEASE-NOTES.md` 的完整清单。要点：

- **保留**：学习 Agent、分层记忆、测验与掌握度、用户级知识库（版本管理、权限隔离、失败与重试）、滚动摘要、情景记忆、Vue 客户端的四个页面、进程内文件处理。
- **移除**：远程执行器路径（适配器、入口、协调器、执行节点、候选模式 schema 与运维面）、`kamaRPC` 依赖、`third_party/rpc-framework-a04-r1-frozen`、内部付费实跑装置（`internal/summaryreadeval` 与 8 个 `cmd/summaryread*`）及其残留的真实运行账本。
- **行为变化**：原有 8 条 + 本轮收口新增 5 条（B-9…B-13），见 `PUBLIC-RELEASE-NOTES.md` §3 与 §3.1。
- **公开树规模**：1,040 个文件 / 10.78 MB；不含 `node_modules`、`web/dist`、`third_party`。

## 7. 真实模型功能验收（本轮执行）

**范围**：公开版（`GopherLearn-public`）的正式入口 `hybrid/cmd/gopherlearn-integrated`，本轮重新编译并运行；真实 Provider（沿用原运行项目的模型、端点与凭据）。本节只覆盖**小规模真实功能验收**，不构成整体质量、稳定性或性能结论。

### 7.1 隔离环境

| 组件 | 本次取值 | 说明 |
|---|---|---|
| 后端 | `127.0.0.1:18094`（公开版二进制，本轮从公开树编译） | 与用户原有 18082/5273 完全分开 |
| 前端 | `http://127.0.0.1:5274`（公开版 `web/`，代理指向 18094） | `VITE_DEV_API_ORIGIN=http://127.0.0.1:18094` |
| MySQL | 容器 `gopherlearn-realcheck-mysql`，`127.0.0.1:13370`，库 `gopherlearn_realcheck_v1` | 独立实例 + 独立 schema + 独立账号；用公开版 `v2/cmd/schemainit` 建表 |
| Redis | 容器 `gopherlearn-realcheck-redis`（Redis Stack），`127.0.0.1:16371`，**db 0** | 独立实例，符合"向量索引用独立实例 + db 0"的既有要求 |
| 知识库目录 | 验证目录下的 `data\knowledge` | 与原项目目录分离 |
| 账号 | 测试账号一个（注册走**本地 SMTP 捕获服务**，验证码只进本机） | 未使用任何真实用户账号、文档或数据库 |
| Provider | `qwen3.8-max`（chat）+ `qwen3.7-text-embedding`（embedding，1024 维），沿用原项目端点与凭据 | 未换模型、未降质量参数、未改 Prompt 与默认策略 |

原始日志、请求响应与用量记录在仓库外：`…\GopherLearn-release-checks\real-model-smoke-v1\run-2026-10-29-1242\`（`logs\`、`tools\`、`data\`），公开树里没有带回任何密钥、日志或私有路径。

### 7.2 五项功能的实测结果

| 项 | 结果 | 实际发生了什么 |
|---|---|---|
| **M1 普通聊天** | ✅ 通过 | 页面创建普通聊天会话并发送「请用一句话说明 TCP 三次握手解决了什么问题」，模型给出相关的一句话回答（非空、与问题相关）；**重新进入该会话后消息完整回读**。provider 侧：1 次 chat，137 输入 / 34 输出 token，1227 ms |
| **M2 文档上传与知识库问答** | ✅ 通过（第一次失败，见 §7.4） | 上传自建 `seagull7-facts.md`（953 字节，含 4 条可逐条核对的事实）；状态 `pending → ready`，`active_version=1`，**5 个切片**，1 次真实 embedding（523 输入 token，341 ms）。提问「在我上传的文件 seagull7-facts.md 里，灰度比例是多少？重试次数上限是多少次？归档保留期是多少天？」→ 答案三条全对且带引用 `[1]`：**灰度比例 12%（不是 15%）、重试上限 3 次、归档保留期 400 天**；「参考来源」面板显示文档名、文档 ID、**版本 v1**、标题路径与**按引用版本及行范围读取的原文**；`agent_steps.source_ids` 记录 5 个 chunk 引用，Redis 里切片的行范围（1–3 / 5–11 / 13–18 / 20–25 / 27–30）都落在文档实际行数内 |
| **M3 学习 Agent 与工具调用** | ✅ 通过（工具路径已验证） | 在**学习 Agent 会话**里提问，步骤面板显示 3 步：`模型规划 → 工具调用 · knowledge_search（159 ms / 333 ms）→ 模型规划`，即**真实工具往返发生并返回了结果**；工具结果带回 5 个 chunk 引用 |
| **M4 学习出题** | ✅ 通过 | 以主题「海鸥-7 灰度比例与重试策略」+ 已选 learning_agent 会话生成测验：**共 3 题**（进度 1/3），题型 `short_answer` / `short_answer` / `concept_comparison`；题目与文档事实相关（第 1 题直接考"灰度比例是多少、若误认为是 15% 怎么纠正"）；**每题都带证据引用** `seagull7-facts.md · 文档ID · v1 · L20–25 · 海鸥-7 内部交接记录 / 明确结论`；**页面上没有暴露参考答案**（页面只有题目、题型与证据定位，参考答案在服务端 `learning_questions.reference_answer`，不随页面下发） |
| **M5 作答、判分与掌握度** | ✅ 通过（恢复路径已在下一轮单独收口，见 §7.8） | 作答「灰度比例是 12%，不是 15%……并指出记录里明确写的是……」，提交判分：返回 `correctness 60 / completeness 25 / clarity 15` → `autoTotal 100 / effectiveTotal 100 / confidence 1`，理由文本与自建材料一致（认可 12%、指出 15% 错误、引用了交接记录）；进度前进到 **2/3**；**掌握度已写入** `topic_progresses`（`mastery_score 100`、`confidence 0.1667`、`valid_attempts 1`、`level insufficient`——只有 1 次有效尝试，判定为证据不足，符合"一次成功不等于整体掌握"的语义）；**刷新页面后「查询原掌握度」能完整回读**同一组数值；**「查询／恢复活动任务」在下一轮用夹具任务单独验证并通过**（§7.8） |

### 7.3 未验证与保留项

| 项 | 说明 |
|---|---|
| 学习任务刷新后回读（`查询／恢复活动任务`） | ✅ **已在下一轮单独收口并通过**（见 §7.8）：上一轮记为"未验证"，本轮用**夹具任务**（不调模型）重做，浏览器里刷新 → 重新选会话 → 点按钮，任务、进度、题目与来源行号全部恢复；当时的"点不动"是**自动化没有把会话选择送进前端状态**（按钮保持禁用，点击无效），不是产品缺陷 |
| 剩余 2 道题与学习任务收尾 | **未做**：只答了第 1 题（按"完成最小验证后立即结束"）。第 2/3 题未作答，任务未完成也未取消——该轮隔离实例连同它的库一起被删除，这个任务**没有被结算**（见 §7.7） |
| 摘要、记忆抽取、episode 投影、dream | **未验证**：本轮真实调用里**这四类后台模型调用一次都没有发生**——摘要触发条件不成立；记忆抽取任务**没有入队**（Agent 那轮提出了草稿，按产品规则跳过抽取）；episode 投影任务在停止时仍是 `pending`（`attempts=0`），未被处理 |
| 检索质量的普遍性 | 只在**一个**自建文档、**一个**账号、**一个**主题上验证过；不代表长文档、混合语言、多文档场景下的检索质量 |
| 判分质量的普遍性 | 只判了**一次**。这一次判分与材料一致，**不能据此宣称整体判分质量已被证明** |
| 真实邮箱投递 | 本轮邮件只进本机捕获服务，未向任何真实收件人发送 |
| 费用金额 | **未知**：没有可靠费率来源，本轮不做金额换算，也不编造数字 |

### 7.4 过程中的两次失败（都不在公开版代码里）

| # | 现象 | 根因 | 处理 |
|---|---|---|---|
| 1 | 文档索引 `failed`，`error_message = embedding: operation failed`（应用按设计脱敏，provider 侧其实是 200） | **本轮自建的用量记录代理的缺陷**：provider 对 embedding 返回 `content-encoding: gzip`，代理剥掉了该头却仍转发压缩字节，应用拿到无法解析的正文。代理改为**去掉请求侧的 `accept-encoding`**（让 provider 返回未压缩响应）后，索引一次通过 | 修的是**验证工具**，不是产品代码。补记：这也说明"provider 返 200"不等于"应用能消费" |
| 2 | 第一次 Agent 检索返回 0 条，答案退化为模型通用知识 | **使用方式问题**：模型把我在提问里写的**标题**《海鸥-7 内部交接记录》当成了 `document_ids` 传给检索工具，而该参数只接受**精确文件名**（`seagull7-facts.md`），过滤后命中为 0。改用精确文件名再问一次即命中，答案全对 | 不是产品缺陷；但**用户指引应写清"按文件名而不是标题指代文档"**，否则很容易误判为检索坏了 |

### 7.5 调用、token 与耗时（实测）

| 口径 | 上限 | 实际 | 说明 |
|---|---|---|---|
| 底层模型请求 | 37 | **15** | 12 次经代理记录（7 chat + 5 embedding）+ 3 次诊断用 embedding（直连 provider，不经代理） |
| ├ chat | 9 | **7** | M1 一次；Agent 两轮各两次（规划 + 收尾）；出题一次；判分一次 |
| ├ embedding | 4（计划值）/ 6（实测含失败重试） | **5 + 3 诊断 = 8** | 索引 1（失败重试 1）、Agent 查询 2、出题取证据 1；诊断 3 次 |
| ├ 摘要 | 0 | **0** | 触发条件不成立 |
| └ 后台（抽取/episode/dream） | ≤ 24 | **0** | 没有入队或未被处理，没有发生后台模型调用 |
| 用户请求次数 | 5 | **5** | M1 提问、Agent 提问 ×2、生成测验、作答 —— 与计划口径一致 |
| 工具调用次数 | ≤ 3 | **2** | `knowledge_search` 两次真实往返 |
| 输入 token | 450,000 | **≈ 8,350** | 代理记录 8,284 + 诊断调用约 66；**embedding 的输入 token 与 chat 分列统计，未重复相加** |
| 输出 token | 48,000 | **737** | 只有 chat 有输出 token |
| 墙钟 | 20 分钟 | **15 分 26 秒**（首次真实请求 13:08:24 → 最后一次 13:23:50） | 其后约 4 分钟只做界面回读核对，**没有再派发任何请求** |
| 金额 | 留空 | **未知** | 没有可靠费率，未换算、未编造 |

**上限是怎么落实的（必须说明口径）**：这三条上限**不是派发前的硬闸门**——应用本身没有"剩余额度"概念。本轮的实际控制是：调用前按批次核对代理记录的累计用量，调用后核对实际增量；接近上限时停止新派发。也就是说，它是**执行后的监控式控制**，不是预先拦截。这一点不粉饰。

**首响应与端到端耗时**（provider 侧 `latency_ms`，含网络与生成）：

| 用途 | 耗时 | 输入 / 输出 token |
|---|---|---|
| M1 普通聊天 | 1227 ms | 137 / 34 |
| Agent 第一轮（规划，含工具决策） | 1489 ms | 1053 / 48 |
| Agent 查询向量 embedding | 140 ms | 27 / — |
| Agent 第二轮（带证据作答） | 1900 ms | 188 / 60 |
| Agent 第二轮（重问，带证据） | 1665 ms + 1845 ms | 1195+2981 / 48+93 |
| 出题（embedding + 生成） | 430 ms + 8821 ms | 26 / 1534 / 375 |
| 判分 | 3338 ms | 593 / 79 |

### 7.6 结论：能否作为开发预览版发布

**可以，但必须带以下披露**：

1. **能用的**：普通聊天（含流式与回读）、文档上传→进程内索引→带引用与行号的问答、学习 Agent 的真实工具往返、出题（含证据定位）、作答判分与掌握度写入与回读——这五条链路在真实模型下都跑通了，跨用户隔离与所有权核验由既有测试覆盖。
2. **必须披露的限制**：
   - 检索**按精确文件名**指代文档更可靠；用标题指代会过滤为空（§7.4-2）。
   - **摘要、记忆抽取、episode 投影、dream 这四类后台行为本轮一次都没有真实执行过**，不能宣称它们可用。
   - 学习任务的**刷新后回读已单独验证通过**（§7.8，零模型调用）；但要注意它是**显式按钮**触发的——切换会话不会自动恢复，这一点是页面自己写明的合同。
   - 判分与检索只各验证了一次，**不构成质量结论**。
   - 真实邮箱投递、真实中继 STARTTLS、既有部署的升级路径仍未验证。
   - 费用未换算（无可靠费率）。
3. **仍影响首版的阻断**：只剩许可证与依赖声明那两项（§4 的 B-3、B-4），以及 GPL-3.0 最终发布确认；**功能层面没有阻断项**。
4. 本节结论**仅限本次小规模真实验收**，不能写成整体质量、稳定性或优化收益已经证明。

### 7.7 本次专用服务的收尾状态（据实记录）

| 项 | 停止时的实际状态 |
|---|---|
| 学习任务 | `waiting_answer`、第 2/3 题、**未完成也未取消**；`expires_at 2026-10-30 13:22:56`。**处置**：该任务从未被结算——隔离实例连同它的库一起被删除，它没有走到完成也没有走到取消（下一轮用夹具任务演示了"取消"这条路径，见 §7.8） |
| episode 投影任务 | 1 条，`pending`、`attempts=0`，**未被处理**，同样随实例删除而消失，没有结算 |
| 记忆抽取任务 | **0 条**（未入队） |
| 文档 | `seagull7-facts.md`，`ready`，`active_version=1`，5 个切片 |
| 在飞请求 | **无**（最后一次 provider 请求 13:23:50 返回 200） |
| 已做的事 | 后端、vite、用量代理、SMTP 捕获服务全部停止；隔离 MySQL / Redis 容器已删除（连同上面的未结算任务与其数据库）；公开树里的 `web/node_modules` 已清理 |
| 用户原运行环境 | **保持原样并已恢复可用**：desktop 栈（MySQL 13360 / Redis 16360）Up 且 healthy；后端 18082 对未认证请求返 401；前端 5273 返 200 且代理 `/api/v1/me` 返 401（指向 18082 正确）。原仓库文件一字未改（`git status` 与本批开始时一致，HEAD 仍 `8254c60`） |

### 7.8 学习任务恢复：单独收口（零真实模型调用）

上一轮把「刷新后回读」记成未验证。本轮只收口这一件事，**不重跑真实模型验收**，也**没有改任何产品代码**。

#### 隔离环境与夹具任务

| 项 | 本次取值 |
|---|---|
| 后端 | `127.0.0.1:18095`，公开版本轮重新编译（`hybrid/cmd/gopherlearn-integrated`） |
| 前端 | `http://127.0.0.1:5275`，公开版 `web/`，代理指向 18095 |
| MySQL / Redis | 独立容器 `13371` / `16372`（Redis 独立实例、db 0），schema `gopherlearn_resume_v1` |
| Provider | **本地 sink**：`tools/provider-sink.js` 记录任何到达它的请求并一律回 503。**sink 日志文件整个不存在**——这就是"零真实模型调用"的证据，而不是一句声明 |
| 账号 | 本轮自建的合成账号（注册验证码走本地 SMTP 捕获，不外发） |
| 夹具任务 | 用**仓库自己的 learning DAO 状态机**造出来：`CreateSession(created) → Transition(asking) → SaveQuestionsAndWait(waiting_answer)`；题目与证据取自仓库既有测试夹具（`v2/service/learning/question_test.go:testEvidence`、`service_test.go:learningQuestions`）。**没有调用模型重新出题，也没有使用任何用户已有任务** |

结果：会话 `018969d8…`（数据库模式 `agent`，HTTP 层映射为 `learning_agent`）、测验 `4b4827d0…`，`status=waiting_answer`、1/3、有效期到次日（**未过期**）。

#### 四项取证

| 检查项 | 结果 |
|---|---|
| **点击是否到达处理函数** | ✅ 到达。按钮在选中会话后**变为可用**（截图可见它由灰转深），点击后返回 200、页面随之更新 |
| **是否发出 current 请求** | ✅ 发出。网络记录：`GET /api/v1/AI/agent/learning/current` → **200**，22 ms（工具会隐去查询串，故参数另由服务端侧核对） |
| **sessionId 是否正确** | ✅ 正确。服务端 `GetActive` 的查询是 `WHERE active_key = ? AND user_id = ? AND session_id = ?`，返回 `rows:1`；直接调接口也返回同一份任务（`sessionId=018969d8…`） |
| **接口响应** | ✅ `{"status_code":1000,"status_msg":"success","learning":{…,"status":"waiting_answer","currentQuestion":1,"totalQuestions":3,"question":{…,"sources":[{…,"startLine":3,"endLine":9}]}}}`——**不含参考答案** |
| **页面是否恢复题目与进度** | ✅ 恢复：「Redis 过期策略 · waiting_answer · 4b4827d0…」「原进度 1 / 3 · 有效期 …」「Redis 有哪些过期删除策略？· short_answer」以及来源 `redis.md · doc-1 · v1 · L3–9 · Redis / 过期`。**刷新页面后重放同一序列，结果完全一致** |

#### 结论：是**验证方法**的问题，不是产品缺陷

- **后端**：直接调 `current` 返回 200 与完整任务视图——查询、参数名（前后端都是 `sessionId`，服务端 `c.Query("sessionId")`）、响应结构三者都对。
- **前端**：仓库**已有**该按钮的回归测试（`web/test/v2LearningUi.test.js` 点击 `[data-test=current]` 并断言恢复结果）。
- **上一轮为什么"点不动"**：会话选择**没有进入前端状态**（`useV2Learning.selectSession` 只在 select 的 `change` 事件里被调用），于是 `sessionId` 仍为空、按钮保持 `disabled`，点击被浏览器忽略——所以既没有请求，也没有页面变化。同一页面上的「查询原掌握度」不依赖会话选择，所以它照常工作，这正是当时的迷惑点。
- **修正后的验证方法（保留原失败记录，不掩盖）**：选中会话后**先确认按钮已变为可用**再点击，并以网络记录 + 服务端 SQL + 页面文本三者互证。
- **本轮未改任何产品代码**——没有发现需要修的产品缺陷，所以也没有"最小修复与针对性回归"可做。

#### pending 测试任务的实际处置

| 任务 | 处置 |
|---|---|
| 本轮夹具任务 `4b4827d0…` | 用**产品自己的取消路径**收尾：在页面上点「请求原取消任务」→ 服务端 `Cancel` → 终态 `status=cancelled`、`active_key=NULL`、`completed_at` 已写；页面同步显示 `cancelled` 且题目卡片消失。随后隔离实例与容器删除 |
| 上一轮真实模型轮留下的活动任务（2/3、waiting_answer）与 1 条 `pending` 的 episode 投影任务 | **没有被结算**——它们随上一轮的隔离实例一起被删除。这是据实记录，不是"已完成" |
| 本轮实例内的后台任务 | episode 投影 0 条、记忆抽取 0 条——本轮没有产生后台任务，也没有模型调用 |
