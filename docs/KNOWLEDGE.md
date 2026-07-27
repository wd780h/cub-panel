# cub-panel 知识库

> 供 Hermes / Grok Build / 运维 agent 续作使用的项目速查。  
> 路径约定：宿主机上源码树为 **`/box/env`**；Grok Build 工作区通过 bind 看到的是 **`/host/box/env`**（内容相同）。  
> 上游仓库：https://github.com/wd780h/cub-panel  

---

## 1. 项目是什么

**cub-panel** 是一套开源的 **Incus/LXD 容器与 KVM 虚拟机售卖 / 管理面板**，主控 + 被控双端架构：

| 组件 | 二进制 | 职责 |
|------|--------|------|
| 主控 | `cub-panel` | Web 前台、用户控制台、管理后台、充值、网页终端代理、计费、邮件验证 |
| 被控 | `cub-agent` | 装在每台 Incus/LXD 宿主机，只连本机 unix socket，执行创建/网络/快照/自升级等 |

- 纯静态 Go 二进制（`CGO_ENABLED=0`），无 Node/Python 运行时依赖  
- 支持 amd64 / arm64  
- UI：中英文 + 明暗主题  
- Go module：`cubpanel`，要求 **Go 1.25+**  
- 主控↔被控：**HTTPS（自签 + 证书指纹钉扎）+ HMAC 共享密钥**

---

## 2. 目录结构

```
/box/env/                    # 或 Grok 侧 /host/box/env
├── bin/                     # 预编译 / 本地构建产物
│   ├── cub-panel, cub-agent
│   ├── cub-*-arm64
│   └── SHA256SUMS
├── src/                     # Go 源码（module: cubpanel）
│   ├── cmd/panel/main.go    # 主控入口
│   ├── cmd/agent/main.go    # 被控入口
│   ├── go.mod / go.sum
│   └── internal/
│       ├── panel/           # Web 应用（handlers / templates / static / mail）
│       ├── agent/           # 被控 HTTP API（含 remote self-update）
│       ├── store/           # DB（sqlite/postgres/mysql）+ schema
│       ├── lxd/             # Incus/LXD 客户端封装
│       └── shared/          # 协议、版本号（ldflags 注入）
├── data/                    # 开发期 SQLite 等（默认可写）
├── deploy/                  # 安装 / 构建 / 服务单元
│   ├── build.sh             # 从源码编出 bin/*
│   ├── install.sh           # 统一入口（panel | agent）
│   ├── install-panel.sh
│   ├── install-agent.sh
│   ├── setup-lxd-node.sh    # 宿主机 Incus/网桥/存储池
│   ├── openrc/              # Alpine OpenRC：cub-panel / cub-agent
│   ├── systemd/             # Debian/Ubuntu unit
│   └── docker/              # Dockerfile + docker-compose（host network）
└── docs/
    ├── GUIDE.md / GUIDE.en.md   # 用户向完整指南
    └── KNOWLEDGE.md             # 本文件（agent/运维速查）
```

### 主控关键文件

| 路径 | 说明 |
|------|------|
| `internal/panel/server.go` | 路由、页面 envelope、限流 |
| `internal/panel/handlers_auth.go` | 登录 / 注册 / **邮箱验证** / 改密 |
| `internal/panel/handlers_admin.go` | 节点、套餐、用户、网站设置、**远程升级 agent** |
| `internal/panel/mail.go` | SMTP 发信（STARTTLS / TLS / 明文） |
| `internal/panel/jobs.go` | 后台任务（含过期验证挑战清理） |
| `internal/panel/templates/` | HTML 模板（embed） |
| `internal/store/schema*.sql` | 三方言 schema |
| `internal/store/email_verify.go` | 待验证注册挑战表 CRUD |
| `internal/agent/update.go` | 被控自升级（拉 release、校验、替换二进制） |
| `internal/shared/version.go` | `Version` 变量，构建时 `-ldflags` 注入 |

---

## 3. 主要功能

1. **公开注册 / 登录**（可用 `CUB_PANEL_ALLOW_SIGNUP` 关闭注册）  
2. **首个注册用户自动成为管理员**  
3. **套餐 + 激活码** 开通实例；也可余额购买 / 升级  
4. **节点管理**：对接多台 agent；NAT / 独立 IPv6 / 独立公网 IPv4、KVM  
5. **实例生命周期**：创建、启停、重装、改密、快照、迁移、流量计量  
6. **网页串口控制台**（WebSocket）  
7. **充值**：易支付（支付宝/微信）、USDT 人工确认、服务端 API 入账  
8. **审计日志**  
9. **注册邮箱验证**（可选，见第 5 节）  
10. **面板远程升级被控**（管理后台触发 agent self-update）  
11. **版本探测**：agent 过旧时面板探测会告警  

