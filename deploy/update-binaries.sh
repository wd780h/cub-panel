#!/bin/sh
# Update production binaries: build under the source tree, install ONLY into PANEL_HOME.
#
# Contract (see docs/OPS-PATHS.md):
#   - Compile workspace : repository root (e.g. /box/env)
#   - Runtime home      : /opt/cub-panel  (ONLY place production binaries are installed)
#   - Never treat <repo>/bin as the live service path.
#
# Usage:
#   sh ./update-binaries.sh                 # build + install panel+agent + restart
#   sh ./update-binaries.sh --install-only  # skip go build
#   sh ./update-binaries.sh --panel         # only cub-panel
#   sh ./update-binaries.sh --agent         # only cub-agent
#   sh ./update-binaries.sh --no-restart    # install without service restart
#
# From an agent container with host root at /host:
#   PANEL_HOME=/host/opt/cub-panel sh /host/box/env/deploy/update-binaries.sh
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SRC_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Prefer host-mounted production path when visible (Hermes/Grok: /host/opt/cub-panel).
if [ -n "${PANEL_HOME:-}" ]; then
	:
elif [ -d /host/opt/cub-panel ]; then
	PANEL_HOME=/host/opt/cub-panel
elif [ -d /opt/cub-panel ]; then
	PANEL_HOME=/opt/cub-panel
else
	PANEL_HOME=/opt/cub-panel
fi

DO_BUILD=1
DO_RESTART=1
DO_PANEL=1
DO_AGENT=1

for arg in "$@"; do
	case "$arg" in
	--install-only|--no-build) DO_BUILD=0 ;;
	--no-restart) DO_RESTART=0 ;;
	--panel)
		DO_PANEL=1
		DO_AGENT=0
		;;
	--agent)
		DO_PANEL=0
		DO_AGENT=1
		;;
	--all)
		DO_PANEL=1
		DO_AGENT=1
		;;
	-h|--help)
		sed -n '2,20p' "$0"
		exit 0
		;;
	*)
		printf 'unknown arg: %s\n' "$arg" >&2
		exit 2
		;;
	esac
