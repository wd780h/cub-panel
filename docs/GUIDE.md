# cub-panel — Incus 容器面板

一套轻量的 Incus 容器售卖 / 管理面板（Incus 是 LXD 的社区分支，REST API 完全兼容，**LXD 宿主机同样可用**），**主控 + 被控**双端架构，使用激活码开机（无支付系统）。

* **主控 `cub-panel`** —— 前台页面、用户控制台、管理后台、网页终端代理。数据存 SQLite。
* **被控 `cub-agent`** —— 装在每台 Incus 宿主机上，只跟本机 Incus/LXD 的 unix socket 说话。二进制约 6.6 MB，无任何运行时依赖。

两个都是纯静态 Go 二进制（`CGO_ENABLED=0`），Alpine 上直接跑，不需要 glibc、Node、Python。

面板支持**中英文双语**（导航右上角切换，`?lang=en`／`?lang=zh`，写 cookie 持久化）与
**明暗主题**（默认跟随系统 `prefers-color-scheme`，可手动切换并记忆）。英文词条在
`internal/panel/i18n.go` 的字典里，缺失的条目自动回退中文——公共首页与租户端已翻译，
管理后台词条可按需补充。
**amd64 与 arm64 均支持**（主控、被控皆可）：`GOARCH=arm64 sh deploy/build.sh` 产出
`bin/cub-*-arm64`，安装脚本会按 `uname -m` 自动选用匹配架构的二进制。

---

## 目录结构（源码 vs 运行时）

> **本机运维硬规则**：编译只在源码树完成，**生产二进制只部署到 `/opt/cub-panel`**。  
> 详见 [OPS-PATHS.md](OPS-PATHS.md)。

**源码 / 编译工作区**（仓库根，本机常为 `/box/env`；Agent 容器内 `/host/box/env`）：

```
/box/env/                       ← 源码 + 编译（不是服务运行目录）
├── bin/                        构建产物缓存（build.sh 输出）
├── src/                        完整 Go 源码
├── deploy/
│   ├── setup-lxd-node.sh       宿主机 Incus / 存储池 / 网桥
│   ├── install-panel.sh        安装主控 → /opt/cub-panel
│   ├── install-agent.sh        安装被控 → /opt/cub-panel
│   ├── build.sh                编译到 ../bin
│   ├── update-binaries.sh      编译并只更新 /opt/cub-panel/bin
│   ├── openrc/                 Alpine 服务文件
│   └── systemd/                Debian/Ubuntu 服务文件
├── docs/OPS-PATHS.md           编译/部署路径规范
└── README.md
```

**运行时安装目标**（`PANEL_HOME`，服务实际读取的路径）：

```
/opt/cub-panel/                 ← 唯一生产部署根目录
├── bin/cub-panel               主控二进制
├── bin/cub-agent               被控二进制
├── cub-panel.env / cub-agent.env
├── data/                       SQLite 等运行时数据
└── DEPLOY_PATHS.txt            路径约定标记
```

---

## 快速开始

### 1. 部署主控

在面板机上：

```sh
cd /box/env/deploy
sh ./install-panel.sh
```

脚本会把二进制装到 `/opt/cub-panel`，生成 `/opt/cub-panel/cub-panel.env`，
并注册开机自启（Alpine 用 OpenRC，Debian 用 systemd）。

然后访问 `http://<面板机IP>:8080/register`。
**第一个注册的账号自动成为管理员。** 建完管理员后，把配置里的
`CUB_PANEL_ALLOW_SIGNUP` 改成 `0` 并重启服务即可关闭公开注册。

### 2. 准备一台容器宿主机

在要跑容器的机器上：

```sh
cd /box/env/deploy
sh ./setup-lxd-node.sh
```

脚本自动安装 **Incus**，无需 snap；已装 LXD 的机器直接复用：

| 宿主系统 | 安装来源 |
|---|---|
| Alpine 3.19+ | apk community 仓库（自动启用） |
| Debian 13+ / Ubuntu 24.04+ | 官方 apt 仓库 |
| Debian 12 | bookworm-backports（自动启用） |
| Debian 11 / Ubuntu 22.04 等 | Zabbly 上游仓库（自动添加，Incus 维护者运营） |

并默认建出 `lxdbr0`（`10.180.0.1/24`，开 NAT）和 `default` 存储池（dir 驱动）。
可用环境变量覆盖：

