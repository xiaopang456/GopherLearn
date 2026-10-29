# 无 Windows 秘密文件的临时 MySQL 验收候选

基底a948409c29f27fd83c18232b23a485706804f0d7；migration/v2-direct-copy，开始时HEAD一致、工作树干净。本轮只读核对并新增方案，没有实现工具、生成密码、创建容器/实验目录、设置ACL、拨号或运行I0/M0。当前方案需负责人确认，真实执行另行授权。

## 现有链路与必要性

| 路径 | 当前做法 | 是否MySQL必需 |
| --- | --- | --- |
| first_batch.DockerBackend.__init__/preflight/create | secrets.token_hex(32)生成root密码；Windows临时目录权限处理；写root-secret；只读bind到/run/gate-root；MYSQL_ROOT_PASSWORD_FILE指定它 | 非空随机密码及认证必需；Windows文件、ACL及bind是工具选择 |
| diagnose.Backend前置与create | prepare_private_acl先设置Owner/DACL，再写root-secret；同样bind及_FILE；还写含测试密码driver.toml | ACL失败阻断工具，但与原V2业务及MySQL自身认证无关 |
| 官方镜像入口/ready/sql | 镜像原入口初始化自身mysql系统库；容器内mysql客户端读取root文件，SQL经exec stdin；查VERSION/server_uuid | 原镜像初始化、身份核验和认证保留；容器内成功不能证明宿主发布端口连通 |
| first_batch.prepare_schema/run_gate | I0/M0各自随机schema和最小权限账号；I0将密码写i0.toml供entry.Load；M0把DSN放专用测试子进程环境 | schema隔离、原DAO/断言与连接语义保留；宿主含密码文件不是驱动必需，现有I0门控确实依赖配置路径 |
| cleanup | 回收已拥有进程，复验镜像/容器ID/标签/网络/端口/挂载后只删确切容器和网络，再删拥有的秘密文件 | 精确身份/回收必需；宿主秘密文件是当前工具附加对象 |

依据：tutorial/v2-real-gates-plan/first_batch.py的preflight/create/sql/prepare_schema/run_gate/cleanup；tutorial/v2-host-port-tool/diagnose.py的相应阶段；hybrid/real_gates_init_test.go的entry.Load→validateInitializer→config.Use→originalMySQL.OpenConfigured→UUID/空schema/InitializeSchema/结构断言；v2/config/explicit.go及锁定TOML DecodeFile的os.Open路径。不将容器内readiness当作宿主就绪。

## 单一推荐链路（未实施）

推荐“控制进程内存随机凭据 → Docker exec stdin → 容器私有tmpfs”，保留同一镜像原入口完成MySQL初始化；测试连接仍发生在Windows宿主，不迁到容器内以掩盖发布端口问题。

1. 新批准批次准入/固定HEAD/完整哈希、指定镜像核验通过后，控制进程以原secrets.token_hex(32)各生成一次root和每个schema的独立测试密码，只在内存持有。当前不生成；不使用空密码、固定密码或MYSQL_RANDOM_ROOT_PASSWORD（它可能打印密码）。无root远程权限扩大或账户权限额外提升。
2. 创建同一个独占容器，原/var/lib/mysql的1GiB tmpfs保留；去掉Windows root-secret bind，新增小型/run/gate-secrets tmpfs（拟64KiB、0700），不共享volume。启动一个固定、无秘密参数的薄bootstrap：创建0600 FIFO，在有界期限内只等待一次秘密输入；不启动无密码网络服务。经一次docker exec -i的stdin注入严格64hex密码，在容器tmpfs形成0600 root文件，先验证输入与写入、再原子提交就绪标识/释放FIFO。bootstrap设置的_FILE只含容器路径，然后exec同镜像/usr/local/bin/docker-entrypoint.sh mysqld。超时/注入失败立即停止并清理，不重复注入、不回退空密码。
3. bootstrap只负责传递，不替代镜像出厂初始化。原官方入口处理_FILE及gosu的读取顺序、root/mysql读取权限必须在下一轮按冻结镜像内容核对；不能凭当前在线master源码声称该digest已验证。文件只供实际需要的入口身份读取，不为解决读取问题chmod成全员可读。若需要超出0600/精确所有者设置则先报告阻塞，不扩大权限。
4. 容器内管理查询优先由同一tmpfs的0600客户端option文件认证，mysql命令行只出现文件路径，SQL继续走stdin；不以mysql -p<密码>或docker exec --env PASSWORD=<值>传值。账号创建SQL保持现有schema和权限，密码仅在stdin，不落SQL脚本。镜像内部可能把_FILE读成动态MYSQL_ROOT_PASSWORD环境，这是需要承认的内部暴露边界，不保证容器内进程环境完全无秘密。
5. 宿主控制进程本来就生成并持有schema密码，无需从容器打印取回。I0公开、无秘密配置文件仍由原entry.Load解析；需要一个**待批准的仅门控输入接缝**：原解析成功后、validateInitializer/config.Use/原连接前，从父进程stdin取唯一密码并赋到解析配置的同一密码字段，禁止改变其他配置。完整验证和原初始化/表结构断言不改。M0继续使用既有专用子进程DSN环境接口（不修改父环境、不写env-file）；不得dump环境。DSN仍相同charset=utf8mb4&parseTime=true&loc=Local，不加clientFoundRows或改认证驱动。

I0当前仅接受含密码配置路径，**原封不动的执行器不能完成整个无宿主秘密文件链**。负责人若不允许这个窄门控输入接缝，本推荐在I0处阻塞，不能自行改Load、改业务配置加载、切换测试到容器、跳过I0或恢复Windows秘密文件。stdin接缝实现及离线验证属于方案批准后的下一阶段。I0配置解析本身与秘密注入分离的验收差异需单列，不伪称含密码文件流程逐字未变。原测试断言、V2驱动、连接选项和业务实现保持。

