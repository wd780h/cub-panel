package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"cubpanel/internal/lxd"
	"cubpanel/internal/shared"
)

// nicPlan assigns device names per network mode. eth0 is the primary NIC:
// the NAT bridge when the mode uses NAT, otherwise the single dedicated NIC.
type nicPlan struct {
	NATDev string // internal NAT NIC ("" when the mode has no NAT)
	DV4Dev string // dedicated public IPv4 NIC
	DV6Dev string // dedicated IPv6 NIC
}

func planNICs(mode shared.NetMode) nicPlan {
	next := 0
	name := func() string { n := fmt.Sprintf("eth%d", next); next++; return n }
	var p nicPlan
	if shared.ModeHasNAT(mode) {
		p.NATDev = name()
	}
	if shared.ModeHasDV4(mode) {
		p.DV4Dev = name()
	}
	if shared.ModeHasDV6(mode) {
		p.DV6Dev = name()
	}
	return p
}

// buildDevices renders the LXD device set for a create request.
func (s *Server) buildDevices(req *shared.CreateRequest) map[string]lxd.Device {
	// The panel's per-node pool wins; the agent env is the fallback so old
	// panels (no storage_pool in the request) keep working.
	pool := req.StoragePool
	if pool == "" {
		pool = s.cfg.StoragePool
	}
	dev := map[string]lxd.Device{
		"root": {
			"type": "disk",
			"path": "/",
			"pool": pool,
			"size": fmt.Sprintf("%dGB", req.DiskGB),
		},
	}

	// Host-directory binds (admin-defined per plan). Paths were validated in
	// the panel; re-check here so a compromised panel cannot mount arbitrary
	// junk like relative or dot-dot paths. shift maps ownership into the
	// guest's user namespace (containers only).
	for i, m := range req.Mounts {
		if !strings.HasPrefix(m.Source, "/") || !strings.HasPrefix(m.Path, "/") ||
			strings.Contains(m.Source, "..") || strings.Contains(m.Path, "..") || m.Path == "/" {
			s.log("skipping invalid mount %q -> %q", m.Source, m.Path)
			continue
		}
		d := lxd.Device{"type": "disk", "source": m.Source, "path": m.Path}
		if m.ReadOnly {
			d["readonly"] = "true"
		}
		if req.InstanceType != "vm" {
			d["shift"] = "true"
		}
		dev[fmt.Sprintf("hostmnt%d", i)] = d
	}

	// Extra data disks: custom pool volumes mounted at /data1, /data2, … .
	// VMs receive them through the Incus guest agent (virtiofs/9p).
	for i := range req.ExtraDisks {
		if i >= 4 {
			break
		}
		dev[fmt.Sprintf("data%d", i+1)] = lxd.Device{
			"type":   "disk",
			"pool":   pool,
			"source": extraDiskVolume(req.Name, i),
			"path":   fmt.Sprintf("/data%d", i+1),
		}
	}
	plan := planNICs(req.Mode)
	nic := func(dname, bridge string) {
		dev[dname] = withRateLimits(lxd.Device{
			"type": "nic", "nictype": "bridged", "parent": bridge, "name": dname,
		}, req.RateDownMbps, req.RateUpMbps)
	}

	if plan.NATDev != "" {
		nic(plan.NATDev, req.NATBridge)
		// Managed bridge: pin the lease so DNAT targets survive reboots.
		if req.NATAddr != "" && req.NATManaged {
			dev[plan.NATDev]["ipv4.address"] = req.NATAddr
		}
		s.addPortForwards(dev, req)
	}
	if plan.DV4Dev != "" {
		nic(plan.DV4Dev, req.V4Bridge) // address configured in-guest
	}
	if plan.DV6Dev != "" {
		nic(plan.DV6Dev, req.V6Bridge)
	}

	s.addFeatureDevices(dev, req)
	s.addExtraBridges(dev, req)
	return dev
}

// isWildcardNATErr matches the Incus refusal of a wildcard listen in nat mode.
func isWildcardNATErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "wildcard address") &&
		strings.Contains(err.Error(), "nat")
}

