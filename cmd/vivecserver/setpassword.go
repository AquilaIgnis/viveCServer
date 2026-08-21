package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

// runSetPassword replaces an existing account's password from the command line.
//
// This is the answer to "the owner forgot the password", and before it existed there was no answer.
// The panel's setup wizard closes permanently once an account exists, `create-account` refuses an
// email it already holds, and the stored hash is Argon2id — so a `psql` prompt cannot produce a
// replacement, and the only thing left was deleting the account row, which cascades away every
// notebook, page, stroke and picture the server holds. A self-hoster with root on the box should
// never be one forgotten password away from that:
//
//	docker compose -f deploy/docker-compose.yml run --rm -T vivecserver set-password -email you@example.com
//
// The password is read exactly the way create-account reads it, so there is one answer to "how do I
// supply it without putting it in my shell history" rather than two.
func runSetPassword(arguments []string) error {
	flags := flag.NewFlagSet("set-password", flag.ContinueOnError)
	emailFlag := flags.String("email", "", "email address of the account to change (required)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: vivecserver set-password -email <address>\n\n")
		fmt.Fprintf(flags.Output(), "Replaces the password of an existing account and signs every browser out of it.\n")
		fmt.Fprintf(flags.Output(), "Registered devices keep syncing; revoke them from the panel if that is not what you want.\n\n")
		fmt.Fprintf(flags.Output(), "On a terminal the password is prompted for, twice and hidden.\n")
		fmt.Fprintf(flags.Output(), "Otherwise it is read from stdin, or from %s.\n\n", passwordEnvironmentVariable)
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if strings.TrimSpace(*emailFlag) == "" {
		flags.Usage()
		return errors.New("-email is required")
	}

	password, err := readPassword("New password for " + *emailFlag + ": ")
	if err != nil {
		return err
	}

	settings, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	// Same reasoning as create-account: the schema may be older than this binary, the call is
	// idempotent, and it takes the advisory lock the server takes.
	if err := store.ApplyMigrations(ctx, settings.DatabaseURL, migrations.Files, logger); err != nil {
		return err
	}

	pool, err := store.OpenPool(ctx, settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	email, err := auth.NormaliseEmail(*emailFlag)
	if err != nil {
		return err
	}

	closedSessions, err := setAccountPassword(ctx, pool, email, password)
	if err != nil {
		return err
	}

	fmt.Printf("password changed for %s\n", email)
	switch closedSessions {
	case 0:
		fmt.Println("no browser was signed in; sign in at the admin panel with the new password")
	case 1:
		fmt.Println("signed 1 browser session out; sign in again with the new password")
	default:
		fmt.Printf("signed %d browser sessions out; sign in again with the new password\n", closedSessions)
	}
	// Said rather than done. A device token is what a tablet syncs with and is not derived from the
	// password, so unpairing every device on a forgotten password would cost the owner their whole
	// sync setup to solve a problem they did not have. When the reset *is* because a device went
	// missing, revoking it is the action that matters and the panel already offers it per device.
	fmt.Println("registered devices are unaffected; revoke any you no longer trust from the Devices tab")
	return nil
}

// ErrAccountNotFoundForEmail names the one mistake this command expects: a typo, or the address the
// owner thinks they used rather than the one they did.
var ErrAccountNotFoundForEmail = errors.New("no account with that email address")

// setAccountPassword validates, hashes and stores a new password, then closes the account's browser
// sessions and reports how many there were.
//
// Split out from the flag and configuration handling above so it can be tested against a real
// database without a process environment: the interesting behaviour is what happens to the stored
// hash and the sessions, not how the password reached the process.
func setAccountPassword(ctx context.Context, pool *pgxpool.Pool, normalisedEmail string, password string) (int64, error) {
	// Checked before the account is looked up, so a weak password is refused the same way here as
	// it is at setup and at registration — from auth.ValidatePassword, the one place that decides.
	if err := auth.ValidatePassword(password); err != nil {
		return 0, err
	}

	account, err := store.FindAccountByEmail(ctx, pool, normalisedEmail)
	if errors.Is(err, store.ErrAccountNotFound) {
		return 0, fmt.Errorf("%w: %s", ErrAccountNotFoundForEmail, normalisedEmail)
	}
	if err != nil {
		return 0, err
	}

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return 0, fmt.Errorf("hashing the password: %w", err)
	}
	if err := store.UpdatePasswordHash(ctx, pool, account.ID, passwordHash); err != nil {
		return 0, err
	}

	// After the hash, never before. The order is what decides which way a crash falls: sessions
	// closed against an unchanged password is an owner signed out for nothing, while a changed
	// password with the old sessions still open is the security hole this exists to close.
	return store.DeleteAdminSessionsForAccount(ctx, pool, account.ID)
}
