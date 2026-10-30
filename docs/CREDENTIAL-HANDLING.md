# 凭据处理

这份文件写**这套栈实际怎么保管口令**，以及哪些做法不要用。它不描述别的部署形态。

一句话原则：**口令不进命令行、不进镜像、不进仓库、不进日志。**

---

## 1. 三份 `.local` 文件

正式入口的所有凭据都放在仓库根目录的三份 `.local` 文件里。后缀 `.local` 同时命中
`.gitignore` 与 `.dockerignore` 的 `*.local` 规则，所以它们不会被提交，也不会进构建上下文。

| 文件 | 里面是什么 | 备注 |
|---|---|---|
| `config.integrated.toml.local` | 模型 key、数据库口令、JWT 签名密钥、schema 名、Redis 地址、存储目录 | 后端的唯一配置；容器内挂到 `/config/integrated.toml` |
| `redis.integrated.conf.local` | `requirepass` 一行 | Redis 服务端自己的访问控制；只读挂到 `/redis-stack.conf` |
| `.env.compose.integrated.local` | MySQL 容器的账号初始化 | compose 的 `env_file` |

可提交的样例是 `config.integrated.example.toml`、`redis.integrated.conf.example`、
`.env.compose.integrated.example`。**样例里的占位串会被 preflight 具名拒绝**，所以把样例
直接复制过去当配置用，会在启动前就失败，而不是带着占位符跑起来。

---

## 2. Redis 口令

### 它在哪里，不在哪里

`deploy/redis-stack-healthcheck.sh` 在运行时从 `/redis-stack.conf` 现取值，经
`REDISCLI_AUTH` 交给探针子进程——这是 `redis-cli` 官方记录的非 argv 通道。

| | |
|---|---|
| **不出现**在命令行参数里 | argv 会出现在 `ps` 与 `docker inspect` 的输出中 |
| **不出现**在 `REDIS_ARGS` 里 | 镜像入口会把 `REDIS_ARGS` 原样追加到服务自己的命令行 |
| **不出现**在 compose 的静态环境里 | `docker compose config` 会把静态环境打印出来 |
| **不出现**在脚本自己的输出里 | 脚本只打印类别（`redis-stack health: authentication refused`），不回显内容 |
| **不出现**在 `preflight` 的任何输出里 | 同上 |
| **会出现**在探针子进程的环境里 | 探针存活期间，同容器内同 uid 的进程与 root 可以读到它 |

最后一行是这条接缝的**真实成本**，写在这里而不是藏起来：它对 Redis 客户端的同伴、对其它
容器、对宿主进程表都不可见，但对同容器内有相应权限的进程是可见的。

### 为什么不用 `redis-cli PING` 当健康检查

`PING` 只证明端口在听；`FT._LIST` 的**应答形态**才证明搜索模块活着。所以健康门检查的是
一条经过认证的 `FT._LIST`，并要求应答是一个**字符串数组**——空数组也是健康的（搜索模块
活着但还没有索引），非空字符串数组同样健康。认证被拒、模块缺失、应答不是 JSON 数组、
元素不全是字符串，都必须非零退出。

### 为什么有 preflight 这一层

镜像的 `/entrypoint.sh` 只在 `/redis-stack.conf` 存在时才设 `CONFFILE`，否则会用**没有任何
访问控制**的配置把服务起起来。所以"文件缺失"不能交给镜像入口判断——那会变成一个没有访问
控制的服务。`deploy/redis-stack-preflight.sh` 是容器的 entrypoint，检查通过前**不 exec**
镜像原入口：

- 文件缺失 / 不可读 / 为空 → 拒绝，容器起不来；
- `requirepass` 不恰好出现一次 → 拒绝（零次意味着无认证启动，两次意味着两个人都以为自己是
  口令的主人）；
- 取值是占位串、含引号或空白、短于 32 字节 → 拒绝（引号或空白会让服务端与健康检查读到
  不同的值）。

失败的形态是**容器起不来**，而不是服务起来了但没保护。

---

## 3. 数据库口令

- **交互输入**：compose 的初始化口令由操作者在终端输入，不要写进命令行——命令行会进
  shell 历史，也会进 `ps`。
- **不要用 `mysql -p<password>`**：那会进 argv。需要连容器内的 MySQL 时，用
  `docker compose -p gopherlearn-integrated -f compose.integrated.yml exec mysql sh`
  然后交互输入。
- **不要用 `docker exec --env PASSWORD=<值>`**：那会进容器的静态环境，`docker inspect` 看得到。
- **不要把口令写进 compose 文件或 `.env` 之外的地方**：`.env.compose.integrated.local` 本身
  是被 `.local` 规则排除的；把它复制到别的文件名就绕过了那条规则。
- **不要在日志里打印口令**：服务日志不能"先落盘再脱敏"。涉及口令的检查脚本只输出类别，
  不回显内容。

---

## 4. 边界

这几条是这套机制**没有**做到的，不要当成它做到了：

- Docker daemon 权限者、宿主高权限进程、容器内的 root，仍然可以读到进程内存、管道与容器
  的 tmpfs。这是信任边界，不是缺陷；读者的能力无法靠工具消除。
- tmpfs 随停止释放，但内存可能进入虚拟机的 swap 或宿主转储——**这不是物理擦除保证**。
  参考 [Docker 的 tmpfs 文档](https://docs.docker.com/engine/storage/tmpfs/)。
- 控制进程自己的内存里是否还残留口令字符串，不承诺可验证的零化。

如果你的威胁模型要求"抵抗 daemon 管理员 / 转储 / swap 持久化"，这套机制**没有**证明满足，
那是一个单独的隔离环境决策，不要靠这里的说明去声称。

---

## 5. 相关文档

- [整合栈部署参考](INTEGRATED-DEPLOYMENT.md)：启动命令、三份文件与忽略规则、每一类主张
  由谁保证、索引接缝。
- [README](../README.md) §7.5：`create_host_path: false` 与 preflight 的两层保证，以及
  Windows Docker Desktop 上"引擎仍可能建出空目录"这个实测事实。
