#!/bin/sh
# Prepare a fresh host to serve as an cub-panel container node.
#
# Installs Incus, initialises a storage pool and the NAT bridge, and optionally
# configures a bridge carrying a routed IPv6 prefix. Safe to re-run.
set -eu

NAT_BRIDGE="${NAT_BRIDGE:-lxdbr0}"
NAT_SUBNET="${NAT_SUBNET:-10.180.0.1/24}"   # bridge address, /24 by default
EXISTING_BRIDGE="${EXISTING_BRIDGE:-}"      # e.g. docker0/br0: reuse it, skip creation
POOL="${POOL:-default}"
# Pool driver. Disk quotas (and a correct in-guest `df`) need a real volume
# driver: lvm (thin, loop-backed — the default here) or btrfs. The dir driver
# silently ignores the root-disk size and guests see the host filesystem.
POOL_DRIVER="${POOL_DRIVER:-lvm}"
POOL_SIZE="${POOL_SIZE:-}"                  # loop file size for lvm/btrfs, e.g. 100GiB; empty = incus default (~20%%, min 5GiB)
V6_BRIDGE="${V6_BRIDGE:-}"                  # e.g. br0; leave empty to skip
V6_CIDR="${V6_CIDR:-}"                      # e.g. 2a01:4f8:1:2::/64
V4_BRIDGE="${V4_BRIDGE:-}"                  # dedicated public IPv4 bridge; leave empty to skip
V4_CIDR="${V4_CIDR:-}"                      # e.g. 203.0.113.0/28
V4_GW="${V4_GW:-}"                          # e.g. 203.0.113.1
KVM="${KVM:-0}"                             # 1 = also install QEMU for KVM guests (beta)

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mxx\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "please run as root"

# ---------- install Incus ----------
#
# cub-panel drives Incus (the community fork of LXD; both speak the same REST
# API). Fresh nodes get Incus from the distro repos — no snap needed. An
# existing LXD install keeps working: we just reuse its CLI.
# $LXC holds the CLI name (incus or lxc) for the rest of this script.

LXC=""
if command -v incus >/dev/null 2>&1; then
	LXC=incus; say "Incus already present"
elif command -v lxc >/dev/null 2>&1; then
	LXC=lxc; say "existing LXD install found — reusing it"
elif command -v apk >/dev/null 2>&1; then
	# incus lives in the community repository; enable it if commented out.
	if ! grep -Eq '^[^#].*/community$' /etc/apk/repositories; then
		if grep -Eq '^#.*/community$' /etc/apk/repositories; then
			say "enabling the community repository in /etc/apk/repositories"
			sed -Ei 's|^#(.*/community)$|\1|' /etc/apk/repositories
		else
			die "the community repository is not configured in /etc/apk/repositories; add it, then re-run"
		fi
	fi
	say "installing Incus via apk"
	apk update -q
	apk add --no-cache incus incus-client incus-openrc lxcfs iptables ip6tables dbus
	LXC=incus
	rc-update add incusd default >/dev/null 2>&1 || true
	rc-update add lxcfs  default >/dev/null 2>&1 || true
	rc-service lxcfs  start >/dev/null 2>&1 || true
	rc-service incusd start >/dev/null 2>&1 || true
elif command -v apt-get >/dev/null 2>&1; then
	# Debian 13+ and Ubuntu 24.04+ ship incus natively. Debian 12 carries it
	# in backports; anything older falls back to the upstream package repo
	# (pkgs.zabbly.com, run by the Incus maintainer).
	say "installing Incus via apt"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	if apt-get install -y -qq incus >/dev/null 2>&1; then
		:
	else
		VERSION_CODENAME=""
		[ -r /etc/os-release ] && . /etc/os-release
		[ -n "${VERSION_CODENAME:-}" ] || die "cannot detect the distro codename; install Incus manually, then re-run"
		if [ "$VERSION_CODENAME" = "bookworm" ]; then
			say "incus is not in the main repos; enabling bookworm-backports"
			echo "deb http://deb.debian.org/debian bookworm-backports main" \
				> /etc/apt/sources.list.d/incus-backports.list
			apt-get update -qq
			apt-get install -y -qq -t bookworm-backports incus
		else
			say "incus is not in the distro repos; adding the upstream Zabbly repository"
			apt-get install -y -qq curl ca-certificates
			mkdir -p /etc/apt/keyrings
			curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc
			cat > /etc/apt/sources.list.d/zabbly-incus-stable.sources <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: $VERSION_CODENAME
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
			apt-get update -qq
			apt-get install -y -qq incus
		fi
	fi
	LXC=incus
	systemctl enable --now incus >/dev/null 2>&1 || true