done

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mxx\033[0m %s\n' "$*" >&2; exit 1; }

# Refuse to install into the source tree itself (common footgun).
case "$PANEL_HOME" in
"$SRC_DIR"|"$SRC_DIR"/*)
	die "PANEL_HOME=$PANEL_HOME must NOT be the source tree ($SRC_DIR). Use /opt/cub-panel."
	;;
esac
case "$PANEL_HOME" in
*/box/env|*/box/env/|*/box/env/*)
	die "PANEL_HOME=$PANEL_HOME looks like the compile workspace. Deploy only to /opt/cub-panel."
	;;
*/usr/local|*/usr/local/*|/usr/local|/usr/local/*)
	die "PANEL_HOME=$PANEL_HOME forbidden. Deploy only to /opt/cub-panel (not /usr/local)."
	;;
esac
# Canonical host path is /opt/cub-panel; agent containers may see /host/opt/cub-panel.
case "$PANEL_HOME" in
/opt/cub-panel|/opt/cub-panel/|/host/opt/cub-panel|/host/opt/cub-panel/)
	;;
*)
	die "PANEL_HOME=$PANEL_HOME rejected. Only /opt/cub-panel (or /host/opt/cub-panel) is allowed."
	;;
esac

[ -d "$SRC_DIR/src" ] || die "source tree not found at $SRC_DIR (expected src/)"
[ -d "$PANEL_HOME" ] || die "PANEL_HOME missing: $PANEL_HOME (create via install-panel.sh first)"

say "source (compile): $SRC_DIR"
say "runtime (deploy): $PANEL_HOME"
say "build=$DO_BUILD panel=$DO_PANEL agent=$DO_AGENT restart=$DO_RESTART"

if [ "$DO_BUILD" = 1 ]; then
	say "building in source tree → $SRC_DIR/bin"
	sh "$SCRIPT_DIR/build.sh"
fi

# Pick arch-suffixed binary if present (same logic as install-*.sh).
pick_bin() {
	base="$1"
	bin="$SRC_DIR/bin/$base"
	case "$(uname -m)" in
	aarch64|arm64) [ -f "$bin-arm64" ] && bin="$bin-arm64" ;;
	riscv64)       [ -f "$bin-riscv64" ] && bin="$bin-riscv64" ;;
	esac
	[ -f "$bin" ] || die "missing build output: $bin (run without --install-only, or build first)"
	printf '%s\n' "$bin"
}

install_one() {
	name="$1"
	src="$(pick_bin "$name")"
	dst_dir="$PANEL_HOME/bin"
	dst="$dst_dir/$name"
	mkdir -p "$dst_dir"
	if [ -f "$dst" ]; then
		bak="$dst.bak.$(date +%Y%m%d%H%M%S)"
		say "backup $dst → $bak"
		cp -a "$dst" "$bak"
	fi
	say "install $src → $dst"
	install -m 0755 "$src" "$dst"
	# Record provenance for operators/agents.
	{
		echo "name=$name"
		echo "installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "source_tree=$SRC_DIR"
		echo "source_bin=$src"
		echo "sha256=$(sha256sum "$dst" | awk '{print $1}')"
		if command -v git >/dev/null 2>&1; then
			echo "git=$(git -C "$SRC_DIR" describe --tags --always --dirty 2>/dev/null || true)"
		fi
	} > "$dst_dir/$name.deployed"
}

[ "$DO_PANEL" = 1 ] && install_one cub-panel
[ "$DO_AGENT" = 1 ] && install_one cub-agent

# Best-effort marker that this tree is the sole runtime home.
if [ -d "$PANEL_HOME" ]; then
	cat > "$PANEL_HOME/DEPLOY_PATHS.txt" <<EOF
# cub-panel runtime home — production binaries live ONLY here.
# Compile workspace: $SRC_DIR
# Runtime home:      $PANEL_HOME
# Updated:           $(date -u +%Y-%m-%dT%H:%M:%SZ)
# Forbidden:         /usr/local/bin, /box/env as deploy targets
# See:               $SRC_DIR/docs/OPS-PATHS.md
# Enforce:           $SRC_DIR/deploy/enforce-paths.sh
EOF
fi

# Strip any accidental host copies outside PANEL_HOME (never /box/env build cache).
if [ -x "$SCRIPT_DIR/enforce-paths.sh" ]; then
	say "enforcing sole deploy path (/opt/cub-panel)"
	sh "$SCRIPT_DIR/enforce-paths.sh" --clean || warn "enforce-paths reported issues (binaries already installed to $PANEL_HOME)"
fi

host_rc() {
	# Prefer chroot into host root when we are in a container with /host mount.
	if [ -x /host/sbin/rc-service ] || [ -f /host/sbin/rc-service ]; then
		chroot /host /sbin/rc-service "$@"
		return
	fi
	if command -v rc-service >/dev/null 2>&1; then
		rc-service "$@"
		return
	fi
	if command -v systemctl >/dev/null 2>&1; then
		# Map: rc-service NAME restart → systemctl restart NAME
		svc="$1"
		action="$2"
		systemctl "$action" "$svc"
		return
	fi
	return 127
}

if [ "$DO_RESTART" = 1 ]; then
	if [ "$DO_PANEL" = 1 ]; then
		say "restart cub-panel service"
		if host_rc cub-panel restart 2>/dev/null || host_rc cub-panel start 2>/dev/null; then
			:
		else
			warn "could not restart cub-panel via OpenRC/systemd; binary is installed — restart on host:"
			warn "  rc-service cub-panel restart   # or: systemctl restart cub-panel"
		fi
	fi
	if [ "$DO_AGENT" = 1 ]; then
		say "restart cub-agent service"
		if host_rc cub-agent restart 2>/dev/null || host_rc cub-agent start 2>/dev/null; then
			:
		else
			warn "could not restart cub-agent via OpenRC/systemd; binary is installed — restart on host:"
			warn "  rc-service cub-agent restart   # or: systemctl restart cub-agent"
		fi
	fi
fi

say "done. production binaries are only under $PANEL_HOME/bin"
ls -lh "$PANEL_HOME/bin/cub-panel" "$PANEL_HOME/bin/cub-agent" 2>/dev/null || true