## 必要参数差异（待确认）

| 项目 | 保持/变化 |
| --- | --- |
| 镜像 | 保持mysql:8.0.46@sha256:62fb722c78b24245ddff1796a0fcee4a49cc5b87e0aaaf20c92d1da9e0a2497b、linux/amd64；既有manifest/config关系守卫；仅预加载、--pull=never |
| 网络/发布 | --internal独占网络、127.0.0.1:<新授权唯一端口>:3306保持；不开放33060、不换绑定、不操作防火墙；占用/未知即停 |
| 数据挂载 | /var/lib/mysql 1GiB tmpfs保持、无业务卷 |
| 秘密挂载 | 去掉/run/gate-root Windows bind；新增/run/gate-secrets私有小tmpfs；两处Mounts/HostConfig守卫需同步登记准确新形状，不简单关闭检查 |
| 入口 | Docker入口覆写为已封存的无秘密bootstrap，最后exec原镜像入口/原mysqld参数；这是明确变化，需确认并离线验收 |
| 配置/凭据 | 去掉root-secret/i0.toml/driver.toml的宿主秘密写入；空Docker config目录仍可保留但不含凭据、不要求Windows ACL；I0加门控stdin，M0仅子进程环境 |

网络参数不改不代表连接恢复；历史connection_refused仍未知。若该路径继续拒绝，按原停止条件终止，不临时改internal/端口/DSN。旧主机端口诊断程序不随本方案自动接通，未来如需使用也必须单独批准相同秘密输入改动。

## 安全边界及成立条件

- 密码不在argv、Docker创建Config.Env、label、entrypoint文本、参数JSON、报告/Git或任何日志中。Docker inspect仅固定ID/镜像/标签/网络/端口/tmpfs配置，原完整inspect不公开。_FILE仅路径不是密码；stdin/容器文件不进入Config.Env。不把“inspect无明文”说成daemon管理员不可见。
- Docker daemon权限者、宿主高权限进程、容器root、调试器仍可读进程内存/管道/容器tmpfs；M0子进程环境同样可被具有相应权限者读取。该信任边界需负责人接受；不向其他用户开放、不提权、不增加--privileged、cap-add或共享服务。读取者能力无法靠本工具消除。
- 关闭shell xtrace/历史和SQL客户端tee/history；拒绝随机密码输出模式。服务日志不能先落盘再脱敏：拟log-driver=none（原参数变化须批准），控制进程/exec输出仅内存闭集过滤，不保存任意原文。镜像内部server general/slow/error/binlog及初始化输出是否可能保存密码，必须核对冻结入口和默认配置；不能安全确认则停止工具实现/执行准入，不以日志权限替代“不落密码日志”。不得为此改变认证、业务或V2 DSN。
- tmpfs随停止释放，但Linux内存可能进入VM swap/宿主转储，并非物理擦除保证；控制进程字符串/内存也不承诺可验证零化。Docker文档说明tmpfs临时性及swap边界：[tmpfs mounts](https://docs.docker.com/engine/storage/tmpfs/)。若要求抵抗daemon管理员/转储或保证无swap持久化，本方案未证明满足，需单独隔离环境决策，不能声称“绝对只在RAM”。
- 官方当前入口的_FILE会变成动态环境、随机密码选项会输出密码；作为风险核对依据：[官方entrypoint源码](https://github.com/docker-library/mysql/blob/master/docker-entrypoint.sh)。不是冻结8.0.46镜像内容验收，下一轮需核对精确内容后再封存bootstrap。

## 结束与失败清理

沿用原进程/整批/清理截止和失败即停：先终止回收拥有的exec/bootstrap/test进程，再复验本批准确容器ID、标签、镜像、network、loopback端口及新的tmpfs/入口身份。删除确切容器（停止后两个tmpfs释放），确认不存在，再确认网络没有外来连接后删除确切network。无共享schema、volume、通配DROP或system prune；I0/M0独立schema随整个独占实例销毁。

容器内root及option文件不复制到宿主；不做密码读回取证。父进程释放引用并退出，关闭stdin管道；仅删除本批无秘密配置/空Docker目录。身份不确定、拥有进程未回收或删除失败，记录精确资源ID及秘密可能仍在该资源中的状态，停止并报告负责人，不销毁未知资源、不承诺已擦除。日志/证据只保留闭集阶段、退出码、tests真正run/pass和清理结果，Skip不是通过。

## 阶段与待授权操作

**现在只请确认方案方向**：容器stdin→tmpfs、固定bootstrap/日志驱动参数变化、I0窄门控密码输入接缝，以及上述privileged observer/环境/swap边界。当前方案是有条件可实现候选，未获授权/未验证可运行。

确认后下一轮仅工具实现和离线验证：复用镜像/资源身份、SQL创建权限、原测试和审计停止/清理；补秘密不进入argv/inspect/日志、注入一次及超时/部分创建、准确新挂载、I0完整断言和M0原接口测试。不扩展通用框架、不改业务、不执行真实Docker。

再复核新固定HEAD/完整哈希后，另行授权：只核验指定预加载镜像内容、创建独占临时network/container/两个tmpfs、一次内存随机凭据与stdin注入、容器认证就绪和身份检查、指定宿主连接、I0/M0各独立随机schema、只执行原确切两个测试、精确清理。首次真实范围也可只授权bootstrap/认证链，不含I0/M0；不能把确认方案当作这些操作授权。Redis/SMTP/Provider/业务库/迁移/默认入口/merge/push均不包含。

本轮只读核验38项原封存文件及历史46bb证据保持；不跑会覆盖历史日志的验证器、不运行门控测试。附verification.json，提交后停止。
