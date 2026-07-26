-- cub-panel schema. Idempotent: safe to run on every boot.

CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT    NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  status        TEXT    NOT NULL DEFAULT 'active',   -- active | suspended
  balance_cents INTEGER NOT NULL DEFAULT 0,          -- 分为单位，避免浮点
  created_at    INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0,
  last_login_ip TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT    PRIMARY KEY,          -- random id, cookie carries id+mac
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip         TEXT    NOT NULL DEFAULT '',
  user_agent TEXT    NOT NULL DEFAULT '',
  csrf       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_exp  ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS nodes (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT    NOT NULL UNIQUE,
  region        TEXT    NOT NULL DEFAULT '',
  endpoint      TEXT    NOT NULL,                    -- https://host:8788
  secret        TEXT    NOT NULL,                    -- HMAC shared secret
  cert_fp       TEXT    NOT NULL DEFAULT '',         -- agent 自签证书 SHA256 指纹（钉扎）
  storage_pool  TEXT    NOT NULL DEFAULT 'default',
  -- NAT networking
  nat_bridge    TEXT    NOT NULL DEFAULT 'lxdbr0',
  nat_subnet    TEXT    NOT NULL DEFAULT '10.180.0.0/24',
  nat_managed   INTEGER NOT NULL DEFAULT 1,           -- 0 = 共用现有网桥（docker0 等）
  nat_gw        TEXT    NOT NULL DEFAULT '',           -- 非托管网桥的网关，空取网段首地址
  nat_reserved  TEXT    NOT NULL DEFAULT '',           -- 保留段：如 10.0.0.1-10.0.0.50,10.0.0.99
  dns           TEXT    NOT NULL DEFAULT '',            -- 实例 DNS，空格/逗号分隔，空 = 默认
  port_min      INTEGER NOT NULL DEFAULT 20000,
  port_max      INTEGER NOT NULL DEFAULT 60000,
  ports_each    INTEGER NOT NULL DEFAULT 10,
  -- Dedicated IPv6
  v6_enabled    INTEGER NOT NULL DEFAULT 0,
  v6_bridge     TEXT    NOT NULL DEFAULT '',
  v6_cidr       TEXT    NOT NULL DEFAULT '',         -- e.g. 2a01:4f8:1:2::/64
  v6_gw         TEXT    NOT NULL DEFAULT '',
  -- Dedicated public IPv4 pool
  v4_enabled    INTEGER NOT NULL DEFAULT 0,
  v4_bridge     TEXT    NOT NULL DEFAULT '',
  v4_cidr       TEXT    NOT NULL DEFAULT '',         -- 公网 v4 段，如 203.0.113.0/28
  v4_gw         TEXT    NOT NULL DEFAULT '',
  -- Capacity
  kvm_enabled   INTEGER NOT NULL DEFAULT 0,           -- 允许调度 KVM 虚拟机（beta）
  max_instances INTEGER NOT NULL DEFAULT 50,
  enabled       INTEGER NOT NULL DEFAULT 1,
  last_seen_at  INTEGER NOT NULL DEFAULT 0,
  last_status   TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS plans (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL,
  description  TEXT    NOT NULL DEFAULT '',
  cpu          INTEGER NOT NULL,
  memory_mb    INTEGER NOT NULL,
  disk_gb      INTEGER NOT NULL,
  mode         TEXT    NOT NULL DEFAULT 'nat',       -- nat | ipv6 | ipv6only
  instance_type TEXT   NOT NULL DEFAULT 'container',  -- container | vm (KVM, beta)
  node_id      INTEGER NOT NULL DEFAULT 0,            -- 指定节点，0 = 自动调度
  features     TEXT    NOT NULL DEFAULT '',           -- tun,fuse,privileged,nesting
  traffic_gb   INTEGER NOT NULL DEFAULT 0,            -- 月流量限额 GB，0 = 不限
  traffic_mode TEXT    NOT NULL DEFAULT 'both',       -- both | up | down
  rate_down_mbps INTEGER NOT NULL DEFAULT 0,          -- 下行带宽 Mbps，0 = 不限
  rate_up_mbps   INTEGER NOT NULL DEFAULT 0,          -- 上行带宽 Mbps，0 = 不限
  extra_bridges  TEXT    NOT NULL DEFAULT '',          -- 附加网卡网桥，逗号分隔
  v4_pool        TEXT    NOT NULL DEFAULT '',          -- 内网 IP 段限制（须在节点网段内），如 10.180.0.100-10.180.0.200
  keep_source_ip INTEGER NOT NULL DEFAULT 1,           -- NAT 端口转发是否保留真实源 IP（DNAT）
  mounts       TEXT    NOT NULL DEFAULT '',            -- 宿主机目录挂载 src:dst[:ro]（管理员配置）
  extra_disks  TEXT    NOT NULL DEFAULT '',            -- 附加数据盘 GB 列表（"20,50"）
  images       TEXT    NOT NULL DEFAULT 'debian/12,debian/13,alpine/3.21,alpine/3.22',
  price_cents  INTEGER NOT NULL DEFAULT 0,           -- 0 = 不可用余额开通
  duration_days INTEGER NOT NULL DEFAULT 30,
  enabled      INTEGER NOT NULL DEFAULT 1,
  sort_order   INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS codes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  code       TEXT    NOT NULL UNIQUE,
  plan_id    INTEGER NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  node_id    INTEGER NOT NULL DEFAULT 0,             -- 0 = any enabled node
  batch      TEXT    NOT NULL DEFAULT '',
  note       TEXT    NOT NULL DEFAULT '',
  used_by    INTEGER NOT NULL DEFAULT 0,
  used_at    INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,             -- 0 = never
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codes_batch ON codes(batch);
CREATE INDEX IF NOT EXISTS idx_codes_used  ON codes(used_by);

CREATE TABLE IF NOT EXISTS instances (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id     INTEGER NOT NULL REFERENCES nodes(id),
  plan_id     INTEGER NOT NULL DEFAULT 0,
  code_id     INTEGER NOT NULL DEFAULT 0,
  name        TEXT    NOT NULL UNIQUE,               -- LXD instance name
  label       TEXT    NOT NULL DEFAULT '',           -- user-facing nickname
  image       TEXT    NOT NULL,
  family      TEXT    NOT NULL DEFAULT 'debian',
  cpu         INTEGER NOT NULL,
  memory_mb   INTEGER NOT NULL,
  disk_gb     INTEGER NOT NULL,
  mode        TEXT    NOT NULL DEFAULT 'nat',
  instance_type TEXT  NOT NULL DEFAULT 'container',
  nat_addr    TEXT    NOT NULL DEFAULT '',
  ssh_port    INTEGER NOT NULL DEFAULT 0,
  port_from   INTEGER NOT NULL DEFAULT 0,
  port_to     INTEGER NOT NULL DEFAULT 0,
  v6_addr     TEXT    NOT NULL DEFAULT '',
  status      TEXT    NOT NULL DEFAULT 'provisioning',
  error       TEXT    NOT NULL DEFAULT '',
  -- Traffic metering (bytes). last_* mirror the node counters for delta math.
  traffic_limit_gb INTEGER NOT NULL DEFAULT 0,        -- 0 = 不限
  traffic_mode     TEXT    NOT NULL DEFAULT 'both',
  used_rx          INTEGER NOT NULL DEFAULT 0,
  used_tx          INTEGER NOT NULL DEFAULT 0,
  last_rx          INTEGER NOT NULL DEFAULT 0,
  last_tx          INTEGER NOT NULL DEFAULT 0,
  traffic_reset_at INTEGER NOT NULL DEFAULT 0,        -- 下次流量清零时间
  rate_down_mbps   INTEGER NOT NULL DEFAULT 0,
  rate_up_mbps     INTEGER NOT NULL DEFAULT 0,
  extra_bridges    TEXT    NOT NULL DEFAULT '',
  mounts           TEXT    NOT NULL DEFAULT '',        -- 宿主机目录挂载（随套餐复制）
  extra_disks      TEXT    NOT NULL DEFAULT '',        -- 附加数据盘（随套餐复制）
  vnc_port         INTEGER NOT NULL DEFAULT 0,        -- KVM: 宿主机 VNC 端口
  vnc_pass         TEXT    NOT NULL DEFAULT '',       -- KVM: VNC 密码（DES 限 8 位）
  v4_addr          TEXT    NOT NULL DEFAULT '',       -- 独立公网 IPv4
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_inst_user ON instances(user_id);
CREATE INDEX IF NOT EXISTS idx_inst_node ON instances(node_id);
CREATE INDEX IF NOT EXISTS idx_inst_exp  ON instances(expires_at);

CREATE TABLE IF NOT EXISTS audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL DEFAULT 0,
  actor      TEXT    NOT NULL DEFAULT '',
  action     TEXT    NOT NULL,
  detail     TEXT    NOT NULL DEFAULT '',
  ip         TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit(created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount_cents  INTEGER NOT NULL,                    -- 有符号：充值为正，扣款为负
  balance_cents INTEGER NOT NULL,                    -- 该笔入账后的余额
  kind          TEXT    NOT NULL,                    -- recharge | purchase | refund | admin
  ref           TEXT    NOT NULL DEFAULT '',         -- 外部订单号，充值幂等去重
  note          TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tx_user ON transactions(user_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tx_ref ON transactions(ref) WHERE ref != '';

CREATE TABLE IF NOT EXISTS recharge_orders (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  order_no     TEXT    NOT NULL UNIQUE,               -- 站内订单号，充值幂等键
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount_cents INTEGER NOT NULL,                      -- 充值金额（分）
  method       TEXT    NOT NULL,                      -- alipay | wxpay | usdt
  status       TEXT    NOT NULL DEFAULT 'pending',    -- pending | paid | expired
  txid         TEXT    NOT NULL DEFAULT '',           -- USDT 交易号 / 第三方流水号
  created_at   INTEGER NOT NULL,
  paid_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_orders_user ON recharge_orders(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_orders_status ON recharge_orders(status);
