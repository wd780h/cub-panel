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

# Hard rule: production deploy root is ONLY /opt/cub-panel (see docs/OPS-PATHS.md).
case "$PANEL_HOME" in
/opt/cub-panel|/opt/cub-panel/|/host/opt/cub-panel|/host/opt/cub-panel/) ;;
*) die "PANEL_HOME=$PANEL_HOME rejected. Install only to /opt/cub-panel (not /box/env or /usr/local)." ;;
esac
case "$PANEL_HOME" in
"$SRC_DIR"|"$SRC_DIR"/*|*/box/env|*/box/env/*|*/usr/local|*/usr/local/*)
	die "PANEL_HOME=$PANEL_HOME forbidden as deploy target."
	;;
esac

# Prompt for the database backend. The installer only connects — it never
# installs a DB server. SQLite is the zero-dependency default.
DB_DRIVER=sqlite
DB_DSN=""
ask_db() {
	DB_DRIVER=sqlite; DB_DSN=""
	if [ -n "${CUB_PANEL_DB_DRIVER:-}" ]; then
		DB_DRIVER="$CUB_PANEL_DB_DRIVER"; DB_DSN="${CUB_PANEL_DB_DSN:-}"; return
	fi
	[ -e /dev/tty ] || { warn "non-interactive — defaulting to SQLite"; return; }
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
		warn "SQLite 局限：单写入者、不适合高并发、无法多面板共享同一库；生产大规模建议 PostgreSQL / MySQL。"
		return
	fi
	say "注意：脚本不安装数据库，仅连接。请确认目标库已创建并可访问。"
	if [ "$DB_DRIVER" = postgres ]; then
		printf 'PostgreSQL DSN（例 postgres://user:pass@127.0.0.1:5432/cub?sslmode=disable）:\n> '
	else
		printf 'MySQL DSN（例 user:pass@tcp(127.0.0.1:3306)/cub）:\n> '
	fi
	read DB_DSN </dev/tty || DB_DSN=""
	[ -n "$DB_DSN" ] || die "DSN 不能为空（选了 $DB_DRIVER）"
}

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
	ask_db
	say "writing default config to $ENV_FILE"
	cat > "$ENV_FILE" <<'EOF'
# cub-panel configuration.

# Listen address. Put a TLS-terminating reverse proxy in front for production.
CUB_PANEL_LISTEN=0.0.0.0:8080

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

# Database backend: sqlite (default) | postgres | mysql. The installer only
# connects — it never installs a DB server. SQLite uses CUB_PANEL_DB; for
# postgres/mysql set CUB_PANEL_DB_DSN below.
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
