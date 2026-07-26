package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

func now() int64 { return time.Now().Unix() }

// ---------- users ----------

// User is a panel account.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	IsAdmin      bool
	Status       string
	BalanceCents int64
	CreatedAt    int64
	LastLoginAt  int64
	LastLoginIP  string
}

const userCols = `id, email, password_hash, is_admin, status, balance_cents, created_at, last_login_at, last_login_ip`

func scanUser(s interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := s.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.Status,
		&u.BalanceCents, &u.CreatedAt, &u.LastLoginAt, &u.LastLoginIP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

// CreateUser inserts a new account and returns its id.
func (d *DB) CreateUser(ctx context.Context, email, hash string, admin bool) (int64, error) {
	return d.insertID(ctx,
		`INSERT INTO users (email, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		normEmail(email), hash, boolInt(admin), now())
}

// UserByEmail looks an account up by address. Emails are stored lower-cased, so
// a plain equality match is case-insensitive across every dialect.
func (d *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(d.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE email = ?`, normEmail(email)))
}

// UserByID looks an account up by id.
func (d *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// ListUsers returns accounts newest first.
func (d *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TouchLogin records a successful sign-in.
func (d *DB) TouchLogin(ctx context.Context, id int64, ip string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE users SET last_login_at = ?, last_login_ip = ? WHERE id = ?`, now(), ip, id)
	return err
}

// SetUserStatus activates or suspends an account.
func (d *DB) SetUserStatus(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, status, id)
	return err
}

// SetUserAdmin grants or revokes administrator rights.
func (d *DB) SetUserAdmin(ctx context.Context, id int64, admin bool) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET is_admin = ? WHERE id = ?`, boolInt(admin), id)
	return err
}

// SetUserPassword replaces an account's password hash.
func (d *DB) SetUserPassword(ctx context.Context, id int64, hash string) error {
	_, err := d.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// CountUsers returns the number of accounts.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// DeleteUser removes an account and cascades to its sessions.
func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// ---------- sessions ----------

// Session is a signed-in browser session.
type Session struct {
	ID        string
	UserID    int64
	ExpiresAt int64
	CSRF      string
}

// CreateSession stores a new session row.
func (d *DB) CreateSession(ctx context.Context, id string, uid int64, csrf, ip, ua string, ttl time.Duration) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, created_at, expires_at, ip, user_agent, csrf)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, uid, now(), time.Now().Add(ttl).Unix(), ip, truncate(ua, 200), csrf)
	return err
}

// GetSession returns a live session, or ErrNotFound if missing/expired.
func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := d.QueryRowContext(ctx,
		`SELECT id, user_id, expires_at, csrf FROM sessions WHERE id = ? AND expires_at > ?`,
		id, now()).Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

// DeleteSession signs a session out.
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteUserSessions invalidates every session for an account.
func (d *DB) DeleteUserSessions(ctx context.Context, uid int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, uid)
	return err
}

// PurgeSessions drops expired session rows.
func (d *DB) PurgeSessions(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	return err
}

// ---------- nodes ----------

// Node is a managed LXD host.
type Node struct {
	ID           int64
	Name         string
	Region       string
	Endpoint     string
	Secret       string
	CertFP       string // pinned SHA-256 of the agent's self-signed cert; empty = TLS without pinning
	StoragePool  string
	NATBridge    string
	NATSubnet    string
	NATManaged   bool   // false: reuse a foreign bridge (docker0…), configure IPs in-guest
	NATGW        string // gateway for unmanaged bridges; empty = first host address
	NATReserved  string // excluded addresses/ranges, e.g. "10.0.0.1-10.0.0.50,10.0.0.99"
	DNS          string // resolvers pushed into guests; empty = distro/bridge default
	PortMin      int
	PortMax      int
	PortsEach    int
	V6Enabled    bool
	V6Bridge     string
	V6CIDR       string
	V6GW         string
	V4Enabled    bool // dedicated public IPv4 pool available
	V4Bridge     string
	V4CIDR       string
	V4GW         string
	KVMEnabled   bool // allow scheduling KVM virtual machines here (beta)
	MaxInstances int
	Enabled      bool
	LastSeenAt   int64
	LastStatus   string
	CreatedAt    int64
	// Populated by ListNodes for the admin view.
	InstanceCount int
}

