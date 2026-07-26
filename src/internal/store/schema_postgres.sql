-- cub-panel schema — PostgreSQL. Idempotent: safe to run on every boot.
-- Booleans are stored as BIGINT 0/1 (like SQLite) so Go scanning stays uniform.

CREATE TABLE IF NOT EXISTS users (
  id            BIGSERIAL PRIMARY KEY,
  email         TEXT    NOT NULL UNIQUE,
  password_hash TEXT    NOT NULL,
  is_admin      BIGINT  NOT NULL DEFAULT 0,
  status        TEXT    NOT NULL DEFAULT 'active',
  balance_cents BIGINT  NOT NULL DEFAULT 0,
  created_at    BIGINT  NOT NULL,
  last_login_at BIGINT  NOT NULL DEFAULT 0,
  last_login_ip TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT    PRIMARY KEY,
  user_id    BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at BIGINT  NOT NULL,
  expires_at BIGINT  NOT NULL,
  ip         TEXT    NOT NULL DEFAULT '',
  user_agent TEXT    NOT NULL DEFAULT '',
  csrf       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_exp  ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS nodes (
  id            BIGSERIAL PRIMARY KEY,
  name          TEXT    NOT NULL UNIQUE,
  region        TEXT    NOT NULL DEFAULT '',
  endpoint      TEXT    NOT NULL,
  secret        TEXT    NOT NULL,
  cert_fp       TEXT    NOT NULL DEFAULT '',
  storage_pool  TEXT    NOT NULL DEFAULT 'default',
  nat_bridge    TEXT    NOT NULL DEFAULT 'lxdbr0',
  nat_subnet    TEXT    NOT NULL DEFAULT '10.180.0.0/24',
  nat_managed   BIGINT  NOT NULL DEFAULT 1,
  nat_gw        TEXT    NOT NULL DEFAULT '',
  nat_reserved  TEXT    NOT NULL DEFAULT '',
  dns           TEXT    NOT NULL DEFAULT '',
  port_min      BIGINT  NOT NULL DEFAULT 20000,
  port_max      BIGINT  NOT NULL DEFAULT 60000,
  ports_each    BIGINT  NOT NULL DEFAULT 10,
  v6_enabled    BIGINT  NOT NULL DEFAULT 0,
  v6_bridge     TEXT    NOT NULL DEFAULT '',
  v6_cidr       TEXT    NOT NULL DEFAULT '',
  v6_gw         TEXT    NOT NULL DEFAULT '',
  v4_enabled    BIGINT  NOT NULL DEFAULT 0,
  v4_bridge     TEXT    NOT NULL DEFAULT '',
  v4_cidr       TEXT    NOT NULL DEFAULT '',
  v4_gw         TEXT    NOT NULL DEFAULT '',
  kvm_enabled   BIGINT  NOT NULL DEFAULT 0,
  max_instances BIGINT  NOT NULL DEFAULT 50,
  enabled       BIGINT  NOT NULL DEFAULT 1,
  last_seen_at  BIGINT  NOT NULL DEFAULT 0,
  last_status   TEXT    NOT NULL DEFAULT '',
  created_at    BIGINT  NOT NULL
);

CREATE TABLE IF NOT EXISTS plans (
  id            BIGSERIAL PRIMARY KEY,
  name          TEXT    NOT NULL,
  description   TEXT    NOT NULL DEFAULT '',
  cpu           BIGINT  NOT NULL,
  memory_mb     BIGINT  NOT NULL,
  disk_gb       BIGINT  NOT NULL,
  mode          TEXT    NOT NULL DEFAULT 'nat',
  instance_type TEXT    NOT NULL DEFAULT 'container',
  features      TEXT    NOT NULL DEFAULT '',
  traffic_gb    BIGINT  NOT NULL DEFAULT 0,
  traffic_mode  TEXT    NOT NULL DEFAULT 'both',
  rate_down_mbps BIGINT NOT NULL DEFAULT 0,
  rate_up_mbps   BIGINT NOT NULL DEFAULT 0,
  extra_bridges  TEXT   NOT NULL DEFAULT '',
  v4_pool        TEXT   NOT NULL DEFAULT '',
  keep_source_ip BIGINT NOT NULL DEFAULT 1,
  images        TEXT    NOT NULL DEFAULT 'debian/12,debian/13,alpine/3.21,alpine/3.22',
  price_cents   BIGINT  NOT NULL DEFAULT 0,
  duration_days BIGINT  NOT NULL DEFAULT 30,
  enabled       BIGINT  NOT NULL DEFAULT 1,
  sort_order    BIGINT  NOT NULL DEFAULT 0,
  created_at    BIGINT  NOT NULL
);

CREATE TABLE IF NOT EXISTS codes (
  id         BIGSERIAL PRIMARY KEY,
  code       TEXT    NOT NULL UNIQUE,
  plan_id    BIGINT  NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  node_id    BIGINT  NOT NULL DEFAULT 0,
  batch      TEXT    NOT NULL DEFAULT '',
  note       TEXT    NOT NULL DEFAULT '',
  used_by    BIGINT  NOT NULL DEFAULT 0,
  used_at    BIGINT  NOT NULL DEFAULT 0,
  expires_at BIGINT  NOT NULL DEFAULT 0,
  created_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codes_batch ON codes(batch);
CREATE INDEX IF NOT EXISTS idx_codes_used  ON codes(used_by);

CREATE TABLE IF NOT EXISTS instances (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id     BIGINT  NOT NULL REFERENCES nodes(id),
  plan_id     BIGINT  NOT NULL DEFAULT 0,
  code_id     BIGINT  NOT NULL DEFAULT 0,
  name        TEXT    NOT NULL UNIQUE,
  label       TEXT    NOT NULL DEFAULT '',
  image       TEXT    NOT NULL,
  family      TEXT    NOT NULL DEFAULT 'debian',
  cpu         BIGINT  NOT NULL,
  memory_mb   BIGINT  NOT NULL,
  disk_gb     BIGINT  NOT NULL,
  mode        TEXT    NOT NULL DEFAULT 'nat',
  instance_type TEXT  NOT NULL DEFAULT 'container',
  nat_addr    TEXT    NOT NULL DEFAULT '',
  ssh_port    BIGINT  NOT NULL DEFAULT 0,
  port_from   BIGINT  NOT NULL DEFAULT 0,
  port_to     BIGINT  NOT NULL DEFAULT 0,
  v6_addr     TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL DEFAULT 'provisioning',
  error       TEXT    NOT NULL DEFAULT '',
  traffic_limit_gb BIGINT NOT NULL DEFAULT 0,
  traffic_mode     TEXT   NOT NULL DEFAULT 'both',
  used_rx          BIGINT NOT NULL DEFAULT 0,
  used_tx          BIGINT NOT NULL DEFAULT 0,
  last_rx          BIGINT NOT NULL DEFAULT 0,
  last_tx          BIGINT NOT NULL DEFAULT 0,
  traffic_reset_at BIGINT NOT NULL DEFAULT 0,
  rate_down_mbps   BIGINT NOT NULL DEFAULT 0,
  rate_up_mbps     BIGINT NOT NULL DEFAULT 0,
  extra_bridges    TEXT   NOT NULL DEFAULT '',
  vnc_port         BIGINT NOT NULL DEFAULT 0,
  vnc_pass         TEXT   NOT NULL DEFAULT '',
  v4_addr          TEXT   NOT NULL DEFAULT '',
  created_at  BIGINT  NOT NULL,
  expires_at  BIGINT  NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_inst_user ON instances(user_id);
CREATE INDEX IF NOT EXISTS idx_inst_node ON instances(node_id);
CREATE INDEX IF NOT EXISTS idx_inst_exp  ON instances(expires_at);

CREATE TABLE IF NOT EXISTS audit (
  id         BIGSERIAL PRIMARY KEY,
  user_id    BIGINT  NOT NULL DEFAULT 0,
  actor      TEXT    NOT NULL DEFAULT '',
  action     TEXT    NOT NULL,
  detail     TEXT    NOT NULL DEFAULT '',
  ip         TEXT    NOT NULL DEFAULT '',
  created_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit(created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
  id            BIGSERIAL PRIMARY KEY,
  user_id       BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount_cents  BIGINT  NOT NULL,
  balance_cents BIGINT  NOT NULL,
  kind          TEXT    NOT NULL,
  ref           TEXT    NOT NULL DEFAULT '',
  note          TEXT    NOT NULL DEFAULT '',
  created_at    BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tx_user ON transactions(user_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tx_ref ON transactions(ref) WHERE ref <> '';

CREATE TABLE IF NOT EXISTS recharge_orders (
  id           BIGSERIAL PRIMARY KEY,
  order_no     TEXT    NOT NULL UNIQUE,
  user_id      BIGINT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount_cents BIGINT  NOT NULL,
  method       TEXT    NOT NULL,
  status       TEXT    NOT NULL DEFAULT 'pending',
  txid         TEXT    NOT NULL DEFAULT '',
  created_at   BIGINT  NOT NULL,
  paid_at      BIGINT  NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_orders_user ON recharge_orders(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_orders_status ON recharge_orders(status);