---

## 4. 配置

### 4.1 主控环境变量 / 启动参数

| 变量 | 默认 | 含义 |
|------|------|------|
| `CUB_PANEL_LISTEN` | `0.0.0.0:8080` | 监听地址 |
| `CUB_PANEL_DB` | `./data/panel.db` | SQLite 路径 |
| `CUB_PANEL_DB_DRIVER` | `sqlite` | `sqlite` \| `postgres` \| `mysql` |
| `CUB_PANEL_DB_DSN` | — | pg/mysql 连接串 |
| `CUB_PANEL_SITE` | `Cub Panel` | 默认站点名（可被后台覆盖） |
| `CUB_PANEL_SECURE_COOKIES` | `false` | HTTPS 时开 |
| `CUB_PANEL_TRUST_PROXY` | `false` | 信任 `X-Forwarded-For` |
| `CUB_PANEL_ALLOW_SIGNUP` | `true` | 是否开放注册 |

安装后通常写到 **`/opt/cub-panel/cub-panel.env`**，由 OpenRC/systemd 在启动时 `source` / `EnvironmentFile`。

### 4.2 被控环境变量（摘要）

| 变量 | 默认 | 含义 |
|------|------|------|
| `CUB_AGENT_LISTEN` | `0.0.0.0:8788` | 监听（建议内网/VPN） |
| `CUB_AGENT_SECRET` | （安装生成） | 与面板节点配置一致的 HMAC 密钥 |
| `CUB_AGENT_SOCKET` | `/var/lib/incus/unix.socket` | Incus/LXD unix socket |
| `CUB_AGENT_POOL` | `cub`（新装）/ 现场可能为 `default` | 存储池名 |
| `CUB_AGENT_IMAGE_SERVER` | images.linuxcontainers.org | simplestreams |
| `CUB_AGENT_ISO_DIR` | `/var/lib/cub-panel/isos` 等 | ISO 目录 |
| `CUB_AGENT_VERBOSE` | 空 | 非空则详细日志 |

证书：`agent-cert.pem` / `agent-key.pem`（工作目录下，常为 `/opt/cub-panel/`），指纹需钉扎进面板节点配置。

### 4.3 后台「网站设置」持久化项（`settings` 表）

- 站点：`site_name`、`announcement`、`hide_repo_link`  
- 支付：`pay_epay_*`、`pay_alipay`、`pay_wxpay`、`pay_usdt*`  
- **邮件验证**：见下表  

| key | 默认 | 说明 |
|-----|------|------|
| `mail_verify_enabled` | `0` | `1` 开启注册邮箱验证 |
| `smtp_host` | | SMTP 主机 |
| `smtp_port` | `587` | 端口 |
| `smtp_user` | | 认证用户（可空） |
| `smtp_pass` | | 密码（保存时留空=不修改） |
| `smtp_from` | | 发件人地址 |
| `smtp_tls` | `starttls` | `starttls` \| `tls` \| `none` |

---

## 5. 注册邮件验证

### 5.1 行为

- **默认关闭**：注册流程与原先一致，提交即建号并登录。  
- **开启后**：
  1. 用户在 `/register` 填邮箱与密码 → POST `/register`  
  2. 校验邮箱未占用后，生成 **6 位数字验证码** + 不透明 `token`，写入 `email_verifications`（TTL **15 分钟**）  
  3. SMTP 发送验证码邮件  
  4. 跳转 `/register/verify?token=...`，用户输入 6 位码  
  5. 校验通过后 `CreateUser`（首号仍为管理员）、建会话、删挑战记录  

其他规则：

- 最多 **8 次** 错误尝试后作废  
- 支持 **重新发送**（`POST /register/verify/resend`，限流）  
- 同一邮箱再次发起注册会覆盖旧挑战  
- 每小时清理过期挑战（`purgeEmailVerifications` job）  
- 开启但 SMTP 未配齐（缺 host/from）时，注册直接提示管理员未配置  

### 5.2 数据表

```sql
email_verifications (
  id, email, password_hash, code, token UNIQUE,
  attempts, expires_at, created_at
)
```

三方言 schema 均已加入：`schema.sql` / `schema_postgres.sql` / `schema_mysql.sql`。  
新表用 `CREATE TABLE IF NOT EXISTS`，重启即可出现，无需手工迁移。

### 5.3 代码落点