const nodeCols = `id, name, region, endpoint, secret, cert_fp, storage_pool, nat_bridge, nat_subnet,
	nat_managed, nat_gw, nat_reserved, dns,
	port_min, port_max, ports_each, v6_enabled, v6_bridge, v6_cidr, v6_gw,
	v4_enabled, v4_bridge, v4_cidr, v4_gw,
	kvm_enabled, max_instances, enabled, last_seen_at, last_status, created_at`

func scanNode(s interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	err := s.Scan(&n.ID, &n.Name, &n.Region, &n.Endpoint, &n.Secret, &n.CertFP, &n.StoragePool,
		&n.NATBridge, &n.NATSubnet, &n.NATManaged, &n.NATGW, &n.NATReserved, &n.DNS,
		&n.PortMin, &n.PortMax, &n.PortsEach,
		&n.V6Enabled, &n.V6Bridge, &n.V6CIDR, &n.V6GW,
		&n.V4Enabled, &n.V4Bridge, &n.V4CIDR, &n.V4GW,
		&n.KVMEnabled, &n.MaxInstances, &n.Enabled, &n.LastSeenAt, &n.LastStatus, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &n, err
}

// SaveNode inserts or updates a node. A zero ID inserts.
func (d *DB) SaveNode(ctx context.Context, n *Node) (int64, error) {
	if n.ID == 0 {
		return d.insertID(ctx,
			`INSERT INTO nodes (name, region, endpoint, secret, cert_fp, storage_pool, nat_bridge, nat_subnet,
			   nat_managed, nat_gw, nat_reserved, dns,
			   port_min, port_max, ports_each, v6_enabled, v6_bridge, v6_cidr, v6_gw,
			   v4_enabled, v4_bridge, v4_cidr, v4_gw,
			   kvm_enabled, max_instances, enabled, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.Name, n.Region, n.Endpoint, n.Secret, n.CertFP, n.StoragePool, n.NATBridge, n.NATSubnet,
			boolInt(n.NATManaged), n.NATGW, n.NATReserved, n.DNS,
			n.PortMin, n.PortMax, n.PortsEach, boolInt(n.V6Enabled), n.V6Bridge, n.V6CIDR, n.V6GW,
			boolInt(n.V4Enabled), n.V4Bridge, n.V4CIDR, n.V4GW,
			boolInt(n.KVMEnabled), n.MaxInstances, boolInt(n.Enabled), now())
	}
	_, err := d.ExecContext(ctx,
		`UPDATE nodes SET name=?, region=?, endpoint=?, secret=?, cert_fp=?, storage_pool=?, nat_bridge=?,
		   nat_subnet=?, nat_managed=?, nat_gw=?, nat_reserved=?, dns=?,
		   port_min=?, port_max=?, ports_each=?, v6_enabled=?, v6_bridge=?,
		   v6_cidr=?, v6_gw=?, v4_enabled=?, v4_bridge=?, v4_cidr=?, v4_gw=?,
		   kvm_enabled=?, max_instances=?, enabled=? WHERE id=?`,
		n.Name, n.Region, n.Endpoint, n.Secret, n.CertFP, n.StoragePool, n.NATBridge, n.NATSubnet,
		boolInt(n.NATManaged), n.NATGW, n.NATReserved, n.DNS,
		n.PortMin, n.PortMax, n.PortsEach, boolInt(n.V6Enabled), n.V6Bridge, n.V6CIDR, n.V6GW,
		boolInt(n.V4Enabled), n.V4Bridge, n.V4CIDR, n.V4GW,
		boolInt(n.KVMEnabled), n.MaxInstances, boolInt(n.Enabled), n.ID)
	return n.ID, err
}

// NodeByID fetches one node.
func (d *DB) NodeByID(ctx context.Context, id int64) (*Node, error) {
	return scanNode(d.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

// ListNodes returns all nodes with their live instance counts.
func (d *DB) ListNodes(ctx context.Context, onlyEnabled bool) ([]*Node, error) {
	q := `SELECT ` + nodeCols + `, (SELECT COUNT(*) FROM instances i WHERE i.node_id = nodes.id)
	      FROM nodes`
	if onlyEnabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY name`
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.Region, &n.Endpoint, &n.Secret, &n.CertFP, &n.StoragePool,
			&n.NATBridge, &n.NATSubnet, &n.NATManaged, &n.NATGW, &n.NATReserved, &n.DNS,
			&n.PortMin, &n.PortMax, &n.PortsEach,
			&n.V6Enabled, &n.V6Bridge, &n.V6CIDR, &n.V6GW,
			&n.V4Enabled, &n.V4Bridge, &n.V4CIDR, &n.V4GW,
			&n.KVMEnabled, &n.MaxInstances, &n.Enabled, &n.LastSeenAt, &n.LastStatus, &n.CreatedAt,
			&n.InstanceCount); err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// TouchNode records the outcome of the last health probe.
func (d *DB) TouchNode(ctx context.Context, id int64, status string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE nodes SET last_seen_at = ?, last_status = ? WHERE id = ?`, now(), truncate(status, 200), id)
	return err
}

// DeleteNode removes a node that has no instances left.
func (d *DB) DeleteNode(ctx context.Context, id int64) error {
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE node_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("node still has instances")
	}
	_, err := d.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// ---------- plans ----------

// Plan is a purchasable/redeemable specification.
type Plan struct {
	ID           int64
	Name         string
	Description  string
	CPU          int
	MemoryMB     int
	DiskGB       int
	Mode         string
	InstanceType string // container | vm (KVM, beta)
	Features     string // comma list: tun,fuse,privileged,nesting
	TrafficGB    int    // monthly traffic allowance, 0 = unlimited
	TrafficMode  string // both | up | down
	RateDownMbps int    // ingress bandwidth cap, 0 = unlimited
	RateUpMbps   int    // egress bandwidth cap, 0 = unlimited
	ExtraBridges string // extra NIC bridges, comma separated
	V4Pool       string // restrict NAT internal IP to this range (within node subnet)
	KeepSourceIP bool   // NAT forwards preserve the real client source IP (DNAT)
	Mounts       string // host-dir binds "src:dst[:ro]" per line/comma (admin-only)
	ExtraDisks   string // extra data volumes in GB, comma list ("20,50"), max 4
	Images       string
	PriceCents   int64 // 0 = not purchasable with balance
	DurationDays int
	Enabled      bool
	SortOrder    int
	CreatedAt    int64
}

// ValidPlanMode reports whether a plan network mode string is supported.
func ValidPlanMode(m string) bool {
	switch m {
	case "nat", "ipv6", "ipv6only", "ipv4", "ipv4only", "ipv4v6":
		return true
	}
	return false
}

// FeatureList splits the plan's comma-separated feature flags.
func (p *Plan) FeatureList() []string {
	var out []string
	for _, s := range strings.Split(p.Features, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// HasFeature reports whether the plan enables one feature flag.
func (p *Plan) HasFeature(f string) bool {
	for _, s := range p.FeatureList() {
		if s == f {
			return true
		}
	}
	return false
}

// ImageList splits the plan's comma-separated image aliases.
func (p *Plan) ImageList() []string {
	var out []string
	for _, s := range strings.Split(p.Images, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// AllowsImage reports whether the alias is offered by this plan.
func (p *Plan) AllowsImage(img string) bool {
	for _, s := range p.ImageList() {
		if s == img {
			return true
		}
	}
	return false
}

const planCols = `id, name, description, cpu, memory_mb, disk_gb, mode, instance_type, features,
	traffic_gb, traffic_mode, rate_down_mbps, rate_up_mbps, extra_bridges, v4_pool, keep_source_ip, mounts, extra_disks, images,
	price_cents, duration_days, enabled, sort_order, created_at`

func scanPlan(s interface{ Scan(...any) error }) (*Plan, error) {
	var p Plan
	err := s.Scan(&p.ID, &p.Name, &p.Description, &p.CPU, &p.MemoryMB, &p.DiskGB,
		&p.Mode, &p.InstanceType, &p.Features, &p.TrafficGB, &p.TrafficMode, &p.RateDownMbps, &p.RateUpMbps,
		&p.ExtraBridges, &p.V4Pool, &p.KeepSourceIP, &p.Mounts, &p.ExtraDisks, &p.Images,
		&p.PriceCents, &p.DurationDays, &p.Enabled, &p.SortOrder, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// SavePlan inserts or updates a plan.
func (d *DB) SavePlan(ctx context.Context, p *Plan) (int64, error) {
	if p.ID == 0 {
		return d.insertID(ctx,
			`INSERT INTO plans (name, description, cpu, memory_mb, disk_gb, mode, instance_type, features,
			   traffic_gb, traffic_mode, rate_down_mbps, rate_up_mbps, extra_bridges, v4_pool, keep_source_ip, mounts, extra_disks, images,
			   price_cents, duration_days, enabled, sort_order, created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.Description, p.CPU, p.MemoryMB, p.DiskGB, p.Mode, p.InstanceType, p.Features,
			p.TrafficGB, p.TrafficMode, p.RateDownMbps, p.RateUpMbps, p.ExtraBridges, p.V4Pool, boolInt(p.KeepSourceIP), p.Mounts, p.ExtraDisks, p.Images,
			p.PriceCents, p.DurationDays, boolInt(p.Enabled), p.SortOrder, now())
	}
	_, err := d.ExecContext(ctx,
		`UPDATE plans SET name=?, description=?, cpu=?, memory_mb=?, disk_gb=?, mode=?, instance_type=?, features=?,
		   traffic_gb=?, traffic_mode=?, rate_down_mbps=?, rate_up_mbps=?, extra_bridges=?, v4_pool=?, keep_source_ip=?,
		   mounts=?, extra_disks=?, images=?, price_cents=?, duration_days=?, enabled=?, sort_order=? WHERE id=?`,
		p.Name, p.Description, p.CPU, p.MemoryMB, p.DiskGB, p.Mode, p.InstanceType, p.Features,
		p.TrafficGB, p.TrafficMode, p.RateDownMbps, p.RateUpMbps, p.ExtraBridges, p.V4Pool, boolInt(p.KeepSourceIP), p.Mounts, p.ExtraDisks, p.Images,
		p.PriceCents, p.DurationDays, boolInt(p.Enabled), p.SortOrder, p.ID)
	return p.ID, err
}

// PlanByID fetches one plan.
func (d *DB) PlanByID(ctx context.Context, id int64) (*Plan, error) {
	return scanPlan(d.QueryRowContext(ctx, `SELECT `+planCols+` FROM plans WHERE id = ?`, id))
}

// ListPlans returns plans in display order.
func (d *DB) ListPlans(ctx context.Context, onlyEnabled bool) ([]*Plan, error) {
	q := `SELECT ` + planCols + ` FROM plans`
	if onlyEnabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY sort_order, id`
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePlan removes a plan.
func (d *DB) DeletePlan(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	return err
}

// ---------- codes ----------

// Code is an activation code.
type Code struct {
	ID        int64
	Code      string
	PlanID    int64
	NodeID    int64
	Batch     string
	Note      string
	UsedBy    int64
	UsedAt    int64
	ExpiresAt int64
	CreatedAt int64
	// Joined for display.
	PlanName  string
	UserEmail string
}

// InsertCodes bulk-inserts a batch of codes in one transaction.
func (d *DB) InsertCodes(ctx context.Context, codes []string, planID, nodeID int64, batch, note string, expiresAt int64) error {
	return d.Tx(ctx, func(tx *Tx) error {
		st, err := tx.PrepareContext(ctx,
			`INSERT INTO codes (code, plan_id, node_id, batch, note, expires_at, created_at)
			 VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		ts := now()
		for _, c := range codes {
			if _, err := st.ExecContext(ctx, c, planID, nodeID, batch, note, expiresAt, ts); err != nil {
				return err
			}
		}
		return nil
	})
}

