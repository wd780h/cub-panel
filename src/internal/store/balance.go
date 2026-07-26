// Balance bookkeeping. Every change to users.balance_cents goes through
// AdjustBalance inside one transaction, paired with a ledger row, so the
// ledger always sums to the live balance.
package store

import (
	"context"
	"errors"
)

// ErrInsufficient is returned when a debit would take a balance negative.
var ErrInsufficient = errors.New("insufficient balance")

// ErrDuplicateRef is returned when a recharge ref was already processed.
var ErrDuplicateRef = errors.New("duplicate ref")

// Transaction is one balance ledger entry.
type Transaction struct {
	ID           int64
	UserID       int64
	AmountCents  int64 // signed: credit > 0, debit < 0
	BalanceCents int64 // balance after this entry
	Kind         string
	Ref          string
	Note         string
	CreatedAt    int64
}

// AdjustBalance applies a signed delta to a user's balance and records the
// ledger entry, returning the new balance. Debits that would go negative
// fail with ErrInsufficient; a non-empty ref that was seen before fails with
// ErrDuplicateRef, which makes recharge callbacks idempotent.
func (d *DB) AdjustBalance(ctx context.Context, userID, delta int64, kind, ref, note string) (int64, error) {
	var balance int64
	err := d.Tx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE users SET balance_cents = balance_cents + ?
			  WHERE id = ? AND balance_cents + ? >= 0`, delta, userID, delta)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Distinguish a missing account from an over-draft.
			var exists int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrInsufficient
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT balance_cents FROM users WHERE id = ?`, userID).Scan(&balance); err != nil {
			return err
		}
		// Idempotency is enforced by the database, never by a racy pre-check:
		// sqlite/postgres use a partial unique index (WHERE ref != ''); MySQL
		// has no partial index, so empty refs are stored as NULL (its UNIQUE
		// ignores NULLs) and real refs collide on the unique key.
		refVal := any(ref)
		if d.driver == MySQL {
			refVal = nullIfEmpty(ref)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO transactions (user_id, amount_cents, balance_cents, kind, ref, note, created_at)
			 VALUES (?,?,?,?,?,?,?)`, userID, delta, balance, kind, refVal, note, now()); err != nil {
			if isUniqueErr(err) {
				return ErrDuplicateRef
			}
			return err
		}
		return nil
	})
	return balance, err
}

// ListTransactions returns a user's ledger, newest first. userID 0 lists all
// users (admin view).
func (d *DB) ListTransactions(ctx context.Context, userID int64, limit int) ([]*Transaction, error) {
	q := `SELECT id, user_id, amount_cents, balance_cents, kind, COALESCE(ref,''), note, created_at
	        FROM transactions`
	args := []any{}
	if userID > 0 {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.UserID, &t.AmountCents, &t.BalanceCents,
			&t.Kind, &t.Ref, &t.Note, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}
