// Recharge orders. A paid order credits the user's balance through the same
// idempotent AdjustBalance path (order_no is the transaction ref), so a
// replayed gateway callback never double-credits.
package store

import (
	"context"
	"database/sql"
	"errors"
)

// Order is one recharge attempt.
type Order struct {
	ID          int64
	OrderNo     string
	UserID      int64
	AmountCents int64
	Method      string // alipay | wxpay | usdt
	Status      string // pending | paid | expired
	TxID        string
	CreatedAt   int64
	PaidAt      int64
	// Joined for admin display.
	UserEmail string
}

const orderCols = `id, order_no, user_id, amount_cents, method, status, txid, created_at, paid_at`

func scanOrder(s interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	err := s.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.AmountCents, &o.Method, &o.Status, &o.TxID, &o.CreatedAt, &o.PaidAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &o, err
}

// CreateOrder inserts a pending recharge order.
func (d *DB) CreateOrder(ctx context.Context, o *Order) (int64, error) {
	return d.insertID(ctx,
		`INSERT INTO recharge_orders (order_no, user_id, amount_cents, method, status, txid, created_at)
		 VALUES (?,?,?,?,'pending',?,?)`,
		o.OrderNo, o.UserID, o.AmountCents, o.Method, o.TxID, now())
}

// OrderByNo looks up an order by its station order number.
func (d *DB) OrderByNo(ctx context.Context, orderNo string) (*Order, error) {
	return scanOrder(d.QueryRowContext(ctx, `SELECT `+orderCols+` FROM recharge_orders WHERE order_no = ?`, orderNo))
}

// OrderByID looks up an order by id.
func (d *DB) OrderByID(ctx context.Context, id int64) (*Order, error) {
	return scanOrder(d.QueryRowContext(ctx, `SELECT `+orderCols+` FROM recharge_orders WHERE id = ?`, id))
}

// SetOrderTxID records the user-supplied transaction id (USDT).
func (d *DB) SetOrderTxID(ctx context.Context, id int64, txid string) error {
	_, err := d.ExecContext(ctx, `UPDATE recharge_orders SET txid = ? WHERE id = ?`, txid, id)
	return err
}

// MarkOrderPaid credits the user's balance and flips the order to paid, all
// in one transaction. Idempotent: a second call (replayed callback / double
// approval) is a no-op that still reports success. Returns whether it was the
// call that actually credited.
func (d *DB) MarkOrderPaid(ctx context.Context, orderNo, txid string) (credited bool, err error) {
	err = d.Tx(ctx, func(tx *Tx) error {
		var o Order
		row := tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM recharge_orders WHERE order_no = ?`, orderNo)
		if e := row.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.AmountCents, &o.Method, &o.Status, &o.TxID, &o.CreatedAt, &o.PaidAt); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrNotFound
			}
			return e
		}
		if o.Status == "paid" {
			return nil // already credited
		}
		if _, e := tx.ExecContext(ctx,
			`UPDATE recharge_orders SET status='paid', txid=?, paid_at=? WHERE id=?`,
			nonEmpty(txid, o.TxID), now(), o.ID); e != nil {
			return e
		}
		// Credit via the ledger, keyed on the order number for idempotency.
		if _, e := tx.ExecContext(ctx,
			`UPDATE users SET balance_cents = balance_cents + ? WHERE id = ?`, o.AmountCents, o.UserID); e != nil {
			return e
		}
		var bal int64
		if e := tx.QueryRowContext(ctx, `SELECT balance_cents FROM users WHERE id = ?`, o.UserID).Scan(&bal); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx,
			`INSERT INTO transactions (user_id, amount_cents, balance_cents, kind, ref, note, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			o.UserID, o.AmountCents, bal, "recharge", "order:"+orderNo, o.Method+" 充值", now()); e != nil {
			return e
		}
		credited = true
		return nil
	})
	return credited, err
}

// ListUserOrders returns a user's recent orders.
func (d *DB) ListUserOrders(ctx context.Context, userID int64, limit int) ([]*Order, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+orderCols+` FROM recharge_orders WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListOrders returns recent orders across all users (admin), newest first.
// onlyPending filters to orders awaiting confirmation.
func (d *DB) ListOrders(ctx context.Context, onlyPending bool, limit int) ([]*Order, error) {
	q := `SELECT o.id, o.order_no, o.user_id, o.amount_cents, o.method, o.status, o.txid, o.created_at, o.paid_at, COALESCE(u.email,'')
	       FROM recharge_orders o LEFT JOIN users u ON u.id = o.user_id`
	if onlyPending {
		q += ` WHERE o.status = 'pending'`
	}
	q += ` ORDER BY o.id DESC LIMIT ?`
	rows, err := d.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.AmountCents, &o.Method,
			&o.Status, &o.TxID, &o.CreatedAt, &o.PaidAt, &o.UserEmail); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
