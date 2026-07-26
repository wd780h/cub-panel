package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
)

// allocMu serialises resource allocation across requests. SQLite already
// serialises writers, but the read-then-write pattern below needs the wider
// critical section to stay race-free.
var allocMu sync.Mutex

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
}

// Allocate reserves the network resources named by spec. It fails rather than
// overcommit.
func (d *DB) Allocate(ctx context.Context, node *Node, spec AllocSpec) (*Allocation, error) {
	allocMu.Lock()
	defer allocMu.Unlock()

	used, err := d.nodeUsage(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	if used.count >= node.MaxInstances {
		return nil, errors.New("node is at capacity")
	}

	a := &Allocation{}

	if spec.WantNAT {
		// Node-reserved ranges + (optional) plan pool restriction.
		reserved, err := parseReserved(node.NATReserved)
		if err != nil {
			return nil, fmt.Errorf("node nat_reserved: %w", err)
		}
		var inPool func(string) bool
		if spec.V4Pool != "" {
			inPool, err = parseReserved(spec.V4Pool) // reuse the range parser
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
		if a.NATAddr, err = pickIPv4Func(node.NATSubnet, avoid); err != nil {
			return nil, err
		}
		if a.SSHPort, a.PortFrom, a.PortTo, err = pickPorts(node, used.ports); err != nil {
			return nil, err
		}
	}
	if spec.WantDV4 {
		if !node.V4Enabled || node.V4CIDR == "" {
			return nil, errors.New("node has no dedicated IPv4 pool configured")
		}
		avoid := func(s string) bool { return used.dv4[s] }
		if a.V4Addr, err = pickIPv4Func(node.V4CIDR, avoid); err != nil {
			return nil, fmt.Errorf("dedicated IPv4 pool exhausted: %w", err)
		}
	}
	if spec.WantDV6 {
		if !node.V6Enabled || node.V6CIDR == "" {
			return nil, errors.New("node has no IPv6 pool configured")
		}
		if a.V6Addr, err = pickIPv6(node.V6CIDR, used.v6); err != nil {
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

// parseReserved compiles the operator's excluded-address spec — a comma list
// of single IPs and inclusive ranges ("10.0.0.1-10.0.0.50,10.0.0.99") — into
// a membership test.
func parseReserved(spec string) (func(string) bool, error) {
	type span struct{ lo, hi netip.Addr }
	var spans []span
	for _, part := range splitTrim(spec, ",") {
		lo, hi, ok := splitRange(part)
		a1, err1 := netip.ParseAddr(lo)
		a2, err2 := netip.ParseAddr(hi)
		if !ok || err1 != nil || err2 != nil || !a1.Is4() || !a2.Is4() || a2.Less(a1) {
			return nil, fmt.Errorf("bad entry %q (want IP or IP-IP)", part)
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

// ValidateReserved checks a reserved-range spec at save time.
func ValidateReserved(spec string) error {
	_, err := parseReserved(spec)
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
	rows, err := d.QueryContext(ctx,
		`SELECT nat_addr, v6_addr, v4_addr, ssh_port, port_from, port_to, vnc_port FROM instances WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	u := &usage{v4: map[string]bool{}, dv4: map[string]bool{}, v6: map[string]bool{}, ports: map[int]bool{}}
	for rows.Next() {
		var v4, v6, dv4 string
		var ssh, from, to, vnc int
		if err := rows.Scan(&v4, &v6, &dv4, &ssh, &from, &to, &vnc); err != nil {
			return nil, err
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

// pickIPv6 returns the lowest free address in the pool, skipping the first few
// which conventionally belong to the gateway.
func pickIPv6(cidr string, used map[string]bool) (string, error) {
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
		if s := addr.String(); !used[s] {
			return s, nil
		}
		addr = addr.Next()
	}
	return "", errors.New("ipv6 pool exhausted")
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