// hostPrimaryIP returns the host's primary outbound IPv4, cached — the
// fallback listen address when Incus rejects the wildcard. The UDP
// "connection" sends no packets: it only asks the kernel which source address
// the default route would pick. On NAT-fronted hosts this is the private NIC
// address, which is why it is a fallback rather than the default.
var (
	hostIPOnce sync.Once
	hostIPVal  string
)

func hostPrimaryIP() string {
	hostIPOnce.Do(func() {
		conn, err := net.Dial("udp", "8.8.8.8:80")
		if err != nil {
			return
		}
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && a.IP.To4() != nil {
			hostIPVal = a.IP.String()
		}
	})
	return hostIPVal
}

// addPortForwards installs the SSH + block forwards for a NAT instance.
// nat=true keeps the client's real source IP (DNAT); a plain proxy hides it
// behind the host. On unmanaged bridges the agent's own iptables DNAT does
// the source-preserving job instead (see nat.go).
func (s *Server) addPortForwards(dev map[string]lxd.Device, req *shared.CreateRequest) {
	if s.useAgentDNAT(req) {
		return // agent-managed iptables DNAT handles it
	}
	target := req.NATAddr
	if target == "" {
		target = "127.0.0.1"
	}
	// nat=true = DNAT (source preserved). Only on a managed bridge, and only
	// when the plan wants the real source IP shown.
	//
	// Listen on the wildcard: it covers every host address, which is what
	// tenants expect and what NAT-fronted hosts (public IP mapped upstream,
	// private address on the NIC) require — binding one probed address there
	// yields a port nothing can reach. Some Incus builds reject a wildcard
	// listen in nat mode; buildDevices' caller retries those via
	// natListenFallback below.
	useNAT := req.NATManaged && req.KeepSourceIP
	proxy := func(proto, listen, connect string) lxd.Device {
		d := lxd.Device{
			"type":    "proxy",
			"listen":  fmt.Sprintf("%s:%s:%s", proto, s.natListenIP, listen),
			"connect": fmt.Sprintf("%s:%s:%s", proto, target, connect),
		}
		if useNAT {
			d["nat"] = "true"
		}
		return d
	}
	if req.SSHPort > 0 {
		dev["panel-ssh"] = proxy("tcp", fmt.Sprintf("%d", req.SSHPort), "22")
	}
	if req.PortFrom > 0 && req.PortTo >= req.PortFrom {
		rng := fmt.Sprintf("%d-%d", req.PortFrom, req.PortTo)
		dev["panel-tcp"] = proxy("tcp", rng, rng)
		dev["panel-udp"] = proxy("udp", rng, rng)
	}
}

// vncQemuArgs builds the self-contained VNC display for a VM: a QEMU secret
// object carries the password so no QMP round-trip is needed. display =
// port-5900. Empty when the request asks for no VNC.
func (s *Server) vncQemuArgs(req *shared.CreateRequest) []string {
	if req.VNCPort < 5900 || req.VNCPass == "" {
		return nil
	}
	display := req.VNCPort - 5900
	return []string{
		"-object", fmt.Sprintf("secret,id=vncsec0,data=%s", req.VNCPass),
		"-vnc", fmt.Sprintf("0.0.0.0:%d,password-secret=vncsec0", display),
	}
}

// addExtraBridges attaches additional bridged NICs after the mode's own NICs.
func (s *Server) addExtraBridges(dev map[string]lxd.Device, req *shared.CreateRequest) {
	base := 0 // count of NICs the mode already placed
	for _, d := range dev {
		if d["type"] == "nic" {
			base++
		}
	}
	for i, br := range req.ExtraBridges {
		if br == "" {
			continue
		}
		name := fmt.Sprintf("eth%d", base+i)
		dev[name] = withRateLimits(lxd.Device{
			"type":    "nic",
			"nictype": "bridged",
			"parent":  br,
			"name":    name,
		}, req.RateDownMbps, req.RateUpMbps)
	}
}

// useAgentDNAT reports whether forwarding is done through agent-managed
// iptables rules rather than incus proxy devices. Requires a NAT mode, a
// foreign bridge with iptables, a static lease, and a plan that wants the
// real source IP preserved.
func (s *Server) useAgentDNAT(req *shared.CreateRequest) bool {
	return shared.ModeHasNAT(req.Mode) && !req.NATManaged && s.hasIPT &&
		req.NATAddr != "" && req.KeepSourceIP
}