```sh
NAT_BRIDGE=lxdbr0 NAT_SUBNET=10.180.0.1/24 POOL_DRIVER=btrfs sh ./setup-lxd-node.sh
```

### 3. 部署被控

同一台宿主机上：

```sh
sh ./install-agent.sh
```

脚本会自动探测 LXD socket 路径、**生成一个 192 位随机共享密钥**，
并在结尾把「Agent 地址 + 共享密钥」打印出来。

### 4. 在面板里添加节点

管理后台 → **节点** → 展开「添加 / 编辑节点」，填入上一步打印的地址与密钥：

| 字段 | 示例 | 说明 |
|---|---|---|
| 节点名 | `hk-01` | 小写字母、数字、连字符 |
| Agent 地址 | `https://10.0.0.5:8788` | 默认 HTTPS，建议走内网或 WireGuard |
| 证书指纹 | `install-agent.sh` 打印的 64 位 SHA-256 | 钉扎 agent 自签证书，防中间人 |
| 共享密钥 | `install-agent.sh` 打印的值 | 必须与该机 `CUB_AGENT_SECRET` 完全一致 |
| NAT 网桥 | `lxdbr0` | 与 `setup-lxd-node.sh` 一致 |
| NAT 子网 | `10.180.0.0/24` | **网段**写法，注意不是网关地址 |
| 端口池 | `20000` – `60000` | 分配 SSH 端口与转发段的范围 |
| 每实例端口数 | `10` | 额外分配的连续端口段长度，`0` 表示不分配 |

保存后点「检测」，状态变成 **在线** 就通了。

### 5. 建套餐、发激活码 / 定价

管理后台 → **套餐** 建规格（CPU / 内存 / 硬盘 / 网络模式 / 可选镜像 / 价格 / 时长），
再到 **激活码** 批量生成。生成后的码会一次性列出，可「复制全部」。

用户拿码到 `/app/redeem` 即可开机；套餐若设置了**价格**，用户也可以在
`/app/deploy` 用**账户余额**直接开通（开通失败自动退款）。

### 6. 镜像管理

管理后台 → **镜像**：按节点查看镜像源当前提供的所有系统别名（会随发行版 EOL
变化）、查看/删除节点上已缓存的镜像，并可**预热**镜像 —— 提前拉取后用户
开通接近秒级，否则首次开通要等镜像下载几分钟。

**套餐表单里的镜像是勾选式的**：只列出各节点已预热的镜像（保证秒级交付），
想上架新系统先在这里预热，再回套餐勾选。没有任何缓存镜像时会退回手填模式。

**容器与 KVM 是两种镜像变体**：simplestreams 里同一个别名（如 `debian/12`）
既有容器 rootfs 也有 KVM 磁盘镜像，两者独立缓存。镜像管理页对开启了 KVM 的节点
会额外给出「预热 KVM」按钮，本地镜像列表也标注类型（容器 / KVM）。
KVM 套餐的镜像别名与容器相同，创建时 Incus 自动解析 VM 变体——**没预热也能开机，
只是首次要现下载 VM 镜像（几百 MB）**，预热后即时。

### 7. 网站设置与充值

管理后台 → **设置**：改**站点名称**（覆盖启动配置，免重启）、写**公告**
（显示在每页顶部横幅），并配置三种**用户自助充值**方式：

* **支付宝 / 微信** —— 对接 **易支付(epay)** 兼容网关：填你自己的 epay 站点地址、
  商户 PID 与密钥，即可同时开支付宝与微信。回调地址 `<站点>/pay/epay/notify`，
  签名校验 + 订单号幂等，回调重放不会重复入账。
* **USDT** —— 填 TRC20 收款地址与汇率（元/USDT）。用户按订单转账后提交交易哈希，
  管理后台 → **订单** 页「确认到账」即为其加余额。

用户在 `/app/recharge`（导航「充值」）选金额与方式充值。所有到账都走幂等入账，
计入余额流水。

### 8. 余额与充值 API

* 管理后台 → **用户**：每个账号可直接充值 / 扣款（正数充值、负数扣款）。
* 对接支付系统用 **充值 API**：在用户页展开「充值 API」生成 Bearer 密钥后：

```
POST /api/v1/recharge
Authorization: Bearer <密钥>
Content-Type: application/json

{"email":"user@example.com","amount_cents":1000,"ref":"order-123","note":"alipay"}
→ {"ok":true,"user_id":1,"balance_cents":1000}
```

