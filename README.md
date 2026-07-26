<div align="center">

# cub-panel

**开源的 Incus 容器 / KVM 虚拟机自助面板**
Self-service panel for selling & managing [Incus](https://linuxcontainers.org/incus/) containers and KVM virtual machines.

主控 + 被控双端架构 · 纯静态 Go 二进制 · 无外部依赖 · 中英双语 · 明暗主题

<sub>License: MIT · Go 1.25+ · amd64 / arm64 · SQLite</sub>

<sub>🤖 采用 Claude Fable 5 开发</sub>

**简体中文** · [English](README.en.md)

</div>

---

## 这是什么

一套轻量的自助面板，用来**创建、售卖和管理**基于 Incus（LXD 的社区分支）的容器与
KVM 虚拟机。用户凭激活码或账户余额自助开机，管理员在后台管节点、套餐、镜像与订单。

* **主控 `cub-panel`** —— 前台、用户控制台、管理后台、网页终端、计费。数据存 SQLite 单文件。
* **被控 `cub-agent`** —— 装在每台宿主机上，只跟本机 Incus 的 unix socket 说话，约 7 MB。

两个都是 `CGO_ENABLED=0` 的纯静态二进制，Alpine / Debian / Ubuntu 直接跑，
不需要 glibc、Node、Python；**amd64 与 arm64 均支持**。

> ⚠️ Incus 是 LXD 的社区分支，REST API 完全兼容，已装 LXD 的机器同样可用。

---

## ✨ 功能一览

| | |
|---|---|
| **实例** | 容器 + **KVM 虚拟机（Beta）**，秒级开通，弹性升级，网页串行终端 |
| **网络** | NAT / 独立 IPv6 / 仅 IPv6 / **独立公网 IPv4** / 仅 IPv4 / 双栈；内网 IP 段限制；多网卡；自定义 DNS |
| **源 IP** | NAT 端口转发可选**保留真实源 IP（DNAT）**或隐藏；支持共用现有网桥（docker0 等） |
| **限额** | 月流量限额（双向 / 仅上行 / 仅下行，超限自动停机）+ 上下行**带宽限速**（Mbps） |
| **KVM** | VNC 控制台、**挂载 ISO**（可从 ISO 引导）、CPU 伪装、AES-NI 透传、嵌套虚拟化 |
| **数据** | **快照**（建 / 还原 / 删）、**跨节点冷迁移**（失败自动回滚，不丢数据） |
| **镜像** | 按节点预热容器 / KVM 变体、类型标注、套餐勾选式选镜像（保证秒开） |
| **挂载** | 套餐级**宿主机目录挂载**（`src:dst[:ro]`，容器实例，管理员配置） |
| **计费** | 套餐定价 + **账户余额开通** + 激活码；充值支持**支付宝 / 微信（易支付）/ USDT** + 充值 API |
| **管理** | 多管理员、网站设置（站名 / 公告）、操作审计日志、节点健康探测 |
| **体验** | **中英双语**（自动记忆）、**明暗主题**（跟随系统 + 手动切换） |
| **安全** | 主控↔被控 **HTTPS（自签 10 年 + 指纹钉扎）+ HMAC 签名**；CSRF / XSS / SQLi / 限流全项加固 |

---

## 🚀 快速开始

无需 Go、无需源码 —— 一键脚本自动下载对应架构（amd64 / arm64）的预编译二进制并装成服务。

### 1. 部署主控（面板机）

```sh
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- panel
```

访问 `http://<面板机IP>:8080/register` —— **第一个注册的账号自动成为管理员**。
建好后把 `CUB_PANEL_ALLOW_SIGNUP=0` 关闭公开注册并重启。

### 2. 准备一台宿主机

```sh
# 先装 Incus、建网桥与存储池 —— 会询问 NAT 内网网段（回车用默认 10.180.0.1/24）
# 可选：KVM=1 装 QEMU；EXISTING_BRIDGE=docker0 共用现有网桥；NAT_SUBNET=… 免交互
curl -fsSL https://raw.githubusercontent.com/wd780h/cub-panel/main/deploy/setup-lxd-node.sh | sh
# 再装被控 —— 自动加载内核模块、生成密钥 + 10 年自签证书
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- agent
```

脚本结尾会打印 **Agent 地址 + 共享密钥 + 证书指纹**，照抄进面板。

> 💾 存储池自动建成 **LVM thin**：套餐磁盘限额真实生效，容器内 `df` 精确显示套餐容量。
> 默认用 loop 稀疏文件（自动取空闲磁盘的 ~90%，`POOL_SIZE=100GiB` 可指定）；
> **`POOL_DEVICE=/dev/sdb` 可把整块盘交给存储池**（免 loop、性能更好，盘上有数据需加
> `POOL_WIPE=1` 确认清盘）。`POOL_DRIVER=btrfs` 可强制 btrfs（限额生效但 df 显示池大小）；
> **dir 池不限容量**，仅建议测试。

### 3. 在面板里添加节点

管理后台 → **节点** → 填入上一步的三样东西，点「检测」变**在线**即可。

### 4. 建套餐、发激活码 / 定价

管理后台 → **套餐**（规格 / 网络模式 / 流量 / 带宽 / 价格）→ **激活码** 批量生成，
或给套餐定价让用户在 `/app/deploy` 用余额开通。

> 完整部署、网络模式、安全设计、支付对接等详见 **[docs/GUIDE.md](docs/GUIDE.md)**（[English](docs/GUIDE.en.md)）。

---

## 🗄 数据库

安装主控时脚本会**询问用数据库**：SQLite（默认）、PostgreSQL 或 MySQL。
面板端**只负责对接，不会替你安装数据库**——选 PG/MySQL 需自备已创建好的实例并填 DSN。

| 后端 | 何时用 | 配置 |
|---|---|---|
| **SQLite**（默认） | 单机、中小规模，零依赖 | `CUB_PANEL_DB_DRIVER=sqlite` + `CUB_PANEL_DB=/opt/cub-panel/data/panel.db` |
| **PostgreSQL** | 大规模 / 高并发 / 多实例，**推荐** | `CUB_PANEL_DB_DRIVER=postgres` + `CUB_PANEL_DB_DSN=postgres://user:pass@host:5432/db?sslmode=disable` |
| **MySQL** | 已有 MySQL 生态 | `CUB_PANEL_DB_DRIVER=mysql` + `CUB_PANEL_DB_DSN=user:pass@tcp(host:3306)/db` |

> ⚠️ **SQLite 局限**：只允许单写入者、高并发下会锁等待、无法多面板共享同一库、
> 迁移只能拷文件。生产 / 大规模 / 需要高可用时请用 PostgreSQL（或 MySQL）。
> 三种后端的建表与查询已用真实 Postgres / MySQL 容器端到端测试。切换只需改
> `cub-panel.env` 重启——但**已有数据不会自动迁移**，换库请在上线前决定。

---

## 🐳 Docker 部署面板端

主控可用 Docker 跑，采用 **host 网络模式**：直连各节点 agent（8788）、无需端口映射、
客户端真实 IP 行为与裸机一致。

```sh
git clone https://github.com/wd780h/cub-panel.git
cd cub-panel/deploy/docker
docker compose up -d --build
```

- 端口在 `docker-compose.yml` 的 `CUB_PANEL_LISTEN` 改（host 模式下即宿主机真实端口，默认 8080）。
- 默认 SQLite 存到命名卷 `cub-data`；接 PostgreSQL/MySQL 把注释里的
  `CUB_PANEL_DB_DRIVER` / `CUB_PANEL_DB_DSN` 打开即可（镜像**只连接、不含数据库**）。
- 镜像多阶段构建、纯静态无 cgo、基于 Alpine。**只容器化主控**——被控 `cub-agent`
  需装在每台 Incus 宿主机上（要访问本机内核与 Incus socket），照常用安装脚本。

---

## 🌐 网络模式

节点可**同时**开 NAT、独立 IPv6、独立公网 IPv4 三个池，套餐按需挑组合：

| 模式 | 说明 |
|---|---|
| `nat` | 内网 NAT + 端口转发 |
| `ipv6` / `ipv6only` | NAT + 独立 IPv6 / 仅独立 IPv6 |
| `ipv4` / `ipv4only` | NAT + 独立公网 IPv4 / 仅独立公网 IPv4 |
| `ipv4v6` | 独立公网 IPv4 + 独立 IPv6 |

独立地址由面板分配、容器内静态配置并持久化。带独立公网地址的套餐只会调度到配好对应池的节点。

---

## 🏗 架构

```
                    HTTPS + HMAC 签名
   ┌────────────┐  ┌───────────────┐        ┌──────────────┐
   │  用户浏览器 │─▶│ cub-panel   │──────▶│ cub-agent  │─▶ Incus (unix.socket)
   └────────────┘  │  (主控/SQLite) │  ...  │ (被控/每节点) │─▶ 容器 / KVM 虚拟机
                   └───────────────┘        └──────────────┘
```

* 主控↔被控每个请求都用**共享密钥 HMAC-SHA256 签名**（覆盖方法+路径+时间戳+nonce+body），
  ±90 秒时间窗 + nonce 防重放；传输走 **HTTPS**，面板按节点**证书指纹钉扎**校验。
* 被控**无状态**、无数据库，一切状态在 Incus 与主控库里。

---

## 🛠 从源码编译

```sh
cd deploy
sh ./build.sh                  # 本机架构 → bin/cub-panel, bin/cub-agent
GOARCH=arm64 sh ./build.sh     # 交叉编译 → bin/cub-*-arm64（安装脚本按 uname -m 自动选）
```

需 Go 1.25+。依赖仅三个：`modernc.org/sqlite`（纯 Go 免 cgo）、`gorilla/websocket`、`golang.org/x/crypto`。

```sh
cd src && go test ./...        # 单元测试
```

---

## 📦 目录结构

```
├── bin/                成品静态二进制（amd64 + arm64）
├── src/               完整 Go 源码
│   ├── cmd/           panel / agent 入口
│   └── internal/      panel · agent · lxd · store · shared
├── deploy/            安装脚本 + OpenRC/systemd 服务文件
├── docs/GUIDE.md      详细部署与运维文档
└── README.md
```

---

## 🔐 安全

参数化 SQL（无拼接）、`html/template` 上下文转义（前端全 `textContent`，零 `innerHTML`）、
严格 CSP、每会话 CSRF token + Origin 校验、bcrypt 口令、IDOR 统一返回 404、按 IP 限流、
开放重定向防护、容器逃逸开关默认关闭。详见 [docs/GUIDE.md](docs/GUIDE.md) 的「安全设计」。

---

## ⚠️ 状态说明

* **容器**功能已在真实节点端到端验证（开通、网络、快照、限流、计费等）。
* **KVM 虚拟机**标注 **Beta**：需节点有 `/dev/kvm`；VNC / CPU 伪装 / ISO 引导等逻辑完整但
  未在真实 KVM 环境端到端验证。
* **跨节点迁移**逻辑完整，建议先在测试节点验证再对生产开放。
* **支付宝 / 微信**通过你自己的**易支付(epay)网关**对接（开源项目不内置任何商户密钥）。

---

## 📄 License

[MIT](LICENSE) —— 随便用、改、闭源商用都行，保留版权声明即可。

---

## 🙋 贡献

欢迎 Issue 与 PR。UI 英文词条在 `src/internal/panel/i18n.go`，缺失自动回退中文，欢迎补全。