// withRateLimits attaches Incus's tc-backed NIC bandwidth caps. ingress is
// traffic INTO the container (the tenant's download), egress is out.
func withRateLimits(nic lxd.Device, downMbps, upMbps int) lxd.Device {
	if downMbps > 0 {
		nic["limits.ingress"] = fmt.Sprintf("%dMbit", downMbps)
	}
	if upMbps > 0 {
		nic["limits.egress"] = fmt.Sprintf("%dMbit", upMbps)
	}
	return nic
}

// addFeatureDevices maps plan feature flags onto extra devices. VMs have a
// full kernel of their own, so the passthrough devices are container-only.
func (s *Server) addFeatureDevices(dev map[string]lxd.Device, req *shared.CreateRequest) {
	if isVM(req) {
		return
	}
	for _, f := range req.Features {
		switch f {
		case "tun":
			dev["tun"] = lxd.Device{"type": "unix-char", "source": "/dev/net/tun", "path": "/dev/net/tun"}
		case "fuse":
			dev["fuse"] = lxd.Device{"type": "unix-char", "source": "/dev/fuse", "path": "/dev/fuse"}
		}
	}
}

func hasFeature(req *shared.CreateRequest, want string) bool {
	for _, f := range req.Features {
		if f == want {
			return true
		}
	}
	return false
}

// isVM reports whether the request asks for a KVM virtual machine (beta).
func isVM(req *shared.CreateRequest) bool { return req.InstanceType == shared.TypeVM }

// Create provisions, starts and configures an instance end to end.
func (s *Server) Create(ctx context.Context, req *shared.CreateRequest) error {
	instType := "container"
	cfg := map[string]string{
		"limits.cpu":     fmt.Sprintf("%d", req.CPU),
		"limits.memory":  fmt.Sprintf("%dMB", req.MemoryMB),
		"user.cub-panel": "1",
	}
	if isVM(req) {
		// KVM (beta). Same image aliases — Incus resolves the VM variant —
		// and the container-only security/limit knobs simply do not apply.
		instType = "virtual-machine"
		if hasFeature(req, "nesting") {
			cfg["security.secureboot"] = "false"
			cfg["limits.cpu.nested"] = "true" // best-effort; ignored on old Incus
		}
		// CPU flags: aes exposes host AES-NI; cpuhide uses a generic model
		// to mask the hypervisor vendor from the guest.
		qemuExtra := s.vncQemuArgs(req)
		if hasFeature(req, "aes") {
			cfg["limits.cpu.hypervisor.aes"] = "true"
		}
		if hasFeature(req, "cpuhide") {
			// Generic qemu64 model + hidden KVM vendor string.
			qemuExtra = append(qemuExtra, "-cpu", "qemu64,-hypervisor,kvm=off")
		}
		if len(qemuExtra) > 0 {
			cfg["raw.qemu"] = strings.Join(qemuExtra, " ")
		}
	} else {
		cfg["limits.memory.enforce"] = "hard"
		// Escape hatches stay closed unless the plan explicitly opts in.
		cfg["security.nesting"] = fmt.Sprintf("%v", hasFeature(req, "nesting"))
		cfg["security.privileged"] = fmt.Sprintf("%v", hasFeature(req, "privileged"))
		cfg["security.syscalls.intercept.mknod"] = "false"
	}
	post := lxd.InstancesPost{
		Name: req.Name,
		Type: instType,
		Source: lxd.InstanceSource{
			Type:     "image",
			Alias:    req.Image,
			Protocol: "simplestreams",
			Server:   s.cfg.ImageServer,
		},
		Config:   cfg,
		Devices:  s.buildDevices(req),
		Profiles: []string{"default"},
	}
	// Record the DNAT mapping on the instance itself so RestoreDNAT can
	// replay the rules after a host reboot.
	if s.useAgentDNAT(req) {
		post.Config["user.cub-panel.dnat"] = dnatSpec{
			Addr: req.NATAddr, SSHPort: req.SSHPort,
			PortFrom: req.PortFrom, PortTo: req.PortTo,
		}.String()
	}
	// Data volumes must exist before the instance references them as devices.
	diskPool := req.StoragePool
	if diskPool == "" {
		diskPool = s.cfg.StoragePool
	}
	if len(req.ExtraDisks) > 0 {
		if err := s.createExtraDisks(ctx, diskPool, req.Name, req.ExtraDisks); err != nil {
			return err
		}
	}
	if err := s.lxd.Create(ctx, post); err != nil {
		// Some Incus builds reject a wildcard listen in nat mode. Bind the
		// host's primary address instead and retry once — the wildcard stays
		// the default because it is what NAT-fronted hosts need.
		if isWildcardNATErr(err) && s.natListenIP == "0.0.0.0" {
			if ip := hostPrimaryIP(); ip != "" {
				s.log("instance %s: wildcard nat listen refused, retrying on %s", req.Name, ip)
				s.natListenIP = ip
				post.Devices = s.buildDevices(req)
				err = s.lxd.Create(ctx, post)
			}
		}
		if err != nil {
			if len(req.ExtraDisks) > 0 {
				s.deleteExtraDisks(ctx, diskPool, req.Name)
			}
			return fmt.Errorf("create: %w", err)
		}
	}
	if s.useAgentDNAT(req) {
		if err := s.applyDNAT(ctx, req.Name, dnatSpec{
			Addr: req.NATAddr, SSHPort: req.SSHPort,
			PortFrom: req.PortFrom, PortTo: req.PortTo,
		}); err != nil {
			s.log("instance %s: dnat: %v", req.Name, err)
		}
	}
	if err := s.lxd.SetState(ctx, req.Name, "start", false); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	netWait := 90 * time.Second
	if isVM(req) {
		// First VM boot includes firmware + guest agent start.
		netWait = 240 * time.Second
	}
	if shared.ModeHasNAT(req.Mode) && req.NATManaged {
		if err := s.waitNetwork(ctx, req.Name, netWait); err != nil {
			// Not fatal: the instance exists and the operator can retry setup.
			s.log("instance %s: network wait: %v", req.Name, err)
		}
	} else {
		// No DHCP on this path — the guest gets a static address below. Just
		// wait until the container reports Running.
		if err := s.waitRunning(ctx, req.Name, 30*time.Second); err != nil {
			s.log("instance %s: run wait: %v", req.Name, err)
		}
	}
	if err := s.provisionGuest(ctx, req); err != nil {
		return fmt.Errorf("guest setup: %w", err)
	}
	return nil
}

