// Package store owns the panel's database. It supports SQLite (default),
// PostgreSQL and MySQL through one small dialect shim: every statement in this
// package is parameterised with `?`, and the shim rebinds to `$N` for
// PostgreSQL. No caller-supplied value is ever concatenated into SQL text.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // pure-Go MySQL driver
	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go PostgreSQL driver
	_ "modernc.org/sqlite"             // pure-Go SQLite driver, no cgo — keeps the build static
)

//go:embed schema.sql schema_postgres.sql schema_mysql.sql
var schemaFS embed.FS

// Supported dialects.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
	MySQL    = "mysql"
)

// DB wraps a database handle and remembers its dialect. It embeds *sql.DB so
// non-query helpers (Close, Ping, …) stay available, and shadows the three
// context query methods to rebind placeholders for the active dialect.
type DB struct {
	*sql.DB
	driver string
}

// Driver reports the active dialect ("sqlite" | "postgres" | "mysql").
func (d *DB) Driver() string { return d.driver }

// Open connects using the given dialect and DSN, applies the schema and (for
// SQLite) runs incremental migrations.
//
//   - sqlite:   dsn is a filesystem path; pragmas are added automatically.
//   - postgres: dsn is a libpq/pgx URL or keyword string.
//   - mysql:    dsn is a go-sql-driver DSN, e.g. user:pass@tcp(host:3306)/dbname
func Open(driver, dsn string) (*DB, error) {
	var sqlDriver, schemaFile string
	switch driver {
	case SQLite, "":
		driver, sqlDriver, schemaFile = SQLite, "sqlite", "schema.sql"
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"+
			"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", dsn)
	case Postgres:
		sqlDriver, schemaFile = "pgx", "schema_postgres.sql"
	case MySQL:
		sqlDriver, schemaFile = "mysql", "schema_mysql.sql"
	default:
		return nil, fmt.Errorf("unsupported db driver %q (use sqlite, postgres or mysql)", driver)
	}

	sdb, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, err
	}
	if driver == SQLite {
		// SQLite tolerates one writer; a small pool avoids lock churn.
		sdb.SetMaxOpenConns(8)
		sdb.SetMaxIdleConns(4)
	} else {
		// Client/server engines handle real concurrency.
		sdb.SetMaxOpenConns(25)
		sdb.SetMaxIdleConns(10)
	}
	sdb.SetConnMaxLifetime(time.Hour)

	if err := sdb.Ping(); err != nil {
		return nil, err
	}
	db := &DB{sdb, driver}

	schema, err := schemaFS.ReadFile(schemaFile)
	if err != nil {
		return nil, err
	}
	if err := db.execScript(string(schema)); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Grow pre-existing databases: the ALTERs parse on every dialect (with a
	// TEXT→VARCHAR rewrite for MySQL) and an already-present column is
	// tolerated.
	if err := migrate(sdb, driver); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// execScript runs a multi-statement schema file one statement at a time. Not
// every driver accepts multiple commands in a single Exec (pgx and MySQL do
// not by default), so we split on `;`. The schema files never contain a `;`
// inside a string literal, which keeps the split safe.
func (d *DB) execScript(script string) error {
	for _, stmt := range splitSQL(script) {
		if _, err := d.DB.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// splitSQL strips `--` line comments and splits a script into trimmed,
// non-empty statements on `;`.
func splitSQL(script string) []string {
	var clean strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		clean.WriteString(line)
		clean.WriteString("\n")
	}
	var out []string
	for _, s := range strings.Split(clean.String(), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// rebind converts `?` placeholders to `$N` for PostgreSQL; other dialects use
// `?` natively. `?` inside a single-quoted string literal is left untouched.
func rebind(driver, q string) string {
	if driver != Postgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case '\'':
			b.WriteByte(c)
			for i++; i < len(q); i++ {
				b.WriteByte(q[i])
				if q[i] == '\'' {
					break
				}
			}
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (d *DB) rebind(q string) string { return rebind(d.driver, q) }

// ---- placeholder-rebinding query methods (shadow the embedded *sql.DB) ----

func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.rebind(q), args...)
}
func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.rebind(q), args...)
}
func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.rebind(q), args...)
}

// insertID inserts a row and returns its generated id. MySQL and SQLite report
// it via LastInsertId; PostgreSQL has no such notion, so we append RETURNING.
func (d *DB) insertID(ctx context.Context, query string, args ...any) (int64, error) {
	if d.driver == Postgres {
		var id int64
		if err := d.QueryRowContext(ctx, query+" RETURNING id", args...).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	res, err := d.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Tx wraps *sql.Tx with the same placeholder rebinding.
type Tx struct {
	*sql.Tx
	driver string
}

func (t *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, rebind(t.driver, q), args...)
}
func (t *Tx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, rebind(t.driver, q), args...)
}
func (t *Tx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, rebind(t.driver, q), args...)
}
func (t *Tx) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return t.Tx.PrepareContext(ctx, rebind(t.driver, q))
}

// Tx runs fn inside a transaction, rolling back on error.
func (d *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	raw, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = raw.Rollback() }()
	if err := fn(&Tx{raw, d.driver}); err != nil {
		return err
	}
	return raw.Commit()
}

// isUniqueErr reports whether err is a unique-constraint violation, across all
// three dialects' driver messages.
func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || // sqlite
		strings.Contains(s, "duplicate key value") || // postgres
		strings.Contains(s, "Duplicate entry") // mysql
}

// nullIfEmpty returns nil for an empty string so it lands as SQL NULL — used
// for optional-unique columns where multiple NULLs must coexist.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// migrate brings pre-existing databases up to the current schema. CREATE
// TABLE IF NOT EXISTS never adds columns, so late additions live here; each
// ALTER is a no-op (tolerated error) when the column already exists. MySQL
// forbids DEFAULT on TEXT columns, so TEXT adds are rewritten to VARCHAR.
func migrate(sdb *sql.DB, driver string) error {
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
		`ALTER TABLE plans ADD COLUMN mounts TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE instances ADD COLUMN mounts TEXT NOT NULL DEFAULT ''`,
	} {
		if driver == MySQL {
			stmt = strings.ReplaceAll(stmt, "TEXT NOT NULL DEFAULT", "VARCHAR(1024) NOT NULL DEFAULT")
		}
		if _, err := sdb.Exec(stmt); err != nil {
			// Tolerated: sqlite "duplicate column name", mysql "Duplicate
			// column name", postgres `column "x" ... already exists`.
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists") {
				continue
			}
			return err
		}
	}
	return nil
}