金额单位为**分**。`ref` 是外部订单号：同一 `ref` 重复提交不会重复入账（幂等，
支付回调重试安全）。查询余额：`GET /api/v1/balance?email=...`（同样带 Bearer 头）。
所有余额变动都记录在 transactions 流水表中，用户在 `/app/deploy` 可见自己的账单。

---

## 网络模式

三种模式按套餐配置：**NAT**、**NAT + 独立 IPv6**、**仅 IPv6**。

### NAT 模式

容器挂在 NAT 网桥上，面板从节点的 `nat_subnet` 里分配一个**固定 IPv4 租约**
（写进网卡的 `ipv4.address`），这样端口转发在容器重启后依然有效。

端口转发用的是 Incus 自带的 **proxy 设备 + `nat=true`**，由 Incus 自己下发 DNAT 规则：

* `panel-ssh` —— 宿主机 `<ssh_port>` → 容器 `22`
* `panel-tcp` / `panel-udp` —— 宿主机 `<from>-<to>` → 容器同名端口段

好处是**完全不用手写 iptables**，删容器时规则自动回收，也不会和宿主机现有防火墙打架。
（LXD 宿主机上行为完全一致。）

### 独立 IPv6 模式

在 NAT 网卡之外再挂一块 `eth1` 到管理员**指定的网桥**上，
从节点配置的 `v6_cidr` 里顺序分配一个独立地址。

地址会在容器内做两件事：

1. 立刻用 `ip -6 addr add` 生效；
2. 写入开机自启，**重启不丢**：
   * Debian → `/etc/systemd/system/cub-panel-net.service` + `/etc/cub-panel-net.env`
   * Alpine → `/etc/local.d/cub-panel-net.start`（`rc-update add local default`）

宿主机侧需要你自己把承载公网 IPv6 前缀的上联口桥进 `v6_bridge`。
`setup-lxd-node.sh` 在传了 `V6_BRIDGE` 时会打印示例配置。

### 独立公网 IPv4 / 混合模式

节点可**同时**开启 NAT、独立 IPv6、独立公网 IPv4 三个池；套餐用「网络模式」挑组合：

| 模式 | eth0 | 附加 | 说明 |
|---|---|---|---|
| `nat` | 内网 NAT | — | 端口转发 |
| `ipv6` | 内网 NAT | 独立 IPv6 | |
| `ipv6only` | 独立 IPv6 | — | 无 IPv4 |
| `ipv4` | 内网 NAT | 独立公网 IPv4 | |
| `ipv4only` | 独立公网 IPv4 | — | 无 NAT |
| `ipv4v6` | 内网 NAT | 独立 v4 + 独立 v6 | 全都要 |

独立公网 IPv4 需在节点上配「独立公网 IPv4 池」（网桥 + CIDR + 网关），
地址由面板分配、容器内静态配置并持久化（与独立 IPv6 一套机制）。带独立公网
地址的套餐只会调度到已配置对应池的节点。

### 内网 IP 段限制

套餐可填「内网 IP 段限制」（如 `10.180.0.100-10.180.0.200`，须在节点 NAT 网段内，
逗号分隔多段），该套餐的 NAT 内网 IP 只从这个范围分配——方便给不同套餐划分内网段。

### NAT 源 IP（DNAT 开关）

套餐勾选「NAT 端口转发显示真实源 IP（DNAT）」：

* **勾选（默认）** —— 容器内看到访客的真实 IP（内核 DNAT / `nat=true`）；
* **取消** —— 走用户态代理，容器看到的源 IP 是宿主机（隐藏访客真实 IP）。

已在真实 NAT 实例上实测：取消勾选后 proxy 设备不带 `nat=true`。

### 仅 IPv6 模式（ipv6only）

彻底不占 IPv4：容器唯一的网卡直接挂在 IPv6 网桥上拿独立地址，
不分配 NAT 租约、SSH 端口和转发段，用户直接 `ssh root@<IPv6>` 连入。
容器内自动写入公共 IPv6 DNS。适合做纯 v6 的廉价套餐。

### 共用现有网桥（docker0 / br0 等）

不想让 Incus 建新网桥时，可以复用宿主机已有的：

```sh
EXISTING_BRIDGE=docker0 sh ./setup-lxd-node.sh
```

