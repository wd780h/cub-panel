package store

import "testing"

func TestPickIPv4StartsAfterReservedRange(t *testing.T) {
	// .0 through .9 are reserved for the bridge and operator use, so the first
	// address handed out is .10.
	got, err := pickIPv4("10.180.0.0/24", map[string]bool{})
	if err != nil {
		t.Fatalf("pickIPv4: %v", err)
	}
	if want := "10.180.0.10"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPickIPv4SkipsUsed(t *testing.T) {
	used := map[string]bool{"10.180.0.10": true, "10.180.0.11": true, "10.180.0.12": true}
	got, err := pickIPv4("10.180.0.0/24", used)
	if err != nil {
		t.Fatalf("pickIPv4: %v", err)
	}
	if want := "10.180.0.13"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestParseReserved(t *testing.T) {
	in, err := parseReserved("10.0.0.1-10.0.0.50, 10.0.0.99")
	if err != nil {
		t.Fatalf("parseReserved: %v", err)
	}
	for ip, want := range map[string]bool{
		"10.0.0.1": true, "10.0.0.25": true, "10.0.0.50": true,
		"10.0.0.99": true, "10.0.0.51": false, "10.0.0.100": false,
	} {
		if got := in(ip); got != want {
			t.Errorf("reserved(%s) = %v, want %v", ip, got, want)
		}
	}
	// Empty spec reserves nothing.
	if none, err := parseReserved(""); err != nil || none("10.0.0.1") {
		t.Errorf("empty spec: err=%v", err)
	}
	// Bad specs are rejected.
	for _, bad := range []string{"nope", "10.0.0.5-", "10.0.0.9-10.0.0.1", "::1"} {
		if _, err := parseReserved(bad); err == nil {
			t.Errorf("parseReserved(%q) should fail", bad)
		}
	}
}

func TestPickIPv4SkipsReservedRange(t *testing.T) {
	reserved, _ := parseReserved("10.180.0.10-10.180.0.19")
	got, err := pickIPv4Func("10.180.0.0/24", func(s string) bool { return reserved(s) })
	if err != nil {
		t.Fatalf("pickIPv4Func: %v", err)
	}
	if want := "10.180.0.20"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPickIPv4RejectsBadInput(t *testing.T) {
	for _, cidr := range []string{"", "not-a-cidr", "2001:db8::/64", "10.0.0.0/33"} {
		if _, err := pickIPv4(cidr, nil); err == nil {
			t.Errorf("pickIPv4(%q) should have failed", cidr)
		}
	}
}

func TestPickIPv4Exhaustion(t *testing.T) {
	// A /29 has 8 addresses, all inside the reserved low range.
	if _, err := pickIPv4("10.0.0.0/29", nil); err == nil {
		t.Error("expected exhaustion error on a /29")
	}
}

func TestPickIPv4SkipsBroadcast(t *testing.T) {
	// /28 -> .0 network, .1-.14 hosts, .15 broadcast. Allocation starts at .10,
	// so taking .10-.14 leaves only the broadcast address as a candidate.
	used := map[string]bool{}
	for i := 10; i <= 14; i++ {
		used[fmtIP(i)] = true
	}
	if _, err := pickIPv4("10.0.0.0/28", used); err == nil {
		t.Error("expected exhaustion rather than handing out the broadcast address")
	}
}

func fmtIP(last int) string {
	return "10.0.0." + itoa(last)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestPickPortsAllocatesDistinctBlocks(t *testing.T) {
	node := &Node{Name: "n1", PortMin: 20000, PortMax: 20100, PortsEach: 10}
	used := map[int]bool{}

	for i := 0; i < 3; i++ {
		ssh, from, to, err := pickPorts(node, used)
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		if to-from+1 != node.PortsEach {
			t.Errorf("block %d has width %d, want %d", i, to-from+1, node.PortsEach)
		}
		if used[ssh] {
			t.Errorf("ssh port %d handed out twice", ssh)
		}
		used[ssh] = true
		for p := from; p <= to; p++ {
			if used[p] {
				t.Errorf("port %d handed out twice", p)
			}
			used[p] = true
		}
	}
}

func TestPickPortsNoBlockWhenPortsEachZero(t *testing.T) {
	node := &Node{Name: "n1", PortMin: 20000, PortMax: 20010, PortsEach: 0}
	ssh, from, to, err := pickPorts(node, map[int]bool{})
	if err != nil {
		t.Fatalf("pickPorts: %v", err)
	}
	if ssh != 20000 || from != 0 || to != 0 {
		t.Errorf("got ssh=%d from=%d to=%d, want 20000/0/0", ssh, from, to)
	}
}

func TestPickPortsRejectsBadRange(t *testing.T) {
	for _, n := range []*Node{
		{Name: "a", PortMin: 100, PortMax: 200, PortsEach: 5},     // below 1024
		{Name: "b", PortMin: 30000, PortMax: 30000, PortsEach: 5}, // hi == lo
		{Name: "c", PortMin: 40000, PortMax: 39000, PortsEach: 5}, // inverted
	} {
		if _, _, _, err := pickPorts(n, map[int]bool{}); err == nil {
			t.Errorf("node %s: expected an error", n.Name)
		}
	}
}

func TestPickPortsExhaustion(t *testing.T) {
	node := &Node{Name: "n1", PortMin: 20000, PortMax: 20005, PortsEach: 10}
	// Only 6 ports available but a block needs 10.
	if _, _, _, err := pickPorts(node, map[int]bool{}); err == nil {
		t.Error("expected no-free-block error")
	}
}

func TestPickIPv6SkipsGatewayRange(t *testing.T) {
	got, err := pickIPv6("2001:db8::/64", map[string]bool{})
	if err != nil {
		t.Fatalf("pickIPv6: %v", err)
	}
	if want := "2001:db8::10"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPickIPv6SkipsUsed(t *testing.T) {
	used := map[string]bool{"2001:db8::10": true, "2001:db8::11": true}
	got, err := pickIPv6("2001:db8::/64", used)
	if err != nil {
		t.Fatalf("pickIPv6: %v", err)
	}
	if want := "2001:db8::12"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPickIPv6RejectsIPv4Prefix(t *testing.T) {
	if _, err := pickIPv6("10.0.0.0/24", nil); err == nil {
		t.Error("expected an error for an IPv4 prefix")
	}
}

func TestCanonV6Normalises(t *testing.T) {
	// The allocator compares canonical forms, so an expanded address written
	// by hand must match the compressed one it generates.
	if a, b := canonV6("2001:0db8:0000:0000:0000:0000:0000:0010"), canonV6("2001:db8::10"); a != b {
		t.Errorf("%s != %s", a, b)
	}
}

func TestValidateCIDR(t *testing.T) {
	if _, err := ValidateCIDR("10.0.0.0/24", false); err != nil {
		t.Errorf("valid IPv4 CIDR rejected: %v", err)
	}
	if _, err := ValidateCIDR("2001:db8::/64", true); err != nil {
		t.Errorf("valid IPv6 CIDR rejected: %v", err)
	}
	if _, err := ValidateCIDR("10.0.0.0/24", true); err == nil {
		t.Error("IPv4 prefix accepted where IPv6 was required")
	}
	if _, err := ValidateCIDR("2001:db8::/64", false); err == nil {
		t.Error("IPv6 prefix accepted where IPv4 was required")
	}
	if _, err := ValidateCIDR("garbage", false); err == nil {
		t.Error("garbage accepted")
	}
}
