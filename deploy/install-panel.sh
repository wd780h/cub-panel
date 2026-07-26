#!/bin/sh
# Install the cub-panel (master) onto this host.
# Works on Alpine (OpenRC) and Debian/Ubuntu (systemd).
set -eu

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PANEL_HOME="${PANEL_HOME:-/opt/cub-panel}"
ENV_FILE="$PANEL_HOME/cub-panel.env"

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mxx\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "please run as root"

# Pick the binary matching this machine: cross-compiled builds carry an
# arch suffix (see build.sh), e.g. cub-panel-arm64 on aarch64 hosts.
BIN="$SRC_DIR/bin/cub-panel"
case "$(uname -m)" in
aarch64|arm64) [ -f "$BIN-arm64" ] && BIN="$BIN-arm64" ;;
riscv64)       [ -f "$BIN-riscv64" ] && BIN="$BIN-riscv64" ;;
esac
[ -f "$BIN" ] || die "missing $BIN"

say "installing into $PANEL_HOME"
mkdir -p "$PANEL_HOME/bin" "$PANEL_HOME/data"
install -m 0755 "$BIN" "$PANEL_HOME/bin/cub-panel"
chmod 0750 "$PANEL_HOME/data"

if [ ! -f "$ENV_FILE" ]; then
	say "writing default config to $ENV_FILE"
	cat > "$ENV_FILE" <<'EOF'
# cub-panel configuration.

# Listen address. Put a TLS-terminating reverse proxy in front for production.
CUB_PANEL_LISTEN=0.0.0.0:8080

# SQLite database path.
CUB_PANEL_DB=/opt/cub-panel/data/panel.db

# Site name shown in the UI.
CUB_PANEL_SITE="cub-panel Cloud"

# Set to 1 once the panel is served over HTTPS. This adds the Secure flag to
# session cookies; leaving it on while serving plain HTTP breaks login.
CUB_PANEL_SECURE_COOKIES=0

# Set to 1 only when behind a reverse proxy you control, so that
# X-Forwarded-For / X-Real-IP are trusted for rate limiting and audit logs.
CUB_PANEL_TRUST_PROXY=0

# Allow public registration. The first account created becomes the admin.
# Turn this off once your admin account exists.
CUB_PANEL_ALLOW_SIGNUP=1
EOF
	chmod 0640 "$ENV_FILE"
else
	say "keeping existing $ENV_FILE"
fi

if command -v rc-update >/dev/null 2>&1; then
	say "installing OpenRC service"
	install -m 0755 "$SRC_DIR/deploy/openrc/cub-panel" /etc/init.d/cub-panel
	rc-update add cub-panel default >/dev/null 2>&1 || true
	rc-service cub-panel restart || rc-service cub-panel start
	say "service: rc-service cub-panel {start|stop|restart|status}"
elif command -v systemctl >/dev/null 2>&1; then
	say "installing systemd unit"
	install -m 0644 "$SRC_DIR/deploy/systemd/cub-panel.service" /etc/systemd/system/cub-panel.service
	systemctl daemon-reload
	systemctl enable --now cub-panel
	systemctl restart cub-panel
	say "service: systemctl {start|stop|restart|status} cub-panel"
else
	warn "no OpenRC or systemd found; start it manually:"
	warn "  set -a; . $ENV_FILE; set +a; $PANEL_HOME/bin/cub-panel"
	exit 0
fi

PORT="$(sed -n 's/^CUB_PANEL_LISTEN=.*:\([0-9]*\)$/\1/p' "$ENV_FILE")"
say "done. open http://<this-host>:${PORT:-8080}/register and create the first account (it becomes admin)."
