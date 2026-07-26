# cub-panel — Deployment & Operations Guide

[简体中文](GUIDE.md) · **English**

A lightweight self-service panel for selling / managing Incus containers and KVM VMs.
**Master + agent** architecture, provisioning via activation codes or account balance.

* **Master `cub-panel`** — storefront, tenant console, admin backend, web-terminal proxy. State in SQLite.
* **Agent `cub-agent`** — installed on every Incus host, talks only to the local Incus/LXD unix socket. ~7 MB, no runtime deps.

Both are static Go binaries (`CGO_ENABLED=0`); run on Alpine/Debian/Ubuntu, **amd64 & arm64**.
The panel is **bilingual (中/EN)** with automatic **light/dark** theme.

---

## Layout

```
/opt/cub-panel/                ← install target
├── bin/
│   ├── cub-panel              master binary
│   └── cub-agent              agent binary
├── src/                         full Go source
├── data/                        SQLite database (created at runtime)
├── deploy/
│   ├── setup-lxd-node.sh        install Incus, storage pool, bridge on a host
│   ├── install-panel.sh         install the master + service
│   ├── install-agent.sh         install the agent + service (auto-generates key & cert)
│   ├── build.sh                 recompile from source
│   ├── openrc/                  Alpine service files
│   └── systemd/                 Debian/Ubuntu service files
└── README.md
```

---

## Quick start

### 1. Deploy the master

On the panel host:

```sh
cd /opt/cub-panel/deploy && sh ./install-panel.sh
```

Installs the binary to `/opt/cub-panel`, writes `/opt/cub-panel/cub-panel.env`, and
registers auto-start (OpenRC on Alpine, systemd on Debian). Then open
`http://<panel-ip>:8080/register` — **the first account to register becomes admin**. After
that, set `CUB_PANEL_ALLOW_SIGNUP=0` and restart to close public sign-up.

### 2. Prepare a host node

```sh
cd /opt/cub-panel/deploy && sh ./setup-lxd-node.sh
```

Automatically installs **Incus** (no snap needed); reuses an existing LXD install:

| Host OS | Source |
|---|---|
| Alpine 3.19+ | apk community repo (auto-enabled) |
| Debian 13+ / Ubuntu 24.04+ | official apt repo |
| Debian 12 | bookworm-backports (auto-enabled) |
| Debian 11 / Ubuntu 22.04 etc. | Zabbly upstream repo (auto-added) |

Creates `lxdbr0` (`10.180.0.1/24`, NAT on) and a `default` storage pool (dir driver). Override:

```sh
NAT_BRIDGE=lxdbr0 NAT_SUBNET=10.180.0.1/24 POOL_DRIVER=btrfs sh ./setup-lxd-node.sh
KVM=1 sh ./setup-lxd-node.sh                 # also install QEMU for KVM guests
EXISTING_BRIDGE=docker0 sh ./setup-lxd-node.sh   # reuse an existing bridge
```

### 3. Deploy the agent

On the same host:

```sh
sh ./install-agent.sh
```

Auto-detects the Incus socket, **generates a 192-bit shared secret and a 10-year self-signed
certificate**, and prints the **agent address + shared secret + certificate fingerprint** at the end.

### 4. Add the node in the panel

Admin → **Nodes** → expand "Add / Edit node", fill in what the script printed:

| Field | Example | Notes |
|---|---|---|
| Name | `hk-01` | lowercase letters, digits, hyphens |
| Agent address | `https://10.0.0.5:8788` | default HTTPS; use a private/VPN network |
| Certificate fingerprint | printed 64-hex SHA-256 | pins the agent's self-signed cert |
| Shared secret | printed value | must match the agent's `CUB_AGENT_SECRET` |
| NAT bridge | `lxdbr0` | matches `setup-lxd-node.sh` |
| NAT subnet | `10.180.0.0/24` | **network** notation, not the gateway |
| Port pool | `20000` – `60000` | range for SSH ports & forwards |

Save, hit "Probe" — **online** means it's connected.

### 5. Plans, codes & pricing

Admin → **Plans** (CPU / RAM / disk / network mode / images / traffic / bandwidth / price / term),
then **Codes** to batch-generate. Codes are shown once with "copy all". Tenants redeem at
`/app/deploy`; if a plan has a **price**, tenants can also provision from **account balance**
(auto-refunded if provisioning fails).

### 6. Image management

Admin → **Images**: per node, view the aliases the image server currently offers, view/delete
cached images, and **pre-warm** — pre-fetching makes provisioning near-instant instead of a
multi-minute first-boot download.