else
	die "unsupported distro: install Incus manually, then re-run"
fi

# Wait for the daemon to accept commands.
say "waiting for the $LXC daemon"
i=0
while ! "$LXC" info >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -gt 30 ] && die "the daemon did not come up; check its logs"
	sleep 1
done

# ---------- optional KVM support (beta) ----------
#
# Off by default. KVM guests need /dev/kvm (bare metal or nested virt) and
# the QEMU stack; opt in with KVM=1. Remember to also tick 「支持 KVM」 on
# this node in the panel.

if [ "$KVM" = "1" ]; then
	say "installing KVM/QEMU support (beta)"
	[ -e /dev/kvm ] || warn "/dev/kvm not present — KVM guests will NOT run on this host (needs bare metal or nested virtualization)"
	if command -v apk >/dev/null 2>&1; then
		apk add --no-cache incus-vm || warn "incus-vm install failed"
	elif command -v apt-get >/dev/null 2>&1; then
		apt-get install -y -qq qemu-system-x86 qemu-utils ovmf || warn "qemu install failed"
	fi
	rc-service incusd restart >/dev/null 2>&1 || systemctl restart incus >/dev/null 2>&1 || true
	say "KVM support installed; tick 「支持 KVM (Beta)」 on this node in the panel"
fi

# ---------- host /dev sanity ----------
#
# Some VPS images ship /dev/null & friends as 0660. Unprivileged containers
# bind-mount these host nodes, so container root (a mapped uid) cannot write
# to them and every in-guest ">/dev/null" fails. Normalise and persist.

if [ "$(stat -c %a /dev/null 2>/dev/null)" != "666" ]; then
	say "fixing restrictive /dev node permissions (this breaks containers)"
	chmod 666 /dev/null /dev/zero /dev/full /dev/random /dev/urandom /dev/tty /dev/ptmx 2>/dev/null || true
	if [ -d /etc/local.d ]; then
		cat > /etc/local.d/fix-dev-perms.start <<'EOF'
#!/bin/sh
# Unprivileged containers bind-mount host /dev nodes; keep them world-writable.
chmod 666 /dev/null /dev/zero /dev/full /dev/random /dev/urandom /dev/tty /dev/ptmx 2>/dev/null || true
EOF
		chmod +x /etc/local.d/fix-dev-perms.start
		rc-update add local default >/dev/null 2>&1 || true
	fi
fi

# ---------- storage pool ----------

# LVM thin pools need the dm-thin kernel module, the LVM userspace and
# mkfs.ext4; per-instance volumes then enforce the plan's disk size and the
# guest's `df` reports it exactly.
if [ "$POOL_DRIVER" = "lvm" ]; then
	if modprobe dm_thin_pool 2>/dev/null; then
		echo dm_thin_pool >> /etc/modules-load.d/cub-panel.conf 2>/dev/null || true
		sort -u /etc/modules-load.d/cub-panel.conf -o /etc/modules-load.d/cub-panel.conf 2>/dev/null || true
		if command -v apk >/dev/null 2>&1; then
			apk add -q lvm2 thin-provisioning-tools e2fsprogs e2fsprogs-extra
		else
			DEBIAN_FRONTEND=noninteractive apt-get install -y -q lvm2 thin-provisioning-tools e2fsprogs >/dev/null
		fi
	else
		warn "kernel lacks dm_thin_pool — falling back to btrfs"
		POOL_DRIVER=btrfs
	fi