然后在面板节点设置里：**取消勾选「网桥由 Incus 管理」**、NAT 网关填网桥地址
（如 `172.17.0.1`）、并在**保留 IP 段**里填入宿主机/Docker 已占用的地址
（如 `172.17.0.1-172.17.0.99`），避免分配冲突。非托管网桥上实例 IP 由
容器内静态配置并持久化，端口转发自动改用用户态代理，其余体验一致。

### 容器特性开关

套餐可勾选：**TUN/TAP**（VPN 必需）、**FUSE**、**嵌套容器**、**特权容器**。
前两者以 unix-char 设备直通 `/dev/net/tun`、`/dev/fuse`；后两者映射为
`security.nesting` / `security.privileged`。
⚠ 特权容器内的 root 约等于宿主机 root，仅面向完全可信的用户开放。

### KVM 虚拟机（Beta）

套餐「实例类型」可选 **KVM 虚拟机**：与容器共用同一套网桥/NAT、固定租约、
端口转发、带宽限速、流量计量与弹性调整机制，镜像别名也相同（Incus 自动
解析 VM 变体，预热时注意 VM 镜像 300–500MB/个）。

前提与开关（默认关闭）：

1. 节点必须有 `/dev/kvm`（独服或开了嵌套虚拟化的 VPS）；
2. 节点上装 QEMU：`KVM=1 sh ./setup-lxd-node.sh`（**agent 安装脚本不会自动装任何东西**）；
3. 面板节点设置勾选「支持 KVM (Beta)」——KVM 套餐只调度到勾选的节点，
   agent 侧创建前还会再校验一次 `/dev/kvm`。

注意：VM 内存**全额预留**（不像容器可超售）；容器特性开关（tun/fuse/特权）对 VM
自动忽略。

**挂载 ISO（KVM 光驱）**：管理后台 → 镜像 → 「ISO 库」，填 URL 把 ISO 下载到节点
（存 `CUB_AGENT_ISO_DIR`，默认 `/var/lib/cub-panel/isos`）。之后在 KVM 虚拟机
详情页选择 ISO **挂载为光驱**，可勾「从 ISO 引导」（`boot.priority`）来装自定义系统；
用完卸载。挂载/卸载后需重启虚拟机生效。仅 KVM 虚拟机支持，容器不支持。

**VM 专属能力**（套餐「特性开关」里的 VM 类）：

* **VNC 控制台** —— VM 自动分配一个宿主机 VNC 端口 + 8 位密码（QEMU 内置 VNC 认证），
  实例详情页显示地址与密码，任意 VNC 客户端可连；
* **嵌套虚拟化** —— 勾「嵌套」，VM 内可再跑 KVM/容器；
* **AES-NI 透传** —— 勾「AES-NI」，向guest 暴露硬件 AES 指令；
* **CPU 伪装** —— 勾「CPU 伪装」，用通用 CPU 型号 + 隐藏 hypervisor 厂商，降低被识别为虚拟机的概率；
* **多网卡** —— 套餐「附加网卡网桥」填逗号分隔的网桥名，为实例额外挂 eth2/eth3…（容器同样适用）。

限速（上下行 Mbps）与流量计量对 VM 与容器**完全一致**（都作用在实例网卡上）。
此功能标注 Beta，尚未在真实 KVM 节点上端到端验证。

### 快照

实例详情页每台可建/还原/删除快照（**每台上限 3 个**，容器与 VM 通用，走 Incus
原生快照）。还原会把实例回滚到快照时刻。容器上已实测通过。

### 迁移服务器（跨节点）

管理后台 → 实例，选目标节点点「迁移」即可**冷迁移**（有 ≥2 个节点时才出现）：

1. 先在目标节点预留网络（IP/端口/VNC）；
2. 源实例停机 → 打包成可移植的 Incus 备份 → **经面板中转流式传到目标节点**导入
   （逐节点密钥不出主控，全程 TLS）；
3. 目标节点按其网络重建设备并启动，成功后才更新数据库并销毁源实例。

**任一步失败都会回滚**：源实例原样重启、目标残留清理，不会丢数据。迁移期间实例
状态显示「迁移中」。大磁盘/VM 迁移耗时较长，后台进行，完成情况见操作日志。
（跨节点迁移需要 ≥2 个节点，当前单节点环境未做端到端验证。）

### 真实源 IP

容器内看到的连接来源始终是**客户端真实 IP**，不是网关/NAT 地址：

