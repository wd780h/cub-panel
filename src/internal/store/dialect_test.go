package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestRebindPostgres locks the `?` → `$N` conversion, including that a `?`
// inside a string literal is left untouched.
func TestRebindPostgres(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT * FROM t WHERE a = ? AND b = ?`, `SELECT * FROM t WHERE a = $1 AND b = $2`},
		{`INSERT INTO t (a,b,c) VALUES (?,?,?)`, `INSERT INTO t (a,b,c) VALUES ($1,$2,$3)`},
		{`UPDATE t SET a = ? WHERE note = 'why?' AND b = ?`, `UPDATE t SET a = $1 WHERE note = 'why?' AND b = $2`},
		{`SELECT 1`, `SELECT 1`},
	}
	for _, c := range cases {
		if got := rebind(Postgres, c.in); got != c.want {
			t.Errorf("rebind(pg, %q) = %q, want %q", c.in, got, c.want)
		}
		// Non-postgres dialects must pass through verbatim.
		if got := rebind(MySQL, c.in); got != c.in {
			t.Errorf("rebind(mysql, %q) = %q, want unchanged", c.in, got)
		}
		if got := rebind(SQLite, c.in); got != c.in {
			t.Errorf("rebind(sqlite, %q) = %q, want unchanged", c.in, got)
		}
	}
}

// TestDialectSuite runs the full store surface against every backend that has a
// DSN configured. SQLite always runs; postgres/mysql run when CUB_TEST_PG_DSN /
// CUB_TEST_MYSQL_DSN are set (see deploy/test-db.sh, which spins up containers).
func TestDialectSuite(t *testing.T) {
	backends := []struct{ driver, dsn string }{
		{SQLite, ""}, // dsn filled in per-run with a temp file
	}
	if dsn := os.Getenv("CUB_TEST_PG_DSN"); dsn != "" {
		backends = append(backends, struct{ driver, dsn string }{Postgres, dsn})
	}
	if dsn := os.Getenv("CUB_TEST_MYSQL_DSN"); dsn != "" {
		backends = append(backends, struct{ driver, dsn string }{MySQL, dsn})
	}
	for _, b := range backends {
		t.Run(b.driver, func(t *testing.T) {
			dsn := b.dsn
			if b.driver == SQLite {
				dsn = t.TempDir() + "/test.db"
			}
			db, err := Open(b.driver, dsn)
			if err != nil {
				t.Fatalf("open %s: %v", b.driver, err)
			}
			defer db.Close()
			if db.Driver() != b.driver {
				t.Fatalf("Driver() = %q, want %q", db.Driver(), b.driver)
			}
			runDialectSuite(t, db)
		})
	}
}

func runDialectSuite(t *testing.T, db *DB) {
	ctx := context.Background()
	// Unique suffix keeps reruns against a persistent container from colliding.
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())

	// --- users + case-insensitive lookup ---
	email := "Admin+" + uniq + "@Example.COM"
	uid, err := db.CreateUser(ctx, email, "hash", true)
	if err != nil || uid == 0 {
		t.Fatalf("CreateUser: id=%d err=%v", uid, err)
	}
	for _, probe := range []string{email, "admin+" + uniq + "@example.com", "  ADMIN+" + uniq + "@EXAMPLE.com  "} {
		u, err := db.UserByEmail(ctx, probe)
		if err != nil {
			t.Fatalf("UserByEmail(%q): %v", probe, err)
		}
		if u.ID != uid {
			t.Fatalf("UserByEmail(%q) id=%d, want %d", probe, u.ID, uid)
		}
	}

	// --- node + plan + instance inserts return generated ids ---
	nid, err := db.SaveNode(ctx, &Node{
		Name: "node-" + uniq, Endpoint: "https://10.0.0.1:8788", Secret: "s3cr3t-shared-secret-value-000000",
	})
	if err != nil || nid == 0 {
		t.Fatalf("SaveNode: id=%d err=%v", nid, err)
	}
	pid, err := db.SavePlan(ctx, &Plan{Name: "plan-" + uniq, CPU: 1, MemoryMB: 512, DiskGB: 10,
		Mounts: "/data/share:/mnt/share:ro"})
	if err != nil || pid == 0 {
		t.Fatalf("SavePlan: id=%d err=%v", pid, err)
	}
	if p, err := db.PlanByID(ctx, pid); err != nil || p.Mounts != "/data/share:/mnt/share:ro" {
		t.Fatalf("PlanByID mounts roundtrip: %+v err=%v", p, err)
	}
	iid, err := db.CreateInstance(ctx, &Instance{
		UserID: uid, NodeID: nid, Name: "cub-" + uniq, Image: "debian/12",
		CPU: 1, MemoryMB: 512, DiskGB: 10, Status: "provisioning",
	})
	if err != nil || iid == 0 {
		t.Fatalf("CreateInstance: id=%d err=%v", iid, err)
	}

	// --- settings upsert ---
	key := "site_name_" + uniq
	if got := db.Setting(ctx, key, "def"); got != "def" {
		t.Fatalf("Setting default = %q, want def", got)
	}
	if err := db.SetSetting(ctx, key, "First"); err != nil {
		t.Fatalf("SetSetting insert: %v", err)
	}
	if err := db.SetSetting(ctx, key, "Second"); err != nil {
		t.Fatalf("SetSetting update: %v", err)
	}
	if got := db.Setting(ctx, key, "def"); got != "Second" {
		t.Fatalf("Setting after upsert = %q, want Second", got)
	}

	// --- balance ledger + ref idempotency ---
	ref := "order:" + uniq
	if _, err := db.AdjustBalance(ctx, uid, 1000, "recharge", ref, "first"); err != nil {
		t.Fatalf("AdjustBalance credit: %v", err)
	}
	if _, err := db.AdjustBalance(ctx, uid, 1000, "recharge", ref, "replay"); !errors.Is(err, ErrDuplicateRef) {
		t.Fatalf("AdjustBalance replay err = %v, want ErrDuplicateRef", err)
	}
	if _, err := db.AdjustBalance(ctx, uid, -100000, "purchase", "", "overdraw"); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("AdjustBalance overdraw err = %v, want ErrInsufficient", err)
	}
	// Empty refs must be allowed repeatedly (no false duplicate).
	for i := 0; i < 2; i++ {
		if _, err := db.AdjustBalance(ctx, uid, 5, "admin", "", "noref"); err != nil {
			t.Fatalf("AdjustBalance empty-ref #%d: %v", i, err)
		}
	}

	// --- recharge order paid-once idempotency ---
	orderNo := "R" + uniq
	if _, err := db.CreateOrder(ctx, &Order{OrderNo: orderNo, UserID: uid, AmountCents: 500, Method: "usdt"}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	credited, err := db.MarkOrderPaid(ctx, orderNo, "tx1")
	if err != nil || !credited {
		t.Fatalf("MarkOrderPaid first: credited=%v err=%v", credited, err)
	}
	credited, err = db.MarkOrderPaid(ctx, orderNo, "tx1")
	if err != nil || credited {
		t.Fatalf("MarkOrderPaid replay: credited=%v err=%v (want credited=false)", credited, err)
	}
}