// CodeByValue looks up an unused, unexpired code.
func (d *DB) CodeByValue(ctx context.Context, code string) (*Code, error) {
	var c Code
	err := d.QueryRowContext(ctx,
		`SELECT id, code, plan_id, node_id, batch, note, used_by, used_at, expires_at, created_at
		 FROM codes WHERE code = ?`, code).
		Scan(&c.ID, &c.Code, &c.PlanID, &c.NodeID, &c.Batch, &c.Note,
			&c.UsedBy, &c.UsedAt, &c.ExpiresAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// ClaimCode atomically marks a code as consumed. It returns false if the code
// was already taken, which is what makes concurrent redemption safe.
func (d *DB) ClaimCode(ctx context.Context, codeID, userID int64) (bool, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE codes SET used_by = ?, used_at = ? WHERE id = ? AND used_by = 0`,
		userID, now(), codeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseCode returns a code to the pool (used when provisioning fails).
func (d *DB) ReleaseCode(ctx context.Context, codeID int64) error {
	_, err := d.ExecContext(ctx, `UPDATE codes SET used_by = 0, used_at = 0 WHERE id = ?`, codeID)
	return err
}

// ListCodes returns codes with plan and redeemer joined, optionally filtered.
func (d *DB) ListCodes(ctx context.Context, batch string, onlyUnused bool, limit int) ([]*Code, error) {
	q := `SELECT c.id, c.code, c.plan_id, c.node_id, c.batch, c.note, c.used_by, c.used_at,
	             c.expires_at, c.created_at,
	             COALESCE(p.name,''), COALESCE(u.email,'')
	      FROM codes c
	      LEFT JOIN plans p ON p.id = c.plan_id
	      LEFT JOIN users u ON u.id = c.used_by
	      WHERE 1=1`
	var args []any
	if batch != "" {
		q += ` AND c.batch = ?`
		args = append(args, batch)
	}
	if onlyUnused {
		q += ` AND c.used_by = 0`
	}
	q += ` ORDER BY c.id DESC LIMIT ?`
	args = append(args, clampLimit(limit))

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Code
	for rows.Next() {
		var c Code
		if err := rows.Scan(&c.ID, &c.Code, &c.PlanID, &c.NodeID, &c.Batch, &c.Note,
			&c.UsedBy, &c.UsedAt, &c.ExpiresAt, &c.CreatedAt, &c.PlanName, &c.UserEmail); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// DeleteCode removes an unused code.
func (d *DB) DeleteCode(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM codes WHERE id = ? AND used_by = 0`, id)
	return err
}

// CodeStats counts total and unused codes.
func (d *DB) CodeStats(ctx context.Context) (total, unused int, err error) {
	err = d.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN used_by = 0 THEN 1 ELSE 0 END),0) FROM codes`).
		Scan(&total, &unused)
	return
}

// ---------- instances ----------

// Instance is a provisioned container as the panel knows it.
type Instance struct {
	ID       int64
	UserID   int64
	NodeID   int64
	PlanID   int64
	CodeID   int64
	Name     string
	Label    string
	Image    string
	Family   string
	CPU      int
	MemoryMB int
	DiskGB   int
	Mode     string
	// InstanceType is container (default) or vm (KVM, beta).
	InstanceType string
	NATAddr      string
	SSHPort      int
	PortFrom     int
	PortTo       int
	V6Addr       string
	Status       string
	Error        string
	// Traffic metering.
	TrafficLimitGB int    // 0 = unlimited
	TrafficMode    string // both | up | down
	UsedRX         int64  // accumulated bytes into the guest
	UsedTX         int64  // accumulated bytes out of the guest
	LastRX         int64  // node counter snapshot for delta math
	LastTX         int64
	TrafficResetAt int64  // next monthly zeroing, 0 = never
	RateDownMbps   int    // ingress bandwidth cap, 0 = unlimited
	RateUpMbps     int    // egress bandwidth cap, 0 = unlimited
	ExtraBridges   string // extra NIC bridges, comma separated
	Mounts         string // host-dir binds "src:dst[:ro]", copied from the plan
	ExtraDisks     string // extra data volumes GB list, copied from the plan
	VNCPort        int    // KVM: host VNC port, 0 = none
	VNCPass        string // KVM: VNC password (8 chars, DES limit)
	V4Addr         string // dedicated public IPv4
	CreatedAt      int64
	ExpiresAt      int64
	// Joined for display.
	NodeName  string
	NodeHost  string
	UserEmail string
}

// TrafficUsed returns the metered bytes under the instance's billing mode.
func (i *Instance) TrafficUsed() int64 {
	switch i.TrafficMode {
	case "up":
		return i.UsedTX
	case "down":
		return i.UsedRX
	default:
		return i.UsedRX + i.UsedTX
	}
}

// TrafficLimitBytes converts the GB allowance to bytes; 0 means unlimited.
func (i *Instance) TrafficLimitBytes() int64 {
	return int64(i.TrafficLimitGB) * 1024 * 1024 * 1024
}

const instCols = `i.id, i.user_id, i.node_id, i.plan_id, i.code_id, i.name, i.label, i.image,
	i.family, i.cpu, i.memory_mb, i.disk_gb, i.mode, i.instance_type, i.nat_addr, i.ssh_port, i.port_from,
	i.port_to, i.v6_addr, i.status, i.error,
	i.traffic_limit_gb, i.traffic_mode, i.used_rx, i.used_tx, i.last_rx, i.last_tx, i.traffic_reset_at,
	i.rate_down_mbps, i.rate_up_mbps, i.extra_bridges, i.mounts, i.extra_disks, i.vnc_port, i.vnc_pass, i.v4_addr,
	i.created_at, i.expires_at`

func scanInst(s interface{ Scan(...any) error }, extra ...any) (*Instance, error) {
	var i Instance
	dst := []any{&i.ID, &i.UserID, &i.NodeID, &i.PlanID, &i.CodeID, &i.Name, &i.Label, &i.Image,
		&i.Family, &i.CPU, &i.MemoryMB, &i.DiskGB, &i.Mode, &i.InstanceType, &i.NATAddr, &i.SSHPort, &i.PortFrom,
		&i.PortTo, &i.V6Addr, &i.Status, &i.Error,
		&i.TrafficLimitGB, &i.TrafficMode, &i.UsedRX, &i.UsedTX, &i.LastRX, &i.LastTX, &i.TrafficResetAt,
		&i.RateDownMbps, &i.RateUpMbps, &i.ExtraBridges, &i.Mounts, &i.ExtraDisks, &i.VNCPort, &i.VNCPass, &i.V4Addr,
		&i.CreatedAt, &i.ExpiresAt}
	dst = append(dst, extra...)
	err := s.Scan(dst...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &i, err
}

// CreateInstance persists a new instance record.
func (d *DB) CreateInstance(ctx context.Context, i *Instance) (int64, error) {
	return d.insertID(ctx,
		`INSERT INTO instances (user_id, node_id, plan_id, code_id, name, label, image, family,
		   cpu, memory_mb, disk_gb, mode, instance_type, nat_addr, ssh_port, port_from, port_to, v6_addr,
		   status, traffic_limit_gb, traffic_mode, traffic_reset_at, rate_down_mbps, rate_up_mbps,
		   extra_bridges, mounts, extra_disks, vnc_port, vnc_pass, v4_addr,
		   created_at, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		i.UserID, i.NodeID, i.PlanID, i.CodeID, i.Name, i.Label, i.Image, i.Family,
		i.CPU, i.MemoryMB, i.DiskGB, i.Mode, i.InstanceType, i.NATAddr, i.SSHPort, i.PortFrom, i.PortTo,
		i.V6Addr, i.Status, i.TrafficLimitGB, i.TrafficMode, i.TrafficResetAt,
		i.RateDownMbps, i.RateUpMbps, i.ExtraBridges, i.Mounts, i.ExtraDisks, i.VNCPort, i.VNCPass, i.V4Addr, now(), i.ExpiresAt)
}