| 文件 | 改动 |
|------|------|
| `store/email_verify.go` | 挑战 CRUD + 过期清理 |
| `panel/mail.go` | SMTP 发送、配置读取 |
| `panel/handlers_auth.go` | 注册分流、验证页/提交/重发 |
| `panel/handlers_admin.go` | 设置读写 |
| `panel/server.go` | 路由 + `verify` 限流器 |
| `panel/jobs.go` | 过期清理任务 |
| `templates/admin_settings.html` | 后台开关与 SMTP 表单 |
| `templates/register.html` / `register_verify.html` | 用户侧 UI |

### 5.4 管理员操作步骤

1. 登录管理后台 → **网站设置**  
2. 填写 SMTP（主机、端口、用户、密码、发件人、加密方式）  
3. 勾选 **开启注册邮箱验证** → 保存  
4. 用真实邮箱走一遍 `/register` 自测  

常见 SMTP：

| 场景 | 端口 | 加密 |
|------|------|------|
| 多数云邮 / 企业邮 | 587 | STARTTLS |
| SMTPS | 465 | TLS/SSL |
| 内网无 TLS 中继 | 25 | none（勿对公网） |

### 5.5 本地编译与单测

```sh
export PATH="/usr/local/go/bin:$PATH"   # 或本机 go 路径
cd /host/box/env/src                    # 宿主机则为 /box/env/src
go test ./internal/store/ ./internal/panel/ -count=1
go build -o ../bin/cub-panel ./cmd/panel/
# 或一键：
# VERSION=vX.Y.Z /host/box/env/deploy/build.sh
CUB_PANEL_DB=/tmp/panel-dev.db ../bin/cub-panel -listen 127.0.0.1:8080
```

---

## 6. 部署方式

### 6.1 一键脚本（官方推荐，无源码）

主控：

```sh
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- panel
```

被控宿主机（先 Incus 再 agent）：

```sh
curl -fsSL https://raw.githubusercontent.com/wd780h/cub-panel/main/deploy/setup-lxd-node.sh | sh
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- agent
```

脚本会：

1. 按架构选取二进制（amd64 / arm64）  
2. 安装到 **`PANEL_HOME` 默认 `/opt/cub-panel`**（`bin/` + `data/` + `*.env`）  
3. 注册 **OpenRC**（Alpine）或 **systemd**（Debian/Ubuntu）服务  
4. agent 安装结束打印：**地址 + 共享密钥 + 证书指纹** → 粘贴进面板「节点」

### 6.2 从本仓库源码构建并安装

```sh
# 构建（静态链接，注入版本）
cd /box/env          # 或 /host/box/env
./deploy/build.sh
# 交叉编译例：GOARCH=arm64 ./deploy/build.sh

# 安装主控 / 被控（root）
./deploy/install-panel.sh
./deploy/install-agent.sh
# 或：./deploy/install.sh panel|agent
```

`build.sh` 要点：

- `CGO_ENABLED=0`  
- `VERSION` 默认 `git describe --tags --always`，可 `VERSION=v0.1.x ./deploy/build.sh`  
- ldflags：`-X cubpanel/internal/shared.Version=$VERSION`  
- 产出：`bin/cub-panel`、`bin/cub-agent`（跨架构带后缀）

### 6.3 服务管理

| 发行版 | 主控 | 被控 | 日志 |
|--------|------|------|------|
| Alpine OpenRC | `rc-service cub-panel start\|stop\|restart` | `rc-service cub-agent …` | `/var/log/cub-panel.log`、`/var/log/cub-agent.log` |
| systemd | `systemctl start cub-panel` | `systemctl start cub-agent` | journalctl `-u cub-panel` |

单元模板：

- `deploy/openrc/cub-panel`、`deploy/openrc/cub-agent`  
- `deploy/systemd/cub-panel.service`、`deploy/systemd/cub-agent.service`  

OpenRC 启动前会 `source /opt/cub-panel/cub-panel.env`（或 agent 对应 env）。

### 6.4 Docker

```sh
cd deploy/docker && docker compose up -d --build
```

- `network_mode: host`（方便直连各节点 agent `:8788`）  
- 默认监听 `0.0.0.0:8080`，数据卷 `cub-data` → 容器内 `/data/panel.db`  
- 详见 `deploy/docker/docker-compose.yml`

### 6.5 手工热更新（本机已有安装）

在不重跑 install 脚本时，常见步骤：

```sh
# 1) 编译
./deploy/build.sh
# 2) 备份并替换二进制
cp /opt/cub-panel/bin/cub-panel /opt/cub-panel/bin/cub-panel.bak.$(date +%Y%m%d%H%M%S)
install -m 0755 bin/cub-panel /opt/cub-panel/bin/cub-panel
# 3) 重启服务
rc-service cub-panel restart   # 或 systemctl restart cub-panel
# 4) 确认监听与日志
ss -lntp | grep 18091          # 以实际 CUB_PANEL_LISTEN 为准
tail -f /var/log/cub-panel.log
```

