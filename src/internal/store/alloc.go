package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
)

// allocMu serialises resource allocation across requests. Allocate reads what
// is in use and picks free addresses/ports, but the claim only becomes durable
// once the caller persists the instance row — so the lock must span BOTH, or
// two concurrent launches can be handed the same address. Callers hold it via
// AllocSection around the whole pick-then-persist critical section.
var allocMu sync.Mutex

// AllocSection runs fn while holding the allocation lock. Wrap Allocate
// together with the write that persists its claim (CreateInstance or the
// instance update); Allocate itself takes no lock.
func AllocSection(fn func() error) error {
	allocMu.Lock()
	defer allocMu.Unlock()
	return fn()
}

// Allocation is the set of network resources reserved for a new instance.
type Allocation struct {
	NATAddr  string
	SSHPort  int
	PortFrom int
	PortTo   int
	V6Addr   string
	V4Addr   string // dedicated public IPv4
	VNCPort  int
}

// AllocSpec says which resources a new instance needs.
type AllocSpec struct {
	WantNAT bool   // internal NAT IPv4 + a forwarded port block
	WantDV4 bool   // dedicated public IPv4 from the node's v4 pool
	WantDV6 bool   // dedicated public IPv6 from the node's v6 pool
	WantVNC bool   // one extra host port for a VM VNC display
	V4Pool  string // restrict the NAT internal address to this range (within the subnet)
	V6Pool  string // restrict dedicated IPv6 to this range (within the node v6 cidr)

	// Prefer* pin exact addresses when non-empty. The address must be free,
	// inside the node pool (and plan pool, if set), and not node-reserved.
	// Empty strings fall back to auto-pick.
	PreferNAT string
	PreferV4  string
	PreferV6  string

	// ExceptInstance, when > 0, treats that instance's current addresses and
	// ports as free — used when reassigning IPs on an existing instance.
	ExceptInstance int64

	// KeepPorts skips SSH/port-block allocation when WantNAT is set. Used by
	// admin IP reassignment so only addresses change.
	KeepPorts bool
}

// Allocate reserves the network resources named by spec. It fails rather than
// overcommit. Call inside AllocSection together with the write that persists
// the claim — the pick is not durable until the instance row exists.
func (d *DB) Allocate(ctx context.Context, node *Node, spec AllocSpec) (*Allocation, error) {
	used, err := d.nodeUsageExcept(ctx, node.ID, spec.ExceptInstance)
	if err != nil {
		return nil, err
	}
	// Capacity only applies when claiming a new slot. IP reassignment
	// (ExceptInstance set) keeps the existing row, so skip the check.
	if spec.ExceptInstance == 0 && used.count >= node.MaxInstances {
		return nil, errors.New("node is at capacity")
	}

	a := &Allocation{}

	if spec.WantNAT {
		// Node-reserved ranges + (optional) plan pool restriction.
		reserved, err := parseIPPool(node.NATReserved, false)
		if err != nil {
			return nil, fmt.Errorf("node nat_reserved: %w", err)
		}
		var inPool func(string) bool
		if spec.V4Pool != "" {
			inPool, err = parseIPPool(spec.V4Pool, false)
			if err != nil {
				return nil, fmt.Errorf("plan v4_pool: %w", err)
			}
		}
		avoid := func(s string) bool {
			if used.v4[s] || reserved(s) {
				return true
			}
			if inPool != nil && !inPool(s) { // outside the plan pool → skip
				return true
			}
			return false
		}
		if spec.PreferNAT != "" {
			addr, err := claimIPv4(node.NATSubnet, spec.PreferNAT, avoid)
			if err != nil {
				return nil, fmt.Errorf("指定内网 IPv4：%w", err)
			}
			a.NATAddr = addr
		} else if a.NATAddr, err = pickIPv4Func(node.NATSubnet, avoid); err != nil {
			return nil, err
		}
		if !spec.KeepPorts {
			if a.SSHPort, a.PortFrom, a.PortTo, err = pickPorts(node, used.ports); err != nil {
				return nil, err
			}
		}
	}
	if spec.WantDV4 {
		if !node.V4Enabled || node.V4CIDR == "" {
			return nil, errors.New("node has no dedicated IPv4 pool configured")
		}
		avoid := func(s string) bool { return used.dv4[s] }
		if spec.PreferV4 != "" {
			addr, err := claimIPv4(node.V4CIDR, spec.PreferV4, avoid)
			if err != nil {
				return nil, fmt.Errorf("指定公网 IPv4：%w", err)
			}
			a.V4Addr = addr
		} else if a.V4Addr, err = pickIPv4Func(node.V4CIDR, avoid); err != nil {
			return nil, fmt.Errorf("dedicated IPv4 pool exhausted: %w", err)
		}
	}
	if spec.WantDV6 {
		if !node.V6Enabled || node.V6CIDR == "" {
			return nil, errors.New("node has no IPv6 pool configured")
		}
		var inPool func(string) bool
		if spec.V6Pool != "" {
			inPool, err = parseIPPool(spec.V6Pool, true)
			if err != nil {
				return nil, fmt.Errorf("plan v6_pool: %w", err)
			}
		}
		avoid := func(s string) bool {
			if used.v6[canonV6(s)] {
				return true
			}
			if inPool != nil && !inPool(s) {
				return true
			}
			return false
		}
		if spec.PreferV6 != "" {
			addr, err := claimIPv6(node.V6CIDR, spec.PreferV6, avoid)
			if err != nil {
				return nil, fmt.Errorf("指定 IPv6：%w", err)
			}
			a.V6Addr = addr
		} else if a.V6Addr, err = pickIPv6Func(node.V6CIDR, avoid); err != nil {
			return nil, err
		}
	}
	if spec.WantVNC {
		for p := node.PortMin; p <= node.PortMax; p++ {
			if !used.ports[p] && p != a.SSHPort && !(p >= a.PortFrom && p <= a.PortTo) {
				a.VNCPort = p
				break
			}
		}
		if a.VNCPort == 0 {
			return nil, errors.New("no free VNC port on node")
		}
	}
	return a, nil
}

