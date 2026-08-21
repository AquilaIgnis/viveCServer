package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

// discardingTestLogger keeps the migration runner quiet. It has something to say on a fresh
// database and nothing on every run after, and neither is what these tests are about.
func discardingTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// uniqueTestEmail keeps each test to its own account.
//
// The suite migrates whatever database it is given and leaves its accounts behind, so a fixed
// address would pass once and then fail on ErrEmailTaken for ever — including against the
// throwaway PostgreSQL a developer keeps running between edits.
func uniqueTestEmail(t *testing.T) string {
	t.Helper()

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generating a test email address: %v", err)
	}
	return "owner-" + hex.EncodeToString(suffix) + "@example.com"
}

// uniqueTokenHash stands in for the digest of a bearer token.
//
// Random for the same reason [uniqueTestEmail] is, and it is the same trap twice: `token_hash` is
// the primary key of `admin_sessions` and unique on `devices`, so a literal passes on a clean
// database and then collides for ever after — including when an earlier failure leaves its rows
// behind, which is exactly when a test most needs to still run.
func uniqueTokenHash(t *testing.T) []byte {
	t.Helper()

	tokenHash := make([]byte, sha256.Size)
	if _, err := rand.Read(tokenHash); err != nil {
		t.Fatalf("generating a test token digest: %v", err)
	}
	return tokenHash
}

// The recovery commands are the one part of this binary an operator reaches for when nothing else
// works, so they are tested against a real database rather than a fake. See
// tests/harness_test.go for the same arrangement and the same environment variable.
func setPasswordPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("VIVE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("VIVE_TEST_DATABASE_URL is not set; skipping the recovery-command tests")
	}

	ctx := context.Background()
	if err := store.ApplyMigrations(ctx, databaseURL, migrations.Files, discardingTestLogger()); err != nil {
		t.Fatalf("migrating the test database: %v", err)
	}
	pool, err := store.OpenPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening the test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestSetPasswordReplacesTheHashAndClosesEveryBrowserSession is the whole of the forgotten-password