被控亦可由面板 **远程升级**（`agent/update.go` + 管理后台节点操作），无需逐台 scp。

### 6.6 反向代理与端口

- 生产建议：TLS 终止反代 → 主控；并设 `CUB_PANEL_SECURE_COOKIES=1`、`CUB_PANEL_TRUST_PROXY=1`  
- 主控默认可绑 `0.0.0.0:8080`；本机生产见第 7 节  
- 被控默认 `:8788` HTTPS，仅应对可信网络开放  
- 注意 **端口占用**：若 OpenRC 报 `crashed` 但端口仍在 LISTEN，可能是孤儿进程占端口，需查 `ss`/`fuser` 后再 `rc-service … start`

---

## 7. 本机（当前宿主机）运行布局

> 供后续 agent 排查时少走弯路。路径均相对**宿主机根**；Grok Build 访问时加 `/host` 前缀。

| 项 | 值 |
|----|-----|
| 源码树 | `/box/env`（Grok：`/host/box/env`） |
| 安装根 | `/opt/cub-panel` |
| 主控二进制 | `/opt/cub-panel/bin/cub-panel` |
| 被控二进制 | `/opt/cub-panel/bin/cub-agent` |
| 主控配置 | `/opt/cub-panel/cub-panel.env` |
| 被控配置 | `/opt/cub-panel/cub-agent.env` |
| SQLite | `/opt/cub-panel/data/panel.db` |
| 主控监听（本机） | `127.0.0.1:18091`（反代/隧道前） |
| 被控监听（本机） | `0.0.0.0:8788` |
| 服务 | OpenRC：`cub-panel`、`cub-agent` |
| 日志 | `/var/log/cub-panel.log`、`/var/log/cub-agent.log` |
| Git remote | `https://github.com/wd780h/cub-panel.git` |

说明：

- 邮件验证版本已合入主线（commit 主题：`optional email verification on registration`）。  
- 历史上出现过 **OpenRC 状态 crashed 但端口仍被占用**（旧进程未退 / 重复启动），以 `ss -lntp` + 日志为准，不要只看 `rc-status`。  
- 备份树：`/box/env.bak.*` 可作对照，日常以 `/box/env` 为准。  
- **勿把 `CUB_AGENT_SECRET`、SMTP 密码写进本知识库或提交 git。**

---

## 8. 与 Hermes Kanban / multi-agent 协作

```
用户 (Telegram 群 / 其他渠道)
    → Hermes Gateway / Agent
    → Kanban 任务 (t_*) 指派 assignee=grok-build
    → Grok Build 在 Workspace 读改代码、跑测试、部署
    → 完成后回写任务结果 / 可选 Telegram
    → 关键结论写入 docs/KNOWLEDGE.md 供续作
```

### 整合要点

1. **路径**：任务写 `/box/env` 时，在 Grok Build 容器内用 **`/host/box/env`**。  
2. **工作区**：Kanban scratch 可能为空目录；真正代码在 `/host/box/env`，不要只在 scratch 里瞎找。  
3. **交付习惯**：代码变更 + 中文摘要 + 必要时更新本文件。  
4. **多 agent 边界**：Grok Build 做实现与部署；Hermes 负责任务编排与消息。  

### 后续可接着做的方向

- 后台「发送测试邮件」按钮  
- 验证码哈希存储（当前明文 6 位码 + 短 TTL + 限次）  
- 登录二次验证 / 找回密码复用 SMTP 模块  
- 把邮件验证摘要同步进 `GUIDE.md` 用户文档  
- 理顺本机 OpenRC 与孤儿进程，使 `rc-status` 与真实监听一致  

---

## 9. 相关任务索引

| 任务 ID | 摘要 |
|---------|------|
| `t_0b5e5a41` | 检查 `/box/env`（`/host/box/env`）项目结构 |
| `t_27089e4d` | 注册邮件验证实现 + 知识库初版 |
| `t_0bd8034c` | 构建并部署邮件验证版本到 `/opt/cub-panel` |
| `t_eb389854` | 排查宿主机 cub-panel 服务崩溃 |
| `t_70295742` | 检查 cloudflared 与端口冲突 |
| `t_f495bd26` | 节点配置不符 / 被控升级 |
| `t_4d9c3526` | 整理 `/box/env` 并提交 GitHub |
| `t_1bf79039` | **恢复 / 补全 `docs/KNOWLEDGE.md`（本交付）** |

---

*最后更新：2026-07-27 — 恢复并扩充知识库（目录结构、邮件验证、部署方式、本机布局）。*
