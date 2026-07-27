# cub-panel 本机运维路径规范

> **强制分工（后续所有更新必须遵守）**
>
> | 职责 | 路径（宿主机） | Agent 容器内 |
> |------|----------------|--------------|
> | **源码 + 编译** | `/box/env` | `/host/box/env` |
> | **运行时部署** | `/opt/cub-panel` | `/host/opt/cub-panel` |
>
> 编译产物写在源码树 `bin/`，**最终二进制只安装到 `/opt/cub-panel`**。  
> **禁止**把 `/box/env` 当运行目录，也禁止把二进制同时拷到多处当“正式部署”。

---

## 1. 两个目录分别是什么

### `/box/env` —— 源码与编译工作区

- Git 仓库根（`src/`、`deploy/`、`docs/`）。
- 编译：`cd /box/env/deploy && sh ./build.sh` → 输出到 **`/box/env/bin/`**。
- `/box/env/bin/` 只是构建产物缓存，**不是** OpenRC/systemd 实际启动的路径。
- 可在此改代码、跑测试、打 Git 提交；**不要**在此写生产 `cub-panel.env` / 生产库。

### `/opt/cub-panel` —— 唯一运行时根目录

- 服务 `cub-panel` / `cub-agent` 的 `PANEL_HOME`（见 `deploy/openrc/*`、`deploy/systemd/*`）。
- 内容：
  - `bin/cub-panel`、`bin/cub-agent` ← **唯一正式二进制**
  - `cub-panel.env`、`cub-agent.env`
  - `data/`（SQLite 等）
  - TLS 证书指纹等运行时文件
- 更新生产：只替换这里的二进制（建议先备份），再重启服务。

---

## 2. 标准更新流程（一条路径）

推荐一键脚本（会 build → 只安装到 `PANEL_HOME` → 尝试重启）：

```sh
# 在宿主机上
cd /box/env/deploy
sh ./update-binaries.sh

# 仅安装已有 bin/、不重新编译
sh ./update-binaries.sh --install-only

# 只更新 panel 或 agent
sh ./update-binaries.sh --panel
sh ./update-binaries.sh --agent
```

Agent / Hermes 容器内（宿主机挂在 `/host`）：

```sh
cd /host/box/env/deploy
PANEL_HOME=/host/opt/cub-panel sh ./update-binaries.sh
```

等价手写步骤：

```sh
# 1) 只在源码树编译
cd /box/env/deploy && sh ./build.sh

# 2) 只部署到运行目录（先备份）
ts=$(date +%Y%m%d%H%M%S)
cp -a /opt/cub-panel/bin/cub-panel /opt/cub-panel/bin/cub-panel.bak.$ts
cp -a /opt/cub-panel/bin/cub-agent /opt/cub-panel/bin/cub-agent.bak.$ts
install -m 0755 /box/env/bin/cub-panel /opt/cub-panel/bin/cub-panel
install -m 0755 /box/env/bin/cub-agent /opt/cub-panel/bin/cub-agent

# 3) 重启官方服务（Alpine OpenRC）
rc-service cub-panel restart
rc-service cub-agent restart
```

首次完整安装仍用官方脚本（同样默认 `PANEL_HOME=/opt/cub-panel`）：

```sh
cd /box/env/deploy
sh ./install-panel.sh
sh ./install-agent.sh
```

---

## 3. 明确禁止

| 禁止行为 | 原因 |
|----------|------|
| 把正式服务指到 `/box/env/bin/*` | 源码区与运行区混用，CIFS/备份会把运行态搞乱 |
| 同时更新 `/opt/cub-panel`、`/usr/local/bin`、`/box/env/bin` 等多处“生产副本” | 无法判断实际跑的是哪份 |
| 在 `/opt/cub-panel` 里改业务源码再“就地编译”当主流程 | 源码不在这里；编译永远在 `/box/env` |
| 用 Docker demo（`cub-panel-demo`，监听 18090）覆盖生产路径约定 | Demo 使用镜像内 `/usr/local/bin/cub-panel` + `/data`，与生产无关 |

---

## 4. 本机现状速查

| 项 | 值 |
|----|-----|
| 源码 | `/box/env`（Storage Box CIFS 挂载） |
| 生产 PANEL_HOME | `/opt/cub-panel` |
| 生产监听 | panel `127.0.0.1:18091`（见 `cub-panel.env`） |
| Agent 监听 | `0.0.0.0:8788` |
| 服务管理 | Alpine OpenRC：`rc-service cub-panel|cub-agent …` |
| 旁路 Demo | 容器 `cub-panel-demo`（`/usr/local/bin/cub-panel`，`:18090`）—— **不是** `/opt/cub-panel` 生产 |

校验当前跑的是否为生产路径：

```sh
# 宿主机或 chroot /host
ps aux | grep -E '[c]ub-panel|[c]ub-agent'
# 期望生产进程：
#   /opt/cub-panel/bin/cub-panel
#   /opt/cub-panel/bin/cub-agent
```

---

## 5. 给自动化 Agent 的硬规则

1. **编译**只在 `/box/env`（容器内 `/host/box/env`）执行 `deploy/build.sh`。
2. **部署**只写 `/opt/cub-panel/bin/*`（容器内 `/host/opt/cub-panel/bin/*`）。
3. 更新任务默认走 `deploy/update-binaries.sh`，不要手写多路径 `cp`。
4. 不要“同步”生产二进制回 `/box/env` 以外的第二生产目录。
5. 重启优先 OpenRC 官方服务名 `cub-panel` / `cub-agent`；容器内无 PID 命名空间时用 `chroot /host` 或 `--pid=host` 操作宿主机。

详见脚本：`deploy/update-binaries.sh`、`deploy/build.sh`、`deploy/install-panel.sh`、`deploy/install-agent.sh`。