* 托管网桥：转发用内核 DNAT（`nat=true` proxy 设备），天然保留源地址；
* 共用网桥：agent 自己下发 iptables DNAT 规则（按实例打标签，删机自动回收，
  **宿主机重启后 agent 会从实例记录自动重放规则**）。仅当宿主机连 iptables
  都没有时才退回用户态代理（此时源地址会显示为宿主机）。

### DNS 设置

节点设置里的「实例 DNS 服务器」（如 `1.1.1.1 8.8.8.8`）会写入该节点新实例的
`resolv.conf` 并随开机持久化；留空用系统默认。仅 IPv6 实例在未配置时自动
使用公共 IPv6 DNS。

### 流量限制

套餐可设**月流量限额（GB）**与**计费方向**：双向（上+下）、仅上行（出）、仅下行（入）。

* 面板每 5 分钟从节点采样字节计数，累计入库（容器重启不丢量）；
* 超限自动**强制停机**，状态显示「流量超限」，用户无法自行开机；
* 每 30 天自动清零并解除限制（下个周期正常开机）；升级套餐后按新套餐额度计；
* 管理后台实例页可随时「重置流量」，同时解除超限停机。

### 带宽限制

套餐可分别设**下行 / 上行带宽（Mbps）**，0 为不限。映射为 Incus 网卡的
`limits.ingress` / `limits.egress`（内核 tc 限速，删机自动回收），
NAT / IPv6 所有网卡统一生效。管理后台实例页可随时热调整；
套餐升级后自动套用新套餐的带宽。

### 弹性配置

* **用户侧**：实例详情页可升级到同网络模式、规格不小于当前且价格更高的套餐，
  只支付与原套餐的**差价**（余额扣款，失败自动退款），CPU/内存热生效，数据不动。
* **管理侧**：实例管理页可直接把任意实例调到任意 CPU/内存/磁盘（磁盘只增不减）。

---

## 系统支持

被控支持 **Debian** 与 **Alpine** 两套开机初始化流程（按镜像别名自动判断）：

| | Debian / Ubuntu | Alpine |
|---|---|---|
| 包管理 | `apt-get` | `apk` |
| 服务 | systemd | OpenRC |
| SSH | 装 `openssh-server`，开 root 密码登录 | 装 `openssh`，`rc-update add sshd` |
| IPv6 持久化 | systemd unit | `/etc/local.d` |

镜像别名走 simplestreams，默认源 `https://images.linuxcontainers.org`（Incus 社区镜像站，发行版最全；
LXD 节点可改用 `https://images.lxd.canonical.com`），
常用的有 `debian/12`、`debian/13`、`alpine/3.20`、`alpine/3.21`、`alpine/edge`。

**被控本身也能跑在 Alpine 上** —— 静态二进制 + OpenRC 服务文件都已备好。

---

## 网页终端（CLI 串行控制台）

`浏览器 ⇄ 主控 ⇄ 被控 ⇄ LXD exec` 三段 websocket 转发，用 xterm.js 渲染。

* 终端字节走 binary 帧，窗口改变走 text 帧的 JSON（`{"type":"resize",...}`）。
* 进容器时优先 `bash`，没有就退回 `sh`，Debian / Alpine 都能用。
* xterm.js 已本地化到 `static/`，**不走任何 CDN**，方便严格 CSP 与内网离线部署。
* 支持手机访问，字号会按屏宽自适应。

---

## 安全设计

面板部分逐项做了加固：

