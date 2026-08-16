package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrEmailTaken is returned when an account already exists for an address.
	ErrEmailTaken = errors.New("an account already exists for that email address")

	// ErrAccountNotFound is returned when no account matches. Callers must not turn this into a
	// distinct response: telling a stranger which addresses have accounts is an enumeration oracle.
	ErrAccountNotFound = errors.New("no such account")
)

// uniqueViolationCode is PostgreSQL's SQLSTATE for a unique index conflict.
const uniqueViolationCode = "23505"

// Account is what the server needs to know about an account to authenticate against it.
type Account struct {
	ID           string
	Email        string
	PasswordHash string
}

// CreateAccount inserts an account and returns its server-assigned id.
//
// Uniqueness is enforced by the index rather than by a preceding SELECT. A check-then-insert is a
// race — two registrations for the same address can both find nothing and both proceed — and the
// index has to exist regardless, so the check would be duplicated work that is also wrong.
func CreateAccount(ctx context.Context, pool *pgxpool.Pool, normalisedEmail string, passwordHash string) (string, error) {
	const statement = `
		INSERT INTO accounts (email, password_hash)
		VALUES ($1, $2)
		RETURNING id::text`

	var accountID string
	err := pool.QueryRow(ctx, statement, normalisedEmail, passwordHash).Scan(&accountID)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == uniqueViolationCode {
			return "", ErrEmailTaken
		}
		return "", fmt.Errorf("creating an account: %w", err)
	}
	return accountID, nil
}

// FindAccountByEmail looks an account up for a sign-in.
func FindAccountByEmail(ctx context.Context, pool *pgxpool.Pool, normalisedEmail string) (Account, error) {
	const statement = `
		SELECT id::text, email, password_hash
		FROM accounts
		WHERE email = $1`

	var account Account
	err := pool.QueryRow(ctx, statement, normalisedEmail).
		Scan(&account.ID, &account.Email, &account.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("looking up an account: %w", err)
	}
	return account, nil
}

// UpdatePasswordHash replaces a stored hash, used to upgrade one that was made with weaker
// parameters than the server now uses.
func UpdatePasswordHash(ctx context.Context, pool *pgxpool.Pool, accountID string, passwordHash string) error {
	const statement = `UPDATE accounts SET password_hash = $2 WHERE id = $1::uuid`

	if _, err := pool.Exec(ctx, statement, accountID, passwordHash); err != nil {
		return fmt.Errorf("updating a password hash: %w", err)
	}
	return nil
}

// CountAccounts reports how many accounts exist, which is what tells a first-run server that it is
// unconfigured.
func CountAccounts(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var accountCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&accountCount); err != nil {
		return 0, fmt.Errorf("counting accounts: %w", err)
	}
	return accountCount, nil
}
