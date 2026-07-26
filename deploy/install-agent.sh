#!/bin/sh
# Install the cub-agent onto an Incus (or LXD) host.
# Works on Alpine (OpenRC) and Debian/Ubuntu (systemd).
set -eu

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PANEL_HOME="${PANEL_HOME:-/opt/cub-panel}"
ENV_FILE="$PANEL_HOME/cub-agent.env"

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mxx\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "please run as root"

# ---------- kernel modules ----------
#
# Instance features need host kernel modules that some minimal/cloud kernels
# don't autoload: tun (TUN/TAP for VPNs), fuse, plus the bridge/NAT and VM
# helpers Incus relies on. Load them now (best effort) and persist via
# /etc/modules-load.d so they come back after a reboot. Modules that are built
# into the kernel or absent are skipped silently — only ones that actually
# load get persisted, so boot never errors on a missing module.

say "loading kernel modules (tun, fuse, …)"
MODLIST="tun fuse overlay br_netfilter vhost_vsock vhost_net nbd"
LOADED=""
for m in $MODLIST; do
	if modprobe -q "$m" 2>/dev/null; then
		LOADED="$LOADED $m"
	fi
done
# Persist the ones that actually loaded so they return after a reboot. Only
# proven-loadable modules are written, so boot never errors on a missing one.
if [ -n "$LOADED" ]; then
	mkdir -p /etc/modules-load.d 2>/dev/null || true
	{ for m in $LOADED; do echo "$m"; done; } > /etc/modules-load.d/cub-panel.conf 2>/dev/null || true
fi
# The tun feature needs /dev/net/tun; create the node if the module is loaded
# but the device is missing (some minimal images).
if [ ! -e /dev/net/tun ] && printf '%s' "$LOADED" | grep -qw tun; then
	mkdir -p /dev/net && mknod /dev/net/tun c 10 200 2>/dev/null && chmod 666 /dev/net/tun 2>/dev/null || true
fi
printf '   loaded:%s\n' "${LOADED:- (none — kernel likely has them builtin)}"

# Pick the binary matching this machine: cross-compiled builds carry an
# arch suffix (see build.sh), e.g. cub-agent-arm64 on aarch64 hosts.
BIN="$SRC_DIR/bin/cub-agent"
case "$(uname -m)" in
aarch64|arm64) [ -f "$BIN-arm64" ] && BIN="$BIN-arm64" ;;
riscv64)       [ -f "$BIN-riscv64" ] && BIN="$BIN-riscv64" ;;
esac
[ -f "$BIN" ] || die "missing $BIN"

# Locate the Incus/LXD socket so the generated config points at the right place.
LXD_SOCKET=""
for c in /var/lib/incus/unix.socket /var/lib/lxd/unix.socket /var/snap/lxd/common/lxd/unix.socket; do
	[ -S "$c" ] && { LXD_SOCKET="$c"; break; }
done
if [ -z "$LXD_SOCKET" ]; then
	warn "no Incus/LXD socket found — install and init Incus first (see setup-lxd-node.sh)"
	LXD_SOCKET=/var/lib/incus/unix.socket
fi
say "daemon socket: $LXD_SOCKET"

say "installing into $PANEL_HOME"
mkdir -p "$PANEL_HOME/bin"
install -m 0755 "$BIN" "$PANEL_HOME/bin/cub-agent"

if [ ! -f "$ENV_FILE" ]; then
	# 48 hex chars = 192 bits of shared secret.
	SECRET="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
	say "generating a new shared secret"
	cat > "$ENV_FILE" <<EOF
# cub-agent configuration.

# Listen address. Bind to a private/VPN interface — this API controls Incus.
CUB_AGENT_LISTEN=0.0.0.0:8788

# Shared secret. Paste this exact value into the panel when adding this node.
CUB_AGENT_SECRET=$SECRET

# Local Incus/LXD unix socket.
CUB_AGENT_SOCKET=$LXD_SOCKET

# Storage pool used for container root disks.
CUB_AGENT_POOL=cub

# simplestreams image server.
CUB_AGENT_IMAGE_SERVER=https://images.linuxcontainers.org

# Directory for uploaded ISO images (KVM CD-ROM library).
CUB_AGENT_ISO_DIR=/var/lib/cub-panel/isos

# Set to any non-empty value for verbose logging.
CUB_AGENT_VERBOSE=

# HTTPS (on by default). A 10-year self-signed certificate is generated on
# first start; the panel pins its SHA-256 fingerprint. Set CUB_AGENT_TLS=0
# to serve plain HTTP instead.
CUB_AGENT_TLS=1
EOF
	chmod 0600 "$ENV_FILE"
else
	say "keeping existing $ENV_FILE"
fi

if command -v rc-update >/dev/null 2>&1; then
	say "installing OpenRC service"
	install -m 0755 "$SRC_DIR/deploy/openrc/cub-agent" /etc/init.d/cub-agent
	rc-update add cub-agent default >/dev/null 2>&1 || true
	rc-service cub-agent restart || rc-service cub-agent start
	say "service: rc-service cub-agent {start|stop|restart|status}"
elif command -v systemctl >/dev/null 2>&1; then
	say "installing systemd unit"
	install -m 0644 "$SRC_DIR/deploy/systemd/cub-agent.service" /etc/systemd/system/cub-agent.service
	mkdir -p /var/lib/cub-panel
	systemctl daemon-reload
	systemctl enable --now cub-agent
	systemctl restart cub-agent
	say "service: systemctl {start|stop|restart|status} cub-agent"
else
	warn "no OpenRC or systemd found; start it manually:"
	warn "  set -a; . $ENV_FILE; set +a; $PANEL_HOME/bin/cub-agent"
fi

echo
# Give the agent a moment to mint its certificate on first start.
FP=""
i=0
while [ $i -lt 15 ]; do
	[ -f "$PANEL_HOME/agent-cert.pem.fp" ] && { FP="$(cat "$PANEL_HOME/agent-cert.pem.fp")"; break; }
	i=$((i + 1)); sleep 1
done
if [ -z "$FP" ] && [ -f "$PANEL_HOME/agent-cert.pem" ] && command -v openssl >/dev/null 2>&1; then
	FP="$(openssl x509 -in "$PANEL_HOME/agent-cert.pem" -outform der 2>/dev/null | sha256sum | awk '{print $1}')"
fi
if [ -z "$FP" ]; then
	warn "the agent did not come up — check it with:"
	if command -v systemctl >/dev/null 2>&1; then
		warn "  systemctl status cub-agent && journalctl -u cub-agent -n 30"
	else
		warn "  rc-service cub-agent status && tail -30 /var/log/cub-agent.log"
	fi
fi
HOSTIP="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' | head -1)"
[ -n "$HOSTIP" ] || HOSTIP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$HOSTIP" ] || HOSTIP='<this-host-ip>'
say "add this node in the panel with these values:"
printf '    Agent 地址 : https://%s:8788\n' "$HOSTIP"
printf '    共享密钥   : %s\n' "$(sed -n 's/^CUB_AGENT_SECRET=//p' "$ENV_FILE")"
printf '    证书指纹   : %s\n' "${FP:-（未获取到，见上方排查命令）}"
echo
warn "the secret above is equivalent to root on this box — keep the agent port off the public internet."
