# cub-panel 知识库

> 整理自 Hermes Kanban 任务 `t_0b5e5a41` / `t_27089e4d`（检查项目 + 注册邮件验证实现）。  
> 工作区：`/host/box/env`（源码与部署脚本一体）。

---

## 1. 项目是什么

**cub-panel** 是一套开源的 **Incus/LXD 容器与 KVM 虚拟机售卖 / 管理面板**，主控 + 被控双端架构：

| 组件 | 二进制 | 职责 |
|------|--------|------|
| 主控 | `cub-panel` | Web 前台、用户控制台、管理后台、充值、网页终端代理、计费 |
| 被控 | `cub-agent` | 装在每台 Incus/LXD 宿主机，只连本机 unix socket，执行创建/网络/快照等 |

- 纯静态 Go 二进制（`CGO_ENABLED=0`），无 Node/Python 运行时依赖  
- 支持 amd64 / arm64  
- UI：中英文 + 明暗主题  
- 上游：https://github.com/wd780h/cub-panel  

---

## 2. 目录结构

```
/host/box/env/
├── bin/                      # 预编译 / 本地构建产物
│   ├── cub-panel, cub-agent
│   └── cub-*-arm64
├── src/                      # Go 源码（module: cubpanel）
│   ├── cmd/panel/main.go     # 主控入口
│   ├── cmd/agent/main.go     # 被控入口
│   └── internal/
│       ├── panel/            # Web 应用（handlers / templates / static / mail）
│       ├── agent/            # 被控 HTTP API
│       ├── store/            # DB（sqlite/postgres/mysql）+ schema
│       ├── lxd/              # Incus/LXD 客户端封装
│       └── shared/           # 协议与版本
├── data/                     # 运行时 SQLite 等（默认可写）
├── deploy/                   # install/build/docker/systemd/openrc
└── docs/
    ├── GUIDE.md / GUIDE.en.md
    └── KNOWLEDGE.md          # 本文件
```

### 主控关键文件

| 路径 | 说明 |
|------|------|
| `internal/panel/server.go` | 路由、页面 envelope、限流 |
| `internal/panel/handlers_auth.go` | 登录 / 注册 / **邮箱验证** / 改密 |
| `internal/panel/handlers_admin.go` | 节点、套餐、用户、**网站设置** |
| `internal/panel/mail.go` | SMTP 发信（STARTTLS / TLS / 明文） |
| `internal/panel/templates/` | HTML 模板（embed） |
| `internal/store/schema*.sql` | 三方言 schema |
| `internal/store/email_verify.go` | 待验证注册挑战表 CRUD |

---

## 3. 主要功能

1. **公开注册 / 登录**（可用 `CUB_PANEL_ALLOW_SIGNUP` 关闭注册）  
2. **首个注册用户自动成为管理员**  
3. **套餐 + 激活码** 开通实例；也可余额购买 / 升级  
4. **节点管理**：对接多台 agent，NAT / 独立 IPv6 / 独立公网 IPv4、KVM  
5. **实例生命周期**：创建、启停、重装、改密、快照、迁移、流量计量  
6. **网页串口控制台**（WebSocket）  
7. **充值**：易支付（支付宝/微信）、USDT 人工确认、服务端 API 入账  
8. **审计日志**  
9. **注册邮箱验证**（可选，见第 5 节）

---

## 4. 配置

### 环境变量 / 启动参数（主控）

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

安装脚本通常写到 `/opt/cub-panel/cub-panel.env`。

### 后台「网站设置」持久化项（`settings` 表）

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

## 5. 注册邮件验证实现

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

### 5.5 编译与测试

```sh
export PATH="/usr/local/go/bin:$PATH"
cd /host/box/env/src
go test ./internal/store/ ./internal/panel/ -count=1
go build -o /host/box/env/bin/cub-panel ./cmd/panel/
# 运行示例
CUB_PANEL_DB=/tmp/panel-dev.db /host/box/env/bin/cub-panel -listen 127.0.0.1:8080
```

---

## 6. 与 Hermes Kanban / multi-agent 协作

本环境中任务由 **Hermes Agent + Kanban** 分发，**Grok Build** 作为执行侧：

```
用户 (Telegram 群 / 其他渠道)
    → Hermes Gateway / Agent
    → Kanban 任务 (t_*) 指派 assignee=grok-build
    → Grok Build 在 Workspace (/host/box/env) 读改代码、跑测试
    → 完成后用 hermes send / bridge 回写 Telegram 群
    → 可选：写入 docs/ 知识库供后续 agent 续作
```

### 整合要点

1. **工作区优先**：任务体写明 `Workspace: dir @ /host/box/env`，避免改错树。  
2. **续任务模式**：`t_27089e4d` 续 `t_0b5e5a41`，在已有勘察结论上直接实现，不重复空分析。  
3. **交付三件套**（本任务要求）：
   - 可运行代码变更  
   - 群内中文总结（变更点 + 使用说明）  
   - 知识库 markdown（本文件）  
4. **Telegram 回传**：群 ID `-1004314611502`；本机可用  
   `hermes send --to telegram:-1004314611502 "..."`  
   或 Grok Telegram bridge（`/opt/data/grok-telegram-bridge`，与 Hermes bot **不同 token**，避免 409）。  
5. **多 agent 边界**：Grok Build 做具体实现；Hermes 负责任务编排与消息；不要假设 Grok Build 自带入站 Telegram。  

### 后续 agent 可接着做的方向

- 后台「发送测试邮件」按钮  
- 验证码哈希存储（当前明文存 6 位码，TTL 短 + 限次，可接受；若合规要求可改 bcrypt）  
- 登录二次验证 / 找回密码复用同一 SMTP 模块  
- 把本知识库摘要同步进 `GUIDE.md` 用户文档章节  

---

## 7. 相关任务索引

| 任务 ID | 摘要 |
|---------|------|
| `t_0b5e5a41` | 检查 `/host/box/env` 项目结构 |
| `t_27089e4d` | 续：邮件验证 + 群回 + 知识库（本交付） |
| `t_bead38a3` | 运行环境信息 |
| `t_114e316a` | `/host` 目录结构 |

---

*最后更新：2026-07-27（注册邮件验证落地）。*