// InstanceByID fetches an instance with its node joined.
func (d *DB) InstanceByID(ctx context.Context, id int64) (*Instance, error) {
	row := d.QueryRowContext(ctx,
		`SELECT `+instCols+`, COALESCE(n.name,''), COALESCE(n.endpoint,''), COALESCE(u.email,'')
		 FROM instances i
		 LEFT JOIN nodes n ON n.id = i.node_id
		 LEFT JOIN users u ON u.id = i.user_id
		 WHERE i.id = ?`, id)
	i := &Instance{}
	var nn, nh, ue string
	i, err := scanInst(row, &nn, &nh, &ue)
	if err != nil {
		return nil, err
	}
	i.NodeName, i.NodeHost, i.UserEmail = nn, nh, ue
	return i, nil
}

// ListInstances returns instances, optionally scoped to one owner.
func (d *DB) ListInstances(ctx context.Context, userID int64) ([]*Instance, error) {
	q := `SELECT ` + instCols + `, COALESCE(n.name,''), COALESCE(n.endpoint,''), COALESCE(u.email,'')
	      FROM instances i
	      LEFT JOIN nodes n ON n.id = i.node_id
	      LEFT JOIN users u ON u.id = i.user_id`
	var args []any
	if userID > 0 {
		q += ` WHERE i.user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY i.id DESC LIMIT 500`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Instance
	for rows.Next() {
		var nn, nh, ue string
		i, err := scanInst(rows, &nn, &nh, &ue)
		if err != nil {
			return nil, err
		}
		i.NodeName, i.NodeHost, i.UserEmail = nn, nh, ue
		out = append(out, i)
	}
	return out, rows.Err()
}

// SetInstanceStatus updates the cached status and error text.
func (d *DB) SetInstanceStatus(ctx context.Context, id int64, status, errText string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET status = ?, error = ? WHERE id = ?`, status, truncate(errText, 500), id)
	return err
}

// SetInstanceLabel renames an instance in the UI (not in LXD).
func (d *DB) SetInstanceLabel(ctx context.Context, id int64, label string) error {
	_, err := d.ExecContext(ctx, `UPDATE instances SET label = ? WHERE id = ?`, truncate(label, 64), id)
	return err
}

// ResizeInstance records new resource limits (after the node applied them).
// A non-zero planID also re-links the instance to the plan it upgraded to.
func (d *DB) ResizeInstance(ctx context.Context, id int64, cpu, memoryMB, diskGB, rateDown, rateUp int, planID int64) error {
	if planID > 0 {
		_, err := d.ExecContext(ctx,
			`UPDATE instances SET cpu = ?, memory_mb = ?, disk_gb = ?, rate_down_mbps = ?, rate_up_mbps = ?, plan_id = ? WHERE id = ?`,
			cpu, memoryMB, diskGB, rateDown, rateUp, planID, id)
		return err
	}
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET cpu = ?, memory_mb = ?, disk_gb = ?, rate_down_mbps = ?, rate_up_mbps = ? WHERE id = ?`,
		cpu, memoryMB, diskGB, rateDown, rateUp, id)
	return err
}