fi
if [ "$POOL_DRIVER" = "btrfs" ]; then
	if modprobe btrfs 2>/dev/null || grep -qw btrfs /proc/filesystems; then
		if command -v apk >/dev/null 2>&1; then apk add -q btrfs-progs; else DEBIAN_FRONTEND=noninteractive apt-get install -y -q btrfs-progs >/dev/null; fi
	else
		warn "kernel lacks btrfs too — falling back to dir. Disk sizes will NOT be enforced"
		warn "and guests will see the host filesystem in df. Fix the kernel and re-run with POOL_DRIVER=lvm."
		POOL_DRIVER=dir
	fi
fi

if "$LXC" storage show "$POOL" >/dev/null 2>&1; then
	say "storage pool '$POOL' already exists ($("$LXC" storage show "$POOL" | sed -n 's/^driver: //p'))"
else
	say "creating storage pool '$POOL' ($POOL_DRIVER${POOL_SIZE:+, $POOL_SIZE})"
	if [ -n "$POOL_SIZE" ] && [ "$POOL_DRIVER" != "dir" ]; then
		"$LXC" storage create "$POOL" "$POOL_DRIVER" size="$POOL_SIZE"
	else
		"$LXC" storage create "$POOL" "$POOL_DRIVER"
	fi
fi

# ---------- NAT bridge ----------

UNMANAGED=0
if [ -n "$EXISTING_BRIDGE" ]; then
	# Reuse a bridge that already exists on the host (docker0, br0…). Incus
	# will attach NICs to it but not manage addressing — the panel configures
	# instance IPs in-guest, so mark the node "非托管" there.
	NAT_BRIDGE="$EXISTING_BRIDGE"
	UNMANAGED=1
	ip link show "$NAT_BRIDGE" >/dev/null 2>&1 || die "bridge '$NAT_BRIDGE' does not exist on this host"
	say "reusing existing (unmanaged) bridge '$NAT_BRIDGE'"
	# Best effort: read the bridge's own address for the summary below.
	BR_CIDR="$(ip -4 -o addr show dev "$NAT_BRIDGE" 2>/dev/null | head -1 | sed 's/.*inet \([0-9./]*\).*/\1/')"
	[ -n "$BR_CIDR" ] && NAT_SUBNET="$BR_CIDR"
elif "$LXC" network show "$NAT_BRIDGE" >/dev/null 2>&1; then
	say "network '$NAT_BRIDGE' already exists"
else
	say "creating NAT bridge '$NAT_BRIDGE' on $NAT_SUBNET"
	"$LXC" network create "$NAT_BRIDGE" \
		ipv4.address="$NAT_SUBNET" \
		ipv4.nat=true \
		ipv6.address=none
fi

# ---------- firewall coexistence ----------
#
# Docker (and some hardening scripts) set the iptables FORWARD policy to
# DROP, which silently kills all egress from the NAT bridge even though the
# Incus nftables NAT rules are in place. Insert explicit accepts and persist
# them via /etc/local.d on OpenRC hosts.

if command -v iptables >/dev/null 2>&1 && iptables -S FORWARD 2>/dev/null | grep -q '^-P FORWARD DROP'; then
	say "FORWARD policy is DROP (Docker?); allowing traffic for '$NAT_BRIDGE'"
	iptables -C FORWARD -i "$NAT_BRIDGE" -j ACCEPT 2>/dev/null || iptables -I FORWARD -i "$NAT_BRIDGE" -j ACCEPT
	iptables -C FORWARD -o "$NAT_BRIDGE" -j ACCEPT 2>/dev/null || iptables -I FORWARD -o "$NAT_BRIDGE" -j ACCEPT
	if [ -d /etc/local.d ]; then
		cat > /etc/local.d/incus-forward.start <<EOF