// parseReserved compiles an IPv4 reserved/pool spec. Kept as a thin wrapper
// around parseIPPool for call sites and tests that pre-date dual-stack pools.
func parseReserved(spec string) (func(string) bool, error) {
	return parseIPPool(spec, false)
}

// parseIPPool compiles a comma list of single IPs and inclusive ranges
// ("10.0.0.1-10.0.0.50,10.0.0.99" or "2001:db8::10-2001:db8::ff") into a
// membership test. wantV6 selects the address family.
func parseIPPool(spec string, wantV6 bool) (func(string) bool, error) {
	type span struct{ lo, hi netip.Addr }
	var spans []span
	for _, part := range splitTrim(spec, ",") {
		lo, hi, ok := splitRange(part)
		a1, err1 := netip.ParseAddr(lo)
		a2, err2 := netip.ParseAddr(hi)
		if !ok || err1 != nil || err2 != nil || a2.Less(a1) {
			return nil, fmt.Errorf("bad entry %q (want IP or IP-IP)", part)
		}
		if wantV6 {
			if !a1.Is6() || !a2.Is6() {
				return nil, fmt.Errorf("bad entry %q (want IPv6)", part)
			}
		} else if !a1.Is4() || !a2.Is4() {
			return nil, fmt.Errorf("bad entry %q (want IPv4)", part)
		}
		spans = append(spans, span{a1, a2})
	}
	return func(s string) bool {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return false
		}
		for _, sp := range spans {
			if !a.Less(sp.lo) && !sp.hi.Less(a) {
				return true
			}
		}
		return false
	}, nil
}