// UpdateTrafficCounters stores the accumulated usage and the raw node
// counter snapshots used for the next delta.
func (d *DB) UpdateTrafficCounters(ctx context.Context, id, usedRX, usedTX, lastRX, lastTX int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET used_rx=?, used_tx=?, last_rx=?, last_tx=? WHERE id=?`,
		usedRX, usedTX, lastRX, lastTX, id)
	return err
}

// ResetTraffic zeroes the accumulated usage and schedules the next zeroing.
func (d *DB) ResetTraffic(ctx context.Context, id, nextReset int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET used_rx=0, used_tx=0, traffic_reset_at=? WHERE id=?`, nextReset, id)
	return err
}

// SetInstanceTraffic re-points an instance at a new allowance (plan upgrade).
func (d *DB) SetInstanceTraffic(ctx context.Context, id int64, limitGB int, mode string, resetAt int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET traffic_limit_gb=?, traffic_mode=?,
		   traffic_reset_at = CASE WHEN traffic_reset_at > 0 THEN traffic_reset_at ELSE ? END
		 WHERE id=?`, limitGB, mode, resetAt, id)
	return err
}

// RelocateInstance re-points an instance at its new node and network
// allocation after a migration.
func (d *DB) RelocateInstance(ctx context.Context, id, nodeID int64, natAddr string, sshPort, portFrom, portTo int, v6Addr, v4Addr string, vncPort int) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET node_id=?, nat_addr=?, ssh_port=?, port_from=?, port_to=?, v6_addr=?, v4_addr=?, vnc_port=?
		 WHERE id=?`, nodeID, natAddr, sshPort, portFrom, portTo, v6Addr, v4Addr, vncPort, id)
	return err
}

