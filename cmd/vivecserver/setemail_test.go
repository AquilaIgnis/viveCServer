package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

// setEmailPool gives these tests an empty account table without deleting anything from the
// developer's ordinary integration database. The command intentionally selects the server's sole
// account, so it cannot be tested against the shared pool that other account-isolation tests fill.
func setEmailPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("VIVE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("VIVE_TEST_DATABASE_URL is not set; skipping the set-email tests")
	}
	ctx := context.Background()
	basePool, err := store.OpenPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening the base test pool: %v", err)
	}
	t.Cleanup(basePool.Close)

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generating a test schema name: %v", err)
	}
	schemaName := "set_email_" + hex.EncodeToString(suffix)
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	if _, err := basePool.Exec(ctx, `CREATE SCHEMA `+quotedSchema); err != nil {
		t.Fatalf("creating the set-email test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := basePool.Exec(context.Background(), `DROP SCHEMA `+quotedSchema+` CASCADE`); err != nil {
			t.Errorf("dropping the set-email test schema: %v", err)
		}
	})

	parsedDatabaseURL, err := url.Parse(databaseURL)
	if err != nil || (parsedDatabaseURL.Scheme != "postgres" && parsedDatabaseURL.Scheme != "postgresql") {
		t.Fatal("VIVE_TEST_DATABASE_URL must be a PostgreSQL URL for the set-email tests")
	}
	query := parsedDatabaseURL.Query()
	query.Set("search_path", schemaName)
	parsedDatabaseURL.RawQuery = query.Encode()
	isolatedDatabaseURL := parsedDatabaseURL.String()
	if err := store.ApplyMigrations(ctx, isolatedDatabaseURL, migrations.Files, discardingTestLogger()); err != nil {
		t.Fatalf("migrating the set-email test schema: %v", err)
	}
	pool, err := store.OpenPool(ctx, isolatedDatabaseURL)
	if err != nil {
		t.Fatalf("opening the set-email test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSetEmailChangesOnlyTheSignInAddress(t *testing.T) {
	pool := setEmailPool(t)
	ctx := context.Background()

	currentEmail := uniqueTestEmail(t)
	newEmail := uniqueTestEmail(t)
	password := "the password stays the same"
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := store.CreateAccount(ctx, pool, currentEmail, passwordHash)
	if err != nil {
		t.Fatalf("creating the account under test: %v", err)
	}

	sessionTokenHash := uniqueTokenHash(t)
	if err := store.CreateAdminSession(ctx, pool, accountID, sessionTokenHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("creating a browser session: %v", err)
	}
	deviceTokenHash := uniqueTokenHash(t)
	deviceID, err := store.RegisterDevice(ctx, pool, accountID, "Studio tablet", "android", deviceTokenHash)
	if err != nil {
		t.Fatalf("registering a device: %v", err)
	}

	if err := setAdminEmail(ctx, pool, newEmail); err != nil {
		t.Fatalf("setting the email: %v", err)
	}

	if _, err := store.FindAccountByEmail(ctx, pool, currentEmail); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("the old email still resolves to an account: %v", err)
	}
	account, err := store.FindAccountByEmail(ctx, pool, newEmail)
	if err != nil {
		t.Fatalf("the new email does not resolve to the account: %v", err)
	}
	if account.ID != accountID {
		t.Fatalf("new email resolved to account %s, want %s", account.ID, accountID)
	}
	if err := auth.VerifyPassword(account.PasswordHash, password); err != nil {
		t.Fatalf("changing the email changed the password: %v", err)
	}

	sessionAccount, err := store.AuthenticateAdminSession(ctx, pool, sessionTokenHash)
	if err != nil {
		t.Fatalf("changing the email signed out the browser: %v", err)
	}
	if sessionAccount.ID != accountID || sessionAccount.Email != newEmail {
		t.Fatalf("browser session resolved to account %s at %s, want %s at %s",
			sessionAccount.ID, sessionAccount.Email, accountID, newEmail)
	}
	syncingDeviceID, syncingAccountID, err := store.AuthenticateDeviceToken(ctx, pool, deviceTokenHash)
	if err != nil {
		t.Fatalf("changing the email disconnected a registered device: %v", err)
	}
	if syncingDeviceID != deviceID || syncingAccountID != accountID {
		t.Fatalf("device token resolved to device %s of account %s, want %s and %s",
			syncingDeviceID, syncingAccountID, deviceID, accountID)
	}
}

func TestAccountCreationStopsAfterTheFirstAccount(t *testing.T) {
	pool := setEmailPool(t)
	ctx := context.Background()

	if err := setAdminEmail(ctx, pool, uniqueTestEmail(t)); err == nil {
		t.Fatal("an empty database was reported as having an admin account")
	}

	passwordHash, err := auth.HashPassword("the password stays the same")
	if err != nil {
		t.Fatal(err)
	}
	firstEmail := uniqueTestEmail(t)
	if _, err := store.CreateAccount(ctx, pool, firstEmail, passwordHash); err != nil {
		t.Fatalf("creating the existing account fixture: %v", err)
	}
	secondEmail := uniqueTestEmail(t)
	if _, err := store.CreateInitialAccount(ctx, pool, secondEmail, passwordHash); !errors.Is(err, store.ErrSetupAlreadyComplete) {
		t.Fatalf("creating another account = %v, want ErrSetupAlreadyComplete", err)
	}
	if _, err := store.FindAccountByEmail(ctx, pool, secondEmail); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("the refused account unexpectedly exists: %v", err)
	}
}
