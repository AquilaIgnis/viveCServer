package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAdminSessionNotFound covers an unknown and an expired browser session. Callers deliberately
// treat both as a request to sign in again.
var ErrAdminSessionNotFound = errors.New("admin session is unknown or expired")

// CreateAdminSession persists a browser session by digest. The plaintext exists only in the
// browser cookie, so a database dump cannot be used to sign into the panel.
func CreateAdminSession(
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID string,
	tokenHash []byte,
	expiresAt time.Time,
) error {
	const statement = `
		INSERT INTO admin_sessions (token_hash, account_id, expires_at)
		VALUES ($1, $2::uuid, $3)`
	if _, err := pool.Exec(ctx, statement, tokenHash, accountID, expiresAt); err != nil {
		return fmt.Errorf("creating an admin session: %w", err)
	}
	return nil
}

// AuthenticateAdminSession resolves an unexpired session to its account.
func AuthenticateAdminSession(ctx context.Context, pool *pgxpool.Pool, tokenHash []byte) (Account, error) {
	const statement = `
		SELECT a.id::text, a.email, a.password_hash, a.created_at, a.storage_bytes
		FROM admin_sessions s
		JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`

	var account Account
	err := pool.QueryRow(ctx, statement, tokenHash).Scan(
		&account.ID,
		&account.Email,
		&account.PasswordHash,
		&account.CreatedAt,
		&account.StorageBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAdminSessionNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("authenticating an admin session: %w", err)
	}
	return account, nil
}

// DeleteAdminSession signs one browser out. Expired sessions are also pruned opportunistically so
// a personal server that runs for years does not accumulate a row for every past login.
func DeleteAdminSession(ctx context.Context, pool *pgxpool.Pool, tokenHash []byte) error {
	const statement = `DELETE FROM admin_sessions WHERE token_hash = $1 OR expires_at <= now()`
	if _, err := pool.Exec(ctx, statement, tokenHash); err != nil {
		return fmt.Errorf("deleting an admin session: %w", err)
	}
	return nil
}