// SetInstanceV6 records the address actually assigned.
func (d *DB) SetInstanceV6(ctx context.Context, id int64, addr string) error {
	_, err := d.ExecContext(ctx, `UPDATE instances SET v6_addr = ? WHERE id = ?`, addr, id)
	return err
}

// ExtendInstance pushes the expiry out by the given number of days.
func (d *DB) ExtendInstance(ctx context.Context, id int64, days int) error {
	_, err := d.ExecContext(ctx,
		`UPDATE instances SET expires_at = MAX(expires_at, ?) + ? WHERE id = ?`,
		now(), int64(days)*86400, id)
	return err
}

// DeleteInstance removes the instance record.
func (d *DB) DeleteInstance(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM instances WHERE id = ?`, id)
	return err
}

// ExpiredInstances lists instances past their expiry.
func (d *DB) ExpiredInstances(ctx context.Context) ([]*Instance, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+instCols+`, COALESCE(n.name,''), COALESCE(n.endpoint,''), ''
		 FROM instances i LEFT JOIN nodes n ON n.id = i.node_id
		 WHERE i.expires_at > 0 AND i.expires_at <= ? AND i.status != 'expired'`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Instance
	for rows.Next() {
		var nn, nh, ue string
		i, err := scanInst(rows, &nn, &nh, &ue)
		if err != nil {
			return nil, err
		}
		i.NodeName, i.NodeHost = nn, nh
		out = append(out, i)
	}
	return out, rows.Err()
}

// CountInstances returns the total instance count.
func (d *DB) CountInstances(ctx context.Context) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances`).Scan(&n)
	return n, err
}