func splitTrim(s, sep string) []string {
	var out []string
	for _, v := range strings.Split(s, sep) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// splitRange splits "a-b" into its endpoints; a single value is both ends.
func splitRange(s string) (string, string, bool) {
	if i := strings.Index(s, "-"); i >= 0 {
		lo, hi := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		return lo, hi, lo != "" && hi != ""
	}
	return s, s, s != ""
}

// ValidateReserved checks an IPv4 reserved/pool-range spec at save time.
func ValidateReserved(spec string) error {
	_, err := parseIPPool(spec, false)
	return err
}

// ValidateIPPool checks a pool-range spec of the requested family at save time.
func ValidateIPPool(spec string, wantV6 bool) error {
	_, err := parseIPPool(spec, wantV6)
	return err
}

type usage struct {
	count int
	v4    map[string]bool // NAT internal addresses
	dv4   map[string]bool // dedicated public IPv4 addresses
	v6    map[string]bool
	ports map[int]bool // every individual port already handed out on this node
}

// nodeUsage reads back everything currently reserved on a node.
func (d *DB) nodeUsage(ctx context.Context, nodeID int64) (*usage, error) {
	return d.nodeUsageExcept(ctx, nodeID, 0)
}

// nodeUsageExcept is nodeUsage but omits one instance (so its addresses can be
// reassigned to itself when an admin changes IPs).
func (d *DB) nodeUsageExcept(ctx context.Context, nodeID, exceptID int64) (*usage, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, nat_addr, v6_addr, v4_addr, ssh_port, port_from, port_to, vnc_port FROM instances WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	u := &usage{v4: map[string]bool{}, dv4: map[string]bool{}, v6: map[string]bool{}, ports: map[int]bool{}}
	for rows.Next() {
		var id int64
		var v4, v6, dv4 string
		var ssh, from, to, vnc int
		if err := rows.Scan(&id, &v4, &v6, &dv4, &ssh, &from, &to, &vnc); err != nil {
			return nil, err
		}
		if exceptID > 0 && id == exceptID {
			// Still counts toward capacity (the row is not going away).
			u.count++
			continue
		}
		u.count++
		if v4 != "" {
			u.v4[v4] = true
		}
		if dv4 != "" {
			u.dv4[dv4] = true
		}
		if v6 != "" {
			u.v6[canonV6(v6)] = true
		}
		if ssh > 0 {
			u.ports[ssh] = true
		}
		if vnc > 0 {
			u.ports[vnc] = true
		}
		for p := from; p > 0 && p <= to; p++ {
			u.ports[p] = true
		}
	}
	return u, rows.Err()
}

// pickIPv4 returns the lowest free host address in the subnet. The first ten
// addresses are reserved for the bridge itself and any operator use.
func pickIPv4(cidr string, used map[string]bool) (string, error) {
	return pickIPv4Func(cidr, func(s string) bool { return used[s] })
}

func pickIPv4Func(cidr string, avoid func(string) bool) (string, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("node nat_subnet %q is not a valid CIDR", cidr)
	}
	if !pfx.Addr().Is4() {
		return "", errors.New("nat_subnet must be IPv4")
	}
	addr := pfx.Masked().Addr()
	// Skip network address + the reserved low range.
	for i := 0; i < 10; i++ {
		addr = addr.Next()
	}
	for pfx.Contains(addr) {
		s := addr.String()
		// Leave the broadcast address alone.
		if !avoid(s) && !isV4Broadcast(pfx, addr) {
			return s, nil
		}
		addr = addr.Next()
	}
	return "", errors.New("nat subnet exhausted")
}

func isV4Broadcast(pfx netip.Prefix, a netip.Addr) bool {
	b := a.As4()
	hostBits := 32 - pfx.Bits()
	if hostBits == 0 {
		return false
	}
	var host uint32
	for i := 0; i < 4; i++ {
		host = host<<8 | uint32(b[i])
	}
	mask := uint32(1)<<uint(hostBits) - 1
	return host&mask == mask
}

// pickPorts reserves one SSH port plus a contiguous forwarding block. The two
// come from opposite ends of the range so the blocks stay tidy.
func pickPorts(node *Node, used map[int]bool) (ssh, from, to int, err error) {
	lo, hi, each := node.PortMin, node.PortMax, node.PortsEach
	if lo < 1024 || hi > 65535 || hi <= lo {
		return 0, 0, 0, fmt.Errorf("node %s has an invalid port range", node.Name)
	}
	if each < 0 {
		each = 0
	}

	// SSH port: first free from the bottom.
	for p := lo; p <= hi; p++ {
		if !used[p] {
			ssh = p
			break
		}
	}
	if ssh == 0 {
		return 0, 0, 0, errors.New("no free ports on node")
	}
	if each == 0 {
		return ssh, 0, 0, nil
	}

	// Forwarding block: first free aligned window above the SSH port.
	for start := ssh + 1; start+each-1 <= hi; start += each {
		free := true
		for p := start; p < start+each; p++ {
			if used[p] || p == ssh {
				free = false
				break
			}
		}
		if free {
			return ssh, start, start + each - 1, nil
		}
	}
	return 0, 0, 0, errors.New("no free port block on node")
}