// recovery path: the new password works, the old one does not, and whoever was already signed in
// no longer is.
func TestSetPasswordReplacesTheHashAndClosesEveryBrowserSession(t *testing.T) {
	pool := setPasswordPool(t)
	ctx := context.Background()

	email := uniqueTestEmail(t)
	originalHash, err := auth.HashPassword("the one they forgot")
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := store.CreateAccount(ctx, pool, email, originalHash)
	if err != nil {
		t.Fatalf("creating the account under test: %v", err)
	}

	// Two browsers signed in, which is what a reset has to invalidate: a session is a bearer token
	// checked against admin_sessions and never against the password, so changing the password alone
	// would leave both of them signed in.
	firstSession := uniqueTokenHash(t)
	secondSession := uniqueTokenHash(t)
	for _, tokenHash := range [][]byte{firstSession, secondSession} {
		if err := store.CreateAdminSession(ctx, pool, accountID, tokenHash, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("creating a browser session: %v", err)
		}
	}

	closed, err := setAccountPassword(ctx, pool, email, "a brand new passphrase")
	if err != nil {
		t.Fatalf("setting the password: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed %d browser sessions, want 2", closed)
	}

	account, err := store.FindAccountByEmail(ctx, pool, email)
	if err != nil {
		t.Fatalf("reloading the account: %v", err)
	}
	if err := auth.VerifyPassword(account.PasswordHash, "a brand new passphrase"); err != nil {
		t.Fatalf("the new password does not verify: %v", err)
	}
	if err := auth.VerifyPassword(account.PasswordHash, "the one they forgot"); err == nil {
		t.Fatal("the forgotten password still works, so nothing was actually reset")
	}

	if _, err := store.AuthenticateAdminSession(ctx, pool, firstSession); !errors.Is(err, store.ErrAdminSessionNotFound) {
		t.Fatalf("a browser signed in before the reset is still signed in: %v", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, pool, secondSession); !errors.Is(err, store.ErrAdminSessionNotFound) {
		t.Fatalf("a second browser signed in before the reset is still signed in: %v", err)
	}
}

// TestSetPasswordLeavesRegisteredDevicesAlone: a device token is not derived from the password, and
// unpairing every tablet over a forgotten password would cost the owner their sync setup to solve a
// problem they did not have. The panel revokes devices when that is the actual intent.
func TestSetPasswordLeavesRegisteredDevicesAlone(t *testing.T) {
	pool := setPasswordPool(t)
	ctx := context.Background()

	email := uniqueTestEmail(t)
	passwordHash, err := auth.HashPassword("the one they forgot")
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := store.CreateAccount(ctx, pool, email, passwordHash)
	if err != nil {
		t.Fatalf("creating the account under test: %v", err)
	}

	tokenHash := uniqueTokenHash(t)
	deviceID, err := store.RegisterDevice(ctx, pool, accountID, "Studio tablet", "android", tokenHash)
	if err != nil {
		t.Fatalf("registering a device: %v", err)
	}

	if _, err := setAccountPassword(ctx, pool, email, "a brand new passphrase"); err != nil {
		t.Fatalf("setting the password: %v", err)
	}

	syncingDeviceID, syncingAccountID, err := store.AuthenticateDeviceToken(ctx, pool, tokenHash)
	if err != nil {
		t.Fatalf("a registered device stopped syncing after a password change: %v", err)
	}
	if syncingDeviceID != deviceID || syncingAccountID != accountID {
		t.Fatalf("device token resolved to device %s of account %s, want %s and %s",
			syncingDeviceID, syncingAccountID, deviceID, accountID)
	}
}

// TestSetPasswordRefusesWhatItCannotDo covers the two ways an operator gets this wrong at three in
// the morning: the wrong address, and a password the server would not have accepted at setup either.
func TestSetPasswordRefusesWhatItCannotDo(t *testing.T) {
	pool := setPasswordPool(t)
	ctx := context.Background()

	email := uniqueTestEmail(t)
	passwordHash, err := auth.HashPassword("the one they forgot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, pool, email, passwordHash); err != nil {
		t.Fatalf("creating the account under test: %v", err)
	}

	if _, err := setAccountPassword(ctx, pool, "nobody-"+email, "a brand new passphrase"); !errors.Is(err, ErrAccountNotFoundForEmail) {
		t.Fatalf("unknown email = %v, want ErrAccountNotFoundForEmail", err)
	}

	// Refused before the account is even looked up, so the weak-password rule is the one in
	// auth.ValidatePassword rather than a second copy of it living in this command.
	if _, err := setAccountPassword(ctx, pool, email, "short"); err == nil {
		t.Fatal("a password too weak for setup was accepted as a replacement")
	}

	// And nothing moved: a refused reset must leave the owner able to sign in with what they have.
	account, err := store.FindAccountByEmail(ctx, pool, email)
	if err != nil {
		t.Fatalf("reloading the account: %v", err)
	}
	if err := auth.VerifyPassword(account.PasswordHash, "the one they forgot"); err != nil {
		t.Fatalf("a refused reset changed the stored password anyway: %v", err)
	}
}

// TestReadPasswordLineTakesTheFirstLineOfAPipe covers the unattended half of readPassword, which is
// the half a test can reach: the interactive half needs a pseudo-terminal, and what makes it
// interactive is precisely that there is not one here.
//
// The cases are the ones a shell actually produces. `echo` adds a newline, a here-string may not,
// and a file written on Windows or copied through one carries a carriage return that would
// otherwise become part of the password and make it unusable for ever after.
func TestReadPasswordLineTakesTheFirstLineOfAPipe(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		piped string
		want  string
	}{
		{name: "newline terminated", piped: "correct horse battery staple\n", want: "correct horse battery staple"},
		{name: "no trailing newline", piped: "correct horse battery staple", want: "correct horse battery staple"},
		{name: "carriage return", piped: "correct horse battery staple\r\n", want: "correct horse battery staple"},
		{name: "later lines ignored", piped: "first line\nsecond line\n", want: "first line"},
		{name: "spaces are part of it", piped: "  padded passphrase  \n", want: "  padded passphrase  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			password, err := readPasswordLine(strings.NewReader(testCase.piped))
			if err != nil {
				t.Fatalf("reading %q: %v", testCase.piped, err)
			}
			if password != testCase.want {
				t.Fatalf("read %q, want %q", password, testCase.want)
			}
		})
	}
}

// TestReadPasswordLineOnEmptyInputSaysWhatToDo: a command that blocks on a closed stdin looks like a
// hang, and one that reports "no password" without saying how to supply it leaves the operator
// reading source at the worst possible moment.
func TestReadPasswordLineOnEmptyInputSaysWhatToDo(t *testing.T) {
	_, err := readPasswordLine(strings.NewReader(""))
	if err == nil {
		t.Fatal("empty stdin produced a password")
	}
	for _, mention := range []string{"stdin", "terminal", passwordEnvironmentVariable} {
		if !strings.Contains(err.Error(), mention) {
			t.Fatalf("error %q does not mention %q, which is one of the three ways out", err, mention)
		}
	}
}
