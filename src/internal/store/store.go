// Package store owns the panel's SQLite database. Every statement in this
// package is parameterised; no caller-supplied value is ever concatenated into
// SQL text.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo, keeps the build static
)

//go:embed schema.sql
var schemaFS embed.FS

// DB wraps the SQLite handle.
type DB struct{ *sql.DB }

// Open connects to the database, applies pragmas and runs the schema.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite tolerates one writer; keeping the pool small avoids lock churn.
	sdb.SetMaxOpenConns(8)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxLifetime(time.Hour)

	if err := sdb.Ping(); err != nil {
		return nil, err
	}
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, err
	}
	if _, err := sdb.Exec(string(schema)); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(sdb); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{sdb}, nil
}

// migrate brings pre-existing databases up to the current schema. CREATE
// TABLE IF NOT EXISTS never adds columns, so late additions live here; each
// ALTER is a no-op (tolerated error) when the column already exists.
func migrate(sdb *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN balance_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN price_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN features TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN nat_managed INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE nodes ADD COLUMN nat_gw TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN nat_reserved TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN dns TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN cert_fp TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plans ADD COLUMN traffic_gb INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN traffic_mode TEXT NOT NULL DEFAULT 'both'`,
		`ALTER TABLE instances ADD COLUMN traffic_limit_gb INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN traffic_mode TEXT NOT NULL DEFAULT 'both'`,
		`ALTER TABLE instances ADD COLUMN used_rx INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN used_tx INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN last_rx INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN last_tx INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN traffic_reset_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN rate_down_mbps INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN rate_up_mbps INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN rate_down_mbps INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN rate_up_mbps INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE plans ADD COLUMN instance_type TEXT NOT NULL DEFAULT 'container'`,
		`ALTER TABLE nodes ADD COLUMN kvm_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN instance_type TEXT NOT NULL DEFAULT 'container'`,
		`ALTER TABLE plans ADD COLUMN extra_bridges TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE instances ADD COLUMN extra_bridges TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE instances ADD COLUMN vnc_port INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE instances ADD COLUMN vnc_pass TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN v4_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN v4_bridge TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN v4_cidr TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN v4_gw TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plans ADD COLUMN v4_pool TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plans ADD COLUMN keep_source_ip INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE instances ADD COLUMN v4_addr TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := sdb.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// Tx runs fn inside a transaction, rolling back on error.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