**Containers and KVM are two image variants**: under one alias (e.g. `debian/12`) simplestreams
carries both a container rootfs and a KVM disk image, cached independently. The images page shows
both a "pre-warm container" and "pre-warm KVM" button and labels the local variant. Plan image
checkboxes badge each alias with the cached variants (container / KVM).

### 7. Site settings & top-up

Admin → **Settings**: change the **site name** (overrides the startup config, no restart), write an
**announcement** (banner on every page), and configure three self-service top-up methods:

* **Alipay / WeChat** — via an **epay-compatible gateway**: plug in your own epay site URL, merchant
  PID and key. Callback URL is `<site>/pay/epay/notify`; signature-verified and idempotent on order
  number (replayed callbacks never double-credit).
* **USDT** — TRC20 address + rate (CNY/USDT). The tenant transfers and submits the tx hash; the admin
  confirms it under **Orders**.

Tenants top up at `/app/recharge` (nav "Top Up"). All credits go through the idempotent ledger.

### 8. Balance & recharge API

* Admin → **Users**: credit/debit any account directly (positive credits, negative debits).
* For payment integration use the **recharge API**: generate a Bearer key on the users page, then:

```
POST /api/v1/recharge
Authorization: Bearer <key>
Content-Type: application/json

{"email":"user@example.com","amount_cents":1000,"ref":"order-123","note":"alipay"}
→ {"ok":true,"user_id":1,"balance_cents":1000}
```

Amount is in **cents**. `ref` is the external order number: the same `ref` never double-credits
(idempotent, safe for callback retries). Query balance: `GET /api/v1/balance?email=...` (also Bearer).
All balance changes are recorded in the `transactions` ledger, visible to the tenant at `/app/recharge`.

---

## Network modes

Three families, configured per plan; a node can enable NAT, dedicated IPv6 and dedicated public
IPv4 pools **at the same time**.

### NAT mode

The instance sits on the NAT bridge with a **fixed IPv4 lease** from the node's `nat_subnet` (written
to `ipv4.address`), so port forwards stay valid across reboots. Forwarding uses Incus's own
**proxy devices with `nat=true`** — kernel DNAT, torn down automatically with the instance, never
touching host iptables:

* `panel-ssh` — host `<ssh_port>` → container `22`
* `panel-tcp` / `panel-udp` — host `<from>-<to>` → same range in the container

### Dedicated IPv6

A second NIC (`eth1`) on an operator-chosen bridge, assigned an address from the node's `v6_cidr`.
Applied immediately with `ip -6 addr add` and persisted (systemd unit on Debian, `/etc/local.d` on
Alpine) so it survives reboots. Bridge the uplink carrying your routed IPv6 prefix into `v6_bridge`.

### Dedicated public IPv4 / mixed

Enable the "dedicated IPv4 pool" on a node (bridge + CIDR + gateway); the panel assigns an address,
configured statically in-guest and persisted (same mechanism as dedicated IPv6). Plan modes:

| Mode | eth0 | extra |
|---|---|---|
| `nat` | internal NAT | — |
| `ipv6` | internal NAT | dedicated IPv6 |
| `ipv6only` | dedicated IPv6 | — |
| `ipv4` | internal NAT | dedicated public IPv4 |
| `ipv4only` | dedicated public IPv4 | — |
| `ipv4v6` | internal NAT | dedicated v4 + v6 |

Plans with dedicated public addresses only schedule onto nodes that have the matching pool.

### Internal IP-pool ranges

A plan can set an "internal IP range" (e.g. `10.180.0.100-10.180.0.200`, must be inside the node's
NAT subnet, comma-separated for multiple). That plan's NAT internal IP is only allocated from this
range — handy for carving up the subnet per plan.

### NAT source IP (DNAT toggle)

Per plan: "NAT forwards show the real source IP (DNAT)":

* **on (default)** — the container sees the visitor's real IP (kernel DNAT / `nat=true`);
* **off** — a userspace proxy is used, so the container sees the host as the source (visitor IP hidden).

### IPv6-only

No IPv4 at all: the container's single NIC sits directly on the IPv6 bridge with a dedicated
address. No NAT lease, no SSH port, no forwards — tenants `ssh root@<IPv6>`. A public IPv6 DNS is
written automatically. Good for cheap v6-only plans.

### Reusing an existing bridge (docker0 / br0)

To avoid creating a new bridge, reuse an existing one:

```sh
EXISTING_BRIDGE=docker0 sh ./setup-lxd-node.sh
```

