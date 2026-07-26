#!/bin/sh
# cub-panel one-click installer — downloads a prebuilt static binary from the
# GitHub Releases and sets it up as a service. No Go toolchain, no source tree.
#
#   Master (panel):   curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- panel
#   Node   (agent):   curl -fsSL https://github.com/wd780h/cub-panel/releases/latest/download/install.sh | sh -s -- agent
#
# Works on Alpine (OpenRC) and Debian/Ubuntu (systemd). Run as root.
#
# Env overrides:
#   REPO=wd780h/cub-panel   VERSION=latest   PANEL_HOME=/opt/cub-panel
set -eu

ROLE="${1:-}"
REPO="${REPO:-wd780h/cub-panel}"
VERSION="${VERSION:-latest}"
PANEL_HOME="${PANEL_HOME:-/opt/cub-panel}"

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mxx\033[0m %s\n' "$*" >&2; exit 1; }

case "$ROLE" in
	panel|agent) ;;
	*) die "usage: install.sh <panel|agent>" ;;
esac
[ "$(id -u)" = "0" ] || die "please run as root"

# ask_db prompts for the panel's database backend. The installer NEVER installs
# a database server — it only wires the panel to connect. SQLite is the default
# and needs nothing; PostgreSQL/MySQL must already exist and you supply a DSN.
# Reads from /dev/tty so it still works under `curl … | sh`.
DB_DRIVER=sqlite
DB_DSN=""
ask_db() {
	DB_DRIVER=sqlite; DB_DSN=""
	[ -e /dev/tty ] || { warn "non-interactive shell — defaulting to SQLite"; return; }
	# Allow a preset via env for unattended installs.
	if [ -n "${CUB_PANEL_DB_DRIVER:-}" ]; then
		DB_DRIVER="$CUB_PANEL_DB_DRIVER"; DB_DSN="${CUB_PANEL_DB_DSN:-}"; return
	fi
	printf '\n选择数据库 / choose the database backend:\n'
	printf '  1) SQLite      默认，零依赖，单文件（中小规模够用）\n'
	printf '  2) PostgreSQL  对接已有实例（大规模 / 多实例推荐）\n'
	printf '  3) MySQL       对接已有实例\n'
	printf '请选择 [1]: '
	read ans </dev/tty || ans=1
	case "$ans" in
		2) DB_DRIVER=postgres ;;
		3) DB_DRIVER=mysql ;;
		*) DB_DRIVER=sqlite ;;
	esac
	if [ "$DB_DRIVER" = sqlite ]; then
		warn "SQLite 局限：只允许单写入者、不适合高并发、无法多面板共享同一库、跨机迁移只能拷文件；"
		warn "生产 / 大规模 / 高可用请选 PostgreSQL 或 MySQL。面板随时可改 cub-panel.env 切换。"
		return
	fi
	say "注意：安装脚本不会安装数据库，仅连接。请确认目标库已创建并可访问。"
	if [ "$DB_DRIVER" = postgres ]; then
		printf 'PostgreSQL DSN（例 postgres://user:pass@127.0.0.1:5432/cub?sslmode=disable）:\n> '
	else
		printf 'MySQL DSN（例 user:pass@tcp(127.0.0.1:3306)/cub）:\n> '
	fi
	read DB_DSN </dev/tty || DB_DSN=""
	[ -n "$DB_DSN" ] || die "DSN 不能为空（选了 $DB_DRIVER）"
}

# ---------- pick the asset for this CPU ----------
case "$(uname -m)" in
	x86_64|amd64)  SUFFIX="" ;;
	aarch64|arm64) SUFFIX="-arm64" ;;
	*) die "unsupported architecture: $(uname -m) (prebuilt binaries are amd64/arm64 only — build from source)" ;;
esac

ASSET="cub-$ROLE$SUFFIX"
if [ "$VERSION" = "latest" ]; then
	URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
	URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
fi