#!/bin/sh
# Docker sets the FORWARD policy to DROP; allow the Incus NAT bridge through.
iptables -C FORWARD -i $NAT_BRIDGE -j ACCEPT 2>/dev/null || iptables -I FORWARD -i $NAT_BRIDGE -j ACCEPT
iptables -C FORWARD -o $NAT_BRIDGE -j ACCEPT 2>/dev/null || iptables -I FORWARD -o $NAT_BRIDGE -j ACCEPT
EOF
		chmod +x /etc/local.d/incus-forward.start
		rc-update add local default >/dev/null 2>&1 || true
	fi
fi

# ---------- default profile ----------

say "wiring the default profile to '$POOL' and '$NAT_BRIDGE'"
"$LXC" profile device remove default root >/dev/null 2>&1 || true
"$LXC" profile device remove default eth0 >/dev/null 2>&1 || true
"$LXC" profile device add default root disk path=/ pool="$POOL" >/dev/null
"$LXC" profile device add default eth0 nic nictype=bridged parent="$NAT_BRIDGE" name=eth0 >/dev/null

# ---------- optional IPv6 bridge ----------

if [ -n "$V6_BRIDGE" ]; then
	if ip link show "$V6_BRIDGE" >/dev/null 2>&1; then
		say "IPv6 bridge '$V6_BRIDGE' already exists on the host"
	else
		warn "bridge '$V6_BRIDGE' does not exist yet."
		warn "Create it in your host's network config and attach the uplink that"
		warn "carries your routed IPv6 prefix, for example on Alpine:"
		cat <<EOF

  # /etc/network/interfaces
  auto $V6_BRIDGE
  iface $V6_BRIDGE inet6 static
      bridge_ports eth0
      address ${V6_CIDR%/*}1
      netmask ${V6_CIDR#*/}

EOF
	fi
	say "in the panel, set this node's IPv6 bridge to '$V6_BRIDGE'${V6_CIDR:+ and range to '$V6_CIDR'}"
fi

# ---------- optional dedicated public IPv4 bridge ----------
#
# For 独立公网 IPv4 plans: bridge the uplink carrying your routed/on-link
# public v4 block into V4_BRIDGE, then set this node's v4 bridge + CIDR + gw
# in the panel. Like IPv6, the panel assigns addresses and the guest applies
# them statically. This script only prints guidance; wiring the uplink is
# host-specific.
if [ -n "${V4_BRIDGE:-}" ]; then
	if ip link show "$V4_BRIDGE" >/dev/null 2>&1; then
		say "public IPv4 bridge '$V4_BRIDGE' already exists on the host"
	else
		warn "bridge '$V4_BRIDGE' does not exist yet — create it and attach the"
		warn "uplink carrying your public IPv4 block, then set it in the panel."
	fi
	say "in the panel, enable dedicated IPv4 and set bridge '$V4_BRIDGE'${V4_CIDR:+, CIDR '$V4_CIDR'}${V4_GW:+, gw '$V4_GW'}"
fi

# ---------- summary ----------

echo
say "node is ready. Verify with:"
echo "    $LXC network show $NAT_BRIDGE"
echo "    $LXC storage show $POOL"
echo
say "panel node settings for this host:"
printf '    NAT 网桥   : %s\n' "$NAT_BRIDGE"
printf '    NAT 子网   : %s\n' "$(echo "$NAT_SUBNET" | sed 's#[0-9]*/#0/#')"
printf '    存储池     : %s\n' "$POOL"
if [ "$UNMANAGED" = "1" ]; then
	echo
	warn "共用网桥模式：在面板节点设置里——"
	warn "  1. 取消勾选「网桥由 Incus 管理」"
	printf '\033[33m!!\033[0m   2. NAT 网关填 %s\n' "${NAT_SUBNET%/*}"
	warn "  3. 在「保留 IP 段」里填入宿主机/Docker 已占用的地址段，避免分配冲突"
fi
echo
say "next: run ./install-agent.sh on this host"