// CountUserInstances returns how many instances an account owns.
func (d *DB) CountUserInstances(ctx context.Context, uid int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE user_id = ?`, uid).Scan(&n)
	return n, err
}

// ---------- audit ----------

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID        int64
	UserID    int64
	Actor     string
	Action    string
	Detail    string
	IP        string
	CreatedAt int64
}

// Audit appends an audit record. Failures are non-fatal to the caller.
func (d *DB) Audit(ctx context.Context, uid int64, actor, action, detail, ip string) {
	_, _ = d.ExecContext(ctx,
		`INSERT INTO audit (user_id, actor, action, detail, ip, created_at) VALUES (?,?,?,?,?,?)`,
		uid, truncate(actor, 128), truncate(action, 64), truncate(detail, 500), ip, now())
}

// ListAudit returns the newest audit entries.
func (d *DB) ListAudit(ctx context.Context, limit int) ([]*AuditEntry, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, user_id, actor, action, detail, ip, created_at
		 FROM audit ORDER BY id DESC LIMIT ?`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.UserID, &a.Actor, &a.Action, &a.Detail, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ---------- settings ----------

// Setting reads a settings value, returning def when absent. `key` is a
// reserved word in MySQL, so it is back-tick quoted there.
func (d *DB) Setting(ctx context.Context, key, def string) string {
	q := `SELECT value FROM settings WHERE key = ?`
	if d.driver == MySQL {
		q = "SELECT value FROM settings WHERE `key` = ?"
	}
	var v string
	if err := d.QueryRowContext(ctx, q, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// SetSetting upserts a settings value using each dialect's upsert form.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	var err error
	switch d.driver {
	case MySQL:
		_, err = d.ExecContext(ctx,
			"INSERT INTO settings (`key`, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)",
			key, value)
	default: // sqlite, postgres — `key` is non-reserved and ON CONFLICT is shared syntax
		_, err = d.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	}
	return err
}

// ---------- helpers ----------

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// normEmail lower-cases and trims an address so equality matches are
// case-insensitive on every dialect without relying on a collation.
func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func clampLimit(n int) int {
	if n <= 0 || n > 1000 {
		return 200
	}
	return n
}
