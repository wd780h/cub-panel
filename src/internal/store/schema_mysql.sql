-- cub-panel schema — MySQL 8.0+. Idempotent: safe to run on every boot.
-- Indexes are declared inline (MySQL lacks CREATE INDEX IF NOT EXISTS); booleans
-- are BIGINT 0/1 like SQLite; `key` is backtick-quoted (reserved word).

CREATE TABLE IF NOT EXISTS users (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  email         VARCHAR(191) NOT NULL UNIQUE,
  password_hash VARCHAR(255) NOT NULL,
  is_admin      BIGINT  NOT NULL DEFAULT 0,
  status        VARCHAR(32) NOT NULL DEFAULT 'active',
  balance_cents BIGINT  NOT NULL DEFAULT 0,
  created_at    BIGINT  NOT NULL,
  last_login_at BIGINT  NOT NULL DEFAULT 0,
  last_login_ip VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sessions (
  id         VARCHAR(191) PRIMARY KEY,
  user_id    BIGINT  NOT NULL,
  created_at BIGINT  NOT NULL,
  expires_at BIGINT  NOT NULL,
  ip         VARCHAR(64)  NOT NULL DEFAULT '',
  user_agent VARCHAR(512) NOT NULL DEFAULT '',
  csrf       VARCHAR(191) NOT NULL,
  KEY idx_sessions_user (user_id),
  KEY idx_sessions_exp (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS nodes (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  name          VARCHAR(191) NOT NULL UNIQUE,
  region        VARCHAR(64)  NOT NULL DEFAULT '',
  endpoint      VARCHAR(255) NOT NULL,
  secret        VARCHAR(255) NOT NULL,
  cert_fp       VARCHAR(191) NOT NULL DEFAULT '',
  domain        VARCHAR(255) NOT NULL DEFAULT '',
  storage_pool  VARCHAR(64)  NOT NULL DEFAULT 'default',
  nat_bridge    VARCHAR(64)  NOT NULL DEFAULT 'lxdbr0',
  nat_subnet    VARCHAR(64)  NOT NULL DEFAULT '10.180.0.0/24',
  nat_managed   BIGINT  NOT NULL DEFAULT 1,
  nat_gw        VARCHAR(64)  NOT NULL DEFAULT '',
  nat_reserved  VARCHAR(512) NOT NULL DEFAULT '',
  dns           VARCHAR(255) NOT NULL DEFAULT '',
  port_min      BIGINT  NOT NULL DEFAULT 20000,
  port_max      BIGINT  NOT NULL DEFAULT 60000,
  ports_each    BIGINT  NOT NULL DEFAULT 10,
  v6_enabled    BIGINT  NOT NULL DEFAULT 0,
  v6_bridge     VARCHAR(64)  NOT NULL DEFAULT '',
  v6_cidr       VARCHAR(64)  NOT NULL DEFAULT '',
  v6_gw         VARCHAR(64)  NOT NULL DEFAULT '',
  v4_enabled    BIGINT  NOT NULL DEFAULT 0,
  v4_bridge     VARCHAR(64)  NOT NULL DEFAULT '',
  v4_cidr       VARCHAR(64)  NOT NULL DEFAULT '',
  v4_gw         VARCHAR(64)  NOT NULL DEFAULT '',
  kvm_enabled   BIGINT  NOT NULL DEFAULT 0,
  max_instances BIGINT  NOT NULL DEFAULT 50,
  enabled       BIGINT  NOT NULL DEFAULT 1,
  last_seen_at  BIGINT  NOT NULL DEFAULT 0,
  last_status   VARCHAR(255) NOT NULL DEFAULT '',
  created_at    BIGINT  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS plans (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  name          VARCHAR(191) NOT NULL,
  description   VARCHAR(1024) NOT NULL DEFAULT '',
  cpu           BIGINT  NOT NULL,
  memory_mb     BIGINT  NOT NULL,
  disk_gb       BIGINT  NOT NULL,
  mode          VARCHAR(32) NOT NULL DEFAULT 'nat',
  instance_type VARCHAR(32) NOT NULL DEFAULT 'container',
  node_id       BIGINT  NOT NULL DEFAULT 0,
  features      VARCHAR(512) NOT NULL DEFAULT '',
  traffic_gb    BIGINT  NOT NULL DEFAULT 0,
  traffic_mode  VARCHAR(16) NOT NULL DEFAULT 'both',
  rate_down_mbps BIGINT NOT NULL DEFAULT 0,
  rate_up_mbps   BIGINT NOT NULL DEFAULT 0,
  extra_bridges  VARCHAR(512) NOT NULL DEFAULT '',
  v4_pool        VARCHAR(255) NOT NULL DEFAULT '',
  v6_pool        VARCHAR(512) NOT NULL DEFAULT '',
  keep_source_ip BIGINT NOT NULL DEFAULT 1,
  mounts        VARCHAR(1024) NOT NULL DEFAULT '',
  extra_disks   VARCHAR(64) NOT NULL DEFAULT '',
  snapshots     BIGINT NOT NULL DEFAULT 3,
  images        VARCHAR(1024) NOT NULL DEFAULT 'debian/12,debian/13,alpine/3.21,alpine/3.22',
  price_cents   BIGINT  NOT NULL DEFAULT 0,
  duration_days BIGINT  NOT NULL DEFAULT 30,
  enabled       BIGINT  NOT NULL DEFAULT 1,
  sort_order    BIGINT  NOT NULL DEFAULT 0,
  created_at    BIGINT  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS codes (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  code       VARCHAR(191) NOT NULL UNIQUE,
  plan_id    BIGINT  NOT NULL,
  node_id    BIGINT  NOT NULL DEFAULT 0,
  batch      VARCHAR(191) NOT NULL DEFAULT '',
  note       VARCHAR(512) NOT NULL DEFAULT '',
  used_by    BIGINT  NOT NULL DEFAULT 0,
  used_at    BIGINT  NOT NULL DEFAULT 0,
  expires_at BIGINT  NOT NULL DEFAULT 0,
  created_at BIGINT  NOT NULL,
  KEY idx_codes_batch (batch),
  KEY idx_codes_used (used_by),
  CONSTRAINT fk_codes_plan FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS instances (
  id          BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id     BIGINT  NOT NULL,
  node_id     BIGINT  NOT NULL,
  plan_id     BIGINT  NOT NULL DEFAULT 0,
  code_id     BIGINT  NOT NULL DEFAULT 0,
  name        VARCHAR(191) NOT NULL UNIQUE,
  label       VARCHAR(255) NOT NULL DEFAULT '',
  image       VARCHAR(255) NOT NULL,
  family      VARCHAR(64)  NOT NULL DEFAULT 'debian',
  cpu         BIGINT  NOT NULL,
  memory_mb   BIGINT  NOT NULL,
  disk_gb     BIGINT  NOT NULL,
  mode        VARCHAR(32) NOT NULL DEFAULT 'nat',
  instance_type VARCHAR(32) NOT NULL DEFAULT 'container',
  nat_addr    VARCHAR(64)  NOT NULL DEFAULT '',
  ssh_port    BIGINT  NOT NULL DEFAULT 0,
  port_from   BIGINT  NOT NULL DEFAULT 0,
  port_to     BIGINT  NOT NULL DEFAULT 0,
  v6_addr     VARCHAR(64)  NOT NULL DEFAULT '',
  status      VARCHAR(32)  NOT NULL DEFAULT 'provisioning',
  error       VARCHAR(1024) NOT NULL DEFAULT '',
  traffic_limit_gb BIGINT NOT NULL DEFAULT 0,
  traffic_mode     VARCHAR(16) NOT NULL DEFAULT 'both',
  used_rx          BIGINT NOT NULL DEFAULT 0,
  used_tx          BIGINT NOT NULL DEFAULT 0,
  last_rx          BIGINT NOT NULL DEFAULT 0,
  last_tx          BIGINT NOT NULL DEFAULT 0,
  traffic_reset_at BIGINT NOT NULL DEFAULT 0,
  rate_down_mbps   BIGINT NOT NULL DEFAULT 0,
  rate_up_mbps     BIGINT NOT NULL DEFAULT 0,
  extra_bridges    VARCHAR(512) NOT NULL DEFAULT '',
  mounts           VARCHAR(1024) NOT NULL DEFAULT '',
  extra_disks      VARCHAR(64) NOT NULL DEFAULT '',
  vnc_port         BIGINT NOT NULL DEFAULT 0,
  vnc_pass         VARCHAR(64) NOT NULL DEFAULT '',
  v4_addr          VARCHAR(64) NOT NULL DEFAULT '',
  created_at  BIGINT  NOT NULL,
  expires_at  BIGINT  NOT NULL DEFAULT 0,
  KEY idx_inst_user (user_id),
  KEY idx_inst_node (node_id),
  KEY idx_inst_exp (expires_at),
  CONSTRAINT fk_inst_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_inst_node FOREIGN KEY (node_id) REFERENCES nodes(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS audit (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id    BIGINT  NOT NULL DEFAULT 0,
  actor      VARCHAR(191) NOT NULL DEFAULT '',
  action     VARCHAR(191) NOT NULL,
  detail     VARCHAR(1024) NOT NULL DEFAULT '',
  ip         VARCHAR(64) NOT NULL DEFAULT '',
  created_at BIGINT  NOT NULL,
  KEY idx_audit_time (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS settings (
  `key`  VARCHAR(191) PRIMARY KEY,
  value  TEXT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS transactions (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  user_id       BIGINT  NOT NULL,
  amount_cents  BIGINT  NOT NULL,
  balance_cents BIGINT  NOT NULL,
  kind          VARCHAR(32) NOT NULL,
  -- ref is NULLable: MySQL UNIQUE ignores NULLs, so empty refs (stored as
  -- NULL) may repeat while real refs are enforced unique — the idempotency
  -- guarantee that sqlite/postgres get from their partial unique index.
  ref           VARCHAR(191) NULL DEFAULT NULL,
  note          VARCHAR(512) NOT NULL DEFAULT '',
  created_at    BIGINT  NOT NULL,
  KEY idx_tx_user (user_id, id),
  UNIQUE KEY idx_tx_ref (ref),
  CONSTRAINT fk_tx_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS recharge_orders (
  id           BIGINT AUTO_INCREMENT PRIMARY KEY,
  order_no     VARCHAR(191) NOT NULL UNIQUE,
  user_id      BIGINT  NOT NULL,
  amount_cents BIGINT  NOT NULL,
  method       VARCHAR(32) NOT NULL,
  status       VARCHAR(32) NOT NULL DEFAULT 'pending',
  txid         VARCHAR(255) NOT NULL DEFAULT '',
  created_at   BIGINT  NOT NULL,
  paid_at      BIGINT  NOT NULL DEFAULT 0,
  KEY idx_orders_user (user_id, id),
  KEY idx_orders_status (status),
  CONSTRAINT fk_orders_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS email_verifications (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  email         VARCHAR(254) NOT NULL,
  password_hash VARCHAR(255) NOT NULL,
  code          VARCHAR(8)   NOT NULL,
  token         VARCHAR(191) NOT NULL UNIQUE,
  attempts      INT    NOT NULL DEFAULT 0,
  expires_at    BIGINT NOT NULL,
  created_at    BIGINT NOT NULL,
  KEY idx_email_verif_email (email),
  KEY idx_email_verif_exp (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