// PortClaim is the result of an admin port reassignment (range or count).
type PortClaim struct {
	SSHPort  int
	PortFrom int
	PortTo   int
}

// ClaimPorts reassigns an instance's SSH port and/or forwarded port block.
//
//   - sshPort: 0 keeps the instance's current SSH port; >0 claims that port.
//   - mode "range": use portFrom/portTo exactly (0/0 clears the block).
//   - mode "count": pick a free contiguous block of size portCount
//     (0 clears the block). preferFrom, when free and wide enough, is tried first.
//
// exceptID treats that instance's current ports as free. The claim is only
// durable once the caller persists it — wrap with AllocSection.
func (d *DB) ClaimPorts(ctx context.Context, node *Node, exceptID int64, sshPort int, mode string, portFrom, portTo, portCount, preferFrom int) (*PortClaim, error) {
	used, err := d.nodeUsageExcept(ctx, node.ID, exceptID)
	if err != nil {
		return nil, err
	}
	lo, hi := node.PortMin, node.PortMax
	if lo < 1024 || hi > 65535 || hi <= lo {
		return nil, fmt.Errorf("node %s has an invalid port range", node.Name)
	}

	// Resolve the instance's current SSH/block when the caller wants to keep them.
	var curSSH, curFrom int
	if exceptID > 0 {
		if inst, ierr := d.InstanceByID(ctx, exceptID); ierr == nil {
			curSSH, curFrom = inst.SSHPort, inst.PortFrom
		}
	}
	ssh := sshPort
	if ssh == 0 {
		ssh = curSSH
	}
	if ssh != 0 {
		if ssh < lo || ssh > hi {
			return nil, fmt.Errorf("SSH 端口 %d 不在节点范围 %d–%d 内", ssh, lo, hi)
		}
		if used.ports[ssh] {
			return nil, fmt.Errorf("SSH 端口 %d 已被占用", ssh)
		}
	}

	claim := &PortClaim{SSHPort: ssh}
	switch mode {
	case "range":
		if portFrom == 0 && portTo == 0 {
			return claim, nil
		}
		if portFrom < lo || portTo > hi || portFrom > portTo {
			return nil, fmt.Errorf("端口范围须在 %d–%d 内且起始≤结束", lo, hi)
		}
		if err := ensurePortsFree(used.ports, portFrom, portTo, ssh); err != nil {
			return nil, err
		}
		claim.PortFrom, claim.PortTo = portFrom, portTo
	case "count":
		if portCount < 0 {
			return nil, errors.New("端口数量不能为负")
		}
		if portCount == 0 {
			return claim, nil
		}
		if portCount > hi-lo+1 {
			return nil, fmt.Errorf("端口数量 %d 超过节点可用范围", portCount)
		}
		// Prefer expanding/keeping around the current block when possible.
		if preferFrom == 0 {
			preferFrom = curFrom
		}
		from, to, perr := pickPortBlock(lo, hi, portCount, used.ports, preferFrom, ssh)
		if perr != nil {
			return nil, perr
		}
		claim.PortFrom, claim.PortTo = from, to
	default:
		return nil, fmt.Errorf("unknown port claim mode %q", mode)
	}
	return claim, nil
}

// ensurePortsFree reports an error if any port in [from,to] is taken or equals
// reserved (typically the SSH port).
func ensurePortsFree(used map[int]bool, from, to int, reserved ...int) error {
	res := map[int]bool{}
	for _, p := range reserved {
		if p > 0 {
			res[p] = true
		}
	}
	for p := from; p <= to; p++ {
		if used[p] {
			return fmt.Errorf("端口 %d 已被占用", p)
		}
		if res[p] {
			return fmt.Errorf("端口 %d 与 SSH/VNC 端口冲突", p)
		}
	}
	return nil
}