Then in the node settings: **uncheck "bridge managed by Incus"**, set the NAT gateway to the bridge
address (e.g. `172.17.0.1`), and fill "reserved IP ranges" with addresses already used by the
host/Docker (e.g. `172.17.0.1-172.17.0.99`) to avoid collisions. On unmanaged bridges the instance
IP is configured statically in-guest and port forwards use the agent's own iptables DNAT.

### Container feature switches

Plans can enable: **TUN/TAP** (VPN), **FUSE**, **nested containers**, **privileged**. The first two
pass through `/dev/net/tun` and `/dev/fuse` as unix-char devices; the latter map to
`security.nesting` / `security.privileged`. ⚠️ Root in a privileged container ≈ host root — only for
fully trusted tenants.

### KVM virtual machines (Beta)

A plan's "instance type" can be **KVM VM**: same bridges/NAT, fixed leases, port forwards, bandwidth
limits, traffic metering and elastic scaling as containers; same image aliases (Incus resolves the VM
variant, ~300–500 MB when pre-warmed).

Prerequisites (off by default):

1. the node must have `/dev/kvm` (bare metal or nested virt);
2. install QEMU: `KVM=1 sh ./setup-lxd-node.sh` (**the agent installer never installs anything**);
3. tick "KVM support (beta)" on the node — KVM plans only schedule onto ticked nodes; the agent
   re-checks `/dev/kvm` before create.