# ---------- fetch the binary ----------
fetch() {  # fetch <url> <dest>
	if command -v curl >/dev/null 2>&1; then
		curl -fSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -O "$2" "$1"
	else
		die "need curl or wget to download"
	fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
say "downloading $ASSET ($VERSION) from $REPO"
fetch "$URL" "$TMP/$ROLE" || die "download failed — has the release been published with this asset?"
[ -s "$TMP/$ROLE" ] || die "downloaded file is empty"

say "installing into $PANEL_HOME"
mkdir -p "$PANEL_HOME/bin"
install -m 0755 "$TMP/$ROLE" "$PANEL_HOME/bin/cub-$ROLE"

# ================================================================= AGENT ====
if [ "$ROLE" = "agent" ]; then
	# --- kernel modules some minimal kernels don't autoload ---
	say "loading kernel modules (tun, fuse, …)"
	LOADED=""
	for m in tun fuse overlay br_netfilter vhost_vsock vhost_net nbd; do
		modprobe -q "$m" 2>/dev/null && LOADED="$LOADED $m"
	done
	if [ -n "$LOADED" ]; then
		mkdir -p /etc/modules-load.d 2>/dev/null || true
		{ for m in $LOADED; do echo "$m"; done; } > /etc/modules-load.d/cub-panel.conf 2>/dev/null || true
	fi
	if [ ! -e /dev/net/tun ] && printf '%s' "$LOADED" | grep -qw tun; then
		mkdir -p /dev/net && mknod /dev/net/tun c 10 200 2>/dev/null && chmod 666 /dev/net/tun 2>/dev/null || true
	fi
	printf '   loaded:%s\n' "${LOADED:- (none — likely builtin)}"

	# --- locate the Incus/LXD socket ---
	LXD_SOCKET=""
	for c in /var/lib/incus/unix.socket /var/lib/lxd/unix.socket /var/snap/lxd/common/lxd/unix.socket; do
		[ -S "$c" ] && { LXD_SOCKET="$c"; break; }
	done
	if [ -z "$LXD_SOCKET" ]; then
		warn "no Incus/LXD socket found — install & init Incus first:"
		warn "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/deploy/setup-lxd-node.sh | sh"
		LXD_SOCKET=/var/lib/incus/unix.socket
	fi
	say "daemon socket: $LXD_SOCKET"

	ENV_FILE="$PANEL_HOME/cub-agent.env"
	if [ ! -f "$ENV_FILE" ]; then
		SECRET="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
		say "generating a new shared secret"
		cat > "$ENV_FILE" <<EOF
# cub-agent configuration.
CUB_AGENT_LISTEN=0.0.0.0:8788
CUB_AGENT_SECRET=$SECRET
CUB_AGENT_SOCKET=$LXD_SOCKET
CUB_AGENT_POOL=default
CUB_AGENT_IMAGE_SERVER=https://images.linuxcontainers.org
CUB_AGENT_ISO_DIR=/var/lib/cub-panel/isos
CUB_AGENT_VERBOSE=
# HTTPS on by default; a 10-year self-signed cert is minted on first start and
# the panel pins its SHA-256 fingerprint. Set to 0 to serve plain HTTP.
CUB_AGENT_TLS=1
EOF
		chmod 0600 "$ENV_FILE"
	else
		say "keeping existing $ENV_FILE"
	fi

	if command -v rc-update >/dev/null 2>&1; then
		say "installing OpenRC service"
		cat > /etc/init.d/cub-agent <<'RC'
#!/sbin/openrc-run
name="cub-agent"
description="cub-agent (node)"
: ${PANEL_HOME:=/opt/cub-panel}
command="${PANEL_HOME}/bin/cub-agent"
command_background=true
command_user="root:root"
directory="${PANEL_HOME}"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/${RC_SVCNAME}.log"
error_log="/var/log/${RC_SVCNAME}.log"
depend() { need net; after lxd firewall; }
start_pre() {
	checkpath --file --owner root:root --mode 0640 "${output_log}"
	[ -f "${PANEL_HOME}/cub-agent.env" ] || { eerror "missing ${PANEL_HOME}/cub-agent.env"; return 1; }
	set -a; . "${PANEL_HOME}/cub-agent.env"; set +a
	[ -n "${CUB_AGENT_SECRET}" ] || { eerror "CUB_AGENT_SECRET is not set"; return 1; }
}
RC
		chmod 0755 /etc/init.d/cub-agent
		rc-update add cub-agent default >/dev/null 2>&1 || true
		rc-service cub-agent restart || rc-service cub-agent start
		say "service: rc-service cub-agent {start|stop|restart|status}"
	elif command -v systemctl >/dev/null 2>&1; then
		say "installing systemd unit"
		cat > /etc/systemd/system/cub-agent.service <<'UNIT'
[Unit]
Description=cub-agent (node)
After=network-online.target lxd.service snap.lxd.daemon.service
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=/opt/cub-panel
EnvironmentFile=/opt/cub-panel/cub-agent.env
ExecStart=/opt/cub-panel/bin/cub-agent
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
RestrictSUIDSGID=true
LockPersonality=true
SystemCallArchitectures=native
[Install]
WantedBy=multi-user.target
UNIT
		systemctl daemon-reload
		systemctl enable --now cub-agent
		systemctl restart cub-agent
		say "service: systemctl {start|stop|restart|status} cub-agent"
	else
		warn "no OpenRC or systemd; start manually:"
		warn "  set -a; . $ENV_FILE; set +a; $PANEL_HOME/bin/cub-agent"
	fi

	# --- wait for the cert fingerprint the panel needs to pin ---
	FP=""; i=0
	while [ $i -lt 10 ]; do
		[ -f "$PANEL_HOME/agent-cert.pem.fp" ] && { FP="$(cat "$PANEL_HOME/agent-cert.pem.fp")"; break; }
		i=$((i + 1)); sleep 1
	done
	echo
	say "add this node in the panel with:"
	printf '    Agent 地址 : https://%s:8788\n' "$(hostname -i 2>/dev/null | awk '{print $1}' || echo '<this-host-ip>')"
	printf '    共享密钥   : %s\n' "$(sed -n 's/^CUB_AGENT_SECRET=//p' "$ENV_FILE")"
	printf '    证书指纹   : %s\n' "${FP:-见 /var/log/cub-agent.log}"
	echo
	warn "the secret above is root-equivalent on this box — keep port 8788 off the public internet."
	exit 0
fi

# ================================================================= PANEL ====
mkdir -p "$PANEL_HOME/data"
chmod 0750 "$PANEL_HOME/data"
ENV_FILE="$PANEL_HOME/cub-panel.env"
if [ ! -f "$ENV_FILE" ]; then
	ask_db
	say "writing default config to $ENV_FILE"
	cat > "$ENV_FILE" <<'EOF'
# cub-panel configuration.
CUB_PANEL_LISTEN=0.0.0.0:8080
# Quote values with spaces — the OpenRC service sources this file.
CUB_PANEL_SITE="cub-panel Cloud"
# Set to 1 once served over HTTPS (adds Secure flag to cookies).
CUB_PANEL_SECURE_COOKIES=0
# Set to 1 only behind a reverse proxy you control (trusts X-Forwarded-For).
CUB_PANEL_TRUST_PROXY=0
# First account created becomes admin; turn off once it exists.
CUB_PANEL_ALLOW_SIGNUP=1

# Database backend: sqlite (default) | postgres | mysql. The installer only
# connects — it never installs a DB server. For postgres/mysql set the DSN;
# for sqlite the panel uses CUB_PANEL_DB below.
#   postgres DSN: postgres://user:pass@host:5432/dbname?sslmode=disable
#   mysql DSN:    user:pass@tcp(host:3306)/dbname
CUB_PANEL_DB=/opt/cub-panel/data/panel.db
EOF
	{
		echo "CUB_PANEL_DB_DRIVER=$DB_DRIVER"
		printf 'CUB_PANEL_DB_DSN=%s\n' "\"$DB_DSN\""
	} >> "$ENV_FILE"
	chmod 0640 "$ENV_FILE"
	say "database backend: $DB_DRIVER"
else
	say "keeping existing $ENV_FILE"
fi

if command -v rc-update >/dev/null 2>&1; then
	say "installing OpenRC service"
	cat > /etc/init.d/cub-panel <<'RC'
#!/sbin/openrc-run
name="cub-panel"
description="cub-panel (master)"
: ${PANEL_HOME:=/opt/cub-panel}
command="${PANEL_HOME}/bin/cub-panel"
command_background=true
command_user="root:root"
directory="${PANEL_HOME}"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/${RC_SVCNAME}.log"
error_log="/var/log/${RC_SVCNAME}.log"
depend() { need net; after firewall; }
start_pre() {
	checkpath --directory --owner root:root --mode 0750 "${PANEL_HOME}/data"
	checkpath --file --owner root:root --mode 0640 "${output_log}"
	[ -f "${PANEL_HOME}/cub-panel.env" ] || { eerror "missing ${PANEL_HOME}/cub-panel.env"; return 1; }
	set -a; . "${PANEL_HOME}/cub-panel.env"; set +a
}
RC
	chmod 0755 /etc/init.d/cub-panel
	rc-update add cub-panel default >/dev/null 2>&1 || true
	rc-service cub-panel restart || rc-service cub-panel start
	say "service: rc-service cub-panel {start|stop|restart|status}"
elif command -v systemctl >/dev/null 2>&1; then
	say "installing systemd unit"
	cat > /etc/systemd/system/cub-panel.service <<'UNIT'
[Unit]
Description=cub-panel (master)
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=/opt/cub-panel
EnvironmentFile=/opt/cub-panel/cub-panel.env
ExecStart=/opt/cub-panel/bin/cub-panel
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/cub-panel/data
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
[Install]
WantedBy=multi-user.target
UNIT
	systemctl daemon-reload
	systemctl enable --now cub-panel
	systemctl restart cub-panel
	say "service: systemctl {start|stop|restart|status} cub-panel"
else
	warn "no OpenRC or systemd; start manually:"
	warn "  set -a; . $ENV_FILE; set +a; $PANEL_HOME/bin/cub-panel"
	exit 0
fi

PORT="$(sed -n 's/^CUB_PANEL_LISTEN=.*:\([0-9]*\)$/\1/p' "$ENV_FILE")"
echo
say "done. open http://<this-host>:${PORT:-8080}/register and create the first account (it becomes admin)."