// pickPortBlock finds a free contiguous window of `each` ports in [lo,hi],
// avoiding used ports and reserved (SSH). preferFrom is tried first when set.
func pickPortBlock(lo, hi, each int, used map[int]bool, preferFrom, reserved int) (from, to int, err error) {
	try := func(start int) bool {
		if start < lo || start+each-1 > hi {
			return false
		}
		for p := start; p < start+each; p++ {
			if used[p] || (reserved > 0 && p == reserved) {
				return false
			}
		}
		return true
	}
	if preferFrom > 0 && try(preferFrom) {
		return preferFrom, preferFrom + each - 1, nil
	}
	for start := lo; start+each-1 <= hi; start++ {
		if try(start) {
			return start, start + each - 1, nil
		}
	}
	return 0, 0, errors.New("no free port block on node")
}

// pickIPv6 returns the lowest free address in the pool, skipping the first few
// which conventionally belong to the gateway.
func pickIPv6(cidr string, used map[string]bool) (string, error) {
	return pickIPv6Func(cidr, func(s string) bool { return used[s] || used[canonV6(s)] })
}

func pickIPv6Func(cidr string, avoid func(string) bool) (string, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("node v6_cidr %q is not a valid CIDR", cidr)
	}
	if !pfx.Addr().Is6() {
		return "", errors.New("v6_cidr must be IPv6")
	}
	addr := pfx.Masked().Addr()
	for i := 0; i < 16; i++ {
		addr = addr.Next()
	}
	// Bound the scan: a /64 is effectively infinite, so cap the effort rather
	// than spin. In practice free addresses cluster at the bottom of the pool.
	for i := 0; i < 100000 && pfx.Contains(addr); i++ {
		if s := addr.String(); !avoid(s) {
			return s, nil
		}
		addr = addr.Next()
	}
	return "", errors.New("ipv6 pool exhausted")
}

// claimIPv4 validates a preferred IPv4 against the pool CIDR and avoid set.
func claimIPv4(cidr, want string, avoid func(string) bool) (string, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("pool %q is not a valid CIDR", cidr)
	}
	if !pfx.Addr().Is4() {
		return "", errors.New("pool must be IPv4")
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(want))
	if err != nil || !addr.Is4() {
		return "", fmt.Errorf("%q is not a valid IPv4 address", want)
	}
	s := addr.String()
	if !pfx.Contains(addr) {
		return "", fmt.Errorf("%s is outside pool %s", s, cidr)
	}
	if isV4Broadcast(pfx, addr) {
		return "", fmt.Errorf("%s is the broadcast address", s)
	}
	// Same reserved low range as auto-pick: network + first 9 hosts.
	net := pfx.Masked().Addr()
	for i := 0; i < 10; i++ {
		if addr == net {
			return "", fmt.Errorf("%s is in the reserved low range", s)
		}
		net = net.Next()
	}
	if avoid != nil && avoid(s) {
		return "", fmt.Errorf("%s is already in use or reserved", s)
	}
	return s, nil
}

// claimIPv6 validates a preferred IPv6 against the pool CIDR and avoid set.
func claimIPv6(cidr, want string, avoid func(string) bool) (string, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("pool %q is not a valid CIDR", cidr)
	}
	if !pfx.Addr().Is6() {
		return "", errors.New("pool must be IPv6")
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(want))
	if err != nil || !addr.Is6() {
		return "", fmt.Errorf("%q is not a valid IPv6 address", want)
	}
	s := addr.String()
	if !pfx.Contains(addr) {
		return "", fmt.Errorf("%s is outside pool %s", s, cidr)
	}
	// Same gateway skip as auto-pick: network + first 15 hosts.
	net := pfx.Masked().Addr()
	for i := 0; i < 16; i++ {
		if addr == net {
			return "", fmt.Errorf("%s is in the reserved gateway range", s)
		}
		net = net.Next()
	}
	if avoid != nil && avoid(s) {
		return "", fmt.Errorf("%s is already in use or outside the plan pool", s)
	}
	return s, nil
}

// ValidateCIDR parses a CIDR and checks its family, so the admin form can
// reject a bad pool at save time instead of at first redemption.
func ValidateCIDR(cidr string, wantV6 bool) (netip.Prefix, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, err
	}
	if wantV6 && !pfx.Addr().Is6() {
		return netip.Prefix{}, errors.New("expected an IPv6 prefix")
	}
	if !wantV6 && !pfx.Addr().Is4() {
		return netip.Prefix{}, errors.New("expected an IPv4 prefix")
	}
	return pfx, nil
}

// canonV6 normalises an address for set comparison.
func canonV6(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}
