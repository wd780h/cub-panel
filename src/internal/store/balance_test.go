package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAllocateV6Only(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	node := &Node{
		Name: "n1", Endpoint: "http://x:8788", Secret: "s", NATManaged: true,
		NATSubnet: "10.180.0.0/24", PortMin: 20000, PortMax: 20100, PortsEach: 5,
		V6Enabled: true, V6CIDR: "2001:db8:1::/64", MaxInstances: 10, Enabled: true,
	}
	id, err := db.SaveNode(ctx, node)
	if err != nil {
		t.Fatalf("save node: %v", err)
	}
	node.ID = id

	// IPv6-only: no NAT lease, no ports, a v6 address.
	a, err := db.Allocate(ctx, node, AllocSpec{WantDV6: true})
	if err != nil {
		t.Fatalf("allocate v6only: %v", err)
	}
	if a.NATAddr != "" || a.SSHPort != 0 || a.PortFrom != 0 {
		t.Errorf("v6only allocated v4 resources: %+v", a)
	}
	if a.V6Addr == "" {
		t.Error("v6only got no IPv6 address")
	}

	// Reserved ranges are honoured for NAT allocations.
	node.NATReserved = "10.180.0.10-10.180.0.20"
	if _, err := db.SaveNode(ctx, node); err != nil {
		t.Fatalf("update node: %v", err)
	}
	b, err := db.Allocate(ctx, node, AllocSpec{WantNAT: true})
	if err != nil {
		t.Fatalf("allocate nat: %v", err)
	}
	if b.NATAddr != "10.180.0.21" {
		t.Errorf("reserved range ignored: got %s", b.NATAddr)
	}

	// A plan v4 pool restricts NAT allocation to its range (within the subnet).
	c, err := db.Allocate(ctx, node, AllocSpec{WantNAT: true, V4Pool: "10.180.0.150-10.180.0.160"})
	if err != nil {
		t.Fatalf("allocate pooled: %v", err)
	}
	if c.NATAddr != "10.180.0.150" {
		t.Errorf("v4_pool ignored: got %s", c.NATAddr)
	}

	// A plan v6 pool restricts dedicated IPv6 to its range.
	d, err := db.Allocate(ctx, node, AllocSpec{WantDV6: true, V6Pool: "2001:db8:1::100-2001:db8:1::1ff"})
	if err != nil {
		t.Fatalf("allocate v6 pooled: %v", err)
	}
	if d.V6Addr != "2001:db8:1::100" {
		t.Errorf("v6_pool ignored: got %s", d.V6Addr)
	}

	// Exact preferred addresses are honoured when free.
	e, err := db.Allocate(ctx, node, AllocSpec{
		WantNAT: true, WantDV6: true,
		PreferNAT: "10.180.0.42", PreferV6: "2001:db8:1::42",
	})
	if err != nil {
		t.Fatalf("allocate preferred: %v", err)
	}
	if e.NATAddr != "10.180.0.42" || e.V6Addr != "2001:db8:1::42" {
		t.Errorf("preferred ignored: nat=%s v6=%s", e.NATAddr, e.V6Addr)
	}
	// Claim the preferred NAT via an instance row, then re-prefer must fail.
	uid, _ := db.CreateUser(ctx, "alloc@example.com", "x", false)
	iid, err := db.CreateInstance(ctx, &Instance{
		UserID: uid, NodeID: node.ID, Name: "cub-pref", Image: "debian/12", Family: "debian",
		CPU: 1, MemoryMB: 256, DiskGB: 5, Mode: "nat", InstanceType: "container",
		NATAddr: "10.180.0.42", SSHPort: 20001, Status: "running",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if _, err := db.Allocate(ctx, node, AllocSpec{WantNAT: true, PreferNAT: "10.180.0.42"}); err == nil {
		t.Error("expected failure for already-used preferred NAT")
	}
	// ExceptInstance frees the current row so an admin can "reclaim" its own IP.
	f, err := db.Allocate(ctx, node, AllocSpec{
		WantNAT: true, PreferNAT: "10.180.0.42", ExceptInstance: iid,
	})
	if err != nil || f.NATAddr != "10.180.0.42" {
		t.Fatalf("except instance: got %+v err %v", f, err)
	}
}

func TestPlanRoundtrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	in := &Plan{
		Name: "p", CPU: 2, MemoryMB: 512, DiskGB: 10, Mode: "ipv4v6",
		InstanceType: "vm", Features: "aes,nesting", TrafficGB: 100, TrafficMode: "up",
		RateDownMbps: 100, RateUpMbps: 30, ExtraBridges: "br-a",
		V4Pool: "10.0.0.10-10.0.0.20", V6Pool: "2001:db8::10-2001:db8::ff", KeepSourceIP: true,
		Snapshots: 5, Images: "debian/12", PriceCents: 500, DurationDays: 30, Enabled: true,
	}
	id, err := db.SavePlan(ctx, in)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := db.PlanByID(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Mode != "ipv4v6" || got.V4Pool != "10.0.0.10-10.0.0.20" ||
		got.V6Pool != "2001:db8::10-2001:db8::ff" || !got.KeepSourceIP ||
		got.RateDownMbps != 100 || got.ExtraBridges != "br-a" || got.Snapshots != 5 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestAllocateDedicatedV4(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	node := &Node{
		Name: "n2", Endpoint: "http://x:8788", Secret: "s", NATManaged: true,
		NATSubnet: "10.180.0.0/24", PortMin: 20000, PortMax: 20100,
		V4Enabled: true, V4CIDR: "203.0.113.0/28", V4GW: "203.0.113.1",
		MaxInstances: 10, Enabled: true,
	}
	id, err := db.SaveNode(ctx, node)
	if err != nil {
		t.Fatalf("save node: %v", err)
	}
	node.ID = id
	a, err := db.Allocate(ctx, node, AllocSpec{WantDV4: true})
	if err != nil {
		t.Fatalf("allocate dv4: %v", err)
	}
	if a.V4Addr == "" {
		t.Error("dedicated v4 not assigned")
	}
	// Without a pool the node must reject dedicated v4.
	node.V4Enabled = false
	if _, err := db.SaveNode(ctx, node); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := db.Allocate(ctx, node, AllocSpec{WantDV4: true}); err == nil {
		t.Error("expected failure when node has no v4 pool")
	}
}

func TestAdjustBalanceLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	uid, err := db.CreateUser(ctx, "t@example.com", "x", false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Credit.
	if bal, err := db.AdjustBalance(ctx, uid, 1000, "recharge", "order-1", ""); err != nil || bal != 1000 {
		t.Fatalf("credit: bal=%d err=%v", bal, err)
	}
	// Debit within balance.
	if bal, err := db.AdjustBalance(ctx, uid, -400, "purchase", "", ""); err != nil || bal != 600 {
		t.Fatalf("debit: bal=%d err=%v", bal, err)
	}
	// Over-draft is refused and changes nothing.
	if _, err := db.AdjustBalance(ctx, uid, -700, "purchase", "", ""); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("overdraft: want ErrInsufficient, got %v", err)
	}
	// Replayed recharge ref is refused (idempotency).
	if _, err := db.AdjustBalance(ctx, uid, 1000, "recharge", "order-1", ""); !errors.Is(err, ErrDuplicateRef) {
		t.Fatalf("replay: want ErrDuplicateRef, got %v", err)
	}
	// Unknown account.
	if _, err := db.AdjustBalance(ctx, 9999, 100, "recharge", "order-2", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: want ErrNotFound, got %v", err)
	}

	// Balance survived the failed attempts, ledger matches.
	u, err := db.UserByID(ctx, uid)
	if err != nil || u.BalanceCents != 600 {
		t.Fatalf("final balance: %+v err=%v", u, err)
	}
	txs, err := db.ListTransactions(ctx, uid, 10)
	if err != nil || len(txs) != 2 {
		t.Fatalf("ledger: %d rows err=%v", len(txs), err)
	}
	if txs[0].BalanceCents != 600 || txs[1].BalanceCents != 1000 {
		t.Fatalf("ledger balances wrong: %d, %d", txs[0].BalanceCents, txs[1].BalanceCents)
	}
}
