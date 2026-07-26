<div align="center">

# cub-panel

**Open-source self-service panel for Incus containers & KVM virtual machines**

Master + agent architecture · static Go binaries · zero external dependencies · bilingual (中/EN) · light & dark theme

<sub>License: MIT · Go 1.25+ · amd64 / arm64 · SQLite</sub>

<sub>🤖 Developed with Claude Fable 5</sub>

[简体中文](README.md) · **English**

</div>

---

## What is it

A lightweight self-service panel to **create, sell and manage** containers and KVM
virtual machines on [Incus](https://linuxcontainers.org/incus/) (the community fork of LXD).
Tenants self-provision with an activation code or account balance; admins manage nodes,
plans, images and orders from the backend.

* **Master `cub-panel`** — storefront, tenant console, admin backend, web terminal, billing. State lives in a single SQLite file.
* **Agent `cub-agent`** — installed on each host, talks only to the local Incus unix socket, ~7 MB.

Both are `CGO_ENABLED=0` static binaries that run on Alpine / Debian / Ubuntu with no
glibc, Node or Python — **amd64 and arm64 supported**.

> ⚠️ Incus is the community fork of LXD with a fully compatible REST API; existing LXD hosts work too.

---

## ✨ Features

| | |
|---|---|
| **Instances** | Containers + **KVM VMs (beta)**, instant provisioning, elastic upgrades, web serial console |
| **Networking** | NAT / dedicated IPv6 / IPv6-only / **dedicated public IPv4** / IPv4-only / dual-stack; internal IP-pool ranges; multi-NIC; custom DNS |
| **Source IP** | NAT forwards can **preserve the real client IP (DNAT)** or hide it; reuse an existing bridge (docker0…) |
| **Quotas** | Monthly traffic caps (both / upload / download, auto-stop on overage) + up/down **bandwidth limits** (Mbps) |
| **KVM** | VNC console, **mount ISO** (boot from ISO), CPU masking, AES-NI passthrough, nested virtualization |
| **Data** | **Snapshots** (create / restore / delete), **cross-node cold migration** (auto-rollback on failure, no data loss) |
| **Images** | Pre-warm container / KVM variants per node, variant badges, checkbox image selection in plans (guarantees instant boot) |
| **Mounts** | Plan-level **host-directory binds** (`src:dst[:ro]`, containers, admin-defined) |
| **Billing** | Plan pricing + **balance provisioning** + activation codes; top-up via **Alipay / WeChat (epay) / USDT** + recharge API |
| **Admin** | Multiple admins, site settings (name / announcement), audit log, node health probing |
| **UX** | **Bilingual (中/EN)** with persistence, **light/dark theme** (follows OS + manual toggle) |
| **Security** | Master↔agent **HTTPS (10-year self-signed + fingerprint pinning) + HMAC signing**; CSRF / XSS / SQLi / rate-limiting hardening |

---

## 🚀 Quick start

No Go, no source tree — the one-click script auto-downloads the prebuilt binary for your
architecture (amd64 / arm64) and installs it as a service.

### 1. Deploy the master (panel host)

```sh
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- panel
```

Open `http://<panel-ip>:8080/register` — **the first account to register becomes admin**.
Afterwards set `CUB_PANEL_ALLOW_SIGNUP=0` to close public sign-up and restart.

### 2. Prepare a host node

```sh
# First install Incus, bridge & storage pool (KVM=1 to add QEMU; EXISTING_BRIDGE=docker0 to reuse a bridge)
curl -fsSL https://raw.githubusercontent.com/wd780h/cub-panel/main/deploy/setup-lxd-node.sh | sh
# Then install the agent — auto-loads kernel modules, generates key + 10-year cert
curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- agent
```

The script prints the **agent address + shared secret + certificate fingerprint** — copy them into the panel.

> 💾 The storage pool is auto-created as **LVM thin**: plan disk quotas are actually
> enforced and `df` inside the guest reports the plan size exactly. By default it's a
> sparse loop file (~90% of free space; override with `POOL_SIZE=100GiB`);
> **`POOL_DEVICE=/dev/sdb` hands a whole disk to the pool** (no loop, better performance;
> add `POOL_WIPE=1` to confirm wiping a non-empty disk). `POOL_DRIVER=btrfs` forces btrfs
> (quotas enforce, df shows the pool size); the **dir driver enforces nothing** — test only.

### 3. Add the node in the panel

Admin → **Nodes** → paste the three values, hit "Probe" until it shows **online**.

### 4. Create plans, issue codes / set prices

Admin → **Plans** (specs / network mode / traffic / bandwidth / price) → **Codes** to batch-generate,
or price a plan so tenants can provision from balance at `/app/deploy`.

> Full deployment, network modes, security design and payment integration: **[docs/GUIDE.en.md](docs/GUIDE.en.md)**.

---

## 🗄 Database

Installing the master **prompts for a database**: SQLite (default), PostgreSQL or MySQL.
The panel **only connects — it never installs a database server**. Choosing PG/MySQL means
you bring an existing instance and supply a DSN.

| Backend | When | Config |
|---|---|---|
| **SQLite** (default) | Single host, small/medium, zero deps | `CUB_PANEL_DB_DRIVER=sqlite` + `CUB_PANEL_DB=/opt/cub-panel/data/panel.db` |
| **PostgreSQL** | Large scale / high concurrency / multi-instance, **recommended** | `CUB_PANEL_DB_DRIVER=postgres` + `CUB_PANEL_DB_DSN=postgres://user:pass@host:5432/db?sslmode=disable` |
| **MySQL** | Existing MySQL estate | `CUB_PANEL_DB_DRIVER=mysql` + `CUB_PANEL_DB_DSN=user:pass@tcp(host:3306)/db` |

> ⚠️ **SQLite limits**: single writer only, lock contention under concurrency, no shared
> database across multiple panels, migration = copying the file. For production / scale /
> HA use PostgreSQL (or MySQL). All three backends are tested end-to-end against real
> Postgres / MySQL containers. Switching is just editing `cub-panel.env` and restarting —
> but **existing data is not auto-migrated**, so decide before you go live.

---

## 🐳 Docker (master)

The master runs in Docker using **host network mode**: it reaches agents directly on
their private addresses (port 8788), needs no port mapping, and client-IP behaviour
matches a bare-metal install.

```sh
git clone https://github.com/wd780h/cub-panel.git
cd cub-panel/deploy/docker
docker compose up -d --build
```

- Change the port via `CUB_PANEL_LISTEN` in `docker-compose.yml` (host mode → that's the
  real host port, default 8080).
- SQLite defaults to the named volume `cub-data`; to use PostgreSQL/MySQL, uncomment
  `CUB_PANEL_DB_DRIVER` / `CUB_PANEL_DB_DSN` (the image **only connects, no DB inside**).
- Multi-stage build, fully static (no cgo), Alpine runtime. **Only the master is
  containerised** — the `cub-agent` must run on each Incus host (it needs the host kernel
  and Incus socket), installed the usual way.

---

## 🌐 Network modes

A node can enable NAT, dedicated IPv6 and dedicated public IPv4 pools **simultaneously**;
plans pick a combination:

| Mode | Description |
|---|---|
| `nat` | Internal NAT + port forwarding |
| `ipv6` / `ipv6only` | NAT + dedicated IPv6 / dedicated IPv6 only |
| `ipv4` / `ipv4only` | NAT + dedicated public IPv4 / dedicated public IPv4 only |
| `ipv4v6` | Dedicated public IPv4 + dedicated IPv6 |

Dedicated addresses are allocated by the panel, configured statically in-guest and persist across reboots.
Plans with dedicated public addresses are only scheduled onto nodes that have the matching pool.

---

## 🏗 Architecture

```
                    HTTPS + HMAC signing
   ┌───────────┐  ┌───────────────┐        ┌──────────────┐
   │  browser  │─▶│ cub-panel   │──────▶│ cub-agent  │─▶ Incus (unix.socket)
   └───────────┘  │ (master/SQLite)│  ...  │ (agent/node) │─▶ container / KVM VM
                  └───────────────┘        └──────────────┘
```

* Every master↔agent request is **HMAC-SHA256 signed** with the shared secret (over
  method+path+timestamp+nonce+body), a ±90s window and nonce replay cache; transport is
  **HTTPS** with per-node **certificate fingerprint pinning**.
* The agent is **stateless** and has no database — all state lives in Incus and the master DB.

---

## 🛠 Build from source

```sh
cd deploy
sh ./build.sh                  # host arch → bin/cub-panel, bin/cub-agent
GOARCH=arm64 sh ./build.sh     # cross-compile → bin/cub-*-arm64 (install scripts auto-pick by uname -m)
```

Needs Go 1.25+. Only three dependencies: `modernc.org/sqlite` (pure Go, no cgo),
`gorilla/websocket`, `golang.org/x/crypto`.

```sh
cd src && go test ./...        # unit tests
```

---

## 📦 Layout

```
├── bin/                built static binaries (amd64 + arm64)
├── src/               full Go source
│   ├── cmd/           panel / agent entry points
│   └── internal/      panel · agent · lxd · store · shared
├── deploy/            install scripts + OpenRC/systemd service files
├── docs/GUIDE.md      detailed deployment & operations guide (中 / [EN](docs/GUIDE.en.md))
└── README.md
```

---

## 🔐 Security

Parameterized SQL (no string-building), `html/template` context-aware escaping (all frontend
writes via `textContent`, zero `innerHTML`), strict CSP, per-session CSRF token + Origin check,
bcrypt passwords, IDOR returns a uniform 404, per-IP rate limiting, open-redirect protection,
container-escape switches off by default. See "Security" in [docs/GUIDE.en.md](docs/GUIDE.en.md).

---

## ⚠️ Status

* **Containers** are verified end-to-end on a real node (provisioning, networking, snapshots, limits, billing…).
* **KVM VMs** are marked **beta**: require `/dev/kvm`; VNC / CPU masking / ISO boot logic is complete
  but not yet verified end-to-end on real KVM hardware.
* **Cross-node migration** logic is complete; verify on a test node before enabling in production.
* **Alipay / WeChat** integrate through your own **epay gateway** (no merchant secrets are bundled).

---

## 📄 License

[MIT](LICENSE) — use, modify and redistribute (including closed-source commercial use); keep the copyright notice.

---

## 🙋 Contributing

Issues and PRs welcome. English UI strings live in `src/internal/panel/i18n.go` and fall back to
Chinese when missing — translations welcome.