| 风险 | 处理 |
|---|---|
| **SQL 注入** | 所有语句参数化，没有一处把用户输入拼进 SQL。筛选条件用固定片段 + 占位符。 |
| **XSS** | 全部页面走 `html/template` 上下文自动转义；前端所有动态写入一律 `textContent`，**代码里没有一处 `innerHTML` 接动态数据**。 |
| **CSP** | `script-src 'self'`（无 `unsafe-inline`、无 `unsafe-eval`），`object-src 'none'`，`base-uri 'none'`，`frame-ancestors 'none'`。样式因 xterm.js 运行时注入 `<style>` 保留了 `unsafe-inline`，在脚本被锁死的前提下只剩观感风险。 |
| **CSRF** | 每会话独立 token（表单字段 + `X-CSRF-Token` 头），常量时间比对；再叠一层 Origin/Referer 同源校验；Cookie 为 `SameSite=Lax`。 |
| **会话** | 256 位随机 ID，`HttpOnly`；改密码后吊销其余全部会话；过期行定时清理。 |
| **口令** | bcrypt cost 12。登录时账号不存在也跑一次比对，避免时序区分。 |
| **越权 / IDOR** | 每个实例操作都校验属主，非属主与不存在**同样返回 404**，不泄漏存在性。 |
| **爆破** | 登录 8 次/10 分钟、注册 5 次/小时、兑换 10 次/10 分钟、操作 120 次/分钟，按 IP 限流。 |
| **开放重定向** | `?next=` 只接受站内绝对路径，`//`、`\`、CRLF 一律回落到 `/app`。 |
| **主控 ⇄ 被控** | 默认 **HTTPS**（agent 首启自动生成 10 年自签 ECDSA 证书，面板按 SHA-256 指纹钉扎校验）；再叠 HMAC-SHA256 签名覆盖 方法 + 路径 + 时间戳 + nonce + body 摘要；±90 秒时间窗；nonce 缓存防重放；常量时间比对。 |
| **命令注入** | 容器内初始化脚本的所有动态值（密码、IPv6、网关）都通过**环境变量**传入，不拼进 shell 文本。实例名、镜像别名、网桥名、IP 在被控侧再用正则复核一遍。 |
| **激活码** | 82 位熵（16 位 `A-Z2-9`）；核销用条件 UPDATE 原子占用，并发兑换不会双开；开机失败自动退回。 |
| **容器逃逸** | 显式关闭 `security.privileged` 与 `security.nesting`。 |

已实测验证（见下方"验证情况"）。

### 上线前务必做的两件事

1. **前面挂 HTTPS**（Caddy / Nginx），然后把 `CUB_PANEL_SECURE_COOKIES=1`；
   若走反代还要把 `CUB_PANEL_TRUST_PROXY=1`，否则限流和审计日志看到的都是反代 IP。
2. **被控端口不要暴露到公网**。`CUB_AGENT_SECRET` 等价于该机 root 权限，
   请用内网、WireGuard 或防火墙只放行主控 IP。

---

## 配置项

### 主控 `/opt/cub-panel/cub-panel.env`

| 变量 | 默认 | 说明 |
|---|---|---|
| `CUB_PANEL_LISTEN` | `0.0.0.0:8080` | 监听地址 |
| `CUB_PANEL_DB` | `/opt/cub-panel/data/panel.db` | SQLite 路径 |
| `CUB_PANEL_SITE` | `Incus Panel` | 站点名 |
| `CUB_PANEL_SECURE_COOKIES` | `0` | HTTPS 下置 `1` |
| `CUB_PANEL_TRUST_PROXY` | `0` | 反代后置 `1` |
| `CUB_PANEL_ALLOW_SIGNUP` | `1` | 建好管理员后建议置 `0` |

### 被控 `/opt/cub-panel/cub-agent.env`

| 变量 | 默认 | 说明 |
|---|---|---|
| `CUB_AGENT_LISTEN` | `0.0.0.0:8788` | 监听地址，建议绑内网 |
| `CUB_AGENT_SECRET` | 安装时生成 | 共享密钥，≥32 字符 |
| `CUB_AGENT_SOCKET` | 自动探测 | Incus/LXD unix socket |
| `CUB_AGENT_POOL` | `default` | 存储池 |
| `CUB_AGENT_IMAGE_SERVER` | `https://images.linuxcontainers.org` | 镜像源 |
| `CUB_AGENT_VERBOSE` | 空 | 非空则输出详细日志 |
| `CUB_AGENT_TLS` | `1` | HTTPS 开关，`0` 退回明文 HTTP |
| `CUB_AGENT_TLS_CERT` / `_KEY` | `agent-cert.pem` / `agent-key.pem` | 证书路径，缺失时自动生成（10 年自签） |

---

## 后台功能

* **概览** —— 节点在线数、实例数、容量水位、激活码余量、最近操作
* **节点** —— 增删改、连通性检测、NAT/IPv6 参数、容量上限、停用
* **套餐** —— 规格、网络模式、可选镜像、时长、上下架、排序
* **激活码** —— 批量生成（≤500/批）、绑定套餐与节点、有效期、批次筛选、一键复制
* **实例** —— 全量列表、续期（+30 天）、彻底删除（连带销毁容器）
* **用户** —— 停用/恢复、重置密码（一次性显示）、登录 IP 与时间、余额充值/扣款、**设为/取消管理员**（支持多管理员，撤管即时踢出会话）
* **日志** —— 所有敏感操作留痕（操作者、动作、详情、IP、时间）

