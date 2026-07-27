#!/bin/sh
# Enforce the single production deploy location for cub-panel / cub-agent.
#
# Host rule (see docs/OPS-PATHS.md):
#   Production binaries live ONLY under /opt/cub-panel/bin/
#   Forbidden as deploy targets: /usr/local/bin, /usr/bin, /box/env, etc.
#
# Usage:
#   sh ./enforce-paths.sh            # verify + remove forbidden host binaries
#   sh ./enforce-paths.sh --check    # verify only (exit 1 if violation)
#   sh ./enforce-paths.sh --clean    # remove forbidden host binaries (default)
#
# From agent container with host root at /host:
#   sh /host/box/env/deploy/enforce-paths.sh
set -eu

CHECK_ONLY=0
DO_CLEAN=1
for arg in "$@"; do
	case "$arg" in
	--check|--verify) CHECK_ONLY=1; DO_CLEAN=0 ;;
	--clean) DO_CLEAN=1; CHECK_ONLY=0 ;;
	-h|--help)
		sed -n '2,18p' "$0"
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
ok()   { printf '\033[32mOK\033[0m %s\n' "$*"; }

# Resolve host root when running inside Hermes/Grok containers.
if [ -d /host/opt/cub-panel ]; then
	HOST_ROOT=/host
	PANEL_HOME=/host/opt/cub-panel
elif [ -d /opt/cub-panel ]; then
	HOST_ROOT=
	PANEL_HOME=/opt/cub-panel
else
	die "production PANEL_HOME not found (expected /opt/cub-panel or /host/opt/cub-panel)"
fi

host_path() {
	# Prefix with HOST_ROOT when set (empty on bare host).
	printf '%s%s\n' "$HOST_ROOT" "$1"
}

VIOLATIONS=0

# Forbidden production binary locations on the *host* (not Docker image layers).
FORBIDDEN_BINS="
/usr/local/bin/cub-panel
/usr/local/bin/cub-agent
/usr/bin/cub-panel
/usr/bin/cub-agent
/bin/cub-panel
/bin/cub-agent
/sbin/cub-panel
/sbin/cub-agent
"

say "PANEL_HOME (sole deploy root): $PANEL_HOME"
say "expected binaries: $PANEL_HOME/bin/cub-panel , $PANEL_HOME/bin/cub-agent"

if [ ! -x "$PANEL_HOME/bin/cub-panel" ]; then
	warn "missing production binary: $PANEL_HOME/bin/cub-panel"
	VIOLATIONS=$((VIOLATIONS + 1))
else
	ok "present $PANEL_HOME/bin/cub-panel"
fi
if [ ! -x "$PANEL_HOME/bin/cub-agent" ]; then
	warn "missing production binary: $PANEL_HOME/bin/cub-agent"
	VIOLATIONS=$((VIOLATIONS + 1))
else
	ok "present $PANEL_HOME/bin/cub-agent"
fi

say "scanning forbidden host paths"
for rel in $FORBIDDEN_BINS; do
	f="$(host_path "$rel")"
	if [ -e "$f" ] || [ -L "$f" ]; then
		warn "forbidden binary present: $f"
		VIOLATIONS=$((VIOLATIONS + 1))
		if [ "$DO_CLEAN" = 1 ]; then
			say "removing $f"
			rm -f "$f"
			VIOLATIONS=$((VIOLATIONS - 1))
			ok "removed $f"
		fi
	fi
done

# Note: <repo>/bin/cub-* is a *build cache*, not a deploy location.
# We do not delete it here (build.sh / update-binaries.sh need it),
# but we refuse to treat it as production (see update-binaries.sh guards).

# Best-effort: report host processes (requires host /proc visibility).
PROC_ROOT="${HOST_ROOT}/proc"
[ -d "$PROC_ROOT" ] || PROC_ROOT=/proc
if [ -d "$PROC_ROOT" ]; then
	say "checking running cub-* processes under $PROC_ROOT"
	found_prod_panel=0
	found_prod_agent=0
	found_foreign=0
	for p in "$PROC_ROOT"/[0-9]*; do
		[ -e "$p/exe" ] || continue
		exe=$(readlink "$p/exe" 2>/dev/null || true)
		case "$exe" in
		*cub-panel*|*cub-agent*)
			pid=${p##*/}
			case "$exe" in
			*/opt/cub-panel/bin/cub-panel|/opt/cub-panel/bin/cub-panel)
				found_prod_panel=1
				ok "prod panel pid=$pid exe=$exe"
				;;
			*/opt/cub-panel/bin/cub-agent|/opt/cub-panel/bin/cub-agent)
				found_prod_agent=1
				ok "prod agent pid=$pid exe=$exe"
				;;
			*)
				# Docker demo uses /usr/local/bin/cub-panel inside its mount NS —
				# warn only; do not kill containers from this script.
				warn "non-production cub process pid=$pid exe=$exe (demo/container OK if intentional)"
				found_foreign=1
				;;
			esac
			;;
		esac
	done
	[ "$found_prod_panel" = 1 ] || warn "no running production cub-panel from /opt/cub-panel/bin"
	[ "$found_prod_agent" = 1 ] || warn "no running production cub-agent from /opt/cub-panel/bin"
fi

# Marker file for operators/agents.
if [ -d "$PANEL_HOME" ] && [ "$CHECK_ONLY" = 0 ]; then
	cat > "$PANEL_HOME/DEPLOY_PATHS.txt" <<EOF
# cub-panel runtime home — production binaries live ONLY here.
# Runtime home:      /opt/cub-panel  (container view: /host/opt/cub-panel)
# Forbidden deploys: /usr/local/bin, /box/env, /usr/bin, ...
# Verified:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
# See:               docs/OPS-PATHS.md  and  deploy/enforce-paths.sh
EOF
fi

if [ "$VIOLATIONS" -gt 0 ]; then
	die "path policy violations remaining: $VIOLATIONS (fix with --clean or reinstall to /opt/cub-panel)"
fi
ok "deploy path policy satisfied: only /opt/cub-panel holds production binaries"
exit 0