VM-only capabilities (under a plan's feature switches):

* **VNC console** — the VM gets a host VNC port + 8-char password (QEMU built-in auth), shown on the
  instance page; connect with any VNC client.
* **Mount ISO** — Admin → Images → "ISO library": download an ISO by URL to the node (stored under
  `CUB_AGENT_ISO_DIR`, default `/var/lib/cub-panel/isos`), then mount it as a CD-ROM on the VM
  detail page; tick "boot from ISO" (`boot.priority`) to install a custom OS. Detach when done; a
  restart applies mount/unmount changes. VM-only.
* **Nested virtualization** — run KVM/containers inside the VM.
* **AES-NI passthrough** — expose hardware AES.
* **CPU masking** — generic CPU model + hidden hypervisor vendor, harder to detect as a VM.
* **Multi-NIC** — a plan's "extra bridges" attaches additional NICs (containers too).

Note: VM RAM is **fully reserved** (no oversell); container feature switches (tun/fuse/privileged)
are ignored for VMs. Marked beta — not yet verified end-to-end on real KVM hardware.

### Snapshots

Each instance can create/restore/delete snapshots (**up to 3**, containers and VMs, backed by Incus
snapshots). Restore rolls back to the snapshot moment. Verified on containers.

### Migrate to another server (cross-node)

Admin → Instances, pick a target node and "Migrate" for a **cold migration** (appears when ≥2 nodes):

1. reserve the target's network (IP/ports/VNC);
2. stop the source → pack into a portable Incus backup → **stream through the panel** to the target
   and import (per-node secrets never leave the master, all over TLS);
3. rebuild devices for the target's network and start; only then is the DB updated and the source
   destroyed.

**Any failure rolls back**: the source restarts intact, the target's partial is cleaned, no data loss.
Status shows "migrating". Large disks/VMs take a while (background). Cross-node migration needs ≥2
nodes — not verified end-to-end in a single-node setup.

### Real source IP

The connection source seen inside the guest is always the **client's real IP**, not the gateway/NAT
address:

* managed bridge: kernel DNAT (`nat=true` proxy device) preserves the source natively;
* shared bridge: the agent installs its own iptables DNAT rules (tagged per instance, reclaimed on
  delete, **replayed by the agent from the instance record after a host reboot**). Only when the host
  has no iptables at all does it fall back to a userspace proxy (source shows as the host).

### DNS settings

A node's "instance DNS servers" (e.g. `1.1.1.1 8.8.8.8`) are written to new instances' `resolv.conf`
and persisted on boot; empty uses the system default. IPv6-only instances get a public IPv6 DNS if
unset.

### Traffic limits

Plans set a **monthly traffic allowance (GB)** and **billing direction**: both (up+down), upload-only,
download-only.

* the panel samples node byte counters every 5 min and accumulates (survives container restarts);
* over-quota **force-stops** the instance, status "over quota", tenants can't restart it;
* auto-reset every 30 days; upgrading a plan applies the new allowance;
* Admin → Instances can "reset traffic" any time, which also clears an over-quota stop.

### Bandwidth limits

Plans set **down / up bandwidth (Mbps)**, 0 = unlimited. Mapped to Incus NIC `limits.ingress` /
`limits.egress` (kernel tc, reclaimed on delete), applied to all NICs regardless of mode. Adjustable
live from the admin instance page; upgrading applies the new plan's bandwidth.

### Elastic scaling

* **Tenant**: the instance detail page can upgrade to a same-network-mode plan whose specs are ≥ the
  current and price is higher, paying only the **difference** (from balance, auto-refunded on failure);
  CPU/RAM hot-apply, data untouched.
* **Admin**: the instance page can set any instance to any CPU/RAM/disk (disk grows only).

---

## OS support

The agent supports **Debian** and **Alpine** boot-init recipes (auto-detected from the image alias):

| | Debian / Ubuntu | Alpine |
|---|---|---|
| package | `apt-get` | `apk` |
| service | systemd | OpenRC |
| SSH | `openssh-server`, root password login | `openssh`, `rc-update add sshd` |
| IPv6 persistence | systemd unit | `/etc/local.d` |

Aliases go through simplestreams; default server `https://images.linuxcontainers.org` (Incus
community mirror, widest coverage; LXD nodes may use `https://images.lxd.canonical.com`). Common:
`debian/12`, `debian/13`, `alpine/3.21`, `alpine/3.22`, `ubuntu/noble`.

**The agent itself also runs on Alpine** — static binary + OpenRC service files are ready.

---

## Web terminal (serial console)

`browser ⇄ master ⇄ agent ⇄ Incus exec` — three-stage websocket forwarding, rendered with xterm.js.

* Terminal bytes ride binary frames; resize rides a text-frame JSON (`{"type":"resize",...}`).
* Prefers `bash`, falls back to `sh`; works on Debian & Alpine.
* xterm.js is bundled under `static/` — **no CDN**, friendly to strict CSP and offline/intranet.
* Mobile-friendly, font scales to width.

---

## Security

The master is hardened point-by-point:

| Risk | Handling |
|---|---|
| **SQL injection** | All statements parameterized; no user input ever concatenated into SQL. |
| **XSS** | Pages use `html/template` context-aware escaping; all dynamic frontend writes use `textContent` — **no `innerHTML` with dynamic data anywhere**. |
| **CSP** | `script-src 'self'` (no `unsafe-inline`/`unsafe-eval`), `object-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'`. |
| **CSRF** | Per-session token (form field + `X-CSRF-Token` header), constant-time compare; plus Origin/Referer same-origin check; `SameSite=Lax` cookie. |
| **Sessions** | 256-bit random id, `HttpOnly`; changing password revokes all others; expired rows reaped. |
| **Passwords** | bcrypt cost 12. Login runs a compare even for missing accounts (no timing oracle). |
| **IDOR** | Every instance op checks ownership; non-owner and missing both return **404** (no existence leak). |
| **Brute force** | Login 8/10min, sign-up 5/hr, redeem 10/10min, actions 120/min, per-IP. |
| **Open redirect** | `?next=` only accepts on-site absolute paths; `//`, `\`, CRLF fall back to `/app`. |
| **Master ⇄ agent** | **HTTPS** (agent auto-generates a 10-year self-signed ECDSA cert, panel pins its SHA-256 fingerprint) + HMAC-SHA256 over method+path+timestamp+nonce+body digest; ±90s window; nonce replay cache; constant-time compare. |
| **Command injection** | All dynamic values in the in-guest init script (password, IPv6, gateway) are passed via **environment variables**, never interpolated into shell text. Instance name, alias, bridge name, IP are re-checked with regexes on the agent side. |
| **Activation codes** | 82 bits of entropy (16 chars `A-Z2-9`); redemption claims via an atomic conditional UPDATE (no double-spend); auto-refund on failure. |
| **Container escape** | `security.privileged` and `security.nesting` explicitly off unless the plan opts in. |

### Two things to do before going live

1. **Put HTTPS in front** (Caddy / Nginx), then set `CUB_PANEL_SECURE_COOKIES=1`; behind a proxy
   also set `CUB_PANEL_TRUST_PROXY=1`, otherwise rate-limiting and audit logs see the proxy IP.
2. **Never expose the agent port to the internet.** `CUB_AGENT_SECRET` equals root on that box —
   use a private network, WireGuard, or a firewall allowing only the master IP.

---

## Configuration

### Master `/opt/cub-panel/cub-panel.env`

| Variable | Default | Notes |
|---|---|---|
| `CUB_PANEL_LISTEN` | `0.0.0.0:8080` | listen address |
| `CUB_PANEL_DB` | `/opt/cub-panel/data/panel.db` | SQLite path |
| `CUB_PANEL_SITE` | `Incus Panel` | site name (overridable in Settings) |
| `CUB_PANEL_SECURE_COOKIES` | `0` | set `1` under HTTPS |
| `CUB_PANEL_TRUST_PROXY` | `0` | set `1` behind a proxy |
| `CUB_PANEL_ALLOW_SIGNUP` | `1` | set `0` after creating the admin |

### Agent `/opt/cub-panel/cub-agent.env`

| Variable | Default | Notes |
|---|---|---|
| `CUB_AGENT_LISTEN` | `0.0.0.0:8788` | listen address, bind private |
| `CUB_AGENT_SECRET` | generated | shared secret, ≥32 chars |
| `CUB_AGENT_SOCKET` | auto-detected | Incus/LXD unix socket |
| `CUB_AGENT_POOL` | `default` | storage pool |
| `CUB_AGENT_IMAGE_SERVER` | `https://images.linuxcontainers.org` | image source |
| `CUB_AGENT_ISO_DIR` | `/var/lib/cub-panel/isos` | ISO library dir (KVM CD-ROM) |
| `CUB_AGENT_TLS` | `1` | HTTPS switch, `0` for plain HTTP |
| `CUB_AGENT_TLS_CERT` / `_KEY` | `agent-cert.pem` / `agent-key.pem` | cert paths, auto-generated (10-year self-signed) |
| `CUB_AGENT_VERBOSE` | empty | non-empty for verbose logging |

---

## Backend features

* **Overview** — node online count, instances, capacity, code balance, recent actions
* **Nodes** — CRUD, connectivity probe, NAT/IPv6/IPv4 params, capacity caps, disable
* **Plans** — specs, network mode, features, images, term, list/unlist, ordering
* **Images** — pre-warm (container/KVM), delete, ISO library
* **Codes** — batch generate (≤500/batch), bind plan+node, validity, batch filter, one-click copy
* **Instances** — full list, extend (+30d), resize, migrate, traffic reset, destroy
* **Users** — suspend/restore, reset password, credit/debit balance, promote/demote admin
* **Orders** — recharge orders, confirm USDT/offline payments
* **Settings** — site name, announcement, payment channels
* **Audit** — every sensitive action logged (actor, action, detail, IP, time)

Background jobs: node health probe (2 min), expiry auto-stop (5 min), traffic metering (5 min),
session cleanup (1 hr).

---

## Rebuild from source

```sh
cd /opt/cub-panel/deploy
sh ./build.sh                  # host arch
GOARCH=arm64 sh ./build.sh     # cross-compile ARM64 → bin/cub-*-arm64
```

Needs Go 1.25+. Three dependencies: `modernc.org/sqlite` (pure Go, no cgo),
`gorilla/websocket`, `golang.org/x/crypto`.

```sh
cd /opt/cub-panel/src && go test ./...
```

---

## FAQ

**Node stuck "error"**
On the panel host: `curl -k https://<node-ip>:8788/v1/health`. A 401 means the network is fine but the
signature doesn't match (wrong secret, or clocks >90s apart); connection refused means firewall or a
wrong `CUB_AGENT_LISTEN` binding.

**Provisioning stuck for a long time**
First image pull takes minutes. Check the agent log: `rc-service cub-agent status` or
`journalctl -u cub-agent -f`. You can pre-warm: `incus image copy images:debian/12 local:`.

**Container gets an IP but no network (apk/apt hangs)**
If the host also runs Docker, Docker sets the iptables FORWARD policy to DROP, dropping all bridge
forwarding. `setup-lxd-node.sh` inserts accept rules and persists them; manual fix:
`iptables -I FORWARD -i lxdbr0 -j ACCEPT; iptables -I FORWARD -o lxdbr0 -j ACCEPT`.

**`>/dev/null` fails with Permission denied inside the container / sshd won't install**
Some VPS images ship `/dev/null` etc. as 0660; unprivileged containers bind-mount them and container
root can't write. `setup-lxd-node.sh` detects and fixes this (`chmod 666 /dev/null …` + `/etc/local.d`
persistence).

**Port forwarding not working**
`incus config device show <instance>` to check `panel-ssh` / `panel-tcp` exist. A `nat=true` error
usually means `nat_subnet` doesn't match the bridge's actual subnet, so the connect address isn't in
the container's network.

**Container can't get IPv6**
Confirm `v6_bridge` exists with the uplink bridged in, the host can ping addresses in the prefix, and
`v6_gw` is the gateway (not the container address). Inside: `ip -6 addr show eth1`.