后台自动任务：节点健康探测（2 分钟）、到期实例自动停机（5 分钟）、过期会话清理（1 小时）。

---

## 从源码重新编译

```sh
cd /host/box/env/deploy
sh ./build.sh                 # 本机架构
GOARCH=arm64 sh ./build.sh    # 交叉编译 ARM64，产物为 bin/cub-*-arm64
```

需要 Go 1.24+。依赖只有三个：`modernc.org/sqlite`（纯 Go，免 cgo）、
`gorilla/websocket`、`golang.org/x/crypto`。

运行测试：

```sh
cd /host/box/env/src && go test ./...
```

---

## 验证情况

本次交付前在容器内实测通过：

* **编译** —— `go build ./...`、`go vet ./...` 全绿，`gofmt` 无差异
* **单元测试** —— 22 项全过（IPv4/IPv6/端口分配的边界与耗尽、HMAC 签名的篡改与时钟偏移）
* **主控实跑** —— 注册→建套餐→发码→列表全流程走通，静态资源与安全响应头正常
* **安全实测** ——
  * CSRF：缺 token / 错 token / 跨站 Origin → 全部 403
  * SQL 注入：登录与兑换处的注入串 → 401 / 格式拒绝，`users` 表完好
  * XSS：`<script>` 与 `"><img onerror=>` 注入套餐名与描述 → 输出全部实体转义，无活动标签
  * 越权：普通用户访问 6 个管理路由 → 全 403；访问他人实例 → 全 404
  * 未登录访问 API → 401
  * 开放重定向 `next=//evil.com` → 回落 `/app`
  * 限流：登录第 9 次起 → 429
  * 被控 HMAC：无签名 / 错密钥 / 超时钟窗 / 重放 → 全部 401；正确签名 → 放行
* **真实节点端到端** —— 已在 Alpine 宿主机 + Incus 上实测通过完整链路：
  激活码兑换 → 拉取 `alpine/3.22` 镜像 → 容器创建并获得固定 NAT 租约 →
  proxy 设备（SSH + TCP/UDP 端口段）DNAT 规则下发 → 容器内装好 sshd →
  **用面板生成的 root 密码 SSH 登录成功**。镜像管理四个接口（本地列表 /
  远端目录 / 预热 / 删除）也在真实节点上验证通过。
  独立 IPv6 下发与网页终端尚未在真实环境验证。

---

## 常见问题

**节点检测一直"异常"**
先在面板机上 `curl -k https://<节点IP>:8788/v1/health`，401 说明网络通、签名没对上
（多半是密钥贴错或两端时钟差超过 90 秒）；连不上就是防火墙或 `CUB_AGENT_LISTEN` 绑错网卡。

**创建卡在"创建中"很久**
首次拉镜像要几分钟。看被控日志 `rc-service cub-agent status` 或
`journalctl -u cub-agent -f`；也可以在节点上先 `incus image copy images:debian/12 local:` 预热。

**容器拿不到 IPv6**
确认 `v6_bridge` 真实存在且上联口已桥入、宿主机能 ping 通该前缀内地址、
`v6_gw` 填的是网关而不是容器地址。进容器 `ip -6 addr show eth1` 看地址是否下发。

**端口转发不生效**
`incus config device show <实例名>` 看 `panel-ssh` / `panel-tcp` 是否存在；
若报 `nat=true` 相关错误，通常是 `nat_subnet` 与网桥实际网段不一致，
导致 connect 地址不在容器网段内。

**容器能拿到 IP 但完全上不了网（apk/apt 卡死）**
宿主机若同时跑 Docker，Docker 会把 iptables 的 FORWARD 默认策略设为 DROP，
把 Incus 网桥的转发全部丢弃。`setup-lxd-node.sh` 会自动插入放行规则并持久化；
手动修复：`iptables -I FORWARD -i lxdbr0 -j ACCEPT; iptables -I FORWARD -o lxdbr0 -j ACCEPT`。

**容器内 `>/dev/null` 报 Permission denied、sshd 装不上**
部分 VPS 的系统镜像把宿主机 `/dev/null` 等设备节点做成了 0660 权限，
非特权容器 bind-mount 后容器内 root 写不了。`setup-lxd-node.sh` 会检测并修复
（`chmod 666 /dev/null ...` + `/etc/local.d` 持久化）。
