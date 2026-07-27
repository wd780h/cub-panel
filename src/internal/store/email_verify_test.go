package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEmailVerificationLifecycle(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(SQLite, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.SaveEmailVerification(ctx, "User@Example.COM", "hash1", "123456", "tok-a", 15*time.Minute); err != nil {
		t.Fatalf("save: %v", err)
	}
	v, err := db.EmailVerificationByToken(ctx, "tok-a")
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if v.Email != "user@example.com" {
		t.Fatalf("email norm: got %q", v.Email)
	}
	if v.Code != "123456" || v.PasswordHash != "hash1" {
		t.Fatalf("fields mismatch: %+v", v)
	}

	// Replace on same email.
	if err := db.SaveEmailVerification(ctx, "user@example.com", "hash2", "654321", "tok-b", 15*time.Minute); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := db.EmailVerificationByToken(ctx, "tok-a"); err != ErrNotFound {
		t.Fatalf("old token should be gone, err=%v", err)
	}
	v2, err := db.EmailVerificationByToken(ctx, "tok-b")
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if v2.Code != "654321" || v2.PasswordHash != "hash2" {
		t.Fatalf("replace fields: %+v", v2)
	}

	if err := db.BumpEmailVerificationAttempts(ctx, v2.ID); err != nil {
		t.Fatalf("bump: %v", err)
	}
	v3, _ := db.EmailVerificationByToken(ctx, "tok-b")
	if v3.Attempts != 1 {
		t.Fatalf("attempts=%d", v3.Attempts)
	}

	if err := db.DeleteEmailVerification(ctx, v3.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.EmailVerificationByToken(ctx, "tok-b"); err != ErrNotFound {
		t.Fatalf("deleted still present: %v", err)
	}
}

func TestPurgeExpiredEmailVerifications(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(SQLite, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Negative TTL → already expired.
	if err := db.SaveEmailVerification(ctx, "a@ex.com", "h", "111111", "tok-exp", -time.Minute); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := db.SaveEmailVerification(ctx, "b@ex.com", "h", "222222", "tok-ok", time.Hour); err != nil {
		t.Fatalf("save ok: %v", err)
	}
	if err := db.PurgeExpiredEmailVerifications(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := db.EmailVerificationByToken(ctx, "tok-exp"); err != ErrNotFound {
		t.Fatalf("expired not purged")
	}
	if _, err := db.EmailVerificationByToken(ctx, "tok-ok"); err != nil {
		t.Fatalf("live row purged: %v", err)
	}
}
