package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// EmailVerification is a pending registration challenge (password hash held
// until the 6-digit code is confirmed).
type EmailVerification struct {
	ID           int64
	Email        string
	PasswordHash string
	Code         string
	Token        string
	Attempts     int
	ExpiresAt    int64
	CreatedAt    int64
}

const emailVerifCols = `id, email, password_hash, code, token, attempts, expires_at, created_at`

func scanEmailVerification(s interface{ Scan(...any) error }) (*EmailVerification, error) {
	var v EmailVerification
	err := s.Scan(&v.ID, &v.Email, &v.PasswordHash, &v.Code, &v.Token,
		&v.Attempts, &v.ExpiresAt, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &v, err
}

// SaveEmailVerification upserts a challenge for email: any previous row for
// the same address is removed so re-register / resend stay single-row.
func (d *DB) SaveEmailVerification(ctx context.Context, email, passwordHash, code, token string, ttl time.Duration) error {
	email = normEmail(email)
	_, _ = d.ExecContext(ctx, `DELETE FROM email_verifications WHERE email = ?`, email)
	ts := now()
	exp := ts + int64(ttl/time.Second)
	_, err := d.ExecContext(ctx,
		`INSERT INTO email_verifications (email, password_hash, code, token, attempts, expires_at, created_at)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		email, passwordHash, code, token, exp, ts)
	return err
}

// EmailVerificationByToken loads a challenge by opaque URL token.
func (d *DB) EmailVerificationByToken(ctx context.Context, token string) (*EmailVerification, error) {
	return scanEmailVerification(d.QueryRowContext(ctx,
		`SELECT `+emailVerifCols+` FROM email_verifications WHERE token = ?`, token))
}

// BumpEmailVerificationAttempts increments the wrong-code counter.
func (d *DB) BumpEmailVerificationAttempts(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE email_verifications SET attempts = attempts + 1 WHERE id = ?`, id)
	return err
}

// DeleteEmailVerification removes a challenge by id.
func (d *DB) DeleteEmailVerification(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM email_verifications WHERE id = ?`, id)
	return err
}

// PurgeExpiredEmailVerifications drops rows past expires_at.
func (d *DB) PurgeExpiredEmailVerifications(ctx context.Context) error {
	_, err := d.ExecContext(ctx,
		`DELETE FROM email_verifications WHERE expires_at < ?`, now())
	return err
}