// waitRunning blocks until the instance reports the Running state.
func (s *Server) waitRunning(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := s.lxd.GetState(ctx, name); err == nil && st.Status == "Running" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for Running")
}

// waitNetwork blocks until the instance reports a routable eth0 address.
func (s *Server) waitNetwork(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.lxd.GetState(ctx, name)
		if err == nil && st.Status == "Running" {
			if n, ok := st.Network["eth0"]; ok {
				for _, a := range n.Addresses {
					if a.Family == "inet" && a.Scope == "global" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timed out waiting for eth0")
}

// provisionGuest sets the root password, installs sshd and pins the static
// IPv6 address. Every dynamic value travels as an environment variable, never
// interpolated into the script text, so a hostile password or address cannot
// break out into shell syntax.
func (s *Server) provisionGuest(ctx context.Context, req *shared.CreateRequest) error {
	env := map[string]string{
		"ICP_PW":          req.RootPassword,
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DEBIAN_FRONTEND": "noninteractive",
	}
	plan := planNICs(req.Mode)
	// Internal NAT static — only for foreign bridges that can't hand out the
	// panel-chosen lease themselves (docker0…).
	if plan.NATDev != "" && !req.NATManaged && req.NATAddr != "" {
		env["ICP_V4"] = req.NATAddr
		env["ICP_V4PFX"] = fmt.Sprintf("%d", req.NATPrefix)
		env["ICP_V4GW"] = req.NATGW
		env["ICP_V4DEV"] = plan.NATDev
	}
	// Dedicated public IPv4 — always configured in-guest on its NIC.
	if plan.DV4Dev != "" && req.V4Addr != "" {
		env["ICP_PUB4"] = req.V4Addr
		env["ICP_PUB4PFX"] = fmt.Sprintf("%d", req.V4Prefix)
		env["ICP_PUB4GW"] = req.V4GW
		env["ICP_PUB4DEV"] = plan.DV4Dev
	}
	// Dedicated IPv6.
	if plan.DV6Dev != "" && req.V6Addr != "" {
		env["ICP_V6"] = req.V6Addr
		env["ICP_V6PFX"] = fmt.Sprintf("%d", req.V6Prefix)
		env["ICP_V6GW"] = req.V6GW
		env["ICP_V6DEV"] = plan.DV6Dev
	}
	// Modes without NAT and without a dedicated v4 gateway have no IPv4 route,
	// so seed a public DNS the guest can reach over its available stack.
	if !shared.ModeHasNAT(req.Mode) && !shared.ModeHasDV4(req.Mode) {
		env["ICP_DNS"] = "2606:4700:4700::1111 2001:4860:4860::8888"
	}
	// Operator-chosen resolvers override any default.
	if req.DNS != "" {
		env["ICP_DNS"] = strings.ReplaceAll(req.DNS, ",", " ")
	}

	script := scriptDebian
	if osFamilyFor(req.Image, req.Family) == shared.OSAlpine {
		script = scriptAlpine
	}
	if isVM(req) {
		// Exec inside a VM goes through the Incus guest agent that boots with
		// the guest; give it time to come up before the real setup script runs.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if err := s.lxd.ExecOnce(ctx, req.Name, []string{"/bin/sh", "-c", "true"}, nil); err == nil {
				break
			} else if time.Now().After(deadline) {
				return fmt.Errorf("vm guest agent did not come up: %w", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	return s.lxd.ExecScript(ctx, req.Name, script, env)
}

// osFamilyFor guesses the provisioning recipe from an image alias when the
// panel did not state one.
func osFamilyFor(image string, stated shared.OSFamily) shared.OSFamily {
	if stated == shared.OSAlpine || stated == shared.OSDebian {
		return stated
	}
	if strings.HasPrefix(strings.ToLower(image), "alpine") {
		return shared.OSAlpine
	}
	return shared.OSDebian
}

// The two provisioning recipes. Both are POSIX sh and read their inputs from
// the environment. IPv6 is applied immediately and persisted through the
// distro's own boot mechanism so it survives a reboot.

// scriptCommonNetApply applies whichever static addresses were handed in and,
// when any exist, persists them through /etc/cub-panel-net.env plus a small
// apply script that the distro-specific boot hook runs.
const scriptCommonNetApply = `
apply_net() {
  # internal NAT static (foreign bridges), device via ICP_V4DEV
  if [ -n "$ICP_V4" ]; then
    D="${ICP_V4DEV:-eth0}"
    ip link set "$D" up 2>/dev/null
    ip addr replace "$ICP_V4/$ICP_V4PFX" dev "$D" 2>/dev/null
    [ -n "$ICP_V4GW" ] && ip route replace default via "$ICP_V4GW" dev "$D" 2>/dev/null
  fi
  # dedicated public IPv4
  if [ -n "$ICP_PUB4" ]; then
    D="${ICP_PUB4DEV:-eth1}"
    ip link set "$D" up 2>/dev/null
    ip addr replace "$ICP_PUB4/$ICP_PUB4PFX" dev "$D" 2>/dev/null
    [ -n "$ICP_PUB4GW" ] && ip route replace default via "$ICP_PUB4GW" dev "$D" 2>/dev/null
  fi
  # dedicated IPv6
  if [ -n "$ICP_V6" ]; then
    D="${ICP_V6DEV:-eth1}"
    ip link set "$D" up 2>/dev/null
    ip -6 addr replace "$ICP_V6/$ICP_V6PFX" dev "$D" 2>/dev/null
    [ -n "$ICP_V6GW" ] && ip -6 route replace default via "$ICP_V6GW" dev "$D" 2>/dev/null
  fi
  [ -n "$ICP_DNS" ] && printf 'nameserver %s\n' $ICP_DNS > /etc/resolv.conf 2>/dev/null
  return 0
}
apply_net || true
if [ -n "$ICP_V4" ] || [ -n "$ICP_PUB4" ] || [ -n "$ICP_V6" ] || [ -n "$ICP_DNS" ]; then
  printf 'ICP_V4=%s\nICP_V4PFX=%s\nICP_V4GW=%s\nICP_V4DEV=%s\nICP_PUB4=%s\nICP_PUB4PFX=%s\nICP_PUB4GW=%s\nICP_PUB4DEV=%s\nICP_V6=%s\nICP_V6PFX=%s\nICP_V6GW=%s\nICP_V6DEV=%s\nICP_DNS=%s\n' \
    "$ICP_V4" "$ICP_V4PFX" "$ICP_V4GW" "${ICP_V4DEV:-eth0}" \
    "$ICP_PUB4" "$ICP_PUB4PFX" "$ICP_PUB4GW" "${ICP_PUB4DEV:-eth1}" \
    "$ICP_V6" "$ICP_V6PFX" "$ICP_V6GW" "${ICP_V6DEV:-eth1}" "$ICP_DNS" \
    > /etc/cub-panel-net.env
  chmod 600 /etc/cub-panel-net.env
  cat > /usr/local/sbin/cub-panel-net-apply <<'APPLY'
#!/bin/sh
. /etc/cub-panel-net.env
D="${ICP_V4DEV:-eth0}"
[ -n "$ICP_V4" ] && { ip link set "$D" up 2>/dev/null; ip addr replace "$ICP_V4/$ICP_V4PFX" dev "$D" 2>/dev/null; [ -n "$ICP_V4GW" ] && ip route replace default via "$ICP_V4GW" dev "$D" 2>/dev/null; }
D="${ICP_PUB4DEV:-eth1}"
[ -n "$ICP_PUB4" ] && { ip link set "$D" up 2>/dev/null; ip addr replace "$ICP_PUB4/$ICP_PUB4PFX" dev "$D" 2>/dev/null; [ -n "$ICP_PUB4GW" ] && ip route replace default via "$ICP_PUB4GW" dev "$D" 2>/dev/null; }
D="${ICP_V6DEV:-eth1}"
[ -n "$ICP_V6" ] && { ip link set "$D" up 2>/dev/null; ip -6 addr replace "$ICP_V6/$ICP_V6PFX" dev "$D" 2>/dev/null; [ -n "$ICP_V6GW" ] && ip -6 route replace default via "$ICP_V6GW" dev "$D" 2>/dev/null; }
[ -n "$ICP_DNS" ] && printf 'nameserver %s\n' $ICP_DNS > /etc/resolv.conf 2>/dev/null
exit 0
APPLY
  chmod +x /usr/local/sbin/cub-panel-net-apply
fi
`

const scriptDebian = `set -e
echo "root:$ICP_PW" | chpasswd
` + scriptCommonNetApply + `
if ! command -v sshd >/dev/null 2>&1; then
  apt-get update -qq >/dev/null 2>&1 || true
  apt-get install -y -qq --no-install-recommends openssh-server iproute2 >/dev/null 2>&1 || true
fi
if [ -f /etc/ssh/sshd_config ]; then
  sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
fi
if [ -x /usr/local/sbin/cub-panel-net-apply ]; then
  cat > /etc/systemd/system/cub-panel-net.service <<'UNIT'
[Unit]
Description=cub-panel static addressing
After=network.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/cub-panel-net-apply
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl enable cub-panel-net.service >/dev/null 2>&1 || true
fi
systemctl enable ssh >/dev/null 2>&1 || true
systemctl restart ssh >/dev/null 2>&1 || service ssh restart >/dev/null 2>&1 || true
`

const scriptAlpine = `set -e
echo "root:$ICP_PW" | chpasswd
` + scriptCommonNetApply + `
apk add --no-cache openssh iproute2 >/dev/null 2>&1 || true
if [ -f /etc/ssh/sshd_config ]; then
  sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
fi
if [ -x /usr/local/sbin/cub-panel-net-apply ]; then
  mkdir -p /etc/local.d
  ln -sf /usr/local/sbin/cub-panel-net-apply /etc/local.d/cub-panel-net.start
  rc-update add local default >/dev/null 2>&1 || true
fi
rc-update add sshd default >/dev/null 2>&1 || true
rc-service sshd restart >/dev/null 2>&1 || /usr/sbin/sshd >/dev/null 2>&1 || true
`
